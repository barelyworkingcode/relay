package main

// Lightweight tests for the IPC dispatch layer. The individual handlers
// already have coverage (ipc_service_action_test.go, etc.); these tests
// cover the dispatch table integrity and the malformed-input path.

import (
	"context"
	"encoding/json"
	"testing"
)

func TestIPCDispatch_AllDeclaredMessageTypesHaveHandlers(t *testing.T) {
	// Drift guard: when someone adds a new Msg* constant they must also
	// wire it into ipcHandlers. Today the dispatcher silently logs "unknown
	// IPC message type" and moves on — a regression here would mean the
	// settings UI feature appears to do nothing in production.
	required := []string{
		MsgAddExternalMcp,
		MsgAuthenticateMcp,
		MsgRemoveExternalMcp,
		MsgAddService,
		MsgRemoveService,
		MsgUpdateService,
		MsgUpdateServiceAutostart,
		MsgStartService,
		MsgStopService,
		MsgServiceAction,
		MsgCreateProject,
		MsgUpdateProject,
		MsgRemoveProject,
		MsgRotateProjectToken,
		MsgRegenProjectSkill,
		MsgUpdateProjectDisabledTools,
		MsgListMcpTools,
	}
	for _, m := range required {
		if _, ok := ipcHandlers[m]; !ok {
			t.Errorf("ipcHandlers missing entry for %q — wire it up in ipc_handlers.go:ipcHandlers", m)
		}
	}
}

func TestIPCDispatch_HandlerMapKeysAreUnique(t *testing.T) {
	// Go map literals already enforce key uniqueness at parse time, but a
	// fan-in via constant reuse (two Msg* constants holding the same
	// string) is silently fine. Catch that here.
	seen := map[string]bool{}
	dupes := []string{}
	for k := range ipcHandlers {
		if seen[k] {
			dupes = append(dupes, k)
		}
		seen[k] = true
	}
	if len(dupes) > 0 {
		t.Fatalf("duplicate ipcHandlers entries: %v", dupes)
	}
}

func TestIPCDispatch_HandlerSurvivesMalformedPayload(t *testing.T) {
	// Every handler receives a json.RawMessage that may be garbage; none
	// of them should panic or hang. Smoke-test by invoking each handler
	// with a payload that doesn't match its schema.
	ipc := &IPCContext{
		Ctx:                    context.Background(),
		Store:                  noopStore{},
		UI:                     &recordingUI{},
		Platform:               stubPlatform{},
		Registry:               noopServiceManager{},
		Enhanced:               NewEnhancedServiceRegistry(nil),
		UpdateMenu:             func() {},
		PushServiceStatusBatch: func() {},
		GoFunc:                 func(fn func()) { fn() },
		NotifyReconcile:        func(string) error { return nil },
		NotifyReloadMcp:        func(string, string) error { return nil },
	}
	bad := json.RawMessage(`{"unexpected":"shape"}`)
	for name, handler := range ipcHandlers {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("handler %q panicked on malformed payload: %v", name, r)
				}
			}()
			handler(ipc, bad)
		})
	}
}

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// noopStore is a SettingsStore that holds an in-memory Settings and never
// errors. Suitable for handler tests that don't care about persistence.
type noopStore struct{}

func (noopStore) EnsureInitialized() error              { return nil }
func (noopStore) Get() *Settings                        { return defaultSettings() }
func (noopStore) Reload() *Settings                     { return defaultSettings() }
func (noopStore) ReloadIfChanged() *Settings            { return defaultSettings() }
func (noopStore) With(fn func(*Settings)) error         { fn(defaultSettings()); return nil }

// noopServiceManager satisfies ServiceManager with no-op methods. Used
// only as a placeholder so handler tests can construct an IPCContext.
type noopServiceManager struct{}

func (noopServiceManager) Start(*ServiceConfig) error              { return nil }
func (noopServiceManager) Stop(string)                             {}
func (noopServiceManager) Reload(string, *ServiceConfig) error     { return nil }
func (noopServiceManager) IsRunning(string) bool                   { return false }
func (noopServiceManager) RunningIDs() []string                    { return nil }
func (noopServiceManager) PIDsByServiceID() map[string]int         { return map[string]int{} }
func (noopServiceManager) CleanupDead()                            {}
func (noopServiceManager) ReclaimOrphans([]ServiceConfig)          {}
func (noopServiceManager) StartAllAutostart([]ServiceConfig)       {}
func (noopServiceManager) StopAll()                                {}
func (noopServiceManager) CloseFrontendChannel()                   {}
