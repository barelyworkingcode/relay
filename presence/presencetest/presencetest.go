// Package presencetest is the injectable fake presence.Provider the rest of
// the suite wires in place of the real LocalAuthentication provider.
//
// It lives in a separate package on purpose (ADR-016 decision 8, §6.8 of the
// ADR-017 implementation spec): "a weakening introduced for a test is the
// weakening most likely to survive." Production code never imports this
// package — TestPresence_SeamIsNotLinkedIntoTheBinary in the presence
// package proves it by parsing every non-test source file in the module,
// and because Go links only what is imported, that is a proof the fake
// cannot reach a shipped binary. The init panic below is the second line of
// defence if that proof ever regresses.
package presencetest

import (
	"context"
	"sync"
	"testing"

	"relaygo/presence"
)

func init() {
	if !testing.Testing() {
		panic("presencetest: linked into a non-test binary; this package must never ship")
	}
}

type fixed struct{ result error }

func (f fixed) Evaluate(context.Context, string) error { return f.result }

// Allow returns a Provider whose Evaluate always succeeds.
func Allow() presence.Provider { return fixed{result: nil} }

// Deny returns a Provider whose Evaluate always refuses, as if the user
// clicked Cancel.
func Deny() presence.Provider { return fixed{result: presence.ErrRefused} }

// NoSession returns a Provider whose Evaluate reports that presence
// checking is unavailable — the shape a sessionless LocalAuthentication
// context produces (§6.5.1), reachable in the hermetic suite only through
// this fake, never through the real provider.
func NoSession() presence.Provider { return fixed{result: presence.ErrUnavailable} }

// Recording wraps a fixed result and counts calls, so a test can assert
// Evaluate was never reached — the proof a peer-with-no-graphic-access
// refusal (or an audit-disabled refusal) needs: that it happened before any
// prompt would have.
type Recording struct {
	mu      sync.Mutex
	result  error
	calls   int
	reasons []string
}

// NewRecording returns a Recording whose Evaluate always returns result.
func NewRecording(result error) *Recording {
	return &Recording{result: result}
}

func (r *Recording) Evaluate(_ context.Context, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.reasons = append(r.reasons, reason)
	return r.result
}

// Calls reports how many times Evaluate has been called.
func (r *Recording) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// Reasons returns every localizedReason Evaluate was called with, in order.
func (r *Recording) Reasons() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.reasons))
	copy(out, r.reasons)
	return out
}
