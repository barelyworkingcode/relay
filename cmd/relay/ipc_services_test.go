package main

// Behavioral coverage for the Services-tab IPC handlers. Previously these were
// only hit by the malformed-payload panic-smoke in ipc_handlers_test.go; their
// real logic — validation, the wasRunning→Reload restart branch, restart-failure
// reporting, and the async stop-then-refresh pattern — was unverified.

import (
	"context"
	"errors"
	"github.com/barelyworkingcode/relay/internal/config"
	"strings"
	"sync"
	"testing"
)

// svcRecorder is a ServiceManager that records lifecycle calls and lets a test
// control IsRunning and force a Reload failure.
type svcRecorder struct {
	noopServiceManager
	mu        sync.Mutex
	running   map[string]bool
	started   []string
	stopped   []string
	reloaded  []string
	reloadErr error
	startErr  error
}

func (r *svcRecorder) Start(c *config.ServiceConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = append(r.started, c.ID)
	return r.startErr
}
func (r *svcRecorder) Stop(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = append(r.stopped, id)
}
func (r *svcRecorder) Reload(id string, _ *config.ServiceConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reloaded = append(r.reloaded, id)
	return r.reloadErr
}
func (r *svcRecorder) IsRunning(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running[id]
}
func (r *svcRecorder) RunningIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for id, on := range r.running {
		if on {
			ids = append(ids, id)
		}
	}
	return ids
}

func newServicesIPC(t *testing.T, store config.SettingsStore, reg ServiceManager) (*IPCContext, *recordingUI) {
	ui := &recordingUI{}
	ipc := &IPCContext{
		Ctx:                    context.Background(),
		Store:                  store,
		UI:                     ui,
		Platform:               stubPlatform{}, // DispatchToMain runs inline
		Registry:               reg,
		UpdateMenu:             func() {},
		PushServiceStatusBatch: func() {},
		GoFunc:                 func(fn func()) { fn() }, // run "async" work inline
		Ops:                    &ServiceOps{Store: store, Registry: reg, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)},
	}
	return ipc, ui
}

// lastSettingsError returns the message of the most recent onSettingsError, or
// fails if none was emitted.
func lastSettingsError(t *testing.T, ui *recordingUI) string {
	t.Helper()
	ui.mu.Lock()
	defer ui.mu.Unlock()
	for i := len(ui.events) - 1; i >= 0; i-- {
		if ui.events[i].Name == "onSettingsError" {
			s, _ := ui.events[i].Args[0].(string)
			return s
		}
	}
	t.Fatal("no onSettingsError emitted")
	return ""
}

func seedService(t *testing.T, store config.SettingsStore, cfg config.ServiceConfig) {
	t.Helper()
	if err := store.With(func(s *config.Settings) { s.UpsertService(cfg) }); err != nil {
		t.Fatalf("seed service: %v", err)
	}
}

func TestIPCAddService_HappyPathPersistsAndEmits(t *testing.T) {
	store := newCLISandboxStore(t)
	reg := &svcRecorder{}
	ipc, ui := newServicesIPC(t, store, reg)

	ipcAddService(ipc, mustJSON(t, ipcServiceMsg{DisplayName: "My Svc", Command: "/bin/x"}))

	svcs := store.Get().Services
	if len(svcs) != 1 {
		t.Fatalf("want 1 persisted service, got %d", len(svcs))
	}
	if !ui.hasEvent("onServiceAdded") {
		t.Error("onServiceAdded not emitted")
	}
	if len(reg.started) != 0 {
		t.Errorf("non-autostart service should not be started; got %v", reg.started)
	}
}

func TestIPCAddService_AutostartStartsService(t *testing.T) {
	store := newCLISandboxStore(t)
	reg := &svcRecorder{}
	ipc, _ := newServicesIPC(t, store, reg)

	ipcAddService(ipc, mustJSON(t, ipcServiceMsg{DisplayName: "Auto Svc", Command: "/bin/x", Autostart: true}))

	id := store.Get().Services[0].ID
	if len(reg.started) != 1 || reg.started[0] != id {
		t.Errorf("autostart service should be started; started=%v want [%s]", reg.started, id)
	}
}

func TestIPCAddService_ValidationErrors(t *testing.T) {
	t.Run("empty display name", func(t *testing.T) {
		store := newCLISandboxStore(t)
		ipc, ui := newServicesIPC(t, store, &svcRecorder{})
		ipcAddService(ipc, mustJSON(t, ipcServiceMsg{DisplayName: "", Command: "/bin/x"}))
		if msg := lastSettingsError(t, ui); msg != "display name is required" {
			t.Errorf("error = %q, want display name is required", msg)
		}
		if len(store.Get().Services) != 0 {
			t.Error("nothing should be persisted on validation failure")
		}
	})
	t.Run("empty command", func(t *testing.T) {
		store := newCLISandboxStore(t)
		ipc, ui := newServicesIPC(t, store, &svcRecorder{})
		ipcAddService(ipc, mustJSON(t, ipcServiceMsg{DisplayName: "X", Command: ""}))
		if msg := lastSettingsError(t, ui); msg != "command is required" {
			t.Errorf("error = %q, want command is required", msg)
		}
	})
}

func TestIPCUpdateService_RunningTriggersReload(t *testing.T) {
	store := newCLISandboxStore(t)
	seedService(t, store, config.ServiceConfig{ID: "svc1", DisplayName: "Svc1", Command: "/bin/old"})
	reg := &svcRecorder{running: map[string]bool{"svc1": true}}
	ipc, _ := newServicesIPC(t, store, reg)

	ipcUpdateService(ipc, mustJSON(t, ipcServiceMsg{ID: "svc1", DisplayName: "Svc1", Command: "/bin/new"}))

	if len(reg.reloaded) != 1 || reg.reloaded[0] != "svc1" {
		t.Errorf("running service should be reloaded; reloaded=%v", reg.reloaded)
	}
	if cfg, _ := config.FindServiceByID(store.Get(), "svc1"); cfg == nil || cfg.Command != "/bin/new" {
		t.Errorf("updated command not persisted: %+v", cfg)
	}
}

func TestIPCUpdateService_NotRunningSkipsReload(t *testing.T) {
	store := newCLISandboxStore(t)
	seedService(t, store, config.ServiceConfig{ID: "svc1", DisplayName: "Svc1", Command: "/bin/old"})
	reg := &svcRecorder{} // not running
	ipc, ui := newServicesIPC(t, store, reg)

	ipcUpdateService(ipc, mustJSON(t, ipcServiceMsg{ID: "svc1", DisplayName: "Svc1", Command: "/bin/new"}))

	if len(reg.reloaded) != 0 {
		t.Errorf("stopped service should not be reloaded; reloaded=%v", reg.reloaded)
	}
	if !ui.hasEvent("onServiceStatus") {
		t.Error("refreshServiceUI should have emitted onServiceStatus")
	}
}

func TestIPCUpdateService_RestartFailureEmitsError(t *testing.T) {
	store := newCLISandboxStore(t)
	seedService(t, store, config.ServiceConfig{ID: "svc1", DisplayName: "Svc1", Command: "/bin/old"})
	reg := &svcRecorder{running: map[string]bool{"svc1": true}, reloadErr: errors.New("boom")}
	ipc, ui := newServicesIPC(t, store, reg)

	ipcUpdateService(ipc, mustJSON(t, ipcServiceMsg{ID: "svc1", DisplayName: "Svc1", Command: "/bin/new"}))

	if msg := lastSettingsError(t, ui); !strings.Contains(msg, "restart failed") {
		t.Errorf("expected restart-failed error, got %q", msg)
	}
}

func TestIPCUpdateService_CommandRequired(t *testing.T) {
	store := newCLISandboxStore(t)
	ipc, ui := newServicesIPC(t, store, &svcRecorder{})
	ipcUpdateService(ipc, mustJSON(t, ipcServiceMsg{ID: "svc1", DisplayName: "Svc1", Command: ""}))
	if msg := lastSettingsError(t, ui); msg != "command is required" {
		t.Errorf("error = %q, want command is required", msg)
	}
}

func TestIPCRemoveService_RemovesAndStops(t *testing.T) {
	store := newCLISandboxStore(t)
	seedService(t, store, config.ServiceConfig{ID: "svc1", DisplayName: "Svc1", Command: "/bin/x"})
	reg := &svcRecorder{}
	ipc, ui := newServicesIPC(t, store, reg)

	ipcRemoveService(ipc, mustJSON(t, ipcIDMsg{ID: "svc1"}))

	if cfg, _ := config.FindServiceByID(store.Get(), "svc1"); cfg != nil {
		t.Error("service should have been removed from settings")
	}
	if !ui.hasEvent("onServiceRemoved") {
		t.Error("onServiceRemoved not emitted")
	}
	if len(reg.stopped) != 1 || reg.stopped[0] != "svc1" {
		t.Errorf("removed service should be stopped; stopped=%v", reg.stopped)
	}
}

func TestIPCStopService_StopsAsync(t *testing.T) {
	store := newCLISandboxStore(t)
	seedService(t, store, config.ServiceConfig{ID: "svc1", DisplayName: "Svc1", Command: "/bin/x"})
	reg := &svcRecorder{}
	ipc, ui := newServicesIPC(t, store, reg)

	ipcStopService(ipc, mustJSON(t, ipcIDMsg{ID: "svc1"}))

	if len(reg.stopped) != 1 || reg.stopped[0] != "svc1" {
		t.Errorf("stop should have been called; stopped=%v", reg.stopped)
	}
	if !ui.hasEvent("onServiceStatus") {
		t.Error("refreshServiceUI should have emitted onServiceStatus")
	}
}

// A validation failure commits nothing, so the UI must not be told the edit landed.
func TestIPCUpdateService_RunningValidationFailureIsNotReportedAsUpdated(t *testing.T) {
	store := newCLISandboxStore(t)
	seedService(t, store, config.ServiceConfig{ID: "svc1", DisplayName: "Svc1", Command: "/bin/old"})
	reg := &svcRecorder{running: map[string]bool{"svc1": true}}
	ipc, ui := newServicesIPC(t, store, reg)

	ipcUpdateService(ipc, mustJSON(t, ipcServiceMsg{ID: "svc1", DisplayName: "Svc1", Command: ""}))

	msg := lastSettingsError(t, ui)
	if strings.Contains(msg, "updated") {
		t.Fatalf("nothing was persisted, so the error must not claim an update landed: %q", msg)
	}
	if msg != "command is required" {
		t.Fatalf("error = %q, want %q", msg, "command is required")
	}
	if svc, _ := config.FindServiceByID(store.Get(), "svc1"); svc.Command != "/bin/old" {
		t.Fatalf("command should be untouched, got %q", svc.Command)
	}
}

// A restart failure DID commit the edit, so the view has to be refreshed.
func TestIPCUpdateService_RestartFailureStillRefreshesUI(t *testing.T) {
	store := newCLISandboxStore(t)
	seedService(t, store, config.ServiceConfig{ID: "svc1", DisplayName: "Svc1", Command: "/bin/old"})
	reg := &svcRecorder{running: map[string]bool{"svc1": true}, reloadErr: errors.New("boom")}
	ipc, ui := newServicesIPC(t, store, reg)

	ipcUpdateService(ipc, mustJSON(t, ipcServiceMsg{ID: "svc1", DisplayName: "Svc1", Command: "/bin/new"}))

	if svc, _ := config.FindServiceByID(store.Get(), "svc1"); svc.Command != "/bin/new" {
		t.Fatalf("the edit committed, got %q", svc.Command)
	}
	var sawStatus bool
	for _, e := range ui.events {
		if e.Name == "onServiceStatus" {
			sawStatus = true
		}
	}
	if !sawStatus {
		t.Fatal("a committed edit must refresh the view even when the restart failed")
	}
}
