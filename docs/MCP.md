# Tailon MCP Server

Tailon can serve a native, read-only MCP (Model Context Protocol) endpoint next
to the existing web UI. The MCP server reuses the same process, HTTP listener,
port, file configuration and access scope. There is no separate daemon and no
additional runtime dependency.

- Transport: MCP Streamable HTTP, stateless, JSON responses.
- Default endpoint: `/mcp`.
- Authentication: none by design; intended for a trusted intranet.
- Tools: `logs_list`, `logs_stat`, `logs_tail`, `logs_search`, `logs_context`,
  `logs_read`, `logs_summary`, `logs_trace`.
- All tools are read-only and can only access files already configured in
  Tailon.

## Enabling the server

### Configuration file

```toml
[mcp]
enabled = true
path = "/mcp"
```

### Command line

```sh
tailon --mcp-enabled --mcp-path /mcp "group=workbench,/var/log/workbench/*.log"
```

MCP is disabled by default. When it is disabled, the configured MCP path
returns `404` and the web UI behaves exactly as before.

As with every Tailon option, supplying `-c` makes the configuration file
authoritative; command-line flags are not merged into it.

Startup output includes:

```text
MCP server enabled
MCP endpoint: /mcp
MCP transport: streamable-http
MCP mode: stateless
MCP tools registered: 8
MCP max concurrent queries: 4
MCP query timeout: 15s
```

## Client configuration

### Claude Code

```sh
claude mcp add --transport http tailon http://192.168.1.100:18029/mcp
```

Or in `.mcp.json`:

```json
{
  "mcpServers": {
    "tailon": {
      "type": "http",
      "url": "http://192.168.1.100:18029/mcp"
    }
  }
}
```

### OpenCode

In `opencode.json`:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "tailon": {
      "type": "remote",
      "url": "http://192.168.1.100:18029/mcp",
      "enabled": true
    }
  }
}
```

### Codex

In `~/.codex/config.toml`:

```toml
[mcp_servers.tailon]
url = "http://192.168.1.100:18029/mcp"
```

### Other Streamable HTTP clients

Use the standard remote-server form:

```json
{
  "mcpServers": {
    "tailon": {
      "type": "http",
      "url": "http://192.168.1.100:18029/mcp"
    }
  }
}
```

No headers, tokens or environment variables are required.

## Tools

All tools use snake_case parameters. Defaults are applied by the server; the
JSON Schema only documents them. Every result contains a `meta` object with
`complete`, `truncated`, `truncation_reason`, `duration_ms`, `scanned_files`,
`scanned_bytes`, `returned_count`, `has_more`, `next_cursor` and `warnings`.

### logs_list

Discover log groups and files. Use it before any other tool.

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `group` | string | all | Filter by group. `all` or empty returns every group. |
| `pattern` | string | all | Glob matched against file display names. |
| `include_files` | boolean | `true` | Include file details. |
| `modified_within` | string | none | Only files modified within a duration such as `24h`. |
| `limit` | integer | 100 | Maximum files (server maximum 500). |
| `cursor` | string | none | Continuation cursor. |

### logs_stat

Inspect file metadata without reading the file.

| Parameter | Type | Required | Description |
| --- | --- | --- | --- |
| `file_id` | string | yes | Identifier from `logs_list`. |

Returns size, modification time, readability, compression and seekability.
For plain files it also samples the first and last timestamps
(`sampled_time_range.sampled` is `true`).

### logs_tail

Read the most recent events.

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `file_id` | string | none | Select one file. |
| `group` | string | all | Select a group. |
| `file_pattern` | string | none | Filter display names by glob. |
| `lines` | integer | 200 | Maximum physical lines across files (server maximum 5000). |
| `level` | string | none | Single level filter. |
| `query` | string | none | Case-insensitive literal filter. |
| `include_stacktrace` | boolean | `true` | Keep multiline exception events. |
| `order` | string | `desc` | `asc` or `desc`. |

### logs_search

Search across files.

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `file_id` | string | none | Search one file. |
| `group` | string | all | Search one group. |
| `file_pattern` | string | none | Filter display names by glob. |
| `query` | string | none | Literal text or Go (RE2) regular expression. |
| `match_mode` | string | `literal` | `literal` or `regex`. |
| `case_sensitive` | boolean | `false` | Case-sensitive matching. |
| `since` | string | none | Relative range: `30m`, `2h`, `7d`. |
| `start_time` | string | none | RFC3339 inclusive start. |
| `end_time` | string | none | RFC3339 exclusive end. |
| `levels` | array | all | Allowed levels: `TRACE`, `DEBUG`, `INFO`, `WARN`, `ERROR`, `FATAL`, `UNKNOWN`. |
| `context_before` | integer | 0 | Physical lines before each match (maximum 100). |
| `context_after` | integer | 0 | Physical lines after each match (maximum 100). |
| `include_stacktrace` | boolean | `true` | Include multiline exception lines. |
| `order` | string | `desc` | Scan/return direction. |
| `limit` | integer | 50 | Maximum matched events (server maximum 500). |
| `cursor` | string | none | Continue a previous search. |

If `query` is empty, at least one time filter or `levels` must be provided.
`since` and `start_time` cannot be combined. Time intervals are
`[start, end)`.

### logs_context

Read around a byte offset previously returned by another tool.

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `file_id` | string | none | File identifier. |
| `byte_offset` | integer | none | Offset returned by a previous result. |
| `cursor` | string | none | Cursor that pins a file position. |
| `before_lines` | integer | 20 | Lines before the event (maximum 100). |
| `after_lines` | integer | 50 | Lines after the event (maximum 100). |
| `include_stacktrace` | boolean | `true` | Expand the full multiline event. |

### logs_read

Read a bounded file segment.

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `file_id` | string | required | File identifier. |
| `offset` | integer | start/end | Starting byte offset. |
| `cursor` | string | none | Continuation cursor. |
| `lines` | integer | 200 | Physical lines per page (maximum 5000). |
| `direction` | string | `forward` | `forward` or `backward`. |
| `include_stacktrace` | boolean | `true` | Do not split multiline events at page boundaries. |

The first page pins the file size snapshot, so pagination always terminates.
New appends are visible to a new `logs_read` call without a cursor.

### logs_summary

Aggregate levels, errors, files and a timeline.

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `file_id` | string | none | One file. |
| `group` | string | all | One group. |
| `file_pattern` | string | none | Display-name glob. |
| `since` | string | `1h` | Relative range. |
| `start_time` | string | none | RFC3339 start. |
| `end_time` | string | none | RFC3339 end. |
| `levels` | array | all | Only count these levels. |
| `top_n` | integer | 10 | Top error fingerprints. |
| `bucket` | string | `5m` | Timeline bucket: `1m`, `5m`, `15m`, `1h`. |
| `include_examples` | boolean | `true` | Include one example per error. |

Error fingerprints normalize timestamps, UUIDs, IPs, long hex identifiers and
numbers. A multiline exception is counted as one event.

### logs_trace

Correlate a trace, request or correlation identifier across groups and files.

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `trace_id` | string | required | Complete identifier (4-128 characters). |
| `id_type` | string | `auto` | `auto`, `trace_id`, `request_id`, `correlation_id`. |
| `groups` | array | all | Groups to search. |
| `file_pattern` | string | none | Display-name glob. |
| `since` | string | `2h` | Relative range. |
| `start_time` | string | none | RFC3339 start. |
| `end_time` | string | none | RFC3339 end. |
| `include_stacktrace` | boolean | `true` | Include exception lines. |
| `limit` | integer | 100 | Maximum observed events. |
| `cursor` | string | none | Continue a previous trace. |

Matching first uses structured fields (`traceId`, `trace_id`, `spanId`,
`requestId`, `correlationId`, JSON keys and MDC text). A bounded field-based
text match is used as a fallback. Plain unconstrained substring matching is not
used for short identifiers.

Timeline ordering depends on synchronized clocks across hosts. The reported
`observed_duration_ms` is the span between the first and last observed event,
not the request's actual latency.

## Recommended workflows

- Deployment check: `logs_list` -> `logs_tail` -> `logs_search` -> `logs_context`.
- Incident triage: `logs_summary` -> `logs_search` -> `logs_context`.
- Cross-service request: `logs_trace` -> `logs_context` -> `logs_search`.
- Large-file analysis: `logs_search` -> `logs_context` -> `logs_read` (cursor).

## Configuration reference

All fields are optional. MCP is disabled unless `enabled = true`.

```toml
[mcp]
enabled = false
path = "/mcp"

# Restrict HTTP Host values. Empty accepts any Host for non-browser clients.
allowed-hosts = []

# Allow these browser origins. Empty means same-origin only.
allowed-origins = []

timezone = "Asia/Shanghai"
default-encoding = "UTF-8"
support-gzip = true

[mcp.limits]
max-concurrent-queries = 4
max-parallel-files = 2
query-timeout = "15s"

max-scan-bytes = 134217728
max-response-bytes = 65536
max-request-bytes = 65536

default-lines = 200
max-lines = 5000

default-results = 50
max-results = 500
max-context-lines = 100

max-files-per-query = 32
max-discovery-entries = 10000
max-list-page-size = 500

max-line-bytes = 65536
max-event-bytes = 131072
max-regex-length = 4096

max-summary-buckets = 288
max-aggregation-keys = 10000

read-buffer-bytes = 65536
cursor-ttl = "10m"

[mcp.redaction]
enabled = false
patterns = [
  '(?i)authorization\s*[:=]\s*\S+',
  '(?i)(access|refresh)[_-]?token\s*[:=]\s*\S+',
  '(?i)password\s*[:=]\s*\S+',
  '(?i)api[_-]?key\s*[:=]\s*\S+',
]
```

Notes:

- `max-parallel-files` is an upper bound. The current implementation scans one
  file at a time to keep ordering and budgets deterministic.
- `allowed-hosts` and `allowed-origins` should be configured when Tailon is
  reachable through a DNS name from browsers.
- Redaction patterns are Go regular expressions and apply to every message,
  context line and stack trace returned by MCP.
- Existing Tailon configuration files remain valid; omitting `[mcp]` leaves the
  feature disabled.

## Security model

MCP is anonymous by design, but it is not an unrestricted file reader:

- Only files in Tailon's configured file specs are visible. File IDs are
  resolved again on every access; stale IDs cannot escape a changed scope.
- Paths are made absolute, cleaned, resolved through symlinks and checked
  against the directory tree of the matching spec. Symlinks that escape the
  configured tree are rejected.
- After opening, the file type is checked and the descriptor is compared with
  the path's current identity to detect replacement between validation and
  open.
- Only regular files are readable. Devices, pipes, sockets and directories are
  rejected.
- MCP never builds shell commands. All tools are implemented with Go file I/O.
- Browser cross-site requests are rejected. Requests without an `Origin`
  header (normal MCP clients) are accepted; same-origin browser requests are
  accepted; `Sec-Fetch-Site: cross-site` is rejected. No permissive CORS
  headers are emitted.
- Request bodies are limited by `max-request-bytes`.
- Log content is untrusted data. It is returned as text and never interpreted
  as instructions or commands.
- Operational logs never contain full log lines or MCP responses.

For DNS-rebinding hardening, set `allowed-hosts` to the exact host names or IP
addresses clients use, for example:

```toml
allowed-hosts = ["10.0.0.21", "logs.internal.example"]
allowed-origins = ["http://10.0.0.21:18029", "https://logs.internal.example"]
```

## Resource limits and truncation

The server enforces the limits above regardless of client request values.
Results may be incomplete; inspect `meta`:

| `truncation_reason` | Meaning |
| --- | --- |
| `RESULT_LIMIT` | The requested result count was reached. |
| `RESPONSE_LIMIT` | The serialized response exceeded `max-response-bytes` and was trimmed. |
| `SCAN_LIMIT` | `max-scan-bytes` was reached. |
| `QUERY_TIMEOUT` | The query exceeded `query-timeout`. |
| `FILE_UNREADABLE` | A file could not be read. |
| `PARTIAL_FILES` | Some selected files failed; other results are still valid. |

An empty result with `complete: true` is a valid "no matches" answer and is not
an error.

## Error codes

Tool errors are returned as MCP tool results with `isError: true` and a JSON
body:

```json
{
  "error": {
    "code": "SERVER_BUSY",
    "message": "the concurrent query limit has been reached",
    "retryable": true,
    "suggestion": "Retry with a narrower query."
  }
}
```

| Code | Meaning |
| --- | --- |
| `INVALID_ARGUMENT` | Invalid tool argument. |
| `INVALID_TIME_RANGE` | Invalid or reversed time range. |
| `INVALID_REGEX` | Invalid regular expression. |
| `FILE_NOT_FOUND` | File is unavailable. |
| `GROUP_NOT_FOUND` | Group does not exist. |
| `FILE_ACCESS_DENIED` | File is outside the allowed scope. |
| `FILE_CHANGED` | File was truncated or replaced. |
| `FILE_ROTATED` | File referenced by a cursor was rotated. |
| `UNSUPPORTED_FILE_TYPE` | Unsupported file type or operation for that type. |
| `INVALID_CURSOR` | Cursor is malformed, tampered with or for another query. |
| `CURSOR_EXPIRED` | Cursor TTL expired. |
| `QUERY_TIMEOUT` | Query exceeded its time budget. |
| `SCAN_LIMIT_EXCEEDED` | Query exceeded its scan budget before producing a result. |
| `RESPONSE_LIMIT_EXCEEDED` | Response could not be reduced to the size limit. |
| `SERVER_BUSY` | Concurrent query limit reached. |
| `INTERNAL_ERROR` | Unexpected internal error. |

## Observability

Each tool call logs one line without log content:

```text
MCP tool completed: tool=logs_search request_id=42 duration_ms=128 scanned_files=3 scanned_bytes=1048576 results=12 truncated=false reason=
```

Failures log the tool, request ID, duration and stable error code. Runtime
counters include tool calls, successes, failures, active queries, rejected
queries, timeouts, truncated queries and scanned bytes.

## Building and deploying

```sh
sh scripts/build-linux-amd64.sh
sh scripts/build-linux-arm64.sh
```

The build uses `CGO_ENABLED=0`, producing static binaries for Linux AMD64 and
ARM64. The resulting binary has no runtime dependency on Go, glibc, Docker,
systemd or root privileges. Distribute the binary and the existing TOML
configuration; no additional process is required.

SLES 12 SP5 with Linux 4.12 is within the supported kernel range for Go
1.23-built static binaries. Run the binary on the target host and verify
startup plus `POST /mcp` with an `initialize` request before switching traffic.

## Testing

```sh
go test ./...
go test -race ./...
go vet ./...

TAILON_LARGE_TEST=1 TAILON_LARGE_TEST_SIZE_MB=64 \
    go test -run TestLargeFileTailBounded -v ./internal/logquery/

TAILON_SOAK=1 TAILON_SOAK_CALLS=1000 \
    go test -run TestSoakToolCalls -v ./internal/logquery/

go test -run '^$' -bench . -benchtime=1x ./internal/logquery/
```

The protocol tests use the official MCP Go SDK client against a real HTTP
server, covering tool discovery, all eight tools, tool errors, stateless
sessions, origin/host checks, request limits and response trimming.

## Known limitations

- MCP is disabled by default and has no authentication; deploy it on a trusted
  network.
- Gzip files support forward scans only. Byte-offset reads and `logs_context`
  return `UNSUPPORTED_FILE_TYPE` for compressed files.
- Cursors are signed with a per-process key. Restarting Tailon invalidates
  outstanding cursors; clients receive `INVALID_CURSOR`.
- Summary and trace scans may be partial on very large time ranges; this is
  reported in `meta` and `warnings`.
- Timestamp and level detection is heuristic for unstructured logs. Unparsed
  timestamps are reported as warnings and are not silently treated as in-range.
- `line_number` is always `null`; byte offsets are the primary position.
- The compressed flag in `logs_list` is derived from the file name; `logs_stat`
  also detects gzip by magic bytes.
- Trace topology is not inferred. Only explicitly present span/parent-span
  fields are returned.
- `max-parallel-files` is currently a validated upper bound; file scans are
  sequential.
