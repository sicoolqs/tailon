# MCP Implementation Test Report

This report records the checks actually executed while adding the native MCP
server. Results are from the development workstation listed below; they are not
a promise of production performance.

## Environment

- OS: Windows 10.0.26200 (development)
- Go: go1.27.1 windows/amd64
- CPU: AMD Ryzen 7 8845H
- Tailon module Go version: 1.23.2
- MCP SDK: `github.com/modelcontextprotocol/go-sdk v1.2.0`
- Release targets: `linux/amd64`, `linux/arm64`, `CGO_ENABLED=0`

## Functional verification

| Check | Command | Result |
| --- | --- | --- |
| Unit and integration tests | `go test ./...` | PASS |
| Race detector | `go test -race ./...` | PASS |
| Static analysis | `go vet ./...` | PASS |
| Build | `go build ./...` | PASS |
| Linux AMD64 build | `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build` | PASS |
| Linux ARM64 build | `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build` | PASS |
| Live binary smoke test | Start Tailon with `[mcp] enabled=true`, call `initialize`, `tools/list`, `logs_list`, `logs_search` | PASS (HTTP 200, 8 tools) |
| Web UI unaffected | `GET /` with MCP enabled | PASS (HTTP 200) |
| MCP disabled | `POST /mcp` with MCP disabled | PASS (HTTP 404) |

### Protocol tests

The MCP tests use the official Go SDK client against a real `httptest` HTTP
server; no hand-written JSON-RPC mocks.

Covered scenarios:

- `initialize` protocol negotiation (2025-06-18).
- `ping`.
- `tools/list` returns exactly the eight tools with input schemas and
  read-only annotations.
- `logs_list`, `logs_stat`, `logs_tail`, `logs_search`, `logs_context`,
  `logs_read`, `logs_summary`, `logs_trace` end-to-end.
- Cursor continuation for `logs_search` and `logs_read`.
- Tool errors (`FILE_NOT_FOUND`) are returned as MCP tool results with
  `isError: true` and a stable JSON error code.
- Stateless mode across two independent client sessions.
- Cross-origin and `Sec-Fetch-Site: cross-site` requests are rejected with 403.
- Oversized request bodies are rejected with 413.
- Oversized responses are trimmed rather than failing the tool call.
- Panic inside a tool handler is recovered and returned as `INTERNAL_ERROR`
  without terminating the process.

### Query engine tests

Covered scenarios include:

- Forward and reverse reading across buffer boundaries.
- LF and CRLF line endings.
- UTF-8 and GBK decoding.
- Overlong line truncation.
- Long multiline Java exception events.
- JSON logs.
- Trace/request/span identifier extraction.
- Literal and RE2 regular expression search.
- Level and time filtering.
- Context extraction around byte offsets.
- Summary levels, fingerprints, timeline and per-file statistics.
- Trace correlation across groups and files.
- Cursor tampering and expiry.
- Symlink escape rejection.
- Directory/non-regular file rejection.
- Glob discovery of new files and rotation with a stable `file_id`.
- Cursor invalidation after truncation.
- Concurrent query limit (`SERVER_BUSY`).
- Scan budget truncation (`SCAN_LIMIT`).
- Canceled request context.
- Gzip tail, search, stat and read restrictions.
- Panic recovery at the tool boundary.

## Performance checks

Benchmarks were run with `-benchtime=1x` on the generated 50,000-line test log
(approximately 4.5 MiB). These are single-run indicative numbers, not
statistical benchmarks.

| Benchmark | Result |
| --- | --- |
| `BenchmarkReverseRead` (200 lines) | ~55 µs/op |
| `BenchmarkLiteralSearch` | ~134 ms/op |
| `BenchmarkRegexSearch` | ~161 ms/op |
| `BenchmarkMultilineParser` | ~17 µs/op |
| `BenchmarkLogSummary` | ~256 ms/op |
| `BenchmarkTraceSearch` | ~34 ms/op |

Large-file behavior (opt-in test):

```text
TAILON_LARGE_TEST=1 TAILON_LARGE_TEST_SIZE_MB=64 go test -run TestLargeFileTailBounded -v ./internal/logquery/
```

Observed:

```text
tail on 64 MiB file: entries=200 scanned_bytes=65537 duration=4.1091ms
```

The tail operation read one 64 KiB buffer plus one byte to strip the trailing
newline. It did not scan the 64 MiB file.

Soak behavior (opt-in test, 1000 iterations of list + search + tail):

```text
soak: iterations=1000 goroutines_before=2 goroutines_after=2
```

No goroutine growth was observed.

## Build artifacts

| Artifact | Size |
| --- | --- |
| `dist/tailon-2.0.0-mcp-linux-amd64` | 12,521,632 bytes |
| `dist/tailon-2.0.0-mcp-linux-arm64` | 11,665,568 bytes |

Both were produced with `CGO_ENABLED=0`, `-trimpath` and `-ldflags="-s -w"`.

## Compatibility notes and limitations

- SLES 12 SP5 / Linux 4.12 runtime verification was **not** performed in this
  environment; the binaries are static and target the supported kernel range,
  but must be run on the target host before release.
- The ARM64 binary was cross-compiled but not executed on ARM hardware in this
  environment.
- Benchmarks and timings are Windows development-host values.
- No 1 GiB or 5 GiB files were generated during this run. The 64 MiB opt-in
  test validates that tail is bounded; use
  `TAILON_LARGE_TEST_SIZE_MB=1024` (or larger) on the target host for a full
  acceptance run.
- `max-parallel-files` is validated but file scans are currently sequential.
- Gzip files support forward scans only; offset-based context/read operations
  return `UNSUPPORTED_FILE_TYPE`.

## Reproduction commands

```sh
go test ./...
go test -race ./...
go vet ./...
go build ./...

TAILON_LARGE_TEST=1 TAILON_LARGE_TEST_SIZE_MB=64 \
  go test -run TestLargeFileTailBounded -v ./internal/logquery/

TAILON_SOAK=1 TAILON_SOAK_CALLS=1000 \
  go test -run TestSoakToolCalls -v ./internal/logquery/

go test -run '^$' -bench . -benchtime=1x ./internal/logquery/

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/tailon-linux-amd64 .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o dist/tailon-linux-arm64 .
```
