package features

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"relaye2e/harness"
)

// These tests adopt a fake outside Claude that runs on the ssh stub host (the
// host is this machine; the stub runs the host script with HOME at the remote
// root). The outside process, its registry entry and its transcript are the
// ones the local adopt tests plant, under the remote root instead of HOME. The
// agent the script starts is the instance's fake agent, found on the host as
// its claude (see g11dStartWith).

const (
	haHostID = "h_acme01"
	haTarget = "acme@testbox"

	haDetachOn  = `"detach_sessions":true,`
	haDetachOff = `"detach_sessions":false,`
)

// haRig is an adRig with the stub host and one host project, beside the local
// project the adRig holds.
type haRig struct {
	*adRig
	g *g11dRig // the host side: run folders, tmux, the fake agent's start log
}

// haStart boots an instance with the ssh stub, a host whose detach_sessions
// member is given as JSON text ("" for none), the fake agent as the host's
// claude, one host project that allows claude-code and one local project. It
// returns once every machine has completed a probe.
func haStart(t *testing.T, detach string) *haRig {
	t.Helper()
	seed := `[{"id":"` + haHostID + `","name":"testbox","target":"` + haTarget + `",` + detach + `"created_at":"2026-01-01T00:00:00Z"}]`
	i := cosStart(t, func(o *harness.Options) {
		o.SSHStub = true
		o.Settings = map[string]json.RawMessage{"hosts": json.RawMessage(seed)}
	})
	g := &g11dRig{i: i, remote: i.RemoteRoot()}

	// The host's claude-code template exists only when a probe finds claude on
	// the host, so the host gets the fake as its claude first.
	claudeDir := filepath.Join(g.remote, "hostclaude")
	g.mkdir(t, claudeDir)
	g.write(t, filepath.Join(claudeDir, "claude"), "#!/bin/sh\nexec "+agShQuote(filepath.Join(i.Home, ".local", "bin", "claude"))+" \"$@\"\n", 0o755)
	g.write(t, filepath.Join(g.remote, ".profile"), "export PATH="+agShQuote(claudeDir)+":\"$PATH\"\n", 0o600)
	i.MustCLI("host", "probe", "--id", haHostID, "--json")

	// The project's path is the resolved one: the probe reports a process's
	// resolved folder, and the launch refuses a folder outside the project.
	resolved, err := filepath.EvalSymlinks(g.remote)
	if err != nil {
		t.Fatalf("resolving the remote root: %v", err)
	}
	hostDir := filepath.Join(resolved, "boxproj")
	g.mkdir(t, hostDir)
	body, _ := json.Marshal(map[string]any{"name": "Acme Box", "path": hostDir, "host_id": haHostID, "allowed_templates": []string{"claude-code"}})
	g.projectID = haCreateProject(t, i, body)
	g.dir = hostDir

	src, err := os.ReadFile("/bin/sleep")
	if err != nil {
		t.Fatalf("reading /bin/sleep: %v", err)
	}
	o := &osRig{t: t, i: i, bin: filepath.Join(t.TempDir(), "claude"), remote: resolved}
	if err := os.WriteFile(o.bin, src, 0o755); err != nil {
		t.Fatalf("writing %s: %v", o.bin, err)
	}
	localRoot := osDir(t, filepath.Join(i.Home, "work"), "acme-adopt")
	local, _ := json.Marshal(map[string]any{"name": "Acme adopt", "path": localRoot, "allowed_templates": []string{"claude-code"}})
	r := &haRig{adRig: &adRig{osRig: o, root: localRoot, pid: haCreateProject(t, i, local)}, g: g}

	// Ready signal: every machine has completed one probe.
	osSubscribe(t, i).until(osBound, func(s osSnap) bool {
		n := 0
		for _, m := range s.Machines {
			if m.LastProbeAt != "" && m.Status == "reachable" {
				n++
			}
		}
		return n == 2
	})
	return r
}

func haCreateProject(t *testing.T, i *harness.Instance, body []byte) string {
	t.Helper()
	res := i.CLIWith(harness.CLIOpts{Stdin: body}, "project", "create", "--file", "-", "--json")
	if res.Code != 0 {
		t.Fatalf("project create exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	var p struct {
		ID string `json:"id"`
	}
	res.JSON(t, &p)
	if p.ID == "" {
		t.Fatalf("project create printed no id: %s", res.Stdout)
	}
	return p.ID
}

// hostFake starts a fake outside Claude in a new folder of the host project.
func (h *haRig) hostFake(name string, ignore ...string) *adFake {
	h.t.Helper()
	return adSpawn(h.t, osDir(h.t, filepath.Join(h.remote, "boxproj"), name), adUUID(), ignore...)
}

// hostRow probes and returns the host's row for f. The console lists the same
// process as a row of its own, so the row is picked by its hostId.
func (h *haRig) hostRow(f *adFake) (id string, row map[string]any) {
	h.t.Helper()
	s, frame := h.probeFrame()
	for _, r := range s.Sessions {
		if r.Directory == f.dir && r.State != "ended" && r.HostID != nil && *r.HostID == haHostID {
			return r.ID, osRowByID(frame, r.ID)
		}
	}
	h.t.Fatalf("no live host row for %s; snapshot %+v", f.dir, s)
	return "", nil
}

// haSessions reads relay session list as untyped rows.
func haSessions(t *testing.T, i *harness.Instance) []map[string]any {
	t.Helper()
	var out struct {
		Sessions []map[string]any `json:"sessions"`
	}
	i.MustCLI("session", "list", "--json").JSON(t, &out)
	return out.Sessions
}

// haAdoptRows returns the intent and completion rows of an audit event, once a
// completion exists.
func haAdoptRows(t *testing.T, i *harness.Instance, event string) (intents, comps []g4Row) {
	t.Helper()
	rows := g4Wait(t, i, g4Has(func(r g4Row) bool { return r.str("phase") == "completion" }), "--event", event)
	return g4Where(rows, func(r g4Row) bool { return r.str("phase") == "intent" }),
		g4Where(rows, func(r g4Row) bool { return r.str("phase") == "completion" })
}

// haOneAdoptPair fails t unless the audit holds one session_adopt intent and
// exactly one completion, both with the host id, and returns the completion.
func haOneAdoptPair(t *testing.T, i *harness.Instance) g4Row {
	t.Helper()
	intents, comps := haAdoptRows(t, i, "session_adopt")
	if len(intents) != 1 || len(comps) != 1 {
		t.Fatalf("session_adopt: %d intent and %d completion rows, want one each", len(intents), len(comps))
	}
	for _, r := range []g4Row{intents[0], comps[0]} {
		if got := r.obj("args").str("host_id"); got != haHostID {
			t.Fatalf("session_adopt %s row carries host_id %q, want %q", r.str("phase"), got, haHostID)
		}
	}
	if intents[0].str("outcome") != "pending" {
		t.Fatalf("session_adopt intent outcome %q, want pending", intents[0].str("outcome"))
	}
	return comps[0]
}

// haStarts counts the fake agent's starts on the host with --resume conv.
func (h *haRig) haStarts(conv string) int {
	h.t.Helper()
	n := 0
	for _, argv := range h.g.hostStarts(h.t) {
		if adHasFlag(argv, "--resume", conv) {
			n++
		}
	}
	return n
}

// terminalLine waits for the terminal line of the session's adopt.state: the
// script has no event, so this is a bounded file watch.
func (h *haRig) terminalLine(sid string) string {
	h.t.Helper()
	var last string
	g11dUntil(h.t, "the terminal line of adopt.state", func() bool {
		raw, err := os.ReadFile(filepath.Join(h.g.agentsDir(sid), "adopt.state"))
		if err != nil {
			return false
		}
		for _, line := range strings.Split(string(raw), "\n") {
			for _, p := range []string{"ok ", "refused ", "failed "} {
				if strings.HasPrefix(line, p) {
					last = line
					return true
				}
			}
		}
		return false
	})
	return last
}

// AC: an idle Claude row on a host is adoptable; the adopt answers 201 with no
// model in the body, ends the outside process and resumes the conversation in
// its folder under the host's tmux session relay-agent-<id>, with one SSH adopt
// command and no kill call; the session lists with its host, headless, and no
// unsandboxed; the audit rows carry host_id and one completion per intent.
func TestAdoptHostSuccess(t *testing.T) {
	t.Parallel()
	h := haStart(t, haDetachOn)
	f := h.hostFake("app")
	f.idle(h.remote, "") // a transcript that names no model: the local door would answer model_unknown
	id, row := h.hostRow(f)
	if row["adoptable"] != true || row[osClaudeIDKey] != f.sid {
		t.Fatalf("host row = %v, want adoptable true and %s %s", row, osClaudeIDKey, f.sid)
	}
	if _, has := row["adoptReason"]; has {
		t.Fatalf("an adoptable host row carries adoptReason: %v", row)
	}

	base := len(h.i.SSHCalls())
	resp := h.adopt(map[string]any{"outsideId": id})
	res := adCreated(t, resp)
	if res.ClaudeSessionID != f.sid || res.Interrupted || res.RecoverySent {
		t.Fatalf("result = %+v, want claudeSessionId %s, interrupted false, recoverySent false", res, f.sid)
	}
	callsAfterAdopt := h.i.SSHCalls()[base:]
	var body map[string]any
	resp.JSON(t, &body)
	adNoKeys(t, "the 201 body", body, "unsandboxed", "agent", "conversationId")
	f.signalAt(f.expect("signal TERM"), "TERM")
	f.gone()

	var sess map[string]any
	for _, s := range haSessions(t, h.i) {
		if s["id"] == res.SessionID {
			sess = s
		}
	}
	if sess == nil {
		t.Fatalf("session list holds no %s", res.SessionID)
	}
	host, _ := sess["host"].(map[string]any)
	if sess["live"] != true || sess["headless"] != true || sess["directory"] != f.dir || sess["projectId"] != h.g.projectID || host["id"] != haHostID {
		t.Fatalf("session = %v, want live, headless, in %s of project %s, on host %s", sess, f.dir, h.g.projectID, haHostID)
	}
	adNoKeys(t, "the session row", sess, "unsandboxed")

	// The first reply is the signal that the agent runs.
	g1sSendText(t, h.i, res.SessionID, "acme next", "echo: acme next")
	if n := h.haStarts(f.sid); n != 1 {
		t.Fatalf("the host agent started %d times with --resume %s, want 1", n, f.sid)
	}
	if !h.g.hasSession(t, res.SessionID) {
		t.Fatalf("the host's tmux has no session %s", g11pName(res.SessionID))
	}

	// Relay sends no signal over SSH itself: the adopt command is the one call
	// that starts the script, and no other call names the outside pid. (The
	// runner's own calls hold kill -0 and helper kills, so "kill" is no test.)
	pidWord := regexp.MustCompile(`\b` + strconv.Itoa(f.pid) + `\b`)
	adoptCalls := 0
	for _, c := range callsAfterAdopt {
		s := c.Remote + "\n" + c.Script()
		if strings.Contains(s, "adopt.sh") {
			adoptCalls++
			continue
		}
		if pidWord.MatchString(s) {
			t.Fatalf("an ssh call outside the adopt command names the outside pid %d: %.300s", f.pid, s)
		}
	}
	if adoptCalls != 1 {
		t.Fatalf("%d ssh calls started the adopt script, want 1", adoptCalls)
	}

	comp := haOneAdoptPair(t, h.i)
	args := comp.obj("args")
	if comp.str("outcome") != "ok" || args["interrupted"] != false || args.str("session_id") != res.SessionID {
		t.Fatalf("session_adopt completion = %v, want ok, interrupted false and session_id %s", comp, res.SessionID)
	}
	if _, has := args["signals"]; has {
		t.Fatalf("a host adopt's completion carries signals: %v", args)
	}
	h.i.WaitEvent(harness.EventQuery{Key: "session.adopt", Trace: resp.Trace, Fields: map[string]any{
		"status": "ok", "session_id": res.SessionID,
	}}, osBound)
}

// AC: a busy session is refused with 409 busy unless the request interrupts;
// nothing is signalled and no session is left.
func TestAdoptHostBusy(t *testing.T) {
	t.Parallel()
	h := haStart(t, haDetachOn)
	f := h.hostFake("app", "INT", "TERM")
	f.busy(h.remote, adModelSn)
	id, row := h.hostRow(f)
	if row["state"] != "running" || row["adoptable"] != true {
		t.Fatalf("busy host row = %v, want state running and adoptable true", row)
	}

	otRefusal(t, h.adopt(map[string]any{"outsideId": id, "interrupt": false}), http.StatusConflict, "busy")
	adNoSignals(t, f)
	if n := len(haSessions(t, h.i)); n != 0 {
		t.Fatalf("a refused adopt left %d sessions", n)
	}
}

// AC: when the pid's start time on the host differs from the row's, the script
// sends nothing and the adopt answers 409 changed; no record remains.
func TestAdoptHostChanged(t *testing.T) {
	t.Parallel()
	h := haStart(t, haDetachOn)
	f := h.hostFake("app", "INT", "TERM")
	f.idle(h.remote, adModelSn)
	id, row := h.hostRow(f)
	if row["adoptable"] != true {
		t.Fatalf("host row = %v, want adoptable true", row)
	}

	h.i.SSHLieStart(f.pid)
	otRefusal(t, h.adopt(map[string]any{"outsideId": id, "interrupt": true}), http.StatusConflict, "changed")
	adNoSignals(t, f)
	if n := len(haSessions(t, h.i)); n != 0 {
		t.Fatalf("a refused adopt left %d sessions", n)
	}
	comp := haOneAdoptPair(t, h.i)
	if comp.str("outcome") != "error" || comp.str("error") != "changed" {
		t.Fatalf("session_adopt completion = %v, want outcome error with error changed", comp)
	}
}

// AC: when the ssh command ends with no result, the adopt answers 502
// host_link_lost and the dormant record stays; the script finishes on the host,
// and Resume attaches the agent it started instead of starting a second.
func TestAdoptHostLinkDrop(t *testing.T) {
	t.Parallel()
	h := haStart(t, haDetachOn)
	// The fake survives SIGTERM, so the script is still in its ladder when the
	// link drops.
	f := h.hostFake("app", "TERM")
	f.idle(h.remote, adModelSn)
	id, _ := h.hostRow(f)

	done := make(chan harness.Response, 1)
	go func() {
		done <- h.i.SocketHTTP(h.i.Credential("cos")).Do("POST", adDoor, map[string]any{"outsideId": id}, harness.ReqOpts{Trace: harness.NewTrace(t)})
	}()
	f.signalAt(f.expect("signal TERM"), "TERM")
	h.i.SSHDown(haTarget)
	var resp harness.Response
	select {
	case resp = <-done:
	case <-time.After(osBound):
		t.Fatalf("the adopt door did not answer within %v of the drop", osBound)
	}
	otRefusal(t, resp, http.StatusBadGateway, "host_link_lost")

	// The session lists as dormant. The id is read from the list; when the list
	// holds none, the check fails and the test goes on with the session_launch
	// row's session_id, to reach the resume.
	var sid string
	listed := haSessions(t, h.i)
	if len(listed) == 1 && listed[0]["live"] == false && listed[0]["directory"] == f.dir {
		sid, _ = listed[0]["id"].(string)
	} else {
		t.Errorf("sessions after the drop = %v, want one dormant session in %s", listed, f.dir)
		rows := g4Wait(t, h.i, g4Has(func(r g4Row) bool { return r.obj("args").str("session_id") != "" }), "--event", "session_launch")
		sid = rows[0].obj("args").str("session_id")
	}
	comp := haOneAdoptPair(t, h.i)
	if comp.str("outcome") != "error" || comp.str("error") != "host_link_lost" {
		t.Fatalf("session_adopt completion = %v, want outcome error with error host_link_lost", comp)
	}

	// The link returns; the script ends the outside process and starts the agent
	// on its own.
	h.i.SSHUp(haTarget)
	if line := h.terminalLine(sid); !strings.HasPrefix(line, "ok ") {
		t.Fatalf("adopt.state ends with %q, want an ok line", line)
	}
	f.gone()

	res := h.i.MustCLI("session", "resume", "--id", sid, "--json")
	var resumed struct {
		SessionID string `json:"session_id"`
		Resumed   bool   `json:"resumed"`
	}
	res.JSON(t, &resumed)
	if resumed.SessionID != sid {
		t.Fatalf("resume printed session_id %q, want %s", resumed.SessionID, sid)
	}
	g1sSendText(t, h.i, sid, "acme next", "echo: acme next")
	if n := h.haStarts(f.sid); n != 1 {
		t.Fatalf("the host agent started %d times with --resume %s, want the one the script started", n, f.sid)
	}
	var gens []string
	for _, g := range h.g.generations(t, sid) {
		if strings.HasPrefix(g, "g") {
			gens = append(gens, g)
		}
	}
	if len(gens) != 1 || gens[0] != "g1" {
		t.Fatalf("generations after the resume are %v, want only g1", gens)
	}
}
