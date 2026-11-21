package logquery

import (
	"fmt"
	"regexp"
)

// RedactionConfig enables output redaction for secrets embedded in log lines.
type RedactionConfig struct {
	Enabled  bool     `mapstructure:"enabled"`
	Patterns []string `mapstructure:"patterns"`
}

// Redactor applies configured regular expressions to MCP output.
type Redactor struct {
	rules []*regexp.Regexp
}

// NewRedactor compiles redaction rules. Invalid patterns fail startup.
func NewRedactor(cfg RedactionConfig) (*Redactor, error) {
	r := &Redactor{}
	if !cfg.Enabled {
		return r, nil
	}
	for _, pattern := range cfg.Patterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid redaction pattern %q: %w", pattern, err)
		}
		r.rules = append(r.rules, re)
	}
	return r, nil
}

// Apply replaces every match with a fixed placeholder.
func (r *Redactor) Apply(s string) string {
	if r == nil || len(r.rules) == 0 || s == "" {
		return s
	}
	for _, re := range r.rules {
		s = re.ReplaceAllString(s, "[REDACTED]")
	}
	return s
}

// ApplyAll redacts every string in a slice.
func (r *Redactor) ApplyAll(lines []string) []string {
	if r == nil || len(r.rules) == 0 || len(lines) == 0 {
		return lines
	}
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = r.Apply(line)
	}
	return out
}
