package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newTestSettings returns a *Settings with the given MCPs.
func newTestSettings(t *testing.T, mcps []ExternalMcp) *Settings {
	t.Helper()
	if mcps == nil {
		mcps = []ExternalMcp{}
	}
	return &Settings{
		Version:      1,
		ExternalMcps: mcps,
		Services:     []ServiceConfig{},
	}
}

// ---------------------------------------------------------------------------
// AddExternalMcp / RemoveExternalMcp
// ---------------------------------------------------------------------------

func TestAddExternalMcp(t *testing.T) {
	t.Run("adds MCP to slice", func(t *testing.T) {
		s := newTestSettings(t, nil)

		mcp := ExternalMcp{ID: "mcp1", DisplayName: "Test MCP"}
		s.AddExternalMcp(mcp)

		if len(s.ExternalMcps) != 1 {
			t.Fatalf("expected 1 MCP, got %d", len(s.ExternalMcps))
		}
		if s.ExternalMcps[0].ID != "mcp1" {
			t.Fatalf("expected ID 'mcp1', got %q", s.ExternalMcps[0].ID)
		}
	})
}

func TestRemoveExternalMcp(t *testing.T) {
	t.Run("removes MCP from slice", func(t *testing.T) {
		s := newTestSettings(t, nil)
		s.AddExternalMcp(ExternalMcp{ID: "mcp1", DisplayName: "Test MCP"})

		s.RemoveExternalMcp("mcp1")

		if len(s.ExternalMcps) != 0 {
			t.Fatalf("expected 0 MCPs, got %d", len(s.ExternalMcps))
		}
	})

	t.Run("no-op for unknown ID", func(t *testing.T) {
		s := newTestSettings(t, nil)
		s.AddExternalMcp(ExternalMcp{ID: "keep"})
		s.RemoveExternalMcp("nonexistent")
		if len(s.ExternalMcps) != 1 {
			t.Fatalf("expected 1 MCP, got %d", len(s.ExternalMcps))
		}
	})
}

// ---------------------------------------------------------------------------
// UpdateExternalMcp
// ---------------------------------------------------------------------------

func TestUpdateExternalMcp(t *testing.T) {
	t.Run("replaces config by ID", func(t *testing.T) {
		s := newTestSettings(t, nil)
		s.AddExternalMcp(ExternalMcp{
			ID:          "mcp1",
			DisplayName: "Old",
			Command:     "/old",
		})

		s.UpdateExternalMcp(ExternalMcp{
			ID:          "mcp1",
			DisplayName: "New",
			Command:     "/new",
		})

		mcp, _ := s.findMcpByID("mcp1")
		if mcp == nil {
			t.Fatal("MCP should still exist after update")
		}
		if mcp.DisplayName != "New" {
			t.Fatalf("expected DisplayName 'New', got %q", mcp.DisplayName)
		}
		if mcp.Command != "/new" {
			t.Fatalf("expected Command '/new', got %q", mcp.Command)
		}
	})

	t.Run("no-op for unknown ID", func(t *testing.T) {
		s := newTestSettings(t, nil)
		// Should not panic.
		s.UpdateExternalMcp(ExternalMcp{ID: "nonexistent", DisplayName: "Ghost"})
	})
}

// ---------------------------------------------------------------------------
// findMcpByID / findServiceByID
// ---------------------------------------------------------------------------

func TestFindMcpByID(t *testing.T) {
	t.Run("finds existing MCP", func(t *testing.T) {
		s := newTestSettings(t, nil)
		s.AddExternalMcp(ExternalMcp{ID: "mcp1", DisplayName: "First"})
		s.AddExternalMcp(ExternalMcp{ID: "mcp2", DisplayName: "Second"})

		mcp, idx := s.findMcpByID("mcp2")
		if mcp == nil {
			t.Fatal("expected to find MCP")
		}
		if mcp.DisplayName != "Second" {
			t.Fatalf("expected 'Second', got %q", mcp.DisplayName)
		}
		if idx != 1 {
			t.Fatalf("expected index 1, got %d", idx)
		}
	})

	t.Run("returns nil for missing ID", func(t *testing.T) {
		s := newTestSettings(t, nil)
		mcp, idx := s.findMcpByID("nope")
		if mcp != nil {
			t.Fatal("expected nil for missing ID")
		}
		if idx != -1 {
			t.Fatalf("expected index -1, got %d", idx)
		}
	})
}

func TestFindServiceByID(t *testing.T) {
	t.Run("finds existing service", func(t *testing.T) {
		s := newTestSettings(t, nil)
		s.AddService(ServiceConfig{ID: "svc1", DisplayName: "Alpha"})
		s.AddService(ServiceConfig{ID: "svc2", DisplayName: "Beta"})

		svc, idx := s.findServiceByID("svc1")
		if svc == nil {
			t.Fatal("expected to find service")
		}
		if svc.DisplayName != "Alpha" {
			t.Fatalf("expected 'Alpha', got %q", svc.DisplayName)
		}
		if idx != 0 {
			t.Fatalf("expected index 0, got %d", idx)
		}
	})

	t.Run("returns nil for missing ID", func(t *testing.T) {
		s := newTestSettings(t, nil)
		svc, idx := s.findServiceByID("nope")
		if svc != nil {
			t.Fatal("expected nil for missing ID")
		}
		if idx != -1 {
			t.Fatalf("expected index -1, got %d", idx)
		}
	})
}

// ---------------------------------------------------------------------------
// Service helpers
// ---------------------------------------------------------------------------

func TestAddRemoveService(t *testing.T) {
	t.Run("add and remove", func(t *testing.T) {
		s := newTestSettings(t, nil)
		s.AddService(ServiceConfig{ID: "svc1", DisplayName: "One"})
		s.AddService(ServiceConfig{ID: "svc2", DisplayName: "Two"})

		if len(s.Services) != 2 {
			t.Fatalf("expected 2 services, got %d", len(s.Services))
		}

		s.RemoveService("svc1")
		if len(s.Services) != 1 {
			t.Fatalf("expected 1 service, got %d", len(s.Services))
		}
		if s.Services[0].ID != "svc2" {
			t.Fatal("wrong service was removed")
		}
	})
}

func TestUpdateService(t *testing.T) {
	t.Run("updates existing service", func(t *testing.T) {
		s := newTestSettings(t, nil)
		s.AddService(ServiceConfig{ID: "svc1", DisplayName: "Old", Command: "/old"})
		s.UpdateService(ServiceConfig{ID: "svc1", DisplayName: "New", Command: "/new"})

		svc, _ := s.findServiceByID("svc1")
		if svc.DisplayName != "New" {
			t.Fatalf("expected 'New', got %q", svc.DisplayName)
		}
		if svc.Command != "/new" {
			t.Fatalf("expected '/new', got %q", svc.Command)
		}
	})

	t.Run("no-op for unknown ID", func(t *testing.T) {
		s := newTestSettings(t, nil)
		// Should not panic.
		s.UpdateService(ServiceConfig{ID: "nonexistent"})
	})
}

// ---------------------------------------------------------------------------
// UpdateOAuthState
// ---------------------------------------------------------------------------

func TestUpdateOAuthState(t *testing.T) {
	s := newTestSettings(t, nil)
	s.AddExternalMcp(ExternalMcp{ID: "mcp1", Transport: "http", URL: "https://example.com"})

	oauth := &OAuthState{ClientID: "cid", AccessToken: "at"}
	s.UpdateOAuthState("mcp1", oauth)

	mcp, _ := s.findMcpByID("mcp1")
	if mcp.OAuthState == nil {
		t.Fatal("OAuthState should be set")
	}
	if mcp.OAuthState.ClientID != "cid" {
		t.Fatalf("expected ClientID 'cid', got %q", mcp.OAuthState.ClientID)
	}

	// No-op for unknown.
	s.UpdateOAuthState("nonexistent", oauth)
}

// ---------------------------------------------------------------------------
// AllExternalMcpIDs
// ---------------------------------------------------------------------------

func TestAllExternalMcpIDs(t *testing.T) {
	s := newTestSettings(t, nil)
	s.AddExternalMcp(ExternalMcp{ID: "b"})
	s.AddExternalMcp(ExternalMcp{ID: "a"})
	s.AddExternalMcp(ExternalMcp{ID: "c"})

	ids := s.AllExternalMcpIDs()
	if len(ids) != 3 {
		t.Fatalf("expected 3 IDs, got %d", len(ids))
	}
	// Order should match insertion order.
	expected := []string{"b", "a", "c"}
	for i, want := range expected {
		if ids[i] != want {
			t.Fatalf("ids[%d] = %q, want %q", i, ids[i], want)
		}
	}
}

// ---------------------------------------------------------------------------
// ExternalMcp.IsHTTP
// ---------------------------------------------------------------------------

func TestIsHTTP(t *testing.T) {
	t.Run("http transport", func(t *testing.T) {
		m := &ExternalMcp{Transport: "http"}
		if !m.IsHTTP() {
			t.Fatal("expected true for http transport")
		}
	})
	t.Run("stdio transport", func(t *testing.T) {
		m := &ExternalMcp{Transport: "stdio"}
		if m.IsHTTP() {
			t.Fatal("expected false for stdio transport")
		}
	})
	t.Run("empty transport", func(t *testing.T) {
		m := &ExternalMcp{}
		if m.IsHTTP() {
			t.Fatal("expected false for empty transport")
		}
	})
}

// ---------------------------------------------------------------------------
// defaultSettings
// ---------------------------------------------------------------------------

func TestDefaultSettings(t *testing.T) {
	s := defaultSettings()
	if s.Version != 1 {
		t.Fatalf("expected version 1, got %d", s.Version)
	}
	if s.ExternalMcps == nil || len(s.ExternalMcps) != 0 {
		t.Fatal("ExternalMcps should be non-nil empty slice")
	}
	if s.Services == nil || len(s.Services) != 0 {
		t.Fatal("Services should be non-nil empty slice")
	}
	if s.Projects == nil || len(s.Projects) != 0 {
		t.Fatal("Projects should be non-nil empty slice")
	}
}

// ---------------------------------------------------------------------------
// Settings cache: Get, Reload, With
// ---------------------------------------------------------------------------

// These tests use NewSettingsStoreAt with a temp directory for full isolation.

func TestSettingsCache(t *testing.T) {
	newStore := func(t *testing.T) (*FileSettingsStore, string) {
		t.Helper()
		dir := t.TempDir()
		return NewSettingsStoreAt(dir), filepath.Join(dir, "settings.json")
	}

	t.Run("Get returns defaults when no file and no cache", func(t *testing.T) {
		store, _ := newStore(t)

		s := store.Get()
		if s == nil {
			t.Fatal("Get should never return nil")
		}
		if s.Version != 1 {
			t.Fatalf("expected version 1, got %d", s.Version)
		}
	})

	t.Run("Get returns distinct snapshots on each call", func(t *testing.T) {
		store, _ := newStore(t)

		s1 := store.Get()
		s2 := store.Get()
		if s1 == s2 {
			t.Fatal("Get should return distinct snapshot pointers")
		}
		if s1.Version != s2.Version {
			t.Fatal("snapshots should be structurally equal")
		}
	})

	t.Run("With writes to disk and updates cache", func(t *testing.T) {
		store, sp := newStore(t)

		err := store.With(func(s *Settings) {
			s.ExternalMcps = append(s.ExternalMcps, ExternalMcp{
				ID:          "cache-test-mcp",
				DisplayName: "Cache Test",
			})
		})
		if err != nil {
			t.Fatalf("With failed: %v", err)
		}

		s := store.Get()
		found := false
		for _, mcp := range s.ExternalMcps {
			if mcp.ID == "cache-test-mcp" {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("cached settings should contain MCP added by With")
		}

		data, err := os.ReadFile(sp)
		if err != nil {
			t.Fatalf("failed to read settings from disk: %v", err)
		}
		var diskSettings Settings
		if err := json.Unmarshal(data, &diskSettings); err != nil {
			t.Fatalf("failed to parse settings from disk: %v", err)
		}
		found = false
		for _, mcp := range diskSettings.ExternalMcps {
			if mcp.ID == "cache-test-mcp" {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("disk settings should contain MCP added by With")
		}
	})

	t.Run("Reload refreshes cache from disk", func(t *testing.T) {
		store, sp := newStore(t)

		err := store.With(func(s *Settings) {
			s.ExternalMcps = []ExternalMcp{{ID: "original", DisplayName: "Original"}}
		})
		if err != nil {
			t.Fatalf("With failed: %v", err)
		}

		modified := Settings{
			Version:      1,
			ExternalMcps: []ExternalMcp{{ID: "disk-written", DisplayName: "Disk"}},
			Services:     []ServiceConfig{},
		}
		data, _ := json.Marshal(modified)
		if err := os.WriteFile(sp, data, 0600); err != nil {
			t.Fatalf("failed to write modified settings: %v", err)
		}

		s := store.Get()
		if len(s.ExternalMcps) == 0 || s.ExternalMcps[0].ID != "original" {
			t.Fatal("Get should return cached (stale) value")
		}

		s = store.Reload()
		if len(s.ExternalMcps) == 0 || s.ExternalMcps[0].ID != "disk-written" {
			t.Fatalf("Reload should return fresh disk data, got %+v", s.ExternalMcps)
		}

		s = store.Get()
		if len(s.ExternalMcps) == 0 || s.ExternalMcps[0].ID != "disk-written" {
			t.Fatal("Get after Reload should return refreshed data")
		}
	})

	t.Run("EnsureInitialized generates AdminSecret if missing", func(t *testing.T) {
		store, _ := newStore(t)

		err := store.EnsureInitialized()
		if err != nil {
			t.Fatalf("EnsureInitialized failed: %v", err)
		}

		s := store.Get()
		if s.AdminSecret == "" {
			t.Fatal("AdminSecret should be auto-generated by EnsureInitialized")
		}
		if len(s.AdminSecret) != 32 {
			t.Fatalf("AdminSecret should be 32 hex chars, got %d", len(s.AdminSecret))
		}
	})

	t.Run("With does not generate AdminSecret", func(t *testing.T) {
		store, _ := newStore(t)

		err := store.With(func(s *Settings) {})
		if err != nil {
			t.Fatalf("With failed: %v", err)
		}

		s := store.Get()
		if s.AdminSecret != "" {
			t.Fatal("With should not generate AdminSecret; that is EnsureInitialized's job")
		}
	})
}

// ---------------------------------------------------------------------------
// deepCopySettings: map isolation
// ---------------------------------------------------------------------------

func TestDeepCopySettings_MapIsolation(t *testing.T) {
	original := &Settings{
		Version: 1,
		ExternalMcps: []ExternalMcp{{
			ID:          "mcp-a",
			DisplayName: "A",
			Env:         map[string]string{"FOO": "bar"},
		}},
		Services: []ServiceConfig{{
			ID:  "svc-a",
			Env: map[string]string{"BAZ": "qux"},
		}},
	}

	cp := deepCopySettings(original)

	// Mutate every map and slice in the copy.
	cp.ExternalMcps[0].Env["FOO"] = "changed"
	cp.ExternalMcps[0].Env["NEW"] = "added"
	cp.Services[0].Env["BAZ"] = "changed"

	// Verify original is untouched.
	if original.ExternalMcps[0].Env["FOO"] != "bar" {
		t.Fatal("original ExternalMcp Env was corrupted")
	}
	if _, ok := original.ExternalMcps[0].Env["NEW"]; ok {
		t.Fatal("original ExternalMcp Env has unexpected key")
	}
	if original.Services[0].Env["BAZ"] != "qux" {
		t.Fatal("original Service Env was corrupted")
	}
}

// TestDeepCopySettings_AllFieldsCovered uses mutation to verify that maps and
// slices in Settings are properly deep-copied. This catches regressions when
// new fields are added but deepCopySettings is not updated.
func TestDeepCopySettings_AllFieldsCovered(t *testing.T) {
	original := &Settings{
		Version: 1,
		ExternalMcps: []ExternalMcp{{
			ID:  "mcp1",
			Env: map[string]string{"K": "V"},
		}},
		Services: []ServiceConfig{{
			ID:  "svc1",
			Env: map[string]string{"A": "B"},
		}},
	}

	cp := deepCopySettings(original)

	// Check top-level slices are different pointers.
	checkSliceCopy(t, "ExternalMcps", original.ExternalMcps, cp.ExternalMcps)
	checkSliceCopy(t, "Services", original.Services, cp.Services)

	// Check nested maps in ExternalMcps.
	checkMapCopy(t, "ExternalMcps[0].Env", original.ExternalMcps[0].Env, cp.ExternalMcps[0].Env)

	// Check nested maps in Services.
	checkMapCopy(t, "Services[0].Env", original.Services[0].Env, cp.Services[0].Env)
}

func checkSliceCopy[T any](t *testing.T, name string, orig, cp []T) {
	t.Helper()
	if len(orig) == 0 {
		return
	}
	if &orig[0] == &cp[0] {
		t.Errorf("deepCopySettings: %s shares backing array with original", name)
	}
}

func checkMapCopy[K comparable, V any](t *testing.T, name string, orig, cp map[K]V) {
	t.Helper()
	if orig == nil {
		return
	}
	// Mutate the copy and verify the original is unchanged.
	origLen := len(orig)
	var zeroK K
	for k := range cp {
		zeroK = k
		break
	}
	delete(cp, zeroK)
	if len(orig) != origLen {
		t.Errorf("deepCopySettings: %s shares map with original", name)
	}
	// Restore the deleted key (best effort).
	var zeroV V
	cp[zeroK] = zeroV
}

// ---------------------------------------------------------------------------
// FileSettingsStore.load: JSON round-trip and normalization
// ---------------------------------------------------------------------------

func TestLoad(t *testing.T) {
	newStore := func(t *testing.T) (*FileSettingsStore, string) {
		t.Helper()
		dir := t.TempDir()
		store := NewSettingsStoreAt(dir)
		return store, store.path()
	}

	t.Run("sets version to 1 if missing", func(t *testing.T) {
		store, sp := newStore(t)
		data := []byte(`{"external_mcps":[],"services":[]}`)
		if err := os.WriteFile(sp, data, 0600); err != nil {
			t.Fatal(err)
		}
		s := store.load()
		if s.Version != 1 {
			t.Fatalf("expected version 1, got %d", s.Version)
		}
	})

	t.Run("ensures nil slices become non-nil", func(t *testing.T) {
		store, sp := newStore(t)
		data := []byte(`{"version":1}`)
		if err := os.WriteFile(sp, data, 0600); err != nil {
			t.Fatal(err)
		}
		s := store.load()
		if s.ExternalMcps == nil {
			t.Fatal("ExternalMcps should not be nil")
		}
		if s.Services == nil {
			t.Fatal("Services should not be nil")
		}
	})

	t.Run("returns defaults for missing file", func(t *testing.T) {
		store, _ := newStore(t)
		s := store.load()
		if s.Version != 1 {
			t.Fatalf("expected version 1, got %d", s.Version)
		}
	})

	t.Run("returns defaults for invalid JSON", func(t *testing.T) {
		store, sp := newStore(t)
		if err := os.WriteFile(sp, []byte(`{not json`), 0600); err != nil {
			t.Fatal(err)
		}
		s := store.load()
		if s.Version != 1 {
			t.Fatalf("expected version 1 for invalid JSON, got %d", s.Version)
		}
	})
}

// ---------------------------------------------------------------------------
// FileSettingsStore.save: atomic write
// ---------------------------------------------------------------------------

func TestSave(t *testing.T) {
	newStore := func(t *testing.T) (*FileSettingsStore, string) {
		t.Helper()
		dir := t.TempDir()
		store := NewSettingsStoreAt(dir)
		return store, store.path()
	}

	t.Run("writes valid JSON", func(t *testing.T) {
		store, sp := newStore(t)
		s := &Settings{
			Version:      1,
			ExternalMcps: []ExternalMcp{{ID: "save-test", DisplayName: "Save Test"}},
			Services:     []ServiceConfig{},
		}
		err := store.save(s)
		if err != nil {
			t.Fatalf("save failed: %v", err)
		}

		data, err := os.ReadFile(sp)
		if err != nil {
			t.Fatalf("failed to read back: %v", err)
		}
		var loaded Settings
		if err := json.Unmarshal(data, &loaded); err != nil {
			t.Fatalf("written file is not valid JSON: %v", err)
		}
		if len(loaded.ExternalMcps) != 1 || loaded.ExternalMcps[0].ID != "save-test" {
			t.Fatal("saved data does not match")
		}
	})

	t.Run("no temp file left behind", func(t *testing.T) {
		store, sp := newStore(t)
		s := defaultSettings()
		_ = store.save(s)

		tmp := sp + ".tmp"
		if _, err := os.Stat(tmp); !os.IsNotExist(err) {
			t.Fatal("temp file should not remain after successful save")
		}
	})

	t.Run("creates directory if missing", func(t *testing.T) {
		// Use a subdirectory that doesn't exist yet within the temp dir.
		base := t.TempDir()
		dir := filepath.Join(base, "nested")
		store := NewSettingsStoreAt(dir)
		s := defaultSettings()
		err := store.save(s)
		if err != nil {
			t.Fatalf("save failed: %v", err)
		}

		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("settings dir should exist: %v", err)
		}
		if !info.IsDir() {
			t.Fatal("settings dir should be a directory")
		}
	})
}

// ---------------------------------------------------------------------------
// ensureAdminSecret
// ---------------------------------------------------------------------------

func TestEnsureAdminSecret(t *testing.T) {
	t.Run("generates secret when empty", func(t *testing.T) {
		s := defaultSettings()
		ensureAdminSecret(s)
		if s.AdminSecret == "" {
			t.Fatal("AdminSecret should be generated")
		}
		if len(s.AdminSecret) != 32 {
			t.Fatalf("expected 32 hex chars, got %d", len(s.AdminSecret))
		}
	})

	t.Run("does not overwrite existing secret", func(t *testing.T) {
		s := defaultSettings()
		s.AdminSecret = "keep-me"
		ensureAdminSecret(s)
		if s.AdminSecret != "keep-me" {
			t.Fatalf("expected 'keep-me', got %q", s.AdminSecret)
		}
	})
}

// ---------------------------------------------------------------------------
// JSON round-trip: Settings serialization
// ---------------------------------------------------------------------------

func TestSettingsJSONRoundTrip(t *testing.T) {
	original := &Settings{
		Version: 1,
		ExternalMcps: []ExternalMcp{
			{
				ID:          "mcp1",
				DisplayName: "Test MCP",
				Command:     "/usr/bin/test",
				Args:        []string{"--flag"},
				Env:         map[string]string{"KEY": "VAL"},
				Transport:   "stdio",
			},
		},
		Services: []ServiceConfig{
			{
				ID:          "svc1",
				DisplayName: "Test Service",
				Command:     "/usr/bin/svc",
				Args:        []string{},
				Env:         map[string]string{},
				Autostart:   true,
			},
		},
		AdminSecret: "secret123",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var restored Settings
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	// Spot-check key fields.
	if restored.Version != 1 {
		t.Fatalf("version: got %d, want 1", restored.Version)
	}
	if len(restored.ExternalMcps) != 1 {
		t.Fatalf("mcps: got %d, want 1", len(restored.ExternalMcps))
	}
	if restored.ExternalMcps[0].Env["KEY"] != "VAL" {
		t.Fatal("env KEY should be VAL")
	}
	if len(restored.Services) != 1 || !restored.Services[0].Autostart {
		t.Fatal("service autostart should be true")
	}
	if restored.AdminSecret != "secret123" {
		t.Fatalf("admin secret: got %q, want 'secret123'", restored.AdminSecret)
	}
}

// ---------------------------------------------------------------------------
// Edge cases
// ---------------------------------------------------------------------------

func TestEdgeCases(t *testing.T) {
	t.Run("store path is under dir", func(t *testing.T) {
		dir := t.TempDir()
		store := NewSettingsStoreAt(dir)
		path := store.path()
		if filepath.Dir(path) != dir {
			t.Fatalf("store.path() %q should be inside dir %q", path, dir)
		}
		if filepath.Base(path) != "settings.json" {
			t.Fatalf("settings file should be named settings.json, got %q", filepath.Base(path))
		}
	})
}

func TestUpsertExternalMcp(t *testing.T) {
	t.Run("inserts new MCP and returns false", func(t *testing.T) {
		s := newTestSettings(t, nil)
		cfg := ExternalMcp{ID: "new-mcp", DisplayName: "New"}
		updated := s.UpsertExternalMcp(cfg)
		if updated {
			t.Fatal("expected insert (false), got update (true)")
		}
		if len(s.ExternalMcps) != 1 || s.ExternalMcps[0].ID != "new-mcp" {
			t.Fatal("MCP not added")
		}
	})

	t.Run("updates existing MCP and returns true", func(t *testing.T) {
		s := newTestSettings(t, []ExternalMcp{
			{ID: "mcp1", DisplayName: "Old", Command: "old-cmd"},
		})
		cfg := ExternalMcp{ID: "mcp1", DisplayName: "Updated", Command: "new-cmd"}
		updated := s.UpsertExternalMcp(cfg)
		if !updated {
			t.Fatal("expected update (true), got insert (false)")
		}
		if len(s.ExternalMcps) != 1 {
			t.Fatal("should still have 1 MCP")
		}
		if s.ExternalMcps[0].Command != "new-cmd" {
			t.Fatal("command not updated")
		}
	})
}

func TestUpsertService(t *testing.T) {
	t.Run("inserts new service and returns false", func(t *testing.T) {
		s := newTestSettings(t, nil)
		cfg := ServiceConfig{ID: "svc1", DisplayName: "Svc 1", Command: "cmd"}
		updated := s.UpsertService(cfg)
		if updated {
			t.Fatal("expected insert (false), got update (true)")
		}
		if len(s.Services) != 1 || s.Services[0].ID != "svc1" {
			t.Fatal("service not added")
		}
	})

	t.Run("updates existing service and returns true", func(t *testing.T) {
		s := newTestSettings(t, nil)
		s.Services = []ServiceConfig{{ID: "svc1", DisplayName: "Old", Command: "old-cmd"}}
		cfg := ServiceConfig{ID: "svc1", DisplayName: "New", Command: "new-cmd"}
		updated := s.UpsertService(cfg)
		if !updated {
			t.Fatal("expected update (true), got insert (false)")
		}
		if len(s.Services) != 1 {
			t.Fatal("should still have 1 service")
		}
		if s.Services[0].Command != "new-cmd" {
			t.Fatal("command not updated")
		}
	})
}

func TestMergeServiceDefaults(t *testing.T) {
	t.Run("fills zero-value fields from existing", func(t *testing.T) {
		s := newTestSettings(t, nil)
		s.Services = []ServiceConfig{{
			ID:         "svc1",
			Command:    "cmd",
			Args:       []string{"--flag"},
			Env:        map[string]string{"K": "V"},
			WorkingDir: "/old/dir",
			URL:        "http://old",
		}}
		cfg := ServiceConfig{ID: "svc1", Command: "new-cmd"}
		s.MergeServiceDefaults(&cfg)
		if cfg.Command != "new-cmd" {
			t.Fatal("should not overwrite non-zero Command")
		}
		if len(cfg.Args) != 1 || cfg.Args[0] != "--flag" {
			t.Fatal("should inherit Args")
		}
		if cfg.Env["K"] != "V" {
			t.Fatal("should inherit Env")
		}
		if cfg.WorkingDir != "/old/dir" {
			t.Fatal("should inherit WorkingDir")
		}
		if cfg.URL != "http://old" {
			t.Fatal("should inherit URL")
		}
	})

	t.Run("does not overwrite non-zero fields", func(t *testing.T) {
		s := newTestSettings(t, nil)
		s.Services = []ServiceConfig{{
			ID:         "svc1",
			Args:       []string{"--old"},
			Env:        map[string]string{"OLD": "1"},
			WorkingDir: "/old",
			URL:        "http://old",
		}}
		cfg := ServiceConfig{
			ID:         "svc1",
			Args:       []string{"--new"},
			Env:        map[string]string{"NEW": "2"},
			WorkingDir: "/new",
			URL:        "http://new",
		}
		s.MergeServiceDefaults(&cfg)
		if cfg.Args[0] != "--new" {
			t.Fatal("should keep caller's Args")
		}
		if cfg.Env["NEW"] != "2" {
			t.Fatal("should keep caller's Env")
		}
		if cfg.WorkingDir != "/new" {
			t.Fatal("should keep caller's WorkingDir")
		}
		if cfg.URL != "http://new" {
			t.Fatal("should keep caller's URL")
		}
	})

	t.Run("no-op for unknown service", func(t *testing.T) {
		s := newTestSettings(t, nil)
		cfg := ServiceConfig{ID: "missing", Command: "cmd"}
		s.MergeServiceDefaults(&cfg)
		if cfg.Command != "cmd" {
			t.Fatal("should not mutate when service not found")
		}
	})
}

func TestResolveMcpID(t *testing.T) {
	s := newTestSettings(t, []ExternalMcp{
		{ID: "mcp1", DisplayName: "My MCP"},
		{ID: "mcp2", DisplayName: "Other MCP"},
	})

	t.Run("finds by id", func(t *testing.T) {
		if s.ResolveMcpID("mcp1", "") != "mcp1" {
			t.Fatal("should find by id")
		}
	})

	t.Run("finds by name", func(t *testing.T) {
		if s.ResolveMcpID("", "Other MCP") != "mcp2" {
			t.Fatal("should find by display name")
		}
	})

	t.Run("returns empty for unknown id", func(t *testing.T) {
		if s.ResolveMcpID("nope", "") != "" {
			t.Fatal("should return empty for unknown id")
		}
	})

	t.Run("returns empty for unknown name", func(t *testing.T) {
		if s.ResolveMcpID("", "Nope") != "" {
			t.Fatal("should return empty for unknown name")
		}
	})

	t.Run("id takes precedence over name", func(t *testing.T) {
		if s.ResolveMcpID("mcp1", "Other MCP") != "mcp1" {
			t.Fatal("id should take precedence")
		}
	})
}

func TestResolveServiceID(t *testing.T) {
	s := newTestSettings(t, nil)
	s.Services = []ServiceConfig{
		{ID: "svc1", DisplayName: "My Service"},
		{ID: "svc2", DisplayName: "Other Service"},
	}

	t.Run("finds by id", func(t *testing.T) {
		if s.ResolveServiceID("svc1", "") != "svc1" {
			t.Fatal("should find by id")
		}
	})

	t.Run("finds by name", func(t *testing.T) {
		if s.ResolveServiceID("", "Other Service") != "svc2" {
			t.Fatal("should find by display name")
		}
	})

	t.Run("returns empty for unknown id", func(t *testing.T) {
		if s.ResolveServiceID("nope", "") != "" {
			t.Fatal("should return empty for unknown id")
		}
	})

	t.Run("returns empty for unknown name", func(t *testing.T) {
		if s.ResolveServiceID("", "Nope") != "" {
			t.Fatal("should return empty for unknown name")
		}
	})
}
