package logquery

import (
	"context"
	"io"
	"time"
)

// Stat inspects file metadata without reading the whole file.
func (s *Service) Stat(ctx context.Context, req StatRequest) (res *StatResult, err error) {
	q, err := s.begin(ctx, "logs_stat")
	if err != nil {
		return nil, err
	}
	res = &StatResult{Meta: newMeta()}
	defer func() {
		if res != nil {
			q.finish(&err, &res.Meta)
		} else {
			q.finish(&err, nil)
		}
	}()

	if req.FileID == "" {
		return nil, NewError(CodeInvalidArgument, "file_id is required").
			WithHint(false, "Call logs_list to discover file identifiers.")
	}
	ref, ok := s.reg.Resolve(req.FileID)
	if !ok {
		return nil, NewError(CodeFileNotFound, "the requested file_id is not available").
			WithHint(false, "Call logs_list to discover current file identifiers.")
	}
	src, err := s.openSource(ref)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	res.Meta.ScannedFiles = 1

	res.FileID = ref.ID
	res.Group = ref.Group
	res.Name = ref.Name
	res.SizeBytes = src.of.Info.Size()
	res.ModifiedAt = src.of.Info.ModTime().In(s.loc).Format(time.RFC3339Nano)
	res.Readable = true
	res.Compressed = src.of.Compressed
	res.Seekable = src.of.Seekable

	budget := newScanBudget(64<<10, s.limits.QueryTimeout)
	sample := s.sampleTimeRange(ctx, src, budget)
	s.setBudgetMeta(&res.Meta, budget)
	res.SampledTime = sample
	if sample == nil {
		res.Meta.Warn("Timestamps could not be sampled from the file.")
	}
	return res, nil
}

// sampleTimeRange reads only the first and last physical lines.
func (s *Service) sampleTimeRange(ctx context.Context, src *fileSource, budget *scanBudget) *SampledRange {
	if src.size == 0 {
		return nil
	}
	var first, last *time.Time

	if src.of.Compressed {
		fr, err := s.forwardReader(ctx, src, 0, -1, budget)
		if err == nil {
			if line, lerr := fr.Next(); lerr == nil {
				p := parseLine(line.Text, s.loc)
				first = p.Timestamp
			}
		}
	} else {
		fr := newForwardReader(ctx, src.of.File, 0, src.size, s.dec, 4096, s.limits.MaxLineBytes, budget)
		if line, lerr := fr.Next(); lerr == nil {
			p := parseLine(line.Text, s.loc)
			first = p.Timestamp
		}
		rr := newReverseReader(ctx, src.of.File, src.size, s.dec, 4096, s.limits.MaxLineBytes, budget)
		if line, lerr := rr.Next(); lerr == nil {
			p := parseLine(line.Text, s.loc)
			last = p.Timestamp
		} else if lerr != io.EOF {
			last = nil
		}
	}

	if first == nil && last == nil {
		return nil
	}
	sample := &SampledRange{Sampled: true, Complete: first != nil && last != nil}
	if first != nil {
		sample.Start = formatTimestamp(first)
	}
	if last != nil {
		sample.End = formatTimestamp(last)
	}
	return sample
}
