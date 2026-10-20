package features

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"relaye2e/harness"
)

// The file-channel runner of docs/ssh-hosts.md ("Detached sessions"): a
// headless Claude session on a Unix host runs under it by default. The fake
// host is this machine, so the generation directory is a local path.

// g11rMessageName is a message id plus its kind: 19 digits of unix nanoseconds,
// a dash and six lowercase hex digits.
var g11rMessageName = regexp.MustCompile(`^[0-9]{19}-[0-9a-f]{6}\.(json|ctl)$`)

// g11rNames lists the entries of dir that do not start with a dot, sorted.
func g11rNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// g11rSentCount counts the messages in the generation's sent/ whose bytes hold
// needle.
func g11rSentCount(t *testing.T, agentsDir, gen, needle string) int {
	t.Helper()
	dir := filepath.Join(agentsDir, gen, "sent")
	n := 0
	for _, name := range g11rNames(t, dir) {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("reading %s: %v", filepath.Join(dir, name), err)
		}
		if bytes.Contains(raw, []byte(needle)) {
			n++
		}
	}
	return n
}

type g11rEvent struct {
	V          int    `json:"v"`
	Type       string `json:"type"`
	Session    string `json:"session"`
	Generation string `json:"generation"`
	Out        *int64 `json:"out"`
	Msg        string `json:"msg"`
	Code       *int   `json:"code"`
	At         string `json:"at"`
}

func g11rParseEvents(t *testing.T, raw []byte) []g11rEvent {
	t.Helper()
	var evs []g11rEvent
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line == "" {
			continue
		}
		var e g11rEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("events.jsonl holds a line that is not JSON: %v\n%s", err, raw)
		}
		evs = append(evs, e)
	}
	return evs
}

func g11rTypes(evs []g11rEvent) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}

// g11rLineStart returns the byte offset of the n-th (0-based) line of out that
// holds needle, or -1.
func g11rLineStart(out []byte, needle string, n int) int64 {
	var off int64
	for _, line := range bytes.SplitAfter(out, []byte("\n")) {
		if bytes.Contains(line, []byte(needle)) {
			if n == 0 {
				return off
			}
			n--
		}
		off += int64(len(line))
	}
	return -1
}

func (r *g11dRig) readFile(t *testing.T, sid, gen, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.agentsDir(sid), gen, name))
	if err != nil {
		t.Fatalf("reading %s of %s/%s: %v", name, sid, gen, err)
	}
	return raw
}

// Criterion 1: a host with no detach_sessions key runs its session under the
// runner, and the session survives a dropped link with no resume.
func TestHostSessionRunsUnderRunnerByDefault(t *testing.T) {
	t.Parallel()
	r := g11dStartWith(t, "")
	i := r.i
	sid := r.startSession(t, "")
	run := r.run(t, sid, "g1")

	gen := filepath.Join(r.agentsDir(sid), "g1")
	for _, d := range []string{"inbox", "sent"} {
		if st, err := os.Stat(filepath.Join(gen, d)); err != nil || !st.IsDir() {
			t.Fatalf("the generation has no %s/ directory: %v", d, err)
		}
	}
	if run.V != 1 || run.Session != sid || run.Started == "" {
		t.Fatalf("runner.json reads %+v, want v 1, session %s and a start time", run, sid)
	}
	g1sSendText(t, i, sid, "hi", "echo: hi")

	g11dDrop(t, i, sid)
	if !g11dAlive(run.PID) {
		t.Fatalf("runner pid %d is not running after the link dropped", run.PID)
	}
	g11dRestore(t, i, sid)
	g1sSendText(t, i, sid, "after the drop", "echo: after the drop")

	if after := r.run(t, sid, "g1"); after.PID != run.PID || after.PGID != run.PGID || after.Started != run.Started {
		t.Fatalf("runner.json went from %+v to %+v across the drop", run, after)
	}
	r.onlyGeneration(t, sid, "g1", "after the drop")
	if got := i.Events(harness.EventQuery{Key: "session.resume"}); len(got) != 0 {
		t.Fatalf("the message after the drop caused %d session.resume lines, want none", len(got))
	}
}

// The power door: detach_sessions false runs the session under ssh -T. The
// host gets no run directory and no call carries the runner.
func TestHostSessionSSHFallbackWhenDetachOff(t *testing.T) {
	t.Parallel()
	r := g11dStartWith(t, `"detach_sessions":false,`)
	i := r.i
	base := len(i.SSHCalls())
	sid := r.startSession(t, "")
	g1sSendText(t, i, sid, "hi", "echo: hi")

	if _, err := os.Stat(r.agentsDir(sid)); !os.IsNotExist(err) {
		t.Fatalf("the run directory %s exists for a host with detach_sessions false: stat err = %v", r.agentsDir(sid), err)
	}
	calls := i.SSHCalls()
	if len(calls) < base {
		t.Fatalf("the ssh call log shrank from %d to %d entries", base, len(calls))
	}
	claude := 0
	for _, c := range calls[base:] {
		s := c.Remote + "\n" + c.Script()
		if strings.Contains(s, "inbox") || strings.Contains(s, "runner.json") {
			t.Fatalf("a session on a host with detach_sessions false made a runner call: %.300s", s)
		}
		if strings.Contains(c.Script(), "claude") && strings.Contains(c.Script(), "stream-json") {
			claude++
		}
	}
	if claude != 1 {
		t.Fatalf("%d ssh calls ran the claude command, want 1", claude)
	}
}

// g11rTmuxCommand matches tmux used as a command: at a command position,
// followed by a subcommand.
var g11rTmuxCommand = regexp.MustCompile(`(^|[\s;|&/])tmux\s+[a-z-]+`)

// Criterion 6: the runner never calls tmux. A tmux on the host's PATH records
// every call it gets; a session still runs headless and none is recorded.
func TestHostRunnerWithoutTmux(t *testing.T) {
	t.Parallel()
	r := g11dStartWith(t, "")
	i := r.i

	// Installed after the probe, so only calls from the session count. The
	// shim fails as a host without a usable tmux would.
	marker := filepath.Join(r.remote, "tmux-called")
	shim := "#!/bin/sh\necho \"$@\" >> " + agShQuote(marker) + "\nexit 127\n"
	r.write(t, filepath.Join(r.remote, "hostclaude", "tmux"), shim, 0o755)

	base := len(i.SSHCalls())
	sid := r.startSession(t, "")
	run := r.run(t, sid, "g1")
	g1sSendText(t, i, sid, "hi", "echo: hi")
	if !g11dAlive(run.PID) {
		t.Fatalf("runner pid %d is not running", run.PID)
	}

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the host's tmux was called during a headless session: stat err = %v", err)
	}
	for _, c := range i.SSHCalls()[base:] {
		// Every Unix host command travels in a base64 launcher, so the decoded
		// script is what names a command. A comment that mentions tmux is not a call.
		if m := g11rTmuxCommand.FindString(c.Script()); m != "" {
			t.Fatalf("an ssh call runs tmux: %q in %.300s", m, c.Script())
		}
		if strings.Contains(c.Remote, "tmux") {
			t.Fatalf("an ssh call names tmux: %.300s", c.Remote)
		}
	}
}

// startLegacyRun turns the session's g1 into a run from before runner.json:
// the runner's group ends, its files go, and a stand-in leads a group named by
// a run.json. relay must be stopped.
func (r *g11dRig) startLegacyRun(t *testing.T, sid string, run g11dRun) (pid int, reaped <-chan struct{}) {
	t.Helper()
	killGroupWhileDown(t, run)
	gen := filepath.Join(r.agentsDir(sid), "g1")
	for _, name := range []string{"runner.json", "inbox", "sent", "events.jsonl", "exit"} {
		if err := os.RemoveAll(filepath.Join(gen, name)); err != nil {
			t.Fatalf("removing %s: %v", name, err)
		}
	}
	r.write(t, filepath.Join(gen, "in.jsonl"), "", 0o600)

	other := exec.Command("/bin/sh", "-c", "exec sleep 600")
	other.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := other.Start(); err != nil {
		t.Fatalf("starting the legacy stand-in: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = other.Wait(); close(done) }()
	t.Cleanup(func() { _ = syscall.Kill(-other.Process.Pid, syscall.SIGKILL); <-done })

	lstart, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(other.Process.Pid)).Output()
	if err != nil {
		t.Fatalf("reading the start time of pid %d: %v", other.Process.Pid, err)
	}
	legacy, _ := json.Marshal(map[string]any{
		"pid": other.Process.Pid, "pgid": other.Process.Pid,
		"started": strings.Join(strings.Fields(string(lstart)), " "), "generation": "g1",
	})
	r.write(t, filepath.Join(gen, "run.json"), string(legacy), 0o600)
	return other.Process.Pid, done
}

// Decision 13: a live run from before runner.json is stopped once, the
// session reads dormant, and a resume starts a runner generation on the same
// conversation.
func TestDetachedLegacyRunStoppedAndResumed(t *testing.T) {
	t.Parallel()
	r := g11dStart(t)
	i := r.i
	sid := r.startSession(t, "")
	run := r.run(t, sid, "g1")
	g1sSendText(t, i, sid, "hi", "echo: hi")
	conversation := r.conversationID(t, sid, "g1")
	before := r.sessionRow(t, sid)

	i.Stop()
	legacyPID, reaped := r.startLegacyRun(t, sid, run)
	r.restart(t)

	res := i.MustCLI("session", "resume", "--id", sid, "--json")
	var resumed struct {
		SessionID string `json:"session_id"`
		Resumed   bool   `json:"resumed"`
	}
	res.JSON(t, &resumed)
	if !resumed.Resumed || resumed.SessionID != sid {
		t.Fatalf("resume printed %+v, want resumed true for %s", resumed, sid)
	}

	// The stop is synchronous: the reaper only has to see the exit the stop
	// already caused.
	select {
	case <-reaped:
	case <-time.After(g1tFrameDeadline):
		t.Fatalf("the legacy run (pid %d) still runs after the resume returned", legacyPID)
	}
	next := r.run(t, sid, "g2")
	if next.PID == legacyPID || !g11dAlive(next.PID) {
		t.Fatalf("g2 runs as pid %d (legacy %d), want a new running runner", next.PID, legacyPID)
	}
	if gens := r.generations(t, sid); len(gens) != 2 || gens[0] != "g1" || gens[1] != "g2" {
		t.Fatalf("generations are %v, want g1 and g2", gens)
	}
	g11dUntil(t, "the resumed agent to start with --resume "+conversation, func() bool {
		for _, argv := range r.hostStarts(t) {
			if agHasPair(argv, "--resume", conversation) {
				return true
			}
		}
		return false
	})
	if row := r.sessionRow(t, sid); !row.Live || row.MessageCount != before.MessageCount {
		t.Fatalf("resumed row live=%v with %d messages, want live with the %d from before", row.Live, row.MessageCount, before.MessageCount)
	}
	g1sSendText(t, i, sid, "again", "echo: again")
}

// stallAgent replaces the host's claude with a wrapper around the fake.
// After it forwards a user line holding "!stall" it stops reading its input
// until gate (a fifo) is opened, so the runner's next write to it blocks once
// the pipe is full. The turn of the stalling line itself ends normally.
func (r *g11dRig) stallAgent(t *testing.T) (gate string) {
	t.Helper()
	gate = filepath.Join(r.remote, "stall.fifo")
	if err := syscall.Mkfifo(gate, 0o600); err != nil {
		t.Fatalf("creating %s: %v", gate, err)
	}
	fake := filepath.Join(r.i.Home, ".local", "bin", "claude")
	script := "#!/bin/sh\n" +
		"{\n" +
		"  while IFS= read -r l; do\n" +
		"    printf '%s\\n' \"$l\"\n" +
		"    case $l in\n" +
		"      *'!stall'*) read x < " + agShQuote(gate) + " ;;\n" +
		"    esac\n" +
		"  done\n" +
		"} | exec " + agShQuote(fake) + " \"$@\"\n"
	r.write(t, filepath.Join(r.remote, "hostclaude", "claude"), script, 0o755)
	return gate
}

// Criterion 3: a send returns success only once the message is in sent/, and
// the move follows the write to the agent. The agent stops reading, so the
// runner's write of a message larger than the pipe cannot finish. A runner
// that moved the message first would give the sender its receipt at once; the
// runner that moves it after the write gives none, and the sender's receipt
// wait ends with send_unknown (exit 7 of the runner send) while the message
// stays queued in inbox/. The deadline of that wait is the runner's own 25 s.
func TestRunnerSendReturnsAfterReceipt(t *testing.T) {
	t.Parallel()
	r := g11dStartWith(t, "")
	i := r.i
	gate := r.stallAgent(t)
	sid := r.startSession(t, "")
	r.run(t, sid, "g1")
	gen := filepath.Join(r.agentsDir(sid), "g1")

	// A send that returns has its message in sent/ and none left in inbox/.
	g1vSend(t, i, sid, "receipt probe")
	if n := g11rSentCount(t, r.agentsDir(sid), "g1", "receipt probe"); n != 1 {
		t.Fatalf("sent/ holds %d copies of the message after its send returned, want 1", n)
	}
	if left := g11rNames(t, filepath.Join(gen, "inbox")); len(left) != 0 {
		t.Fatalf("inbox/ still holds %v after the send returned", left)
	}
	for _, name := range g11rNames(t, filepath.Join(gen, "sent")) {
		if !g11rMessageName.MatchString(name) {
			t.Fatalf("sent/ holds %q, want <19 digits>-<6 hex>.json or .ctl", name)
		}
	}

	// The next turn is the last the agent reads until the gate opens.
	if _, w := g1vWaitFor(t, i, sid); w.State != "idle" {
		t.Fatalf("wait after the probe reads %q, want idle", w.State)
	}
	g1sSendText(t, i, sid, "!stall", "echo: !stall")
	sentBefore := g11rNames(t, filepath.Join(gen, "sent"))

	big := "BIG" + strings.Repeat("a", 600000)
	body, _ := json.Marshal(map[string]string{"text": big})
	res := i.CLIWith(harness.CLIOpts{Stdin: body, Deadline: 58 * time.Second},
		"session", "send", "--id", sid, "--file", "-", "--json")
	if res.Code != 1 || g1vErrCode(res) != "send_unknown" {
		t.Fatalf("a send the agent has not read exited %d with code %q, want 1 and send_unknown\nstdout: %.300s\nstderr: %.300s",
			res.Code, g1vErrCode(res), res.Stdout, res.Stderr)
	}
	queued := g11rNames(t, filepath.Join(gen, "inbox"))
	if len(queued) != 1 || !g11rMessageName.MatchString(queued[0]) {
		t.Fatalf("inbox/ holds %v after the receipt wait ended, want the one queued message", queued)
	}
	if now := g11rNames(t, filepath.Join(gen, "sent")); strings.Join(now, ",") != strings.Join(sentBefore, ",") {
		t.Fatalf("sent/ went from %v to %v while the agent had not read the message", sentBefore, now)
	}

	// The agent reads again: the queued message is written once, then moved.
	opened := make(chan *os.File, 1)
	go func() {
		f, err := os.OpenFile(gate, os.O_WRONLY, 0)
		if err != nil {
			close(opened)
			return
		}
		opened <- f
	}()
	select {
	case f := <-opened:
		if f == nil {
			t.Fatalf("opening %s failed", gate)
		}
		_ = f.Close()
	case <-time.After(g1tFrameDeadline):
		t.Fatalf("the agent never waited on %s", gate)
	}
	id := strings.TrimSuffix(queued[0], filepath.Ext(queued[0]))
	g11dUntil(t, "the queued message to move to sent/ with its reply written", func() bool {
		_, err := os.Stat(filepath.Join(gen, "sent", queued[0]))
		out, _ := os.ReadFile(filepath.Join(gen, "out.jsonl"))
		var st g11dRun
		raw, _ := os.ReadFile(filepath.Join(gen, "runner.json"))
		_ = json.Unmarshal(raw, &st)
		return err == nil && bytes.Count(out, []byte(`"type":"result"`)) == 3 && st.Delivered == id
	})
	if n := bytes.Count(r.readFile(t, sid, "g1", "out.jsonl"), []byte(`"text":"echo: BIG`)); n != 1 {
		t.Fatalf("the queued message was answered %d times, want once", n)
	}
	if left := g11rNames(t, filepath.Join(gen, "inbox")); len(left) != 0 {
		t.Fatalf("inbox/ still holds %v after delivery", left)
	}
}

// Criterion 4: a reader that comes back at its saved cursor gets each line the
// agent wrote during the drop once and in order, and none it had read.
func TestRunnerCursorAcrossDropEachLineOnce(t *testing.T) {
	t.Parallel()
	r := g11dStartWith(t, "")
	i := r.i
	sid := r.startSession(t, "")
	r.run(t, sid, "g1")
	g1sSendText(t, i, sid, "hi", "echo: hi")

	first := g1vEventsAfter(t, i, sid, "")
	if g1vAssistantCount(first.Events, "echo: hi") != 1 {
		t.Fatalf("the first read holds no single reply to hi: %s", g1vJoin(first.Events))
	}

	// A turn held on a fifo ends while the link is down.
	text, opened := g1vHold(t, r.remote)
	g1vSend(t, i, sid, text)
	hold := g1vReading(t, opened)
	g11dDrop(t, i, sid)
	if err := hold.Close(); err != nil {
		t.Fatalf("closing the hold: %v", err)
	}
	out := filepath.Join(r.agentsDir(sid), "g1", "out.jsonl")
	g11dUntil(t, "the agent to write the held turn's second result to out.jsonl", func() bool {
		raw, _ := os.ReadFile(out)
		return bytes.Count(raw, []byte(`"type":"result"`)) == 2
	})
	g11dRestore(t, i, sid)
	if res, w := g1vWaitFor(t, i, sid); res.Code != 0 || w.State != "idle" {
		t.Fatalf("wait after the drop exited %d with state %q, want 0 and idle", res.Code, w.State)
	}

	second := g1vEventsAfter(t, i, sid, first.Next)
	if n := g1vAssistantCount(second.Events, "exit: "); n != 1 {
		t.Fatalf("the read at the saved cursor holds the held turn's reply %d times, want once: %s", n, g1vJoin(second.Events))
	}
	if n := g1vAssistantCount(second.Events, "echo: hi"); n != 0 {
		t.Fatalf("the read at the saved cursor repeats %d lines it had already delivered", n)
	}
	third := g1vEventsAfter(t, i, sid, second.Next)
	if len(third.Events) != 0 {
		t.Fatalf("a read at the cursor of the last line holds %d events, want none: %s", len(third.Events), g1vJoin(third.Events))
	}

	whole := g1vEventsAfter(t, i, sid, "")
	chained := append(append([]json.RawMessage{}, first.Events...), second.Events...)
	if g1vJoin(whole.Events) != g1vJoin(chained) {
		t.Fatalf("the reads chained by cursor differ from one read of the whole run\nchained:\n%s\nwhole:\n%s",
			g1vJoin(chained), g1vJoin(whole.Events))
	}
}

// Criterion 7: the runner appends one event line per state change, each with
// the byte offset of the line in out.jsonl that caused it.
func TestRunnerWritesEventLines(t *testing.T) {
	t.Parallel()
	r := g11dStartWith(t, "")
	i := r.i
	sid := r.startSession(t, "")
	r.run(t, sid, "g1")
	gen := filepath.Join(r.agentsDir(sid), "g1")
	// Open before the stop, which removes the directory: an open file stays
	// readable.
	events, err := os.Open(filepath.Join(gen, "events.jsonl"))
	if err != nil {
		t.Fatalf("opening events.jsonl: %v", err)
	}
	defer events.Close()
	outFile, err := os.Open(filepath.Join(gen, "out.jsonl"))
	if err != nil {
		t.Fatalf("opening out.jsonl: %v", err)
	}
	defer outFile.Close()

	// An event comes before the line that causes it reaches out.jsonl, so a
	// reader that has the reply has the events: no wait.
	g1sSendText(t, i, sid, "hi", "echo: hi")
	out := r.readFile(t, sid, "g1", "out.jsonl")
	evs := g11rParseEvents(t, r.readFile(t, sid, "g1", "events.jsonl"))
	if got := g11rTypes(evs); strings.Join(got, ",") != "turn_started,turn_ended" {
		t.Fatalf("events after one turn are %v, want turn_started, turn_ended", got)
	}
	started, ended := evs[0], evs[1]
	if started.Out == nil || *started.Out != 0 {
		t.Fatalf("turn_started out is %v, want 0: out.jsonl was empty when the message was written", started.Out)
	}
	if want := g11rLineStart(out, `"type":"result"`, 0); ended.Out == nil || *ended.Out != want {
		t.Fatalf("turn_ended out is %v, want %d, the start of the result line", ended.Out, want)
	}
	if _, err := os.Stat(filepath.Join(gen, "sent", started.Msg+".json")); err != nil {
		t.Fatalf("turn_started names message %q, which is not in sent/: %v", started.Msg, err)
	}
	afterFirst := int64(len(out))

	// A permission prompt: the turn starts, the agent asks, the answer is a
	// control message that starts no turn, and the turn ends.
	ws := i.WebSocket("/ws", i.Credential("mounter"))
	defer ws.Close()
	ws.Send(map[string]any{"type": "join_session", "sessionId": sid})
	g1vSend(t, i, sid, "!ask Write")
	var ask g11rPermission
	for n := 0; ask.PermissionID == ""; n++ {
		if n == 4*g1tFrameLimit {
			t.Fatalf("no permission_request for %s in %d frames", sid, n)
		}
		var f g11rPermission
		if err := json.Unmarshal(ws.Next(g1tFrameDeadline), &f); err != nil {
			t.Fatalf("decoding a frame: %v", err)
		}
		if f.Type == "permission_request" && f.SessionID == sid {
			ask = f
		}
	}
	out = r.readFile(t, sid, "g1", "out.jsonl")
	evs = g11rParseEvents(t, r.readFile(t, sid, "g1", "events.jsonl"))
	if got := g11rTypes(evs); strings.Join(got, ",") != "turn_started,turn_ended,turn_started,asking" {
		t.Fatalf("events at the prompt are %v, want turn_started, turn_ended, turn_started, asking", got)
	}
	if evs[2].Out == nil || *evs[2].Out != afterFirst {
		t.Fatalf("second turn_started out is %v, want %d, the size of out.jsonl at the write", evs[2].Out, afterFirst)
	}
	if want := g11rLineStart(out, `"subtype":"can_use_tool"`, 0); evs[3].Out == nil || *evs[3].Out != want {
		t.Fatalf("asking out is %v, want %d, the start of the permission request line", evs[3].Out, want)
	}

	ws.Send(map[string]any{"type": "permission_response", "permissionId": ask.PermissionID, "approved": true})
	b367Next(t, ws, sid, "session_state", "idle")
	out = r.readFile(t, sid, "g1", "out.jsonl")
	evs = g11rParseEvents(t, r.readFile(t, sid, "g1", "events.jsonl"))
	if got := g11rTypes(evs); strings.Join(got, ",") != "turn_started,turn_ended,turn_started,asking,turn_ended" {
		t.Fatalf("events after the answer are %v, want a fifth, turn_ended, and no new turn_started", got)
	}
	if want := g11rLineStart(out, `"type":"result"`, 1); evs[4].Out == nil || *evs[4].Out != want {
		t.Fatalf("second turn_ended out is %v, want %d, the start of the second result line", evs[4].Out, want)
	}

	// Stop: the runner is ended by TERM and writes exited with the agent's code
	// and the final size of out.jsonl.
	i.MustCLI("session", "stop", "--id", sid, "--json")
	rest, err := readAllFrom(events)
	if err != nil {
		t.Fatalf("reading events.jsonl after the stop: %v", err)
	}
	final, err := outFile.Stat()
	if err != nil {
		t.Fatalf("stat out.jsonl after the stop: %v", err)
	}
	evs = g11rParseEvents(t, rest)
	if got := g11rTypes(evs); len(got) != 6 || got[5] != "exited" {
		t.Fatalf("events after the stop are %v, want the five from before and exited last", got)
	}
	exited := evs[5]
	if exited.Code == nil || *exited.Code != 0 || exited.Out == nil || *exited.Out != final.Size() {
		t.Fatalf("exited reads code %v and out %v, want code 0 and out %d, the final size", exited.Code, exited.Out, final.Size())
	}

	for _, e := range evs {
		if e.V != 1 || e.Session != sid || e.Generation != "g1" {
			t.Fatalf("event %+v, want v 1, session %s and generation g1", e, sid)
		}
		if _, err := time.Parse(time.RFC3339, e.At); err != nil {
			t.Fatalf("event %s has at %q, want RFC3339: %v", e.Type, e.At, err)
		}
	}
}

// readAllFrom reads f from its start to its current end.
func readAllFrom(f *os.File) ([]byte, error) {
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	_, err := b.ReadFrom(f)
	return b.Bytes(), err
}

// Criterion 1 on a host whose awk is mawk (the Debian and Ubuntu default),
// which block-buffers piped output. A runner that does not line-buffer shows no
// reply until the agent exits, so the reply must reach the sender while the
// agent still runs.
func TestHostRunnerLinesFlowUnderMawk(t *testing.T) {
	t.Parallel()
	r := g11dStartWith(t, "")
	i := r.i

	mawk, err := exec.LookPath("mawk")
	if err != nil {
		t.Fatalf("mawk is not installed on this machine (brew install mawk): %v", err)
	}
	// The host's PATH leads with hostclaude, so the link wins over the system awk.
	if err := os.Symlink(mawk, filepath.Join(r.remote, "hostclaude", "awk")); err != nil {
		t.Fatalf("linking awk to %s: %v", mawk, err)
	}

	sid := r.startSession(t, "")
	run := r.run(t, sid, "g1")
	g1sSendText(t, i, sid, "hi", "echo: hi")

	if !g11dAlive(run.PID) {
		t.Fatalf("runner pid %d is not running: the reply came only at the agent's exit", run.PID)
	}
	evs := g11rParseEvents(t, r.readFile(t, sid, "g1", "events.jsonl"))
	if got := g11rTypes(evs); strings.Join(got, ",") != "turn_started,turn_ended" {
		t.Fatalf("events after one turn under mawk are %v, want turn_started, turn_ended", got)
	}
	if n := g1vAssistantCount(g1vEventsAfter(t, i, sid, "").Events, "echo: hi"); n != 1 {
		t.Fatalf("events hold %d replies to hi, want 1", n)
	}
}
