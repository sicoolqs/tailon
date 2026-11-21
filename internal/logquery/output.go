package logquery

import (
	"encoding/json"
	"strings"
)

// FitResponse reduces a result until its serialized JSON fits maxBytes. It
// reports whether the result was modified. Truncation is always recorded in
// the result metadata; offsets that can no longer be trusted lose their cursor.
func FitResponse(v any, maxBytes int) bool {
	if maxBytes <= 0 || jsonSize(v) <= maxBytes {
		return false
	}
	changed := false
	for jsonSize(v) > maxBytes {
		if !shrinkOnce(v) {
			break
		}
		changed = true
	}
	if changed {
		markResponseTruncated(v)
	}
	return changed
}

func jsonSize(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 1 << 30
	}
	return len(b)
}

func shrinkOnce(v any) bool {
	switch r := v.(type) {
	case *ListResult:
		if len(r.Files) == 0 {
			return false
		}
		r.Files = r.Files[:len(r.Files)/2]
		r.Meta.ReturnedCount = len(r.Files)
		return true
	case *TailResult:
		if len(r.Entries) == 0 {
			return false
		}
		r.Entries = r.Entries[:len(r.Entries)/2]
		r.Meta.ReturnedCount = len(r.Entries)
		return true
	case *SearchResult:
		if len(r.Results) == 0 {
			return false
		}
		r.Results = r.Results[:len(r.Results)/2]
		r.Meta.ReturnedCount = len(r.Results)
		return true
	case *ContextResult:
		if len(r.Lines) == 0 {
			return false
		}
		r.Lines = r.Lines[:len(r.Lines)/2]
		r.Meta.ReturnedCount = len(r.Lines)
		return true
	case *ReadResult:
		if r.Content == "" {
			return false
		}
		r.Content = truncateAtLineBoundary(r.Content, len(r.Content)/2)
		r.ReturnedLines = strings.Count(r.Content, "\n") + 1
		if r.Content == "" {
			r.ReturnedLines = 0
		}
		r.EOF = false
		r.NextCursor = nil
		r.Meta.HasMore = false
		r.Meta.NextCursor = nil
		r.Meta.ReturnedCount = r.ReturnedLines
		return true
	case *SummaryResult:
		switch {
		case len(r.Timeline) > 0:
			r.Timeline = r.Timeline[:len(r.Timeline)/2]
			return true
		case len(r.TopErrors) > 0:
			for i := range r.TopErrors {
				r.TopErrors[i].Example = nil
			}
			if len(r.TopErrors) > 1 {
				r.TopErrors = r.TopErrors[:len(r.TopErrors)/2]
			} else {
				r.TopErrors = nil
			}
			r.Meta.ReturnedCount = len(r.TopErrors)
			return true
		case len(r.ByFile) > 0:
			r.ByFile = r.ByFile[:len(r.ByFile)/2]
			return true
		}
		return false
	case *TraceResult:
		switch {
		case len(r.Timeline) > 0:
			r.Timeline = r.Timeline[:len(r.Timeline)/2]
			r.EventCount = len(r.Timeline) + len(r.Untimed)
			r.Meta.ReturnedCount = r.EventCount
			return true
		case len(r.Untimed) > 0:
			r.Untimed = nil
			r.EventCount = len(r.Timeline)
			r.Meta.ReturnedCount = r.EventCount
			return true
		case len(r.Errors) > 0:
			r.Errors = nil
			return true
		}
		return false
	case *StatResult:
		return false
	}
	return false
}

func markResponseTruncated(v any) {
	switch r := v.(type) {
	case *ListResult:
		r.Meta.Truncate(TruncationResponseLimit)
	case *TailResult:
		r.Meta.Truncate(TruncationResponseLimit)
	case *SearchResult:
		r.Meta.Truncate(TruncationResponseLimit)
	case *ContextResult:
		r.Meta.Truncate(TruncationResponseLimit)
	case *ReadResult:
		r.Meta.Truncate(TruncationResponseLimit)
	case *SummaryResult:
		r.Meta.Truncate(TruncationResponseLimit)
	case *TraceResult:
		r.Meta.Truncate(TruncationResponseLimit)
	}
}

func truncateAtLineBoundary(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := s[:limit]
	if idx := strings.LastIndexByte(cut, '\n'); idx >= 0 {
		return cut[:idx]
	}
	// Avoid splitting a UTF-8 sequence.
	for limit > 0 && (s[limit]&0xC0) == 0x80 {
		limit--
	}
	return s[:limit]
}
