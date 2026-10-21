package features

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"relaye2e/harness"
)

// These tests adopt a fake outside Claude (fakes/fakeoutside, run under the
// base name claude) through POST /api/sessions/adopt. The test writes the
// registry entry and the transcript under the instance's HOME, as Claude Code
// does, and reads the fake's stdout lines for what it received. The resumed
// session is the instance's fake agent, so its call log shows the start line
// and the first input.

const (
	adDoor = "/api/sessions/adopt"
	// The adopt waits (docs/session-adoption.md): each step of the ladder
	// waits this long on the server clock before the next signal.
	adInterruptWait = 5 * time.Second
	adTermWait      = 5 * time.Second

	adPrompt  = "acme refactor the invoice parser"
	adToolID  = "toolu_acme_open_1"
	adModelSn = "claude-sonnet-4-5"
)

var adToolInput = map[string]any{"command": "touch acme-invoice.txt"}

// adUUID is a random version 4 UUID, the form a registry sessionId must have.
func adUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// adFake is one running fakeoutside and the files that stand for its session.
type adFake struct {
	t     *testing.T
	cmd   *exec.Cmd
	pid   int
	dir   string
	sid   string
	start time.Time // the kernel start time, truncated to the second
	lstr  string    // the same, as the registry writes procStart
	lines chan string
	exit  chan struct{} // closed after Wait returns
	ws    syscall.WaitStatus

	children []int
}

// adSpawn starts the fake in dir, named claude, and returns after its ready line.
func adSpawn(t *testing.T, dir, sid string, ignore ...string) *adFake {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.Link(harness.FakeOutsidePath(), bin); err != nil {
		data, rerr := os.ReadFile(harness.FakeOutsidePath())
		if rerr != nil {
			t.Fatalf("reading the fake outside Claude: %v", rerr)
		}
		if werr := os.WriteFile(bin, data, 0o755); werr != nil {
			t.Fatalf("copying the fake outside Claude: %v", werr)
		}
	}
	var args []string
	if len(ignore) > 0 {
		args = []string{"--ignore", strings.Join(ignore, ",")}
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating a pipe: %v", err)
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
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
		for _, c := range f.children {
			_ = syscall.Kill(c, syscall.SIGKILL)
		}
		_ = pr.Close()
	})
	f.expect("ready ")

	f.lstr, f.start = adLstart(t, f.pid)
	return f
}

// adLstart is the kernel start time of pid as the registry writes procStart,
// and parsed (UTC, truncated to the second).
func adLstart(t *testing.T, pid int) (string, time.Time) {
	t.Helper()
	ps := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	ps.Env = append(os.Environ(), "TZ=UTC", "LC_ALL=C")
	out, err := ps.Output()
	if err != nil {
		t.Fatalf("ps for pid %d: %v", pid, err)
	}
	lstr := strings.Join(strings.Fields(string(out)), " ")
	at, err := time.Parse("Mon Jan 2 15:04:05 2006", lstr)
	if err != nil {
		t.Fatalf("parsing the start time %q: %v", lstr, err)
	}
	return lstr, at
}

// expect returns the next stdout line, which must begin with prefix.
func (f *adFake) expect(prefix string) string {
	f.t.Helper()
	select {
	case line, ok := <-f.lines:
		if !ok {
			f.t.Fatalf("the fake (pid %d) closed stdout before a %q line", f.pid, prefix)
		}
		if !strings.HasPrefix(line, prefix) {
			f.t.Fatalf("the fake (pid %d) printed %q, want a line starting %q", f.pid, line, prefix)
		}
		return line
	case <-time.After(osBound):
		f.t.Fatalf("no %q line from the fake (pid %d) within %v", prefix, f.pid, osBound)
		return ""
	}
}

// signalAt parses a "signal NAME <unix ns>" line.
func (f *adFake) signalAt(line, name string) time.Time {
	f.t.Helper()
	parts := strings.Fields(line)
	if len(parts) != 3 || parts[0] != "signal" || parts[1] != name {
		f.t.Fatalf("line %q, want signal %s <unix ns>", line, name)
	}
	ns, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		f.t.Fatalf("line %q holds no unix ns: %v", line, err)
	}
	return time.Unix(0, ns)
}

// sync makes the fake start a child and returns the signal lines it printed
// up to the child line. Stdout is ordered, so a signal sent before the call
// is in the result; this is how a test shows that none was sent.
func (f *adFake) sync() (signals []string) {
	f.t.Helper()
	return f.syncWith(syscall.SIGUSR1)
}

// syncWith is sync with the signal that picks the child: SIGUSR1 a sleep,
// SIGUSR2 the caffeinate helper a terminal Claude starts at a turn.
func (f *adFake) syncWith(sig syscall.Signal) (signals []string) {
	f.t.Helper()
	if err := syscall.Kill(f.pid, sig); err != nil {
		f.t.Fatalf("signal %d to pid %d: %v", sig, f.pid, err)
	}
	for {
		select {
		case line, ok := <-f.lines:
			if !ok {
				f.t.Fatalf("the fake (pid %d) closed stdout before its child line; signals so far %v", f.pid, signals)
			}
			if rest, found := strings.CutPrefix(line, "child "); found {
				pid, err := strconv.Atoi(rest)
				if err != nil {
					f.t.Fatalf("child line %q holds no pid", line)
				}
				f.children = append(f.children, pid)
				return signals
			}
			signals = append(signals, line)
		case <-time.After(osBound):
			f.t.Fatalf("no child line from the fake (pid %d) within %v; signals so far %v", f.pid, osBound, signals)
		}
	}
}

// laterChild starts children until one has a kernel start second later than
// the process's. A child that starts too early is killed before the next is
// asked for, so at most one extra child is alive at a time. It returns the
// signal lines seen and the kept child's start.
func (f *adFake) laterChild() (signals []string, childStart time.Time) {
	f.t.Helper()
	return f.laterChildWith(syscall.SIGUSR1)
}

// laterChildWith is laterChild for the child that sig starts.
func (f *adFake) laterChildWith(sig syscall.Signal) (signals []string, childStart time.Time) {
	f.t.Helper()
	for tries := 0; ; tries++ {
		signals = append(signals, f.syncWith(sig)...)
		pid := f.children[len(f.children)-1]
		if _, childStart = adLstart(f.t, pid); childStart.After(f.start) {
			return signals, childStart
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
		f.children = f.children[:len(f.children)-1]
		if tries > 2000 {
			f.t.Fatalf("no child of pid %d started in a later second than the process (%v)", f.pid, f.start)
		}
	}
}

// alive fails t when the fake has exited.
func (f *adFake) alive() {
	f.t.Helper()
	select {
	case <-f.exit:
		f.t.Fatalf("the fake (pid %d) exited (%v), want it alive", f.pid, f.ws)
	default:
	}
}

// gone waits for the fake to exit.
func (f *adFake) gone() {
	f.t.Helper()
	select {
	case <-f.exit:
	case <-time.After(osBound):
		f.t.Fatalf("the fake (pid %d) is still alive after %v", f.pid, osBound)
	}
}

// register writes the registry entry Claude Code keeps for the process.
func (f *adFake) register(home, status string, updated time.Time, mut ...func(map[string]any)) {
	f.t.Helper()
	e := map[string]any{
		"pid": f.pid, "sessionId": f.sid, "cwd": f.dir, "startedAt": time.Now().UnixMilli(),
		"procStart": f.lstr, "status": status, "statusUpdatedAt": updated.UnixMilli(),
		"kind": "interactive", "entrypoint": "cli", "name": "Acme adopt",
	}
	for _, m := range mut {
		m(e)
	}
	raw, _ := json.Marshal(e)
	osWrite(f.t, filepath.Join(home, ".claude", "sessions", fmt.Sprintf("%d.json", f.pid)), raw)
}

func (f *adFake) transcript(home string, lines ...string) {
	f.t.Helper()
	osWrite(f.t, osTranscriptPath(home, f.dir, f.sid), []byte(strings.Join(lines, "\n")+"\n"))
}

func (f *adFake) user(at time.Time, text string) string {
	return osRec("user", map[string]any{"role": "user", "content": text}, f.sid, f.dir, "acme-branch", at)
}

func (f *adFake) answer(at time.Time, model, text string) string {
	m := map[string]any{"role": "assistant", "stop_reason": "end_turn", "content": []any{map[string]any{"type": "text", "text": text}}}
	if model != "" {
		m["model"] = model
	}
	return osRec("assistant", m, f.sid, f.dir, "acme-branch", at)
}

func (f *adFake) toolUse(at time.Time, model string) string {
	m := map[string]any{"role": "assistant", "stop_reason": "tool_use", "content": []any{
		map[string]any{"type": "tool_use", "id": adToolID, "name": "Bash", "input": adToolInput},
	}}
	if model != "" {
		m["model"] = model
	}
	return osRec("assistant", m, f.sid, f.dir, "acme-branch", at)
}

// toolCall is an assistant record that holds one tool call with the given id.
func (f *adFake) toolCall(at time.Time, model, id string) string {
	m := map[string]any{"role": "assistant", "stop_reason": "tool_use", "content": []any{
		map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": adToolInput},
	}}
	if model != "" {
		m["model"] = model
	}
	return osRec("assistant", m, f.sid, f.dir, "acme-branch", at)
}

// toolResult is the user record that closes the tool call id.
func (f *adFake) toolResult(at time.Time, id, text string) string {
	m := map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": id, "content": text},
	}}
	return osRec("user", m, f.sid, f.dir, "acme-branch", at)
}

// laterOf returns the later of a and b.
func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// idle plants a finished turn and an idle registry entry written with it.
// The first user record carries the process start time, the earliest a
// record written after the process started can have.
func (f *adFake) idle(home, model string) {
	f.t.Helper()
	now := time.Now().UTC()
	f.transcript(home, f.user(f.start, adPrompt), f.answer(now, model, "acme parser done"))
	f.register(home, "idle", now)
}

// busy plants a finished turn and a registry entry that says busy, written after it.
func (f *adFake) busy(home, model string) {
	f.t.Helper()
	now := time.Now().UTC()
	f.transcript(home, f.user(f.start, adPrompt), f.answer(now, model, "acme parser done"))
	f.register(home, "busy", now.Add(time.Minute))
}

// openTool plants a turn whose last record is a tool call with no result,
// and a registry entry of the given status written with it.
func (f *adFake) openTool(home, model, status string) {
	f.t.Helper()
	now := time.Now().UTC()
	f.transcript(home, f.user(f.start, adPrompt), f.toolUse(now, model))
	f.register(home, status, now)
}

// adRig is an osRig with one registered local project that holds the fakes.
type adRig struct {
	*osRig
	root string
	pid  string
}

func adStart(t *testing.T, mut ...func(*harness.Options)) *adRig {
	t.Helper()
	return adStartHost(t, false, mut...)
}

// adStartHost is adStart with the ssh stub host when host is true.
func adStartHost(t *testing.T, host bool, mut ...func(*harness.Options)) *adRig {
	t.Helper()
	r := osStart(t, host, mut...)
	root := osDir(t, filepath.Join(r.i.Home, "work"), "acme-adopt")
	body, _ := json.Marshal(map[string]any{"name": "Acme adopt", "path": root, "allowed_templates": []string{"claude-code"}})
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
	return &adRig{osRig: r, root: root, pid: p.ID}
}

// folder creates a folder inside the project and returns its resolved path.
func (a *adRig) folder(name string) string { return osDir(a.t, a.root, name) }

// fake starts a fake outside Claude in a new folder of the project.
func (a *adRig) fake(name string, ignore ...string) *adFake {
	a.t.Helper()
	return adSpawn(a.t, a.folder(name), adUUID(), ignore...)
}

// adopt posts the adopt door as the owner: the socket, an execute credential,
// no scope.
func (r *osRig) adopt(body any) harness.Response {
	r.t.Helper()
	return r.i.SocketHTTP(r.i.Credential("cos")).Do("POST", adDoor, body, harness.ReqOpts{Trace: harness.NewTrace(r.t)})
}

type adResult struct {
	SessionID       string `json:"sessionId"`
	ClaudeSessionID string `json:"claudeSessionId"`
	Interrupted     bool   `json:"interrupted"`
	RecoverySent    bool   `json:"recoverySent"`
}

func adCreated(t *testing.T, resp harness.Response) adResult {
	t.Helper()
	if resp.Status != http.StatusCreated {
		t.Fatalf("the adopt door answered %d, want 201: %s", resp.Status, resp.Body)
	}
	var res adResult
	resp.JSON(t, &res)
	if res.SessionID == "" {
		t.Fatalf("the 201 body holds no sessionId: %s", resp.Body)
	}
	return res
}

// adNoSignals fails t unless the fakes were sent nothing and still live.
func adNoSignals(t *testing.T, fakes ...*adFake) {
	t.Helper()
	for _, f := range fakes {
		if sig := f.sync(); len(sig) != 0 {
			t.Fatalf("the fake (pid %d) was sent signals %v, want none", f.pid, sig)
		}
		f.alive()
	}
}

type adSession struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	Name      string `json:"name"`
	Directory string `json:"directory"`
	Live      bool   `json:"live"`
	Headless  bool   `json:"headless"`
	Origin    string `json:"origin"`
}

func adSessions(t *testing.T, i *harness.Instance) []adSession {
	t.Helper()
	var out struct {
		Sessions []adSession `json:"sessions"`
	}
	i.MustCLI("session", "list", "--json").JSON(t, &out)
	return out.Sessions
}

// adStartLine sends one turn to the adopted session sid and returns the argv
// of its start line. The agent writes the start line before the first reply,
// so the reply is the ready signal.
func adStartLine(t *testing.T, i *harness.Instance, sid, text string) []string {
	t.Helper()
	g1sSendText(t, i, sid, text, "echo: "+text)
	for _, c := range i.FakeAgentCalls("claude") {
		if c.Event == "start" && c.Env["RELAY_SESSION_ID"] == sid {
			return c.Argv
		}
	}
	t.Fatalf("no claude start line carries RELAY_SESSION_ID %s", sid)
	return nil
}

// adHasFlag reports whether argv holds name immediately followed by value.
func adHasFlag(argv []string, name, value string) bool {
	for k := 0; k+1 < len(argv); k++ {
		if argv[k] == name && argv[k+1] == value {
			return true
		}
	}
	return false
}

// adInputs are the fake agent's user-turn inputs, in order.
func adInputs(i *harness.Instance) []harness.AgentCall {
	var out []harness.AgentCall
	for _, c := range i.FakeAgentCalls("claude") {
		if c.Event == "input" && c.Text != "" {
			out = append(out, c)
		}
	}
	return out
}

func adNoKeys(t *testing.T, where string, row map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, has := row[k]; has {
			t.Fatalf("%s carries %q, which never leaves relay: %v", where, k, row)
		}
	}
}

// AC1: a live interactive session lists its conversation id and a state read
// from the agent's own registry entry, on the list, the detail and the WS.
func TestAdoptRowListsConversationAndState(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	f := a.fake("app")
	f.idle(a.i.Home, adModelSn)

	s, frame := a.probeFrame()
	live := s.liveAt(f.dir)
	if live == nil {
		t.Fatalf("no live row for %s; snapshot %+v", f.dir, s)
	}
	row := osRowByID(frame, live.ID)
	if row["state"] != "idle" || row["adoptable"] != true || row[osClaudeIDKey] != f.sid {
		t.Fatalf("idle row = %v, want state idle, adoptable true and %s %s", row, osClaudeIDKey, f.sid)
	}
	if _, has := row["adoptReason"]; has {
		t.Fatalf("an adoptable row carries adoptReason: %v", row)
	}
	adNoKeys(t, "WS row", row, "pid", "procStart", "kind", "entrypoint")
	detail := a.detailSession(live.ID)
	if detail["state"] != "idle" || detail["adoptable"] != true || detail[osClaudeIDKey] != f.sid {
		t.Fatalf("detail = %v, want state idle, adoptable true and %s %s", detail, osClaudeIDKey, f.sid)
	}
	adNoKeys(t, "detail", detail, "pid", "procStart", "kind", "entrypoint")
	listed := false
	for _, lr := range a.endedList() {
		if lr["id"] == live.ID {
			listed = true
			if lr["state"] != "idle" || lr["adoptable"] != true || lr[osClaudeIDKey] != f.sid {
				t.Fatalf("list row = %v, want state idle, adoptable true and %s %s", lr, osClaudeIDKey, f.sid)
			}
		}
	}
	if !listed {
		t.Fatalf("the list holds no row %s", live.ID)
	}

	// A newer busy entry reads running; a busy row stays adoptable (it may be
	// adopted with an interrupt).
	f.busy(a.i.Home, adModelSn)
	_, frame = a.probeFrame()
	row = osRowByID(frame, live.ID)
	if row == nil || row["state"] != "running" || row["adoptable"] != true || row[osClaudeIDKey] != f.sid {
		t.Fatalf("busy row = %v, want state running, adoptable true and %s %s", row, osClaudeIDKey, f.sid)
	}
}

// AC1 (eligibility): each reason an outside row is not adoptable, on the row
// and at the door.
func TestAdoptReasonsOnRows(t *testing.T) {
	t.Parallel()
	r := adStartHost(t, true)
	a := r.osRig

	bg := r.fake("bg", "INT", "TERM")
	bg.idle(a.i.Home, adModelSn)
	bg.register(a.i.Home, "idle", time.Now(), func(e map[string]any) { e["kind"] = "bg" })
	sdk := r.fake("sdk", "INT", "TERM")
	sdk.idle(a.i.Home, adModelSn)
	sdk.register(a.i.Home, "idle", time.Now(), func(e map[string]any) { e["entrypoint"] = "sdk-cli" })
	// A folder under no registered project.
	loose := adSpawn(t, osDir(t, t.TempDir(), "loose"), adUUID(), "INT", "TERM")
	loose.idle(a.i.Home, adModelSn)
	// A session whose process is gone: an ended row.
	edir := osDir(t, r.root, "gone")
	const endedStem = "acme-ended-stem"
	a.ended(a.i.Home, edir, endedStem, 5*osEndedHour, "acme gone")
	// A host row, on the ssh stub.
	h := a.spawn(a.remote, filepath.Join(a.remote, "boxproj"), "host")
	h.transcript(t, osPrompt("acme hello"), osAnswer("acme hi"))

	s, frame := a.probeFrame()
	cases := []struct {
		name, reason string
		row          *osRow
		id           string
	}{
		{"kind bg", "not_interactive", s.liveAt(bg.dir), ""},
		{"entrypoint sdk-cli", "not_command_line", s.liveAt(sdk.dir), ""},
		{"no project", "no_project", s.liveAt(loose.dir), ""},
		{"ended", "ended", s.endedAt(edir), ""},
		{"host", "host_unsupported", s.liveAt(h.dir), ""},
	}
	for _, c := range cases {
		if c.row == nil {
			t.Fatalf("%s: no row in %+v", c.name, s)
		}
		wire := osRowByID(frame, c.row.ID)
		if wire["adoptable"] != false || wire["adoptReason"] != c.reason {
			t.Fatalf("%s: row = %v, want adoptable false and adoptReason %s", c.name, wire, c.reason)
		}
		if got := a.detailSession(c.row.ID); got["adoptable"] != false || got["adoptReason"] != c.reason {
			t.Fatalf("%s: detail = %v, want adoptable false and adoptReason %s", c.name, got, c.reason)
		}
		otRefusal(t, a.adopt(map[string]any{"outsideId": c.row.ID, "interrupt": true}), http.StatusConflict, c.reason)
	}
	adNoSignals(t, bg, sdk, loose)
}

// AC2: adopting an idle session ends the process and resumes the same
// conversation as a relay headless session in the same folder.
func TestAdoptIdleResumesSameConversation(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	f := a.fake("app")
	f.idle(a.i.Home, adModelSn)
	s, frame := a.probeFrame()
	live := s.liveAt(f.dir)
	if live == nil {
		t.Fatalf("no live row for %s; snapshot %+v", f.dir, s)
	}
	name, _ := osRowByID(frame, live.ID)["name"].(string)

	resp := a.adopt(map[string]any{"outsideId": live.ID, "interrupt": false})
	res := adCreated(t, resp)
	if res.ClaudeSessionID != f.sid || res.Interrupted || res.RecoverySent {
		t.Fatalf("result = %+v, want claudeSessionId %s, interrupted false, recoverySent false", res, f.sid)
	}
	f.signalAt(f.expect("signal TERM"), "TERM")
	f.gone()

	var sess *adSession
	for _, x := range adSessions(t, a.i) {
		if x.ID == res.SessionID {
			sess = &x
		}
	}
	if sess == nil || !sess.Live || !sess.Headless || sess.Directory != f.dir || sess.ProjectID != a.pid || sess.Origin != "" {
		t.Fatalf("session %s = %+v, want a live headless session in %s of project %s with no origin", res.SessionID, sess, f.dir, a.pid)
	}
	if sess.Name != name {
		t.Fatalf("session name %q, want the row's name %q", sess.Name, name)
	}
	argv := adStartLine(t, a.i, res.SessionID, "acme next")
	if !adHasFlag(argv, "--resume", f.sid) || !adHasFlag(argv, "--model", "sonnet") {
		t.Fatalf("start line argv = %v, want --resume %s and --model sonnet", argv, f.sid)
	}
	if in := adInputs(a.i); len(in) != 1 || in[0].Text != "acme next" {
		t.Fatalf("the agent's inputs = %+v, want only the owner's message: an idle adopt sends none", in)
	}
	if s := a.probe(); s.at(f.dir) != nil {
		t.Fatalf("after the adopt a probe still lists %s: %+v", f.dir, s.at(f.dir))
	}
	a.i.WaitEvent(harness.EventQuery{Key: "session.adopt", Trace: resp.Trace, Fields: map[string]any{
		"status": "ok", "outside_id": live.ID, "claude_session_id": f.sid, "project_id": a.pid,
		"session_id": res.SessionID, "interrupted": false,
	}}, osBound)
}

// AC2 (power selector): adopt by Claude session id before any probe saw the
// process; the body's model wins, else the transcript's family decides.
func TestAdoptByClaudeSessionID(t *testing.T) {
	t.Parallel()
	t.Run("body model", func(t *testing.T) {
		a := adStart(t)
		f := a.fake("app")
		f.idle(a.i.Home, adModelSn)
		res := adCreated(t, a.adopt(map[string]any{"claudeSessionId": f.sid, "interrupt": false, "model": "haiku"}))
		f.gone()
		argv := adStartLine(t, a.i, res.SessionID, "hi")
		if res.ClaudeSessionID != f.sid || !adHasFlag(argv, "--resume", f.sid) || !adHasFlag(argv, "--model", "haiku") {
			t.Fatalf("result %+v, argv %v, want --resume %s and --model haiku", res, argv, f.sid)
		}
	})
	t.Run("transcript model", func(t *testing.T) {
		a := adStart(t)
		f := a.fake("app")
		f.idle(a.i.Home, adModelSn)
		res := adCreated(t, a.adopt(map[string]any{"claudeSessionId": f.sid, "interrupt": false}))
		f.gone()
		if argv := adStartLine(t, a.i, res.SessionID, "hi"); !adHasFlag(argv, "--model", "sonnet") {
			t.Fatalf("argv = %v, want --model sonnet from a claude-sonnet transcript", argv)
		}
	})
	t.Run("model unknown", func(t *testing.T) {
		a := adStart(t)
		f := a.fake("app", "INT", "TERM")
		f.idle(a.i.Home, "")
		otRefusal(t, a.adopt(map[string]any{"claudeSessionId": f.sid, "interrupt": true}), http.StatusConflict, "model_unknown")
		adNoSignals(t, f)
	})
}

// AC3: a busy session is refused unless the caller asks to interrupt; each
// veto of the idle check answers 409 busy and nothing is signalled.
func TestAdoptBusyRefusedWithoutInterrupt(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	f := a.fake("app", "INT", "TERM")
	body := map[string]any{"claudeSessionId": f.sid, "interrupt": false}

	f.busy(a.i.Home, adModelSn) // registry_busy
	otRefusal(t, a.adopt(body), http.StatusConflict, "busy")
	f.alive()

	f.openTool(a.i.Home, adModelSn, "idle") // open_tool_call
	otRefusal(t, a.adopt(body), http.StatusConflict, "busy")
	f.alive()

	// child_process: a child that started inside the last turn's tool window,
	// at or after the first assistant record that follows the last prompt and at
	// or before the newest record. The child must start in a later second than
	// the process; a child is started again until one does, the kernel start time
	// being the signal. The last turn's prompt and tool call carry the child's
	// start second, which is no later than its kernel start.
	signals, childStart := f.laterChild()
	now := laterOf(time.Now().UTC(), childStart)
	f.transcript(a.i.Home, adToolChildTurns(f, childStart, now)...)
	f.register(a.i.Home, "idle", now)
	adChildVeto(t, a.adopt(body))
	signals = append(signals, f.sync()...)
	if len(signals) != 0 {
		t.Fatalf("the fake was sent signals %v by refused adopts, want none", signals)
	}
	f.alive()
	if n := len(adSessions(t, a.i)); n != 0 {
		t.Fatalf("refused adopts left %d sessions", n)
	}
}

// adChildVeto asserts the idle check's child_process veto: 409 busy, the
// reason named in the message.
func adChildVeto(t *testing.T, resp harness.Response) {
	t.Helper()
	otRefusal(t, resp, http.StatusConflict, "busy")
	var e struct{ Message string }
	resp.JSON(t, &e)
	if !strings.Contains(e.Message, "(child_process)") {
		t.Fatalf("the busy refusal names %q, want the reason child_process", e.Message)
	}
}

// adToolChildTurns is a transcript whose child, started at childStart, lies
// inside the last turn's tool window: turn 1 at the process start, then a turn
// whose prompt and tool call carry the child's start second, the tool's result
// and a final answer at newest.
func adToolChildTurns(f *adFake, childStart, newest time.Time) []string {
	return []string{
		f.user(f.start, adPrompt), f.answer(f.start, adModelSn, "acme first answer"),
		f.user(childStart, adPrompt), f.toolCall(childStart, adModelSn, adToolID),
		f.toolResult(newest, adToolID, "acme output"), f.answer(newest, adModelSn, "acme parser done"),
	}
}

// A long-lived child, the stand-in for an MCP server, that starts after the
// session's last assistant record is no turn's work: the idle session is
// adopted without an interrupt.
func TestAdoptIdleIgnoresLateLongLivedChild(t *testing.T) {
	t.Parallel()
	adopted := func(t *testing.T, a *adRig, f *adFake) {
		t.Helper()
		res := adCreated(t, a.adopt(map[string]any{"claudeSessionId": f.sid, "interrupt": false}))
		if res.ClaudeSessionID != f.sid || res.Interrupted {
			t.Fatalf("result = %+v, want claudeSessionId %s, interrupted false", res, f.sid)
		}
		f.signalAt(f.expect("signal TERM"), "TERM")
		f.gone()
	}
	t.Run("single_turn", func(t *testing.T) {
		t.Parallel()
		a := adStart(t)
		f := a.fake("app")
		if signals, _ := f.laterChild(); len(signals) != 0 {
			t.Fatalf("the fake sent signals %v before the adopt, want none", signals)
		}
		// The prompt and the reply land in the process start second, before the child.
		f.transcript(a.i.Home, f.user(f.start, adPrompt), f.answer(f.start, adModelSn, "acme parser done"))
		f.register(a.i.Home, "idle", time.Now().UTC())
		adopted(t, a, f)
	})
	t.Run("multi_turn", func(t *testing.T) {
		t.Parallel()
		a := adStart(t)
		f := a.fake("app")
		_, childStart := f.laterChild()
		// The last turn, prompt and reply, follows the child's start.
		at := laterOf(time.Now().UTC(), childStart.Add(time.Second))
		f.transcript(a.i.Home, f.user(f.start, adPrompt), f.answer(f.start, adModelSn, "acme first answer"),
			f.user(at, adPrompt), f.answer(at, adModelSn, "acme parser done"))
		f.register(a.i.Home, "idle", at)
		adopted(t, a, f)
	})
}

// A child that started after the last turn's tool call and before its newest
// record is that turn's work: it vetoes, and nothing is signalled.
func TestAdoptToolChildOfLastTurnVetoes(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	f := a.fake("app", "INT", "TERM")
	signals, childStart := f.laterChild()
	now := laterOf(time.Now().UTC(), childStart)
	f.transcript(a.i.Home, adToolChildTurns(f, childStart, now)...)
	f.register(a.i.Home, "idle", now)

	adChildVeto(t, a.adopt(map[string]any{"claudeSessionId": f.sid, "interrupt": false}))
	signals = append(signals, f.sync()...)
	if len(signals) != 0 {
		t.Fatalf("the fake was sent signals %v by a refused adopt, want none", signals)
	}
	f.alive()
	if n := len(adSessions(t, a.i)); n != 0 {
		t.Fatalf("a refused adopt left %d sessions", n)
	}
}

// AC3, decision 5: a terminal Claude's idle turn helper, /usr/bin/caffeinate,
// started after the first assistant record, never vetoes the adopt.
func TestAdoptIdleIgnoresTurnHelper(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	f := a.fake("app")
	signals, _ := f.laterChildWith(syscall.SIGUSR2)
	if len(signals) != 0 {
		t.Fatalf("the fake sent signals %v before the adopt, want none", signals)
	}
	now := time.Now().UTC()
	f.transcript(a.i.Home, f.user(f.start, adPrompt), f.answer(f.start, adModelSn, "acme parser done"))
	f.register(a.i.Home, "idle", now)
	res := adCreated(t, a.adopt(map[string]any{"claudeSessionId": f.sid, "interrupt": false}))
	if res.ClaudeSessionID != f.sid || res.Interrupted {
		t.Fatalf("result = %+v, want claudeSessionId %s, interrupted false", res, f.sid)
	}
	f.signalAt(f.expect("signal TERM"), "TERM")
	f.gone()
}

// AC3, decision 5: the child-process veto holds when the transcript is longer
// than the tail the idle check reads, so the last turn's prompt lies at the
// file's start, far from the newest record.
func TestAdoptChildVetoInLongTranscript(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	f := a.fake("app", "INT", "TERM")
	body := map[string]any{"claudeSessionId": f.sid, "interrupt": false}

	signals, childStart := f.laterChild()

	filler := strings.Repeat("acme ordinary turn ", 12)
	// The last turn's prompt and first tool call precede the child; more than
	// the tail's size of closed tool pairs, the oldest in the tail included,
	// follow the child's start, and a final answer comes last.
	lines := []string{f.user(f.start, adPrompt), f.toolCall(f.start, adModelSn, "toolu_acme_0"), f.toolResult(f.start, "toolu_acme_0", "acme output")}
	size := len(lines[0]) + len(lines[1]) + len(lines[2])
	after := childStart.Add(time.Second)
	for n := 1; size <= 300<<10; n++ {
		id := fmt.Sprintf("toolu_acme_%d", n)
		u, r := f.toolCall(after, adModelSn, id), f.toolResult(after, id, filler)
		lines = append(lines, u, r)
		size += len(u) + len(r) + 2
	}
	now := laterOf(time.Now().UTC(), after)
	lines = append(lines, f.answer(now, adModelSn, "acme parser done"))
	f.transcript(a.i.Home, lines...)
	f.register(a.i.Home, "idle", now)

	adChildVeto(t, a.adopt(body))
	signals = append(signals, f.sync()...)
	if len(signals) != 0 {
		t.Fatalf("the fake was sent signals %v by a refused adopt, want none", signals)
	}
	f.alive()
	if n := len(adSessions(t, a.i)); n != 0 {
		t.Fatalf("a refused adopt left %d sessions", n)
	}
}

type adSignal struct {
	Signal string `json:"signal"`
	At     string `json:"at"`
}

// adCompletion returns the signals of the completion row of event.
func adCompletion(t *testing.T, i *harness.Instance, event string) []adSignal {
	t.Helper()
	rows := g4Wait(t, i, g4Has(func(r g4Row) bool { return r.str("phase") == "completion" && r.str("outcome") == "ok" }), "--event", event)
	rows = g4Where(rows, func(r g4Row) bool { return r.str("phase") == "completion" && r.str("outcome") == "ok" })
	raw, _ := json.Marshal(rows[0].obj("args")["signals"])
	var sig []adSignal
	if err := json.Unmarshal(raw, &sig); err != nil {
		t.Fatalf("decoding the signals of %s: %v", event, err)
	}
	return sig
}

func adAt(t *testing.T, s adSignal) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, s.At)
	if err != nil {
		t.Fatalf("signal %s has at %q, want RFC 3339: %v", s.Signal, s.At, err)
	}
	return at
}

// AC4: an interrupt sends SIGINT, then SIGTERM, then SIGKILL, each after a
// bounded wait on the server clock.
func TestAdoptInterruptLadder(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	f := a.fake("app", "INT", "TERM")
	f.openTool(a.i.Home, adModelSn, "busy")

	done := make(chan harness.Response, 1)
	go func() {
		done <- a.i.SocketHTTP(a.i.Credential("cos")).Do("POST", adDoor, map[string]any{"claudeSessionId": f.sid, "interrupt": true}, harness.ReqOpts{Trace: harness.NewTrace(t)})
	}()
	f.signalAt(f.expect("signal INT"), "INT")
	a.i.ClockAdvance(adInterruptWait)
	f.signalAt(f.expect("signal TERM"), "TERM")
	a.i.ClockAdvance(adTermWait)
	f.gone()
	if !f.ws.Signaled() || f.ws.Signal() != syscall.SIGKILL {
		t.Fatalf("the fake ended with %v, want death by SIGKILL", f.ws)
	}
	var resp harness.Response
	select {
	case resp = <-done:
	case <-time.After(osBound):
		t.Fatalf("the adopt door did not answer within %v of the kill", osBound)
	}
	res := adCreated(t, resp)
	if !res.Interrupted {
		t.Fatalf("result = %+v, want interrupted true", res)
	}

	intr := adCompletion(t, a.i, "session_interrupt")
	adopt := adCompletion(t, a.i, "session_adopt")
	if len(intr) != 1 || intr[0].Signal != "INT" || len(adopt) != 2 || adopt[0].Signal != "TERM" || adopt[1].Signal != "KILL" {
		t.Fatalf("completion signals: interrupt %+v, adopt %+v, want [INT] then [TERM KILL]", intr, adopt)
	}
	if d := adAt(t, adopt[0]).Sub(adAt(t, intr[0])); d < adInterruptWait {
		t.Fatalf("TERM came %v after INT, want at least %v", d, adInterruptWait)
	}
	if d := adAt(t, adopt[1]).Sub(adAt(t, adopt[0])); d < adTermWait {
		t.Fatalf("KILL came %v after TERM, want at least %v", d, adTermWait)
	}
}

// AC5: after an interrupted takeover the first message names the open tool
// call and asks the agent to check what it touched.
func TestAdoptRecoveryMessageNamesOpenTool(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	f := a.fake("app")
	f.openTool(a.i.Home, adModelSn, "busy")

	res := adCreated(t, a.adopt(map[string]any{"claudeSessionId": f.sid, "interrupt": true}))
	if !res.Interrupted || !res.RecoverySent {
		t.Fatalf("result = %+v, want interrupted true and recoverySent true", res)
	}
	f.signalAt(f.expect("signal INT"), "INT")
	a.i.MustCLI("session", "wait", "--id", res.SessionID, "--json")

	in := adInputs(a.i)
	if len(in) != 1 {
		t.Fatalf("the agent has %d user inputs, want the one recovery message", len(in))
	}
	input, _ := json.Marshal(adToolInput)
	for _, want := range []string{"Bash", adToolID, string(input), adPrompt, "Check whether it took effect"} {
		if !strings.Contains(in[0].Text, want) {
			t.Fatalf("the first message lacks %q: %q", want, in[0].Text)
		}
	}
	rows := g4Wait(t, a.i, g4Has(func(r g4Row) bool {
		return r.obj("args").str("session_id") == res.SessionID && r.obj("args").str("origin") == "relay-recovery"
	}), "--event", "session_message")
	if n := len(g4Where(rows, func(r g4Row) bool { return r.obj("args").str("origin") == "relay-recovery" })); n == 0 {
		t.Fatalf("no session_message row with origin relay-recovery for %s", res.SessionID)
	}
}

// AC6: a match whose process start time differs from the registry entry is
// never adopted.
func TestAdoptStartMismatchNeverAdopted(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	f := a.fake("app", "INT", "TERM")
	f.idle(a.i.Home, adModelSn)
	f.register(a.i.Home, "idle", time.Now(), func(e map[string]any) {
		e["procStart"] = f.start.Add(time.Hour).Format("Mon Jan 2 15:04:05 2006")
	})

	s, frame := a.probeFrame()
	live := s.liveAt(f.dir)
	if live == nil {
		t.Fatalf("no live row for %s; snapshot %+v", f.dir, s)
	}
	if row := osRowByID(frame, live.ID); row["adoptable"] != false || row["adoptReason"] != "start_mismatch" {
		t.Fatalf("row = %v, want adoptable false and adoptReason start_mismatch", row)
	}
	otRefusal(t, a.adopt(map[string]any{"outsideId": live.ID, "interrupt": true}), http.StatusConflict, "start_mismatch")
	otRefusal(t, a.adopt(map[string]any{"claudeSessionId": f.sid, "interrupt": true}), http.StatusConflict, "start_mismatch")
	adNoSignals(t, f)
	if n := len(adSessions(t, a.i)); n != 0 {
		t.Fatalf("refused adopts left %d sessions", n)
	}
}

// Epic: an ambiguous match is never adopted.
func TestAdoptAmbiguousNeverAdopted(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	dir := a.folder("app")
	sid := adUUID()
	f1 := adSpawn(t, dir, sid, "INT", "TERM")
	f2 := adSpawn(t, dir, sid, "INT", "TERM")
	f1.idle(a.i.Home, adModelSn)
	f2.register(a.i.Home, "idle", time.Now())

	s, frame := a.probeFrame()
	var ids []string
	for _, row := range s.Sessions {
		if row.Directory == dir && row.State != "ended" {
			ids = append(ids, row.ID)
		}
	}
	if len(ids) != 2 {
		t.Fatalf("%d live rows for %s, want 2: %+v", len(ids), dir, s)
	}
	for _, id := range ids {
		if row := osRowByID(frame, id); row["adoptable"] != false || row["adoptReason"] != "ambiguous" {
			t.Fatalf("row = %v, want adoptable false and adoptReason ambiguous", row)
		}
		otRefusal(t, a.adopt(map[string]any{"outsideId": id, "interrupt": true}), http.StatusConflict, "ambiguous")
	}
	adNoSignals(t, f1, f2)
}

// Door: the Chief of Staff scope cannot reach the adopt door, and a
// read-only credential is refused; the process lives.
func TestAdoptScopeRefused(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	f := a.fake("app", "INT", "TERM")
	f.idle(a.i.Home, adModelSn)
	body := map[string]any{"claudeSessionId": f.sid, "interrupt": true}

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

// AC7: each adopt, interrupt and recovery message writes an intent row before
// it acts; completions follow. No clock move: audit time is real time here.
func TestAdoptAuditRowsBeforeAct(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	f := a.fake("app")
	f.openTool(a.i.Home, adModelSn, "busy")

	res := adCreated(t, a.adopt(map[string]any{"claudeSessionId": f.sid, "interrupt": true}))
	a.i.MustCLI("session", "wait", "--id", res.SessionID, "--json")
	firstSignal := f.signalAt(f.expect("signal INT"), "INT")
	var firstInput time.Time
	for _, c := range a.i.FakeAgentCalls("claude") {
		if c.Event == "input" {
			firstInput = c.TS
			break
		}
	}
	if firstInput.IsZero() {
		t.Fatalf("the fake agent logged no input")
	}

	rowTime := func(r g4Row) time.Time {
		at, err := time.Parse(time.RFC3339Nano, r.str("ts"))
		if err != nil {
			t.Fatalf("audit row ts %q: %v", r.str("ts"), err)
		}
		return at
	}
	for _, ev := range []string{"session_adopt", "session_interrupt"} {
		rows := g4Wait(t, a.i, g4Has(func(r g4Row) bool { return r.str("phase") == "completion" }), "--event", ev)
		intents := g4Where(rows, func(r g4Row) bool { return r.str("phase") == "intent" })
		comps := g4Where(rows, func(r g4Row) bool { return r.str("phase") == "completion" })
		if len(intents) != 1 || len(comps) != 1 {
			t.Fatalf("%s: %d intent and %d completion rows, want one each", ev, len(intents), len(comps))
		}
		if intents[0].str("outcome") != "pending" || comps[0].str("outcome") != "ok" {
			t.Fatalf("%s: outcomes %q then %q, want pending then ok", ev, intents[0].str("outcome"), comps[0].str("outcome"))
		}
		if rowTime(intents[0]).After(firstSignal) {
			t.Fatalf("%s intent at %v is after the first signal at %v", ev, rowTime(intents[0]), firstSignal)
		}
		if rowTime(comps[0]).Before(rowTime(intents[0])) {
			t.Fatalf("%s completion precedes its intent", ev)
		}
	}
	msgs := g4Wait(t, a.i, g4Has(func(r g4Row) bool {
		return r.obj("args").str("origin") == "relay-recovery" && r.str("phase") == "intent"
	}), "--event", "session_message")
	msgs = g4Where(msgs, func(r g4Row) bool {
		return r.obj("args").str("origin") == "relay-recovery" && r.str("phase") == "intent"
	})
	if len(msgs) != 1 || msgs[0].str("outcome") != "pending" {
		t.Fatalf("recovery intent rows = %v, want one pending", msgs)
	}
	if rowTime(msgs[0]).After(firstInput) {
		t.Fatalf("recovery intent at %v is after the agent's first input at %v", rowTime(msgs[0]), firstInput)
	}
}

// AC7 (fail closed): with auditing off the door refuses and signals nothing.
func TestAdoptRefusedWithAuditingOff(t *testing.T) {
	t.Parallel()
	// Auditing is off from boot, so the project gate cannot record a project:
	// the door refuses before it looks for one.
	a := osStart(t, false, func(o *harness.Options) {
		if o.Settings == nil {
			o.Settings = map[string]json.RawMessage{}
		}
		o.Settings["audit"] = json.RawMessage(`{"enabled": false}`)
	})
	f := adSpawn(t, osDir(t, t.TempDir(), "app"), adUUID(), "INT", "TERM")
	f.openTool(a.i.Home, adModelSn, "busy")

	otRefusal(t, a.adopt(map[string]any{"claudeSessionId": f.sid, "interrupt": true}), http.StatusServiceUnavailable, "audit_unavailable")
	adNoSignals(t, f)
	if n := len(adSessions(t, a.i)); n != 0 {
		t.Fatalf("a refused adopt left %d sessions", n)
	}
}
