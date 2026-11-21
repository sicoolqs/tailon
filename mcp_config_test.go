package main

import (
	"testing"

	"github.com/pelletier/go-toml"
)

func TestParseMCPConfig(t *testing.T) {
	src := `
[mcp]
enabled = true
path = "/edge/mcp"
timezone = "UTC"
default-encoding = "GBK"
support-gzip = false
allowed-hosts = ["10.0.0.1"]
allowed-origins = ["http://10.0.0.1:18029"]

[mcp.limits]
max-concurrent-queries = 2
max-parallel-files = 1
query-timeout = "20s"
max-scan-bytes = 1048576
cursor-ttl = "5m"

[mcp.redaction]
enabled = true
patterns = ["(?i)token=([^ ]+)"]
`
	tree, err := toml.Load(src)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := parseMCPConfig(treeOrNil(tree.Get("mcp")))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled || cfg.Path != "/edge/mcp" {
		t.Fatalf("config = %#v", cfg)
	}
	if cfg.Timezone != "UTC" || cfg.DefaultEncoding != "GBK" || cfg.SupportGzip {
		t.Fatalf("config = %#v", cfg)
	}
	if cfg.Limits.MaxConcurrentQueries != 2 || cfg.Limits.MaxParallelFiles != 1 {
		t.Fatalf("limits = %#v", cfg.Limits)
	}
	if cfg.Limits.QueryTimeout.Seconds() != 20 || cfg.Limits.CursorTTL.Minutes() != 5 {
		t.Fatalf("durations = %s / %s", cfg.Limits.QueryTimeout, cfg.Limits.CursorTTL)
	}
	if cfg.Limits.MaxScanBytes != 1048576 {
		t.Fatalf("max scan bytes = %d", cfg.Limits.MaxScanBytes)
	}
	// Unspecified fields keep their defaults.
	if cfg.Limits.MaxLines != 5000 || cfg.Limits.DefaultLines != 200 {
		t.Fatalf("defaults were lost: %#v", cfg.Limits)
	}
	if !cfg.Redaction.Enabled || len(cfg.Redaction.Patterns) != 1 {
		t.Fatalf("redaction = %#v", cfg.Redaction)
	}
}

func TestParseMCPConfigMissingSection(t *testing.T) {
	cfg, err := parseMCPConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled {
		t.Fatal("MCP must remain disabled when the section is absent")
	}
}
