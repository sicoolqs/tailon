package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/igm/sockjs-go/v3/sockjs"
)

var sockjsSessionID int64

func TestSockJSWebSocketLifecycle(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "app.log", "one\ntwo\n")
	installTestConfig(t, []string{path}, map[string]CommandSpec{
		"cat":  {Action: append(helperAction("cat"), "$path")},
		"tail": {Action: helperAction("stream")},
	}, []string{"cat", "tail"})

	options := sockjs.DefaultOptions
	options.WebsocketWriteTimeout = time.Second
	server := httptest.NewServer(sockjs.NewHandler("/ws", options, wsHandler))
	defer server.Close()

	conn := dialTestSockJS(t, server.URL)
	expectOpenFrame(t, conn)

	sendSockJS(t, conn, []string{"list"})
	waitForSockJSListing(t, conn, path)

	msg := jsonCommand(t, "cat", path, "", 10)
	sendSockJS(t, conn, []string{msg})
	waitForSockJSLine(t, conn, "one")

	// Invalid frames must be ignored without dropping the connection.
	for _, invalid := range []string{"", "{", `{"command":"nope","entry":{"path":"` + path + `"}}`} {
		sendSockJS(t, conn, []string{invalid})
	}
	sendSockJS(t, conn, []string{"list"})
	waitForSockJSListing(t, conn, path)

	// Abrupt close simulates a browser refresh or tab close.
	if err := conn.Close(); err != nil {
		t.Fatalf("close websocket: %v", err)
	}

	// Reconnect repeatedly to cover refresh/close cycles.
	for i := 0; i < 10; i++ {
		conn = dialTestSockJS(t, server.URL)
		expectOpenFrame(t, conn)
		sendSockJS(t, conn, []string{"list"})
		waitForSockJSListing(t, conn, path)
		if err := conn.Close(); err != nil {
			t.Fatalf("close websocket during reconnect loop: %v", err)
		}
	}
}

func dialTestSockJS(t *testing.T, serverURL string) *websocket.Conn {
	t.Helper()
	id := atomic.AddInt64(&sockjsSessionID, 1)
	url := "ws" + strings.TrimPrefix(serverURL, "http") + "/ws/" + strconv.FormatInt(id, 10) + "/0/websocket"
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, _, err := dialer.Dial(url, http.Header{"Origin": []string{serverURL}})
	if err != nil {
		t.Fatalf("dial sockjs websocket: %v", err)
	}
	return conn
}

func expectOpenFrame(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read open frame: %v", err)
	}
	if string(data) != "o" {
		t.Fatalf("unexpected open frame %q", data)
	}
}

func sendSockJS(t *testing.T, conn *websocket.Conn, messages []string) {
	t.Helper()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteJSON(messages); err != nil {
		t.Fatalf("write sockjs frame: %v", err)
	}
}

func readSockJSBatch(t *testing.T, conn *websocket.Conn) []string {
	t.Helper()
	for {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read sockjs frame: %v", err)
		}
		frame := string(data)
		switch {
		case frame == "o" || frame == "h":
			continue
		case strings.HasPrefix(frame, "a["):
			var messages []string
			if err := json.Unmarshal([]byte(frame[1:]), &messages); err != nil {
				t.Fatalf("decode sockjs batch %q: %v", frame, err)
			}
			return messages
		default:
			t.Fatalf("unexpected sockjs frame %q", frame)
		}
	}
}

func waitForSockJSListing(t *testing.T, conn *websocket.Conn, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, msg := range readSockJSBatch(t, conn) {
			if !strings.HasPrefix(msg, "{") {
				continue
			}
			var listing map[string][]*ListEntry
			if err := json.Unmarshal([]byte(msg), &listing); err != nil {
				continue
			}
			for _, entries := range listing {
				for _, entry := range entries {
					if entry.Path == path {
						return
					}
				}
			}
		}
	}
	t.Fatalf("file listing for %s was not received", path)
}

func waitForSockJSLine(t *testing.T, conn *websocket.Conn, contains string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, msg := range readSockJSBatch(t, conn) {
			var pair []string
			if err := json.Unmarshal([]byte(msg), &pair); err != nil || len(pair) != 2 {
				continue
			}
			if pair[0] == "o" && strings.Contains(pair[1], contains) {
				return
			}
		}
	}
	t.Fatalf("output line containing %q was not received", contains)
}

func TestSockJSFrameParsing(t *testing.T) {
	// Keep the frame parser honest: the canonical batch is a[...] where the
	// payload itself is a JSON array of strings.
	frame := `a[` + strconv.Quote(fmt.Sprintf("%s", `["o","line"]`)) + `]`
	var messages []string
	if err := json.Unmarshal([]byte(frame[1:]), &messages); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0] != `["o","line"]` {
		t.Fatalf("unexpected decoded messages: %#v", messages)
	}
}
