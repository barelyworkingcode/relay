package features

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"relaye2e/harness"
)

// These tests attach Relay to a thread of a fake codex daemon: a WebSocket
// server on the control socket under the instance's own HOME. The fake logs
// every upgrade path, request, reply and response as one stdout line, so a
// test reads exactly what Relay asked the daemon. Its -client mode stands in
// for the terminal UI that owns the thread.

// ccReadOnly are the methods that only observe the daemon. A refusal may use
// them; anything else is a change Relay asked of the daemon.
var ccReadOnly = []string{"initialize", "initialized", "thread/loaded/list", "thread/read", "thread/turns/list"}

// ccAllowed is the daemon method allowlist: nothing else may ever be sent.
var ccAllowed = append(slices.Clone(ccReadOnly), "thread/resume", "turn/start", "turn/interrupt", "thread/unsubscribe")

var ccOutsideID = regexp.MustCompile(`^outside:[0-9a-f]{32}$`)

// ccProc is a fake daemon or terminal-UI process. A reader drains its stdout
// to EOF before Wait runs, so the last line is never lost.
type ccProc struct {
	t       *testing.T
	cmd     *exec.Cmd
	in      io.WriteCloser
	exit    chan struct{}
	tag     int
	mu      sync.Mutex
	lines   []map[string]any
	changed chan struct{}
	done    bool
}

func ccLaunch(t *testing.T, bin string, args ...string) *ccProc {
	t.Helper()
	cmd := exec.Command(bin, args...)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &ccProc{t: t, cmd: cmd, in: in, exit: make(chan struct{}), changed: make(chan struct{})}
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 1<<16), 1<<22)
		for sc.Scan() {
			var v map[string]any
			if json.Unmarshal(sc.Bytes(), &v) == nil {
				p.push(v, false)
			}
		}
		p.push(nil, true)
		_ = cmd.Wait()
		close(p.exit)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-p.exit
		_ = in.Close()
	})
	return p
}

func (p *ccProc) push(v map[string]any, done bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v != nil {
		p.lines = append(p.lines, v)
	}
	p.done = p.done || done
	close(p.changed)
	p.changed = make(chan struct{})
}

// until returns the first line, old or new, that pred accepts. It wakes on a
// new line or on the process ending, and fails at osBound.
func (p *ccProc) until(what string, pred func(map[string]any) bool) map[string]any {
	p.t.Helper()
	deadline := time.NewTimer(osBound)
	defer deadline.Stop()
	for next := 0; ; {
		p.mu.Lock()
		for ; next < len(p.lines); next++ {
			if pred(p.lines[next]) {
				l := p.lines[next]
				p.mu.Unlock()
				return l
			}
		}
		ch, done := p.changed, p.done
		p.mu.Unlock()
		if done {
			p.t.Fatalf("the fake process ended before %s", what)
		}
		select {
		case <-ch:
		case <-deadline.C:
			p.t.Fatalf("no %s within %v", what, osBound)
		}
	}
}

func (p *ccProc) snapshot() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.lines)
}

// alive reports whether the process is still running.
func (p *ccProc) alive() bool {
	select {
	case <-p.exit:
		return false
	default:
		return true
	}
}

func (p *ccProc) kill() {
	_ = p.cmd.Process.Kill()
	<-p.exit
}

// do sends one stdin command and returns its ack. The ack is the signal that
// the command took effect, notifications included.
func (p *ccProc) do(cmd map[string]any) map[string]any {
	p.t.Helper()
	p.tag++
	tag := float64(p.tag)
	cmd["tag"] = tag
	b, err := json.Marshal(cmd)
	if err != nil {
		p.t.Fatal(err)
	}
	if _, err = fmt.Fprintln(p.in, string(b)); err != nil {
		p.t.Fatal(err)
	}
	ack := p.until("ack of "+fmt.Sprint(cmd["cmd"]), func(l map[string]any) bool { return l["event"] == "ack" && l["tag"] == tag })
	if ack["ok"] != true {
		p.t.Fatalf("command %v failed: %v", cmd, ack)
	}
	return ack
}

// barrier returns once every line written before it is in the log: the
// refusal window of a request that already answered is closed.
func (p *ccProc) barrier() { p.t.Helper(); p.do(map[string]any{"cmd": "barrier"}) }

// relay returns the requests the Relay connections sent, in order.
func (p *ccProc) relay() []map[string]any {
	var out []map[string]any
	for _, l := range p.snapshot() {
		if l["event"] == "request" && l["client"] == "relay" {
			out = append(out, l)
		}
	}
	return out
}

func ccMethods(ls []map[string]any) []string {
	var out []string
	for _, l := range ls {
		out = append(out, fmt.Sprint(l["method"]))
	}
	return out
}

// mutations are the methods Relay sent that change or drive the daemon.
func (p *ccProc) mutations() []string {
	var out []string
	for _, m := range ccMethods(p.relay()) {
		if !slices.Contains(ccReadOnly, m) {
			out = append(out, m)
		}
	}
	return out
}

func (p *ccProc) relayCalls(method string) []map[string]any {
	var out []map[string]any
	for _, l := range p.relay() {
		if l["method"] == method {
			out = append(out, l)
		}
	}
	return out
}

func ccRelayReq(method string) func(map[string]any) bool {
	return func(l map[string]any) bool {
		return l["event"] == "request" && l["client"] == "relay" && l["method"] == method
	}
}

func ccEvent(name string) func(map[string]any) bool {
	return func(l map[string]any) bool { return l["event"] == name }
}

func ccParams(l map[string]any) map[string]any { p, _ := l["params"].(map[string]any); return p }

func ccKeys(m map[string]any) []string {
	var k []string
	for key := range m {
		k = append(k, key)
	}
	sort.Strings(k)
	return k
}

func ccParamText(l map[string]any) string {
	in, _ := ccParams(l)["input"].([]any)
	if len(in) == 0 {
		return ""
	}
	item, _ := in[0].(map[string]any)
	s, _ := item["text"].(string)
	return s
}

func ccTime(t *testing.T, v any) time.Time {
	t.Helper()
	s, _ := v.(string)
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("timestamp %v: %v", v, err)
	}
	return at
}

// ccThread is a thread the fake daemon holds.
type ccThread struct {
	ID, Dir, Model, Source, Parent, Preview, Status string
	Turns                                           []string
	Ephemeral                                       bool
	Created                                         int64
}

type ccRig struct {
	*osRig
	root, project, bin string
}

func ccStart(t *testing.T, mut ...func(*harness.Options)) *ccRig {
	t.Helper()
	r := osStart(t, false, append([]func(*harness.Options){func(o *harness.Options) {
		if o.Settings == nil {
			o.Settings = map[string]json.RawMessage{}
		}
		o.Settings["audit"] = json.RawMessage(`{"enabled":true}`)
	}}, mut...)...)
	p := g1tProject(t, r.i, "acme-codex", map[string]any{"allowed_templates": []string{"codex"}})
	root, err := filepath.EvalSymlinks(g1tDir(r.i, "acme-codex"))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "fakecodexdaemon")
	build := exec.Command("go", "build", "-o", bin, "./fakes/fakecodexdaemon")
	build.Dir = filepath.Join(harness.RepoRoot(), "e2e")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake codex daemon: %v\n%s", err, out)
	}
	return &ccRig{osRig: r, root: root, project: p.ID, bin: bin}
}

// daemon starts a fake daemon on the instance's own home and returns once it
// listens.
func (r *ccRig) daemon(extra ...string) *ccProc {
	r.t.Helper()
	d := ccLaunch(r.t, r.bin, append([]string{"-home", r.i.Home}, extra...)...)
	d.until("the fake daemon ready line", ccEvent("ready"))
	return d
}

// terminal starts the stand-in terminal UI on a thread and returns once it
// has subscribed.
func (r *ccRig) terminal(th ccThread) *ccProc {
	r.t.Helper()
	c := ccLaunch(r.t, r.bin, "-client", "-home", r.i.Home, "-thread", th.ID)
	c.until("the terminal UI subscription", ccEvent("ready"))
	return c
}

// thread is an idle thread with one finished turn in a new folder of the
// registered project.
func (r *ccRig) thread(name string) ccThread {
	return ccThread{ID: adUUID(), Dir: osDir(r.t, r.root, name), Model: "fake-echo", Turns: []string{"completed"},
		Preview: "Acme private preview", Created: time.Now().Add(-time.Hour).Unix()}
}

func (d *ccProc) add(th ccThread) {
	d.t.Helper()
	d.do(map[string]any{"cmd": "add_thread", "id": th.ID, "cwd": th.Dir, "model": th.Model, "source": th.Source,
		"parentThreadId": th.Parent, "preview": th.Preview, "ephemeral": th.Ephemeral, "created": th.Created,
		"turns": th.Turns, "status": th.Status})
}

func (r *ccRig) snapshot() ([]map[string]any, []byte) {
	r.t.Helper()
	_, raw := r.probeRaw()
	var s struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		r.t.Fatal(err)
	}
	return s.Sessions, raw
}

// row probes and returns the codex row of the thread, or nil.
func (r *ccRig) row(th ccThread) map[string]any {
	r.t.Helper()
	rows, _ := r.snapshot()
	for _, row := range rows {
		if row["agent"] == "codex" && row["conversationId"] == th.ID {
			return row
		}
	}
	return nil
}

func (r *ccRig) mustRow(th ccThread) map[string]any {
	r.t.Helper()
	row := r.row(th)
	if row == nil {
		r.t.Fatalf("no codex row for thread %s", th.ID)
	}
	return row
}

func (r *ccRig) attach(id string) harness.Response {
	r.t.Helper()
	return r.adopt(map[string]any{"outsideId": id})
}

// attached adopts the thread through its row and returns the managed session.
func (r *ccRig) attached(th ccThread) string {
	r.t.Helper()
	row := r.mustRow(th)
	if row["adoptable"] != true {
		r.t.Fatalf("idle thread row not adoptable: %v", row)
	}
	resp := r.attach(row["id"].(string))
	var v struct {
		SessionID, Agent, ConversationID string
		Interrupted, RecoverySent        bool
	}
	resp.JSON(r.t, &v)
	if resp.Status != http.StatusCreated || v.SessionID == "" || v.Agent != "codex" || v.ConversationID != th.ID || v.Interrupted || v.RecoverySent {
		r.t.Fatalf("attach answered %d %s", resp.Status, resp.Body)
	}
	return v.SessionID
}

func (r *ccRig) join(sid string) *harness.WSConn {
	r.t.Helper()
	ws := r.i.WebSocket("/ws", r.i.Credential("cos"))
	r.t.Cleanup(ws.Close)
	ws.Send(map[string]any{"type": "join_session", "sessionId": sid})
	g1aNext(r.t, ws, g1tFrameDeadline, g1aIs("session_joined", sid))
	return ws
}

func ccState(sid, state string) func(map[string]any) bool {
	return func(v map[string]any) bool {
		return v["type"] == "session_state" && v["sessionId"] == sid && v["state"] == state
	}
}

// hold starts a turn that stays open until the daemon is told to end it, and
// returns once the daemon has the turn.
func (r *ccRig) hold(d *ccProc, ws *harness.WSConn, sid string) {
	r.t.Helper()
	ws.Send(map[string]any{"type": "send_message", "sessionId": sid, "text": "!hold"})
	g1aNext(r.t, ws, g1tFrameDeadline, ccState(sid, "running"))
	d.until("the held turn at the daemon", func(l map[string]any) bool {
		return ccRelayReq("turn/start")(l) && ccParamText(l) == "!hold"
	})
}

func (r *ccRig) stop(ws *harness.WSConn, sid string) {
	ws.Send(map[string]any{"type": "stop_generation", "sessionId": sid})
}

func (r *ccRig) noSessions() {
	r.t.Helper()
	if got := adSessions(r.t, r.i); len(got) != 0 {
		r.t.Fatalf("a refused attach created sessions: %v", got)
	}
}

// untouched closes the refusal window and fails if Relay asked the daemon for
// anything beyond reads, or left a session behind.
func (r *ccRig) untouched(d *ccProc) {
	r.t.Helper()
	d.barrier()
	if m := d.mutations(); len(m) != 0 {
		r.t.Fatalf("Relay sent the daemon %v after a refusal", m)
	}
	r.noSessions()
}

func ccNoCodexProgram(t *testing.T, i *harness.Instance) {
	t.Helper()
	for _, c := range i.FakeAgentCalls("codex") {
		if c.Event != "start" {
			continue
		}
		for _, word := range []string{"app-server", "daemon", "stop", "restart", "update"} {
			if slices.Contains(c.Argv, word) {
				t.Fatalf("Relay ran a codex program: %v", c.Argv)
			}
		}
	}
}

func (r *ccRig) restart() {
	r.t.Helper()
	since := time.Now()
	r.i.Restart()
	r.i.WaitEvent(harness.EventQuery{Key: "service.manifest.register", Since: since,
		Fields: map[string]any{"service_id": "relaysessions", "status": "ok"}}, g1sHostDeadline)
	r.i.WaitSessionHost(g1sHostDeadline)
}

func ccArg(row map[string]any, key string) any { a, _ := row["args"].(map[string]any); return a[key] }

func TestCodexAttachRowListsThread(t *testing.T) {
	t.Parallel()
	r := ccStart(t)
	d := r.daemon()
	idle := r.thread("listed")
	busy := r.thread("listed-busy")
	busy.Turns = []string{"completed", "inProgress"}
	d.add(idle)
	d.add(busy)
	rows, raw := r.snapshot()
	find := func(th ccThread) map[string]any {
		for _, row := range rows {
			if row["agent"] == "codex" && row["conversationId"] == th.ID {
				return row
			}
		}
		t.Fatalf("no codex row for %s in %s", th.ID, raw)
		return nil
	}
	row := find(idle)
	if id, _ := row["id"].(string); !ccOutsideID.MatchString(id) {
		t.Fatalf("invalid outside id: %v", row)
	}
	if row["outside"] != true || row["readOnly"] != true || row["viewReason"] != "not_in_tmux" || row["adoptable"] != true {
		t.Fatalf("codex row flags: %v", row)
	}
	if row["state"] != "idle" || row["directory"] != idle.Dir || row["projectId"] != r.project {
		t.Fatalf("codex row identity: %v", row)
	}
	if got := ccTime(t, row["since"]); !got.Equal(time.Unix(idle.Created, 0)) {
		t.Fatalf("since = %v, want the thread's createdAt", got)
	}
	adNoKeys(t, "codex row", row, "claudeSessionId", "pid", "procStart", "argv", "environment", "preview")
	if strings.Contains(string(raw), idle.Preview) {
		t.Fatalf("the snapshot carries thread text: %s", raw)
	}
	running := find(busy)
	if running["state"] != "running" || running["adoptable"] != false || running["adoptReason"] != "busy" {
		t.Fatalf("busy codex row: %v", running)
	}
	detail := r.detail(row["id"].(string))
	if detail.Status != http.StatusOK {
		t.Fatalf("detail answered %d: %s", detail.Status, detail.Body)
	}
	var dv struct {
		LastTurn struct {
			Text string `json:"text"`
		} `json:"lastTurn"`
	}
	detail.JSON(t, &dv)
	if dv.LastTurn.Text != "" || strings.Contains(string(detail.Body), idle.Preview) {
		t.Fatalf("detail holds thread text: %s", detail.Body)
	}
	if page := r.transcriptPage(row["id"].(string), ""); len(page.Entries) != 0 {
		t.Fatalf("codex transcript page is not empty: %+v", page)
	}
}

func TestCodexAttachNoTurnNoRow(t *testing.T) {
	t.Parallel()
	r := ccStart(t)
	d := r.daemon()
	empty := r.thread("empty")
	empty.Turns = nil
	control := r.thread("control")
	d.add(empty)
	d.add(control)
	// The control row proves discovery ran after both threads were loaded.
	r.mustRow(control)
	if row := r.row(empty); row != nil {
		t.Fatalf("a thread with no turn is listed: %v", row)
	}
	// Positive control: the same thread lists once it has a finished turn.
	d.do(map[string]any{"cmd": "start_turn", "id": empty.ID})
	d.do(map[string]any{"cmd": "complete_turn", "id": empty.ID})
	if row := r.row(empty); row == nil {
		t.Fatal("the thread is not listed after its first turn")
	}
}

func TestCodexAttachIdleNoKill(t *testing.T) {
	t.Parallel()
	r := ccStart(t)
	d := r.daemon()
	th := r.thread("attach")
	d.add(th)
	ui := r.terminal(th)
	sid := r.attached(th)
	g1sSendText(t, r.i, sid, "Acme from the phone", "echo: Acme from the phone")
	ui.until("the terminal UI sees the Relay turn", func(l map[string]any) bool {
		item, _ := ccParams(l)["item"].(map[string]any)
		return l["method"] == "item/completed" && item["text"] == "echo: Acme from the phone"
	})
	if !d.alive() || !ui.alive() {
		t.Fatal("attach ended the daemon or the terminal UI")
	}
	for _, l := range ui.snapshot() {
		if l["event"] == "closed" {
			t.Fatal("the terminal UI lost its connection")
		}
	}
	if got := ccMethods(d.relayCalls("thread/resume")); len(got) != 1 {
		t.Fatalf("Relay sent %d thread/resume requests, want one", len(got))
	}
	if row := r.row(th); row != nil {
		t.Fatalf("the attached thread is still listed outside: %v", row)
	}
	list, _ := g1sListSessions(t, r.i)
	if s, ok := g1sFindSession(list, sid); !ok || !s.Live {
		t.Fatalf("attached session not live: %+v", s)
	}
}

func ccForbiddenKeys(t *testing.T, where string, v any) {
	t.Helper()
	forbidden := []string{"sandbox", "sandboxPolicy", "approvalPolicy", "approvalsReviewer", "model", "modelProvider", "cwd",
		"baseInstructions", "developerInstructions", "instructions", "personality", "effort", "summary", "config",
		"serviceTier", "permissions", "capabilities", "collaborationMode", "outputSchema"}
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if slices.Contains(forbidden, k) {
				t.Fatalf("%s carries the override %q", where, k)
			}
			ccForbiddenKeys(t, where, child)
		}
	case []any:
		for _, child := range x {
			ccForbiddenKeys(t, where, child)
		}
	}
}

func TestCodexAttachSendsNoOverride(t *testing.T) {
	t.Parallel()
	r := ccStart(t)
	d := r.daemon()
	th := r.thread("overrides")
	d.add(th)
	sid := r.attached(th)
	g1sSendText(t, r.i, sid, "Acme plain turn", "echo: Acme plain turn")
	d.barrier()
	want := map[string][]string{
		"initialize":         {"clientInfo"},
		"thread/read":        {"threadId"},
		"thread/turns/list":  {"limit", "sortDirection", "threadId"},
		"thread/resume":      {"threadId"},
		"turn/start":         {"input", "threadId"},
		"thread/unsubscribe": {"threadId"},
	}
	seen := map[string]bool{}
	for _, l := range d.relay() {
		m := fmt.Sprint(l["method"])
		seen[m] = true
		ccForbiddenKeys(t, m, l["params"])
		if keys, ok := want[m]; ok && !slices.Equal(ccKeys(ccParams(l)), keys) {
			t.Fatalf("%s params keys %v, want %v", m, ccKeys(ccParams(l)), keys)
		}
		if m == "thread/loaded/list" {
			for _, k := range ccKeys(ccParams(l)) {
				if k != "cursor" && k != "limit" {
					t.Fatalf("thread/loaded/list carries %q", k)
				}
			}
		}
		if m == "initialize" {
			info, _ := ccParams(l)["clientInfo"].(map[string]any)
			if !slices.Equal(ccKeys(info), []string{"name", "version"}) || info["name"] != "relay" {
				t.Fatalf("initialize clientInfo: %v", info)
			}
		}
		if m == "thread/turns/list" && (ccParams(l)["limit"] != float64(1) || ccParams(l)["sortDirection"] != "desc") {
			t.Fatalf("thread/turns/list params: %v", ccParams(l))
		}
	}
	for _, m := range []string{"initialize", "thread/resume", "turn/start"} {
		if !seen[m] {
			t.Fatalf("Relay never sent %s; log: %v", m, ccMethods(d.relay()))
		}
	}
	// The resume asked for nothing, so the daemon kept the thread's own policy.
	if ack := d.do(map[string]any{"cmd": "status", "id": th.ID}); ack["cwd"] != th.Dir {
		t.Fatalf("the thread's folder changed: %v", ack)
	}
}

func TestCodexAttachNeverStopsDaemon(t *testing.T) {
	t.Parallel()
	r := ccStart(t)
	d := r.daemon()
	th := r.thread("never-stop")
	d.add(th)
	sid := r.attached(th)
	g1sSendText(t, r.i, sid, "Acme first", "echo: Acme first")
	ws := r.join(sid)
	r.hold(d, ws, sid)
	r.stop(ws, sid)
	d.until("the interrupt at the daemon", ccRelayReq("turn/interrupt"))
	g1aNext(t, ws, g1tFrameDeadline, ccState(sid, "idle"))
	// The daemon closes the thread: the session reads dormant, and a resume is
	// a new attach to the same daemon.
	d.do(map[string]any{"cmd": "close_thread", "id": th.ID})
	r.i.WaitEvent(harness.EventQuery{Key: "session.exited", Fields: map[string]any{"session_id": sid}}, g1sHostDeadline)
	d.add(th)
	r.i.MustCLI("session", "resume", "--id", sid, "--json")
	g1sSendText(t, r.i, sid, "Acme again", "echo: Acme again")
	resp := r.i.SocketHTTP(r.i.Credential("cos")).Do("DELETE", "/api/sessions/"+sid, nil)
	if resp.Status != http.StatusNoContent {
		t.Fatalf("delete answered %d: %s", resp.Status, resp.Body)
	}
	d.until("the detach at the daemon", ccRelayReq("thread/unsubscribe"))
	d.barrier()
	for _, l := range d.snapshot() {
		if l["event"] == "upgrade" && l["path"] != "/" {
			t.Fatalf("a WebSocket upgrade on %v", l["path"])
		}
		if l["event"] == "upgrade_refused" {
			t.Fatalf("an upgrade the daemon refused: %v", l)
		}
	}
	for _, m := range ccMethods(d.relay()) {
		if !slices.Contains(ccAllowed, m) {
			t.Fatalf("Relay sent %q, which is not an allowed daemon method", m)
		}
	}
	ccNoCodexProgram(t, r.i)
	if !d.alive() {
		t.Fatal("the daemon ended")
	}
}

func TestCodexAttachBusyRefused(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"turn-in-progress", "status-active"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			r := ccStart(t)
			d := r.daemon()
			th := r.thread("busy")
			if kind == "turn-in-progress" {
				th.Turns = []string{"completed", "inProgress"}
			} else {
				th.Status = "active"
			}
			d.add(th)
			row := r.mustRow(th)
			if row["adoptable"] == true || row["adoptReason"] != "busy" {
				t.Fatalf("busy thread row: %v", row)
			}
			id := row["id"].(string)
			for _, interrupt := range []bool{false, true} {
				otRefusal(t, r.adopt(map[string]any{"outsideId": id, "interrupt": interrupt}), http.StatusConflict, "busy")
				r.untouched(d)
			}
			// Positive control: the same thread attaches once it is idle.
			if kind == "turn-in-progress" {
				d.do(map[string]any{"cmd": "complete_turn", "id": th.ID})
			} else {
				d.do(map[string]any{"cmd": "set_status", "id": th.ID, "status": "idle"})
			}
			if row := r.mustRow(th); row["adoptable"] != true {
				t.Fatalf("idle again, still not adoptable: %v", row)
			}
		})
	}
}

func TestCodexAttachChangedOrAmbiguousRefused(t *testing.T) {
	t.Parallel()
	// Threads that are never adoptable: a subagent, an ephemeral thread, one not
	// from the command line, an id that is not a UUID.
	t.Run("not-adoptable", func(t *testing.T) {
		t.Parallel()
		ccNotAdoptable(t)
	})
	for _, kind := range []string{"cwd", "closed", "ambiguous", "restarted-daemon"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			r := ccStart(t)
			d := r.daemon()
			th := r.thread("changed")
			d.add(th)
			id := r.mustRow(th)["id"].(string)
			status, codes := http.StatusConflict, []string{kind}
			switch kind {
			case "cwd":
				d.do(map[string]any{"cmd": "set_cwd", "id": th.ID, "cwd": osDir(t, r.root, "elsewhere")})
				codes = []string{"changed"}
			case "closed":
				d.do(map[string]any{"cmd": "close_thread", "id": th.ID})
				codes = []string{"ended"}
			case "ambiguous":
				d.do(map[string]any{"cmd": "dup", "id": th.ID})
			case "restarted-daemon":
				d.kill()
				d = r.daemon()
				d.add(th)
				status, codes = 0, []string{"changed", "ended", "outside_session_not_found"}
			}
			resp := r.attach(id)
			var e struct{ Error string }
			resp.JSON(t, &e)
			if (status != 0 && resp.Status != status) || resp.Status == http.StatusCreated || !slices.Contains(codes, e.Error) {
				t.Fatalf("attach answered %d %q, want one of %v", resp.Status, e.Error, codes)
			}
			r.untouched(d)
		})
	}
}

func ccNotAdoptable(t *testing.T) {
	r := ccStart(t)
	d := r.daemon()
	control := r.thread("control")
	cases := map[string]ccThread{}
	sub := r.thread("subagent")
	sub.Parent = adUUID()
	cases["subagent"] = sub
	eph := r.thread("ephemeral")
	eph.Ephemeral = true
	cases["ephemeral"] = eph
	ide := r.thread("not-cli")
	ide.Source = "vscode"
	cases["not-cli"] = ide
	bad := r.thread("bad-id")
	bad.ID = "not-a-uuid"
	cases["bad-id"] = bad
	d.add(control)
	for _, th := range cases {
		d.add(th)
	}
	rows, _ := r.snapshot()
	listed := map[string]map[string]any{}
	for _, row := range rows {
		if row["agent"] == "codex" {
			listed[fmt.Sprint(row["conversationId"])] = row
		}
	}
	if listed[control.ID]["adoptable"] != true {
		t.Fatalf("the control thread is not adoptable: %v", listed)
	}
	for name, th := range cases {
		if row, ok := listed[th.ID]; ok {
			if row["adoptable"] == true {
				t.Fatalf("%s thread is adoptable: %v", name, row)
			}
			if resp := r.attach(row["id"].(string)); resp.Status == http.StatusCreated {
				t.Fatalf("%s thread attached: %s", name, resp.Body)
			}
		}
	}
	r.untouched(d)
}

func TestCodexAttachManagedRefused(t *testing.T) {
	t.Parallel()
	r := ccStart(t)
	d := r.daemon()
	th := r.thread("managed")
	control := r.thread("managed-control")
	d.add(th)
	d.add(control)
	first := r.mustRow(th)["id"].(string)
	r.attached(th)
	if row := r.row(th); row != nil {
		t.Fatalf("a managed thread is listed outside: %v", row)
	}
	if r.row(control) == nil {
		t.Fatal("the control thread vanished with the managed one")
	}
	resp := r.attach(first)
	var e struct{ Error string }
	resp.JSON(t, &e)
	if resp.Status == http.StatusCreated || !slices.Contains([]string{"managed", "outside_session_not_found"}, e.Error) {
		t.Fatalf("a second attach answered %d %q", resp.Status, e.Error)
	}
	// The managed identity persists: after a Relay restart the session is
	// dormant and its thread stays off the list.
	r.restart()
	if row := r.row(th); row != nil {
		t.Fatalf("a dormant managed thread is listed outside: %v", row)
	}
	if got := ccMethods(d.relayCalls("thread/resume")); len(got) != 1 {
		t.Fatalf("Relay sent %d thread/resume requests for one attach", len(got))
	}
	if got := adSessions(t, r.i); len(got) != 1 {
		t.Fatalf("sessions after the refusals: %v", got)
	}
}

func TestCodexAttachSocketUnsafeNoRows(t *testing.T) {
	t.Parallel()
	r := ccStart(t)
	d := r.daemon("-unsafe")
	th := r.thread("unsafe")
	d.add(th)
	dir := filepath.Join(r.i.Home, ".codex", "app-server-control")
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o777 {
		t.Fatalf("the socket folder is not world-writable: %v %v", info, err)
	}
	// The Claude row proves the probe itself ran.
	claude := r.spawn(r.i.Home, t.TempDir(), "control")
	claude.transcript(t, osPrompt("acme one"), osAnswer("acme done"))
	snap := r.probe()
	if snap.at(claude.dir) == nil {
		t.Fatalf("the probe lost the Claude row: %+v", snap)
	}
	if snap.at(th.Dir) != nil {
		t.Fatalf("a codex row came from an unsafe socket: %+v", snap)
	}
	d.barrier()
	for _, l := range d.snapshot() {
		if l["event"] == "upgrade" || l["event"] == "request" {
			t.Fatalf("Relay spoke to an unsafe socket: %v", l)
		}
	}
	// Positive control: the same thread lists once the folder is private.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if r.row(th) == nil {
		t.Fatal("the thread is not listed from a safe socket")
	}
}

func TestCodexAttachAuditBeforeAttach(t *testing.T) {
	t.Parallel()
	r := ccStart(t)
	d := r.daemon()
	th := r.thread("audited")
	d.add(th)
	id := r.mustRow(th)["id"].(string)
	sid := r.attached(th)
	receipt := ccTime(t, d.until("the attach at the daemon", ccRelayReq("thread/resume"))["ts"])
	var intent, completion map[string]any
	rows := g4Wait(t, r.i, g4Has(func(row g4Row) bool { return row.str("phase") == "completion" }), "--event", "session_adopt")
	for _, row := range rows {
		if ccArg(row, "conversation_id") != th.ID {
			continue
		}
		if row["phase"] == "intent" {
			intent = row
		} else {
			completion = row
		}
	}
	if intent == nil || completion == nil {
		t.Fatalf("session_adopt rows for the thread: intent %v, completion %v", intent, completion)
	}
	for key, want := range map[string]any{"outside_id": id, "agent": "codex", "conversation_id": th.ID, "project_id": r.project,
		"directory": th.Dir, "interrupt": false, "method": "attach", "relay_sandbox": false, "model": "codex/fake-echo"} {
		if ccArg(intent, key) != want {
			t.Fatalf("intent arg %s = %v, want %v: %v", key, ccArg(intent, key), want, intent)
		}
	}
	if intent["outcome"] != "pending" || completion["outcome"] != "ok" || ccArg(completion, "session_id") != sid {
		t.Fatalf("outcomes: intent %v, completion %v", intent, completion)
	}
	if at := ccTime(t, intent["ts"]); at.After(receipt) {
		t.Fatalf("the adopt intent at %v follows the daemon's receipt at %v", at, receipt)
	}
	launch := ccSessionRow(t, r, "session_launch", sid)
	if ccArg(launch, "sandbox") != false {
		t.Fatalf("the attach's session_launch row says sandbox %v, want false: %v", ccArg(launch, "sandbox"), launch)
	}
	if ccTime(t, completion["ts"]).Before(ccTime(t, intent["ts"])) {
		t.Fatal("the completion precedes its intent")
	}
}

func TestCodexAttachAuditFailureSendsNothing(t *testing.T) {
	t.Parallel()
	r := ccStart(t, paLowAudit)
	d := r.daemon()
	th := r.thread("audit-failure")
	d.add(th)
	id := r.mustRow(th)["id"].(string)
	paBlockAuditWrites(t, &paRig{osRig: r.osRig})
	otRefusal(t, r.attach(id), http.StatusServiceUnavailable, "audit_unavailable")
	r.untouched(d)
}

func TestCodexAttachInterruptAudited(t *testing.T) {
	t.Parallel()
	r := ccStart(t)
	d := r.daemon()
	th := r.thread("interrupt")
	d.add(th)
	sid := r.attached(th)
	ws := r.join(sid)
	r.hold(d, ws, sid)
	r.stop(ws, sid)
	receipt := d.until("the interrupt at the daemon", ccRelayReq("turn/interrupt"))
	g1aNext(t, ws, g1tFrameDeadline, ccState(sid, "idle"))
	params := ccParams(receipt)
	if params["threadId"] != th.ID {
		t.Fatalf("interrupt params: %v", params)
	}
	for _, k := range ccKeys(params) {
		if k != "threadId" && k != "turnId" {
			t.Fatalf("turn/interrupt carries %q", k)
		}
	}
	// A managed interrupt writes an intent row only; it acknowledges persistence.
	rows := g4Wait(t, r.i, g4Has(func(row g4Row) bool {
		return row.str("phase") == "intent" && ccArg(row, "session_id") == sid
	}), "--event", "session_interrupt")
	var intents []map[string]any
	for _, row := range rows {
		if ccArg(row, "session_id") == sid && row["phase"] == "intent" {
			intents = append(intents, row)
		}
	}
	if len(intents) != 1 {
		t.Fatalf("%d session_interrupt intent rows for the session, want one", len(intents))
	}
	if intents[0]["outcome"] != "pending" {
		t.Fatalf("interrupt intent outcome %v, want pending", intents[0]["outcome"])
	}
	for key, want := range map[string]any{"agent": "codex", "method": "turn/interrupt", "project_id": r.project} {
		if ccArg(intents[0], key) != want {
			t.Fatalf("interrupt intent arg %s = %v, want %v", key, ccArg(intents[0], key), want)
		}
	}
	if at := ccTime(t, intents[0]["ts"]); at.After(ccTime(t, receipt["ts"])) {
		t.Fatalf("interrupt intent at %v follows the daemon's receipt", at)
	}
	if n := len(d.relayCalls("turn/interrupt")); n != 1 {
		t.Fatalf("daemon got %d interrupts, want one", n)
	}
}

func TestCodexAttachInterruptAuditFailureKeepsTurn(t *testing.T) {
	t.Parallel()
	r := ccStart(t, paLowAudit)
	d := r.daemon()
	th := r.thread("interrupt-failure")
	d.add(th)
	sid := r.attached(th)
	ws := r.join(sid)
	r.hold(d, ws, sid)
	restore := paBlockAuditWrites(t, &paRig{osRig: r.osRig})
	r.stop(ws, sid)
	frames := g1aNext(t, ws, g1tFrameDeadline, func(v map[string]any) bool { return v["type"] == "error" && v["sessionId"] == sid })
	if got := frames[len(frames)-1]["code"]; got != "audit_unavailable" {
		t.Fatalf("interrupt refusal code %v", got)
	}
	// The coded frame is the completion marker of the refused handler.
	d.barrier()
	if n := len(d.relayCalls("turn/interrupt")); n != 0 {
		t.Fatalf("an audit refusal still sent %d turn/interrupt requests", n)
	}
	if ack := d.do(map[string]any{"cmd": "status", "id": th.ID}); ack["status"] != "active" {
		t.Fatalf("the refused interrupt ended the turn: %v", ack)
	}
	list, _ := g1sListSessions(t, r.i)
	if row, ok := g1sFindSession(list, sid); !ok || row.Attention == nil || row.Attention.State != "running" {
		t.Fatalf("the refused interrupt changed the session: %+v", row)
	}
	// The recorder keeps its first write error: restore, then restart, its
	// lifecycle boundary. The session reads dormant. A resume attaches again
	// only to an idle thread, so the terminal ends the open turn first.
	restore()
	ws.Close()
	r.restart()
	list, _ = g1sListSessions(t, r.i)
	if row, ok := g1sFindSession(list, sid); !ok || row.Live {
		t.Fatalf("session after the restart: listed %v, %+v, want dormant", ok, row)
	}
	d.do(map[string]any{"cmd": "complete_turn", "id": th.ID})
	r.i.MustCLI("session", "resume", "--id", sid, "--json")
	ws = r.join(sid)
	r.hold(d, ws, sid)
	r.stop(ws, sid)
	d.until("the interrupt at the daemon after audit recovery", ccRelayReq("turn/interrupt"))
	g1aNext(t, ws, g1tFrameDeadline, ccState(sid, "idle"))
	if n := len(d.relayCalls("turn/interrupt")); n != 1 {
		t.Fatalf("interrupt after audit recovery reached the daemon %d times, want once", n)
	}
}

func TestCodexAttachStateFollowsStatus(t *testing.T) {
	t.Parallel()
	r := ccStart(t)
	d := r.daemon()
	th := r.thread("status")
	d.add(th)
	ui := r.terminal(th)
	sid := r.attached(th)
	ws := r.join(sid)
	// Only the thread's status changes: no turn/started and no turn/completed
	// reaches Relay, so the state can follow nothing but thread/status/changed.
	d.do(map[string]any{"cmd": "set_status", "id": th.ID, "status": "active"})
	g1aNext(t, ws, g1tFrameDeadline, ccState(sid, "running"))
	ui.until("the terminal UI sees the status", func(l map[string]any) bool {
		status, _ := ccParams(l)["status"].(map[string]any)
		return l["method"] == "thread/status/changed" && status["type"] == "active"
	})
	d.do(map[string]any{"cmd": "set_status", "id": th.ID, "status": "idle"})
	g1aNext(t, ws, g1tFrameDeadline, ccState(sid, "idle"))
	for _, l := range ui.snapshot() {
		if l["method"] == "turn/started" || l["method"] == "turn/completed" {
			t.Fatalf("the test sent a turn notification: %v", l)
		}
	}
	if m := d.mutations(); !slices.Equal(m, []string{"thread/resume"}) {
		t.Fatalf("Relay sent %v, want only the attach", m)
	}
}

func TestCodexAttachApprovalsDeclined(t *testing.T) {
	t.Parallel()
	r := ccStart(t)
	d := r.daemon()
	th := r.thread("approvals")
	d.add(th)
	sid := r.attached(th)
	ws := r.join(sid)
	r.hold(d, ws, sid)
	cases := map[string]string{
		"item/commandExecution/requestApproval": "decision",
		"item/fileChange/requestApproval":       "decision",
		"mcpServer/elicitation/request":         "action",
	}
	for method, field := range cases {
		ack := d.do(map[string]any{"cmd": "approval", "id": th.ID, "method": method})
		if ack["sent"] != float64(1) {
			t.Fatalf("%s reached %v Relay connections, want one", method, ack["sent"])
		}
		rid := ack["rid"]
		resp := d.until("Relay's answer to "+method, func(l map[string]any) bool { return l["event"] == "response" && l["id"] == rid })
		if resp["error"] != nil {
			t.Fatalf("%s got an error reply, want a decline result: %v", method, resp)
		}
		result, _ := resp["result"].(map[string]any)
		if got := fmt.Sprint(result[field]); got != "decline" && got != "denied" {
			t.Fatalf("%s answered %v, want a decline", method, resp)
		}
	}
	for _, l := range d.snapshot() {
		if l["event"] == "response" {
			for _, approve := range []string{"accept", "acceptForSession", "approved", "approved_for_session"} {
				if strings.Contains(fmt.Sprint(l["result"]), approve) {
					t.Fatalf("Relay approved a request: %v", l)
				}
			}
		}
	}
}

func TestCodexAttachDeleteDetachesOnly(t *testing.T) {
	t.Parallel()
	r := ccStart(t)
	d := r.daemon()
	th := r.thread("delete")
	d.add(th)
	ui := r.terminal(th)
	sid := r.attached(th)
	attach := d.until("the attach", ccRelayReq("thread/resume"))
	resp := r.i.SocketHTTP(r.i.Credential("cos")).Do("DELETE", "/api/sessions/"+sid, nil)
	if resp.Status != http.StatusNoContent {
		t.Fatalf("delete answered %d: %s", resp.Status, resp.Body)
	}
	unsub := d.until("the detach", ccRelayReq("thread/unsubscribe"))
	if !slices.Equal(ccKeys(ccParams(unsub)), []string{"threadId"}) || ccParams(unsub)["threadId"] != th.ID {
		t.Fatalf("thread/unsubscribe params: %v", ccParams(unsub))
	}
	d.until("the Relay connection to close", func(l map[string]any) bool { return l["event"] == "closed" && l["conn"] == attach["conn"] })
	if m := d.mutations(); !slices.Equal(m, []string{"thread/resume", "thread/unsubscribe"}) {
		t.Fatalf("Relay sent %v around the delete, want the attach and the detach", m)
	}
	// The thread and its terminal keep running and the terminal keeps its
	// subscription: a turn it starts still reaches it.
	if ack := d.do(map[string]any{"cmd": "status", "id": th.ID}); fmt.Sprint(ack["subscribers"]) != "[terminal-ui]" {
		t.Fatalf("subscribers after the delete: %v, want only the terminal UI", ack["subscribers"])
	}
	ack := d.do(map[string]any{"cmd": "start_turn", "id": th.ID})
	ui.until("a turn after the delete", func(l map[string]any) bool {
		turn, _ := ccParams(l)["turn"].(map[string]any)
		return l["method"] == "turn/started" && turn["id"] == ack["turn"]
	})
	d.do(map[string]any{"cmd": "complete_turn", "id": th.ID})
	ui.until("the turn ends after the delete", func(l map[string]any) bool {
		status, _ := ccParams(l)["status"].(map[string]any)
		return l["method"] == "thread/status/changed" && status["type"] == "idle"
	})
	if !d.alive() || !ui.alive() {
		t.Fatal("the delete ended the daemon or the terminal UI")
	}
	if len(d.relayCalls("turn/interrupt")) != 0 {
		t.Fatal("the delete interrupted the thread")
	}
}

func TestCodexAttachDormantReattaches(t *testing.T) {
	t.Parallel()
	r := ccStart(t)
	d := r.daemon()
	th := r.thread("dormant")
	d.add(th)
	sid := r.attached(th)
	d.kill()
	r.i.WaitEvent(harness.EventQuery{Key: "session.exited", Fields: map[string]any{"session_id": sid}}, g1sHostDeadline)
	list, _ := g1sListSessions(t, r.i)
	if row, ok := g1sFindSession(list, sid); !ok || row.Live {
		t.Fatalf("session after the daemon died: listed %v, %+v, want dormant", ok, row)
	}
	// With no daemon the resume refuses and writes no intent; Relay starts no
	// codex of its own.
	if res := r.i.CLI("session", "resume", "--id", sid, "--json"); res.Code == 0 {
		t.Fatalf("resume with the daemon down succeeded: %s", res.Stdout)
	}
	ccNoCodexProgram(t, r.i)
	if got := ccReattachRows(t, r, th, "intent"); len(got) != 0 {
		t.Fatalf("a resume with the daemon down wrote a re-attach intent: %v", got)
	}
	list, _ = g1sListSessions(t, r.i)
	if row, ok := g1sFindSession(list, sid); !ok || row.Live {
		t.Fatalf("a refused resume made the session live: %+v", row)
	}
	d2 := r.daemon()
	d2.add(th)
	r.i.MustCLI("session", "resume", "--id", sid, "--json")
	g1sSendText(t, r.i, sid, "Acme after the restart", "echo: Acme after the restart")
	receipt := ccTime(t, d2.until("the re-attach at the restarted daemon", ccRelayReq("thread/resume"))["ts"])
	if got := ccMethods(d2.relayCalls("thread/resume")); len(got) != 1 {
		t.Fatalf("the restarted daemon saw %d attaches, want one", len(got))
	}
	intents := ccReattachRows(t, r, th, "intent")
	completions := ccReattachRows(t, r, th, "completion")
	if len(intents) != 1 || len(completions) != 1 {
		t.Fatalf("re-attach rows: intents %v, completions %v", intents, completions)
	}
	intent, completion := intents[0], completions[0]
	for key, want := range map[string]any{"session_id": sid, "method": "reattach", "relay_sandbox": false, "agent": "codex",
		"conversation_id": th.ID, "project_id": r.project, "directory": th.Dir, "interrupt": false} {
		if ccArg(intent, key) != want {
			t.Fatalf("re-attach intent arg %s = %v, want %v: %v", key, ccArg(intent, key), want, intent)
		}
	}
	if intent["outcome"] != "pending" || completion["outcome"] != "ok" {
		t.Fatalf("re-attach outcomes: intent %v, completion %v", intent["outcome"], completion["outcome"])
	}
	if at := ccTime(t, intent["ts"]); at.After(receipt) {
		t.Fatalf("the re-attach intent at %v follows the daemon's receipt at %v", at, receipt)
	}
	var resumed map[string]any
	for _, row := range g4Wait(t, r.i, g4Has(func(row g4Row) bool {
		return ccArg(row, "session_id") == sid && row.str("outcome") == "ok"
	}), "--event", "session_resume") {
		if ccArg(row, "session_id") == sid && row["outcome"] == "ok" {
			resumed = row
		}
	}
	if ccArg(resumed, "sandbox") != false {
		t.Fatalf("the session_resume row says sandbox %v, want false: %v", ccArg(resumed, "sandbox"), resumed)
	}
	ccNoCodexProgram(t, r.i)
}

// ccAuditRows returns the audit rows of one event.
func ccAuditRows(t *testing.T, r *ccRig, event string) []g4Row {
	t.Helper()
	res := r.i.CLI("audit", "--tail", "100000", "--json", "--event", event)
	if res.Code != 0 {
		t.Fatalf("relay audit --event %s exited %d: %s", event, res.Code, res.Stderr)
	}
	return g4Parse(t, res.Stdout)
}

// ccSessionRow returns the one row of event that names the session.
func ccSessionRow(t *testing.T, r *ccRig, event, sid string) g4Row {
	t.Helper()
	rows := g4Wait(t, r.i, g4Has(func(row g4Row) bool { return ccArg(row, "session_id") == sid }), "--event", event)
	for _, row := range rows {
		if ccArg(row, "session_id") == sid {
			return row
		}
	}
	t.Fatalf("no %s row for session %s", event, sid)
	return nil
}

// ccReattachRows returns the session_adopt rows of the thread with method
// "reattach" in one phase.
func ccReattachRows(t *testing.T, r *ccRig, th ccThread, phase string) []g4Row {
	t.Helper()
	var out []g4Row
	for _, row := range ccAuditRows(t, r, "session_adopt") {
		if ccArg(row, "conversation_id") == th.ID && ccArg(row, "method") == "reattach" && row["phase"] == phase {
			out = append(out, row)
		}
	}
	return out
}

func (r *ccRig) resume(sid string) harness.Response {
	r.t.Helper()
	return r.i.SocketHTTP(r.i.Credential("cos")).Do("POST", "/api/sessions/"+sid+"/resume", nil)
}

// dormant ends the daemon and returns once the session reads dormant.
func (r *ccRig) dormant(d *ccProc, sid string) {
	r.t.Helper()
	d.kill()
	r.i.WaitEvent(harness.EventQuery{Key: "session.exited", Fields: map[string]any{"session_id": sid}}, g1sHostDeadline)
	r.stillDormant(sid)
}

func (r *ccRig) stillDormant(sid string) {
	r.t.Helper()
	list, _ := g1sListSessions(r.t, r.i)
	if row, ok := g1sFindSession(list, sid); !ok || row.Live {
		r.t.Fatalf("session %s: listed %v, %+v, want dormant", sid, ok, row)
	}
}

func TestCodexAttachResumeRefused(t *testing.T) {
	t.Parallel()
	// Managed is not reachable here: the only session on the thread is the
	// dormant one being resumed.
	for _, kind := range []string{"busy", "changed", "ended", "ambiguous"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			r := ccStart(t)
			d := r.daemon()
			th := r.thread("resume-" + kind)
			d.add(th)
			sid := r.attached(th)
			r.dormant(d, sid)
			d2 := r.daemon()
			now := th
			switch kind {
			case "busy":
				now.Turns = []string{"completed", "inProgress"}
			case "changed":
				now.Dir = osDir(t, r.root, "elsewhere")
			}
			if kind != "ended" {
				d2.add(now)
			}
			if kind == "ambiguous" {
				d2.do(map[string]any{"cmd": "dup", "id": th.ID})
			}
			if resp := r.resume(sid); resp.Status != http.StatusConflict {
				t.Fatalf("resume answered %d, want 409: %s", resp.Status, resp.Body)
			}
			d2.barrier()
			if m := d2.mutations(); len(m) != 0 {
				t.Fatalf("Relay sent the daemon %v after a refused resume", m)
			}
			r.stillDormant(sid)
			ccNoCodexProgram(t, r.i)
			// The route's body carries no code: the refusal code is the error of the
			// session_resume audit row.
			rows := g4Wait(t, r.i, g4Has(func(row g4Row) bool { return row.str("error") == kind }), "--event", "session_resume")
			for _, row := range rows {
				if row["error"] == kind && row["outcome"] == "ok" {
					t.Fatalf("a refused resume row says ok: %v", row)
				}
			}
			// Positive control: with the thread as it was, the same resume attaches.
			// add_thread replaces the thread, its busy turn and its duplicate flag.
			d2.add(th)
			r.i.MustCLI("session", "resume", "--id", sid, "--json")
			g1sSendText(t, r.i, sid, "Acme after the refusal", "echo: Acme after the refusal")
		})
	}
}

func TestCodexAttachResumeAuditFailureSendsNothing(t *testing.T) {
	t.Parallel()
	r := ccStart(t, paLowAudit)
	d := r.daemon()
	th := r.thread("resume-audit-failure")
	d.add(th)
	sid := r.attached(th)
	r.dormant(d, sid)
	d2 := r.daemon()
	d2.add(th)
	restore := paBlockAuditWrites(t, &paRig{osRig: r.osRig})
	if resp := r.resume(sid); resp.Status != http.StatusServiceUnavailable {
		t.Fatalf("resume answered %d, want 503: %s", resp.Status, resp.Body)
	}
	d2.barrier()
	if m := d2.mutations(); len(m) != 0 {
		t.Fatalf("an audit refusal still sent the daemon %v", m)
	}
	r.stillDormant(sid)
	ccNoCodexProgram(t, r.i)
	// The recorder keeps its first write error: restore, then restart, its
	// lifecycle boundary. Then the same resume attaches, with one thread/resume.
	restore()
	r.restart()
	r.i.MustCLI("session", "resume", "--id", sid, "--json")
	g1sSendText(t, r.i, sid, "Acme after audit recovery", "echo: Acme after audit recovery")
	if n := len(d2.relayCalls("thread/resume")); n != 1 {
		t.Fatalf("the daemon got %d thread/resume requests, want one", n)
	}
}

func TestCodexAttachTerminalTurnApprovalUnanswered(t *testing.T) {
	t.Parallel()
	r := ccStart(t)
	d := r.daemon()
	th := r.thread("terminal-approval")
	d.add(th)
	ui := r.terminal(th)
	sid := r.attached(th)
	ws := r.join(sid)
	receiver := func(ack map[string]any, client string) any {
		recs, _ := ack["receivers"].([]any)
		for _, rec := range recs {
			if m, _ := rec.(map[string]any); m["client"] == client {
				return m["rid"]
			}
		}
		t.Fatalf("approval receivers %v hold no %s", ack["receivers"], client)
		return nil
	}
	// A turn the terminal UI started: the request goes to every subscriber.
	d.do(map[string]any{"cmd": "start_turn", "id": th.ID})
	g1aNext(t, ws, g1tFrameDeadline, ccState(sid, "running"))
	first := d.do(map[string]any{"cmd": "approval", "id": th.ID, "method": "item/commandExecution/requestApproval", "to": "all"})
	relayFirst, uiFirst := receiver(first, "relay"), receiver(first, "terminal-ui")
	ui.until("the terminal's own request", func(l map[string]any) bool { return l["event"] == "server_request" && l["id"] == uiFirst })
	ui.do(map[string]any{"cmd": "answer", "id": uiFirst, "result": map[string]any{"decision": "accept"}})
	d.until("the terminal's answer at the daemon", func(l map[string]any) bool {
		return l["event"] == "response" && l["client"] == "terminal-ui" && l["id"] == uiFirst
	})
	d.do(map[string]any{"cmd": "complete_turn", "id": th.ID})
	g1aNext(t, ws, g1tFrameDeadline, ccState(sid, "idle"))
	// A turn Relay started: its request is declined.
	r.hold(d, ws, sid)
	second := d.do(map[string]any{"cmd": "approval", "id": th.ID, "method": "item/commandExecution/requestApproval", "to": "all"})
	relaySecond := receiver(second, "relay")
	decline := d.until("Relay's answer to the request on its own turn", func(l map[string]any) bool {
		return l["event"] == "response" && l["client"] == "relay" && l["id"] == relaySecond
	})
	result, _ := decline["result"].(map[string]any)
	if decline["error"] != nil || fmt.Sprint(result["decision"]) != "decline" {
		t.Fatalf("Relay's answer on its own turn: %v, want a decline", decline)
	}
	d.barrier()
	for _, l := range d.snapshot() {
		if l["event"] == "response" && l["client"] == "relay" && l["id"] == relayFirst {
			t.Fatalf("Relay answered a request for a turn it did not start: %v", l)
		}
	}
}

func TestCodexAttachClearRefused(t *testing.T) {
	t.Parallel()
	r := ccStart(t)
	d := r.daemon()
	th := r.thread("clear")
	d.add(th)
	sid := r.attached(th)
	g1sSendText(t, r.i, sid, "Acme before the clear", "echo: Acme before the clear")
	ws := r.join(sid)
	ws.Send(map[string]any{"type": "clear_session", "sessionId": sid})
	// The refusal is a plain error frame without a session id.
	g1aNext(t, ws, g1tFrameDeadline, func(v map[string]any) bool { return v["type"] == "error" })
	list, _ := g1sListSessions(t, r.i)
	if row, ok := g1sFindSession(list, sid); !ok || !row.Live {
		t.Fatalf("the refused clear changed the session: listed %v, %+v", ok, row)
	}
	// A following resume starts no codex program, and the session keeps working.
	_ = r.resume(sid)
	ccNoCodexProgram(t, r.i)
	g1sSendText(t, r.i, sid, "Acme after the clear", "echo: Acme after the clear")
	d.barrier()
	if m := d.mutations(); !slices.Equal(m, []string{"thread/resume", "turn/start", "turn/start"}) {
		t.Fatalf("Relay sent %v, want the attach and the two turns", m)
	}
	ccNoCodexProgram(t, r.i)
}

func TestCodexAttachScopeRefused(t *testing.T) {
	t.Parallel()
	r := ccStart(t)
	d := r.daemon()
	th := r.thread("scope")
	d.add(th)
	body := map[string]any{"outsideId": r.mustRow(th)["id"]}
	scoped := r.i.SocketHTTP(r.i.Credential("cos")).Do("POST", adDoor, body, harness.ReqOpts{Header: g1tScope, Trace: harness.NewTrace(t)})
	if scoped.Status != http.StatusForbidden {
		t.Fatalf("the chief-of-staff scope got %d, want 403: %s", scoped.Status, scoped.Body)
	}
	reader := r.i.SocketHTTP(r.i.Credential("reader")).Do("POST", adDoor, body, harness.ReqOpts{Trace: harness.NewTrace(t)})
	if reader.Status != http.StatusForbidden {
		t.Fatalf("a read-only credential got %d, want 403: %s", reader.Status, reader.Body)
	}
	r.untouched(d)
}

func TestCodexAttachModelRefused(t *testing.T) {
	t.Parallel()
	r := ccStart(t)
	d := r.daemon()
	th := r.thread("model")
	d.add(th)
	id := r.mustRow(th)["id"]
	// Attach changes no setting, so a model, or another way to name the
	// thread, is a malformed body.
	for name, body := range map[string]map[string]any{
		"model":           {"outsideId": id, "model": "codex/fake-echo"},
		"conversation-id": {"agent": "codex", "conversationId": th.ID},
		"claude-id":       {"claudeSessionId": th.ID},
		"outside-and-id":  {"outsideId": id, "conversationId": th.ID},
	} {
		t.Run(name, func(t *testing.T) {
			otRefusal(t, r.adopt(body), http.StatusBadRequest, "invalid_body")
		})
	}
	r.untouched(d)
}
