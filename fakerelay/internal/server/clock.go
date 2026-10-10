package server

import (
	"sync"
	"time"
)

// SimClock is the instance clock: real time shifted by an offset a test can
// set or advance.
type SimClock struct {
	mu     sync.Mutex
	offset time.Duration
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
}

func (c *SimClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.offset += d
	c.mu.Unlock()
}
