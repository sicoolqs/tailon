package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestExpandCommandArgs(t *testing.T) {
	frontend := FrontendCommand{
		Script: "error|warn",
		Entry:  ListEntry{Path: "/var/log/app.log"},
		Nlines: 25,
	}
	expanded := expandCommandArgs([]string{"tail", "-n", "$lines", "-F", "$path", "$script"}, frontend)
	want := []string{"tail", "-n", "25", "-F", "/var/log/app.log", "error|warn"}
	if strings.Join(expanded, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("expanded args = %#v, want %#v", expanded, want)
	}
}

func TestPrepareCommandValidation(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "app.log", "one\n")
	commands := map[string]CommandSpec{
		"tail":    {Action: []string{"tail", "-n", "$lines", "-F", "$path"}},
		"grep":    {Stdin: "tail", Action: []string{"grep", "-e", "$script"}, Default: ".*"},
		"empty":   {Action: nil},
		"missing": {Stdin: "missing-stdin", Action: []string{"cat"}},
	}
	installTestConfig(t, []string{path}, commands, []string{"tail", "grep", "empty", "missing"})

	valid, err := prepareCommand(FrontendCommand{Command: "tail", Entry: ListEntry{Path: path}, Nlines: 10})
	if err != nil || len(valid.mainAction) != 5 {
		t.Fatalf("valid command rejected: %v (%#v)", err, valid)
	}
	if _, err := prepareCommand(FrontendCommand{Command: "", Entry: ListEntry{Path: path}}); err == nil {
		t.Fatal("empty command name was accepted")
	}
	if _, err := prepareCommand(FrontendCommand{Command: "unknown", Entry: ListEntry{Path: path}}); err == nil {
		t.Fatal("unknown command was accepted")
	}
	if _, err := prepareCommand(FrontendCommand{Command: "empty", Entry: ListEntry{Path: path}}); err == nil {
		t.Fatal("command with empty action was accepted")
	}
	if _, err := prepareCommand(FrontendCommand{Command: "missing", Entry: ListEntry{Path: path}}); err == nil {
		t.Fatal("command with missing stdin source was accepted")
	}
	if _, err := prepareCommand(FrontendCommand{Command: "tail", Entry: ListEntry{Path: path}, Nlines: -1}); err == nil {
		t.Fatal("negative line count was accepted")
	}
	if _, err := prepareCommand(FrontendCommand{Command: "grep", Script: strings.Repeat("x", maxCommandArgBytes+1), Entry: ListEntry{Path: path}}); err == nil {
		t.Fatal("oversized script argument was accepted")
	}
}

func TestPrepareCommandRequiresAllowList(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "app.log", "one\n")
	commands := map[string]CommandSpec{
		"tail":   {Action: []string{"tail", "-n", "$lines", "-F", "$path"}},
		"secret": {Action: []string{"tail", "-n", "$lines", "-F", "$path"}},
	}
	installTestConfig(t, []string{path}, commands, []string{"tail"})

	if _, err := prepareCommand(FrontendCommand{Command: "secret", Entry: ListEntry{Path: path}}); err == nil {
		t.Fatal("command outside allow-commands was accepted")
	}
}

func TestServeSessionListAndGracefulClose(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "app.log", "one\n")
	installTestConfig(t, []string{path}, map[string]CommandSpec{
		"tail": {Action: helperAction("stream")},
	}, []string{"tail"})

	session := newFakeSession()
	done := startSession(t, session)
	session.push(t, "list")
	listing := waitForListing(t, session)
	if len(listing["__default__"]) != 1 || listing["__default__"][0].Path != path {
		t.Fatalf("unexpected listing: %#v", listing)
	}

	session.cancel()
	waitSessionDone(t, done)
}

func TestServeSessionInvalidInputsDoNotPanic(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "app.log", "one\n")
	installTestConfig(t, []string{path}, map[string]CommandSpec{
		"tail": {Action: helperAction("stream")},
	}, []string{"tail"})

	session := newFakeSession()
	startSession(t, session)

	invalid := []string{
		"",
		"not a command",
		"{",
		`{"command":"","entry":{"path":"` + path + `"},"nlines":10}`,
		`{"command":"nope","entry":{"path":"` + path + `"},"nlines":10}`,
		`{"command":"tail","entry":{"path":"/does/not/exist"},"nlines":10}`,
		`{"command":"tail","entry":{"path":"` + path + `"},"nlines":-1}`,
		`{"command":"tail","script":"` + strings.Repeat("x", maxCommandArgBytes+1) + `","entry":{"path":"` + path + `"},"nlines":10}`,
	}
	for _, msg := range invalid {
		session.push(t, msg)
	}

	session.push(t, "list")
	waitForListing(t, session)

	// A valid command must still work after all invalid input.
	session.push(t, jsonCommand(t, "tail", path, "", 10))
	waitForLine(t, session, "o", "line-")

	session.cancel()
}

func TestServeSessionCommandStartFailure(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "app.log", "one\n")
	commands := map[string]CommandSpec{
		"missing":    {Action: []string{"tailon-command-that-does-not-exist"}},
		"brokenpipe": {Stdin: "missing-stdin", Action: helperAction("passthrough")},
		"missing-stdin": {
			Action: []string{"tailon-stdin-command-that-does-not-exist"},
		},
		"ok": {Action: helperAction("exit0")},
	}
	installTestConfig(t, []string{path}, commands, []string{"missing", "brokenpipe", "ok"})

	session := newFakeSession()
	startSession(t, session)

	session.push(t, jsonCommand(t, "missing", path, "", 10))
	session.push(t, jsonCommand(t, "brokenpipe", path, "", 10))
	session.push(t, "list")
	waitForListing(t, session)

	// The session is still healthy and can run another command.
	session.push(t, jsonCommand(t, "ok", path, "", 10))
	waitForLine(t, session, "o", "helper-done")

	session.cancel()
}

func TestServeSessionNormalAndAbnormalExit(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "app.log", "one\n")
	commands := map[string]CommandSpec{
		"ok":  {Action: helperAction("exit0")},
		"bad": {Action: helperAction("exit3")},
	}
	installTestConfig(t, []string{path}, commands, []string{"ok", "bad"})

	session := newFakeSession()
	startSession(t, session)

	session.push(t, jsonCommand(t, "ok", path, "", 10))
	waitForLine(t, session, "o", "helper-done")
	session.push(t, "list")
	waitForListing(t, session)

	session.push(t, jsonCommand(t, "bad", path, "", 10))
	waitForLine(t, session, "e", "helper-failed")
	session.push(t, "list")
	waitForListing(t, session)

	session.cancel()
}

func TestRunningCommandStopIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "app.log", "one\n")
	installTestConfig(t, []string{path}, map[string]CommandSpec{
		"sleep": {Action: helperAction("sleep")},
	}, []string{"sleep"})

	prepared, err := prepareCommand(FrontendCommand{Command: "sleep", Entry: ListEntry{Path: path}})
	if err != nil {
		t.Fatalf("prepare command: %v", err)
	}
	running, err := startCommand(prepared)
	if err != nil {
		t.Fatalf("start command: %v", err)
	}

	finished := make(chan struct{})
	go func() {
		running.stopAndWait()
		running.stopAndWait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("repeated stop did not return")
	}
}

func TestRunningCommandStopDrainsFullStream(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "app.log", "one\n")
	installTestConfig(t, []string{path}, map[string]CommandSpec{
		"burst": {Action: helperAction("burst")},
	}, []string{"burst"})

	prepared, err := prepareCommand(FrontendCommand{Command: "burst", Entry: ListEntry{Path: path}})
	if err != nil {
		t.Fatalf("prepare command: %v", err)
	}
	running, err := startCommand(prepared)
	if err != nil {
		t.Fatalf("start command: %v", err)
	}

	// Let stdout fill the bounded streaming channel while nobody reads it.
	time.Sleep(200 * time.Millisecond)

	started := time.Now()
	running.stopAndWait()
	elapsed := time.Since(started)
	if elapsed >= 3*time.Second {
		t.Fatalf("stopping a command with a full output channel took %s", elapsed)
	}
}

func TestServeSessionRapidFileSwitching(t *testing.T) {
	dir := t.TempDir()
	paths := make([]string, 0, 15)
	for i := 0; i < 15; i++ {
		paths = append(paths, writeTestFile(t, dir, filepath.Base(dir)+"-"+string(rune('a'+i))+".log", "file-"+string(rune('a'+i))+"\n"))
	}
	installTestConfig(t, paths, map[string]CommandSpec{
		"cat": {Action: append(helperAction("cat"), "$path")},
	}, []string{"cat"})

	session := newFakeSession()
	startSession(t, session)

	for i := 0; i < 120; i++ {
		path := paths[i%len(paths)]
		session.push(t, jsonCommand(t, "cat", path, "", 10))
	}
	session.push(t, "list")
	waitForListing(t, session)

	session.cancel()
}

func TestServeSessionFilterToggle(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "app.log", "one\n")
	commands := map[string]CommandSpec{
		"tail": {Action: helperAction("stream")},
		"grep": {Stdin: "tail", Action: helperAction("passthrough"), Default: ".*"},
	}
	installTestConfig(t, []string{path}, commands, []string{"tail", "grep"})

	session := newFakeSession()
	startSession(t, session)

	for i := 0; i < 40; i++ {
		switch i % 3 {
		case 0:
			session.push(t, jsonCommand(t, "grep", path, "line", 10))
		case 1:
			session.push(t, jsonCommand(t, "grep", path, "line-1", 10))
		default:
			session.push(t, jsonCommand(t, "tail", path, "", 10))
		}
	}
	session.push(t, "list")
	waitForListing(t, session)

	session.cancel()
}

func TestServeSessionSlowAndFailedWrites(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "app.log", "one\n")
	installTestConfig(t, []string{path}, map[string]CommandSpec{
		"tail": {Action: helperAction("stream")},
	}, []string{"tail"})

	t.Run("slow client", func(t *testing.T) {
		session := newFakeSession()
		session.setSendHook(func(msg string) error {
			if strings.Contains(msg, "line-") {
				<-session.Context().Done()
				return errors.New("write deadline exceeded")
			}
			return nil
		})
		done := startSession(t, session)
		session.push(t, jsonCommand(t, "tail", path, "", 10))

		time.Sleep(50 * time.Millisecond)
		session.cancel()
		waitSessionDone(t, done)
	})

	t.Run("write failure", func(t *testing.T) {
		session := newFakeSession()
		session.setSendHook(func(msg string) error {
			if strings.Contains(msg, "line-") {
				return errSimulatedWriteFailure
			}
			return nil
		})
		done := startSession(t, session)
		session.push(t, jsonCommand(t, "tail", path, "", 10))
		waitSessionDone(t, done)
	})
}

func TestServeSessionConcurrentSessionsAndGoroutineBaseline(t *testing.T) {
	dir := t.TempDir()
	paths := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		paths = append(paths, writeTestFile(t, dir, filepath.Base(dir)+"-"+string(rune('a'+i))+".log", "file\n"))
	}
	installTestConfig(t, paths, map[string]CommandSpec{
		"cat":  {Action: append(helperAction("cat"), "$path")},
		"tail": {Action: helperAction("stream")},
	}, []string{"cat", "tail"})

	// Let earlier tests finish tearing down their helper processes.
	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	const sessionCount = 8
	var wg sync.WaitGroup
	errs := make(chan error, sessionCount)
	for i := 0; i < sessionCount; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			session := newFakeSession()
			done := make(chan struct{})
			go func() {
				defer close(done)
				serveSession(session)
			}()

			session.push(t, "list")
			for n := 0; n < 20; n++ {
				path := paths[(id+n)%len(paths)]
				session.push(t, jsonCommand(t, "cat", path, "", 10))
			}
			session.push(t, "list")

			deadline := time.Now().Add(5 * time.Second)
			for {
				found := false
				for _, msg := range session.messages() {
					if strings.HasPrefix(msg, "{") {
						found = true
						break
					}
				}
				if found || time.Now().After(deadline) {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			session.cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				errs <- errors.New("session did not stop")
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	// A small tolerance accounts for runtime background goroutines. The
	// important property is that repeated sessions do not accumulate.
	got := settleGoroutines(baseline+4, 5*time.Second)
	if got > baseline+4 {
		t.Fatalf("goroutine count grew from %d to %d", baseline, got)
	}
}

func TestFileListingConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "app.log", "one\n")
	specs := []FileSpec{{Path: path, Type: "file"}}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 200; n++ {
				if i%2 == 0 {
					createListing(specs)
				} else {
					_ = fileAllowed(path)
				}
			}
		}(i)
	}
	wg.Wait()
	if !fileAllowed(path) {
		t.Fatal("file should be allowed after concurrent listing updates")
	}
}

func TestFileInfoMissingFile(t *testing.T) {
	entry := fileInfo(filepath.Join(t.TempDir(), "missing.log"))
	if entry.Exists {
		t.Fatal("missing file reported as existing")
	}
}

func TestDownloadHandlerWithoutPath(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "app.log", "content\n")
	oldConfig := config
	config = &Config{AllowDownload: true}
	t.Cleanup(func() { config = oldConfig })
	createListing([]FileSpec{{Path: path, Type: "file"}})

	rec := httptest.NewRecorder()
	downloadHandler(rec, httptest.NewRequest(http.MethodGet, "/files/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing path returned %d, want %d", rec.Code, http.StatusNotFound)
	}

	rec = httptest.NewRecorder()
	downloadHandler(rec, httptest.NewRequest(http.MethodGet, "/files/?path="+path, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "content") {
		t.Fatalf("known file returned %d: %q", rec.Code, rec.Body.String())
	}
}

func TestExpandAndValidateRejectsTooManyArgs(t *testing.T) {
	action := make([]string, maxCommandArgs+1)
	for i := range action {
		action[i] = "x"
	}
	if _, err := expandAndValidateArgs(action, FrontendCommand{Entry: ListEntry{Path: "/tmp/x"}}); err == nil {
		t.Fatal("too many arguments were accepted")
	}
}
