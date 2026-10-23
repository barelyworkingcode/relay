package features

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"
	"testing"
	"time"

	"relaye2e/harness"
)

// These tests cover the terminal side of the session history (docs/session-history.md,
// "Terminal records and resume"): the ledger record of a terminal launched from a
// project's claude-code template, the conversation link, and the resume door.
//
// Two rigs. With outside set, the claude-code template runs fakeoutside under the
// base name claude: it prints "ready <pid>" to the terminal, which the test reads
// from the terminal's own output, and the test plants that pid's registry entry.
// fakeoutside has no flag parser for --resume, so the resume tests keep the
// harness's own claude-code template (the fake agent), whose call log shows the
// argv and the RELAY_SESSION_ID marker of each start.

const htRegistryOld = "Mon Jan 1 00:00:00 2001"

type htRig struct {
	*hx
	pid     string // the project id
	dir     string // the project folder, resolved
	outside bool
}

func htCopyFake(t *testing.T, bin string) {
	t.Helper()
	if err := os.Link(harness.FakeOutsidePath(), bin); err != nil {
		data, rerr := os.ReadFile(harness.FakeOutsidePath())
		if rerr != nil {
			t.Fatalf("reading the fake outside Claude: %v", rerr)
		}
		if werr := os.WriteFile(bin, data, 0o755); werr != nil {
			t.Fatalf("copying the fake outside Claude: %v", werr)
		}
	}
}

func htStart(t *testing.T, outside bool) *htRig {
	t.Helper()
	templates := []map[string]any{{"id": "acme-sleep", "name": "Acme sleep", "command": "/bin/sleep", "args": []string{"600"}}}
	if outside {
		bin := filepath.Join(t.TempDir(), "claude")
		htCopyFake(t, bin)
		templates = append(templates, map[string]any{"id": "claude-code", "name": "Claude Code", "command": bin})
	}
	raw, _ := json.Marshal(templates)
	r := osStart(t, false, func(o *harness.Options) {
		if o.Settings == nil {
			o.Settings = map[string]json.RawMessage{}
		}
		o.Settings["terminal_templates"] = raw
	})
	h := hxReady(t, r, false)
	dir := osDir(t, filepath.Join(r.i.Home, "work"), "acme-term")
	body, _ := json.Marshal(map[string]any{"name": "Acme term", "path": dir, "allowed_templates": []string{"claude-code", "acme-sleep", "pi", "codex"}})
	res := r.i.CLIWith(harness.CLIOpts{Stdin: body}, "project", "create", "--file", "-", "--json")
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
	return &htRig{hx: h, pid: p.ID, dir: dir, outside: outside}
}

func (r *htRig) cos() *harness.Client { return r.i.SocketHTTP(r.i.Credential("cos")) }

func (r *htRig) post(path string, body any) harness.Response {
	r.t.Helper()
	return r.cos().Do("POST", path, body, harness.ReqOpts{Trace: harness.NewTrace(r.t)})
}

func (r *htRig) launch(template string, extra map[string]any) harness.Response {
	r.t.Helper()
	body := map[string]any{"projectId": r.pid, "templateId": template, "cols": 80, "rows": 24}
	for k, v := range extra {
		body[k] = v
	}
	return r.post("/api/terminals", body)
}

// start launches a terminal and returns its id.
func (r *htRig) start(template string) string {
	r.t.Helper()
	resp := r.launch(template, nil)
	if resp.Status != http.StatusCreated {
		r.t.Fatalf("POST /api/terminals for %s answered %d, want 201: %s", template, resp.Status, resp.Body)
	}
	var tm struct {
		TerminalID string `json:"terminalId"`
	}
	resp.JSON(r.t, &tm)
	if tm.TerminalID == "" {
		r.t.Fatalf("the 201 body holds no terminalId: %s", resp.Body)
	}
	return tm.TerminalID
}

func (r *htRig) ledger() []map[string]any {
	r.t.Helper()
	var recs []map[string]any
	if err := json.Unmarshal(mustRead(r.t, filepath.Join(r.i.ConfigDir, g1rLedger)), &recs); err != nil {
		r.t.Fatalf("ledger does not decode: %v", err)
	}
	return recs
}

func (r *htRig) record(id string) map[string]any {
	r.t.Helper()
	for _, rec := range r.ledger() {
		if rec["session_id"] == id {
			return rec
		}
	}
	return nil
}

func (r *htRig) ptyRecords() int {
	r.t.Helper()
	n := 0
	for _, rec := range r.ledger() {
		if rec["kind"] == "pty" {
			n++
		}
	}
	return n
}

func (b hxBody) byID(id string) *hxRow {
	for k := range b.Rows {
		if b.Rows[k].ID == id {
			return &b.Rows[k]
		}
	}
	return nil
}

func (b hxBody) countConv(conv string) int {
	n := 0
	for _, r := range b.Rows {
		if r.ConversationID != nil && *r.ConversationID == conv {
			n++
		}
	}
	return n
}

func (r *htRig) row(id string) *hxRow { return r.rows().byID(id) }

// rawRow is the row as the wire prints it, for the fields hxRow does not carry.
func (r *htRig) rawRow(id string) map[string]any {
	r.t.Helper()
	resp := r.cos().Do("GET", hxPath, nil)
	var body struct {
		Rows []map[string]any `json:"rows"`
	}
	resp.JSON(r.t, &body)
	return osRowByID(body.Rows, id)
}

func (r *htRig) terminals() []string {
	r.t.Helper()
	resp := r.cos().Do("GET", "/api/terminals", nil)
	if resp.Status != http.StatusOK {
		r.t.Fatalf("GET /api/terminals answered %d, want 200: %s", resp.Status, resp.Body)
	}
	var out struct {
		Terminals []struct {
			ID string `json:"id"`
		} `json:"terminals"`
	}
	resp.JSON(r.t, &out)
	var ids []string
	for _, tm := range out.Terminals {
		ids = append(ids, tm.ID)
	}
	return ids
}

func (r *htRig) resume(id string, body any) harness.Response {
	r.t.Helper()
	return r.post(hxPath+"/"+id+"/resume", body)
}

var htSize = map[string]any{"cols": 100, "rows": 30}

// refused checks a refusal of the history door: the status and the code. The
// terminal list is the proof that nothing was started.
func (r *htRig) refused(id string, body any, status int, code string) {
	r.t.Helper()
	before := r.terminals()
	resp := r.resume(id, body)
	var e struct {
		Code string `json:"code"`
	}
	resp.JSON(r.t, &e)
	if resp.Status != status || e.Code != code {
		r.t.Fatalf("resume of %s answered %d %q, want %d %q: %s", id, resp.Status, e.Code, status, code, resp.Body)
	}
	if after := r.terminals(); len(after) != len(before) {
		r.t.Fatalf("a refused resume of %s changed the terminals from %v to %v", id, before, after)
	}
}

// readyPID reads "ready <pid>" from the terminal's output (its replay or its
// live bytes): fakeoutside prints it once its handlers are installed.
func (r *htRig) readyPID(tid string) int {
	r.t.Helper()
	ws := r.i.WebSocket("/ws", r.i.Credential("cos"))
	r.t.Cleanup(ws.Close)
	ws.Send(map[string]any{"type": "join_terminal", "terminalId": tid})
	re := regexp.MustCompile(`ready (\d+)`)
	seen := ""
	for n := 0; n < 200; n++ {
		var f struct {
			Type       string `json:"type"`
			TerminalID string `json:"terminalId"`
			Data       string `json:"data"`
			Scrollback string `json:"scrollback"`
		}
		if err := json.Unmarshal(ws.Next(osBound), &f); err != nil {
			r.t.Fatalf("decoding a frame: %v", err)
		}
		if f.TerminalID != tid {
			continue
		}
		enc := f.Data
		if f.Type == "terminal_joined" {
			enc = f.Scrollback
		}
		if b, err := base64.StdEncoding.DecodeString(enc); err == nil {
			seen += string(b)
		}
		if m := re.FindStringSubmatch(seen); m != nil {
			var pid int
			fmt.Sscan(m[1], &pid)
			return pid
		}
	}
	r.t.Fatalf("terminal %s printed no ready line; output %q", tid, seen)
	return 0
}

// agentStart is the start line of the fake claude a terminal ran. Waits: none
// possible: relay writes no event when the terminal's claude starts and the
// fake writes nothing to the pty, so its call log is polled to its bound.
func (r *htRig) agentStart(tid string) harness.AgentCall {
	r.t.Helper()
	var call harness.AgentCall
	g11dUntil(r.t, "the fake claude of terminal "+tid+" to log its start", func() bool {
		for _, c := range r.i.FakeAgentCalls("claude") {
			if c.Event == "start" && c.Env["RELAY_SESSION_ID"] == tid {
				call = c
				return true
			}
		}
		return false
	})
	return call
}

// resumeStart is the start line of a terminal that ran with --resume conv.
func (r *htRig) resumeStart(tid, conv string) harness.AgentCall {
	r.t.Helper()
	c := r.agentStart(tid)
	if !agHasPair(c.Argv, "--resume", conv) {
		r.t.Fatalf("the terminal %s started claude with %v, want --resume %s", tid, c.Argv, conv)
	}
	return c
}

func (r *htRig) pidOf(tid string) int {
	r.t.Helper()
	if r.outside {
		return r.readyPID(tid)
	}
	return r.agentStart(tid).Pid
}

func (r *htRig) registryPath(pid int) string {
	return filepath.Join(r.i.Home, ".claude", "sessions", fmt.Sprintf("%d.json", pid))
}

// register writes the registry entry Claude Code keeps for pid.
func (r *htRig) register(pid int, conv string, mut ...func(map[string]any)) {
	r.t.Helper()
	lstr, _ := adLstart(r.t, pid)
	(&adFake{t: r.t, pid: pid, dir: r.dir, sid: conv, lstr: lstr}).register(r.i.Home, "idle", time.Now(), mut...)
}

// link registers pid for conv and waits for the link event of record tid.
func (r *htRig) link(tid string, pid int, conv string) {
	r.t.Helper()
	r.register(pid, conv)
	since := time.Now()
	r.i.ClockAdvance(osProbeStep)
	r.i.WaitEvent(harness.EventQuery{Key: "session.history.link", Since: since,
		Fields: map[string]any{"session_id": tid, "status": "ok"}}, osBound)
}

// closeTerm closes the terminal and waits for relay's exit report.
func (r *htRig) closeTerm(tid string) {
	r.t.Helper()
	if del := r.cos().Do("DELETE", "/api/terminals/"+tid, nil); del.Status != http.StatusNoContent {
		r.t.Fatalf("DELETE /api/terminals/%s answered %d, want 204: %s", tid, del.Status, del.Body)
	}
	r.i.WaitEvent(harness.EventQuery{Key: "session.exited", Fields: map[string]any{"session_id": tid}}, g1tEventDeadline)
}

// transcript plants an interactive transcript of conv in dir with a client title.
func (r *htRig) transcript(dir, conv, title string) string {
	r.t.Helper()
	end := hxBase.Add(time.Hour)
	return hxPlant(r.t, r.i.Home, dir, conv, end,
		hxUser(conv, dir, end.Add(-time.Minute), "acme resume prompt", nil),
		hxRec("custom-title", conv, "", time.Time{}, map[string]any{"customTitle": title}),
		hxEnd(conv, dir, end))
}

// dormant launches a terminal, links it to a planted transcript's conversation,
// closes it and returns once relay has refreshed the held set and the listing.
func (r *htRig) dormant(conv, title string) (tid string, pid int) {
	r.t.Helper()
	tid = r.start("claude-code")
	pid = r.pidOf(tid)
	r.transcript(r.dir, conv, title)
	r.link(tid, pid, conv)
	r.closeTerm(tid)
	// Claude removes its registry entry when it exits; the fake cannot.
	if err := os.Remove(r.registryPath(pid)); err != nil {
		r.t.Fatalf("removing the registry entry: %v", err)
	}
	r.probe()
	r.scan(nil)
	return tid, pid
}

// htOutsider starts a fakeoutside, named claude, that relay did not launch,
// with marker as its RELAY_SESSION_ID.
func htOutsider(t *testing.T, dir, sid, marker string) *adFake {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "claude")
	htCopyFake(t, bin)
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating a pipe: %v", err)
	}
	cmd := exec.Command(bin)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "RELAY_SESSION_ID="+marker)
	cmd.Stdout = pw
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", bin, err)
	}
	_ = pw.Close()
	f := &adFake{t: t, cmd: cmd, pid: cmd.Process.Pid, dir: dir, sid: sid, lines: make(chan string, 256), exit: make(chan struct{})}
	go func() {
		defer close(f.lines)
		buf := make([]byte, 0, 4096)
		one := make([]byte, 1)
		for {
			n, err := pr.Read(one)
			if n == 1 {
				if one[0] == '\n' {
					f.lines <- string(buf)
					buf = buf[:0]
				} else {
					buf = append(buf, one[0])
				}
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		_ = cmd.Wait()
		f.ws = cmd.ProcessState.Sys().(syscall.WaitStatus)
		close(f.exit)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-f.exit
		_ = pr.Close()
	})
	f.expect("ready ")
	f.lstr, f.start = adLstart(t, f.pid)
	return f
}

// terminate ends a fake with TERM and waits for it to exit.
func (f *adFake) terminate() {
	f.t.Helper()
	if err := syscall.Kill(f.pid, syscall.SIGTERM); err != nil {
		f.t.Fatalf("signal TERM to pid %d: %v", f.pid, err)
	}
	f.signalAt(f.expect("signal TERM"), "TERM")
	f.gone()
}

// A terminal launched from the claude-code template has a live ledger record;
// a drop-in terminal and any other template have none.
func TestHistoryTerminalRecordAtLaunch(t *testing.T) {
	t.Parallel()
	r := htStart(t, false)

	tid := r.start("claude-code")
	rec := r.record(tid)
	if rec == nil {
		t.Fatalf("the ledger holds no record for the claude-code terminal %s", tid)
	}
	if rec["kind"] != "pty" || rec["agent"] != "claude" || rec["state"] != "live" || rec["template_id"] != "claude-code" || rec["project_id"] != r.pid {
		t.Fatalf("terminal record = %v, want kind pty, agent claude, state live, template claude-code, project %s", rec, r.pid)
	}
	if rec["directory"] != r.dir {
		t.Fatalf("terminal record directory = %v, want %s", rec["directory"], r.dir)
	}
	if c, has := rec["conversation_id"]; has && c != "" {
		t.Fatalf("a fresh terminal record carries conversation %v, want none", c)
	}
	row := r.row(tid)
	if row == nil || row.Record != "launched" || row.Kind != "terminal" || row.State != "live" || row.ProjectID == nil || *row.ProjectID != r.pid ||
		row.HostID != nil || row.Directory != r.dir || row.ConversationID != nil || row.Resume != "none" || hxStr(row.ResumeReason) != "live" {
		t.Fatalf("history row = %+v, want a live launched terminal of project %s with no conversation", row, r.pid)
	}
	if raw := r.rawRow(tid); raw["agent"] != "claude" {
		t.Fatalf("history row agent = %v, want claude", raw["agent"])
	}

	// Any other terminal template: no record, no row.
	for _, tmpl := range []string{"acme-sleep", "pi", "codex"} {
		other := r.start(tmpl)
		if rec := r.record(other); rec != nil {
			t.Fatalf("the %s terminal has the ledger record %v, want none", tmpl, rec)
		}
		if row := r.row(other); row != nil {
			t.Fatalf("the %s terminal has the history row %+v, want none", tmpl, row)
		}
	}

	// A drop-in terminal on a headless Claude session: the session keeps its
	// own record, the terminal gets none.
	sid := g1tStartSession(t, r.i, r.pid, "sonnet", `{"headless":true,"agent":true}`)
	g1sSendText(t, r.i, sid, "hi", "echo: hi")
	resp := r.post("/api/sessions/"+sid+"/drop-in", map[string]any{})
	var drop struct {
		Terminal struct {
			TerminalID string `json:"terminalId"`
		} `json:"terminal"`
	}
	resp.JSON(t, &drop)
	if resp.Status != http.StatusCreated || drop.Terminal.TerminalID == "" {
		t.Fatalf("drop-in answered %d with terminal %q, want 201 and a terminal: %s", resp.Status, drop.Terminal.TerminalID, resp.Body)
	}
	if rec := r.record(drop.Terminal.TerminalID); rec != nil {
		t.Fatalf("the drop-in terminal has the ledger record %v, want none", rec)
	}
	if n := r.ptyRecords(); n != 1 {
		t.Fatalf("the ledger holds %d pty records, want only the claude-code terminal's", n)
	}
	if del := r.cos().Do("DELETE", "/api/terminals/"+drop.Terminal.TerminalID, nil); del.Status != http.StatusNoContent {
		t.Fatalf("DELETE the drop-in terminal answered %d, want 204: %s", del.Status, del.Body)
	}
	r.i.WaitEvent(harness.EventQuery{Key: "session.hand_back", Fields: map[string]any{"status": "ok", "session_id": sid}}, g1tEventDeadline)

	// The conversation to resume is internal: a request body cannot set it.
	conv := hxConv(90)
	forged := r.launch("claude-code", map[string]any{"resumeConversation": conv, "ResumeConversation": conv})
	if forged.Status != http.StatusCreated && forged.Status != http.StatusBadRequest {
		t.Fatalf("a launch with a resumeConversation field answered %d, want 201 or 400: %s", forged.Status, forged.Body)
	}
	if forged.Status == http.StatusCreated {
		var tm struct {
			TerminalID string `json:"terminalId"`
		}
		forged.JSON(t, &tm)
		if c := r.agentStart(tm.TerminalID); agHasPair(c.Argv, "--resume", conv) {
			t.Fatalf("a request body set the resume conversation: claude started with %v", c.Argv)
		}
	}
	for _, rec := range r.ledger() {
		if rec["conversation_id"] == conv {
			t.Fatalf("a request body set the conversation of record %v", rec)
		}
	}
}

// The link needs the marker, the pid's own registry entry, and on the console a
// descendant of the record's root; it reads no transcript.
func TestHistoryTerminalLink(t *testing.T) {
	t.Parallel()

	t.Run("marker and registry", func(t *testing.T) {
		t.Parallel()
		r := htStart(t, true)
		tid := r.start("claude-code")
		pid := r.pidOf(tid)
		conv := hxConv(21)
		// No transcript exists anywhere: the link cannot come from a scan.
		r.link(tid, pid, conv)
		row := r.row(tid)
		if row == nil || row.ConversationID == nil || *row.ConversationID != conv || row.State != "live" || row.Record != "launched" {
			t.Fatalf("row after the link = %+v, want the live launched row with conversation %s", row, conv)
		}
		if rec := r.record(tid); rec == nil || rec["conversation_id"] != conv || rec["state"] != "live" {
			t.Fatalf("ledger record after the link = %v, want conversation %s", rec, conv)
		}
	})

	t.Run("a forged link keeps a discovered record of another folder", func(t *testing.T) {
		t.Parallel()
		r := htStart(t, true)
		tid := r.start("claude-code")
		pid := r.pidOf(tid)
		conv := hxConv(25)
		elsewhere := osDir(t, t.TempDir(), "elsewhere")
		r.transcript(elsewhere, conv, "Acme elsewhere")
		r.scan(nil)
		id := hxDiscoveredID(conv)
		if row := r.row(id); row == nil || row.Record != "discovered" {
			t.Fatalf("row %s before the link = %+v, want a discovered row", id, row)
		}
		// The process claims a conversation whose record lies in a folder outside
		// the terminal's: the link may set the terminal's conversation, but it
		// removes nothing of the other record.
		r.link(tid, pid, conv)
		if rec := r.record(id); rec == nil || rec["state"] != "discovered" {
			t.Fatalf("discovered record of another folder after the link = %v, want it kept", rec)
		}
	})

	t.Run("a link removes the discovered record of its own folder", func(t *testing.T) {
		t.Parallel()
		r := htStart(t, true)
		tid := r.start("claude-code")
		pid := r.pidOf(tid)
		conv := hxConv(26)
		r.transcript(r.dir, conv, "Acme here")
		r.scan(nil)
		id := hxDiscoveredID(conv)
		if row := r.row(id); row == nil || row.Record != "discovered" {
			t.Fatalf("row %s before the link = %+v, want a discovered row", id, row)
		}
		r.link(tid, pid, conv)
		if rec := r.record(id); rec != nil {
			t.Fatalf("discovered record of the terminal's folder after the link = %v, want it removed", rec)
		}
		if n := r.rows().countConv(conv); n != 1 {
			t.Fatalf("%d rows carry the conversation after the link, want the terminal's alone", n)
		}
	})

	t.Run("an entry that does not match the process", func(t *testing.T) {
		t.Parallel()
		r := htStart(t, true)
		tid := r.start("claude-code")
		pid := r.pidOf(tid)
		conv := hxConv(22)
		sub := osDir(t, r.dir, "sub")
		for name, mut := range map[string]func(map[string]any){
			"another cwd":      func(e map[string]any) { e["cwd"] = sub },
			"another lifetime": func(e map[string]any) { e["procStart"] = htRegistryOld; e["startedAt"] = int64(978307200000) },
			"not a conversation id": func(e map[string]any) {
				e["sessionId"] = "acme/../not a conversation"
			},
		} {
			r.register(pid, conv, mut)
			r.probe()
			if rec := r.record(tid); rec == nil || (rec["conversation_id"] != nil && rec["conversation_id"] != "") {
				t.Fatalf("%s: record = %v, want no conversation", name, rec)
			}
			if got := r.i.Events(harness.EventQuery{Key: "session.history.link", Fields: map[string]any{"session_id": tid}}); len(got) != 0 {
				t.Fatalf("%s: %d session.history.link lines, want none", name, len(got))
			}
		}
		// Control: the right entry for the same process links.
		r.link(tid, pid, conv)
		if rec := r.record(tid); rec == nil || rec["conversation_id"] != conv {
			t.Fatalf("record after the matching entry = %v, want conversation %s", rec, conv)
		}
	})

	t.Run("a process outside the terminal", func(t *testing.T) {
		t.Parallel()
		r := htStart(t, true)
		tid := r.start("claude-code")
		pid := r.pidOf(tid)
		sub := osDir(t, r.dir, "outsider")
		// Started by the test, not by relay, with the terminal's marker, inside the
		// record's folder and with a registry entry of its own.
		out := htOutsider(t, sub, hxConv(23), tid)
		out.register(r.i.Home, "idle", time.Now())
		r.probe()
		if rec := r.record(tid); rec == nil || (rec["conversation_id"] != nil && rec["conversation_id"] != "") {
			t.Fatalf("record after an outsider carried the marker = %v, want no conversation", rec)
		}
		if got := r.i.Events(harness.EventQuery{Key: "session.history.link", Fields: map[string]any{"session_id": tid}}); len(got) != 0 {
			t.Fatalf("%d session.history.link lines for an outsider, want none", len(got))
		}
		// Control: with the outsider gone, the terminal's own process links.
		out.terminate()
		if err := os.Remove(filepath.Join(r.i.Home, ".claude", "sessions", fmt.Sprintf("%d.json", out.pid))); err != nil {
			t.Fatalf("removing the outsider's registry entry: %v", err)
		}
		conv := hxConv(24)
		r.link(tid, pid, conv)
		if rec := r.record(tid); rec == nil || rec["conversation_id"] != conv {
			t.Fatalf("record after the terminal's own entry = %v, want conversation %s", rec, conv)
		}
	})
}

// The record is dormant after the terminal exits, after its tab is closed and
// after a relay restart, and it is listed with its title afterwards.
func TestHistoryTerminalRecordSurvivesRestart(t *testing.T) {
	t.Parallel()
	r := htStart(t, true)
	closed, exited, live := r.start("claude-code"), r.start("claude-code"), r.start("claude-code")
	exitedPID, livePID := r.pidOf(exited), r.pidOf(live)
	conv := hxConv(31)
	const title = "Acme survives restart"
	r.transcript(r.dir, conv, title)
	r.link(live, livePID, conv)

	r.closeTerm(closed)
	if err := syscall.Kill(exitedPID, syscall.SIGTERM); err != nil {
		t.Fatalf("signal TERM to pid %d: %v", exitedPID, err)
	}
	r.i.WaitEvent(harness.EventQuery{Key: "session.exited", Fields: map[string]any{"session_id": exited}}, g1tEventDeadline)

	before := r.rows()
	for _, id := range []string{closed, exited} {
		if row := before.byID(id); row == nil || row.State != "dormant" || row.Record != "launched" || row.Kind != "terminal" {
			t.Fatalf("row of %s before the restart = %+v, want a dormant launched terminal", id, row)
		}
	}
	if row := before.byID(live); row == nil || row.State != "live" {
		t.Fatalf("row of the running terminal before the restart = %+v, want live", row)
	}
	if err := os.Remove(r.registryPath(livePID)); err != nil {
		t.Fatalf("removing the registry entry: %v", err)
	}

	since := time.Now()
	r.i.Restart()
	r.i.WaitSessionHost(osBound)
	r.i.WaitEvent(harness.EventQuery{Key: "session.history.scan", Since: since, Fields: map[string]any{"machine": "console"}}, osBound)

	after := r.rows()
	for _, id := range []string{closed, exited, live} {
		if row := after.byID(id); row == nil || row.State != "dormant" || row.Record != "launched" || row.Kind != "terminal" || row.ProjectID == nil || *row.ProjectID != r.pid {
			t.Fatalf("row of %s after the restart = %+v, want a dormant launched terminal of project %s", id, row, r.pid)
		}
	}
	row := after.byID(live)
	if row.ConversationID == nil || *row.ConversationID != conv || hxStr(row.Title) != title {
		t.Fatalf("linked row after the restart = %+v, want conversation %s and title %q", row, conv, title)
	}
	for _, id := range []string{closed, exited} {
		if row := after.byID(id); row.ConversationID != nil || row.Resume != "none" || hxStr(row.ResumeReason) != "no_conversation" {
			t.Fatalf("unlinked row %+v after the restart, want no conversation and resume none/no_conversation", row)
		}
	}
	for _, id := range []string{closed, exited, live} {
		if rec := r.record(id); rec == nil || rec["state"] != "dormant" {
			t.Fatalf("ledger record of %s after the restart = %v, want dormant", id, rec)
		}
	}
}

// The door opens a claude-code terminal with --resume on a dormant terminal
// record's conversation, links the new record and removes the old one.
func TestHistoryResumeTerminal(t *testing.T) {
	t.Parallel()
	r := htStart(t, false)

	// Refusals that start nothing.
	r.refused("acme-missing-record", htSize, http.StatusNotFound, "history_not_found")
	unlinked := r.start("claude-code")
	r.agentStart(unlinked)
	r.closeTerm(unlinked)
	r.refused(unlinked, map[string]any{"cols": 0, "rows": 30}, http.StatusBadRequest, "invalid_body")
	r.refused(unlinked, map[string]any{"cols": 100, "rows": 1001}, http.StatusBadRequest, "invalid_body")
	r.refused(unlinked, htSize, http.StatusConflict, "no_conversation")
	agent := g1tStartSession(t, r.i, r.pid, "sonnet", `{"headless":true,"agent":true}`)
	r.refused(agent, htSize, http.StatusConflict, "use_session_resume")

	conv := hxConv(41)
	const title = "Acme resume me"
	tid := r.start("claude-code")
	pid := r.agentStart(tid).Pid
	path := r.transcript(r.dir, conv, title)
	r.link(tid, pid, conv)
	r.refused(tid, htSize, http.StatusConflict, "live")

	r.closeTerm(tid)
	if err := os.Remove(r.registryPath(pid)); err != nil {
		t.Fatalf("removing the registry entry: %v", err)
	}
	r.probe()
	r.scan(nil)
	row := r.row(tid)
	if row == nil || row.State != "dormant" || row.Resume != "history" || hxStr(row.Title) != title || row.ConversationID == nil || *row.ConversationID != conv {
		t.Fatalf("row before the resume = %+v, want dormant, resume history, title %q and conversation %s", row, title, conv)
	}

	// The transcript now derives another folder than the record's: the terminal
	// record keeps its own folder, so the door refuses.
	other := osDir(t, r.dir, "other")
	moved := osTranscriptPath(r.i.Home, other, conv)
	if err := os.MkdirAll(filepath.Dir(moved), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(moved), err)
	}
	hxPlant(t, r.i.Home, other, conv, hxBase.Add(2*time.Hour), hxUser(conv, other, hxBase, "acme resume prompt", nil), hxEnd(conv, other, hxBase.Add(2*time.Hour)))
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing %s: %v", path, err)
	}
	r.scan(nil)
	r.refused(tid, htSize, http.StatusConflict, "no_project")
	if err := os.Remove(moved); err != nil {
		t.Fatalf("removing %s: %v", moved, err)
	}
	r.transcript(r.dir, conv, title)
	r.scan(nil)

	resp := r.resume(tid, htSize)
	var got struct {
		TerminalID string `json:"terminalId"`
		TemplateID string `json:"templateId"`
		Directory  string `json:"directory"`
		Supersedes string `json:"supersedes"`
	}
	resp.JSON(t, &got)
	if resp.Status != http.StatusCreated || got.TerminalID == "" || got.TerminalID == tid || got.Supersedes != tid || got.TemplateID != "claude-code" || got.Directory != r.dir {
		t.Fatalf("resume answered %d %+v, want 201 with a new terminal in %s superseding %s: %s", resp.Status, got, r.dir, tid, resp.Body)
	}
	r.i.WaitEvent(harness.EventQuery{Key: "session.history.resume",
		Fields: map[string]any{"status": "ok", "session_id": tid, "new_session_id": got.TerminalID}}, osBound)
	r.resumeStart(got.TerminalID, conv)

	after := r.rows()
	if after.byID(tid) != nil {
		t.Fatalf("the old row %s is still listed after the resume", tid)
	}
	if n := after.countConv(conv); n != 1 {
		t.Fatalf("%d rows carry conversation %s after the resume, want 1", n, conv)
	}
	nrow := after.byID(got.TerminalID)
	if nrow == nil || nrow.Record != "launched" || nrow.Kind != "terminal" || nrow.State != "live" || nrow.ConversationID == nil || *nrow.ConversationID != conv ||
		nrow.ProjectID == nil || *nrow.ProjectID != r.pid {
		t.Fatalf("new row = %+v, want a live launched terminal of project %s carrying %s", nrow, r.pid, conv)
	}
	if rec := r.record(tid); rec != nil {
		t.Fatalf("the old ledger record survived the resume: %v", rec)
	}
	rec := r.record(got.TerminalID)
	if rec == nil || rec["kind"] != "pty" || rec["agent"] != "claude" || rec["state"] != "live" || rec["conversation_id"] != conv || rec["project_id"] != r.pid {
		t.Fatalf("new ledger record = %v, want a live claude pty record carrying %s", rec, conv)
	}
	launched := false
	for _, a := range r.i.Audit(harness.AuditQuery{Event: "session_launch", Tail: 100000}) {
		if args, _ := a["args"].(map[string]any); args["session_id"] == got.TerminalID && a["outcome"] == "ok" {
			launched = true
		}
	}
	if !launched {
		t.Fatalf("no ok session_launch audit row names the new terminal %s", got.TerminalID)
	}
	r.refused(tid, htSize, http.StatusNotFound, "history_not_found")
}

// A discovered record resumes only when relay re-reads the transcript now and
// its folder lies in a registered local project, and never while held.
func TestHistoryResumeDiscoveredChecks(t *testing.T) {
	t.Parallel()

	// discovered plants an interactive transcript of conv in dir and scans.
	discovered := func(r *htRig, dir, conv string) (string, string) {
		r.t.Helper()
		path := r.transcript(dir, conv, "Acme found")
		r.scan(nil)
		id := hxDiscoveredID(conv)
		if row := r.row(id); row == nil || row.Record != "discovered" {
			r.t.Fatalf("row %s before the resume = %+v, want a discovered row", id, row)
		}
		return id, path
	}

	t.Run("a registered project", func(t *testing.T) {
		t.Parallel()
		r := htStart(t, false)
		conv := hxConv(51)
		id, _ := discovered(r, r.dir, conv)
		if row := r.row(id); row.ProjectID == nil || *row.ProjectID != r.pid || row.Resume != "history" {
			t.Fatalf("discovered row inside the project = %+v, want its project and resume history", row)
		}
		resp := r.resume(id, htSize)
		var got struct {
			TerminalID string `json:"terminalId"`
			Supersedes string `json:"supersedes"`
		}
		resp.JSON(t, &got)
		if resp.Status != http.StatusCreated || got.TerminalID == "" || got.Supersedes != id {
			t.Fatalf("resume answered %d %+v, want 201 with a terminal superseding %s: %s", resp.Status, got, id, resp.Body)
		}
		r.resumeStart(got.TerminalID, conv)
		after := r.rows()
		if after.byID(id) != nil || after.countConv(conv) != 1 {
			t.Fatalf("rows after the resume: discovered still listed %v, %d rows carry the conversation, want gone and 1", after.byID(id) != nil, after.countConv(conv))
		}
		if rec := r.record(got.TerminalID); rec == nil || rec["conversation_id"] != conv || rec["kind"] != "pty" || rec["project_id"] != r.pid {
			t.Fatalf("new ledger record = %v, want a pty record of project %s carrying %s", rec, r.pid, conv)
		}
		if rec := r.record(id); rec != nil {
			t.Fatalf("the discovered ledger record survived the resume: %v", rec)
		}
	})

	t.Run("outside every project", func(t *testing.T) {
		t.Parallel()
		r := htStart(t, false)
		conv := hxConv(52)
		id, _ := discovered(r, osDir(t, t.TempDir(), "elsewhere"), conv)
		if row := r.row(id); row.Resume != "none" || hxStr(row.ResumeReason) != "no_project" {
			t.Fatalf("row outside every project = %+v, want resume none/no_project", row)
		}
		r.refused(id, htSize, http.StatusConflict, "no_project")
		if row := r.row(id); row == nil || row.Record != "discovered" {
			t.Fatalf("row after the refusal = %+v, want the discovered record kept", row)
		}
	})

	t.Run("a forged cwd", func(t *testing.T) {
		t.Parallel()
		r := htStart(t, false)
		conv := hxConv(53)
		id, path := discovered(r, r.dir, conv)
		if row := r.row(id); row.Resume != "history" {
			t.Fatalf("row before the forgery = %+v, want resume history", row)
		}
		// The file stays where a project's folder is, but its records now say
		// another folder: no record maps back to this file. No scan runs, so only
		// a fresh read of the transcript can see it.
		lie := osDir(t, t.TempDir(), "forged")
		end := hxBase.Add(3 * time.Hour)
		osPlantAt(t, path, []string{hxUser(conv, lie, hxBase, "acme forged", nil), hxEnd(conv, lie, end)}, end)
		r.refused(id, htSize, http.StatusConflict, "no_project")
	})

	t.Run("an absent transcript", func(t *testing.T) {
		t.Parallel()
		r := htStart(t, false)
		conv := hxConv(54)
		id, path := discovered(r, r.dir, conv)
		if err := os.Remove(path); err != nil {
			t.Fatalf("removing %s: %v", path, err)
		}
		r.refused(id, htSize, http.StatusConflict, "transcript_absent")
	})

	t.Run("held by a live process", func(t *testing.T) {
		t.Parallel()
		r := htStart(t, false)
		conv := hxConv(55)
		dir := osDir(t, r.dir, "live")
		live := adSpawn(t, dir, conv)
		r.transcript(dir, conv, "Acme held")
		live.register(r.i.Home, "idle", time.Now())
		r.probe()
		r.scan(nil)
		id := hxDiscoveredID(conv)
		if row := r.row(id); row == nil || row.Record != "discovered" || row.Resume != "none" || hxStr(row.ResumeReason) != "held" {
			t.Fatalf("row of a conversation a live process holds = %+v, want resume none/held", row)
		}
		r.refused(id, htSize, http.StatusConflict, "held")
		live.alive()

		// Control: with the process gone, the same door starts the terminal.
		live.terminate()
		if err := os.Remove(r.registryPath(live.pid)); err != nil {
			t.Fatalf("removing the registry entry: %v", err)
		}
		r.probe()
		resp := r.resume(id, htSize)
		if resp.Status != http.StatusCreated {
			t.Fatalf("resume after the process ended answered %d, want 201: %s", resp.Status, resp.Body)
		}
	})

	t.Run("held by relay-sessions", func(t *testing.T) {
		t.Parallel()
		r := htStart(t, false)
		// The conversation belongs to a managed Claude session whose agent ended
		// while relay was down. The outside probe's held set includes every managed
		// id, so the door refuses through that set: the door's own managed-list
		// check cannot be isolated, because every probe, and the adopt that starts
		// a managed session on a chosen conversation, refreshes the held set.
		resp := r.post("/api/sessions", map[string]any{"projectId": r.pid, "model": "sonnet"})
		var made struct {
			SessionID string `json:"sessionId"`
		}
		resp.JSON(t, &made)
		if resp.Status != http.StatusCreated || made.SessionID == "" {
			t.Fatalf("POST /api/sessions answered %d, want 201 and a session id: %s", resp.Status, resp.Body)
		}
		agent := made.SessionID
		g1sSendText(t, r.i, agent, "hi", "echo: hi")
		conv := ""
		for _, c := range r.i.FakeAgentCalls("claude") {
			if c.Event == "start" && c.Env["RELAY_SESSION_ID"] == agent {
				conv = g1xArgv(c.Argv, "--session-id")
			}
		}
		if conv == "" {
			t.Fatalf("the session's start names no --session-id")
		}
		g1lEndAgentWhileDown(t, r.i, agent)
		since := time.Now()
		r.i.Restart()
		r.i.WaitSessionHost(osBound)
		r.i.WaitEvent(harness.EventQuery{Key: "session.history.scan", Since: since, Fields: map[string]any{"machine": "console"}}, osBound)

		tid := r.start("claude-code")
		pid := r.agentStart(tid).Pid
		r.transcript(r.dir, conv, "Acme managed")
		r.link(tid, pid, conv)
		r.closeTerm(tid)
		if err := os.Remove(r.registryPath(pid)); err != nil {
			t.Fatalf("removing the registry entry: %v", err)
		}
		r.probe()
		r.scan(nil)
		r.refused(tid, htSize, http.StatusConflict, "held")
	})
}

// POST /api/sessions/{id}/resume refuses a terminal record and a discovered
// record, and starts nothing.
func TestSessionResumeRefusesHistoryRecords(t *testing.T) {
	t.Parallel()
	r := htStart(t, false)
	tid, _ := r.dormant(hxConv(61), "Acme terminal")
	found := hxConv(62)
	r.transcript(osDir(t, t.TempDir(), "found"), found, "Acme found")
	r.scan(nil)
	did := hxDiscoveredID(found)
	if row := r.row(did); row == nil || row.Record != "discovered" {
		t.Fatalf("row %s = %+v, want a discovered row", did, row)
	}

	live := r.start("claude-code")
	for name, id := range map[string]string{"terminal record": tid, "discovered record": did, "live terminal record": live} {
		before := r.terminals()
		resp := r.post("/api/sessions/"+id+"/resume", nil)
		if resp.Status != http.StatusForbidden {
			t.Fatalf("%s: POST /api/sessions/%s/resume answered %d, want 403: %s", name, id, resp.Status, resp.Body)
		}
		r.i.WaitEvent(harness.EventQuery{Key: "session.resume",
			Fields: map[string]any{"status": "denied", "reason": "session_not_resumable", "session_id": id}}, osBound)
		if after := r.terminals(); len(after) != len(before) {
			t.Fatalf("%s: the refused resume changed the terminals from %v to %v", name, before, after)
		}
		if row := r.row(id); row == nil || (id != live && row.State == "live") {
			t.Fatalf("%s: row after the refusal = %+v, want it kept in its state", name, row)
		}
	}
	if rec := r.record(tid); rec == nil || rec["state"] != "dormant" {
		t.Fatalf("terminal record after the refusal = %v, want dormant", rec)
	}
	if rec := r.record(live); rec == nil || rec["state"] != "live" {
		t.Fatalf("live terminal record after the refusal = %v, want live", rec)
	}
	denied := 0
	for _, a := range r.i.Audit(harness.AuditQuery{Event: "session_resume", Tail: 100000}) {
		switch a["outcome"] {
		case "ok":
			t.Fatalf("a refused resume wrote an ok session_resume row: %v", a)
		case "denied":
			denied++
		}
	}
	if denied < 3 {
		t.Fatalf("%d denied session_resume rows, want one per refusal (3)", denied)
	}
	// The history door is the way back.
	if resp := r.resume(tid, htSize); resp.Status != http.StatusCreated {
		t.Fatalf("history resume of the terminal record answered %d, want 201: %s", resp.Status, resp.Body)
	}
}

// The Chief of Staff scope, a read-only credential and the TCP listener cannot
// reach the resume door.
func TestHistoryResumeScopeRefused(t *testing.T) {
	t.Parallel()
	r := htStart(t, false)
	tid, _ := r.dormant(hxConv(71), "Acme scoped")
	if row := r.row(tid); row == nil || row.Resume != "history" {
		t.Fatalf("row before the refusals = %+v, want resume history", row)
	}
	path := hxPath + "/" + tid + "/resume"
	before := r.terminals()

	if resp := cosDo(t, r.i, "POST", path, htSize); resp.Status != http.StatusForbidden {
		t.Fatalf("chief-of-staff scope answered %d, want 403: %s", resp.Status, resp.Body)
	}
	if resp := r.i.SocketHTTP(r.i.Credential("reader")).Do("POST", path, htSize); resp.Status != http.StatusForbidden {
		t.Fatalf("read-class credential answered %d, want 403: %s", resp.Status, resp.Body)
	}
	if resp := r.i.HTTP(r.i.Credential("cos")).Do("POST", path, htSize); resp.Status != http.StatusNotFound {
		t.Fatalf("TCP listener answered %d, want 404: the route is absent there: %s", resp.Status, resp.Body)
	}
	if got := r.i.Events(harness.EventQuery{Key: "session.history.resume"}); len(got) != 0 {
		t.Fatalf("%d session.history.resume lines after refused doors, want none", len(got))
	}
	if after := r.terminals(); len(after) != len(before) {
		t.Fatalf("the refused doors changed the terminals from %v to %v", before, after)
	}
	if row := r.row(tid); row == nil || row.State != "dormant" {
		t.Fatalf("row after the refusals = %+v, want the dormant record kept", row)
	}

	// Control: the same call from the owner's socket works.
	if resp := r.resume(tid, htSize); resp.Status != http.StatusCreated {
		t.Fatalf("the unscoped call answered %d, want 201: %s", resp.Status, resp.Body)
	}
}
