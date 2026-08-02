// Package ratelimit provides the per-token and per-account request limiting
// that the webhook API advertises.
//
// notiphy is self-hosted, so limiting is off by default — it exists for wire
// parity with Hark's 429 behaviour and to stop a runaway script from burning
// through a phone's notification budget.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter is a fixed-window counter keyed by an arbitrary string.
//
// A fixed window rather than a sliding one is deliberate: the limit exists to
// stop runaway loops, not to police a paid quota, and the window boundary
// matches the Retry-After hint we return.
type Limiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	buckets map[string]*bucket
	// now is swappable so tests do not have to sleep through a window.
	now func() time.Time
}

type bucket struct {
	count      int
	windowEnds time.Time
}

// New returns a Limiter allowing limit requests per window per key. A limit of
// zero or less disables limiting entirely.
func New(limit int, window time.Duration) *Limiter {
	if window <= 0 {
		window = time.Minute
	}
	return &Limiter{
		limit:   limit,
		window:  window,
		buckets: make(map[string]*bucket),
		now:     time.Now,
	}
}

// Enabled reports whether this limiter enforces anything.
func (l *Limiter) Enabled() bool { return l != nil && l.limit > 0 }

// Allow records a request against key and reports whether it may proceed. The
// second return is how long the caller should wait, meaningful only when the
// request was denied.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	if !l.Enabled() {
		return true, 0
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[key]
	if !ok || now.After(b.windowEnds) {
		l.buckets[key] = &bucket{count: 1, windowEnds: now.Add(l.window)}
		return true, 0
	}

	if b.count >= l.limit {
		return false, b.windowEnds.Sub(now)
	}
	b.count++
	return true, 0
}

// Cleanup drops buckets whose window has closed, so a server handed many
// distinct tokens does not accumulate them forever.
func (l *Limiter) Cleanup() {
	if !l.Enabled() {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	for k, b := range l.buckets {
		if now.After(b.windowEnds) {
			delete(l.buckets, k)
		}
	}
}
