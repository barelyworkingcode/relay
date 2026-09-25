package session_test

import (
	"encoding/json"
	"testing"

	"github.com/barelyworkingcode/relay/internal/sessions/events"
	"github.com/barelyworkingcode/relay/internal/sessions/session"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// A killed claude/pi provider reports its exit on its own goroutine, after
// Kill has returned, so the event can land after DeleteSession has already
// removed the file. Emitting through the captured handler once DeleteSession
// returns pins that ordering deterministically.
func TestManager_DeleteSession_LateProviderEvent_DoesNotResurrect(t *testing.T) {
	exitData, _ := json.Marshal(map[string]any{"exitCode": 0})
	cases := []struct {
		name      string
		eventType string
		data      json.RawMessage
	}{
		{"process_exited", "process_exited", exitData},
		{"message_complete", events.HandlerMessageComplete, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr, store := newTestManager(t)
			var handler sessionstypes.EventHandler
			mgr.SetProviderFactory(func(_ *sessionstypes.Session, _ session.CreateSpec, h sessionstypes.EventHandler) (sessionstypes.Provider, error) {
				handler = h
				return &fakeProvider{}, nil
			})

			const id = "55555555-5555-5555-5555-555555555555"
			if _, err := mgr.Create(session.CreateSpec{SessionID: id, ProjectID: "proj-1", Kind: session.KindClaude}); err != nil {
				t.Fatalf("Create: %v", err)
			}

			mgr.DeleteSession(id)
			handler(tc.eventType, tc.data)

			if _, err := store.Load(id); err == nil {
				t.Fatalf("store still holds %s after DeleteSession and a late %s", id, tc.eventType)
			}
			for _, s := range mgr.List() {
				if s.ID == id {
					t.Fatalf("List still returns %s after DeleteSession and a late %s", id, tc.eventType)
				}
			}
		})
	}
}
