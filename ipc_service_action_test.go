package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"relaygo/bridge"
)

// ---------------------------------------------------------------------------
// IPC dispatcher — end-to-end whitelist enforcement
// ---------------------------------------------------------------------------

// recordingUI captures every settings event emitted during a test run so
// assertions don't depend on platform UI plumbing.
type recordingUI struct {
	mu     sync.Mutex
	events []struct {
		Name string
		Args []interface{}
	}
}

func (r *recordingUI) EmitEvent(name string, args ...interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, struct {
		Name string
		Args []interface{}
	}{name, args})
}

func (r *recordingUI) lastResult(t *testing.T) map[string]interface{} {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.events) - 1; i >= 0; i-- {
		if r.events[i].Name == "onServiceActionResult" {
			payload, ok := r.events[i].Args[0].(map[string]interface{})
			if !ok {
				t.Fatalf("onServiceActionResult arg was %T, want map", r.events[i].Args[0])
			}
			return payload
		}
	}
	t.Fatal("no onServiceActionResult emitted")
	return nil
}

// stubPlatform is a minimal Platform impl — only DispatchToMain is used by
// the dispatcher (via dispatchEmit). Inline-runs the closure so test
// assertions can observe results synchronously.
type stubPlatform struct{}

func (stubPlatform) Init()                                     {}
func (stubPlatform) Run()                                      {}
func (stubPlatform) SetupTray(rgba []byte, w, h int)           {}
func (stubPlatform) UpdateMenu(menuJSON string)                {}
func (stubPlatform) OpenSettings(html string)                  {}
func (stubPlatform) EvalSettingsJS(js string)                  {}
func (stubPlatform) DispatchToMain(fn func())                  { fn() }
func (stubPlatform) OpenURL(url string)                        {}

func newDispatcherIPC(t *testing.T, enhanced *EnhancedServiceRegistry) (*IPCContext, *recordingUI) {
	t.Helper()
	ui := &recordingUI{}
	ipc := &IPCContext{
		Ctx:                    context.Background(),
		UI:                     ui,
		Platform:               stubPlatform{},
		Enhanced:               enhanced,
		PushServiceStatusBatch: func() {},
		// GoFunc runs inline so the test observes the action result
		// without needing a goroutine wait or a tracked WaitGroup.
		GoFunc: func(fn func()) { fn() },
	}
	return ipc, ui
}

// The manifest IS the whitelist. An action ID not declared by the named
// service must be rejected before any HTTP dispatch happens.
func TestIPCServiceAction_RejectsUnknownAction(t *testing.T) {
	srv := newFakeServiceServer(t)
	reg := NewEnhancedServiceRegistry(nil)
	_ = reg.RegisterManifest("svc-a", srv.socket, "tok", bridge.Manifest{
		Routes:  []string{"/api/a/"},
		Actions: []bridge.ActionDecl{{ID: "ping", Label: "Ping", Method: "GET", PathTemplate: "/ping"}},
	})

	ipc, ui := newDispatcherIPC(t, reg)
	raw, _ := json.Marshal(ipcServiceActionMsg{ServiceID: "svc-a", ActionID: "destroy"})
	ipcServiceAction(ipc, raw)

	got := ui.lastResult(t)
	if got["ok"] != false {
		t.Errorf("unknown action should fail: %+v", got)
	}
	if !strings.Contains(got["error"].(string), "destroy") {
		t.Errorf("error should name the action: %+v", got)
	}
	// No HTTP call should have reached the upstream — the rejection
	// happens before dispatch.
	if reqs := srv.recorded(); len(reqs) != 0 {
		t.Errorf("expected zero upstream requests on rejected action, got %d", len(reqs))
	}
}

// Unknown service ID is also rejected pre-dispatch.
func TestIPCServiceAction_RejectsUnknownService(t *testing.T) {
	reg := NewEnhancedServiceRegistry(nil)
	ipc, ui := newDispatcherIPC(t, reg)
	raw, _ := json.Marshal(ipcServiceActionMsg{ServiceID: "ghost", ActionID: "ping"})
	ipcServiceAction(ipc, raw)
	got := ui.lastResult(t)
	if got["ok"] != false {
		t.Errorf("unknown service should fail: %+v", got)
	}
}

// Happy path: declared action with a forEach placeholder dispatches the
// correct method + substituted path, and the result fires onto the UI.
func TestIPCServiceAction_HappyPathDispatchesAndReports(t *testing.T) {
	srv := newFakeServiceServer(t)
	srv.script("DELETE", "/api/x/abc", 204, "")
	reg := NewEnhancedServiceRegistry(nil)
	_ = reg.RegisterManifest("svc-a", srv.socket, "tok", bridge.Manifest{
		Routes: []string{"/api/x/"},
		Actions: []bridge.ActionDecl{{
			ID: "del", Label: "Delete", Method: "DELETE",
			PathTemplate: "/api/x/{id}", ForEach: "items",
		}},
	})

	ipc, ui := newDispatcherIPC(t, reg)
	raw, _ := json.Marshal(ipcServiceActionMsg{
		ServiceID: "svc-a",
		ActionID:  "del",
		Row:       map[string]json.RawMessage{"id": json.RawMessage(`"abc"`)},
	})
	ipcServiceAction(ipc, raw)

	got := ui.lastResult(t)
	if got["ok"] != true {
		t.Errorf("happy path should succeed: %+v", got)
	}
	reqs := srv.recorded()
	if len(reqs) != 1 || reqs[0].Method != "DELETE" || reqs[0].Path != "/api/x/abc" {
		t.Errorf("upstream call mismatch: %+v", reqs)
	}
}

// ---------------------------------------------------------------------------
// buildActionPath — the placeholder-substitution + URL-escaping core. This is
// the security boundary between UI-supplied row data and the path that
// actually gets dispatched.
// ---------------------------------------------------------------------------

func TestBuildActionPath_ForEachSubstitutesRowKey(t *testing.T) {
	action := &bridge.ActionDecl{
		ID:           "stop-llama",
		Method:       "DELETE",
		PathTemplate: "/api/llama/instances/{alias}",
		ForEach:      "instances",
	}
	row := map[string]json.RawMessage{"alias": json.RawMessage(`"qwen3-8b"`)}

	got, err := buildActionPath(action, row)
	if err != nil {
		t.Fatalf("buildActionPath: %v", err)
	}
	if want := "/api/llama/instances/qwen3-8b"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildActionPath_URLEscapesValue(t *testing.T) {
	// A row value containing a slash must not break out of its path segment.
	action := &bridge.ActionDecl{
		ID:           "stop-llama",
		PathTemplate: "/api/llama/instances/{alias}",
		ForEach:      "instances",
	}
	row := map[string]json.RawMessage{"alias": json.RawMessage(`"foo/bar"`)}

	got, err := buildActionPath(action, row)
	if err != nil {
		t.Fatalf("buildActionPath: %v", err)
	}
	if !strings.Contains(got, "foo%2Fbar") {
		t.Errorf("slash should be percent-escaped, got %q", got)
	}
}

func TestBuildActionPath_MissingRowKeyRejected(t *testing.T) {
	action := &bridge.ActionDecl{
		ID:           "stop-llama",
		PathTemplate: "/api/llama/instances/{alias}",
		ForEach:      "instances",
	}
	_, err := buildActionPath(action, map[string]json.RawMessage{"port": json.RawMessage(`8090`)})
	if err == nil {
		t.Fatal("expected error when row missing the {alias} key")
	}
	if !strings.Contains(err.Error(), "alias") {
		t.Errorf("error should name the missing key: %v", err)
	}
}

func TestBuildActionPath_GlobalActionRejectsRow(t *testing.T) {
	// A no-forEach action that somehow gets a row is a UI bug — we surface
	// it as an error rather than silently dispatching a global action with
	// surprising context.
	action := &bridge.ActionDecl{
		ID:           "reload",
		PathTemplate: "/api/reload",
	}
	_, err := buildActionPath(action, map[string]json.RawMessage{"x": json.RawMessage(`1`)})
	if err == nil {
		t.Fatal("global action with row payload should error")
	}
}

func TestBuildActionPath_GlobalActionPassesThroughCleanly(t *testing.T) {
	action := &bridge.ActionDecl{
		ID:           "reload",
		PathTemplate: "/api/reload",
	}
	got, err := buildActionPath(action, nil)
	if err != nil {
		t.Fatalf("global action with nil row: %v", err)
	}
	if got != "/api/reload" {
		t.Errorf("got %q, want /api/reload", got)
	}
}

// findAction is part of the dispatch whitelist — only declared actions
// match, by exact ID.
func TestFindAction_OnlyMatchesByID(t *testing.T) {
	actions := []bridge.ActionDecl{
		{ID: "stop-llama"},
		{ID: "restart"},
	}
	if got := findAction(actions, "stop-llama"); got == nil {
		t.Error("known action should match")
	}
	if got := findAction(actions, "STOP-LLAMA"); got != nil {
		t.Error("matching is case-sensitive; STOP-LLAMA should not match stop-llama")
	}
	if got := findAction(actions, "fake"); got != nil {
		t.Error("unknown action must not match")
	}
}
