// Package backoff is the capped, jittered exponential backoff shared by the
// outbox relay and the update consumer (DESIGN.md §6).
package backoff

import (
	"context"
	"math/rand/v2"
	"time"
)

// Backoff doubles a wait from Min up to Max. The zero value is not usable;
// build one with New.
type Backoff struct {
	min, max, next time.Duration
}

func New(minWait, maxWait time.Duration) *Backoff {
	return &Backoff{min: minWait, max: maxWait, next: minWait}
}

// Next returns the wait to use now, jittered, and doubles the one after it.
func (b *Backoff) Next() time.Duration {
	d := b.next
	b.next = min(2*b.next, b.max)
	return Jitter(d)
}

// Reset goes back to the minimum wait, after a success.
func (b *Backoff) Reset() {
	b.next = b.min
}

// Jitter spreads d over [d/2, d], so processes that failed together (say,
// replicas when the database went away) don't all retry at the same instant.
func Jitter(d time.Duration) time.Duration {
	half := d / 2
	return half + rand.N(half+1) //nolint:gosec // timing jitter, not security
}

// Sleep waits for d, or until ctx is done, in which case it returns ctx.Err().
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
