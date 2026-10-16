package features

import (
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"relaye2e/harness"
)

const windowEventDeadline = 30 * time.Second

// closeDeadline bounds how long relay may take to close a connection it is
// ending. It is well under relay's 30 s idle timeout, which would close any
// quiet connection and so prove nothing.
const closeDeadline = 5 * time.Second

// g10WaitClose waits for the enrolment.window.close line that carries reason.
func g10WaitClose(i *harness.Instance, reason string) harness.Event {
	return i.WaitEvent(harness.EventQuery{Key: "enrolment.window.close", Fields: map[string]any{"status": "ok", "target": "profile", "close_reason": reason}}, windowEventDeadline)
}

func g10RequireWindowClosed(t *testing.T, i *harness.Instance) {
	t.Helper()
	if w := g10RemoteShow(t, i).EnrolmentWindow; w.Open {
		t.Fatalf("remote show reports an open enrolment window: %+v", w)
	}
}

// g10NoWindowOpenRows fails when an open left a control_decision or issuance row.
func g10NoWindowOpenRows(t *testing.T, i *harness.Instance) {
	t.Helper()
	for _, row := range i.Audit(harness.AuditQuery{Event: "credential_issued"}) {
		if row["credential"] == "enrolment_window" {
			t.Fatalf("a credential_issued enrolment_window row exists: %v", row)
		}
	}
}

func TestEnrolmentWindowOpen(t *testing.T) {
	t.Parallel()
	r := g10Start(t, nil)
	i := r.i

	res := i.MustCLI("enrol", "open", "--profile", r.profile, "--json")
	var w g10WindowView
	res.JSON(t, &w)
	if !w.Open || w.Target != "profile" || w.ProfileID != r.profile || w.ProfileName != "acme-profile" || w.Listen == "" {
		t.Fatalf("enrol open --json reports %+v, want an open profile window for %s with an address", w, r.profile)
	}
	expires, err := time.Parse(time.RFC3339, w.Expires)
	if err != nil {
		t.Fatalf("expires %q is not RFC 3339: %v", w.Expires, err)
	}
	if want := i.Clock().Now.Add(5 * time.Minute); expires.After(want.Add(time.Minute)) || expires.Before(want.Add(-time.Minute)) {
		t.Fatalf("the window expires at %s, want about five minutes from %s", expires, i.Clock().Now)
	}
	i.EnrolDial(w.Listen)
	if shown := g10RemoteShow(t, i).EnrolmentWindow; shown != w {
		t.Fatalf("remote show reports the window %+v, enrol open reported %+v", shown, w)
	}

	opened := requireEvent(t, i, harness.EventQuery{Key: "enrolment.window.open", Trace: res.Trace, Fields: map[string]any{"status": "ok", "profile_id": r.profile}})
	var issued map[string]any
	for _, row := range i.Audit(harness.AuditQuery{Event: "credential_issued", Outcome: "ok"}) {
		if row["credential"] == "enrolment_window" {
			issued = row
		}
	}
	if issued == nil {
		t.Fatalf("no credential_issued row with credential enrolment_window")
	}
	if grants, _ := issued["grants"].([]any); issued["subject"] != r.profile || len(grants) != 1 || grants[0] != r.profile {
		t.Fatalf("the enrolment_window row names subject %v grants %v, want the profile %s", issued["subject"], issued["grants"], r.profile)
	}
	if id, _ := issued["presence_id"].(string); id == "" {
		t.Fatalf("the enrolment_window row has no presence_id: %v", issued)
	}
	// The ok event is written after the bind, so a row stamped before it was recorded first.
	rowAt, err1 := time.Parse(time.RFC3339Nano, issued["ts"].(string))
	eventAt, err2 := time.Parse(time.RFC3339Nano, opened.Str("ts"))
	if err1 != nil || err2 != nil || rowAt.After(eventAt) {
		t.Fatalf("the issuance row at %v is not before the open event at %v (%v %v)", issued["ts"], opened["ts"], err1, err2)
	}
}

func TestEnrolmentWindowRefusesNonProfile(t *testing.T) {
	t.Parallel()
	// A refused prompt would leave a denied control_decision row; none means the refusal came first.
	r := g10Start(t, map[string]harness.Outcome{"enrolment.window.open": harness.OutcomeDeny})
	i := r.i
	local, _ := createProject(t, i, "acme-local")

	requireNoPrompt := func(t *testing.T, res harness.Result, what string) {
		t.Helper()
		if res.Code != 1 {
			t.Fatalf("enrol open for %s exited %d, want 1\nstderr: %s", what, res.Code, res.Stderr)
		}
		requireEvent(t, i, harness.EventQuery{Key: "enrolment.window.open", Trace: res.Trace, Fields: map[string]any{"status": "error", "reason": "invalid"}})
		for _, row := range i.Audit(harness.AuditQuery{Event: "control_decision"}) {
			if row["method"] == "enrolment.window.open" {
				t.Fatalf("a prompt was raised for %s: %v", what, row)
			}
		}
		g10RequireWindowClosed(t, i)
	}
	requireNoPrompt(t, i.CLI("enrol", "open", "--profile", "acme-no-such-id", "--json"), "an unknown id")
	requireNoPrompt(t, i.CLI("enrol", "open", "--profile", local.ID, "--json"), "a local project")
	g10NoWindowOpenRows(t, i)

	off := g10Start(t, map[string]harness.Outcome{"enrolment.window.open": harness.OutcomeDeny}, func(o *harness.Options) {
		o.Settings = map[string]json.RawMessage{"remote": json.RawMessage(`{"enabled":false,"listen":"127.0.0.1:0","enrolment_listen":"127.0.0.1:0"}`)}
	})
	res := off.i.CLI("enrol", "open", "--profile", off.profile, "--json")
	if res.Code != 1 {
		t.Fatalf("enrol open with the remote listener off exited %d, want 1\nstderr: %s", res.Code, res.Stderr)
	}
	requireEvent(t, off.i, harness.EventQuery{Key: "enrolment.window.open", Trace: res.Trace, Fields: map[string]any{"status": "error", "reason": "invalid"}})
	if !strings.Contains(string(res.Stderr), "turn the remote listener on first") {
		t.Fatalf("the refusal does not say how to turn the remote listener on: %s", res.Stderr)
	}
	for _, row := range off.i.Audit(harness.AuditQuery{Event: "control_decision"}) {
		if row["method"] == "enrolment.window.open" {
			t.Fatalf("a prompt was raised with the remote listener off: %v", row)
		}
	}
	g10NoWindowOpenRows(t, off.i)
}

func TestEnrolmentWindowOpenDenied(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.window.open": harness.OutcomeDeny})
	i := r.i

	res := i.CLI("enrol", "open", "--profile", r.profile, "--json")
	g10RequireRefusedCLI(t, i, res, "enrolment.window.open", "enrolment.window.open")
	g10RequireWindowClosed(t, i)
	g10NoWindowOpenRows(t, i)
	if addr := i.ReloadReady().Listeners["enrolment"]; addr != "" {
		t.Fatalf("ready.json lists an enrolment listener %q after a refused open", addr)
	}
}

func TestEnrolmentPortClosedWithoutWindow(t *testing.T) {
	t.Parallel()
	// A fixed address lets the test dial a port that no window bound. The old
	// enrolment_requests switch is seeded on to show relay ignores it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a loopback port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("releasing the port: %v", err)
	}
	r := g10Start(t, nil, func(o *harness.Options) {
		o.Settings = map[string]json.RawMessage{"remote": json.RawMessage(`{"enabled":true,"listen":"127.0.0.1:0","enrolment_requests":true,"enrolment_listen":"` + addr + `"}`)}
	})
	i := r.i

	if !i.EnrolRefused(addr) {
		t.Fatalf("a connection to %s succeeded with no window open", addr)
	}
	if got := i.ReloadReady().Listeners["enrolment"]; got != "" {
		t.Fatalf("ready.json lists an enrolment listener %q with no window open", got)
	}
}

func TestEnrolmentWindowTakesOneRequest(t *testing.T) {
	t.Parallel()
	r := g10Start(t, nil)
	i := r.i
	addr := i.EnrolOpen(r.profile)
	early := i.EnrolDial(addr) // opened before the window closes
	l := g10LodgeOn(t, i.EnrolDial(addr), "acme-laptop")

	g10WaitClose(i, "lodged")
	if !i.EnrolRefused(addr) {
		t.Fatalf("a new connection to %s succeeded after the first lodge", addr)
	}
	g10RequireWindowClosed(t, i)

	i.ClockAdvance(11 * time.Second)
	reply, ok := early.Send(harness.Lodge(harness.NewRemoteIdentity(t, "acme-late"), "acme-late", harness.NewSASClient(t)))
	if !ok || reply["type"] != "Error" || g10Code(reply) != -32001 {
		t.Fatalf("a lodge on a connection opened before the close answered ok=%v %v, want Error -32001", ok, reply)
	}
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.request.lodge", Fields: map[string]any{"status": "denied", "reason": "not_open"}})
	if st, _ := l.poll(t)["status"].(string); st != "pending" {
		t.Fatalf("the lodged request reads %q after a refused second lodge, want pending", st)
	}
}

func TestEnrolmentPollOnlyOnLodgeConnection(t *testing.T) {
	t.Parallel()
	r := g10Start(t, nil)
	i := r.i
	addr := i.EnrolOpen(r.profile)
	other := i.EnrolDial(addr)
	l := g10LodgeOn(t, i.EnrolDial(addr), "acme-laptop")

	reply, ok := other.Send(map[string]any{"type": "EnrolmentRequestPoll", "request_id": l.requestID})
	result, _ := reply["result"].(map[string]any)
	if !ok || reply["type"] != "Result" || result["status"] != "unknown" {
		t.Fatalf("a poll from another connection answered ok=%v %v, want status unknown", ok, reply)
	}
	if !other.Closed(closeDeadline) {
		t.Fatalf("relay left the connection open after an unknown answer")
	}
	if st, _ := l.poll(t)["status"].(string); st != "pending" {
		t.Fatalf("the lodging connection reads %q, want pending", st)
	}
}

func TestEnrolmentLodgeRefusesRequestedProfile(t *testing.T) {
	t.Parallel()
	r := g10Start(t, nil)
	i := r.i
	conn := i.EnrolDial(i.EnrolOpen(r.profile))
	id := harness.NewRemoteIdentity(t, "acme-laptop")

	frame := harness.Lodge(id, "acme-laptop", harness.NewSASClient(t))
	frame["requested_profile"] = r.profile
	reply, ok := conn.Send(frame)
	if !ok || reply["type"] != "Error" || g10Code(reply) != -32602 {
		t.Fatalf("a lodge naming requested_profile answered ok=%v %v, want Error -32602", ok, reply)
	}
	if pending := g10PendingCount(t, i); pending != 0 {
		t.Fatalf("a refused lodge left %d pending requests", pending)
	}

	// The same frame without the field is a valid lodge.
	i.ClockAdvance(11 * time.Second)
	g10OpenAndLodge(t, r, "acme-laptop")
}

func TestEnrolmentLodgeRequiresSASCommit(t *testing.T) {
	t.Parallel()
	r := g10Start(t, nil)
	i := r.i
	conn := i.EnrolDial(i.EnrolOpen(r.profile))
	id := harness.NewRemoteIdentity(t, "acme-laptop")

	reply, ok := conn.Send(map[string]any{"type": "EnrolmentRequest", "csr_pem": string(id.CSRPEM()), "label": "acme-laptop"})
	if !ok || reply["type"] != "Error" || g10Code(reply) != -32602 {
		t.Fatalf("a lodge without sas_commit answered ok=%v %v, want Error -32602", ok, reply)
	}
	if pending := g10PendingCount(t, i); pending != 0 {
		t.Fatalf("a refused lodge left %d pending requests", pending)
	}
}

func g10PendingCount(t *testing.T, i *harness.Instance) int {
	t.Helper()
	var pending []json.RawMessage
	i.MustCLI("enrol", "requests", "--json").JSON(t, &pending)
	return len(pending)
}

func TestEnrolmentWindowExpires(t *testing.T) {
	t.Parallel()
	r := g10Start(t, nil)
	i := r.i
	addr := i.EnrolOpen(r.profile)

	i.ClockAdvance(5*time.Minute - 10*time.Second)
	if w := g10RemoteShow(t, i).EnrolmentWindow; !w.Open {
		t.Fatalf("the window closed before its five minutes ran out: %+v", w)
	}
	i.EnrolDial(addr)
	if got := i.Events(harness.EventQuery{Key: "enrolment.window.close"}); len(got) != 0 {
		t.Fatalf("a close event was written before the window expired: %v", got)
	}

	i.ClockAdvance(20 * time.Second)
	g10WaitClose(i, "expired")
	if !i.EnrolRefused(addr) {
		t.Fatalf("a connection to %s succeeded after the window expired", addr)
	}
	g10RequireWindowClosed(t, i)
}

func TestEnrolmentWindowClose(t *testing.T) {
	t.Parallel()
	r := g10Start(t, nil)
	i := r.i
	addr := i.EnrolOpen(r.profile)

	res := i.MustCLI("enrol", "close", "--json")
	var closed struct {
		Closed []string `json:"closed"`
	}
	res.JSON(t, &closed)
	if len(closed.Closed) != 1 || closed.Closed[0] != "profile" {
		t.Fatalf("enrol close --json reports %+v, want [profile]", closed)
	}
	g10WaitClose(i, "cancelled")
	if !i.EnrolRefused(addr) {
		t.Fatalf("a connection to %s succeeded after enrol close", addr)
	}
	g10RequireWindowClosed(t, i)

	again := i.MustCLI("enrol", "close", "--json")
	closed.Closed = nil
	again.JSON(t, &closed)
	if len(closed.Closed) != 0 {
		t.Fatalf("enrol close with no window open reports %v, want none", closed.Closed)
	}
}

func TestEnrolmentWindowReplaced(t *testing.T) {
	t.Parallel()
	r := g10Start(t, nil)
	i := r.i
	first := i.EnrolOpen(r.profile)
	second := i.EnrolOpen(r.profile)

	g10WaitClose(i, "replaced")
	if !i.EnrolRefused(first) {
		t.Fatalf("a connection to the replaced window's address %s succeeded", first)
	}
	i.EnrolDial(second)
	if w := g10RemoteShow(t, i).EnrolmentWindow; !w.Open || w.Listen != second {
		t.Fatalf("remote show reports %+v, want the second window open at %s", w, second)
	}
}

func TestEnrolmentWindowClosesWhenRemoteTurnsOff(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"remote.configure": harness.OutcomeApprove})
	i := r.i
	addr := i.EnrolOpen(r.profile)

	res := i.CLIWith(harness.CLIOpts{Stdin: []byte(`{"enabled":false,"listen":"127.0.0.1:0","enrolment_listen":"127.0.0.1:0"}`)}, "remote", "set", "--file", "-", "--json")
	if res.Code != 0 {
		t.Fatalf("an approved remote set exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	g10WaitClose(i, "listener_off")
	if !i.EnrolRefused(addr) {
		t.Fatalf("a connection to %s succeeded after the remote listener turned off", addr)
	}
}

func TestEnrolmentLodgedConnectionClosesWhenRemoteTurnsOff(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"remote.configure": harness.OutcomeApprove})
	i := r.i
	l := g10OpenAndLodge(t, r, "acme-laptop")

	res := i.CLIWith(harness.CLIOpts{Stdin: []byte(`{"enabled":false,"listen":"127.0.0.1:0","enrolment_listen":"127.0.0.1:0"}`)}, "remote", "set", "--file", "-", "--json")
	if res.Code != 0 {
		t.Fatalf("an approved remote set exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	i.WaitEvent(harness.EventQuery{Key: "enrolment.request.abandon", Fields: map[string]any{"status": "ok", "request_id": l.requestID}}, windowEventDeadline)
	if !l.conn.Closed(closeDeadline) {
		t.Fatalf("relay left the lodging connection open after the remote listener turned off")
	}
	if n := g10PendingCount(t, i); n != 0 {
		t.Fatalf("enrol requests lists %d requests after the remote listener turned off, want none", n)
	}
}

func TestEnrolmentWindowGoneAfterRestart(t *testing.T) {
	t.Parallel()
	r := g10Start(t, nil)
	i := r.i
	addr := i.EnrolOpen(r.profile)

	i.Restart()
	g10RequireWindowClosed(t, i)
	if got := i.ReloadReady().Listeners["enrolment"]; got != "" {
		t.Fatalf("ready.json lists an enrolment listener %q after a restart", got)
	}
	if !i.EnrolRefused(addr) {
		t.Fatalf("a connection to %s succeeded after a restart", addr)
	}
}

func TestEnrolmentApproveUsesWindowProfile(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.sign": harness.OutcomeApprove})
	i := r.i
	other := g10SecondProfile(t, r, "acme-other")

	first := g10OpenAndLodge(t, r, "acme-laptop")
	first.pollOpen(t)
	var pending []struct {
		RequestID   string `json:"request_id"`
		ProfileID   string `json:"profile_id"`
		ProfileName string `json:"profile_name"`
	}
	i.MustCLI("enrol", "requests", "--json").JSON(t, &pending)
	if len(pending) != 1 || pending[0].ProfileID != r.profile || pending[0].ProfileName != "acme-profile" {
		t.Fatalf("enrol requests lists %+v, want the request with profile %s", pending, r.profile)
	}
	if res := g10Approve(i, first.requestID, "acme-first"); res.Code != 0 {
		t.Fatalf("enrol approve exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	if e, resp := g10GetEnrolment(t, i, "acme-first"); resp.Status != 200 || len(e.ProjectIDs) != 1 || e.ProjectIDs[0] != r.profile {
		t.Fatalf("the enrolment is %d %+v, want exactly the window's profile %s", resp.Status, e, r.profile)
	}

	// A window on another profile enrols into that one, so the profile comes from the window.
	i.ClockAdvance(11 * time.Second)
	second := g10LodgeOn(t, i.EnrolDial(i.EnrolOpen(other)), "acme-tablet")
	second.pollOpen(t)
	if res := g10Approve(i, second.requestID, "acme-second"); res.Code != 0 {
		t.Fatalf("enrol approve exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	if e, resp := g10GetEnrolment(t, i, "acme-second"); resp.Status != 200 || len(e.ProjectIDs) != 1 || e.ProjectIDs[0] != other {
		t.Fatalf("the second enrolment is %d %+v, want exactly the other window's profile %s", resp.Status, e, other)
	}
}

func TestEnrolmentApproveRefusesGrant(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.sign": harness.OutcomeApprove})
	i := r.i
	other := g10SecondProfile(t, r, "acme-other")
	l := g10OpenAndLodge(t, r, "acme-laptop")
	l.pollOpen(t)

	for _, flag := range [][]string{{"--grant", other}, {"--no-grant"}} {
		args := append([]string{"enrol", "approve", "--id", l.requestID, "--client-id", "acme-flagged"}, flag...)
		if res := i.CLI(args...); res.Code != 2 {
			t.Fatalf("enrol approve %v exited %d, want 2\nstderr: %s", flag, res.Code, res.Stderr)
		}
	}
	g10RequireNoEnrolment(t, i, "acme-flagged")

	// The admin op refuses a body that names projects, and the same body without them approves.
	approve := func(extra map[string]any) harness.BridgeReply {
		args := map[string]any{"request_id": l.requestID, "client_id": "acme-bridged"}
		for k, v := range extra {
			args[k] = v
		}
		return i.BridgeSend(map[string]any{"type": "admin_op", "name": "enrolment.request.approve", "arguments": args})
	}
	if reply := approve(map[string]any{"project_ids": []string{other}}); reply.Type != "Error" {
		t.Fatalf("the approve admin op with project_ids answered %s, want Error", reply.Type)
	}
	g10RequireNoEnrolment(t, i, "acme-bridged")
	if reply := approve(nil); reply.Type == "Error" {
		t.Fatalf("the approve admin op without project_ids answered Error %d: the refusal above may not be about project_ids", reply.Code)
	}
	if e, resp := g10GetEnrolment(t, i, "acme-bridged"); resp.Status != 200 || len(e.ProjectIDs) != 1 || e.ProjectIDs[0] != r.profile {
		t.Fatalf("the approved enrolment is %d %+v, want the window's profile %s", resp.Status, e, r.profile)
	}
}

func TestEnrolmentApproveAfterWindowCloses(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.sign": harness.OutcomeApprove})
	i := r.i
	addr := i.EnrolOpen(r.profile)
	l := g10LodgeOn(t, i.EnrolDial(addr), "acme-laptop")
	l.pollOpen(t)
	g10WaitClose(i, "lodged")
	if !i.EnrolRefused(addr) {
		t.Fatalf("the window still accepts connections at %s", addr)
	}

	if res := g10Approve(i, l.requestID, "acme-late"); res.Code != 0 {
		t.Fatalf("approving after the window closed exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	poll := l.poll(t)
	cert, _ := poll["cert_pem"].(string)
	ca, _ := poll["ca_pem"].(string)
	ids, _ := poll["project_ids"].([]any)
	projects, _ := poll["projects"].([]any)
	if poll["status"] != "approved" || cert == "" || ca == "" || len(ids) != 1 || ids[0] != r.profile || len(projects) != 1 {
		t.Fatalf("the poll on the lodging connection answered %v, want approved with a certificate and the one profile %s", poll, r.profile)
	}
	if !l.conn.Closed(closeDeadline) {
		t.Fatalf("relay left the lodging connection open after an approved answer")
	}
	if names, ok := g10ListTools(t, i.RemoteDial(l.id, []byte(cert), []byte(ca)), r.profile); !ok || len(names) != 2 {
		t.Fatalf("the collected certificate lists tools %v (ok=%v), want the profile's two", names, ok)
	}
}

func TestEnrolmentDisconnectDropsRequest(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.sign": harness.OutcomeApprove})
	i := r.i
	l := g10OpenAndLodge(t, r, "acme-laptop")
	if n := g10PendingCount(t, i); n != 1 {
		t.Fatalf("enrol requests lists %d requests before the disconnect, want 1", n)
	}

	l.conn.Close()
	i.WaitEvent(harness.EventQuery{Key: "enrolment.request.abandon", Fields: map[string]any{"status": "ok", "request_id": l.requestID}}, windowEventDeadline)
	if n := g10PendingCount(t, i); n != 0 {
		t.Fatalf("enrol requests lists %d requests after the disconnect, want none", n)
	}
	if res := g10Approve(i, l.requestID, "acme-gone"); res.Code != 1 {
		t.Fatalf("approving a disconnected request exited %d, want 1\nstderr: %s", res.Code, res.Stderr)
	}
	g10RequireNoEnrolment(t, i, "acme-gone")
}

func TestEnrolmentApproveRefusesFailedCodeExchange(t *testing.T) {
	t.Parallel()
	// A refused prompt would leave a denied row; an approving instance proves nothing is issued either way.
	r := g10Start(t, map[string]harness.Outcome{"enrolment.sign": harness.OutcomeApprove})
	i := r.i
	l := g10OpenAndLodge(t, r, "acme-laptop")

	// Open the commitment with a value it was not made from.
	reply, ok := l.conn.Send(map[string]any{"type": "EnrolmentRequestPoll", "request_id": l.requestID, "sas_open": "00000000000000000000000000000000"})
	if !ok || reply["type"] != "Error" || g10Code(reply) != -32602 {
		t.Fatalf("a poll with a wrong sas_open answered ok=%v %v, want Error -32602", ok, reply)
	}
	var pending []struct {
		RequestID string `json:"request_id"`
		SASFailed bool   `json:"sas_failed"`
	}
	i.MustCLI("enrol", "requests", "--json").JSON(t, &pending)
	if len(pending) != 1 || pending[0].RequestID != l.requestID || !pending[0].SASFailed {
		t.Fatalf("enrol requests lists %+v, want the request with sas_failed", pending)
	}

	if res := g10Approve(i, l.requestID, "acme-failed"); res.Code != 1 {
		t.Fatalf("approving a request whose code exchange failed exited %d, want 1\nstderr: %s", res.Code, res.Stderr)
	}
	bridged := i.BridgeSend(map[string]any{"type": "admin_op", "name": "enrolment.request.approve", "arguments": map[string]any{"request_id": l.requestID, "client_id": "acme-failed"}})
	if bridged.Type != "Error" {
		t.Fatalf("the approve admin op for a failed exchange answered %s, want Error", bridged.Type)
	}
	g10RequireNoEnrolment(t, i, "acme-failed")
	for _, row := range i.Audit(harness.AuditQuery{Event: "control_decision"}) {
		if row["method"] == "enrolment.sign" {
			t.Fatalf("a prompt was raised for a request that cannot be approved: %v", row)
		}
	}
}

func TestCAFingerprintGone(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: map[string]harness.Outcome{"enrolment.sign": harness.OutcomeApprove}})
	// A CA exists once a certificate is signed, so a surviving verb would print its fingerprint.
	id := harness.NewRemoteIdentity(t, "acme-ca")
	out := filepath.Join(i.Dir, "signed-out")
	if res := i.CLIWith(harness.CLIOpts{Stdin: id.CSRPEM()}, "enrol", "sign", "--client-id", "acme-ca", "--csr", "-", "--out", out); res.Code != 0 {
		t.Fatalf("enrol sign exited %d\nstderr: %s", res.Code, res.Stderr)
	}

	res := i.CLI("enrol", "ca-fingerprint")
	if res.Code != 1 || len(res.Stdout) != 0 {
		t.Fatalf("enrol ca-fingerprint exited %d with %d stdout bytes, want exit 1 and none", res.Code, len(res.Stdout))
	}
	if !strings.Contains(strings.ToLower(string(res.Stderr)), "unknown") {
		t.Fatalf("enrol ca-fingerprint stderr does not name an unknown command: %s", res.Stderr)
	}
}
