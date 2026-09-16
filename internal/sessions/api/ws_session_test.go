package api

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/session"
	"github.com/barelyworkingcode/relay/internal/sessions/testutil"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// wsFakeProvider is this file's own minimal sessionstypes.Provider double —
// duplicated rather than shared with internal/sessions/session's own
// unexported test fake (package session_test), since this file lives in
// package api and needs no more than Alive()/Kill() control.
type wsFakeProvider struct {
	mu    sync.Mutex
	alive bool
	sent  []string
}

func (p *wsFakeProvider) Start() error { p.mu.Lock(); p.alive = true; p.mu.Unlock(); return nil }
func (p *wsFakeProvider) SendMessage(text string, _ []sessionstypes.FileAttachment) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, text)
	return nil
}
func (p *wsFakeProvider) StopGeneration()      {}
func (p *wsFakeProvider) Kill()                { p.mu.Lock(); p.alive = false; p.mu.Unlock() }
func (p *wsFakeProvider) DeleteSession() error { return nil }
func (p *wsFakeProvider) Alive() bool          { p.mu.Lock(); defer p.mu.Unlock(); return p.alive }
func (p *wsFakeProvider) GetState() json.RawMessage { return nil }
func (p *wsFakeProvider) RestoreState(json.RawMessage) {}

func newTestSessionSetup(t *testing.T) (*Hub, *session.Manager, *SessionHandlers) {
	t.Helper()
	hub := NewHub()
	store := session.NewStore(t.TempDir())
	mgr := session.NewManager(session.Config{}, store, nil)
	sh := NewSessionHandlers(hub, mgr, nil)
	mgr.SetEventSink(sh)
	return hub, mgr, sh
}

func TestJoinSession_UnknownID_SendsError(t *testing.T) {
	hub, _, _ := newTestSessionSetup(t)
	conn := dialHub(t, hub)

	if err := conn.WriteJSON(map[string]any{"type": "join_session", "sessionId": "no-such-id"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := readJSONWithTimeout(t, conn, 2*time.Second)
	if got["type"] != "error" {
		t.Fatalf("type = %v, want error", got["type"])
	}
}

func TestJoinSession_LiveFieldReflectsProviderState(t *testing.T) {
	hub, mgr, _ := newTestSessionSetup(t)
	fp := &wsFakeProvider{}
	mgr.SetProviderFactory(func(*sessionstypes.Session, session.CreateSpec, sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		return fp, nil
	})
	sess, err := mgr.Create(session.CreateSpec{SessionID: "s1", ProjectID: "proj-1", Kind: session.KindClaude})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	conn := dialHub(t, hub)
	if err := conn.WriteJSON(map[string]any{"type": "join_session", "sessionId": sess.ID}); err != nil {
		t.Fatalf("write join: %v", err)
	}
	joined := readJSONWithTimeout(t, conn, 2*time.Second)
	if joined["type"] != "session_joined" {
		t.Fatalf("type = %v, want session_joined (%+v)", joined["type"], joined)
	}
	if live, ok := joined["live"].(bool); !ok || !live {
		t.Fatalf("live = %v, want true", joined["live"])
	}

	fp.Kill()

	conn2 := dialHub(t, hub)
	if err := conn2.WriteJSON(map[string]any{"type": "join_session", "sessionId": sess.ID}); err != nil {
		t.Fatalf("write join 2: %v", err)
	}
	joined2 := readJSONWithTimeout(t, conn2, 2*time.Second)
	if live, ok := joined2["live"].(bool); !ok || live {
		t.Fatalf("live after kill = %v, want false", joined2["live"])
	}
}

// TestSendMessage_ProjectSessionDeadProvider_SendsResumeRequiredFrame covers
// SH-6's headline wire behavior: a project-bound session whose provider is
// not running gets an {"type":"error","code":"resume_required",...} frame,
// not a silent respawn and not a generic error string.
func TestSendMessage_ProjectSessionDeadProvider_SendsResumeRequiredFrame(t *testing.T) {
	hub, mgr, _ := newTestSessionSetup(t)
	fp := &wsFakeProvider{}
	mgr.SetProviderFactory(func(*sessionstypes.Session, session.CreateSpec, sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		return fp, nil
	})
	sess, err := mgr.Create(session.CreateSpec{SessionID: "s1", ProjectID: "proj-1", Kind: session.KindClaude})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fp.Kill()

	conn := dialHub(t, hub)
	if err := conn.WriteJSON(map[string]any{"type": "send_message", "sessionId": sess.ID, "text": "hello"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := readJSONWithTimeout(t, conn, 2*time.Second)
	if got["type"] != "error" {
		t.Fatalf("type = %v, want error", got["type"])
	}
	if got["code"] != "resume_required" {
		t.Fatalf("code = %v, want resume_required (%+v)", got["code"], got)
	}
	if got["sessionId"] != sess.ID {
		t.Fatalf("sessionId = %v, want %v", got["sessionId"], sess.ID)
	}
	if len(fp.sent) != 0 {
		t.Fatalf("dead provider unexpectedly received a message: %v", fp.sent)
	}
}

func TestSendMessage_AdHocDeadProvider_RespawnsAndDelivers(t *testing.T) {
	hub, mgr, _ := newTestSessionSetup(t)
	var built int32
	mgr.SetProviderFactory(func(*sessionstypes.Session, session.CreateSpec, sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		atomic.AddInt32(&built, 1)
		return &wsFakeProvider{}, nil
	})
	sess, err := mgr.Create(session.CreateSpec{SessionID: "s1", ProjectID: "", Kind: session.KindClaude})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sess.Provider().(*wsFakeProvider).Kill()

	conn := dialHub(t, hub)
	if err := conn.WriteJSON(map[string]any{"type": "send_message", "sessionId": sess.ID, "text": "hello"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	testutil.WaitFor(t, 2*time.Second, func() bool { return atomic.LoadInt32(&built) == 2 })
}

func TestDeleteSession_BroadcastsSessionEnded(t *testing.T) {
	hub, mgr, _ := newTestSessionSetup(t)
	mgr.SetProviderFactory(func(*sessionstypes.Session, session.CreateSpec, sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		return &wsFakeProvider{}, nil
	})
	sess, err := mgr.Create(session.CreateSpec{SessionID: "s1", ProjectID: "proj-1", Kind: session.KindClaude})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	conn := dialHub(t, hub)
	if err := conn.WriteJSON(map[string]any{"type": "delete_session", "sessionId": sess.ID}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := readJSONWithTimeout(t, conn, 2*time.Second)
	if got["type"] != "session_ended" {
		t.Fatalf("type = %v, want session_ended (%+v)", got["type"], got)
	}
	if _, ok := mgr.Get(sess.ID); ok {
		t.Fatal("delete_session must remove the session")
	}
}
