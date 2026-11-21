package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gvalkov/tailon/internal/logquery"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	base := time.Now().UTC().Add(-15 * time.Minute)
	lines := []string{
		base.Format("2006-01-02 15:04:05.000") + " INFO Received request traceId=tr-abcdef",
		base.Add(time.Minute).Format("2006-01-02 15:04:05.000") + " ERROR Failed to process request traceId=tr-abcdef",
		"java.lang.NullPointerException: request failed",
		"    at com.example.Service.execute(Service.java:31)",
		base.Add(2*time.Minute).Format("2006-01-02 15:04:05.000") + " WARN downstream slow",
		base.Add(3*time.Minute).Format("2006-01-02 15:04:05.000") + " INFO done",
	}
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.Timezone = "UTC"
	cfg.Limits.QueryTimeout = 5 * time.Second
	svc, err := logquery.NewService(func() []logquery.Spec {
		return []logquery.Spec{{Path: path, Type: "file", Group: "app"}}
	}, cfg.ServiceConfig())
	if err != nil {
		t.Fatal(err)
	}
	return NewWithService(svc, cfg), path
}

func connectClient(t *testing.T, handler http.Handler) (*mcp.ClientSession, context.Context, func()) {
	t.Helper()
	ts := httptest.NewServer(handler)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	client := mcp.NewClient(&mcp.Implementation{Name: "tailon-test", Version: "1.0.0"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:   ts.URL,
		HTTPClient: ts.Client(),
		MaxRetries: -1,
	}
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		cancel()
		ts.Close()
		t.Fatalf("connect MCP client: %v", err)
	}
	cleanup := func() {
		_ = session.Close()
		cancel()
		ts.Close()
	}
	return session, ctx, cleanup
}

func callTool(t *testing.T, ctx context.Context, session *mcp.ClientSession, name string, args map[string]any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s returned a tool error: %s", name, textOf(t, res))
	}
	if out != nil {
		if err := json.Unmarshal([]byte(textOf(t, res)), out); err != nil {
			t.Fatalf("decode %s result: %v\n%s", name, err, textOf(t, res))
		}
	}
	return res
}

func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	for _, block := range res.Content {
		if text, ok := block.(*mcp.TextContent); ok {
			return text.Text
		}
	}
	t.Fatalf("result has no text content: %#v", res.Content)
	return ""
}

func TestMCPToolDiscovery(t *testing.T) {
	srv, _ := newTestServer(t)
	session, ctx, cleanup := connectClient(t, srv.Handler())
	defer cleanup()

	if err := session.Ping(ctx, nil); err != nil {
		t.Fatalf("ping: %v", err)
	}

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	want := []string{"logs_list", "logs_stat", "logs_tail", "logs_search", "logs_context", "logs_read", "logs_summary", "logs_trace"}
	got := map[string]bool{}
	for _, tool := range tools.Tools {
		got[tool.Name] = true
		if tool.InputSchema == nil {
			t.Errorf("tool %s has no input schema", tool.Name)
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %s is not marked read-only", tool.Name)
		}
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("tool %s was not registered", name)
		}
	}
	if len(tools.Tools) != len(want) {
		t.Errorf("registered %d tools, want %d", len(tools.Tools), len(want))
	}
}

func TestMCPEndToEndTools(t *testing.T) {
	srv, _ := newTestServer(t)
	session, ctx, cleanup := connectClient(t, srv.Handler())
	defer cleanup()

	var list logquery.ListResult
	callTool(t, ctx, session, "logs_list", map[string]any{"group": "app"}, &list)
	if len(list.Files) != 1 {
		t.Fatalf("list files = %#v", list.Files)
	}
	fileID := list.Files[0].FileID

	var stat logquery.StatResult
	callTool(t, ctx, session, "logs_stat", map[string]any{"file_id": fileID}, &stat)
	if stat.SizeBytes == 0 || !stat.Readable {
		t.Fatalf("stat = %#v", stat)
	}

	var tail logquery.TailResult
	callTool(t, ctx, session, "logs_tail", map[string]any{"file_id": fileID, "lines": 2}, &tail)
	if len(tail.Entries) != 2 {
		t.Fatalf("tail entries = %d", len(tail.Entries))
	}

	var search logquery.SearchResult
	callTool(t, ctx, session, "logs_search", map[string]any{
		"group": "app", "query": "Failed to process", "limit": 1,
		"context_before": 1, "context_after": 1,
	}, &search)
	if len(search.Results) != 1 {
		t.Fatalf("search results = %#v", search.Results)
	}
	hit := search.Results[0]
	if len(hit.Stacktrace) == 0 {
		t.Fatalf("expected stacktrace: %#v", hit)
	}

	var contextRes logquery.ContextResult
	callTool(t, ctx, session, "logs_context", map[string]any{
		"file_id": hit.FileID, "byte_offset": hit.ByteOffset, "before_lines": 1, "after_lines": 1,
	}, &contextRes)
	if len(contextRes.Lines) == 0 {
		t.Fatal("context returned no lines")
	}

	var read logquery.ReadResult
	callTool(t, ctx, session, "logs_read", map[string]any{
		"file_id": fileID, "lines": 2, "direction": "forward", "include_stacktrace": false,
	}, &read)
	if read.ReturnedLines != 2 || read.NextCursor == nil {
		t.Fatalf("read = %#v", read)
	}
	var read2 logquery.ReadResult
	callTool(t, ctx, session, "logs_read", map[string]any{
		"file_id": fileID, "lines": 2, "direction": "forward", "include_stacktrace": false,
		"cursor": *read.NextCursor,
	}, &read2)
	if read2.StartOffset < read.EndOffset {
		t.Fatalf("read pages overlap: %d < %d", read2.StartOffset, read.EndOffset)
	}

	var summary logquery.SummaryResult
	callTool(t, ctx, session, "logs_summary", map[string]any{"group": "app", "since": "1h", "bucket": "5m"}, &summary)
	if summary.Levels["ERROR"] == 0 {
		t.Fatalf("summary levels = %#v", summary.Levels)
	}

	var trace logquery.TraceResult
	callTool(t, ctx, session, "logs_trace", map[string]any{"trace_id": "tr-abcdef", "since": "1h"}, &trace)
	if trace.EventCount != 2 || len(trace.Errors) != 1 {
		t.Fatalf("trace = %#v", trace)
	}
}

func TestMCPToolError(t *testing.T) {
	srv, _ := newTestServer(t)
	session, ctx, cleanup := connectClient(t, srv.Handler())
	defer cleanup()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "logs_stat",
		Arguments: map[string]any{"file_id": "f_missing"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected a tool error")
	}
	var er logquery.ErrorResult
	if err := json.Unmarshal([]byte(textOf(t, res)), &er); err != nil {
		t.Fatal(err)
	}
	if er.Error == nil || er.Error.Code != logquery.CodeFileNotFound {
		t.Fatalf("error = %#v", er.Error)
	}
}

func TestMCPStatelessIndependentSessions(t *testing.T) {
	srv, _ := newTestServer(t)

	session1, ctx1, cleanup1 := connectClient(t, srv.Handler())
	defer cleanup1()
	var list1 logquery.ListResult
	callTool(t, ctx1, session1, "logs_list", map[string]any{}, &list1)

	session2, ctx2, cleanup2 := connectClient(t, srv.Handler())
	defer cleanup2()
	var list2 logquery.ListResult
	callTool(t, ctx2, session2, "logs_list", map[string]any{}, &list2)

	if len(list1.Files) != len(list2.Files) {
		t.Fatalf("sessions disagree: %d vs %d", len(list1.Files), len(list2.Files))
	}
}

func TestMCPOriginAndHostChecks(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader("{}"))
	req.Header.Set("Origin", "http://evil.example")
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodPost, ts.URL, strings.NewReader("{}"))
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Content-Type", "application/json")
	resp, err = ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site status = %d, want 403", resp.StatusCode)
	}
}

func TestMCPRequestBodyLimit(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.cfg.Limits.MaxRequestBytes = 1024
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := strings.Repeat("x", 4096)
	req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized request status = %d, want 413", resp.StatusCode)
	}
}

func TestRunToolPanicRecovery(t *testing.T) {
	srv, _ := newTestServer(t)
	res, err := runTool(srv, context.Background(), &mcp.CallToolRequest{}, "logs_list",
		func(ctx context.Context, in logquery.ListRequest) (any, error) {
			panic("simulated tool panic")
		})
	if err != nil {
		t.Fatalf("panic escaped the tool boundary: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected a tool error result, got %#v", res)
	}
	if srv.stats.Panics.Load() != 1 {
		t.Fatalf("panic counter = %d", srv.stats.Panics.Load())
	}
	var er logquery.ErrorResult
	if err := json.Unmarshal([]byte(textOf(t, res)), &er); err != nil {
		t.Fatal(err)
	}
	if er.Error == nil || er.Error.Code != logquery.CodeInternalError {
		t.Fatalf("error = %#v", er.Error)
	}
}

func TestMCPResponseLimit(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.cfg.Limits.MaxResponseBytes = 1200
	session, ctx, cleanup := connectClient(t, srv.Handler())
	defer cleanup()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "logs_search",
		Arguments: map[string]any{"group": "app", "levels": []string{"INFO", "WARN", "ERROR"}, "limit": 50, "context_before": 5, "context_after": 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("response was rejected instead of trimmed: %s", textOf(t, res))
	}
	if len(textOf(t, res)) > 1200 {
		t.Fatalf("response is %d bytes, limit is 1200", len(textOf(t, res)))
	}
}
