package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Tests for applyProjectCreate / applyProjectUpdate's remote-project
// validation. GenerateSkill, AllowCwdAuth, and ShellTemplates aren't
// parameters of CreateProjectWithTokenKind (project_test.go covers that
// function directly) — they're applied by follow-on mutators inside
// applyProjectCreate, so exercising their rejection requires going through
// the full projectCreateFields path tested here. All hermetic: these use a
// bare in-memory *Settings, never the real config dir (mirrors project_test.go).

// TestApplyProjectCreate_RemoteRejectsAllowCwdAuth proves the full create
// path (not just CreateProjectWithTokenKind) refuses allow_cwd_auth on a
// remote project, and that nothing is left behind on rejection.
func TestApplyProjectCreate_RemoteRejectsAllowCwdAuth(t *testing.T) {
	s := &Settings{Version: 1}
	f := projectCreateFields{
		Name:         "Agent VM",
		Kind:         ProjectKindRemote,
		AllowCwdAuth: true,
	}
	if _, err := applyProjectCreate(s, f, nil); err == nil {
		t.Fatal("expected rejection of remote project with allow_cwd_auth")
	}
	if len(s.Projects) != 0 {
		t.Fatalf("rejected create must not persist a project; got %d", len(s.Projects))
	}
}

// TestApplyProjectCreate_RemoteRejectsGenerateSkill proves generate_skill is
// refused rather than silently accepted as an inert toggle — today
// regenProjectSkills just skips pathless projects, which would make the flag
// a lie about what it does.
func TestApplyProjectCreate_RemoteRejectsGenerateSkill(t *testing.T) {
	s := &Settings{Version: 1}
	f := projectCreateFields{
		Name:          "Agent VM",
		Kind:          ProjectKindRemote,
		GenerateSkill: true,
	}
	if _, err := applyProjectCreate(s, f, nil); err == nil {
		t.Fatal("expected rejection of remote project with generate_skill")
	}
	if len(s.Projects) != 0 {
		t.Fatalf("rejected create must not persist a project; got %d", len(s.Projects))
	}
}

// TestApplyProjectCreate_RemoteRejectsShellTemplates proves shell templates
// (which launch a host terminal) are refused on a project with no host.
func TestApplyProjectCreate_RemoteRejectsShellTemplates(t *testing.T) {
	s := &Settings{Version: 1}
	f := projectCreateFields{
		Name: "Agent VM",
		Kind: ProjectKindRemote,
		ShellTemplates: []ShellTemplate{
			{ID: "ssh-1", Name: "SSH box"},
		},
	}
	if _, err := applyProjectCreate(s, f, nil); err == nil {
		t.Fatal("expected rejection of remote project with shell templates")
	}
	if len(s.Projects) != 0 {
		t.Fatalf("rejected create must not persist a project; got %d", len(s.Projects))
	}
}

// TestApplyProjectCreate_RemoteRejectsPathScopedGrant proves the full create
// path also enforces ValidateProjectGrants (not just CreateProjectWithTokenKind
// directly), naming the offending MCP.
func TestApplyProjectCreate_RemoteRejectsPathScopedGrant(t *testing.T) {
	s := &Settings{Version: 1}
	f := projectCreateFields{
		Name:          "Agent VM",
		Kind:          ProjectKindRemote,
		AllowedMcpIDs: []string{"fsmcp"},
	}
	_, err := applyProjectCreate(s, f, testSchemas())
	if err == nil {
		t.Fatal("expected rejection of remote project granted a path-scoped MCP")
	}
	if !strings.Contains(err.Error(), "fsmcp") {
		t.Errorf("expected error to name the offending MCP (fsmcp), got: %v", err)
	}
}

// TestApplyProjectCreate_RemoteZeroMcpsSucceeds proves the full create path
// accepts a remote project enrolled with zero grants.
func TestApplyProjectCreate_RemoteZeroMcpsSucceeds(t *testing.T) {
	s := &Settings{Version: 1}
	f := projectCreateFields{
		Name: "Agent VM",
		Kind: ProjectKindRemote,
	}
	created, err := applyProjectCreate(s, f, nil)
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

// TestApplyProjectUpdate_RemoteRejectsAllowCwdAuthFlip proves the update path
// re-validates the FINAL shape, not just the touched field: flipping
// AllowCwdAuth on an already-remote project must be refused, and — critically
// — nothing about the project may change when it is (found=true, but the
// stored project is untouched).
func TestApplyProjectUpdate_RemoteRejectsAllowCwdAuthFlip(t *testing.T) {
	s := &Settings{Version: 1}
	created, err := applyProjectCreate(s, projectCreateFields{Name: "Agent VM", Kind: ProjectKindRemote}, nil)
	if err != nil {
		t.Fatalf("setup create: %v", err)
	}

	allow := true
	_, found, err := applyProjectUpdate(s, created.ID, projectUpdateFields{AllowCwdAuth: &allow}, func() map[string]json.RawMessage { return nil })
	_ = found
	if err == nil {
		t.Fatal("expected rejection of allow_cwd_auth flip on a remote project")
	}
	after, _ := s.findProjectByID(created.ID)
	if after.AllowCwdAuth {
		t.Error("rejected update must not mutate the project")
	}
}

// TestApplyProjectUpdate_RemoteRejectsWildcardMcps proves the update path
// re-checks the wildcard rule when allowed_mcp_ids changes.
func TestApplyProjectUpdate_RemoteRejectsWildcardMcps(t *testing.T) {
	s := &Settings{Version: 1}
	created, err := applyProjectCreate(s, projectCreateFields{Name: "Agent VM", Kind: ProjectKindRemote}, nil)
	if err != nil {
		t.Fatalf("setup create: %v", err)
	}

	wildcard := []string{"*"}
	_, _, err = applyProjectUpdate(s, created.ID, projectUpdateFields{AllowedMcpIDs: &wildcard}, func() map[string]json.RawMessage { return nil })
	if err == nil {
		t.Fatal("expected rejection of wildcard allowed_mcp_ids on update for a remote project")
	}
	after, _ := s.findProjectByID(created.ID)
	if len(after.AllowedMcpIDs) != 0 {
		t.Errorf("rejected update must not mutate the project, got AllowedMcpIDs=%v", after.AllowedMcpIDs)
	}
}
