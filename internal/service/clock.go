package service

import (
	"sync"
	"time"
)

// Clock abstracts "now" and a cancellable delay so restart backoff can be
// driven deterministically in a test instead of waiting out real timers.
// Production leaves Registry.Clock nil, which reads as realClock.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// FakeClock is a manually-advanced Clock. Nothing in production constructs
// one; a supervision test injects it via Registry.Clock so a backoff
// sequence and a give-up can be driven attempt by attempt, and a "stayed up
// long enough to be stable" run can be simulated without an real wait.
type FakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWaiter
}

type fakeWaiter struct {
	deadline time.Time
	ch       chan time.Time
}

// NewFakeClock starts the clock at start. Tests that don't care about the
// absolute value typically pass time.Now().
func NewFakeClock(start time.Time) *FakeClock {
	return &FakeClock{now: start}
}

func (f *FakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *FakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	deadline := f.now.Add(d)
	if d <= 0 {
		ch <- deadline
		return ch
	}
	f.waiters = append(f.waiters, fakeWaiter{deadline: deadline, ch: ch})
	return ch
}

// Advance moves the clock forward by d and fires every waiter whose
// deadline has now passed. Filtering in place (remaining shares waiters'
// backing array) is safe here: it only ever writes to an index at or before
// the one it is reading.
func (f *FakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	remaining := f.waiters[:0]
	for _, w := range f.waiters {
		if !w.deadline.After(f.now) {
			w.ch <- w.deadline
		} else {
			remaining = append(remaining, w)
		}
	}
	f.waiters = remaining
}
