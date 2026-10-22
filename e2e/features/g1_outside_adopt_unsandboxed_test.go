package features

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"relaye2e/harness"
)

// These tests adopt a fake outside Claude that runs in a folder no registered
// project holds (docs/session-adoption.md, "A Claude session in no project").
// The adopted session runs unsandboxed: no sandbox profile, so a launch gets no
// CLAUDE_CONFIG_DIR, which only a sandboxed Claude is given. The helpers of
// g1_outside_adopt_test.go start the fake and the instance.

// nsAdopted is one adopted session in no project.
type nsAdopted struct {
	a    *adRig
	f    *adFake
	sid  string
	body map[string]any
}

// nsLoose starts a fake outside Claude in a new folder under no project.
func nsLoose(a *adRig, ignore ...string) *adFake {
	a.t.Helper()
	return adSpawn(a.t, osDir(a.t, a.t.TempDir(), "loose"), adUUID(), ignore...)
}

// nsRow returns the wire row of the fake after a probe.
func nsRow(t *testing.T, a *adRig, f *adFake) map[string]any {
	t.Helper()
	s, frame := a.probeFrame()
	live := s.liveAt(f.dir)
	if live == nil {
		t.Fatalf("no live row for %s; snapshot %+v", f.dir, s)
	}
	return osRowByID(frame, live.ID)
}

// nsAdoptIdle adopts an idle fake in no project and waits for it to end. The
// HTTP answer is the signal for the adopt; the fake's exit is the signal for
// the process.
func nsAdoptIdle(t *testing.T, a *adRig) *nsAdopted {
	t.Helper()
	f := nsLoose(a)
	f.idle(a.i.Home, adModelSn)
	row := nsRow(t, a, f)
	resp := a.adopt(map[string]any{"outsideId": row["id"], "interrupt": false})
	res := adCreated(t, resp)
	var body map[string]any
	resp.JSON(t, &body)
	f.signalAt(f.expect("signal TERM"), "TERM")
	f.gone()
	g1cKillRuns(t, a.i, res.SessionID)
	return &nsAdopted{a: a, f: f, sid: res.SessionID, body: body}
}

func nsSessionList(t *testing.T, i *harness.Instance) []map[string]any {
	t.Helper()
	var out struct {
		Sessions []map[string]any `json:"sessions"`
	}
	i.MustCLI("session", "list", "--json").JSON(t, &out)
	return out.Sessions
}

func nsSessionRow(t *testing.T, i *harness.Instance, sid string) map[string]any {
	t.Helper()
	for _, s := range nsSessionList(t, i) {
		if s["id"] == sid {
			return s
		}
	}
	t.Fatalf("session list holds no %s", sid)
	return nil
}

func nsTerminalRow(t *testing.T, i *harness.Instance, tid string) map[string]any {
	t.Helper()
	var out struct {
		Terminals []map[string]any `json:"terminals"`
	}
	i.MustCLI("terminal", "list", "--json").JSON(t, &out)
	for _, tr := range out.Terminals {
		if tr["id"] == tid {
			return tr
		}
	}
	t.Fatalf("terminal list holds no %s", tid)
	return nil
}

// nsCallerDo posts as the owner: the socket and the execute credential.
func nsCallerDo(a *adRig, method, path string, body any) harness.Response {
	a.t.Helper()
	return a.i.SocketHTTP(a.i.Credential("cos")).Do(method, path, body, harness.ReqOpts{Trace: harness.NewTrace(a.t)})
}

// nsSend sends one turn so the headless agent has started.
func nsSend(t *testing.T, ad *nsAdopted, text string) {
	t.Helper()
	g1sSendText(t, ad.a.i, ad.sid, text, "echo: "+text)
}

// nsStarts are the start lines of the instance's fake claude, in order.
func nsStarts(i *harness.Instance) []harness.AgentCall {
	var out []harness.AgentCall
	for _, c := range i.FakeAgentCalls("claude") {
		if c.Event == "start" && (len(c.Argv) == 0 || (c.Argv[0] != "debug" && c.Argv[0] != "--list-models")) {
			out = append(out, c)
		}
	}
	return out
}

func nsResumeStarts(i *harness.Instance, conv string) int {
	n := 0
	for _, c := range nsStarts(i) {
		if adHasFlag(c.Argv, "--resume", conv) {
			n++
		}
	}
	return n
}

// nsNoSandbox fails t when a start line carries the config folder a sandboxed
// Claude is given.
func nsNoSandbox(t *testing.T, i *harness.Instance) {
	t.Helper()
	for _, c := range nsStarts(i) {
		if v, has := c.Env["CLAUDE_CONFIG_DIR"]; has {
			t.Fatalf("a claude start %v carries CLAUDE_CONFIG_DIR=%q, which only a sandboxed launch gets", c.Argv, v)
		}
	}
}

// nsDropIn drops in on sid and returns the terminal id. Waits: none possible
// for the terminal's claude to start: relay writes no event then, and the fake
// claude writes nothing to the pty, so the call log is polled to its bound.
func nsDropIn(t *testing.T, a *adRig, sid, conv string, starts int) string {
	t.Helper()
	resp := nsCallerDo(a, "POST", "/api/sessions/"+sid+"/drop-in", map[string]any{})
	var out struct {
		ClaudeSessionID string `json:"claudeSessionId"`
		Terminal        struct {
			TerminalID string `json:"terminalId"`
		} `json:"terminal"`
	}
	resp.JSON(t, &out)
	if resp.Status != http.StatusCreated || out.Terminal.TerminalID == "" || out.ClaudeSessionID != conv {
		t.Fatalf("drop-in answered %d with terminal %q and conversation %q, want 201, a terminal and %s: %s",
			resp.Status, out.Terminal.TerminalID, out.ClaudeSessionID, conv, resp.Body)
	}
	g11dUntil(t, "the drop-in terminal to start claude with --resume "+conv, func() bool {
		return nsResumeStarts(a.i, conv) >= starts
	})
	return out.Terminal.TerminalID
}

// nsHandBack closes the terminal and waits for the hand back's event.
func nsHandBack(t *testing.T, a *adRig, sid, tid string) {
	t.Helper()
	if del := nsCallerDo(a, "DELETE", "/api/terminals/"+tid, nil); del.Status != http.StatusNoContent {
		t.Fatalf("DELETE /api/terminals/%s answered %d, want 204: %s", tid, del.Status, del.Body)
	}
	a.i.WaitEvent(harness.EventQuery{Key: "session.hand_back",
		Fields: map[string]any{"status": "ok", "session_id": sid}}, g1tEventDeadline)
}

// AC: a live console Claude row in no project is adoptable with no reason,
// on the snapshot row and on the detail. A registry reason still applies.
func TestAdoptNoProjectRowAdoptable(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	idle := nsLoose(a, "INT", "TERM")
	idle.idle(a.i.Home, adModelSn)
	busy := nsLoose(a, "INT", "TERM")
	busy.busy(a.i.Home, adModelSn)
	bg := nsLoose(a, "INT", "TERM")
	bg.idle(a.i.Home, adModelSn)
	bg.register(a.i.Home, "idle", idle.start, func(e map[string]any) { e["kind"] = "bg" })

	s, frame := a.probeFrame()
	for _, c := range []struct {
		name string
		f    *adFake
	}{{"idle", idle}, {"busy", busy}} {
		live := s.liveAt(c.f.dir)
		if live == nil {
			t.Fatalf("%s: no live row for %s; snapshot %+v", c.name, c.f.dir, s)
		}
		row := osRowByID(frame, live.ID)
		if row["projectId"] != nil || row["adoptable"] != true || row[osClaudeIDKey] != c.f.sid {
			t.Fatalf("%s: row = %v, want projectId null, adoptable true and %s %s", c.name, row, osClaudeIDKey, c.f.sid)
		}
		if _, has := row["adoptReason"]; has {
			t.Fatalf("%s: an adoptable row carries adoptReason: %v", c.name, row)
		}
		if d := a.detailSession(live.ID); d["adoptable"] != true {
			t.Fatalf("%s: detail = %v, want adoptable true", c.name, d)
		} else if _, has := d["adoptReason"]; has {
			t.Fatalf("%s: an adoptable detail carries adoptReason: %v", c.name, d)
		}
	}
	live := s.liveAt(bg.dir)
	if live == nil {
		t.Fatalf("no live row for %s; snapshot %+v", bg.dir, s)
	}
	if row := osRowByID(frame, live.ID); row["adoptable"] != false || row["adoptReason"] != "not_interactive" {
		t.Fatalf("a bg row in no project = %v, want adoptable false and adoptReason not_interactive", row)
	}
	adNoSignals(t, idle, busy, bg)
}

// Pi and Codex rows in no project keep adoptable false and no_project, and the
// door refuses them.
func TestAdoptNoProjectPiCodexKeepReason(t *testing.T) {
	t.Parallel()
	t.Run("pi", func(t *testing.T) {
		t.Parallel()
		r := paStart(t, nil)
		loose := *r
		loose.root = osDir(t, t.TempDir(), "pi-loose")
		f := loose.fake("loose")
		f.idle()
		row := r.row(f)
		if row["projectId"] != nil || row["adoptable"] != false || row["adoptReason"] != "no_project" {
			t.Fatalf("Pi row in no project = %v, want projectId null, adoptable false and adoptReason no_project", row)
		}
		otRefusal(t, r.adopt(row["id"].(string), true), http.StatusConflict, "no_project")
		f.untouched()
		if n := len(adSessions(t, r.i)); n != 0 {
			t.Fatalf("a refused adopt left %d sessions", n)
		}
	})
	t.Run("codex", func(t *testing.T) {
		t.Parallel()
		r := ccStart(t)
		d := r.daemon()
		th := r.thread("unused")
		th.Dir = osDir(t, t.TempDir(), "codex-loose")
		d.add(th)
		row := r.mustRow(th)
		if row["projectId"] != nil || row["adoptable"] != false || row["adoptReason"] != "no_project" {
			t.Fatalf("Codex row in no project = %v, want projectId null, adoptable false and adoptReason no_project", row)
		}
		otRefusal(t, r.attach(row["id"].(string)), http.StatusConflict, "no_project")
		r.untouched(d)
	})
}

// The 201 answer names the conversation and unsandboxed; the session runs in
// the row's folder with no project and no sandbox.
func TestAdoptNoProjectResumesUnsandboxed(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	ad := nsAdoptIdle(t, a)

	if ad.body["unsandboxed"] != true || ad.body["claudeSessionId"] != ad.f.sid || ad.body["interrupted"] != false {
		t.Fatalf("201 body = %v, want unsandboxed true, claudeSessionId %s and interrupted false", ad.body, ad.f.sid)
	}
	row := nsSessionRow(t, a.i, ad.sid)
	if row["live"] != true || row["headless"] != true || row["directory"] != ad.f.dir || row["projectId"] != "" || row["unsandboxed"] != true {
		t.Fatalf("session = %v, want a live headless session in %s with projectId \"\" and unsandboxed true", row, ad.f.dir)
	}
	argv := adStartLine(t, a.i, ad.sid, "acme next")
	if !adHasFlag(argv, "--resume", ad.f.sid) || !adHasFlag(argv, "--model", "sonnet") {
		t.Fatalf("start line argv = %v, want --resume %s and --model sonnet", argv, ad.f.sid)
	}
	nsNoSandbox(t, a.i)
	if _, err := os.Lstat(b181Dir(a.i)); !os.IsNotExist(err) {
		t.Fatalf("the sandbox Claude folder after an unsandboxed adopt: Lstat error %v, want not exist", err)
	}
	a.i.WaitEvent(harness.EventQuery{Key: "session.adopt", Fields: map[string]any{
		"status": "ok", "claude_session_id": ad.f.sid, "session_id": ad.sid,
	}}, osBound)
}

// A folder that is gone refuses 409 directory_unusable and the process lives.
func TestAdoptNoProjectDirectoryUnusable(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	f := nsLoose(a, "INT", "TERM")
	f.idle(a.i.Home, adModelSn)
	row := nsRow(t, a, f)
	if row["adoptable"] != true {
		t.Fatalf("row = %v, want adoptable true", row)
	}
	if err := os.RemoveAll(f.dir); err != nil {
		t.Fatalf("removing %s: %v", f.dir, err)
	}

	otRefusal(t, a.adopt(map[string]any{"outsideId": row["id"], "interrupt": true}), http.StatusConflict, "directory_unusable")
	adNoSignals(t, f)
	if n := len(adSessions(t, a.i)); n != 0 {
		t.Fatalf("a refused adopt left %d sessions", n)
	}
}

// A project that holds the folder at adopt time refuses 409 changed and the
// process lives; the person adopts again from the refreshed row.
func TestAdoptNoProjectFolderNowInProject(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	f := nsLoose(a, "INT", "TERM")
	f.idle(a.i.Home, adModelSn)
	row := nsRow(t, a, f)
	if row["projectId"] != nil || row["adoptable"] != true {
		t.Fatalf("row = %v, want projectId null and adoptable true", row)
	}
	body, _ := json.Marshal(map[string]any{"name": "Acme late", "path": f.dir, "allowed_templates": []string{"claude-code"}})
	if res := a.i.CLIWith(harness.CLIOpts{Stdin: body}, "project", "create", "--file", "-", "--json"); res.Code != 0 {
		t.Fatalf("project create exited %d\nstderr: %s", res.Code, res.Stderr)
	}

	otRefusal(t, a.adopt(map[string]any{"outsideId": row["id"], "interrupt": true}), http.StatusConflict, "changed")
	adNoSignals(t, f)
	if n := len(adSessions(t, a.i)); n != 0 {
		t.Fatalf("a refused adopt left %d sessions", n)
	}
}

// With auditing off an adoptable row in no project still refuses 503 and
// nothing is signalled.
func TestAdoptNoProjectAuditOff(t *testing.T) {
	t.Parallel()
	r := osStart(t, false, func(o *harness.Options) {
		if o.Settings == nil {
			o.Settings = map[string]json.RawMessage{}
		}
		o.Settings["audit"] = json.RawMessage(`{"enabled": false}`)
	})
	a := &adRig{osRig: r}
	f := nsLoose(a, "INT", "TERM")
	f.idle(r.i.Home, adModelSn)
	row := nsRow(t, a, f)
	if row["adoptable"] != true {
		t.Fatalf("row = %v, want adoptable true", row)
	}

	otRefusal(t, r.adopt(map[string]any{"outsideId": row["id"], "interrupt": true}), http.StatusServiceUnavailable, "audit_unavailable")
	adNoSignals(t, f)
	if n := len(adSessions(t, r.i)); n != 0 {
		t.Fatalf("a refused adopt left %d sessions", n)
	}
}

// Interrupt and adopt of a busy row in no project interrupts, adopts
// unsandboxed and sends the recovery message, as for a project row.
func TestAdoptNoProjectInterrupt(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	f := nsLoose(a)
	f.openTool(a.i.Home, adModelSn, "busy")
	row := nsRow(t, a, f)

	resp := a.adopt(map[string]any{"outsideId": row["id"], "interrupt": true})
	res := adCreated(t, resp)
	var body map[string]any
	resp.JSON(t, &body)
	g1cKillRuns(t, a.i, res.SessionID)
	if !res.Interrupted || !res.RecoverySent || body["unsandboxed"] != true {
		t.Fatalf("201 body = %v, want interrupted true, recoverySent true and unsandboxed true", body)
	}
	f.signalAt(f.expect("signal INT"), "INT")
	a.i.MustCLI("session", "wait", "--id", res.SessionID, "--json")
	if in := adInputs(a.i); len(in) != 1 {
		t.Fatalf("the agent has %d user inputs, want the one recovery message", len(in))
	}
	rows := g4Wait(t, a.i, g4Has(func(r g4Row) bool {
		return r.obj("args").str("origin") == "relay-recovery" && r.str("phase") == "completion"
	}), "--event", "session_message")
	if n := len(g4Where(rows, func(r g4Row) bool { return r.obj("args").str("session_id") == res.SessionID })); n == 0 {
		t.Fatalf("no recovery session_message row names %s", res.SessionID)
	}
	if got := nsSessionRow(t, a.i, res.SessionID); got["unsandboxed"] != true || got["directory"] != f.dir {
		t.Fatalf("session = %v, want unsandboxed true in %s", got, f.dir)
	}
}

// The session and its drop-in terminal carry unsandboxed; a session adopted in
// a project, its adopt answer and its terminal do not.
func TestAdoptNoProjectListMark(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	ad := nsAdoptIdle(t, a)

	pf := a.fake("app")
	pf.idle(a.i.Home, adModelSn)
	prow := nsRow(t, a, pf)
	presp := a.adopt(map[string]any{"outsideId": prow["id"], "interrupt": false})
	pres := adCreated(t, presp)
	var pbody map[string]any
	presp.JSON(t, &pbody)
	pf.signalAt(pf.expect("signal TERM"), "TERM")
	pf.gone()
	g1cKillRuns(t, a.i, pres.SessionID)
	if _, has := pbody["unsandboxed"]; has {
		t.Fatalf("the adopt answer of a project row carries unsandboxed: %v", pbody)
	}

	nsSend(t, ad, "acme hello")
	g1sSendText(t, a.i, pres.SessionID, "acme hello", "echo: acme hello")
	looseTerm := nsDropIn(t, a, ad.sid, ad.f.sid, 2)
	projTerm := nsDropIn(t, a, pres.SessionID, pf.sid, 2)

	for _, s := range nsSessionList(t, a.i) {
		_, has := s["unsandboxed"]
		switch s["id"] {
		case ad.sid:
			if s["unsandboxed"] != true || s["projectId"] != "" {
				t.Fatalf("the session in no project = %v, want unsandboxed true and projectId \"\"", s)
			}
		default:
			if has {
				t.Fatalf("a session in a project carries unsandboxed: %v", s)
			}
		}
	}
	if tr := nsTerminalRow(t, a.i, looseTerm); tr["unsandboxed"] != true {
		t.Fatalf("the drop-in terminal in no project = %v, want unsandboxed true", tr)
	}
	if tr := nsTerminalRow(t, a.i, projTerm); tr["unsandboxed"] != nil {
		t.Fatalf("the drop-in terminal in a project carries unsandboxed: %v", tr)
	}
}

// nsNoProjectArgs fails t unless the row records no_project and no relay
// sandbox, with no project_id.
func nsNoProjectArgs(t *testing.T, r g4Row, what string) {
	t.Helper()
	args := r.obj("args")
	sandbox, has := args["relay_sandbox"]
	if args["no_project"] != true || !has || sandbox != false {
		t.Fatalf("%s row args = %v, want no_project true and relay_sandbox false", what, args)
	}
	if _, has := args["project_id"]; has {
		t.Fatalf("%s row carries project_id: %v", what, args)
	}
}

// nsLaunchArgs fails t unless the launch or resume row records no sandbox and
// no project.
func nsLaunchArgs(t *testing.T, r g4Row, what string) {
	t.Helper()
	args := r.obj("args")
	sandbox, has := args["sandbox"]
	if args["no_project"] != true || !has || sandbox != false {
		t.Fatalf("%s row args = %v, want no_project true and sandbox false", what, args)
	}
	if p := r.actorProject(); p != "" {
		t.Fatalf("%s row names actor project %q, want none", what, p)
	}
}

// The adopt, interrupt, swap and message rows record no_project and
// relay_sandbox false; the launch and resume rows record sandbox false and
// no_project.
func TestAdoptNoProjectAuditRows(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	f := nsLoose(a)
	f.openTool(a.i.Home, adModelSn, "busy")
	row := nsRow(t, a, f)
	res := adCreated(t, a.adopt(map[string]any{"outsideId": row["id"], "interrupt": true}))
	g1cKillRuns(t, a.i, res.SessionID)
	f.signalAt(f.expect("signal INT"), "INT")
	a.i.MustCLI("session", "wait", "--id", res.SessionID, "--json")
	ad := &nsAdopted{a: a, f: f, sid: res.SessionID}
	tid := nsDropIn(t, a, ad.sid, f.sid, 2)
	nsHandBack(t, a, ad.sid, tid)

	for _, ev := range []string{"session_adopt", "session_interrupt"} {
		rows := g4Wait(t, a.i, g4Has(func(r g4Row) bool { return r.str("phase") == "completion" }), "--event", ev)
		if len(rows) < 2 {
			t.Fatalf("%s: %d rows, want an intent and a completion", ev, len(rows))
		}
		for _, r := range rows {
			nsNoProjectArgs(t, r, ev+" "+r.str("phase"))
		}
	}
	swaps := g4Wait(t, a.i, func(rows []g4Row) bool {
		return len(g4Where(rows, func(r g4Row) bool { return r.str("phase") == "completion" })) >= 2
	}, "--event", "session_swap")
	if len(swaps) != 4 {
		t.Fatalf("%d session_swap rows, want 4: %v", len(swaps), swaps)
	}
	for _, r := range swaps {
		nsNoProjectArgs(t, r, "session_swap "+r.str("phase"))
	}
	msgs := g4Wait(t, a.i, g4Has(func(r g4Row) bool {
		return r.obj("args").str("origin") == "relay-recovery" && r.str("phase") == "completion"
	}), "--event", "session_message")
	named := g4Where(msgs, func(r g4Row) bool { return r.obj("args").str("session_id") == ad.sid })
	if len(named) == 0 {
		t.Fatalf("no session_message row names %s", ad.sid)
	}
	for _, r := range named {
		nsNoProjectArgs(t, r, "session_message "+r.str("phase"))
	}
	launches := g4Wait(t, a.i, func(rows []g4Row) bool {
		kinds := map[string]bool{}
		for _, r := range rows {
			if r.obj("args").str("directory") == f.dir {
				kinds[r.obj("args").str("session_kind")] = true
			}
		}
		return kinds["claude"] && kinds["pty"]
	}, "--event", "session_launch")
	for _, r := range launches {
		if r.obj("args").str("directory") == f.dir {
			nsLaunchArgs(t, r, "session_launch "+r.obj("args").str("session_kind"))
		}
	}
	resumes := g4Wait(t, a.i, g4Has(func(r g4Row) bool { return r.obj("args").str("directory") == f.dir }), "--event", "session_resume")
	for _, r := range resumes {
		if r.obj("args").str("directory") == f.dir {
			nsLaunchArgs(t, r, "session_resume")
		}
	}
}

// Drop in opens an unsandboxed terminal on the same conversation in the same
// folder; a send while it holds answers dropped_in; closing it resumes the
// session headless, unsandboxed, in the same folder.
func TestAdoptNoProjectDropInAndHandBack(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	ad := nsAdoptIdle(t, a)
	nsSend(t, ad, "acme hello")

	tid := nsDropIn(t, a, ad.sid, ad.f.sid, 2)
	tr := nsTerminalRow(t, a.i, tid)
	if tr["unsandboxed"] != true || tr["directory"] != ad.f.dir {
		t.Fatalf("terminal = %v, want unsandboxed true in %s", tr, ad.f.dir)
	}
	res := a.i.CLI("session", "message", "--id", ad.sid, "--text", "while held", "--json")
	if res.Code != 1 || g1vErrCode(res) != "dropped_in" {
		t.Fatalf("session message while held exited %d with code %q, want 1 and dropped_in\nstdout: %s\nstderr: %s",
			res.Code, g1vErrCode(res), res.Stdout, res.Stderr)
	}

	nsHandBack(t, a, ad.sid, tid)
	row := nsSessionRow(t, a.i, ad.sid)
	if row["live"] != true || row["headless"] != true || row["unsandboxed"] != true || row["directory"] != ad.f.dir || row["projectId"] != "" {
		t.Fatalf("session after the hand back = %v, want live headless unsandboxed in %s with projectId \"\"", row, ad.f.dir)
	}
	nsSend(t, ad, "acme again")
	if n := nsResumeStarts(a.i, ad.f.sid); n != 3 {
		t.Fatalf("%d claude starts carry --resume %s, want 3: the adopt, the terminal and the hand back", n, ad.f.sid)
	}
	nsNoSandbox(t, a.i)
}

// A Chief of Staff send reaches the session before a drop-in and after the
// hand back.
func TestAdoptNoProjectChiefOfStaffSend(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	ad := nsAdoptIdle(t, a)
	send := func(text string) {
		t.Helper()
		resp := a.i.SocketHTTP(a.i.Credential("cos")).Do("POST", "/api/chief-of-staff/messages",
			map[string]any{"sessionId": ad.sid, "text": text}, harness.ReqOpts{Header: g1tScope, Trace: harness.NewTrace(t)})
		if resp.Status != http.StatusAccepted {
			t.Fatalf("a Chief of Staff send answered %d, want 202: %s", resp.Status, resp.Body)
		}
		a.i.WaitEvent(harness.EventQuery{Key: "chief_of_staff.send", Trace: resp.Trace,
			Fields: map[string]any{"session_id": ad.sid}}, g1tEventDeadline)
	}
	send("acme before")
	tid := nsDropIn(t, a, ad.sid, ad.f.sid, 2)
	nsHandBack(t, a, ad.sid, tid)
	send("acme after")
}

// A resume of the dormant session runs unsandboxed in the same folder; with
// the folder gone a resume or a drop-in refuses 409 directory_unusable and the
// session does not change.
func TestAdoptNoProjectResumeDormant(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	ad := nsAdoptIdle(t, a)
	nsSend(t, ad, "acme hello")
	g1lEndAgentWhileDown(t, a.i, ad.sid)
	g1lRestart(t, a.i)

	if row := nsSessionRow(t, a.i, ad.sid); row["live"] == true || row["unsandboxed"] != true {
		t.Fatalf("dormant session = %v, want live false and unsandboxed true", row)
	}
	if err := os.Rename(ad.f.dir, ad.f.dir+".gone"); err != nil {
		t.Fatalf("moving %s away: %v", ad.f.dir, err)
	}
	// The resume door's refusal body carries the message as error, so the code
	// is read from the session.resume event, whose reason is the refusal code.
	resp := nsCallerDo(a, "POST", "/api/sessions/"+ad.sid+"/resume", nil)
	if resp.Status != http.StatusConflict {
		t.Fatalf("resume with the folder gone answered %d, want 409: %s", resp.Status, resp.Body)
	}
	a.i.WaitEvent(harness.EventQuery{Key: "session.resume", Trace: resp.Trace,
		Fields: map[string]any{"status": "denied", "reason": "directory_unusable", "session_id": ad.sid}}, g1tEventDeadline)
	if row := nsSessionRow(t, a.i, ad.sid); row["live"] == true {
		t.Fatalf("a refused resume left the session live: %v", row)
	}
	if err := os.Rename(ad.f.dir+".gone", ad.f.dir); err != nil {
		t.Fatalf("restoring %s: %v", ad.f.dir, err)
	}

	resp = nsCallerDo(a, "POST", "/api/sessions/"+ad.sid+"/resume", nil)
	var out struct {
		SessionID string `json:"session_id"`
		Resumed   bool   `json:"resumed"`
	}
	resp.JSON(t, &out)
	if resp.Status != http.StatusOK || !out.Resumed || out.SessionID != ad.sid {
		t.Fatalf("resume answered %d %+v, want 200 and resumed true: %s", resp.Status, out, resp.Body)
	}
	g1sSendText(t, a.i, ad.sid, "acme again", "echo: acme again")
	row := nsSessionRow(t, a.i, ad.sid)
	if row["live"] != true || row["unsandboxed"] != true || row["directory"] != ad.f.dir {
		t.Fatalf("resumed session = %v, want live and unsandboxed in %s", row, ad.f.dir)
	}
	if n := nsResumeStarts(a.i, ad.f.sid); n != 2 {
		t.Fatalf("%d claude starts carry --resume %s, want 2: the adopt and the resume", n, ad.f.sid)
	}
	nsNoSandbox(t, a.i)

	// With the live session's folder gone a drop-in refuses before it stops.
	if err := os.Rename(ad.f.dir, ad.f.dir+".gone"); err != nil {
		t.Fatalf("moving %s away: %v", ad.f.dir, err)
	}
	otRefusal(t, nsCallerDo(a, "POST", "/api/sessions/"+ad.sid+"/drop-in", map[string]any{}), http.StatusConflict, "directory_unusable")
	if row := nsSessionRow(t, a.i, ad.sid); row["live"] != true {
		t.Fatalf("a refused drop-in changed the session: %v", row)
	}
	if err := os.Rename(ad.f.dir+".gone", ad.f.dir); err != nil {
		t.Fatalf("restoring %s: %v", ad.f.dir, err)
	}
}

// After a relay restart the session keeps unsandboxed, and its rebind names the
// reason unsandboxed, not project_gone.
func TestAdoptNoProjectRelayRestart(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	ad := nsAdoptIdle(t, a)
	nsSend(t, ad, "acme hello")

	since := g1rRestart(t, a.i)
	ev := g1rRebindEvent(a.i, ad.sid, since)
	if ev["reason"] != "unsandboxed" {
		t.Fatalf("session.rebind line = %v, want reason unsandboxed", ev)
	}
	done := g1rRebindRows(t, a.i, ad.sid, "completion")
	if len(done) != 1 || done[0].str("error") != "unsandboxed" {
		t.Fatalf("session_rebind completion rows = %v, want one with error unsandboxed", done)
	}
	if row := nsSessionRow(t, a.i, ad.sid); row["unsandboxed"] != true || row["projectId"] != "" {
		t.Fatalf("session after the restart = %v, want unsandboxed true and projectId \"\"", row)
	}
}

// The Chief of Staff scope cannot reach the adopt door on a row in no project,
// and a read-only credential is refused; the process lives.
func TestAdoptNoProjectScopeRefused(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	f := nsLoose(a, "INT", "TERM")
	f.idle(a.i.Home, adModelSn)
	row := nsRow(t, a, f)
	body := map[string]any{"outsideId": row["id"], "interrupt": true}

	scoped := a.i.SocketHTTP(a.i.Credential("cos")).Do("POST", adDoor, body, harness.ReqOpts{Header: g1tScope, Trace: harness.NewTrace(t)})
	if scoped.Status != http.StatusForbidden {
		t.Fatalf("the chief-of-staff scope got %d, want 403: %s", scoped.Status, scoped.Body)
	}
	reader := a.i.SocketHTTP(a.i.Credential("reader")).Do("POST", adDoor, body, harness.ReqOpts{Trace: harness.NewTrace(t)})
	if reader.Status != http.StatusForbidden {
		t.Fatalf("a read-only credential got %d, want 403: %s", reader.Status, reader.Body)
	}
	adNoSignals(t, f)
	if n := len(adSessions(t, a.i)); n != 0 {
		t.Fatalf("refused adopts left %d sessions", n)
	}
}

// With claude_token_file set the unsandboxed session gets the token on fd 5 and
// never in its environment or argv; relay session mode then refuses, because a
// restart in place cannot reuse the per-launch token.
func TestAdoptNoProjectTokenDescriptor(t *testing.T) {
	t.Parallel()
	r := osStart(t, false, func(o *harness.Options) {
		if o.Settings == nil {
			o.Settings = map[string]json.RawMessage{}
		}
		o.Settings["claude_token_file"] = ctJSON(t, ctSetting)
	})
	a := &adRig{osRig: r}
	token := ctNewToken(t)
	ctWriteToken(t, r.i, token, 0o600)
	ad := nsAdoptIdle(t, a)
	adStartLine(t, r.i, ad.sid, "acme next")

	var start *harness.AgentCall
	for _, c := range nsStarts(r.i) {
		if c.Env["RELAY_SESSION_ID"] == ad.sid {
			c := c
			start = &c
		}
	}
	if start == nil {
		t.Fatalf("no claude start line carries RELAY_SESSION_ID %s", ad.sid)
	}
	ctWantToken(t, "unsandboxed adopted session", *start, token)
	ctScan(t, "the start line's environment", strings.Join(start.Environ, "\n"), token)
	ctScan(t, "the start line's argv", strings.Join(start.Argv, "\n"), token)
	nsNoSandbox(t, r.i)

	res := r.i.CLI("session", "mode", "--id", ad.sid, "--mode", "plan", "--json")
	if res.Code != 1 {
		t.Fatalf("session mode on the unsandboxed session exited %d, want 1 (resume_required)\nstdout: %s\nstderr: %s", res.Code, res.Stdout, res.Stderr)
	}
	if row := nsSessionRow(t, r.i, ad.sid); row["live"] != true {
		t.Fatalf("the session is not live after the refused mode change: %v", row)
	}
}
