package logquery

import (
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeGzipFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	if _, err := gz.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGzipTailSearchAndStat(t *testing.T) {
	dir := t.TempDir()
	content := "2026-11-21 10:00:00 INFO startup\n" +
		"2026-11-21 10:01:00 ERROR failed request\n" +
		"2026-11-21 10:02:00 INFO done\n"
	path := writeGzipFile(t, dir, "app.log.gz", content)

	cfg := Config{Timezone: "UTC", DefaultEncoding: "UTF-8", SupportGzip: true, Limits: DefaultLimits()}
	svc, err := NewService(func() []Spec {
		return []Spec{{Path: path, Type: "file", Group: "app"}}
	}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	list, err := svc.List(ctx, ListRequest{Limit: 10})
	if err != nil || len(list.Files) != 1 {
		t.Fatalf("list = %#v, err = %v", list, err)
	}
	fileID := list.Files[0].FileID

	stat, err := svc.Stat(ctx, StatRequest{FileID: fileID})
	if err != nil {
		t.Fatal(err)
	}
	if !stat.Compressed || stat.Seekable {
		t.Fatalf("stat = %#v", stat)
	}

	tail, err := svc.Tail(ctx, TailRequest{FileID: fileID, Lines: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(tail.Entries) != 1 || !strings.Contains(tail.Entries[0].Message, "done") {
		t.Fatalf("tail = %#v", tail.Entries)
	}

	search, err := svc.Search(ctx, SearchRequest{FileID: fileID, Query: "ERROR", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(search.Results) != 1 {
		t.Fatalf("search = %#v", search.Results)
	}

	read, err := svc.Read(ctx, ReadRequest{FileID: fileID, Lines: 1, Direction: "forward"})
	if err != nil || read.ReturnedLines != 1 {
		t.Fatalf("read = %#v, err = %v", read, err)
	}
	offset := int64(10)
	if _, err := svc.Read(ctx, ReadRequest{FileID: fileID, Offset: &offset}); CodeOf(err) != CodeUnsupportedType {
		t.Fatalf("offset read error = %v", err)
	}
	if _, err := svc.Context(ctx, ContextRequest{FileID: fileID, ByteOffset: &offset}); CodeOf(err) != CodeUnsupportedType {
		t.Fatalf("context error = %v", err)
	}
}

func TestCursorExpiry(t *testing.T) {
	codec := NewCursorCodec(time.Second)
	now := time.Unix(1_700_000_000, 0)
	codec.now = func() time.Time { return now }

	token := codec.Encode(cursorPayload{Tool: "logs_read", Query: "q", Files: []CursorFile{{ID: "f", Size: 1}}})
	if _, err := codec.Decode(token); err != nil {
		t.Fatalf("fresh cursor should decode: %v", err)
	}
	now = now.Add(2 * time.Second)
	if _, err := codec.Decode(token); CodeOf(err) != CodeCursorExpired {
		t.Fatalf("expired cursor error = %v", err)
	}
}

func TestConcurrentQueryLimit(t *testing.T) {
	cfg := Config{Timezone: "UTC", DefaultEncoding: "UTF-8", SupportGzip: true, Limits: DefaultLimits()}
	cfg.Limits.MaxConcurrentQueries = 1
	svc, err := NewService(func() []Spec { return nil }, cfg)
	if err != nil {
		t.Fatal(err)
	}
	first, err := svc.begin(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.begin(context.Background(), "test"); CodeOf(err) != CodeServerBusy {
		t.Fatalf("second query error = %v", err)
	}
	first.finish(nil, nil)

	second, err := svc.begin(context.Background(), "test")
	if err != nil {
		t.Fatalf("slot was not released: %v", err)
	}
	second.finish(nil, nil)
}

func TestScanBudgetTruncation(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 0; i < 5000; i++ {
		b.WriteString("2026-11-21 10:00:00 INFO a fairly long log line with some payload data\n")
	}
	path := filepath.Join(dir, "big.log")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Timezone: "UTC", DefaultEncoding: "UTF-8", SupportGzip: true, Limits: DefaultLimits()}
	cfg.Limits.MaxScanBytes = 1024
	svc, err := NewService(func() []Spec {
		return []Spec{{Path: path, Type: "file", Group: "app"}}
	}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.Search(context.Background(), SearchRequest{Query: "payload", Limit: 500})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !res.Meta.Truncated || res.Meta.TruncationReason != TruncationScanLimit {
		t.Fatalf("meta = %#v", res.Meta)
	}
}

func TestTraceBracketMDCFormat(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().UTC().Add(-10 * time.Minute)
	lines := []string{
		base.Format("2006-01-02T15:04:05.000Z07:00") + " INFO  [thread-1] [abcdef123456-0123456789abcdef] received",
		base.Add(time.Second).Format("2006-01-02T15:04:05.000Z07:00") + " ERROR [thread-1] [abcdef123456-0123456789abcdef] failed",
		base.Add(2*time.Second).Format("2006-01-02T15:04:05.000Z07:00") + " INFO  [thread-2] [ffffffffffff-1111111111111111] other",
	}
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Timezone: "UTC", DefaultEncoding: "UTF-8", SupportGzip: true, Limits: DefaultLimits()}
	svc, err := NewService(func() []Spec {
		return []Spec{{Path: path, Type: "file", Group: "app"}}
	}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.Trace(context.Background(), TraceRequest{TraceID: "abcdef123456", Since: "1h", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.EventCount != 2 {
		t.Fatalf("event count = %d (%#v)", res.EventCount, res.Timeline)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("errors = %#v", res.Errors)
	}
	if res.Timeline[0].SpanID == nil || *res.Timeline[0].SpanID != "0123456789abcdef" {
		t.Fatalf("span id = %v", res.Timeline[0].SpanID)
	}
}

func TestGlobDiscoveryAndRotation(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.log")
	if err := os.WriteFile(pathA, []byte("2026-11-21 10:00:00 INFO first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Timezone: "UTC", DefaultEncoding: "UTF-8", SupportGzip: true, Limits: DefaultLimits()}
	svc, err := NewService(func() []Spec {
		return []Spec{{Path: filepath.Join(dir, "*.log"), Type: "glob", Group: "app"}}
	}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	first, err := svc.List(ctx, ListRequest{Limit: 10})
	if err != nil || len(first.Files) != 1 {
		t.Fatalf("initial list = %#v, err = %v", first, err)
	}
	oldID := first.Files[0].FileID
	oldSize := first.Files[0].SizeBytes

	pathB := filepath.Join(dir, "b.log")
	if err := os.WriteFile(pathB, []byte("2026-11-21 10:01:00 INFO second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := svc.List(ctx, ListRequest{Limit: 10})
	if err != nil || len(second.Files) != 2 {
		t.Fatalf("list after new file = %#v, err = %v", second, err)
	}

	// Rotate a.log: the path and file_id stay stable, but the contents change.
	if err := os.Rename(pathA, filepath.Join(dir, "a.log.1")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathA, []byte("2026-11-21 10:02:00 INFO rotated file has new content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	third, err := svc.List(ctx, ListRequest{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var newSize int64
	found := false
	for _, f := range third.Files {
		if f.FileID == oldID {
			found = true
			newSize = f.SizeBytes
		}
	}
	if !found {
		t.Fatal("file_id changed after rotation")
	}
	if newSize == oldSize {
		t.Fatal("expected the rotated file to have a different size")
	}
}

func TestReadCursorDetectsTruncation(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	list, err := env.svc.List(ctx, ListRequest{Group: "app", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	id := list.Files[0].FileID
	read, err := env.svc.Read(ctx, ReadRequest{FileID: id, Lines: 1, Direction: "forward", IncludeStacktrace: boolPtr(false)})
	if err != nil || read.NextCursor == nil {
		t.Fatalf("read = %#v, err = %v", read, err)
	}
	if err := os.WriteFile(env.appPath, []byte("short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.Read(ctx, ReadRequest{FileID: id, Lines: 1, Direction: "forward", Cursor: *read.NextCursor, IncludeStacktrace: boolPtr(false)}); CodeOf(err) != CodeFileChanged && CodeOf(err) != CodeFileRotated {
		t.Fatalf("truncation error = %v", err)
	}
}

func TestCanceledContext(t *testing.T) {
	env := newTestEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := env.svc.Search(ctx, SearchRequest{Query: "INFO"}); err == nil {
		t.Fatal("expected a cancellation error")
	}
}
