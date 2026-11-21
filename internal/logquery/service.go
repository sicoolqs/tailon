package logquery

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Config configures the log query service.
type Config struct {
	Timezone        string
	DefaultEncoding string
	SupportGzip     bool
	Limits          Limits
	Redaction       RedactionConfig
}

// Service is the shared, read-only log query engine used by every MCP tool.
type Service struct {
	reg         *Registry
	limits      Limits
	loc         *time.Location
	dec         textDecoder
	supportGzip bool
	redactor    *Redactor
	cursors     *CursorCodec
	sem         chan struct{}
	stats       serviceStats
	now         func() time.Time
}

type serviceStats struct {
	toolCalls    atomic.Int64
	success      atomic.Int64
	failed       atomic.Int64
	active       atomic.Int64
	rejected     atomic.Int64
	timedOut     atomic.Int64
	truncated    atomic.Int64
	scannedBytes atomic.Int64
}

// StatsSnapshot is a point-in-time view of runtime counters.
type StatsSnapshot struct {
	ToolCalls     int64 `json:"tool_calls"`
	Success       int64 `json:"success"`
	Failed        int64 `json:"failed"`
	ActiveQueries int64 `json:"active_queries"`
	Rejected      int64 `json:"rejected"`
	TimedOut      int64 `json:"timed_out"`
	Truncated     int64 `json:"truncated"`
	ScannedBytes  int64 `json:"scanned_bytes"`
}

// NewService validates the configuration and creates the query service.
func NewService(source Source, cfg Config) (*Service, error) {
	if err := cfg.Limits.Validate(); err != nil {
		return nil, err
	}
	loc, err := loadLocation(cfg.Timezone)
	if err != nil {
		return nil, err
	}
	enc := newTextDecoder(cfg.DefaultEncoding)
	if cfg.DefaultEncoding != "" && enc.encoding == "UTF-8" {
		switch strings.ToUpper(strings.TrimSpace(cfg.DefaultEncoding)) {
		case "UTF-8", "UTF8":
		default:
			return nil, fmt.Errorf("unsupported default-encoding %q (use UTF-8, GBK or GB18030)", cfg.DefaultEncoding)
		}
	}
	redactor, err := NewRedactor(cfg.Redaction)
	if err != nil {
		return nil, err
	}
	s := &Service{
		reg:         NewRegistry(source),
		limits:      cfg.Limits,
		loc:         loc,
		dec:         enc,
		supportGzip: cfg.SupportGzip,
		redactor:    redactor,
		cursors:     NewCursorCodec(cfg.Limits.CursorTTL),
		sem:         make(chan struct{}, cfg.Limits.MaxConcurrentQueries),
		now:         time.Now,
	}
	return s, nil
}

// Limits returns the effective server limits.
func (s *Service) Limits() Limits { return s.limits }

// Timezone returns the configured timezone used for timestamp-free logs.
func (s *Service) Timezone() *time.Location { return s.loc }

// Stats returns a consistent snapshot of runtime counters.
func (s *Service) Stats() StatsSnapshot {
	return StatsSnapshot{
		ToolCalls:     s.stats.toolCalls.Load(),
		Success:       s.stats.success.Load(),
		Failed:        s.stats.failed.Load(),
		ActiveQueries: s.stats.active.Load(),
		Rejected:      s.stats.rejected.Load(),
		TimedOut:      s.stats.timedOut.Load(),
		Truncated:     s.stats.truncated.Load(),
		ScannedBytes:  s.stats.scannedBytes.Load(),
	}
}

func loadLocation(name string) (*time.Location, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.EqualFold(name, "local") {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(name)
	if err == nil {
		return loc, nil
	}
	// Minimal systems may not ship tzdata. Keep the two most common defaults
	// usable without changing binary size.
	switch name {
	case "Asia/Shanghai", "PRC", "CST":
		return time.FixedZone("CST", 8*60*60), nil
	case "UTC", "Etc/UTC", "GMT":
		return time.UTC, nil
	}
	return nil, fmt.Errorf("unknown timezone %q: %w", name, err)
}

// querySession holds the concurrency slot and timeout of one tool call.
type querySession struct {
	ctx      context.Context
	cancel   context.CancelFunc
	svc      *Service
	started  time.Time
	released bool
}

func (s *Service) begin(ctx context.Context, tool string) (*querySession, error) {
	select {
	case s.sem <- struct{}{}:
	default:
		s.stats.rejected.Add(1)
		return nil, NewError(CodeServerBusy, "the concurrent query limit has been reached").
			WithHint(true, "Retry with a narrower query.")
	}
	s.stats.toolCalls.Add(1)
	s.stats.active.Add(1)
	qctx, cancel := context.WithTimeout(ctx, s.limits.QueryTimeout)
	return &querySession{
		ctx:     qctx,
		cancel:  cancel,
		svc:     s,
		started: s.now(),
	}, nil
}

func (q *querySession) finish(errp *error, meta *Meta) {
	if q == nil || q.released {
		return
	}
	q.released = true
	q.cancel()
	<-q.svc.sem
	q.svc.stats.active.Add(-1)

	if meta != nil {
		meta.DurationMs = time.Since(q.started).Milliseconds()
		if meta.Truncated {
			q.svc.stats.truncated.Add(1)
		}
		q.svc.stats.scannedBytes.Add(meta.ScannedBytes)
	}
	if errp == nil || *errp == nil {
		q.svc.stats.success.Add(1)
		return
	}
	q.svc.stats.failed.Add(1)
	switch {
	case errors.Is(*errp, context.DeadlineExceeded):
		q.svc.stats.timedOut.Add(1)
		*errp = NewError(CodeQueryTimeout, "the query exceeded its time budget").
			WithHint(true, "Retry with a narrower time range or fewer files.")
	case errors.Is(*errp, context.Canceled):
		*errp = NewError(CodeInternalError, "the query was canceled")
	case errors.Is(*errp, errBudgetExceeded):
		*errp = NewError(CodeScanLimitExceeded, "the query exceeded its scan budget").
			WithHint(true, "Retry with a narrower time range.")
	}
}

func (s *Service) newBudget(timeout time.Duration) *scanBudget {
	return newScanBudget(s.limits.MaxScanBytes, timeout)
}

func (s *Service) setBudgetMeta(meta *Meta, budget *scanBudget) {
	if budget == nil {
		return
	}
	meta.ScannedBytes = budget.used
	if budget.exhausted {
		meta.Truncate(TruncationScanLimit)
	}
}

// Selection chooses files for a query.
type Selection struct {
	FileID      string
	Group       string
	FilePattern string
}

// selectFiles resolves a selection against the current file registry and sorts
// files from most recently modified to least recently modified.
func (s *Service) selectFiles(sel Selection, warnings *[]string) ([]FileRef, error) {
	if sel.FileID != "" {
		ref, ok := s.reg.Resolve(sel.FileID)
		if !ok {
			return nil, NewError(CodeFileNotFound, "the requested file_id is not available").
				WithHint(false, "Call logs_list to discover current file identifiers.")
		}
		return []FileRef{ref}, nil
	}

	all := s.reg.List()
	groups := s.reg.Groups()

	group := strings.TrimSpace(sel.Group)
	filterByGroup := group != "" && !strings.EqualFold(group, "all")
	if filterByGroup {
		if _, ok := groups[group]; !ok {
			return nil, NewError(CodeGroupNotFound, fmt.Sprintf("log group %q does not exist", group)).
				WithHint(false, "Call logs_list to discover available groups.")
		}
	}

	var out []FileRef
	for _, ref := range all {
		if filterByGroup && ref.Group != group {
			continue
		}
		if sel.FilePattern != "" {
			matched, err := matchFilePattern(ref.Name, sel.FilePattern)
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

func matchFilePattern(name, pattern string) (bool, error) {
	ok, err := filepath.Match(pattern, name)
	if err != nil {
		return false, NewError(CodeInvalidArgument, "file_pattern is not a valid glob pattern").
			WithHint(false, "Use a simple pattern such as *.log.")
	}
	if ok {
		return true, nil
	}
	base := filepath.Base(name)
	if base != name {
		ok, _ = filepath.Match(pattern, base)
	}
	return ok, nil
}

// fileSource owns one open file and its optional gzip decoder.
type fileSource struct {
	of   *OpenedFile
	gz   *gzip.Reader
	size int64
}

func (s *Service) openSource(ref FileRef) (*fileSource, error) {
	of, err := s.reg.Open(ref.ID, s.supportGzip)
	if err != nil {
		return nil, err
	}
	src := &fileSource{of: of, size: of.Info.Size()}
	if of.Compressed {
		gz, err := gzip.NewReader(of.File)
		if err != nil {
			_ = of.Close()
			return nil, NewError(CodeUnsupportedType, "the gzip file could not be decoded")
		}
		src.gz = gz
		src.size = -1
	}
	return src, nil
}

func (f *fileSource) Close() {
	if f == nil {
		return
	}
	if f.gz != nil {
		_ = f.gz.Close()
	}
	if f.of != nil {
		_ = f.of.Close()
	}
}

func (s *Service) forwardReader(ctx context.Context, f *fileSource, start, end int64, budget *scanBudget) (lineReader, error) {
	if f.of.Compressed {
		if start != 0 {
			return nil, NewError(CodeUnsupportedType, "byte offsets are not supported for compressed files").
				WithHint(false, "Read compressed files only from the beginning.")
		}
		return newGzipForwardReader(ctx, f.gz, s.dec, s.limits.ReadBufferBytes, s.limits.MaxLineBytes, budget), nil
	}
	if end <= 0 {
		end = f.of.Info.Size()
	}
	if start < 0 || start > end || end > f.of.Info.Size() {
		return nil, NewError(CodeInvalidArgument, "the requested byte range is outside the file")
	}
	return newForwardReader(ctx, f.of.File, start, end, s.dec, s.limits.ReadBufferBytes, s.limits.MaxLineBytes, budget), nil
}

func (s *Service) reverseReader(ctx context.Context, f *fileSource, end int64, budget *scanBudget) (lineReader, error) {
	if f.of.Compressed {
		return nil, NewError(CodeUnsupportedType, "reverse reading is not supported for compressed files").
			WithHint(false, "Use forward reads or search instead.")
	}
	if end <= 0 {
		end = f.of.Info.Size()
	}
	if end > f.of.Info.Size() {
		return nil, NewError(CodeInvalidArgument, "the requested byte range is outside the file")
	}
	return newReverseReader(ctx, f.of.File, end, s.dec, s.limits.ReadBufferBytes, s.limits.MaxLineBytes, budget), nil
}

// timeRange is a half-open [start, end) range. A nil bound is unbounded.
type timeRange struct {
	start *time.Time
	end   *time.Time
}

func (t timeRange) empty() bool { return t.start == nil && t.end == nil }

// contains returns whether ts is in range and whether the timestamp was unknown.
func (t timeRange) contains(ts *time.Time) (matched bool, unknown bool) {
	if t.empty() {
		return true, false
	}
	if ts == nil {
		return false, true
	}
	if t.start != nil && ts.Before(*t.start) {
		return false, false
	}
	if t.end != nil && !ts.Before(*t.end) {
		return false, false
	}
	return true, false
}

func (s *Service) parseTimeRange(since, startTime, endTime string, defaultSince time.Duration) (timeRange, error) {
	since = strings.TrimSpace(since)
	startTime = strings.TrimSpace(startTime)
	endTime = strings.TrimSpace(endTime)

	if since != "" && startTime != "" {
		return timeRange{}, NewError(CodeInvalidArgument, "since and start_time must not be used together")
	}

	now := s.now().In(s.loc)
	var tr timeRange
	switch {
	case since != "":
		d, err := parseFlexibleDuration(since)
		if err != nil || d <= 0 {
			return timeRange{}, NewError(CodeInvalidArgument, "since must be a positive duration such as 30m, 2h or 7d")
		}
		start := now.Add(-d)
		tr.start = &start
		tr.end = &now
	case startTime != "":
		start, err := parseAbsoluteTime(startTime, s.loc)
		if err != nil {
			return timeRange{}, err
		}
		tr.start = start
		if endTime != "" {
			end, err := parseAbsoluteTime(endTime, s.loc)
			if err != nil {
				return timeRange{}, err
			}
			tr.end = end
		}
	case endTime != "":
		end, err := parseAbsoluteTime(endTime, s.loc)
		if err != nil {
			return timeRange{}, err
		}
		tr.end = end
	case defaultSince > 0:
		start := now.Add(-defaultSince)
		tr.start = &start
		tr.end = &now
	}

	if tr.start != nil && tr.end != nil && !tr.start.Before(*tr.end) {
		return timeRange{}, NewError(CodeInvalidTimeRange, "start_time must be earlier than end_time")
	}
	return tr, nil
}

func (t timeRange) startString(loc *time.Location) string {
	if t.start == nil {
		return ""
	}
	return t.start.In(loc).Format(time.RFC3339Nano)
}

func (t timeRange) endString(loc *time.Location) string {
	if t.end == nil {
		return ""
	}
	return t.end.In(loc).Format(time.RFC3339Nano)
}

func parseAbsoluteTime(s string, loc *time.Location) (*time.Time, error) {
	if t := parseTimestampString(s, loc); t != nil {
		return t, nil
	}
	return nil, NewError(CodeInvalidTimeRange, fmt.Sprintf("invalid time value %q", s)).
		WithHint(false, "Use RFC3339, for example 2026-11-21T10:00:00+08:00.")
}

func parseFlexibleDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, errors.New("empty duration")
	}
	if strings.HasSuffix(s, "d") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil {
			return 0, err
		}
		return time.Duration(n * 24 * float64(time.Hour)), nil
	}
	return time.ParseDuration(s)
}

func parseLevels(levels []string) (map[string]bool, error) {
	if len(levels) == 0 {
		return nil, nil
	}
	set := make(map[string]bool, len(levels))
	for _, level := range levels {
		if strings.EqualFold(strings.TrimSpace(level), "UNKNOWN") {
			set["UNKNOWN"] = true
			continue
		}
		normalized := normalizeLevel(level)
		if normalized == "" {
			return nil, NewError(CodeInvalidArgument, fmt.Sprintf("unknown log level %q", level)).
				WithHint(false, "Use TRACE, DEBUG, INFO, WARN, ERROR, FATAL or UNKNOWN.")
		}
		set[normalized] = true
	}
	return set, nil
}

func levelAllowed(level string, set map[string]bool) bool {
	if set == nil {
		return true
	}
	if level == "" {
		level = "UNKNOWN"
	}
	return set[level]
}

func boolValue(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func normalizeOrder(order string, def string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(order)) {
	case "", strings.ToLower(def):
		return def, nil
	case "asc":
		return "asc", nil
	case "desc":
		return "desc", nil
	}
	return "", NewError(CodeInvalidArgument, "order must be asc or desc")
}

func formatTimestamp(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}

func fileModifiedString(ref FileRef) string {
	if ref.ModTime <= 0 {
		return ""
	}
	return time.Unix(ref.ModTime, 0).UTC().Format(time.RFC3339)
}
