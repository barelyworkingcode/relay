package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReloadIfChanged_DetectsExternalWrite(t *testing.T) {
	dir := mkSandboxRelayHome(t)
	store := NewSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}

	if got := store.ReloadIfChanged(); got != nil {
		t.Fatalf("ReloadIfChanged on an unchanged file should return nil, got %+v", got)
	}

	path := filepath.Join(dir, "settings.json")
	cur := store.Get()
	cur.AdminSecret = "externally-rotated-secret"
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
	if got.AdminSecret != "externally-rotated-secret" {
		t.Fatalf("reloaded AdminSecret = %q, want externally-rotated-secret", got.AdminSecret)
	}
	if store.Get().AdminSecret != "externally-rotated-secret" {
		t.Fatal("cache not updated after ReloadIfChanged")
	}
	if got := store.ReloadIfChanged(); got != nil {
		t.Fatalf("second ReloadIfChanged with no change should return nil, got %+v", got)
	}
}

func TestReloadIfChanged_NilAfterInternalWrite(t *testing.T) {
	dir := mkSandboxRelayHome(t)
	store := NewSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}

	if err := store.With(func(s *Settings) { s.AdminSecret = "internally-set" }); err != nil {
		t.Fatalf("With: %v", err)
	}
	if got := store.ReloadIfChanged(); got != nil {
		t.Fatalf("ReloadIfChanged after our own With should return nil (modtime seeded), got %+v", got)
	}
}

func TestReloadIfChanged_MissingFileReturnsNil(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	store := NewSettingsStoreAt(dir) // deliberately no EnsureInitialized → no file
	if got := store.ReloadIfChanged(); got != nil {
		t.Fatalf("ReloadIfChanged with no settings file should return nil, got %+v", got)
	}
}
