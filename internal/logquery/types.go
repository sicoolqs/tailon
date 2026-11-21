package logquery

// Truncation reasons reported in query metadata.
const (
	TruncationResultLimit      = "RESULT_LIMIT"
	TruncationResponseLimit    = "RESPONSE_LIMIT"
	TruncationScanLimit        = "SCAN_LIMIT"
	TruncationTimeout          = "QUERY_TIMEOUT"
	TruncationFileUnreadable   = "FILE_UNREADABLE"
	TruncationPartialFiles     = "PARTIAL_FILES"
	TruncationUnknownTimestamp = "TIMESTAMP_UNKNOWN"
	TruncationFileChanged      = "FILE_CHANGED"
)

// Meta is the execution metadata attached to query results.
type Meta struct {
	Complete         bool     `json:"complete"`
	Truncated        bool     `json:"truncated"`
	TruncationReason string   `json:"truncation_reason,omitempty"`
	DurationMs       int64    `json:"duration_ms"`
	ScannedFiles     int      `json:"scanned_files"`
	ScannedBytes     int64    `json:"scanned_bytes"`
	ReturnedCount    int      `json:"returned_count"`
	HasMore          bool     `json:"has_more"`
	NextCursor       *string  `json:"next_cursor"`
	Warnings         []string `json:"warnings"`
}

func newMeta() Meta {
	return Meta{Complete: true, Warnings: []string{}}
}

// Truncate marks a result as incomplete.
func (m *Meta) Truncate(reason string) {
	m.Complete = false
	m.Truncated = true
	if m.TruncationReason == "" {
		m.TruncationReason = reason
	}
}

func (m *Meta) Warn(msg string) {
	m.Warnings = append(m.Warnings, msg)
}

// ListRequest identifies which files to include in a discovery listing.
type ListRequest struct {
	Group          string `json:"group"`
	Pattern        string `json:"pattern"`
	IncludeFiles   *bool  `json:"include_files"`
	ModifiedWithin string `json:"modified_within"`
	Limit          int    `json:"limit"`
	Cursor         string `json:"cursor"`
}

type GroupInfo struct {
	Name      string `json:"name"`
	FileCount int    `json:"file_count"`
}

type FileInfo struct {
	FileID     string `json:"file_id"`
	Group      string `json:"group"`
	Name       string `json:"name"`
	SizeBytes  int64  `json:"size_bytes"`
	ModifiedAt string `json:"modified_at,omitempty"`
	Readable   bool   `json:"readable"`
	Compressed bool   `json:"compressed"`
}

type ListResult struct {
	Groups     []GroupInfo `json:"groups"`
	Files      []FileInfo  `json:"files"`
	HasMore    bool        `json:"has_more"`
	NextCursor *string     `json:"next_cursor"`
	Meta       Meta        `json:"meta"`
}

// StatRequest inspects a single file without reading its contents.
type StatRequest struct {
	FileID string `json:"file_id"`
}

type SampledRange struct {
	Start    string `json:"start"`
	End      string `json:"end"`
	Sampled  bool   `json:"sampled"`
	Complete bool   `json:"complete"`
}

type StatResult struct {
	FileID      string        `json:"file_id"`
	Group       string        `json:"group"`
	Name        string        `json:"name"`
	SizeBytes   int64         `json:"size_bytes"`
	ModifiedAt  string        `json:"modified_at"`
	Readable    bool          `json:"readable"`
	Compressed  bool          `json:"compressed"`
	Seekable    bool          `json:"seekable"`
	SampledTime *SampledRange `json:"sampled_time_range,omitempty"`
	Meta        Meta          `json:"meta"`
}

// Entry is a single log event returned by tail-style tools.
type Entry struct {
	FileID     string   `json:"file_id"`
	Group      string   `json:"group"`
	FileName   string   `json:"file_name"`
	Timestamp  *string  `json:"timestamp"`
	Level      *string  `json:"level"`
	LineNumber *int64   `json:"line_number"`
	ByteOffset int64    `json:"byte_offset"`
	EndOffset  int64    `json:"end_offset"`
	Message    string   `json:"message"`
	TraceID    *string  `json:"trace_id"`
	Stacktrace []string `json:"stacktrace,omitempty"`
}

// TailRequest returns the most recent events from selected files.
type TailRequest struct {
	FileID            string `json:"file_id"`
	Group             string `json:"group"`
	FilePattern       string `json:"file_pattern"`
	Lines             int    `json:"lines"`
	Level             string `json:"level"`
	Query             string `json:"query"`
	IncludeStacktrace *bool  `json:"include_stacktrace"`
	Order             string `json:"order"`
}

type TailResult struct {
	Entries []Entry `json:"entries"`
	Meta    Meta    `json:"meta"`
}

// SearchRequest is the core log search request.
type SearchRequest struct {
	FileID            string   `json:"file_id"`
	Group             string   `json:"group"`
	FilePattern       string   `json:"file_pattern"`
	Query             string   `json:"query"`
	MatchMode         string   `json:"match_mode"`
	CaseSensitive     *bool    `json:"case_sensitive"`
	Since             string   `json:"since"`
	StartTime         string   `json:"start_time"`
	EndTime           string   `json:"end_time"`
	Levels            []string `json:"levels"`
	ContextBefore     int      `json:"context_before"`
	ContextAfter      int      `json:"context_after"`
	IncludeStacktrace *bool    `json:"include_stacktrace"`
	Order             string   `json:"order"`
	Limit             int      `json:"limit"`
	Cursor            string   `json:"cursor"`
}

type SearchHit struct {
	FileID        string   `json:"file_id"`
	Group         string   `json:"group"`
	FileName      string   `json:"file_name"`
	Timestamp     *string  `json:"timestamp"`
	Level         *string  `json:"level"`
	ByteOffset    int64    `json:"byte_offset"`
	EndOffset     int64    `json:"end_offset"`
	Message       string   `json:"message"`
	TraceID       *string  `json:"trace_id"`
	ContextBefore []string `json:"context_before"`
	ContextAfter  []string `json:"context_after"`
	Stacktrace    []string `json:"stacktrace,omitempty"`
}

type SearchResult struct {
	Results []SearchHit `json:"results"`
	Meta    Meta        `json:"meta"`
}

// ContextRequest retrieves lines around a byte offset returned by another tool.
type ContextRequest struct {
	FileID            string `json:"file_id"`
	ByteOffset        *int64 `json:"byte_offset"`
	Cursor            string `json:"cursor"`
	BeforeLines       int    `json:"before_lines"`
	AfterLines        int    `json:"after_lines"`
	IncludeStacktrace *bool  `json:"include_stacktrace"`
}

type ContextLine struct {
	Offset    int64  `json:"offset"`
	EndOffset int64  `json:"end_offset"`
	Text      string `json:"text"`
	Target    bool   `json:"target"`
}

type ContextResult struct {
	FileID       string        `json:"file_id"`
	Group        string        `json:"group"`
	FileName     string        `json:"file_name"`
	TargetOffset int64         `json:"target_offset"`
	TargetStart  int64         `json:"target_start"`
	TargetEnd    int64         `json:"target_end"`
	Lines        []ContextLine `json:"lines"`
	Meta         Meta          `json:"meta"`
}

// ReadRequest reads a bounded segment of a file.
type ReadRequest struct {
	FileID            string `json:"file_id"`
	Offset            *int64 `json:"offset"`
	Cursor            string `json:"cursor"`
	Lines             int    `json:"lines"`
	Direction         string `json:"direction"`
	IncludeStacktrace *bool  `json:"include_stacktrace"`
}

type ReadResult struct {
	FileID        string  `json:"file_id"`
	Group         string  `json:"group"`
	FileName      string  `json:"file_name"`
	StartOffset   int64   `json:"start_offset"`
	EndOffset     int64   `json:"end_offset"`
	Content       string  `json:"content"`
	ReturnedLines int     `json:"returned_lines"`
	EOF           bool    `json:"eof"`
	NextCursor    *string `json:"next_cursor"`
	Meta          Meta    `json:"meta"`
}

// SummaryRequest aggregates log statistics over a time range.
type SummaryRequest struct {
	FileID          string   `json:"file_id"`
	Group           string   `json:"group"`
	FilePattern     string   `json:"file_pattern"`
	Since           string   `json:"since"`
	StartTime       string   `json:"start_time"`
	EndTime         string   `json:"end_time"`
	Levels          []string `json:"levels"`
	TopN            int      `json:"top_n"`
	Bucket          string   `json:"bucket"`
	IncludeExamples *bool    `json:"include_examples"`
}

type SummaryTimeRange struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type ErrorExample struct {
	FileID     string  `json:"file_id"`
	FileName   string  `json:"file_name"`
	ByteOffset int64   `json:"byte_offset"`
	Timestamp  *string `json:"timestamp"`
	Level      *string `json:"level"`
	Message    string  `json:"message"`
}

type ErrorGroup struct {
	Fingerprint   string        `json:"fingerprint"`
	Exception     string        `json:"exception,omitempty"`
	Count         int64         `json:"count"`
	FirstSeen     *string       `json:"first_seen"`
	LastSeen      *string       `json:"last_seen"`
	AffectedFiles int           `json:"affected_files"`
	Example       *ErrorExample `json:"example,omitempty"`
}

type FileSummary struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	Total    int64  `json:"total"`
	Errors   int64  `json:"errors"`
	Warnings int64  `json:"warnings"`
	Unknown  int64  `json:"unknown"`
}

type TimeBucket struct {
	Start    string `json:"start"`
	Total    int64  `json:"total"`
	Errors   int64  `json:"errors"`
	Warnings int64  `json:"warnings"`
}

type SummaryResult struct {
	Group     string           `json:"group,omitempty"`
	TimeRange SummaryTimeRange `json:"time_range"`
	Levels    map[string]int64 `json:"levels"`
	TopErrors []ErrorGroup     `json:"top_errors"`
	ByFile    []FileSummary    `json:"by_file"`
	Timeline  []TimeBucket     `json:"timeline"`
	Meta      Meta             `json:"meta"`
}

// TraceRequest correlates events that share a request identifier.
type TraceRequest struct {
	TraceID           string   `json:"trace_id"`
	IDType            string   `json:"id_type"`
	Groups            []string `json:"groups"`
	FilePattern       string   `json:"file_pattern"`
	Since             string   `json:"since"`
	StartTime         string   `json:"start_time"`
	EndTime           string   `json:"end_time"`
	IncludeStacktrace *bool    `json:"include_stacktrace"`
	Limit             int      `json:"limit"`
	Cursor            string   `json:"cursor"`
}

type TraceEvent struct {
	Timestamp    *string  `json:"timestamp"`
	Group        string   `json:"group"`
	FileID       string   `json:"file_id"`
	FileName     string   `json:"file_name"`
	Level        *string  `json:"level"`
	Message      string   `json:"message"`
	ByteOffset   int64    `json:"byte_offset"`
	EndOffset    int64    `json:"end_offset"`
	TraceID      *string  `json:"trace_id,omitempty"`
	SpanID       *string  `json:"span_id,omitempty"`
	ParentSpanID *string  `json:"parent_span_id,omitempty"`
	Stacktrace   []string `json:"stacktrace,omitempty"`
}

type TraceResult struct {
	TraceID            string           `json:"trace_id"`
	TimeRange          SummaryTimeRange `json:"time_range"`
	MatchedGroups      []string         `json:"matched_groups"`
	MatchedFiles       int              `json:"matched_files"`
	EventCount         int              `json:"event_count"`
	ObservedDurationMs *int64           `json:"observed_duration_ms"`
	Timeline           []TraceEvent     `json:"timeline"`
	Untimed            []TraceEvent     `json:"untimed"`
	Errors             []TraceEvent     `json:"errors"`
	Meta               Meta             `json:"meta"`
}
