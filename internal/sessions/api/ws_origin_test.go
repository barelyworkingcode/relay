package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/barelyworkingcode/relay/internal/sessions/session"
	"github.com/barelyworkingcode/relay/internal/sessions/testutil"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const originSessionID = "66666666-6666-6666-6666-666666666666"

// readFrameOfType reads frames until one has the given type.
func readFrameOfType(t *testing.T, conn *websocket.Conn, typ string) map[string]any {
	t.Helper()
	for i := 0; i < 20; i++ {
		if m := readJSONWithTimeout(t, conn, 2*time.Second); m["type"] == typ {
			return m
		}
	}
	t.Fatalf("no %q frame within 20 frames", typ)
	return nil
}

// newOriginSession builds a hub, a manager with a scriptable fake provider
// and one project session.
func newOriginSession(t *testing.T) (*Hub, *session.Manager, *sessionstypes.Session) {
	t.Helper()
	hub, mgr, _ := newTestSessionSetup(t)
	mgr.SetProviderFactory(func(_ *sessionstypes.Session, _ session.CreateSpec, h sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		fp := testutil.NewFakeProvider(h)
		fp.ScriptText("noted")
		fp.ScriptResult("end_turn", sessionstypes.SessionStats{})
		return fp, nil
	})
	t.Cleanup(mgr.StopAll)
	sess, err := mgr.Create(session.CreateSpec{SessionID: originSessionID, ProjectID: "p1", Kind: session.KindClaude})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return hub, mgr, sess
}

func joinSession(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	if err := conn.WriteJSON(map[string]any{"type": "join_session", "sessionId": originSessionID}); err != nil {
		t.Fatalf("join: %v", err)
	}
	return readFrameOfType(t, conn, "session_joined")
}

func TestWSOrigin_ChiefOfStaffSendMarksLiveFrameAndJoinHistory(t *testing.T) {
	hub, mgr, sess := newOriginSession(t)
	viewer := dialHub(t, hub)
	joinSession(t, viewer)

	if err := mgr.SendMessageAs(originSessionID, "restart the job", nil, sessionstypes.OriginChiefOfStaff); err != nil {
		t.Fatalf("SendMessageAs: %v", err)
	}
	frame := readFrameOfType(t, viewer, "user_message")
	if frame["origin"] != sessionstypes.OriginChiefOfStaff || frame["text"] != "restart the job" || frame["sessionId"] != originSessionID {
		t.Fatalf("live frame = %v, want the text marked origin chief-of-staff", frame)
	}

	// The person's own message, sent through the ordinary door, is unmarked.
	testutil.WaitFor(t, 2*time.Second, func() bool { return !sess.IsProcessing() })
	if err := viewer.WriteJSON(map[string]any{"type": "send_message", "sessionId": originSessionID, "text": "thanks"}); err != nil {
		t.Fatal(err)
	}
	frame = readFrameOfType(t, viewer, "user_message")
	if _, has := frame["origin"]; has {
		t.Fatalf("a person's live frame carries origin: %v", frame)
	}

	late := dialHub(t, hub)
	joined := joinSession(t, late)
	history, _ := joined["history"].([]any)
	var users []map[string]any
	for _, h := range history {
		if m, _ := h.(map[string]any); m["role"] == "user" {
			users = append(users, m)
		}
	}
	if len(users) != 2 {
		t.Fatalf("join history has %d user messages, want 2: %v", len(users), history)
	}
	if users[0]["origin"] != sessionstypes.OriginChiefOfStaff {
		t.Errorf("first history message origin = %v, want chief-of-staff", users[0]["origin"])
	}
	if _, has := users[1]["origin"]; has {
		t.Errorf("person's history message carries origin: %v", users[1])
	}
}

// No inbound decoder takes an origin, so a claim in a person-facing door's
// body must leave the frame and the stored message unmarked.
func TestWSOrigin_BodyClaimIsIgnoredOnPersonDoors(t *testing.T) {
	for _, tc := range []struct {
		name string
		send func(t *testing.T, mgr *session.Manager, viewer *websocket.Conn)
	}{
		{"websocket send_message", func(t *testing.T, _ *session.Manager, viewer *websocket.Conn) {
			err := viewer.WriteJSON(map[string]any{
				"type": "send_message", "sessionId": originSessionID, "text": "do it", "origin": sessionstypes.OriginChiefOfStaff,
			})
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"sync http message", func(t *testing.T, mgr *session.Manager, _ *websocket.Conn) {
			body, _ := json.Marshal(map[string]any{"text": "do it", "origin": sessionstypes.OriginChiefOfStaff})
			req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+originSessionID+"/message", bytes.NewReader(body))
			w := httptest.NewRecorder()
			HandleSessionMessageSync(mgr, originSessionID, w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub, mgr, sess := newOriginSession(t)
			viewer := dialHub(t, hub)
			joinSession(t, viewer)

			tc.send(t, mgr, viewer)

			frame := readFrameOfType(t, viewer, "user_message")
			if _, has := frame["origin"]; has {
				t.Fatalf("frame carries a claimed origin: %v", frame)
			}
			// Let the scripted turn finish so its persistence does not outlive the test.
			testutil.WaitFor(t, 2*time.Second, func() bool { return !sess.IsProcessing() })
			sess.Lock()
			defer sess.Unlock()
			if len(sess.Messages) == 0 || sess.Messages[0].Role != "user" || sess.Messages[0].Origin != "" {
				t.Fatalf("stored messages = %+v, want a user message with no origin", sess.Messages)
			}
		})
	}
}

// A Claude transcript has no origin field; join must align relay's own
// record onto it.
func TestWSOrigin_ClaudeTranscriptJoinHistoryCarriesOrigin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const claudeID = "claude-fixture-1"
	projectDir := filepath.Join(home, ".claude", "projects", strings.ReplaceAll(dir, "/", "-"))
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonl := strings.Join([]string{
		`{"type":"user","sessionId":"` + claudeID + `","timestamp":"2026-01-01T00:00:01Z","message":{"role":"user","content":"deploy now"}}`,
		`{"type":"assistant","sessionId":"` + claudeID + `","timestamp":"2026-01-01T00:00:02Z","message":{"id":"m1","content":[{"type":"text","text":"deployed"}]}}`,
		`{"type":"user","sessionId":"` + claudeID + `","timestamp":"2026-01-01T00:00:03Z","message":{"role":"user","content":"thanks"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(projectDir, claudeID+".jsonl"), []byte(jsonl), 0o644); err != nil {
		t.Fatal(err)
	}

	userMsg := func(text, origin string) sessionstypes.Message {
		c, _ := json.Marshal(text)
		return sessionstypes.Message{Timestamp: "t", Role: "user", Content: c, Origin: origin}
	}
	store := session.NewStore(t.TempDir())
	if err := store.Save(&sessionstypes.Session{
		ID: originSessionID, ProjectID: "p1", Directory: dir, ProviderType: session.KindClaude,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
		Messages:      []sessionstypes.Message{userMsg("deploy now", sessionstypes.OriginChiefOfStaff), userMsg("thanks", "")},
		ProviderState: json.RawMessage(`{"claudeSessionId":"` + claudeID + `"}`),
	}); err != nil {
		t.Fatal(err)
	}
	hub := NewHub()
	mgr := session.NewManager(session.Config{}, store, nil)
	mgr.SetEventSink(NewSessionHandlers(hub, mgr, nil))

	joined := joinSession(t, dialHub(t, hub))
	history, _ := joined["history"].([]any)
	if len(history) != 3 {
		t.Fatalf("history has %d entries, want the 3 transcript entries: %v", len(history), history)
	}
	first, _ := history[0].(map[string]any)
	last, _ := history[2].(map[string]any)
	if first["origin"] != sessionstypes.OriginChiefOfStaff {
		t.Errorf("first transcript message origin = %v, want chief-of-staff", first["origin"])
	}
	if _, has := last["origin"]; has {
		t.Errorf("person's transcript message carries origin: %v", last)
	}
}
