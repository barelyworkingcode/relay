package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"relaygo/mcp"
)

// testSchemas returns a schema map marking fsmcp as a filesystem MCP.
func testSchemas() map[string]json.RawMessage {
	return map[string]json.RawMessage{
		"fsmcp": json.RawMessage(`{"allowed_dirs":{"type":"array"}}`),
	}
}

// ---------------------------------------------------------------------------
// TestProjectCreate — create a project with temp dir, verify inline token
// ---------------------------------------------------------------------------

func TestProjectCreate(t *testing.T) {
	tmpDir := t.TempDir()

	s := &Settings{
		Version:      1,
		ExternalMcps: []ExternalMcp{
			{ID: "fsmcp", DisplayName: "fsMCP"},
			{ID: "macmcp", DisplayName: "macMCP"},
			{ID: "searchmcp", DisplayName: "searchMCP"},
		},
		Services: []ServiceConfig{},
		Projects: []Project{},
	}

	proj, err := s.CreateProjectWithToken("TestProject", tmpDir, []string{"fsmcp", "macmcp"}, []string{"claude-opus"}, nil, testSchemas())
	if err != nil {
		t.Fatalf("CreateProjectWithToken failed: %v", err)
	}

	// Verify project fields.
	if proj.Name != "TestProject" {
		t.Errorf("expected name 'TestProject', got %q", proj.Name)
	}
	if proj.Path != tmpDir {
		t.Errorf("expected path %q, got %q", tmpDir, proj.Path)
	}
	if len(proj.AllowedMcpIDs) != 2 {
		t.Fatalf("expected 2 allowed MCPs, got %d", len(proj.AllowedMcpIDs))
	}
	if len(proj.AllowedModels) != 1 || proj.AllowedModels[0] != "claude-opus" {
		t.Errorf("expected allowed models [claude-opus], got %v", proj.AllowedModels)
	}
	if proj.Token == "" {
		t.Fatal("expected non-empty token plaintext")
	}
	if proj.TokenHash == "" {
		t.Fatal("expected non-empty token hash")
	}

	// Verify project is in settings.
	if len(s.Projects) != 1 {
		t.Fatalf("expected 1 project in settings, got %d", len(s.Projects))
	}

	// Verify fsMCP context: allowed_dirs should contain the project path.
	stored := s.Projects[0]
	fsmcpCtx := stored.Context["fsmcp"]
	if fsmcpCtx == nil {
		t.Fatal("expected fsmcp context")
	}
	var ctxMap map[string]interface{}
	if err := json.Unmarshal(fsmcpCtx, &ctxMap); err != nil {
		t.Fatalf("failed to parse fsmcp context: %v", err)
	}
	dirs, ok := ctxMap["allowed_dirs"].([]interface{})
	if !ok || len(dirs) != 1 {
		t.Fatalf("expected allowed_dirs with 1 entry, got %v", ctxMap["allowed_dirs"])
	}
	if dirs[0] != tmpDir {
		t.Errorf("expected allowed_dirs[0] = %q, got %q", tmpDir, dirs[0])
	}

	// Verify fs_bash is disabled for fsMCP.
	if disabled := stored.DisabledTools["fsmcp"]; len(disabled) == 0 || disabled[0] != "fs_bash" {
		t.Error("expected fs_bash to be disabled for fsmcp")
	}

	// Verify macmcp has no context.
	if stored.Context["macmcp"] != nil {
		t.Error("expected no context for macmcp")
	}

	// Verify AuthenticateProject derives permissions from AllowedMcpIDs.
	// Allowed MCPs are absent from permissions (implicit allow).
	// Disallowed MCPs have explicit PermOff entries.
	authTok, err := s.AuthenticateProject(proj.Token)
	if err != nil {
		t.Fatalf("AuthenticateProject failed: %v", err)
	}
	if authTok.Permissions["fsmcp"] == PermOff {
		t.Error("fsmcp should be allowed (in allowed list)")
	}
	if authTok.Permissions["searchmcp"] != PermOff {
		t.Error("expected searchmcp PermOff (not in allowed list)")
	}
}

// ---------------------------------------------------------------------------
// TestProjectUpdate — modify MCPs, verify inline permissions update
// ---------------------------------------------------------------------------

func TestProjectUpdate(t *testing.T) {
	tmpDir := t.TempDir()

	s := &Settings{
		Version:      1,
		ExternalMcps: []ExternalMcp{
			{ID: "fsmcp", DisplayName: "fsMCP"},
			{ID: "macmcp", DisplayName: "macMCP"},
			{ID: "searchmcp", DisplayName: "searchMCP"},
		},
		Services: []ServiceConfig{},
		Projects: []Project{},
	}

	proj, err := s.CreateProjectWithToken("UpdateTest", tmpDir, []string{"fsmcp"}, nil, nil, testSchemas())
	if err != nil {
		t.Fatalf("CreateProjectWithToken failed: %v", err)
	}

	// Update: change allowed MCPs.
	s.UpdateProjectMcps(proj.ID, []string{"macmcp", "searchmcp"}, testSchemas())

	p2, _ := s.findProjectByID(proj.ID)
	if len(p2.AllowedMcpIDs) != 2 {
		t.Fatalf("expected 2 allowed MCPs after update, got %d", len(p2.AllowedMcpIDs))
	}

	// Verify auth derives correct permissions from updated AllowedMcpIDs.
	authTok, err := s.AuthenticateProject(proj.Token)
	if err != nil {
		t.Fatalf("AuthenticateProject failed: %v", err)
	}
	if authTok.Permissions["fsmcp"] != PermOff {
		t.Error("expected fsmcp PermOff after update (removed from allowed)")
	}
	if authTok.Permissions["macmcp"] == PermOff {
		t.Error("macmcp should be allowed after update")
	}
	if authTok.Permissions["searchmcp"] == PermOff {
		t.Error("searchmcp should be allowed after update")
	}
}

// ---------------------------------------------------------------------------
// TestProjectDelete — create then delete, verify cleanup
// ---------------------------------------------------------------------------

func TestProjectDelete(t *testing.T) {
	tmpDir := t.TempDir()

	s := &Settings{
		Version:      1,
		ExternalMcps: []ExternalMcp{
			{ID: "fsmcp", DisplayName: "fsMCP"},
		},
		Services: []ServiceConfig{},
		Projects: []Project{},
	}

	proj, err := s.CreateProjectWithToken("DeleteTest", tmpDir, []string{"fsmcp"}, nil, nil, testSchemas())
	if err != nil {
		t.Fatalf("CreateProjectWithToken failed: %v", err)
	}

	if len(s.Projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(s.Projects))
	}

	s.RemoveProject(proj.ID)

	if len(s.Projects) != 0 {
		t.Errorf("expected 0 projects after delete, got %d", len(s.Projects))
	}

	// Verify token no longer authenticates.
	_, err = s.AuthenticateProject(proj.Token)
	if err == nil {
		t.Error("expected auth failure after project deletion")
	}
}

// ---------------------------------------------------------------------------
// TestProjectTokenScoping — verify router enforces project token permissions
// ---------------------------------------------------------------------------

func TestProjectTokenScoping(t *testing.T) {
	tmpDir := t.TempDir()

	s := &Settings{
		Version:      1,
		ExternalMcps: []ExternalMcp{
			{ID: "fsmcp", DisplayName: "fsMCP"},
			{ID: "macmcp", DisplayName: "macMCP"},
		},
		Services:    []ServiceConfig{},
		Projects:    []Project{},
		AdminSecret: "test-admin",
	}

	// Create project with only fsmcp.
	proj, err := s.CreateProjectWithToken("ScopeTest", tmpDir, []string{"fsmcp"}, nil, nil, testSchemas())
	if err != nil {
		t.Fatalf("CreateProjectWithToken failed: %v", err)
	}

	// Set up mock MCP connections.
	mgr := NewExternalMcpManager(nil)
	addMockConn(mgr, "fsmcp", newMockConn("fsmcp", []mcp.Tool{
		{Name: "fs_read", Description: "Read file"},
		{Name: "fs_write", Description: "Write file"},
		{Name: "fs_bash", Description: "Run bash"},
	}, func(_ context.Context, _ string, _ interface{}) (json.RawMessage, error) {
		return json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`), nil
	}))
	addMockConn(mgr, "macmcp", newMockConn("macmcp", simpleTools("capture_screenshot"),
		func(_ context.Context, _ string, _ interface{}) (json.RawMessage, error) {
			return json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`), nil
		}))

	store := &FileSettingsStore{cache: s, dir: t.TempDir()}
	r := &appRouter{
		store:    store,
		tools:    mgr,
		services: NewServiceRegistry(),
		onChange: func() {},
	}

	// ListTools with project token — should only see fsmcp tools, minus fs_bash (disabled).
	result, err := r.ListTools(context.Background(), proj.Token)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	tools := unmarshalTools(t, result)
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools (fs_read, fs_write — fs_bash disabled), got %d: %v", len(tools), toolNames(tools))
	}
	for _, tool := range tools {
		if tool.Name == "fs_bash" {
			t.Error("fs_bash should be excluded (disabled)")
		}
		if tool.Name == "capture_screenshot" {
			t.Error("capture_screenshot should be excluded (macmcp not in project)")
		}
	}

	// CallTool to allowed MCP — should succeed.
	_, err = r.CallTool(context.Background(), "fs_read", json.RawMessage(`{"path":"/tmp"}`), proj.Token)
	if err != nil {
		t.Fatalf("expected fs_read to succeed, got: %v", err)
	}

	// CallTool to disallowed MCP — should be denied.
	_, err = r.CallTool(context.Background(), "capture_screenshot", nil, proj.Token)
	if err == nil {
		t.Fatal("expected error calling tool from disallowed MCP")
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("expected 'access denied', got %q", err.Error())
	}

	// CallTool to disabled tool (fs_bash) — should be denied.
	_, err = r.CallTool(context.Background(), "fs_bash", json.RawMessage(`{"command":"ls"}`), proj.Token)
	if err == nil {
		t.Fatal("expected error calling disabled tool fs_bash")
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("expected 'access denied', got %q", err.Error())
	}
}

// ---------------------------------------------------------------------------
// TestProjectPersistence — round-trip through FileSettingsStore
// ---------------------------------------------------------------------------

func TestProjectPersistence(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewSettingsStoreAt(tmpDir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized failed: %v", err)
	}

	projectDir := t.TempDir()

	// Create a project via store.With.
	var projToken string
	err := store.With(func(s *Settings) {
		s.ExternalMcps = append(s.ExternalMcps, ExternalMcp{
			ID:          "fsmcp",
			DisplayName: "fsMCP",
		})
		proj, err := s.CreateProjectWithToken("PersistTest", projectDir, []string{"fsmcp"}, []string{"claude-opus"}, nil, testSchemas())
		if err != nil {
			t.Fatalf("CreateProjectWithToken failed: %v", err)
		}
		projToken = proj.Token
	})
	if err != nil {
		t.Fatalf("store.With failed: %v", err)
	}

	// Reload from disk and verify.
	reloaded := store.Reload()
	if len(reloaded.Projects) != 1 {
		t.Fatalf("expected 1 project after reload, got %d", len(reloaded.Projects))
	}
	proj := reloaded.Projects[0]
	if proj.Name != "PersistTest" {
		t.Errorf("expected name 'PersistTest', got %q", proj.Name)
	}
	if proj.Token != projToken {
		t.Error("token plaintext not preserved after reload")
	}
	if len(proj.AllowedMcpIDs) != 1 || proj.AllowedMcpIDs[0] != "fsmcp" {
		t.Errorf("expected allowed MCPs [fsmcp] after reload, got %v", proj.AllowedMcpIDs)
	}

	// Verify project token authenticates.
	authTok, err := reloaded.AuthenticateProject(projToken)
	if err != nil {
		t.Fatalf("expected project token to authenticate after reload: %v", err)
	}
	if authTok.Permissions["fsmcp"] == PermOff {
		t.Error("fsmcp should be allowed on authenticated project token")
	}

	// Delete the project via store.With.
	err = store.With(func(s *Settings) {
		s.RemoveProject(proj.ID)
	})
	if err != nil {
		t.Fatalf("store.With delete failed: %v", err)
	}

	// Verify cleanup after reload.
	reloaded2 := store.Reload()
	if len(reloaded2.Projects) != 0 {
		t.Errorf("expected 0 projects after delete, got %d", len(reloaded2.Projects))
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func toolNames(tools []mcp.Tool) []string {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	return names
}
