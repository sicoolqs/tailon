package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gvalkov/tailon/internal/mcpserver"
)

func TestMCPRouteRegistration(t *testing.T) {
	oldConfig := config
	oldHandler := mcpHandler
	oldServer := mcpServer
	defer func() {
		config = oldConfig
		mcpHandler = oldHandler
		mcpServer = oldServer
	}()

	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("2026-11-21 10:00:00 INFO ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	config = &Config{
		RelativeRoot: "/",
		FileSpecs:    []FileSpec{{Path: path, Type: "file", Group: "app"}},
		MCP:          mcpserver.DefaultConfig(),
	}
	config.MCP.Enabled = true
	config.MCP.Timezone = "UTC"
	if err := initMCP(); err != nil {
		t.Fatalf("initMCP: %v", err)
	}

	router := setupRoutes("/")
	ts := httptest.NewServer(router)
	defer ts.Close()

	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(initialize))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize status = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"tailon"`) {
		t.Fatalf("initialize response = %s", body)
	}

	// The existing web application must still be served on the same port.
	indexResp, err := ts.Client().Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	indexResp.Body.Close()
	if indexResp.StatusCode != http.StatusOK {
		t.Fatalf("index status = %d", indexResp.StatusCode)
	}
}

func TestMCPExplicitlyDisabled(t *testing.T) {
	oldConfig := config
	oldHandler := mcpHandler
	defer func() {
		config = oldConfig
		mcpHandler = oldHandler
	}()
	config = &Config{RelativeRoot: "/", MCP: mcpserver.DefaultConfig()}
	config.MCP.Enabled = false
	mcpHandler = nil

	router := setupRoutes("/")
	ts := httptest.NewServer(router)
	defer ts.Close()

	resp, err := ts.Client().Post(ts.URL+"/mcp", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled MCP status = %d, want 404", resp.StatusCode)
	}
}
