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

	"github.com/barelyworkingcode/relay/internal/config"
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

// TestEveList_CLINeverPrintsKeyMaterial mirrors
// TestLoginList_CLINeverPrintsKeyMaterial: the mirror carries no public key
// at all (decision 8), so this mainly proves `eve list` reads the mirror
// straight off disk and renders every column, including STATUS.
func TestEveList_CLINeverPrintsKeyMaterial(t *testing.T) {
	store, _ := lcNewStore(t)
	assertNoErr(t, store.With(func(s *config.Settings) {
		s.EvePasskeys = []config.EvePasskey{
			{ID: "cred-id-0123456789abcdef", Label: "iPhone", Created: "2026-09-07T10:00:00Z", LastUsed: "2026-09-07T11:00:00Z"},
			{ID: "cred-id-fedcba9876543210", Label: "MacBook", Created: "2026-09-06T10:00:00Z"},
		}
		s.EvePasskeyRevocations = []config.EvePasskeyRevocation{{ID: "cred-id-fedcba9876543210", Requested: "2026-09-07T12:00:00Z"}}
	}), "seed mirror")

	out := lcCapture(t, func() { eveList(store) })

	if !strings.Contains(out, "iPhone") || !strings.Contains(out, "MacBook") {
		t.Fatalf("listing did not show both labels: %q", out)
	}
	if !strings.Contains(out, "revocation pending") {
		t.Fatalf("listing did not mark the pending revocation: %q", out)
	}
	if !strings.Contains(out, "LABEL") || !strings.Contains(out, "CREDENTIAL ID") || !strings.Contains(out, "LAST USED") || !strings.Contains(out, "STATUS") {
		t.Fatalf("listing is missing an expected column header: %q", out)
	}
}

func TestEveList_CLIEmptyMirror(t *testing.T) {
	store, _ := lcNewStore(t)

	out := lcCapture(t, func() { eveList(store) })
	if !strings.Contains(out, "no eve passkeys reported") {
		t.Fatalf("empty-mirror output = %q, want the no-passkeys message", out)
	}
}

// TestEveRevoke_CLIPrintsPendingAndRecordsIt is `eve revoke`'s brokered
// round-trip, mirroring TestEveEnrol_CLIPrintsExpiryAndInstructions.
func TestEveRevoke_CLIPrintsPendingAndRecordsIt(t *testing.T) {
	store, _ := lcNewStore(t)
	assertNoErr(t, store.With(func(s *config.Settings) {
		s.EvePasskeys = []config.EvePasskey{{ID: "p1", Label: "iPhone"}, {ID: "p2", Label: "MacBook"}}
	}), "seed two eve passkeys")
	serveBroker(t, newBrokerRouter(t, store, nil))

	out := lcCapture(t, func() { eveRevoke([]string{"--id", "p1"}) })

	if !strings.Contains(out, "revocation pending") {
		t.Fatalf("output does not say the revocation is pending: %q", out)
	}
	if !strings.Contains(out, "stop working on its next use") {
		t.Fatalf("output does not say what happens next: %q", out)
	}
	if !strings.Contains(out, "signs out every session") {
		t.Fatalf("output does not mention eve signing sessions out: %q", out)
	}

	revs := store.Get().EvePasskeyRevocations
	if len(revs) != 1 || revs[0].ID != "p1" {
		t.Fatalf("no pending revocation was written: %+v", revs)
	}
}
