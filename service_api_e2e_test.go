package main

// End-to-end for ADR-014's claim, narrowed by ADR-015: every service
// capability is reachable through the same ServiceOps core the tray uses,
// but create (execute-class — the caller supplies the `command` that runs)
// is socket-only, while the rest of the lifecycle is read/configure and
// works the same over loopback TCP.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServiceAPI_LifecycleOverLoopback(t *testing.T) {
	dir := mkShortTempDir(t, "apie2e-")
	store := NewSettingsStoreAt(mkEmptySandboxRelayHome(t))
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	reg := &svcRecorder{}
	extMgr := NewExternalMcpManager(nil)
	ops := &ServiceOps{Store: store, Registry: reg}

	// execute-class: POST /api/services writes the caller-supplied `command`
	// into settings (ADR-015 decision 1), so it exists only on the socket
	// transport. Proven directly against a socket RouteRegistrar rather than
	// the full loopback server, which never registers this route at all.
	socketMux := http.NewServeMux()
	RegisterServiceRoutes(&RouteRegistrar{Mux: socketMux, Transport: TransportSocket}, ops)
	socketSrv := httptest.NewServer(socketMux)
	defer socketSrv.Close()

	status, body := doJSON(t, "POST", socketSrv.URL+"/api/services", map[string]interface{}{
		"display_name": "Worker",
		"command":      "/bin/sleep",
		"args":         []string{"30"},
	})
	if status.StatusCode != http.StatusCreated {
		t.Fatalf("create over socket: %d %s", status.StatusCode, body)
	}
	var created serviceView
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if created.ID != "worker" || created.Running {
		t.Fatalf("unexpected created record: %+v", created)
	}

	// The rest of the lifecycle is read/configure (ADR-015 decision 2), so
	// it runs over a real FrontendServer bound to loopback TCP — sharing the
	// same store and ops the socket half used, the same way a browser client
	// and the tray's own socket traffic share one ServiceOps.
	srv, err := NewFrontendServer(
		store, extMgr, extMgr, extMgr,
		Endpoint{Socket: filepath.Join(dir, "frontend.sock"), Token: "tok"},
		NewEnhancedServiceRegistry(nil), nil, nil, ops, nil, nil, nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("NewFrontendServer: %v", err)
	}
	if err := srv.ListenLoopback("127.0.0.1:0"); err != nil {
		t.Fatalf("ListenLoopback: %v", err)
	}
	go func() { _ = srv.Serve() }()
	go func() { _ = srv.ServeLoopback() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})

	base := "http://" + srv.tcpLn.Addr().String()
	call := func(method, path string, body interface{}) (int, []byte) {
		t.Helper()
		resp, raw := doJSONAuth(t, method, base+path, body, "tok")
		return resp.StatusCode, raw
	}

	if status, body := call("POST", "/api/services/worker/start", nil); status != http.StatusOK {
		t.Fatalf("start: %d %s", status, body)
	}
	reg.mu.Lock()
	started := append([]string(nil), reg.started...)
	reg.mu.Unlock()
	if len(started) != 1 || started[0] != "worker" {
		t.Fatalf("the API must drive the same registry the tray does, got %v", started)
	}

	if status, body := call("PUT", "/api/services/worker/autostart", map[string]interface{}{"autostart": true}); status != http.StatusOK {
		t.Fatalf("autostart: %d %s", status, body)
	}
	if svc, _ := store.Get().findServiceByID("worker"); svc == nil || !svc.Autostart {
		t.Fatal("autostart must be persisted to settings")
	}

	listStatus, listBody := call("GET", "/api/services", nil)
	if listStatus != http.StatusOK {
		t.Fatalf("list: %d %s", listStatus, listBody)
	}
	var list []serviceView
	if err := json.Unmarshal(listBody, &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(list) != 1 || list[0].ID != "worker" {
		t.Fatalf("list: %+v", list)
	}

	if status, body := call("DELETE", "/api/services/worker", nil); status != http.StatusNoContent {
		t.Fatalf("delete: %d %s", status, body)
	}
	if svc, _ := store.Get().findServiceByID("worker"); svc != nil {
		t.Fatal("delete must remove the record from settings")
	}
	reg.mu.Lock()
	stopped := append([]string(nil), reg.stopped...)
	reg.mu.Unlock()
	if len(stopped) != 1 || stopped[0] != "worker" {
		t.Fatalf("delete must stop the process, got %v", stopped)
	}
}

func TestServiceAPI_UnauthenticatedIsRefused(t *testing.T) {
	dir := mkShortTempDir(t, "apiauth-")
	store := NewSettingsStoreAt(mkEmptySandboxRelayHome(t))
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	extMgr := NewExternalMcpManager(nil)
	srv, err := NewFrontendServer(
		store, extMgr, extMgr, extMgr,
		Endpoint{Socket: filepath.Join(dir, "frontend.sock"), Token: "tok"},
		NewEnhancedServiceRegistry(nil), nil, nil,
		&ServiceOps{Store: store, Registry: &svcRecorder{}}, nil, nil, nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("NewFrontendServer: %v", err)
	}
	if err := srv.ListenLoopback("127.0.0.1:0"); err != nil {
		t.Fatalf("ListenLoopback: %v", err)
	}
	go func() { _ = srv.ServeLoopback() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})

	resp, err := http.Get("http://" + srv.tcpLn.Addr().String() + "/api/services")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the TCP door must not be an unauthenticated bypass, got %d", resp.StatusCode)
	}
}

// TestServiceAPI_TCPMuxRejectsExecuteRoutesAsMissing pins ADR-015 decision
// 2 end to end: POST /api/services and PUT /api/remote are execute-class,
// so registerFrontendRoutes never hands them to the TCP mux at all. Both
// paths ARE registered on TCP under another method (GET /api/services, GET
// /api/remote), so http.ServeMux answers 405 with an Allow header naming
// what it does serve. That is the mux refusing before any handler, and the
// only reason it is not a 404: the "/" catch-all is socket-only (ADR-016
// decision 4), so nothing on this mux absorbs a near-miss. Each assertion also
// checks the side effect the real handler would have caused (a persisted
// service record; a changed remote config) stayed absent, which is what
// tells the mux's refusal apart from a handler that ran, refused, and
// happened to answer on its own.
func TestServiceAPI_TCPMuxRejectsExecuteRoutesAsMissing(t *testing.T) {
	dir := mkShortTempDir(t, "apie2e-tcp404-")
	store := NewSettingsStoreAt(mkEmptySandboxRelayHome(t))
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	extMgr := NewExternalMcpManager(nil)
	ops := &ServiceOps{Store: store, Registry: &svcRecorder{}}
	enrolmentOps := &EnrolmentOps{Store: store}

	srv, err := NewFrontendServer(
		store, extMgr, extMgr, extMgr,
		Endpoint{Socket: filepath.Join(dir, "frontend.sock"), Token: "tok"},
		NewEnhancedServiceRegistry(nil), nil, nil, ops, enrolmentOps, nil, nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("NewFrontendServer: %v", err)
	}
	if err := srv.ListenLoopback("127.0.0.1:0"); err != nil {
		t.Fatalf("ListenLoopback: %v", err)
	}
	go func() { _ = srv.Serve() }()
	go func() { _ = srv.ServeLoopback() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})

	base := "http://" + srv.tcpLn.Addr().String()

	resp, body := doJSONAuth(t, "POST", base+"/api/services", map[string]interface{}{
		"display_name": "Phantom",
		"command":      "/bin/true",
	}, "tok")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/services on TCP must be the mux's own refusal, got %d: %s", resp.StatusCode, body)
	}
	if allow := resp.Header.Get("Allow"); allow == "" || strings.Contains(allow, "POST") {
		t.Fatalf("Allow = %q; want http.ServeMux's own 405 naming only the methods it serves", allow)
	}
	if svc, _ := store.Get().findServiceByID("phantom"); svc != nil {
		t.Fatal("POST /api/services must never have reached ServiceOps.Create on TCP")
	}

	before, err := enrolmentOps.RemoteConfig()
	if err != nil {
		t.Fatalf("RemoteConfig: %v", err)
	}
	resp, body = doJSONAuth(t, "PUT", base+"/api/remote", map[string]interface{}{
		"enabled": true,
		"listen":  "127.0.0.1:9999",
	}, "tok")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /api/remote on TCP must be the mux's own refusal, got %d: %s", resp.StatusCode, body)
	}
	if allow := resp.Header.Get("Allow"); allow == "" || strings.Contains(allow, "PUT") {
		t.Fatalf("Allow = %q; want http.ServeMux's own 405 naming only the methods it serves", allow)
	}
	after, err := enrolmentOps.RemoteConfig()
	if err != nil {
		t.Fatalf("RemoteConfig: %v", err)
	}
	if after != before {
		t.Fatalf("PUT /api/remote must never have reached EnrolmentOps.SetRemoteConfig on TCP: before=%+v after=%+v", before, after)
	}
}
