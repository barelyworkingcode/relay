package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"relaygo/bridge"
)

// ---------------------------------------------------------------------------
// Test doubles for the config editor handler
// ---------------------------------------------------------------------------

// fixedStore returns a fixed Settings so findServiceByID resolves a service's
// WorkingDir (the config editor's allowed root).
type fixedStore struct{ s *Settings }

func (f fixedStore) EnsureInitialized() error      { return nil }
func (f fixedStore) Get() *Settings                { return f.s }
func (f fixedStore) Reload() *Settings             { return f.s }
func (f fixedStore) ReloadIfChanged() *Settings    { return f.s }
func (f fixedStore) With(fn func(*Settings)) error { fn(f.s); return nil }

// recordingServiceManager records Reload calls and reports a configurable
// IsRunning so the restart-on-save path can be asserted.
type recordingServiceManager struct {
	noopServiceManager
	running   bool
	reloadIDs []string
}

func (m *recordingServiceManager) IsRunning(string) bool { return m.running }
func (m *recordingServiceManager) Reload(id string, _ *ServiceConfig) error {
	m.reloadIDs = append(m.reloadIDs, id)
	return nil
}

// lastEventNamed returns the payload of the most recent event with the given
// name. Same-package access to recordingUI (defined in ipc_service_action_test).
func (r *recordingUI) lastEventNamed(t *testing.T, name string) map[string]interface{} {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.events) - 1; i >= 0; i-- {
		if r.events[i].Name == name {
			payload, ok := r.events[i].Args[0].(map[string]interface{})
			if !ok {
				t.Fatalf("%s arg was %T, want map", name, r.events[i].Args[0])
			}
			return payload
		}
	}
	t.Fatalf("no %s emitted", name)
	return nil
}

func (r *recordingUI) hasEvent(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.Name == name {
			return true
		}
	}
	return false
}

// newConfigIPC wires an IPCContext for config-handler tests with the service's
// WorkingDir set to workdir (the allowed root). GoFunc/DispatchToMain run inline
// so emitted events are observable synchronously.
func newConfigIPC(t *testing.T, reg *EnhancedServiceRegistry, mgr ServiceManager, serviceID, workdir string) (*IPCContext, *recordingUI) {
	t.Helper()
	ui := &recordingUI{}
	store := fixedStore{s: &Settings{Services: []ServiceConfig{{ID: serviceID, WorkingDir: workdir}}}}
	ipc := &IPCContext{
		Ctx:                    context.Background(),
		Store:                  store,
		UI:                     ui,
		Platform:               stubPlatform{},
		Registry:               mgr,
		Enhanced:               reg,
		PushServiceStatusBatch: func() {},
		GoFunc:                 func(fn func()) { fn() },
	}
	return ipc, ui
}

func configManifest(path string) bridge.Manifest {
	return bridge.Manifest{
		Routes: []string{"/api/"},
		Config: &bridge.ConfigDecl{
			Path:   path,
			Format: bridge.ConfigFormatJSONC,
			Schema: []bridge.FieldDecl{{ID: "a", Type: bridge.FieldTypeText}},
		},
	}
}

func dispatchConfig(ipc *IPCContext, serviceID, op, text string) {
	msg := ipcServiceConfigMsg{ServiceID: serviceID, Op: op, Text: text}
	raw, _ := json.Marshal(msg)
	ipcServiceConfig(ipc, raw)
}

// ---------------------------------------------------------------------------
// get
// ---------------------------------------------------------------------------

func TestIPCServiceConfig_GetReturnsFileText(t *testing.T) {
	mkSandboxRelayHome(t)
	root := t.TempDir()
	cfg := filepath.Join(root, "settings.json")
	content := "{\n  // hi\n  \"a\": \"hello\"\n}"
	writeFile(t, cfg, content)

	reg := NewEnhancedServiceRegistry(nil)
	_ = reg.RegisterManifest("relayllm", "/sock", "tok", configManifest(cfg))

	ipc, ui := newConfigIPC(t, reg, &recordingServiceManager{}, "relayllm", root)
	dispatchConfig(ipc, "relayllm", "get", "")

	got := ui.lastEventNamed(t, "onServiceConfigResult")
	if got["ok"] != true {
		t.Fatalf("get should succeed: %+v", got)
	}
	if got["text"] != content {
		t.Errorf("text mismatch:\n got  %q\n want %q", got["text"], content)
	}
}

func TestIPCServiceConfig_RejectedWhenNoConfigDeclared(t *testing.T) {
	reg := NewEnhancedServiceRegistry(nil)
	_ = reg.RegisterManifest("svc", "/sock", "tok", bridge.Manifest{Routes: []string{"/api/"}})

	ipc, ui := newConfigIPC(t, reg, &recordingServiceManager{}, "svc", t.TempDir())
	dispatchConfig(ipc, "svc", "get", "")

	got := ui.lastEventNamed(t, "onServiceConfigResult")
	if got["ok"] != false || !strings.Contains(got["error"].(string), "declares no config file") {
		t.Errorf("want no-config rejection, got %+v", got)
	}
}

func TestIPCServiceConfig_RejectedForUnknownService(t *testing.T) {
	reg := NewEnhancedServiceRegistry(nil)
	ipc, ui := newConfigIPC(t, reg, &recordingServiceManager{}, "ghost", t.TempDir())
	dispatchConfig(ipc, "ghost", "get", "")

	got := ui.lastEventNamed(t, "onServiceConfigResult")
	if got["ok"] != false || !strings.Contains(got["error"].(string), "not registered") {
		t.Errorf("want not-registered rejection, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// save
// ---------------------------------------------------------------------------

func TestIPCServiceConfig_SaveWritesAndRestarts(t *testing.T) {
	mkSandboxRelayHome(t)
	root := t.TempDir()
	cfg := filepath.Join(root, "settings.json")
	writeFile(t, cfg, `{"a":"old"}`)

	reg := NewEnhancedServiceRegistry(nil)
	_ = reg.RegisterManifest("relayllm", "/sock", "tok", configManifest(cfg))
	mgr := &recordingServiceManager{running: true}
	ipc, ui := newConfigIPC(t, reg, mgr, "relayllm", root)

	edited := "{\n  \"a\": \"new\"\n}"
	dispatchConfig(ipc, "relayllm", "save", edited)

	if got := ui.lastEventNamed(t, "onServiceConfigResult"); got["ok"] != true {
		t.Fatalf("save should succeed: %+v", got)
	}
	// File written verbatim.
	onDisk, _ := os.ReadFile(cfg)
	if string(onDisk) != edited {
		t.Errorf("file not written:\n got  %q\n want %q", string(onDisk), edited)
	}
	// Restart triggered.
	if len(mgr.reloadIDs) != 1 || mgr.reloadIDs[0] != "relayllm" {
		t.Errorf("expected Reload(relayllm); got %v", mgr.reloadIDs)
	}
	applied := ui.lastEventNamed(t, "onServiceConfigApplied")
	if applied["mode"] != "restarting" {
		t.Errorf("want mode=restarting, got %+v", applied)
	}
}

func TestIPCServiceConfig_SaveMalformedLeavesFileUnchanged(t *testing.T) {
	mkSandboxRelayHome(t)
	root := t.TempDir()
	cfg := filepath.Join(root, "settings.json")
	original := `{"a":"keep"}`
	writeFile(t, cfg, original)

	reg := NewEnhancedServiceRegistry(nil)
	_ = reg.RegisterManifest("relayllm", "/sock", "tok", configManifest(cfg))
	mgr := &recordingServiceManager{running: true}
	ipc, ui := newConfigIPC(t, reg, mgr, "relayllm", root)

	dispatchConfig(ipc, "relayllm", "save", `{"a":}`) // malformed

	got := ui.lastEventNamed(t, "onServiceConfigResult")
	if got["ok"] != false || !strings.Contains(got["error"].(string), "does not parse") {
		t.Errorf("malformed save should be rejected, got %+v", got)
	}
	// Load-bearing: the file must be untouched.
	onDisk, _ := os.ReadFile(cfg)
	if string(onDisk) != original {
		t.Errorf("file mutated on malformed save: got %q", string(onDisk))
	}
	if len(mgr.reloadIDs) != 0 {
		t.Errorf("no restart on rejected save; got %v", mgr.reloadIDs)
	}
}

func TestIPCServiceConfig_SaveLiveSkipsRestart(t *testing.T) {
	mkSandboxRelayHome(t)
	root := t.TempDir()
	cfg := filepath.Join(root, "settings.json")
	writeFile(t, cfg, `{"a":"old"}`)

	m := configManifest(cfg)
	m.Config.ApplyMode = bridge.ConfigApplyLive
	reg := NewEnhancedServiceRegistry(nil)
	_ = reg.RegisterManifest("relayllm", "/sock", "tok", m)
	mgr := &recordingServiceManager{running: true}
	ipc, ui := newConfigIPC(t, reg, mgr, "relayllm", root)

	dispatchConfig(ipc, "relayllm", "save", `{"a":"new"}`)

	if got := ui.lastEventNamed(t, "onServiceConfigResult"); got["ok"] != true {
		t.Fatalf("live save should succeed: %+v", got)
	}
	if len(mgr.reloadIDs) != 0 {
		t.Errorf("applyMode=live must not restart; got %v", mgr.reloadIDs)
	}
	applied := ui.lastEventNamed(t, "onServiceConfigApplied")
	if applied["mode"] != "saved" {
		t.Errorf("want mode=saved for live, got %+v", applied)
	}
}
