package main

// Coverage for FileSettingsStore.ReloadIfChanged — the modtime-gated
// cross-process reload primitive the status poller and CLI-driven settings
// edits rely on. Previously untested: a regression that always-reloads
// (hammering disk) or never-reloads (settings changes never propagate) would
// have passed the suite.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestReloadIfChanged_DetectsExternalWrite proves the happy path: an external
// writer (another relay process, a hand edit) bumps the file modtime, and the
// next ReloadIfChanged picks up the new contents exactly once.
func TestReloadIfChanged_DetectsExternalWrite(t *testing.T) {
	dir := mkSandboxRelayHome(t)
	store := NewSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}

	// No change since init → nil.
	if got := store.ReloadIfChanged(); got != nil {
		t.Fatalf("ReloadIfChanged on an unchanged file should return nil, got %+v", got)
	}

	// Simulate an external writer changing settings.json out from under us.
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
	// Cache was updated too, so plain Get sees the new value.
	if store.Get().AdminSecret != "externally-rotated-secret" {
		t.Fatal("cache not updated after ReloadIfChanged")
	}
	// A second call with no further change must return nil (modtime now seeded).
	if got := store.ReloadIfChanged(); got != nil {
		t.Fatalf("second ReloadIfChanged with no change should return nil, got %+v", got)
	}
}

// TestReloadIfChanged_NilAfterInternalWrite verifies With() seeds the modtime,
// so our own writes don't trigger a redundant re-read on the next poll tick.
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

// TestReloadIfChanged_MissingFileReturnsNil ensures a missing settings.json is
// handled gracefully (no panic, no spurious reload) rather than treated as a
// change.
func TestReloadIfChanged_MissingFileReturnsNil(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	store := NewSettingsStoreAt(dir) // deliberately no EnsureInitialized → no file
	if got := store.ReloadIfChanged(); got != nil {
		t.Fatalf("ReloadIfChanged with no settings file should return nil, got %+v", got)
	}
}
