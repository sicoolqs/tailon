package logquery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func benchmarkFile(b *testing.B, lines int) (string, *Service) {
	b.Helper()
	dir := b.TempDir()
	path := filepath.Join(dir, "bench.log")
	var sb strings.Builder
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < lines; i++ {
		level := "INFO"
		msg := "handled request normally"
		if i%50 == 0 {
			level = "ERROR"
			msg = "failed to handle request traceId=tr-bench-0001"
		}
		sb.WriteString(fmt.Sprintf("%s %s %s\n", base.Add(time.Duration(i)*time.Millisecond).Format("2006-01-02 15:04:05.000"), level, msg))
		if i%500 == 0 {
			sb.WriteString("java.lang.RuntimeException: request failed\n")
			sb.WriteString("    at com.example.Service.execute(Service.java:31)\n")
			sb.WriteString("Caused by: java.lang.NullPointerException\n")
			sb.WriteString("    at com.example.Repository.query(Repository.java:18)\n")
		}
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		b.Fatal(err)
	}
	cfg := Config{Timezone: "UTC", DefaultEncoding: "UTF-8", SupportGzip: true, Limits: DefaultLimits()}
	cfg.Limits.QueryTimeout = 30 * time.Second
	svc, err := NewService(func() []Spec {
		return []Spec{{Path: path, Type: "file", Group: "app"}}
	}, cfg)
	if err != nil {
		b.Fatal(err)
	}
	return path, svc
}

func BenchmarkReverseRead(b *testing.B) {
	path, _ := benchmarkFile(b, 50_000)
	f, err := os.Open(path)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	info, _ := f.Stat()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		budget := newScanBudget(1<<20, time.Minute)
		r := newReverseReader(context.Background(), f, info.Size(), newTextDecoder("UTF-8"), 64<<10, 64<<10, budget)
		for j := 0; j < 200; j++ {
			if _, err := r.Next(); err != nil {
				break
			}
		}
	}
}

func BenchmarkLiteralSearch(b *testing.B) {
	_, svc := benchmarkFile(b, 50_000)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := svc.Search(ctx, SearchRequest{Group: "app", Query: "NullPointerException", Limit: 50}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRegexSearch(b *testing.B) {
	_, svc := benchmarkFile(b, 50_000)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := svc.Search(ctx, SearchRequest{Group: "app", Query: `NullPointerException|RuntimeException`, MatchMode: "regex", Limit: 50}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMultilineParser(b *testing.B) {
	lines := []Line{
		{Text: "2026-11-21 10:25:31.125 ERROR Failed to process request", Start: 0, End: 58},
		{Text: "java.lang.RuntimeException: Request failed", Start: 58, End: 102},
		{Text: "    at com.example.Service.execute(Service.java:31)", Start: 102, End: 154},
		{Text: "    at com.example.Controller.handle(Controller.java:21)", Start: 154, End: 208},
		{Text: "Caused by: java.lang.NullPointerException", Start: 208, End: 255},
		{Text: "    at com.example.Repository.query(Repository.java:18)", Start: 255, End: 309},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ev := newEvent(lines, time.UTC)
		if ev.Exception == "" || len(ev.Stack) == 0 {
			b.Fatal("event was not parsed")
		}
	}
}

func BenchmarkLogSummary(b *testing.B) {
	_, svc := benchmarkFile(b, 50_000)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := svc.Summary(ctx, SummaryRequest{Group: "app", Since: "2h", Bucket: "5m", TopN: 10}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTraceSearch(b *testing.B) {
	_, svc := benchmarkFile(b, 50_000)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := svc.Trace(ctx, TraceRequest{TraceID: "tr-bench-0001", Since: "2h", Limit: 100}); err != nil {
			b.Fatal(err)
		}
	}
}
