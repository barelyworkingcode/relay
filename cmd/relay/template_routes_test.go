package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
)

func newTemplateRoutesServer(t *testing.T) (*httptest.Server, config.SettingsStore) {
	t.Helper()
	store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	seedTestTemplates(t, store)
	mux := http.NewServeMux()
	RegisterTemplateRoutes(&control.RouteRegistrar{Mux: mux, Transport: control.TransportSocket}, store)
	return httptest.NewServer(mux), store
}

// The list is what settings.json holds, nothing computed in code.
func TestTemplateRoutes_ListReturnsTheSettingsTemplates(t *testing.T) {
	srv, _ := newTemplateRoutesServer(t)
	defer srv.Close()

	resp, body := doJSON(t, "GET", srv.URL+"/api/terminal/templates", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var got []config.TerminalTemplate
	mustUnmarshal(t, body, &got)
	if want := len(testTerminalTemplates()); len(got) != want {
		t.Fatalf("got %d templates, want the %d in settings.json: %s", len(got), want, body)
	}
}

func TestTemplateRoutes_GetByID(t *testing.T) {
	srv, _ := newTemplateRoutesServer(t)
	defer srv.Close()

	resp, body := doJSON(t, "GET", srv.URL+"/api/terminal/templates/claude-code", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var got config.TerminalTemplate
	mustUnmarshal(t, body, &got)
	if got.ID != "claude-code" || got.Command != "claude" {
		t.Fatalf("unexpected template: %+v", got)
	}
}

func TestTemplateRoutes_GetByIDNotFound(t *testing.T) {
	srv, _ := newTemplateRoutesServer(t)
	defer srv.Close()

	resp, _ := doJSON(t, "GET", srv.URL+"/api/terminal/templates/does-not-exist", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestTemplateRoutes_ClassIsRead pins C1's route table: both routes must be
// reachable on every transport read is reachable on (socket and TCP), never
// gated behind a stronger class.
func TestTemplateRoutes_ClassIsRead(t *testing.T) {
	if !control.ClassReachableOn(control.ClassRead, control.TransportTCP) {
		t.Fatal("test assumption broken: ClassRead should be reachable on TCP")
	}
	store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	mux := http.NewServeMux()
	RegisterTemplateRoutes(&control.RouteRegistrar{Mux: mux, Transport: control.TransportTCP}, store)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, body := doJSON(t, "GET", srv.URL+"/api/terminal/templates", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/terminal/templates on TCP: status = %d, body = %s", resp.StatusCode, body)
	}
}

// TestTemplateRoutes_GoldenJSON pins the wire shape against a golden file so
// a field rename or reorder is a visible diff. The golden's fields mirror
// relayLLM's former GET /api/terminal/templates response
// (relayLLM/internal/config/terminal.go's TerminalTemplate: id, name,
// command, args, env, description, icon, idleTimeout, env_passthrough) minus
// useRelayToken (retired, never ported — see internal/config/templates.go) and
// builtIn (nothing is built in any more), plus the fields added since
// (sandbox, model_key, read, read_write).
func TestTemplateRoutes_GoldenJSON(t *testing.T) {
	srv, _ := newTemplateRoutesServer(t)
	defer srv.Close()

	resp, body := doJSON(t, "GET", srv.URL+"/api/terminal/templates", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	golden, err := os.ReadFile(filepath.Join("testdata", "golden_terminal_templates.json"))
	if err != nil {
		t.Fatalf("reading golden file: %v", err)
	}
	if string(body) != string(golden) {
		t.Fatalf("GET /api/terminal/templates response drifted from the golden file.\ngot:\n%s\nwant:\n%s", body, golden)
	}
}
