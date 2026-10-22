package features

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"relaye2e/harness"
)

// The swap rows of G1.16 for Pi and Codex sessions: a drop-in swaps a headless
// Pi or Codex session to a terminal that runs the project's template on the
// same conversation, and closing the terminal swaps it back with no further
// message (docs/session-host.md, "Drop-in"; docs/routes.md; docs/cli.md,
// "relay drop-in"). The fake pi and fake codex have a terminal persona: they
// log tui_ready once the terminal is up, log one input line per line typed in
// the terminal, and end on "/quit". The fake daemon has the same persona for
// `codex resume --remote`, named codex-tui in the daemon's log.

var (
	g1xUUID     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	g1xUUIDIn   = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	g1xHeadless = `{"headless":true,"agent":true}`
)

// g1xRig is one instance with one headless session of one kind that has run a
// turn, so the session has a conversation.
type g1xRig struct {
	t       *testing.T
	i       *harness.Instance
	kind    string // "pi" or "codex"
	dir     string // the project folder
	project string
	sid     string
	conv    string // the Pi session id or the Codex thread id
}

type g1xOpts struct {
	auditOff bool
	// templates replaces the harness's templates of the same ids.
	templates string
	// allowed is the project's allowed_templates; the kind's template when empty.
	allowed []string
}

func g1xStart(t *testing.T, kind string, o g1xOpts) *g1xRig {
	t.Helper()
	opts := harness.Options{Credentials: g1tCreds, Presence: g1tApprove}
	if kind == "pi" {
		opts.FakeModelHost = fakeEchoModels()
	}
	if o.templates != "" {
		opts.Settings = map[string]json.RawMessage{"terminal_templates": json.RawMessage(o.templates)}
	}
	i := harness.Start(t, opts)
	if kind == "pi" {
		i.WaitModelHost(g1tHostDeadline)
	}
	i.WaitSessionHost(g1tHostDeadline)
	allowed := o.allowed
	if allowed == nil {
		allowed = []string{kind}
	}
	p := g1tProject(t, i, "acme-swap-"+kind, map[string]any{"allowed_templates": allowed})
	if o.auditOff {
		g1xAuditOff(t, i)
	}
	model := "codex/fake-echo"
	if kind == "pi" {
		model = ""
		_, ids := modelIDs(t, i)
		for _, id := range ids {
			if strings.HasPrefix(id, "pi/") && strings.HasSuffix(id, "/fake-echo") {
				model = id
			}
		}
		if model == "" {
			t.Fatalf("model list %v holds no pi/<provider>/fake-echo", ids)
		}
	}
	r := &g1xRig{t: t, i: i, kind: kind, dir: g1tDir(i, "acme-swap-"+kind), project: p.ID}
	r.sid = g1tStartSession(t, i, p.ID, model, g1xHeadless)
	g1sSendText(t, i, r.sid, "acme one", "echo: acme one")
	r.conv = r.conversation()
	return r
}

// g1xAuditOff turns auditing off in settings.json and restarts. Creating a
// project is a gated write that auditing off refuses, so the project exists
// first.
func g1xAuditOff(t *testing.T, i *harness.Instance) {
	t.Helper()
	i.Stop()
	path := filepath.Join(i.ConfigDir, "settings.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading settings.json: %v", err)
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("decoding settings.json: %v", err)
	}
	settings["audit"] = json.RawMessage(`{"enabled": false}`)
	if raw, err = json.Marshal(settings); err != nil {
		t.Fatalf("encoding settings.json: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("writing settings.json: %v", err)
	}
	i.Restart()
	i.WaitSessionHost(g1tHostDeadline)
}

func g1xArgv(argv []string, flag string) string {
	for k := 0; k+1 < len(argv); k++ {
		if argv[k] == flag {
			return argv[k+1]
		}
	}
	return ""
}

// g1xHeadlessStarts are the agent processes relay ran without a terminal.
func g1xHeadlessStarts(i *harness.Instance, kind string) [][]string {
	var out [][]string
	for _, argv := range agStartArgvs(i, kind) {
		if (kind == "pi" && slices.Contains(argv, "--mode")) || (kind == "codex" && len(argv) > 0 && argv[0] == "app-server") {
			out = append(out, argv)
		}
	}
	return out
}

// g1xTerminalStarts are the agent processes a drop-in terminal ran.
func g1xTerminalStarts(i *harness.Instance, kind string) [][]string {
	var out [][]string
	for _, argv := range agStartArgvs(i, kind) {
		if (kind == "pi" && !slices.Contains(argv, "--mode")) || (kind == "codex" && len(argv) > 0 && argv[0] == "resume") {
			out = append(out, argv)
		}
	}
	return out
}

func g1xCalls(i *harness.Instance, persona, event string) []harness.AgentCall {
	var out []harness.AgentCall
	for _, c := range i.FakeAgentCalls(persona) {
		if c.Event == event {
			out = append(out, c)
		}
	}
	return out
}

// conversation is the Pi session id the headless start named, or the Codex
// thread id the fake logged.
func (r *g1xRig) conversation() string {
	r.t.Helper()
	if r.kind == "pi" {
		starts := g1xHeadlessStarts(r.i, "pi")
		if len(starts) == 0 {
			r.t.Fatalf("the session made no pi start")
		}
		if id := g1xUUIDIn.FindString(g1xArgv(starts[0], "--session")); id != "" {
			return id
		}
		// A new session has no --session: pi names it, and its file in
		// --session-dir carries the id.
		files, _ := filepath.Glob(filepath.Join(g1xArgv(starts[0], "--session-dir"), "*.jsonl"))
		if len(files) == 1 {
			if id := g1xUUIDIn.FindString(filepath.Base(files[0])); id != "" {
				return id
			}
		}
		r.t.Fatalf("the headless pi start %v names no session id and its folder holds %v", starts[0], files)
	}
	threads := g1xCalls(r.i, "codex", "thread")
	if len(threads) == 0 || !g1xUUID.MatchString(threads[0].Text) {
		r.t.Fatalf("the fake codex logged threads %v, want a UUID thread id", threads)
	}
	return threads[0].Text
}

// waitTerminalReady waits for the n-th tui_ready line. Waits: none possible:
// the fake writes its ready line to the terminal, which the harness reads only
// when the CLI exits, so the fake's own tui_ready call-log line is polled.
func (r *g1xRig) waitTerminalReady(n int) {
	r.t.Helper()
	g11dUntil(r.t, fmt.Sprintf("tui_ready number %d of the fake %s terminal", n, r.kind), func() bool {
		return len(g1xCalls(r.i, r.kind, "tui_ready")) >= n
	})
}

// waitTerminalInput waits for the fake terminal to log the line typed in it.
func (r *g1xRig) waitTerminalInput(text string) {
	r.t.Helper()
	g11dUntil(r.t, "the fake "+r.kind+" terminal to log the input "+text, func() bool {
		for _, c := range g1xCalls(r.i, r.kind, "input") {
			if c.Text == text {
				return true
			}
		}
		return false
	})
}

// dropIn runs `relay drop-in` and returns once the fake terminal is up.
func (r *g1xRig) dropIn(readyNumber int) *harness.TTYProc {
	r.t.Helper()
	tty := r.i.StartCLITTY(harness.CLIOpts{}, r.dir, "drop-in", r.sid)
	r.i.WaitEvent(harness.EventQuery{Key: "session.drop_in",
		Fields: map[string]any{"status": "ok", "session_id": r.sid}}, g1tEventDeadline)
	r.waitTerminalReady(readyNumber)
	return tty
}

// quit ends the terminal with /quit and waits for the hand back.
func (r *g1xRig) quit(tty *harness.TTYProc) harness.Result {
	r.t.Helper()
	tty.Write([]byte("/quit\n"))
	res := tty.Wait()
	r.i.WaitEvent(harness.EventQuery{Key: "session.hand_back",
		Fields: map[string]any{"status": "ok", "session_id": r.sid}}, g1tEventDeadline)
	return res
}

// dropInHTTP posts the drop-in and decodes the answer.
func (r *g1xRig) dropInHTTP() (int, map[string]any) {
	r.t.Helper()
	resp := r.i.SocketHTTP(r.i.Credential("scoped")).Do("POST", "/api/sessions/"+r.sid+"/drop-in", map[string]any{})
	var body map[string]any
	resp.JSON(r.t, &body)
	return resp.Status, body
}

func g1xTerminalID(t *testing.T, body map[string]any) string {
	t.Helper()
	term, _ := body["terminal"].(map[string]any)
	id, _ := term["terminalId"].(string)
	if id == "" {
		t.Fatalf("the drop-in answer holds no terminal id: %v", body)
	}
	return id
}

// piSessionFiles are the files of the session in the directory.
func g1xPiFiles(t *testing.T, dir, conv string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*_"+conv+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

type g1xPiEntry struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	ParentID string `json:"parentId"`
	Message  struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	} `json:"message"`
}

func g1xPiEntries(t *testing.T, file string) []g1xPiEntry {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	var out []g1xPiEntry
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e g1xPiEntry
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("session file line %q is not JSON: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

// g1xPiTerminalArgs checks the terminal's pi argv and returns the session
// directory it names.
func g1xPiTerminalArgs(t *testing.T, argv []string, conv string) string {
	t.Helper()
	if slices.Contains(argv, "--mode") {
		t.Fatalf("the terminal's pi argv %v has --mode, want the terminal UI", argv)
	}
	session, dir := g1xArgv(argv, "--session"), g1xArgv(argv, "--session-dir")
	if g1xUUIDIn.FindString(session) != conv {
		t.Fatalf("the terminal's --session %q does not name session %s", session, conv)
	}
	if !filepath.IsAbs(dir) {
		t.Fatalf("the terminal's --session-dir %q is not absolute", dir)
	}
	if filepath.IsAbs(session) && filepath.Dir(session) != dir {
		t.Fatalf("the terminal's --session file %q is not in its --session-dir %q", session, dir)
	}
	return dir
}

// Criteria 1, 4, 8 and 10: a drop-in on a headless Pi session opens a terminal
// on the same session file, and closing it resumes the same file headless.
func TestSwapPiSameFile(t *testing.T) {
	t.Parallel()
	r := g1xStart(t, "pi", g1xOpts{})
	i := r.i

	tty := r.dropIn(1)
	starts := g1xTerminalStarts(i, "pi")
	if len(starts) != 1 {
		t.Fatalf("the drop-in made %d pi terminal starts, want 1: %v", len(starts), starts)
	}
	dir := g1xPiTerminalArgs(t, starts[0], r.conv)
	tty.Write([]byte("acme terminal\n"))
	r.waitTerminalInput("acme terminal")
	r.quit(tty)

	g1sSendText(t, i, r.sid, "acme two", "echo: acme two")
	if got := len(g1xHeadlessStarts(i, "pi")); got != 2 {
		t.Fatalf("pi headless starts are %d, want the first and the one after the hand back", got)
	}
	if argv := g1xHeadlessStarts(i, "pi")[1]; g1xUUIDIn.FindString(g1xArgv(argv, "--session")) != r.conv {
		t.Fatalf("the headless pi start after the hand back is %v, want it on session %s", argv, r.conv)
	}

	files := g1xPiFiles(t, dir, r.conv)
	if len(files) != 1 {
		t.Fatalf("session %s has %d files in %s, want 1: %v", r.conv, len(files), dir, files)
	}
	var users []string
	prev, headers := "", 0
	for _, e := range g1xPiEntries(t, files[0]) {
		if e.Type == "session" {
			headers++
			continue
		}
		if e.ParentID != prev {
			t.Fatalf("entry %s has parent %q, want %q: the file is not one chain", e.ID, e.ParentID, prev)
		}
		prev = e.ID
		if s, ok := e.Message.Content.(string); ok && e.Message.Role == "user" {
			users = append(users, s)
		}
	}
	if headers != 1 || !slices.Equal(users, []string{"acme one", "acme terminal", "acme two"}) {
		t.Fatalf("the session file holds %d headers and user entries %v, want 1 and the headless, terminal and headless entries in order", headers, users)
	}
}

// Criteria 2, 8 and 18: a drop-in on a Codex session relay started runs
// `codex resume --no-daemon <thread>`, never reaches the daemon, and the hand
// back resumes the same thread.
func TestSwapCodexRunnerNoDaemon(t *testing.T) {
	t.Parallel()
	r := ccStart(t)
	d := r.daemon()
	sid := g1tStartSession(t, r.i, r.project, "codex/fake-echo", g1xHeadless)
	g1sSendText(t, r.i, sid, "acme one", "echo: acme one")
	g := &g1xRig{t: t, i: r.i, kind: "codex", dir: r.root, project: r.project, sid: sid}
	g.conv = g.conversation()

	tty := g.dropIn(1)
	starts := g1xTerminalStarts(g.i, "codex")
	if len(starts) != 1 {
		t.Fatalf("the drop-in made %d codex terminal starts, want 1: %v", len(starts), starts)
	}
	argv := starts[0]
	if argv[0] != "resume" || !slices.Contains(argv, "--no-daemon") || argv[len(argv)-1] != g.conv || slices.Contains(argv, "--remote") {
		t.Fatalf("the codex terminal argv is %v, want resume --no-daemon ... %s with no --remote", argv, g.conv)
	}
	if !agHasPair(argv, "--sandbox", "danger-full-access") {
		t.Fatalf("the sandboxed codex terminal argv %v lacks --sandbox danger-full-access", argv)
	}
	g.quit(tty)

	g1sSendText(t, g.i, sid, "acme two", "echo: acme two")
	for _, th := range g1xCalls(g.i, "codex", "thread") {
		if th.Text != g.conv {
			t.Fatalf("the fake codex saw thread %s, want only %s across the swap", th.Text, g.conv)
		}
	}
	d.barrier()
	if m := d.mutations(); len(m) != 0 {
		t.Fatalf("Relay sent the daemon %v during a Relay-started drop-in", m)
	}
	for _, l := range d.snapshot() {
		if l["event"] == "request" && l["client"] != "relay" {
			t.Fatalf("a client other than Relay used the codex daemon during a Relay-started drop-in: %v", l)
		}
		if ccParams(l)["threadId"] == g.conv {
			t.Fatalf("the daemon was asked about the session's thread during a Relay-started drop-in: %v", l)
		}
	}
	if !d.alive() {
		t.Fatalf("the fake codex daemon ended")
	}
}

// g1xBuildDaemon builds the fake daemon where a template can name it.
func g1xBuildDaemon(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fakecodexdaemon")
	build := exec.Command("go", "build", "-o", bin, "./fakes/fakecodexdaemon")
	build.Dir = filepath.Join(harness.RepoRoot(), "e2e")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake codex daemon: %v\n%s", err, out)
	}
	return bin
}

// g1xAttach attaches Relay to an idle thread of a fake daemon whose
// `codex resume --remote` terminal is the fake daemon's own terminal persona.
func g1xAttach(t *testing.T) (*ccRig, *ccProc, ccThread, *g1xRig) {
	t.Helper()
	bin := g1xBuildDaemon(t)
	r := ccStart(t, func(o *harness.Options) {
		o.Settings["terminal_templates"] = json.RawMessage(fmt.Sprintf(
			`[{"id":"codex","name":"Codex","command":%q,"sandbox":false}]`, bin))
	})
	d := r.daemon()
	th := r.thread("swap-attach")
	d.add(th)
	sid := r.attached(th)
	g := &g1xRig{t: t, i: r.i, kind: "codex", dir: r.root, project: r.project, sid: sid, conv: th.ID}
	g1sSendText(t, r.i, sid, "acme before", "echo: acme before")
	return r, d, th, g
}

// g1xResumeLog reads the terminal persona's log of the fake daemon.
func g1xResumeLog(t *testing.T, i *harness.Instance) []map[string]any {
	t.Helper()
	var out []map[string]any
	_ = filepath.WalkDir(i.Tmp, func(path string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() || e.Name() != "fakecodexdaemon-resume.jsonl" {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("reading %s: %v", path, rerr)
		}
		for _, line := range bytes.Split(raw, []byte("\n")) {
			var v map[string]any
			if json.Unmarshal(line, &v) == nil {
				out = append(out, v)
			}
		}
		return nil
	})
	return out
}

// subscribed waits for the terminal persona's thread/resume at the daemon.
func (d *ccProc) tuiSubscribed() {
	d.until("the codex terminal's subscription", func(l map[string]any) bool {
		return l["event"] == "subscribed" && l["client"] == "codex-tui"
	})
}

func g1xSubscribers(d *ccProc, id string) []string {
	ack := d.do(map[string]any{"cmd": "status", "id": id})
	var out []string
	subs, _ := ack["subscribers"].([]any)
	for _, s := range subs {
		out = append(out, fmt.Sprint(s))
	}
	return out
}

// g1xDaemonIndex is the position of the first daemon line after from that pred accepts.
func g1xDaemonIndex(lines []map[string]any, from int, pred func(map[string]any) bool) int {
	for k := from; k < len(lines); k++ {
		if pred(lines[k]) {
			return k
		}
	}
	return -1
}

func g1xRequest(client, method string) func(map[string]any) bool {
	return func(l map[string]any) bool {
		return l["event"] == "request" && l["client"] == client && l["method"] == method
	}
}

// Criteria 3, 5, 9, 12, 13 and 18: a drop-in on an attached Codex thread runs
// `codex resume --remote unix://<socket> <thread>` with no other flag after
// Relay lets go of its own connection, leaves the daemon's other client alone,
// and the hand back attaches again through the adopt intent.
func TestSwapCodexAttachRemote(t *testing.T) {
	t.Parallel()
	r, d, th, g := g1xAttach(t)
	other := r.terminal(th)

	tty := g.dropIn(0)
	d.tuiSubscribed()

	start := g1xResumeLog(t, r.i)
	if len(start) == 0 || start[0]["event"] != "start" {
		t.Fatalf("the codex terminal log is %v, want a start line first", start)
	}
	var argv []string
	raw, _ := json.Marshal(start[0]["argv"])
	_ = json.Unmarshal(raw, &argv)
	sock := filepath.Join(r.i.Home, ".codex", "app-server-control", "app-server-control.sock")
	if len(argv) != 4 || argv[0] != "resume" || argv[1] != "--remote" || argv[3] != th.ID || !strings.HasPrefix(argv[2], "unix://") {
		t.Fatalf("the codex terminal argv is %v, want exactly resume --remote unix://<socket> %s", argv, th.ID)
	}
	got, err1 := filepath.EvalSymlinks(strings.TrimPrefix(argv[2], "unix://"))
	want, err2 := filepath.EvalSymlinks(sock)
	if err1 != nil || err2 != nil || got != want {
		t.Fatalf("the terminal's socket is %q, want the daemon's control socket %q", argv[2], sock)
	}

	subs := g1xSubscribers(d, th.ID)
	if slices.Contains(subs, "relay") || !slices.Contains(subs, "codex-tui") || !slices.Contains(subs, "terminal-ui") {
		t.Fatalf("subscribers during the drop-in are %v, want the terminal and the other client and not relay", subs)
	}
	d.do(map[string]any{"cmd": "start_turn", "id": th.ID})
	other.until("the other client's turn/started", func(l map[string]any) bool { return l["method"] == "turn/started" })
	d.do(map[string]any{"cmd": "complete_turn", "id": th.ID})
	other.until("the other client's turn/completed", func(l map[string]any) bool { return l["method"] == "turn/completed" })

	g.quit(tty)
	if !other.alive() || !d.alive() {
		t.Fatalf("the daemon alive=%v and its other client alive=%v after the swap, want both", d.alive(), other.alive())
	}
	g1sSendText(t, r.i, g.sid, "acme after", "echo: acme after")
	if subs := g1xSubscribers(d, th.ID); !slices.Contains(subs, "relay") || slices.Contains(subs, "codex-tui") {
		t.Fatalf("subscribers after the hand back are %v, want relay and not the terminal", subs)
	}

	lines := d.snapshot()
	for _, m := range ccMethods(d.relay()) {
		if !slices.Contains(ccAllowed, m) {
			t.Fatalf("Relay sent the daemon %q, which is outside the allowlist", m)
		}
	}
	tuiResume := g1xDaemonIndex(lines, 0, g1xRequest("codex-tui", "thread/resume"))
	back := g1xDaemonIndex(lines, tuiResume, g1xRequest("relay", "thread/resume"))
	if tuiResume < 0 || back < 0 {
		t.Fatalf("the daemon log holds no terminal thread/resume (%d) followed by Relay's (%d)", tuiResume, back)
	}
	var intent map[string]any
	for _, row := range r.i.Audit(harness.AuditQuery{Event: "session_adopt"}) {
		if a, _ := row["args"].(map[string]any); row["phase"] == "intent" && a["method"] == "reattach" {
			intent = row
		}
	}
	if intent == nil {
		t.Fatalf("no session_adopt intent row with method reattach after the hand back")
	}
	if g1wTime(t, intent).After(ccTime(t, lines[back]["ts"])) {
		t.Fatalf("the reattach intent at %v came after Relay's thread/resume at %v", intent["ts"], lines[back]["ts"])
	}
}

// Criterion 5: relay lets go of the session's agent before the terminal starts.
func TestSwapAgentsStopBeforeStart(t *testing.T) {
	t.Parallel()
	t.Run("pi", func(t *testing.T) {
		t.Parallel()
		r := g1xStart(t, "pi", g1xOpts{})
		tty := r.dropIn(1)
		var exit, start *harness.AgentCall
		calls := r.i.FakeAgentCalls("pi")
		slices.SortStableFunc(calls, func(a, b harness.AgentCall) int { return a.TS.Compare(b.TS) })
		for k := range calls {
			c := &calls[k]
			if c.Event == "exit" && exit == nil {
				exit = c
			}
			if c.Event == "start" && start == nil && !slices.Contains(c.Argv, "--mode") && !slices.Contains(c.Argv, "--list-models") {
				start = c
			}
		}
		if exit == nil || start == nil || start.TS.Before(exit.TS) {
			t.Fatalf("the pi runner exit is %+v and the terminal start %+v, want the exit first", exit, start)
		}
		r.quit(tty)
	})
	t.Run("attach", func(t *testing.T) {
		t.Parallel()
		_, d, th, g := g1xAttach(t)
		tty := g.dropIn(0)
		d.tuiSubscribed()
		lines := d.snapshot()
		unsub := g1xDaemonIndex(lines, 0, g1xRequest("relay", "thread/unsubscribe"))
		resume := g1xDaemonIndex(lines, 0, g1xRequest("codex-tui", "thread/resume"))
		if unsub < 0 || resume < 0 || unsub > resume {
			t.Fatalf("Relay's thread/unsubscribe is at %d and the terminal's thread/resume at %d, want the unsubscribe first", unsub, resume)
		}
		if got := ccMethods(d.relayCalls("turn/interrupt")); len(got) != 0 {
			t.Fatalf("Relay interrupted the thread %s to drop in", th.ID)
		}
		g.quit(tty)
	})
}

// Criterion 4: the drop-in route answers a Pi or Codex session with the agent
// and its conversation.
func TestSwapAgentsHTTPBody(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"pi", "codex"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			r := g1xStart(t, kind, g1xOpts{})
			status, body := r.dropInHTTP()
			if status != http.StatusCreated {
				t.Fatalf("drop-in answered %d, want 201: %v", status, body)
			}
			if body["sessionId"] != r.sid || body["agent"] != kind || body["conversationId"] != r.conv {
				t.Fatalf("the 201 body is %v, want session %s, agent %s and conversation %s", body, r.sid, kind, r.conv)
			}
			if _, has := body["claudeSessionId"]; has {
				t.Fatalf("the 201 body of a %s drop-in carries claudeSessionId: %v", kind, body)
			}
			g1wCloseTerminal(t, r.i, r.sid, g1xTerminalID(t, body))
		})
	}
}

// Criterion 6: while the terminal holds a Pi or Codex session, a send answers
// dropped_in.
func TestSwapAgentsSendDroppedIn(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"pi", "codex"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			r := g1xStart(t, kind, g1xOpts{})
			tid := g1wDropIn(t, r.i, r.sid)
			res := r.i.CLI("session", "message", "--id", r.sid, "--text", "while held", "--json")
			if res.Code != 1 || g1vErrCode(res) != "dropped_in" {
				t.Fatalf("session message while held exited %d with code %q, want 1 and dropped_in\nstdout: %s\nstderr: %s",
					res.Code, g1vErrCode(res), res.Stdout, res.Stderr)
			}
			g1wCloseTerminal(t, r.i, r.sid, tid)
		})
	}
}

// Criterion 7: a Pi session in a turn is taken after the turn ends.
func TestSwapAgentsWaitForTurnEnd(t *testing.T) {
	t.Parallel()
	r := g1xStart(t, "pi", g1xOpts{})
	i := r.i
	ws := i.WebSocket("/ws", i.Credential("mounter"))
	defer ws.Close()
	ws.Send(map[string]any{"type": "join_session", "sessionId": r.sid})
	ws.Send(map[string]any{"type": "send_message", "sessionId": r.sid, "text": "!hold"})
	g1aNext(t, ws, g1tFrameDeadline, ccState(r.sid, "running"))

	answered := make(chan harness.Response, 1)
	go func() {
		answered <- i.SocketHTTP(i.Credential("scoped")).Do("POST", "/api/sessions/"+r.sid+"/drop-in", map[string]any{})
	}()
	// The to_terminal intent is written before the handoff waits, so a row
	// proves the request is in relay and holding for the turn.
	g4Wait(t, i, g4Has(func(row g4Row) bool {
		return row.obj("args").str("session_id") == r.sid && row.obj("args").str("direction") == "to_terminal"
	}), "--event", "session_swap")
	if got := i.Events(harness.EventQuery{Key: "session.drop_in", Fields: map[string]any{"session_id": r.sid}}); len(got) != 0 {
		t.Fatalf("the drop-in answered while the turn was still open: %v", got)
	}
	if got := g1xTerminalStarts(i, "pi"); len(got) != 0 {
		t.Fatalf("a pi terminal started while the turn was still open: %v", got)
	}

	stopped := time.Now()
	ws.Send(map[string]any{"type": "stop_generation", "sessionId": r.sid})
	resp := <-answered
	var out map[string]any
	resp.JSON(t, &out)
	if resp.Status != http.StatusCreated {
		t.Fatalf("drop-in after the turn ended answered %d: %s", resp.Status, resp.Body)
	}
	dropped := i.WaitEvent(harness.EventQuery{Key: "session.drop_in",
		Fields: map[string]any{"status": "ok", "session_id": r.sid}}, g1tEventDeadline)
	// The fake logs pi_abort before it ends the held turn, so the drop-in event
	// cannot precede it unless relay took the session mid-turn.
	aborts := g1xCalls(i, "pi", "pi_abort")
	if len(aborts) != 1 || g1wTimeOf(t, dropped.Str("ts")).Before(aborts[0].TS) || g1wTimeOf(t, dropped.Str("ts")).Before(stopped) {
		t.Fatalf("session.drop_in at %s, want it after the stop at %v and the fake's abort %v", dropped.Str("ts"), stopped, aborts)
	}
	g1wCloseTerminal(t, i, r.sid, g1xTerminalID(t, out))
}

func g1wTimeOf(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("timestamp %q does not parse: %v", s, err)
	}
	return at
}

// Criteria 11 and 18: the Pi drop-in terminal runs under the project's template
// sandbox with the grants a headless Pi session gets, and no more. A plain
// terminal of the same sandboxed template cannot open the session file.
func TestSwapPiTerminalGrant(t *testing.T) {
	t.Parallel()
	r := g1xStart(t, "pi", g1xOpts{allowed: []string{"*"}})
	i := r.i
	headless := g1xHeadlessStarts(i, "pi")[0]
	dir := g1xArgv(headless, "--session-dir")
	if want, _ := filepath.EvalSymlinks(filepath.Join(i.ConfigDir, "sessions", "pi-sessions")); dir != "" {
		got, _ := filepath.EvalSymlinks(dir)
		if got != want {
			t.Fatalf("the headless --session-dir is %q, want the pi-sessions folder under the config dir", dir)
		}
	}
	files, _ := filepath.Glob(filepath.Join(i.ConfigDir, "sessions", "pi-sessions", "*_"+r.conv+".jsonl"))
	if len(files) != 1 {
		t.Fatalf("session %s has files %v under the config dir's pi-sessions, want 1", r.conv, files)
	}

	plain := i.SocketHTTP(i.Credential("configurer")).Do("POST", "/api/terminal/templates", map[string]any{
		"id": "acme-pi-plain", "name": "Acme pi plain", "command": filepath.Join(i.Home, ".local", "bin", "pi"),
		"args": []string{"--session", files[0], "--session-dir", filepath.Dir(files[0])}, "sandbox": true})
	if plain.Status != http.StatusCreated {
		t.Fatalf("POST the plain pi template answered %d, want 201: %s", plain.Status, plain.Body)
	}
	if res := i.StartCLITTY(harness.CLIOpts{}, r.dir, "sandbox", "acme-pi-plain").Wait(); res.Code == 0 {
		t.Fatalf("a plain terminal of the sandboxed pi template opened the session file, want it withheld")
	}
	if got := len(g1xCalls(i, "pi", "tui_failed")); got != 1 || len(g1xCalls(i, "pi", "tui_ready")) != 0 {
		t.Fatalf("the plain terminal logged %d tui_failed and %d tui_ready, want 1 and 0", got, len(g1xCalls(i, "pi", "tui_ready")))
	}

	tty := r.dropIn(1)
	tty.Write([]byte("acme grant\n"))
	r.waitTerminalInput("acme grant")
	r.quit(tty)
	var users []string
	for _, e := range g1xPiEntries(t, files[0]) {
		if s, ok := e.Message.Content.(string); ok {
			users = append(users, s)
		}
	}
	if !slices.Contains(users, "acme grant") {
		t.Fatalf("the drop-in terminal's entry is missing from %s: %v", files[0], users)
	}
}

// Epic criterion 14: each direction writes an intent row with the agent before
// anything stops or starts, and a completion row with the same id.
func TestSwapAgentsAuditRows(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"pi", "codex"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			r := g1xStart(t, kind, g1xOpts{})
			tid := g1wDropIn(t, r.i, r.sid)
			g1wCloseTerminal(t, r.i, r.sid, tid)

			pair := map[string]map[string]map[string]any{}
			for _, row := range r.i.Audit(harness.AuditQuery{Event: "session_swap"}) {
				args, _ := row["args"].(map[string]any)
				if args["session_id"] != r.sid {
					continue
				}
				dir, _ := args["direction"].(string)
				phase, _ := row["phase"].(string)
				if pair[dir] == nil {
					pair[dir] = map[string]map[string]any{}
				}
				pair[dir][phase] = row
			}
			for _, dir := range []string{"to_terminal", "to_headless"} {
				intent, done := pair[dir]["intent"], pair[dir]["completion"]
				if intent == nil || done == nil || intent["id"] == "" || intent["id"] != done["id"] {
					t.Fatalf("direction %s has rows %v, want an intent and a completion with one id", dir, pair[dir])
				}
				if intent["outcome"] != "pending" || done["outcome"] != "ok" {
					t.Fatalf("%s outcomes are %v and %v, want pending and ok", dir, intent["outcome"], done["outcome"])
				}
				if args, _ := intent["args"].(map[string]any); args["agent"] != kind {
					t.Fatalf("%s intent names agent %v, want %s", dir, args["agent"], kind)
				}
			}
			var ended []map[string]any
			for _, row := range r.i.Audit(harness.AuditQuery{Event: "session_end"}) {
				if args, _ := row["args"].(map[string]any); args["session_id"] == r.sid {
					ended = append(ended, row)
				}
			}
			if len(ended) == 0 || g1wTime(t, pair["to_terminal"]["intent"]).After(g1wTime(t, ended[0])) {
				t.Fatalf("the to_terminal intent did not come before the agent's session_end: %v", ended)
			}
		})
	}
}

// Epic criterion 14: with auditing off a Pi drop-in is refused 503 and nothing
// stops.
func TestSwapAgentsRefusedWhenAuditOff(t *testing.T) {
	t.Parallel()
	r := g1xStart(t, "pi", g1xOpts{auditOff: true})
	i := r.i
	before := len(agStartArgvs(i, "pi"))

	status, body := r.dropInHTTP()
	if status != http.StatusServiceUnavailable || body["error"] != "audit_unavailable" {
		t.Fatalf("a drop-in with auditing off answered %d %v, want 503 audit_unavailable", status, body)
	}
	if res := i.StartCLITTY(harness.CLIOpts{}, r.dir, "drop-in", r.sid).Wait(); res.Code == 0 {
		t.Fatalf("relay drop-in with auditing off exited 0, want a refusal")
	}
	if row, ok := g1tSessions(t, i)[r.sid]; !ok || !row.Live {
		t.Fatalf("the session is not live after the refused drop-ins: listed=%v", ok)
	}
	g1sSendText(t, i, r.sid, "still here", "echo: still here")
	if after := len(agStartArgvs(i, "pi")); after != before {
		t.Fatalf("the refused drop-ins caused %d new pi starts, want none", after-before)
	}
}

// Criterion 15: a drop-in whose terminal cannot start resumes the session
// headless on the same conversation.
func TestSwapAgentsLaunchFailureResumes(t *testing.T) {
	t.Parallel()
	r := g1xStart(t, "pi", g1xOpts{})
	i := r.i
	broken := i.SocketHTTP(i.Credential("configurer")).Do("PUT", "/api/terminal/templates/pi",
		map[string]any{"id": "pi", "name": "Pi", "command": "/nonexistent/acme/pi", "sandbox": false})
	if broken.Status != http.StatusOK {
		t.Fatalf("PUT the pi template answered %d, want 200: %s", broken.Status, broken.Body)
	}

	status, body := r.dropInHTTP()
	if status != http.StatusBadGateway || body["error"] != "launch_failed" {
		t.Fatalf("a drop-in whose template command is missing answered %d %v, want 502 launch_failed", status, body)
	}
	i.WaitEvent(harness.EventQuery{Key: "session.hand_back",
		Fields: map[string]any{"status": "ok", "session_id": r.sid}}, g1tEventDeadline)
	g1sSendText(t, i, r.sid, "acme again", "echo: acme again")
	starts := g1xHeadlessStarts(i, "pi")
	if len(starts) != 2 || g1xUUIDIn.FindString(g1xArgv(starts[1], "--session")) != r.conv {
		t.Fatalf("pi headless starts are %v, want the session %s resumed once", starts, r.conv)
	}
	if got := g1xTerminalStarts(i, "pi"); len(got) != 0 {
		t.Fatalf("a terminal started although its command is missing: %v", got)
	}
}

// Criterion 16: a hand back of an attached thread that fails leaves the session
// dormant, and a resume after the daemon is back attaches again.
func TestSwapCodexAttachHandbackRefused(t *testing.T) {
	t.Parallel()
	r, d, th, g := g1xAttach(t)
	tty := g.dropIn(0)
	d.tuiSubscribed()

	d.kill()
	// The terminal ends on its own when its daemon connection closes.
	tty.Wait()
	back := r.i.WaitEvent(harness.EventQuery{Key: "session.hand_back", Fields: map[string]any{"session_id": g.sid}}, g1tEventDeadline)
	if back.Str("status") == "ok" {
		t.Fatalf("session.hand_back is %v with the daemon down, want a refusal", back)
	}
	var done map[string]any
	for _, row := range r.i.Audit(harness.AuditQuery{Event: "session_swap"}) {
		if a, _ := row["args"].(map[string]any); a["session_id"] == g.sid && a["direction"] == "to_headless" && row["phase"] == "completion" {
			done = row
		}
	}
	if done == nil || done["outcome"] == "ok" || done["error"] != "unavailable" {
		t.Fatalf("the to_headless completion is %v, want a failure with code unavailable", done)
	}
	if row, ok := g1tSessions(t, r.i)[g.sid]; !ok || row.Live {
		t.Fatalf("the session listed=%v live=%v after the failed hand back, want it listed and dormant", ok, row.Live)
	}

	d2 := r.daemon()
	d2.add(th)
	var resumed struct {
		Resumed bool `json:"resumed"`
	}
	r.i.MustCLI("session", "resume", "--id", g.sid, "--json").JSON(t, &resumed)
	if !resumed.Resumed {
		t.Fatalf("resume after the daemon came back printed resumed=false")
	}
	g1sSendText(t, r.i, g.sid, "acme after", "echo: acme after")
}

// Criterion 17: when the terminal closes, relay drop-in prints the command that
// resumes the session if it stays dormant.
func TestSwapAgentsCLIClosingLine(t *testing.T) {
	t.Parallel()
	r := g1xStart(t, "codex", g1xOpts{})
	tty := r.dropIn(1)
	res := r.quit(tty)
	if want := "relay session resume --id " + r.sid; !bytes.Contains(res.Stdout, []byte(want)) {
		t.Fatalf("relay drop-in printed %q, want it to name %q", res.Stdout, want)
	}
}

// Criterion 19: a Codex drop-in in an SSH host project is refused 409
// not_local and nothing stops.
func TestSwapAgentsHostedRefused(t *testing.T) {
	t.Parallel()
	r := g1cHostCodex(t)
	i := r.i
	sid := g1tStartSession(t, i, r.projectID, "codex/fake-echo", g1xHeadless)
	run := r.run(t, sid, "g1")
	g1sSendText(t, i, sid, "hi", "echo: hi")

	resp := i.SocketHTTP(i.Credential("scoped")).Do("POST", "/api/sessions/"+sid+"/drop-in", map[string]any{})
	var refused struct {
		Error string `json:"error"`
	}
	resp.JSON(t, &refused)
	if resp.Status != http.StatusConflict || refused.Error != "not_local" {
		t.Fatalf("a codex drop-in in a host project answered %d %q, want 409 not_local: %s", resp.Status, refused.Error, resp.Body)
	}
	if !g11dAlive(run.PID) {
		t.Fatalf("the host runner %d stopped although the drop-in was refused", run.PID)
	}
	g1sSendText(t, i, sid, "still here", "echo: still here")
	if rows := i.Audit(harness.AuditQuery{Event: "session_swap"}); len(rows) != 0 {
		t.Fatalf("the refused drop-in wrote %d session_swap rows, want none", len(rows))
	}
}
