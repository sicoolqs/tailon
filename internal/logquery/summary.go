package logquery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Summary aggregates level counts, error fingerprints, per-file statistics and
// a bucketed timeline over a time range.
func (s *Service) Summary(ctx context.Context, req SummaryRequest) (res *SummaryResult, err error) {
	q, err := s.begin(ctx, "logs_summary")
	if err != nil {
		return nil, err
	}
	res = &SummaryResult{
		Levels:    map[string]int64{},
		TopErrors: []ErrorGroup{},
		ByFile:    []FileSummary{},
		Timeline:  []TimeBucket{},
		Meta:      newMeta(),
	}
	defer func() {
		if res != nil {
			q.finish(&err, &res.Meta)
		} else {
			q.finish(&err, nil)
		}
	}()

	levelSet, err := parseLevels(req.Levels)
	if err != nil {
		return nil, err
	}
	tr, err := s.parseTimeRange(req.Since, req.StartTime, req.EndTime, time.Hour)
	if err != nil {
		return nil, err
	}
	res.TimeRange = SummaryTimeRange{Start: tr.startString(s.loc), End: tr.endString(s.loc)}
	res.Group = strings.TrimSpace(req.Group)
	if strings.EqualFold(res.Group, "all") {
		res.Group = ""
	}

	bucket := 5 * time.Minute
	if strings.TrimSpace(req.Bucket) != "" {
		d, derr := parseFlexibleDuration(req.Bucket)
		if derr != nil || d <= 0 {
			return nil, NewError(CodeInvalidArgument, "bucket must be a positive duration such as 1m, 5m or 1h")
		}
		bucket = d
	}
	if tr.start != nil && tr.end != nil {
		count := int(tr.end.Sub(*tr.start) / bucket)
		if count > s.limits.MaxSummaryBuckets {
			return nil, NewError(CodeInvalidArgument, "bucket would create too many time buckets").
				WithHint(false, fmt.Sprintf("Use a larger bucket or a shorter range; the limit is %d buckets.", s.limits.MaxSummaryBuckets))
		}
	}

	topN := req.TopN
	if topN <= 0 {
		topN = 10
	}
	if topN > s.limits.MaxAggregationKeys {
		topN = s.limits.MaxAggregationKeys
	}
	includeExamples := boolValue(req.IncludeExamples, true)

	files, err := s.selectFiles(Selection{FileID: req.FileID, Group: req.Group, FilePattern: req.FilePattern}, &res.Meta.Warnings)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return res, nil
	}

	budget := s.newBudget(s.limits.QueryTimeout)
	state := newSummaryState(s.limits, bucket, s.loc, s.redactor, levelSet, includeExamples)
	for _, ref := range files {
		src, oerr := s.openSource(ref)
		if oerr != nil {
			if len(files) == 1 {
				return nil, oerr
			}
			res.Meta.Warn(fmt.Sprintf("%s: %s", ref.Name, AsError(oerr).Message))
			res.Meta.Truncate(TruncationPartialFiles)
			continue
		}
		serr := s.summaryFile(ctx, src, tr, state, budget)
		src.Close()
		if serr != nil {
			if isContextError(serr) {
				return nil, serr
			}
			if len(files) == 1 {
				return nil, serr
			}
			res.Meta.Warn(fmt.Sprintf("%s: %s", ref.Name, AsError(serr).Message))
			res.Meta.Truncate(TruncationPartialFiles)
			continue
		}
		res.Meta.ScannedFiles++
	}

	if state.unknownTimestamps > 0 {
		res.Meta.Warn(fmt.Sprintf("%d event(s) without a parsable timestamp were excluded by the time filter.", state.unknownTimestamps))
	}
	if state.groupOverflow {
		res.Meta.Warn(fmt.Sprintf("Error aggregation stopped at %d distinct fingerprints.", s.limits.MaxAggregationKeys))
	}
	if state.bucketOverflow {
		res.Meta.Warn(fmt.Sprintf("Timeline was limited to %d buckets.", s.limits.MaxSummaryBuckets))
	}

	for level, count := range state.levels {
		if count > 0 {
			res.Levels[level] = count
		}
	}
	res.TopErrors = state.topErrors(topN)
	res.ByFile = state.byFile()
	res.Timeline = state.timeline()
	res.Meta.ReturnedCount = len(res.TopErrors)
	if state.truncated {
		res.Meta.Truncate(TruncationScanLimit)
	}
	s.setBudgetMeta(&res.Meta, budget)
	return res, nil
}

func (s *Service) summaryFile(ctx context.Context, src *fileSource, tr timeRange, st *summaryState, budget *scanBudget) error {
	var scanner interface {
		Next() (*Event, error)
	}
	if src.of.Compressed {
		fr, err := s.forwardReader(ctx, src, 0, -1, budget)
		if err != nil {
			return err
		}
		scanner = newForwardEventScanner(fr, s.loc, s.limits.MaxEventBytes)
	} else {
		rr, err := s.reverseReader(ctx, src, src.size, budget)
		if err != nil {
			return err
		}
		scanner = newReverseEventScanner(rr, s.loc, s.limits.MaxEventBytes)
	}

	for {
		ev, err := scanner.Next()
		if err == io.EOF {
			return nil
		}
		if err == errBudgetExceeded {
			st.truncated = true
			return nil
		}
		if err != nil {
			return err
		}
		if ev == nil {
			continue
		}
		inRange, unknown := tr.contains(ev.Timestamp)
		if unknown {
			st.unknownTimestamps++
			continue
		}
		if !inRange {
			continue
		}
		if !levelAllowed(ev.Level, st.levelSet) {
			continue
		}
		st.add(src.of.Ref, ev)
	}
}

type summaryState struct {
	limits   Limits
	bucket   time.Duration
	loc      *time.Location
	redactor *Redactor
	levelSet map[string]bool
	examples bool

	levels            map[string]int64
	files             map[string]*FileSummary
	groups            map[string]*errorAccumulator
	buckets           map[int64]*TimeBucket
	groupOverflow     bool
	bucketOverflow    bool
	unknownTimestamps int
	truncated         bool
	fileOrder         []string
}

type errorAccumulator struct {
	fingerprint string
	exception   string
	count       int64
	first       *time.Time
	last        *time.Time
	files       map[string]bool
	example     *ErrorExample
}

func newSummaryState(limits Limits, bucket time.Duration, loc *time.Location, redactor *Redactor, levelSet map[string]bool, examples bool) *summaryState {
	return &summaryState{
		limits:   limits,
		bucket:   bucket,
		loc:      loc,
		redactor: redactor,
		levelSet: levelSet,
		examples: examples,
		levels:   map[string]int64{},
		files:    map[string]*FileSummary{},
		groups:   map[string]*errorAccumulator{},
		buckets:  map[int64]*TimeBucket{},
	}
}

// buckets is kept separately so summaryState literals stay readable.
func (st *summaryState) initBuckets() {
	if st.buckets == nil {
		st.buckets = map[int64]*TimeBucket{}
	}
}

func (st *summaryState) add(ref FileRef, ev *Event) {
	if st.files == nil {
		st.files = map[string]*FileSummary{}
	}
	level := ev.Level
	if level == "" {
		level = "UNKNOWN"
	}
	st.levels[level]++

	fs := st.files[ref.ID]
	if fs == nil {
		fs = &FileSummary{FileID: ref.ID, FileName: ref.Name}
		st.files[ref.ID] = fs
		st.fileOrder = append(st.fileOrder, ref.ID)
	}
	fs.Total++
	switch level {
	case "ERROR", "FATAL":
		fs.Errors++
	case "WARN":
		fs.Warnings++
	case "UNKNOWN":
		fs.Unknown++
	}

	st.initBuckets()
	if ev.Timestamp != nil {
		key := ev.Timestamp.Truncate(st.bucket).Unix()
		b := st.buckets[key]
		if b == nil {
			if len(st.buckets) >= st.limits.MaxSummaryBuckets {
				st.bucketOverflow = true
			} else {
				b = &TimeBucket{Start: ev.Timestamp.Truncate(st.bucket).In(st.loc).Format(time.RFC3339)}
				st.buckets[key] = b
			}
		}
		if b != nil {
			b.Total++
			switch level {
			case "ERROR", "FATAL":
				b.Errors++
			case "WARN":
				b.Warnings++
			}
		}
	}

	if level != "ERROR" && level != "FATAL" {
		return
	}
	fp, exception := fingerprintEvent(ev)
	acc := st.groups[fp]
	if acc == nil {
		if len(st.groups) >= st.limits.MaxAggregationKeys {
			st.groupOverflow = true
			return
		}
		acc = &errorAccumulator{fingerprint: fp, exception: st.redactor.Apply(exception), files: map[string]bool{}}
		st.groups[fp] = acc
	}
	acc.count++
	acc.files[ref.ID] = true
	if ev.Timestamp != nil {
		if acc.first == nil || ev.Timestamp.Before(*acc.first) {
			t := *ev.Timestamp
			acc.first = &t
		}
		if acc.last == nil || ev.Timestamp.After(*acc.last) {
			t := *ev.Timestamp
			acc.last = &t
		}
	}
	if st.examples && acc.example == nil {
		acc.example = &ErrorExample{
			FileID:     ref.ID,
			FileName:   ref.Name,
			ByteOffset: ev.Start,
			Timestamp:  formatTimePtr(ev.Timestamp),
			Level:      levelPtr(ev.Level),
			Message:    st.redactor.Apply(ev.Message),
		}
	}
}

func (st *summaryState) topErrors(n int) []ErrorGroup {
	out := make([]ErrorGroup, 0, len(st.groups))
	for _, acc := range st.groups {
		out = append(out, ErrorGroup{
			Fingerprint:   acc.fingerprint,
			Exception:     acc.exception,
			Count:         acc.count,
			FirstSeen:     formatTimePtr(acc.first),
			LastSeen:      formatTimePtr(acc.last),
			AffectedFiles: len(acc.files),
			Example:       acc.example,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Fingerprint < out[j].Fingerprint
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func (st *summaryState) byFile() []FileSummary {
	out := make([]FileSummary, 0, len(st.files))
	for _, id := range st.fileOrder {
		if fs := st.files[id]; fs != nil {
			out = append(out, *fs)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Errors != out[j].Errors {
			return out[i].Errors > out[j].Errors
		}
		return out[i].FileName < out[j].FileName
	})
	return out
}

func (st *summaryState) timeline() []TimeBucket {
	keys := make([]int64, 0, len(st.buckets))
	for k := range st.buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]TimeBucket, 0, len(keys))
	for _, k := range keys {
		out = append(out, *st.buckets[k])
	}
	return out
}

var (
	fpTimestampRE = regexp.MustCompile(`\d{4}[-/]\d{2}[-/]\d{2}[T ]\d{2}:\d{2}:\d{2}(?:[.,]\d+)?`)
	fpUUIDRE      = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	fpHexRE       = regexp.MustCompile(`(?i)\b[0-9a-f]{8,}\b`)
	fpIPRE        = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
	fpNumberRE    = regexp.MustCompile(`\b\d+\b`)
	fpSpaceRE     = regexp.MustCompile(`\s+`)
)

// normalizeMessage removes high-variance tokens so similar errors share a
// fingerprint without merging genuinely different failures.
func normalizeMessage(s string) string {
	s = fpTimestampRE.ReplaceAllString(s, "<ts>")
	s = fpUUIDRE.ReplaceAllString(s, "<uuid>")
	s = fpIPRE.ReplaceAllString(s, "<ip>")
	s = fpHexRE.ReplaceAllString(s, "<hex>")
	s = fpNumberRE.ReplaceAllString(s, "<n>")
	s = fpSpaceRE.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func fingerprintEvent(ev *Event) (string, string) {
	exception := ev.Exception
	if exception == "" {
		for _, line := range ev.Stack {
			if ex := extractException(line); ex != "" {
				exception = ex
				break
			}
		}
	}
	frame := ""
	for _, line := range ev.Stack {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "at ") {
			frame = normalizeMessage(trimmed)
			break
		}
	}
	msg := normalizeMessage(ev.Message)
	if len(msg) > 512 {
		msg = msg[:512]
	}
	sum := sha256.Sum256([]byte(exception + "|" + msg + "|" + frame))
	return "err_" + hex.EncodeToString(sum[:])[:8], exception
}
