package logquery

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// Search scans selected files for literal or regular expression matches.
func (s *Service) Search(ctx context.Context, req SearchRequest) (res *SearchResult, err error) {
	q, err := s.begin(ctx, "logs_search")
	if err != nil {
		return nil, err
	}
	res = &SearchResult{Results: []SearchHit{}, Meta: newMeta()}
	defer func() {
		if res != nil {
			q.finish(&err, &res.Meta)
		} else {
			q.finish(&err, nil)
		}
	}()

	mode := strings.ToLower(strings.TrimSpace(req.MatchMode))
	if mode == "" {
		mode = "literal"
	}
	if mode != "literal" && mode != "regex" {
		return nil, NewError(CodeInvalidArgument, "match_mode must be literal or regex")
	}
	caseSensitive := boolValue(req.CaseSensitive, false)
	includeStack := boolValue(req.IncludeStacktrace, true)
	order, err := normalizeOrder(req.Order, "desc")
	if err != nil {
		return nil, err
	}
	limit := s.limits.clampResults(req.Limit)
	if req.Limit <= 0 {
		limit = s.limits.DefaultResults
	}
	if req.Limit > s.limits.MaxResults {
		res.Meta.Warn(fmt.Sprintf("limit was capped at %d", s.limits.MaxResults))
	}

	levelSet, err := parseLevels(req.Levels)
	if err != nil {
		return nil, err
	}
	tr, err := s.parseTimeRange(req.Since, req.StartTime, req.EndTime, 0)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Query) == "" && tr.empty() && levelSet == nil {
		return nil, NewError(CodeInvalidArgument, "an empty query requires a time range or a level filter").
			WithHint(false, "Provide query, since/start_time, or levels.")
	}

	matcher, err := compileMatcher(req.Query, mode, caseSensitive, s.limits.MaxRegexLength)
	if err != nil {
		return nil, err
	}

	before := s.limits.clampContext(req.ContextBefore)
	after := s.limits.clampContext(req.ContextAfter)

	files, err := s.selectFiles(Selection{FileID: req.FileID, Group: req.Group, FilePattern: req.FilePattern}, &res.Meta.Warnings)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return res, nil
	}

	digest := queryDigest(
		"logs_search", req.FileID, req.Group, req.FilePattern, req.Query, mode,
		strconv.FormatBool(caseSensitive), req.Since, req.StartTime, req.EndTime,
		strings.Join(req.Levels, ","), strconv.Itoa(before), strconv.Itoa(after),
		strconv.FormatBool(includeStack), order,
	)

	var payload *cursorPayload
	if req.Cursor != "" {
		payload, err = s.cursors.Decode(req.Cursor)
		if err != nil {
			return nil, err
		}
		if payload.Tool != "logs_search" || payload.Query != digest {
			return nil, NewError(CodeInvalidCursor, "the cursor does not match the current search parameters").
				WithHint(false, "Restart the search without a cursor.")
		}
		files, err = s.resumeFiles(payload.Files)
		if err != nil {
			return nil, err
		}
	}

	budget := s.newBudget(s.limits.QueryTimeout)
	state := &searchState{
		limit:   limit,
		order:   order,
		digest:  digest,
		payload: payload,
		tr:      tr,
		levels:  levelSet,
		matcher: matcher,
		before:  before,
		after:   after,
		stack:   includeStack,
		budget:  budget,
		files:   files,
	}

	indices := make([]int, len(files))
	for i := range files {
		indices[i] = i
	}
	if order == "asc" {
		reverseInts(indices)
	}
	if payload != nil {
		filtered := indices[:0]
		for _, idx := range indices {
			if order == "desc" && idx < payload.Index {
				continue
			}
			if order == "asc" && idx > payload.Index {
				continue
			}
			filtered = append(filtered, idx)
		}
		indices = filtered
	}
	for _, idx := range indices {
		if state.done {
			break
		}
		ref := files[idx]
		startEnd := int64(0)
		if order == "desc" {
			startEnd = ref.Size
		}
		if payload != nil && idx == payload.Index {
			startEnd = payload.Offset
		}
		if err := s.searchFile(ctx, ref, idx, startEnd, state); err != nil {
			if isContextError(err) {
				return nil, err
			}
			if len(files) == 1 {
				return nil, err
			}
			res.Meta.Warn(fmt.Sprintf("%s: %s", ref.Name, AsError(err).Message))
			res.Meta.Truncate(TruncationPartialFiles)
			continue
		}
		res.Meta.ScannedFiles++
	}

	res.Results = state.results
	res.Meta.ReturnedCount = len(res.Results)
	if state.done {
		state.cursorToken = s.encodeSearchCursor(state, digest, order)
	}
	if state.compressedNoContext {
		res.Meta.Warn("Context lines are not available for compressed files.")
	}
	if state.unknownTimestamps > 0 {
		res.Meta.Warn(fmt.Sprintf("%d event(s) without a parsable timestamp were excluded by the time filter.", state.unknownTimestamps))
	}
	if state.cursorToken != "" {
		res.Meta.Truncate(TruncationResultLimit)
		res.Meta.HasMore = true
		res.Meta.NextCursor = &state.cursorToken
	} else if state.resultLimited {
		res.Meta.Truncate(TruncationResultLimit)
	} else if state.truncated {
		res.Meta.Truncate(TruncationScanLimit)
	}
	if res.Meta.Truncated && order == "desc" && len(files) > 1 {
		res.Meta.Warn("Results cover only the scanned range; matches in unscanned older data may exist.")
	}
	s.setBudgetMeta(&res.Meta, budget)
	return res, nil
}

type searchState struct {
	limit   int
	order   string
	digest  string
	payload *cursorPayload
	tr      timeRange
	levels  map[string]bool
	matcher func(string) bool
	before  int
	after   int
	stack   bool
	budget  *scanBudget
	files   []FileRef

	results             []SearchHit
	unknownTimestamps   int
	done                bool
	truncated           bool
	resultLimited       bool
	cursorToken         string
	cursorIndex         int
	cursorOffset        int64
	cursorIdentity      string
	cursorSize          int64
	compressedNoContext bool
}

func (s *Service) searchFile(ctx context.Context, ref FileRef, index int, startEnd int64, st *searchState) error {
	src, err := s.openSource(ref)
	if err != nil {
		return err
	}
	defer src.Close()

	if st.payload != nil && index == st.payload.Index {
		pinned := st.payload.Files[index]
		if src.of.Identity != "" && pinned.Identity != "" && src.of.Identity != pinned.Identity {
			return NewError(CodeFileRotated, "the log file was rotated since the cursor was issued").
				WithHint(false, "Restart the search without a cursor.")
		}
		if !src.of.Compressed && src.of.Info.Size() < pinned.Size {
			return NewError(CodeFileChanged, "the log file was truncated since the cursor was issued").
				WithHint(false, "Restart the search without a cursor.")
		}
	}

	coveredLow, coveredHigh := int64(1), int64(-1)

	if st.order == "desc" && !src.of.Compressed {
		if startEnd <= 0 {
			startEnd = src.of.Info.Size()
		}
		rr, rerr := s.reverseReader(ctx, src, startEnd, st.budget)
		if rerr != nil {
			return rerr
		}
		scanner := newReverseEventScanner(rr, s.loc, s.limits.MaxEventBytes)
		for {
			ev, serr := scanner.Next()
			if serr == io.EOF {
				break
			}
			if serr == errBudgetExceeded {
				st.truncated = true
				st.cursorIndex = index
				st.cursorOffset = 0
				return nil
			}
			if serr != nil {
				return serr
			}
			matched, unknown := s.eventMatches(ev, st)
			if unknown {
				st.unknownTimestamps++
			}
			if !matched {
				continue
			}
			hit := s.searchHit(ctx, src, ev, st, &coveredLow, &coveredHigh)
			st.results = append(st.results, hit)
			if len(st.results) >= st.limit {
				st.done = true
				st.cursorIndex = index
				st.cursorOffset = ev.Start
				st.cursorIdentity = src.of.Identity
				st.cursorSize = src.of.Info.Size()
				return nil
			}
		}
		return nil
	}

	// Forward scan: ascending searches, and all compressed files.
	if startEnd < 0 {
		startEnd = 0
	}
	start := int64(0)
	if st.order == "desc" && src.of.Compressed {
		start = 0
	} else if st.payload != nil && index == st.payload.Index {
		start = st.payload.Offset
	}
	fr, rerr := s.forwardReader(ctx, src, start, -1, st.budget)
	if rerr != nil {
		return rerr
	}
	scanner := newForwardEventScanner(fr, s.loc, s.limits.MaxEventBytes)

	if st.order == "desc" {
		// Compressed files cannot be read backwards. Keep the newest matches
		// in a ring while scanning forward.
		type ringItem struct {
			hit SearchHit
			ev  *Event
		}
		var ring []ringItem
		for {
			ev, serr := scanner.Next()
			if serr == io.EOF {
				break
			}
			if serr == errBudgetExceeded {
				st.truncated = true
				break
			}
			if serr != nil {
				return serr
			}
			matched, unknown := s.eventMatches(ev, st)
			if unknown {
				st.unknownTimestamps++
			}
			if !matched {
				continue
			}
			low, high := int64(1), int64(-1)
			hit := s.searchHit(ctx, src, ev, st, &low, &high)
			ring = append(ring, ringItem{hit: hit, ev: ev})
			if len(ring) > st.limit {
				ring = ring[1:]
				st.resultLimited = true
			}
		}
		for i := len(ring) - 1; i >= 0; i-- {
			st.results = append(st.results, ring[i].hit)
		}
		if st.truncated {
			st.cursorToken = ""
		}
		return nil
	}

	for {
		ev, serr := scanner.Next()
		if serr == io.EOF {
			break
		}
		if serr == errBudgetExceeded {
			st.truncated = true
			st.cursorIndex = index
			st.cursorOffset = src.size
			return nil
		}
		if serr != nil {
			return serr
		}
		matched, unknown := s.eventMatches(ev, st)
		if unknown {
			st.unknownTimestamps++
		}
		if !matched {
			continue
		}
		hit := s.searchHit(ctx, src, ev, st, &coveredLow, &coveredHigh)
		st.results = append(st.results, hit)
		if len(st.results) >= st.limit {
			st.done = true
			st.cursorIndex = index
			st.cursorOffset = ev.End
			st.cursorIdentity = src.of.Identity
			st.cursorSize = src.of.Info.Size()
			return nil
		}
	}
	return nil
}

func (s *Service) eventMatches(ev *Event, st *searchState) (bool, bool) {
	matchedRange, unknown := st.tr.contains(ev.Timestamp)
	if !matchedRange {
		return false, unknown
	}
	if !levelAllowed(ev.Level, st.levels) {
		return false, false
	}
	if st.matcher != nil && !st.matcher(eventSearchText(ev)) {
		return false, false
	}
	return true, false
}

func eventSearchText(ev *Event) string {
	var b strings.Builder
	for _, line := range ev.Lines {
		b.WriteString(line.Text)
		b.WriteByte('\n')
	}
	if ev.IsJSON {
		for _, line := range ev.Stack {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func (s *Service) searchHit(ctx context.Context, src *fileSource, ev *Event, st *searchState, coveredLow, coveredHigh *int64) SearchHit {
	hit := SearchHit{
		FileID:        src.of.Ref.ID,
		Group:         src.of.Ref.Group,
		FileName:      src.of.Ref.Name,
		Timestamp:     formatTimePtr(ev.Timestamp),
		Level:         levelPtr(ev.Level),
		ByteOffset:    ev.Start,
		EndOffset:     ev.End,
		Message:       s.redactor.Apply(ev.Message),
		TraceID:       nonEmptyPtr(ev.TraceID),
		ContextBefore: []string{},
		ContextAfter:  []string{},
	}
	if st.stack && len(ev.Stack) > 0 {
		hit.Stacktrace = s.redactor.ApplyAll(ev.Stack)
	}
	if st.before <= 0 && st.after <= 0 {
		return hit
	}
	if src.of.Compressed {
		st.compressedNoContext = true
		return hit
	}
	beforeLines, afterLines := s.contextLines(ctx, src, ev.Start, ev.End, st.before, st.after, st.budget)
	// Avoid returning the same context bytes twice when matches are adjacent.
	for _, line := range beforeLines {
		if line.Offset >= *coveredLow && line.EndOffset <= *coveredHigh {
			continue
		}
		hit.ContextBefore = append(hit.ContextBefore, line.Text)
		updateCovered(line.Offset, line.EndOffset, coveredLow, coveredHigh)
	}
	for _, line := range afterLines {
		if line.Offset >= *coveredLow && line.EndOffset <= *coveredHigh {
			continue
		}
		hit.ContextAfter = append(hit.ContextAfter, line.Text)
		updateCovered(line.Offset, line.EndOffset, coveredLow, coveredHigh)
	}
	return hit
}

func updateCovered(low, high int64, coveredLow, coveredHigh *int64) {
	if low < *coveredLow {
		*coveredLow = low
	}
	if high > *coveredHigh {
		*coveredHigh = high
	}
}

func (s *Service) contextLines(ctx context.Context, src *fileSource, start, end int64, before, after int, budget *scanBudget) ([]ContextLine, []ContextLine) {
	var beforeOut, afterOut []ContextLine
	if before > 0 && start > 0 {
		rr, err := s.reverseReader(ctx, src, start, budget)
		if err == nil {
			for i := 0; i < before; i++ {
				line, lerr := rr.Next()
				if lerr != nil {
					break
				}
				beforeOut = append(beforeOut, ContextLine{
					Offset: line.Start, EndOffset: line.End, Text: s.redactor.Apply(line.Text),
				})
			}
		}
		for i, j := 0, len(beforeOut)-1; i < j; i, j = i+1, j-1 {
			beforeOut[i], beforeOut[j] = beforeOut[j], beforeOut[i]
		}
	}
	if after > 0 {
		fr, err := s.forwardReader(ctx, src, end, -1, budget)
		if err == nil {
			for i := 0; i < after; i++ {
				line, lerr := fr.Next()
				if lerr != nil {
					break
				}
				afterOut = append(afterOut, ContextLine{
					Offset: line.Start, EndOffset: line.End, Text: s.redactor.Apply(line.Text),
				})
			}
		}
	}
	return beforeOut, afterOut
}

func compileMatcher(query, mode string, caseSensitive bool, maxRegexLen int) (func(string) bool, error) {
	if query == "" {
		return nil, nil
	}
	if mode == "regex" {
		if maxRegexLen <= 0 {
			maxRegexLen = 4096
		}
		if len(query) > maxRegexLen {
			return nil, NewError(CodeInvalidArgument, "the regular expression is too long").
				WithHint(false, "Keep regular expressions within the configured max-regex-length.")
		}
		pattern := query
		if !caseSensitive {
			pattern = "(?i)" + pattern
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, NewError(CodeInvalidRegex, "the regular expression is invalid").
				WithHint(false, "Use Go (RE2) regular expression syntax.")
		}
		return re.MatchString, nil
	}
	if caseSensitive {
		return func(s string) bool { return strings.Contains(s, query) }, nil
	}
	needle := strings.ToLower(query)
	return func(s string) bool { return strings.Contains(strings.ToLower(s), needle) }, nil
}

func (s *Service) resumeFiles(pinned []CursorFile) ([]FileRef, error) {
	if len(pinned) == 0 {
		return nil, NewError(CodeInvalidCursor, "the cursor contains no files")
	}
	out := make([]FileRef, 0, len(pinned))
	for _, p := range pinned {
		ref, ok := s.reg.Resolve(p.ID)
		if !ok {
			return nil, NewError(CodeFileRotated, "a file referenced by the cursor is no longer available").
				WithHint(false, "Restart the search without a cursor.")
		}
		out = append(out, ref)
	}
	return out, nil
}

func reverseInts(v []int) {
	for i, j := 0, len(v)-1; i < j; i, j = i+1, j-1 {
		v[i], v[j] = v[j], v[i]
	}
}

func (s *Service) encodeSearchCursor(st *searchState, digest, order string) string {
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
		Tool:   "logs_search",
		Query:  digest,
		Files:  files,
		Index:  st.cursorIndex,
		Offset: st.cursorOffset,
		Dir:    order,
	})
}
