package main

import (
	"errors"
	"strings"
	"testing"

	"relaygo/presence"
)

// TestAdminOpErrorText_ExpandsTheNoSessionRefusal is rule 4's CLI-legibility
// requirement (ADR-017 implementation spec §6.6): a privileged operation
// over a session that cannot show a prompt must read as the named refusal,
// not a generic RPC error. The bridge round trip discards the error's type
// (checkError wraps every non-nil response in a plain fmt.Errorf), so this
// is exercised against exactly the shape that survives the wire — a message
// containing presence.ErrNoSession's text, wrapped the way bridge's
// checkError wraps it.
func TestAdminOpErrorText_ExpandsTheNoSessionRefusal(t *testing.T) {
	wireErr := errors.New("bridge error (code -32603): " + presence.ErrNoSession.Error())

	got := adminOpErrorText(wireErr)

	if !strings.Contains(got, "cannot show a prompt") || !strings.Contains(got, "over SSH") {
		t.Fatalf("adminOpErrorText did not expand the no-session refusal: %q", got)
	}
	if !strings.Contains(got, "relay audit") || !strings.Contains(got, "relay grant") {
		t.Fatalf("expanded refusal does not name the read commands that still work: %q", got)
	}
}

// TestAdminOpErrorText_PassesThroughEverythingElse is the other half: a
// validation error, a not-found, an issuance-auditing refusal — anything
// that isn't the no-session sentinel — must print exactly as the service
// returned it, not get silently rewritten into the SSH text.
func TestAdminOpErrorText_PassesThroughEverythingElse(t *testing.T) {
	wireErr := errors.New(`bridge error (code -32602): a credential name is required`)

	got := adminOpErrorText(wireErr)

	if got != wireErr.Error() {
		t.Fatalf("adminOpErrorText = %q, want it unchanged: %q", got, wireErr.Error())
	}
	if strings.Contains(got, "cannot show a prompt") {
		t.Fatalf("an unrelated error was rewritten into the SSH refusal: %q", got)
	}
}
