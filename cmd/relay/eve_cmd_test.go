package main

// CLI coverage for `relay eve enrol`, mirroring login_cmd_test.go's
// TestLoginEnrol_CLIPrintsCodeButStoresOnlyItsHash: eveEnrol is brokered
// (ADR-017 decision 2), so this needs a real bridge server behind a wired
// EveEnrolmentOps over the same store the output is checked against. The
// "service not running" refusal is covered generically, alongside every
// other brokered command, by cli_subprocess_test.go's brokeredCLICommands
// table.

import (
	"strings"
	"testing"
)

func TestEveEnrol_CLIPrintsExpiryAndInstructions(t *testing.T) {
	store, _ := lcNewStore(t)
	serveBroker(t, newBrokerRouter(t, store, nil))

	out := lcCapture(t, func() { eveEnrol() })

	if !strings.Contains(out, "eve passkey enrolment open until ") {
		t.Fatalf("output does not open with the expected line: %q", out)
	}
	if !strings.Contains(out, "(5m0s, single use)") {
		t.Fatalf("output does not name the TTL and single-use rule: %q", out)
	}
	if !strings.Contains(out, `open Eve, and tap "Add this browser"`) {
		t.Fatalf("output does not tell the operator what to do next: %q", out)
	}

	if store.Get().EveEnrolment == nil {
		t.Fatal("no eve_enrolment record was written")
	}
}
