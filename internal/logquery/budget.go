package logquery

import (
	"context"
	"errors"
	"time"
)

// errBudgetExceeded is returned by the readers once the byte budget is spent.
var errBudgetExceeded = errors.New("scan byte budget exceeded")

// scanBudget tracks bytes read during a single tool call. All files in a
// multi-file query share the same budget.
type scanBudget struct {
	limit     int64
	used      int64
	exhausted bool
	deadline  time.Time
}

func newScanBudget(limit int64, timeout time.Duration) *scanBudget {
	return &scanBudget{limit: limit, deadline: time.Now().Add(timeout)}
}

func (b *scanBudget) consume(n int64) error {
	if b == nil {
		return nil
	}
	if b.exhausted {
		return errBudgetExceeded
	}
	if n < 0 {
		n = 0
	}
	if b.used+n > b.limit {
		b.exhausted = true
		return errBudgetExceeded
	}
	b.used += n
	return nil
}

func (b *scanBudget) remaining() int64 {
	if b == nil {
		return 0
	}
	r := b.limit - b.used
	if r < 0 {
		return 0
	}
	return r
}

func (b *scanBudget) checkContext(ctx context.Context) error {
	if b == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if !b.deadline.IsZero() && time.Now().After(b.deadline) {
		return context.DeadlineExceeded
	}
	return nil
}
