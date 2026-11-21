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
- Release targets: `GOOS=linux GOARCH=amd64 CGO_ENABLED=0` and
  `GOOS=linux GOARCH=arm64 CGO_ENABLED=0`.
- Produced binaries are static and must not require Go, glibc, Docker, systemd,
  or root on the target host.
- Supported production environment: SLES 12 SP5, Linux 4.12 kernel or newer.
- Keep the existing TOML format, HTTP routes, port behavior, and frontend
  protocol compatible with `v2.0.0`.
- MCP dependency: `github.com/modelcontextprotocol/go-sdk v1.2.0`. This release
  supports the module's `go 1.23` directive. SDK v1.4.0 and newer require
  Go 1.25; do not upgrade without revalidating the target toolchain and
  deployment environment.

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

Direct cross-compilation without the scripts:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -buildvcs=false -trimpath -ldflags="-s -w" \
    -o dist/tailon-2.0.0-mcp-linux-amd64 .

CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build \
    -buildvcs=false -trimpath -ldflags="-s -w" \
    -o dist/tailon-2.0.0-mcp-linux-arm64 .
```

`-buildvcs=false` avoids VCS stamping failures in workspaces that contain
multiple VCS roots.

Run the MCP protocol and query-engine tests with the official SDK client:

```sh
go test ./internal/logquery/ ./internal/mcpserver/
go test -race ./internal/logquery/ ./internal/mcpserver/
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
dist/tailon-2.0.0-mcp-linux-amd64
dist/tailon-2.0.0-mcp-linux-arm64
dist/tailon-2.0.0-stability-linux-amd64
```

`dist/` is ignored by Git. Do not commit build artifacts.

## Runtime architecture and invariants

### MCP server

- MCP is enabled by default (`[mcp].enabled` defaults to `true`) and shares the
  existing HTTP server, port and configured file scope. Setting `enabled =
  false` disables it. Do not start a separate listener. The default endpoint is
  `/mcp`.
- No configuration is required to enable MCP. `--mcp-enabled` and
  `--mcp-path` only override built-in defaults when no config file is used.
  When disabled, the MCP path returns 404. When `-c` is used, the configuration
  file is authoritative and command-line flags are not merged.
- Every file access must go through `logquery.Registry.Open`, which re-resolves
  the `file_id`, resolves symlinks, re-checks the configured scope and verifies
  the opened descriptor.
- MCP tools are read-only. Never build shell commands from tool arguments.
- The registered tools are `logs_list`, `logs_stat`, `logs_tail`, `logs_search`,
  `logs_context`, `logs_read`, `logs_summary`, `logs_trace`.
- All query results must respect `mcp.limits`: scan bytes, response bytes,
  result counts, context lines, regex length, concurrency and timeout.
- Recover panics at the tool and HTTP handler boundaries. A tool failure must
  never terminate the process or disturb the web UI.
- Keep cursor payloads HMAC-signed and re-validate file identity on resume.
  Cursors are per-process; a restart invalidates them.
- New tools must be added to `internal/mcpserver/schema.go`, registered in
  `tools.go`, covered by tests, and documented in `docs/MCP.md`.
- Gzip input supports forward scans only; byte-offset reads and `logs_context`
  must return `UNSUPPORTED_FILE_TYPE` for compressed files.
- `max-parallel-files` is currently a validated upper bound; file scans are
  sequential so ordering and budgets stay deterministic.
- Summary and trace results may cover only the scanned range on very large
  inputs; mark this in `meta` and `warnings`, never as a complete result.

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
- MCP tests must use the official `modelcontextprotocol/go-sdk` client against a
  real HTTP handler. Do not replace protocol coverage with JSON-RPC mocks.
- MCP tests cover tool discovery, all eight tools, cursors, tool errors,
  stateless sessions, Host/Origin checks, request limits and response trimming.
- Opt-in MCP tests use `TAILON_LARGE_TEST` / `TAILON_LARGE_TEST_SIZE_MB` and
  `TAILON_SOAK` / `TAILON_SOAK_CALLS`; they must remain skipped by default.
- Do not use `recover()` or automatic process restart as a substitute for
  fixing lifecycle bugs.
- Shell scripts must use English messages for status and errors.
