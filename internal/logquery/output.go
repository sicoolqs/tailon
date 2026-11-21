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
		if truncateTailMessages(r) {
			return true
		}
		if len(r.Entries) == 0 {
			return false
		}
		r.Entries = r.Entries[:len(r.Entries)/2]
		r.Meta.ReturnedCount = len(r.Entries)
		return true
	case *SearchResult:
		if stripSearchDetails(r) {
			return true
		}
		if len(r.Results) == 0 {
			return false
		}
		r.Results = r.Results[:len(r.Results)/2]
		r.Meta.ReturnedCount = len(r.Results)
		return true
	case *ContextResult:
		if truncateContextLines(r) {
			return true
		}
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
		if stripTraceDetails(r) {
			return true
		}
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
		markResponseMeta(&r.Meta)
	case *TailResult:
		markResponseMeta(&r.Meta)
	case *SearchResult:
		markResponseMeta(&r.Meta)
	case *ContextResult:
		markResponseMeta(&r.Meta)
	case *ReadResult:
		markResponseMeta(&r.Meta)
	case *SummaryResult:
		markResponseMeta(&r.Meta)
	case *TraceResult:
		markResponseMeta(&r.Meta)
	}
}

func markResponseMeta(m *Meta) {
	m.Complete = false
	m.Truncated = true
	// Response trimming is the most immediate reason, even if a result limit
	// was also reached during the scan.
	m.TruncationReason = TruncationResponseLimit
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

// responseMessageLimit is the point at which response trimming shortens a
// message before dropping an entire result.
const responseMessageLimit = 512

func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	if cut <= 0 {
		return "..."
	}
	return s[:cut] + "..."
}

func truncateTailMessages(r *TailResult) bool {
	changed := false
	for i := range r.Entries {
		if len(r.Entries[i].Message) > responseMessageLimit {
			r.Entries[i].Message = truncateUTF8(r.Entries[i].Message, responseMessageLimit)
			changed = true
		}
	}
	return changed
}

func stripSearchDetails(r *SearchResult) bool {
	changed := false
	for i := range r.Results {
		h := &r.Results[i]
		if len(h.Stacktrace) > 0 {
			h.Stacktrace = nil
			changed = true
		}
		if len(h.ContextBefore) > 0 {
			h.ContextBefore = []string{}
			changed = true
		}
		if len(h.ContextAfter) > 0 {
			h.ContextAfter = []string{}
			changed = true
		}
	}
	if changed {
		return true
	}
	for i := range r.Results {
		if len(r.Results[i].Message) > responseMessageLimit {
			r.Results[i].Message = truncateUTF8(r.Results[i].Message, responseMessageLimit)
			changed = true
		}
	}
	return changed
}

func truncateContextLines(r *ContextResult) bool {
	changed := false
	for i := range r.Lines {
		if len(r.Lines[i].Text) > responseMessageLimit {
			r.Lines[i].Text = truncateUTF8(r.Lines[i].Text, responseMessageLimit)
			changed = true
		}
	}
	return changed
}

func stripTraceDetails(r *TraceResult) bool {
	changed := false
	lists := [][]TraceEvent{r.Timeline, r.Untimed, r.Errors}
	for _, list := range lists {
		for i := range list {
			if len(list[i].Stacktrace) > 0 {
				list[i].Stacktrace = nil
				changed = true
			}
		}
	}
	if changed {
		return true
	}
	for _, list := range lists {
		for i := range list {
			if len(list[i].Message) > responseMessageLimit {
				list[i].Message = truncateUTF8(list[i].Message, responseMessageLimit)
				changed = true
			}
		}
	}
	return changed
}
