package features

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"relaye2e/harness"
)

// A local Claude session runs under the runner of docs/session-host.md
// ("claude"), in <config dir>/sessions/runners/<session id>/<generation>/. The
// agent outlives relay, so these tests read the run directory and the process
// table with kill(2) and ps, as the detached-host tests do.

// g1lClaudeTemplate is a sandboxed claude-code template that grants the
// project folder, so a control command can prove a sandboxed write works.
const g1lClaudeTemplate = `[{"id":"claude-code","name":"Claude Code","command":"claude","sandbox":true,
 "read_write":["~/.claude","${PROJECT_PATH}"],"read":["~/.local/bin"]}]`

// g1lRun is the part of runner.json these tests read.
type g1lRun struct {
	PID        int    `json:"pid"`
	PGID       int    `json:"pgid"`
	Started    string `json:"started"`
	Session    string `json:"session"`
	Generation string `json:"generation"`
}

// g1lRunnersDir is the directory that holds one directory per generation.
func g1lRunnersDir(i *harness.Instance, sid string) string {
	return filepath.Join(i.ConfigDir, "sessions", "runners", sid)
}

// g1lGenerations lists the session's generation directories, oldest first.
func g1lGenerations(t *testing.T, i *harness.Instance, sid string) []string {
	t.Helper()
	entries, err := os.ReadDir(g1lRunnersDir(i, sid))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading %s: %v", g1lRunnersDir(i, sid), err)
	}
	var gens []string
	for _, e := range entries {
		if e.IsDir() {
			gens = append(gens, e.Name())
		}
	}
	sort.Slice(gens, func(a, b int) bool {
		if len(gens[a]) != len(gens[b]) {
			return len(gens[a]) < len(gens[b])
		}
		return gens[a] < gens[b]
	})
	return gens
}

// g1lReadRun reads runner.json of one generation.
func g1lReadRun(dir string) (g1lRun, error) {
	var run g1lRun
	raw, err := os.ReadFile(filepath.Join(dir, "runner.json"))
	if err != nil {
		return run, err
	}
	return run, json.Unmarshal(raw, &run)
}

// g1lLatest reads runner.json of the session's latest generation.
func g1lLatest(t *testing.T, i *harness.Instance, sid string) (gen string, run g1lRun) {
	t.Helper()
	gens := g1lGenerations(t, i, sid)
	if len(gens) == 0 {
		t.Fatalf("session %s has no run directory under %s", sid, g1lRunnersDir(i, sid))
	}
	gen = gens[len(gens)-1]
	run, err := g1lReadRun(filepath.Join(g1lRunnersDir(i, sid), gen))
	if err != nil {
		t.Fatalf("reading runner.json of %s/%s: %v", sid, gen, err)
	}
	if run.PID <= 0 || run.Started == "" {
		t.Fatalf("runner.json of %s/%s reads %+v, want a pid and a start time", sid, gen, run)
	}
	return gen, run
}

// g1lTree returns root and every process below it, from one snapshot of the
// process table.
func g1lTree(root int) []int {
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=").Output()
	if err != nil {
		return []int{root}
	}
	kids := map[int][]int{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		if e1 == nil && e2 == nil {
			kids[ppid] = append(kids[ppid], pid)
		}
	}
	tree := []int{root}
	for n := 0; n < len(tree); n++ {
		tree = append(tree, kids[tree[n]]...)
	}
	return tree
}

// g1lKillTree ends the runner and everything below it, as a reboot or an
// operator would, and returns once every one of them is gone.
func g1lKillTree(root int) []int {
	tree := g1lTree(root)
	for _, pid := range tree {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	return tree
}

// g1lEndAgentWhileDown stops relay, then ends the session's agent: the runner
// and every process below it. It returns once none of them runs.
func g1lEndAgentWhileDown(t *testing.T, i *harness.Instance, sid string) {
	t.Helper()
	i.Stop()
	_, run := g1lLatest(t, i, sid)
	for _, pid := range g1lKillTree(run.PID) {
		pid := pid
		g11dUntil(t, "process "+strconv.Itoa(pid)+" of the agent's tree to end", func() bool { return !g11dAlive(pid) })
	}
}

// g1lRestart restarts relay and waits on the session host's manifest
// registration that follows. The earlier boot's line is already in the log, so
// the wait is bounded below by the restart's start.
func g1lRestart(t *testing.T, i *harness.Instance) {
	t.Helper()
	since := time.Now()
	i.Restart()
	i.WaitEvent(harness.EventQuery{Key: "service.manifest.register", Since: since,
		Fields: map[string]any{"service_id": "relaysessions", "status": "ok"}}, g1tHostDeadline)
}

// g1lStart boots an instance with the sandboxed claude template, a project
// whose PreToolUse hook is relay-sessions' own hook mode and one session in
// it. A runner outlives relay, so cleanup ends whatever is left of the
// session's runners.
func g1lStart(t *testing.T, name string) (*harness.Instance, string) {
	t.Helper()
	i, sid := g1lSetup(t, name, g1lClaudeTemplate)
	t.Cleanup(func() {
		gens, err := os.ReadDir(g1lRunnersDir(i, sid))
		if err != nil {
			return
		}
		for _, g := range gens {
			if run, err := g1lReadRun(filepath.Join(g1lRunnersDir(i, sid), g.Name())); err == nil && run.PID > 0 {
				g1lKillTree(run.PID)
			}
		}
	})
	return i, sid
}

// g1lSetup is g1aSetup with the claude-code template given.
func g1lSetup(t *testing.T, name, template string) (*harness.Instance, string) {
	t.Helper()
	i := harness.Start(t, harness.Options{
		Credentials: g1tCreds, Presence: g1tApprove,
		Settings: map[string]json.RawMessage{"terminal_templates": json.RawMessage(template)},
	})
	i.WaitSessionHost(g1tHostDeadline)
	p := g1tProject(t, i, name, map[string]any{"allowed_templates": []string{"claude-code"}})
	settings := map[string]any{"hooks": map[string]any{"PreToolUse": []any{map[string]any{
		"matcher": "",
		"hooks":   []any{map[string]any{"type": "command", "command": "'" + harness.BundlePaths().Sessions + "' hook"}},
	}}}}
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("encoding the hook settings: %v", err)
	}
	dir := filepath.Join(g1tDir(i, name), ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.local.json"), raw, 0o600); err != nil {
		t.Fatalf("writing the hook settings: %v", err)
	}
	return i, g1tStartSession(t, i, p.ID, "sonnet", "")
}

// g1lAgentStarts lists the argv of every start the fake claude logged.
func g1lAgentStarts(i *harness.Instance) [][]string {
	var out [][]string
	for _, c := range i.FakeAgentCalls("claude") {
		if c.Event == "start" {
			out = append(out, c.Argv)
		}
	}
	return out
}

// g1lServing counts the starts that serve a session: the stream-json ones.
func g1lServing(starts [][]string) int {
	n := 0
	for _, argv := range starts {
		for _, a := range argv {
			if a == "--input-format" {
				n++
				break
			}
		}
	}
	return n
}

// Criteria 1 and 2: the agent keeps running when relay restarts, and the next
// message is answered by the same agent with no resume.
func TestLocalRunnerSurvivesRelayRestart(t *testing.T) {
	t.Parallel()
	i, sid := g1lStart(t, "acme-survive")
	g1sSendText(t, i, sid, "hi", "echo: hi")

	gen, before := g1lLatest(t, i, sid)
	if !g11dAlive(before.PID) {
		t.Fatalf("runner pid %d is not running before the restart", before.PID)
	}
	first := g1vEventsAfter(t, i, sid, "")
	startsBefore := len(g1lAgentStarts(i))

	g1lRestart(t, i)

	list, _ := g1sListSessions(t, i)
	if row, ok := g1sFindSession(list, sid); !ok || !row.Live {
		t.Fatalf("session %s reads %+v after the restart, want a live row", sid, row)
	}
	gen2, after := g1lLatest(t, i, sid)
	if gen2 != gen || after.PID != before.PID || after.Started != before.Started {
		t.Fatalf("runner.json went from %s %+v to %s %+v across the restart, want the same generation, pid and start time", gen, before, gen2, after)
	}
	if !g11dAlive(before.PID) {
		t.Fatalf("runner pid %d is not running after the restart", before.PID)
	}

	g1sSendText(t, i, sid, "after", "echo: after")

	if gens := g1lGenerations(t, i, sid); len(gens) != 1 || gens[0] != gen {
		t.Fatalf("generations after the restart are %v, want only %s", gens, gen)
	}
	if now := len(g1lAgentStarts(i)); now != startsBefore {
		t.Fatalf("the fake agent logged %d starts after the restart, %d before: a second agent started", now, startsBefore)
	}
	if n := g1lServing(g1lAgentStarts(i)); n != 1 {
		t.Fatalf("%d agents served the session, want 1", n)
	}
	if got := i.Events(harness.EventQuery{Key: "session.resume"}); len(got) != 0 {
		t.Fatalf("the message after the restart caused %d session.resume lines, want none", len(got))
	}
	if rows := i.Audit(harness.AuditQuery{Event: "session_launch", Outcome: "ok"}); len(rows) != 1 {
		t.Fatalf("audit holds %d ok session_launch rows, want 1", len(rows))
	}
	if rows := i.Audit(harness.AuditQuery{Event: "session_resume"}); len(rows) != 0 {
		t.Fatalf("audit holds %d session_resume rows, want none", len(rows))
	}

	// A cursor read before the restart still works after it: the reply to the
	// new message arrives once, and the earlier one not at all.
	second := g1vEventsAfter(t, i, sid, first.Next)
	if g1vAssistantCount(second.Events, "echo: after") != 1 || g1vAssistantCount(second.Events, "echo: hi") != 0 {
		t.Fatalf("events after the saved cursor are %s, want the reply to the new message once and none from before", g1vJoin(second.Events))
	}
}

// Criterion 7: an agent that exited while relay was down reads dormant, and a
// resume brings it back on its conversation.
func TestLocalRunnerExitedReadsDormant(t *testing.T) {
	t.Parallel()
	i, sid := g1lStart(t, "acme-exited")
	g1sSendText(t, i, sid, "hi", "echo: hi")
	before, _ := g1sListSessions(t, i)
	beforeRow, ok := g1sFindSession(before, sid)
	if !ok {
		t.Fatalf("session list holds no %s before the restart", sid)
	}
	var conversation string
	for _, argv := range g1lAgentStarts(i) {
		for n, a := range argv {
			if a == "--session-id" && n+1 < len(argv) {
				conversation = argv[n+1]
			}
		}
	}
	if conversation == "" {
		t.Fatalf("no start of the fake agent carries --session-id")
	}

	g1lEndAgentWhileDown(t, i, sid)
	g1lRestart(t, i)

	dormant, _ := g1sListSessions(t, i)
	row, ok := g1sFindSession(dormant, sid)
	if !ok {
		t.Fatalf("session list holds no %s after the restart", sid)
	}
	if row.Live {
		t.Fatalf("a session whose agent ended while relay was down reads live")
	}

	res := i.MustCLI("session", "resume", "--id", sid, "--json")
	var resumed struct {
		SessionID string `json:"session_id"`
		Resumed   bool   `json:"resumed"`
	}
	res.JSON(t, &resumed)
	if !resumed.Resumed || resumed.SessionID != sid {
		t.Fatalf("resume printed %+v, want resumed true for %s", resumed, sid)
	}
	g1sEvent(t, i, harness.EventQuery{Key: "session.resume", Trace: res.Trace,
		Fields: map[string]any{"status": "ok", "session_id": sid}})

	g1sSendText(t, i, sid, "again", "echo: again")
	found := false
	for _, argv := range g1lAgentStarts(i) {
		if agHasPair(argv, "--resume", conversation) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no agent started with --resume %s after the resume", conversation)
	}
	if gens := g1lGenerations(t, i, sid); len(gens) != 2 {
		t.Fatalf("generations after the resume are %v, want 2", gens)
	}
	after, _ := g1sListSessions(t, i)
	if afterRow, ok := g1sFindSession(after, sid); !ok || !afterRow.Live || afterRow.MessageCount < beforeRow.MessageCount {
		t.Fatalf("resumed row %+v, want a live session with at least the %d messages from before", afterRow, beforeRow.MessageCount)
	}
}

// g1lHookPost posts a permission request to relay-sessions' hook socket from
// this process, which is outside the agent's process tree, and returns the
// HTTP status.
func g1lHookPost(t *testing.T, i *harness.Instance, sid string) int {
	t.Helper()
	sock := filepath.Join(i.ConfigDir, "relaysessions-hook.sock")
	client := &http.Client{
		Timeout: g1tFrameDeadline,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}},
	}
	body, _ := json.Marshal(map[string]any{
		"sessionId": sid, "toolName": "Bash", "toolInput": map[string]any{}, "toolUseId": "toolu_acme",
	})
	req, err := http.NewRequest("POST", "http://relaysessions/permission", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("building the hook request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("posting to %s: %v", sock, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// Criterion 4: the permission hook of the surviving agent still reaches
// relay's check after a restart, and a caller outside the agent's tree is
// refused.
func TestLocalRunnerHookRootAfterRestart(t *testing.T) {
	t.Parallel()
	i, sid := g1lStart(t, "acme-hookroot")
	g1sSendText(t, i, sid, "hi", "echo: hi")

	g1lRestart(t, i)

	// A caller the walk cannot place gets 403, from this process and from the
	// real hook client, which turns it into a deny.
	if got := g1lHookPost(t, i, sid); got != http.StatusForbidden {
		t.Fatalf("a hook call from outside the agent's process tree answered %d, want 403", got)
	}
	hook := exec.Command(harness.BundlePaths().Sessions, "hook")
	hook.Env = []string{
		"PATH=/usr/bin:/bin",
		"RELAY_SESSIONS_HOOK_SOCKET=" + filepath.Join(i.ConfigDir, "relaysessions-hook.sock"),
		"RELAY_SESSION_ID=" + sid,
	}
	hook.Stdin = strings.NewReader(`{"tool_name":"Bash","tool_input":{},"tool_use_id":"toolu_acme"}`)
	out, err := hook.Output()
	if err != nil {
		t.Fatalf("the hook client exited with %v, want 0 and a deny", err)
	}
	var decided struct {
		Specific struct {
			Decision string `json:"permissionDecision"`
		} `json:"hookSpecificOutput"`
	}
	if json.Unmarshal(out, &decided) != nil || decided.Specific.Decision != "deny" {
		t.Fatalf("a hook run outside the agent's process tree printed %q, want a deny", out)
	}

	// The agent's own hook reaches the check: the tool call that needs a
	// person reads asking, and an approval lets it through.
	since := time.Now()
	g1vSend(t, i, sid, "!tool Bash")
	i.WaitEvent(harness.EventQuery{Key: "session.state", Since: since,
		Fields: map[string]any{"session_id": sid, "to": "asking"}}, g1tEventDeadline)
	if res, w := g1vWaitFor(t, i, sid); res.Code != 1 || w.State != "asking" {
		t.Fatalf("wait on the prompted tool call exited %d with state %q, want 1 and asking", res.Code, w.State)
	}
	ws, req := g1aReplay(t, i, sid)
	defer ws.Close()
	ws.Send(map[string]any{"type": "permission_response", "permissionId": req["permissionId"], "approved": true})
	if res, w := g1vWaitFor(t, i, sid); res.Code != 0 || w.State != "idle" || !strings.HasPrefix(w.Excerpt, "hook: allow") {
		t.Fatalf("wait after the approval exited %d with state %q and excerpt %q, want 0, idle and a hook allow", res.Code, w.State, w.Excerpt)
	}
}

// localEventLine is one line of events.jsonl.
type g1lEventLine struct {
	V          int    `json:"v"`
	Type       string `json:"type"`
	Session    string `json:"session"`
	Generation string `json:"generation"`
}

// Criterion 5: the runner writes an asking line when the session waits on a
// permission. The state change follows the line, so a read after the event
// needs no wait.
func TestLocalRunnerAskingEventLine(t *testing.T) {
	t.Parallel()
	i, sid := g1lStart(t, "acme-asking")
	gen, _ := g1lLatest(t, i, sid)

	since := time.Now()
	g1vSend(t, i, sid, "!tool Bash")
	i.WaitEvent(harness.EventQuery{Key: "session.state", Since: since,
		Fields: map[string]any{"session_id": sid, "to": "asking"}}, g1tEventDeadline)

	raw, err := os.ReadFile(filepath.Join(g1lRunnersDir(i, sid), gen, "events.jsonl"))
	if err != nil {
		t.Fatalf("reading events.jsonl: %v", err)
	}
	asking := 0
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		var e g1lEventLine
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("events.jsonl holds a line that is not JSON: %v\n%s", err, raw)
		}
		if e.Type != "asking" {
			continue
		}
		asking++
		if e.V != 1 || e.Session != sid || e.Generation != gen {
			t.Fatalf("asking line %+v, want v 1, session %s and generation %s", e, sid, gen)
		}
	}
	if asking != 1 {
		t.Fatalf("events.jsonl holds %d asking lines after the session asked once, want 1\n%s", asking, raw)
	}
}

// Criterion 6: a sandboxed session reads and writes no file in any runner
// directory. A write into the project folder, which the template grants, shows
// the commands run.
func TestLocalRunnerDirOutsideSandbox(t *testing.T) {
	t.Parallel()
	i, sid := g1lStart(t, "acme-sandboxed")
	gen, _ := g1lLatest(t, i, sid)
	runnersBase := filepath.Join(i.ConfigDir, "sessions", "runners")
	runDir := filepath.Join(runnersBase, sid, gen)
	if _, err := os.Stat(filepath.Join(runDir, "runner.json")); err != nil {
		t.Fatalf("the run directory has no runner.json to read: %v", err)
	}

	say := func(cmd string) string {
		t.Helper()
		var reply struct {
			Text string `json:"text"`
		}
		i.MustCLI("session", "message", "--id", sid, "--text", "!sh "+cmd, "--json").JSON(t, &reply)
		return reply.Text
	}

	project := g1tDir(i, "acme-sandboxed")
	if got := say("/usr/bin/touch " + agShQuote(filepath.Join(project, "control"))); got != "exit: 0" {
		t.Fatalf("a write to the project folder answered %q, want exit: 0", got)
	}
	if _, err := os.Stat(filepath.Join(project, "control")); err != nil {
		t.Fatalf("the control write left no file: %v", err)
	}

	for name, cmd := range map[string]string{
		"read of runner.json":       "/bin/cat " + agShQuote(filepath.Join(runDir, "runner.json")),
		"listing of the generation": "/bin/ls " + agShQuote(runDir),
		"listing of the session":    "/bin/ls " + agShQuote(filepath.Join(runnersBase, sid)),
		"listing of runners/":       "/bin/ls " + agShQuote(runnersBase),
	} {
		if got := say(cmd); got == "exit: 0" {
			t.Errorf("a %s answered exit: 0, want a refusal", name)
		}
	}
	for _, probe := range []string{
		filepath.Join(runDir, "probe"),
		filepath.Join(runDir, "inbox", "probe"),
		filepath.Join(runnersBase, sid, "probe"),
		filepath.Join(runnersBase, "probe"),
	} {
		if got := say("/usr/bin/touch " + agShQuote(probe)); got == "exit: 0" {
			t.Errorf("a write to %s answered exit: 0, want a refusal", probe)
		}
		if _, err := os.Stat(probe); err == nil {
			t.Errorf("the sandboxed write left %s", probe)
		}
	}
}
