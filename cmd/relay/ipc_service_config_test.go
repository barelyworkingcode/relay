package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/sealed"
	"github.com/barelyworkingcode/relay/internal/service"
)

type fixedStore struct{ s *config.Settings }

func (f fixedStore) EnsureInitialized() error             { return nil }
func (f fixedStore) Get() *config.Settings                { return f.s }
func (f fixedStore) Reload() *config.Settings             { return f.s }
func (f fixedStore) ReloadIfChanged() *config.Settings    { return f.s }
func (f fixedStore) With(fn func(*config.Settings)) error { fn(f.s); return nil }
func (f fixedStore) Sealer() sealed.Sealer                { return nil }

type recordingServiceManager struct {
	noopServiceManager
	running   bool
	reloadIDs []string
}

func (m *recordingServiceManager) IsRunning(string) bool { return m.running }
func (m *recordingServiceManager) Reload(id string, _ *config.ServiceConfig) error {
	m.reloadIDs = append(m.reloadIDs, id)
	return nil
}

// recordingUI is defined in ipc_service_action_test; this is a same-package
// extension method on it.
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

// GoFunc runs inline, so emitted events are observable synchronously.
func newConfigIPC(t *testing.T, reg *EnhancedServiceRegistry, mgr service.Manager, serviceID, workdir string) (*IPCContext, *recordingUI) {
	t.Helper()
	ui := &recordingUI{}
	store := fixedStore{s: &config.Settings{Services: []config.ServiceConfig{{ID: serviceID, WorkingDir: workdir}}}}
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
	onDisk, _ := os.ReadFile(cfg)
	if string(onDisk) != edited {
		t.Errorf("file not written:\n got  %q\n want %q", string(onDisk), edited)
	}
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

	dispatchConfig(ipc, "relayllm", "save", `{"a":}`)

	got := ui.lastEventNamed(t, "onServiceConfigResult")
	if got["ok"] != false || !strings.Contains(got["error"].(string), "does not parse") {
		t.Errorf("malformed save should be rejected, got %+v", got)
	}
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
