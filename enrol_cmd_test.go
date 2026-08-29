package main

// create, update and revoke are brokered (ADR-017 decision 2): `enrolUpdate`
// no longer calls updateEnrolment directly, so its flag-parsing is tested on
// its own, against parseEnrolUpdateFlags, with no store and no service dial
// involved at all. The end-to-end path (a real bridge server, a wired
// EnrolmentOps, the actual admin_op round trip) is covered separately below
// and in audit_issuance_test.go.

import (
	"slices"
	"testing"
)

// Creates an enrolment via the same path the tray's EnrolmentOps.Create
// uses, without going through flag parsing or a bridge round trip, so a
// test can set up a starting state concisely.
func enrolCreateForCLITest(t *testing.T, store SettingsStore, clientID string, projectIDs []string) {
	t.Helper()
	_, err := createEnrolment(store, enrolmentRequest{ClientID: clientID, ProjectIDs: projectIDs})
	assertNoErr(t, err, "createEnrolment")
}

// Flag detection is fs.Visit-based, not a zero check, so an unnamed flag must
// leave its field untouched (nil) rather than reset it (a pointer to zero) —
// this is the CLI-level check that the distinction survives flag parsing,
// independent of whatever door eventually applies the resulting request.
func TestParseEnrolUpdateFlags_OnlyTheNamedBudgetFlagIsSet(t *testing.T) {
	req := parseEnrolUpdateFlags([]string{"--client-id", "hermes-mail", "--max-calls", "999"})

	if req.ClientID != "hermes-mail" {
		t.Fatalf("ClientID = %q, want hermes-mail", req.ClientID)
	}
	if req.Budget.MaxCalls == nil || *req.Budget.MaxCalls != 999 {
		t.Fatalf("Budget.MaxCalls = %v, want a pointer to 999", req.Budget.MaxCalls)
	}
	if req.Budget.WindowSeconds != nil || req.Budget.MaxResultBytes != nil {
		t.Fatalf("an unnamed flag's field was set: %+v", req.Budget)
	}
	if req.ProjectIDs != nil {
		t.Fatalf("ProjectIDs = %v, want nil (neither --grant nor --clear-grants was passed)", req.ProjectIDs)
	}
}

func TestParseEnrolUpdateFlags_GrantAndClearGrants(t *testing.T) {
	req := parseEnrolUpdateFlags([]string{"--client-id", "hermes-mail", "--grant", "cal-project"})
	if req.ProjectIDs == nil || !slices.Equal(*req.ProjectIDs, []string{"cal-project"}) {
		t.Fatalf("--grant did not produce a replacement grant list: %v", req.ProjectIDs)
	}

	req = parseEnrolUpdateFlags([]string{"--client-id", "hermes-mail", "--clear-grants"})
	if req.ProjectIDs == nil || len(*req.ProjectIDs) != 0 {
		t.Fatalf("--clear-grants did not produce an empty (non-nil) grant list: %v", req.ProjectIDs)
	}
}

// TestEnrolUpdate_CLIDispatchesTheParsedRequestThroughTheBroker is the
// integration half: a real bridge server, a wired EnrolmentOps, and the
// actual admin_op round trip, proving the flags parsed above reach
// EnrolmentOps.Update rather than merely being well-formed on their own.
func TestEnrolUpdate_CLIDispatchesTheParsedRequestThroughTheBroker(t *testing.T) {
	store := newCLISandboxStore(t)
	profile := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
	enrolCreateForCLITest(t, store, "hermes-mail", []string{profile.ID})
	serveBroker(t, newBrokerRouter(t, store, nil))

	enrolUpdate(store, []string{"--client-id", "hermes-mail", "--max-calls", "999"})

	got := store.Get().FindEnrolment("hermes-mail").Budget
	if got.MaxCalls != 999 {
		t.Fatalf("MaxCalls = %d, want 999", got.MaxCalls)
	}
}
