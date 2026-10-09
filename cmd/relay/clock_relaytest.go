//go:build relaytest

package main

import (
	"sync"
	"time"
)

// testClock is wall time plus an offset. Untouched, it runs at wall time;
// Set and Advance move the offset. It lives in memory, so a restart returns
// to wall time.
type testClock struct {
	mu      sync.Mutex
	offset  time.Duration
	waiters map[*clockWaiter]struct{}
}

type clockWaiter struct {
	deadline time.Time // in test-clock time
	ch       chan time.Time
	timer    *time.Timer
}

func newServerClock(configDir string) serverClock {
	return &testClock{waiters: map[*clockWaiter]struct{}{}}
}

func (c *testClock) nowLocked() time.Time { return time.Now().Add(c.offset) }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nowLocked()
}

// Offset is how far the clock is from wall time.
func (c *testClock) Offset() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.offset
}

// After fires when the clock reaches its deadline, by real time passing or by
// a clock move. The channel is buffered so a firing never blocks on a reader.
func (c *testClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := &clockWaiter{deadline: c.nowLocked().Add(d), ch: make(chan time.Time, 1)}
	c.waiters[w] = struct{}{}
	w.timer = time.AfterFunc(d, func() { c.fire(w) })
	return w.ch
}

// Set moves the clock to t and returns the new now. Waiters now due fire
// before it returns.
func (c *testClock) Set(t time.Time) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset = time.Until(t)
	return c.settleLocked()
}

// Advance moves the clock forward by by and returns the new now.
func (c *testClock) Advance(by time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += by
	return c.settleLocked()
}

// settleLocked fires every due waiter and re-arms the rest against the new
// offset. A backward Set leaves a waiter further from due than its real timer
// believes, so re-arming is what keeps it from firing early.
func (c *testClock) settleLocked() time.Time {
	now := c.nowLocked()
	for w := range c.waiters {
		w.timer.Stop()
		if !now.Before(w.deadline) {
			c.fireLocked(w, now)
			continue
		}
		w.timer.Reset(w.deadline.Sub(now))
	}
	return now
}

func (c *testClock) fire(w *clockWaiter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, live := c.waiters[w]; !live {
		return
	}
	now := c.nowLocked()
	if now.Before(w.deadline) {
		w.timer.Reset(w.deadline.Sub(now))
		return
	}
	c.fireLocked(w, now)
}

func (c *testClock) fireLocked(w *clockWaiter, now time.Time) {
	delete(c.waiters, w)
	w.ch <- now
}
