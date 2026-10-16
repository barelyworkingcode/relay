package features

import (
	"encoding/json"
	"errors"
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

// A Claude session on a host with detach_sessions set keeps its agent when
// relay's link to the host drops. The fake host is this machine: the ssh stub
// runs the remote command as a child of the stub, with HOME at the instance's
// remote root, so the agent's run directory is a local path and its process
// state is read with kill(2).

const (
	g11dTarget = "acme@testbox"
	g11dHostID = "h_acme01"
	// g11dLinkDeadline bounds the wait for a session.host_link event. An
	// upper bound only: the event follows the drop at once.
	g11dLinkDeadline = 30 * time.Second
)

type g11dRun struct {
	PID        int    `json:"pid"`
	PGID       int    `json:"pgid"`
	Started    string `json:"started"`
	Generation string `json:"generation"`
}

type g11dRig struct {
	i         *harness.Instance
	remote    string
	projectID string
	dir       string
}

// g11dStart boots an instance with the ssh stub, a host with detach_sessions
// set and one host project whose claude is the fake agent.
func g11dStart(t *testing.T) *g11dRig {
	t.Helper()
	seed := `[{"id":"` + g11dHostID + `","name":"testbox","target":"` + g11dTarget + `","detach_sessions":true,"created_at":"2026-01-01T00:00:00Z"}]`
	i := harness.Start(t, harness.Options{
		Credentials: g1tCreds, Presence: g1tApprove, SSHStub: true,
		Settings: map[string]json.RawMessage{"hosts": json.RawMessage(seed)},
	})
	i.WaitSessionHost(g1tHostDeadline)
	r := &g11dRig{i: i, remote: i.RemoteRoot()}

	// The host's claude-code template exists only when a probe finds claude
	// on the host, so the host gets the fake as its claude first.
	claudeDir := filepath.Join(r.remote, "hostclaude")
	r.mkdir(t, claudeDir)
	fake := filepath.Join(i.Home, ".local", "bin", "claude")
	r.write(t, filepath.Join(claudeDir, "claude"), "#!/bin/sh\nexec "+agShQuote(fake)+" \"$@\"\n", 0o755)
	r.write(t, filepath.Join(r.remote, ".profile"), "export PATH="+agShQuote(claudeDir)+":\"$PATH\"\n", 0o600)
	i.MustCLI("host", "probe", "--id", g11dHostID, "--json")

	r.dir = filepath.Join(r.remote, "boxproj")
	r.mkdir(t, r.dir)
	body, _ := json.Marshal(map[string]any{
		"name": "Acme Box", "path": r.dir, "host_id": g11dHostID, "allowed_templates": []string{"claude-code"},
	})
	res := i.CLIWith(harness.CLIOpts{Stdin: body}, "project", "create", "--file", "-", "--json")
	if res.Code != 0 {
		t.Fatalf("project create for the host project exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	var p struct {
		ID string `json:"id"`
	}
	res.JSON(t, &p)
	r.projectID = p.ID
	return r
}

func (r *g11dRig) mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
}

func (r *g11dRig) write(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func (r *g11dRig) agentsDir(sid string) string {
	return filepath.Join(r.remote, ".relay", "agents", sid)
}

// startSession starts a Claude session on the host. settings is a JSON
// object, or "" for none.
func (r *g11dRig) startSession(t *testing.T, settings string) string {
	t.Helper()
	return g1tStartSession(t, r.i, r.projectID, "sonnet", settings)
}

// run reads the generation's run.json. relay session start returns only after
// run.json exists, so no wait is needed. The agent's group is killed at
// cleanup so a failing test leaves nothing behind.
func (r *g11dRig) run(t *testing.T, sid, gen string) g11dRun {
	t.Helper()
	path := filepath.Join(r.agentsDir(sid), gen, "run.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the started session has no run.json: %v", err)
	}
	var run g11dRun
	if err := json.Unmarshal(raw, &run); err != nil {
		t.Fatalf("decoding %s: %v\n%s", path, err, raw)
	}
	if run.PID <= 1 || run.PGID <= 1 || run.Generation != gen {
		t.Fatalf("run.json reads %+v, want a pid, a pgid and generation %s", run, gen)
	}
	t.Cleanup(func() { _ = syscall.Kill(-run.PGID, syscall.SIGKILL) })
	return run
}

// generations lists the generation directories of a session.
func (r *g11dRig) generations(t *testing.T, sid string) []string {
	t.Helper()
	entries, err := os.ReadDir(r.agentsDir(sid))
	if err != nil {
		t.Fatalf("reading %s: %v", r.agentsDir(sid), err)
	}
	var gens []string
	for _, e := range entries {
		gens = append(gens, e.Name())
	}
	return gens
}

// g11dAlive reports whether pid is a running process. A zombie is not running.
func g11dAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && !strings.Contains(string(out), "Z")
}

func g11dGroupGone(pgid int) bool {
	return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH)
}

// g11dDrop drops the link and waits for relay to say it lost it.
func g11dDrop(t *testing.T, i *harness.Instance, sid string) {
	t.Helper()
	i.SSHDown(g11dTarget)
	i.WaitEvent(harness.EventQuery{Key: "session.host_link",
		Fields: map[string]any{"link": "lost", "session_id": sid, "host_id": g11dHostID}}, g11dLinkDeadline)
}

// g11dRestore brings the link back and waits for relay to say so.
func g11dRestore(t *testing.T, i *harness.Instance, sid string) {
	t.Helper()
	i.SSHUp(g11dTarget)
	i.WaitEvent(harness.EventQuery{Key: "session.host_link",
		Fields: map[string]any{"link": "restored", "session_id": sid, "host_id": g11dHostID}}, g11dLinkDeadline)
}

// g11dUntil polls host state that has no signal of its own (the agent's
// output file) until ok holds. The deadline is an upper bound.
func g11dUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(g1tFrameDeadline)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("gave up waiting for %s", what)
		}
		<-tick.C
	}
}

// g11dNoticed reports whether relay has noticed the drop: the feature writes
// session.host_link lost; without it the agent dies with the link and the
// session ends or errors.
func g11dNoticed(i *harness.Instance, sid string) bool {
	queries := []harness.EventQuery{
		{Key: "session.host_link", Fields: map[string]any{"link": "lost", "session_id": sid}},
		{Key: "session.state", Fields: map[string]any{"session_id": sid, "to": "ended"}},
		{Key: "session.state", Fields: map[string]any{"session_id": sid, "to": "errored"}},
	}
	for _, q := range queries {
		if len(i.Events(q)) > 0 {
			return true
		}
	}
	return false
}

// Criteria 1 and 2: the link drops and returns, and a message gets its reply
// with no resume. The reply is asserted first; the detach bookkeeping after.
func TestDetachedAgentSurvivesLinkDrop(t *testing.T) {
	t.Parallel()
	r := g11dStart(t)
	sid := r.startSession(t, "")

	r.i.SSHDown(g11dTarget)
	g11dUntil(t, "relay to notice the dropped link", func() bool { return g11dNoticed(r.i, sid) })
	r.i.SSHUp(g11dTarget)
	// A send while the link is down fails, so the reply waits for the restore;
	// without the feature the session has ended and nothing restores.
	g11dUntil(t, "relay to restore the link", func() bool {
		return len(r.i.Events(harness.EventQuery{Key: "session.host_link", Fields: map[string]any{"link": "restored", "session_id": sid}})) > 0 ||
			len(r.i.Events(harness.EventQuery{Key: "session.state", Fields: map[string]any{"session_id": sid, "to": "ended"}})) > 0 ||
			len(r.i.Events(harness.EventQuery{Key: "session.state", Fields: map[string]any{"session_id": sid, "to": "errored"}})) > 0
	})

	g1sSendText(t, r.i, sid, "after the drop", "echo: after the drop")

	for _, link := range []string{"lost", "restored"} {
		r.i.WaitEvent(harness.EventQuery{Key: "session.host_link",
			Fields: map[string]any{"link": link, "session_id": sid, "host_id": g11dHostID}}, g11dLinkDeadline)
	}
	run := r.run(t, sid, "g1")
	if !g11dAlive(run.PID) {
		t.Fatalf("agent pid %d is not running after the drop and the reply", run.PID)
	}
	if gens := r.generations(t, sid); len(gens) != 1 || gens[0] != "g1" {
		t.Fatalf("generations after the drop are %v, want only g1: the message must not resume", gens)
	}
}

type g11dFrame struct {
	Type  string `json:"type"`
	SID   string `json:"sessionId"`
	State string `json:"state"`
	Event struct {
		Delta struct {
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`
}

// collect reads frames of sid until the n-th message_complete.
func g11dCollect(t *testing.T, ws *harness.WSConn, sid string, n int) []g11dFrame {
	t.Helper()
	var frames []g11dFrame
	done := 0
	for k := 0; k < 4*g1tFrameLimit && done < n; k++ {
		var f g11dFrame
		if err := json.Unmarshal(ws.Next(g1tFrameDeadline), &f); err != nil {
			t.Fatalf("decoding a frame: %v", err)
		}
		if f.SID != sid {
			continue
		}
		if f.Type == "process_exited" {
			t.Fatalf("the agent exited during the drop: process_exited for %s", sid)
		}
		frames = append(frames, f)
		if f.Type == "message_complete" {
			done++
		}
	}
	if done < n {
		t.Fatalf("saw %d message_complete frames for %s, want %d", done, sid, n)
	}
	return frames
}

func g11dCount(frames []g11dFrame, match func(g11dFrame) bool) (n, first int) {
	first = -1
	for k, f := range frames {
		if match(f) {
			if first < 0 {
				first = k
			}
			n++
		}
	}
	return n, first
}

// Criterion 3: a viewer joined across the drop gets each event the agent
// wrote during it once, in order.
func TestDetachedViewerGetsDropEventsOnce(t *testing.T) {
	t.Parallel()
	r := g11dStart(t)
	i := r.i
	sid := r.startSession(t, "")
	run := r.run(t, sid, "g1")

	fifo := filepath.Join(r.remote, "hold.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("creating %s: %v", fifo, err)
	}
	// The write end opens only once the agent's turn reads the fifo, which
	// proves the turn reached the agent; holding it open holds the turn.
	held := make(chan *os.File, 1)
	go func() {
		f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			close(held)
			return
		}
		held <- f
	}()

	ws := i.WebSocket("/ws", i.Credential("mounter"))
	defer ws.Close()
	ws.Send(map[string]any{"type": "join_session", "sessionId": sid})
	ws.Send(map[string]any{"type": "send_message", "sessionId": sid, "text": "!sh cat " + agShQuote(fifo), "trace_id": harness.NewTrace(t)})
	var hold *os.File
	select {
	case hold = <-held:
		if hold == nil {
			t.Fatalf("opening %s for writing failed", fifo)
		}
	case <-time.After(g1tFrameDeadline):
		t.Fatalf("the agent never read %s: the turn did not reach it", fifo)
	}

	g11dDrop(t, i, sid)
	if err := hold.Close(); err != nil {
		t.Fatalf("closing %s: %v", fifo, err)
	}
	// The agent now writes the reply and the result with no link up.
	out := filepath.Join(r.agentsDir(sid), "g1", "out.jsonl")
	g11dUntil(t, "the agent to write its result to out.jsonl", func() bool {
		b, _ := os.ReadFile(out)
		return strings.Contains(string(b), `"type":"result"`)
	})
	if !g11dAlive(run.PID) {
		t.Fatalf("agent pid %d died during the drop", run.PID)
	}
	g11dRestore(t, i, sid)

	// The held turn ends at the viewer first. A second turn is the fence: a
	// duplicate of the first turn's events would arrive before its
	// message_complete. It is sent only after the first ends, since a message
	// during a turn is refused.
	frames := g11dCollect(t, ws, sid, 1)
	ws.Send(map[string]any{"type": "send_message", "sessionId": sid, "text": "next", "trace_id": harness.NewTrace(t)})
	frames = append(frames, g11dCollect(t, ws, sid, 1)...)

	text, textAt := g11dCount(frames, func(f g11dFrame) bool { return strings.Contains(f.Event.Delta.Text, "exit: 0") })
	done, doneAt := g11dCount(frames, func(f g11dFrame) bool { return f.Type == "turn_done" })
	complete, completeAt := g11dCount(frames, func(f g11dFrame) bool { return f.Type == "message_complete" })
	if text != 1 || done != 2 || complete != 2 {
		t.Fatalf("viewer saw the held turn's reply %d times, turn_done %d times and message_complete %d times, want 1, 2 and 2", text, done, complete)
	}
	if !(textAt < doneAt && doneAt < completeAt) {
		t.Fatalf("frame order: reply at %d, turn_done at %d, message_complete at %d, want that order", textAt, doneAt, completeAt)
	}
}

// Criterion 4: a start whose run directory cannot be created fails and names
// the directory.
func TestDetachedStartNamesRunDirectoryItCannotCreate(t *testing.T) {
	t.Parallel()
	r := g11dStart(t)
	r.mkdir(t, filepath.Join(r.remote, ".relay"))
	// A regular file where the agents directory belongs.
	r.write(t, filepath.Join(r.remote, ".relay", "agents"), "not a directory\n", 0o600)

	res := r.i.CLI("session", "start", "--project", r.projectID, "--model", "sonnet", "--json")
	if res.Code == 0 {
		t.Fatalf("session start exited 0 though the run directory cannot be created\nstdout: %s", res.Stdout)
	}
	if !strings.Contains(string(res.Stderr), ".relay/agents") {
		t.Fatalf("the refusal does not name the run directory .relay/agents\nstderr: %s", res.Stderr)
	}
	if got := agStartArgvs(r.i, "claude"); len(got) != 0 {
		t.Fatalf("a start that failed still launched %d agents: %v", len(got), got)
	}
}

// Criterion 5, stop: session stop ends the agent and everything in its group,
// the input reader included, and removes the run directory.
func TestDetachedSessionStopEndsAgentGroup(t *testing.T) {
	t.Parallel()
	r := g11dStart(t)
	sid := r.startSession(t, "")
	run := r.run(t, sid, "g1")
	g1sSendText(t, r.i, sid, "hi", "echo: hi")
	if !g11dAlive(run.PID) {
		t.Fatalf("agent pid %d is not running before the stop", run.PID)
	}

	r.i.MustCLI("session", "stop", "--id", sid, "--json")
	if g11dAlive(run.PID) {
		t.Fatalf("agent pid %d still runs after session stop returned", run.PID)
	}
	if !g11dGroupGone(run.PGID) {
		t.Fatalf("process group %d still has members after session stop returned", run.PGID)
	}
	if _, err := os.Stat(r.agentsDir(sid)); !os.IsNotExist(err) {
		t.Fatalf("the run directory %s outlives the stopped session: stat err = %v", r.agentsDir(sid), err)
	}
}

// Criterion 5, drop-in: handing the session to a terminal ends the headless
// agent and its group.
func TestDetachedDropInEndsAgentGroup(t *testing.T) {
	t.Parallel()
	r := g11dStart(t)
	i := r.i
	sid := r.startSession(t, `{"headless":true,"agent":true}`)
	run := r.run(t, sid, "g1")
	g1sSendText(t, i, sid, "hi", "echo: hi")

	ws := i.WebSocket("/ws", i.Credential("mounter"))
	defer ws.Close()
	ws.Send(map[string]any{"type": "join_session", "sessionId": sid})
	tty := i.StartCLITTY(harness.CLIOpts{}, r.dir, "drop-in", sid)
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
	tty.Hangup()
	tty.Wait()
}

// Criterion 6: ending a detached session never signals a process that run.json
// no longer describes. The record is made stale on purpose, as a recycled pid
// leaves it: it names an unrelated process under a start time that is not its.
func TestDetachedStopSparesUnrelatedProcess(t *testing.T) {
	t.Parallel()
	r := g11dStart(t)
	sid := r.startSession(t, "")
	r.run(t, sid, "g1") // registers the real agent's group for cleanup

	other := exec.Command("/bin/sleep", "600")
	other.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := other.Start(); err != nil {
		t.Fatalf("starting the unrelated process: %v", err)
	}
	reaped := make(chan struct{})
	go func() { _ = other.Wait(); close(reaped) }()
	t.Cleanup(func() { _ = other.Process.Kill(); <-reaped })

	stale, _ := json.Marshal(g11dRun{PID: other.Process.Pid, PGID: other.Process.Pid, Started: "stale", Generation: "g1"})
	r.write(t, filepath.Join(r.agentsDir(sid), "g1", "run.json"), string(stale), 0o600)

	r.i.CLI("session", "stop", "--id", sid, "--json")
	select {
	case <-reaped:
		t.Fatalf("session stop killed pid %d, which run.json named under a start time that is not its own", other.Process.Pid)
	default:
	}
	if !g11dAlive(other.Process.Pid) {
		t.Fatalf("unrelated pid %d is not running after session stop", other.Process.Pid)
	}
}

// Criterion 7: a resume of a session whose agent still runs answers
// runner_alive and starts no second agent.
func TestDetachedResumeWhileAgentRunsAnswersRunnerAlive(t *testing.T) {
	t.Parallel()
	r := g11dStart(t)
	i := r.i
	sid := r.startSession(t, "")
	run := r.run(t, sid, "g1")
	g1sSendText(t, i, sid, "hi", "echo: hi")

	// Every session is dormant after a relay restart; the detached agent is
	// not relay's child, so it outlives it.
	i.Restart()
	i.WaitSessionHost(g1tHostDeadline)
	if !g11dAlive(run.PID) {
		t.Fatalf("agent pid %d did not outlive the relay restart", run.PID)
	}

	resp := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/sessions/"+sid+"/resume", nil)
	// docs/routes.md: a handler error body is human text; the code is the
	// event's reason.
	if resp.Status != 409 {
		t.Fatalf("resume answered %d, want 409 (runner_alive)\n%s", resp.Status, resp.Body)
	}
	i.WaitEvent(harness.EventQuery{Key: "session.resume",
		Fields: map[string]any{"reason": "runner_alive"}}, g11dLinkDeadline)
	if gens := r.generations(t, sid); len(gens) != 1 || gens[0] != "g1" {
		t.Fatalf("generations after the refused resume are %v, want only g1", gens)
	}
	if after := r.run(t, sid, "g1"); after.PID != run.PID || !g11dAlive(run.PID) {
		t.Fatalf("run.json pid went from %d to %d, or the agent stopped", run.PID, after.PID)
	}
}
