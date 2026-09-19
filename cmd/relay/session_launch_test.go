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
	"github.com/barelyworkingcode/relay/internal/sessions/ledger"
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

// newLaunchTestLedger gives each test its own on-disk ledger, matching how
// AuthorizeLaunch and a real relay both use one ledger per ConfigDir.
func newLaunchTestLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	l, err := ledger.Open(t.TempDir())
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	return l
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
	sessions := newLaunchTestLedger(t)
	req := LaunchRequest{Caller: bearerCaller(control.ClassRead), Kind: KindPTY, TemplateID: "shell"}
	_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
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
	sessions := newLaunchTestLedger(t)
	req := LaunchRequest{Caller: bearerCaller(control.ClassProxy), Kind: KindPTY, TemplateID: "shell"}
	_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
	if refusal == nil {
		t.Fatal("bearer holding only proxy was allowed to launch")
	}
	if refusal.Status != 403 {
		t.Fatalf("status = %d, want 403", refusal.Status)
	}
}

func TestAuthorizeLaunch_EmptyClassesRefuses(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	req := LaunchRequest{Caller: bearerCaller(), Kind: KindPTY, TemplateID: "shell"}
	if _, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req); refusal == nil {
		t.Fatal("bearer with no classes at all was allowed to launch")
	}
}

func TestAuthorizeLaunch_NoCallerAtAllRefuses(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	req := LaunchRequest{Kind: KindPTY, TemplateID: "shell"}
	if _, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req); refusal == nil {
		t.Fatal("a request naming neither an identity nor a credential was allowed to launch")
	}
}

func TestAuthorizeLaunch_NonFrontendIdentityRefuses(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	// A service identity that never claimed the frontend capability (e.g. a
	// manifest-only service) must not be treated as eve.
	caller := LaunchCaller{Identity: &service.Identity{
		Kind: service.IdentityKindService, Name: "relayLLM",
		Capabilities: []config.ServiceCapability{config.ServiceCapabilityManifest},
	}}
	req := LaunchRequest{Caller: caller, Kind: KindPTY, TemplateID: "shell"}
	if _, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req); refusal == nil {
		t.Fatal("a manifest-only identity was allowed to launch")
	}
}

func TestAuthorizeLaunch_ExecuteBearerAdHocPtySucceeds(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), Kind: KindPTY, TemplateID: "shell"}
	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
	if refusal != nil {
		t.Fatalf("valid ad-hoc pty launch refused: %+v", refusal)
	}
	if result.Spec.Project != nil {
		t.Fatalf("ad-hoc launch carries a project: %s", result.Spec.Project)
	}
}

func TestAuthorizeLaunch_FrontendIdentitySucceeds(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	req := LaunchRequest{Caller: frontendIdentityCaller("eve"), Kind: KindPTY, TemplateID: "shell"}
	if _, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req); refusal != nil {
		t.Fatalf("eve's own frontend identity was refused: %+v", refusal)
	}
}

// ---------------------------------------------------------------------------
// Project
// ---------------------------------------------------------------------------

func TestAuthorizeLaunch_RemoteProjectRefuses(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	if err := store.With(func(s *config.Settings) {
		s.Projects = append(s.Projects, config.Project{
			ID: "remote-1", Name: "Remote", Kind: config.ProjectKindRemote,
			AllowedMcpIDs: []string{}, AllowedModels: []string{},
		})
	}); err != nil {
		t.Fatalf("store.With: %v", err)
	}
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: "remote-1", Kind: KindPTY, TemplateID: "shell"}
	_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
	if refusal == nil {
		t.Fatal("a remote project was allowed to host a session launch")
	}
	if refusal.Status != 403 {
		t.Fatalf("status = %d, want 403", refusal.Status)
	}
}

func TestAuthorizeLaunch_UnknownProjectRefusesIdenticallyToRemote(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: "does-not-exist", Kind: KindPTY, TemplateID: "shell"}
	_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
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
	sessions := newLaunchTestLedger(t)
	proj := addLaunchTestProject(t, store, nil)
	outside := t.TempDir()

	req := LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID,
		Kind: KindPTY, TemplateID: "shell", Directory: outside,
	}
	_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
	if refusal == nil {
		t.Fatal("a directory outside the project was accepted")
	}
	if refusal.Code != "directory_outside_project" {
		t.Fatalf("code = %q, want directory_outside_project", refusal.Code)
	}
}

func TestAuthorizeLaunch_SymlinkEscapeRefuses(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
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
	_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
	if refusal == nil {
		t.Fatal("a symlink escaping the project directory was accepted")
	}
	if refusal.Code != "directory_outside_project" {
		t.Fatalf("code = %q, want directory_outside_project", refusal.Code)
	}
}

func TestAuthorizeLaunch_EmptyDirectoryDefaultsToProjectPath(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	proj := addLaunchTestProject(t, store, nil)
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "shell"}
	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
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
	sessions := newLaunchTestLedger(t)
	proj := addLaunchTestProject(t, store, nil)
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "no-such-template"}
	_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
	if refusal == nil {
		t.Fatal("an unknown template id was accepted")
	}
	if refusal.Code != "template_not_allowed" {
		t.Fatalf("code = %q, want template_not_allowed", refusal.Code)
	}
}

func TestAuthorizeLaunch_ProjectScopedTemplateNotVisibleToOtherProject(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	addLaunchTestProject(t, store, func(p *config.Project) {
		p.ID = "owner"
		p.ShellTemplates = []config.ShellTemplate{{ID: "private-shell", Name: "Private", Command: "zsh"}}
	})
	other := addLaunchTestProject(t, store, func(p *config.Project) { p.ID = "other" })

	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: other.ID, Kind: KindPTY, TemplateID: "private-shell"}
	_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
	if refusal == nil {
		t.Fatal("a template scoped to another project was accepted")
	}
}

func TestAuthorizeLaunch_BuiltinTemplateSucceeds(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	proj := addLaunchTestProject(t, store, nil)
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "claude-code"}
	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
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
	sessions := newLaunchTestLedger(t)
	proj := addLaunchTestProject(t, store, func(p *config.Project) {
		p.AllowedModels = []string{"claude-sonnet-4.5"}
	})
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindClaude, Model: "gpt-5"}
	_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
	if refusal == nil {
		t.Fatal("a disallowed model was accepted")
	}
	if refusal.Code != "model_not_allowed" {
		t.Fatalf("code = %q, want model_not_allowed", refusal.Code)
	}
}

func TestAuthorizeLaunch_AllowedModelSucceeds(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	proj := addLaunchTestProject(t, store, func(p *config.Project) {
		p.AllowedModels = []string{"claude-sonnet-4.5"}
	})
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindClaude, Model: "claude-sonnet-4.5"}
	if _, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req); refusal != nil {
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
	sessions := newLaunchTestLedger(t)
	policy := &config.PermissionPolicy{DefaultMode: "plan", AllowedTools: []string{"Read"}, DeniedTools: []string{"Bash:rm *"}}
	proj := addLaunchTestProject(t, store, func(p *config.Project) { p.PermissionPolicy = policy })

	clientSettings, _ := json.Marshal(map[string]any{
		"permissionPolicy": map[string]any{"allowedTools": []string{"Bash:*"}, "deniedTools": []string{}},
	})
	req := LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindChat,
		Model: "gpt-5", ClientSettings: clientSettings,
	}
	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
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
	sessions := newLaunchTestLedger(t)
	proj := addLaunchTestProject(t, store, nil)
	t.Setenv("SHELL", "")

	req := LaunchRequest{
		// A fresh launch never names its own session id (F1) — the golden
		// value below is what AuthorizeLaunch itself minted, not an input.
		Caller:    bearerCaller(control.ClassExecute),
		ProjectID: proj.ID, Kind: KindPTY, TemplateID: "shell", Name: "term 1", Cols: 100, Rows: 30,
	}
	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
	if refusal != nil {
		t.Fatalf("refused: %+v", refusal)
	}
	if result.SessionID == "" {
		t.Fatal("AuthorizeLaunch did not mint a session id for a fresh launch")
	}

	wantDir, _ := filepath.EvalSymlinks(proj.Path)
	projJSON, _ := json.Marshal(struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Path string `json:"path"`
	}{proj.ID, proj.Name, proj.Path})

	want := hostapi.LaunchRequest{
		V: 1, SessionID: result.SessionID, Kind: KindPTY, Resume: false,
		Project: projJSON, Directory: wantDir, Name: "term 1", TemplateID: "shell",
		Argv: []string{"/bin/zsh"}, IdleTimeoutSec: 1440 * 60,
		PTY:     &hostapi.PTYSpec{Cols: 100, Rows: 30},
		Sandbox: &hostapi.SandboxSpec{ProfilePath: filepath.Join(sessionProfilesDir(), result.SessionID+".sb")},
		Env:     map[string]string{"TERM": "xterm-256color", "COLORTERM": "truecolor"},
	}

	gotJSON, _ := json.Marshal(result.Spec)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("pty LaunchSpec mismatch:\n got  %s\n want %s", gotJSON, wantJSON)
	}
}

func TestAuthorizeLaunch_ClaudeLaunchSpecGolden(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	proj := addLaunchTestProject(t, store, nil)

	req := LaunchRequest{
		// A fresh launch never names its own session id (F1); AuthorizeLaunch
		// mints one and it's asserted against below via result.SessionID.
		Caller:    bearerCaller(control.ClassExecute),
		ProjectID: proj.ID, Kind: KindClaude, Model: "claude-sonnet-4.5", Name: "my session",
	}
	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
	if refusal != nil {
		t.Fatalf("refused: %+v", refusal)
	}
	if result.SessionID == "" {
		t.Fatal("AuthorizeLaunch did not mint a session id for a fresh launch")
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
	// A claude session is sandboxed by default (C7), so AuthorizeLaunch has
	// already written the profile the shim will hand sandbox-exec; what that
	// file contains is session_sandbox_test.go's subject.
	if result.Spec.Sandbox == nil || !filepath.IsAbs(result.Spec.Sandbox.ProfilePath) {
		t.Fatalf("Sandbox = %+v, want an absolute profile path", result.Spec.Sandbox)
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
	sessions := newLaunchTestLedger(t)
	proj := addLaunchTestProject(t, store, nil)
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPi, Model: "gpt-5"}
	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
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
	sessions := newLaunchTestLedger(t)
	proj := addLaunchTestProject(t, store, nil)
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindChat, Model: "gpt-5"}
	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
	if refusal != nil {
		t.Fatalf("refused: %+v", refusal)
	}
	if !strings.HasPrefix(result.Spec.ModelKey, "rmk_") {
		t.Fatalf("chat session model_key = %q, want an rmk_-prefixed key", result.Spec.ModelKey)
	}
}

func TestAuthorizeLaunch_PtyTemplateWithModelKeyMintsOne(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	proj := addLaunchTestProject(t, store, nil)
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "pi"}
	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
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
	sessions := newLaunchTestLedger(t)
	proj := addLaunchTestProject(t, store, nil)
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "shell"}
	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
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
	sessions := newLaunchTestLedger(t)
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
	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
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

func TestAuthorizeLaunch_HostedProjectDirectoryIsLexicalNotLocalRealpath(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	if err := store.With(func(s *config.Settings) {
		s.Hosts = append(s.Hosts, config.Host{ID: "h1", Name: "devbox", Target: "devbox.example"})
	}); err != nil {
		t.Fatalf("store.With hosts: %v", err)
	}
	// /tmp is a real local symlink to /private/tmp on macOS; a hosted
	// project's directory must never be resolved against relay's own
	// filesystem, so the result must be the lexically-cleaned "/tmp", never
	// realpath's local rewrite "/private/tmp".
	proj := addLaunchTestProject(t, store, func(p *config.Project) {
		p.HostID = "h1"
		p.Path = "/tmp"
	})

	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "shell"}
	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
	if refusal != nil {
		t.Fatalf("refused: %+v", refusal)
	}
	if result.Spec.Directory != "/tmp" {
		t.Fatalf("directory = %q, want the lexically-cleaned \"/tmp\" (a local realpath rewrite leaked relay's own filesystem into a remote path)", result.Spec.Directory)
	}
}

func TestAuthorizeLaunch_HostedProjectDirectoryOutsideProjectRefusesLexically(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	if err := store.With(func(s *config.Settings) {
		s.Hosts = append(s.Hosts, config.Host{ID: "h1", Name: "devbox", Target: "devbox.example"})
	}); err != nil {
		t.Fatalf("store.With hosts: %v", err)
	}
	proj := addLaunchTestProject(t, store, func(p *config.Project) {
		p.HostID = "h1"
		p.Path = "/home/remote/project"
	})

	req := LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "shell",
		Directory: "/home/remote/other",
	}
	_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
	if refusal == nil {
		t.Fatal("a hosted directory outside the project was accepted")
	}
	if refusal.Code != "directory_outside_project" {
		t.Fatalf("code = %q, want directory_outside_project", refusal.Code)
	}
}

// ---------------------------------------------------------------------------
// Ad-hoc scope (F2): SH §3.1 sanctions no-project launches for terminals
// only, never claude/pi/chat.
// ---------------------------------------------------------------------------

func TestAuthorizeLaunch_AdHocClaudeRefuses(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), Kind: KindClaude, Model: "claude-sonnet-4.5"}
	_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
	if refusal == nil {
		t.Fatal("an ad-hoc (no-project) claude launch was accepted")
	}
	if refusal.Code != "project_required" {
		t.Fatalf("code = %q, want project_required", refusal.Code)
	}
	if refusal.Status != 403 {
		t.Fatalf("status = %d, want 403", refusal.Status)
	}
}

func TestAuthorizeLaunch_AdHocPiAndChatRefuse(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	for _, kind := range []string{KindPi, KindChat} {
		req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), Kind: kind, Model: "gpt-5"}
		_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
		if refusal == nil {
			t.Fatalf("an ad-hoc (no-project) %s launch was accepted", kind)
		}
	}
}

func TestAuthorizeLaunch_AdHocPtyStillSucceeds(t *testing.T) {
	// F2 narrows the ad-hoc exemption to terminals; this confirms it still
	// applies there, alongside TestAuthorizeLaunch_ExecuteBearerAdHocPtySucceeds.
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), Kind: KindPTY, TemplateID: "shell"}
	if _, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req); refusal != nil {
		t.Fatalf("an ad-hoc pty launch was refused: %+v", refusal)
	}
}

// ---------------------------------------------------------------------------
// Session id / resume (F1)
// ---------------------------------------------------------------------------

func TestAuthorizeLaunch_FreshLaunchNamingExistingLiveSessionIDRefuses(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	proj := addLaunchTestProject(t, store, nil)

	// The attack: a fresh launch reuses a live session id, so
	// ModelKeyTable.Revoke(projectID, "session:<id>") later deletes both
	// the squatter's and the victim's key, since Revoke keys on the label
	// alone.
	victim := ledger.Record{SessionID: "victim-session", Kind: KindChat, ProjectID: proj.ID, State: ledger.StateLive}
	if err := sessions.Put(victim); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}

	req := LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), SessionID: "victim-session", Resume: false,
		ProjectID: proj.ID, Kind: KindChat, Model: "gpt-5",
	}
	_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
	if refusal == nil {
		t.Fatal("a fresh (non-resume) launch reusing a live session id was accepted")
	}
	if refusal.Code != "session_id_not_allowed" {
		t.Fatalf("code = %q, want session_id_not_allowed", refusal.Code)
	}
}

func TestAuthorizeLaunch_FreshLaunchNamingAnySessionIDRefusesEvenIfUnused(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	proj := addLaunchTestProject(t, store, nil)

	req := LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), SessionID: "caller-picked-id", Resume: false,
		ProjectID: proj.ID, Kind: KindChat, Model: "gpt-5",
	}
	_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
	if refusal == nil {
		t.Fatal("a fresh launch naming its own session id was accepted")
	}
}

func TestAuthorizeLaunch_ResumeNamingDifferentProjectThanOwnerRefuses(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	owner := addLaunchTestProject(t, store, func(p *config.Project) { p.ID = "owner-project" })
	attacker := addLaunchTestProject(t, store, func(p *config.Project) { p.ID = "attacker-project" })

	// The attack: Resume:true names project A's session id, but the
	// request's own ProjectID claims project B.
	rec := ledger.Record{SessionID: "s1", Kind: KindChat, ProjectID: owner.ID, State: ledger.StateDormant}
	if err := sessions.Put(rec); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}

	req := LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), SessionID: "s1", Resume: true,
		ProjectID: attacker.ID, Kind: KindChat, Model: "gpt-5",
	}
	_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
	if refusal == nil {
		t.Fatal("resuming project A's session under project B's id was accepted")
	}
	if refusal.Code != "session_not_resumable" {
		t.Fatalf("code = %q, want session_not_resumable", refusal.Code)
	}
}

func TestAuthorizeLaunch_ResumeLiveSessionRefuses(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	proj := addLaunchTestProject(t, store, nil)

	rec := ledger.Record{SessionID: "s1", Kind: KindChat, ProjectID: proj.ID, State: ledger.StateLive}
	if err := sessions.Put(rec); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}

	req := LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), SessionID: "s1", Resume: true,
		ProjectID: proj.ID, Kind: KindChat, Model: "gpt-5",
	}
	_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
	if refusal == nil {
		t.Fatal("resuming an already-live session through AuthorizeLaunch was accepted")
	}
}

func TestAuthorizeLaunch_ResumeUnknownSessionRefuses(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	proj := addLaunchTestProject(t, store, nil)

	req := LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), SessionID: "does-not-exist", Resume: true,
		ProjectID: proj.ID, Kind: KindChat, Model: "gpt-5",
	}
	_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
	if refusal == nil {
		t.Fatal("resuming an unknown session id was accepted")
	}
}

func TestAuthorizeLaunch_ResumeDormantSameProjectSucceeds(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	proj := addLaunchTestProject(t, store, nil)

	rec := ledger.Record{SessionID: "s1", Kind: KindChat, ProjectID: proj.ID, State: ledger.StateDormant}
	if err := sessions.Put(rec); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}

	req := LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), SessionID: "s1", Resume: true,
		ProjectID: proj.ID, Kind: KindChat, Model: "gpt-5",
	}
	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
	if refusal != nil {
		t.Fatalf("a legitimate resume of a dormant session in its own project was refused: %+v", refusal)
	}
	if result.SessionID != "s1" {
		t.Fatalf("SessionID = %q, want the resumed id s1 unchanged", result.SessionID)
	}
	if !result.Spec.Resume {
		t.Fatal("Spec.Resume was not carried through for a resume request")
	}
}

// TestResolveTemplateEnv_TerminalDefaults pins that a pty always gets a real
// TERM. Relay runs with none, and a child that inherits that cannot redraw on
// backspace; the template's own value, from env or passthrough, still wins.
func TestResolveTemplateEnv_TerminalDefaults(t *testing.T) {
	t.Run("bare template gets both defaults", func(t *testing.T) {
		got := resolveTemplateEnv(config.TerminalTemplate{})
		if got["TERM"] != "xterm-256color" || got["COLORTERM"] != "truecolor" {
			t.Fatalf("env = %v, want TERM=xterm-256color and COLORTERM=truecolor", got)
		}
	})

	t.Run("template env wins", func(t *testing.T) {
		got := resolveTemplateEnv(config.TerminalTemplate{Env: map[string]string{"TERM": "vt100", "FOO": "bar"}})
		if got["TERM"] != "vt100" || got["FOO"] != "bar" || got["COLORTERM"] != "truecolor" {
			t.Fatalf("env = %v, want the template's TERM kept and COLORTERM defaulted", got)
		}
	})

	t.Run("passthrough wins", func(t *testing.T) {
		t.Setenv("TERM", "screen-256color")
		got := resolveTemplateEnv(config.TerminalTemplate{EnvPassthrough: []string{"TERM"}})
		if got["TERM"] != "screen-256color" {
			t.Fatalf("TERM = %q, want the passed-through value", got["TERM"])
		}
	})
}
