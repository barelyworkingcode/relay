package main

// The CLI half of ADR-019's operator surface: the SAS column on `relay enrol
// requests`, and `relay enrol approve`'s refusal to issue a certificate that
// reaches nothing without being told to.

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// §4.7 — the SAS column
// ---------------------------------------------------------------------------

// The three non-code states are words, not a blank cell. A blank cell reads
// as "nothing to compare" for all three, and only one of them means that.
func TestEnrolRequestSASColumn_TellsTheThreeStatesApart(t *testing.T) {
	cases := []struct {
		name string
		item enrolmentRequestListItem
		want string
	}{
		{"legacy carried-pin row", enrolmentRequestListItem{IsLegacyRequest: true}, "-"},
		{"lodged, not yet opened", enrolmentRequestListItem{}, "(waiting)"},
		{"opened wrongly", enrolmentRequestListItem{SASFailed: true}, "FAILED"},
		{"ready", enrolmentRequestListItem{SASReady: true, SAS: "7K3P4Q"}, "7K3P4Q"},
		// Belt and braces: a row that says ready but carries no code is not
		// a code, and rendering an empty cell for it would read as legacy.
		{"ready but codeless", enrolmentRequestListItem{SASReady: true}, "(waiting)"},
		// A failed row that somehow also carries a code is still refused —
		// the failure is the answer, not the code.
		{"failed with a stale code", enrolmentRequestListItem{SASFailed: true, SASReady: true, SAS: "7K3P4Q"}, "FAILED"},
	}
	for _, c := range cases {
		if got := enrolRequestSASColumn(c.item); got != c.want {
			t.Errorf("%s: SAS column = %q, want %q", c.name, got, c.want)
		}
	}
}

// End to end through the broker: a register row that has completed its
// comparison shows its code, a legacy `relayremote request` row shows "-",
// and the header names the column.
func TestEnrolRequests_RendersTheSASColumn(t *testing.T) {
	store := newCLISandboxStore(t)
	table := newEnrolmentRequestTable()
	seedCAInto(t, table)

	legacy, err := table.Lodge(genClientCSRPEM(t, "carried"), "vm-carried", "", "", "10.0.0.5:1")
	assertNoErr(t, err, "legacy Lodge")

	waiting := newSASClient(t, "waiting")
	wl, err := table.Lodge(waiting.csrPEM, "vm-waiting", "", waiting.commit, "10.0.0.6:1")
	assertNoErr(t, err, "waiting Lodge")

	ready := newSASClient(t, "ready")
	rl, err := table.Lodge(ready.csrPEM, "vm-ready", "", ready.commit, "10.0.0.7:1")
	assertNoErr(t, err, "ready Lodge")
	if _, err := table.Poll(rl.RequestID, ready.open()); err != nil {
		t.Fatalf("Poll with a good open: %v", err)
	}
	code := viewFor(t, table, rl.RequestID).SAS
	if code == "" {
		t.Fatal("fixture did not produce a comparison code")
	}

	serveBroker(t, newBrokerRouter(t, store, func(r *appRouter) {
		r.enrolmentOps.Requests = table
	}))

	out := captureStdout(t, func() { enrolRequests([]string{}) })

	if !strings.Contains(out, "SAS") {
		t.Fatalf("the requests table has no SAS column:\n%s", out)
	}
	for _, want := range []string{
		legacy.RequestID + "  -",
		wl.RequestID + "  (waiting)",
		rl.RequestID + "  " + code,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("requests output is missing %q:\n%s", want, out)
		}
	}
}

// ---------------------------------------------------------------------------
// AC-46 (CLI half) — a grant is required, and --no-grant is the way to say no
// ---------------------------------------------------------------------------

// The refusal is loud and the fix is one flag. This runs in a subprocess
// because exitError calls os.Exit(1), and what is being checked is what a
// real process does: a non-zero exit, and a message naming the escape hatch
// rather than a flag reference dump.
func TestEnrolApprove_RefusesWithNoGrantAndNamesTheFlag(t *testing.T) {
	dir := mkShortTempDir(t, "relay-approve-nogrant-")
	out, code := runCLISubprocess(t, dir, "enrol", "approve", "--id", "req_x", "--client-id", "hermes-mail")

	if code == 0 {
		t.Fatalf("approving with no --grant exited 0:\n%s", out)
	}
	for _, want := range []string{"--grant is required", "--no-grant"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not contain %q:\n%s", want, out)
		}
	}
	// It refuses on its own argv, before anything is dialled: the operator
	// gets the flag error, not "requires the service".
	if strings.Contains(out, "requires the service") {
		t.Errorf("the missing-grant refusal was masked by the broker refusal:\n%s", out)
	}
}

// --grant and --no-grant together is a contradiction, not a precedence rule:
// whichever one won silently would be the wrong one half the time.
func TestEnrolApprove_RefusesGrantAndNoGrantTogether(t *testing.T) {
	dir := mkShortTempDir(t, "relay-approve-both-")
	out, code := runCLISubprocess(t, dir, "enrol", "approve",
		"--id", "req_x", "--client-id", "hermes-mail", "--grant", "proj_mail", "--no-grant")

	if code == 0 {
		t.Fatalf("--grant with --no-grant exited 0:\n%s", out)
	}
	if !strings.Contains(out, "mutually exclusive") {
		t.Errorf("the refusal does not say the two flags are mutually exclusive:\n%s", out)
	}
}

// --no-grant is a real answer, not a bypass that smuggles a grant in: it
// produces the same empty ProjectIDs an unflagged approve used to produce
// silently, with the operator having said so.
func TestParseEnrolApproveFlags_NoGrantYieldsAnEmptyGrantList(t *testing.T) {
	got := parseEnrolApproveFlags([]string{"--id", "req_123", "--client-id", "hermes-mail", "--no-grant"})
	if got.RequestID != "req_123" || got.ClientID != "hermes-mail" {
		t.Fatalf("got = %+v", got)
	}
	if len(got.ProjectIDs) != 0 {
		t.Fatalf("ProjectIDs = %v, want empty", got.ProjectIDs)
	}
}
