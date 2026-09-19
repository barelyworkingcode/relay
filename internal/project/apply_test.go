package project

import (
	"github.com/barelyworkingcode/relay/internal/config"
	"strings"
	"testing"
)

// GenerateSkill and AllowedTemplates aren't parameters of
// CreateWithTokenKind (see project_test.go) — they're applied by
// follow-on mutators inside ApplyCreate, so testing their rejection
// requires the full CreateFields path.
//
// Deliberate: refused rather than silently accepted as an inert toggle —
// regenProjectSkills just skips pathless projects, which would make the flag
// a lie about what it does.
func TestApplyProjectCreate_RemoteRejectsGenerateSkill(t *testing.T) {
	s := &config.Settings{Version: 1}
	f := CreateFields{
		Name:          "Agent VM",
		Kind:          config.ProjectKindRemote,
		GenerateSkill: true,
	}
	if _, err := ApplyCreate(s, f, nil); err == nil {
		t.Fatal("expected rejection of remote project with generate_skill")
	}
	if len(s.Projects) != 0 {
		t.Fatalf("rejected create must not persist a project; got %d", len(s.Projects))
	}
}

func TestApplyProjectCreate_RemoteRejectsAllowedTemplates(t *testing.T) {
	s := &config.Settings{Version: 1}
	f := CreateFields{
		Name:             "Agent VM",
		Kind:             config.ProjectKindRemote,
		AllowedTemplates: []string{"shell"},
	}
	if _, err := ApplyCreate(s, f, nil); err == nil {
		t.Fatal("expected rejection of remote project with allowed templates")
	}
	if len(s.Projects) != 0 {
		t.Fatalf("rejected create must not persist a project; got %d", len(s.Projects))
	}
}

func TestApplyProjectCreate_RemoteRejectsPathScopedGrant(t *testing.T) {
	s := &config.Settings{Version: 1}
	f := CreateFields{
		Name:          "Agent VM",
		Kind:          config.ProjectKindRemote,
		AllowedMcpIDs: []string{"fsmcp"},
	}
	_, err := ApplyCreate(s, f, testSchemas())
	if err == nil {
		t.Fatal("expected rejection of remote project granted a path-scoped MCP")
	}
	if !strings.Contains(err.Error(), "fsmcp") {
		t.Errorf("expected error to name the offending MCP (fsmcp), got: %v", err)
	}
}

func TestApplyProjectCreate_RemoteZeroMcpsSucceeds(t *testing.T) {
	s := &config.Settings{Version: 1}
	f := CreateFields{
		Name: "Agent VM",
		Kind: config.ProjectKindRemote,
	}
	created, err := ApplyCreate(s, f, nil)
	if err != nil {
		t.Fatalf("expected zero-MCP remote project to be created, got: %v", err)
	}
	if !created.IsRemote() {
		t.Errorf("expected created project to be remote, got Kind=%q", created.Kind)
	}
	if len(created.AllowedMcpIDs) != 0 {
		t.Errorf("expected zero allowed MCPs, got %v", created.AllowedMcpIDs)
	}
}

func TestApplyProjectUpdate_RemoteRejectsWildcardMcps(t *testing.T) {
	s := &config.Settings{Version: 1}
	created, err := ApplyCreate(s, CreateFields{Name: "Agent VM", Kind: config.ProjectKindRemote}, nil)
	if err != nil {
		t.Fatalf("setup create: %v", err)
	}

	wildcard := []string{"*"}
	_, _, err = ApplyUpdate(s, created.ID, UpdateFields{AllowedMcpIDs: &wildcard}, func() McpSurfaces { return nil })
	if err == nil {
		t.Fatal("expected rejection of wildcard allowed_mcp_ids on update for a remote project")
	}
	after, _ := config.FindProjectByID(s, created.ID)
	if len(after.AllowedMcpIDs) != 0 {
		t.Errorf("rejected update must not mutate the project, got AllowedMcpIDs=%v", after.AllowedMcpIDs)
	}
}

// TestApplyProjectCreate_MountsIsPersisted pins CreateFields.Mounts actually
// reaching storage: it is applied as a follow-on mutator (like AllowExternal
// and the rest), not a candidate-only field that validates but never lands.
func TestApplyProjectCreate_MountsIsPersisted(t *testing.T) {
	s := &config.Settings{Version: 1}
	mounts := []config.MountGrant{{ID: "src", Path: t.TempDir(), Access: "write"}}
	created, err := ApplyCreate(s, CreateFields{Name: "Agent VM", Kind: config.ProjectKindRemote, Mounts: mounts}, nil)
	if err != nil {
		t.Fatalf("ApplyCreate: %v", err)
	}
	if len(created.Mounts) != 1 || created.Mounts[0].ID != "src" {
		t.Fatalf("expected mounts to be persisted, got %+v", created.Mounts)
	}
	after, _ := config.FindProjectByID(s, created.ID)
	if len(after.Mounts) != 1 {
		t.Fatalf("mounts not found on re-read: %+v", after.Mounts)
	}
}

func TestApplyProjectCreate_LocalRejectsMounts(t *testing.T) {
	s := &config.Settings{Version: 1}
	mounts := []config.MountGrant{{ID: "src", Path: t.TempDir(), Access: "write"}}
	if _, err := ApplyCreate(s, CreateFields{Name: "Local", Path: t.TempDir(), Mounts: mounts}, nil); err == nil {
		t.Fatal("expected rejection of mounts on a kind:local project")
	}
	if len(s.Projects) != 0 {
		t.Fatalf("rejected create must not persist a project; got %d", len(s.Projects))
	}
}

// TestApplyProjectUpdate_MountsIsPersisted is the same pin as the create
// test, for the patch path: UpdateFields.Mounts existed as a decodable
// field before this fix with nothing in ApplyUpdate ever reading it — a
// request setting it silently no-opped. This is the regression test for
// that.
func TestApplyProjectUpdate_MountsIsPersisted(t *testing.T) {
	s := &config.Settings{Version: 1}
	created, err := ApplyCreate(s, CreateFields{Name: "Agent VM", Kind: config.ProjectKindRemote}, nil)
	if err != nil {
		t.Fatalf("setup create: %v", err)
	}
	if len(created.Mounts) != 0 {
		t.Fatalf("expected no mounts at create, got %+v", created.Mounts)
	}

	mounts := []config.MountGrant{{ID: "src", Path: t.TempDir(), Access: "read"}}
	updated, found, err := ApplyUpdate(s, created.ID, UpdateFields{Mounts: &mounts}, func() McpSurfaces { return nil })
	if err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}
	if !found {
		t.Fatal("project not found")
	}
	if len(updated.Mounts) != 1 || updated.Mounts[0].ID != "src" {
		t.Fatalf("expected mounts to be persisted by the update, got %+v", updated.Mounts)
	}
	after, _ := config.FindProjectByID(s, created.ID)
	if len(after.Mounts) != 1 {
		t.Fatalf("mounts not found on re-read: %+v", after.Mounts)
	}
}

func TestValidateShape_AllowedTemplates(t *testing.T) {
	for name, c := range map[string]struct {
		ids []string
		ok  bool
	}{
		"none":            {[]string{}, true},
		"star":            {[]string{"*"}, true},
		"listed":          {[]string{"shell", "pi"}, true},
		"star and others": {[]string{"*", "shell"}, false},
		"blank entry":     {[]string{"shell", " "}, false},
	} {
		p := config.Project{Path: "/tmp/x", AllowedTemplates: c.ids}
		if err := ValidateShape(&p); (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", name, err, c.ok)
		}
	}
}
