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
