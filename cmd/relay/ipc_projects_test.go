package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/mcp"
	"github.com/barelyworkingcode/relay/internal/project"
)

type fakeTools struct {
	infos map[string][]config.ToolInfo
	// surfaces is what mcpSurfacesFrom hands to the apply layer. Nil (the
	// default) means SyncProjectToken skips scope derivation; a test that
	// exercises a v2 contextSchema sets it.
	surfaces project.McpSurfaces
}

func (f *fakeTools) ToolInfos(id string) []config.ToolInfo {
	if f == nil {
		return nil
	}
	return f.infos[id]
}

func (f *fakeTools) AllMcpSurfaces() project.McpSurfaces {
	if f == nil {
		return nil
	}
	return f.surfaces
}

type fakeSkillLister struct {
	calls int
	mu    sync.Mutex
}

func (f *fakeSkillLister) ListTools(_ context.Context, _ string) (json.RawMessage, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return json.RawMessage(`[{"name":"fs_read","description":"read a file"}]`), nil
}

func (f *fakeSkillLister) ListSkillBuckets(_ context.Context, _ string) ([]SkillBucket, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return []SkillBucket{{Key: "Files", Slug: "files", Tools: []mcp.Tool{{Name: "fs_read", Description: "read a file"}}}}, nil
}

func newProjectsIPC(t *testing.T) (*IPCContext, config.SettingsStore, *recordingUI, *fakeSkillLister) {
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
	tools := &fakeTools{
		infos: map[string][]config.ToolInfo{
			"fsmcp":  {{Name: "fs_read"}, {Name: "fs_write"}, {Name: "fs_bash"}},
			"macmcp": {{Name: "runScript"}, {Name: "openApp"}},
		},
	}
	skillLister := &fakeSkillLister{}
	ui := &recordingUI{}
	ipc := &IPCContext{
		Ctx:                    context.Background(),
		Store:                  store,
		UI:                     ui,
		Platform:               stubPlatform{},
		Registry:               noopServiceManager{},
		Enhanced:               NewEnhancedServiceRegistry(nil),
		UpdateMenu:             func() {},
		PushServiceStatusBatch: func() {},
		GoFunc:                 func(fn func()) { fn() }, // inline for deterministic assertions
		NotifyReconcile:        func(string) error { return nil },
		NotifyReloadMcp:        func(string, string) error { return nil },
		Tools:                  tools,
		SkillLister:            skillLister,
		ProjectOps:             &ProjectOps{Store: store, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)},
	}
	return ipc, store, ui, skillLister
}

func findEvent(ui *recordingUI, name string) ([]interface{}, bool) {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	for i := len(ui.events) - 1; i >= 0; i-- {
		if ui.events[i].Name == name {
			return ui.events[i].Args, true
		}
	}
	return nil, false
}

func mustRaw(t *testing.T, v interface{}) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

func TestIPCCreateProject_HappyPath(t *testing.T) {
	ipc, store, ui, _ := newProjectsIPC(t)
	raw := mustRaw(t, map[string]interface{}{
		"name":            "Alpha",
		"path":            t.TempDir(),
		"allowed_mcp_ids": []string{"fsmcp"},
		"allowed_models":  []string{"*"},
		"generate_skill":  false,
	})

	ipcCreateProject(ipc, raw)

	args, ok := findEvent(ui, "onProjectAdded")
	if !ok {
		t.Fatalf("expected onProjectAdded event; got events=%+v", ui.events)
	}
	rawAdded := args[0].(json.RawMessage)
	var added config.Project
	if err := json.Unmarshal(rawAdded, &added); err != nil {
		t.Fatalf("unmarshal added: %v", err)
	}
	addedToken, _ := added.Token.Reveal()
	if added.Name != "Alpha" || addedToken == "" {
		t.Fatalf("unexpected added project: %+v", added)
	}
	persisted, _ := config.FindProjectByID(store.Get(), added.ID)
	if persisted == nil || persisted.Name != "Alpha" {
		t.Errorf("project not persisted")
	}
}

func TestIPCCreateProject_AppliesGenerateSkillAndDisabledTools(t *testing.T) {
	ipc, store, ui, lister := newProjectsIPC(t)
	raw := mustRaw(t, map[string]interface{}{
		"name":            "Alpha",
		"path":            t.TempDir(),
		"allowed_mcp_ids": []string{"fsmcp", "macmcp"},
		"generate_skill":  true,
		"disabled_tools": map[string][]string{
			"macmcp": {"runScript"},
		},
	})

	ipcCreateProject(ipc, raw)
	args, _ := findEvent(ui, "onProjectAdded")
	var added config.Project
	_ = json.Unmarshal(args[0].(json.RawMessage), &added)
	persisted, _ := config.FindProjectByID(store.Get(), added.ID)
	if !persisted.GenerateSkill {
		t.Errorf("generate_skill not set")
	}
	if !reflect.DeepEqual(persisted.DisabledTools["macmcp"], []string{"runScript"}) {
		t.Errorf("disabled_tools[macmcp] = %v; want [runScript]", persisted.DisabledTools["macmcp"])
	}
	if lister.calls == 0 {
		t.Errorf("expected skill regen to invoke ListTools at least once")
	}
}

// TestIPCUpdateProject_GenerateSkillPointerSemantics re-points a property
// the deleted TestIPCProject_AllowCwdAuthRoundTrips used to pin onto the one
// *bool remaining in project.UpdateFields: an explicit false actually
// persists (so the toggle is revocable from the UI, not just settable), and
// an unrelated patch leaves it alone (the whole point of it being a pointer
// rather than a plain bool — an absent field means "unchanged", not
// "false").
func TestIPCUpdateProject_GenerateSkillPointerSemantics(t *testing.T) {
	ipc, store, ui, _ := newProjectsIPC(t)
	raw := mustRaw(t, map[string]interface{}{
		"name":            "Alpha",
		"path":            t.TempDir(),
		"allowed_mcp_ids": []string{"fsmcp"},
		"generate_skill":  true,
	})

	ipcCreateProject(ipc, raw)
	args, ok := findEvent(ui, "onProjectAdded")
	if !ok {
		t.Fatalf("expected onProjectAdded; got events=%+v", ui.events)
	}
	var added config.Project
	_ = json.Unmarshal(args[0].(json.RawMessage), &added)
	if persisted, _ := config.FindProjectByID(store.Get(), added.ID); !persisted.GenerateSkill {
		t.Fatalf("generate_skill not persisted on create")
	}

	off := false
	ipcUpdateProject(ipc, mustRaw(t, ipcUpdateProjectMsg{
		ID:           added.ID,
		UpdateFields: project.UpdateFields{GenerateSkill: &off},
	}))
	if persisted, _ := config.FindProjectByID(store.Get(), added.ID); persisted.GenerateSkill {
		t.Errorf("generate_skill still set after patching it off")
	}

	// An unrelated patch leaves the flag alone (pointer semantics).
	on := true
	ipcUpdateProject(ipc, mustRaw(t, ipcUpdateProjectMsg{
		ID:           added.ID,
		UpdateFields: project.UpdateFields{GenerateSkill: &on},
	}))
	newName := "Bravo"
	ipcUpdateProject(ipc, mustRaw(t, ipcUpdateProjectMsg{
		ID:           added.ID,
		UpdateFields: project.UpdateFields{Name: &newName},
	}))
	if persisted, _ := config.FindProjectByID(store.Get(), added.ID); !persisted.GenerateSkill {
		t.Errorf("generate_skill cleared by an unrelated patch")
	}
}

func TestIPCCreateProject_BadPermissionPolicyEmitsErrorAndRollsBack(t *testing.T) {
	ipc, store, ui, _ := newProjectsIPC(t)
	raw := mustRaw(t, map[string]interface{}{
		"name":              "Alpha",
		"path":              t.TempDir(),
		"allowed_mcp_ids":   []string{"fsmcp"},
		"permission_policy": map[string]interface{}{"default_mode": "not-a-real-mode"},
	})

	ipcCreateProject(ipc, raw)
	if _, ok := findEvent(ui, "onProjectAdded"); ok {
		t.Fatalf("project added despite policy validation failure")
	}
	if _, ok := findEvent(ui, "onProjectError"); !ok {
		t.Fatalf("expected onProjectError event")
	}
	if len(store.Get().Projects) != 0 {
		t.Errorf("project list not empty after rollback: %+v", store.Get().Projects)
	}
}

func TestIPCUpdateProject_PatchesNamedFieldsOnly(t *testing.T) {
	ipc, store, ui, _ := newProjectsIPC(t)
	proj := createTestProject(t, store, "Alpha", t.TempDir(), []string{"fsmcp"})
	originalPath := proj.Path

	newName := "Bravo"
	raw := mustRaw(t, ipcUpdateProjectMsg{
		ID:           proj.ID,
		UpdateFields: project.UpdateFields{Name: &newName},
	})
	ipcUpdateProject(ipc, raw)

	if _, ok := findEvent(ui, "onProjectUpdated"); !ok {
		t.Fatalf("expected onProjectUpdated")
	}
	persisted, _ := config.FindProjectByID(store.Get(), proj.ID)
	if persisted.Name != "Bravo" {
		t.Errorf("name = %q; want Bravo", persisted.Name)
	}
	if persisted.Path != originalPath {
		t.Errorf("path mutated: got %q want %q", persisted.Path, originalPath)
	}
}

func TestIPCRemoveProject_DeletesAndEmits(t *testing.T) {
	ipc, store, ui, _ := newProjectsIPC(t)
	proj := createTestProject(t, store, "Alpha", t.TempDir(), []string{"fsmcp"})

	raw := mustRaw(t, ipcIDMsg{ID: proj.ID})
	ipcRemoveProject(ipc, raw)

	args, ok := findEvent(ui, "onProjectRemoved")
	if !ok {
		t.Fatalf("expected onProjectRemoved")
	}
	if args[0].(string) != proj.ID {
		t.Errorf("emit had wrong id: %v", args[0])
	}
	if p, _ := config.FindProjectByID(store.Get(), proj.ID); p != nil {
		t.Errorf("project still present after remove")
	}
}

func TestIPCRotateProjectToken_EmitsNewPlaintextAndInvalidatesOld(t *testing.T) {
	ipc, store, ui, _ := newProjectsIPC(t)
	proj := createTestProject(t, store, "Alpha", t.TempDir(), []string{"fsmcp"})
	oldPlain, _ := proj.Token.Reveal()

	raw := mustRaw(t, ipcIDMsg{ID: proj.ID})
	ipcRotateProjectToken(ipc, raw)

	args, ok := findEvent(ui, "onProjectTokenRotated")
	if !ok {
		t.Fatalf("expected onProjectTokenRotated")
	}
	if args[0].(string) != proj.ID {
		t.Fatalf("rotated event had wrong id")
	}
	newPlain := args[1].(string)
	if newPlain == "" || newPlain == oldPlain {
		t.Fatalf("rotated plaintext is empty or unchanged")
	}
	if _, err := store.Get().AuthenticateProject(oldPlain); err == nil {
		t.Fatalf("old token still authenticates after rotation")
	}
}

func TestIPCRotateProjectToken_UnknownIDEmitsError(t *testing.T) {
	ipc, _, ui, _ := newProjectsIPC(t)
	raw := mustRaw(t, ipcIDMsg{ID: "nope"})
	ipcRotateProjectToken(ipc, raw)
	if _, ok := findEvent(ui, "onProjectError"); !ok {
		t.Fatalf("expected onProjectError for unknown project id")
	}
}

func TestIPCRegenProjectSkill_OK(t *testing.T) {
	ipc, store, ui, lister := newProjectsIPC(t)
	proj := createTestProject(t, store, "Alpha", t.TempDir(), []string{"fsmcp"})

	raw := mustRaw(t, ipcIDMsg{ID: proj.ID})
	ipcRegenProjectSkill(ipc, raw)

	args, ok := findEvent(ui, "onProjectSkillRegen")
	if !ok {
		t.Fatalf("expected onProjectSkillRegen")
	}
	if args[1].(bool) != true {
		t.Fatalf("regen reported failure: %+v", args)
	}
	if lister.calls == 0 {
		t.Fatalf("expected ListTools to be invoked at least once")
	}
}

func TestIPCRegenProjectSkill_NoLister_EmitsServiceUnavailableMessage(t *testing.T) {
	ipc, store, ui, _ := newProjectsIPC(t)
	ipc.SkillLister = nil
	proj := createTestProject(t, store, "Alpha", t.TempDir(), []string{"fsmcp"})

	raw := mustRaw(t, ipcIDMsg{ID: proj.ID})
	ipcRegenProjectSkill(ipc, raw)

	args, ok := findEvent(ui, "onProjectSkillRegen")
	if !ok {
		t.Fatalf("expected onProjectSkillRegen even when lister is nil")
	}
	if args[1].(bool) != false {
		t.Fatalf("expected failure flag, got: %+v", args)
	}
}

// TestIPCRegenProjectSkill_RefusesHostedProject is item 1's IPC-side
// regression test: the "regen now" button must refuse a hosted project the
// same way the HTTP route does, instead of writing .claude/skills at a path
// that only describes the remote machine.
func TestIPCRegenProjectSkill_RefusesHostedProject(t *testing.T) {
	ipc, store, ui, lister := newProjectsIPC(t)
	consolePath := filepath.Join(t.TempDir(), "hosted-project-path")
	store.With(func(s *config.Settings) {
		s.AddProject(config.Project{
			ID:     "p_hosted",
			Name:   "hosted",
			Path:   consolePath,
			HostID: "h_devbox",
		})
	})

	raw := mustRaw(t, ipcIDMsg{ID: "p_hosted"})
	ipcRegenProjectSkill(ipc, raw)

	args, ok := findEvent(ui, "onProjectSkillRegen")
	if !ok {
		t.Fatalf("expected onProjectSkillRegen")
	}
	if args[1].(bool) != false {
		t.Fatalf("expected regen to report failure for a hosted project, got: %+v", args)
	}
	if lister.calls != 0 {
		t.Fatalf("expected ListTools/ListSkillBuckets never to be called for a hosted project")
	}
	if _, statErr := os.Stat(filepath.Join(consolePath, ".claude", "skills")); !os.IsNotExist(statErr) {
		t.Fatalf("expected no skills dir written on the console for a hosted project, stat err = %v", statErr)
	}
}

func TestIPCUpdateProjectDisabledTools_PersistsAndEmits(t *testing.T) {
	ipc, store, ui, _ := newProjectsIPC(t)
	proj := createTestProject(t, store, "Alpha", t.TempDir(), []string{"fsmcp", "macmcp"})

	raw := mustRaw(t, ipcProjectDisabledToolsMsg{
		ID:       proj.ID,
		McpID:    "macmcp",
		Disabled: []string{"runScript"},
	})
	ipcUpdateProjectDisabledTools(ipc, raw)

	if _, ok := findEvent(ui, "onProjectUpdated"); !ok {
		t.Fatalf("expected onProjectUpdated event")
	}
	persisted, _ := config.FindProjectByID(store.Get(), proj.ID)
	if !reflect.DeepEqual(persisted.DisabledTools["macmcp"], []string{"runScript"}) {
		t.Errorf("disabled_tools not persisted: %v", persisted.DisabledTools)
	}
}

func TestIPCListMcpTools_ReturnsLiveList(t *testing.T) {
	ipc, _, ui, _ := newProjectsIPC(t)
	raw := mustRaw(t, ipcListMcpToolsMsg{McpID: "macmcp"})
	ipcListMcpTools(ipc, raw)

	args, ok := findEvent(ui, "onMcpToolsListed")
	if !ok {
		t.Fatalf("expected onMcpToolsListed")
	}
	if args[0].(string) != "macmcp" {
		t.Fatalf("event had wrong id")
	}
	var infos []config.ToolInfo
	_ = json.Unmarshal(args[1].(json.RawMessage), &infos)
	if len(infos) != 2 {
		t.Fatalf("expected 2 tools for macmcp, got %d: %+v", len(infos), infos)
	}
}

func TestIPCListMcpTools_NoToolsProviderEmitsEmptyList(t *testing.T) {
	ipc, _, ui, _ := newProjectsIPC(t)
	ipc.Tools = nil
	raw := mustRaw(t, ipcListMcpToolsMsg{McpID: "macmcp"})
	ipcListMcpTools(ipc, raw)

	args, ok := findEvent(ui, "onMcpToolsListed")
	if !ok {
		t.Fatalf("expected onMcpToolsListed even with nil provider")
	}
	var infos []config.ToolInfo
	_ = json.Unmarshal(args[1].(json.RawMessage), &infos)
	if len(infos) != 0 {
		t.Errorf("expected empty list, got %+v", infos)
	}
}

func TestProjectLifecycle_CreateWithSkill_Delete_CleansUpSkillFile(t *testing.T) {
	ipc, store, ui, _ := newProjectsIPC(t)
	projDir := t.TempDir()

	rawCreate := mustRaw(t, map[string]interface{}{
		"name":            "Lifecycle",
		"path":            projDir,
		"allowed_mcp_ids": []string{"fsmcp"},
		"generate_skill":  true,
	})
	ipcCreateProject(ipc, rawCreate)
	args, ok := findEvent(ui, "onProjectAdded")
	if !ok {
		t.Fatalf("expected onProjectAdded; events=%+v", ui.events)
	}
	var created config.Project
	_ = json.Unmarshal(args[0].(json.RawMessage), &created)

	skillPath := filepath.Join(projectSkillDir(created), "relay-files", "SKILL.md")
	if _, err := readFileExists(skillPath); err != nil {
		t.Fatalf("expected SKILL.md at %s after create: %v", skillPath, err)
	}

	rawDelete := mustRaw(t, ipcIDMsg{ID: created.ID})
	ipcRemoveProject(ipc, rawDelete)
	if _, ok := findEvent(ui, "onProjectRemoved"); !ok {
		t.Fatalf("expected onProjectRemoved")
	}
	if _, err := readFileExists(skillPath); err == nil {
		t.Fatalf("SKILL.md still present after project delete: %s", skillPath)
	}
	if p, _ := config.FindProjectByID(store.Get(), created.ID); p != nil {
		t.Fatalf("project still in store after delete")
	}
}

func readFileExists(path string) ([]byte, error) {
	return os.ReadFile(path)
}
