package session_test

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/provider"
	"github.com/barelyworkingcode/relay/internal/sessions/session"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

type exitSink struct {
	mu     sync.Mutex
	exited []string
}

func (s *exitSink) SendToSession(id string, msg map[string]any) {
	if msg["type"] != "process_exited" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exited = append(s.exited, id)
}

func (s *exitSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.exited)
}

// When the restart's Start fails, the session is dead: the killed spawn's
// exit must still reach the WS and the exit handler, which relay-sessions
// reports to relay as SessionExited and relay audits as session_end.
func TestManager_PermissionModeRestartStartFails_ReportsExit(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	mgr, _ := newTestManager(t)
	var claude *provider.ClaudeProvider
	mgr.SetProviderFactory(func(sess *sessionstypes.Session, _ session.CreateSpec, handler sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		claude = provider.NewClaudeProvider(sess, handler, provider.ClaudeConfig{Binary: binary}, nil)
		return claude, nil
	})
	sink := &exitSink{}
	mgr.SetEventSink(sink)
	exited := make(chan string, 2)
	mgr.SetExitHandler(func(id string, _ int) { exited <- id })

	sess, err := mgr.Create(session.CreateSpec{
		SessionID: "44444444-4444-4444-4444-444444444444",
		ProjectID: "proj-1",
		Kind:      session.KindClaude,
		Directory: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(claude.Kill)

	if err := os.Remove(binary); err != nil {
		t.Fatal(err)
	}
	if err := claude.SetPermissionMode("plan"); err == nil {
		t.Fatal("SetPermissionMode succeeded with the claude binary gone")
	}
	if claude.Alive() {
		t.Fatal("provider alive after a failed restart")
	}

	select {
	case id := <-exited:
		if id != sess.ID {
			t.Fatalf("exit handler fired for %q, want %q", id, sess.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a failed permission-mode restart never fired the exit handler: relay records no session_end")
	}

	select {
	case id := <-exited:
		t.Fatalf("exit handler fired a second time for %q", id)
	case <-time.After(200 * time.Millisecond):
	}
	if n := sink.count(); n != 1 {
		t.Fatalf("WS got %d process_exited frames, want 1", n)
	}
}
