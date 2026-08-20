package main

// Tests for the project-management mutators added in support of the native
// Projects tab: RotateProjectToken, UpdateProjectDisabledTools,
// SetProjectGenerateSkill, plus the cross-cutting SyncProjectToken behavior
// they rely on. All hermetic — see ADR-001 (docs/decisions/001-testing-strategy.md).

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

// newProjectsTestStore stands up a sandboxed SettingsStore with a couple of
// MCPs registered, then returns the store ready for project mutations.
func newProjectsTestStore(t *testing.T) SettingsStore {
	t.Helper()
	_ = mkSandboxRelayHome(t)
	store := NewSettingsStoreAt(mkEmptySandboxRelayHome(t))
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	store.With(func(s *Settings) {
		s.ExternalMcps = []ExternalMcp{
			{ID: "fsmcp", DisplayName: "fsMCP"},
			{ID: "macmcp", DisplayName: "macMCP"},
		}
	})
	return store
}

// fsSchemas returns a schemas map where fsmcp declares allowed_dirs (the
// trigger for filesystem auto-detection in SyncProjectToken).
func fsSchemas() map[string]json.RawMessage {
	return map[string]json.RawMessage{
		"fsmcp":  json.RawMessage(`{"allowed_dirs": {"type": "array"}}`),
		"macmcp": json.RawMessage(`{}`),
	}
}

func createTestProject(t *testing.T, store SettingsStore, name, path string, mcpIDs []string) Project {
	t.Helper()
	var proj Project
	store.With(func(s *Settings) {
		var err error
		proj, err = s.CreateProjectWithToken(name, path, mcpIDs, []string{"*"}, nil, fsSchemas())
		if err != nil {
			t.Fatalf("CreateProjectWithToken: %v", err)
		}
	})
	return proj
}

func TestRotateProjectToken_ReplacesPlaintextAndHash(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := createTestProject(t, store, "Alpha", t.TempDir(), []string{"fsmcp"})
	oldPlain, oldHash := proj.Token, proj.TokenHash

	var newPlain string
	var ok bool
	var rotErr error
	store.With(func(s *Settings) {
		newPlain, ok, rotErr = s.RotateProjectToken(proj.ID)
	})
	if rotErr != nil {
		t.Fatalf("RotateProjectToken: %v", rotErr)
	}
	if !ok {
		t.Fatalf("RotateProjectToken: project not found")
	}
	if newPlain == "" || newPlain == oldPlain {
		t.Fatalf("rotated token unchanged or empty: old=%q new=%q", oldPlain, newPlain)
	}
	after, _ := store.Get().findProjectByID(proj.ID)
	if after.Token != newPlain {
		t.Errorf("stored plaintext = %q; want %q", after.Token, newPlain)
	}
	if after.TokenHash == oldHash {
		t.Errorf("hash unchanged after rotation: %q", oldHash)
	}
	if after.TokenHash != hashToken(newPlain) {
		t.Errorf("stored hash does not match new plaintext")
	}
}

func TestRotateProjectToken_OldTokenRejectedOnNextAuth(t *testing.T) {
	// Security regression: a rotated token must NOT continue to authenticate.
	store := newProjectsTestStore(t)
	proj := createTestProject(t, store, "Alpha", t.TempDir(), []string{"fsmcp"})
	oldPlain := proj.Token

	store.With(func(s *Settings) {
		_, _, _ = s.RotateProjectToken(proj.ID)
	})

	stored, err := store.Get().AuthenticateProject(oldPlain)
	if err == nil {
		t.Fatalf("old token still authenticates: got %+v", stored)
	}
}

func TestRotateProjectToken_UnknownIDReturnsFalse(t *testing.T) {
	store := newProjectsTestStore(t)
	var plain string
	var ok bool
	store.With(func(s *Settings) {
		plain, ok, _ = s.RotateProjectToken("nope")
	})
	if ok || plain != "" {
		t.Fatalf("rotated unknown project: ok=%v plain=%q", ok, plain)
	}
}

func TestUpdateProjectDisabledTools_ReplacesSlice(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := createTestProject(t, store, "Alpha", t.TempDir(), []string{"fsmcp", "macmcp"})

	store.With(func(s *Settings) {
		s.UpdateProjectDisabledTools(proj.ID, "macmcp", []string{"runScript", "openApp"})
	})
	after, _ := store.Get().findProjectByID(proj.ID)
	got := after.DisabledTools["macmcp"]
	want := []string{"runScript", "openApp"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("disabled tools for macmcp = %v; want %v", got, want)
	}
}

func TestUpdateProjectDisabledTools_EmptySliceDeletesKey(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := createTestProject(t, store, "Alpha", t.TempDir(), []string{"fsmcp"})
	// fsmcp creation already disabled fs_bash; confirm it's there first.
	store.With(func(s *Settings) {
		s.UpdateProjectDisabledTools(proj.ID, "fsmcp", nil)
	})
	after, _ := store.Get().findProjectByID(proj.ID)
	if _, present := after.DisabledTools["fsmcp"]; present {
		t.Errorf("expected fsmcp key deleted, got %v", after.DisabledTools)
	}
}

func TestUpdateProjectDisabledTools_RefusesNotInAllowedMcps(t *testing.T) {
	// Security regression: scoping tools for an unallowed MCP would leave a
	// stale list ready to grant unintended access if the MCP is later added.
	store := newProjectsTestStore(t)
	proj := createTestProject(t, store, "Alpha", t.TempDir(), []string{"fsmcp"})

	store.With(func(s *Settings) {
		s.UpdateProjectDisabledTools(proj.ID, "macmcp", []string{"shouldNotPersist"})
	})
	after, _ := store.Get().findProjectByID(proj.ID)
	if _, present := after.DisabledTools["macmcp"]; present {
		t.Fatalf("disabled_tools[macmcp] persisted despite macmcp not in AllowedMcpIDs: %v", after.DisabledTools)
	}
}

func TestUpdateProjectDisabledTools_DeduplicatesAndDropsEmpty(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := createTestProject(t, store, "Alpha", t.TempDir(), []string{"fsmcp"})

	store.With(func(s *Settings) {
		s.UpdateProjectDisabledTools(proj.ID, "fsmcp", []string{"a", "a", "", "b"})
	})
	after, _ := store.Get().findProjectByID(proj.ID)
	got := after.DisabledTools["fsmcp"]
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("dedup/empty-skip wrong: got %v", got)
	}
}

func TestUpdateProjectDisabledTools_WildcardProjectAcceptsAnyMcp(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := createTestProject(t, store, "WildAlpha", t.TempDir(), []string{"*"})

	store.With(func(s *Settings) {
		s.UpdateProjectDisabledTools(proj.ID, "macmcp", []string{"runScript"})
	})
	after, _ := store.Get().findProjectByID(proj.ID)
	if !slices.Contains(after.DisabledTools["macmcp"], "runScript") {
		t.Errorf("wildcard project rejected disabled-tools update for macmcp: %v", after.DisabledTools)
	}
}

func TestSetProjectGenerateSkill_TogglesFlag(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := createTestProject(t, store, "Alpha", t.TempDir(), []string{"fsmcp"})

	store.With(func(s *Settings) {
		s.SetProjectGenerateSkill(proj.ID, true)
	})
	after, _ := store.Get().findProjectByID(proj.ID)
	if !after.GenerateSkill {
		t.Errorf("GenerateSkill not set to true")
	}

	store.With(func(s *Settings) {
		s.SetProjectGenerateSkill(proj.ID, false)
	})
	after, _ = store.Get().findProjectByID(proj.ID)
	if after.GenerateSkill {
		t.Errorf("GenerateSkill not cleared")
	}
}

func TestSyncProjectToken_PreservesUserDisabledToolsAcrossMcpResync(t *testing.T) {
	// Regression check: SyncProjectToken (run on every MCPs/path change) must
	// not blow away a user-set tool-disable list for MCPs that are still
	// allowed. It should only clean entries for MCPs that left the allow list.
	store := newProjectsTestStore(t)
	proj := createTestProject(t, store, "Alpha", t.TempDir(), []string{"fsmcp", "macmcp"})

	store.With(func(s *Settings) {
		s.UpdateProjectDisabledTools(proj.ID, "macmcp", []string{"runScript"})
		// Now drop fsmcp, leave macmcp.
		s.UpdateProjectMcps(proj.ID, []string{"macmcp"}, fsSchemas())
	})
	after, _ := store.Get().findProjectByID(proj.ID)
	if _, present := after.DisabledTools["fsmcp"]; present {
		t.Errorf("fsmcp disabled-tools survived MCP removal: %v", after.DisabledTools)
	}
	if !slices.Contains(after.DisabledTools["macmcp"], "runScript") {
		t.Errorf("macmcp disabled-tools cleared by resync: %v", after.DisabledTools)
	}
}

func TestSyncProjectToken_WildcardPreservesPriorDisabledTools(t *testing.T) {
	// Going from explicit MCP set to wildcard should keep prior disable
	// entries for MCPs that are still registered (wildcard MCP != wildcard
	// tools).
	store := newProjectsTestStore(t)
	proj := createTestProject(t, store, "Alpha", t.TempDir(), []string{"fsmcp", "macmcp"})

	store.With(func(s *Settings) {
		s.UpdateProjectDisabledTools(proj.ID, "macmcp", []string{"runScript"})
		s.UpdateProjectMcps(proj.ID, []string{"*"}, fsSchemas())
	})
	after, _ := store.Get().findProjectByID(proj.ID)
	if !slices.Contains(after.DisabledTools["macmcp"], "runScript") {
		t.Errorf("expected macmcp disabled tools preserved across wildcard switch, got %v", after.DisabledTools)
	}
}

// TestSyncProjectToken_LocalStillGetsAllowedDirsAndFsBashDisabled pins the
// pre-remote behavior for LOCAL projects: this is one of the two existing
// behaviors (see settings.go SyncProjectToken) that the remote defense-in-
// depth guard must not disturb.
func TestSyncProjectToken_LocalStillGetsAllowedDirsAndFsBashDisabled(t *testing.T) {
	store := newProjectsTestStore(t)
	dir := t.TempDir()
	proj := createTestProject(t, store, "Alpha", dir, []string{"fsmcp"})

	after, _ := store.Get().findProjectByID(proj.ID)
	var ctxMap map[string]interface{}
	if err := json.Unmarshal(after.Context["fsmcp"], &ctxMap); err != nil {
		t.Fatalf("expected fsmcp context to be set for a local project: %v", err)
	}
	dirs, _ := ctxMap["allowed_dirs"].([]interface{})
	if len(dirs) != 1 || dirs[0] != dir {
		t.Errorf("expected allowed_dirs=[%q], got %v", dir, ctxMap["allowed_dirs"])
	}
	if !slices.Contains(after.DisabledTools["fsmcp"], "fs_bash") {
		t.Errorf("expected fs_bash auto-disabled for a local project, got %v", after.DisabledTools["fsmcp"])
	}
}

// TestSyncProjectToken_RemoteNeverWritesAllowedDirs proves defence (b): even
// if validation is somehow bypassed, SyncProjectToken itself refuses to
// derive allowed_dirs for a remote project. The Project is constructed
// directly (not through CreateProjectWithTokenKind/ValidateProjectGrants) so
// this test exercises SyncProjectToken's own guard in isolation, independent
// of whether validation would have caught the same grant.
func TestSyncProjectToken_RemoteNeverWritesAllowedDirs(t *testing.T) {
	store := newProjectsTestStore(t)
	var proj Project
	store.With(func(s *Settings) {
		proj = Project{
			ID:            "remote-1",
			Name:          "Bypassed",
			Kind:          ProjectKindRemote,
			Path:          "", // remote projects have no path
			AllowedMcpIDs: []string{"fsmcp"},
			CreatedAt:     "now",
		}
		s.AddProject(proj)
		p, _ := s.findProjectByID(proj.ID)
		s.SyncProjectToken(p, fsSchemas())
	})
	after, _ := store.Get().findProjectByID(proj.ID)
	if after == nil {
		t.Fatal("project not found after SyncProjectToken")
	}
	if _, present := after.Context["fsmcp"]; present {
		t.Errorf("remote project must never get an allowed_dirs context entry, got %v", after.Context["fsmcp"])
	}
}
