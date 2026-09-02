package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

func TestSec_FrontendTokenDoesNotLeakToUpstream_RegressionGuard(t *testing.T) {
	registry := NewEnhancedServiceRegistry(nil)
	fake := NewFakeService(t, FakeServiceOptions{
		ServiceID: "svc-secret-leak",
		Manifest:  newManifest("/api/secret/"),
	})
	_ = registry.RegisterManifest(fake.ServiceID(), fake.Socket(), fake.Token(), fake.Manifest())

	dispatcher := NewFrontendDispatcher(registry)
	srv := httptest.NewServer(dispatcher)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/secret/", nil)
	const secret = "SUPER-SECRET-FRONTEND-TOKEN-NEVER-LEAK"
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := http.DefaultClient.Do(req)
	assertNoErr(t, err, "dispatch")
	resp.Body.Close()

	got := fake.LastRequest()
	if got == nil {
		t.Fatal("upstream never reached")
	}
	if strings.Contains(got.Headers.Get("Authorization"), secret) {
		t.Fatalf("frontend token leaked to upstream: %q", got.Headers.Get("Authorization"))
	}
}

func TestSec_ConfigPath_SymlinkEscapeRejectedAtSave_RegressionGuard(t *testing.T) {
	mkSandboxRelayHome(t)
	root := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "victim.json")
	const sentinel = `{"do-not":"overwrite"}`
	if err := os.WriteFile(target, []byte(sentinel), 0o600); err != nil {
		t.Fatalf("seed victim: %v", err)
	}
	link := filepath.Join(root, "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	reg := NewEnhancedServiceRegistry(nil)
	_ = reg.RegisterManifest("relayllm", "/sock", "tok", configManifest(link))
	mgr := &recordingServiceManager{running: true}
	ipc, ui := newConfigIPC(t, reg, mgr, "relayllm", root)

	dispatchConfig(ipc, "relayllm", "save", `{"pwned":true}`)

	got := ui.lastEventNamed(t, "onServiceConfigResult")
	if got["ok"] != false || !strings.Contains(got["error"].(string), "escapes allowed root") {
		t.Errorf("symlink-escape save should be rejected, got %+v", got)
	}
	after, _ := os.ReadFile(target)
	if string(after) != sentinel {
		t.Fatalf("victim file was overwritten through symlink: %q", string(after))
	}
	if len(mgr.reloadIDs) != 0 {
		t.Errorf("no restart on a rejected save; got %v", mgr.reloadIDs)
	}
}

func TestSec_ServiceDeclaredTokenInjectedOnEveryRequest_RegressionGuard(t *testing.T) {
	registry := NewEnhancedServiceRegistry(nil)
	fake := NewFakeService(t, FakeServiceOptions{
		ServiceID: "svc-token-check",
		Manifest:  newManifest("/api/x/"),
	})
	_ = registry.RegisterManifest(fake.ServiceID(), fake.Socket(), fake.Token(), fake.Manifest())

	srv := httptest.NewServer(NewFrontendDispatcher(registry))
	defer srv.Close()

	for i := 0; i < 3; i++ {
		resp, err := http.Get(srv.URL + "/api/x/")
		assertNoErr(t, err, "dispatch iter %d", i)
		resp.Body.Close()
	}
	for i, req := range fake.Requests() {
		if got := req.Headers.Get("Authorization"); got != "Bearer "+fake.Token() {
			t.Fatalf("request[%d] upstream Authorization = %q; want %q", i, got, "Bearer "+fake.Token())
		}
	}
}

func TestSec_ActionWhitelist_RejectsUndeclaredAction(t *testing.T) {
	registry := NewEnhancedServiceRegistry(nil)
	_ = registry.RegisterManifest("svc-x", "/tmp/x.sock", "tok-x", bridge.Manifest{
		Routes: []string{"/api/x"},
		Actions: []bridge.ActionDecl{
			{ID: "real", Label: "Real", Method: "DELETE", PathTemplate: "/api/x/real"},
		},
	})
	ipc, ui := newDispatcherIPC(t, registry)

	ipcServiceAction(ipc, mustJSON(t, ipcServiceActionMsg{
		ServiceID: "svc-x",
		ActionID:  "evil-undeclared", // not in manifest
	}))
	res := ui.lastResult(t)
	if res["ok"] != false {
		t.Fatalf("undeclared action must be rejected; got %v", res)
	}
	if !strings.Contains(res["error"].(string), "not declared") {
		t.Fatalf("error should mention not-declared; got %q", res["error"])
	}
}

func TestSec_ActionWhitelist_RejectsUnknownService(t *testing.T) {
	registry := NewEnhancedServiceRegistry(nil)
	ipc, ui := newDispatcherIPC(t, registry)

	ipcServiceAction(ipc, mustJSON(t, ipcServiceActionMsg{
		ServiceID: "svc-does-not-exist",
		ActionID:  "anything",
	}))
	res := ui.lastResult(t)
	if res["ok"] != false {
		t.Fatalf("unknown service must be rejected; got %v", res)
	}
}

func TestSec_PathTemplate_EscapesRowValues(t *testing.T) {
	registry := NewEnhancedServiceRegistry(nil)
	_ = registry.RegisterManifest("svc-x", "/tmp/x.sock", "tok", bridge.Manifest{
		Routes: []string{"/api/x"},
		Actions: []bridge.ActionDecl{
			{ID: "del", Label: "Del", Method: "DELETE", PathTemplate: "/api/x/{id}", ForEach: "items"},
		},
	})

	action := &registry.Get("svc-x").Manifest.Actions[0]
	row := map[string]json.RawMessage{
		"id": json.RawMessage(`"../../../../etc/passwd"`),
	}
	path, err := buildActionPath(action, row)
	if err != nil {
		t.Fatalf("buildActionPath: %v", err)
	}
	if strings.Contains(path, "../") {
		t.Fatalf("path traversal not escaped: %q", path)
	}
	if !strings.HasPrefix(path, "/api/x/") {
		t.Fatalf("path prefix lost: %q", path)
	}
}

func TestSec_PathTemplate_EscapesSlashesInRow(t *testing.T) {
	registry := NewEnhancedServiceRegistry(nil)
	_ = registry.RegisterManifest("svc-x", "/tmp/x.sock", "tok", bridge.Manifest{
		Routes: []string{"/api/x"},
		Actions: []bridge.ActionDecl{
			{ID: "del", Label: "Del", Method: "DELETE", PathTemplate: "/api/x/{id}", ForEach: "items"},
		},
	})
	action := &registry.Get("svc-x").Manifest.Actions[0]

	row := map[string]json.RawMessage{
		"id": json.RawMessage(`"foo/bar?query=1#frag"`),
	}
	path, err := buildActionPath(action, row)
	assertNoErr(t, err, "buildActionPath")
	// Template `/api/x/{id}` has 3 slashes; any extra means a `/` from the row
	// value leaked through unescaped, which would let the user pivot to a
	// sibling endpoint by submitting "real/evil-action" as the id.
	if strings.Count(path, "/") != 3 {
		t.Fatalf("slash in row value must be escaped; got %q", path)
	}
	if strings.ContainsAny(path[len("/api/x/"):], "?#") {
		t.Fatalf("query/fragment characters must be escaped; got %q", path)
	}
}

func TestSec_PathTemplate_RejectsDotDotSegment(t *testing.T) {
	// url.PathEscape does not escape ".", so a bare "." or ".." row value has
	// no slashes for the guard above to catch, and would otherwise survive as
	// a live relative-path segment ("/api/x/.." → parent route).
	action := &bridge.ActionDecl{
		ID: "del", Method: "DELETE", PathTemplate: "/api/x/{id}", ForEach: "items",
	}
	for _, bad := range []string{`".."`, `"."`} {
		row := map[string]json.RawMessage{"id": json.RawMessage(bad)}
		if _, err := buildActionPath(action, row); err == nil {
			t.Fatalf("expected rejection of traversal value %s", bad)
		}
	}
}

func TestSec_PathTemplate_NonForEachRejectsRow(t *testing.T) {
	action := &bridge.ActionDecl{
		ID: "global", Method: "POST", PathTemplate: "/api/x/restart",
	}
	_, err := buildActionPath(action, map[string]json.RawMessage{"id": json.RawMessage(`"x"`)})
	if err == nil {
		t.Fatal("expected error on non-forEach action with row supplied")
	}
}

func TestSec_RouteConflict_PreventsClaimSquatting(t *testing.T) {
	r := NewEnhancedServiceRegistry(nil)
	_ = r.RegisterManifest("legit", "/tmp/legit.sock", "tok-legit", newManifest("/api/sessions/"))
	err := r.RegisterManifest("attacker", "/tmp/atk.sock", "tok-atk", newManifest("/api/sessions/"))
	if err == nil {
		t.Fatal("route squat must be rejected")
	}
	if r.Get("attacker") != nil {
		t.Fatal("attacker registration must not be persisted")
	}
	if r.Get("legit").InternalSocket != "/tmp/legit.sock" {
		t.Fatal("legit registration must remain authoritative")
	}
}

func TestSec_LongestPrefix_ExactRouteNeverMatchesSubpath(t *testing.T) {
	r := NewEnhancedServiceRegistry(nil)
	_ = r.RegisterManifest("svc-exact", "/tmp/e.sock", "tok", newManifest("/api/exact"))

	if r.LookupByPath("/api/exactly-different") != nil {
		t.Fatal("exact route must not match similar-prefixed paths")
	}
	if r.LookupByPath("/api/exact/sub") != nil {
		t.Fatal("exact route must not match sub-paths")
	}
}

func TestSec_RegisterManifest_RejectsEmptyServiceID(t *testing.T) {
	r := bridge.RegisterManifestRequest{
		Manifest:       bridge.Manifest{Routes: []string{"/api/x"}},
		InternalSocket: "/tmp/x.sock",
		InternalToken:  "tok",
	}
	if err := r.Validate(); err == nil {
		t.Fatal("Validate must reject empty serviceID — that's relay's only handle on this service")
	}
}

func TestSec_RegisterManifest_RejectsEmptyInternalToken(t *testing.T) {
	r := bridge.RegisterManifestRequest{
		ServiceID:      "svc-x",
		Manifest:       bridge.Manifest{Routes: []string{"/api/x"}},
		InternalSocket: "/tmp/x.sock",
		// InternalToken intentionally empty
	}
	if err := r.Validate(); err == nil {
		t.Fatal("Validate must reject empty internalToken — otherwise upstream auth is silently disabled")
	}
}

func TestSec_RegisterManifest_RejectsRoutesWithoutLeadingSlash(t *testing.T) {
	m := bridge.Manifest{Routes: []string{"api/x"}}
	if err := m.Validate(); err == nil {
		t.Fatal("Validate must reject relative-path routes — confusion with proxy targets")
	}
}

func TestSec_RegisterManifest_RejectsDuplicateRoutesWithinSingleManifest(t *testing.T) {
	m := bridge.Manifest{Routes: []string{"/api/x", "/api/x"}}
	if err := m.Validate(); err == nil {
		t.Fatal("Validate must reject duplicate routes within a single manifest")
	}
}

func TestSec_ActionDecl_RejectsUnsupportedHTTPMethod(t *testing.T) {
	m := bridge.Manifest{
		Routes: []string{"/api/x"},
		Actions: []bridge.ActionDecl{
			{ID: "bad", Label: "Bad", Method: "TRACE", PathTemplate: "/api/x"},
		},
	}
	if err := m.Validate(); err == nil {
		t.Fatal("Validate must reject non-standard HTTP methods to limit attack surface")
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
