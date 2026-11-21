package logquery

import (
	"context"
	"io"
	"strconv"
	"strings"
)

// Read returns a bounded segment of a file using byte offsets and cursors.
func (s *Service) Read(ctx context.Context, req ReadRequest) (res *ReadResult, err error) {
	q, err := s.begin(ctx, "logs_read")
	if err != nil {
		return nil, err
	}
	res = &ReadResult{Meta: newMeta()}
	defer func() {
		if res != nil {
			q.finish(&err, &res.Meta)
		} else {
			q.finish(&err, nil)
		}
	}()

	direction := strings.ToLower(strings.TrimSpace(req.Direction))
	if direction == "" {
		direction = "forward"
	}
	if direction != "forward" && direction != "backward" {
		return nil, NewError(CodeInvalidArgument, "direction must be forward or backward")
	}
	lines := s.limits.clampLines(req.Lines)
	includeStack := boolValue(req.IncludeStacktrace, true)

	fileID := req.FileID
	var payload *cursorPayload
	digest := func(id string) string {
		return queryDigest("logs_read", id, direction, strconv.FormatBool(includeStack))
	}
	if req.Cursor != "" {
		payload, err = s.cursors.Decode(req.Cursor)
		if err != nil {
			return nil, err
		}
		if payload.Tool != "logs_read" {
			return nil, NewError(CodeInvalidCursor, "the cursor belongs to a different tool")
		}
		if len(payload.Files) != 1 {
			return nil, NewError(CodeInvalidCursor, "the cursor payload is incomplete")
		}
		if fileID != "" && fileID != payload.Files[0].ID {
			return nil, NewError(CodeInvalidCursor, "the cursor belongs to a different file")
		}
		fileID = payload.Files[0].ID
		if payload.Query != digest(fileID) {
			return nil, NewError(CodeInvalidCursor, "the cursor does not match the current read parameters").
				WithHint(false, "Restart the read without a cursor.")
		}
	}
	if fileID == "" {
		return nil, NewError(CodeInvalidArgument, "file_id is required").
			WithHint(false, "Call logs_list to discover file identifiers.")
	}

	ref, ok := s.reg.Resolve(fileID)
	if !ok {
		return nil, NewError(CodeFileNotFound, "the requested file_id is not available").
			WithHint(false, "Call logs_list again to rediscover available files.")
	}
	src, err := s.openSource(ref)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	res.Meta.ScannedFiles = 1
	res.FileID = ref.ID
	res.Group = ref.Group
	res.FileName = ref.Name

	snapshot := src.of.Info.Size()
	if src.of.Compressed {
		snapshot = -1
	}
	offset := int64(0)
	if direction == "backward" {
		offset = src.of.Info.Size()
	}
	if req.Offset != nil {
		offset = *req.Offset
	}
	if payload != nil {
		pinned := payload.Files[0]
		if src.of.Identity != "" && pinned.Identity != "" && src.of.Identity != pinned.Identity {
			return nil, NewError(CodeFileRotated, "the log file was rotated since the cursor was issued").
				WithHint(false, "Call logs_list and restart the read.")
		}
		if !src.of.Compressed && src.of.Info.Size() < pinned.Size {
			return nil, NewError(CodeFileChanged, "the log file was truncated since the cursor was issued").
				WithHint(false, "Restart the read with the current file contents.")
		}
		snapshot = pinned.Size
		offset = payload.Offset
	}
	if src.of.Compressed {
		if direction != "forward" || offset != 0 {
			return nil, NewError(CodeUnsupportedType, "compressed files can only be read forward from the beginning").
				WithHint(false, "Use logs_search or logs_tail for compressed files.")
		}
	} else if offset < 0 || offset > snapshot {
		return nil, NewError(CodeInvalidArgument, "offset is outside the file")
	}

	budget := s.newBudget(s.limits.QueryTimeout)
	var contentLines []string
	var startOffset, endOffset int64
	truncated := false

	if direction == "forward" {
		startOffset = offset
		endOffset = offset
		fr, rerr := s.forwardReader(ctx, src, offset, snapshot, budget)
		if rerr != nil {
			return nil, rerr
		}
		count := 0
		eofSeen := false
		midEvent := false
		done := false
		for count < lines && !done {
			line, lerr := fr.Next()
			if lerr == io.EOF {
				eofSeen = true
				break
			}
			if lerr == errBudgetExceeded {
				res.Meta.Truncate(TruncationScanLimit)
				truncated = true
				break
			}
			if lerr != nil {
				return nil, lerr
			}
			if count == 0 && includeStack && isContinuation(line.Text) {
				midEvent = true
			}
			contentLines = append(contentLines, s.redactor.Apply(line.Text))
			count++
			endOffset = line.End
			if line.Truncated {
				truncated = true
				res.Meta.Warn("A log line exceeded max-line-bytes and was truncated.")
			}

			if includeStack {
				// Keep reading until the multiline event is complete. The first
				// line of the next event is consumed but not returned; the next
				// cursor points before it so it can be read again.
				expandBytes := len(line.Text)
				for {
					next, perr := fr.Next()
					if perr == io.EOF {
						eofSeen = true
						break
					}
					if perr == errBudgetExceeded {
						res.Meta.Truncate(TruncationScanLimit)
						truncated = true
						break
					}
					if perr != nil {
						return nil, perr
					}
					if !isContinuation(next.Text) {
						endOffset = next.Start
						done = true
						break
					}
					contentLines = append(contentLines, s.redactor.Apply(next.Text))
					count++
					endOffset = next.End
					expandBytes += len(next.Text)
					if expandBytes > s.limits.MaxEventBytes {
						done = true
						truncated = true
						res.Meta.Warn("A multiline event exceeded max-event-bytes and was truncated.")
						break
					}
				}
			}
		}
		if midEvent {
			res.Meta.Warn("The read started in the middle of a multiline event.")
		}
		res.EOF = eofSeen || (!src.of.Compressed && endOffset >= snapshot)
		if !res.EOF && !src.of.Compressed {
			token := s.cursorForRead(fileID, digest(fileID), direction, src, snapshot, endOffset)
			res.NextCursor = &token
			res.Meta.HasMore = true
			res.Meta.NextCursor = &token
			res.Meta.Complete = false
		}
	} else {
		startOffset = offset
		rr, rerr := s.reverseReader(ctx, src, offset, budget)
		if rerr != nil {
			return nil, rerr
		}
		count := 0
		eofSeen := false
		for count < lines {
			line, lerr := rr.Next()
			if lerr == io.EOF {
				eofSeen = true
				break
			}
			if lerr == errBudgetExceeded {
				res.Meta.Truncate(TruncationScanLimit)
				truncated = true
				break
			}
			if lerr != nil {
				return nil, lerr
			}
			contentLines = append(contentLines, s.redactor.Apply(line.Text))
			count++
			startOffset = line.Start
			if line.Truncated {
				truncated = true
				res.Meta.Warn("A log line exceeded max-line-bytes and was truncated.")
			}
		}
		if includeStack {
			// Older lines are appended so that reversing the slice after the
			// loop produces chronological order.
			for isContinuationContent(contentLines) {
				line, lerr := rr.Next()
				if lerr != nil {
					if lerr == io.EOF {
						eofSeen = true
					}
					break
				}
				contentLines = append(contentLines, s.redactor.Apply(line.Text))
				count++
				startOffset = line.Start
			}
		}
		endOffset = offset
		reverseStrings(contentLines)
		res.EOF = startOffset <= 0 || eofSeen
		if !res.EOF {
			token := s.cursorForRead(fileID, digest(fileID), direction, src, snapshot, startOffset)
			res.NextCursor = &token
			res.Meta.HasMore = true
			res.Meta.NextCursor = &token
			res.Meta.Complete = false
		}
	}

	res.StartOffset = startOffset
	res.EndOffset = endOffset
	res.Content = strings.Join(contentLines, "\n")
	res.ReturnedLines = len(contentLines)
	if truncated && !res.Meta.Truncated {
		res.Meta.Truncate(TruncationResultLimit)
	}
	if src.of.Compressed && !res.EOF {
		res.Meta.Warn("Continuation cursors are not supported for compressed files.")
		res.Meta.Truncate(TruncationResultLimit)
	}
	res.Meta.ReturnedCount = len(contentLines)
	s.setBudgetMeta(&res.Meta, budget)
	return res, nil
}

func (s *Service) cursorForRead(fileID, digest, direction string, src *fileSource, snapshot, offset int64) string {
	return s.cursors.Encode(cursorPayload{
		Tool:         "logs_read",
		Query:        digest,
		Files:        []CursorFile{{ID: fileID, Size: snapshot, Identity: src.of.Identity}},
		Offset:       offset,
		Dir:          direction,
		SnapshotSize: snapshot,
	})
}

func reverseStrings(lines []string) {
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
}

func isContinuationContent(lines []string) bool {
	if len(lines) == 0 {
		return false
	}
	// Content is collected newest to oldest before being reversed.
	return isContinuation(lines[len(lines)-1])
}
