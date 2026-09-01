package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/mcp"
)

func testSchemas() McpSurfaces {
	return McpSurfaces{
		"fsmcp": {Schema: json.RawMessage(`{"allowed_dirs":{"type":"array"}}`)},
	}
}

// An unsafe path would feed fsMCP's allowed_dirs and the skill-write target
// directly.
func TestProjectCreate_RejectsUnsafePath(t *testing.T) {
	s := &config.Settings{Version: 1}
	for _, bad := range []string{"", "relative/dir", "../escape", "/ok/../../etc"} {
		if _, err := createProjectWithToken(s, "P", bad, nil, nil, nil, nil); err == nil {
			t.Fatalf("expected rejection of unsafe path %q", bad)
		}
	}
	if len(s.Projects) != 0 {
		t.Fatalf("no project should have been created from invalid paths; got %d", len(s.Projects))
	}
	if _, err := createProjectWithToken(s, "P", t.TempDir(), nil, nil, nil, nil); err != nil {
		t.Fatalf("clean absolute path should be accepted: %v", err)
	}
}

func TestProjectCreateRemote_NoPathSucceeds(t *testing.T) {
	s := &config.Settings{Version: 1}
	proj, err := createProjectWithTokenKind(s, config.ProjectKindRemote, "Agent VM", "", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("expected pathless remote project to be created, got: %v", err)
	}
	if proj.Path != "" {
		t.Errorf("expected empty path, got %q", proj.Path)
	}
	if !proj.IsRemote() {
		t.Errorf("expected IsRemote() true, got Kind=%q", proj.Kind)
	}
	if len(s.Projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(s.Projects))
	}
}

// Empty grant list is a valid resting state (enroll now, grant tools later)
// — unlike the wildcard, which is always rejected.
func TestProjectCreateRemote_ZeroAllowedMcpsValid(t *testing.T) {
	s := &config.Settings{Version: 1}
	proj, err := createProjectWithTokenKind(s, config.ProjectKindRemote, "Agent VM", "", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("expected zero-MCP remote project to be created, got: %v", err)
	}
	if len(proj.AllowedMcpIDs) != 0 {
		t.Errorf("expected zero allowed MCPs, got %v", proj.AllowedMcpIDs)
	}
}

func TestProjectCreateRemote_RejectsPath(t *testing.T) {
	s := &config.Settings{Version: 1}
	if _, err := createProjectWithTokenKind(s, config.ProjectKindRemote, "Agent VM", "/some/host/dir", nil, nil, nil, nil); err == nil {
		t.Fatal("expected rejection of remote project with a path")
	}
	if len(s.Projects) != 0 {
		t.Fatalf("rejected create must not persist a project; got %d", len(s.Projects))
	}
}

// Deliberate: "*" would let a future MCP registration silently widen what a
// remote machine can reach, with nothing to review.
func TestProjectCreateRemote_RejectsWildcardMcps(t *testing.T) {
	s := &config.Settings{Version: 1}
	if _, err := createProjectWithTokenKind(s, config.ProjectKindRemote, "Agent VM", "", []string{"*"}, nil, nil, nil); err == nil {
		t.Fatal("expected rejection of remote project with wildcard allowed_mcp_ids")
	}
}

// Subtle: modelAllowedForProject treats both an empty list and ["*"] as
// unrestricted, so a non-empty list is the only shape that would mean
// something — and remote has no model-scoping story yet.
func TestProjectCreateRemote_RejectsNonEmptyModels(t *testing.T) {
	s := &config.Settings{Version: 1}
	if _, err := createProjectWithTokenKind(s, config.ProjectKindRemote, "Agent VM", "", nil, []string{"claude-opus"}, nil, nil); err == nil {
		t.Fatal("expected rejection of remote project with non-empty allowed_models")
	}
}

func TestProjectCreateRemote_RejectsPathScopedMcpGrant(t *testing.T) {
	s := &config.Settings{Version: 1}
	_, err := createProjectWithTokenKind(s, config.ProjectKindRemote, "Agent VM", "", []string{"fsmcp"}, nil, nil, testSchemas())
	if err == nil {
		t.Fatal("expected rejection of remote project granted a path-scoped MCP")
	}
	if !strings.Contains(err.Error(), "fsmcp") {
		t.Errorf("expected error to name the offending MCP (fsmcp), got: %v", err)
	}
	if len(s.Projects) != 0 {
		t.Fatalf("rejected create must not persist a project; got %d", len(s.Projects))
	}
}

func TestProjectKind_ZeroValueRoundTripsAsLocal(t *testing.T) {
	raw := []byte(`{"id":"p1","name":"NoKindKey","path":"/tmp/x","allowed_mcp_ids":["*"],"allowed_models":["*"]}`)
	var proj config.Project
	if err := json.Unmarshal(raw, &proj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if proj.Kind != "" {
		t.Fatalf("expected zero-value Kind, got %q", proj.Kind)
	}
	if proj.IsRemote() {
		t.Fatal("a project loaded from JSON with no kind key must not read as remote")
	}
}

// Deliberate: local never writes "kind":"local" — it persists as the absent
// key, the same wire shape as before this field existed.
func TestProjectKind_LocalSerializesWithNoKindKey(t *testing.T) {
	s := &config.Settings{Version: 1}
	proj, err := createProjectWithToken(s, "Local", t.TempDir(), nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateProjectWithToken: %v", err)
	}
	sealMe := &config.Settings{Projects: []config.Project{proj}}
	if err := config.SealAllSecrets(sealMe, testSealer()); err != nil {
		t.Fatalf("seal: %v", err)
	}
	b, err := json.Marshal(sealMe.Projects[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), `"kind"`) {
		t.Errorf("expected no kind key in serialized local project, got %s", b)
	}
}

func TestProjectCreate(t *testing.T) {
	tmpDir := t.TempDir()

	s := &config.Settings{
		Version: 1,
		ExternalMcps: []config.ExternalMcp{
			{ID: "fsmcp", DisplayName: "fsMCP"},
			{ID: "macmcp", DisplayName: "macMCP"},
			{ID: "searchmcp", DisplayName: "searchMCP"},
		},
		Services: []config.ServiceConfig{},
		Projects: []config.Project{},
	}

	proj, err := createProjectWithToken(s, "TestProject", tmpDir, []string{"fsmcp", "macmcp"}, []string{"claude-opus"}, nil, testSchemas())
	if err != nil {
		t.Fatalf("CreateProjectWithToken failed: %v", err)
	}

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
	if tok, _ := proj.Token.Reveal(); tok == "" {
		t.Fatal("expected non-empty token plaintext")
	}
	if proj.TokenHash == "" {
		t.Fatal("expected non-empty token hash")
	}

	if len(s.Projects) != 1 {
		t.Fatalf("expected 1 project in settings, got %d", len(s.Projects))
	}

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

	if disabled := stored.DisabledTools["fsmcp"]; len(disabled) == 0 || disabled[0] != "fs_bash" {
		t.Error("expected fs_bash to be disabled for fsmcp")
	}

	if stored.Context["macmcp"] != nil {
		t.Error("expected no context for macmcp")
	}

	// Subtle: allowed MCPs are absent from Permissions (implicit allow); only
	// disallowed MCPs get explicit PermOff entries.
	projTok, _ := proj.Token.Reveal()
	authTok, err := s.AuthenticateProject(projTok)
	if err != nil {
		t.Fatalf("AuthenticateProject failed: %v", err)
	}
	if authTok.Permissions["fsmcp"] == config.PermOff {
		t.Error("fsmcp should be allowed (in allowed list)")
	}
	if authTok.Permissions["searchmcp"] != config.PermOff {
		t.Error("expected searchmcp PermOff (not in allowed list)")
	}
}

func TestProjectUpdate(t *testing.T) {
	tmpDir := t.TempDir()

	s := &config.Settings{
		Version: 1,
		ExternalMcps: []config.ExternalMcp{
			{ID: "fsmcp", DisplayName: "fsMCP"},
			{ID: "macmcp", DisplayName: "macMCP"},
			{ID: "searchmcp", DisplayName: "searchMCP"},
		},
		Services: []config.ServiceConfig{},
		Projects: []config.Project{},
	}

	proj, err := createProjectWithToken(s, "UpdateTest", tmpDir, []string{"fsmcp"}, nil, nil, testSchemas())
	if err != nil {
		t.Fatalf("CreateProjectWithToken failed: %v", err)
	}

	updateProjectMcps(s, proj.ID, []string{"macmcp", "searchmcp"}, testSchemas())

	p2, _ := config.FindProjectByID(s, proj.ID)
	if len(p2.AllowedMcpIDs) != 2 {
		t.Fatalf("expected 2 allowed MCPs after update, got %d", len(p2.AllowedMcpIDs))
	}

	projTok, _ := proj.Token.Reveal()
	authTok, err := s.AuthenticateProject(projTok)
	if err != nil {
		t.Fatalf("AuthenticateProject failed: %v", err)
	}
	if authTok.Permissions["fsmcp"] != config.PermOff {
		t.Error("expected fsmcp PermOff after update (removed from allowed)")
	}
	if authTok.Permissions["macmcp"] == config.PermOff {
		t.Error("macmcp should be allowed after update")
	}
	if authTok.Permissions["searchmcp"] == config.PermOff {
		t.Error("searchmcp should be allowed after update")
	}
}

func TestProjectDelete(t *testing.T) {
	tmpDir := t.TempDir()

	s := &config.Settings{
		Version: 1,
		ExternalMcps: []config.ExternalMcp{
			{ID: "fsmcp", DisplayName: "fsMCP"},
		},
		Services: []config.ServiceConfig{},
		Projects: []config.Project{},
	}

	proj, err := createProjectWithToken(s, "DeleteTest", tmpDir, []string{"fsmcp"}, nil, nil, testSchemas())
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

	projTok, _ := proj.Token.Reveal()
	_, err = s.AuthenticateProject(projTok)
	if err == nil {
		t.Error("expected auth failure after project deletion")
	}
}

func TestProjectTokenScoping(t *testing.T) {
	tmpDir := t.TempDir()

	s := &config.Settings{
		Version: 1,
		ExternalMcps: []config.ExternalMcp{
			{ID: "fsmcp", DisplayName: "fsMCP"},
			{ID: "macmcp", DisplayName: "macMCP"},
		},
		Services:    []config.ServiceConfig{},
		Projects:    []config.Project{},
		AdminSecret: config.NewSecret("test-admin"),
	}

	proj, err := createProjectWithToken(s, "ScopeTest", tmpDir, []string{"fsmcp"}, nil, nil, testSchemas())
	if err != nil {
		t.Fatalf("CreateProjectWithToken failed: %v", err)
	}

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

	store := storeWithCache(t.TempDir(), testSealer(), s)
	r := &appRouter{
		store:    store,
		tools:    mgr,
		services: NewServiceRegistry(),
		onChange: func() {},
	}

	projTok, _ := proj.Token.Reveal()
	result, err := r.ListTools(context.Background(), projTok)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	tools := unmarshalTools(t, result)
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools (fs_read, fs_write — fs_bash disabled), got %d: %v", len(tools), toolNamesOf(tools))
	}
	for _, tool := range tools {
		if tool.Name == "fs_bash" {
			t.Error("fs_bash should be excluded (disabled)")
		}
		if tool.Name == "capture_screenshot" {
			t.Error("capture_screenshot should be excluded (macmcp not in project)")
		}
	}

	_, err = r.CallTool(context.Background(), "fs_read", json.RawMessage(`{"path":"/tmp"}`), projTok)
	if err != nil {
		t.Fatalf("expected fs_read to succeed, got: %v", err)
	}

	_, err = r.CallTool(context.Background(), "capture_screenshot", nil, projTok)
	if err == nil {
		t.Fatal("expected error calling tool from disallowed MCP")
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("expected 'access denied', got %q", err.Error())
	}

	_, err = r.CallTool(context.Background(), "fs_bash", json.RawMessage(`{"command":"ls"}`), projTok)
	if err == nil {
		t.Fatal("expected error calling disabled tool fs_bash")
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("expected 'access denied', got %q", err.Error())
	}
}

func TestProjectPersistence(t *testing.T) {
	tmpDir := t.TempDir()
	store := sealedSettingsStoreAt(tmpDir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized failed: %v", err)
	}

	projectDir := t.TempDir()

	var projToken string
	err := store.With(func(s *config.Settings) {
		s.ExternalMcps = append(s.ExternalMcps, config.ExternalMcp{
			ID:          "fsmcp",
			DisplayName: "fsMCP",
		})
		proj, err := createProjectWithToken(s, "PersistTest", projectDir, []string{"fsmcp"}, []string{"claude-opus"}, nil, testSchemas())
		if err != nil {
			t.Fatalf("CreateProjectWithToken failed: %v", err)
		}
		projToken, _ = proj.Token.Reveal()
	})
	if err != nil {
		t.Fatalf("store.With failed: %v", err)
	}

	reloaded := store.Reload()
	if len(reloaded.Projects) != 1 {
		t.Fatalf("expected 1 project after reload, got %d", len(reloaded.Projects))
	}
	proj := reloaded.Projects[0]
	if proj.Name != "PersistTest" {
		t.Errorf("expected name 'PersistTest', got %q", proj.Name)
	}
	if reloadedTok, _ := proj.Token.Reveal(); reloadedTok != projToken {
		t.Error("token plaintext not preserved after reload")
	}
	if len(proj.AllowedMcpIDs) != 1 || proj.AllowedMcpIDs[0] != "fsmcp" {
		t.Errorf("expected allowed MCPs [fsmcp] after reload, got %v", proj.AllowedMcpIDs)
	}

	authTok, err := reloaded.AuthenticateProject(projToken)
	if err != nil {
		t.Fatalf("expected project token to authenticate after reload: %v", err)
	}
	if authTok.Permissions["fsmcp"] == config.PermOff {
		t.Error("fsmcp should be allowed on authenticated project token")
	}

	err = store.With(func(s *config.Settings) {
		s.RemoveProject(proj.ID)
	})
	if err != nil {
		t.Fatalf("store.With delete failed: %v", err)
	}

	reloaded2 := store.Reload()
	if len(reloaded2.Projects) != 0 {
		t.Errorf("expected 0 projects after delete, got %d", len(reloaded2.Projects))
	}
}

func toolNamesOf(tools []mcp.Tool) []string {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	return names
}
