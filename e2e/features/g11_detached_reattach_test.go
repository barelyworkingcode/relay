package features

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"relaye2e/harness"
)

// A relay restart picks a detached session back up: the agent outlives relay,
// and relay reattaches to it from the run record on the host. The tests reuse
// the rig of g11_detached_host_test.go, whose fake host is this machine, so
// the agent's process state is read with kill(2) and ps.

// restart restarts relay on the same config dir and waits on the session
// host's manifest registration that follows. The line of the earlier boot is
// already in the log, so the wait is bounded below by the restart's start.
func (r *g11dRig) restart(t *testing.T) {
	t.Helper()
	since := time.Now()
	r.i.Restart()
	r.i.WaitEvent(harness.EventQuery{Key: "service.manifest.register", Since: since,
		Fields: map[string]any{"service_id": "relaysessions", "status": "ok"}}, g1tHostDeadline)
}

// restartHostDown restarts relay while the host is unreachable, so the startup
// check cannot read the run record, and brings the host back once relay is up.
func (r *g11dRig) restartHostDown(t *testing.T) {
	t.Helper()
	r.i.Stop()
	r.i.SSHDown(g11dTarget)
	r.restart(t)
	r.i.SSHUp(g11dTarget)
}

// hostStarts lists the argv of every start the host-side fake claude logged.
// The ssh stub runs the agent with TMPDIR under the remote root, so its call
// log is not under the instance's own TMPDIR and i.FakeAgentCalls does not
// see it. A probe's start counts too: tests compare against a count taken
// after the probe.
func (r *g11dRig) hostStarts(t *testing.T) [][]string {
	t.Helper()
	var out [][]string
	_ = filepath.WalkDir(filepath.Join(r.remote, "tmp"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "fakeagent-claude.jsonl" {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("reading %s: %v", path, rerr)
		}
		sc := bufio.NewScanner(bytes.NewReader(raw))
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			var c harness.AgentCall
			if json.Unmarshal(sc.Bytes(), &c) == nil && c.Event == "start" {
				out = append(out, c.Argv)
			}
		}
		return nil
	})
	return out
}

// killGroupWhileDown ends the agent's whole process group, as a host reboot or
// an operator would while relay is down, and waits until it is gone.
func killGroupWhileDown(t *testing.T, run g11dRun) {
	t.Helper()
	if err := syscall.Kill(-run.PGID, syscall.SIGKILL); err != nil {
		t.Fatalf("killing process group %d: %v", run.PGID, err)
	}
	g11dUntil(t, "the agent's process group to end", func() bool { return g11dGroupGone(run.PGID) })
}

// sessionRow reads the session's row in relay session list.
func (r *g11dRig) sessionRow(t *testing.T, sid string) g1sSessionRow {
	t.Helper()
	list, _ := g1sListSessions(t, r.i)
	row, ok := g1sFindSession(list, sid)
	if !ok {
		t.Fatalf("session list holds no %s", sid)
	}
	return row
}

// onlyGeneration fails unless the session has the one generation gen.
func (r *g11dRig) onlyGeneration(t *testing.T, sid, gen, why string) {
	t.Helper()
	if gens := r.generations(t, sid); len(gens) != 1 || gens[0] != gen {
		t.Fatalf("generations %s are %v, want only %s", why, gens, gen)
	}
}

// conversationID reads the agent's conversation id from the result line it
// wrote to out.jsonl.
func (r *g11dRig) conversationID(t *testing.T, sid, gen string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.agentsDir(sid), gen, "out.jsonl"))
	if err != nil {
		t.Fatalf("reading out.jsonl: %v", err)
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var line struct {
			Type      string `json:"type"`
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(sc.Bytes(), &line) == nil && line.Type == "result" && line.SessionID != "" {
			return line.SessionID
		}
	}
	t.Fatalf("out.jsonl of %s/%s holds no result line with a session_id", sid, gen)
	return ""
}

// Criterion 1: after a relay restart a detached session whose agent still
// runs answers its next message with no resume.
func TestDetachedReattachAfterRestart(t *testing.T) {
	t.Parallel()
	r := g11dStart(t)
	i := r.i
	base := len(r.hostStarts(t))
	sid := r.startSession(t, "")
	run := r.run(t, sid, "g1")
	g1sSendText(t, i, sid, "hi", "echo: hi")

	r.restart(t)
	if !g11dAlive(run.PID) {
		t.Fatalf("agent pid %d did not outlive the relay restart", run.PID)
	}

	g1sSendText(t, i, sid, "after the restart", "echo: after the restart")

	if after := r.run(t, sid, "g1"); after.PID != run.PID || after.PGID != run.PGID || after.Started != run.Started {
		t.Fatalf("run.json went from %+v to %+v across the restart and a message", run, after)
	}
	r.onlyGeneration(t, sid, "g1", "after the restart and a message")
	if got := i.Events(harness.EventQuery{Key: "session.resume"}); len(got) != 0 {
		t.Fatalf("the message after the restart caused %d session.resume lines, want none", len(got))
	}
	if got := r.hostStarts(t); len(got) != base+1 {
		t.Fatalf("the host saw %d agent starts since the probe, want one: %v", len(got)-base, got)
	}
}

// Criterion 2: after a relay restart a detached session whose agent exited
// reads dormant and resumes with its history. The agent's group is killed
// while relay is down.
func TestDetachedExitedReadsDormantAfterRestart(t *testing.T) {
	t.Parallel()
	r := g11dStart(t)
	i := r.i
	sid := r.startSession(t, "")
	run := r.run(t, sid, "g1")
	g1sSendText(t, i, sid, "hi", "echo: hi")
	before := r.sessionRow(t, sid)

	i.Stop()
	killGroupWhileDown(t, run)
	r.restart(t)

	if row := r.sessionRow(t, sid); row.Live {
		t.Fatalf("session %s reads live after relay restarted over an exited agent", sid)
	}

	res := i.MustCLI("session", "resume", "--id", sid, "--json")
	var resumed struct {
		SessionID string `json:"session_id"`
		Resumed   bool   `json:"resumed"`
	}
	res.JSON(t, &resumed)
	if !resumed.Resumed || resumed.SessionID != sid {
		t.Fatalf("resume of the dormant session printed %+v, want resumed true for %s", resumed, sid)
	}
	next := r.run(t, sid, "g2")
	if next.PID == run.PID || !g11dAlive(next.PID) {
		t.Fatalf("the resumed agent is pid %d (old %d), want a new running process", next.PID, run.PID)
	}
	if row := r.sessionRow(t, sid); !row.Live || row.MessageCount != before.MessageCount {
		t.Fatalf("resumed row live=%v with %d messages, want live with the %d from before", row.Live, row.MessageCount, before.MessageCount)
	}
	g1sSendText(t, i, sid, "again", "echo: again")
}

// Criterion 3: a resume of a detached session whose agent still runs
// reattaches it and starts no second agent. The restart happens with the host
// unreachable, so the session reads dormant and the resume is what attaches.
func TestDetachedResumeReattachesNoSecondAgent(t *testing.T) {
	t.Parallel()
	r := g11dStart(t)
	i := r.i
	base := len(r.hostStarts(t))
	sid := r.startSession(t, "")
	run := r.run(t, sid, "g1")
	g1sSendText(t, i, sid, "hi", "echo: hi")

	r.restartHostDown(t)
	if !g11dAlive(run.PID) {
		t.Fatalf("agent pid %d did not outlive the relay restart", run.PID)
	}

	res := i.MustCLI("session", "resume", "--id", sid, "--json")
	var resumed struct {
		SessionID string `json:"session_id"`
		Resumed   bool   `json:"resumed"`
	}
	res.JSON(t, &resumed)
	if resumed.SessionID != sid || !resumed.Resumed {
		t.Fatalf("resume printed %+v, want resumed true for %s", resumed, sid)
	}

	if after := r.run(t, sid, "g1"); after.PID != run.PID || after.Generation != run.Generation || !g11dAlive(run.PID) {
		t.Fatalf("run.json went from %+v to %+v, or the agent stopped", run, after)
	}
	r.onlyGeneration(t, sid, "g1", "after the resume")
	if got := r.hostStarts(t); len(got) != base+1 {
		t.Fatalf("the host saw %d agent starts since the probe, want one: %v", len(got)-base, got)
	}
	g1sSendText(t, i, sid, "after the resume", "echo: after the resume")
}

// Criterion 4: a run record whose pid now names another process counts as
// exited, and that process keeps running. The record is made stale on purpose,
// as a recycled pid leaves it: the old start time with a stand-in's pid.
func TestDetachedReusedPidCountsExited(t *testing.T) {
	t.Parallel()
	r := g11dStart(t)
	i := r.i
	sid := r.startSession(t, "")
	run := r.run(t, sid, "g1")
	g1sSendText(t, i, sid, "hi", "echo: hi")

	i.Stop()
	killGroupWhileDown(t, run)

	other := exec.Command("/bin/sleep", "600")
	other.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := other.Start(); err != nil {
		t.Fatalf("starting the stand-in process: %v", err)
	}
	reaped := make(chan struct{})
	go func() { _ = other.Wait(); close(reaped) }()
	t.Cleanup(func() { _ = other.Process.Kill(); <-reaped })
	stale, _ := json.Marshal(g11dRun{PID: other.Process.Pid, PGID: other.Process.Pid, Started: run.Started, Generation: "g1"})
	r.write(t, filepath.Join(r.agentsDir(sid), "g1", "run.json"), string(stale), 0o600)

	r.restart(t)
	if row := r.sessionRow(t, sid); row.Live {
		t.Fatalf("session %s reads live though run.json names a pid that is not its agent", sid)
	}

	res := i.MustCLI("session", "resume", "--id", sid, "--json")
	var resumed struct {
		Resumed bool `json:"resumed"`
	}
	res.JSON(t, &resumed)
	if !resumed.Resumed {
		t.Fatalf("resume over a reused pid printed resumed false, want a new agent")
	}
	r.run(t, sid, "g2") // registers the new agent's group for cleanup
	g1sSendText(t, i, sid, "again", "echo: again")
	i.MustCLI("session", "stop", "--id", sid, "--json")

	select {
	case <-reaped:
		t.Fatalf("relay ended pid %d, a process that run.json named under a start time that is not its own", other.Process.Pid)
	default:
	}
	if !g11dAlive(other.Process.Pid) {
		t.Fatalf("stand-in pid %d is not running after the resume and the stop", other.Process.Pid)
	}
}

// Criterion 5: a relay-sessions restart on its own leaves detached agents
// running. relay service restart --id relaysessions restarts the session host
// under relay's supervision; its manifest registration is the ready signal.
func TestDetachedSessionsRestartKeepsAgent(t *testing.T) {
	t.Parallel()
	r := g11dStart(t)
	i := r.i
	sid := r.startSession(t, "")
	run := r.run(t, sid, "g1")
	g1sSendText(t, i, sid, "hi", "echo: hi")

	since := time.Now()
	res := i.MustCLI("service", "restart", "--id", "relaysessions")
	i.WaitEvent(harness.EventQuery{Key: "service.restart", Trace: res.Trace,
		Fields: map[string]any{"status": "ok", "service_id": "relaysessions"}}, g1tEventDeadline)
	i.WaitEvent(harness.EventQuery{Key: "service.manifest.register", Since: since,
		Fields: map[string]any{"service_id": "relaysessions", "status": "ok"}}, g1tHostDeadline)

	if !g11dAlive(run.PID) {
		t.Fatalf("agent pid %d did not outlive the session host restart", run.PID)
	}
	if after := r.run(t, sid, "g1"); after.PID != run.PID || after.Started != run.Started {
		t.Fatalf("run.json went from %+v to %+v across the session host restart", run, after)
	}
	r.onlyGeneration(t, sid, "g1", "after the session host restart")
}

// dropIn takes the session over in a terminal and checks the headless agent's
// group ends and the terminal's claude resumes the agent's conversation. The
// terminal's start is logged by the host's fake once it runs; no event marks
// it, so the log is polled under the frame deadline.
func (r *g11dRig) dropIn(t *testing.T, sid string, run g11dRun) {
	t.Helper()
	i := r.i
	conversation := r.conversationID(t, sid, "g1")
	ws := i.WebSocket("/ws", i.Credential("mounter"))
	defer ws.Close()
	ws.Send(map[string]any{"type": "join_session", "sessionId": sid})
	tty := i.StartCLITTY(harness.CLIOpts{}, r.dir, "drop-in", sid)
	t.Cleanup(func() { tty.Hangup(); tty.Wait() })
	i.WaitEvent(harness.EventQuery{Key: "session.drop_in",
		Fields: map[string]any{"status": "ok", "session_id": sid}}, g1tEventDeadline)
	for n := 0; ; n++ {
		if n == 4*g1tFrameLimit {
			t.Fatalf("no process_exited for %s in %d frames after the drop-in", sid, n)
		}
		var f g11dFrame
		if err := json.Unmarshal(ws.Next(g1tFrameDeadline), &f); err != nil {
			t.Fatalf("decoding a frame: %v", err)
		}
		if f.Type == "process_exited" && f.SID == sid {
			break
		}
	}
	if g11dAlive(run.PID) || !g11dGroupGone(run.PGID) {
		t.Fatalf("agent pid %d or its group %d still runs after the drop-in took the session", run.PID, run.PGID)
	}
	g11dUntil(t, "the drop-in terminal to start claude with --resume "+conversation, func() bool {
		for _, argv := range r.hostStarts(t) {
			if agHasPair(argv, "--resume", conversation) {
				return true
			}
		}
		return false
	})
}

// Criterion 6: relay drop-in on a detached host session ends its agent at turn
// end and opens the conversation with its history.
func TestDetachedDropIn(t *testing.T) {
	t.Parallel()
	r := g11dStart(t)
	sid := r.startSession(t, `{"headless":true,"agent":true}`)
	run := r.run(t, sid, "g1")
	g1sSendText(t, r.i, sid, "hi", "echo: hi")
	r.dropIn(t, sid, run)
}

// A drop-in after a relay restart takes the reattached session the same way.
func TestDetachedDropInAfterRestart(t *testing.T) {
	t.Parallel()
	r := g11dStart(t)
	sid := r.startSession(t, `{"headless":true,"agent":true}`)
	run := r.run(t, sid, "g1")
	g1sSendText(t, r.i, sid, "hi", "echo: hi")

	r.restart(t)
	if !g11dAlive(run.PID) {
		t.Fatalf("agent pid %d did not outlive the relay restart", run.PID)
	}
	r.dropIn(t, sid, run)
}

type g11rPermission struct {
	Type         string `json:"type"`
	SessionID    string `json:"sessionId"`
	PermissionID string `json:"permissionId"`
	ToolName     string `json:"toolName"`
}

// A permission the agent was waiting on when relay restarted is raised again:
// wait reports asking, a viewer that joins gets the permission_request, and
// its answer reaches the agent once.
func TestDetachedPermissionReraisedAfterRestart(t *testing.T) {
	t.Parallel()
	r := g11dStart(t)
	i := r.i
	sid := r.startSession(t, "")
	run := r.run(t, sid, "g1")

	before := i.WebSocket("/ws", i.Credential("mounter"))
	before.Send(map[string]any{"type": "join_session", "sessionId": sid})
	g1vSend(t, i, sid, "!ask Write")
	b367Next(t, before, sid, "session_state", "asking")
	before.Close()

	r.restart(t)
	if !g11dAlive(run.PID) {
		t.Fatalf("agent pid %d did not outlive the relay restart", run.PID)
	}
	if res, w := g1vWaitFor(t, i, sid); res.Code != 1 || w.State != "asking" {
		t.Fatalf("wait after the restart exited %d with state %q, want 1 and asking", res.Code, w.State)
	}

	ws := i.WebSocket("/ws", i.Credential("mounter"))
	defer ws.Close()
	ws.Send(map[string]any{"type": "join_session", "sessionId": sid})
	var ask g11rPermission
	for n := 0; ; n++ {
		if n == 4*g1tFrameLimit {
			t.Fatalf("no permission_request for %s in %d frames after the join", sid, n)
		}
		var f g11rPermission
		if err := json.Unmarshal(ws.Next(g1tFrameDeadline), &f); err != nil {
			t.Fatalf("decoding a frame: %v", err)
		}
		if f.Type == "permission_request" && f.SessionID == sid {
			ask = f
			break
		}
	}
	if ask.PermissionID == "" || ask.ToolName != "Write" {
		t.Fatalf("permission_request is %+v, want a permissionId for the Write tool", ask)
	}

	ws.Send(map[string]any{"type": "permission_response", "permissionId": ask.PermissionID, "approved": true})
	b367Next(t, ws, sid, "session_state", "idle")

	if all := g1vEventsAfter(t, i, sid, ""); g1vAssistantCount(all.Events, "ask: allow Write") != 1 {
		t.Fatalf("the turn did not end once with ask: allow Write: %s", g1vJoin(all.Events))
	}
	raw, err := os.ReadFile(filepath.Join(r.agentsDir(sid), "g1", "in.jsonl"))
	if err != nil {
		t.Fatalf("reading in.jsonl: %v", err)
	}
	if n := bytes.Count(raw, []byte(`"type":"control_response"`)); n != 1 {
		t.Fatalf("in.jsonl holds %d control_response lines, want 1\n%s", n, raw)
	}
}

// After a relay-sessions-only restart a detached session whose agent was
// reattached is still found by project removal, which ends its agent and
// group. docs/cli.md names no asynchronous stop, so the agent is expected
// gone when relay project remove returns; the poll is an upper bound for host
// state that has no signal of its own.
func TestDetachedProjectDeleteAfterSessionsRestartEndsAgent(t *testing.T) {
	t.Parallel()
	r := g11dStart(t)
	i := r.i
	sid := r.startSession(t, "")
	run := r.run(t, sid, "g1")
	g1sSendText(t, i, sid, "hi", "echo: hi")

	since := time.Now()
	res := i.MustCLI("service", "restart", "--id", "relaysessions")
	i.WaitEvent(harness.EventQuery{Key: "service.restart", Trace: res.Trace,
		Fields: map[string]any{"status": "ok", "service_id": "relaysessions"}}, g1tEventDeadline)
	i.WaitEvent(harness.EventQuery{Key: "service.manifest.register", Since: since,
		Fields: map[string]any{"service_id": "relaysessions", "status": "ok"}}, g1tHostDeadline)
	if !g11dAlive(run.PID) {
		t.Fatalf("agent pid %d did not outlive the session host restart", run.PID)
	}

	i.MustCLI("project", "remove", "--id", r.projectID, "--json")
	g11dUntil(t, "the removed project's agent and group to end", func() bool {
		return !g11dAlive(run.PID) && g11dGroupGone(run.PGID)
	})
}

// A host-down restart leaves the session dormant; a message reattaches it,
// and removing its project then ends the agent and its group.
func TestDetachedProjectDeleteAfterHostDownRestartEndsAgent(t *testing.T) {
	t.Parallel()
	r := g11dStart(t)
	i := r.i
	sid := r.startSession(t, "")
	run := r.run(t, sid, "g1")
	g1sSendText(t, i, sid, "hi", "echo: hi")

	r.restartHostDown(t)
	g1sSendText(t, i, sid, "after the restart", "echo: after the restart")
	if after := r.run(t, sid, "g1"); after.PID != run.PID || !g11dAlive(run.PID) {
		t.Fatalf("run.json went from %+v to %+v, or the agent stopped", run, after)
	}

	i.MustCLI("project", "remove", "--id", r.projectID, "--json")
	g11dUntil(t, "the removed project's agent and group to end", func() bool {
		return !g11dAlive(run.PID) && g11dGroupGone(run.PGID)
	})
}
