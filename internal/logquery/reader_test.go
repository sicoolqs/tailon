package logquery

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTempFile(t *testing.T, name, content string) (*os.File, int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open temp file: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	info, err := f.Stat()
	if err != nil {
		t.Fatalf("stat temp file: %v", err)
	}
	return f, info.Size()
}

func collectReverse(t *testing.T, f *os.File, size int64, chunk, maxLine int, budget *scanBudget) []Line {
	t.Helper()
	r := newReverseReader(context.Background(), f, size, newTextDecoder("UTF-8"), chunk, maxLine, budget)
	var lines []Line
	for {
		line, err := r.Next()
		if err == io.EOF {
			return lines
		}
		if err != nil {
			t.Fatalf("reverse read: %v", err)
		}
		lines = append(lines, line)
	}
}

func TestReverseReader(t *testing.T) {
	cases := []struct {
		name      string
		content   string
		want      []string
		wantStart []int64
	}{
		{"trailing newline", "a\nbb\nccc\n", []string{"ccc", "bb", "a"}, []int64{5, 2, 0}},
		{"no trailing newline", "a\nbb\nccc", []string{"ccc", "bb", "a"}, []int64{5, 2, 0}},
		{"crlf", "one\r\ntwo\r\n", []string{"two", "one"}, []int64{5, 0}},
		{"single line", "single", []string{"single"}, []int64{0}},
		{"utf8", "第一行\n第二行\n", []string{"第二行", "第一行"}, []int64{10, 0}},
		{"empty", "", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, size := writeTempFile(t, "log.txt", tc.content)
			budget := newScanBudget(1<<20, time.Minute)
			lines := collectReverse(t, f, size, 3, 1<<16, budget)
			if len(lines) != len(tc.want) {
				t.Fatalf("got %d lines %#v, want %d", len(lines), lines, len(tc.want))
			}
			for i, want := range tc.want {
				if lines[i].Text != want {
					t.Errorf("line %d = %q, want %q", i, lines[i].Text, want)
				}
				if lines[i].Start != tc.wantStart[i] {
					t.Errorf("line %d start = %d, want %d", i, lines[i].Start, tc.wantStart[i])
				}
			}
		})
	}
}

func TestForwardReaderCrossBuffer(t *testing.T) {
	content := "alpha\nbeta\ngamma\n"
	f, size := writeTempFile(t, "log.txt", content)
	budget := newScanBudget(1<<20, time.Minute)
	r := newForwardReader(context.Background(), f, 0, size, newTextDecoder("UTF-8"), 2, 1<<16, budget)

	var got []string
	for {
		line, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("forward read: %v", err)
		}
		got = append(got, line.Text)
	}
	want := []string{"alpha", "beta", "gamma"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestForwardReaderLongLine(t *testing.T) {
	content := "0123456789ABCDEF\nok\n"
	f, size := writeTempFile(t, "log.txt", content)
	budget := newScanBudget(1<<20, time.Minute)
	r := newForwardReader(context.Background(), f, 0, size, newTextDecoder("UTF-8"), 4, 8, budget)

	first, err := r.Next()
	if err != nil {
		t.Fatalf("first line: %v", err)
	}
	if !first.Truncated || first.Text != "01234567" {
		t.Fatalf("long line = %#v", first)
	}
	second, err := r.Next()
	if err != nil {
		t.Fatalf("second line: %v", err)
	}
	if second.Text != "ok" {
		t.Fatalf("second line = %q", second.Text)
	}
}

func TestReverseReaderLongLine(t *testing.T) {
	content := "0123456789ABCDEF\nok\n"
	f, size := writeTempFile(t, "log.txt", content)
	budget := newScanBudget(1<<20, time.Minute)
	lines := collectReverse(t, f, size, 4, 8, budget)
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 lines, got %#v", lines)
	}
	if lines[0].Text != "ok" {
		t.Fatalf("first reverse line = %q", lines[0].Text)
	}
	if !lines[1].Truncated {
		t.Fatalf("expected truncated overlong line, got %#v", lines[1])
	}
	if !strings.HasSuffix(lines[1].Text, "89ABCDEF") {
		t.Fatalf("truncated text = %q", lines[1].Text)
	}
}

func TestReaderBudget(t *testing.T) {
	content := strings.Repeat("abcdefgh\n", 100)
	f, size := writeTempFile(t, "log.txt", content)
	budget := newScanBudget(32, time.Minute)
	r := newForwardReader(context.Background(), f, 0, size, newTextDecoder("UTF-8"), 16, 1<<16, budget)
	for {
		_, err := r.Next()
		if err == nil {
			continue
		}
		if err != errBudgetExceeded {
			t.Fatalf("expected budget error, got %v", err)
		}
		return
	}
}

func TestGBKDecoding(t *testing.T) {
	// "中文" encoded as GBK: D6 D0 CE C4
	content := []byte{0xD6, 0xD0, 0xCE, 0xC4, '\n'}
	path := filepath.Join(t.TempDir(), "gbk.log")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, _ := f.Stat()

	budget := newScanBudget(1<<20, time.Minute)
	r := newForwardReader(context.Background(), f, 0, info.Size(), newTextDecoder("GBK"), 16, 1<<16, budget)
	line, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if line.Text != "中文" {
		t.Fatalf("decoded GBK = %q, want 中文", line.Text)
	}
}
