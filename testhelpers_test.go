package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/igm/sockjs-go/v3/sockjs"
)

// TestHelperProcess is not a normal test. It is re-executed as a child process
// so lifecycle tests can use deterministic commands on every platform.
func TestHelperProcess(t *testing.T) {
	mode, args, ok := helperInvocation()
	if !ok {
		return
	}
	runHelper(mode, args)
	os.Exit(0)
}

func helperInvocation() (string, []string, bool) {
	for i, arg := range os.Args {
		if arg == "tailon-test-helper" && i+1 < len(os.Args) {
			return os.Args[i+1], os.Args[i+2:], true
		}
	}
	return "", nil, false
}

func helperAction(mode string, extra ...string) []string {
	args := []string{os.Args[0], "-test.run=^TestHelperProcess$", "--", "tailon-test-helper", mode}
	return append(args, extra...)
}

func runHelper(mode string, args []string) {
	switch mode {
	case "stream":
		for i := 0; ; i++ {
			fmt.Printf("line-%d\n", i)
			time.Sleep(2 * time.Millisecond)
		}
	case "burst":
		for i := 0; i < 5000; i++ {
			fmt.Printf("burst-%d\n", i)
		}
		time.Sleep(10 * time.Minute)
	case "passthrough":
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			fmt.Printf("grep:%s\n", scanner.Text())
		}
	case "cat":
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "cat helper requires a path")
			os.Exit(2)
		}
		file, err := os.Open(args[0])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		defer file.Close()
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			fmt.Println(scanner.Text())
		}
	case "exit0":
		fmt.Println("helper-done")
	case "exit3":
		fmt.Fprintln(os.Stderr, "helper-failed")
		os.Exit(3)
	case "sleep":
		time.Sleep(10 * time.Minute)
	case "pidfile":
		if len(args) == 0 {
			os.Exit(2)
		}
		if err := os.WriteFile(args[0], []byte(fmt.Sprintf("%d", os.Getpid())), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		time.Sleep(10 * time.Minute)
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		os.Exit(2)
	}
}

// fakeSession implements wsSession without a network connection. It records
// everything sent by the server and can simulate slow or broken clients.
type fakeSession struct {
	ctx    context.Context
	cancel context.CancelFunc
	recv   chan string

	mu       sync.Mutex
	sent     []string
	sendHook func(string) error
}

func newFakeSession() *fakeSession {
	ctx, cancel := context.WithCancel(context.Background())
	return &fakeSession{
		ctx:    ctx,
		cancel: cancel,
		recv:   make(chan string, 64),
	}
}

func (f *fakeSession) Send(msg string) error {
	f.mu.Lock()
	hook := f.sendHook
	f.mu.Unlock()
	if hook != nil {
		if err := hook(msg); err != nil {
			return err
		}
	}

	f.mu.Lock()
	f.sent = append(f.sent, msg)
	f.mu.Unlock()
	return nil
}

func (f *fakeSession) Recv() (string, error) {
	select {
	case msg := <-f.recv:
		return msg, nil
	case <-f.ctx.Done():
		return "", sockjs.ErrSessionNotOpen
	}
}

func (f *fakeSession) Context() context.Context {
	return f.ctx
}

func (f *fakeSession) Close(status uint32, reason string) error {
	f.cancel()
	return nil
}

func (f *fakeSession) setSendHook(hook func(string) error) {
	f.mu.Lock()
	f.sendHook = hook
	f.mu.Unlock()
}

func (f *fakeSession) push(t *testing.T, msg string) {
	t.Helper()
	select {
	case f.recv <- msg:
	case <-f.ctx.Done():
		t.Fatalf("session closed before message could be queued")
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out queueing session message")
	}
}

func (f *fakeSession) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.sent))
	copy(out, f.sent)
	return out
}

func (f *fakeSession) waitFor(t *testing.T, timeout time.Duration, what string, predicate func([]string) bool) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		messages := f.messages()
		if predicate(messages) {
			return messages
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; received %d messages: %v", what, len(messages), messages)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForListing(t *testing.T, session *fakeSession) map[string][]*ListEntry {
	t.Helper()
	var listing map[string][]*ListEntry
	session.waitFor(t, 3*time.Second, "file listing", func(messages []string) bool {
		for _, msg := range messages {
			if len(msg) == 0 || msg[0] != '{' {
				continue
			}
			var candidate map[string][]*ListEntry
			if err := json.Unmarshal([]byte(msg), &candidate); err == nil {
				listing = candidate
				return true
			}
		}
		return false
	})
	return listing
}

func waitForLine(t *testing.T, session *fakeSession, stream string, contains string) {
	t.Helper()
	session.waitFor(t, 5*time.Second, fmt.Sprintf("%s line containing %q", stream, contains), func(messages []string) bool {
		for _, msg := range messages {
			var pair []string
			if err := json.Unmarshal([]byte(msg), &pair); err != nil || len(pair) != 2 {
				continue
			}
			if pair[0] == stream && strings.Contains(pair[1], contains) {
				return true
			}
		}
		return false
	})
}

func startSession(t *testing.T, session *fakeSession) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveSession(session)
	}()
	t.Cleanup(func() {
		session.cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("session did not stop after cancellation")
		}
	})
	return done
}

func waitSessionDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("session did not stop within timeout")
	}
}

func installTestConfig(t *testing.T, paths []string, commands map[string]CommandSpec, allow []string) *Config {
	t.Helper()
	oldConfig := config

	specs := make([]FileSpec, 0, len(paths))
	for _, path := range paths {
		specs = append(specs, FileSpec{Path: path, Type: "file"})
	}

	config = &Config{
		RelativeRoot:      "/",
		CommandSpecs:      commands,
		AllowCommandNames: allow,
		CommandScripts:    map[string]string{},
		FileSpecs:         specs,
	}
	createListing(specs)

	t.Cleanup(func() {
		config = oldConfig
		createListing(oldConfig.FileSpecs)
	})
	return config
}

func jsonCommand(t *testing.T, command, path, script string, nlines int) string {
	t.Helper()
	msg := FrontendCommand{
		Command: command,
		Script:  script,
		Entry:   ListEntry{Path: path},
		Nlines:  nlines,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal frontend command: %v", err)
	}
	return string(data)
}

func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := dir + string(os.PathSeparator) + name
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	return path
}

func settleGoroutines(target int, timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	count := runtime.NumGoroutine()
	for count > target && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		count = runtime.NumGoroutine()
	}
	return count
}

var errSimulatedWriteFailure = errors.New("simulated write failure")
