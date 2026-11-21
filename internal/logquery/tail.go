package logquery

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const maxGzipTailBuffer = 16 << 20

type eventMatcher struct {
	levels map[string]bool
	query  string
}

func (m eventMatcher) match(ev *Event) bool {
	if ev == nil {
		return false
	}
	if !levelAllowed(ev.Level, m.levels) {
		return false
	}
	if m.query != "" {
		text := strings.ToLower(ev.MessageText(true))
		if !strings.Contains(text, m.query) {
			return false
		}
	}
	return true
}

type tailItem struct {
	entry Entry
	ts    *time.Time
	seq   int
}

// Tail returns the most recent matching events from the selected files.
func (s *Service) Tail(ctx context.Context, req TailRequest) (res *TailResult, err error) {
	q, err := s.begin(ctx, "logs_tail")
	if err != nil {
		return nil, err
	}
	res = &TailResult{Entries: []Entry{}, Meta: newMeta()}
	defer func() {
		if res != nil {
			q.finish(&err, &res.Meta)
		} else {
			q.finish(&err, nil)
		}
	}()

	lines := s.limits.clampLines(req.Lines)
	order, err := normalizeOrder(req.Order, "desc")
	if err != nil {
		return nil, err
	}
	includeStack := boolValue(req.IncludeStacktrace, true)

	matcher := eventMatcher{query: strings.ToLower(strings.TrimSpace(req.Query))}
	if req.Level != "" {
		level := normalizeLevel(req.Level)
		if level == "" {
			return nil, NewError(CodeInvalidArgument, fmt.Sprintf("unknown log level %q", req.Level)).
				WithHint(false, "Use TRACE, DEBUG, INFO, WARN, ERROR, FATAL or UNKNOWN.")
		}
		matcher.levels = map[string]bool{level: true}
	}

	files, err := s.selectFiles(Selection{FileID: req.FileID, Group: req.Group, FilePattern: req.FilePattern}, &res.Meta.Warnings)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return res, nil
	}

	budget := s.newBudget(s.limits.QueryTimeout)
	var items []tailItem
	remaining := lines
	seq := 0

	for i, ref := range files {
		if remaining <= 0 || budget.exhausted {
			break
		}
		filesLeft := len(files) - i
		target := remaining / filesLeft
		if target < 1 {
			target = 1
		}
		if target > remaining {
			target = remaining
		}

		src, oerr := s.openSource(ref)
		if oerr != nil {
			if len(files) == 1 {
				return nil, oerr
			}
			res.Meta.Warn(fmt.Sprintf("%s: %s", ref.Name, AsError(oerr).Message))
			res.Meta.Truncate(TruncationPartialFiles)
			continue
		}
		entries, ts, used, truncated, terr := s.tailFile(ctx, src, target, matcher, includeStack, budget)
		src.Close()
		res.Meta.ScannedFiles++
		if terr != nil {
			if isContextError(terr) {
				return nil, terr
			}
			if len(files) == 1 {
				return nil, terr
			}
			res.Meta.Warn(fmt.Sprintf("%s: %s", ref.Name, AsError(terr).Message))
			res.Meta.Truncate(TruncationPartialFiles)
			continue
		}
		for j := range entries {
			items = append(items, tailItem{entry: entries[j], ts: ts[j], seq: seq})
			seq++
		}
		remaining -= used
		if truncated {
			res.Meta.Truncate(TruncationScanLimit)
		}
	}
	if remaining <= 0 && len(files) > res.Meta.ScannedFiles {
		res.Meta.Complete = false
	}

	if order == "asc" {
		sort.SliceStable(items, func(i, j int) bool {
			a, b := items[i], items[j]
			switch {
			case a.ts != nil && b.ts != nil:
				if !a.ts.Equal(*b.ts) {
					return a.ts.Before(*b.ts)
				}
				return a.seq > b.seq
			case a.ts != nil:
				return true
			case b.ts != nil:
				return false
			default:
				return a.seq > b.seq
			}
		})
	}

	for _, item := range items {
		res.Entries = append(res.Entries, item.entry)
	}
	res.Meta.ReturnedCount = len(res.Entries)
	s.setBudgetMeta(&res.Meta, budget)
	return res, nil
}

func (s *Service) tailFile(ctx context.Context, src *fileSource, target int, matcher eventMatcher, includeStack bool, budget *scanBudget) ([]Entry, []*time.Time, int, bool, error) {
	if src.of.Compressed {
		return s.tailFileGzip(ctx, src, target, matcher, includeStack, budget)
	}

	rr, err := s.reverseReader(ctx, src, src.size, budget)
	if err != nil {
		return nil, nil, 0, false, err
	}
	scanner := newReverseEventScanner(rr, s.loc, s.limits.MaxEventBytes)

	var entries []Entry
	var times []*time.Time
	used := 0
	truncated := false
	for used < target {
		ev, serr := scanner.Next()
		if serr == io.EOF {
			break
		}
		if serr == errBudgetExceeded {
			truncated = true
			break
		}
		if serr != nil {
			return nil, nil, used, truncated, serr
		}
		if !matcher.match(ev) {
			continue
		}
		entries = append(entries, s.entryFromEvent(src.of.Ref, ev, includeStack))
		times = append(times, ev.Timestamp)
		used += len(ev.Lines)
	}
	return entries, times, used, truncated, nil
}

// tailFileGzip scans compressed input forward and keeps a bounded ring of the
// most recent matching events.
func (s *Service) tailFileGzip(ctx context.Context, src *fileSource, target int, matcher eventMatcher, includeStack bool, budget *scanBudget) ([]Entry, []*time.Time, int, bool, error) {
	fr, err := s.forwardReader(ctx, src, 0, -1, budget)
	if err != nil {
		return nil, nil, 0, false, err
	}
	scanner := newForwardEventScanner(fr, s.loc, s.limits.MaxEventBytes)

	type ringItem struct {
		entry Entry
		ts    *time.Time
		lines int
		bytes int
	}
	var ring []ringItem
	lines := 0
	bytes := 0
	truncated := false

	for {
		ev, serr := scanner.Next()
		if serr == io.EOF {
			break
		}
		if serr == errBudgetExceeded {
			truncated = true
			break
		}
		if serr != nil {
			return nil, nil, lines, truncated, serr
		}
		if !matcher.match(ev) {
			continue
		}
		entry := s.entryFromEvent(src.of.Ref, ev, includeStack)
		ring = append(ring, ringItem{entry: entry, ts: ev.Timestamp, lines: len(ev.Lines), bytes: len(entry.Message)})
		lines += len(ev.Lines)
		bytes += len(entry.Message)
		for (lines > target && len(ring) > 0) || bytes > maxGzipTailBuffer {
			lines -= ring[0].lines
			bytes -= ring[0].bytes
			ring = ring[1:]
			truncated = true
		}
	}

	entries := make([]Entry, 0, len(ring))
	times := make([]*time.Time, 0, len(ring))
	for i := len(ring) - 1; i >= 0; i-- {
		entries = append(entries, ring[i].entry)
		times = append(times, ring[i].ts)
	}
	return entries, times, lines, truncated, nil
}

func (s *Service) entryFromEvent(ref FileRef, ev *Event, includeStack bool) Entry {
	return Entry{
		FileID:     ref.ID,
		Group:      ref.Group,
		FileName:   ref.Name,
		Timestamp:  formatTimePtr(ev.Timestamp),
		Level:      levelPtr(ev.Level),
		ByteOffset: ev.Start,
		EndOffset:  ev.End,
		Message:    s.redactor.Apply(ev.MessageText(includeStack)),
		TraceID:    nonEmptyPtr(ev.TraceID),
	}
}

func isContextError(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded ||
		AsError(err).Code == CodeQueryTimeout
}
