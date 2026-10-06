package hostapi_test

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/attention"
	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
	"github.com/barelyworkingcode/relay/internal/sessions/session"
	"github.com/barelyworkingcode/relay/internal/sessions/testutil"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const (
	agentID     = "66666666-0000-0000-0000-000000000001"
	conversatID = "7d3f1c52-9a4e-4b6a-8c21-0e5f6a7b8c9d"
)

type dropInFixture struct {
	sessions *session.Manager
	client   *http.Client
	bearer   string
	provider *testutil.FakeProvider
}

// newDropInFixture serves a real host with one idle headless Claude agent
// (id agentID) that already has a conversation id.
func newDropInFixture(t *testing.T, settings map[string]any) *dropInFixture {
	t.Helper()
	terminals, sessions := buildManagers(t)
	f := &dropInFixture{sessions: sessions}
	sessions.SetProviderFactory(func(_ *sessionstypes.Session, _ session.CreateSpec, handler sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		f.provider = testutil.NewFakeProvider(handler)
		return f.provider, nil
	})
	_, internalSock, _, bearer := startServerWithManagers(t, os.Getpid(), terminals, sessions)
	f.client, f.bearer = unixClient(internalSock), bearer

	resp := postJSON(t, f.client, "http://h/launch", bearer, map[string]any{
		"v": 1, "session_id": agentID, "kind": "claude",
		"session_request": map[string]any{"projectId": "p1", "directory": "/tmp/p1", "model": "sonnet", "settings": settings},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("launch status = %d, want 201", resp.StatusCode)
	}
	f.provider.RestoreState(json.RawMessage(`{"claudeSessionId":"` + conversatID + `"}`))
	return f
}

func (f *dropInFixture) post(t *testing.T, path string, body any) (int, map[string]string) {
	t.Helper()
	resp := postJSON(t, f.client, "http://h"+path, f.bearer, body)
	defer resp.Body.Close()
	var out map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestHandoff_ResponseShapes(t *testing.T) {
	headless := map[string]any{"headless": true, "agent": true}
	f := newDropInFixture(t, headless)

	if code, _ := f.post(t, "/handoff", map[string]any{"session_id": "no-such-session"}); code != http.StatusNotFound {
		t.Errorf("unknown session: status = %d, want 404", code)
	}
	code, out := f.post(t, "/handoff", map[string]any{"session_id": agentID})
	if code != http.StatusOK || out["session_id"] != agentID || out["claude_session_id"] != conversatID {
		t.Fatalf("handoff = %d %v, want 200 with session_id and claude_session_id", code, out)
	}
	if f.provider.Alive() {
		t.Error("headless process still alive after handoff")
	}
	code, out = f.post(t, "/handoff", map[string]any{"session_id": agentID})
	if code != http.StatusConflict || out["error"] != "dropped_in" || out["message"] == "" {
		t.Fatalf("second handoff = %d %v, want 409 dropped_in with a message", code, out)
	}
}

func TestHandoff_NotHeadlessIs409WithCode(t *testing.T) {
	f := newDropInFixture(t, map[string]any{})
	code, out := f.post(t, "/handoff", map[string]any{"session_id": agentID})
	if code != http.StatusConflict || out["error"] != "not_headless" || out["message"] == "" {
		t.Fatalf("handoff = %d %v, want 409 not_headless", code, out)
	}
}

func TestHandback_IsAlways204AndIdempotent(t *testing.T) {
	f := newDropInFixture(t, map[string]any{"headless": true, "agent": true})
	f.post(t, "/handoff", map[string]any{"session_id": agentID})
	for _, id := range []string{agentID, agentID, "no-such-session"} {
		if code, _ := f.post(t, "/handback", map[string]any{"session_id": id}); code != http.StatusNoContent {
			t.Fatalf("handback %s status = %d, want 204", id, code)
		}
	}
	if f.sessions.Held(agentID) {
		t.Fatal("session still held after handback")
	}
}

// A drop-in terminal may only launch for a held session no other terminal
// has, and its exit hands the session back as idle.
func TestLaunch_DropInFor_RequiresHoldAndExitHandsBack(t *testing.T) {
	_, target := buildBinaries(t)
	f := newDropInFixture(t, map[string]any{"headless": true, "agent": true})
	launch := func(termID string) (int, map[string]string) {
		body := launchBody(termID, []string{target})
		body["drop_in_for"] = agentID
		return f.post(t, "/launch", body)
	}

	if code, out := launch("term-1"); code != http.StatusConflict || out["error"] != hostapi.ErrNotHeld {
		t.Fatalf("launch before handoff = %d %v, want 409 not_held", code, out)
	}
	f.post(t, "/handoff", map[string]any{"session_id": agentID})
	if code, out := launch("term-1"); code != http.StatusCreated {
		t.Fatalf("launch after handoff = %d %v, want 201", code, out)
	}
	if code, out := launch("term-2"); code != http.StatusConflict || out["error"] != hostapi.ErrNotHeld {
		t.Fatalf("second terminal for the same session = %d %v, want 409 not_held", code, out)
	}

	f.post(t, "/terminate", map[string]any{"session_id": "term-1", "reason": "revoked"})
	testutil.WaitFor(t, 5*time.Second, func() bool { return !f.sessions.Held(agentID) })
	for _, s := range f.sessions.List() {
		if s.ID == agentID && (s.Attention == nil || s.Attention.State != attention.Idle) {
			t.Fatalf("attention after terminal exit = %+v, want idle", s.Attention)
		}
	}
}
