package session_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/sessions/session"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const testSessionID = "11111111-1111-1111-1111-111111111111"

func TestStore_SaveLoadDelete(t *testing.T) {
	store := session.NewStore(t.TempDir())
	sess := &sessionstypes.Session{
		ID:           testSessionID,
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

	info, err := os.Stat(filepath.Join(store.Dir(), testSessionID+".json"))
	if err != nil {
		t.Fatalf("stat saved file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("saved file mode = %v, want 0600", info.Mode().Perm())
	}

	loaded, err := store.Load(testSessionID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Name != sess.Name || loaded.Directory != sess.Directory || len(loaded.Messages) != 1 {
		t.Fatalf("loaded session mismatch: %+v", loaded)
	}

	if err := store.Delete(testSessionID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Load(testSessionID); err == nil {
		t.Fatal("Load after Delete: want an error, got nil")
	}
	// Deleting an id that no longer exists is not an error.
	if err := store.Delete(testSessionID); err != nil {
		t.Fatalf("Delete (already gone): %v", err)
	}
}

func TestStore_LoadAll(t *testing.T) {
	store := session.NewStore(t.TempDir())
	ids := []string{
		"11111111-1111-1111-1111-111111111111",
		"22222222-2222-2222-2222-222222222222",
		"33333333-3333-3333-3333-333333333333",
	}
	for _, id := range ids {
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

// TestStore_NeverCarriesCredentialShape is R2's replacement for the old
// top-level-key-name check, which would pass unchanged even if a credential
// rode out inside Settings, ProviderState, or Host.SSHArgv — each an
// opaque, verbatim-persisted carrier (internal/sessions/migrate copies
// relayLLM's session directory in the same verbatim way). It plants a
// distinct canary in each of those three fields, saves through the real
// Store.Save path, then byte-scans the saved file: the canaries must be
// present (proving the test actually exercises these fields, not a vacuous
// file) and nothing credential-shaped — the "rmk_" model-key prefix, or a
// 64-lowercase-hex secret — may appear anywhere in it. Matches
// internal/sessions/ledger's TestLedger_FileNeverCarriesModelKeyOrSecretShape
// methodology.
func TestStore_NeverCarriesCredentialShape(t *testing.T) {
	const settingsCanary = "CANARY-SETTINGS-4b7e91"
	const providerStateCanary = "CANARY-PROVIDERSTATE-2af06c"
	const sshArgvCanary = "CANARY-SSHARGV-e10d33"

	store := session.NewStore(t.TempDir())
	settings, _ := json.Marshal(map[string]any{
		"permissionMode": "default",
		"permissionPolicy": map[string]any{
			"allowedTools": []string{"Bash:git *", "Read"},
			"deniedTools":  []string{"Bash:rm *"},
			"defaultMode":  "default",
		},
		"marker": settingsCanary,
	})
	sess := &sessionstypes.Session{
		ID:            testSessionID,
		ProjectID:     "proj-1",
		Directory:     "/Users/alice/code/widget",
		Model:         "sonnet",
		ProviderType:  "claude",
		Settings:      settings,
		ProviderState: json.RawMessage(`{"claudeSessionId":"` + providerStateCanary + `"}`),
		Host: &sessionstypes.HostSpec{
			ID:      "host-1",
			Name:    "build box",
			SSHArgv: []string{"-p", "22", sshArgvCanary + "@example.com"},
		},
	}
	if err := store.Save(sess); err != nil {
		t.Fatalf("Save: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(store.Dir(), testSessionID+".json"))
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}

	for _, canary := range []string{settingsCanary, providerStateCanary, sshArgvCanary} {
		if !bytes.Contains(data, []byte(canary)) {
			t.Fatalf("saved file missing canary %q — test does not exercise this field", canary)
		}
	}
	if regexp.MustCompile(`rmk_`).Match(data) {
		t.Fatal("saved session file contains the model-key prefix rmk_")
	}
	if m := regexp.MustCompile(`[0-9a-f]{64}`).Find(data); m != nil {
		t.Fatalf("saved session file contains a 64-lowercase-hex substring shaped like a launch secret: %q", m)
	}
}

// TestStore_ModelKeyNeverPersisted confirms the actual credential this
// package handles — C8's per-launch pi ModelKey — never rides along into a
// session file, carried through the real path a caller supplies it by
// (Manager.Create's CreateSpec) rather than constructed directly on a
// Session. CreateSpec.ModelKey only ever reaches buildProvider's PiConfig;
// sessionstypes.Session has no field for it at all.
func TestStore_ModelKeyNeverPersisted(t *testing.T) {
	modelKeyCanary := "rmk_" + strings.Repeat("a", 64)

	mgr, store := newTestManager(t)
	mgr.SetProviderFactory(factoryReturning(&fakeProvider{}))

	sess, err := mgr.Create(session.CreateSpec{
		SessionID: testSessionID,
		ProjectID: "proj-1",
		Kind:      session.KindPi,
		ModelKey:  modelKeyCanary,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Save(sess); err != nil {
		t.Fatalf("Save: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(store.Dir(), testSessionID+".json"))
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if bytes.Contains(data, []byte(modelKeyCanary)) {
		t.Fatal("saved session file contains the ModelKey supplied at Create time")
	}
	if regexp.MustCompile(`rmk_`).Match(data) {
		t.Fatal("saved session file contains the model-key prefix rmk_")
	}
}

// TestStore_Save_RejectsTraversalID is R1: a session id must never reach
// filepath.Join unvalidated, or a crafted id can write outside the store
// directory.
func TestStore_Save_RejectsTraversalID(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, "store"))
	if err := store.Save(&sessionstypes.Session{ID: "../escaped"}); err == nil {
		t.Fatal("Save(../escaped): want an error, got nil")
	}
	if _, err := os.Stat(filepath.Join(dir, "escaped.json")); !os.IsNotExist(err) {
		t.Fatal("a path-traversing id escaped the store directory")
	}
}

// TestStore_Save_RejectsNonUUIDShapedID is R1's second required rejection
// case: an id that isn't a path traversal but also isn't UUID-shaped.
func TestStore_Save_RejectsNonUUIDShapedID(t *testing.T) {
	store := session.NewStore(t.TempDir())
	for _, id := range []string{"", "not-a-uuid", "sess-dup"} {
		if err := store.Save(&sessionstypes.Session{ID: id}); err == nil {
			t.Errorf("Save(%q): want an error, got nil", id)
		}
	}
	entries, err := os.ReadDir(store.Dir())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("Save wrote files despite refusing every id: %v", entries)
	}
}

// TestStore_Load_RejectsBadID plants real files at exactly the paths a
// crafted id would resolve to — one outside the store directory (the
// traversal target), one inside it with a non-UUID-shaped name — so a Load
// that skipped validation would succeed and return someone else's session
// instead of merely hitting a "file not found" that proves nothing about
// the validation itself.
func TestStore_Load_RejectsBadID(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, "store"))
	if err := os.MkdirAll(store.Dir(), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "escaped.json"), []byte(`{"sessionId":"escaped"}`), 0o600); err != nil {
		t.Fatalf("write outside-store file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir(), "not-a-uuid.json"), []byte(`{"sessionId":"not-a-uuid"}`), 0o600); err != nil {
		t.Fatalf("write malformed-name file: %v", err)
	}

	cases := []string{
		"",
		"../escaped",
		"not-a-uuid",
		"11111111-2222-3333-4444-55555555555",  // 35 chars
		"11111111x2222-3333-4444-555555555555", // wrong separator
		"11111111-2222-3333-4444-5555555555gg", // non-hex
	}
	for _, id := range cases {
		if _, err := store.Load(id); err == nil {
			t.Errorf("Load(%q): want an error, got nil", id)
		}
	}
}
