package main

// CLI-level coverage for `relay enrol update`, mirroring service_cmd_test.go's
// approach: drive the CLI entry point directly against a sandboxed store (no
// subprocess, no touching the real config dir) and read the persisted result
// back. The happy paths only — exitError calls os.Exit(1), which would kill
// the test binary, so refusal paths are covered at the updateEnrolment level
// in enrolment_test.go instead.

import (
	"slices"
	"testing"
)

// enrolCreateForCLITest creates an enrolment via the same path enrolCreate
// uses, without going through flag parsing, so a test can set up a starting
// state concisely.
func enrolCreateForCLITest(t *testing.T, store SettingsStore, clientID string, projectIDs []string) {
	t.Helper()
	_, err := createEnrolment(store, enrolmentRequest{ClientID: clientID, ProjectIDs: projectIDs})
	assertNoErr(t, err, "createEnrolment")
}

// `relay enrol update --client-id X --max-calls N` changes only MaxCalls,
// leaving WindowSeconds and MaxResultBytes exactly as they were created —
// this is the CLI-level check that fs.Visit-based flag detection (not a zero
// check) actually reaches updateEnrolment correctly.
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

// A second update naming a different flag leaves the first update's change in
// place — each update touches only what it names, not a snapshot of "what was
// last typed".
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

// --grant replaces the whole grant list, and --clear-grants empties it —
// both without touching the fingerprint.
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
