package main

// R-S4a hermetic tests: SH §3.2's authorization matrix, each check asserted
// to refuse independently (not just "a fully valid request succeeds"), plus
// the LaunchSpec builder's shape per kind and the permission-policy merge's
// exact client/project asymmetry.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
	seedTestTemplates(t, store)
	return store
}

// testTerminalTemplates is the set a launch test finds in settings.json. Relay
// computes no templates in code, so a test that launches one has to put it
// there. They mirror what a real install holds: a shell with the whole home
// directory, Claude Code and pi with only the folders they need, and one
// template that opts out of the sandbox.
func testTerminalTemplates() []config.TerminalTemplate {
	dotfiles := []string{"~/.gitconfig", "~/.zshenv", "~/.zprofile", "~/.zshrc"}
	return []config.TerminalTemplate{
		{
			ID: "shell", Name: "Shell", Icon: "shell", Description: "Default system shell",
			Sandbox: true, ReadWrite: []string{"~"},
		},
		{
			ID: "claude-code", Name: "Claude Code", Command: "claude", Icon: "terminal", Description: "Claude Code CLI agent",
			EnvPassthrough: []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY"},
			Sandbox:        true,
			ReadWrite:      []string{"~/.claude", "~/.claude.json", "~/.cache", "~/.npm", "~/go/pkg", "~/Library/Caches", "/private/tmp/cc-socks"},
			Read:           append([]string{"~/Library/Keychains", "/opt/homebrew", "~/.local/bin", "~/.local/share/claude"}, dotfiles...),
		},
		{
			ID: "pi", Name: "pi", Command: "pi", Icon: "terminal", Description: "pi coding agent",
			Sandbox: true, ModelKey: true,
			ReadWrite: []string{"~/.pi", "~/.cache", "~/.npm", "~/go/pkg", "~/Library/Caches"},
			Read:      append([]string{"/opt/homebrew", "~/.bun/bin", "~/.bun/install/global"}, dotfiles...),
		},
		{ID: "plain", Name: "Plain", Icon: "terminal", Description: "A template that opts out of the sandbox"},
	}
}

func seedTestTemplates(t *testing.T, store config.SettingsStore) {
	t.Helper()
	if err := store.With(func(s *config.Settings) { s.TerminalTemplates = testTerminalTemplates() }); err != nil {
		t.Fatalf("seed terminal templates: %v", err)
	}
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
		// Every launch test that is not about the template list gets every
		// template; an empty list would refuse them all.
		AllowedTemplates: []string{"*"},
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

func TestAuthorizeLaunch_ExecuteBearerAdHocPtyRefuses(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), Kind: KindPTY, TemplateID: "shell"}
	_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
	if refusal == nil || refusal.Code != "project_required" {
		t.Fatalf("an ad-hoc pty launch was not refused with project_required: %+v", refusal)
	}
}

func TestAuthorizeLaunch_FrontendIdentitySucceeds(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	proj := addLaunchTestProject(t, store, nil)
	req := LaunchRequest{Caller: frontendIdentityCaller("eve"), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "shell"}
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

// The project's allowed_templates gates every kind: an unlisted terminal
// template and the claude session's own kind template are both refused.
func TestAuthorizeLaunch_TemplateNotInProjectListRefuses(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	proj := addLaunchTestProject(t, store, func(p *config.Project) { p.AllowedTemplates = []string{"shell"} })

	for name, req := range map[string]LaunchRequest{
		"unlisted pty":        {Kind: KindPTY, TemplateID: "claude-code"},
		"claude, no template": {Kind: KindClaude, Model: "claude-sonnet-4.5"},
	} {
		req.Caller, req.ProjectID = bearerCaller(control.ClassExecute), proj.ID
		if _, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req); refusal == nil || refusal.Code != "template_not_allowed" {
			t.Errorf("%s: refusal = %+v, want template_not_allowed", name, refusal)
		}
	}
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "shell"}
	if _, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req); refusal != nil {
		t.Fatalf("a listed template was refused: %+v", refusal)
	}
}

func TestAuthorizeLaunch_EmptyTemplateListRefusesEverything(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	proj := addLaunchTestProject(t, store, func(p *config.Project) { p.AllowedTemplates = []string{} })
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "shell"}
	if _, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req); refusal == nil || refusal.Code != "template_not_allowed" {
		t.Fatalf("refusal = %+v, want template_not_allowed", refusal)
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

// addLaunchTestHostedProject saves host h1 with templates and a project p1
// on it. The project allows every template, so any refusal is the host's.
func addLaunchTestHostedProject(t *testing.T, store config.SettingsStore, templates []config.TerminalTemplate) config.Project {
	t.Helper()
	if err := store.With(func(s *config.Settings) {
		s.Hosts = append(s.Hosts, config.Host{ID: "h1", Name: "devbox", Target: "devbox.example", TerminalTemplates: templates})
	}); err != nil {
		t.Fatalf("store.With hosts: %v", err)
	}
	return addLaunchTestProject(t, store, func(p *config.Project) {
		p.HostID = "h1"
		p.Path = "/home/remote/project"
	})
}

func TestAuthorizeLaunch_HostedPtyArgvComesFromTheHostTemplate(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	proj := addLaunchTestHostedProject(t, store, []config.TerminalTemplate{
		{ID: "shell", Name: "Shell"},
		{ID: "tool", Name: "Tool", Command: "/opt/tool", Args: []string{"--project", "${PROJECT_ID}"}},
	})
	t.Setenv("SHELL", "/bin/console-shell")

	launch := func(tid string) []string {
		t.Helper()
		req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: tid}
		result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
		if refusal != nil {
			t.Fatalf("%s: refused: %+v", tid, refusal)
		}
		return result.Spec.Argv
	}
	if argv := launch("shell"); argv != nil {
		t.Fatalf("hosted shell template: argv = %q, want nil (the host's login shell), never the console's $SHELL", argv)
	}
	if got, want := strings.Join(launch("tool"), " "), "/opt/tool --project "+proj.ID; got != want {
		t.Fatalf("hosted command template: argv = %q, want %q", got, want)
	}

	// A console project's empty-command template still gets the console shell.
	if err := store.With(func(s *config.Settings) {
		s.Projects = append(s.Projects, config.Project{ID: "local", Name: "Local", Path: t.TempDir(), AllowedTemplates: []string{"*"}, AllowedModels: []string{"*"}})
	}); err != nil {
		t.Fatalf("store.With: %v", err)
	}
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: "local", Kind: KindPTY, TemplateID: "plain"}
	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
	if refusal != nil {
		t.Fatalf("console plain: refused: %+v", refusal)
	}
	if got := strings.Join(result.Spec.Argv, " "); got != "/bin/console-shell" {
		t.Fatalf("console plain: argv = %q, want defaultShell()", got)
	}
}

// A hosted project is offered its host's templates only: a console template
// id is not allowed even though allowed_templates is "*".
func TestAuthorizeLaunch_HostedProjectRefusesAConsoleTemplate(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	proj := addLaunchTestHostedProject(t, store, []config.TerminalTemplate{{ID: "shell", Name: "Shell"}})
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "plain"}
	if _, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req); refusal == nil || refusal.Code != "template_not_allowed" {
		t.Fatalf("refusal = %+v, want template_not_allowed", refusal)
	}
}

func TestAuthorizeLaunch_HostedKindGates(t *testing.T) {
	for name, c := range map[string]struct {
		templates []config.TerminalTemplate
		kind      string
		model     string
		wantCode  string // "" means allowed
	}{
		"claude, host has claude-code": {[]config.TerminalTemplate{{ID: "claude-code", Name: "Claude Code", Command: "claude"}}, KindClaude, "claude-sonnet-4.5", ""},
		"claude, host has only shell":  {[]config.TerminalTemplate{{ID: "shell", Name: "Shell"}}, KindClaude, "claude-sonnet-4.5", "template_not_allowed"},
		"pi on a host":                 {[]config.TerminalTemplate{{ID: "pi", Name: "pi", Command: "pi"}}, KindPi, "gpt-5", "provider_not_available_on_host"},
	} {
		t.Run(name, func(t *testing.T) {
			store := newLaunchTestStore(t)
			sessions := newLaunchTestLedger(t)
			proj := addLaunchTestHostedProject(t, store, c.templates)
			req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: c.kind, Model: c.model}
			_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
			switch {
			case c.wantCode == "" && refusal != nil:
				t.Fatalf("refused: %+v", refusal)
			case c.wantCode != "" && (refusal == nil || refusal.Code != c.wantCode):
				t.Fatalf("refusal = %+v, want %s", refusal, c.wantCode)
			}
		})
	}
}

func TestAuthorizeLaunch_HostedProjectGetsHostSpecAndNoIdentity(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	if err := store.With(func(s *config.Settings) {
		s.Hosts = append(s.Hosts, config.Host{ID: "h1", Name: "devbox", Target: "devbox.example", TerminalTemplates: []config.TerminalTemplate{{ID: "shell", Name: "Shell"}}})
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
		s.Hosts = append(s.Hosts, config.Host{ID: "h1", Name: "devbox", Target: "devbox.example", TerminalTemplates: []config.TerminalTemplate{{ID: "shell", Name: "Shell"}}})
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
		got, err := resolveTemplateEnv(config.TerminalTemplate{}, &config.Settings{})
		if err != nil {
			t.Fatalf("resolveTemplateEnv: %v", err)
		}
		if got["TERM"] != "xterm-256color" || got["COLORTERM"] != "truecolor" {
			t.Fatalf("env = %v, want TERM=xterm-256color and COLORTERM=truecolor", got)
		}
	})

	t.Run("template env wins", func(t *testing.T) {
		got, err := resolveTemplateEnv(config.TerminalTemplate{Env: map[string]string{"TERM": "vt100", "FOO": "bar"}}, &config.Settings{})
		if err != nil {
			t.Fatalf("resolveTemplateEnv: %v", err)
		}
		if got["TERM"] != "vt100" || got["FOO"] != "bar" || got["COLORTERM"] != "truecolor" {
			t.Fatalf("env = %v, want the template's TERM kept and COLORTERM defaulted", got)
		}
	})

	t.Run("passthrough wins", func(t *testing.T) {
		t.Setenv("TERM", "screen-256color")
		got, err := resolveTemplateEnv(config.TerminalTemplate{EnvPassthrough: []string{"TERM"}}, &config.Settings{})
		if err != nil {
			t.Fatalf("resolveTemplateEnv: %v", err)
		}
		if got["TERM"] != "screen-256color" {
			t.Fatalf("TERM = %q, want the passed-through value", got["TERM"])
		}
	})
}

// TestResolveTemplateEnv_ModelEndpointURL pins the one substitution relay fills
// in itself: the model endpoint's URL, from model_endpoint.listen.
func TestResolveTemplateEnv_ModelEndpointURL(t *testing.T) {
	tmpl := config.TerminalTemplate{
		ID: "mapped", ModelKey: true,
		Env: map[string]string{
			"ANTHROPIC_BASE_URL":       config.ModelEndpointURLMarker,
			"ANTHROPIC_CUSTOM_HEADERS": "X-Relay-Key: " + config.ModelKeyMarker,
		},
	}

	t.Run("listener on", func(t *testing.T) {
		settings := &config.Settings{ModelEndpoint: &config.ModelEndpointConfig{Listen: "127.0.0.1:8180"}}
		got, err := resolveTemplateEnv(tmpl, settings)
		if err != nil {
			t.Fatalf("resolveTemplateEnv: %v", err)
		}
		if got["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:8180" {
			t.Fatalf("ANTHROPIC_BASE_URL = %q, want http://127.0.0.1:8180", got["ANTHROPIC_BASE_URL"])
		}
		if got["ANTHROPIC_CUSTOM_HEADERS"] != "X-Relay-Key: "+config.ModelKeyMarker {
			t.Fatalf("relay altered the key mapping, which relay-sessions expands at spawn: %q", got["ANTHROPIC_CUSTOM_HEADERS"])
		}
	})

	for name, settings := range map[string]*config.Settings{
		"no model_endpoint block":     {},
		"listener explicitly cleared": {ModelEndpoint: &config.ModelEndpointConfig{}},
		"nil settings":                nil,
	} {
		t.Run("listener off, "+name, func(t *testing.T) {
			if got, err := resolveTemplateEnv(tmpl, settings); err == nil {
				t.Fatalf("resolved %v with no listener; it must refuse, or the ${MODEL_KEY} header reaches the client's real provider", got)
			}
		})
	}
}

// TestAuthorizeLaunch_RefusesAMappedTemplateWithNoListener pins that the
// refusal reaches the caller with a reason, and leaves nothing behind: no
// model key minted, no profile written.
func TestAuthorizeLaunch_RefusesAMappedTemplateWithNoListener(t *testing.T) {
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	if err := store.With(func(s *config.Settings) {
		s.TerminalTemplates = append(s.TerminalTemplates, config.TerminalTemplate{
			ID: "mapped", Name: "Mapped", Command: "claude", Sandbox: true, ModelKey: true,
			Env: map[string]string{"ANTHROPIC_BASE_URL": config.ModelEndpointURLMarker, "H": config.ModelKeyMarker},
		})
	}); err != nil {
		t.Fatalf("store.With: %v", err)
	}

	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), newLaunchTestLedger(t), LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "mapped",
	})
	if result != nil || refusal == nil {
		t.Fatalf("launch was authorized with no model endpoint listener: %+v", result)
	}
	if refusal.Code != "model_endpoint_unavailable" {
		t.Fatalf("refusal code = %q, want model_endpoint_unavailable", refusal.Code)
	}
	if entries, err := os.ReadDir(sessionProfilesDir()); err == nil && len(entries) != 0 {
		t.Fatalf("a refused launch left %d profile(s) behind", len(entries))
	}
}

// persistLaunchProjectID is uuid-shaped: a persist session name carries the
// first 8 characters of the project id, and a shorter id yields a name
// ParsePersistSessionName refuses.
const persistLaunchProjectID = "0123abcd-1111-2222-3333-444455556666"

// persistLaunchTemplates is a host's templates: one persist command, one
// persist login shell, one ordinary command.
func persistLaunchTemplates() []config.TerminalTemplate {
	return []config.TerminalTemplate{
		{ID: "claude", Name: "Claude", Command: "claude", Args: []string{"--project", "${PROJECT_ID}"}, Persist: true},
		{ID: "shell", Name: "Shell", Persist: true},
		{ID: "tool", Name: "Tool", Command: "/opt/tool"},
	}
}

// addPersistLaunchTestProject is addLaunchTestHostedProject with a uuid
// project id and a host whose tmux is hostTmux (override) or probeTmux.
func addPersistLaunchTestProject(t *testing.T, store config.SettingsStore, hostTmux, probeTmux string) config.Project {
	t.Helper()
	if err := store.With(func(s *config.Settings) {
		s.Hosts = append(s.Hosts, config.Host{ID: "h1", Name: "devbox", Target: "devbox.example",
			TmuxPath: hostTmux, Probe: &config.HostProbe{OK: true, TmuxPath: probeTmux},
			TerminalTemplates: persistLaunchTemplates()})
	}); err != nil {
		t.Fatalf("store.With hosts: %v", err)
	}
	return addLaunchTestProject(t, store, func(p *config.Project) {
		p.ID = persistLaunchProjectID
		p.HostID = "h1"
		p.Path = "/home/remote/project"
	})
}

// stubPersistSessionNames replaces the host's tmux listing for one test and
// counts the calls.
func stubPersistSessionNames(t *testing.T, names []string, err error) *int {
	t.Helper()
	calls := 0
	prev := listPersistSessionNames
	listPersistSessionNames = func(context.Context, config.Host) ([]string, error) {
		calls++
		return names, err
	}
	t.Cleanup(func() { listPersistSessionNames = prev })
	return &calls
}

func persistLaunch(t *testing.T, store config.SettingsStore, templateID, persistSession string) (*LaunchResult, *LaunchRefusal) {
	t.Helper()
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: persistLaunchProjectID, Kind: KindPTY,
		TemplateID: templateID, PersistSession: persistSession}
	return AuthorizeLaunch(store, NewModelKeyTable(), newLaunchTestLedger(t), req)
}

// A fresh persist launch takes the next n among this project's and
// template's sessions on the host, runs the command inside
// `tmux new-session -A -s <name>`, and names the terminal after the session
// (what attached_here matches on).
func TestAuthorizeLaunch_PersistTemplateMintsTheNextSessionName(t *testing.T) {
	store := newLaunchTestStore(t)
	proj := addPersistLaunchTestProject(t, store, "", "/usr/bin/tmux")
	stubPersistSessionNames(t, []string{
		"relay-0123abcd-claude-1",
		"relay-0123abcd-claude-4",
		"relay-0123abcd-shell-9",  // other template
		"relay-ffffffff-claude-7", // other project
	}, nil)

	result, refusal := persistLaunch(t, store, "claude", "")
	if refusal != nil {
		t.Fatalf("refused: %+v", refusal)
	}
	const wantName = "relay-0123abcd-claude-5"
	want := []string{"/usr/bin/tmux", "new-session", "-A", "-s", wantName, "claude", "--project", proj.ID}
	if !slices.Equal(result.Spec.Argv, want) {
		t.Fatalf("argv = %q, want %q", result.Spec.Argv, want)
	}
	if result.Spec.Name != wantName {
		t.Fatalf("spec.Name = %q, want %q", result.Spec.Name, wantName)
	}

	// An empty-command persist template runs tmux's own shell: no tail.
	result, refusal = persistLaunch(t, store, "shell", "")
	if refusal != nil {
		t.Fatalf("shell: refused: %+v", refusal)
	}
	if want := []string{"/usr/bin/tmux", "new-session", "-A", "-s", "relay-0123abcd-shell-10"}; !slices.Equal(result.Spec.Argv, want) {
		t.Fatalf("shell: argv = %q, want %q", result.Spec.Argv, want)
	}
}

// persist_session reattaches by that exact name, through the host's tmux
// override, without listing the host.
func TestAuthorizeLaunch_PersistSessionReattachesByName(t *testing.T) {
	store := newLaunchTestStore(t)
	proj := addPersistLaunchTestProject(t, store, "/opt/tmux", "/usr/bin/tmux")
	calls := stubPersistSessionNames(t, nil, errors.New("listing must not run on reattach"))

	const name = "relay-0123abcd-claude-2"
	result, refusal := persistLaunch(t, store, "claude", name)
	if refusal != nil {
		t.Fatalf("refused: %+v", refusal)
	}
	want := []string{"/opt/tmux", "new-session", "-A", "-s", name, "claude", "--project", proj.ID}
	if !slices.Equal(result.Spec.Argv, want) || result.Spec.Name != name {
		t.Fatalf("argv = %q name = %q, want %q name %q", result.Spec.Argv, result.Spec.Name, want, name)
	}
	if *calls != 0 {
		t.Fatalf("listPersistSessionNames called %d times on reattach, want 0", *calls)
	}
}

func TestAuthorizeLaunch_PersistSessionRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		templateID, session string
	}{
		"another project's session":      {"claude", "relay-ffffffff-claude-2"},
		"another template's session":     {"claude", "relay-0123abcd-shell-2"},
		"malformed name":                 {"claude", "relay-0123abcd-claude-02"},
		"foreign tmux session":           {"claude", "main"},
		"persist_session on non-persist": {"tool", "relay-0123abcd-tool-1"},
	} {
		t.Run(name, func(t *testing.T) {
			store := newLaunchTestStore(t)
			addPersistLaunchTestProject(t, store, "", "/usr/bin/tmux")
			stubPersistSessionNames(t, nil, nil)
			_, refusal := persistLaunch(t, store, c.templateID, c.session)
			if refusal == nil || refusal.Code != "persist_session_invalid" || refusal.Status != 403 {
				t.Fatalf("refusal = %+v, want 403 persist_session_invalid", refusal)
			}
		})
	}
}

func TestAuthorizeLaunch_PersistNeedsTmuxAndAListing(t *testing.T) {
	t.Run("no tmux", func(t *testing.T) {
		store := newLaunchTestStore(t)
		addPersistLaunchTestProject(t, store, "", "")
		stubPersistSessionNames(t, nil, nil)
		if _, refusal := persistLaunch(t, store, "claude", ""); refusal == nil || refusal.Code != "tmux_not_available" {
			t.Fatalf("refusal = %+v, want tmux_not_available", refusal)
		}
	})
	t.Run("listing fails", func(t *testing.T) {
		store := newLaunchTestStore(t)
		addPersistLaunchTestProject(t, store, "", "/usr/bin/tmux")
		stubPersistSessionNames(t, nil, errors.New("tmux ls on devbox: Connection refused"))
		_, refusal := persistLaunch(t, store, "claude", "")
		if refusal == nil || refusal.Code != "persist_list_failed" || refusal.Status != 502 {
			t.Fatalf("refusal = %+v, want 502 persist_list_failed", refusal)
		}
		if !strings.Contains(refusal.Message, "Connection refused") {
			t.Fatalf("message = %q, want the listing's reason", refusal.Message)
		}
	})
}
