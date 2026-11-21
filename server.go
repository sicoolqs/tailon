//go:generate go run frontend_gen.go

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gorilla/handlers"
	"github.com/gvalkov/tailon/cmd"
	"github.com/igm/sockjs-go/v3/sockjs"
	"github.com/shurcooL/httpfs/html/vfstemplate"
	"github.com/shurcooL/httpgzip"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// maxFrontendMessageSize bounds the amount of data accepted from a client
	// before it is parsed. Valid requests are a few hundred bytes at most.
	maxFrontendMessageSize = 1 << 20

	// Command templates come from the server configuration, but their expanded
	// arguments contain client-controlled data. Keep the expansion bounded.
	maxCommandArgs     = 256
	maxCommandArgBytes = 64 << 10

	// maxTailLines prevents a client from asking tail for an unreasonable
	// amount of history. The default UI value is 10.
	maxTailLines = 1_000_000

	// commandStopTimeout bounds how long a single cleanup step waits for an
	// exiting process. It is not a request timeout; log scrolling continues
	// for as long as the client stays connected.
	commandStopTimeout = 2 * time.Second

	// commandStartTimeout bounds the wait for the asynchronous cmd.Cmd start.
	commandStartTimeout = 2 * time.Second

	// sessionWriterShutdownTimeout bounds how long the session reader waits
	// for the writer to release subprocesses after the client disconnects.
	sessionWriterShutdownTimeout = 5 * time.Second

	// websocketWriteTimeout prevents a dead or stalled TCP connection from
	// blocking session.Send forever.
	websocketWriteTimeout = 10 * time.Second
)

func setupRoutes(relativeroot string) *http.ServeMux {
	router := http.NewServeMux()

	// Use either "frontend_dev.go" or "frontend_bin.go", depending on the "dev" build tag.
	staticHandler := noCacheControl(httpgzip.FileServer(FrontendAssets, httpgzip.FileServerOptions{IndexHTML: true}))

	sockjsOptions := sockjs.DefaultOptions
	sockjsOptions.WebsocketWriteTimeout = websocketWriteTimeout
	sockjsHandler := sockjs.NewHandler(relativeroot+"ws", sockjsOptions, wsHandler)
	staticHandler = http.StripPrefix(relativeroot+"vfs/", staticHandler)

	router.Handle(relativeroot+"vfs/", staticHandler)
	router.Handle(relativeroot+"ws/", sockjsHandler)
	router.HandleFunc(relativeroot+"files/", downloadHandler)
	if mcpHandler != nil {
		router.Handle(config.MCP.Path, mcpHandler)
	} else if path := config.MCP.Path; strings.HasPrefix(path, "/") && path != "/" {
		// Keep the reserved MCP path from being served by the web index when
		// MCP is disabled. Misconfiguration is reported by the MCP validation
		// only when MCP is enabled.
		router.HandleFunc(path, http.NotFound)
	}
	router.HandleFunc(relativeroot+"", indexHandler)

	return router
}

func setupServer(config *Config, addr string, logger *log.Logger) *http.Server {
	router := setupRoutes(config.RelativeRoot)
	loggingRouter := handlers.LoggingHandler(os.Stderr, router)

	writeTimeout := 10 * time.Second
	if config.MCP.Enabled {
		// MCP queries have their own budget; the HTTP write deadline must not
		// cut a valid response short.
		if t := config.MCP.Limits.QueryTimeout + 10*time.Second; t > writeTimeout {
			writeTimeout = t
		}
	}

	server := http.Server{
		Addr:         addr,
		Handler:      loggingRouter,
		ErrorLog:     logger,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: writeTimeout,
		IdleTimeout:  15 * time.Second,
	}

	return &server
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	t := template.Must(vfstemplate.ParseFiles(FrontendAssets, nil, "/templates/base.html", "/templates/tailon.html"))
	t.Execute(w, config)
}

func downloadHandler(w http.ResponseWriter, r *http.Request) {
	if !config.AllowDownload {
		http.Error(w, "downloads forbidden by server", http.StatusForbidden)
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" || !fileAllowed(path) {
		log.Printf("warn: attempt to access unknown file: %s", path)
		http.Error(w, "unknown file", http.StatusNotFound)
		return
	}
	http.ServeFile(w, r, path)
}

func noCacheControl(h http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		h.ServeHTTP(w, r)
	}

	return http.HandlerFunc(fn)
}

// FrontendCommand instances are the messages that the client sends to the server when the file, tool or script change.
type FrontendCommand struct {
	Command string
	Script  string
	Entry   ListEntry
	Nlines  int
}

// wsSession is the subset of sockjs.Session used by the websocket backend.
// Keeping it as an interface makes session lifecycle and failure handling
// directly testable without a network connection.
type wsSession interface {
	Send(string) error
	Recv() (string, error)
	Context() context.Context
	Close(status uint32, reason string) error
}

// The main sockjs handler.
func wsHandler(session sockjs.Session) {
	serveSession(session)
}

// serveSession owns one client session. It reads client messages and waits for
// the writer to release subprocesses before returning.
func serveSession(session wsSession) {
	messages := make(chan string)
	done := make(chan struct{})
	writerDone := make(chan struct{})

	go func() {
		defer close(writerDone)
		wsWriter(session, messages, done)
	}()

	defer func() {
		close(done)
		select {
		case <-writerDone:
		case <-time.After(sessionWriterShutdownTimeout):
			log.Printf("warn: session writer did not stop within %s", sessionWriterShutdownTimeout)
		}
	}()

	for {
		msg, err := session.Recv()
		if err != nil {
			if !isSessionClosedError(err) {
				log.Printf("warn: websocket receive failed: %v", err)
			}
			return
		}

		select {
		case messages <- msg:
		case <-writerDone:
			return
		case <-session.Context().Done():
			return
		}
	}
}

func isSessionClosedError(err error) bool {
	return errors.Is(err, sockjs.ErrSessionNotOpen) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, io.EOF)
}

// wsWriter owns the subprocess pipeline of a single session. All sending and
// process lifecycle operations happen in this goroutine, so stdout/stderr,
// status and client messages are serialized without extra channels or locks.
func wsWriter(session wsSession, messages <-chan string, done <-chan struct{}) {
	var current *runningCommand

	defer func() {
		if current != nil {
			current.stopAndWait()
		}
	}()

	for {
		var stdout, stderr <-chan string
		var status <-chan cmd.Status
		if current != nil {
			stdout = current.stdout()
			stderr = current.stderr()
			status = current.statusChan
		}

		select {
		case <-done:
			return
		case <-session.Context().Done():
			return

		case msg, ok := <-messages:
			if !ok {
				return
			}
			if err := handleFrontendMessage(session, msg, &current); err != nil {
				log.Printf("error: %v", err)
				_ = session.Close(1011, "server write error")
				return
			}

		case line := <-stdout:
			if err := sendStreamLine(session, "o", line); err != nil {
				log.Printf("error: websocket output send failed: %v", err)
				_ = session.Close(1011, "server write error")
				return
			}
		case line := <-stderr:
			if err := sendStreamLine(session, "e", line); err != nil {
				log.Printf("error: websocket error-stream send failed: %v", err)
				_ = session.Close(1011, "server write error")
				return
			}

		case st := <-status:
			logCommandStatus(st)
			if current != nil {
				if err := current.drain(session); err != nil {
					log.Printf("error: websocket final output send failed: %v", err)
					_ = session.Close(1011, "server write error")
					current.stopAndWait()
					return
				}
				current.stopAndWait()
				current = nil
			}
		}
	}
}

// handleFrontendMessage validates and applies one client message. Invalid
// messages are logged and ignored; they never take down the server or the
// currently running command.
func handleFrontendMessage(session wsSession, msg string, current **runningCommand) error {
	if len(msg) > maxFrontendMessageSize {
		log.Printf("warn: ignoring oversized frontend message (%d bytes)", len(msg))
		return nil
	}
	if msg == "" {
		log.Print("warn: ignoring empty frontend message")
		return nil
	}

	if msg == "list" {
		return sendFileListing(session)
	}

	if msg[0] != '{' {
		log.Printf("warn: ignoring unknown frontend message: %q", truncateForLog(msg, 80))
		return nil
	}

	frontend := FrontendCommand{}
	if err := json.Unmarshal([]byte(msg), &frontend); err != nil {
		log.Printf("warn: invalid frontend JSON: %v", err)
		return nil
	}

	if !fileAllowed(frontend.Entry.Path) {
		log.Printf("warn: attempt to access unknown file: %q", frontend.Entry.Path)
		return nil
	}

	// Validate the complete command before stopping the current pipeline. A
	// malformed request must not interrupt a stream that is already running.
	prepared, err := prepareCommand(frontend)
	if err != nil {
		log.Printf("warn: rejected command request: %v", err)
		return nil
	}

	if *current != nil {
		(*current).stopAndWait()
		*current = nil
	}

	running, err := startCommand(prepared)
	if err != nil {
		log.Printf("error: could not start command: %v", err)
		return nil
	}
	*current = running
	return nil
}

func sendFileListing(session wsSession) error {
	lst := createListing(config.FileSpecs)
	b, err := json.Marshal(lst)
	if err != nil {
		log.Printf("error: could not encode file listing: %v", err)
		return nil
	}
	if err := session.Send(string(b)); err != nil {
		return fmt.Errorf("send file listing: %w", err)
	}
	return nil
}

func sendStreamLine(session wsSession, stream, line string) error {
	data, err := json.Marshal([]string{stream, line})
	if err != nil {
		return fmt.Errorf("encode output line: %w", err)
	}
	if err := session.Send(string(data)); err != nil {
		return fmt.Errorf("send output line: %w", err)
	}
	return nil
}

func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// commandRequest is a fully validated command pipeline. It can only be built by
// prepareCommand, which ensures that every executable and argument exists.
type commandRequest struct {
	stdinAction []string
	mainAction  []string
}

func commandNameAllowed(name string) bool {
	for _, allowed := range config.AllowCommandNames {
		if name == allowed {
			return true
		}
	}
	return false
}

func prepareCommand(frontend FrontendCommand) (*commandRequest, error) {
	if frontend.Command == "" {
		return nil, errors.New("empty command name")
	}
	if !commandNameAllowed(frontend.Command) {
		return nil, fmt.Errorf("command %q is not in the allow-commands list", frontend.Command)
	}

	spec, ok := config.CommandSpecs[frontend.Command]
	if !ok {
		return nil, fmt.Errorf("command %q is not defined", frontend.Command)
	}

	mainAction, err := expandAndValidateArgs(spec.Action, frontend)
	if err != nil {
		return nil, fmt.Errorf("command %q: %w", frontend.Command, err)
	}

	var stdinAction []string
	if spec.Stdin != "" {
		stdinSpec, ok := config.CommandSpecs[spec.Stdin]
		if !ok {
			return nil, fmt.Errorf("command %q: stdin command %q is not defined", frontend.Command, spec.Stdin)
		}
		stdinAction, err = expandAndValidateArgs(stdinSpec.Action, frontend)
		if err != nil {
			return nil, fmt.Errorf("stdin command %q: %w", spec.Stdin, err)
		}
	}

	return &commandRequest{stdinAction: stdinAction, mainAction: mainAction}, nil
}

// Expands the variables in main.CommandSpec.Action with the values in the
// frontend command. For example:
//
//	["tail", "-n", "$lines", "-F", "$path"] -> ["tail", "-n", "10", "-F", "f1.txt"]
func expandCommandArgs(action []string, cmd FrontendCommand) []string {
	var res = make([]string, 0, len(action))

	for _, arg := range action {
		switch arg {
		case "$lines":
			res = append(res, strconv.Itoa(cmd.Nlines))
		case "$path":
			res = append(res, cmd.Entry.Path)
		case "$script":
			res = append(res, cmd.Script)
		default:
			res = append(res, arg)
		}
	}

	return res
}

func expandAndValidateArgs(action []string, frontend FrontendCommand) ([]string, error) {
	if len(action) == 0 {
		return nil, errors.New("command has an empty action")
	}
	if len(action) > maxCommandArgs {
		return nil, fmt.Errorf("command has too many arguments (%d)", len(action))
	}

	res := make([]string, 0, len(action))
	totalBytes := 0
	for i, arg := range action {
		switch arg {
		case "$lines":
			if frontend.Nlines < 0 || frontend.Nlines > maxTailLines {
				return nil, fmt.Errorf("invalid line count %d", frontend.Nlines)
			}
			arg = strconv.Itoa(frontend.Nlines)
		case "$path":
			arg = frontend.Entry.Path
		case "$script":
			arg = frontend.Script
		}

		if i == 0 && arg == "" {
			return nil, errors.New("command has an empty executable name")
		}
		totalBytes += len(arg)
		if totalBytes > maxCommandArgBytes {
			return nil, fmt.Errorf("expanded command arguments exceed %d bytes", maxCommandArgBytes)
		}
		res = append(res, arg)
	}

	return res, nil
}

// runningCommand represents one started process pipeline. It is created and
// destroyed only by wsWriter, which also serializes all access to its channels.
type runningCommand struct {
	procA        *exec.Cmd
	procB        *cmd.Cmd
	stdin        io.ReadCloser
	statusChan   <-chan cmd.Status
	procAStarted bool
	stopOnce     sync.Once
}

func startCommand(request *commandRequest) (*runningCommand, error) {
	r := &runningCommand{}

	if request.stdinAction != nil {
		procA := exec.Command(request.stdinAction[0], request.stdinAction[1:]...)
		stdout, err := procA.StdoutPipe()
		if err != nil {
			return nil, fmt.Errorf("create stdout pipe for %q: %w", request.stdinAction[0], err)
		}
		r.procA = procA
		r.stdin = stdout
	}

	r.procB = cmd.NewCmdOptions(cmd.Options{Buffered: false, Streaming: true}, request.mainAction[0], request.mainAction[1:]...)
	if r.stdin != nil {
		r.procB.Stdin = r.stdin
	}

	if r.procA != nil {
		log.Printf("Running command: %v", request.stdinAction)
		if err := r.procA.Start(); err != nil {
			_ = r.stdin.Close()
			return nil, fmt.Errorf("start command %q: %w", request.stdinAction[0], err)
		}
		r.procAStarted = true
	}

	log.Printf("Running command: %v", request.mainAction)
	r.statusChan = r.procB.Start()
	if err := r.procB.WaitStarted(commandStartTimeout); err != nil {
		r.stopAndWait()
		return nil, fmt.Errorf("start command %q: %w", request.mainAction[0], err)
	}
	return r, nil
}

func (r *runningCommand) stdout() <-chan string {
	if r == nil || r.procB == nil {
		return nil
	}
	return r.procB.Stdout
}

func (r *runningCommand) stderr() <-chan string {
	if r == nil || r.procB == nil {
		return nil
	}
	return r.procB.Stderr
}

// drain delivers output that was buffered before the command exited. All
// streaming writes complete before cmd.Status becomes available, so a
// non-blocking drain is sufficient and cannot spin forever.
func (r *runningCommand) drain(session wsSession) error {
	if r == nil || r.procB == nil {
		return nil
	}

	for {
		select {
		case line := <-r.procB.Stdout:
			if err := sendStreamLine(session, "o", line); err != nil {
				return err
			}
		case line := <-r.procB.Stderr:
			if err := sendStreamLine(session, "e", line); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

// stopAndWait is idempotent. It closes stdin, asks the process groups to stop,
// falls back to a forced kill, and waits for a bounded amount of time.
func (r *runningCommand) stopAndWait() {
	if r == nil {
		return
	}
	r.stopOnce.Do(r.stop)
}

func (r *runningCommand) stop() {
	if r.stdin != nil {
		_ = r.stdin.Close()
	}

	if r.procB != nil {
		status := r.procB.Status()
		if status.PID > 0 {
			log.Printf("Stopping pid %d", status.PID)
		}
		if err := r.procB.Stop(); err != nil {
			log.Printf("warn: stop command %q: %v", r.procB.Name, err)
		}
		if !waitForCommandDone(r.procB, commandStopTimeout) {
			log.Printf("warn: command %q did not stop within %s; killing it", r.procB.Name, commandStopTimeout)
			if err := r.procB.Kill(); err != nil {
				log.Printf("warn: kill command %q: %v", r.procB.Name, err)
			}
			waitForCommandDone(r.procB, commandStopTimeout)
		}
	}

	if r.procA != nil && r.procAStarted {
		if r.procA.Process != nil {
			log.Printf("Stopping pid %d", r.procA.Process.Pid)
			if err := r.procA.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				log.Printf("warn: kill command %q: %v", r.procA.Path, err)
			}
		}
		if !waitForProcess(r.procA, commandStopTimeout) {
			log.Printf("warn: command %q did not exit within %s", r.procA.Path, commandStopTimeout)
		}
	}
}

// waitForCommandDone waits for cmd.Cmd to finish while draining its streaming
// channels. Without the drain, a command whose child process has exited can
// stay blocked in cmd.Wait because exec's stdout copier is blocked writing to a
// full streaming channel that nobody is reading.
func waitForCommandDone(command *cmd.Cmd, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-command.Done():
			return true
		case <-command.Stdout:
		case <-command.Stderr:
		case <-timer.C:
			return false
		}
	}
}

func waitForProcess(process *exec.Cmd, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		_ = process.Wait()
		close(done)
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func logCommandStatus(status cmd.Status) {
	if status.PID == 0 && status.Error != nil {
		log.Printf("error: command %q failed to start: %v", status.Cmd, status.Error)
		return
	}
	if status.Error != nil {
		log.Printf("command %q exited: %v", status.Cmd, status.Error)
		return
	}
	log.Printf("command %q exited with status %d", status.Cmd, status.Exit)
}
