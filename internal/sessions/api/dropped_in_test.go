package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/session"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// conversationProvider reports a Claude conversation id so Handoff can hold
// the session.
type conversationProvider struct{ *wsFakeProvider }

func (conversationProvider) GetState() json.RawMessage {
	return json.RawMessage(`{"claudeSessionId":"7d3f1c52-9a4e-4b6a-8c21-0e5f6a7b8c9d"}`)
}

func heldSession(t *testing.T, mgr *session.Manager) *sessionstypes.Session {
	t.Helper()
	mgr.SetProviderFactory(func(*sessionstypes.Session, session.CreateSpec, sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		return conversationProvider{&wsFakeProvider{}}, nil
	})
	sess, err := mgr.Create(session.CreateSpec{SessionID: wsTestSessionID, ProjectID: "proj-1", Kind: session.KindClaude, Settings: json.RawMessage(`{"headless":true}`)})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := mgr.Handoff(context.Background(), sess.ID, time.Second, nil); err != nil {
		t.Fatalf("Handoff: %v", err)
	}
	return sess
}

func TestHTTPMessage_WhileHeldIs409DroppedIn(t *testing.T) {
	mgr := newHTTPTestManager(t)
	sess := heldSession(t, mgr)

	body, _ := json.Marshal(map[string]any{"text": "hi"})
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+sess.ID+"/message", bytes.NewReader(body))
	w := httptest.NewRecorder()
	HandleSessionMessageSync(mgr, sess.ID, w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got["error"] != "dropped_in" {
		t.Fatalf("body = %s (%v), want error dropped_in", w.Body.String(), err)
	}
}

func TestWSSendMessage_WhileHeldSendsDroppedInErrorFrame(t *testing.T) {
	hub, mgr, _ := newTestSessionSetup(t)
	sess := heldSession(t, mgr)

	conn := dialHub(t, hub)
	if err := conn.WriteJSON(map[string]any{"type": "send_message", "sessionId": sess.ID, "text": "hello"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := readJSONWithTimeout(t, conn, 2*time.Second)
	if got["type"] != "error" || got["code"] != "dropped_in" || got["sessionId"] != sess.ID {
		t.Fatalf("frame = %+v, want error frame code dropped_in for %s", got, sess.ID)
	}
}
