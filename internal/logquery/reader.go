package logquery

import (
	"bytes"
	"context"
	"io"
)

// Line is one physical log line with its byte range in the underlying file.
// End is exclusive. For gzip input, offsets are logical offsets in the
// decompressed stream and must not be used with the plain file readers.
type Line struct {
	Text      string
	Start     int64
	End       int64
	Truncated bool
	Replaced  bool
}

type lineReader interface {
	Next() (Line, error)
}

// forwardReader reads physical lines from a reader that supports random
// access. It never holds more than roughly one line plus one buffer worth of
// data in memory.
type forwardReader struct {
	ra     io.ReaderAt
	start  int64
	size   int64
	ctx    context.Context
	budget *scanBudget
	dec    textDecoder

	maxLine int
	chunk   int

	data      []byte
	dataStart int64
	eof       bool
	skipping  bool
}

func newForwardReader(ctx context.Context, ra io.ReaderAt, start, end int64, dec textDecoder, chunk, maxLine int, budget *scanBudget) *forwardReader {
	if start < 0 {
		start = 0
	}
	if end < start {
		end = start
	}
	return &forwardReader{
		ra: ra, start: start, size: end, ctx: ctx, budget: budget, dec: dec,
		maxLine: maxLine, chunk: chunk, dataStart: start,
	}
}

func (r *forwardReader) Next() (Line, error) {
	for {
		if err := r.budget.checkContext(r.ctx); err != nil {
			return Line{}, err
		}

		if r.skipping {
			if i := bytes.IndexByte(r.data, '\n'); i >= 0 {
				r.discard(i + 1)
				r.skipping = false
				continue
			}
			r.discard(len(r.data))
			if r.eof || r.dataStart >= r.size {
				return Line{}, io.EOF
			}
			if err := r.fill(); err != nil {
				return Line{}, err
			}
			continue
		}

		if i := bytes.IndexByte(r.data, '\n'); i >= 0 {
			line := r.makeLine(r.data[:i], r.dataStart, r.dataStart+int64(i)+1)
			r.discard(i + 1)
			return line, nil
		}

		if r.eof || r.dataStart+int64(len(r.data)) >= r.size {
			if len(r.data) == 0 {
				return Line{}, io.EOF
			}
			line := r.makeLine(r.data, r.dataStart, r.size)
			r.discard(len(r.data))
			return line, nil
		}

		if len(r.data) > r.maxLine {
			line := r.makeLine(r.data[:r.maxLine], r.dataStart, r.dataStart+int64(r.maxLine))
			line.Truncated = true
			r.discard(r.maxLine)
			r.skipping = true
			return line, nil
		}

		if err := r.fill(); err != nil {
			if err == io.EOF {
				r.eof = true
				continue
			}
			return Line{}, err
		}
	}
}

func (r *forwardReader) fill() error {
	pos := r.dataStart + int64(len(r.data))
	if pos >= r.size {
		r.eof = true
		return io.EOF
	}
	n := int64(r.chunk)
	if rem := r.size - pos; rem < n {
		n = rem
	}
	if rem := r.budget.remaining(); rem < n {
		n = rem
	}
	if n <= 0 {
		return errBudgetExceeded
	}
	buf := make([]byte, n)
	nn, err := r.ra.ReadAt(buf, pos)
	if nn > 0 {
		if cerr := r.budget.consume(int64(nn)); cerr != nil {
			return cerr
		}
		r.data = append(r.data, buf[:nn]...)
	}
	if err != nil {
		if err == io.EOF {
			r.eof = true
			return nil
		}
		return err
	}
	if nn == 0 {
		r.eof = true
	}
	return nil
}

func (r *forwardReader) discard(n int) {
	if n <= 0 {
		return
	}
	r.data = r.data[n:]
	r.dataStart += int64(n)
	if len(r.data) == 0 {
		r.data = nil
	}
}

func (r *forwardReader) makeLine(raw []byte, start, end int64) Line {
	text, replaced := r.dec.Decode(trimCR(raw))
	return Line{Text: text, Start: start, End: end, Replaced: replaced}
}

// gzipForwardReader sequentially decompresses gzip input. Random access is
// impossible, so callers must use only forward scans with this reader.
type gzipForwardReader struct {
	r      io.Reader
	ctx    context.Context
	budget *scanBudget
	dec    textDecoder

	maxLine int
	chunk   int

	data      []byte
	dataStart int64
	eof       bool
	skipping  bool
}

func newGzipForwardReader(ctx context.Context, r io.Reader, dec textDecoder, chunk, maxLine int, budget *scanBudget) *gzipForwardReader {
	return &gzipForwardReader{
		r: r, ctx: ctx, budget: budget, dec: dec,
		maxLine: maxLine, chunk: chunk,
	}
}

func (r *gzipForwardReader) Next() (Line, error) {
	for {
		if err := r.budget.checkContext(r.ctx); err != nil {
			return Line{}, err
		}

		if r.skipping {
			if i := bytes.IndexByte(r.data, '\n'); i >= 0 {
				r.discard(i + 1)
				r.skipping = false
				continue
			}
			r.discard(len(r.data))
			if r.eof {
				return Line{}, io.EOF
			}
			if err := r.fill(); err != nil {
				return Line{}, err
			}
			continue
		}

		if i := bytes.IndexByte(r.data, '\n'); i >= 0 {
			line := r.makeLine(r.data[:i], r.dataStart, r.dataStart+int64(i)+1)
			r.discard(i + 1)
			return line, nil
		}
		if r.eof {
			if len(r.data) == 0 {
				return Line{}, io.EOF
			}
			end := r.dataStart + int64(len(r.data))
			line := r.makeLine(r.data, r.dataStart, end)
			r.discard(len(r.data))
			return line, nil
		}
		if len(r.data) > r.maxLine {
			line := r.makeLine(r.data[:r.maxLine], r.dataStart, r.dataStart+int64(r.maxLine))
			line.Truncated = true
			r.discard(r.maxLine)
			r.skipping = true
			return line, nil
		}
		if err := r.fill(); err != nil {
			if err == io.EOF {
				r.eof = true
				continue
			}
			return Line{}, err
		}
	}
}

func (r *gzipForwardReader) fill() error {
	if r.eof {
		return io.EOF
	}
	n := r.chunk
	if rem := r.budget.remaining(); int64(rem) < int64(n) {
		n = int(rem)
	}
	if n <= 0 {
		return errBudgetExceeded
	}
	buf := make([]byte, n)
	nn, err := r.r.Read(buf)
	if nn > 0 {
		if cerr := r.budget.consume(int64(nn)); cerr != nil {
			return cerr
		}
		r.data = append(r.data, buf[:nn]...)
	}
	if err != nil {
		if err == io.EOF {
			r.eof = true
			return nil
		}
		return err
	}
	return nil
}

func (r *gzipForwardReader) discard(n int) {
	if n <= 0 {
		return
	}
	r.data = r.data[n:]
	r.dataStart += int64(n)
	if len(r.data) == 0 {
		r.data = nil
	}
}

func (r *gzipForwardReader) makeLine(raw []byte, start, end int64) Line {
	text, replaced := r.dec.Decode(trimCR(raw))
	return Line{Text: text, Start: start, End: end, Replaced: replaced}
}

// reverseReader reads physical lines from the end of a plain file. It keeps a
// bounded window of bytes and loads older chunks on demand.
type reverseReader struct {
	ra     io.ReaderAt
	size   int64
	ctx    context.Context
	budget *scanBudget
	dec    textDecoder

	maxLine int
	chunk   int

	pending      []byte
	pendingStart int64
	pendingEnd   int64
	initialized  bool
	skipping     bool
}

func newReverseReader(ctx context.Context, ra io.ReaderAt, end int64, dec textDecoder, chunk, maxLine int, budget *scanBudget) *reverseReader {
	if end < 0 {
		end = 0
	}
	return &reverseReader{
		ra: ra, size: end, ctx: ctx, budget: budget, dec: dec,
		maxLine: maxLine, chunk: chunk,
	}
}

func (r *reverseReader) Next() (Line, error) {
	for {
		if err := r.budget.checkContext(r.ctx); err != nil {
			return Line{}, err
		}

		if !r.initialized {
			r.initialized = true
			r.pendingStart, r.pendingEnd = r.size, r.size
			if r.size > 0 {
				var b [1]byte
				if err := r.budget.consume(1); err != nil {
					return Line{}, err
				}
				n, err := r.ra.ReadAt(b[:], r.size-1)
				if n == 1 && b[0] == '\n' {
					r.pendingStart--
					r.pendingEnd--
				} else if err != nil && err != io.EOF {
					return Line{}, err
				}
			}
			if r.pendingStart == 0 {
				return Line{}, io.EOF
			}
		}

		if r.skipping {
			if i := bytes.LastIndexByte(r.pending, '\n'); i >= 0 {
				r.pending = r.pending[:i]
				r.pendingEnd = r.pendingStart + int64(i)
				r.skipping = false
				continue
			}
			if r.pendingStart == 0 {
				r.pending = nil
				r.pendingStart, r.pendingEnd = 0, 0
				r.skipping = false
				continue
			}
			if err := r.fillBack(); err != nil {
				return Line{}, err
			}
			continue
		}

		if r.pendingStart == 0 && len(r.pending) == 0 {
			return Line{}, io.EOF
		}

		if i := bytes.LastIndexByte(r.pending, '\n'); i >= 0 {
			text, replaced := r.dec.Decode(trimCR(r.pending[i+1:]))
			line := Line{
				Text:     text,
				Start:    r.pendingStart + int64(i) + 1,
				End:      r.pendingEnd,
				Replaced: replaced,
			}
			r.pending = r.pending[:i]
			r.pendingEnd = r.pendingStart + int64(i)
			return line, nil
		}

		if r.pendingStart == 0 {
			text, replaced := r.dec.Decode(trimCR(r.pending))
			line := Line{Text: text, Start: 0, End: r.pendingEnd, Replaced: replaced}
			r.pending = nil
			r.pendingStart, r.pendingEnd = 0, 0
			return line, nil
		}

		if len(r.pending) > r.maxLine {
			start := r.pendingEnd - int64(r.maxLine)
			if start < 0 {
				start = 0
			}
			text, replaced := r.dec.Decode(r.pending[len(r.pending)-r.maxLine:])
			line := Line{
				Text:      text,
				Start:     start,
				End:       r.pendingEnd,
				Truncated: true,
				Replaced:  replaced,
			}
			r.pending = nil
			r.pendingStart = r.pendingEnd
			r.skipping = true
			return line, nil
		}

		if err := r.fillBack(); err != nil {
			return Line{}, err
		}
	}
}

func (r *reverseReader) fillBack() error {
	if r.pendingStart == 0 {
		return io.EOF
	}
	n := int64(r.chunk)
	if r.pendingStart < n {
		n = r.pendingStart
	}
	if rem := r.budget.remaining(); rem < n {
		n = rem
	}
	if n <= 0 {
		return errBudgetExceeded
	}
	off := r.pendingStart - n
	buf := make([]byte, n)
	nn, err := r.ra.ReadAt(buf, off)
	if nn > 0 {
		if cerr := r.budget.consume(int64(nn)); cerr != nil {
			return cerr
		}
		r.pending = append(buf[:nn], r.pending...)
		r.pendingStart = off
	}
	if err != nil && err != io.EOF {
		return err
	}
	if nn == 0 {
		return io.EOF
	}
	return nil
}

func trimCR(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\r' {
		return b[:n-1]
	}
	return b
}
