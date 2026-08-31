package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// The decisive assertion in this file is that settings.json was not REWRITTEN,
// not that its contents are unchanged. A rewrite of identical bytes is still a
// write: it is another chance to lose a concurrent writer's change on a file
// relay writes from several processes (docs/tokens.md), and the operation that
// caused it decided there was nothing to do.

func odwSandbox(t *testing.T) (string, *FileSettingsStore) {
	t.Helper()
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	return dir, store
}

// odwHookStore runs preWrite exactly once, immediately before the wrapped
// store's own declinable write — the point a second process's commit lands
// relative to a caller that read before it. It forwards WithDeclinable rather
// than only With so the wrapped store can still leave the file untouched;
// sorHookStore (service_ops_race_test.go) deliberately does not, which is what
// makes it a stand-in for a store that cannot decline.
type odwHookStore struct {
	*FileSettingsStore
	preWrite func()
}

func (h *odwHookStore) fire() {
	if h.preWrite != nil {
		trigger := h.preWrite
		h.preWrite = nil
		trigger()
	}
}

func (h *odwHookStore) WithDeclinable(fn func(*Settings) error) error {
	h.fire()
	return h.FileSettingsStore.WithDeclinable(fn)
}

// This is deliberate: With is hooked too, even though nothing under test calls
// it. Without it a regression that stopped reaching WithDeclinable would land
// on "the fixture proves nothing" instead of on the assertion this test exists
// for.
func (h *odwHookStore) With(fn func(*Settings)) error {
	h.fire()
	return h.FileSettingsStore.With(fn)
}

type odwSnapshot struct {
	info  os.FileInfo
	bytes []byte
}

func odwSnap(t *testing.T, dir string) odwSnapshot {
	t.Helper()
	return odwSnapshot{info: sdStat(t, dir), bytes: sdRead(t, dir)}
}

func (before odwSnapshot) assertUntouched(t *testing.T, dir, what string) {
	t.Helper()
	after := sdStat(t, dir)
	if !os.SameFile(before.info, after) {
		t.Errorf("%s rewrote settings.json (a new file was renamed over it) after deciding there was nothing to do", what)
	}
	if got := sdRead(t, dir); string(got) != string(before.bytes) {
		t.Errorf("%s changed settings.json:\n before %s\n after  %s", what, before.bytes, got)
	}
}

func odwSeedService(t *testing.T, store SettingsStore) {
	t.Helper()
	if err := store.With(func(s *Settings) {
		s.UpsertService(ServiceConfig{ID: "keeper", DisplayName: "Keeper", Command: "/bin/true"})
	}); err != nil {
		t.Fatalf("seed service: %v", err)
	}
}

func odwSeedMcp(t *testing.T, store SettingsStore) {
	t.Helper()
	if err := store.With(func(s *Settings) {
		s.UpsertExternalMcp(ExternalMcp{ID: "keeper", DisplayName: "Keeper", Command: "/bin/true"})
	}); err != nil {
		t.Fatalf("seed mcp: %v", err)
	}
}

// TestOpsThatFindNothingWriteNothing pins the whole family: each of these
// resolves its record inside the settings write, and a resolution that finds
// nothing must decline the write rather than commit the unchanged settings.
func TestOpsThatFindNothingWriteNothing(t *testing.T) {
	cases := []struct {
		name string
		seed func(t *testing.T, dir string, store *FileSettingsStore)
		run  func(t *testing.T, dir string, store *FileSettingsStore) error
		want error
	}{
		{
			name: "ServiceOps.Update",
			seed: func(t *testing.T, _ string, store *FileSettingsStore) { odwSeedService(t, store) },
			run: func(t *testing.T, _ string, store *FileSettingsStore) error {
				ops := &ServiceOps{Store: store, Registry: &noopServiceManager{}, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
				_, err := ops.Update(context.Background(), "ghost", serviceFields{Command: "/bin/new"}, auditViaIPC, "")
				return err
			},
			want: errServiceNotFound,
		},
		{
			name: "ServiceOps.Remove",
			seed: func(t *testing.T, _ string, store *FileSettingsStore) { odwSeedService(t, store) },
			run: func(t *testing.T, _ string, store *FileSettingsStore) error {
				ops := &ServiceOps{Store: store, Registry: &noopServiceManager{}, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
				return ops.Remove(context.Background(), "ghost", auditViaIPC, "")
			},
			want: errServiceNotFound,
		},
		{
			name: "ServiceOps.SetAutostart",
			seed: func(t *testing.T, _ string, store *FileSettingsStore) { odwSeedService(t, store) },
			run: func(t *testing.T, _ string, store *FileSettingsStore) error {
				ops := &ServiceOps{Store: store, Registry: &noopServiceManager{}}
				return ops.SetAutostart("ghost", true)
			},
			want: errServiceNotFound,
		},
		{
			name: "McpOps.Remove",
			seed: func(t *testing.T, _ string, store *FileSettingsStore) { odwSeedMcp(t, store) },
			run: func(t *testing.T, _ string, store *FileSettingsStore) error {
				ops := &McpOps{Store: store, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
				return ops.Remove(context.Background(), "ghost", auditViaIPC, "")
			},
			want: errMcpNotFound,
		},
		{
			name: "revokePasskey",
			seed: func(t *testing.T, _ string, store *FileSettingsStore) {
				if err := store.With(func(s *Settings) {
					s.Passkeys = append(s.Passkeys, Passkey{ID: "keeper", Name: "Keeper"})
				}); err != nil {
					t.Fatalf("seed passkey: %v", err)
				}
			},
			run: func(t *testing.T, _ string, store *FileSettingsStore) error {
				_, err := revokePasskey(store, "ghost")
				return err
			},
			want: errPasskeyNotFound,
		},
		{
			name: "revokeEnrolment",
			seed: func(t *testing.T, _ string, store *FileSettingsStore) {},
			run: func(t *testing.T, _ string, store *FileSettingsStore) error {
				_, err := revokeEnrolment(store, "ghost")
				return err
			},
			want: errEnrolmentNotFound,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, store := odwSandbox(t)
			tc.seed(t, dir, store)
			before := odwSnap(t, dir)

			err := tc.run(t, dir, store)
			if !errors.Is(err, tc.want) {
				t.Fatalf("%s error = %v, want one matching %v", tc.name, err, tc.want)
			}
			before.assertUntouched(t, dir, tc.name)
		})
	}
}

// updateEnrolment's refusals carry no sentinel, so they are checked on their
// text rather than with errors.Is — the same contract the callers rely on.
func TestUpdateEnrolmentThatFindsNothingWritesNothing(t *testing.T) {
	dir, store := odwSandbox(t)
	before := odwSnap(t, dir)

	window := 60
	_, _, err := updateEnrolment(store, enrolmentUpdateRequest{
		ClientID: "ghost",
		Budget:   enrolmentBudgetUpdate{WindowSeconds: &window},
	})
	if err == nil || !strings.Contains(err.Error(), "no enrolment found with client id") {
		t.Fatalf("updateEnrolment error = %v, want a not-found refusal naming the client id", err)
	}
	before.assertUntouched(t, dir, "updateEnrolment")
}

// A refused enrolment persists nothing — including the settings write itself.
// The record's absence is already pinned by enrolment_test.go; what is pinned
// here is that the refusal did not rewrite the file to say so.
func TestCreateEnrolmentThatIsRefusedWritesNothing(t *testing.T) {
	dir, store := odwSandbox(t)
	local := mkStoreProject(t, store, ProjectKindLocal, "Workspace", t.TempDir())
	before := odwSnap(t, dir)

	_, err := createEnrolment(store, enrolmentRequest{
		ClientID:   "hermes-mail",
		ProjectIDs: []string{local.ID},
	})
	if !errors.Is(err, errEnrolmentInvalid) {
		t.Fatalf("createEnrolment error = %v, want one matching errEnrolmentInvalid", err)
	}
	before.assertUntouched(t, dir, "createEnrolment")
}

// The frontend-token migration reports whether it changed anything, and the
// caller must honour that answer. What reaches the no-change case is the
// ordinary cross-process one — two relay starts, or a start racing a tray,
// migrating the same token — which the hook makes deterministic.
func TestFrontendTokenMigrationThatFindsNothingToDoWritesNothing(t *testing.T) {
	dir, store := odwSandbox(t)
	const token = "odw-frontend-token"

	hooked := &odwHookStore{FileSettingsStore: store}
	var before odwSnapshot
	hooked.preWrite = func() {
		ensureFrontendTokenIsCredential(sealedSettingsStoreAt(dir), token)
		before = odwSnap(t, dir)
	}

	ensureFrontendTokenIsCredential(hooked, token)

	if before.info == nil {
		t.Fatal("the migration never reached its write, so the fixture proves nothing")
	}
	before.assertUntouched(t, dir, "ensureFrontendTokenIsCredential")
	if freshSettings(store).AuthenticateAPICredential(token) == nil {
		t.Fatal("the token does not authenticate after the second migration declined")
	}
}

// StartOAuth resolves the MCP outside the settings lock and persists inside it,
// with network discovery and a browser callback listener in between — the
// widest check-then-act window in the ops cores. StartFlow stands in for that
// ceremony so the removal lands inside it deterministically, with no OAuth
// server involved.
func TestStartOAuthDoesNotResurrectAnMcpRemovedMidCeremony(t *testing.T) {
	dir, store := odwSandbox(t)
	if err := store.With(func(s *Settings) {
		s.UpsertExternalMcp(ExternalMcp{
			ID:          "authy",
			DisplayName: "Authy",
			Transport:   "http",
			URL:         "https://mcp.example.test/",
		})
	}); err != nil {
		t.Fatalf("seed mcp: %v", err)
	}

	var before odwSnapshot
	ops := &McpOps{Store: store, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
	ops.StartFlow = func(mcpURL string, _ func(string)) (*oauthResult, error) {
		if err := (&McpOps{Store: sealedSettingsStoreAt(dir), Gate: ops.Gate, Issuance: ops.Issuance}).Remove(context.Background(), "authy", auditViaIPC, ""); err != nil {
			t.Errorf("concurrent remove: %v", err)
		}
		before = odwSnap(t, dir)
		return &oauthResult{AccessToken: "granted"}, nil
	}

	state, err := ops.StartOAuth(context.Background(), "authy", func(string) {}, auditViaIPC, "")
	if !errors.Is(err, errMcpNotFound) {
		t.Fatalf("StartOAuth error = %v, want one matching errMcpNotFound", err)
	}
	if state != nil {
		t.Fatalf("StartOAuth returned an OAuth state (%+v) for a record that was never persisted", state)
	}
	if before.info == nil {
		t.Fatal("StartFlow never ran, so the fixture proves nothing")
	}
	before.assertUntouched(t, dir, "StartOAuth")
}
