package main

import (
	"reflect"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
)

func cosIPCProject(t *testing.T, store config.SettingsStore) config.Project {
	t.Helper()
	p := createTestProject(t, store, "Acme", t.TempDir(), []string{"fsmcp"})
	store.With(func(s *config.Settings) {
		sp, _ := config.FindProjectByID(s, p.ID)
		sp.AllowedTemplates = []string{"claude-code"}
	})
	return p
}

func TestChiefOfStaff_IPCSetStoresAndEmitsGetShape(t *testing.T) {
	ipc, store, ui, _ := newProjectsIPC(t)
	p := cosIPCProject(t, store)

	ipcHandlers[MsgSetChiefOfStaff](ipc, mustRaw(t, map[string]any{"project_id": p.ID, "model": "haiku", "daily_model_calls": 40}))

	if args, bad := findEvent(ui, "onProjectError"); bad {
		t.Fatalf("onProjectError: %v", args)
	}
	args, ok := findEvent(ui, "onChiefOfStaffUpdated")
	if !ok {
		t.Fatal("onChiefOfStaffUpdated not emitted")
	}
	want := map[string]any{"configured": true, "projectId": p.ID, "model": "haiku", "dailyModelCalls": float64(40)}
	if got := pmEventJSON(t, args); !reflect.DeepEqual(got, want) {
		t.Errorf("event = %v, want %v", got, want)
	}
	if got, ok := store.Get().ChiefOfStaffSetting(); !ok || got != (config.ChiefOfStaffConfig{ProjectID: p.ID, Model: "haiku", DailyModelCalls: 40}) {
		t.Errorf("stored = %+v %v", got, ok)
	}
}

func TestChiefOfStaff_IPCEmptyProjectClears(t *testing.T) {
	ipc, store, ui, _ := newProjectsIPC(t)
	p := cosIPCProject(t, store)
	ipcHandlers[MsgSetChiefOfStaff](ipc, mustRaw(t, map[string]any{"project_id": p.ID, "model": "haiku", "daily_model_calls": 40}))

	ipcHandlers[MsgSetChiefOfStaff](ipc, mustRaw(t, map[string]any{"project_id": "", "model": "opus", "daily_model_calls": 7}))

	if store.Get().ChiefOfStaff != nil {
		t.Error("an empty project_id left the setting stored")
	}
	args, ok := findEvent(ui, "onChiefOfStaffUpdated")
	if !ok {
		t.Fatal("onChiefOfStaffUpdated not emitted")
	}
	if got := pmEventJSON(t, args); !reflect.DeepEqual(got, map[string]any{"configured": false}) {
		t.Errorf("event after clear = %v, want configured:false", got)
	}
}

func TestChiefOfStaff_IPCRefusalEmitsProjectErrorAndKeepsStored(t *testing.T) {
	cases := []struct {
		name string
		msg  func(id string) map[string]any
	}{
		{"unknown project", func(string) map[string]any {
			return map[string]any{"project_id": "p-gone", "model": "haiku", "daily_model_calls": 5}
		}},
		{"bad model", func(id string) map[string]any {
			return map[string]any{"project_id": id, "model": "gpt", "daily_model_calls": 5}
		}},
		{"zero limit", func(id string) map[string]any { return map[string]any{"project_id": id, "model": "haiku"} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ipc, store, ui, _ := newProjectsIPC(t)
			p := cosIPCProject(t, store)
			ipcHandlers[MsgSetChiefOfStaff](ipc, mustRaw(t, map[string]any{"project_id": p.ID, "model": "sonnet", "daily_model_calls": 9}))
			before := pmCountEvents(ui, "onChiefOfStaffUpdated")

			ipcHandlers[MsgSetChiefOfStaff](ipc, mustRaw(t, c.msg(p.ID)))

			if _, ok := findEvent(ui, "onProjectError"); !ok {
				t.Error("onProjectError not emitted")
			}
			if n := pmCountEvents(ui, "onChiefOfStaffUpdated"); n != before {
				t.Error("onChiefOfStaffUpdated emitted for a refusal")
			}
			if got, _ := store.Get().ChiefOfStaffSetting(); got.Model != "sonnet" || got.DailyModelCalls != 9 {
				t.Errorf("a refusal changed the stored setting: %+v", got)
			}
		})
	}
}

func TestChiefOfStaff_PushFullSettingsCarriesTheView(t *testing.T) {
	store := newCLISandboxStore(t)
	p := &pmJSPlatform{}
	app := &App{store: store, platform: p, registry: &trayRegistry{}, extMgr: mcpbroker.NewManager(nil)}
	app.settingsOpen.Store(true)
	app.pushFullSettings()
	if got := string(p.settingsReloaded(t)["chief_of_staff"]); got != `{"configured":false}` {
		t.Errorf("unset chief_of_staff pushed as %s", got)
	}
	store.With(func(s *config.Settings) {
		s.ChiefOfStaff = &config.ChiefOfStaffConfig{ProjectID: "p1", Model: "opus", DailyModelCalls: 3}
	})
	app.pushFullSettings()
	if got := string(p.settingsReloaded(t)["chief_of_staff"]); got != `{"configured":true,"projectId":"p1","model":"opus","dailyModelCalls":3}` {
		t.Errorf("chief_of_staff pushed as %s", got)
	}
}
