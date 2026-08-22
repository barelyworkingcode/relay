package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Converting an existing LOCAL project to remote is the sharp edge: the project
// already carries allowed_dirs context from its former life, and that context is
// what relay injects into _meta on every tool call. If a conversion could keep
// it, a remote client would inherit host filesystem scope — precisely the thing
// the remote kind exists to prevent.
func TestProjectConvertLocalToRemote_CannotInheritFilesystemScope(t *testing.T) {
	dir := t.TempDir()
	s := &Settings{ExternalMcps: []ExternalMcp{{ID: "fsmcp", DisplayName: "fsMCP"}}}
	schemas := func() McpSurfaces {
		return McpSurfaces{"fsmcp": {Schema: json.RawMessage(`{"allowed_dirs":{"type":"array"}}`)}}
	}

	proj, err := s.CreateProjectWithToken("Local", dir, []string{"fsmcp"}, nil, nil, schemas())
	if err != nil {
		t.Fatalf("create local: %v", err)
	}
	stored, _ := s.findProjectByID(proj.ID)
	if len(stored.Context["fsmcp"]) == 0 {
		t.Fatalf("precondition: local project should have fsmcp allowed_dirs context, got %+v", stored.Context)
	}

	// Conversion attempt 1: flip kind, clear path, keep the filesystem grant.
	remote := ProjectKindRemote
	empty := ""
	_, _, err = applyProjectUpdate(s, proj.ID, projectUpdateFields{Kind: &remote, Path: &empty}, schemas)
	if err == nil {
		t.Fatal("converting a project holding a filesystem-scoped MCP to remote must be refused")
	}

	// The refusal must have changed nothing.
	after, _ := s.findProjectByID(proj.ID)
	if after.IsRemote() || after.Path != dir {
		t.Fatalf("refused conversion mutated the project: kind=%q path=%q", after.Kind, after.Path)
	}

	// Conversion attempt 2: drop the filesystem grant in the same request.
	// This is legal, and the stale allowed_dirs context must not survive it.
	none := []string{}
	if _, _, err := applyProjectUpdate(s, proj.ID, projectUpdateFields{
		Kind: &remote, Path: &empty, AllowedMcpIDs: &none,
	}, schemas); err != nil {
		t.Fatalf("dropping the grant should make conversion legal: %v", err)
	}
	converted, _ := s.findProjectByID(proj.ID)
	if !converted.IsRemote() {
		t.Fatal("project did not convert to remote")
	}
	if raw, ok := converted.Context["fsmcp"]; ok {
		t.Fatalf("converted project still carries host filesystem scope: %s", raw)
	}
}

// ---------------------------------------------------------------------------
// The two controls a profile can hold and cannot use (ADR-011 decision 2b's
// argument, ADR-009 decision 2's rule)
// ---------------------------------------------------------------------------
//
// A permission policy is a set of Claude CLI gates on a session, and a chat
// template is a preset for starting one. An access profile launches no session
// — resolvePtyEnv and resolveProjectTemplate both refuse a remote record — so
// both are inert on one. "Refusing it at the door is more honest than a control
// that quietly no-ops" is the same rule that already removes the path, the
// skill toggle, the shell templates, the model allowlist and directory auth,
// and it is the reason the editor no longer renders either as "inert".

func TestProjectCreateRemote_RejectsPermissionPolicy(t *testing.T) {
	s := &Settings{Version: 1}
	_, err := applyProjectCreate(s, projectCreateFields{
		Name: "Hermes Mail", Kind: ProjectKindRemote,
		PermissionPolicy: &PermissionPolicy{DefaultMode: "bypassPermissions"},
	}, nil)
	if err == nil {
		t.Fatal("a profile carrying a permission policy was accepted")
	}
	if !containsAll(err.Error(), "permission_policy", "access") {
		t.Errorf("the refusal must name the field and what does bound a client: %v", err)
	}
	if len(s.Projects) != 0 {
		t.Fatalf("a refused create persisted a project: %d", len(s.Projects))
	}
}

// An EMPTY policy is not a policy. The update path already reads one as "clear
// it", so refusing on it would refuse the very request that clears one.
func TestProjectCreateRemote_AcceptsAnEmptyPermissionPolicy(t *testing.T) {
	s := &Settings{Version: 1}
	created, err := applyProjectCreate(s, projectCreateFields{
		Name: "Hermes Mail", Kind: ProjectKindRemote,
		PermissionPolicy: &PermissionPolicy{},
	}, nil)
	if err != nil {
		t.Fatalf("an emptied policy was refused as a policy: %v", err)
	}
	if created.PermissionPolicy != nil {
		t.Errorf("an empty policy was stored rather than treated as absent: %+v", created.PermissionPolicy)
	}
}

func TestProjectCreateRemote_RejectsChatTemplates(t *testing.T) {
	s := &Settings{Version: 1}
	_, err := applyProjectCreate(s, projectCreateFields{
		Name: "Hermes Mail", Kind: ProjectKindRemote,
		ChatTemplates: []ChatTemplate{{ID: "t1", Name: "Default", Model: "claude-opus"}},
	}, nil)
	if err == nil {
		t.Fatal("a profile carrying a chat template was accepted")
	}
	if !containsAll(err.Error(), "chat templates", "no sessions") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	if len(s.Projects) != 0 {
		t.Fatalf("a refused create persisted a project: %d", len(s.Projects))
	}
	// The lower-level mutator refuses it too, so a caller that reaches past
	// applyProjectCreate cannot store one either.
	if _, err := s.CreateProjectWithTokenKind(ProjectKindRemote, "Hermes Mail", "", nil, nil,
		[]ChatTemplate{{ID: "t1", Name: "Default"}}, nil); err == nil {
		t.Fatal("CreateProjectWithTokenKind stored chat templates on a remote record")
	}
}

// The conversion escape hatch, mirroring disabled_tools: an existing local
// project carrying either one is refused while it still carries it, and the
// SAME request that clears it converts cleanly. Without this an operator who
// ever set a policy could never turn that project into a profile — a wall with
// no door in it.
func TestProjectConvertLocalToRemote_ClearingTheInertControlsMakesItLegal(t *testing.T) {
	dir := t.TempDir()
	s := &Settings{Version: 1}
	proj, err := s.CreateProjectWithToken("Local", dir, nil, nil,
		[]ChatTemplate{{ID: "t1", Name: "Default", Model: "claude-opus"}}, nil)
	if err != nil {
		t.Fatalf("create local: %v", err)
	}
	s.UpdateProjectPermissionPolicy(proj.ID, &PermissionPolicy{DefaultMode: "acceptEdits"})

	remote := ProjectKindRemote
	empty := ""
	noSurfaces := func() McpSurfaces { return nil }

	// Flipping kind alone is refused — twice over, once per control.
	if _, _, err := applyProjectUpdate(s, proj.ID, projectUpdateFields{Kind: &remote, Path: &empty}, noSurfaces); err == nil {
		t.Fatal("converting a project that still carries a policy and templates was allowed")
	}
	after, _ := s.findProjectByID(proj.ID)
	if after.IsRemote() {
		t.Fatal("a refused conversion mutated the record")
	}

	// Clearing both in the same request converts.
	noTemplates := []ChatTemplate{}
	if _, _, err := applyProjectUpdate(s, proj.ID, projectUpdateFields{
		Kind: &remote, Path: &empty,
		ChatTemplates:    &noTemplates,
		PermissionPolicy: &PermissionPolicy{},
	}, noSurfaces); err != nil {
		t.Fatalf("clearing both should make the conversion legal: %v", err)
	}
	converted, _ := s.findProjectByID(proj.ID)
	if !converted.IsRemote() {
		t.Fatal("project did not convert")
	}
	if converted.PermissionPolicy != nil || len(converted.ChatTemplates) != 0 {
		t.Fatalf("the converted profile still carries an inert control: policy=%+v templates=%+v",
			converted.PermissionPolicy, converted.ChatTemplates)
	}
}

// A LOCAL project is untouched by any of this: both controls are exactly what
// they always were.
func TestProjectLocal_KeepsItsPolicyAndTemplates(t *testing.T) {
	s := &Settings{Version: 1}
	created, err := applyProjectCreate(s, projectCreateFields{
		Name: "Workspace", Path: t.TempDir(),
		PermissionPolicy: &PermissionPolicy{DefaultMode: "acceptEdits"},
		ChatTemplates:    []ChatTemplate{{ID: "t1", Name: "Default", Model: "claude-opus"}},
	}, nil)
	if err != nil {
		t.Fatalf("a local project was refused: %v", err)
	}
	if created.PermissionPolicy == nil || created.PermissionPolicy.DefaultMode != "acceptEdits" {
		t.Errorf("the local project lost its policy: %+v", created.PermissionPolicy)
	}
	if len(created.ChatTemplates) != 1 {
		t.Errorf("the local project lost its templates: %+v", created.ChatTemplates)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
