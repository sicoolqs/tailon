package mcpserver

// JSON schemas for the eight MCP tools. The server always applies defaults
// itself; the schema defaults are documentation for clients.

func objectSchema(props map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func stringProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func stringEnumProp(desc string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": values}
}

func boolProp(desc string, def bool) map[string]any {
	return map[string]any{"type": "boolean", "description": desc, "default": def}
}

func intProp(desc string, def, min, max int) map[string]any {
	return map[string]any{
		"type": "integer", "description": desc,
		"default": def, "minimum": min, "maximum": max,
	}
}

func stringArrayProp(desc string) map[string]any {
	return map[string]any{
		"type": "array", "description": desc,
		"items": map[string]any{"type": "string"},
	}
}

func listSchema() map[string]any {
	return objectSchema(map[string]any{
		"group":           stringProp("Filter by log group. Use \"all\" or omit for every group."),
		"pattern":         stringProp("Glob pattern matched against file display names, for example *.log."),
		"include_files":   boolProp("Include file details in addition to group counts.", true),
		"modified_within": stringProp("Only include files modified within this duration, for example 24h or 7d."),
		"limit":           intProp("Maximum number of files to return.", 100, 1, 500),
		"cursor":          stringProp("Opaque cursor returned by a previous logs_list call."),
	})
}

func statSchema() map[string]any {
	return objectSchema(map[string]any{
		"file_id": stringProp("File identifier returned by logs_list."),
	}, "file_id")
}

func tailSchema() map[string]any {
	return objectSchema(map[string]any{
		"file_id":            stringProp("Read one specific file."),
		"group":              stringProp("Select every file in this log group."),
		"file_pattern":       stringProp("Filter selected files by display name glob."),
		"lines":              intProp("Maximum physical lines returned across all files.", 200, 1, 5000),
		"level":              stringProp("Optional log level filter (TRACE, DEBUG, INFO, WARN, ERROR, FATAL)."),
		"query":              stringProp("Optional case-insensitive literal keyword."),
		"include_stacktrace": boolProp("Keep multiline exception events intact.", true),
		"order":              stringEnumProp("Chronological order of the returned window.", "asc", "desc"),
	})
}

func searchSchema() map[string]any {
	return objectSchema(map[string]any{
		"file_id":            stringProp("Search one specific file."),
		"group":              stringProp("Search every file in this log group."),
		"file_pattern":       stringProp("Filter selected files by display name glob."),
		"query":              stringProp("Literal text or regular expression to find."),
		"match_mode":         stringEnumProp("Match query as literal text or a Go (RE2) regular expression.", "literal", "regex"),
		"case_sensitive":     boolProp("Match case-sensitively.", false),
		"since":              stringProp("Relative time range such as 30m, 2h or 7d. Cannot be combined with start_time."),
		"start_time":         stringProp("Absolute inclusive start time in RFC3339 format."),
		"end_time":           stringProp("Absolute exclusive end time in RFC3339 format."),
		"levels":             stringArrayProp("Allowed log levels, for example [\"ERROR\", \"WARN\"]."),
		"context_before":     intProp("Physical lines to include before each match.", 0, 0, 100),
		"context_after":      intProp("Physical lines to include after each match.", 0, 0, 100),
		"include_stacktrace": boolProp("Preserve multiline exception events.", true),
		"order":              stringEnumProp("Scan and return direction.", "asc", "desc"),
		"limit":              intProp("Maximum matched events to return.", 50, 1, 500),
		"cursor":             stringProp("Opaque cursor returned by a previous logs_search call."),
	})
}

func contextSchema() map[string]any {
	return objectSchema(map[string]any{
		"file_id":            stringProp("File identifier returned by logs_search or logs_list."),
		"byte_offset":        map[string]any{"type": "integer", "description": "Byte offset returned by a previous result."},
		"cursor":             stringProp("Opaque cursor that pins a file position."),
		"before_lines":       intProp("Physical lines to include before the target event.", 20, 0, 100),
		"after_lines":        intProp("Physical lines to include after the target event.", 50, 0, 100),
		"include_stacktrace": boolProp("Expand multiline exception events.", true),
	})
}

func readSchema() map[string]any {
	return objectSchema(map[string]any{
		"file_id":            stringProp("File identifier returned by logs_list."),
		"offset":             map[string]any{"type": "integer", "description": "Starting byte offset. Defaults to the beginning (forward) or the snapshot end (backward)."},
		"cursor":             stringProp("Opaque continuation cursor returned by a previous logs_read call."),
		"lines":              intProp("Maximum physical lines to return.", 200, 1, 5000),
		"direction":          stringEnumProp("Read direction.", "forward", "backward"),
		"include_stacktrace": boolProp("Do not split multiline events at page boundaries.", true),
	}, "file_id")
}

func summarySchema() map[string]any {
	return objectSchema(map[string]any{
		"file_id":          stringProp("Summarize one specific file."),
		"group":            stringProp("Summarize every file in this log group."),
		"file_pattern":     stringProp("Filter selected files by display name glob."),
		"since":            stringProp("Relative time range. Defaults to 1h."),
		"start_time":       stringProp("Absolute inclusive start time in RFC3339 format."),
		"end_time":         stringProp("Absolute exclusive end time in RFC3339 format."),
		"levels":           stringArrayProp("Only count these log levels."),
		"top_n":            intProp("Number of top error fingerprints to return.", 10, 1, 500),
		"bucket":           stringProp("Timeline bucket size, for example 1m, 5m, 15m or 1h. Defaults to 5m."),
		"include_examples": boolProp("Include one representative example per top error.", true),
	})
}

func traceSchema() map[string]any {
	return objectSchema(map[string]any{
		"trace_id":           stringProp("Complete trace, request or correlation identifier."),
		"id_type":            stringEnumProp("Identifier field to match.", "auto", "trace_id", "request_id", "correlation_id"),
		"groups":             stringArrayProp("Log groups to search. Defaults to every configured group."),
		"file_pattern":       stringProp("Filter selected files by display name glob."),
		"since":              stringProp("Relative time range. Defaults to 2h."),
		"start_time":         stringProp("Absolute inclusive start time in RFC3339 format."),
		"end_time":           stringProp("Absolute exclusive end time in RFC3339 format."),
		"include_stacktrace": boolProp("Include multiline exception events.", true),
		"limit":              intProp("Maximum observed events to return.", 100, 1, 500),
		"cursor":             stringProp("Opaque cursor returned by a previous logs_trace call."),
	}, "trace_id")
}
