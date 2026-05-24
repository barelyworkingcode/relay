package main

// Hermetic tests for the Projects-tab IPC handlers. Mirrors the HTTP route
// coverage in project_routes_test.go: every handler proves it (a) emits the
// right event, (b) persists through the SettingsStore, and (c) honors the
// security boundaries documented on its mutator (see settings_projects_test.go
// for the unit-level coverage of the underlying methods).

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"sync"
	"testing"
)

// fakeTools is an in-memory MCPToolsProvider for handler tests. The handler
// surface is narrow — handlers either receive a tool list or get nil/empty —
// so a map-backed stub is plenty.
type fakeTools struct {
	infos map[string][]ToolInfo
}

func (f *fakeTools) ToolInfos(id string) []ToolInfo {
	if f == nil {
		return nil
	}
	return f.infos[id]
}

// AllContextSchemas lets fakeTools also stand in as the
// ContextSchemasProvider that mcpContextSchemasFrom looks for. Returning nil
// causes SyncProjectToken to skip filesystem auto-detection — fine for unit
// tests that don't exercise allowed_dirs.
func (f *fakeTools) AllContextSchemas() map[string]json.RawMessage { return nil }

// fakeSkillLister returns a fixed minimal tool list so EmitSkill can produce
// a stable SKILL.md without spinning up a real router.
type fakeSkillLister struct {
	calls int
	mu    sync.Mutex
}

func (f *fakeSkillLister) ListTools(_ context.Context, _ string) (json.RawMessage, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	// EmitSkill expects a JSON array of mcp.Tool. Return one so the rendered
	// SKILL.md is non-empty (and so the test exercises the non-trivial path).
	return json.RawMessage(`[{"name":"fs_read","description":"read a file"}]`), nil
}

// newProjectsIPC stands up an IPCContext wired to a sandboxed store with two
// MCPs registered. Returns the context, store, and the recording UI so tests
// can assert on emitted events.
func newProjectsIPC(t *testing.T) (*IPCContext, SettingsStore, *recordingUI, *fakeSkillLister) {
	t.Helper()
	_ = mkSandboxRelayHome(t)
	store := NewSettingsStoreAt(mkEmptySandboxRelayHome(t))
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	store.With(func(s *Settings) {
		s.ExternalMcps = []ExternalMcp{
			{ID: "fsmcp", DisplayName: "fsMCP"},
			{ID: "macmcp", DisplayName: "macMCP"},
		}
	})
	tools := &fakeTools{
		infos: map[string][]ToolInfo{
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
	var added Project
	if err := json.Unmarshal(rawAdded, &added); err != nil {
		t.Fatalf("unmarshal added: %v", err)
	}
	if added.Name != "Alpha" || added.Token == "" {
		t.Fatalf("unexpected added project: %+v", added)
	}
	persisted, _ := store.Get().findProjectByID(added.ID)
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
	var added Project
	_ = json.Unmarshal(args[0].(json.RawMessage), &added)
	persisted, _ := store.Get().findProjectByID(added.ID)
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
	// Roll-back: no projects persisted.
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
		ID:   proj.ID,
		Name: &newName,
	})
	ipcUpdateProject(ipc, raw)

	if _, ok := findEvent(ui, "onProjectUpdated"); !ok {
		t.Fatalf("expected onProjectUpdated")
	}
	persisted, _ := store.Get().findProjectByID(proj.ID)
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
	if p, _ := store.Get().findProjectByID(proj.ID); p != nil {
		t.Errorf("project still present after remove")
	}
}

func TestIPCRotateProjectToken_EmitsNewPlaintextAndInvalidatesOld(t *testing.T) {
	ipc, store, ui, _ := newProjectsIPC(t)
	proj := createTestProject(t, store, "Alpha", t.TempDir(), []string{"fsmcp"})
	oldPlain := proj.Token

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
	// Security regression: old token no longer authenticates.
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
	persisted, _ := store.Get().findProjectByID(proj.ID)
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
	var infos []ToolInfo
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
	var infos []ToolInfo
	_ = json.Unmarshal(args[1].(json.RawMessage), &infos)
	if len(infos) != 0 {
		t.Errorf("expected empty list, got %+v", infos)
	}
}

// ---------------------------------------------------------------------------
// End-to-end project lifecycle: create → SKILL.md written → delete → SKILL.md
// removed. This is the workflow the user explicitly asked about in the
// kickoff message; the test exercises every layer (IPC handler, settings
// store, EmitSkill, RemoveSkill) without spawning any subprocesses.
// ---------------------------------------------------------------------------

func TestProjectLifecycle_CreateWithSkill_Delete_CleansUpSkillFile(t *testing.T) {
	ipc, store, ui, _ := newProjectsIPC(t)
	projDir := t.TempDir()

	// Create with generate_skill = true.
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
	var created Project
	_ = json.Unmarshal(args[0].(json.RawMessage), &created)

	// SKILL.md should now exist under <path>/.claude/skills/relay/.
	skillPath := projectSkillDir(created) + "/SKILL.md"
	if _, err := readFileExists(skillPath); err != nil {
		t.Fatalf("expected SKILL.md at %s after create: %v", skillPath, err)
	}

	// Delete the project; SKILL.md should be gone.
	rawDelete := mustRaw(t, ipcIDMsg{ID: created.ID})
	ipcRemoveProject(ipc, rawDelete)
	if _, ok := findEvent(ui, "onProjectRemoved"); !ok {
		t.Fatalf("expected onProjectRemoved")
	}
	if _, err := readFileExists(skillPath); err == nil {
		t.Fatalf("SKILL.md still present after project delete: %s", skillPath)
	}
	if p, _ := store.Get().findProjectByID(created.ID); p != nil {
		t.Fatalf("project still in store after delete")
	}
}

// readFileExists is a tiny helper — returns non-nil error if the file is missing.
func readFileExists(path string) ([]byte, error) {
	return os.ReadFile(path)
}
