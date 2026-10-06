package attention_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/sessions/attention"
	"github.com/barelyworkingcode/relay/internal/sessions/testutil"
)

var allSignals = []attention.Signal{
	attention.Launching, attention.Launched, attention.LaunchFailed, attention.TurnStarted,
	attention.Activity, attention.Asked, attention.Answered, attention.TurnEnded,
	attention.TurnFailed, attention.TurnStopped, attention.ProcessExited,
	attention.StallTimeout, attention.SessionEnded,
}

// TestNext_Table pins every non-blank cell of the contract's transition
// table; every other (state, signal) pair must leave the state unchanged.
func TestNext_Table(t *testing.T) {
	type key struct {
		cur attention.State
		sig attention.Signal
	}
	want := map[key]attention.State{}
	set := func(sig attention.Signal, to attention.State, from ...attention.State) {
		for _, f := range from {
			want[key{f, sig}] = to
		}
	}
	every := []attention.State{attention.Starting, attention.Idle, attention.Running, attention.Asking, attention.Stalled, attention.Errored}
	set(attention.Launching, attention.Starting, every...)
	set(attention.SessionEnded, attention.Ended, every...)
	set(attention.Launched, attention.Idle, attention.Starting)
	set(attention.LaunchFailed, attention.Errored, attention.Starting)
	set(attention.TurnStarted, attention.Running, attention.Starting, attention.Idle, attention.Errored)
	set(attention.Activity, attention.Running, attention.Stalled)
	set(attention.Asked, attention.Asking, attention.Running, attention.Stalled)
	set(attention.Answered, attention.Running, attention.Asking)
	set(attention.StallTimeout, attention.Stalled, attention.Running)
	set(attention.TurnEnded, attention.Idle, attention.Running, attention.Asking, attention.Stalled)
	set(attention.TurnStopped, attention.Idle, attention.Running, attention.Asking, attention.Stalled)
	set(attention.TurnFailed, attention.Errored, attention.Running, attention.Asking, attention.Stalled)
	set(attention.ProcessExited, attention.Ended, attention.Starting, attention.Idle)
	set(attention.ProcessExited, attention.Errored, attention.Running, attention.Asking, attention.Stalled)

	for _, cur := range every {
		for _, sig := range allSignals {
			exp, ok := want[key{cur, sig}]
			if !ok {
				exp = cur
			}
			if got := attention.Next(cur, sig); got != exp {
				t.Errorf("Next(%q, signal %d) = %q, want %q", cur, sig, got, exp)
			}
		}
	}
}

type rec struct {
	mu     sync.Mutex
	events []string
	dones  []attention.TurnDone
	states []attention.Change
}

func (r *rec) StateChanged(c attention.Change) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, string(c.State))
	r.states = append(r.states, c)
}

func (r *rec) TurnDone(d attention.TurnDone) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, "turn_done")
	r.dones = append(r.dones, d)
}

func (r *rec) seq() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.events, ",")
}

var t0 = time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC)

func newBoard(t *testing.T) (*attention.Board, *rec, *testutil.FakeClock) {
	t.Helper()
	clk := testutil.NewFakeClock(t0)
	r := &rec{}
	return attention.NewBoard(clk, r), r, clk
}

func idle(b *attention.Board, id string) {
	b.Signal(id, attention.Launching)
	b.Signal(id, attention.Launched)
}

func TestBoard_TurnDoneOnceBeforeIdleFrame(t *testing.T) {
	b, r, _ := newBoard(t)
	idle(b, "p1")
	b.Signal("p1", attention.TurnStarted)
	b.Reply("p1", "done")
	b.Signal("p1", attention.TurnEnded)
	b.Signal("p1", attention.TurnEnded)   // late second end: nothing
	b.Signal("p1", attention.TurnStopped) // outside a turn: nothing

	if got, want := r.seq(), "starting,idle,running,turn_done,idle"; got != want {
		t.Fatalf("frames = %s, want %s", got, want)
	}
	if r.dones[0].Excerpt != "done" || r.dones[0].SessionID != "p1" {
		t.Fatalf("turn_done = %+v", r.dones[0])
	}
}

func TestBoard_ExcerptKeepsLast500RunesOnRuneBoundary(t *testing.T) {
	b, r, _ := newBoard(t)
	idle(b, "p1")
	b.Signal("p1", attention.TurnStarted)
	var all []rune
	for i := 0; i < 600; i++ {
		all = append(all, []rune{'a', 'é', '世'}[i%3])
	}
	for i := 0; i < 600; i += 7 { // chunks that straddle nothing but arrive piecemeal
		end := min(i+7, 600)
		b.Reply("p1", string(all[i:end]))
	}
	b.Signal("p1", attention.TurnEnded)

	got := r.dones[0].Excerpt
	if !utf8.ValidString(got) {
		t.Fatalf("excerpt is not valid UTF-8")
	}
	if want := string(all[100:]); got != want {
		t.Fatalf("excerpt = %d runes, want the last 500 (%d runes)", utf8.RuneCountInString(got), 500)
	}

	b.Signal("p1", attention.TurnStarted)
	b.Reply("p1", "short")
	b.Signal("p1", attention.TurnEnded)
	if got := r.dones[1].Excerpt; got != "short" {
		t.Fatalf("second turn excerpt = %q, want only that turn's text", got)
	}
}

func TestBoard_StallAt300sSinceIsLastEventPlus300s(t *testing.T) {
	b, r, clk := newBoard(t)
	idle(b, "p1")
	b.Signal("p1", attention.TurnStarted)
	clk.Advance(100 * time.Second)
	b.Signal("p1", attention.Activity)
	last := clk.Now()

	clk.Advance(299 * time.Second)
	b.Sweep()
	if a, _ := b.Get("p1"); a.State != attention.Running {
		t.Fatalf("state at 299s idle = %s, want running", a.State)
	}
	clk.Advance(51 * time.Second) // sweep notices late; since must not drift
	b.Sweep()
	a, _ := b.Get("p1")
	if a.State != attention.Stalled {
		t.Fatalf("state at 350s idle = %s, want stalled", a.State)
	}
	if want := attention.FormatTime(last.Add(attention.StallAfter)); a.Since != want {
		t.Fatalf("stalled since = %s, want %s", a.Since, want)
	}
	if got := r.states[len(r.states)-1].Since; !got.Equal(last.Add(300 * time.Second)) {
		t.Fatalf("frame since = %v", got)
	}

	b.Signal("p1", attention.Activity)
	if a, _ := b.Get("p1"); a.State != attention.Running {
		t.Fatalf("state after activity = %s, want running", a.State)
	}
}

func TestBoard_AskingNeverStalls(t *testing.T) {
	b, _, clk := newBoard(t)
	idle(b, "p1")
	b.Signal("p1", attention.TurnStarted)
	b.Signal("p1", attention.Asked)
	clk.Advance(3600 * time.Second)
	b.Sweep()
	if a, _ := b.Get("p1"); a.State != attention.Asking {
		t.Fatalf("state = %s, want asking", a.State)
	}
}

func TestBoard_SweepLoopStallsWithoutManualSweep(t *testing.T) {
	b, _, clk := newBoard(t)
	idle(b, "p1")
	b.Signal("p1", attention.TurnStarted)
	testutil.WaitFor(t, 2*time.Second, func() bool { return clk.Waiters() >= 1 })
	clk.Advance(attention.StallAfter)
	testutil.WaitFor(t, 2*time.Second, func() bool {
		a, _ := b.Get("p1")
		return a.State == attention.Stalled
	})
}

func TestBoard_UnknownIDIgnoredAndEndedDropsEntry(t *testing.T) {
	b, r, _ := newBoard(t)
	b.Signal("ghost", attention.TurnStarted)
	if _, ok := b.Get("ghost"); ok || r.seq() != "" {
		t.Fatalf("signal for unknown id created state: frames=%q", r.seq())
	}
	idle(b, "p1")
	b.Signal("p1", attention.SessionEnded)
	if _, ok := b.Get("p1"); ok {
		t.Fatal("ended session still has an entry")
	}
	if got, want := r.seq(), "starting,idle,ended"; got != want {
		t.Fatalf("frames = %s, want %s", got, want)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestBoard_OneLogLinePerChangeWithoutText(t *testing.T) {
	buf := &syncBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler(buf, logging.Options{
		DefaultService: "relaysessions", Getenv: func(string) string { return "" },
	})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	b, _, _ := newBoard(t)
	idle(b, "p1")
	b.Signal("p1", attention.TurnStarted)
	b.Reply("p1", "classified-reply-text")
	b.Signal("p1", attention.TurnEnded)
	b.Signal("p1", attention.TurnEnded) // no change, no line

	type want struct{ from, to string }
	wants := []want{{"", "starting"}, {"starting", "idle"}, {"idle", "running"}, {"running", "idle"}}
	var lines []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.Contains(raw, "classified-reply-text") {
			t.Fatalf("log line carries reply text: %s", raw)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("not JSON: %s", raw)
		}
		lines = append(lines, m)
	}
	if len(lines) != len(wants) {
		t.Fatalf("got %d log lines, want %d:\n%s", len(lines), len(wants), buf.String())
	}
	for i, w := range wants {
		m := lines[i]
		if m["op"] != "session.state" || m["session_id"] != "p1" || m["from"] != w.from || m["to"] != w.to {
			t.Errorf("line %d = %v, want op=session.state session_id=p1 from=%q to=%q", i, m, w.from, w.to)
		}
	}
}
