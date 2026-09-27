package project

import (
	"github.com/barelyworkingcode/relay/internal/config"
	"strings"
	"testing"
)

// An access profile launches no session — resolvePtyEnv and
// resolveProjectTemplate both refuse a remote record — so both are inert on
// one. "Refusing it at the door is more honest than a control that quietly
// no-ops" is the same rule that already removes the path, the skill toggle,
// the shell templates, the model allowlist and directory auth.
func TestProjectCreateRemote_RejectsPermissionPolicy(t *testing.T) {
	s := &config.Settings{Version: 1}
	_, err := ApplyCreate(s, CreateFields{
		Name: "Hermes Mail", Kind: config.ProjectKindRemote,
		PermissionPolicy: &config.PermissionPolicy{DefaultMode: "bypassPermissions"},
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
	s := &config.Settings{Version: 1}
	created, err := ApplyCreate(s, CreateFields{
		Name: "Hermes Mail", Kind: config.ProjectKindRemote,
		PermissionPolicy: &config.PermissionPolicy{},
	}, nil)
	if err != nil {
		t.Fatalf("an emptied policy was refused as a policy: %v", err)
	}
	if created.PermissionPolicy != nil {
		t.Errorf("an empty policy was stored rather than treated as absent: %+v", created.PermissionPolicy)
	}
}

func TestProjectCreateRemote_RejectsChatTemplates(t *testing.T) {
	s := &config.Settings{Version: 1}
	_, err := ApplyCreate(s, CreateFields{
		Name: "Hermes Mail", Kind: config.ProjectKindRemote,
		ChatTemplates: []config.ChatTemplate{{ID: "t1", Name: "Default", Model: "claude-opus"}},
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
	// ApplyCreate cannot store one either.
	if _, err := CreateWithTokenKind(s, config.ProjectKindRemote, "Hermes Mail", "", nil, nil,
		[]config.ChatTemplate{{ID: "t1", Name: "Default"}}, nil); err == nil {
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
	s := &config.Settings{Version: 1}
	proj, err := CreateWithToken(s, "Local", dir, nil, nil,
		[]config.ChatTemplate{{ID: "t1", Name: "Default", Model: "claude-opus"}}, nil)
	if err != nil {
		t.Fatalf("create local: %v", err)
	}
	s.UpdateProjectPermissionPolicy(proj.ID, &config.PermissionPolicy{DefaultMode: "acceptEdits"})

	remote := config.ProjectKindRemote
	empty := ""
	noSurfaces := func() McpSurfaces { return nil }

	// Flipping kind alone is refused — twice over, once per control.
	if _, _, err := ApplyUpdate(s, proj.ID, UpdateFields{Kind: &remote, Path: &empty}, noSurfaces); err == nil {
		t.Fatal("converting a project that still carries a policy and templates was allowed")
	}
	after, _ := config.FindProjectByID(s, proj.ID)
	if after.IsRemote() {
		t.Fatal("a refused conversion mutated the record")
	}

	noTemplates := []config.ChatTemplate{}
	if _, _, err := ApplyUpdate(s, proj.ID, UpdateFields{
		Kind: &remote, Path: &empty,
		ChatTemplates:    &noTemplates,
		PermissionPolicy: &config.PermissionPolicy{},
	}, noSurfaces); err != nil {
		t.Fatalf("clearing both should make the conversion legal: %v", err)
	}
	converted, _ := config.FindProjectByID(s, proj.ID)
	if !converted.IsRemote() {
		t.Fatal("project did not convert")
	}
	if converted.PermissionPolicy != nil || len(converted.ChatTemplates) != 0 {
		t.Fatalf("the converted profile still carries an inert control: policy=%+v templates=%+v",
			converted.PermissionPolicy, converted.ChatTemplates)
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
