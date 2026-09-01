package project

import (
	"github.com/barelyworkingcode/relay/internal/config"
	"strings"
	"testing"
)

// GenerateSkill, AllowCwdAuth, and ShellTemplates aren't parameters of
// CreateWithTokenKind (see project_test.go) — they're applied by
// follow-on mutators inside ApplyCreate, so testing their rejection
// requires the full CreateFields path.
func TestApplyProjectCreate_RemoteRejectsAllowCwdAuth(t *testing.T) {
	s := &config.Settings{Version: 1}
	f := CreateFields{
		Name:         "Agent VM",
		Kind:         config.ProjectKindRemote,
		AllowCwdAuth: true,
	}
	if _, err := ApplyCreate(s, f, nil); err == nil {
		t.Fatal("expected rejection of remote project with allow_cwd_auth")
	}
	if len(s.Projects) != 0 {
		t.Fatalf("rejected create must not persist a project; got %d", len(s.Projects))
	}
}

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

func TestApplyProjectCreate_RemoteRejectsShellTemplates(t *testing.T) {
	s := &config.Settings{Version: 1}
	f := CreateFields{
		Name: "Agent VM",
		Kind: config.ProjectKindRemote,
		ShellTemplates: []config.ShellTemplate{
			{ID: "ssh-1", Name: "SSH box"},
		},
	}
	if _, err := ApplyCreate(s, f, nil); err == nil {
		t.Fatal("expected rejection of remote project with shell templates")
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

// Subtle: the update path re-validates the FINAL shape, not just the touched
// field — and on rejection found=true but the stored project is untouched.
func TestApplyProjectUpdate_RemoteRejectsAllowCwdAuthFlip(t *testing.T) {
	s := &config.Settings{Version: 1}
	created, err := ApplyCreate(s, CreateFields{Name: "Agent VM", Kind: config.ProjectKindRemote}, nil)
	if err != nil {
		t.Fatalf("setup create: %v", err)
	}

	allow := true
	_, found, err := ApplyUpdate(s, created.ID, UpdateFields{AllowCwdAuth: &allow}, func() McpSurfaces { return nil })
	_ = found
	if err == nil {
		t.Fatal("expected rejection of allow_cwd_auth flip on a remote project")
	}
	after, _ := config.FindProjectByID(s, created.ID)
	if after.AllowCwdAuth {
		t.Error("rejected update must not mutate the project")
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
