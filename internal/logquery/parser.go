package logquery

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Event is one logical log record. Continuation lines belonging to a Java
// stack trace are kept together with the line that started the event.
type Event struct {
	Lines     []Line
	Start     int64
	End       int64
	Timestamp *time.Time
	Level     string
	Message   string

	TraceID       string
	SpanID        string
	ParentSpanID  string
	RequestID     string
	CorrelationID string

	Exception string
	Stack     []string
	IsJSON    bool
	Truncated bool
	Replaced  bool
}

type parsedLine struct {
	Timestamp     *time.Time
	Level         string
	Message       string
	TraceID       string
	SpanID        string
	ParentSpanID  string
	RequestID     string
	CorrelationID string
	Exception     string
	Stack         []string
	IsJSON        bool
}

var timestampLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999Z0700",
	"2006-01-02T15:04:05Z0700",
	"2006-01-02T15:04:05.999999999Z07:00",
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05Z07:00",
	"2006-01-02 15:04:05.999999999Z0700",
	"2006-01-02 15:04:05Z0700",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006/01/02 15:04:05.999999999",
	"2006/01/02 15:04:05",
	"02/Jan/2006:15:04:05 -0700",
	"02/Jan/2006:15:04:05",
	"Jan  2 15:04:05",
	"Jan 2 15:04:05",
}

var timestampRE = regexp.MustCompile(
	`\d{4}[-/]\d{2}[-/]\d{2}[T ]\d{2}:\d{2}:\d{2}(?:[.,]\d{1,9})?(?:Z|[+-]\d{2}:?\d{2})?` +
		`|\d{2}/[A-Za-z]{3}/\d{4}:\d{2}:\d{2}:\d{2}(?: [+-]\d{4})?` +
		`|[A-Z][a-z]{2} +\d{1,2} \d{2}:\d{2}:\d{2}`)

var idFieldsRE = regexp.MustCompile(`(?i)\b(trace[_-]?id|traceid|tid|request[_-]?id|req[_-]?id|requestid|correlation[_-]?id|correlationid|corr[_-]?id|parent[_-]?span[_-]?id|parentspanid|span[_-]?id|spanid)\s*[=:]\s*["']?([A-Za-z0-9._:/-]{4,128})`)

// bracketTraceRE recognizes the common Logback MDC pattern
// "[<traceId>-<spanId>]" used by this project's Java services.
var bracketTraceRE = regexp.MustCompile(`\[([0-9a-fA-F]{8,32})-([0-9a-fA-F]{8,32})\]`)

var exceptionRE = regexp.MustCompile(`\b((?:[A-Za-z_$][\w$]*\.)+[A-Za-z_$][\w$]*(?:Exception|Error)|[A-Za-z_$][\w$]*(?:Exception|Error))\b`)

var levelTokens = []string{"WARNING", "CRITICAL", "SEVERE", "TRACE", "DEBUG", "INFO", "WARN", "ERROR", "FATAL"}

// parseLine extracts structured fields from a physical log line. Parsing is
// best-effort: any unparseable field is simply left empty.
func parseLine(text string, loc *time.Location) parsedLine {
	var p parsedLine
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") {
		if jp, ok := parseJSONLine(trimmed, loc); ok {
			return jp
		}
	}
	p.Message = text
	ts, end := extractTimestamp(text, loc)
	if ts != nil {
		p.Timestamp = ts
	}
	searchStart := 0
	if end > 0 {
		searchStart = end
		if searchStart > len(text) {
			searchStart = len(text)
		}
	}
	p.Level = detectLevel(text, searchStart, ts != nil)
	p.extractIDs(text)
	p.Exception = extractException(text)
	return p
}

func extractTimestamp(text string, loc *time.Location) (*time.Time, int) {
	lead := 0
	for lead < len(text) && (text[lead] == ' ' || text[lead] == '\t') {
		lead++
	}
	if lead < len(text) && (text[lead] == '[' || text[lead] == '(') {
		lead++
	}
	region := text[lead:]
	if len(region) > 96 {
		region = region[:96]
	}
	idx := timestampRE.FindStringIndex(region)
	if idx == nil || idx[0] > 2 {
		return nil, 0
	}
	if t := parseTimestampString(region[idx[0]:idx[1]], loc); t != nil {
		return t, lead + idx[1]
	}
	return nil, 0
}

func parseTimestampString(s string, loc *time.Location) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	// Go accepts both '.' and ',' for fractional seconds when the layout
	// contains a fractional section, but normalizing makes the intent clear.
	s = strings.Replace(s, ",", ".", 1)
	if loc == nil {
		loc = time.UTC
	}
	for _, layout := range timestampLayouts {
		var (
			t   time.Time
			err error
		)
		if strings.Contains(layout, "Z07") {
			t, err = time.Parse(layout, s)
		} else {
			t, err = time.ParseInLocation(layout, s, loc)
		}
		if err == nil {
			if t.Year() == 0 {
				t = t.AddDate(time.Now().In(loc).Year(), 0, 0)
			}
			return &t
		}
	}
	return nil
}

func detectLevel(text string, searchStart int, hasTimestamp bool) string {
	upper := strings.ToUpper(text)
	limit := 24
	if hasTimestamp {
		limit = 96
	}
	end := searchStart + limit
	if end > len(upper) {
		end = len(upper)
	}
	if searchStart < 0 || searchStart > end {
		searchStart = 0
	}
	region := upper[searchStart:end]

	bestPos, bestLen, best := -1, 0, ""
	for _, token := range levelTokens {
		pos := indexToken(region, token)
		if pos < 0 {
			continue
		}
		if bestPos < 0 || pos < bestPos || (pos == bestPos && len(token) > bestLen) {
			bestPos, bestLen, best = pos, len(token), token
		}
	}
	return normalizeLevel(best)
}

func indexToken(s, token string) int {
	offset := 0
	for offset <= len(s) {
		i := strings.Index(s[offset:], token)
		if i < 0 {
			return -1
		}
		i += offset
		beforeOK := i == 0 || !isWordByte(s[i-1])
		after := i + len(token)
		afterOK := after >= len(s) || !isWordByte(s[after])
		if beforeOK && afterOK {
			return i
		}
		offset = i + 1
	}
	return -1
}

func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_'
}

func normalizeLevel(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "TRACE":
		return "TRACE"
	case "DEBUG":
		return "DEBUG"
	case "INFO", "INFORMATION", "NOTICE":
		return "INFO"
	case "WARN", "WARNING":
		return "WARN"
	case "ERROR", "ERR":
		return "ERROR"
	case "FATAL", "CRITICAL", "SEVERE", "PANIC":
		return "FATAL"
	default:
		return ""
	}
}

func (p *parsedLine) extractIDs(text string) {
	hasLabel := strings.Contains(text, "Id") || strings.Contains(text, "ID") || strings.Contains(text, "id")
	if !hasLabel {
		// Bracket MDC identifiers do not contain the literal "id" label.
		if !strings.Contains(text, "[") || !strings.Contains(text, "-") {
			return
		}
	}
	matches := idFieldsRE.FindAllStringSubmatch(text, 8)
	for _, m := range matches {
		field := canonicalField(m[1])
		value := strings.Trim(m[2], "\"'")
		if value == "" {
			continue
		}
		switch field {
		case "trace":
			if p.TraceID == "" {
				p.TraceID = value
			}
		case "span":
			if p.SpanID == "" {
				p.SpanID = value
			}
		case "parentspan":
			if p.ParentSpanID == "" {
				p.ParentSpanID = value
			}
		case "request":
			if p.RequestID == "" {
				p.RequestID = value
			}
		case "correlation":
			if p.CorrelationID == "" {
				p.CorrelationID = value
			}
		}
	}
	if p.TraceID == "" {
		if m := bracketTraceRE.FindStringSubmatch(text); m != nil {
			p.TraceID = m[1]
			if p.SpanID == "" {
				p.SpanID = m[2]
			}
		}
	}
}

func canonicalField(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	switch {
	case strings.Contains(out, "parent") && strings.Contains(out, "span"):
		return "parentspan"
	case strings.Contains(out, "span"):
		return "span"
	case strings.Contains(out, "trace"):
		return "trace"
	case strings.Contains(out, "correlation") || strings.Contains(out, "corr"):
		return "correlation"
	case strings.Contains(out, "request") || strings.EqualFold(out, "reqid"):
		return "request"
	}
	return out
}

func extractException(text string) string {
	if !strings.Contains(text, "Exception") && !strings.Contains(text, "Error") {
		// Still allow all-lowercase package names ending in .exception/.error.
		lower := strings.ToLower(text)
		if !strings.Contains(lower, "exception") && !strings.Contains(lower, "error") {
			return ""
		}
	}
	m := exceptionRE.FindString(text)
	return m
}

func parseJSONLine(text string, loc *time.Location) (parsedLine, bool) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return parsedLine{}, false
	}
	lower := make(map[string]any, len(obj))
	for k, v := range obj {
		lower[canonicalJSONKey(k)] = v
	}

	p := parsedLine{IsJSON: true, Message: text}
	if v, ok := lookupString(lower, "message", "msg", "text", "body", "log"); ok {
		p.Message = v
	}
	if v, ok := lower["timestamp"]; ok {
		p.Timestamp = parseJSONTime(v, loc)
	} else if v, ok := lower["time"]; ok {
		p.Timestamp = parseJSONTime(v, loc)
	} else if v, ok := lower["ts"]; ok {
		p.Timestamp = parseJSONTime(v, loc)
	} else if v, ok := lower["date"]; ok {
		p.Timestamp = parseJSONTime(v, loc)
	}
	if v, ok := lookupString(lower, "level", "severity", "severitytext", "loglevel"); ok {
		p.Level = normalizeLevel(v)
	} else if v, ok := lower["level"]; ok {
		if n, ok := v.(json.Number); ok {
			if i, err := n.Int64(); err == nil {
				p.Level = syslogLevel(i)
			}
		}
	}
	if v, ok := lookupString(lower, "traceid", "tid"); ok {
		p.TraceID = v
	}
	if v, ok := lookupString(lower, "spanid"); ok {
		p.SpanID = v
	}
	if v, ok := lookupString(lower, "parentspanid"); ok {
		p.ParentSpanID = v
	}
	if v, ok := lookupString(lower, "requestid", "reqid"); ok {
		p.RequestID = v
	}
	if v, ok := lookupString(lower, "correlationid", "corrid"); ok {
		p.CorrelationID = v
	}
	if v, ok := lookupString(lower, "exception", "errortype", "errorname"); ok {
		p.Exception = v
	} else if v, ok := lower["error"]; ok {
		switch e := v.(type) {
		case string:
			p.Exception = extractException(e)
			if p.Exception == "" {
				p.Exception = e
			}
		case map[string]any:
			if s, ok := e["type"].(string); ok {
				p.Exception = s
			} else if s, ok := e["message"].(string); ok {
				p.Exception = extractException(s)
			}
		}
	}
	if v, ok := lower["stacktrace"]; ok {
		p.Stack = toStringSlice(v)
	} else if v, ok := lower["stack"]; ok {
		p.Stack = toStringSlice(v)
	}
	return p, true
}

func canonicalJSONKey(k string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(k) {
		if r == '@' {
			continue
		}
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func lookupString(m map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch s := v.(type) {
			case string:
				return s, true
			case json.Number:
				return s.String(), true
			}
		}
	}
	return "", false
}

func parseJSONTime(v any, loc *time.Location) *time.Time {
	switch t := v.(type) {
	case string:
		if ts := parseTimestampString(t, loc); ts != nil {
			return ts
		}
		return nil
	case json.Number:
		f, err := strconv.ParseFloat(t.String(), 64)
		if err != nil {
			return nil
		}
		return unixToTime(f)
	case float64:
		return unixToTime(t)
	}
	return nil
}

func unixToTime(f float64) *time.Time {
	var n int64
	switch {
	case f >= 1e18:
		n = int64(f / 1e9) // nanoseconds
	case f >= 1e15:
		n = int64(f / 1e6) // microseconds
	case f >= 1e12:
		n = int64(f / 1e3) // milliseconds
	default:
		n = int64(f)
	}
	t := time.Unix(n, 0).UTC()
	return &t
}

func syslogLevel(n int64) string {
	switch {
	case n <= 3:
		return "ERROR"
	case n == 4:
		return "WARN"
	case n <= 6:
		return "INFO"
	default:
		return "DEBUG"
	}
}

func toStringSlice(v any) []string {
	switch s := v.(type) {
	case []any:
		out := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok {
				out = append(out, str)
			}
		}
		return out
	case string:
		if s == "" {
			return nil
		}
		return strings.Split(s, "\n")
	}
	return nil
}

// isContinuation reports whether a line belongs to an already-started event.
func isContinuation(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	if len(text) > 0 && (text[0] == ' ' || text[0] == '\t') {
		return true
	}
	switch {
	case strings.HasPrefix(trimmed, "Caused by:"),
		strings.HasPrefix(trimmed, "Suppressed:"),
		strings.HasPrefix(trimmed, "... "),
		strings.HasPrefix(trimmed, "at "),
		strings.HasPrefix(trimmed, "WARNING:"),
		strings.HasPrefix(trimmed, "SEVERE:"):
		return true
	}
	// Exception class at column zero, e.g. "java.lang.NullPointerException: ..."
	if idx := exceptionRE.FindStringIndex(trimmed); idx != nil && idx[0] == 0 {
		return true
	}
	return false
}

func newEvent(lines []Line, loc *time.Location) *Event {
	if len(lines) == 0 {
		return nil
	}
	first := lines[0]
	p := parseLine(first.Text, loc)
	ev := &Event{
		Lines:         lines,
		Start:         first.Start,
		End:           lines[len(lines)-1].End,
		Timestamp:     p.Timestamp,
		Level:         p.Level,
		Message:       p.Message,
		TraceID:       p.TraceID,
		SpanID:        p.SpanID,
		ParentSpanID:  p.ParentSpanID,
		RequestID:     p.RequestID,
		CorrelationID: p.CorrelationID,
		Exception:     p.Exception,
		IsJSON:        p.IsJSON,
	}
	ev.Stack = append(ev.Stack, p.Stack...)
	for _, line := range lines {
		if line.Truncated || line.Replaced {
			ev.Truncated = ev.Truncated || line.Truncated
			ev.Replaced = ev.Replaced || line.Replaced
		}
	}
	for _, line := range lines[1:] {
		text := strings.TrimRight(line.Text, "\r")
		if text == "" {
			continue
		}
		ev.Stack = append(ev.Stack, text)
		if ev.Exception == "" {
			ev.Exception = extractException(text)
		}
	}
	return ev
}
