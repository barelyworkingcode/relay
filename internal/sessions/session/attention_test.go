package session_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/sessions/attention"
	"github.com/barelyworkingcode/relay/internal/sessions/permission"
	"github.com/barelyworkingcode/relay/internal/sessions/session"
	"github.com/barelyworkingcode/relay/internal/sessions/testutil"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

type attnRec struct {
	mu     sync.Mutex
	events []string // state names and "turn_done", in arrival order
	states []attention.Change
}

func (r *attnRec) StateChanged(c attention.Change) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, string(c.State))
	r.states = append(r.states, c)
}

func (r *attnRec) TurnDone(attention.TurnDone) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, "turn_done")
}

func (r *attnRec) seq() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.events, ",")
}

func (r *attnRec) count(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e == name {
			n++
		}
	}
	return n
}

type attnHarness struct {
	mgr      *session.Manager
	rec      *attnRec
	clk      *testutil.FakeClock
	mu       sync.Mutex
	handlers map[string]sessionstypes.EventHandler
}

func newAttnHarness(t *testing.T, perms *permission.PermissionManager) *attnHarness {
	t.Helper()
	h := &attnHarness{
		rec:      &attnRec{},
		clk:      testutil.NewFakeClock(time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC)),
		handlers: map[string]sessionstypes.EventHandler{},
	}
	h.mgr = session.NewManager(session.Config{Clock: h.clk}, session.NewStore(t.TempDir()), perms)
	h.mgr.SetAttentionSink(h.rec)
	h.mgr.SetProviderFactory(func(s *sessionstypes.Session, _ session.CreateSpec, handler sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		h.mu.Lock()
		h.handlers[s.ID] = handler
		h.mu.Unlock()
		return &fakeProvider{}, nil
	})
	t.Cleanup(h.mgr.StopAll)
	return h
}

func (h *attnHarness) emit(id, event string, data string) {
	h.mu.Lock()
	fn := h.handlers[id]
	h.mu.Unlock()
	var raw json.RawMessage
	if data != "" {
		raw = json.RawMessage(data)
	}
	fn(event, raw)
}

func (h *attnHarness) create(t *testing.T, id, kind, settings string) {
	t.Helper()
	spec := session.CreateSpec{SessionID: id, ProjectID: "11111111-0000-0000-0000-000000000001", Kind: kind}
	if settings != "" {
		spec.Settings = json.RawMessage(settings)
	}
	if _, err := h.mgr.Create(spec); err != nil {
		t.Fatalf("Create %s: %v", id, err)
	}
}

func (h *attnHarness) send(t *testing.T, id string) {
	t.Helper()
	if err := h.mgr.SendMessage(id, "go", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
}

func (h *attnHarness) row(id string) (session.Summary, bool) {
	for _, s := range h.mgr.List() {
		if s.ID == id {
			return s, true
		}
	}
	return session.Summary{}, false
}

const textDelta = `{"type":"assistant","delta":{"type":"text_delta","text":"hi"}}`

// Claude and pi take one path: the same script gives the same states, and a
// failed turn (isError, or pi's error event) is errored.
func TestAttention_ClaudeAndPiShareStateSequence(t *testing.T) {
	const want = "starting,idle,running,turn_done,idle,running,turn_done,errored,running,turn_done,errored"
	for _, kind := range []string{session.KindClaude, session.KindPi, session.KindCodex} {
		t.Run(kind, func(t *testing.T) {
			h := newAttnHarness(t, nil)
			h.create(t, "11111111-0000-0000-0000-000000000001", kind, "")
			h.send(t, "11111111-0000-0000-0000-000000000001")
			h.emit("11111111-0000-0000-0000-000000000001", "llm_event", textDelta)
			h.emit("11111111-0000-0000-0000-000000000001", "stats_update", `{}`)
			h.emit("11111111-0000-0000-0000-000000000001", "message_complete", "")
			h.send(t, "11111111-0000-0000-0000-000000000001")
			h.emit("11111111-0000-0000-0000-000000000001", "message_complete", `{"isError":true}`)
			h.send(t, "11111111-0000-0000-0000-000000000001")
			h.emit("11111111-0000-0000-0000-000000000001", "error", `"model failed"`)

			if got := h.rec.seq(); got != want {
				t.Fatalf("sequence = %s\nwant       %s", got, want)
			}
		})
	}
}

func TestAttention_PermissionRequestAsksThenResolveResumes(t *testing.T) {
	perms := permission.NewPermissionManager()
	h := newAttnHarness(t, perms)
	h.create(t, "11111111-0000-0000-0000-000000000001", session.KindClaude, "")
	h.send(t, "11111111-0000-0000-0000-000000000001")

	req, _ := perms.CreateRequest("11111111-0000-0000-0000-000000000001", "tool", `{}`, "u1")
	perms.Resolve(req.ID, permission.PermissionDecision{Decision: "allow"})

	if got, want := h.rec.seq(), "starting,idle,running,asking,running"; got != want {
		t.Fatalf("sequence = %s, want %s", got, want)
	}
}

func TestAttention_HarnessQuestionAsksThenToolResultResumes(t *testing.T) {
	h := newAttnHarness(t, nil)
	h.create(t, "11111111-0000-0000-0000-000000000001", session.KindClaude, "")
	h.send(t, "11111111-0000-0000-0000-000000000001")
	h.emit("11111111-0000-0000-0000-000000000001", "llm_event", `{"type":"system","subtype":"question"}`)
	h.emit("11111111-0000-0000-0000-000000000001", "llm_event", `{"type":"result","subtype":"tool_result"}`)

	if got, want := h.rec.seq(), "starting,idle,running,asking,running"; got != want {
		t.Fatalf("sequence = %s, want %s", got, want)
	}
}

func TestAttention_StopGenerationIdlesWithOneTurnDone(t *testing.T) {
	h := newAttnHarness(t, nil)
	h.create(t, "11111111-0000-0000-0000-000000000001", session.KindClaude, "")
	h.send(t, "11111111-0000-0000-0000-000000000001")
	if err := h.mgr.StopGeneration("11111111-0000-0000-0000-000000000001"); err != nil {
		t.Fatal(err)
	}
	h.emit("11111111-0000-0000-0000-000000000001", "message_complete", "") // the provider's own late completion

	if got, want := h.rec.seq(), "starting,idle,running,turn_done,idle"; got != want {
		t.Fatalf("sequence = %s, want %s", got, want)
	}
}

func TestAttention_StallThroughSweepLoopAfter300s(t *testing.T) {
	h := newAttnHarness(t, nil)
	h.create(t, "11111111-0000-0000-0000-000000000001", session.KindClaude, "")
	h.send(t, "11111111-0000-0000-0000-000000000001")
	started := h.clk.Now()
	testutil.WaitFor(t, 2*time.Second, func() bool { return h.clk.Waiters() >= 1 })
	h.clk.Advance(attention.StallAfter)
	testutil.WaitFor(t, 2*time.Second, func() bool { return h.rec.count("stalled") == 1 })

	row, _ := h.row("11111111-0000-0000-0000-000000000001")
	if row.Attention == nil || row.Attention.State != attention.Stalled ||
		row.Attention.Since != attention.FormatTime(started.Add(attention.StallAfter)) {
		t.Fatalf("attention = %+v, want stalled since start+300s", row.Attention)
	}
}

func TestAttention_ListShowsStateForLiveTrackedOnly(t *testing.T) {
	h := newAttnHarness(t, nil)
	h.create(t, "11111111-0000-0000-0000-000000000001", session.KindClaude, "")
	h.create(t, "11111111-0000-0000-0000-000000000002", session.KindChat, "")

	row, ok := h.row("11111111-0000-0000-0000-000000000001")
	if !ok || row.Attention == nil || row.Attention.State != attention.Idle ||
		row.Attention.Since != attention.FormatTime(h.clk.Now()) {
		t.Fatalf("live claude row = %+v ok=%v, want attention idle since now", row.Attention, ok)
	}
	if row, ok := h.row("11111111-0000-0000-0000-000000000002"); !ok || row.Attention != nil {
		t.Fatalf("chat row = %+v ok=%v, want listed with no attention", row.Attention, ok)
	}

	h.mgr.EndSession("11111111-0000-0000-0000-000000000001")
	if row, ok := h.row("11111111-0000-0000-0000-000000000001"); ok && row.Attention != nil {
		t.Fatalf("ended row carries attention %+v", row.Attention)
	}
}

func TestAttention_HeadlessAgentListedHeadlessRoutineNot(t *testing.T) {
	h := newAttnHarness(t, nil)
	h.create(t, "22222222-0000-0000-0000-000000000001", session.KindClaude, `{"headless":true,"agent":true}`)
	h.create(t, "22222222-0000-0000-0000-000000000002", session.KindPi, `{"headless":true,"agent":true}`)
	h.create(t, "33333333-0000-0000-0000-000000000001", session.KindClaude, `{"headless":true}`)
	h.create(t, "33333333-0000-0000-0000-000000000002", session.KindChat, `{"headless":true}`)

	for _, id := range []string{"33333333-0000-0000-0000-000000000001", "33333333-0000-0000-0000-000000000002"} {
		if _, ok := h.row(id); ok {
			t.Errorf("headless %s without agent is listed", id)
		}
	}
	for _, id := range []string{"22222222-0000-0000-0000-000000000001", "22222222-0000-0000-0000-000000000002"} {
		row, ok := h.row(id)
		if !ok || row.Attention == nil || row.Attention.State != attention.Idle {
			t.Errorf("headless agent %s: listed=%v attention=%+v, want listed and idle", id, ok, row.Attention)
		}
	}

	// Ended agent stays listed from disk (the flag persists) with no state.
	h.mgr.EndSession("22222222-0000-0000-0000-000000000001")
	if row, ok := h.row("22222222-0000-0000-0000-000000000001"); !ok || row.Attention != nil {
		t.Errorf("ended agent: listed=%v attention=%+v, want listed with no attention", ok, row.Attention)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestAttention_UntrackedSessionsGetNoFramesOrLogLines(t *testing.T) {
	buf := &syncBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler(buf, logging.Options{
		DefaultService: "relaysessions", Getenv: func(string) string { return "" },
	})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := newAttnHarness(t, nil)
	h.create(t, "44444444-0000-0000-0000-000000000001", session.KindClaude, "")
	untracked := map[string]string{"33333333-0000-0000-0000-000000000001": session.KindClaude, "33333333-0000-0000-0000-000000000002": session.KindChat}
	for id, kind := range untracked {
		h.create(t, id, kind, `{"headless":true}`)
		h.send(t, id)
		h.emit(id, "llm_event", textDelta)
		h.emit(id, "message_complete", "")
		h.mgr.EndSession(id)
	}

	frames := h.rec.states
	for _, c := range frames {
		if c.SessionID != "44444444-0000-0000-0000-000000000001" {
			t.Errorf("frame for untracked session %s", c.SessionID)
		}
	}
	if len(frames) == 0 {
		t.Fatal("tracked session produced no frames; the check proves nothing")
	}
	logged := false
	for _, line := range strings.Split(buf.String(), "\n") {
		if !strings.Contains(line, `"session.state"`) {
			continue
		}
		logged = logged || strings.Contains(line, `"44444444-0000-0000-0000-000000000001"`)
		for id := range untracked {
			if strings.Contains(line, fmt.Sprintf("%q", id)) {
				t.Errorf("state log line for untracked %s: %s", id, line)
			}
		}
	}
	if !logged {
		t.Fatal("no session.state line for the tracked session; the check proves nothing")
	}
}
