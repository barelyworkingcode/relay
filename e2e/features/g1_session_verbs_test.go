package features

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"relaye2e/harness"
)

// The session verbs send, wait and events let a coordinator drive a turn
// without holding a connection for it. A turn is held open on a fifo: the
// fake agent runs `read x < fifo`, and the test's write end opens only once
// the agent reads, which proves the turn is running; closing it ends the turn.

type g1vWait struct {
	SessionID string `json:"session_id"`
	State     string `json:"state"`
	Excerpt   string `json:"excerpt"`
	Exit      *struct {
		Code   int       `json:"code"`
		Stderr *[]string `json:"stderr"`
	} `json:"exit"`
}

type g1vEvents struct {
	SessionID string            `json:"session_id"`
	Events    []json.RawMessage `json:"events"`
	Next      string            `json:"next"`
}

// g1vHold makes a fifo under dir and returns the turn text that blocks on it
// and the channel that yields the write end once the agent reads;
// g1vReading closes it at cleanup so a failing test frees the agent.
func g1vHold(t *testing.T, dir string) (string, <-chan *os.File) {
	t.Helper()
	fifo := filepath.Join(dir, "hold-"+harness.NewTrace(t)[3:11]+".fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("creating %s: %v", fifo, err)
	}
	opened := make(chan *os.File, 1)
	go func() {
		f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			close(opened)
			return
		}
		opened <- f
	}()
	return "!sh read x < " + agShQuote(fifo), opened
}

// g1vReading waits until the agent reads the fifo: the turn is running.
func g1vReading(t *testing.T, opened <-chan *os.File) *os.File {
	t.Helper()
	select {
	case f := <-opened:
		if f == nil {
			t.Fatalf("opening the hold fifo failed")
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	case <-time.After(g1tFrameDeadline):
		t.Fatalf("the agent never read the hold fifo: the turn did not reach it")
	}
	return nil
}

// g1vErrCode reads the error code a failed verb printed, from stdout or
// stderr, at the top level or under "error".
func g1vErrCode(res harness.Result) string {
	for _, b := range [][]byte{res.Stdout, res.Stderr} {
		var m map[string]any
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		if s, ok := m["code"].(string); ok {
			return s
		}
		if e, ok := m["error"].(map[string]any); ok {
			if s, ok := e["code"].(string); ok {
				return s
			}
		}
	}
	return ""
}

// g1vAsync waits for p on its own goroutine so a test can check the process
// has not returned yet.
func g1vAsync(p *harness.Proc) <-chan harness.Result {
	done := make(chan harness.Result, 1)
	go func() { done <- p.Wait() }()
	return done
}

func g1vSend(t *testing.T, i *harness.Instance, sid, text string) {
	t.Helper()
	var out struct {
		SessionID string `json:"session_id"`
		At        string `json:"at"`
	}
	i.MustCLI("session", "send", "--id", sid, "--text", text, "--json").JSON(t, &out)
	if out.SessionID != sid || out.At == "" {
		t.Fatalf("send printed %+v, want session_id %s and an at time", out, sid)
	}
}

func g1vWaitFor(t *testing.T, i *harness.Instance, sid string, extra ...string) (harness.Result, g1vWait) {
	t.Helper()
	args := append([]string{"session", "wait", "--id", sid, "--json"}, extra...)
	res := i.CLI(args...)
	var w g1vWait
	res.JSON(t, &w)
	return res, w
}

func g1vLocal(t *testing.T) (*harness.Instance, string) {
	t.Helper()
	i := harness.Start(t, harness.Options{Credentials: g1tCreds, Presence: g1tApprove})
	i.WaitSessionHost(g1tHostDeadline)
	p := g1tProject(t, i, "acme-verbs", map[string]any{"allowed_templates": []string{"claude-code"}})
	return i, g1tStartSession(t, i, p.ID, "sonnet", "")
}

func g1vEventsAfter(t *testing.T, i *harness.Instance, sid, after string) g1vEvents {
	t.Helper()
	args := []string{"session", "events", "--id", sid, "--json"}
	if after != "" {
		args = append(args, "--after", after)
	}
	var out g1vEvents
	i.MustCLI(args...).JSON(t, &out)
	if out.SessionID != sid || out.Next == "" {
		t.Fatalf("events printed session_id %q and next %q, want %s and a cursor", out.SessionID, out.Next, sid)
	}
	return out
}

func g1vJoin(evs []json.RawMessage) string {
	var b strings.Builder
	for _, e := range evs {
		b.Write(e)
		b.WriteByte('\n')
	}
	return b.String()
}

// g1vAssistantCount counts assistant events whose text holds s.
func g1vAssistantCount(evs []json.RawMessage, s string) int {
	n := 0
	for _, e := range evs {
		var h struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(e, &h) == nil && h.Type == "assistant" && strings.Contains(string(e), s) {
			n++
		}
	}
	return n
}

// Criterion 1: send returns once the message is written, while the turn runs.
func TestSessionSendReturnsBeforeTurnEnds(t *testing.T) {
	t.Parallel()
	i, sid := g1vLocal(t)
	text, opened := g1vHold(t, g1tDir(i, "acme-verbs"))

	g1vSend(t, i, sid, text)
	// send has returned; the turn is still blocked on the fifo.
	hold := g1vReading(t, opened)
	_ = hold.Close()
}

// Criterion 2: wait returns the reply excerpt when the current turn ends.
func TestSessionWaitReturnsExcerptAtTurnEnd(t *testing.T) {
	t.Parallel()
	i, sid := g1vLocal(t)
	text, opened := g1vHold(t, g1tDir(i, "acme-verbs"))
	g1vSend(t, i, sid, text)
	hold := g1vReading(t, opened)

	done := g1vAsync(i.StartCLI(harness.CLIOpts{}, "session", "wait", "--id", sid, "--json"))
	_ = hold.Close()
	res := <-done
	var w g1vWait
	res.JSON(t, &w)
	if res.Code != 0 || w.State != "idle" || w.SessionID != sid || !strings.Contains(w.Excerpt, "exit:") {
		t.Fatalf("wait exited %d with %+v, want 0, idle and the turn's reply as the excerpt", res.Code, w)
	}
}

// Criterion 3: wait returns at once when no turn runs.
func TestSessionWaitIdleReturnsAtOnce(t *testing.T) {
	t.Parallel()
	i, sid := g1vLocal(t)
	g1sSendText(t, i, sid, "hi", "echo: hi")

	res := i.CLIWith(harness.CLIOpts{Deadline: 20 * time.Second}, "session", "wait", "--id", sid, "--json")
	var w g1vWait
	res.JSON(t, &w)
	if res.Code != 0 || w.State != "idle" {
		t.Fatalf("wait on an idle session exited %d with state %q, want 0 and idle", res.Code, w.State)
	}
}

// Criterion 4: a turn waiting on a permission ends wait with state asking. The
// fake agent's `!ask <tool>` turn raises a can_use_tool control_request and
// waits for the answer; relay forwards it only for a host session.
func TestSessionWaitAsking(t *testing.T) {
	t.Parallel()
	r := g11dStart(t)
	sid := r.startSession(t, "")
	ws := r.i.WebSocket("/ws", r.i.Credential("mounter"))
	defer ws.Close()
	ws.Send(map[string]any{"type": "join_session", "sessionId": sid})
	g1vSend(t, r.i, sid, "!ask Write")
	b367Next(t, ws, sid, "session_state", "asking")

	res, w := g1vWaitFor(t, r.i, sid)
	if res.Code != 1 || w.State != "asking" {
		t.Fatalf("wait on a permission exited %d with state %q, want 1 and asking", res.Code, w.State)
	}
}

// Criterion 5: the agent dying mid-turn ends wait with its exit.
func TestSessionWaitAgentDied(t *testing.T) {
	t.Parallel()
	i, sid := g1vLocal(t)
	ws := i.WebSocket("/ws", i.Credential("mounter"))
	defer ws.Close()
	ws.Send(map[string]any{"type": "join_session", "sessionId": sid})
	g1vSend(t, i, sid, "!sh kill -9 $PPID")
	b367Next(t, ws, sid, "process_exited", "")

	res, w := g1vWaitFor(t, i, sid)
	if res.Code != 1 || w.State != "errored" || w.Exit == nil || w.Exit.Code == 0 || w.Exit.Stderr == nil {
		t.Fatalf("wait after the agent died exited %d with %+v, want 1, errored and an exit with a non-zero code and a stderr list", res.Code, w)
	}
}

// Criterion 6: the cap ends wait with state timeout and the turn goes on.
func TestSessionWaitTimeoutLeavesTurnRunning(t *testing.T) {
	t.Parallel()
	i, sid := g1vLocal(t)
	text, opened := g1vHold(t, g1tDir(i, "acme-verbs"))
	g1vSend(t, i, sid, text)
	hold := g1vReading(t, opened)

	res, w := g1vWaitFor(t, i, sid, "--timeout", "1s")
	if res.Code != 1 || w.State != "timeout" {
		t.Fatalf("wait --timeout on a held turn exited %d with state %q, want 1 and timeout", res.Code, w.State)
	}
	list, _ := g1sListSessions(t, i)
	if row, ok := g1sFindSession(list, sid); !ok || !row.Live {
		t.Fatalf("session %s is not live after the wait cap: %+v", sid, row)
	}
	_ = hold.Close()
	if res, w := g1vWaitFor(t, i, sid); res.Code != 0 || w.State != "idle" {
		t.Fatalf("wait after the release exited %d with state %q, want 0 and idle", res.Code, w.State)
	}
}

// Criterion 7: events --after C prints the events after C in order, with the
// next cursor.
func TestSessionEventsAfterCursor(t *testing.T) {
	t.Parallel()
	r := g11dStart(t)
	sid := r.startSession(t, "")
	g1sSendText(t, r.i, sid, "one", "echo: one")
	first := g1vEventsAfter(t, r.i, sid, "")
	if g1vAssistantCount(first.Events, "echo: one") != 1 {
		t.Fatalf("events from the start hold no reply to the first message: %s", g1vJoin(first.Events))
	}
	g1sSendText(t, r.i, sid, "two", "echo: two")
	g1sSendText(t, r.i, sid, "three", "echo: three")

	full := g1vEventsAfter(t, r.i, sid, "")
	tail := g1vEventsAfter(t, r.i, sid, first.Next)
	if g1vJoin(tail.Events) != g1vJoin(full.Events[len(first.Events):]) || tail.Next != full.Next {
		t.Fatalf("events --after %s printed %d events, want the %d after the cursor in order, next %s\n%s",
			first.Next, len(tail.Events), len(full.Events)-len(first.Events), full.Next, g1vJoin(tail.Events))
	}
	two := strings.Index(g1vJoin(tail.Events), "echo: two")
	three := strings.Index(g1vJoin(tail.Events), "echo: three")
	if two < 0 || three < two || strings.Contains(g1vJoin(tail.Events), "echo: one") {
		t.Fatalf("events after the cursor are out of order or repeat the first turn: %s", g1vJoin(tail.Events))
	}
}

// Criterion 8: calls chained by cursor print no event twice and miss none.
func TestSessionEventsChainedByCursor(t *testing.T) {
	t.Parallel()
	r := g11dStart(t)
	sid := r.startSession(t, "")
	g1sSendText(t, r.i, sid, "one", "echo: one")
	e1 := g1vEventsAfter(t, r.i, sid, "")
	g1sSendText(t, r.i, sid, "two", "echo: two")
	e2 := g1vEventsAfter(t, r.i, sid, e1.Next)
	g1sSendText(t, r.i, sid, "three", "echo: three")
	e3 := g1vEventsAfter(t, r.i, sid, e2.Next)

	full := g1vEventsAfter(t, r.i, sid, "")
	chained := g1vJoin(e1.Events) + g1vJoin(e2.Events) + g1vJoin(e3.Events)
	if chained != g1vJoin(full.Events) || e3.Next != full.Next {
		t.Fatalf("chained pages hold %d events, the whole stream %d: an event repeats or is missing", len(e1.Events)+len(e2.Events)+len(e3.Events), len(full.Events))
	}
}

// Criterion 9: a session with no runner has no replay.
func TestSessionEventsReplayUnavailable(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g1tCreds, Presence: g1tApprove, FakeModelHost: g1tEchoHost()})
	i.WaitModelHost(g1tHostDeadline)
	i.WaitSessionHost(g1tHostDeadline)
	p := g1tProject(t, i, "acme-chat-events", map[string]any{"allowed_templates": []string{"chat"}})
	sid := g1tStartSession(t, i, p.ID, "fake-echo", "")
	g1sSendText(t, i, sid, "hi", "echo: hi")

	res := i.CLI("session", "events", "--id", sid, "--json")
	if res.Code != 1 || g1vErrCode(res) != "replay_unavailable" {
		t.Fatalf("events on a chat session exited %d with code %q, want 1 and replay_unavailable\nstdout: %s\nstderr: %s",
			res.Code, g1vErrCode(res), res.Stdout, res.Stderr)
	}
}
