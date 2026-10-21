package features

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"relaye2e/harness"
)

// G11.15: a headless Claude session on a host with tmux 3.0 or later runs its
// runner as the pane of the tmux session relay-agent-<session id>, a drop-in
// swaps that pane to Claude's UI, and the hand back swaps it to the runner
// again (docs/ssh-hosts.md, "Detached sessions"; docs/session-adoption.md,
// "Swapping the driver"). The stub host runs this machine's tmux under its own
// TMUX_TMPDIR (<remote root>/tmux), so no test touches the user's tmux server;
// the harness ends that server at teardown.

const g11pSettings = `{"headless":true,"agent":true}`

// g11pPane is what tmux reports about the pane of an agent's tmux session.
type g11pPane struct {
	SessionID string // #{session_id}: unique for the life of the tmux server
	Created   string // #{session_created}
	PID       int    // #{pane_pid}
}

// g11pTmux runs the host's tmux against the stub host's own tmux server. ok is
// false when tmux exits non-zero, which is how a missing session answers.
func g11pTmux(t *testing.T, r *g11dRig, args ...string) (out string, ok bool) {
	t.Helper()
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatalf("tmux is required for the host pane tests: %v", err)
	}
	cmd := exec.Command(bin, args...)
	// The environment is built, not inherited: a TMUX variable would aim the
	// command at the server of whatever pane runs the tests.
	cmd.Env = []string{"TMUX_TMPDIR=" + filepath.Join(r.remote, "tmux"), "PATH=" + os.Getenv("PATH"), "HOME=" + r.remote}
	b, err := cmd.Output()
	return strings.TrimSpace(string(b)), err == nil
}

func g11pName(sid string) string { return "relay-agent-" + sid }

// pane reads the session id, creation time and pane pid of sid's tmux session.
func (r *g11dRig) pane(t *testing.T, sid string) g11pPane {
	t.Helper()
	out, ok := g11pTmux(t, r, "display-message", "-p", "-t", "="+g11pName(sid)+":", "#{session_id}|#{session_created}|#{pane_pid}")
	if !ok {
		t.Fatalf("tmux has no session %s on the host", g11pName(sid))
	}
	f := strings.Split(out, "|")
	pid, err := strconv.Atoi(f[len(f)-1])
	if len(f) != 3 || err != nil || pid <= 1 {
		t.Fatalf("tmux reported %q for the pane of %s, want <session id>|<created>|<pane pid>", out, g11pName(sid))
	}
	return g11pPane{SessionID: f[0], Created: f[1], PID: pid}
}

// hasSession reports whether the host's tmux has the session of sid.
func (r *g11dRig) hasSession(t *testing.T, sid string) bool {
	t.Helper()
	_, ok := g11pTmux(t, r, "has-session", "-t", "="+g11pName(sid))
	return ok
}

// g11pUIInPane reports whether a Claude UI for conv runs as a child of the
// pane's process: the argv carries --resume conv and no --print. The process
// table is the only place the pane's program shows; tmux and the fake claude
// raise no event when it starts.
func g11pUIInPane(t *testing.T, panePID int, conv string) bool {
	t.Helper()
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,command=").Output()
	if err != nil {
		t.Fatalf("reading the process table: %v", err)
	}
	parent := map[int]int{}
	var ui []int
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		ppid, _ := strconv.Atoi(f[1])
		parent[pid] = ppid
		cmd := strings.Join(f[2:], " ")
		if pid != panePID && strings.Contains(cmd, "--resume "+conv) && !strings.Contains(cmd, "--print") {
			ui = append(ui, pid)
		}
	}
	for _, pid := range ui {
		for p, hops := pid, 0; p > 1 && hops < 32; p, hops = parent[p], hops+1 {
			if p == panePID {
				return true
			}
		}
	}
	return false
}

// uiStarted waits for the drop-in terminal's Claude UI to run in the pane of
// sid and checks the fake's start log holds the UI argv: --resume conv, no
// --print. Waits: none possible: tmux gives relay no event when a pane's
// program starts (ruling on G1.12); a bounded poll, as TestDetachedDropIn does.
func (r *g11dRig) uiStarted(t *testing.T, sid, conv string) {
	t.Helper()
	g11dUntil(t, "the drop-in's Claude UI to run as the pane's process with --resume "+conv, func() bool {
		return g11pUIInPane(t, r.pane(t, sid).PID, conv)
	})
	for _, argv := range r.hostStarts(t) {
		if agHasPair(argv, "--resume", conv) {
			for _, a := range argv {
				if a == "--print" {
					t.Fatalf("the terminal's claude started with %v, want the UI with no --print", argv)
				}
			}
			return
		}
	}
	t.Fatalf("the host's start log holds no claude start with --resume %s: %v", conv, r.hostStarts(t))
}

// dropInTTY starts relay drop-in on a terminal and waits for the drop-in to be
// recorded. hangup ends the terminal once; the cleanup hangs up if the test did
// not, and waits for the hand back so it finishes before the instance stops.
func (r *g11dRig) dropInTTY(t *testing.T, sid string) *harness.TTYProc {
	t.Helper()
	i := r.i
	tty := i.StartCLITTY(harness.CLIOpts{}, r.dir, "drop-in", sid)
	t.Cleanup(func() {
		tty.Hangup()
		tty.Wait()
		i.WaitEvent(harness.EventQuery{Key: "session.hand_back",
			Fields: map[string]any{"session_id": sid}}, g1tEventDeadline)
	})
	i.WaitEvent(harness.EventQuery{Key: "session.drop_in",
		Fields: map[string]any{"status": "ok", "session_id": sid}}, g1tEventDeadline)
	return tty
}

// handBack waits for the swap back to finish and checks it ran in the same
// pane: one tmux session, the runner of generation g2 as its pane process, the
// conversation resumed, and the next message answered.
func (r *g11dRig) handBack(t *testing.T, sid, conv string, before g11pPane, resumes int) {
	t.Helper()
	r.i.WaitEvent(harness.EventQuery{Key: "session.hand_back",
		Fields: map[string]any{"status": "ok", "session_id": sid}}, g1tEventDeadline)

	g2 := r.run(t, sid, "g2")
	after := r.pane(t, sid)
	if after.SessionID != before.SessionID || after.Created != before.Created {
		t.Fatalf("tmux session of %s is %+v after the hand back, want the session %+v from before", g11pName(sid), after, before)
	}
	if after.PID != g2.PID || g2.PID != g2.PGID {
		t.Fatalf("pane pid is %d, g2's runner.json reads pid %d pgid %d, want the runner as the pane process", after.PID, g2.PID, g2.PGID)
	}
	if got := g1wResumeFlags(r.hostStarts(t), conv); got != resumes {
		t.Fatalf("the host saw %d claude starts with --resume %s, want %d: %v", got, conv, resumes, r.hostStarts(t))
	}
	g1sSendText(t, r.i, sid, "again", "echo: again")
}

// started is a host session with its first turn done, in its tmux pane.
type g11pStarted struct {
	sid, conv string
	g1        g11dRun
	pane      g11pPane
}

func (r *g11dRig) startedInPane(t *testing.T) g11pStarted {
	t.Helper()
	sid := r.startSession(t, g11pSettings)
	g1 := r.run(t, sid, "g1")
	g1sSendText(t, r.i, sid, "hi", "echo: hi")
	return g11pStarted{sid: sid, conv: r.conversationID(t, sid, "g1"), g1: g1, pane: r.pane(t, sid)}
}

// Criterion 1: the runner is the pane of relay-agent-<id>: the pane's pid is
// runner.json's pid, and that pid leads its own group.
func TestHostSessionRunsInTmuxPane(t *testing.T) {
	t.Parallel()
	r := g11dStart(t)
	sid := r.startSession(t, g11pSettings)
	run := r.run(t, sid, "g1")

	names, ok := g11pTmux(t, r, "list-sessions", "-F", "#{session_name}")
	if !ok || names != g11pName(sid) {
		t.Fatalf("tmux list-sessions on the host reads %q (ok %v), want only %s", names, ok, g11pName(sid))
	}
	pane := r.pane(t, sid)
	if pane.PID != run.PID {
		t.Fatalf("pane pid is %d, runner.json reads pid %d, want the runner as the pane process", pane.PID, run.PID)
	}
	if run.PID != run.PGID {
		t.Fatalf("runner.json reads pid %d and pgid %d, want a runner that leads its own group", run.PID, run.PGID)
	}
	g1sSendText(t, r.i, sid, "hi", "echo: hi")
}

// Criteria 2 and 3: an idle session swaps to Claude's UI in the same pane on
// the same conversation, and closing the terminal swaps the same pane back to
// the runner, a new generation on the same conversation.
func TestHostPaneSwapToTerminalAndBack(t *testing.T) {
	t.Parallel()
	r := g11dStart(t)
	s := r.startedInPane(t)

	tty := r.dropInTTY(t, s.sid)
	r.uiStarted(t, s.sid, s.conv)
	if during := r.pane(t, s.sid); during.SessionID != s.pane.SessionID || during.Created != s.pane.Created {
		t.Fatalf("tmux session of %s is %+v during the drop-in, want the session %+v from before", g11pName(s.sid), during, s.pane)
	}
	if !g11dGroupGone(s.g1.PGID) {
		t.Fatalf("the headless runner's group %d still has members during the drop-in", s.g1.PGID)
	}

	// The tab closes while Claude still runs.
	tty.Hangup()
	tty.Wait()
	r.handBack(t, s.sid, s.conv, s.pane, 2)
}

// Criterion 3, the other way: quitting Claude in the terminal ends the terminal
// and the pane returns to the runner. Ctrl-D through the terminal is the end of
// input to the UI, which the fake claude ends on, as it does at a closed stdin.
func TestHostPaneQuitClaudeHandsBack(t *testing.T) {
	t.Parallel()
	r := g11dStart(t)
	s := r.startedInPane(t)

	tty := r.dropInTTY(t, s.sid)
	r.uiStarted(t, s.sid, s.conv)

	tty.Write([]byte{4})
	// The terminal ends on its own: no hangup. Wait returns when the CLI exits
	// and fails the test at the CLI's own deadline if it does not.
	tty.Wait()
	r.handBack(t, s.sid, s.conv, s.pane, 2)
}

// g11pStartOldTmux is g11dStart for a host whose tmux is older than 3.0: a
// tmux that answers -V with 2.9a and fails every other call stands first on
// the host's PATH, and a second probe records it as the host's tmux. The login
// shell's PATH on this machine holds the real tmux whatever the stub's PATH
// is, so a host with no tmux at all cannot be built through the stub; the
// criterion says "without tmux 3.0 or later", which this host is.
func g11pStartOldTmux(t *testing.T) *g11dRig {
	t.Helper()
	r := g11dStart(t)
	r.write(t, filepath.Join(r.remote, "hostclaude", "tmux"),
		"#!/bin/sh\ncase \"$1\" in -V) echo 'tmux 2.9a';; *) exit 1;; esac\n", 0o755)
	var probed struct {
		Probe struct {
			OK       bool   `json:"ok"`
			TmuxPath string `json:"tmux_path"`
		} `json:"probe"`
	}
	r.i.MustCLI("host", "probe", "--id", g11dHostID, "--json").JSON(t, &probed)
	if want := filepath.Join(r.remote, "hostclaude", "tmux"); !probed.Probe.OK || probed.Probe.TmuxPath != want {
		t.Fatalf("probe is %+v, want ok with tmux_path %s", probed.Probe, want)
	}
	return r
}

// Criterion 4: a host without tmux 3.0 or later runs the session under the
// runner alone, and its drop-in opens a terminal that resumes the conversation,
// as before. No tmux session is ever made on the host.
func TestHostWithoutTmuxDropInUsesTerminal(t *testing.T) {
	t.Parallel()
	r := g11pStartOldTmux(t)
	i := r.i
	sid := r.startSession(t, g11pSettings)
	run := r.run(t, sid, "g1")
	g1sSendText(t, i, sid, "hi", "echo: hi")
	conv := r.conversationID(t, sid, "g1")
	if run.PID != run.PGID {
		t.Fatalf("runner.json reads pid %d and pgid %d, want a runner that leads its own group", run.PID, run.PGID)
	}
	noPane := func(when string) {
		t.Helper()
		if out, ok := g11pTmux(t, r, "list-sessions", "-F", "#{session_name}"); ok {
			t.Fatalf("the host's tmux server holds sessions %q %s, want none", out, when)
		}
	}
	noPane("after the start")

	tty := r.dropInTTY(t, sid)
	g11dUntil(t, "the drop-in terminal to start claude with --resume "+conv, func() bool {
		return g1wResumeFlags(r.hostStarts(t), conv) == 1
	})
	tty.Hangup()
	tty.Wait()
	i.WaitEvent(harness.EventQuery{Key: "session.hand_back",
		Fields: map[string]any{"status": "ok", "session_id": sid}}, g1tEventDeadline)

	g2 := r.run(t, sid, "g2")
	if g2.PID != g2.PGID {
		t.Fatalf("g2 runner.json reads pid %d and pgid %d, want a runner that leads its own group", g2.PID, g2.PGID)
	}
	if got := g1wResumeFlags(r.hostStarts(t), conv); got != 2 {
		t.Fatalf("the host saw %d claude starts with --resume %s, want the terminal and generation g2: %v", got, conv, r.hostStarts(t))
	}
	g1sSendText(t, i, sid, "again", "echo: again")
	noPane("after the hand back")
}

// Criterion 5: deleting the session ends its tmux session.
func TestHostPaneDeleteEndsTmuxSession(t *testing.T) {
	t.Parallel()
	r := g11dStart(t)
	sid := r.startSession(t, g11pSettings)
	r.run(t, sid, "g1")
	g1sSendText(t, r.i, sid, "hi", "echo: hi")
	if !r.hasSession(t, sid) {
		t.Fatalf("tmux has no session %s before the delete", g11pName(sid))
	}

	// relay session stop is the CLI of DELETE /api/sessions/{id}.
	r.i.MustCLI("session", "stop", "--id", sid, "--json")
	if r.hasSession(t, sid) {
		t.Fatalf("tmux still has the session %s after the session was deleted", g11pName(sid))
	}
}

// g11pUIPid returns the pid of the Claude UI itself under the pane's process:
// the argv carries --resume conv and no --print, and the process is neither the
// pane's own shell wrappers (their argv names relay-ui) nor the pane process.
// It is 0 while no such process runs.
func g11pUIPid(t *testing.T, panePID int, conv string) int {
	t.Helper()
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,command=").Output()
	if err != nil {
		t.Fatalf("reading the process table: %v", err)
	}
	parent := map[int]int{}
	var ui []int
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		ppid, _ := strconv.Atoi(f[1])
		parent[pid] = ppid
		cmd := strings.Join(f[2:], " ")
		if pid != panePID && strings.Contains(cmd, "--resume "+conv) && !strings.Contains(cmd, "--print") && !strings.Contains(cmd, "relay-ui") {
			ui = append(ui, pid)
		}
	}
	for _, pid := range ui {
		for p, hops := pid, 0; p > 1 && hops < 32; p, hops = parent[p], hops+1 {
			if p == panePID {
				return pid
			}
		}
	}
	return 0
}

// g11pCwd reads a process's working directory with lsof.
func g11pCwd(t *testing.T, pid int) string {
	t.Helper()
	out, err := exec.Command("lsof", "-a", "-d", "cwd", "-p", strconv.Itoa(pid), "-Fn").Output()
	if err != nil {
		t.Fatalf("lsof for the cwd of pid %d: %v", pid, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n") {
			return line[1:]
		}
	}
	t.Fatalf("lsof printed no cwd for pid %d: %q", pid, out)
	return ""
}

// Decision 4 (Amendment 2): the drop-in's UI starts in the project's directory
// when that directory holds a "#". tmux reads a -c value as a format, so a
// "C#Projects" path must not reach tmux as one.
func TestHostPaneDropInDirWithHash(t *testing.T) {
	t.Parallel()
	r := g11dStart(t)
	hashDir := filepath.Join(r.remote, "C#Projects", "boxproj")
	r.mkdir(t, hashDir)
	body, _ := json.Marshal(map[string]any{
		"name": "Acme Hash Box", "path": hashDir, "host_id": g11dHostID, "allowed_templates": []string{"claude-code"},
	})
	res := r.i.CLIWith(harness.CLIOpts{Stdin: body}, "project", "create", "--file", "-", "--json")
	if res.Code != 0 {
		t.Fatalf("project create for the hash-dir host project exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	var p struct {
		ID string `json:"id"`
	}
	res.JSON(t, &p)
	r.projectID = p.ID

	s := r.startedInPane(t)
	r.dropInTTY(t, s.sid)

	var ui int
	g11dUntil(t, "the drop-in's Claude UI to run under the pane with --resume "+s.conv, func() bool {
		ui = g11pUIPid(t, r.pane(t, s.sid).PID, s.conv)
		return ui != 0
	})
	got, gerr := filepath.EvalSymlinks(g11pCwd(t, ui))
	want, werr := filepath.EvalSymlinks(hashDir)
	if gerr != nil || werr != nil {
		t.Fatalf("resolving the cwd %v and the project dir %v", gerr, werr)
	}
	if got != want {
		t.Fatalf("the UI's cwd is %s, want the project dir %s", got, want)
	}
	r.uiStarted(t, s.sid, s.conv)
}
