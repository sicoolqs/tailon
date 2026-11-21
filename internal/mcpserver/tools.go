package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/gvalkov/tailon/internal/logquery"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	listDescription    = "List available log groups and files. Use this tool before searching or reading logs to discover valid groups and file identifiers."
	statDescription    = "Inspect metadata of an available log file, including its size, modification time, compression state and whether it is seekable."
	tailDescription    = "Read the most recent log lines or events. Use this tool for startup checks, deployment verification, and recent failures."
	searchDescription  = "Search logs across available files using literal keywords, regular expressions, time ranges, and log levels. Prefer narrow time ranges and specific groups to reduce scanning cost."
	contextDescription = "Retrieve surrounding log context around a previously identified file offset. Use this tool to inspect exception stack traces and related events before and after a match."
	readDescription    = "Read a bounded segment of a log file. Supports byte offsets and continuation cursors for incremental log analysis."
	summaryDescription = "Summarize recent application logs, including log levels, frequent exceptions, error fingerprints, affected files, and time buckets. Use this tool to identify abnormal patterns before running targeted searches."
	traceDescription   = "Find and correlate log events sharing a trace or request identifier across applications and service instances. Returns a chronological timeline of observed events and associated errors."
)

func readOnlyAnnotations() *mcp.ToolAnnotations {
	no := false
	return &mcp.ToolAnnotations{
		ReadOnlyHint:    true,
		DestructiveHint: &no,
		IdempotentHint:  true,
		OpenWorldHint:   &no,
	}
}

func (s *Server) registerTools() {
	ann := readOnlyAnnotations()

	s.mcpServer.AddTool(
		&mcp.Tool{Name: "logs_list", Description: listDescription, InputSchema: listSchema(), Annotations: ann},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return runTool(s, ctx, req, "logs_list", func(ctx context.Context, in logquery.ListRequest) (any, error) {
				return s.svc.List(ctx, in)
			})
		},
	)

	s.mcpServer.AddTool(
		&mcp.Tool{Name: "logs_stat", Description: statDescription, InputSchema: statSchema(), Annotations: ann},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return runTool(s, ctx, req, "logs_stat", func(ctx context.Context, in logquery.StatRequest) (any, error) {
				return s.svc.Stat(ctx, in)
			})
		},
	)

	s.mcpServer.AddTool(
		&mcp.Tool{Name: "logs_tail", Description: tailDescription, InputSchema: tailSchema(), Annotations: ann},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return runTool(s, ctx, req, "logs_tail", func(ctx context.Context, in logquery.TailRequest) (any, error) {
				return s.svc.Tail(ctx, in)
			})
		},
	)

	s.mcpServer.AddTool(
		&mcp.Tool{Name: "logs_search", Description: searchDescription, InputSchema: searchSchema(), Annotations: ann},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return runTool(s, ctx, req, "logs_search", func(ctx context.Context, in logquery.SearchRequest) (any, error) {
				return s.svc.Search(ctx, in)
			})
		},
	)

	s.mcpServer.AddTool(
		&mcp.Tool{Name: "logs_context", Description: contextDescription, InputSchema: contextSchema(), Annotations: ann},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return runTool(s, ctx, req, "logs_context", func(ctx context.Context, in logquery.ContextRequest) (any, error) {
				return s.svc.Context(ctx, in)
			})
		},
	)

	s.mcpServer.AddTool(
		&mcp.Tool{Name: "logs_read", Description: readDescription, InputSchema: readSchema(), Annotations: ann},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return runTool(s, ctx, req, "logs_read", func(ctx context.Context, in logquery.ReadRequest) (any, error) {
				return s.svc.Read(ctx, in)
			})
		},
	)

	s.mcpServer.AddTool(
		&mcp.Tool{Name: "logs_summary", Description: summaryDescription, InputSchema: summarySchema(), Annotations: ann},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return runTool(s, ctx, req, "logs_summary", func(ctx context.Context, in logquery.SummaryRequest) (any, error) {
				return s.svc.Summary(ctx, in)
			})
		},
	)

	s.mcpServer.AddTool(
		&mcp.Tool{Name: "logs_trace", Description: traceDescription, InputSchema: traceSchema(), Annotations: ann},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return runTool(s, ctx, req, "logs_trace", func(ctx context.Context, in logquery.TraceRequest) (any, error) {
				return s.svc.Trace(ctx, in)
			})
		},
	)
}

var requestCounter atomic.Uint64

func runTool[In any](s *Server, ctx context.Context, req *mcp.CallToolRequest, name string, call func(context.Context, In) (any, error)) (result *mcp.CallToolResult, err error) {
	requestID := requestCounter.Add(1)
	started := time.Now()

	defer func() {
		if r := recover(); r != nil {
			s.stats.Panics.Add(1)
			log.Printf("error: MCP tool %s panicked: request_id=%d panic=%v\n%s", name, requestID, r, debug.Stack())
			result = toolError(logquery.NewError(logquery.CodeInternalError, "an internal error occurred"))
			err = nil
		}
	}()

	var in In
	if derr := decodeArguments(req, &in); derr != nil {
		log.Printf("MCP tool rejected: tool=%s request_id=%d error=%q", name, requestID, derr.Error())
		return toolError(logquery.NewError(logquery.CodeInvalidArgument, "tool arguments are invalid: "+derr.Error())), nil
	}

	v, ferr := call(ctx, in)
	elapsed := time.Since(started).Milliseconds()
	if ferr != nil {
		appErr := logquery.AsError(ferr)
		log.Printf("MCP tool failed: tool=%s request_id=%d duration_ms=%d code=%s message=%q",
			name, requestID, elapsed, appErr.Code, appErr.Message)
		return toolError(appErr), nil
	}

	logquery.FitResponse(v, s.cfg.Limits.MaxResponseBytes)
	body, merr := json.Marshal(v)
	if merr != nil {
		log.Printf("error: MCP tool %s could not encode its result: %v", name, merr)
		return toolError(logquery.NewError(logquery.CodeInternalError, "the result could not be encoded")), nil
	}
	if len(body) > s.cfg.Limits.MaxResponseBytes {
		log.Printf("MCP tool failed: tool=%s request_id=%d duration_ms=%d code=%s size=%d",
			name, requestID, elapsed, logquery.CodeResponseLimit, len(body))
		return toolError(logquery.NewError(logquery.CodeResponseLimit, "the response exceeded the configured size limit").
			WithHint(true, "Retry with a smaller limit or fewer context lines.")), nil
	}

	meta := metaOf(v)
	count := countOf(v)
	truncatedReason := ""
	if meta != nil {
		if meta.Truncated {
			truncatedReason = meta.TruncationReason
		}
		log.Printf("MCP tool completed: tool=%s request_id=%d duration_ms=%d scanned_files=%d scanned_bytes=%d results=%d truncated=%t reason=%s",
			name, requestID, elapsed, meta.ScannedFiles, meta.ScannedBytes, count, meta.Truncated, truncatedReason)
	} else {
		log.Printf("MCP tool completed: tool=%s request_id=%d duration_ms=%d results=%d truncated=false",
			name, requestID, elapsed, count)
	}

	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}}, nil
}

func decodeArguments(req *mcp.CallToolRequest, out any) error {
	if req == nil || req.Params.Arguments == nil {
		return nil
	}
	raw := bytes.TrimSpace(req.Params.Arguments)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func toolError(e *logquery.Error) *mcp.CallToolResult {
	body, err := json.Marshal(logquery.ErrorResult{Error: e})
	if err != nil {
		body = []byte(`{"error":{"code":"INTERNAL_ERROR","message":"internal error","retryable":false}}`)
	}
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
	}
}

func metaOf(v any) *logquery.Meta {
	switch r := v.(type) {
	case *logquery.ListResult:
		return &r.Meta
	case *logquery.StatResult:
		return &r.Meta
	case *logquery.TailResult:
		return &r.Meta
	case *logquery.SearchResult:
		return &r.Meta
	case *logquery.ContextResult:
		return &r.Meta
	case *logquery.ReadResult:
		return &r.Meta
	case *logquery.SummaryResult:
		return &r.Meta
	case *logquery.TraceResult:
		return &r.Meta
	}
	return nil
}

func countOf(v any) int {
	if meta := metaOf(v); meta != nil {
		return meta.ReturnedCount
	}
	return 0
}
