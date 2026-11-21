package mcpserver

import (
	"strings"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Enabled {
		t.Fatal("MCP must be disabled by default")
	}
	if cfg.Path != "/mcp" {
		t.Fatalf("path = %q", cfg.Path)
	}
	if cfg.Limits.QueryTimeout != 15*time.Second || cfg.Limits.MaxConcurrentQueries != 4 {
		t.Fatalf("limits = %#v", cfg.Limits)
	}
	if err := cfg.Validate("/"); err != nil {
		t.Fatalf("default config should validate: %v", err)
	}
}

func TestValidatePathConflicts(t *testing.T) {
	cases := []struct {
		path string
		root string
		ok   bool
	}{
		{"/mcp", "/", true},
		{"/edge/mcp", "/tailon/", true},
		{"/", "/", false},
		{"/vfs", "/", false},
		{"/vfs/data", "/", false},
		{"/ws", "/", false},
		{"/files", "/", false},
		{"/tailon", "/tailon/", false},
		{"/tailon/vfs", "/tailon/", false},
		{"mcp", "/", false},
	}
	for _, tc := range cases {
		cfg := DefaultConfig()
		cfg.Path = tc.path
		err := cfg.Validate(tc.root)
		if tc.ok && err != nil {
			t.Errorf("path %q root %q: unexpected error: %v", tc.path, tc.root, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("path %q root %q: expected an error", tc.path, tc.root)
		}
	}
}

func TestValidateLimitsAndOrigins(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AllowedOrigins = []string{"not-a-url"}
	if err := cfg.Validate("/"); err == nil {
		t.Fatal("expected origin validation error")
	}

	cfg = DefaultConfig()
	cfg.AllowedOrigins = []string{"http://127.0.0.1:8080"}
	cfg.AllowedHosts = []string{"http://example.com"}
	if err := cfg.Validate("/"); err == nil {
		t.Fatal("expected host validation error")
	}

	cfg = DefaultConfig()
	cfg.Limits.MaxConcurrentQueries = 0
	if err := cfg.Validate("/"); err == nil {
		t.Fatal("expected limit validation error")
	}
}

func TestServiceConfigEncoding(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DefaultEncoding = "GB18030"
	svcCfg := cfg.ServiceConfig()
	if !strings.EqualFold(svcCfg.DefaultEncoding, "GB18030") {
		t.Fatalf("encoding = %q", svcCfg.DefaultEncoding)
	}
	cfg.DefaultEncoding = "KOI8-R"
	if err := cfg.Validate("/"); err == nil {
		t.Fatal("expected encoding validation error")
	}
}
