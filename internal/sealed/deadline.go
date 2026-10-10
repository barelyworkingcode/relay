package sealed

import (
	"fmt"
	"time"
)

// errCopyTimeout is the refusal for a keychain read that did not answer. It
// wraps ErrKeyUnreadable on purpose: from the operator's chair an unanswered
// read and a foreign-ACL item both mean "an item is there and relay could not
// use it". The file provider's slow fault returns the same text.
func errCopyTimeout(service, account string) error {
	return fmt.Errorf("%w: the login keychain did not answer within %s for %s/%s -- "+
		"consistent with a confirmation dialog waiting on a human relay will not wait for",
		ErrKeyUnreadable, keychainReadTimeout, service, account)
}

// errDeleteTimeout is the refusal for a keychain delete that did not finish.
func errDeleteTimeout(service, account string) error {
	return fmt.Errorf("sealed: deleting keychain item %s/%s did not complete within %s -- "+
		"consistent with a confirmation dialog waiting on a human relay will not wait for",
		service, account, keychainReadTimeout)
}

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
