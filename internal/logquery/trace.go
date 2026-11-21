package logquery

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Trace correlates events sharing a trace or request identifier.
func (s *Service) Trace(ctx context.Context, req TraceRequest) (res *TraceResult, err error) {
	q, err := s.begin(ctx, "logs_trace")
	if err != nil {
		return nil, err
	}
	res = &TraceResult{
		MatchedGroups: []string{},
		Timeline:      []TraceEvent{},
		Untimed:       []TraceEvent{},
		Errors:        []TraceEvent{},
		Meta:          newMeta(),
	}
	defer func() {
		if res != nil {
			q.finish(&err, &res.Meta)
		} else {
			q.finish(&err, nil)
		}
	}()

	traceID := strings.TrimSpace(req.TraceID)
	if err := validateTraceID(traceID); err != nil {
		return nil, err
	}
	idType := strings.ToLower(strings.TrimSpace(req.IDType))
	if idType == "" {
		idType = "auto"
	}
	switch idType {
	case "auto", "trace_id", "traceid", "request_id", "requestid", "correlation_id", "correlationid":
	default:
		return nil, NewError(CodeInvalidArgument, "id_type must be auto, trace_id, request_id or correlation_id")
	}
	idType = strings.ReplaceAll(idType, "_", "")

	tr, err := s.parseTimeRange(req.Since, req.StartTime, req.EndTime, 2*time.Hour)
	if err != nil {
		return nil, err
	}
	res.TimeRange = SummaryTimeRange{Start: tr.startString(s.loc), End: tr.endString(s.loc)}
	includeStack := boolValue(req.IncludeStacktrace, true)
	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > s.limits.MaxResults {
		limit = s.limits.MaxResults
		res.Meta.Warn(fmt.Sprintf("limit was capped at %d", s.limits.MaxResults))
	}

	files, err := s.traceFiles(req.Groups, req.FilePattern, &res.Meta.Warnings)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		res.TraceID = traceID
		return res, nil
	}

	fallback := compileTraceMatcher(traceID, idType)
	digest := queryDigest("logs_trace", traceID, idType, strings.Join(req.Groups, ","), req.FilePattern,
		req.Since, req.StartTime, req.EndTime, strconv.FormatBool(includeStack))

	var payload *cursorPayload
	if req.Cursor != "" {
		payload, err = s.cursors.Decode(req.Cursor)
		if err != nil {
			return nil, err
		}
		if payload.Tool != "logs_trace" || payload.Query != digest {
			return nil, NewError(CodeInvalidCursor, "the cursor does not match the current trace parameters")
		}
		files, err = s.resumeFiles(payload.Files)
		if err != nil {
			return nil, err
		}
	}
	indices := make([]int, 0, len(files))
	for i := range files {
		if payload != nil && i < payload.Index {
			continue
		}
		indices = append(indices, i)
	}

	budget := s.newBudget(s.limits.QueryTimeout)
	state := &traceState{
		traceID:  traceID,
		idType:   idType,
		fallback: fallback,
		tr:       tr,
		stack:    includeStack,
		limit:    limit,
		files:    files,
		budget:   budget,
	}

	for _, idx := range indices {
		if state.done {
			break
		}
		ref := files[idx]
		startEnd := ref.Size
		if payload != nil && idx == payload.Index {
			startEnd = payload.Offset
		}
		if terr := s.traceFile(ctx, ref, idx, startEnd, payload, state); terr != nil {
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
		res.Meta.ScannedFiles++
	}

	items := state.items
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		switch {
		case a.ts != nil && b.ts != nil:
			return a.ts.Before(*b.ts)
		case a.ts != nil:
			return true
		case b.ts != nil:
			return false
		default:
			return a.event.ByteOffset < b.event.ByteOffset
		}
	})
	groupSet := map[string]bool{}
	fileSet := map[string]bool{}
	for _, item := range items {
		if item.ts != nil {
			res.Timeline = append(res.Timeline, item.event)
		} else {
			res.Untimed = append(res.Untimed, item.event)
		}
		groupSet[item.event.Group] = true
		fileSet[item.event.FileID] = true
		if item.event.Level != nil && (*item.event.Level == "ERROR" || *item.event.Level == "FATAL") {
			res.Errors = append(res.Errors, item.event)
		}
	}
	for group := range groupSet {
		res.MatchedGroups = append(res.MatchedGroups, group)
	}
	sort.Strings(res.MatchedGroups)
	res.TraceID = traceID
	res.MatchedFiles = len(fileSet)
	res.EventCount = len(items)
	if len(res.Timeline) >= 2 {
		first := items[0]
		last := items[len(res.Timeline)-1]
		if first.ts != nil && last.ts != nil {
			ms := last.ts.Sub(*first.ts).Milliseconds()
			res.ObservedDurationMs = &ms
		}
	}

	res.Meta.ReturnedCount = len(items)
	if state.done {
		state.cursorToken = s.encodeTraceCursor(state, digest)
	}
	if state.cursorToken != "" {
		res.Meta.Truncate(TruncationResultLimit)
		res.Meta.HasMore = true
		res.Meta.NextCursor = &state.cursorToken
	} else if state.truncated {
		res.Meta.Truncate(TruncationScanLimit)
	}
	if len(res.Untimed) > 0 {
		res.Meta.Warn(fmt.Sprintf("%d event(s) had no parsable timestamp and are reported separately.", len(res.Untimed)))
	}
	res.Meta.Warn("Timeline ordering depends on synchronized clocks across the hosts that produced these logs.")
	s.setBudgetMeta(&res.Meta, budget)
	return res, nil
}

type traceItem struct {
	event TraceEvent
	ts    *time.Time
}

type traceState struct {
	traceID  string
	idType   string
	fallback func(string) bool
	tr       timeRange
	stack    bool
	limit    int
	files    []FileRef
	budget   *scanBudget

	items          []traceItem
	done           bool
	truncated      bool
	cursorIndex    int
	cursorOffset   int64
	cursorIdentity string
	cursorSize     int64
	cursorToken    string
}

func (s *Service) traceFiles(groups []string, pattern string, warnings *[]string) ([]FileRef, error) {
	all := s.reg.List()
	available := s.reg.Groups()
	groupSet := map[string]bool{}
	for _, group := range groups {
		group = strings.TrimSpace(group)
		if group == "" {
			continue
		}
		if _, ok := available[group]; !ok {
			return nil, NewError(CodeGroupNotFound, fmt.Sprintf("log group %q does not exist", group)).
				WithHint(false, "Call logs_list to discover available groups.")
		}
		groupSet[group] = true
	}

	var out []FileRef
	for _, ref := range all {
		if len(groupSet) > 0 && !groupSet[ref.Group] {
			continue
		}
		if pattern != "" {
			matched, err := matchFilePattern(ref.Name, pattern)
			if err != nil {
				return nil, err
			}
			if !matched {
				continue
			}
		}
		out = append(out, ref)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ModTime != out[j].ModTime {
			return out[i].ModTime > out[j].ModTime
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > s.limits.MaxFilesPerQuery {
		out = out[:s.limits.MaxFilesPerQuery]
		if warnings != nil {
			*warnings = append(*warnings, fmt.Sprintf("File selection was limited to %d files.", s.limits.MaxFilesPerQuery))
		}
	}
	return out, nil
}

func (s *Service) traceFile(ctx context.Context, ref FileRef, index int, startEnd int64, payload *cursorPayload, st *traceState) error {
	src, err := s.openSource(ref)
	if err != nil {
		return err
	}
	defer src.Close()

	if payload != nil && index == payload.Index {
		pinned := payload.Files[index]
		if src.of.Identity != "" && pinned.Identity != "" && src.of.Identity != pinned.Identity {
			return NewError(CodeFileRotated, "the log file was rotated since the cursor was issued").
				WithHint(false, "Restart the trace without a cursor.")
		}
		if !src.of.Compressed && src.of.Info.Size() < pinned.Size {
			return NewError(CodeFileChanged, "the log file was truncated since the cursor was issued").
				WithHint(false, "Restart the trace without a cursor.")
		}
	}
	if startEnd <= 0 {
		startEnd = src.of.Info.Size()
	}

	var scanner interface{ Next() (*Event, error) }
	if src.of.Compressed {
		fr, ferr := s.forwardReader(ctx, src, 0, -1, st.budget)
		if ferr != nil {
			return ferr
		}
		scanner = newForwardEventScanner(fr, s.loc, s.limits.MaxEventBytes)
	} else {
		rr, rerr := s.reverseReader(ctx, src, startEnd, st.budget)
		if rerr != nil {
			return rerr
		}
		scanner = newReverseEventScanner(rr, s.loc, s.limits.MaxEventBytes)
	}

	for {
		ev, serr := scanner.Next()
		if serr == io.EOF {
			return nil
		}
		if serr == errBudgetExceeded {
			st.truncated = true
			return nil
		}
		if serr != nil {
			return serr
		}
		inRange, unknown := st.tr.contains(ev.Timestamp)
		if st.tr.start != nil || st.tr.end != nil {
			if unknown {
				// An event without a timestamp may still match; include it in
				// the untimed list instead of silently dropping it.
				if !s.traceMatches(ev, st) {
					continue
				}
			} else if !inRange {
				continue
			}
		}
		if !s.traceMatches(ev, st) {
			continue
		}
		item := traceItem{
			event: TraceEvent{
				Timestamp:    formatTimePtr(ev.Timestamp),
				Group:        ref.Group,
				FileID:       ref.ID,
				FileName:     ref.Name,
				Level:        levelPtr(ev.Level),
				Message:      s.redactor.Apply(ev.Message),
				ByteOffset:   ev.Start,
				EndOffset:    ev.End,
				TraceID:      nonEmptyPtr(ev.TraceID),
				SpanID:       nonEmptyPtr(ev.SpanID),
				ParentSpanID: nonEmptyPtr(ev.ParentSpanID),
			},
			ts: ev.Timestamp,
		}
		if st.stack && len(ev.Stack) > 0 {
			item.event.Stacktrace = s.redactor.ApplyAll(ev.Stack)
		}
		st.items = append(st.items, item)
		if len(st.items) >= st.limit {
			st.done = true
			st.cursorIndex = index
			st.cursorOffset = ev.Start
			st.cursorIdentity = src.of.Identity
			st.cursorSize = src.of.Info.Size()
			return nil
		}
	}
}

func (s *Service) traceMatches(ev *Event, st *traceState) bool {
	id := st.traceID
	switch st.idType {
	case "traceid":
		if ev.TraceID == id {
			return true
		}
	case "requestid":
		if ev.RequestID == id {
			return true
		}
	case "correlationid":
		if ev.CorrelationID == id {
			return true
		}
	default:
		if ev.TraceID == id || ev.RequestID == id || ev.CorrelationID == id {
			return true
		}
	}
	if st.fallback == nil {
		return false
	}
	return st.fallback(eventSearchText(ev))
}

func compileTraceMatcher(id, idType string) func(string) bool {
	var labels string
	switch idType {
	case "traceid":
		labels = `trace[_-]?id|traceid|tid`
	case "requestid":
		labels = `request[_-]?id|requestid|req[_-]?id`
	case "correlationid":
		labels = `correlation[_-]?id|correlationid|corr[_-]?id`
	default:
		labels = `trace[_-]?id|traceid|tid|request[_-]?id|requestid|correlation[_-]?id|correlationid`
	}
	pattern := `(?i)\b(?:` + labels + `)\s*[=:]\s*["']?` + regexp.QuoteMeta(id) + `\b`
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil
	}
	return re.MatchString
}

func validateTraceID(id string) error {
	if len(id) < 4 || len(id) > 128 {
		return NewError(CodeInvalidArgument, "trace_id must be between 4 and 128 characters").
			WithHint(false, "Pass the complete identifier, not a prefix.")
	}
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			continue
		}
		switch r {
		case '.', '_', ':', '-', '/':
			continue
		}
		return NewError(CodeInvalidArgument, "trace_id contains unsupported characters")
	}
	return nil
}

func (s *Service) encodeTraceCursor(st *traceState, digest string) string {
	files := make([]CursorFile, 0, len(st.files))
	for i, ref := range st.files {
		cf := CursorFile{ID: ref.ID, Size: ref.Size}
		if i == st.cursorIndex {
			cf.Identity = st.cursorIdentity
			cf.Size = st.cursorSize
		}
		files = append(files, cf)
	}
	return s.cursors.Encode(cursorPayload{
		Tool:   "logs_trace",
		Query:  digest,
		Files:  files,
		Index:  st.cursorIndex,
		Offset: st.cursorOffset,
		Dir:    "desc",
	})
}
