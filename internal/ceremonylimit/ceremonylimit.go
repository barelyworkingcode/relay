// Package ceremonylimit is a pure sync/time backoff for a two-step ceremony
// that is unauthenticated by construction. Two unrelated callers drive one:
// the WebAuthn login ceremony (internal/login's WebAuthnVerifier) and the
// network CSR-lodging listener (cmd/relay's enrolment-request table). Neither
// caller's protocol, error type or vocabulary appears here — a Limiter knows
// only that something succeeded or failed and how long to make the next
// caller wait.
package ceremonylimit

import (
	"sync"
	"testing"
	"time"
)

const (
	// FailureGrace is exported because both callers' own tests drive a
	// Limiter past its grace count to reach the escalated backoff, and a
	// magic 3 repeated at each call site would drift from this one the
	// first time the curve was retuned.
	FailureGrace         = 3
	ceremonyFailureDelay = 2 * time.Second
	ceremonyMaxDelay     = 30 * time.Second
	ceremonyFailureDecay = 5 * time.Minute
)

// Limiter returns the delay it wants rather than sleeping: a caller that
// slept would hold a handler goroutine per attempt, which hands the
// unauthenticated caller a cheaper denial of service than the one being
// rate-limited.
type Limiter struct {
	mu          sync.Mutex
	failures    int
	nextAllowed time.Time
	lastFailure time.Time
	now         func() time.Time
}

func New() *Limiter {
	return &Limiter{now: time.Now}
}

func (l *Limiter) Allow() (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if !l.lastFailure.IsZero() && now.Sub(l.lastFailure) >= ceremonyFailureDecay {
		l.failures = 0
		l.nextAllowed = time.Time{}
	}
	if now.Before(l.nextAllowed) {
		return l.nextAllowed.Sub(now), false
	}
	return 0, true
}

func (l *Limiter) RecordFailure() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.failures++
	l.lastFailure = now
	over := l.failures - FailureGrace
	if over <= 0 {
		return
	}
	delay := time.Duration(over) * ceremonyFailureDelay
	if delay > ceremonyMaxDelay {
		delay = ceremonyMaxDelay
	}
	l.nextAllowed = now.Add(delay)
}

func (l *Limiter) RecordSuccess() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures = 0
	l.nextAllowed = time.Time{}
	l.lastFailure = time.Time{}
}

// SetClock is a test seam, matching enrolment.Budgets.SetClock's shape.
func (l *Limiter) SetClock(fn func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = fn
}

// Failures and NextAllowed are the read seam the two callers' own tests
// need: those tests live beside the Limiter each embeds (cmd/relay's
// enrolment-request table, internal/login's WebAuthnVerifier), and both
// embedders reach a Limiter across a package boundary where its fields are
// unexported. Both panic outside a test binary — production has no business
// reading a running total, only Allow/RecordFailure/RecordSuccess do.
func (l *Limiter) Failures() int {
	if !testing.Testing() {
		panic("ceremonylimit: Failures is a test seam and must not be reached in a shipped binary")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.failures
}

func (l *Limiter) NextAllowed() time.Time {
	if !testing.Testing() {
		panic("ceremonylimit: NextAllowed is a test seam and must not be reached in a shipped binary")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.nextAllowed
}
