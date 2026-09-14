package main

// Freshness of the settings store, from both ends.
//
// READ side: what an authorization decision resolves against when
// settings.json is in some state other than "a good file we already loaded".
// Every one of those states must fail CLOSED, deletion included — deleting
// settings.json is the move an operator reaches for to lock the control plane
// out, so it is the one state that may not keep a loaded credential alive in
// memory. Driven through the real composed stack (NewFrontendServer + a real
// credentialAuthorizer + real listeners) rather than against the store alone,
// because the claim is about what the API answers, not about what a method
// returns.
//
// WRITE side: settings.json has more than one writer. `relay credential mint`,
// `relay enrol create` and `relay service register` each run in their own
// process, and a tray that applied a mutation to a cache predating one of
// those writes would erase it. Two FileSettingsStore values over one directory
// is the only way to express that in-process: they have independent caches, so
// the first can learn about the second's write only by re-reading the file.

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/enrolment"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ssfBumpModTime pushes settings.json's modtime distinctly into the future.
//
// This is subtle: a stat is the ONLY signal the store has that the file it
// last read is not the file on disk, so a state change that leaves the modtime
// alone is invisible to it by construction. Writes move it on their own and
// this only defends against filesystem timestamp granularity; a chmod does
// not, and for that state this call is what makes the change observable at
// all. See docs/tokens.md for what that costs.
func ssfBumpModTime(t *testing.T, path string) {
	t.Helper()
	future := time.Now().Add(2 * time.Second)
	assertNoErr(t, os.Chtimes(path, future, future), "chtimes %s", path)
}

// ssfStack is the composed read-side fixture: a store over a real config dir,
// a real frontend server with a real credentialAuthorizer over that same
// store, one minted read-class credential, and the seeded read+configure+proxy bearer.
type ssfStack struct {
	dir       string
	store     *config.FileSettingsStore
	srv       *accServer
	readToken string
}

func ssfNewStack(t *testing.T) *ssfStack {
	t.Helper()
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")

	srv := accNewServer(t, store, accLegacyToken)
	_, plaintext, err := mintAPICredential(store, credentialMintRequest{
		Name:    "ssf-reader",
		Classes: []string{"read"},
	})
	assertNoErr(t, err, "mint the read credential")

	return &ssfStack{dir: dir, store: store, srv: srv, readToken: plaintext}
}

func (k *ssfStack) settingsPath() string { return filepath.Join(k.dir, "settings.json") }

// probes runs the three requests the reviewer's probe ran, over the same
// server and the same already-connected clients.
func (k *ssfStack) probes() []struct {
	name  string
	token string
	tcp   bool
} {
	return []struct {
		name  string
		token string
		tcp   bool
	}{
		{"socket/read", k.readToken, false},
		{"socket/legacy", accLegacyToken, false},
		{"tcp/read", k.readToken, true},
	}
}

func (k *ssfStack) status(t *testing.T, token string, overTCP bool) int {
	t.Helper()
	if overTCP {
		resp, _ := k.srv.tcp(t, "GET", "/api/services", token, nil)
		return resp.StatusCode
	}
	resp, _ := k.srv.socket(t, "GET", "/api/services", token, nil)
	return resp.StatusCode
}

func (k *ssfStack) assertAllReach(t *testing.T) {
	t.Helper()
	for _, p := range k.probes() {
		if got := k.status(t, p.token, p.tcp); got == http.StatusUnauthorized {
			t.Fatalf("%s: status = 401 before the state under test was applied; the fixture proves nothing", p.name)
		}
	}
}

func (k *ssfStack) assertAllRefused(t *testing.T, state string) {
	t.Helper()
	for _, p := range k.probes() {
		if got := k.status(t, p.token, p.tcp); got != http.StatusUnauthorized {
			t.Errorf("%s with %s: status = %d, want 401 — settings in this state must fail closed", p.name, state, got)
		}
	}
}

// TestSSFEveryDegradedSettingsStateFailsClosed pins all five runtime states
// against one server and one set of in-flight clients. Deletion is the state
// this test exists for: every other one already resolved to empty settings
// because load() answers a read or parse failure with defaults, while a
// deleted file left the last-loaded cache authenticating indefinitely.
func TestSSFEveryDegradedSettingsStateFailsClosed(t *testing.T) {
	states := []struct {
		name  string
		apply func(t *testing.T, k *ssfStack)
	}{
		{"credentials emptied in file", func(t *testing.T, k *ssfStack) {
			raw, err := os.ReadFile(k.settingsPath())
			assertNoErr(t, err, "read settings")
			var s config.Settings
			assertNoErr(t, json.Unmarshal(raw, &s), "unmarshal settings")
			s.APICredentials = nil
			out, err := json.MarshalIndent(&s, "", "  ")
			assertNoErr(t, err, "marshal settings")
			assertNoErr(t, os.WriteFile(k.settingsPath(), out, 0600), "write settings")
			ssfBumpModTime(t, k.settingsPath())
		}},
		{"settings.json corrupt", func(t *testing.T, k *ssfStack) {
			assertNoErr(t, os.WriteFile(k.settingsPath(), []byte(`{"projects": [ NOT JSON`), 0600), "write corrupt settings")
			ssfBumpModTime(t, k.settingsPath())
		}},
		{"settings.json truncated to zero", func(t *testing.T, k *ssfStack) {
			assertNoErr(t, os.WriteFile(k.settingsPath(), nil, 0600), "truncate settings")
			ssfBumpModTime(t, k.settingsPath())
		}},
		{"settings.json unreadable (000)", func(t *testing.T, k *ssfStack) {
			assertNoErr(t, os.Chmod(k.settingsPath(), 0000), "chmod settings")
			t.Cleanup(func() { _ = os.Chmod(k.settingsPath(), 0600) })
			ssfBumpModTime(t, k.settingsPath())
		}},
		{"settings.json deleted", func(t *testing.T, k *ssfStack) {
			assertNoErr(t, os.Remove(k.settingsPath()), "remove settings")
		}},
	}

	for _, state := range states {
		t.Run(state.name, func(t *testing.T) {
			k := ssfNewStack(t)
			k.assertAllReach(t)
			state.apply(t, k)
			k.assertAllRefused(t, state.name)
		})
	}
}

// A deleted settings.json revokes an enrolment for the same reason it revokes
// a credential: both are records in a file that is no longer there.
func TestSSFDeletingSettingsRevokesAnEnrolment(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")

	proj := mkStoreProject(t, store, config.ProjectKindRemote, "ssf-remote", "")
	assertNoErr(t, store.With(func(s *config.Settings) {
		// Appended directly: internal/enrolment's own mutator is unexported
		// there, and what this fixture needs is a stored record, not the
		// create path.
		s.Enrolments = append(s.Enrolments, config.Enrolment{ClientID: "ssf-client", Fingerprint: "ssf-fp", ProjectIDs: []string{proj.ID}})
	}), "add enrolment")

	if enrolment.FindByFingerprint(config.FreshSettings(store), "ssf-fp") == nil {
		t.Fatal("the enrolment is not resolvable before the deletion; the fixture proves nothing")
	}

	assertNoErr(t, os.Remove(filepath.Join(dir, "settings.json")), "remove settings")

	if enrolment.FindByFingerprint(config.FreshSettings(store), "ssf-fp") != nil {
		t.Fatal("the enrolment still resolves after settings.json was deleted; a certificate the file no longer grants must stop being enrolled")
	}
}

// TestSSFFirstStartWithNoSettingsFileWorks is the other side of the deletion
// rule: "not created yet" must not be read as "deleted". A fresh install has
// no settings.json until something writes one, and the tray must still come
// up and create it.
func TestSSFFirstStartWithNoSettingsFileWorks(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	path := filepath.Join(dir, "settings.json")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("fixture is not a fresh install: stat = %v", err)
	}

	store := sealedSettingsStoreAt(dir)
	if got := store.ReloadIfChanged(); got != nil {
		t.Fatalf("a settings file that was never created is not a change, got %+v", got)
	}
	if got := config.FreshSettings(store); got == nil {
		t.Fatal("freshSettings returned nil on a fresh install")
	}

	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized on a fresh install")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("EnsureInitialized did not create settings.json: %v", err)
	}
	if pt, ok := store.Get().AdminSecret.Reveal(); !ok || pt == "" {
		t.Fatal("EnsureInitialized did not generate an admin secret")
	}

	assertNoErr(t, store.With(func(s *config.Settings) {
		s.UpsertService(config.ServiceConfig{ID: "ssf-first", DisplayName: "ssf-first", Command: "/bin/true"})
	}), "first mutation after a fresh start")
	if _, idx := config.FindServiceByID(sealedSettingsStoreAt(dir).Get(), "ssf-first"); idx < 0 {
		t.Fatal("the first mutation after a fresh start did not reach disk")
	}
}

// A store can legitimately hold settings that nothing has written out yet —
// that is what a first start is before EnsureInitialized runs, and what the
// router test fixtures are throughout. With must persist those, not mistake
// the absent file for a deletion and empty them.
func TestSSFWithPersistsSettingsThatWereNeverOnDisk(t *testing.T) {
	dir := mkShortTempDir(t, "ssf-nofile-")
	store := storeWithCache(dir, testSealer(), &config.Settings{
		Version:     config.CurrentSettingsVersion,
		Projects:    []config.Project{{ID: "ssf-p", Name: "ssf", Path: dir}},
		AdminSecret: config.NewSecret("ssf-secret"),
	})

	assertNoErr(t, store.With(func(s *config.Settings) {
		if len(s.Projects) == 0 {
			t.Fatal("With emptied settings that were never written to disk; a file that was never created is not a deleted file")
		}
		s.Projects[0].AllowCwdAuth = true
	}), "With over a store whose settings are not on disk yet")

	proj, _ := config.FindProjectByID(sealedSettingsStoreAt(dir).Get(), "ssf-p")
	if proj == nil {
		t.Fatal("the project never reached disk")
	}
	if !proj.AllowCwdAuth {
		t.Fatal("the mutation never reached disk")
	}
}

// ---------------------------------------------------------------------------
// Write side: two stores, one directory
// ---------------------------------------------------------------------------

// TestSSFCrossProcessWriteSurvivesTheNextWith is the write-side counterpart of
// TestACCCredentialMintedByASeparateProcessAuthenticatesImmediately. That one
// proves the tray READS a CLI's record; this one proves the tray's next write
// does not destroy it. The credential is the worst case — its plaintext is
// printed once and is unrecoverable, so an operator whose token was erased is
// left holding something that 401s with nothing saying why — and the service
// stands in for every other record type on the same path.
func TestSSFCrossProcessWriteSurvivesTheNextWith(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	trayStore := sealedSettingsStoreAt(dir)
	assertNoErr(t, trayStore.EnsureInitialized(), "EnsureInitialized (tray)")
	srv := accNewServer(t, trayStore, accLegacyToken)

	// Sealed, like the tray: a CLI-shaped store (no sealer) now refuses
	// every write by design (§5.4), so a second WRITING process here is
	// modeled the same way the tray itself is until brokering (S6) lands.
	cliStore := sealedSettingsStoreAt(dir)
	assertNoErr(t, cliStore.EnsureInitialized(), "EnsureInitialized (cli)")

	_, plaintext, err := mintAPICredential(cliStore, credentialMintRequest{Name: "ssf-cli", Classes: []string{"read"}})
	assertNoErr(t, err, "mint from the CLI process")
	assertNoErr(t, cliStore.With(func(s *config.Settings) {
		s.UpsertService(config.ServiceConfig{ID: "ssf-cli-svc", DisplayName: "ssf-cli-svc", Command: "/bin/true"})
	}), "register a service from the CLI process")

	// The tray's own next mutation, made from a cache that predates both.
	assertNoErr(t, trayStore.With(func(s *config.Settings) {
		s.UpsertService(config.ServiceConfig{ID: "ssf-tray-svc", DisplayName: "ssf-tray-svc", Command: "/bin/true"})
	}), "the tray's next write")

	onDisk := sealedSettingsStoreAt(dir).Get()
	if authenticateAPICredential(onDisk, plaintext) == nil {
		t.Error("the credential the CLI minted is gone from settings.json after the tray's next write; its plaintext was printed once and cannot be reissued")
	}
	if _, idx := config.FindServiceByID(onDisk, "ssf-cli-svc"); idx < 0 {
		t.Error("the service the CLI registered is gone from settings.json after the tray's next write")
	}
	if _, idx := config.FindServiceByID(onDisk, "ssf-tray-svc"); idx < 0 {
		t.Error("the tray's own write did not land")
	}

	resp, body := srv.socket(t, "GET", "/api/services", plaintext, nil)
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("the credential minted by the CLI no longer authenticates after the tray's next write; body=%s", body)
	}
}

// The reload inside With must not cost the caller its own edit: a record this
// store legitimately mutates still has to be written correctly on top of
// whatever the reload brought in.
func TestSSFOwnMutationIsWrittenCorrectlyAfterAReload(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	trayStore := sealedSettingsStoreAt(dir)
	assertNoErr(t, trayStore.EnsureInitialized(), "EnsureInitialized (tray)")

	proj := mkStoreProject(t, trayStore, config.ProjectKindLocal, "ssf-owned", t.TempDir())

	// Sealed, like the tray: a CLI-shaped store (no sealer) now refuses
	// every write by design (§5.4), so a second WRITING process here is
	// modeled the same way the tray itself is until brokering (S6) lands.
	cliStore := sealedSettingsStoreAt(dir)
	assertNoErr(t, cliStore.EnsureInitialized(), "EnsureInitialized (cli)")
	assertNoErr(t, cliStore.With(func(s *config.Settings) {
		s.UpsertExternalMcp(config.ExternalMcp{ID: "ssf-cli-mcp", DisplayName: "ssf-cli-mcp", Command: "/bin/true"})
	}), "register an MCP from the CLI process")

	// The callback sees the post-reload state, so a record the caller read
	// before calling With is still the record the callback finds.
	var seen bool
	assertNoErr(t, trayStore.With(func(s *config.Settings) {
		p, _ := config.FindProjectByID(s, proj.ID)
		if p == nil {
			return
		}
		seen = true
		p.AllowCwdAuth = true
	}), "the tray's update to its own project")
	if !seen {
		t.Fatal("the project this store created was not visible to its own With callback after the reload")
	}

	onDisk := sealedSettingsStoreAt(dir).Get()
	updated, _ := config.FindProjectByID(onDisk, proj.ID)
	if updated == nil {
		t.Fatal("the project is gone from settings.json")
	}
	if !updated.AllowCwdAuth {
		t.Error("the tray's update to its own project did not reach disk")
	}
	if updated.TokenHash != proj.TokenHash {
		t.Error("the project's token hash changed; the reload replaced the record rather than the callback mutating it")
	}
	if _, idx := config.FindExternalMcpByID(onDisk, "ssf-cli-mcp"); idx < 0 {
		t.Error("the MCP the CLI registered was clobbered by the tray's update")
	}
}

// ---------------------------------------------------------------------------
// A file that exists and cannot be used is UNKNOWN, not empty
// ---------------------------------------------------------------------------

// ssfDegradation is one way settings.json can exist and be unusable, paired
// with the operator act that makes it usable again.
//
// Deletion is deliberately not one of them. An absent file is *empty*
// settings — a state relay knows the contents of and may legitimately write
// over, which is what makes a fresh install work.
type ssfDegradation struct {
	name   string
	apply  func(t *testing.T, path string)
	repair func(t *testing.T, path string, good []byte)
}

func ssfDegradations() []ssfDegradation {
	writeBack := func(t *testing.T, path string, good []byte) {
		t.Helper()
		assertNoErr(t, os.WriteFile(path, good, 0600), "restore settings")
		ssfBumpModTime(t, path)
	}
	return []ssfDegradation{
		{
			name: "corrupt",
			apply: func(t *testing.T, path string) {
				t.Helper()
				assertNoErr(t, os.WriteFile(path, []byte(`{ this is not json`), 0600), "write corrupt settings")
				ssfBumpModTime(t, path)
			},
			repair: writeBack,
		},
		{
			name: "truncated to zero",
			apply: func(t *testing.T, path string) {
				t.Helper()
				assertNoErr(t, os.WriteFile(path, nil, 0600), "truncate settings")
				ssfBumpModTime(t, path)
			},
			repair: writeBack,
		},
		{
			name: "unreadable (000)",
			apply: func(t *testing.T, path string) {
				t.Helper()
				assertNoErr(t, os.Chmod(path, 0000), "chmod settings")
				t.Cleanup(func() { _ = os.Chmod(path, 0600) })
				ssfBumpModTime(t, path)
			},
			// This is subtle: the repair moves no timestamp, deliberately. A
			// chmod back is the whole transient-failure case — a store that
			// trusted the modtime here would keep refusing writes after the
			// file became readable, with nothing left to move it.
			repair: func(t *testing.T, path string, _ []byte) {
				t.Helper()
				assertNoErr(t, os.Chmod(path, 0600), "chmod settings back")
			},
		},
	}
}

// ssfRawSettings reads settings.json whatever its mode, restoring the mode
// afterwards, so a test can compare the bytes of a file it just made
// unreadable.
func ssfRawSettings(t *testing.T, path string) []byte {
	t.Helper()
	info, err := os.Stat(path)
	assertNoErr(t, err, "stat %s", path)
	if info.Mode().Perm()&0400 == 0 {
		assertNoErr(t, os.Chmod(path, 0600), "chmod %s readable", path)
		defer func() { _ = os.Chmod(path, info.Mode().Perm()) }()
	}
	raw, err := os.ReadFile(path)
	assertNoErr(t, err, "read %s", path)
	return raw
}

// TestSSFMutationOverAnUnusableFileRefusesAndLeavesItAlone is the write half of
// the rule the five-state test pins for reads. Both halves start from the same
// fact — a file relay could not read resolves to empty settings — and they must
// draw opposite conclusions from it: a read may treat unknown as empty and fail
// closed, while a write that treats unknown as empty serializes defaults plus
// its own one change over every project, token hash, credential and enrolment
// on the host, and returns nil.
func TestSSFMutationOverAnUnusableFileRefusesAndLeavesItAlone(t *testing.T) {
	for _, d := range ssfDegradations() {
		t.Run(d.name, func(t *testing.T) {
			k := ssfNewStack(t)
			path := k.settingsPath()
			proj := mkStoreProject(t, k.store, config.ProjectKindLocal, "ssf-keep-me", t.TempDir())
			k.assertAllReach(t)

			d.apply(t, path)
			before := ssfRawSettings(t, path)

			err := k.store.With(func(s *config.Settings) {
				s.UpsertService(config.ServiceConfig{ID: "ssf-new", DisplayName: "ssf-new", Command: "/bin/true"})
			})
			if err == nil {
				t.Error("With reported success over a settings.json it could not read; the caller has no way to learn its change was applied to defaults")
			}
			if !errors.Is(err, config.ErrSettingsUnreadable) {
				t.Errorf("With error = %v, want one matching errSettingsUnreadable so a caller can tell a refusal from a save that failed", err)
			}

			after := ssfRawSettings(t, path)
			if !bytes.Equal(before, after) {
				t.Fatalf("settings.json was rewritten by a mutation that could not be based on it\nbefore: %q\nafter:  %q", before, after)
			}
			var wrote config.Settings
			if json.Unmarshal(after, &wrote) == nil {
				if _, idx := config.FindServiceByID(&wrote, "ssf-new"); idx >= 0 {
					t.Error("the refused mutation reached disk: settings.json now holds defaults plus that one change")
				}
				if p, _ := config.FindProjectByID(&wrote, proj.ID); p == nil {
					t.Error("the project is gone from settings.json after a mutation the store refused to make")
				}
			}

			// The refusal must not have rehabilitated the cache: a file relay
			// cannot read still has nothing to authenticate against.
			k.assertAllRefused(t, d.name)
		})
	}
}

// TestSSFStoreRecoversOnceTheFileIsUsableAgain: an EACCES or a hand-edit that
// did not parse is a moment, not a verdict. Nothing may latch the store into
// refusing once the file is readable and valid again — for the chmod case
// there is no later write to un-latch it with.
func TestSSFStoreRecoversOnceTheFileIsUsableAgain(t *testing.T) {
	for _, d := range ssfDegradations() {
		t.Run(d.name, func(t *testing.T) {
			k := ssfNewStack(t)
			path := k.settingsPath()
			proj := mkStoreProject(t, k.store, config.ProjectKindLocal, "ssf-keep-me", t.TempDir())
			good := ssfRawSettings(t, path)

			d.apply(t, path)
			if err := k.store.With(func(s *config.Settings) {}); err == nil {
				t.Fatal("With did not refuse while the file was unusable; the fixture proves nothing")
			}

			d.repair(t, path, good)

			assertNoErr(t, k.store.With(func(s *config.Settings) {
				s.UpsertService(config.ServiceConfig{ID: "ssf-recovered", DisplayName: "ssf-recovered", Command: "/bin/true"})
			}), "mutation once settings.json was usable again")

			onDisk := sealedSettingsStoreAt(k.dir).Get()
			if _, idx := config.FindServiceByID(onDisk, "ssf-recovered"); idx < 0 {
				t.Error("the mutation made after recovery did not reach disk")
			}
			if p, _ := config.FindProjectByID(onDisk, proj.ID); p == nil {
				t.Error("the project did not survive the round trip through the unusable state")
			}
			if authenticateAPICredential(onDisk, k.readToken) == nil {
				t.Error("the credential did not survive the round trip through the unusable state")
			}
			for _, p := range k.probes() {
				if got := k.status(t, p.token, p.tcp); got == http.StatusUnauthorized {
					t.Errorf("%s: still 401 after settings.json became usable again", p.name)
				}
			}
		})
	}
}

// TestSSFEnsureInitializedRefusesAnUnusableFile pins the tray-start decision.
// EnsureInitialized exists to create a settings file that is not there; a file
// that is there and cannot be read is not that case, and writing one over it is
// a total wipe repeated at every launch. It refuses instead, and runTrayApp
// exits on the error — see the method's own reasoning for why refusing beats
// coming up with the empty settings the read produced.
func TestSSFEnsureInitializedRefusesAnUnusableFile(t *testing.T) {
	for _, d := range ssfDegradations() {
		t.Run(d.name, func(t *testing.T) {
			dir := mkEmptySandboxRelayHome(t)
			path := filepath.Join(dir, "settings.json")
			seed := sealedSettingsStoreAt(dir)
			assertNoErr(t, seed.EnsureInitialized(), "EnsureInitialized (seed)")
			assertNoErr(t, seed.With(func(s *config.Settings) {
				s.UpsertService(config.ServiceConfig{ID: "ssf-keep-me", DisplayName: "ssf-keep-me", Command: "/bin/true"})
			}), "seed a record worth losing")

			d.apply(t, path)
			before := ssfRawSettings(t, path)

			// A fresh process, as the next tray start is.
			err := sealedSettingsStoreAt(dir).EnsureInitialized()
			if err == nil {
				t.Error("EnsureInitialized succeeded over a settings.json it could not read; at tray start that is a silent total wipe on every launch")
			}
			if !errors.Is(err, config.ErrSettingsUnreadable) {
				t.Errorf("EnsureInitialized error = %v, want one matching errSettingsUnreadable", err)
			}
			if after := ssfRawSettings(t, path); !bytes.Equal(before, after) {
				t.Fatalf("EnsureInitialized rewrote a settings.json it could not read\nbefore: %q\nafter:  %q", before, after)
			}
		})
	}

	// The contrast, and the case EnsureInitialized exists for: the refusal must
	// not fire on a file that is merely absent.
	t.Run("absent file is still created", func(t *testing.T) {
		dir := mkEmptySandboxRelayHome(t)
		store := sealedSettingsStoreAt(dir)
		assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized on a fresh install")
		if _, err := os.Stat(filepath.Join(dir, "settings.json")); err != nil {
			t.Fatalf("EnsureInitialized did not create settings.json: %v", err)
		}
	})
}
