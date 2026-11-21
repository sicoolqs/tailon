package logquery

import (
	"context"
	"io"
)

// Context returns the lines surrounding a previously identified byte offset.
func (s *Service) Context(ctx context.Context, req ContextRequest) (res *ContextResult, err error) {
	q, err := s.begin(ctx, "logs_context")
	if err != nil {
		return nil, err
	}
	res = &ContextResult{Lines: []ContextLine{}, Meta: newMeta()}
	defer func() {
		if res != nil {
			q.finish(&err, &res.Meta)
		} else {
			q.finish(&err, nil)
		}
	}()

	before := req.BeforeLines
	if before == 0 {
		before = 20
	}
	after := req.AfterLines
	if after == 0 {
		after = 50
	}
	before = s.limits.clampContext(before)
	after = s.limits.clampContext(after)
	includeStack := boolValue(req.IncludeStacktrace, true)

	fileID := req.FileID
	var offset int64
	hasOffset := false
	if req.ByteOffset != nil {
		offset = *req.ByteOffset
		hasOffset = true
	}
	if req.Cursor != "" {
		payload, derr := s.cursors.Decode(req.Cursor)
		if derr != nil {
			return nil, derr
		}
		if len(payload.Files) != 1 {
			return nil, NewError(CodeInvalidCursor, "the cursor payload is incomplete")
		}
		if fileID != "" && fileID != payload.Files[0].ID {
			return nil, NewError(CodeInvalidCursor, "the cursor belongs to a different file")
		}
		fileID = payload.Files[0].ID
		if !hasOffset {
			offset = payload.Offset
			hasOffset = true
		}
	}
	if fileID == "" {
		return nil, NewError(CodeInvalidArgument, "file_id is required").
			WithHint(false, "Use the file_id returned by logs_search.")
	}
	if !hasOffset {
		return nil, NewError(CodeInvalidArgument, "byte_offset or cursor is required")
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
	res.TargetOffset = offset

	if src.of.Compressed {
		return nil, NewError(CodeUnsupportedType, "context lookup is not supported for compressed files").
			WithHint(false, "Use logs_search or logs_tail for compressed files.")
	}
	size := src.of.Info.Size()
	if offset < 0 {
		return nil, NewError(CodeInvalidArgument, "byte_offset must not be negative")
	}
	if offset > size {
		return nil, NewError(CodeFileChanged, "the file is shorter than the requested byte offset").
			WithHint(false, "The file may have been truncated or rotated. Call logs_list and query again.")
	}
	if offset == size && size > 0 {
		offset-- // position the cursor inside the last byte
	}
	budget := s.newBudget(s.limits.QueryTimeout)

	// Locate the line that contains the offset. If the byte before the offset
	// is a newline, the offset is already a line boundary; otherwise read
	// backwards to find the start of the partial line.
	var targetStart int64 = offset
	if offset > 0 {
		var prev [1]byte
		if n, _ := src.of.File.ReadAt(prev[:], offset-1); !(n == 1 && prev[0] == '\n') {
			rr := newReverseReader(ctx, src.of.File, offset, s.dec, s.limits.ReadBufferBytes, s.limits.MaxLineBytes, budget)
			if line, lerr := rr.Next(); lerr == nil {
				targetStart = line.Start
			} else if lerr != io.EOF && lerr != errBudgetExceeded {
				return nil, lerr
			}
		}
	}
	fr, rerr := s.forwardReader(ctx, src, targetStart, size, budget)
	if rerr != nil {
		return nil, rerr
	}
	targetLine, lerr := fr.Next()
	if lerr != nil && lerr != io.EOF && lerr != errBudgetExceeded {
		return nil, lerr
	}

	eventStart := targetStart
	eventEnd := targetStart
	eventText := ""
	if lerr == nil {
		eventEnd = targetLine.End
		eventText = targetLine.Text
	}

	if includeStack && eventText != "" {
		// Expand to the beginning of a multiline event.
		for isContinuation(eventText) && eventStart > 0 {
			rr := newReverseReader(ctx, src.of.File, eventStart, s.dec, s.limits.ReadBufferBytes, s.limits.MaxLineBytes, budget)
			prev, perr := rr.Next()
			if perr != nil {
				break
			}
			eventStart = prev.Start
			eventText = prev.Text
		}
		// Expand to the end of the multiline event.
		expandBytes := len(eventText)
		efr, eerr := s.forwardReader(ctx, src, eventEnd, size, budget)
		if eerr == nil {
			for expandBytes < s.limits.MaxEventBytes {
				next, nerr := efr.Next()
				if nerr != nil {
					break
				}
				if !isContinuation(next.Text) {
					break
				}
				eventEnd = next.End
				expandBytes += len(next.Text)
			}
		}
	}

	var lines []ContextLine
	if before > 0 && eventStart > 0 {
		brr := newReverseReader(ctx, src.of.File, eventStart, s.dec, s.limits.ReadBufferBytes, s.limits.MaxLineBytes, budget)
		var back []ContextLine
		for i := 0; i < before; i++ {
			line, berr := brr.Next()
			if berr != nil {
				break
			}
			back = append(back, ContextLine{
				Offset:    line.Start,
				EndOffset: line.End,
				Text:      s.redactor.Apply(line.Text),
			})
		}
		for i := len(back) - 1; i >= 0; i-- {
			lines = append(lines, back[i])
		}
	}

	// Read the target event itself (it may span multiple physical lines).
	if eventEnd > eventStart || eventText != "" {
		eventFR, eerr := s.forwardReader(ctx, src, eventStart, eventEnd, budget)
		if eerr == nil {
			for {
				line, nerr := eventFR.Next()
				if nerr != nil {
					break
				}
				lines = append(lines, ContextLine{
					Offset:    line.Start,
					EndOffset: line.End,
					Text:      s.redactor.Apply(line.Text),
					Target:    true,
				})
			}
		}
	}

	if after > 0 && eventEnd < size {
		afr, aerr := s.forwardReader(ctx, src, eventEnd, size, budget)
		if aerr == nil {
			for i := 0; i < after; i++ {
				line, nerr := afr.Next()
				if nerr != nil {
					break
				}
				lines = append(lines, ContextLine{
					Offset:    line.Start,
					EndOffset: line.End,
					Text:      s.redactor.Apply(line.Text),
				})
			}
		}
	}

	res.TargetStart = eventStart
	res.TargetEnd = eventEnd
	res.Lines = lines
	res.Meta.ReturnedCount = len(lines)
	s.setBudgetMeta(&res.Meta, budget)
	return res, nil
}
