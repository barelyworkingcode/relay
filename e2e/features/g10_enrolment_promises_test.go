package features

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"relaye2e/harness"
)

// Threat-model promises proven elsewhere, by test:
//
//	TM4.5 a window takes one request, and the request cannot choose its profile:
//	  TestEnrolmentWindowTakesOneRequest, TestEnrolmentLodgeRefusesRequestedProfile,
//	  TestEnrolmentApproveUsesWindowProfile, TestEnrolmentApproveRefusesGrant.
//	TM2.4 one enrolment reaches one profile:
//	  TestEnrolmentOneProfileCreate, TestEnrolmentOneProfileSign, TestEnrolmentOneProfileUpdate,
//	  TestEnrolmentNamingTwoProfilesRefused, TestRemoteCallsScopedAndAudited.
//	TM2.5 deleting a profile revokes its enrolments, live connections included:
//	  TestProfileRemoveRevokesEnrolments.

// TestPromiseNoPortWithoutWindow proves TM4.4: a peer reaches no enrolment
// socket unless a window is open, before the first window and after each way a
// window ends.
func TestPromiseNoPortWithoutWindow(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a loopback port: %v", err)
	}
	fixed := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("releasing the port: %v", err)
	}
	r := g10Start(t, nil, func(o *harness.Options) {
		o.Settings = map[string]json.RawMessage{"remote": json.RawMessage(`{"enabled":true,"listen":"127.0.0.1:0","enrolment_listen":"` + fixed + `"}`)}
	})
	i := r.i

	if !i.EnrolRefused(fixed) {
		t.Fatalf("a connection to the configured enrolment address %s succeeded with no window ever opened", fixed)
	}

	// A window closed by the operator.
	addr := i.EnrolOpen(r.profile)
	i.EnrolDial(addr)
	i.MustCLI("enrol", "close", "--json")
	g10WaitClose(i, "cancelled")
	if !i.EnrolRefused(addr) || !i.EnrolRefused(fixed) {
		t.Fatalf("a connection to %s or %s succeeded after enrol close", addr, fixed)
	}

	// A window closed by its one request.
	addr = i.EnrolOpen(r.profile)
	g10LodgeOn(t, i.EnrolDial(addr), "acme-laptop")
	g10WaitClose(i, "lodged")
	if !i.EnrolRefused(addr) || !i.EnrolRefused(fixed) {
		t.Fatalf("a connection to %s or %s succeeded after the lodge closed the window", addr, fixed)
	}

	// A window closed by its timer.
	i.ClockAdvance(11 * time.Second)
	addr = i.EnrolOpen(r.profile)
	i.EnrolDial(addr)
	i.ClockAdvance(5*time.Minute + time.Second)
	g10WaitClose(i, "expired")
	if !i.EnrolRefused(addr) || !i.EnrolRefused(fixed) {
		t.Fatalf("a connection to %s or %s succeeded after the window expired", addr, fixed)
	}
	g10RequireWindowClosed(t, i)
	if got := i.ReloadReady().Listeners["enrolment"]; got != "" {
		t.Fatalf("ready.json lists an enrolment listener %q with no window open", got)
	}
}

// TestPromiseLodgeRaisesNoPrompt proves TM4.3 for G10.03: a stranger's lodge
// reaches no presence prompt. Only enrolment.window.open is approvable, so a
// prompt raised by the lodge would be answered deny and leave a row.
func TestPromiseLodgeRaisesNoPrompt(t *testing.T) {
	t.Parallel()
	r := g10Start(t, nil)
	i := r.i
	addr := i.EnrolOpen(r.profile)

	answers := func() int { return len(i.Events(harness.EventQuery{Key: "debug.presence.answer"})) }
	decisions := func() int { return len(i.Audit(harness.AuditQuery{Event: "control_decision"})) }
	answersBefore, decisionsBefore := answers(), decisions()
	if answersBefore == 0 || decisionsBefore == 0 {
		t.Fatalf("the open left %d presence answers and %d control_decision rows, want the open's own", answersBefore, decisionsBefore)
	}

	l := g10LodgeOn(t, i.EnrolDial(addr), "acme-laptop")
	l.poll(t)
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.request.lodge", Fields: map[string]any{"status": "ok", "request_id": l.requestID}})
	g10WaitClose(i, "lodged")

	if got := answers(); got != answersBefore {
		t.Fatalf("the lodge consumed a presence answer: %d before, %d after", answersBefore, got)
	}
	if got := decisions(); got != decisionsBefore {
		t.Fatalf("the lodge wrote a control_decision row: %d before, %d after", decisionsBefore, got)
	}
}

// TestPromiseProfileDeleteRefusesClient proves TM2.5: deleting a profile ends
// its client's live connection and refuses the same certificate afterwards.
// Relay closes an unenrolled certificate without a reply, so the refusal is a
// closed connection, not an answer.
func TestPromiseProfileDeleteRefusesClient(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.sign": harness.OutcomeApprove})
	i := r.i
	c := g10Sign(t, r, "acme-promise")
	live := r.dial(c)
	if names, ok := g10ListTools(t, live, r.profile); !ok || len(names) != 2 {
		t.Fatalf("the enrolled client lists tools %v (ok=%v) before the delete, want the profile's two", names, ok)
	}

	res := i.CLI("project", "remove", "--id", r.profile)
	if res.Code != 0 {
		t.Fatalf("project remove exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.remove", Trace: res.Trace, Fields: map[string]any{"status": "ok", "project_id": r.profile}})

	if names, ok := g10ListTools(t, live, r.profile); ok {
		t.Fatalf("the live connection still lists tools %v after its profile was deleted", names)
	}
	if names, ok := g10ListTools(t, r.dial(c), r.profile); ok {
		t.Fatalf("a new connection with the same certificate lists tools %v after its profile was deleted", names)
	}
	if _, resp := g10GetEnrolment(t, i, "acme-promise"); resp.Status != 404 {
		t.Fatalf("GET of the enrolment after its profile was deleted answered %d, want 404", resp.Status)
	}
}
