package main

import (
	"encoding/json"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/project"
)

func newProjectsTestStore(t *testing.T) config.SettingsStore {
	t.Helper()
	_ = mkSandboxRelayHome(t)
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
// auto-detection in the project package's scope derivation.
func fsSchemas() project.McpSurfaces {
	return project.McpSurfaces{
		"fsmcp":  {Schema: json.RawMessage(`{"allowed_dirs": {"type": "array"}}`)},
		"macmcp": {Schema: json.RawMessage(`{}`)},
	}
}

func createTestProject(t *testing.T, store config.SettingsStore, name, path string, mcpIDs []string) config.Project {
	t.Helper()
	var proj config.Project
	store.With(func(s *config.Settings) {
		var err error
		proj, err = project.CreateWithToken(s, name, path, mcpIDs, []string{"*"}, nil, fsSchemas())
		if err != nil {
			t.Fatalf("project.CreateWithToken: %v", err)
		}
	})
	return proj
}
