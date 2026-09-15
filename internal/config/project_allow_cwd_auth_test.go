package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProject_AllowCwdAuthIsDroppedOnLoad pins the R-S2b requirement
// directly (plan-broker-and-sessions.md §2 C3, "allow_cwd_auth is
// removed"): a settings.json written before the field was retired still
// loads without error, the value is simply gone (the field no longer
// exists on config.Project, so json.Unmarshal drops the unrecognised key on
// its own — the same "never a hard error" property
// dropRetiredCapabilities gives a retired service capability), and it is
// absent from the file the next time anything writes it.
func TestProject_AllowCwdAuthIsDroppedOnLoad(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	path := filepath.Join(dir, "settings.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse settings.json: %v", err)
	}
	doc["projects"] = json.RawMessage(`[{
		"id": "legacy-cwd-project",
		"name": "legacy",
		"path": "/tmp/legacy-cwd-project",
		"allowed_mcp_ids": ["*"],
		"allowed_models": ["*"],
		"token": "legacy-plaintext-token",
		"token_hash": "` + HashToken("legacy-plaintext-token") + `",
		"allow_cwd_auth": true
	}]`)
	raw, err = json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal doc: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write legacy settings.json: %v", err)
	}

	s := store.Reload()
	proj, _ := FindProjectByID(s, "legacy-cwd-project")
	if proj == nil {
		t.Fatal("a project record holding the retired allow_cwd_auth field was not loaded at all")
	}
	if proj.Name != "legacy" || proj.Path != "/tmp/legacy-cwd-project" {
		t.Fatalf("unrelated fields did not survive the load: %+v", proj)
	}

	if err := store.With(func(*Settings) {}); err != nil {
		t.Fatalf("With: %v", err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(written), "allow_cwd_auth") {
		t.Fatal("allow_cwd_auth was written back to disk")
	}

	reloaded := store.Reload()
	if p, _ := FindProjectByID(reloaded, "legacy-cwd-project"); p == nil {
		t.Fatal("the project did not survive a reload after the field was dropped")
	}
}
