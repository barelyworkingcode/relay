package api

import (
	"encoding/json"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/barelyworkingcode/relay/internal/sessions/session"
	"github.com/barelyworkingcode/relay/internal/sessions/testutil"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const attnSessionID = "55555555-5555-5555-5555-555555555555"

// readAttentionFrames collects session_state and turn_done frames until it
// has n of them, skipping every other frame type.
func readAttentionFrames(t *testing.T, conn *websocket.Conn, n int) []map[string]any {
	t.Helper()
	var out []map[string]any
	for len(out) < n {
		m := readJSONWithTimeout(t, conn, 2*time.Second)
		if m["type"] == "session_state" || m["type"] == "turn_done" {
			out = append(out, m)
		}
	}
	return out
}

func keysOf(m map[string]any) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Both connections get every frame, including the one that never joined the
// session, in the order turn_done then idle at a turn's end.
func TestWSAttention_BroadcastFramesReachJoinedAndUnjoinedConnections(t *testing.T) {
	hub, mgr, sh := newTestSessionSetup(t)
	mgr.SetAttentionSink(sh)
	t.Cleanup(mgr.StopAll)
	var emit sessionstypes.EventHandler
	mgr.SetProviderFactory(func(_ *sessionstypes.Session, _ session.CreateSpec, h sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		emit = h
		return &wsFakeProvider{}, nil
	})

	joined, unjoined := dialHub(t, hub), dialHub(t, hub)
	testutil.WaitFor(t, 2*time.Second, func() bool { return len(hub.Conns()) == 2 })

	if _, err := mgr.Create(session.CreateSpec{SessionID: attnSessionID, ProjectID: "proj-1", Kind: session.KindClaude}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := joined.WriteJSON(map[string]any{"type": "join_session", "sessionId": attnSessionID}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SendMessage(attnSessionID, "go", nil); err != nil {
		t.Fatal(err)
	}
	emit("llm_event", json.RawMessage(`{"type":"assistant","delta":{"type":"text_delta","text":"all done"}}`))
	emit("message_complete", nil)

	// Both connections were registered before Create, so each saw starting
	// and idle first; compare from running on.
	wantTypes := []string{"session_state", "turn_done", "session_state"}
	for name, conn := range map[string]*websocket.Conn{"unjoined": unjoined, "joined": joined} {
		frames := readAttentionFrames(t, conn, 5)
		frames = frames[2:]
		for i, f := range frames {
			if f["type"] != wantTypes[i] || f["sessionId"] != attnSessionID {
				t.Fatalf("%s frame %d = %v", name, i, f)
			}
		}
		running, done, idle := frames[0], frames[1], frames[2]
		if running["state"] != "running" || idle["state"] != "idle" {
			t.Fatalf("%s states = %v, %v", name, running["state"], idle["state"])
		}
		if got := keysOf(running); !slices.Equal(got, []string{"sessionId", "since", "state", "type"}) {
			t.Fatalf("%s session_state keys = %v", name, got)
		}
		if got := keysOf(done); !slices.Equal(got, []string{"at", "excerpt", "sessionId", "type"}) {
			t.Fatalf("%s turn_done keys = %v", name, got)
		}
		if done["excerpt"] != "all done" {
			t.Fatalf("%s excerpt = %v", name, done["excerpt"])
		}
		for _, ts := range []any{running["since"], idle["since"], done["at"]} {
			s, _ := ts.(string)
			if _, err := time.Parse("2006-01-02T15:04:05.000Z", s); err != nil || len(s) != 24 {
				t.Fatalf("%s time %v is not RFC 3339 UTC with milliseconds", name, ts)
			}
		}
	}
}
