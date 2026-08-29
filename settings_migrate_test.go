package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"relaygo/sealed"
)

// TestMigration_SealsARealDirtyConfigDir runs EnsureInitialized against the
// hermetic fixture at test/fixtures/relay-home — a settings.json with a
// legacy plaintext admin_secret and three projects, each with a plaintext
// token whose sha256 matches its own token_hash, exactly the shape §4.7
// describes and the invariant §4.2 requires. After migration: every
// project's token still authenticates, admin_secret is sealed, and
// sealed_key_id names the key that sealed it.
func TestMigration_SealsARealDirtyConfigDir(t *testing.T) {
	dir := mkSandboxRelayHome(t)
	rawBefore := sdRead(t, dir)
	var before Settings
	assertNoErr(t, json.Unmarshal(rawBefore, &before), "unmarshal fixture")
	if len(before.Projects) == 0 {
		t.Fatal("fixture has no projects; this test proves nothing")
	}
	plaintexts := make([]string, 0, len(before.Projects)+1)
	adminPT, ok := before.AdminSecret.Reveal()
	if !ok || adminPT == "" {
		t.Fatal("fixture's admin_secret is not legacy plaintext; this test proves nothing")
	}
	plaintexts = append(plaintexts, adminPT)
	for _, p := range before.Projects {
		pt, ok := p.Token.Reveal()
		if !ok || pt == "" {
			t.Fatalf("fixture project %s has no legacy plaintext token; this test proves nothing", p.ID)
		}
		if hashToken(pt) != p.TokenHash {
			t.Fatalf("fixture project %s: sha256(token) != token_hash; fix the fixture, not this test", p.ID)
		}
		plaintexts = append(plaintexts, pt)
	}

	keyring := sealed.NewMemoryKeyring("", nil)
	store, err := ResolveSealedStore(dir, keyring)
	assertNoErr(t, err, "ResolveSealedStore")
	if store.Sealer() == nil {
		t.Fatal("a dirty-but-unsealed fixture with no key yet must resolve to a working (freshly created) sealer")
	}
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized (migration)")

	rawAfter := sdRead(t, dir)
	for _, pt := range plaintexts {
		if strings.Contains(string(rawAfter), pt) {
			t.Errorf("settings.json still contains plaintext %q after migration", pt)
		}
	}
	var after map[string]any
	assertNoErr(t, json.Unmarshal(rawAfter, &after), "unmarshal migrated settings.json")
	if _, ok := after["sealed_key_id"].(string); !ok {
		t.Errorf("migrated settings.json has no sealed_key_id: %s", rawAfter)
	}

	reloaded := store.Get()
	for i, p := range before.Projects {
		pt, ok := reloaded.Projects[i].Token.Reveal()
		if !ok {
			t.Fatalf("migrated project %s token could not be opened", p.ID)
		}
		origPT, _ := p.Token.Reveal()
		if pt != origPT {
			t.Errorf("project %s token changed across migration: got %q, want %q", p.ID, pt, origPT)
		}
		if _, err := reloaded.AuthenticateProject(pt); err != nil {
			t.Errorf("project %s token no longer authenticates after migration: %v", p.ID, err)
		}
	}
}

// TestMigration_RefusesOnTokenHashMismatch is AC-6a: a project whose
// plaintext does not hash to its own token_hash means the file is not what
// relay thinks it is. Migration must refuse the whole thing and write
// nothing, naming the project.
func TestMigration_RefusesOnTokenHashMismatch(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	body := `{"version":1,"admin_secret":"admin-plain","projects":[` +
		`{"id":"proj-bad","name":"Bad","token":"actual-token","token_hash":"` + hashToken("different-token") + `"}` +
		`],"external_mcps":[],"services":[]}`
	path := filepath.Join(dir, "settings.json")
	assertNoErr(t, os.WriteFile(path, []byte(body), 0600), "seed dirty settings.json")

	keyring := sealed.NewMemoryKeyring("", nil)
	store, err := ResolveSealedStore(dir, keyring)
	assertNoErr(t, err, "ResolveSealedStore")

	err = store.EnsureInitialized()
	if err == nil {
		t.Fatal("EnsureInitialized succeeded despite a token/token_hash mismatch")
	}
	if !strings.Contains(err.Error(), "proj-bad") {
		t.Errorf("refusal does not name the offending project: %v", err)
	}

	after, readErr := os.ReadFile(path)
	assertNoErr(t, readErr, "read settings.json")
	if string(after) != body {
		t.Error("migration wrote something despite refusing — settings.json must be untouched")
	}
	// §4.7 step 1 creates the key before step 2's hash check runs — a key
	// existing afterwards is expected, not a leak; what must never happen
	// is settings.json reflecting a half-finished migration under it.
}

// TestMigration_NamesStaleCopiesAndDeletesNothing is AC-10b: a stale
// plaintext copy beside settings.json is named, not touched. Deleting a
// file relay did not create is not something migration may do, however
// obviously stale it looks (§4.7, §11.3).
func TestMigration_NamesStaleCopiesAndDeletesNothing(t *testing.T) {
	dir := mkSandboxRelayHome(t)
	stalePath := filepath.Join(dir, "settings.json.bak-fsreview")
	staleBody := sdRead(t, dir) // byte-identical copy of the still-legacy fixture
	assertNoErr(t, os.WriteFile(stalePath, staleBody, 0600), "seed stale copy")

	keyring := sealed.NewMemoryKeyring("", nil)
	store, err := ResolveSealedStore(dir, keyring)
	assertNoErr(t, err, "ResolveSealedStore")

	stdout := captureStdout(t, func() {
		assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized (migration)")
	})

	if !strings.Contains(stdout, "settings.json.bak-fsreview") {
		t.Errorf("migration did not name the stale copy on stdout: %s", stdout)
	}
	if !strings.Contains(stdout, "PLAINTEXT") {
		t.Errorf("migration's stale-copy warning does not say PLAINTEXT: %s", stdout)
	}

	after, readErr := os.ReadFile(stalePath)
	assertNoErr(t, readErr, "read stale copy after migration")
	if string(after) != string(staleBody) {
		t.Error("migration modified a stale copy it did not create")
	}
}

// TestMigration_CAKeyIsFoldedIntoSealedFile is §4.7 step 4 / §5.7: a real
// plaintext ca.key (and its ca.crt) found at LoadOrCreateCA time is sealed
// into ca.key.sealed, the plaintext ca.key is removed, and the resulting
// CA still signs certificates that verify against the same ca.crt — a
// migrated CA is not a new one.
func TestMigration_CAKeyIsFoldedIntoSealedFile(t *testing.T) {
	// Produce a real plaintext ca.key/ca.crt pair the way a pre-ADR-017
	// install would have one: generate a CA the normal (sealed) way, then
	// unseal the key back to plaintext and write it out by hand, deleting
	// the sealed form. This is the legacy shape LoadOrCreateCA must migrate.
	dir := mkEmptySandboxRelayHome(t)
	sealer := testSealer()
	ca, err := LoadOrCreateCA(sealer) // generates sealed from the start
	assertNoErr(t, err, "generate CA")
	origFingerprint := ca.CertPEM()

	sealedData, err := os.ReadFile(filepath.Join(dir, caKeySealedFile))
	assertNoErr(t, err, "read ca.key.sealed")
	var env sealed.Envelope
	assertNoErr(t, json.Unmarshal(sealedData, &env), "parse ca.key.sealed")
	keyPEM, err := sealer.Unseal(env, []byte(caAADPrefix+"ca.key"))
	assertNoErr(t, err, "unseal ca.key.sealed")

	// Roll back to the legacy, pre-migration shape.
	assertNoErr(t, os.Remove(filepath.Join(dir, caKeySealedFile)), "remove ca.key.sealed")
	assertNoErr(t, os.WriteFile(filepath.Join(dir, "ca.key"), keyPEM, 0600), "write legacy plaintext ca.key")

	migrated, err := LoadOrCreateCA(sealer)
	assertNoErr(t, err, "LoadOrCreateCA (migrate)")

	if _, statErr := os.Stat(filepath.Join(dir, "ca.key")); !os.IsNotExist(statErr) {
		t.Error("plaintext ca.key survived migration")
	}
	if _, statErr := os.Stat(filepath.Join(dir, caKeySealedFile)); statErr != nil {
		t.Error("ca.key.sealed was not written by migration")
	}
	if string(migrated.CertPEM()) != string(origFingerprint) {
		t.Error("migration produced a different CA certificate — every enrolment it signed is now orphaned")
	}
	_, _, fp, err := migrated.IssueClientCert("post-migration-client")
	assertNoErr(t, err, "issue a client cert from the migrated CA")
	if fp == "" {
		t.Error("migrated CA could not issue a usable client certificate")
	}
}

// captureStdout redirects os.Stdout for the duration of fn and returns
// what was printed — migration's stale-copy notice is deliberately on
// stdout as well as slog (§4.7's example text), since an operator running
// the tray by hand should see it without digging through the log file.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	saved := os.Stdout
	r, w, err := os.Pipe()
	assertNoErr(t, err, "os.Pipe")
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		out, _ := io.ReadAll(r)
		done <- string(out)
	}()
	func() {
		defer func() {
			os.Stdout = saved
			w.Close()
		}()
		fn()
	}()
	return <-done
}
