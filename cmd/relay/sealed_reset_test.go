package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/enrolment"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/presence/presencetest"
	"github.com/barelyworkingcode/relay/internal/sealed"
)

// srSetup seeds a sandbox with a fully sealed install (a project, a
// credential-worthy admin secret, and a real CA) under keyID/key, and
// returns the store plus the memory keyring standing in for the keychain.
func srSetup(t *testing.T, keyID string) (dir string, store *config.FileSettingsStore, keyring sealed.Keyring) {
	t.Helper()
	dir = mkEmptySandboxRelayHome(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = 0x77
	}
	keyring = sealed.NewMemoryKeyring(keyID, key)
	var err error
	store, err = config.ResolveSealedStore(dir, keyring)
	assertNoErr(t, err, "ResolveSealedStore")
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")

	// A CA so the reset has something real to delete beyond settings.json.
	_, err = enrolment.LoadOrCreateCA(store.Sealer())
	assertNoErr(t, err, "enrolment.LoadOrCreateCA")

	assertNoErr(t, store.With(func(s *config.Settings) {
		hash := config.HashToken("seed-token")
		s.Projects = append(s.Projects, config.Project{ID: "p1", Name: "P1", Token: config.NewSecret("seed-token"), TokenHash: hash})
	}), "seed a project")

	for _, name := range []string{"settings.json", enrolment.CAKeySealedFile, enrolment.CACertFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("fixture setup: %s missing before the test even starts: %v", name, err)
		}
	}
	return dir, store, keyring
}

// TestResetSealedStore_RequiresPresence is AC-25c's negative half and the
// nil-gate default every gated act shares (§6.7): with no gate wired at
// all, the reset refuses exactly like every other gated operation and
// touches nothing.
func TestResetSealedStore_RequiresPresence(t *testing.T) {
	dir, store, keyring := srSetup(t, "aaaaaaaaaaaaaaaa")
	before := sdRead(t, dir)

	err := resetSealedStore(context.Background(), dir, store, keyring, nil)
	if !errors.Is(err, errPresenceGateNotWired) {
		t.Fatalf("resetSealedStore with a nil gate: err = %v, want errPresenceGateNotWired", err)
	}
	assertResetLeftEverythingIntact(t, dir, before, keyring, "aaaaaaaaaaaaaaaa")
}

// TestResetSealedStore_RefusedPresenceLeavesEverythingIntact is AC-25c's
// other negative case: a live gate that refuses the prompt must not delete
// anything either — deletion only begins after Require succeeds.
func TestResetSealedStore_RefusedPresenceLeavesEverythingIntact(t *testing.T) {
	dir, store, keyring := srSetup(t, "aaaaaaaaaaaaaaaa")
	before := sdRead(t, dir)

	gate, err := presence.NewGate(presencetest.Deny())
	assertNoErr(t, err, "NewGate")

	err = resetSealedStore(context.Background(), dir, store, keyring, gate)
	if !errors.Is(err, presence.ErrRefused) {
		t.Fatalf("resetSealedStore with a denying gate: err = %v, want presence.ErrRefused", err)
	}
	assertResetLeftEverythingIntact(t, dir, before, keyring, "aaaaaaaaaaaaaaaa")
}

func assertResetLeftEverythingIntact(t *testing.T, dir string, before []byte, keyring sealed.Keyring, wantKeyID string) {
	t.Helper()
	after := sdRead(t, dir)
	if string(before) != string(after) {
		t.Error("a refused/unwired reset changed settings.json")
	}
	for _, name := range []string{enrolment.CAKeySealedFile, enrolment.CACertFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was removed despite the reset refusing: %v", name, err)
		}
	}
	keyID, _, err := keyring.Load()
	assertNoErr(t, err, "keyring.Load after a refused reset")
	if keyID != wantKeyID {
		t.Errorf("keyring key id = %q, want the original %q — the refused reset touched the key", keyID, wantKeyID)
	}
}

// TestResetSealedStore_DeletesEverythingAndReinitialisesWithAFreshKey is
// AC-25c's positive case: on a granted presence check, the reset removes
// settings.json, ca.key.sealed, ca.crt and the keychain item together, and
// leaves relay re-initialised under a KEY THE OPERATOR HAS NEVER APPROVED
// BEFORE THIS CALL — which is fine, because approving exactly that is what
// the presence check was for.
func TestResetSealedStore_DeletesEverythingAndReinitialisesWithAFreshKey(t *testing.T) {
	dir, store, keyring := srSetup(t, "aaaaaaaaaaaaaaaa")

	gate, err := presence.NewGate(presencetest.Allow())
	assertNoErr(t, err, "NewGate")

	assertNoErr(t, resetSealedStore(context.Background(), dir, store, keyring, gate), "resetSealedStore")

	// The store handed to resetSealedStore is the SAME instance every
	// already-wired subsystem in a running tray holds — the whole point of
	// Reresolve over constructing a fresh *FileSettingsStore. It must now
	// serve a freshly initialised settings.json, not the deleted one.
	if store.Sealer() == nil {
		t.Fatal("store has no working sealer after the reset — it should have minted and adopted a fresh key")
	}
	got := store.Get()
	if len(got.Projects) != 0 {
		t.Errorf("the reset should have wiped every project; got %d", len(got.Projects))
	}
	if got.SealedKeyID == "" || got.SealedKeyID == "aaaaaaaaaaaaaaaa" {
		t.Errorf("settings.json's sealed_key_id = %q, want a NEW key id", got.SealedKeyID)
	}

	newKeyID, _, err := keyring.Load()
	assertNoErr(t, err, "keyring.Load after reset")
	if newKeyID == "aaaaaaaaaaaaaaaa" {
		t.Fatal("the keychain item still holds the OLD key id — Destroy did not run, or ran after a key was re-adopted")
	}
	if newKeyID != got.SealedKeyID {
		t.Errorf("keyring key id %q does not match settings.json's sealed_key_id %q", newKeyID, got.SealedKeyID)
	}

	// ca.crt/ca.key.sealed are regenerated by the next enrolment.LoadOrCreateCA call,
	// not by resetSealedStore itself — assert they are GONE right after the
	// reset, before anything has had a chance to regenerate them.
	if _, err := os.Stat(filepath.Join(dir, enrolment.CAKeySealedFile)); !os.IsNotExist(err) {
		t.Errorf("ca.key.sealed still exists after the reset (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, enrolment.CACertFile)); !os.IsNotExist(err) {
		t.Errorf("ca.crt still exists after the reset (err=%v)", err)
	}
}

// TestResetSealedStore_DigestBindsBothKeyIDs is AC-22d's argument applied
// to sealed.reset specifically (§6.4's table entry: settings_key_id and
// keychain_key_id, either absent). A grant approved while looking at one
// pair of key ids must not be spendable against another.
func TestResetSealedStore_DigestBindsBothKeyIDs(t *testing.T) {
	base := sealedResetDigest("aaaaaaaaaaaaaaaa", sealed.NewMemoryKeyring("bbbbbbbbbbbbbbbb", make([]byte, 32)))

	sameSettingsDifferentKeychain := sealedResetDigest("aaaaaaaaaaaaaaaa", sealed.NewMemoryKeyring("cccccccccccccccc", make([]byte, 32)))
	if base == sameSettingsDifferentKeychain {
		t.Error("changing the keychain key id did not change the digest")
	}

	differentSettingsSameKeychain := sealedResetDigest("dddddddddddddddd", sealed.NewMemoryKeyring("bbbbbbbbbbbbbbbb", make([]byte, 32)))
	if base == differentSettingsSameKeychain {
		t.Error("changing the settings_key_id did not change the digest")
	}

	// Absent is not empty (AC-22e): no keychain key at all must digest
	// differently from a present-but-different one.
	noKeychainKey := sealedResetDigest("aaaaaaaaaaaaaaaa", sealed.NewMemoryKeyring("", nil))
	if base == noKeychainKey {
		t.Error("an absent keychain key id produced the same digest as a present one")
	}

	if sealedResetDigest("x", sealed.NewMemoryKeyring("y", make([]byte, 32))) != sealedResetDigest("x", sealed.NewMemoryKeyring("y", make([]byte, 32))) { //nolint:staticcheck // deliberate: same input twice checks the digest is deterministic, not a copy-paste
		t.Error("sealedResetDigest is not deterministic over the same inputs")
	}
}
