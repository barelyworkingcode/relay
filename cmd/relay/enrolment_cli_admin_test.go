package main

// Coverage for ADR-018 step 2 (SPEC-step2-cli-admin.md §1, §2): the
// cli_admin bit on an enrolment record and the presence-gated
// `relay enrol update --cli-admin` toggle that flips it. Acceptance
// criteria references are the spec's own numbering (§8).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/presence/presencetest"
)

// cliAdminColSplit splits a newTabWriter-rendered row on its column
// padding (at least 2 spaces), leaving single internal spaces like the one
// in "CLIENT ID" alone.
var cliAdminColSplit = regexp.MustCompile(`  +`)

// AC-1: a settings.json fixture whose enrolments[0] has no cli_admin key
// loads with CLIAdmin == false, and re-saving produces a file with no
// cli_admin key at all — the same round-trip guarantee every other
// omitempty field on Enrolment carries.
func TestEnrolment_CLIAdminAbsentMeansOffAndRoundTripsAbsent(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	body := `{"version":1,"external_mcps":[],"services":[],"projects":[],"enrolments":[` +
		`{"client_id":"hermes-mail","fingerprint":"sha256:` + strings.Repeat("a", 64) + `",` +
		`"project_ids":[],"budget":{"window_seconds":60,"max_calls":10,"max_result_bytes":1000},` +
		`"created_at":"2024-01-01T00:00:00Z"}]}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("seed settings.json: %v", err)
	}
	store := sealedSettingsStoreAt(dir)

	e := store.Get().FindEnrolment("hermes-mail")
	if e == nil {
		t.Fatal("fixture enrolment did not load")
	}
	if e.CLIAdmin {
		t.Fatal("CLIAdmin = true for a record with no cli_admin key, want false")
	}

	// Force a rewrite (a no-op mutation still rewrites the whole file) and
	// check the key never got invented.
	assertNoErr(t, store.With(func(s *Settings) {}), "With")
	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	assertNoErr(t, err, "read settings.json after rewrite")
	if strings.Contains(string(raw), "cli_admin") {
		t.Fatalf("a rewrite invented a cli_admin key for a record that never had one:\n%s", raw)
	}
}

// AC-2: cloneEnrolment (via Settings.Clone) carries the bit, and mutating
// the clone does not affect the original — cloneEnrolment itself needs no
// change (a bool rides the value copy), so this pins that it keeps working
// rather than re-testing cloneEnrolment's implementation.
func TestSettingsClone_CarriesCLIAdminAndIsolatesTheCopy(t *testing.T) {
	s := &Settings{
		Enrolments: []Enrolment{{ClientID: "hermes-mail", CLIAdmin: true}},
	}
	clone := s.Clone()
	if len(clone.Enrolments) != 1 || !clone.Enrolments[0].CLIAdmin {
		t.Fatalf("clone.Enrolments[0].CLIAdmin = %+v, want CLIAdmin true", clone.Enrolments)
	}
	clone.Enrolments[0].CLIAdmin = false
	if !s.Enrolments[0].CLIAdmin {
		t.Fatal("mutating the clone's CLIAdmin field changed the original")
	}
}

// AC-3: the presence digest binds cli_admin. A grant minted with cli_admin
// absent must not redeem for a request that sets it; a grant for
// cli_admin=true must not redeem for cli_admin=false. A positive control
// (same digest twice) proves the harness itself is capable of succeeding.
func TestEnrolmentUpdateRequest_DigestBindsCLIAdmin(t *testing.T) {
	gate := allowGate(t)

	absent := enrolmentUpdateRequest{ClientID: "hermes-mail"}
	on := true
	setTrue := enrolmentUpdateRequest{ClientID: "hermes-mail", CLIAdmin: &on}
	off := false
	setFalse := enrolmentUpdateRequest{ClientID: "hermes-mail", CLIAdmin: &off}

	// Positive control: redeeming against the identical digest succeeds.
	grant, err := gate.Request(context.Background(), "enrolment.update", absent.presenceDigest(), "update")
	assertNoErr(t, err, "Request")
	if err := gate.Redeem(grant, "enrolment.update", absent.presenceDigest()); err != nil {
		t.Fatalf("redeeming against the SAME digest failed: %v", err)
	}

	// absent -> setTrue must not redeem.
	grant, err = gate.Request(context.Background(), "enrolment.update", absent.presenceDigest(), "update")
	assertNoErr(t, err, "Request")
	if err := gate.Redeem(grant, "enrolment.update", setTrue.presenceDigest()); !errors.Is(err, presence.ErrGrantInvalid) {
		t.Fatalf("a grant minted with cli_admin absent redeemed for a request setting it: err = %v, want ErrGrantInvalid", err)
	}

	// setTrue -> setFalse must not redeem.
	grant, err = gate.Request(context.Background(), "enrolment.update", setTrue.presenceDigest(), "update")
	assertNoErr(t, err, "Request")
	if err := gate.Redeem(grant, "enrolment.update", setFalse.presenceDigest()); !errors.Is(err, presence.ErrGrantInvalid) {
		t.Fatalf("a grant minted for cli_admin=true redeemed for cli_admin=false: err = %v, want ErrGrantInvalid", err)
	}
}

// AC-4: toggling calls the presence provider exactly once and the reason
// names cli-admin and the client id. Turning it off also prompts.
func TestEnrolmentOpsUpdate_CLIAdminPromptsBothDirections(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	mail := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
	_, err := createEnrolment(store, enrolmentRequest{ClientID: "hermes-mail", ProjectIDs: []string{mail.ID}})
	assertNoErr(t, err, "createEnrolment")

	recording := presencetest.NewRecording(nil)
	gate, err := presence.NewGate(recording)
	assertNoErr(t, err, "NewGate")
	ops := &EnrolmentOps{Store: store, Gate: gate, Issuance: pgwWithIssuance(t)}

	on := true
	_, _, err = ops.Update(context.Background(), enrolmentUpdateRequest{ClientID: "hermes-mail", CLIAdmin: &on}, auditViaCLI, "")
	assertNoErr(t, err, "Update turning cli-admin on")
	if n := recording.Calls(); n != 1 {
		t.Fatalf("Calls() after turning on = %d, want 1", n)
	}
	if reason := recording.Reasons()[0]; !strings.Contains(reason, "cli-admin") || !strings.Contains(reason, "hermes-mail") {
		t.Fatalf("reason[0] = %q, want it to name cli-admin and hermes-mail", reason)
	}

	off := false
	_, _, err = ops.Update(context.Background(), enrolmentUpdateRequest{ClientID: "hermes-mail", CLIAdmin: &off}, auditViaCLI, "")
	assertNoErr(t, err, "Update turning cli-admin off")
	if n := recording.Calls(); n != 2 {
		t.Fatalf("Calls() after turning off = %d, want 2 (turning off must prompt too)", n)
	}
	if reason := recording.Reasons()[1]; !strings.Contains(reason, "cli-admin") || !strings.Contains(reason, "hermes-mail") {
		t.Fatalf("reason[1] = %q, want it to name cli-admin and hermes-mail", reason)
	}
}

// AC-5: toggling with a nil gate refuses before touching the store.
func TestEnrolmentOpsUpdate_CLIAdminNilGateRefusesBeforeTouchingStore(t *testing.T) {
	dir, store := newEnrolmentSandbox(t)
	mail := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
	_, err := createEnrolment(store, enrolmentRequest{ClientID: "hermes-mail", ProjectIDs: []string{mail.ID}})
	assertNoErr(t, err, "createEnrolment")

	before := odwSnap(t, dir)
	ops := &EnrolmentOps{Store: store, Gate: nil, Issuance: pgwWithIssuance(t)}
	on := true
	_, _, err = ops.Update(context.Background(), enrolmentUpdateRequest{ClientID: "hermes-mail", CLIAdmin: &on}, auditViaCLI, "")
	if !errors.Is(err, errPresenceGateNotWired) {
		t.Fatalf("Update with a nil gate: err = %v, want errPresenceGateNotWired", err)
	}
	before.assertUntouched(t, dir, "cli-admin toggle with a nil gate")
}

// AC-6: toggling with issuance auditing unavailable refuses before the
// provider is ever called.
func TestEnrolmentOpsUpdate_CLIAdminAuditingOffRefusesBeforeProvider(t *testing.T) {
	dir, store := newEnrolmentSandbox(t)
	mail := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
	_, err := createEnrolment(store, enrolmentRequest{ClientID: "hermes-mail", ProjectIDs: []string{mail.ID}})
	assertNoErr(t, err, "createEnrolment")

	before := odwSnap(t, dir)
	recording := presencetest.NewRecording(nil)
	gate, err := presence.NewGate(recording)
	assertNoErr(t, err, "NewGate")
	ops := &EnrolmentOps{Store: store, Gate: gate, Issuance: nil}
	on := true
	_, _, err = ops.Update(context.Background(), enrolmentUpdateRequest{ClientID: "hermes-mail", CLIAdmin: &on}, auditViaCLI, "")
	if !errors.Is(err, errIssuanceAuditingRequired) {
		t.Fatalf("Update with auditing off: err = %v, want errIssuanceAuditingRequired", err)
	}
	if n := recording.Calls(); n != 0 {
		t.Fatalf("presence provider called %d time(s) with auditing off; must refuse before prompting", n)
	}
	before.assertUntouched(t, dir, "cli-admin toggle with auditing off")
}

// AC-7: a cli-admin-only update does not re-validate grants. An enrolment
// whose ProjectIDs names a since-deleted profile can still have cli_admin
// toggled, mirroring TestUpdateEnrolment_BudgetOnlyUpdateSurvivesDanglingGrant.
func TestUpdateEnrolment_CLIAdminOnlyUpdateSurvivesDanglingGrant(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	mail := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
	_, err := createEnrolment(store, enrolmentRequest{ClientID: "hermes-mail", ProjectIDs: []string{mail.ID}})
	assertNoErr(t, err, "createEnrolment")

	assertNoErr(t, store.With(func(s *Settings) { s.RemoveProject(mail.ID) }), "delete the granted profile out from under the enrolment")

	on := true
	_, after, err := updateEnrolment(store, enrolmentUpdateRequest{ClientID: "hermes-mail", CLIAdmin: &on})
	assertNoErr(t, err, "a cli-admin-only update must not re-validate untouched grants")
	if !after.CLIAdmin {
		t.Fatal("cli_admin was not applied")
	}
	if len(after.ProjectIDs) != 1 || after.ProjectIDs[0] != mail.ID {
		t.Fatalf("grants changed on a cli-admin-only update: %v", after.ProjectIDs)
	}
}

// AC-21: `relay enrol update --cli-admin` produces a config_change record
// with credential == "enrolment", subject = client id, grants containing
// cli_admin=on, and a non-empty presence_id. Off produces cli_admin=off.
// Drives the real CLI door through the broker, the way
// TestEnrolUpdate_CLIDispatchesTheParsedRequestThroughTheBroker does.
func TestEnrolUpdate_CLIAdminToggleIsAudited(t *testing.T) {
	store := newCLISandboxStore(t)
	mail := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
	enrolCreateForCLITest(t, store, "hermes-mail", []string{mail.ID})
	serveBroker(t, newBrokerRouter(t, store, nil))

	enrolUpdate(store, []string{"--client-id", "hermes-mail", "--cli-admin"})

	events := aiParse(t, aiLogText(t))
	var onRecords []AuditEvent
	for _, ev := range events {
		if ev.Event == AuditEventConfigChange && ev.Subject == "hermes-mail" {
			onRecords = append(onRecords, ev)
		}
	}
	if len(onRecords) != 1 {
		t.Fatalf("want exactly one config_change record for hermes-mail after turning cli-admin on, got %d: %s", len(onRecords), aiSummarise(events))
	}
	on := onRecords[0]
	if on.Credential != auditCredentialEnrolment {
		t.Errorf("credential = %q, want %q", on.Credential, auditCredentialEnrolment)
	}
	if !slices.Contains(on.Grants, "cli_admin=on") {
		t.Errorf("grants = %v, want to contain cli_admin=on", on.Grants)
	}
	if on.PresenceID == "" {
		t.Error("presence_id is empty")
	}

	enrolUpdate(store, []string{"--client-id", "hermes-mail", "--cli-admin=false"})
	events = aiParse(t, aiLogText(t))
	var offRecords []AuditEvent
	for _, ev := range events {
		if ev.Event == AuditEventConfigChange && ev.Subject == "hermes-mail" && slices.Contains(ev.Grants, "cli_admin=off") {
			offRecords = append(offRecords, ev)
		}
	}
	if len(offRecords) != 1 {
		t.Fatalf("want exactly one config_change record for cli_admin=off, got %d: %s", len(offRecords), aiSummarise(events))
	}
}

// AC-23: GET /api/enrolments returns cli_admin: true for a toggled record
// and omits the key otherwise.
func TestEnrolmentRoutes_ListShowsCLIAdminAndOmitsWhenOff(t *testing.T) {
	store := newCLISandboxStore(t)
	mail := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
	enrolCreateForCLITest(t, store, "hermes-mail", []string{mail.ID})
	enrolCreateForCLITest(t, store, "hermes-off", []string{mail.ID})

	on := true
	_, _, err := updateEnrolment(store, enrolmentUpdateRequest{ClientID: "hermes-mail", CLIAdmin: &on})
	assertNoErr(t, err, "updateEnrolment turning cli-admin on")

	ops := &EnrolmentOps{Store: store, Gate: allowGate(t), Audit: enabledIssuanceRecorder(t)}
	mux := http.NewServeMux()
	RegisterEnrolmentRoutes(&control.RouteRegistrar{CredentialID: APICredentialIDFromContext, Mux: mux, Transport: control.TransportSocket}, ops)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, body := doJSON(t, "GET", srv.URL+"/api/enrolments", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("list: status %d, body %s", resp.StatusCode, body)
	}
	var listed []enrolmentView
	assertNoErr(t, json.Unmarshal(body, &listed), "decode list")

	var on_, off_ *enrolmentView
	for i := range listed {
		switch listed[i].ClientID {
		case "hermes-mail":
			on_ = &listed[i]
		case "hermes-off":
			off_ = &listed[i]
		}
	}
	if on_ == nil || off_ == nil {
		t.Fatalf("expected both enrolments in the list, got %+v", listed)
	}
	if !on_.CLIAdmin {
		t.Error("hermes-mail: CLIAdmin = false, want true")
	}
	if off_.CLIAdmin {
		t.Error("hermes-off: CLIAdmin = true, want false")
	}
	// The bit-off record must not even carry the key.
	offIdx := strings.Index(string(body), `"hermes-off"`)
	if offIdx < 0 {
		t.Fatal("hermes-off not found in raw response body")
	}
	// Find the object bounds around hermes-off's client_id by locating the
	// next client_id after it (or end of array) and checking no cli_admin
	// key appears inside that slice.
	rest := string(body)[offIdx:]
	if strings.Contains(rest[:min(len(rest), 400)], "cli_admin") {
		t.Errorf("hermes-off's JSON object carries a cli_admin key though the bit is off:\n%s", rest[:min(len(rest), 400)])
	}
}

// The enrol list column: relay enrol list prints a CLI-ADMIN column between
// PROFILES and CALLS/WINDOW, "on" when set and "-" when not.
func TestEnrolList_PrintsCLIAdminColumn(t *testing.T) {
	dir, store := newEnrolmentSandbox(t)
	mail := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
	_, err := createEnrolment(store, enrolmentRequest{ClientID: "hermes-on", ProjectIDs: []string{mail.ID}})
	assertNoErr(t, err, "createEnrolment on")
	_, err = createEnrolment(store, enrolmentRequest{ClientID: "hermes-off", ProjectIDs: []string{mail.ID}})
	assertNoErr(t, err, "createEnrolment off")

	on := true
	_, _, err = updateEnrolment(store, enrolmentUpdateRequest{ClientID: "hermes-on", CLIAdmin: &on})
	assertNoErr(t, err, "updateEnrolment")

	// Reload the store the way `relay enrol list` does: a fresh
	// FileSettingsStore over the same directory, so this exercises the
	// same read path the CLI process uses.
	listStore := sealedSettingsStoreAt(dir)

	out := aiQuiet(t, func() { enrolList(listStore) })
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected a header and two rows, got:\n%s", out)
	}
	// tabwriter renders aligned columns as run-padded spaces, not literal
	// tabs, once Flush has written them out — split on runs of 2+ spaces
	// (newTabWriter's own padding width) rather than "\t" or a single
	// space, since "CLIENT ID" itself holds one internal space.
	splitCols := func(line string) []string { return cliAdminColSplit.Split(strings.TrimRight(line, " "), -1) }
	header := splitCols(lines[0])
	col := -1
	for i, h := range header {
		if strings.TrimSpace(h) == "CLI-ADMIN" {
			col = i
		}
	}
	if col < 0 {
		t.Fatalf("no CLI-ADMIN column in header: %q", lines[0])
	}
	// PROFILES must precede CLI-ADMIN and CALLS/WINDOW must follow it.
	var profilesCol, callsCol int = -1, -1
	for i, h := range header {
		switch strings.TrimSpace(h) {
		case "PROFILES":
			profilesCol = i
		case "CALLS/WINDOW":
			callsCol = i
		}
	}
	if !(profilesCol < col && col < callsCol) {
		t.Fatalf("CLI-ADMIN is not between PROFILES and CALLS/WINDOW: header = %q", lines[0])
	}

	foundOn, foundOff := false, false
	for _, line := range lines[1:] {
		fields := splitCols(line)
		if len(fields) <= col {
			t.Fatalf("row has fewer fields than the header: %q", line)
		}
		switch fields[0] {
		case "hermes-on":
			if fields[col] != "on" {
				t.Errorf("hermes-on CLI-ADMIN column = %q, want %q", fields[col], "on")
			}
			foundOn = true
		case "hermes-off":
			if fields[col] != "-" {
				t.Errorf("hermes-off CLI-ADMIN column = %q, want %q", fields[col], "-")
			}
			foundOff = true
		}
	}
	if !foundOn || !foundOff {
		t.Fatalf("did not find both rows: %s", out)
	}
}
