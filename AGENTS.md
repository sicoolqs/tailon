# AGENTS.md

## Project

Tailon is a Go web application for viewing and filtering log files. This
repository is based on the official `v2.0.0` release and runs external commands
such as `tail`, `grep`, `sed`, and `awk` configured through TOML.

Main files:

```text
main.go            CLI, TOML configuration, startup
server.go          HTTP/SockJS handlers, command execution, session lifecycle
filelister.go      File specs, UI file listing, allowed-file lookup
cmd/               Vendored command runner with streaming stdout/stderr
internal/logquery/ Read-only log query engine (registry, readers, parser, search)
internal/mcpserver/ MCP Streamable HTTP server, tools, middleware, schemas
mcp.go             MCP configuration parsing and startup wiring
frontend/          Vue frontend sources and prebuilt dist assets
frontend_gen.go    Embeds frontend/dist and frontend/templates
scripts/           Build helper scripts
dist/              Local build output (ignored by Git)
docs/MCP.md        MCP configuration, tools and client documentation
docs/MCP_TEST_REPORT.md  Recorded MCP verification results
```

## Toolchain and targets

- Module Go version: `go 1.23.2`.
- Local development has been validated with Go 1.27.1.
- Release target: `GOOS=linux GOARCH=amd64 CGO_ENABLED=0`.
- Produced binaries are static and must not require Go, glibc, Docker, systemd,
  or root on the target host.
- Supported production environment: SLES 12 SP5, Linux 4.12 kernel or newer.
- Keep the existing TOML format, HTTP routes, port behavior, and frontend
  protocol compatible with `v2.0.0`.

## Common commands

Generate the embedded frontend file (`frontend_bin.go`, ignored by Git):

```sh
go generate ./...
```

Run tests:

```sh
go test ./...
go test -race ./...
go vet ./...
```

Tests can also run against development frontend assets:

```sh
go test -tags dev ./...
```

Integration tests with a real `tail` binary:

```sh
go test -run TestTailFollowAppendAndRotation -v .
```

Opt-in large-file and soak tests:

```sh
TAILON_LARGE_TEST=1 TAILON_LARGE_TEST_SIZE_MB=64 \
    go test -run TestLargeFileStreamingIntegration -v .

TAILON_SOAK=1 TAILON_SOAK_SECONDS=10 \
    go test -run TestSoakManyFilesAndSessions -v .
```

Build the Linux AMD64 release binary:

```sh
sh scripts/build-linux-amd64.sh
# or provide a custom file name:
sh scripts/build-linux-amd64.sh tailon-2.0.0-custom-linux-amd64
```

Build the Linux ARM64 binary:

```sh
sh scripts/build-linux-arm64.sh
```

Opt-in MCP large-file and soak tests:

```sh
TAILON_LARGE_TEST=1 TAILON_LARGE_TEST_SIZE_MB=64 \
    go test -run TestLargeFileTailBounded -v ./internal/logquery/

TAILON_SOAK=1 TAILON_SOAK_CALLS=1000 \
    go test -run TestSoakToolCalls -v ./internal/logquery/
```

Output directory is always `dist/`:

```text
dist/tailon-2.0.0-stability-linux-amd64
```

`dist/` is ignored by Git. Do not commit build artifacts.

## Runtime architecture and invariants

### MCP server

- MCP is disabled by default and shares the existing HTTP server, port and
  configured file scope. Do not start a separate listener.
- Every file access must go through `logquery.Registry.Open`, which re-resolves
  the `file_id`, resolves symlinks, re-checks the configured scope and verifies
  the opened descriptor.
- MCP tools are read-only. Never build shell commands from tool arguments.
- All query results must respect `mcp.limits`: scan bytes, response bytes,
  result counts, context lines, regex length, concurrency and timeout.
- Recover panics at the tool and HTTP handler boundaries. A tool failure must
  never terminate the process or disturb the web UI.
- Keep cursor payloads HMAC-signed and re-validate file identity on resume.
- New tools must be added to `internal/mcpserver/schema.go`, registered in
  `tools.go`, covered by tests, and documented in `docs/MCP.md`.

### Web UI and command pipeline

- One `serveSession` goroutine reads client messages.
- One `wsWriter` goroutine owns the complete command pipeline and is the only
  goroutine that sends WebSocket messages or stops the current command.
- Do not access `exec.Cmd.Process` before `Start` succeeds.
  `cmd.Cmd.WaitStarted` must be used before stopping an asynchronously started
  command.
- Command cleanup uses bounded waits, drains stdout/stderr channels, sends
  SIGTERM first, and uses SIGKILL only as a fallback.
- Client commands must pass `prepareCommand` validation:
  - command present in `allow-commands`;
  - command present in `CommandSpecs`;
  - stdin source exists;
  - action is non-empty;
  - expanded argument count and byte length are bounded;
  - requested file is present in the allowed file listing.
- Invalid client messages are logged and ignored; they must never panic the
  process or terminate an already running view.
- `allFiles` is guarded by `allFilesMu`. Build a new map and swap it under the
  write lock; readers use `fileAllowed`.
- Log viewing must remain streaming. Never read a complete log file into
  memory. Preserve `tail -F` append and rotation behavior.
- WebSocket messages use the existing protocol:
  - client: `"list"` or a JSON `FrontendCommand`;
  - server: file-list JSON object, or `["o", line]` / `["e", line]`.

## Platform notes

- `cmd/proc_unix.go` implements POSIX process-group signaling.
- `cmd/proc_windows.go` exists so tests and development builds compile on
  Windows; release artifacts target Linux.
- `filelister.go` uses `filepath` for cross-platform glob aliases. Linux output
  is unchanged from the original `path`-based implementation.
- `frontend_bin.go` is generated and ignored. Run `go generate ./...` before a
  production build or plain `go test` when it is absent.

## Testing notes

- Lifecycle tests use `TestHelperProcess` as a re-executed child process. Keep
  helper modes deterministic and terminate promptly.
- Every test that starts a session or subprocess must have a timeout and a
  cleanup path.
- Prefer table-driven tests and existing helper functions in
  `testhelpers_test.go`.
- Run `go test -race ./...` after changing session, channel, process, or file
  listing code.
- Do not use `recover()` or automatic process restart as a substitute for
  fixing lifecycle bugs.
- Shell scripts must use English messages for status and errors.
