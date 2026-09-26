package api

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/permission"
	"github.com/barelyworkingcode/relay/internal/sessions/session"
	"github.com/barelyworkingcode/relay/internal/sessions/testutil"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const wsTestSessionID = "11111111-1111-1111-1111-111111111111"

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
func (p *wsFakeProvider) StopGeneration()              {}
func (p *wsFakeProvider) Kill()                        { p.mu.Lock(); p.alive = false; p.mu.Unlock() }
func (p *wsFakeProvider) DeleteSession() error         { return nil }
func (p *wsFakeProvider) Alive() bool                  { p.mu.Lock(); defer p.mu.Unlock(); return p.alive }
func (p *wsFakeProvider) GetState() json.RawMessage    { return nil }
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
	sess, err := mgr.Create(session.CreateSpec{SessionID: wsTestSessionID, ProjectID: "proj-1", Kind: session.KindClaude})
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
	sess, err := mgr.Create(session.CreateSpec{SessionID: wsTestSessionID, ProjectID: "proj-1", Kind: session.KindClaude})
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

func TestSendMessage_LazyLoadedAdHoc_SendsResumeRequiredWithGuidance(t *testing.T) {
	hub := NewHub()
	mgr := newLazyAdHocManager(t)
	mgr.SetEventSink(NewSessionHandlers(hub, mgr, nil))

	conn := dialHub(t, hub)
	if err := conn.WriteJSON(map[string]any{"type": "send_message", "sessionId": httpTestSessionID, "text": "hello"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := readJSONWithTimeout(t, conn, 2*time.Second)
	if got["type"] != "error" || got["code"] != "resume_required" {
		t.Fatalf("frame = %+v, want an error frame with code resume_required", got)
	}
	assertResumeGuidance(t, got["message"])
}

func TestSendMessage_AdHocDeadProvider_RespawnsAndDelivers(t *testing.T) {
	hub, mgr, _ := newTestSessionSetup(t)
	var built int32
	mgr.SetProviderFactory(func(*sessionstypes.Session, session.CreateSpec, sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		atomic.AddInt32(&built, 1)
		return &wsFakeProvider{}, nil
	})
	sess, err := mgr.Create(session.CreateSpec{SessionID: wsTestSessionID, ProjectID: "", Kind: session.KindClaude})
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
	sess, err := mgr.Create(session.CreateSpec{SessionID: wsTestSessionID, ProjectID: "proj-1", Kind: session.KindClaude})
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

// TestJoinSession_ConcurrentRenameSession_NoRace asserts handleJoinSession's
// snapshot of ProviderState/Name/Folder/Directory/Model/Headless is race-free
// against RenameSession's concurrent write to Name. Run with -race; a
// join_session loop racing a concurrent RenameSession must come back clean.
func TestJoinSession_ConcurrentRenameSession_NoRace(t *testing.T) {
	hub, mgr, _ := newTestSessionSetup(t)
	mgr.SetProviderFactory(func(*sessionstypes.Session, session.CreateSpec, sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		return &wsFakeProvider{}, nil
	})
	sess, err := mgr.Create(session.CreateSpec{SessionID: wsTestSessionID, ProjectID: "proj-1", Kind: session.KindClaude})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = mgr.RenameSession(sess.ID, fmt.Sprintf("name-%d", i))
		}
	}()

	conn := dialHub(t, hub)
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if err := conn.WriteJSON(map[string]any{"type": "join_session", "sessionId": sess.ID}); err != nil {
			t.Fatalf("write join: %v", err)
		}
		readJSONWithTimeout(t, conn, 2*time.Second)
	}
	close(stop)
	wg.Wait()
}

// TestPermissionResponse_UnjoinedConnection_Refused pins the join-scoping
// check handlePermissionResponse now runs before Resolve: the first
// tool-approval authority in the system must not be resolvable by a
// connection that never joined the session the pending request belongs to.
// Relay's frontend socket is a single trust domain (any frontend-capable
// caller reaches every session's WS traffic), so without this check any
// connection that merely guesses or observes a permissionId could allow or
// deny a tool call for a session it was never shown.
func TestPermissionResponse_UnjoinedConnection_Refused(t *testing.T) {
	hub := NewHub()
	store := session.NewStore(t.TempDir())
	mgr := session.NewManager(session.Config{}, store, nil)
	perms := permission.NewPermissionManager()
	sh := NewSessionHandlers(hub, mgr, perms)
	mgr.SetEventSink(sh)

	req, ch := perms.CreateRequest(wsTestSessionID, "Bash", `{"command":"ls"}`, "tu-1")

	conn := dialHub(t, hub)
	// Deliberately no join_session for wsTestSessionID.
	if err := conn.WriteJSON(map[string]any{
		"type": "permission_response", "permissionId": req.ID, "approved": true,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := readJSONWithTimeout(t, conn, 2*time.Second)
	if got["type"] != "error" {
		t.Fatalf("type = %v, want error (an unjoined connection must be refused)", got["type"])
	}

	select {
	case d := <-ch:
		t.Fatalf("decision must not have been delivered to an unresolved request, got %+v", d)
	case <-time.After(200 * time.Millisecond):
	}
	if perms.PendingCount() != 1 {
		t.Fatalf("PendingCount = %d, want 1 (the request must remain pending after a refused resolve attempt)", perms.PendingCount())
	}
}

// TestPermissionResponse_JoinedConnection_Resolves is the positive half: a
// connection that DID join_session first is exactly what this scoping check
// must still allow through, unchanged from before this fix.
func TestPermissionResponse_JoinedConnection_Resolves(t *testing.T) {
	hub := NewHub()
	store := session.NewStore(t.TempDir())
	mgr := session.NewManager(session.Config{}, store, nil)
	perms := permission.NewPermissionManager()
	sh := NewSessionHandlers(hub, mgr, perms)
	mgr.SetEventSink(sh)

	mgr.SetProviderFactory(func(*sessionstypes.Session, session.CreateSpec, sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		return &wsFakeProvider{}, nil
	})
	sess, err := mgr.Create(session.CreateSpec{SessionID: wsTestSessionID, ProjectID: "proj-1", Kind: session.KindClaude})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	req, ch := perms.CreateRequest(sess.ID, "Bash", `{"command":"ls"}`, "tu-1")

	conn := dialHub(t, hub)
	if err := conn.WriteJSON(map[string]any{"type": "join_session", "sessionId": sess.ID}); err != nil {
		t.Fatalf("write join: %v", err)
	}
	readJSONWithTimeout(t, conn, 2*time.Second) // session_joined

	if err := conn.WriteJSON(map[string]any{
		"type": "permission_response", "permissionId": req.ID, "approved": true, "reason": "ok",
	}); err != nil {
		t.Fatalf("write permission_response: %v", err)
	}

	select {
	case d := <-ch:
		if d.Decision != "allow" {
			t.Fatalf("decision = %+v, want allow", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the decision to be delivered")
	}
}
