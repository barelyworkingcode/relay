package project

import (
	"encoding/json"
	"github.com/barelyworkingcode/relay/internal/config"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func newProjectsTestStore(t *testing.T) config.SettingsStore {
	t.Helper()
	store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	store.With(func(s *config.Settings) {
		s.ExternalMcps = []config.ExternalMcp{
			{ID: "fsmcp", DisplayName: "fsMCP"},
			{ID: "macmcp", DisplayName: "macMCP"},
		}
	})
	return store
}

// fsSchemas declares fsmcp's allowed_dirs field, the trigger for filesystem
// auto-detection in SyncProjectToken.
func fsSchemas() McpSurfaces {
	return McpSurfaces{
		"fsmcp":  {Schema: json.RawMessage(`{"allowed_dirs": {"type": "array"}}`)},
		"macmcp": {Schema: json.RawMessage(`{}`)},
	}
}

func createTestProject(t *testing.T, store config.SettingsStore, name, path string, mcpIDs []string) config.Project {
	t.Helper()
	var proj config.Project
	store.With(func(s *config.Settings) {
		var err error
		proj, err = CreateWithToken(s, name, path, mcpIDs, []string{"*"}, nil, fsSchemas())
		if err != nil {
			t.Fatalf("CreateProjectWithToken: %v", err)
		}
	})
	return proj
}

func TestRotateProjectToken_ReplacesPlaintextAndHash(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := createTestProject(t, store, "Alpha", t.TempDir(), []string{"fsmcp"})
	oldPlain, _ := proj.Token.Reveal()
	oldHash := proj.TokenHash

	var newPlain string
	var ok bool
	var rotErr error
	store.With(func(s *config.Settings) {
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
	after, _ := config.FindProjectByID(store.Get(), proj.ID)
	afterPlain, _ := after.Token.Reveal()
	if afterPlain != newPlain {
		t.Errorf("stored plaintext = %q; want %q", afterPlain, newPlain)
	}
	if after.TokenHash == oldHash {
		t.Errorf("hash unchanged after rotation: %q", oldHash)
	}
	if after.TokenHash != config.HashToken(newPlain) {
		t.Errorf("stored hash does not match new plaintext")
	}
}

func TestRotateProjectToken_OldTokenRejectedOnNextAuth(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := createTestProject(t, store, "Alpha", t.TempDir(), []string{"fsmcp"})
	oldPlain, _ := proj.Token.Reveal()

	store.With(func(s *config.Settings) {
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
	store.With(func(s *config.Settings) {
		plain, ok, _ = s.RotateProjectToken("nope")
	})
	if ok || plain != "" {
		t.Fatalf("rotated unknown project: ok=%v plain=%q", ok, plain)
	}
}

func TestUpdateProjectDisabledTools_ReplacesSlice(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := createTestProject(t, store, "Alpha", t.TempDir(), []string{"fsmcp", "macmcp"})

	store.With(func(s *config.Settings) {
		s.UpdateProjectDisabledTools(proj.ID, "macmcp", []string{"runScript", "openApp"})
	})
	after, _ := config.FindProjectByID(store.Get(), proj.ID)
	got := after.DisabledTools["macmcp"]
	want := []string{"runScript", "openApp"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("disabled tools for macmcp = %v; want %v", got, want)
	}
}

func TestUpdateProjectDisabledTools_EmptySliceDeletesKey(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := createTestProject(t, store, "Alpha", t.TempDir(), []string{"fsmcp"})
	// Creating an fsmcp project already auto-disables fs_bash, so DisabledTools
	// is non-empty before this call.
	store.With(func(s *config.Settings) {
		s.UpdateProjectDisabledTools(proj.ID, "fsmcp", nil)
	})
	after, _ := config.FindProjectByID(store.Get(), proj.ID)
	if _, present := after.DisabledTools["fsmcp"]; present {
		t.Errorf("expected fsmcp key deleted, got %v", after.DisabledTools)
	}
}

func TestUpdateProjectDisabledTools_RefusesNotInAllowedMcps(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := createTestProject(t, store, "Alpha", t.TempDir(), []string{"fsmcp"})

	store.With(func(s *config.Settings) {
		s.UpdateProjectDisabledTools(proj.ID, "macmcp", []string{"shouldNotPersist"})
	})
	after, _ := config.FindProjectByID(store.Get(), proj.ID)
	if _, present := after.DisabledTools["macmcp"]; present {
		t.Fatalf("disabled_tools[macmcp] persisted despite macmcp not in AllowedMcpIDs: %v", after.DisabledTools)
	}
}

func TestUpdateProjectDisabledTools_DeduplicatesAndDropsEmpty(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := createTestProject(t, store, "Alpha", t.TempDir(), []string{"fsmcp"})

	store.With(func(s *config.Settings) {
		s.UpdateProjectDisabledTools(proj.ID, "fsmcp", []string{"a", "a", "", "b"})
	})
	after, _ := config.FindProjectByID(store.Get(), proj.ID)
	got := after.DisabledTools["fsmcp"]
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("dedup/empty-skip wrong: got %v", got)
	}
}

func TestUpdateProjectDisabledTools_WildcardProjectAcceptsAnyMcp(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := createTestProject(t, store, "WildAlpha", t.TempDir(), []string{"*"})

	store.With(func(s *config.Settings) {
		s.UpdateProjectDisabledTools(proj.ID, "macmcp", []string{"runScript"})
	})
	after, _ := config.FindProjectByID(store.Get(), proj.ID)
	if !slices.Contains(after.DisabledTools["macmcp"], "runScript") {
		t.Errorf("wildcard project rejected disabled-tools update for macmcp: %v", after.DisabledTools)
	}
}

func TestSetProjectGenerateSkill_TogglesFlag(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := createTestProject(t, store, "Alpha", t.TempDir(), []string{"fsmcp"})

	store.With(func(s *config.Settings) {
		s.SetProjectGenerateSkill(proj.ID, true)
	})
	after, _ := config.FindProjectByID(store.Get(), proj.ID)
	if !after.GenerateSkill {
		t.Errorf("GenerateSkill not set to true")
	}

	store.With(func(s *config.Settings) {
		s.SetProjectGenerateSkill(proj.ID, false)
	})
	after, _ = config.FindProjectByID(store.Get(), proj.ID)
	if after.GenerateSkill {
		t.Errorf("GenerateSkill not cleared")
	}
}

func TestSyncProjectToken_PreservesUserDisabledToolsAcrossMcpResync(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := createTestProject(t, store, "Alpha", t.TempDir(), []string{"fsmcp", "macmcp"})

	store.With(func(s *config.Settings) {
		s.UpdateProjectDisabledTools(proj.ID, "macmcp", []string{"runScript"})
		updateProjectMcps(s, proj.ID, []string{"macmcp"}, fsSchemas())
	})
	after, _ := config.FindProjectByID(store.Get(), proj.ID)
	if _, present := after.DisabledTools["fsmcp"]; present {
		t.Errorf("fsmcp disabled-tools survived MCP removal: %v", after.DisabledTools)
	}
	if !slices.Contains(after.DisabledTools["macmcp"], "runScript") {
		t.Errorf("macmcp disabled-tools cleared by resync: %v", after.DisabledTools)
	}
}

// Wildcard MCP membership and wildcard tool access are different things:
// going from an explicit MCP set to wildcard must keep prior disable entries
// for MCPs that are still registered.
func TestSyncProjectToken_WildcardPreservesPriorDisabledTools(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := createTestProject(t, store, "Alpha", t.TempDir(), []string{"fsmcp", "macmcp"})

	store.With(func(s *config.Settings) {
		s.UpdateProjectDisabledTools(proj.ID, "macmcp", []string{"runScript"})
		updateProjectMcps(s, proj.ID, []string{"*"}, fsSchemas())
	})
	after, _ := config.FindProjectByID(store.Get(), proj.ID)
	if !slices.Contains(after.DisabledTools["macmcp"], "runScript") {
		t.Errorf("expected macmcp disabled tools preserved across wildcard switch, got %v", after.DisabledTools)
	}
}

func TestSyncProjectToken_LocalStillGetsAllowedDirsAndFsBashDisabled(t *testing.T) {
	store := newProjectsTestStore(t)
	dir := t.TempDir()
	proj := createTestProject(t, store, "Alpha", dir, []string{"fsmcp"})

	after, _ := config.FindProjectByID(store.Get(), proj.ID)
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

// The Project is constructed directly rather than through
// CreateWithTokenKind/ValidateGrants, so this test exercises
// SyncProjectToken's own guard in isolation — independent of whether
// validation would have caught the same grant.
func TestSyncProjectToken_RemoteNeverWritesAllowedDirs(t *testing.T) {
	store := newProjectsTestStore(t)
	var proj config.Project
	store.With(func(s *config.Settings) {
		proj = config.Project{
			ID:            "remote-1",
			Name:          "Bypassed",
			Kind:          config.ProjectKindRemote,
			Path:          "", // remote projects have no path
			AllowedMcpIDs: []string{"fsmcp"},
			CreatedAt:     "now",
		}
		s.AddProject(proj)
		p, _ := config.FindProjectByID(s, proj.ID)
		syncProjectToken(s, p, fsSchemas())
	})
	after, _ := config.FindProjectByID(store.Get(), proj.ID)
	if after == nil {
		t.Fatal("project not found after SyncProjectToken")
	}
	if _, present := after.Context["fsmcp"]; present {
		t.Errorf("remote project must never get an allowed_dirs context entry, got %v", after.Context["fsmcp"])
	}
}

func TestProjectConvertRemoteToLocal_RefusedWhileEnrolled(t *testing.T) {
	s := &config.Settings{}
	mail, err := CreateWithTokenKind(s, config.ProjectKindRemote, "Mail", "", []string{}, []string{}, nil, nil)
	assertNoErr(t, err, "create remote project")
	// Seeded directly rather than through internal/enrolment's own
	// mutator, which is unexported there: what these two tests are about is
	// the project mutator's refusal, and the enrolment is setup for it.
	s.Enrolments = append(s.Enrolments, config.Enrolment{
		ClientID:    "hermes-mail",
		Fingerprint: "sha256:" + strings.Repeat("a", 64),
		ProjectIDs:  []string{mail.ID},
	})
	schemas := func() McpSurfaces { return nil }

	local := config.ProjectKindLocal
	path := t.TempDir()
	_, _, err = ApplyUpdate(s, mail.ID, UpdateFields{Kind: &local, Path: &path}, schemas)
	if err == nil {
		t.Fatal("converting a remote project to local must be refused while an enrolment grants it")
	}
	if !strings.Contains(err.Error(), "hermes-mail") {
		t.Fatalf("refusal must name the offending enrolment, got: %v", err)
	}

	// The refusal must have changed nothing.
	after, _ := config.FindProjectByID(s, mail.ID)
	if !after.IsRemote() || after.Path != "" {
		t.Fatalf("refused conversion mutated the project: kind=%q path=%q", after.Kind, after.Path)
	}

	// Revoking the enrolment makes the conversion legal — capability and
	// device revocation stay independent, and neither strands the other.
	s.Enrolments = nil
	if _, _, err := ApplyUpdate(s, mail.ID, UpdateFields{Kind: &local, Path: &path}, schemas); err != nil {
		t.Fatalf("conversion should be legal once no enrolment grants the project: %v", err)
	}
	converted, _ := config.FindProjectByID(s, mail.ID)
	if converted.IsRemote() {
		t.Fatal("project did not convert to local after the enrolment was revoked")
	}
}

// Belt-and-braces, in the shape updateProjectPath already uses: the mutator
// refuses the same conversion on its own, so a caller inside this package
// that skips ApplyUpdate cannot produce the silent widening.
func TestUpdateProjectKind_RefusesRemoteToLocalWhileEnrolled(t *testing.T) {
	s := &config.Settings{}
	mail, err := CreateWithTokenKind(s, config.ProjectKindRemote, "Mail", "", []string{}, []string{}, nil, nil)
	assertNoErr(t, err, "create remote project")
	s.Enrolments = append(s.Enrolments, config.Enrolment{
		ClientID:    "hermes-mail",
		Fingerprint: "sha256:" + strings.Repeat("b", 64),
		ProjectIDs:  []string{mail.ID},
	})

	updateProjectKind(s, mail.ID, config.ProjectKindLocal)
	if proj, _ := config.FindProjectByID(s, mail.ID); !proj.IsRemote() {
		t.Fatal("UpdateProjectKind converted an enrolled remote project to local")
	}

	// Unrelated projects, and remote→remote no-ops, stay unaffected.
	other, err := CreateWithTokenKind(s, config.ProjectKindRemote, "Calendar", "", []string{}, []string{}, nil, nil)
	assertNoErr(t, err, "create second remote project")
	updateProjectKind(s, other.ID, config.ProjectKindLocal)
	if proj, _ := config.FindProjectByID(s, other.ID); proj.IsRemote() {
		t.Fatal("an unenrolled remote project must still be convertible")
	}
}
