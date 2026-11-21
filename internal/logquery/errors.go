// Package logquery implements the read-only log query engine used by the
// Tailon MCP server. It deliberately has no dependency on the MCP SDK so that
// it can be tested and benchmarked on its own.
package logquery

import (
	"errors"
	"fmt"
)

// Code is a stable application error code returned to MCP clients.
type Code string

const (
	CodeInvalidArgument   Code = "INVALID_ARGUMENT"
	CodeInvalidTimeRange  Code = "INVALID_TIME_RANGE"
	CodeInvalidRegex      Code = "INVALID_REGEX"
	CodeFileNotFound      Code = "FILE_NOT_FOUND"
	CodeGroupNotFound     Code = "GROUP_NOT_FOUND"
	CodeFileAccessDenied  Code = "FILE_ACCESS_DENIED"
	CodeFileChanged       Code = "FILE_CHANGED"
	CodeFileRotated       Code = "FILE_ROTATED"
	CodeUnsupportedType   Code = "UNSUPPORTED_FILE_TYPE"
	CodeInvalidCursor     Code = "INVALID_CURSOR"
	CodeCursorExpired     Code = "CURSOR_EXPIRED"
	CodeQueryTimeout      Code = "QUERY_TIMEOUT"
	CodeScanLimitExceeded Code = "SCAN_LIMIT_EXCEEDED"
	CodeResponseLimit     Code = "RESPONSE_LIMIT_EXCEEDED"
	CodeServerBusy        Code = "SERVER_BUSY"
	CodeInternalError     Code = "INTERNAL_ERROR"
)

// Error is a tool-level error with a stable code and optional remediation hint.
type Error struct {
	Code       Code   `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable"`
	Suggestion string `json:"suggestion,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// NewError creates an application error.
func NewError(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Errorf creates a formatted application error.
func Errorf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithHint attaches a remediation hint and retryability flag.
func (e *Error) WithHint(retryable bool, suggestion string) *Error {
	e.Retryable = retryable
	e.Suggestion = suggestion
	return e
}

// CodeOf returns the application error code of err, if any.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeInternalError
}

// AsError converts any error into an application error.
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: CodeInternalError, Message: err.Error()}
}

// IsCode reports whether err carries the given application code.
func IsCode(err error, code Code) bool {
	return CodeOf(err) == code
}

// ErrorResult is the JSON body returned for failed tool calls.
type ErrorResult struct {
	Error *Error `json:"error"`
}
