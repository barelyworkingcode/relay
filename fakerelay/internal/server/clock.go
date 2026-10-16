package server

import (
	"sync"
	"time"
)

// SimClock is the instance clock: real time shifted by an offset a test can
// set or advance.
type SimClock struct {
	mu       sync.Mutex
	offset   time.Duration
	watchers []func(now time.Time)
}

// Watch registers fn to run after every Set or Advance, on the caller's
// goroutine and before it returns, so a POST /v1/clock answers only after the
// watchers finished. It makes SimClock a ClockWatcher.
func (c *SimClock) Watch(fn func(now time.Time)) {
	c.mu.Lock()
	c.watchers = append(c.watchers, fn)
	c.mu.Unlock()
}

// moved runs the watchers outside the lock, so one may read the clock.
func (c *SimClock) moved() {
	c.mu.Lock()
	ws := append([]func(time.Time){}, c.watchers...)
	c.mu.Unlock()
	now := c.Now()
	for _, fn := range ws {
		fn(now)
	}
}

func NewClock() *SimClock { return &SimClock{} }

func (c *SimClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *SimClock) Set(t time.Time) {
	c.mu.Lock()
	c.offset = time.Until(t)
	c.mu.Unlock()
	c.moved()
}

func (c *SimClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.offset += d
	c.mu.Unlock()
	c.moved()
}
