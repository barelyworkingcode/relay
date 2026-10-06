package session_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/attention"
	"github.com/barelyworkingcode/relay/internal/sessions/session"
	"github.com/barelyworkingcode/relay/internal/sessions/testutil"
)

const (
	dropID       = "55555555-0000-0000-0000-000000000001"
	dropConvID   = "7d3f1c52-9a4e-4b6a-8c21-0e5f6a7b8c9d"
	headlessAgnt = `{"headless":true,"agent":true}`
	toolOpenBash = `{"type":"assistant","content_block_stop":true,"content_block":{"type":"tool_use","id":"t1","name":"Bash"}}`
	toolResultT1 = `{"type":"result","subtype":"tool_result","tool_use_id":"t1"}`
)

type handoffOutcome struct {
	claudeID string
	err      error
}

// dropInSession creates a headless Claude agent that has a conversation id,
// with a turn in flight when inTurn is set.
func dropInSession(t *testing.T, inTurn bool) (*attnHarness, *fakeProvider) {
	t.Helper()
	h := newAttnHarness(t, nil)
	h.create(t, dropID, session.KindClaude, headlessAgnt)
	sess, _ := h.mgr.Get(dropID)
	p := sess.Provider().(*fakeProvider)
	p.RestoreState([]byte(`{"claudeSessionId":"` + dropConvID + `"}`))
	if inTurn {
		h.send(t, dropID)
	}
	return h, p
}

func (h *attnHarness) handoff(ctx context.Context, beforeStop func()) <-chan handoffOutcome {
	out := make(chan handoffOutcome, 1)
	go func() {
		id, err := h.mgr.Handoff(ctx, dropID, 60*time.Second, beforeStop)
		out <- handoffOutcome{id, err}
	}()
	return out
}

func awaitOutcome(t *testing.T, out <-chan handoffOutcome) handoffOutcome {
	t.Helper()
	select {
	case o := <-out:
		return o
	case <-time.After(5 * time.Second):
		t.Fatal("Handoff did not return")
		return handoffOutcome{}
	}
}

func wantRefusal(t *testing.T, err error, code string) *session.HandoffError {
	t.Helper()
	var he *session.HandoffError
	if !errors.As(err, &he) || he.Code != code {
		t.Fatalf("err = %v, want refusal %q", err, code)
	}
	return he
}

func (h *attnHarness) waitForHandoffWait(t *testing.T) {
	t.Helper()
	testutil.WaitFor(t, 2*time.Second, func() bool { return h.clk.Waiters() >= 1 })
}

func TestHandoff_IdleSessionIsStoppedOnceHeldAndSlotKept(t *testing.T) {
	h, p := dropInSession(t, false)
	before := 0
	id, err := h.mgr.Handoff(context.Background(), dropID, 60*time.Second, func() { before++ })
	if err != nil || id != dropConvID {
		t.Fatalf("Handoff = %q, %v; want %s", id, err, dropConvID)
	}
	if before != 1 || p.Kills() != 1 || !h.mgr.Held(dropID) {
		t.Fatalf("beforeStop=%d kills=%d held=%v; want 1, 1, true", before, p.Kills(), h.mgr.Held(dropID))
	}
	if _, ok := h.mgr.Get(dropID); !ok {
		t.Fatal("slot dropped by Handoff; the exit must still reach the host")
	}
	// A late exit report from the killed process must not move the board.
	h.emit(dropID, "process_exited", "")
	if row, _ := h.row(dropID); row.Attention == nil || row.Attention.State != attention.Running {
		t.Fatalf("attention while held = %+v, want running", row.Attention)
	}
}

func TestHandoff_WaitsForMessageCompleteThenHandsOff(t *testing.T) {
	h, p := dropInSession(t, true)
	out := h.handoff(context.Background(), nil)
	h.waitForHandoffWait(t)
	h.emit(dropID, "message_complete", "")

	if o := awaitOutcome(t, out); o.err != nil || o.claudeID != dropConvID {
		t.Fatalf("Handoff = %q, %v", o.claudeID, o.err)
	}
	if p.Kills() != 1 {
		t.Fatalf("kills = %d, want 1", p.Kills())
	}
	// The turn finished (turn_done, idle) before the hold began.
	if got, want := h.rec.seq(), "starting,idle,running,turn_done,idle,running"; got != want {
		t.Fatalf("frames = %s, want %s", got, want)
	}
}

func TestHandoff_TurnTimeoutAfterWaitHoldsNothing(t *testing.T) {
	h, p := dropInSession(t, true)
	out := h.handoff(context.Background(), nil)
	h.waitForHandoffWait(t)
	h.clk.Advance(60 * time.Second)

	wantRefusal(t, awaitOutcome(t, out).err, "turn_timeout")
	if h.mgr.Held(dropID) || p.Kills() != 0 {
		t.Fatalf("held=%v kills=%d after timeout; want nothing stopped", h.mgr.Held(dropID), p.Kills())
	}
}

func TestHandoff_CancelledContextHoldsNothing(t *testing.T) {
	h, p := dropInSession(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	out := h.handoff(ctx, nil)
	h.waitForHandoffWait(t)
	cancel()

	if err := awaitOutcome(t, out).err; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if h.mgr.Held(dropID) || p.Kills() != 0 {
		t.Fatalf("held=%v kills=%d after cancel; want nothing stopped", h.mgr.Held(dropID), p.Kills())
	}
}

func TestHandoff_OpenToolRefusesNamingToolAtRequestAndDuringWait(t *testing.T) {
	t.Run("open at request", func(t *testing.T) {
		h, p := dropInSession(t, true)
		h.emit(dropID, "llm_event", toolOpenBash)
		he := wantRefusal(t, awaitOutcome(t, h.handoff(context.Background(), nil)).err, "tool_running")
		if !strings.Contains(he.Message, "Bash") {
			t.Fatalf("message %q does not name the tool", he.Message)
		}
		if h.mgr.Held(dropID) || p.Kills() != 0 {
			t.Fatalf("held=%v kills=%d; want nothing stopped", h.mgr.Held(dropID), p.Kills())
		}
	})
	t.Run("opens during wait", func(t *testing.T) {
		h, _ := dropInSession(t, true)
		out := h.handoff(context.Background(), nil)
		h.waitForHandoffWait(t)
		h.emit(dropID, "llm_event", toolOpenBash)
		wantRefusal(t, awaitOutcome(t, out).err, "tool_running")
		if h.mgr.Held(dropID) {
			t.Fatal("session held after tool_running")
		}
	})
}

func TestHandoff_ToolResultReleasesTheToolRefusal(t *testing.T) {
	h, _ := dropInSession(t, true)
	h.emit(dropID, "llm_event", toolOpenBash)
	h.emit(dropID, "llm_event", toolResultT1)
	out := h.handoff(context.Background(), nil)
	h.waitForHandoffWait(t)
	h.emit(dropID, "message_complete", "")
	if o := awaitOutcome(t, out); o.err != nil {
		t.Fatalf("Handoff after tool_result = %v, want success", o.err)
	}
}

func TestHandoff_HeldSessionRefusesSendAndResumeUntilHandBack(t *testing.T) {
	h, _ := dropInSession(t, false)
	if _, err := h.mgr.Handoff(context.Background(), dropID, 60*time.Second, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.mgr.SendMessage(dropID, "hi", nil); !errors.Is(err, session.ErrDroppedIn) {
		t.Fatalf("SendMessage while held = %v, want ErrDroppedIn", err)
	}
	_, err := h.mgr.Create(session.CreateSpec{SessionID: dropID, ProjectID: "11111111-0000-0000-0000-000000000001", Kind: session.KindClaude, Settings: []byte(headlessAgnt), Resume: true})
	if !errors.Is(err, session.ErrDroppedIn) {
		t.Fatalf("Create(Resume) while held = %v, want ErrDroppedIn", err)
	}
	wantRefusal(t, func() error { _, e := h.mgr.Handoff(context.Background(), dropID, time.Second, nil); return e }(), "dropped_in")

	h.mgr.HandBack(dropID)
	frames := h.rec.seq()
	if got, want := frames, "starting,idle,running,idle"; got != want {
		t.Fatalf("frames after HandBack = %s, want %s", got, want)
	}
	h.mgr.HandBack(dropID)
	h.mgr.HandBack("no-such-session")
	if h.mgr.Held(dropID) || h.rec.seq() != frames {
		t.Fatalf("repeat HandBack changed state: held=%v frames=%s", h.mgr.Held(dropID), h.rec.seq())
	}
	if err := h.mgr.SendMessage(dropID, "hi", nil); errors.Is(err, session.ErrDroppedIn) {
		t.Fatalf("SendMessage after HandBack = %v", err)
	}
}

func TestHandoff_Refusals(t *testing.T) {
	t.Run("not_claude", func(t *testing.T) {
		h := newAttnHarness(t, nil)
		h.create(t, dropID, session.KindPi, headlessAgnt)
		_, err := h.mgr.Handoff(context.Background(), dropID, time.Second, nil)
		he := wantRefusal(t, err, "not_claude")
		if !strings.Contains(he.Message, "pi") {
			t.Errorf("message %q does not name the kind", he.Message)
		}
	})
	t.Run("not_headless", func(t *testing.T) {
		h := newAttnHarness(t, nil)
		h.create(t, dropID, session.KindClaude, "")
		_, err := h.mgr.Handoff(context.Background(), dropID, time.Second, nil)
		wantRefusal(t, err, "not_headless")
	})
	t.Run("no_conversation leaves the session usable", func(t *testing.T) {
		h := newAttnHarness(t, nil)
		h.create(t, dropID, session.KindClaude, headlessAgnt)
		_, err := h.mgr.Handoff(context.Background(), dropID, time.Second, nil)
		wantRefusal(t, err, "no_conversation")
		if h.mgr.Held(dropID) {
			t.Fatal("held after no_conversation")
		}
		if err := h.mgr.SendMessage(dropID, "hi", nil); err != nil {
			t.Fatalf("SendMessage after refusal = %v; the claim must be released", err)
		}
	})
	t.Run("unknown session", func(t *testing.T) {
		h := newAttnHarness(t, nil)
		if _, err := h.mgr.Handoff(context.Background(), "no-such-session", time.Second, nil); !errors.Is(err, session.ErrSessionNotFound) {
			t.Fatalf("err = %v, want ErrSessionNotFound", err)
		}
	})
}
