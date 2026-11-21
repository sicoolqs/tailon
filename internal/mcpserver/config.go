package mcpserver

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/gvalkov/tailon/internal/logquery"
)

// Config is the optional [mcp] section of the Tailon TOML configuration.
type Config struct {
	Enabled         bool                     `mapstructure:"enabled"`
	Path            string                   `mapstructure:"path"`
	AllowedHosts    []string                 `mapstructure:"allowed-hosts"`
	AllowedOrigins  []string                 `mapstructure:"allowed-origins"`
	Timezone        string                   `mapstructure:"timezone"`
	DefaultEncoding string                   `mapstructure:"default-encoding"`
	SupportGzip     bool                     `mapstructure:"support-gzip"`
	Limits          logquery.Limits          `mapstructure:"limits"`
	Redaction       logquery.RedactionConfig `mapstructure:"redaction"`
}

// DefaultConfig returns the MCP defaults. MCP is enabled by default; an
// explicit `enabled = false` in the [mcp] section disables it.
func DefaultConfig() Config {
	return Config{
		Enabled:         true,
		Path:            "/mcp",
		Timezone:        "Asia/Shanghai",
		DefaultEncoding: "UTF-8",
		SupportGzip:     true,
		Limits:          logquery.DefaultLimits(),
	}
}

// Validate checks the MCP configuration and its interaction with existing
// Tailon routes.
func (c *Config) Validate(relativeRoot string) error {
	if c.Path == "" {
		c.Path = "/mcp"
	}
	if !strings.HasPrefix(c.Path, "/") {
		return fmt.Errorf("mcp.path must start with '/' (got %q)", c.Path)
	}
	if strings.ContainsAny(c.Path, "?#") {
		return fmt.Errorf("mcp.path must not contain a query or fragment")
	}
	if strings.Contains(c.Path, "..") {
		return fmt.Errorf("mcp.path must not contain '..'")
	}
	if c.Path == "/" {
		return fmt.Errorf("mcp.path must not be the web application root")
	}
	if err := checkPathConflict(c.Path, relativeRoot); err != nil {
		return err
	}

	for _, host := range c.AllowedHosts {
		if strings.TrimSpace(host) == "" || strings.Contains(host, "://") {
			return fmt.Errorf("mcp.allowed-hosts entries must be bare host names (got %q)", host)
		}
	}
	for _, origin := range c.AllowedOrigins {
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("mcp.allowed-origins entries must be http(s) origins (got %q)", origin)
		}
	}

	switch strings.ToUpper(strings.TrimSpace(c.DefaultEncoding)) {
	case "", "UTF-8", "UTF8", "GBK", "GB18030", "GB2312":
	default:
		return fmt.Errorf("mcp.default-encoding must be UTF-8, GBK or GB18030 (got %q)", c.DefaultEncoding)
	}
	if err := c.Limits.Validate(); err != nil {
		return fmt.Errorf("mcp.limits: %w", err)
	}
	if c.Redaction.Enabled {
		for _, pattern := range c.Redaction.Patterns {
			if strings.TrimSpace(pattern) == "" {
				return fmt.Errorf("mcp.redaction.patterns must not contain empty patterns")
			}
		}
	}
	return nil
}

func checkPathConflict(path, relativeRoot string) error {
	if relativeRoot == "" {
		relativeRoot = "/"
	}
	if !strings.HasSuffix(relativeRoot, "/") {
		relativeRoot += "/"
	}
	root := strings.TrimSuffix(relativeRoot, "/")
	cleanPath := strings.TrimSuffix(path, "/")

	// The web application root is the index catch-all.
	if root != "" && cleanPath == root {
		return fmt.Errorf("mcp.path %q conflicts with the web application root", path)
	}

	// The other routes are subtrees; neither their parents nor their children
	// may be used for MCP.
	reserved := []string{
		relativeRoot + "vfs",
		relativeRoot + "ws",
		relativeRoot + "files",
	}
	for _, r := range reserved {
		r = strings.TrimSuffix(r, "/")
		if cleanPath == r || strings.HasPrefix(cleanPath, r+"/") || strings.HasPrefix(r, cleanPath+"/") {
			return fmt.Errorf("mcp.path %q conflicts with the existing Tailon route %q", path, r)
		}
	}
	return nil
}

// ServiceConfig converts the MCP config into the logquery service config.
func (c Config) ServiceConfig() logquery.Config {
	return logquery.Config{
		Timezone:        c.Timezone,
		DefaultEncoding: c.DefaultEncoding,
		SupportGzip:     c.SupportGzip,
		Limits:          c.Limits,
		Redaction:       c.Redaction,
	}
}
