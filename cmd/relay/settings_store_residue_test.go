package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
)

// A stat that fails must close READS as well as writes. readErr alone refuses
// only the next write; leaving the cache and the return in place lets an
// authorization decision resolving through FreshSettings answer from a copy
// older than the failure. Every other degraded state — corrupt, truncated,
// unreadable, deleted — resolves to empty settings
// (settings_store_freshness_test.go pins all four).
//
// The path is broken by replacing the config DIRECTORY with a regular file,
// which makes the stat fail ENOTDIR. That is a stat failure that is not
// IsNotExist and needs no chmod, so it behaves the same for a test run as
// root.
//
// This test stays in package main rather than moving to internal/config with
// the rest of the store's residue proofs: the property is that a real
// AUTHORIZATION decision cannot resolve from the stale cache, and the real
// authenticator is authenticateAPICredential here. Reproducing its lookup
// inside the config package would leave the test passing while the thing it
// names stopped being exercised.
func TestSettingsStore_AFailedStatClosesReadsRatherThanServingTheOldCache(t *testing.T) {
	home := mkEmptySandboxRelayHome(t)
	dir := filepath.Join(home, "cfg")
	store := sealedSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	if err := store.With(func(s *config.Settings) {
		s.APICredentials = append(s.APICredentials, config.APICredential{
			ID:      "residue-cred",
			Name:    "residue",
			Hash:    config.HashToken("residue-plaintext"),
			Classes: []control.CapabilityClass{control.ClassRead},
		})
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if authenticateAPICredential(config.FreshSettings(store), "residue-plaintext") == nil {
		t.Fatal("the fixture never authenticated in the first place")
	}

	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove config dir: %v", err)
	}
	if err := os.WriteFile(dir, []byte("this is not a directory"), 0600); err != nil {
		t.Fatalf("replace config dir with a file: %v", err)
	}
	_, statErr := os.Stat(filepath.Join(dir, "settings.json"))
	if statErr == nil || os.IsNotExist(statErr) {
		t.Fatalf("fixture did not produce a non-IsNotExist stat failure: %v", statErr)
	}

	if authenticateAPICredential(config.FreshSettings(store), "residue-plaintext") != nil {
		t.Fatal("a credential still authenticates from a cache older than the stat failure that made settings.json unreadable")
	}
	if got := len(config.FreshSettings(store).APICredentials); got != 0 {
		t.Fatalf("FreshSettings served %d credentials from the stale cache, want 0", got)
	}
}
