package main

// R-S4a hermetic tests: SH §3.2's authorization matrix, each check asserted
// to refuse independently (not just "a fully valid request succeeds"), plus
// the LaunchSpec builder's shape per kind and the permission-policy merge's
// exact client/project asymmetry.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/service"
	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
)

func newLaunchTestStore(t *testing.T) config.SettingsStore {
	t.Helper()
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	return store
}

// addLaunchTestProject creates a project rooted at a real temp directory
// (so directory-confinement tests have real inodes and symlinks to check
// against) and saves it. mutate may override any field before it is saved.
func addLaunchTestProject(t *testing.T, store config.SettingsStore, mutate func(*config.Project)) config.Project {
	t.Helper()
	proj := config.Project{
		ID:            "p1",
		Name:          "Widget",
		Path:          t.TempDir(),
		AllowedMcpIDs: []string{"*"},
		AllowedModels: []string{"*"},
	}
	if mutate != nil {
		mutate(&proj)
	}
	if err := store.With(func(s *config.Settings) {
		s.Projects = append(s.Projects, proj)
	}); err != nil {
		t.Fatalf("store.With: %v", err)
	}
	return proj
}

func bearerCaller(classes ...control.CapabilityClass) LaunchCaller {
	return LaunchCaller{Credential: &config.APICredential{ID: "cred-1", Classes: classes}}
}

func frontendIdentityCaller(name string) LaunchCaller {
	return LaunchCaller{Identity: &service.Identity{
		Kind: service.IdentityKindService, Name: name,
		Capabilities: []config.ServiceCapability{config.ServiceCapabilityFrontend},
	}}
}

// ---------------------------------------------------------------------------
// Caller
// ---------------------------------------------------------------------------

func TestAuthorizeLaunch_NoExecuteClassRefuses(t *testing.T) {
	store := newLaunchTestStore(t)
	req := LaunchRequest{Caller: bearerCaller(control.ClassRead), Kind: KindPTY, TemplateID: "shell"}
	_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), req)
	if refusal == nil {
		t.Fatal("bearer holding only read was allowed to launch")
	}
	if refusal.Status != 403 {
		t.Fatalf("status = %d, want 403", refusal.Status)
	}
	if refusal.Audit.Outcome != "denied" {
		t.Fatalf("audit outcome = %q, want denied", refusal.Audit.Outcome)
	}
}

func TestAuthorizeLaunch_ProxyOnlyBearerRefuses(t *testing.T) {
	store := newLaunchTestStore(t)
	req := LaunchRequest{Caller: bearerCaller(control.ClassProxy), Kind: KindPTY, TemplateID: "shell"}
	_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), req)
	if refusal == nil {
		t.Fatal("bearer holding only proxy was allowed to launch")
	}
	if refusal.Status != 403 {
		t.Fatalf("status = %d, want 403", refusal.Status)
	}
}

func TestAuthorizeLaunch_EmptyClassesRefuses(t *testing.T) {
	store := newLaunchTestStore(t)
	req := LaunchRequest{Caller: bearerCaller(), Kind: KindPTY, TemplateID: "shell"}
	if _, refusal := AuthorizeLaunch(store, NewModelKeyTable(), req); refusal == nil {
		t.Fatal("bearer with no classes at all was allowed to launch")
	}
}

func TestAuthorizeLaunch_NoCallerAtAllRefuses(t *testing.T) {
	store := newLaunchTestStore(t)
	req := LaunchRequest{Kind: KindPTY, TemplateID: "shell"}
	if _, refusal := AuthorizeLaunch(store, NewModelKeyTable(), req); refusal == nil {
		t.Fatal("a request naming neither an identity nor a credential was allowed to launch")
	}
}

func TestAuthorizeLaunch_NonFrontendIdentityRefuses(t *testing.T) {
	store := newLaunchTestStore(t)
	// A service identity that never claimed the frontend capability (e.g. a
	// manifest-only service) must not be treated as eve.
	caller := LaunchCaller{Identity: &service.Identity{
		Kind: service.IdentityKindService, Name: "relayLLM",
		Capabilities: []config.ServiceCapability{config.ServiceCapabilityManifest},
	}}
	req := LaunchRequest{Caller: caller, Kind: KindPTY, TemplateID: "shell"}
	if _, refusal := AuthorizeLaunch(store, NewModelKeyTable(), req); refusal == nil {
		t.Fatal("a manifest-only identity was allowed to launch")
	}
}

func TestAuthorizeLaunch_ExecuteBearerAdHocPtySucceeds(t *testing.T) {
	store := newLaunchTestStore(t)
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), Kind: KindPTY, TemplateID: "shell"}
	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), req)
	if refusal != nil {
		t.Fatalf("valid ad-hoc pty launch refused: %+v", refusal)
	}
	if result.Spec.Project != nil {
		t.Fatalf("ad-hoc launch carries a project: %s", result.Spec.Project)
	}
}

func TestAuthorizeLaunch_FrontendIdentitySucceeds(t *testing.T) {
	store := newLaunchTestStore(t)
	req := LaunchRequest{Caller: frontendIdentityCaller("eve"), Kind: KindPTY, TemplateID: "shell"}
	if _, refusal := AuthorizeLaunch(store, NewModelKeyTable(), req); refusal != nil {
		t.Fatalf("eve's own frontend identity was refused: %+v", refusal)
	}
}

// ---------------------------------------------------------------------------
// Project
// ---------------------------------------------------------------------------

func TestAuthorizeLaunch_RemoteProjectRefuses(t *testing.T) {
	store := newLaunchTestStore(t)
	if err := store.With(func(s *config.Settings) {
		s.Projects = append(s.Projects, config.Project{
			ID: "remote-1", Name: "Remote", Kind: config.ProjectKindRemote,
			AllowedMcpIDs: []string{}, AllowedModels: []string{},
		})
	}); err != nil {
		t.Fatalf("store.With: %v", err)
	}
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: "remote-1", Kind: KindPTY, TemplateID: "shell"}
	_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), req)
	if refusal == nil {
		t.Fatal("a remote project was allowed to host a session launch")
	}
	if refusal.Status != 403 {
		t.Fatalf("status = %d, want 403", refusal.Status)
	}
}

func TestAuthorizeLaunch_UnknownProjectRefusesIdenticallyToRemote(t *testing.T) {
	store := newLaunchTestStore(t)
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: "does-not-exist", Kind: KindPTY, TemplateID: "shell"}
	_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), req)
	if refusal == nil {
		t.Fatal("an unknown project id was allowed to launch")
	}
	if refusal.Code != "project_not_available" {
		t.Fatalf("code = %q, want project_not_available (not found and remote must not be distinguishable)", refusal.Code)
	}
}

// ---------------------------------------------------------------------------
// Directory
// ---------------------------------------------------------------------------

func TestAuthorizeLaunch_DirectoryOutsideProjectRefuses(t *testing.T) {
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	outside := t.TempDir()

	req := LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID,
		Kind: KindPTY, TemplateID: "shell", Directory: outside,
	}
	_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), req)
	if refusal == nil {
		t.Fatal("a directory outside the project was accepted")
	}
	if refusal.Code != "directory_outside_project" {
		t.Fatalf("code = %q, want directory_outside_project", refusal.Code)
	}
}

func TestAuthorizeLaunch_SymlinkEscapeRefuses(t *testing.T) {
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	outside := t.TempDir()

	escape := filepath.Join(proj.Path, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	req := LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID,
		Kind: KindPTY, TemplateID: "shell", Directory: escape,
	}
	_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), req)
	if refusal == nil {
		t.Fatal("a symlink escaping the project directory was accepted")
	}
	if refusal.Code != "directory_outside_project" {
		t.Fatalf("code = %q, want directory_outside_project", refusal.Code)
	}
}

func TestAuthorizeLaunch_EmptyDirectoryDefaultsToProjectPath(t *testing.T) {
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "shell"}
	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), req)
	if refusal != nil {
		t.Fatalf("refused: %+v", refusal)
	}
	want, _ := filepath.EvalSymlinks(proj.Path)
	if result.Spec.Directory != want {
		t.Fatalf("directory = %q, want project path %q", result.Spec.Directory, want)
	}
}

// ---------------------------------------------------------------------------
// Template
// ---------------------------------------------------------------------------

func TestAuthorizeLaunch_UnknownTemplateRefuses(t *testing.T) {
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "no-such-template"}
	_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), req)
	if refusal == nil {
		t.Fatal("an unknown template id was accepted")
	}
	if refusal.Code != "template_not_allowed" {
		t.Fatalf("code = %q, want template_not_allowed", refusal.Code)
	}
}

func TestAuthorizeLaunch_ProjectScopedTemplateNotVisibleToOtherProject(t *testing.T) {
	store := newLaunchTestStore(t)
	addLaunchTestProject(t, store, func(p *config.Project) {
		p.ID = "owner"
		p.ShellTemplates = []config.ShellTemplate{{ID: "private-shell", Name: "Private", Command: "zsh"}}
	})
	other := addLaunchTestProject(t, store, func(p *config.Project) { p.ID = "other" })

	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: other.ID, Kind: KindPTY, TemplateID: "private-shell"}
	_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), req)
	if refusal == nil {
		t.Fatal("a template scoped to another project was accepted")
	}
}

func TestAuthorizeLaunch_BuiltinTemplateSucceeds(t *testing.T) {
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "claude-code"}
	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), req)
	if refusal != nil {
		t.Fatalf("refused: %+v", refusal)
	}
	if result.Spec.TemplateID != "claude-code" {
		t.Fatalf("template_id = %q, want claude-code", result.Spec.TemplateID)
	}
	if !result.AuditFields.Sandbox {
		t.Fatal("claude-code template's own Sandbox default (true) was not carried through")
	}
}

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

func TestAuthorizeLaunch_DisallowedModelRefuses(t *testing.T) {
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, func(p *config.Project) {
		p.AllowedModels = []string{"claude-sonnet-4.5"}
	})
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindClaude, Model: "gpt-5"}
	_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), req)
	if refusal == nil {
		t.Fatal("a disallowed model was accepted")
	}
	if refusal.Code != "model_not_allowed" {
		t.Fatalf("code = %q, want model_not_allowed", refusal.Code)
	}
}

func TestAuthorizeLaunch_AllowedModelSucceeds(t *testing.T) {
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, func(p *config.Project) {
		p.AllowedModels = []string{"claude-sonnet-4.5"}
	})
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindClaude, Model: "claude-sonnet-4.5"}
	if _, refusal := AuthorizeLaunch(store, NewModelKeyTable(), req); refusal != nil {
		t.Fatalf("an allowed model was refused: %+v", refusal)
	}
}

// ---------------------------------------------------------------------------
// Client cannot widen permission policy
// ---------------------------------------------------------------------------

func TestMergePermissionSettings_ClientCannotWidenToolPolicy(t *testing.T) {
	project := &config.PermissionPolicy{
		DefaultMode:  "plan",
		AllowedTools: []string{"Read"},
		DeniedTools:  []string{"Bash:rm *"},
	}
	clientSettings := []byte(`{"permissionPolicy":{"allowedTools":["Bash:*"],"deniedTools":[]},"permissionMode":"bypassPermissions"}`)

	merged, err := mergePermissionSettings(clientSettings, project)
	if err != nil {
		t.Fatalf("mergePermissionSettings: %v", err)
	}

	var out struct {
		PermissionPolicy struct {
			DefaultMode  string   `json:"defaultMode"`
			AllowedTools []string `json:"allowedTools"`
			DeniedTools  []string `json:"deniedTools"`
		} `json:"permissionPolicy"`
		PermissionMode string `json:"permissionMode"`
	}
	if err := json.Unmarshal(merged, &out); err != nil {
		t.Fatalf("Unmarshal merged settings: %v", err)
	}

	if len(out.PermissionPolicy.AllowedTools) != 1 || out.PermissionPolicy.AllowedTools[0] != "Read" {
		t.Fatalf("allowedTools = %v, want the project's own [Read] — client widened it", out.PermissionPolicy.AllowedTools)
	}
	if len(out.PermissionPolicy.DeniedTools) != 1 || out.PermissionPolicy.DeniedTools[0] != "Bash:rm *" {
		t.Fatalf("deniedTools = %v, want the project's own — client narrowed/cleared it", out.PermissionPolicy.DeniedTools)
	}
	// The client DID set permissionMode explicitly, so it survives — the
	// one axis eve lets the client control.
	if out.PermissionMode != "bypassPermissions" {
		t.Fatalf("permissionMode = %q, want the client's own explicit choice to survive", out.PermissionMode)
	}
}

func TestMergePermissionSettings_UnsetClientModeFallsBackToProjectDefault(t *testing.T) {
	project := &config.PermissionPolicy{DefaultMode: "plan"}
	merged, err := mergePermissionSettings(nil, project)
	if err != nil {
		t.Fatalf("mergePermissionSettings: %v", err)
	}
	var out struct {
		PermissionMode string `json:"permissionMode"`
	}
	if err := json.Unmarshal(merged, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.PermissionMode != "plan" {
		t.Fatalf("permissionMode = %q, want the project's defaultMode (plan) since the client set none", out.PermissionMode)
	}
}

func TestMergePermissionSettings_LiteralDefaultModeIsNeverForced(t *testing.T) {
	project := &config.PermissionPolicy{DefaultMode: "default"}
	merged, err := mergePermissionSettings(nil, project)
	if err != nil {
		t.Fatalf("mergePermissionSettings: %v", err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(merged, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, present := out["permissionMode"]; present {
		t.Fatalf("permissionMode was set from a literal \"default\" defaultMode, must stay absent")
	}
}

func TestMergePermissionSettings_NoProjectPolicyPassesClientSettingsThroughUnchanged(t *testing.T) {
	// This is the exact asymmetry eve's own handleCreateSession has: when
	// the project has no PermissionPolicy at all, the client's own
	// settings — including any permissionPolicy it supplied — are never
	// touched. Documented, not silently tightened.
	clientSettings := []byte(`{"permissionPolicy":{"allowedTools":["Bash:*"]},"voice":"aria"}`)
	merged, err := mergePermissionSettings(clientSettings, nil)
	if err != nil {
		t.Fatalf("mergePermissionSettings: %v", err)
	}
	if string(merged) != string(clientSettings) {
		// Key order may legitimately differ; compare decoded.
		var got, want map[string]json.RawMessage
		if err := json.Unmarshal(merged, &got); err != nil {
			t.Fatalf("Unmarshal got: %v", err)
		}
		if err := json.Unmarshal(clientSettings, &want); err != nil {
			t.Fatalf("Unmarshal want: %v", err)
		}
		if string(got["permissionPolicy"]) != string(want["permissionPolicy"]) {
			t.Fatalf("permissionPolicy = %s, want client's own %s unchanged", got["permissionPolicy"], want["permissionPolicy"])
		}
	}
}

func TestAuthorizeLaunch_ClientCannotWidenPolicyEndToEnd(t *testing.T) {
	store := newLaunchTestStore(t)
	policy := &config.PermissionPolicy{DefaultMode: "plan", AllowedTools: []string{"Read"}, DeniedTools: []string{"Bash:rm *"}}
	proj := addLaunchTestProject(t, store, func(p *config.Project) { p.PermissionPolicy = policy })

	clientSettings, _ := json.Marshal(map[string]any{
		"permissionPolicy": map[string]any{"allowedTools": []string{"Bash:*"}, "deniedTools": []string{}},
	})
	req := LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindChat,
		Model: "gpt-5", ClientSettings: clientSettings,
	}
	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), req)
	if refusal != nil {
		t.Fatalf("refused: %+v", refusal)
	}

	var body struct {
		Settings struct {
			PermissionPolicy struct {
				AllowedTools []string `json:"allowedTools"`
			} `json:"permissionPolicy"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(result.Spec.SessionRequest, &body); err != nil {
		t.Fatalf("Unmarshal session_request: %v", err)
	}
	if len(body.Settings.PermissionPolicy.AllowedTools) != 1 || body.Settings.PermissionPolicy.AllowedTools[0] != "Read" {
		t.Fatalf("allowedTools reaching the LaunchSpec = %v, want the project's own [Read]", body.Settings.PermissionPolicy.AllowedTools)
	}
}

// ---------------------------------------------------------------------------
// LaunchSpec shape per kind
// ---------------------------------------------------------------------------

func TestAuthorizeLaunch_PtyLaunchSpecGolden(t *testing.T) {
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	t.Setenv("SHELL", "")

	req := LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), SessionID: "11111111-1111-1111-1111-111111111111",
		ProjectID: proj.ID, Kind: KindPTY, TemplateID: "shell", Name: "term 1", Cols: 100, Rows: 30,
	}
	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), req)
	if refusal != nil {
		t.Fatalf("refused: %+v", refusal)
	}

	wantDir, _ := filepath.EvalSymlinks(proj.Path)
	projJSON, _ := json.Marshal(struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Path string `json:"path"`
	}{proj.ID, proj.Name, proj.Path})

	want := hostapi.LaunchRequest{
		V: 1, SessionID: "11111111-1111-1111-1111-111111111111", Kind: KindPTY, Resume: false,
		Project: projJSON, Directory: wantDir, Name: "term 1", TemplateID: "shell",
		Argv: []string{"/bin/zsh"}, IdleTimeoutSec: 1440 * 60,
		PTY: &hostapi.PTYSpec{Cols: 100, Rows: 30},
	}

	gotJSON, _ := json.Marshal(result.Spec)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("pty LaunchSpec mismatch:\n got  %s\n want %s", gotJSON, wantJSON)
	}
}

func TestAuthorizeLaunch_ClaudeLaunchSpecGolden(t *testing.T) {
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)

	req := LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), SessionID: "22222222-2222-2222-2222-222222222222",
		ProjectID: proj.ID, Kind: KindClaude, Model: "claude-sonnet-4.5", Name: "my session",
	}
	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), req)
	if refusal != nil {
		t.Fatalf("refused: %+v", refusal)
	}

	if result.Spec.ModelKey != "" {
		t.Fatalf("claude session minted a model key %q, want none", result.Spec.ModelKey)
	}
	if result.Spec.PTY != nil || result.Spec.Argv != nil || result.Spec.TemplateID != "" {
		t.Fatalf("claude LaunchSpec carries pty-only fields: %+v", result.Spec)
	}
	if result.Spec.Identity != nil {
		t.Fatal("Identity must stay nil out of AuthorizeLaunch — minting it is R-S4b's job")
	}
	if result.Spec.Sandbox != nil {
		t.Fatal("Sandbox must stay nil out of AuthorizeLaunch — R-S8's extension point")
	}
	if !result.AuditFields.Sandbox {
		t.Fatal("claude sessions must want sandboxing per C7's default table")
	}
	if result.Ledger == nil || result.Ledger.Kind != KindClaude || result.Ledger.State != "live" {
		t.Fatalf("ledger record = %+v, want a live claude record", result.Ledger)
	}

	wantDir, _ := filepath.EvalSymlinks(proj.Path)
	var body struct {
		ProjectID string `json:"projectId"`
		Directory string `json:"directory"`
		Name      string `json:"name"`
		Model     string `json:"model"`
	}
	if err := json.Unmarshal(result.Spec.SessionRequest, &body); err != nil {
		t.Fatalf("Unmarshal session_request: %v", err)
	}
	if body.ProjectID != proj.ID || body.Directory != wantDir || body.Name != "my session" || body.Model != "claude-sonnet-4.5" {
		t.Fatalf("session_request = %+v", body)
	}
}

func TestAuthorizeLaunch_PiSessionMintsModelKey(t *testing.T) {
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPi, Model: "gpt-5"}
	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), req)
	if refusal != nil {
		t.Fatalf("refused: %+v", refusal)
	}
	if !strings.HasPrefix(result.Spec.ModelKey, "rmk_") {
		t.Fatalf("pi session model_key = %q, want an rmk_-prefixed key", result.Spec.ModelKey)
	}
	if result.ModelKeyLabel != "session:"+result.SessionID {
		t.Fatalf("ModelKeyLabel = %q, want session:%s", result.ModelKeyLabel, result.SessionID)
	}
	if result.Ledger == nil || result.Ledger.Kind != KindPi {
		t.Fatalf("ledger record = %+v, want a pi record", result.Ledger)
	}
}

func TestAuthorizeLaunch_ChatSessionMintsModelKey(t *testing.T) {
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindChat, Model: "gpt-5"}
	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), req)
	if refusal != nil {
		t.Fatalf("refused: %+v", refusal)
	}
	if !strings.HasPrefix(result.Spec.ModelKey, "rmk_") {
		t.Fatalf("chat session model_key = %q, want an rmk_-prefixed key", result.Spec.ModelKey)
	}
}

func TestAuthorizeLaunch_PtyTemplateWithModelKeyMintsOne(t *testing.T) {
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "pi"}
	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), req)
	if refusal != nil {
		t.Fatalf("refused: %+v", refusal)
	}
	if !strings.HasPrefix(result.Spec.ModelKey, "rmk_") {
		t.Fatalf("pty template with model_key:true produced %q, want an rmk_ key", result.Spec.ModelKey)
	}
	if result.Ledger != nil {
		t.Fatal("a pty (terminal) launch must never be persisted to the ledger")
	}
}

func TestAuthorizeLaunch_PtyTemplateWithoutModelKeyMintsNone(t *testing.T) {
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "shell"}
	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), req)
	if refusal != nil {
		t.Fatalf("refused: %+v", refusal)
	}
	if result.Spec.ModelKey != "" {
		t.Fatalf("shell template minted a model key %q, want none", result.Spec.ModelKey)
	}
}

// ---------------------------------------------------------------------------
// Hosted (SSH) project
// ---------------------------------------------------------------------------

func TestAuthorizeLaunch_HostedProjectGetsHostSpecAndNoIdentity(t *testing.T) {
	store := newLaunchTestStore(t)
	if err := store.With(func(s *config.Settings) {
		s.Hosts = append(s.Hosts, config.Host{ID: "h1", Name: "devbox", Target: "devbox.example"})
	}); err != nil {
		t.Fatalf("store.With hosts: %v", err)
	}
	proj := addLaunchTestProject(t, store, func(p *config.Project) {
		p.HostID = "h1"
		p.Path = "/home/remote/project" // deliberately not a local path
	})

	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "shell"}
	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), req)
	if refusal != nil {
		t.Fatalf("refused: %+v", refusal)
	}
	if result.Spec.Host == nil {
		t.Fatal("hosted project produced a nil Host")
	}
	if result.Spec.Identity != nil {
		t.Fatal("SH §3.1: identity must be null for an SSH-hosted session")
	}
}
