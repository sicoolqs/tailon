package logquery

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testEnv struct {
	svc      *Service
	dir      string
	appPath  string
	authPath string
}

func ts(base time.Time, offset time.Duration, level, msg string) string {
	return base.Add(offset).Format("2006-01-02 15:04:05.000") + " " + level + " " + msg
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	base := time.Now().UTC().Add(-20 * time.Minute)

	appLines := []string{
		ts(base, 0, "INFO", "Received request traceId=tr-123456"),
		ts(base, time.Minute, "ERROR", "Failed to process request traceId=tr-123456"),
		"java.lang.NullPointerException: request failed",
		"    at com.example.Service.execute(Service.java:31)",
		"    at com.example.Controller.handle(Controller.java:21)",
		ts(base, 2*time.Minute, "WARN", "slow downstream call"),
		ts(base, 3*time.Minute, "INFO", "done"),
		ts(base, -2*time.Hour, "ERROR", "ancient failure"),
	}
	authLines := []string{
		ts(base, 30*time.Second, "WARN", "slow auth traceId=tr-123456"),
		ts(base, 90*time.Second, "ERROR", "Failed to authenticate request traceId=tr-123456"),
		ts(base, 2*time.Minute, "INFO", "auth done"),
	}
	appPath := filepath.Join(dir, "app.log")
	authPath := filepath.Join(dir, "auth.log")
	if err := os.WriteFile(appPath, []byte(strings.Join(appLines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authPath, []byte(strings.Join(authLines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		Timezone:        "UTC",
		DefaultEncoding: "UTF-8",
		SupportGzip:     true,
		Limits:          DefaultLimits(),
	}
	cfg.Limits.QueryTimeout = 5 * time.Second
	specs := []Spec{
		{Path: appPath, Type: "file", Group: "app"},
		{Path: authPath, Type: "file", Group: "auth"},
	}
	svc, err := NewService(func() []Spec { return specs }, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &testEnv{svc: svc, dir: dir, appPath: appPath, authPath: authPath}
}

func TestListAndStat(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	list, err := env.svc.List(ctx, ListRequest{IncludeFiles: boolPtr(true), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Groups) != 2 || len(list.Files) != 2 {
		t.Fatalf("list = %#v", list)
	}
	var appID string
	for _, f := range list.Files {
		if f.Group == "app" {
			appID = f.FileID
		}
	}
	if appID == "" {
		t.Fatal("app file id not found")
	}

	again, err := env.svc.List(ctx, ListRequest{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range again.Files {
		if f.FileID == appID {
			found = true
		}
	}
	if !found {
		t.Fatal("file id is not stable across listings")
	}

	stat, err := env.svc.Stat(ctx, StatRequest{FileID: appID})
	if err != nil {
		t.Fatal(err)
	}
	if stat.SizeBytes <= 0 || !stat.Readable || stat.Compressed {
		t.Fatalf("stat = %#v", stat)
	}
	if stat.SampledTime == nil {
		t.Fatal("expected sampled time range")
	}
}

func TestSearchLiteralAndCursor(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	first, err := env.svc.Search(ctx, SearchRequest{Group: "app", Query: "ERROR", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Results) != 1 || !first.Meta.HasMore || first.Meta.NextCursor == nil {
		t.Fatalf("first page = %#v", first)
	}

	second, err := env.svc.Search(ctx, SearchRequest{
		Group: "app", Query: "ERROR", Limit: 10, Cursor: *first.Meta.NextCursor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Results) == 0 {
		t.Fatal("expected more results from cursor")
	}
	if second.Results[0].ByteOffset == first.Results[0].ByteOffset {
		t.Fatal("cursor returned a duplicate result")
	}
}

func TestSearchRegexLevelsAndTime(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	regex, err := env.svc.Search(ctx, SearchRequest{Group: "app", Query: `Fail(ed)? .*request`, MatchMode: "regex", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(regex.Results) != 1 {
		t.Fatalf("regex results = %#v", regex.Results)
	}

	levels, err := env.svc.Search(ctx, SearchRequest{Group: "app", Levels: []string{"ERROR"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(levels.Results) != 2 {
		t.Fatalf("level results = %d", len(levels.Results))
	}

	recent, err := env.svc.Search(ctx, SearchRequest{Group: "app", Levels: []string{"ERROR"}, Since: "30m", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(recent.Results) != 1 {
		t.Fatalf("time-filtered results = %d", len(recent.Results))
	}
}

func TestSearchContextAndMultiline(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	res, err := env.svc.Search(ctx, SearchRequest{
		Group: "app", Query: "Failed to process", ContextBefore: 1, ContextAfter: 1,
		IncludeStacktrace: boolPtr(true), Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 1 {
		t.Fatalf("results = %d", len(res.Results))
	}
	hit := res.Results[0]
	if len(hit.ContextBefore) != 1 {
		t.Fatalf("context before = %#v", hit.ContextBefore)
	}
	if len(hit.Stacktrace) < 2 {
		t.Fatalf("stacktrace = %#v", hit.Stacktrace)
	}

	ctxRes, err := env.svc.Context(ctx, ContextRequest{
		FileID: hit.FileID, ByteOffset: &hit.ByteOffset, BeforeLines: 2, AfterLines: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ctxRes.TargetStart > hit.ByteOffset || ctxRes.TargetEnd < hit.EndOffset {
		t.Fatalf("target range = %d..%d, hit = %d..%d", ctxRes.TargetStart, ctxRes.TargetEnd, hit.ByteOffset, hit.EndOffset)
	}
	target := false
	for _, line := range ctxRes.Lines {
		if line.Target && strings.Contains(line.Text, "Failed to process") {
			target = true
		}
	}
	if !target {
		t.Fatalf("target line not marked: %#v", ctxRes.Lines)
	}
}

func TestReadForwardAndBackward(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	list, err := env.svc.List(ctx, ListRequest{Group: "app", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	id := list.Files[0].FileID

	read, err := env.svc.Read(ctx, ReadRequest{FileID: id, Lines: 2, Direction: "forward", IncludeStacktrace: boolPtr(false)})
	if err != nil {
		t.Fatal(err)
	}
	if read.ReturnedLines != 2 || read.NextCursor == nil || read.EOF {
		t.Fatalf("forward read = %#v", read)
	}

	next, err := env.svc.Read(ctx, ReadRequest{FileID: id, Lines: 2, Direction: "forward", Cursor: *read.NextCursor, IncludeStacktrace: boolPtr(false)})
	if err != nil {
		t.Fatal(err)
	}
	if next.StartOffset < read.EndOffset {
		t.Fatalf("pages overlap: first end %d, second start %d", read.EndOffset, next.StartOffset)
	}

	back, err := env.svc.Read(ctx, ReadRequest{FileID: id, Lines: 2, Direction: "backward", IncludeStacktrace: boolPtr(false)})
	if err != nil {
		t.Fatal(err)
	}
	if back.ReturnedLines != 2 || !strings.Contains(back.Content, "done") {
		t.Fatalf("backward read = %#v", back)
	}
}

func TestTail(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	res, err := env.svc.Tail(ctx, TailRequest{Group: "app", Lines: 3, Order: "asc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 3 {
		t.Fatalf("entries = %d", len(res.Entries))
	}
	for i := 1; i < len(res.Entries); i++ {
		prev := res.Entries[i-1].Timestamp
		cur := res.Entries[i].Timestamp
		if prev != nil && cur != nil && *prev > *cur {
			t.Fatalf("ascending order violated: %s > %s", *prev, *cur)
		}
	}

	errors, err := env.svc.Tail(ctx, TailRequest{Group: "app", Lines: 10, Level: "ERROR"})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range errors.Entries {
		if entry.Level == nil || *entry.Level != "ERROR" {
			t.Fatalf("level filter leaked %#v", entry)
		}
	}
	if len(errors.Entries) == 0 {
		t.Fatal("expected error entries")
	}
}

func TestSummary(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	res, err := env.svc.Summary(ctx, SummaryRequest{Group: "app", Since: "1h", TopN: 5, Bucket: "5m"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Levels["ERROR"] == 0 || res.Levels["INFO"] == 0 {
		t.Fatalf("levels = %#v", res.Levels)
	}
	if len(res.TopErrors) == 0 {
		t.Fatal("expected top errors")
	}
	if res.TopErrors[0].Count != 1 {
		t.Fatalf("ancient error should be outside the range: %#v", res.TopErrors[0])
	}
	if len(res.Timeline) == 0 {
		t.Fatal("expected timeline buckets")
	}
}

func TestTrace(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	res, err := env.svc.Trace(ctx, TraceRequest{TraceID: "tr-123456", Since: "1h", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if res.EventCount != 4 {
		t.Fatalf("event count = %d (%#v)", res.EventCount, res.Timeline)
	}
	if len(res.MatchedGroups) != 2 || res.MatchedFiles != 2 {
		t.Fatalf("matched groups/files = %#v / %d", res.MatchedGroups, res.MatchedFiles)
	}
	if len(res.Errors) != 2 {
		t.Fatalf("errors = %#v", res.Errors)
	}
	if res.ObservedDurationMs == nil {
		t.Fatal("expected observed duration")
	}
	for i := 1; i < len(res.Timeline); i++ {
		if res.Timeline[i-1].Timestamp != nil && res.Timeline[i].Timestamp != nil &&
			*res.Timeline[i-1].Timestamp > *res.Timeline[i].Timestamp {
			t.Fatal("timeline is not chronological")
		}
	}
}

func TestInvalidInputs(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	if _, err := env.svc.Stat(ctx, StatRequest{FileID: "f_nope"}); CodeOf(err) != CodeFileNotFound {
		t.Fatalf("stat error = %v", err)
	}
	if _, err := env.svc.Search(ctx, SearchRequest{Query: "", Group: "app"}); CodeOf(err) != CodeInvalidArgument {
		t.Fatalf("empty query error = %v", err)
	}
	if _, err := env.svc.Search(ctx, SearchRequest{Query: "(", MatchMode: "regex"}); CodeOf(err) != CodeInvalidRegex {
		t.Fatalf("invalid regex error = %v", err)
	}
	if _, err := env.svc.Search(ctx, SearchRequest{Query: "x", Since: "1h", StartTime: "2026-01-01T00:00:00Z"}); CodeOf(err) != CodeInvalidArgument {
		t.Fatalf("since+start error = %v", err)
	}
	if _, err := env.svc.Trace(ctx, TraceRequest{TraceID: "ab"}); CodeOf(err) != CodeInvalidArgument {
		t.Fatalf("short trace error = %v", err)
	}
	if _, err := env.svc.List(ctx, ListRequest{Group: "missing"}); CodeOf(err) != CodeGroupNotFound {
		t.Fatalf("group error = %v", err)
	}
}

func TestCursorTampering(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	res, err := env.svc.Search(ctx, SearchRequest{Group: "app", Query: "INFO", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Meta.NextCursor == nil {
		t.Fatal("expected cursor")
	}
	token := *res.Meta.NextCursor
	tampered := token[:len(token)-2] + "aa"
	if _, err := env.svc.Search(ctx, SearchRequest{Group: "app", Query: "INFO", Limit: 1, Cursor: tampered}); CodeOf(err) != CodeInvalidCursor {
		t.Fatalf("tampered cursor error = %v", err)
	}
}

func TestSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	allowed := filepath.Join(dir, "allowed")
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(allowed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret.log")
	if err := os.WriteFile(secret, []byte("top secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(allowed, "link.log")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}

	cfg := Config{Timezone: "UTC", DefaultEncoding: "UTF-8", SupportGzip: true, Limits: DefaultLimits()}
	svc, err := NewService(func() []Spec {
		return []Spec{{Path: filepath.Join(allowed, "*.log"), Type: "glob", Group: "app"}}
	}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	list, err := svc.List(context.Background(), ListRequest{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Files) != 1 {
		t.Fatalf("files = %#v", list.Files)
	}
	if _, err := svc.Stat(context.Background(), StatRequest{FileID: list.Files[0].FileID}); CodeOf(err) != CodeFileAccessDenied {
		t.Fatalf("symlink escape error = %v", err)
	}
}

func TestDirectoryIsRejected(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "notafile.log")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Timezone: "UTC", DefaultEncoding: "UTF-8", SupportGzip: true, Limits: DefaultLimits()}
	svc, err := NewService(func() []Spec {
		return []Spec{{Path: sub, Type: "file", Group: "app"}}
	}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	list, err := svc.List(context.Background(), ListRequest{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Files) != 1 {
		t.Fatalf("files = %#v", list.Files)
	}
	if _, err := svc.Stat(context.Background(), StatRequest{FileID: list.Files[0].FileID}); CodeOf(err) != CodeUnsupportedType {
		t.Fatalf("directory error = %v", err)
	}
}

func boolPtr(v bool) *bool { return &v }
