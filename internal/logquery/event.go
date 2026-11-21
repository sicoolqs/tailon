package logquery

import (
	"io"
	"strings"
	"time"
)

// forwardEventScanner groups physical lines into logical events while reading
// forward through a file.
type forwardEventScanner struct {
	r             lineReader
	loc           *time.Location
	maxEventBytes int
	pending       *Line
}

func newForwardEventScanner(r lineReader, loc *time.Location, maxEventBytes int) *forwardEventScanner {
	return &forwardEventScanner{r: r, loc: loc, maxEventBytes: maxEventBytes}
}

func (s *forwardEventScanner) Next() (*Event, error) {
	var (
		lines      []Line
		total      int
		truncating bool
	)
	for {
		var line Line
		if s.pending != nil {
			line = *s.pending
			s.pending = nil
		} else {
			l, err := s.r.Next()
			if err != nil {
				if err == io.EOF {
					if len(lines) == 0 {
						return nil, io.EOF
					}
					break
				}
				return nil, err
			}
			line = l
		}

		if strings.TrimSpace(line.Text) == "" {
			if len(lines) == 0 {
				continue
			}
			break // a blank line terminates the current event
		}

		if len(lines) == 0 {
			lines = append(lines, line)
			total += len(line.Text)
			continue
		}

		cont := isContinuation(line.Text)
		if truncating {
			if cont {
				continue
			}
			s.pending = &line
			break
		}
		if cont {
			if total+len(line.Text) > s.maxEventBytes {
				truncating = true
				continue
			}
			lines = append(lines, line)
			total += len(line.Text)
			continue
		}

		s.pending = &line
		break
	}

	ev := newEvent(lines, s.loc)
	if ev == nil {
		return nil, io.EOF
	}
	ev.Truncated = ev.Truncated || truncating
	return ev, nil
}

// reverseEventScanner emits logical events from newest to oldest. When it
// encounters continuation lines it keeps reading backwards until the line
// that started the event is found.
type reverseEventScanner struct {
	r             lineReader
	loc           *time.Location
	maxEventBytes int
}

func newReverseEventScanner(r lineReader, loc *time.Location, maxEventBytes int) *reverseEventScanner {
	return &reverseEventScanner{r: r, loc: loc, maxEventBytes: maxEventBytes}
}

func (s *reverseEventScanner) Next() (*Event, error) {
	for {
		line, err := s.r.Next()
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(line.Text) == "" {
			continue
		}
		if !isContinuation(line.Text) {
			return newEvent([]Line{line}, s.loc), nil
		}

		group := []Line{line}
		total := len(line.Text)
		truncating := false
		for {
			l, err := s.r.Next()
			if err != nil {
				if err == io.EOF {
					break
				}
				return nil, err
			}
			if strings.TrimSpace(l.Text) == "" {
				continue
			}
			if isContinuation(l.Text) {
				if total+len(l.Text) > s.maxEventBytes {
					truncating = true
					continue
				}
				group = append(group, l)
				total += len(l.Text)
				continue
			}
			group = append(group, l)
			break
		}

		reverseLines(group)
		ev := newEvent(group, s.loc)
		ev.Truncated = ev.Truncated || truncating
		return ev, nil
	}
}

func reverseLines(lines []Line) {
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
}

func formatTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.RFC3339Nano)
	return &s
}

func levelPtr(level string) *string {
	if level == "" {
		return nil
	}
	s := level
	return &s
}

func nonEmptyPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// MessageText returns the event message, optionally including the continuation
// lines that make up a multiline exception event.
func (e *Event) MessageText(includeStack bool) string {
	if e == nil {
		return ""
	}
	if !includeStack || len(e.Stack) == 0 {
		return e.Message
	}
	var b strings.Builder
	b.WriteString(e.Message)
	for _, line := range e.Stack {
		b.WriteByte('\n')
		b.WriteString(line)
	}
	return b.String()
}
