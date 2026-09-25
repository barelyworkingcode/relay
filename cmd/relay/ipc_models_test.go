package main

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/modelbroker"
	sessiontypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

func listModelsEventView(t *testing.T, ui *recordingUI) ModelCatalogView {
	t.Helper()
	args, ok := findEvent(ui, "onModelsListed")
	if !ok || len(args) != 1 {
		t.Fatalf("onModelsListed not emitted once with one arg: %v", args)
	}
	raw, ok := args[0].(json.RawMessage)
	if !ok {
		t.Fatalf("onModelsListed arg is %T, want json.RawMessage", args[0])
	}
	var view ModelCatalogView
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatalf("decode onModelsListed: %v (%s)", err, raw)
	}
	return view
}

func TestIPCListModels_EmitsCatalogView(t *testing.T) {
	ipc, _, ui, _ := newProjectsIPC(t)
	ipc.ModelCatalog = &ModelCatalogOps{
		HostModels: func(context.Context) ([]sessiontypes.ModelInfo, error) { return catalogHostRows(), nil },
		BrokerRows: func(context.Context) ([]modelbroker.Row, error) { return catalogBrokerRows(), nil },
	}

	ipcHandlers[MsgListModels](ipc, nil)

	want := buildModelCatalogView(catalogHostRows(), catalogBrokerRows())
	if got := listModelsEventView(t, ui); !reflect.DeepEqual(got, want) {
		t.Fatalf("onModelsListed = %+v, want %+v", got, want)
	}
}

func TestIPCListModels_NilCatalogEmitsUnavailable(t *testing.T) {
	ipc, _, ui, _ := newProjectsIPC(t)
	ipc.ModelCatalog = nil

	ipcHandlers[MsgListModels](ipc, nil)

	got := listModelsEventView(t, ui)
	if got.Status != "unavailable" || got.Error != "session host unavailable" || got.Models == nil || len(got.Models) != 0 {
		t.Fatalf("onModelsListed = %+v, want unavailable with models []", got)
	}
}

func TestIPCUpdateProject_SavesUnknownModelIDsVerbatim(t *testing.T) {
	ipc, store, ui, _ := newProjectsIPC(t)
	proj := createTestProject(t, store, "Acme", t.TempDir(), []string{"fsmcp"})
	want := []string{"haiku", "gone-model", "Chat"}

	ipcUpdateProject(ipc, mustRaw(t, map[string]interface{}{"id": proj.ID, "allowed_models": want}))

	if args, bad := findEvent(ui, "onProjectError"); bad {
		t.Fatalf("onProjectError: %v", args)
	}
	if _, ok := findEvent(ui, "onProjectUpdated"); !ok {
		t.Fatal("onProjectUpdated not emitted")
	}
	persisted, _ := config.FindProjectByID(store.Get(), proj.ID)
	if persisted == nil || !reflect.DeepEqual(persisted.AllowedModels, want) {
		t.Fatalf("persisted allowed_models = %v, want %v", persisted.AllowedModels, want)
	}
}
