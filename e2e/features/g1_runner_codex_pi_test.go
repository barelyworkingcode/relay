package features

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"relaye2e/harness"
)

// Codex and pi sessions run under the file-channel runner, as Claude's do
// (docs/session-host.md, "Codex under the runner" and "pi"; docs/ssh-hosts.md,
// "Codex on a host"). A local run directory is
// <config dir>/sessions/runners/<session id>/<generation>/; on the fake host it
// is <remote root>/.relay/agents/<session id>/<generation>/. The agent outlives
// relay, so these tests read run directories and the process table directly.

// g1cKind names one agent kind the tests drive and what the runner matches in
// its output.
type g1cKind struct {
	name      string
	turnEnded string // the fixed string the runner matches to end a turn
	// turnOutput lists fixed strings that appear only in the agent's output for
	// the first turn: the earliest of those lines is where turn_started points.
	turnOutput []string
}

var (
	g1cCodex = g1cKind{name: "codex", turnEnded: `"method":"turn/completed"`,
		turnOutput: []string{`"status":"inProgress"`}}
	g1cPi = g1cKind{name: "pi", turnEnded: `"type":"agent_settled"`,
		turnOutput: []string{`"command":"prompt"`, `"type":"agent_start"`}}
)

// g1cKillRuns ends every run of the session under the local runners directory
// at cleanup, so a failing test leaves no agent behind.
func g1cKillRuns(t *testing.T, i *harness.Instance, sid string) {
	t.Helper()
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
}

// g1cLocal boots an instance and starts one local session of the kind.
func g1cLocal(t *testing.T, k g1cKind) (*harness.Instance, string) {
	t.Helper()
	opts := harness.Options{Credentials: g1tCreds, Presence: g1tApprove}
	if k.name == "pi" {
		opts.FakeModelHost = fakeEchoModels()
	}
	i := harness.Start(t, opts)
	if k.name == "pi" {
		i.WaitModelHost(g1tHostDeadline)
	}
	i.WaitSessionHost(g1tHostDeadline)

	model := "codex/fake-echo"
	if k.name == "pi" {
		_, ids := modelIDs(t, i)
		model = ""
		for _, id := range ids {
			if strings.HasPrefix(id, "pi/") && strings.HasSuffix(id, "/fake-echo") {
				model = id
			}
		}
		if model == "" {
			t.Fatalf("model list %v holds no pi/<provider>/fake-echo", ids)
		}
	}
	p := g1tProject(t, i, "acme-"+k.name+"-runner", map[string]any{"allowed_templates": []string{k.name}})
	sid := g1tStartSession(t, i, p.ID, model, "")
	g1cKillRuns(t, i, sid)
	return i, sid
}

// g1cCtlLines counts the lines of the .ctl files in every generation's sent/
// and inbox/ under runners that hold needle.
func g1cCtlLines(t *testing.T, runners, needle string) int {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(runners, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".ctl" {
			return nil
		}
		dir := filepath.Base(filepath.Dir(path))
		if dir != "sent" && dir != "inbox" {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("reading %s: %v", path, rerr)
		}
		for _, line := range bytes.Split(raw, []byte("\n")) {
			if bytes.Contains(line, []byte(needle)) {
				n++
			}
		}
		return nil
	})
	return n
}

// g1cAppServerStarts counts the app-server starts in the fake codex's call
// log lines, given the log's start argvs.
func g1cAppServerStarts(argvs [][]string) int {
	n := 0
	for _, argv := range argvs {
		if len(argv) > 0 && argv[0] == "app-server" {
			n++
		}
	}
	return n
}

func g1cLocalCodexStarts(i *harness.Instance) int {
	var argvs [][]string
	for _, c := range i.FakeAgentCalls("codex") {
		if c.Event == "start" {
			argvs = append(argvs, c.Argv)
		}
	}
	return g1cAppServerStarts(argvs)
}

// g1cHostCodex boots an instance with the ssh stub, a Unix host with no
// detach_sessions key, a codex template whose command wraps the fake codex
// on the host, and one host project that allows codex.
func g1cHostCodex(t *testing.T) *g11dRig {
	t.Helper()
	seed := `[{"id":"` + g11dHostID + `","name":"testbox","target":"` + g11dTarget + `","created_at":"2026-01-01T00:00:00Z"}]`
	i := harness.Start(t, harness.Options{
		Credentials: g1tCreds, Presence: g1tApprove, SSHStub: true,
		Settings: map[string]json.RawMessage{"hosts": json.RawMessage(seed)},
	})
	i.WaitSessionHost(g1tHostDeadline)
	r := &g11dRig{i: i, remote: i.RemoteRoot()}

	bin := filepath.Join(r.remote, "hostcodex")
	r.mkdir(t, bin)
	fake := filepath.Join(i.Home, ".local", "bin", "codex")
	wrapper := filepath.Join(bin, "codex")
	r.write(t, wrapper, "#!/bin/sh\nexec "+agShQuote(fake)+" \"$@\"\n", 0o755)
	i.MustCLI("host", "probe", "--id", g11dHostID, "--json")

	tmpl := i.HTTP(i.Credential("configurer")).Do("POST", "/api/hosts/"+g11dHostID+"/templates",
		map[string]any{"id": "codex", "name": "Codex", "command": wrapper})
	if tmpl.Status != 201 {
		t.Fatalf("POST host codex template answered %d, want 201: %s", tmpl.Status, tmpl.Body)
	}

	r.dir = filepath.Join(r.remote, "boxproj")
	r.mkdir(t, r.dir)
	body, _ := json.Marshal(map[string]any{
		"name": "Acme Box", "path": r.dir, "host_id": g11dHostID, "allowed_templates": []string{"codex"},
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

// g1cHostCodexStarts counts the app-server starts the host-side fake codex
// logged. The stub gives the agent a TMPDIR under the remote root.
func g1cHostCodexStarts(t *testing.T, r *g11dRig) int {
	t.Helper()
	var argvs [][]string
	_ = filepath.WalkDir(filepath.Join(r.remote, "tmp"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "fakeagent-codex.jsonl" {
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
				argvs = append(argvs, c.Argv)
			}
		}
		return nil
	})
	return g1cAppServerStarts(argvs)
}

// Criterion 1: a local codex agent keeps running when relay restarts, and the
// next message is answered by the same agent.
func TestCodexRunnerSurvivesRelayRestart(t *testing.T) {
	t.Parallel()
	i, sid := g1cLocal(t, g1cCodex)
	g1sSendText(t, i, sid, "hi", "echo: hi")

	gen, before := g1lLatest(t, i, sid)
	if !g11dAlive(before.PID) {
		t.Fatalf("runner pid %d is not running before the restart", before.PID)
	}
	startsBefore := g1cLocalCodexStarts(i)

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
	if now := g1cLocalCodexStarts(i); now != startsBefore || now != 1 {
		t.Fatalf("the fake codex logged %d app-server starts (%d before the restart), want 1: a second agent started", now, startsBefore)
	}
	if got := i.Events(harness.EventQuery{Key: "session.resume"}); len(got) != 0 {
		t.Fatalf("the message after the restart caused %d session.resume lines, want none", len(got))
	}
}

// Criterion 2: a codex session on a Unix host with no detach_sessions key runs
// under the runner on the host, and a dropped link leaves the agent running.
func TestCodexHostRunnerSurvivesLinkDrop(t *testing.T) {
	t.Parallel()
	r := g1cHostCodex(t)
	i := r.i
	sid := g1tStartSession(t, i, r.projectID, "codex/fake-echo", "")
	run := r.run(t, sid, "g1")
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
	if n := g1cHostCodexStarts(t, r); n != 1 {
		t.Fatalf("the host's fake codex logged %d app-server starts, want 1", n)
	}
	if got := i.Events(harness.EventQuery{Key: "session.resume"}); len(got) != 0 {
		t.Fatalf("the message after the drop caused %d session.resume lines, want none", len(got))
	}
}

// Criterion 3: a pi session runs under the runner, and its runner is gone once
// relay has stopped; the session then reads dormant after a start.
func TestPiRunnerEndsAtShutdown(t *testing.T) {
	t.Parallel()
	i, sid := g1cLocal(t, g1cPi)
	g1sSendText(t, i, sid, "hi", "echo: hi")

	_, run := g1lLatest(t, i, sid)
	if !g11dAlive(run.PID) {
		t.Fatalf("runner pid %d is not running before the shutdown", run.PID)
	}

	// Stop returns after relay-sessions has exited, and its shutdown kills
	// every pi runner. kill(2) answers success for a killed process that is
	// not yet reaped, so the wait is on the process table: ESRCH, or a zombie
	// (g11dAlive), for the runner and then for its group.
	i.Stop()
	g11dUntil(t, "the pi runner "+strconv.Itoa(run.PID)+" to be gone after the shutdown", func() bool { return !g11dAlive(run.PID) })
	g11dUntil(t, "the pi runner's group "+strconv.Itoa(run.PGID)+" to be gone after the shutdown", func() bool {
		return errors.Is(syscall.Kill(-run.PGID, 0), syscall.ESRCH)
	})

	g1lRestart(t, i)
	list, _ := g1sListSessions(t, i)
	row, ok := g1sFindSession(list, sid)
	if !ok {
		t.Fatalf("session list holds no %s after the restart", sid)
	}
	if row.Live {
		t.Fatalf("a pi session reads live after a relay restart, want dormant")
	}
}

// Criterion 4: a send returns success only once the message is in sent/.
func TestCodexPiSendReturnsAfterReceipt(t *testing.T) {
	t.Parallel()
	for _, k := range []g1cKind{g1cCodex, g1cPi} {
		k := k
		t.Run(k.name, func(t *testing.T) {
			t.Parallel()
			i, sid := g1cLocal(t, k)
			gen, _ := g1lLatest(t, i, sid)
			dir := filepath.Join(g1lRunnersDir(i, sid), gen)

			g1vSend(t, i, sid, "receipt probe")
			if n := g11rSentCount(t, g1lRunnersDir(i, sid), gen, "receipt probe"); n != 1 {
				t.Fatalf("sent/ holds %d copies of the message after its send returned, want 1", n)
			}
			if left := g11rNames(t, filepath.Join(dir, "inbox")); len(left) != 0 {
				t.Fatalf("inbox/ still holds %v after the send returned", left)
			}
			sentJSON := 0
			for _, name := range g11rNames(t, filepath.Join(dir, "sent")) {
				if !g11rMessageName.MatchString(name) {
					t.Fatalf("sent/ holds %q, want <19 digits>-<6 hex>.json or .ctl", name)
				}
				raw, err := os.ReadFile(filepath.Join(dir, "sent", name))
				if err != nil {
					t.Fatalf("reading %s: %v", name, err)
				}
				if strings.HasSuffix(name, ".json") && bytes.Contains(raw, []byte("receipt probe")) {
					sentJSON++
				}
			}
			if sentJSON != 1 {
				t.Fatalf("sent/ holds %d .json messages with the text, want 1: the message starts a turn", sentJSON)
			}
			if _, w := g1vWaitFor(t, i, sid); w.State != "idle" {
				t.Fatalf("wait after the send reads %q, want idle", w.State)
			}
		})
	}
}

// Criterion 5: the runner writes turn_started, turn_ended and exited lines for
// codex and pi, each with the offset in out.jsonl of the line that caused it.
func TestCodexPiRunnerEventLines(t *testing.T) {
	t.Parallel()
	for _, k := range []g1cKind{g1cCodex, g1cPi} {
		k := k
		t.Run(k.name, func(t *testing.T) {
			t.Parallel()
			i, sid := g1cLocal(t, k)
			gen, _ := g1lLatest(t, i, sid)
			dir := filepath.Join(g1lRunnersDir(i, sid), gen)
			// Open before the stop, which removes the directory: an open file
			// stays readable.
			events, err := os.Open(filepath.Join(dir, "events.jsonl"))
			if err != nil {
				t.Fatalf("opening events.jsonl: %v", err)
			}
			defer events.Close()
			outFile, err := os.Open(filepath.Join(dir, "out.jsonl"))
			if err != nil {
				t.Fatalf("opening out.jsonl: %v", err)
			}
			defer outFile.Close()

			g1sSendText(t, i, sid, "hi", "echo: hi")
			// The event precedes the line that causes it, so the event is
			// readable once that line is. Pi's reply completes at agent_end and
			// its turn ends on the later agent_settled line, which has no
			// signal of its own.
			g11dUntil(t, "the "+k.name+" turn-ending line in out.jsonl", func() bool {
				raw, err := os.ReadFile(filepath.Join(dir, "out.jsonl"))
				return err == nil && bytes.Contains(raw, []byte(k.turnEnded))
			})
			out, err := os.ReadFile(filepath.Join(dir, "out.jsonl"))
			if err != nil {
				t.Fatalf("reading out.jsonl: %v", err)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
			if err != nil {
				t.Fatalf("reading events.jsonl: %v", err)
			}
			evs := g11rParseEvents(t, raw)
			if got := g11rTypes(evs); strings.Join(got, ",") != "turn_started,turn_ended" {
				t.Fatalf("events after one %s turn are %v, want turn_started, turn_ended", k.name, got)
			}
			started, ended := evs[0], evs[1]
			firstTurnLine := int64(-1)
			for _, needle := range k.turnOutput {
				if at := g11rLineStart(out, needle, 0); at >= 0 && (firstTurnLine < 0 || at < firstTurnLine) {
					firstTurnLine = at
				}
			}
			if firstTurnLine < 0 {
				t.Fatalf("out.jsonl holds none of %v:\n%s", k.turnOutput, out)
			}
			if started.Out == nil || *started.Out != firstTurnLine {
				t.Fatalf("turn_started out is %v, want %d, the size of out.jsonl when the message was written", started.Out, firstTurnLine)
			}
			if want := g11rLineStart(out, k.turnEnded, 0); ended.Out == nil || *ended.Out != want {
				t.Fatalf("turn_ended out is %v, want %d, the start of the %s line", ended.Out, want, k.turnEnded)
			}
			if _, err := os.Stat(filepath.Join(dir, "sent", started.Msg+".json")); err != nil {
				t.Fatalf("turn_started names message %q, which is not a .json in sent/: %v", started.Msg, err)
			}

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
			if got := g11rTypes(evs); len(got) != 3 || got[2] != "exited" {
				t.Fatalf("events after the stop are %v, want turn_started, turn_ended, exited", got)
			}
			if exited := evs[2]; exited.Out == nil || *exited.Out != final.Size() {
				t.Fatalf("exited out is %v, want %d, the final size of out.jsonl", exited.Out, final.Size())
			}
			for _, e := range evs {
				if e.V != 1 || e.Session != sid || e.Generation != gen {
					t.Fatalf("event %+v, want v 1, session %s and generation %s", e, sid, gen)
				}
			}
		})
	}
}

// Criterion 6: relay sends no second initialize to a running codex agent after
// a restart, and starts no second agent.
func TestCodexRunnerNoSecondInitialize(t *testing.T) {
	t.Parallel()
	i, sid := g1cLocal(t, g1cCodex)
	g1sSendText(t, i, sid, "hi", "echo: hi")
	runners := g1lRunnersDir(i, sid)
	if n := g1cCtlLines(t, runners, `"method":"initialize"`); n != 1 {
		t.Fatalf("the run holds %d initialize .ctl lines after the start, want 1", n)
	}

	for restart := 1; restart <= 2; restart++ {
		g1lRestart(t, i)
		g1sSendText(t, i, sid, "after", "echo: after")
	}

	if n := g1cCtlLines(t, runners, `"method":"initialize"`); n != 1 {
		t.Fatalf("the run holds %d initialize .ctl lines across two restarts, want 1", n)
	}
	if gens := g1lGenerations(t, i, sid); len(gens) != 1 {
		t.Fatalf("generations after the restarts are %v, want one", gens)
	}
	if n := g1cLocalCodexStarts(i); n != 1 {
		t.Fatalf("the fake codex logged %d app-server starts, want 1", n)
	}
}

// Amendment 3: the runner gives the agent a pipe for stderr and copies what it
// writes to stderr.log, so a sandboxed agent never holds a file the session
// profile does not grant (docs/session-host.md, "What a child inherits").
func TestRunnerAgentStderrIsPipe(t *testing.T) {
	t.Parallel()
	i, sid := g1cLocal(t, g1cPi)
	gen, _ := g1lLatest(t, i, sid)
	stderrLog := filepath.Join(g1lRunnersDir(i, sid), gen, "stderr.log")

	// The fake writes one stderr line at start; its arrival in stderr.log is
	// the signal that the start line is in the call log.
	g11dUntil(t, "the fake pi's stderr line in stderr.log", func() bool {
		raw, err := os.ReadFile(stderrLog)
		return err == nil && bytes.Contains(raw, []byte("fakeagent: stderr probe"))
	})

	var kinds []string
	_ = filepath.WalkDir(i.Tmp, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "fakeagent-pi.jsonl" {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("reading %s: %v", path, rerr)
		}
		for _, line := range bytes.Split(raw, []byte("\n")) {
			var c struct {
				Event      string   `json:"event"`
				Argv       []string `json:"argv"`
				StderrKind string   `json:"stderr_kind"`
			}
			// The model listing is a pi start too; only the rpc agent counts.
			if json.Unmarshal(line, &c) == nil && c.Event == "start" && slices.Contains(c.Argv, "rpc") {
				kinds = append(kinds, c.StderrKind)
			}
		}
		return nil
	})
	if len(kinds) != 1 || kinds[0] != "fifo" {
		t.Fatalf("the fake pi's start lines record stderr_kind %v, want exactly [fifo]", kinds)
	}
}
