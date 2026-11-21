package logquery

import (
	"fmt"
	"time"
)

// Limits are the server-side resource limits for every MCP log query. Clients
// can request smaller values, but can never override these hard limits.
type Limits struct {
	MaxConcurrentQueries int           `mapstructure:"max-concurrent-queries"`
	MaxParallelFiles     int           `mapstructure:"max-parallel-files"`
	QueryTimeout         time.Duration `mapstructure:"query-timeout"`

	MaxScanBytes     int64 `mapstructure:"max-scan-bytes"`
	MaxResponseBytes int   `mapstructure:"max-response-bytes"`
	MaxRequestBytes  int   `mapstructure:"max-request-bytes"`

	DefaultLines int `mapstructure:"default-lines"`
	MaxLines     int `mapstructure:"max-lines"`

	DefaultResults  int `mapstructure:"default-results"`
	MaxResults      int `mapstructure:"max-results"`
	MaxContextLines int `mapstructure:"max-context-lines"`

	MaxFilesPerQuery    int `mapstructure:"max-files-per-query"`
	MaxDiscoveryEntries int `mapstructure:"max-discovery-entries"`
	MaxListPageSize     int `mapstructure:"max-list-page-size"`

	MaxLineBytes   int `mapstructure:"max-line-bytes"`
	MaxEventBytes  int `mapstructure:"max-event-bytes"`
	MaxRegexLength int `mapstructure:"max-regex-length"`

	MaxSummaryBuckets  int `mapstructure:"max-summary-buckets"`
	MaxAggregationKeys int `mapstructure:"max-aggregation-keys"`

	ReadBufferBytes int           `mapstructure:"read-buffer-bytes"`
	CursorTTL       time.Duration `mapstructure:"cursor-ttl"`
}

// DefaultLimits returns the documented default resource limits.
func DefaultLimits() Limits {
	return Limits{
		MaxConcurrentQueries: 4,
		MaxParallelFiles:     2,
		QueryTimeout:         15 * time.Second,

		MaxScanBytes:     128 << 20,
		MaxResponseBytes: 64 << 10,
		MaxRequestBytes:  64 << 10,

		DefaultLines: 200,
		MaxLines:     5000,

		DefaultResults:  50,
		MaxResults:      500,
		MaxContextLines: 100,

		MaxFilesPerQuery:    32,
		MaxDiscoveryEntries: 10000,
		MaxListPageSize:     500,

		MaxLineBytes:   64 << 10,
		MaxEventBytes:  128 << 10,
		MaxRegexLength: 4096,

		MaxSummaryBuckets:  288,
		MaxAggregationKeys: 10000,

		ReadBufferBytes: 64 << 10,
		CursorTTL:       10 * time.Minute,
	}
}

// Validate rejects nonsensical limits at startup.
func (l Limits) Validate() error {
	checks := []struct {
		name string
		val  int
		min  int
		max  int
	}{
		{"max-concurrent-queries", l.MaxConcurrentQueries, 1, 1024},
		{"max-parallel-files", l.MaxParallelFiles, 1, 256},
		{"max-response-bytes", l.MaxResponseBytes, 1024, 64 << 20},
		{"max-request-bytes", l.MaxRequestBytes, 1024, 64 << 20},
		{"default-lines", l.DefaultLines, 1, 1_000_000},
		{"max-lines", l.MaxLines, 1, 1_000_000},
		{"default-results", l.DefaultResults, 1, 100_000},
		{"max-results", l.MaxResults, 1, 100_000},
		{"max-context-lines", l.MaxContextLines, 0, 100_000},
		{"max-files-per-query", l.MaxFilesPerQuery, 1, 100_000},
		{"max-discovery-entries", l.MaxDiscoveryEntries, 1, 10_000_000},
		{"max-list-page-size", l.MaxListPageSize, 1, 1_000_000},
		{"max-line-bytes", l.MaxLineBytes, 256, 16 << 20},
		{"max-event-bytes", l.MaxEventBytes, 256, 64 << 20},
		{"max-regex-length", l.MaxRegexLength, 8, 1 << 20},
		{"max-summary-buckets", l.MaxSummaryBuckets, 1, 100_000},
		{"max-aggregation-keys", l.MaxAggregationKeys, 1, 1_000_000},
		{"read-buffer-bytes", l.ReadBufferBytes, 1024, 16 << 20},
	}
	for _, c := range checks {
		if c.val < c.min || c.val > c.max {
			return fmt.Errorf("%s must be between %d and %d (got %d)", c.name, c.min, c.max, c.val)
		}
	}
	if l.MaxScanBytes < 1<<10 {
		return fmt.Errorf("max-scan-bytes must be at least %d (got %d)", 1<<10, l.MaxScanBytes)
	}
	if l.QueryTimeout <= 0 || l.QueryTimeout > time.Hour {
		return fmt.Errorf("query-timeout must be between 1ns and 1h (got %s)", l.QueryTimeout)
	}
	if l.CursorTTL <= 0 || l.CursorTTL > 24*time.Hour {
		return fmt.Errorf("cursor-ttl must be between 1ns and 24h (got %s)", l.CursorTTL)
	}
	if l.DefaultLines > l.MaxLines {
		return fmt.Errorf("default-lines (%d) must not exceed max-lines (%d)", l.DefaultLines, l.MaxLines)
	}
	if l.DefaultResults > l.MaxResults {
		return fmt.Errorf("default-results (%d) must not exceed max-results (%d)", l.DefaultResults, l.MaxResults)
	}
	if l.MaxEventBytes < l.MaxLineBytes {
		return fmt.Errorf("max-event-bytes (%d) must be at least max-line-bytes (%d)", l.MaxEventBytes, l.MaxLineBytes)
	}
	return nil
}

// clampLines applies the configured default and hard maximum.
func (l Limits) clampLines(n int) int {
	if n <= 0 {
		n = l.DefaultLines
	}
	if n > l.MaxLines {
		n = l.MaxLines
	}
	return n
}

func (l Limits) clampResults(n int) int {
	if n <= 0 {
		n = l.DefaultResults
	}
	if n > l.MaxResults {
		n = l.MaxResults
	}
	return n
}

func (l Limits) clampContext(n int) int {
	if n < 0 {
		return 0
	}
	if n > l.MaxContextLines {
		return l.MaxContextLines
	}
	return n
}
