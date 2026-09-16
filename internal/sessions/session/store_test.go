package session_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/barelyworkingcode/relay/internal/sessions/session"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

func TestStore_SaveLoadDelete(t *testing.T) {
	store := session.NewStore(t.TempDir())
	sess := &sessionstypes.Session{
		ID:           "s1",
		ProjectID:    "proj-1",
		Name:         "test session",
		Directory:    "/tmp/proj",
		Model:        "sonnet",
		ProviderType: "claude",
		Messages:     []sessionstypes.Message{{Timestamp: "t0", Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	if err := store.Save(sess); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(filepath.Join(store.Dir(), "s1.json"))
	if err != nil {
		t.Fatalf("stat saved file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("saved file mode = %v, want 0600", info.Mode().Perm())
	}

	loaded, err := store.Load("s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Name != sess.Name || loaded.Directory != sess.Directory || len(loaded.Messages) != 1 {
		t.Fatalf("loaded session mismatch: %+v", loaded)
	}

	if err := store.Delete("s1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Load("s1"); err == nil {
		t.Fatal("Load after Delete: want an error, got nil")
	}
	// Deleting an id that no longer exists is not an error.
	if err := store.Delete("s1"); err != nil {
		t.Fatalf("Delete (already gone): %v", err)
	}
}

func TestStore_LoadAll(t *testing.T) {
	store := session.NewStore(t.TempDir())
	for _, id := range []string{"a", "b", "c"} {
		if err := store.Save(&sessionstypes.Session{ID: id}); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
	}
	// A non-.json file in the store dir must be ignored, not fail LoadAll.
	if err := os.WriteFile(filepath.Join(store.Dir(), "not-a-session.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write stray file: %v", err)
	}

	all, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("LoadAll returned %d sessions, want 3", len(all))
	}
}

// TestStore_NoCredentialField documents, as an executable check rather than
// only a comment, that a session round-tripped through Store never carries
// anything token/secret/bearer-shaped: relayLLM's old session model never
// persisted one either (verified by reading session_store.go and
// types/session.go directly), but this pins the contract against a future
// field addition that would reintroduce it.
func TestStore_NoCredentialField(t *testing.T) {
	store := session.NewStore(t.TempDir())
	sess := &sessionstypes.Session{
		ID:            "s1",
		ProviderState: json.RawMessage(`{"claudeSessionId":"abc"}`),
	}
	if err := store.Save(sess); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(store.Dir(), "s1.json"))
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatalf("unmarshal saved file: %v", err)
	}
	for _, forbidden := range []string{"token", "Token", "secret", "Secret", "bearer", "Bearer", "apiKey", "modelKey"} {
		if _, ok := generic[forbidden]; ok {
			t.Fatalf("persisted session unexpectedly carries a %q field", forbidden)
		}
	}
}
