package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	newStore := func(t *testing.T) (*FileSettingsStore, string) {
		t.Helper()
		dir := t.TempDir()
		store := sealedSettingsStoreAt(dir)
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

func TestSave(t *testing.T) {
	newStore := func(t *testing.T) (*FileSettingsStore, string) {
		t.Helper()
		dir := t.TempDir()
		store := sealedSettingsStoreAt(dir)
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
		s := DefaultSettings()
		_ = store.save(s)

		tmp := sp + ".tmp"
		if _, err := os.Stat(tmp); !os.IsNotExist(err) {
			t.Fatal("temp file should not remain after successful save")
		}
	})

	t.Run("creates directory if missing", func(t *testing.T) {
		base := t.TempDir()
		dir := filepath.Join(base, "nested")
		store := sealedSettingsStoreAt(dir)
		s := DefaultSettings()
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

func TestEnsureAdminSecret(t *testing.T) {
	t.Run("generates secret when empty", func(t *testing.T) {
		s := DefaultSettings()
		ensureAdminSecret(s)
		pt, ok := s.AdminSecret.Reveal()
		if !ok || pt == "" {
			t.Fatal("AdminSecret should be generated")
		}
		if len(pt) != 32 {
			t.Fatalf("expected 32 hex chars, got %d", len(pt))
		}
	})

	t.Run("does not overwrite existing secret", func(t *testing.T) {
		s := DefaultSettings()
		s.AdminSecret = NewSecret("keep-me")
		ensureAdminSecret(s)
		if pt, _ := s.AdminSecret.Reveal(); pt != "keep-me" {
			t.Fatalf("expected 'keep-me', got %q", pt)
		}
	})
}

// A Settings value must go through sealAllSecrets before it can be
// marshalled at all (§4.4) — this pins that a document sealed that way
// still round-trips its clear fields untouched, and its sealed fields open
// back to the exact plaintext they held before the round trip.
func TestSettingsJSONRoundTrip(t *testing.T) {
	original := &Settings{
		Version: 1,
		ExternalMcps: []ExternalMcp{
			{
				ID:          "mcp1",
				DisplayName: "Test MCP",
				Command:     "/usr/bin/test",
				Args:        []string{"--flag"},
				Env:         secretMapFromPlain(map[string]string{"KEY": "VAL"}),
				Transport:   "stdio",
			},
		},
		Services: []ServiceConfig{
			{
				ID:          "svc1",
				DisplayName: "Test Service",
				Command:     "/usr/bin/svc",
				Args:        []string{},
				Env:         map[string]Secret{},
				Autostart:   true,
			},
		},
		AdminSecret: NewSecret("secret123"),
	}

	if err := SealAllSecrets(original, testSealer()); err != nil {
		t.Fatalf("seal failed: %v", err)
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var restored Settings
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if errs := openAllSecrets(&restored, testSealer()); len(errs) != 0 {
		t.Fatalf("open failed: %v", errs)
	}

	if restored.Version != 1 {
		t.Fatalf("version: got %d, want 1", restored.Version)
	}
	if len(restored.ExternalMcps) != 1 {
		t.Fatalf("mcps: got %d, want 1", len(restored.ExternalMcps))
	}
	if got, _ := restored.ExternalMcps[0].Env["KEY"].Reveal(); got != "VAL" {
		t.Fatal("env KEY should be VAL")
	}
	if len(restored.Services) != 1 || !restored.Services[0].Autostart {
		t.Fatal("service autostart should be true")
	}
	if pt, _ := restored.AdminSecret.Reveal(); pt != "secret123" {
		t.Fatalf("admin secret: got %q, want 'secret123'", pt)
	}
}

func TestEdgeCases(t *testing.T) {
	t.Run("store path is under dir", func(t *testing.T) {
		dir := t.TempDir()
		store := sealedSettingsStoreAt(dir)
		path := store.path()
		if filepath.Dir(path) != dir {
			t.Fatalf("store.path() %q should be inside dir %q", path, dir)
		}
		if filepath.Base(path) != "settings.json" {
			t.Fatalf("settings file should be named settings.json, got %q", filepath.Base(path))
		}
	})
}
