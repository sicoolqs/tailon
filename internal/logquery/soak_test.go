package logquery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestLargeFileTailBounded verifies that tailing a large file reads only the
// tail, not the whole file. Enabled with TAILON_LARGE_TEST=1.
func TestLargeFileTailBounded(t *testing.T) {
	if os.Getenv("TAILON_LARGE_TEST") == "" {
		t.Skip("set TAILON_LARGE_TEST=1 to run the large-file test")
	}
	sizeMB := 64
	if v := os.Getenv("TAILON_LARGE_TEST_SIZE_MB"); v != "" {
		fmt.Sscanf(v, "%d", &sizeMB)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "large.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	line := "2026-11-21 10:00:00 INFO a representative log line with some payload\n"
	lineBytes := int64(len(line))
	target := int64(sizeMB) << 20
	written := int64(0)
	for written < target {
		if _, err := f.WriteString(line); err != nil {
			t.Fatal(err)
		}
		written += lineBytes
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	cfg := Config{Timezone: "UTC", DefaultEncoding: "UTF-8", SupportGzip: true, Limits: DefaultLimits()}
	svc, err := NewService(func() []Spec {
		return []Spec{{Path: path, Type: "file", Group: "app"}}
	}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res, err := svc.Tail(context.Background(), TailRequest{Group: "app", Lines: 200})
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if len(res.Entries) != 200 {
		t.Fatalf("entries = %d", len(res.Entries))
	}
	const maxScanned = 4 << 20
	if res.Meta.ScannedBytes > maxScanned {
		t.Fatalf("tail scanned %d bytes of a %d MiB file; expected under %d", res.Meta.ScannedBytes, sizeMB, maxScanned)
	}
	t.Logf("tail on %d MiB file: entries=%d scanned_bytes=%d duration=%s", sizeMB, len(res.Entries), res.Meta.ScannedBytes, elapsed)
}

// TestSoakToolCalls exercises repeated queries and checks for goroutine
// growth. Enabled with TAILON_SOAK=1.
func TestSoakToolCalls(t *testing.T) {
	if os.Getenv("TAILON_SOAK") == "" {
		t.Skip("set TAILON_SOAK=1 to run the soak test")
	}
	iterations := 1000
	if v := os.Getenv("TAILON_SOAK_CALLS"); v != "" {
		fmt.Sscanf(v, "%d", &iterations)
	}
	env := newTestEnv(t)
	env.svc.limits.QueryTimeout = time.Minute
	ctx := context.Background()

	// Warm up lazily initialized runtime machinery.
	for i := 0; i < 10; i++ {
		_, _ = env.svc.List(ctx, ListRequest{Limit: 10})
	}
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	for i := 0; i < iterations; i++ {
		if _, err := env.svc.List(ctx, ListRequest{Limit: 10}); err != nil {
			t.Fatal(err)
		}
		if _, err := env.svc.Search(ctx, SearchRequest{Group: "app", Query: "ERROR", Limit: 5}); err != nil {
			t.Fatal(err)
		}
		if _, err := env.svc.Tail(ctx, TailRequest{Group: "app", Lines: 5}); err != nil {
			t.Fatal(err)
		}
	}
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()
	if after > before+10 {
		t.Fatalf("goroutines grew from %d to %d after %d iterations", before, after, iterations)
	}
	t.Logf("soak: iterations=%d goroutines_before=%d goroutines_after=%d", iterations, before, after)
}
