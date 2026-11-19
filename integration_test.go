package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func requireTail(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("tail")
	if err != nil {
		t.Skip("tail is not available on this host")
	}
	return path
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open file for append: %v", err)
	}
	if _, err := file.WriteString(content); err != nil {
		file.Close()
		t.Fatalf("append file: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close appended file: %v", err)
	}
}

func waitForOutputLine(t *testing.T, session *fakeSession, contains string, timeout time.Duration) {
	t.Helper()
	session.waitFor(t, timeout, "output line containing "+contains, func(messages []string) bool {
		for _, msg := range messages {
			var pair []string
			if err := json.Unmarshal([]byte(msg), &pair); err != nil || len(pair) != 2 {
				continue
			}
			if pair[0] == "o" && strings.Contains(pair[1], contains) {
				return true
			}
		}
		return false
	})
}

// TestTailFollowAppendAndRotation exercises the production tail -F command
// against real appends and log rotation. It is skipped on hosts without tail.
func TestTailFollowAppendAndRotation(t *testing.T) {
	tailPath := requireTail(t)
	dir := t.TempDir()
	path := writeTestFile(t, dir, "app.log", "one\ntwo\n")
	installTestConfig(t, []string{path}, map[string]CommandSpec{
		"tail": {Action: []string{tailPath, "-n", "2", "-F", "$path"}},
	}, []string{"tail"})

	session := newFakeSession()
	done := startSession(t, session)
	session.push(t, jsonCommand(t, "tail", path, "", 2))
	waitForOutputLine(t, session, "two", 10*time.Second)

	appendFile(t, path, "three\n")
	waitForOutputLine(t, session, "three", 10*time.Second)

	rotated := path + ".1"
	if err := os.Rename(path, rotated); err != nil {
		t.Fatalf("rotate log: %v", err)
	}
	if err := os.WriteFile(path, []byte("four\nfive\n"), 0o600); err != nil {
		t.Fatalf("create replacement log: %v", err)
	}
	waitForOutputLine(t, session, "five", 20*time.Second)

	session.cancel()
	waitSessionDone(t, done)
}

// TestLargeFileStreamingIntegration is an opt-in test for the 500 MB-class
// workload. Run it with:
//
//	TAILON_LARGE_TEST=1 go test -run TestLargeFileStreamingIntegration -v .
//
// TAILON_LARGE_TEST_SIZE_MB can override the default 64 MB.
func TestLargeFileStreamingIntegration(t *testing.T) {
	if os.Getenv("TAILON_LARGE_TEST") != "1" {
		t.Skip("set TAILON_LARGE_TEST=1 to run the large-file integration test")
	}
	tailPath := requireTail(t)

	sizeMB := 64
	if raw := os.Getenv("TAILON_LARGE_TEST_SIZE_MB"); raw != "" {
		if _, err := fmt.Sscanf(raw, "%d", &sizeMB); err != nil || sizeMB <= 0 {
			t.Fatalf("invalid TAILON_LARGE_TEST_SIZE_MB=%q", raw)
		}
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "large.log")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create large log: %v", err)
	}
	line := bytes.Repeat([]byte("x"), 1023)
	line = append(line, '\n')
	const linesPerChunk = 1024
	chunk := bytes.Repeat(line, linesPerChunk)
	for written := 0; written < sizeMB; written++ {
		if _, err := file.Write(chunk); err != nil {
			file.Close()
			t.Fatalf("write large log: %v", err)
		}
	}
	if _, err := file.WriteString("last-marker\n"); err != nil {
		file.Close()
		t.Fatalf("write final marker: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close large log: %v", err)
	}

	installTestConfig(t, []string{path}, map[string]CommandSpec{
		"tail": {Action: []string{tailPath, "-n", "2", "-F", "$path"}},
	}, []string{"tail"})

	session := newFakeSession()
	done := startSession(t, session)
	started := time.Now()
	session.push(t, jsonCommand(t, "tail", path, "", 2))
	waitForOutputLine(t, session, "last-marker", 30*time.Second)
	t.Logf("received output from a %d MB log in %s", sizeMB, time.Since(started))

	session.cancel()
	waitSessionDone(t, done)
}

// TestSoakManyFilesAndSessions is an opt-in stress test that drives multiple
// clients through 20 logs while the logs are appended to and rotated.
// Run it with:
//
//	TAILON_SOAK=1 TAILON_SOAK_SECONDS=10 go test -run TestSoakManyFilesAndSessions -v .
func TestSoakManyFilesAndSessions(t *testing.T) {
	if os.Getenv("TAILON_SOAK") != "1" {
		t.Skip("set TAILON_SOAK=1 to run the soak test")
	}

	duration := 5 * time.Second
	if raw := os.Getenv("TAILON_SOAK_SECONDS"); raw != "" {
		seconds, err := time.ParseDuration(raw + "s")
		if err != nil || seconds <= 0 {
			t.Fatalf("invalid TAILON_SOAK_SECONDS=%q", raw)
		}
		duration = seconds
	}

	runtime.GC()
	baseline := runtime.NumGoroutine()

	dir := t.TempDir()
	paths := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		paths = append(paths, writeTestFile(t, dir, fmt.Sprintf("app-%02d.log", i), "initial\n"))
	}
	installTestConfig(t, paths, map[string]CommandSpec{
		"cat":  {Action: append(helperAction("cat"), "$path")},
		"tail": {Action: helperAction("stream")},
		"grep": {Stdin: "tail", Action: helperAction("passthrough"), Default: "line"},
	}, []string{"cat", "tail", "grep"})

	stopWriters := make(chan struct{})
	var writerWG sync.WaitGroup
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		n := 0
		for {
			select {
			case <-stopWriters:
				return
			case <-ticker.C:
				path := paths[n%len(paths)]
				_ = appendFileNoFail(path, fmt.Sprintf("line-%d\n", n))
				if n%200 == 0 && n > 0 {
					_ = os.Rename(path, path+".1")
					_ = os.WriteFile(path, []byte("rotated\n"), 0o600)
				}
				n++
			}
		}
	}()
	defer func() {
		close(stopWriters)
		writerWG.Wait()
	}()

	deadline := time.Now().Add(duration)
	const clients = 6
	var clientsWG sync.WaitGroup
	errs := make(chan error, clients)
	for c := 0; c < clients; c++ {
		clientsWG.Add(1)
		go func(id int) {
			defer clientsWG.Done()
			session := newFakeSession()
			done := make(chan struct{})
			go func() {
				defer close(done)
				serveSession(session)
			}()
			push := func(msg string) bool {
				select {
				case session.recv <- msg:
					return true
				case <-session.Context().Done():
					return false
				case <-time.After(2 * time.Second):
					errs <- errors.New("timed out queueing soak message")
					return false
				}
			}

			push("list")
			n := id
			for time.Now().Before(deadline) {
				path := paths[n%len(paths)]
				var msg string
				switch n % 5 {
				case 0, 1:
					msg = jsonCommandNoFail("cat", path, "", 10)
				case 2, 3:
					msg = jsonCommandNoFail("grep", path, "line", 10)
				default:
					msg = jsonCommandNoFail("tail", path, "", 10)
				}
				if !push(msg) {
					break
				}
				n++
				time.Sleep(10 * time.Millisecond)
			}
			session.cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				errs <- errors.New("soak session did not stop")
			}
		}(c)
	}
	clientsWG.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	got := settleGoroutines(baseline+8, 10*time.Second)
	if got > baseline+8 {
		t.Fatalf("goroutine count grew from %d to %d after soak", baseline, got)
	}
	t.Logf("soak complete: %d files, %d clients, %s", len(paths), clients, duration)
}

func appendFileNoFail(path, content string) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.WriteString(content)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func jsonCommandNoFail(command, path, script string, nlines int) string {
	data, _ := json.Marshal(FrontendCommand{
		Command: command,
		Script:  script,
		Entry:   ListEntry{Path: path},
		Nlines:  nlines,
	})
	return string(data)
}
