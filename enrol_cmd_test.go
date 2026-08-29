package main

// Happy paths only: exitError calls os.Exit(1), which would kill the test
// binary, so refusal paths are covered at the updateEnrolment level in
// enrolment_test.go instead.

import (
	"slices"
	"testing"
)

// Creates an enrolment via the same path enrolCreate uses, without going
// through flag parsing, so a test can set up a starting state concisely.
func enrolCreateForCLITest(t *testing.T, store SettingsStore, clientID string, projectIDs []string) {
	t.Helper()
	_, err := createEnrolment(store, enrolmentRequest{ClientID: clientID, ProjectIDs: projectIDs})
	assertNoErr(t, err, "createEnrolment")
}

// Flag detection is fs.Visit-based, not a zero check, so an unnamed flag must
// leave its field untouched rather than reset it — this is the CLI-level
// check that the distinction actually reaches updateEnrolment.
func TestEnrolUpdate_CLIChangesOnlyTheNamedBudgetFlag(t *testing.T) {
	store := newCLISandboxStore(t)
	enrolCreateForCLITest(t, store, "hermes-mail", nil)

	before := store.Get().FindEnrolment("hermes-mail").Budget

	enrolUpdate(store, []string{"--client-id", "hermes-mail", "--max-calls", "999"})

	after := store.Get().FindEnrolment("hermes-mail").Budget
	if after.MaxCalls != 999 {
		t.Fatalf("MaxCalls = %d, want 999", after.MaxCalls)
	}
	if after.WindowSeconds != before.WindowSeconds || after.MaxResultBytes != before.MaxResultBytes {
		t.Fatalf("an unnamed flag's field moved: before=%+v after=%+v", before, after)
	}
}

func TestEnrolUpdate_CLISuccessiveUpdatesAreIndependent(t *testing.T) {
	store := newCLISandboxStore(t)
	enrolCreateForCLITest(t, store, "hermes-mail", nil)

	enrolUpdate(store, []string{"--client-id", "hermes-mail", "--max-calls", "999"})
	enrolUpdate(store, []string{"--client-id", "hermes-mail", "--window-seconds", "120"})

	got := store.Get().FindEnrolment("hermes-mail").Budget
	if got.MaxCalls != 999 {
		t.Fatalf("an earlier update's change was lost: %+v", got)
	}
	if got.WindowSeconds != 120 {
		t.Fatalf("the later update's own field did not apply: %+v", got)
	}
}

func TestEnrolUpdate_CLIGrantAndClearGrants(t *testing.T) {
	store := newCLISandboxStore(t)
	mail := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
	cal := mkStoreProject(t, store, ProjectKindRemote, "Calendar", "")
	enrolCreateForCLITest(t, store, "hermes-mail", []string{mail.ID})

	fingerprintBefore := store.Get().FindEnrolment("hermes-mail").Fingerprint

	enrolUpdate(store, []string{"--client-id", "hermes-mail", "--grant", cal.ID})
	got := store.Get().FindEnrolment("hermes-mail")
	if !slices.Equal(got.ProjectIDs, []string{cal.ID}) {
		t.Fatalf("--grant did not replace the grant list: %v", got.ProjectIDs)
	}

	enrolUpdate(store, []string{"--client-id", "hermes-mail", "--clear-grants"})
	got = store.Get().FindEnrolment("hermes-mail")
	if len(got.ProjectIDs) != 0 {
		t.Fatalf("--clear-grants did not empty the grant list: %v", got.ProjectIDs)
	}

	if got.Fingerprint != fingerprintBefore {
		t.Fatalf("the fingerprint moved across grant updates: before=%q after=%q", fingerprintBefore, got.Fingerprint)
	}
}
