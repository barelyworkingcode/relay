package project

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
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
		if _, err := CreateWithToken(s, "P", bad, nil, nil, nil, nil); err == nil {
			t.Fatalf("expected rejection of unsafe path %q", bad)
		}
	}
	if len(s.Projects) != 0 {
		t.Fatalf("no project should have been created from invalid paths; got %d", len(s.Projects))
	}
	if _, err := CreateWithToken(s, "P", t.TempDir(), nil, nil, nil, nil); err != nil {
		t.Fatalf("clean absolute path should be accepted: %v", err)
	}
}

func TestProjectCreateRemote_RejectsPath(t *testing.T) {
	s := &config.Settings{Version: 1}
	if _, err := CreateWithTokenKind(s, config.ProjectKindRemote, "Agent VM", "/some/host/dir", nil, nil, nil, nil); err == nil {
		t.Fatal("expected rejection of remote project with a path")
	}
	if len(s.Projects) != 0 {
		t.Fatalf("rejected create must not persist a project; got %d", len(s.Projects))
	}
}

// Subtle: modelAllowedForProject treats both an empty list and ["*"] as
// unrestricted, so a non-empty list is the only shape that would mean
// something — and remote has no model-scoping story yet.
func TestProjectCreateRemote_RejectsNonEmptyModels(t *testing.T) {
	s := &config.Settings{Version: 1}
	if _, err := CreateWithTokenKind(s, config.ProjectKindRemote, "Agent VM", "", nil, []string{"claude-opus"}, nil, nil); err == nil {
		t.Fatal("expected rejection of remote project with non-empty allowed_models")
	}
}

// Deliberate: local never writes "kind":"local" — it persists as the absent
// key, the same wire shape as before this field existed.
func TestProjectKind_LocalSerializesWithNoKindKey(t *testing.T) {
	s := &config.Settings{Version: 1}
	proj, err := CreateWithToken(s, "Local", t.TempDir(), nil, nil, nil, nil)
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

	proj, err := CreateWithToken(s, "TestProject", tmpDir, []string{"fsmcp", "macmcp"}, []string{"claude-opus"}, nil, testSchemas())
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
		proj, err := CreateWithToken(s, "PersistTest", projectDir, []string{"fsmcp"}, []string{"claude-opus"}, nil, testSchemas())
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

func TestValidateChatTemplatePresets(t *testing.T) {
	home := []config.ProjectMode{config.ProjectModeHome}
	work := []config.ProjectMode{config.ProjectModeWork}
	tmpl := func(name, mode string, p ...config.ProjectMode) config.ChatTemplate {
		return config.ChatTemplate{ID: name, Name: name, Model: "m", Mode: mode, PresetFor: p}
	}
	cases := []struct {
		name string
		ts   []config.ChatTemplate
		want string // empty = accepted
	}{
		{"none", []config.ChatTemplate{tmpl("A", "")}, ""},
		{"ask and voice same mode", []config.ChatTemplate{tmpl("A", "", home...), tmpl("V", "voice", home...)}, ""},
		{"both modes one template", []config.ChatTemplate{tmpl("A", "", config.ProjectModeHome, config.ProjectModeWork)}, ""},
		{"different modes", []config.ChatTemplate{tmpl("A", "", home...), tmpl("B", "", work...)}, ""},
		{"both refused", []config.ChatTemplate{tmpl("A", "", config.ProjectModeBoth)}, "must be home or work"},
		{"empty refused", []config.ChatTemplate{tmpl("A", "", "")}, "must be home or work"},
		{"unknown refused", []config.ChatTemplate{tmpl("A", "", "Home")}, "must be home or work"},
		{"duplicate in one", []config.ChatTemplate{tmpl("A", "", config.ProjectModeHome, config.ProjectModeHome)}, "twice"},
		{"two home Ask", []config.ChatTemplate{tmpl("Quick", "", home...), tmpl("Plain", "chat", home...)},
			`chat templates "Quick" and "Plain" are both the home Ask preset; a project has at most one Ask preset and one voice preset per mode`},
		{"two work voice", []config.ChatTemplate{tmpl("V1", "voice", work...), tmpl("V2", "voice", work...)}, `"V1" and "V2" are both the work voice preset`},
	}
	for _, c := range cases {
		proj := &config.Project{Path: t.TempDir(), ChatTemplates: c.ts}
		err := ValidateShape(proj)
		if c.want == "" && err != nil {
			t.Errorf("%s: unexpected refusal: %v", c.name, err)
		}
		if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: err = %v, want containing %q", c.name, err, c.want)
		}
	}
}

func TestValidateShape_RemoteRefusesTemplatesBeforePresets(t *testing.T) {
	proj := &config.Project{Kind: config.ProjectKindRemote, ChatTemplates: []config.ChatTemplate{
		{ID: "a", Name: "A", PresetFor: []config.ProjectMode{config.ProjectModeBoth}},
	}}
	err := ValidateShape(proj)
	if err == nil || !strings.Contains(err.Error(), "must not have chat templates") {
		t.Fatalf("err = %v, want the remote chat-templates refusal", err)
	}
}
