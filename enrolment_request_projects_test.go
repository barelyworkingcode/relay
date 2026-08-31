package main

// The approved poll payload names each granted access profile (spec §5.7),
// and no other poll answer does (ADR-018 §8 P2's narrowing). Both halves are
// asserted against the MARSHALLED JSON the listener's own handler produces,
// not against the Go struct: a name that leaked through a shared type would
// pass a struct-level check and still be on the wire.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"relaygo/bridge"
	"relaygo/jsonrpc"
)

// pollWireJSON runs the exact handler enrolmentRequestHandlers dispatches a
// poll frame to, and returns the Result bytes that would reach the client.
func pollWireJSON(t *testing.T, table *enrolmentRequestTable, requestID string) []byte {
	t.Helper()
	resp := handleEnrolmentPoll(table, []byte(pollJSON(requestID)), "10.0.0.5:41233")
	if resp.Type != bridge.RespResult {
		t.Fatalf("poll for %s = %s %q, want a result", requestID, resp.Type, resp.Message)
	}
	return resp.Result
}

// pollWireKeys decodes a poll result into a bare map, so a test can ask
// whether a KEY is present rather than whether a typed field is zero.
func pollWireKeys(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode poll result %s: %v", data, err)
	}
	return out
}

// ---------------------------------------------------------------------------
// A name reaches the approved payload
// ---------------------------------------------------------------------------

// The two halves of each pair are pinned to the two operator surfaces that
// print them: `relay enrol list` renders the granted ids (formatGrants), and
// `relay grant` renders the display name (newGrantView). The client's closing
// report joins them -- "Hermes Mail Inbox  (proj_mail)" -- so an operator on
// the Mac has something to match against.
func TestEnrolmentPoll_ApprovedPayloadNamesEachGrantedProfile(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	mail := mkStoreProject(t, store, ProjectKindRemote, "Hermes Mail Inbox", "")
	cal := mkStoreProject(t, store, ProjectKindRemote, "Hermes Calendar", "")
	table, l := aoLodge(t, "hermes-mail", "10.0.0.5:41233")

	ops := &EnrolmentOps{Store: store, Gate: allowGate(t), Issuance: pgwWithIssuance(t), Requests: table}
	_, err := ops.Approve(context.Background(), approveFields{
		RequestID:  l.RequestID,
		ClientID:   "hermes-mail",
		ProjectIDs: []string{mail.ID, cal.ID},
	}, auditViaCLI, "")
	assertNoErr(t, err, "Approve")

	var approved enrolmentRequestPollResult
	data := pollWireJSON(t, table, l.RequestID)
	assertNoErr(t, json.Unmarshal(data, &approved), "decode the approved poll result")

	if approved.Status != "approved" {
		t.Fatalf("status = %q, want approved", approved.Status)
	}
	// project_ids is untouched: a client reading ids today keeps working.
	wantIDs := formatGrants([]string{mail.ID, cal.ID})
	if got := formatGrants(approved.ProjectIDs); got != wantIDs {
		t.Fatalf("project_ids = %q, want %q (what `relay enrol list` shows for this enrolment)", got, wantIDs)
	}
	if len(approved.Projects) != 2 {
		t.Fatalf("projects = %+v, want one entry per granted id", approved.Projects)
	}
	s := store.Get()
	for i, want := range []Project{mail, cal} {
		got := approved.Projects[i]
		if got.ID != want.ID {
			t.Fatalf("projects[%d].id = %q, want %q", i, got.ID, want.ID)
		}
		// newGrantView is what `relay grant` prints; the payload must not
		// have its own idea of the profile's name.
		if operatorName := newGrantView(s, want).Name; got.Name != operatorName {
			t.Fatalf("projects[%d].name = %q, want %q -- the name `relay grant` shows for %s",
				i, got.Name, operatorName, want.ID)
		}
	}
	if !strings.Contains(string(data), `"name":"Hermes Mail Inbox"`) {
		t.Fatalf("the approved payload does not carry the profile's display name: %s", data)
	}
}

// ---------------------------------------------------------------------------
// ADR-018 §8 P2: only the approved payload carries a name
// ---------------------------------------------------------------------------

// One table, four answers: approved, pending, refused, unknown. The profile
// name must appear in the first and in none of the other three -- and the
// `projects` key must be absent from their JSON entirely, which is the check
// a struct-level assertion cannot make.
func TestEnrolmentPoll_PendingRefusedAndUnknownCarryNoProfileName(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	const profileName = "Hermes Mail Inbox"
	profile := mkStoreProject(t, store, ProjectKindRemote, profileName, "")

	table := newEnrolmentRequestTable()
	approvedRow, err := table.Lodge(genClientCSRPEM(t, "hermes-approved"), "", "", "", "10.0.0.5:1")
	assertNoErr(t, err, "lodge the row to approve")
	pendingRow, err := table.Lodge(genClientCSRPEM(t, "hermes-pending"), "", "", "", "10.0.0.6:1")
	assertNoErr(t, err, "lodge the pending row")
	refusedRow, err := table.Lodge(genClientCSRPEM(t, "hermes-refused"), "", "", "", "10.0.0.7:1")
	assertNoErr(t, err, "lodge the row to refuse")

	ops := &EnrolmentOps{Store: store, Gate: allowGate(t), Issuance: pgwWithIssuance(t), Requests: table}
	_, err = ops.Approve(context.Background(), approveFields{
		RequestID:  approvedRow.RequestID,
		ClientID:   "hermes-mail",
		ProjectIDs: []string{profile.ID},
	}, auditViaCLI, "")
	assertNoErr(t, err, "Approve")
	if !table.Refuse(nil, refusedRow.RequestID) {
		t.Fatal("Refuse reported the record was not found")
	}

	approved := pollWireJSON(t, table, approvedRow.RequestID)
	if _, ok := pollWireKeys(t, approved)["projects"]; !ok {
		t.Fatalf("the approved payload has no projects key: %s", approved)
	}

	for _, answer := range []struct {
		status    string
		requestID string
	}{
		{"pending", pendingRow.RequestID},
		{"refused", refusedRow.RequestID},
		{"unknown", "req_never_existed"},
	} {
		data := pollWireJSON(t, table, answer.requestID)
		keys := pollWireKeys(t, data)
		if got, _ := keys["status"].(string); got != answer.status {
			t.Fatalf("status for %s = %q, want %q", answer.requestID, got, answer.status)
		}
		if _, ok := keys["projects"]; ok {
			t.Fatalf("a %q poll carries a projects key -- ADR-018 §8 P2's narrowing holds only because "+
				"an unauthenticated peer reaching this answer learns no host configuration: %s", answer.status, data)
		}
		if strings.Contains(string(data), profileName) {
			t.Fatalf("a %q poll carries the profile's display name: %s", answer.status, data)
		}
	}
}

// ---------------------------------------------------------------------------
// A profile with no usable name degrades to its id, never to ""
// ---------------------------------------------------------------------------

// The client prints "name  (id)". An empty name renders as a gap and reads as
// a broken relay, so both the unnamed and the vanished profile fall back to
// the id the client already holds.
func TestEnrolmentApprovedProjects_UnnamedOrMissingProfileDegradesToTheID(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	named := mkStoreProject(t, store, ProjectKindRemote, "Hermes Mail Inbox", "")
	blank := mkStoreProject(t, store, ProjectKindRemote, "To Be Blanked", "")
	assertNoErr(t, store.With(func(s *Settings) {
		s.UpdateProjectName(blank.ID, "   ")
	}), "blank the profile's name")

	ops := &EnrolmentOps{Store: store, Gate: allowGate(t), Issuance: pgwWithIssuance(t), Requests: newEnrolmentRequestTable()}
	got := ops.approvedProjects([]string{named.ID, blank.ID, "proj_never_existed"})

	want := []approvedProject{
		{ID: named.ID, Name: "Hermes Mail Inbox"},
		{ID: blank.ID, Name: blank.ID},
		{ID: "proj_never_existed", Name: "proj_never_existed"},
	}
	if len(got) != len(want) {
		t.Fatalf("approvedProjects = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("approvedProjects[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// The per-source throttle is a throttle, not a malformed request
// ---------------------------------------------------------------------------

// It used to fall through to CodeInvalidParams, so the operator read
// "malformed" for a well-formed frame and started checking their command
// line -- and the client had to string-match the message to tell the two
// apart. The message text is deliberately unchanged; the code is what moved.
func TestEnrolmentLodge_PerSourceThrottleAnswersThrottledNotInvalidParams(t *testing.T) {
	table := newEnrolmentRequestTable()
	now := time.Now()
	table.setClock(func() time.Time { return now })

	first := handleEnrolmentLodge(table, []byte(lodgeJSON(genClientCSRPEM(t, "throttle-a"), "vm-a")), "10.0.0.9:41233")
	if first.Type != bridge.RespResult {
		t.Fatalf("first lodge = %s %q, want a result", first.Type, first.Message)
	}

	// A DIFFERENT key from the same host inside the window: not the
	// idempotent re-lodge path, so the per-source window is what refuses it.
	second := handleEnrolmentLodge(table, []byte(lodgeJSON(genClientCSRPEM(t, "throttle-b"), "vm-b")), "10.0.0.9:41234")
	if second.Type != bridge.RespError {
		t.Fatalf("second lodge = %s, want an error", second.Type)
	}
	if second.Code != codeEnrolmentThrottled {
		t.Fatalf("per-source throttle code = %d, want %d (codeEnrolmentThrottled); %d is CodeInvalidParams and "+
			"tells the operator a well-formed request was malformed", second.Code, codeEnrolmentThrottled, jsonrpc.CodeInvalidParams)
	}
	if want := "too many enrolment requests from 10.0.0.9; wait a moment and try again"; second.Message != want {
		t.Fatalf("message = %q, want %q -- the client still matches this text until it reads the code instead",
			second.Message, want)
	}
	var retry enrolmentRateLimitResult
	assertNoErr(t, json.Unmarshal(second.Result, &retry), "decode retry_after_seconds")
	if retry.RetryAfterSeconds < 1 || retry.RetryAfterSeconds > int(perSourceLodgeInterval/time.Second) {
		t.Fatalf("retry_after_seconds = %d, want 1..%d", retry.RetryAfterSeconds, int(perSourceLodgeInterval/time.Second))
	}
}

// The other two throttle shapes are untouched: the global ceremony limiter
// still carries retry_after_seconds, and a full table still carries none --
// it empties when rows expire or an operator acts, not on a clock this
// listener can quote.
func TestEnrolmentErrorResponse_OtherThrottleShapesUnchanged(t *testing.T) {
	limited := enrolmentErrorResponse(&enrolmentRateLimitedError{RetryAfter: 3 * time.Second})
	if limited.Code != codeEnrolmentThrottled {
		t.Fatalf("rate-limit code = %d, want %d", limited.Code, codeEnrolmentThrottled)
	}
	var retry enrolmentRateLimitResult
	assertNoErr(t, json.Unmarshal(limited.Result, &retry), "decode retry_after_seconds")
	if retry.RetryAfterSeconds != 3 {
		t.Fatalf("retry_after_seconds = %d, want 3", retry.RetryAfterSeconds)
	}

	full := enrolmentErrorResponse(errEnrolmentTableFull)
	if full.Code != codeEnrolmentThrottled {
		t.Fatalf("table-full code = %d, want %d", full.Code, codeEnrolmentThrottled)
	}
	if len(full.Result) != 0 {
		t.Fatalf("table-full carries a result payload it never had: %s", full.Result)
	}

	// And nothing else was swept into the throttled code: a genuinely
	// malformed request must still read as one.
	bad := enrolmentErrorResponse(errors.New(invalidEnrolmentLabelMessage()))
	if bad.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("a bad label = %d, want CodeInvalidParams (%d)", bad.Code, jsonrpc.CodeInvalidParams)
	}
}
