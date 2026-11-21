package mcpserver

import "sync/atomic"

// Stats tracks MCP transport-level counters. Query-level counters live in the
// logquery service.
type Stats struct {
	HTTPRejected atomic.Int64
	Panics       atomic.Int64
}

type StatsSnapshot struct {
	HTTPRejected int64 `json:"http_rejected"`
	Panics       int64 `json:"panics"`
}

func (s *Stats) Snapshot() StatsSnapshot {
	if s == nil {
		return StatsSnapshot{}
	}
	return StatsSnapshot{
		HTTPRejected: s.HTTPRejected.Load(),
		Panics:       s.Panics.Load(),
	}
}
