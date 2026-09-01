package main

import (
	"encoding/json"
	"github.com/barelyworkingcode/relay/internal/config"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReloadIfChanged_DetectsExternalWrite(t *testing.T) {
	dir := mkSandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}

	if got := store.ReloadIfChanged(); got != nil {
		t.Fatalf("ReloadIfChanged on an unchanged file should return nil, got %+v", got)
	}

	path := filepath.Join(dir, "settings.json")
	cur := store.Get()
	cur.AdminSecret = config.NewSecret("externally-rotated-secret")
	// sealAllSecrets before marshalling, exactly as save() does: a
	// Secret with no envelope refuses to serialise at all (§4.4), so this
	// simulates a real external writer rather than a fixture that bypasses
	// sealing.
	if err := config.SealAllSecrets(cur, testSealer()); err != nil {
		t.Fatalf("seal: %v", err)
	}
	data, err := json.MarshalIndent(cur, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("external write: %v", err)
	}
	// Force a distinctly newer modtime so the test is robust regardless of
	// filesystem timestamp granularity.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	got := store.ReloadIfChanged()
	if got == nil {
		t.Fatal("ReloadIfChanged should have detected the external write")
	}
	if pt, _ := got.AdminSecret.Reveal(); pt != "externally-rotated-secret" {
		t.Fatalf("reloaded AdminSecret = %q, want externally-rotated-secret", pt)
	}
	if pt, _ := store.Get().AdminSecret.Reveal(); pt != "externally-rotated-secret" {
		t.Fatal("cache not updated after ReloadIfChanged")
	}
	if got := store.ReloadIfChanged(); got != nil {
		t.Fatalf("second ReloadIfChanged with no change should return nil, got %+v", got)
	}
}

func TestReloadIfChanged_NilAfterInternalWrite(t *testing.T) {
	dir := mkSandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}

	if err := store.With(func(s *config.Settings) { s.AdminSecret = config.NewSecret("internally-set") }); err != nil {
		t.Fatalf("With: %v", err)
	}
	if got := store.ReloadIfChanged(); got != nil {
		t.Fatalf("ReloadIfChanged after our own With should return nil (modtime seeded), got %+v", got)
	}
}

func TestReloadIfChanged_MissingFileReturnsNil(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir) // deliberately no EnsureInitialized → no file
	if got := store.ReloadIfChanged(); got != nil {
		t.Fatalf("ReloadIfChanged with no settings file should return nil, got %+v", got)
	}
}
