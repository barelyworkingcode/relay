package main

// HTTP coverage for the ADR-014 services slice: RegisterServiceRoutes shares
// ServiceOps with the Services-tab IPC handlers (ipc_services_test.go), so
// these tests focus on the envelope — status codes, request/response shape —
// rather than re-proving validation and restart semantics ServiceOps already
// covers. svcRecorder (ipc_services_test.go) is reused as the fake
// service.Manager rather than duplicated.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/service"
)

func newServiceRoutesServer(t *testing.T, reg service.Manager, onChange func()) (*httptest.Server, config.SettingsStore) {
	t.Helper()
	store := newCLISandboxStore(t)
	ops := &ServiceOps{Store: store, Registry: reg, OnChange: onChange, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
	mux := http.NewServeMux()
	RegisterServiceRoutes(&control.RouteRegistrar{CredentialID: APICredentialIDFromContext, Mux: mux, Transport: control.TransportSocket}, ops)
	return httptest.NewServer(mux), store
}

func TestServiceRoutes_CreateAndGet(t *testing.T) {
	srv, _ := newServiceRoutesServer(t, &svcRecorder{}, nil)
	defer srv.Close()

	resp, body := doJSON(t, "POST", srv.URL+"/api/services", map[string]interface{}{
		"display_name": "My Svc",
		"command":      "/bin/x",
		"args":         []string{"--flag"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d, body %s", resp.StatusCode, body)
	}
	var created serviceView
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	if created.ID != "my-svc" {
		t.Errorf("id = %q, want slugified my-svc", created.ID)
	}
	if created.Running {
		t.Error("freshly created (non-autostart) service should not be running")
	}

	resp, body = doJSON(t, "GET", srv.URL+"/api/services/"+created.ID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: status %d, body %s", resp.StatusCode, body)
	}
	var fetched serviceView
	if err := json.Unmarshal(body, &fetched); err != nil {
		t.Fatalf("decode fetched: %v", err)
	}
	if fetched.ID != created.ID || fetched.Command != "/bin/x" {
		t.Errorf("fetched mismatch: %+v", fetched)
	}

	resp, body = doJSON(t, "GET", srv.URL+"/api/services", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: status %d, body %s", resp.StatusCode, body)
	}
	var listed []serviceView
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Errorf("expected 1 service with id %s, got %+v", created.ID, listed)
	}
}

func TestServiceRoutes_CreateValidation(t *testing.T) {
	srv, _ := newServiceRoutesServer(t, &svcRecorder{}, nil)
	defer srv.Close()

	t.Run("missing display name", func(t *testing.T) {
		resp, body := doJSON(t, "POST", srv.URL+"/api/services", map[string]interface{}{
			"command": "/bin/x",
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status %d, body %s", resp.StatusCode, body)
		}
	})
	t.Run("missing command", func(t *testing.T) {
		resp, body := doJSON(t, "POST", srv.URL+"/api/services", map[string]interface{}{
			"display_name": "X",
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status %d, body %s", resp.StatusCode, body)
		}
	})
	t.Run("malformed JSON", func(t *testing.T) {
		resp, err := http.Post(srv.URL+"/api/services", "application/json", nil)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", resp.StatusCode)
		}
	})
}

func TestServiceRoutes_CreateAutostartStartsService(t *testing.T) {
	reg := &svcRecorder{}
	srv, _ := newServiceRoutesServer(t, reg, nil)
	defer srv.Close()

	resp, body := doJSON(t, "POST", srv.URL+"/api/services", map[string]interface{}{
		"display_name": "Auto Svc",
		"command":      "/bin/x",
		"autostart":    true,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d, body %s", resp.StatusCode, body)
	}
	if len(reg.started) != 1 || reg.started[0] != "auto-svc" {
		t.Errorf("autostart service should be started; started=%v", reg.started)
	}
}

func TestServiceRoutes_UpdateValidation(t *testing.T) {
	srv, store := newServiceRoutesServer(t, &svcRecorder{}, nil)
	defer srv.Close()
	seedService(t, store, config.ServiceConfig{ID: "svc1", DisplayName: "Svc1", Command: "/bin/old"})

	resp, body := doJSON(t, "PUT", srv.URL+"/api/services/svc1", map[string]interface{}{
		"display_name": "Svc1",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, body %s", resp.StatusCode, body)
	}
}

func TestServiceRoutes_UpdateRestartsOnlyIfRunning(t *testing.T) {
	t.Run("running service reloads", func(t *testing.T) {
		reg := &svcRecorder{running: map[string]bool{"svc1": true}}
		srv, store := newServiceRoutesServer(t, reg, nil)
		defer srv.Close()
		seedService(t, store, config.ServiceConfig{ID: "svc1", DisplayName: "Svc1", Command: "/bin/old"})

		resp, body := doJSON(t, "PUT", srv.URL+"/api/services/svc1", map[string]interface{}{
			"display_name": "Svc1",
			"command":      "/bin/new",
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("update: status %d, body %s", resp.StatusCode, body)
		}
		if len(reg.reloaded) != 1 || reg.reloaded[0] != "svc1" {
			t.Errorf("running service should be reloaded; reloaded=%v", reg.reloaded)
		}
	})

	t.Run("stopped service does not reload", func(t *testing.T) {
		reg := &svcRecorder{}
		srv, store := newServiceRoutesServer(t, reg, nil)
		defer srv.Close()
		seedService(t, store, config.ServiceConfig{ID: "svc1", DisplayName: "Svc1", Command: "/bin/old"})

		resp, body := doJSON(t, "PUT", srv.URL+"/api/services/svc1", map[string]interface{}{
			"display_name": "Svc1",
			"command":      "/bin/new",
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("update: status %d, body %s", resp.StatusCode, body)
		}
		if len(reg.reloaded) != 0 {
			t.Errorf("stopped service should not be reloaded; reloaded=%v", reg.reloaded)
		}
		if cfg, _ := config.FindServiceByID(store.Get(), "svc1"); cfg == nil || cfg.Command != "/bin/new" {
			t.Errorf("updated command not persisted: %+v", cfg)
		}
	})
}

func TestServiceRoutes_Delete(t *testing.T) {
	reg := &svcRecorder{}
	srv, store := newServiceRoutesServer(t, reg, nil)
	defer srv.Close()
	seedService(t, store, config.ServiceConfig{ID: "svc1", DisplayName: "Svc1", Command: "/bin/x"})

	resp, _ := doJSON(t, "DELETE", srv.URL+"/api/services/svc1", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}
	if cfg, _ := config.FindServiceByID(store.Get(), "svc1"); cfg != nil {
		t.Error("service should have been removed from settings")
	}
	if len(reg.stopped) != 1 || reg.stopped[0] != "svc1" {
		t.Errorf("deleted service should be stopped; stopped=%v", reg.stopped)
	}

	resp, _ = doJSON(t, "DELETE", srv.URL+"/api/services/svc1", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 on second delete, got %d", resp.StatusCode)
	}
}

func TestServiceRoutes_StartStop(t *testing.T) {
	reg := &svcRecorder{}
	srv, store := newServiceRoutesServer(t, reg, nil)
	defer srv.Close()
	seedService(t, store, config.ServiceConfig{ID: "svc1", DisplayName: "Svc1", Command: "/bin/x"})

	resp, body := doJSON(t, "POST", srv.URL+"/api/services/svc1/start", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start: status %d, body %s", resp.StatusCode, body)
	}
	if len(reg.started) != 1 || reg.started[0] != "svc1" {
		t.Errorf("start should have called Registry.Start; started=%v", reg.started)
	}

	resp, body = doJSON(t, "POST", srv.URL+"/api/services/svc1/stop", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stop: status %d, body %s", resp.StatusCode, body)
	}
	if len(reg.stopped) != 1 || reg.stopped[0] != "svc1" {
		t.Errorf("stop should have called Registry.Stop; stopped=%v", reg.stopped)
	}
}

func TestServiceRoutes_Autostart(t *testing.T) {
	srv, store := newServiceRoutesServer(t, &svcRecorder{}, nil)
	defer srv.Close()
	seedService(t, store, config.ServiceConfig{ID: "svc1", DisplayName: "Svc1", Command: "/bin/x", Autostart: false})

	resp, body := doJSON(t, "PUT", srv.URL+"/api/services/svc1/autostart", map[string]interface{}{
		"autostart": true,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("autostart: status %d, body %s", resp.StatusCode, body)
	}
	if cfg, _ := config.FindServiceByID(store.Get(), "svc1"); cfg == nil || !cfg.Autostart {
		t.Errorf("autostart flag not persisted: %+v", cfg)
	}
}

func TestServiceRoutes_ListRunningState(t *testing.T) {
	reg := &svcRecorder{running: map[string]bool{"svc1": true}}
	srv, store := newServiceRoutesServer(t, reg, nil)
	defer srv.Close()
	seedService(t, store, config.ServiceConfig{ID: "svc1", DisplayName: "Svc1", Command: "/bin/x"})
	seedService(t, store, config.ServiceConfig{ID: "svc2", DisplayName: "Svc2", Command: "/bin/y"})

	_, body := doJSON(t, "GET", srv.URL+"/api/services", nil)
	var listed []serviceView
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	running := map[string]bool{}
	for _, v := range listed {
		running[v.ID] = v.Running
	}
	if !running["svc1"] {
		t.Error("svc1 should report running:true")
	}
	if running["svc2"] {
		t.Error("svc2 should report running:false")
	}
}

func TestServiceRoutes_UnknownID404(t *testing.T) {
	srv, _ := newServiceRoutesServer(t, &svcRecorder{}, nil)
	defer srv.Close()

	cases := []struct {
		method string
		path   string
		body   interface{}
	}{
		{"GET", "/api/services/nope", nil},
		{"PUT", "/api/services/nope", map[string]interface{}{"display_name": "X", "command": "/bin/x"}},
		{"DELETE", "/api/services/nope", nil},
		{"POST", "/api/services/nope/start", nil},
		{"POST", "/api/services/nope/stop", nil},
		{"PUT", "/api/services/nope/autostart", map[string]interface{}{"autostart": true}},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			resp, body := doJSON(t, c.method, srv.URL+c.path, c.body)
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("status %d, body %s", resp.StatusCode, body)
			}
		})
	}
}

func TestServiceRoutes_OnChangeFires(t *testing.T) {
	var changed int
	srv, _ := newServiceRoutesServer(t, &svcRecorder{}, func() { changed++ })
	defer srv.Close()

	_, body := doJSON(t, "POST", srv.URL+"/api/services", map[string]interface{}{
		"display_name": "ChangeMe",
		"command":      "/bin/x",
	})
	var created serviceView
	_ = json.Unmarshal(body, &created)
	if changed != 1 {
		t.Fatalf("expected onChange after create, got %d calls", changed)
	}

	doJSON(t, "PUT", srv.URL+"/api/services/"+created.ID, map[string]interface{}{
		"display_name": "ChangeMe",
		"command":      "/bin/y",
	})
	if changed != 2 {
		t.Fatalf("expected onChange after update, got %d calls total", changed)
	}

	doJSON(t, "DELETE", srv.URL+"/api/services/"+created.ID, nil)
	if changed != 3 {
		t.Fatalf("expected onChange after delete, got %d calls total", changed)
	}
}

// A store whose mutations always fail, so a test can tell "nothing was
// persisted" apart from "persisted, but the process action failed".
type failingStore struct {
	config.SettingsStore
	err error
}

func (f *failingStore) With(func(*config.Settings)) error { return f.err }

func TestServiceRoutes_CreateSurvivesAutostartFailure(t *testing.T) {
	reg := &svcRecorder{startErr: errors.New("exec: no such file")}
	srv, store := newServiceRoutesServer(t, reg, nil)
	defer srv.Close()

	resp, body := doJSON(t, "POST", srv.URL+"/api/services", map[string]interface{}{
		"display_name": "Flaky",
		"command":      "/bin/nope",
		"autostart":    true,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("a committed record with a failed autostart must still be 201, got %d: %s", resp.StatusCode, body)
	}
	var view serviceView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if view.ID != "flaky" {
		t.Fatalf("response must carry the record that landed, got id %q", view.ID)
	}
	if view.ProcessError == "" {
		t.Fatal("a failed autostart must be reported in process_error, not swallowed")
	}
	if svc, _ := config.FindServiceByID(store.Get(), "flaky"); svc == nil {
		t.Fatal("the record must be persisted even though autostart failed")
	}
}

func TestServiceRoutes_CreateReportsFailedPersist(t *testing.T) {
	base := newCLISandboxStore(t)
	ops := &ServiceOps{Store: &failingStore{SettingsStore: base, err: errors.New("disk full")}, Registry: &svcRecorder{}, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
	mux := http.NewServeMux()
	RegisterServiceRoutes(&control.RouteRegistrar{CredentialID: APICredentialIDFromContext, Mux: mux, Transport: control.TransportSocket}, ops)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, body := doJSON(t, "POST", srv.URL+"/api/services", map[string]interface{}{
		"display_name": "Doomed",
		"command":      "/bin/x",
	})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("a create that persisted nothing must not report success, got %d: %s", resp.StatusCode, body)
	}
	if svc, _ := config.FindServiceByID(base.Get(), "doomed"); svc != nil {
		t.Fatal("nothing should have been persisted")
	}
}

// A service unregistered from settings while its process is still running is
// reachable only through the registry; refusing to stop it would strand it.
func TestServiceRoutes_StopRunningServiceAbsentFromSettings(t *testing.T) {
	reg := &svcRecorder{running: map[string]bool{"ghost": true}}
	srv, _ := newServiceRoutesServer(t, reg, nil)
	defer srv.Close()

	resp, body := doJSON(t, "POST", srv.URL+"/api/services/ghost/stop", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stopping a live but unconfigured service must succeed, got %d: %s", resp.StatusCode, body)
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if len(reg.stopped) != 1 || reg.stopped[0] != "ghost" {
		t.Fatalf("registry should have been asked to stop the process, got %v", reg.stopped)
	}
}

func doJSONAuth(t *testing.T, method, url string, body interface{}, token string) (*http.Response, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, raw
}
