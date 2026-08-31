package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// A stat that fails must close READS as well as writes. readErr alone refuses
// only the next write; leaving the cache and the return in place lets an
// authorization decision resolving through freshSettings answer from a copy
// older than the failure. Every other degraded state — corrupt, truncated,
// unreadable, deleted — resolves to empty settings
// (settings_store_freshness_test.go pins all four).
//
// The path is broken by replacing the config DIRECTORY with a regular file,
// which makes the stat fail ENOTDIR. That is a stat failure that is not
// IsNotExist and needs no chmod, so it behaves the same for a test run as
// root.
func TestSettingsStore_AFailedStatClosesReadsRatherThanServingTheOldCache(t *testing.T) {
	home := mkEmptySandboxRelayHome(t)
	dir := filepath.Join(home, "cfg")
	store := sealedSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	if err := store.With(func(s *Settings) {
		s.APICredentials = append(s.APICredentials, APICredential{
			ID:      "residue-cred",
			Name:    "residue",
			Hash:    hashToken("residue-plaintext"),
			Classes: []CapabilityClass{ClassRead},
		})
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if freshSettings(store).AuthenticateAPICredential("residue-plaintext") == nil {
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

	if freshSettings(store).AuthenticateAPICredential("residue-plaintext") != nil {
		t.Fatal("a credential still authenticates from a cache older than the stat failure that made settings.json unreadable")
	}
	if got := len(freshSettings(store).APICredentials); got != 0 {
		t.Fatalf("freshSettings served %d credentials from the stale cache, want 0", got)
	}
}

// The boundary on the rule above, and the same one the deletion branch draws:
// a store that has NEVER seen settings.json reports no change, so a first
// start's in-hand settings are not emptied before anything has written them
// out. Only a file this store had seen may invalidate its cache.
func TestSettingsStore_AFailedStatOnAFileNeverSeenReportsNoChange(t *testing.T) {
	home := mkEmptySandboxRelayHome(t)
	dir := filepath.Join(home, "cfg")
	if err := os.WriteFile(dir, []byte("this is not a directory"), 0600); err != nil {
		t.Fatalf("replace config dir with a file: %v", err)
	}

	store := sealedSettingsStoreAt(dir)
	if got := store.ReloadIfChanged(); got != nil {
		t.Fatalf("a settings.json this store never saw must report no change, got %+v", got)
	}
	if err := store.With(func(s *Settings) { s.AdminSecret = NewSecret("x") }); err == nil {
		t.Fatal("With must still refuse over a path it could not read")
	}
}

// normalize() runs on load(), and must also run on what a callback produced:
// without it a record appended by a mutation reaches disk with `"args": null`
// where every record that has been through a load spells the same emptiness
// `[]`. A round trip repairs it, so nothing in-process sees the difference —
// but `relay audit`, `relay grant` and a hand-edit read the file.
func TestSettingsStore_ACallbacksRecordIsNormalizedBeforeItReachesDisk(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	if err := store.With(func(s *Settings) {
		s.UpsertExternalMcp(ExternalMcp{ID: "mcp1", DisplayName: "MCP One", Command: "/bin/true"})
		s.UpsertService(ServiceConfig{ID: "svc1", DisplayName: "Svc One", Command: "/bin/true"})
	}); err != nil {
		t.Fatalf("write records: %v", err)
	}

	raw := sdRead(t, dir)
	for _, null := range []string{`"args": null`, `"env": null`} {
		if bytes.Contains(raw, []byte(null)) {
			t.Errorf("settings.json holds %s, which no record that has been through load() ever spells that way:\n%s", null, raw)
		}
	}
}

// The two properties normalize() has to have before save() may rely on it: it
// must be idempotent, and it must not turn a legitimately-empty value into
// something else or overwrite a populated one.
func TestSettingsNormalizeIsIdempotentAndDestroysNothing(t *testing.T) {
	populated := func() *Settings {
		return &Settings{
			ExternalMcps: []ExternalMcp{
				{ID: "empty", Args: []string{}, Env: map[string]Secret{}},
				{ID: "full", Args: []string{"--root", "/tmp"}, Env: secretMapFromPlain(map[string]string{"K": "V"})},
			},
			Services: []ServiceConfig{{ID: "svc", Args: []string{"serve"}}},
			Projects: []Project{{ID: "p", AllowedMcpIDs: []string{"empty"}, AllowedModels: []string{}}},
		}
	}

	once := populated()
	once.normalize()
	twice := populated()
	twice.normalize()
	twice.normalize()
	// Compared as Go values, not marshalled JSON: sealAllSecrets draws a
	// fresh random nonce on every call (by design, §4.5), so two
	// independently sealed copies of the identical plaintext never produce
	// byte-identical ciphertext — that would be a nonce-reuse bug, not an
	// idempotency failure.
	if !reflect.DeepEqual(once, twice) {
		t.Fatalf("normalize is not idempotent:\n once %+v\n twice %+v", once, twice)
	}

	if got := once.ExternalMcps[0].Args; got == nil || len(got) != 0 {
		t.Errorf("a legitimately empty args became %#v", got)
	}
	if got := once.ExternalMcps[1].Args; len(got) != 2 || got[0] != "--root" || got[1] != "/tmp" {
		t.Errorf("a populated args became %#v", got)
	}
	if got, ok := once.ExternalMcps[1].Env["K"].Reveal(); !ok || got != "V" {
		t.Errorf("a populated env became %#v", once.ExternalMcps[1].Env)
	}
	if got := once.Projects[0].AllowedMcpIDs; len(got) != 1 || got[0] != "empty" {
		t.Errorf("a populated allowed_mcp_ids became %#v", got)
	}
	if got := once.Projects[0].AllowedModels; got == nil || len(got) != 0 {
		t.Errorf("an empty allowed_models — the one value modelAllowedForProject won't read as unrestricted — became %#v", got)
	}
	if once.Version != currentSettingsVersion {
		t.Errorf("version = %d, want %d", once.Version, currentSettingsVersion)
	}
}
