package main

import (
	"encoding/json"
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
	schemas := func() map[string]json.RawMessage {
		return map[string]json.RawMessage{"fsmcp": json.RawMessage(`{"allowed_dirs":{"type":"array"}}`)}
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
