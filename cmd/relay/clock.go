package main

import "time"

// serverClock is the one source of time for decisions that grant, refuse,
// expire, retry or close a window. startServerCore builds it once and hands
// it to each consumer; there is no package-level clock.
type serverClock interface {
	Now() time.Time
	// After fires once the clock reaches Now()+d.
	After(d time.Duration) <-chan time.Time
}

type wallClock struct{}

func (wallClock) Now() time.Time                         { return time.Now() }
func (wallClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
