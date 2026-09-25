package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
)

func pmEventJSON(t *testing.T, args []interface{}) map[string]any {
	t.Helper()
	if len(args) != 1 {
		t.Fatalf("event args = %v, want one", args)
	}
	raw, err := json.Marshal(args[0])
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

func pmCountEvents(ui *recordingUI, name string) int {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	n := 0
	for _, e := range ui.events {
		if e.Name == name {
			n++
		}
	}
	return n
}

func TestDefaultProject_IPCSetEmitsEffectiveView(t *testing.T) {
	ipc, store, ui, _ := newProjectsIPC(t)
	p := createTestProject(t, store, "Acme", t.TempDir(), []string{"fsmcp"})

	ipcHandlers[MsgSetDefaultProject](ipc, mustRaw(t, map[string]string{"mode": "work", "project_id": p.ID}))

	if args, bad := findEvent(ui, "onProjectError"); bad {
		t.Fatalf("onProjectError: %v", args)
	}
	args, ok := findEvent(ui, "onDefaultProjectUpdated")
	if !ok {
		t.Fatal("onDefaultProjectUpdated not emitted")
	}
	if view := pmEventJSON(t, args); view["work"] != p.ID || (view["home"] != nil && view["home"] != "") {
		t.Errorf("onDefaultProjectUpdated view = %v, want work=%s only", view, p.ID)
	}
	if got := store.Get().DefaultProjectFor(config.ProjectModeWork); got != p.ID {
		t.Errorf("stored work default = %q", got)
	}
}

func TestDefaultProject_IPCRefusalEmitsProjectError(t *testing.T) {
	cases := []struct{ name, mode string }{
		{"mode mismatch", "home"},
		{"no mode", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ipc, store, ui, _ := newProjectsIPC(t)
			p := createTestProject(t, store, "Acme work", t.TempDir(), []string{"fsmcp"})
			store.With(func(s *config.Settings) { s.SetProjectMode(p.ID, config.ProjectModeWork) })
			before := store.Get().DefaultProject

			ipcHandlers[MsgSetDefaultProject](ipc, mustRaw(t, map[string]string{"mode": c.mode, "project_id": p.ID}))

			if _, ok := findEvent(ui, "onProjectError"); !ok {
				t.Error("onProjectError not emitted")
			}
			if pmCountEvents(ui, "onDefaultProjectUpdated") != 0 {
				t.Error("onDefaultProjectUpdated emitted for a refusal")
			}
			if after := store.Get().DefaultProject; !reflect.DeepEqual(after, before) {
				t.Errorf("a refusal changed default_project: %+v", after)
			}
		})
	}
}

func TestProjectMode_IPCCreateAndUpdateCarryMode(t *testing.T) {
	ipc, store, ui, _ := newProjectsIPC(t)
	ipcCreateProject(ipc, mustRaw(t, map[string]any{
		"name": "Acme", "path": t.TempDir(), "allowed_mcp_ids": []string{"fsmcp"}, "allowed_models": []string{"*"}, "mode": "work",
	}))
	args, ok := findEvent(ui, "onProjectAdded")
	if !ok {
		t.Fatalf("onProjectAdded not emitted; events=%+v", ui.events)
	}
	id, _ := pmEventJSON(t, args)["id"].(string)
	if p, _ := config.FindProjectByID(store.Get(), id); p == nil || p.Mode != config.ProjectModeWork {
		t.Fatalf("created project = %+v, want mode work", p)
	}

	ipcUpdateProject(ipc, mustRaw(t, map[string]any{"id": id, "mode": "both"}))
	if args, bad := findEvent(ui, "onProjectError"); bad {
		t.Fatalf("onProjectError: %v", args)
	}
	if p, _ := config.FindProjectByID(store.Get(), id); p.Mode != "" {
		t.Errorf("mode both stored as %q, want \"\"", p.Mode)
	}
}

type pmJSPlatform struct {
	recordingPlatform
	jsMu sync.Mutex
	js   []string
}

func (p *pmJSPlatform) EvalSettingsJS(js string) {
	p.jsMu.Lock()
	defer p.jsMu.Unlock()
	p.js = append(p.js, js)
}

func (p *pmJSPlatform) settingsReloaded(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	p.jsMu.Lock()
	defer p.jsMu.Unlock()
	for i := len(p.js) - 1; i >= 0; i-- {
		if arg, ok := strings.CutPrefix(p.js[i], "onSettingsReloaded("); ok {
			var out map[string]json.RawMessage
			if err := json.Unmarshal([]byte(strings.TrimSuffix(arg, ")")), &out); err != nil {
				t.Fatalf("decode onSettingsReloaded: %v", err)
			}
			return out
		}
	}
	t.Fatal("onSettingsReloaded not pushed")
	return nil
}

func TestDefaultProject_PushFullSettingsCarriesTheRawBlock(t *testing.T) {
	store := newCLISandboxStore(t)
	p := &pmJSPlatform{}
	app := &App{store: store, platform: p, registry: &trayRegistry{}, extMgr: mcpbroker.NewManager(nil)}
	app.settingsOpen.Store(true)

	app.pushFullSettings()
	if got := string(p.settingsReloaded(t)["default_project"]); got != "null" {
		t.Errorf("never-configured default_project pushed as %q, want null", got)
	}

	store.With(func(s *config.Settings) { s.DefaultProject = &config.DefaultProjects{Home: "p-gone"} })
	app.pushFullSettings()
	if got := string(p.settingsReloaded(t)["default_project"]); got != `{"home":"p-gone"}` {
		t.Errorf("default_project pushed as %s, want the raw stored block", got)
	}
}
