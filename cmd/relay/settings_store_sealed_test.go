package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/sealed"
)

// TestResolveSealedStore_FirstRunCreatesKey is §5.5's first row: no key and
// no sealed_key_id is the only condition under which relay may create one.
func TestResolveSealedStore_FirstRunCreatesKey(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	keyring := sealed.NewMemoryKeyring("", nil)

	store, err := config.ResolveSealedStore(dir, keyring)
	if err != nil {
		t.Fatalf("ResolveSealedStore: %v", err)
	}
	if store.Sealer() == nil {
		t.Fatal("a fresh install must resolve to a working sealer")
	}
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	if _, _, err := keyring.Load(); err != nil {
		t.Fatalf("Keyring.Load after first run: %v", err)
	}
	raw := sdRead(t, dir)
	var onDisk map[string]any
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("unmarshal settings.json: %v", err)
	}
	if _, ok := onDisk["sealed_key_id"].(string); !ok {
		t.Errorf("settings.json has no sealed_key_id after first run: %s", raw)
	}
}

// TestResolveSealedStore_MissingKeyDegrades is §5.6 row 1: the keychain
// item is simply gone. Relay must start, serve the read half, and refuse
// every write — and, above all, never create a replacement key (AC-25b,
// AC-25e).
func TestResolveSealedStore_MissingKeyDegrades(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	seedSealedInstall(t, dir, "aaaaaaaaaaaaaaaa")

	keyring := sealed.NewMemoryKeyring("", nil) // the item is gone
	store, err := config.ResolveSealedStore(dir, keyring)
	if err != nil {
		t.Fatalf("ResolveSealedStore returned a fatal error for a degraded case: %v", err)
	}
	if store.Sealer() != nil {
		t.Fatal("a missing key must not resolve to a working sealer")
	}
	status := store.SealStatus()
	if status == nil {
		t.Fatal("SealStatus must name the degraded reason")
	}
	if !strings.Contains(status.Error(), "no such key is in the login keychain") {
		t.Errorf("degraded message = %q, missing the named reason", status.Error())
	}
	assertDegradedStoreBehaviour(t, store, dir)

	// AC-25e: no key was created as a side effect of any of the above.
	if _, _, err := keyring.Load(); err == nil {
		t.Fatal("a replacement key was created for a missing one — this is the single most dangerous failure in the design")
	}
}

// TestResolveSealedStore_KeyMismatchDegrades is §5.6 row 2: a key exists,
// but it is not the one settings.json names.
func TestResolveSealedStore_KeyMismatchDegrades(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	seedSealedInstall(t, dir, "aaaaaaaaaaaaaaaa")

	keyring := sealed.NewMemoryKeyring("bbbbbbbbbbbbbbbb", make([]byte, 32))
	store, err := config.ResolveSealedStore(dir, keyring)
	if err != nil {
		t.Fatalf("ResolveSealedStore: %v", err)
	}
	status := store.SealStatus()
	if status == nil || !strings.Contains(status.Error(), "bound to key bbbbbbbbbbbbbbbb") ||
		!strings.Contains(status.Error(), "expects key aaaaaaaaaaaaaaaa") {
		t.Fatalf("degraded message = %v, want it to name both keys", status)
	}
	assertDegradedStoreBehaviour(t, store, dir)
}

// TestResolveSealedStore_ForeignKeyNeverAdopted is AC-25f: a different
// 32-byte key planted under the same keychain service/account name (here,
// the same memory keyring slot) must never be adopted, never seal or
// unseal anything, and leave settings.json byte-identical.
func TestResolveSealedStore_ForeignKeyNeverAdopted(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	seedSealedInstall(t, dir, "aaaaaaaaaaaaaaaa")
	before := sdRead(t, dir)

	foreignKey := make([]byte, 32)
	for i := range foreignKey {
		foreignKey[i] = byte(i + 1)
	}
	keyring := sealed.NewMemoryKeyring("cccccccccccccccc", foreignKey)
	store, err := config.ResolveSealedStore(dir, keyring)
	if err != nil {
		t.Fatalf("ResolveSealedStore: %v", err)
	}
	if store.Sealer() != nil {
		t.Fatal("a foreign key must never be adopted as a working sealer")
	}
	assertDegradedStoreBehaviour(t, store, dir)

	after := sdRead(t, dir)
	if string(before) != string(after) {
		t.Fatal("settings.json changed after resolving against a foreign key")
	}
	if strings.Contains(string(after), "cccccccccccccccc") {
		t.Fatal("the foreign key id leaked into settings.json")
	}
}

// fakeUnreadableKeyring is an injectable sealed.Keyring standing in for the
// real keychainKeyring's -25308 (errSecInteractionNotAllowed) condition —
// an item is present under relay's service/account, but relay is not on
// its ACL — without needing the real keychain. createCalls lets a test
// assert Create() was never reached, which is §5.5.1's rule stated as a
// fact a test can check rather than only as a comment.
type fakeUnreadableKeyring struct {
	loadErr     error
	createCalls int
}

func (f *fakeUnreadableKeyring) Load() (string, []byte, error) { return "", nil, f.loadErr }

func (f *fakeUnreadableKeyring) Create() (string, []byte, error) {
	f.createCalls++
	return "", nil, fmt.Errorf("fakeUnreadableKeyring: Create must never be called for an unreadable item")
}

func (f *fakeUnreadableKeyring) Destroy() error { return nil }

// unreadableErr mimics what keychain_darwin.go's copyItem actually returns
// for OSStatus -25308: sealed.ErrKeyUnreadable, wrapped with the same shape
// of detail a real caller would see.
func unreadableErr() error {
	return fmt.Errorf("%w: OSStatus -25308 (errSecInteractionNotAllowed) for com.barelyworkingcode.relay/config-seal-key",
		sealed.ErrKeyUnreadable)
}

// TestResolveSealedStore_UnreadableKeyDegrades_DistinctFromMissing is the
// hermetic half of the regression test for the startup-blocking defect:
// settings.json names a key, and the keyring answers with the condition a
// real foreign-ACL keychain item produces (errSecInteractionNotAllowed),
// not ErrKeyMissing. TestResolveSealedStore_MissingKeyDegrades already
// covers "the item is gone"; this covers "the item is there and relay
// cannot use it" — a different operator problem, and the messages must not
// collide. This is what settings_store_sealed_test.go could not previously
// exercise: every existing degraded-state test used ErrKeyMissing, so a
// keyring.Load failure that was NOT ErrKeyMissing fell through
// resolveSealer's default branch and got the "no such key is in the login
// keychain" message anyway — wrong, but nothing here caught it.
func TestResolveSealedStore_UnreadableKeyDegrades_DistinctFromMissing(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	seedSealedInstall(t, dir, "aaaaaaaaaaaaaaaa")
	before := sdRead(t, dir)

	keyring := &fakeUnreadableKeyring{loadErr: unreadableErr()}
	store, err := config.ResolveSealedStore(dir, keyring)
	if err != nil {
		t.Fatalf("ResolveSealedStore returned a fatal error for a degraded case: %v", err)
	}
	if store.Sealer() != nil {
		t.Fatal("an unreadable key must not resolve to a working sealer")
	}
	status := store.SealStatus()
	if status == nil {
		t.Fatal("SealStatus must name the degraded reason")
	}
	if strings.Contains(status.Error(), "no such key is in the login keychain") {
		t.Errorf("degraded message = %q, reads as simply absent — an unreadable item is a different condition", status.Error())
	}
	if !strings.Contains(status.Error(), "refused permission to read it") {
		t.Errorf("degraded message = %q, does not name the item as present-but-unreadable", status.Error())
	}
	assertDegradedStoreBehaviour(t, store, dir)

	if keyring.createCalls != 0 {
		t.Fatal("Create was called for an unreadable key — this is §5.5.1's rule broken")
	}
	after := sdRead(t, dir)
	if string(before) != string(after) {
		t.Fatal("settings.json changed after resolving against an unreadable key")
	}
}

// TestResolveSealedStore_FirstRunUnreadableItemDegrades_NeverCreates covers
// §5.5's other branch: no sealed_key_id yet (a genuine first run, or a
// pre-sealing settings.json), but a foreign, unreadable item already
// occupies relay's keychain slot. Before this fix, any keyring.Load error
// other than ErrKeyMissing on this branch made resolveSealer return a
// fatal err, which runTrayApp turns into os.Exit(1) — not a hang, but
// still a violation of "relay starts, it does not exit" for a condition
// that is, by §5.5.1's own rule, supposed to degrade like every other
// missing/mismatched/foreign/unreadable key.
func TestResolveSealedStore_FirstRunUnreadableItemDegrades_NeverCreates(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)

	keyring := &fakeUnreadableKeyring{loadErr: unreadableErr()}
	store, err := config.ResolveSealedStore(dir, keyring)
	if err != nil {
		t.Fatalf("ResolveSealedStore returned a fatal error instead of degrading: %v", err)
	}
	if store.Sealer() != nil {
		t.Fatal("an unreadable item on first run must not resolve to a working sealer")
	}
	status := store.SealStatus()
	if status == nil {
		t.Fatal("SealStatus must name the degraded reason")
	}
	if !strings.Contains(status.Error(), "refused permission to read it") {
		t.Errorf("degraded message = %q, does not name the item as present-but-unreadable", status.Error())
	}
	if keyring.createCalls != 0 {
		t.Fatal("Create was called over an item relay could not read — this is §5.5.1's rule broken")
	}
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized must not fail on a degraded first-run store: %v", err)
	}
	if store.Get() == nil {
		t.Fatal("a degraded first-run store must still serve Get()")
	}
}

// seedSealedInstall writes a minimal, already-sealed settings.json naming
// keyID, using its own throwaway sealer — standing in for "a machine that
// was fully initialised under a key this test's keyring does not hold."
func seedSealedInstall(t *testing.T, dir, keyID string) {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = 0x11
	}
	sealer, err := sealed.NewAESSealer(keyID, key)
	if err != nil {
		t.Fatalf("NewAESSealer: %v", err)
	}
	store := config.NewSettingsStoreSealed(dir, sealer)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("seedSealedInstall: EnsureInitialized: %v", err)
	}
}

// assertDegradedStoreBehaviour is §5.6's behavioural contract, shared by
// every degraded-state test: relay started (the caller already has a
// store), the read half works, and every write refuses leaving the file
// untouched.
func assertDegradedStoreBehaviour(t *testing.T, store *config.FileSettingsStore, dir string) {
	t.Helper()
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized must not fail on a degraded store: %v", err)
	}
	if store.Get() == nil {
		t.Fatal("a degraded store must still serve Get()")
	}

	before := sdRead(t, dir)
	beforeInfo := sdStat(t, dir)
	err := store.With(func(s *config.Settings) { s.AdminSecret = config.NewSecret("attempted-write") })
	if err == nil {
		t.Fatal("a degraded store accepted a write")
	}
	if !strings.Contains(err.Error(), "sealed store is unavailable") {
		t.Errorf("write refusal = %q, want it to name the degraded state", err)
	}
	after := sdRead(t, dir)
	afterInfo := sdStat(t, dir)
	if string(before) != string(after) {
		t.Error("a degraded store's refused write changed settings.json")
	}
	if !os.SameFile(beforeInfo, afterInfo) {
		t.Error("a degraded store's refused write replaced settings.json")
	}
}

// TestFileSettingsStore_CLIShapeRefusesEveryWrite is AC-14b: a store
// constructed the way every CLI command constructs one (no sealer, no
// degraded reason) refuses every write with errSealerRequired and leaves
// the file untouched — a second, structural guarantee behind brokering
// that holds even if a future change reintroduced a direct call.
func TestFileSettingsStore_CLIShapeRefusesEveryWrite(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	seedSealedInstall(t, dir, "aaaaaaaaaaaaaaaa")
	before := sdRead(t, dir)

	store := config.NewSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("a CLI-shaped store must still initialise for reads: %v", err)
	}
	if got := store.Get(); got == nil {
		t.Fatal("a CLI-shaped store must still serve reads")
	}

	err := store.With(func(s *config.Settings) { s.AdminSecret = config.NewSecret("x") })
	if err == nil {
		t.Fatal("a CLI-shaped store accepted a write")
	}
	if err != config.ErrSealerRequired {
		t.Errorf("error = %v, want errSealerRequired exactly", err)
	}
	after := sdRead(t, dir)
	if string(before) != string(after) {
		t.Error("a refused CLI write changed settings.json")
	}
}

// TestNoPlaintextReachesDiskIncludingStagingFile is AC-9/AC-10. It has two
// halves.
//
// The first is a real failure injected between CreateTemp and Rename:
// chflags uchg marks settings.json immutable, so reads and CreateTemp (a
// different, new file in the same directory) still succeed, but the final
// os.Rename onto the immutable file is refused by the kernel — the exact
// window a crash could otherwise land in. atomicWriteFile removes its
// staging file on every one of its own error returns, including this one
// (it is not modified to do anything else — the residue rule is satisfied
// by sealing before serialisation, not by adding cleanup machinery here),
// so this half's assertion is that the failure is clean: no leftover
// staging file, settings.json unchanged, and — checked anyway, since a
// bug here would be exactly this test's reason to exist — no plaintext
// anywhere the failure could have put it.
//
// The second half is what actually makes a hard kill (a real crash, which
// no error return or cleanup code runs for) safe: save() calls
// sealAllSecrets before json.MarshalIndent, so the bytes passed into
// atomicWriteFile — and therefore anything a kill could leave sitting in
// the staging file — are already fully sealed before that function is
// ever invoked. TestSealAllSecrets_RoundTripsThroughOpen pins that those
// bytes contain no plaintext; this test pins that save() really does call
// sealAllSecrets first by using the same sentinels end to end.
func TestNoPlaintextReachesDiskIncludingStagingFile(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}

	const sentinelToken = "sentinel-project-token-do-not-leak"
	const sentinelAdmin = "sentinel-admin-secret-do-not-leak"
	const sentinelEnv = "sentinel-env-value-do-not-leak"
	assertNoErr(t, store.With(func(s *config.Settings) {
		s.AdminSecret = config.NewSecret(sentinelAdmin)
		hash := config.HashToken(sentinelToken)
		s.Projects = append(s.Projects, config.Project{ID: "p1", Token: config.NewSecret(sentinelToken), TokenHash: hash})
		s.ExternalMcps = append(s.ExternalMcps, config.ExternalMcp{
			ID: "mcp1", Env: map[string]config.Secret{"KEY": config.NewSecret(sentinelEnv)},
		})
	}), "seed sentinels")

	path := filepath.Join(dir, "settings.json")
	assertNoErr(t, exec.Command("chflags", "uchg", path).Run(), "chflags uchg")
	t.Cleanup(func() { _ = exec.Command("chflags", "nouchg", path).Run() })

	writeErr := store.With(func(s *config.Settings) { s.AdminSecret = config.NewSecret("second-write") })
	if writeErr == nil {
		t.Fatal("With succeeded despite settings.json being immutable — the fixture proves nothing")
	}
	if !strings.Contains(writeErr.Error(), "rename") {
		t.Fatalf("write failed for a reason other than the induced rename failure: %v", writeErr)
	}

	entries, err := os.ReadDir(dir)
	assertNoErr(t, err, "read config dir")
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "settings.json.") || !strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		t.Errorf("atomicWriteFile left a staging file behind after a failed rename: %s", e.Name())
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		assertNoErr(t, err, "read staging file %s", e.Name())
		for _, sentinel := range []string{sentinelToken, sentinelAdmin, sentinelEnv, "second-write"} {
			if strings.Contains(string(data), sentinel) {
				t.Errorf("staging file %s contains plaintext %q", e.Name(), sentinel)
			}
		}
	}

	// The real settings.json is untouched and still holds only what the
	// first, successful write sealed.
	final := sdRead(t, dir)
	for _, sentinel := range []string{sentinelToken, sentinelAdmin, sentinelEnv} {
		if strings.Contains(string(final), sentinel) {
			t.Errorf("settings.json contains plaintext %q", sentinel)
		}
	}
	if strings.Contains(string(final), "second-write") {
		t.Error("the refused write's payload reached settings.json anyway")
	}
}

// TestDegradedStore_CorruptFieldUnderTheCorrectKey is §5.6's third and
// fourth rows: the key matches (unlike the earlier degraded-state tests),
// but one field's envelope has been altered. The read half still works in
// full for every OTHER field — the clear ones, and any sealed field that
// still opens — while the corrupt field is closed and any write refuses,
// because sealAllSecrets has no plaintext to reseal it from.
func TestDegradedStore_CorruptFieldUnderTheCorrectKey(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	sealer := testSealer()
	store := config.NewSettingsStoreSealed(dir, sealer)
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")
	assertNoErr(t, store.With(func(s *config.Settings) {
		hash := config.HashToken("real-token")
		s.Projects = append(s.Projects, config.Project{ID: "p1", Name: "P1", Token: config.NewSecret("real-token"), TokenHash: hash})
	}), "seed a project")

	// Flip a byte in the project's sealed token ciphertext, on disk,
	// leaving everything else (including admin_secret's own envelope)
	// alone.
	raw := sdRead(t, dir)
	var doc map[string]json.RawMessage
	assertNoErr(t, json.Unmarshal(raw, &doc), "unmarshal settings.json")
	var projects []map[string]json.RawMessage
	assertNoErr(t, json.Unmarshal(doc["projects"], &projects), "unmarshal projects")
	var tokenEnv map[string]string
	assertNoErr(t, json.Unmarshal(projects[0]["token"], &tokenEnv), "unmarshal token envelope")
	tokenEnv["ct"] = corruptBase64(t, tokenEnv["ct"])
	corrupted, err := json.Marshal(tokenEnv)
	assertNoErr(t, err, "remarshal corrupted envelope")
	projects[0]["token"] = corrupted
	newProjects, err := json.Marshal(projects)
	assertNoErr(t, err, "remarshal projects")
	doc["projects"] = newProjects
	newDoc, err := json.Marshal(doc)
	assertNoErr(t, err, "remarshal settings.json")
	assertNoErr(t, os.WriteFile(filepath.Join(dir, "settings.json"), newDoc, 0600), "write corrupted settings.json")

	reloaded := config.NewSettingsStoreSealed(dir, sealer)
	got := reloaded.Get()
	if pt, ok := got.AdminSecret.Reveal(); !ok || pt == "" {
		t.Error("an unrelated sealed field (admin_secret) must still open under the correct key")
	}
	if got.Projects[0].Name != "P1" {
		t.Error("a clear field on the same record must still read correctly")
	}
	if _, ok := got.Projects[0].Token.Reveal(); ok {
		t.Fatal("the corrupted field opened anyway — the fixture proves nothing")
	}
	if status := reloaded.SealStatus(); status == nil {
		t.Error("SealStatus must name the corrupt field")
	}

	before := sdRead(t, dir)
	writeErr := reloaded.With(func(s *config.Settings) {
		s.Projects[0].Name = "renamed"
	})
	if writeErr == nil {
		t.Fatal("a write succeeded despite an unopenable field — it would have to silently drop or fabricate the corrupt token")
	}
	after := sdRead(t, dir)
	if string(before) != string(after) {
		t.Error("a refused write over a corrupt field changed settings.json")
	}
}

// corruptBase64 flips the first byte of the base64-decoded value of s and
// re-encodes it, producing a same-length ciphertext that fails AEAD
// authentication rather than base64 decoding.
func corruptBase64(t *testing.T, s string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(s)
	assertNoErr(t, err, "decode base64")
	if len(raw) == 0 {
		t.Fatal("nothing to corrupt")
	}
	raw[0] ^= 0xFF
	return base64.StdEncoding.EncodeToString(raw)
}
