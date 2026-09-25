package api

import (
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/barelyworkingcode/relay/internal/sessions/session"
	"github.com/barelyworkingcode/relay/internal/sessions/testutil"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

func TestHasViewers_TracksJoinLeaveAndDisconnect(t *testing.T) {
	hub, mgr, sh := newTestSessionSetup(t)
	mgr.SetProviderFactory(func(*sessionstypes.Session, session.CreateSpec, sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		return &wsFakeProvider{}, nil
	})
	sess, err := mgr.Create(session.CreateSpec{SessionID: wsTestSessionID, ProjectID: "proj-1", Kind: session.KindClaude})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	const otherSession = "22222222-2222-2222-2222-222222222222"

	if sh.HasViewers(sess.ID) {
		t.Fatal("HasViewers = true before any join")
	}

	join := func() *websocket.Conn {
		t.Helper()
		conn := dialHub(t, hub)
		if err := conn.WriteJSON(map[string]any{"type": "join_session", "sessionId": sess.ID}); err != nil {
			t.Fatalf("write join: %v", err)
		}
		readJSONWithTimeout(t, conn, 2*time.Second) // session_joined
		if !sh.HasViewers(sess.ID) {
			t.Fatal("HasViewers = false after a join")
		}
		if sh.HasViewers(otherSession) {
			t.Fatal("HasViewers = true for a session nobody joined")
		}
		return conn
	}

	if err := join().WriteJSON(map[string]any{"type": "leave_session", "sessionId": sess.ID}); err != nil {
		t.Fatalf("write leave: %v", err)
	}
	testutil.WaitFor(t, 2*time.Second, func() bool { return !sh.HasViewers(sess.ID) })

	_ = join().Close()
	testutil.WaitFor(t, 2*time.Second, func() bool { return !sh.HasViewers(sess.ID) })
}
