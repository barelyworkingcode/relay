package sealed

import "time"

// callWithin runs fn on its own goroutine and returns its result, or false
// once d has passed with fn still running. The goroutine is never cancelled:
// a blocked keychain call cannot be, so fn must own everything it touches
// (allocate and free its own C strings inside the closure) and the result
// channel is buffered so a late send never blocks.
func callWithin[T any](d time.Duration, fn func() T) (T, bool) {
	done := make(chan T, 1)
	go func() { done <- fn() }()
	select {
	case v := <-done:
		return v, true
	case <-time.After(d):
		var zero T
		return zero, false
	}
}
