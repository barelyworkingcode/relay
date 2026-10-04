package logging

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// DefaultRepeatInterval is how long a Repeat holds back a failure that keeps
// happening before it writes the next line.
const DefaultRepeatInterval = time.Minute

// Repeat limits one failing site to a line per interval. The first occurrence
// is written at once; later ones inside the interval are counted, and the next
// line written carries the count as "repeats". There is no timer: a count that
// is never followed by another occurrence is never written, which is deliberate
// (nothing is running to write it).
//
// Each failure site owns its own Repeat. A shared one would let a noisy site
// hide a quiet one.
type Repeat struct {
	interval time.Duration
	now      func() time.Time

	mu         sync.Mutex
	written    bool
	lastLine   time.Time
	suppressed int
}

// NewRepeat returns a limiter. An interval of zero or less means
// DefaultRepeatInterval; a nil now means time.Now.
func NewRepeat(interval time.Duration, now func() time.Time) *Repeat {
	if interval <= 0 {
		interval = DefaultRepeatInterval
	}
	if now == nil {
		now = time.Now
	}
	return &Repeat{interval: interval, now: now}
}

// Log writes the line through the default logger, or counts it and returns
// false. A nil *Repeat writes every occurrence, so a zero-value owner is safe.
func (r *Repeat) Log(ctx context.Context, level slog.Level, msg string, attrs ...slog.Attr) bool {
	if r == nil {
		slog.Default().LogAttrs(ctx, level, msg, attrs...)
		return true
	}
	r.mu.Lock()
	now := r.now()
	if r.written && now.Before(r.lastLine.Add(r.interval)) {
		r.suppressed++
		r.mu.Unlock()
		return false
	}
	n := r.suppressed
	r.suppressed = 0
	r.written = true
	r.lastLine = now
	r.mu.Unlock()

	if n > 0 {
		attrs = append(attrs[:len(attrs):len(attrs)], slog.Int("repeats", n))
	}
	slog.Default().LogAttrs(ctx, level, msg, attrs...)
	return true
}
