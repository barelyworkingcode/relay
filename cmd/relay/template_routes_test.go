package main

import (
	"encoding/json"
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
	RegisterTemplateRoutes(&control.RouteRegistrar{Mux: mux, Transport: control.TransportSocket}, store, &TemplateOps{Store: store})
	return httptest.NewServer(mux), store
}

// The list is what settings.json holds, narrowed to the project asked about;
// with no project it is empty, and always a JSON array.
func TestTemplateRoutes_ListIsPerProject(t *testing.T) {
	srv, store := newTemplateRoutesServer(t)
	defer srv.Close()
	if err := store.With(func(s *config.Settings) {
		s.Projects = append(s.Projects,
			config.Project{ID: "all", AllowedTemplates: []string{"*"}},
			config.Project{ID: "one", AllowedTemplates: []string{"claude-code"}},
			config.Project{ID: "none", AllowedTemplates: []string{}})
	}); err != nil {
		t.Fatal(err)
	}

	for name, c := range map[string]struct {
		query string
		want  int
	}{
		"no project":      {"", 0},
		"unknown project": {"?project=nope", 0},
		"none":            {"?project=none", 0},
		"one":             {"?project=one", 1},
		"all":             {"?project=all", len(testTerminalTemplates())},
	} {
		resp, body := doJSON(t, "GET", srv.URL+"/api/terminal/templates"+c.query, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, body = %s", name, resp.StatusCode, body)
		}
		var got []config.TerminalTemplate
		mustUnmarshal(t, body, &got)
		if got == nil || len(got) != c.want {
			t.Errorf("%s: got %s, want %d templates as an array", name, body, c.want)
		}
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
	RegisterTemplateRoutes(&control.RouteRegistrar{Mux: mux, Transport: control.TransportTCP}, store, &TemplateOps{Store: store})
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
	srv, store := newTemplateRoutesServer(t)
	defer srv.Close()
	if err := store.With(func(s *config.Settings) {
		s.Projects = append(s.Projects, config.Project{ID: "all", AllowedTemplates: []string{"*"}})
	}); err != nil {
		t.Fatal(err)
	}

	resp, body := doJSON(t, "GET", srv.URL+"/api/terminal/templates?project=all", nil)
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

func TestTemplateRoutes_CreateUpdateDelete(t *testing.T) {
	srv, store := newTemplateRoutesServer(t)
	defer srv.Close()
	url := srv.URL + "/api/terminal/templates"

	tmpl := config.TerminalTemplate{ID: "scratch", Name: "Scratch", Sandbox: ptr(true), ReadWrite: []string{"~"}, Deny: []string{"~/.ssh"}}
	if resp, body := doJSON(t, "POST", url, tmpl); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status = %d, body = %s", resp.StatusCode, body)
	}
	if resp, _ := doJSON(t, "POST", url, tmpl); resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate create: status = %d, want 409", resp.StatusCode)
	}

	tmpl.Name = "Renamed"
	tmpl.Deny = nil
	if resp, body := doJSON(t, "PUT", url+"/scratch", tmpl); resp.StatusCode != http.StatusOK {
		t.Fatalf("update: status = %d, body = %s", resp.StatusCode, body)
	}
	got, ok := config.GetTerminalTemplate(store.Get(), "scratch")
	if !ok || got.Name != "Renamed" || len(got.Deny) != 0 {
		t.Fatalf("update did not replace the record: %+v", got)
	}
	if resp, _ := doJSON(t, "PUT", url+"/missing", tmpl); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("update of a missing template: status = %d, want 404", resp.StatusCode)
	}

	if resp, _ := doJSON(t, "DELETE", url+"/scratch", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: status = %d, want 204", resp.StatusCode)
	}
	if _, ok := config.GetTerminalTemplate(store.Get(), "scratch"); ok {
		t.Fatal("template survived its delete")
	}
	if resp, _ := doJSON(t, "DELETE", url+"/scratch", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete: status = %d, want 404", resp.StatusCode)
	}
}

// The wire mirrors storage: a template that never set sandbox comes back
// without the key, not as false, and an explicit value comes back as sent.
func TestTemplateRoutes_SandboxKeyRoundTrips(t *testing.T) {
	srv, _ := newTemplateRoutesServer(t)
	defer srv.Close()
	for _, c := range []struct {
		id, field, want string // want "" means the key is absent
	}{
		{"unset", ``, ``},
		{"off", `,"sandbox":false`, `false`},
		{"on", `,"sandbox":true`, `true`},
	} {
		posted := json.RawMessage(`{"id":"` + c.id + `","name":"N"` + c.field + `}`)
		if resp, body := doJSON(t, "POST", srv.URL+"/api/terminal/templates", posted); resp.StatusCode != http.StatusCreated {
			t.Fatalf("%s: create status = %d, body = %s", c.id, resp.StatusCode, body)
		}
		resp, body := doJSON(t, "GET", srv.URL+"/api/terminal/templates/"+c.id, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: get status = %d, body = %s", c.id, resp.StatusCode, body)
		}
		var got map[string]json.RawMessage
		mustUnmarshal(t, body, &got)
		if v, has := got["sandbox"]; string(v) != c.want || has != (c.want != "") {
			t.Errorf("%s: sandbox = %q (present %v), want %q in %s", c.id, v, has, c.want, body)
		}
	}
}

func TestTemplateRoutes_RefuseInvalidTemplates(t *testing.T) {
	srv, _ := newTemplateRoutesServer(t)
	defer srv.Close()
	for name, tmpl := range map[string]config.TerminalTemplate{
		"bad id":          {ID: "../x", Name: "X"},
		"relay token":     {ID: "x", Name: "X", Args: []string{"${RELAY_TOKEN}"}},
		"relative folder": {ID: "x", Name: "X", Sandbox: ptr(true), Deny: []string{"tools"}},
	} {
		if resp, body := doJSON(t, "POST", srv.URL+"/api/terminal/templates", tmpl); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", name, resp.StatusCode, body)
		}
	}
}

// --- host templates (docs/ssh-hosts.md) ---

// newHostTemplateRoutesServer serves the console and host template routes
// over one store holding host h1 (with a shell template) and project hp on it.
func newHostTemplateRoutesServer(t *testing.T) *httptest.Server {
	t.Helper()
	store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	seedTestTemplates(t, store)
	if err := store.With(func(s *config.Settings) {
		s.Hosts = append(s.Hosts, config.Host{ID: "h1", Name: "devbox", Target: "devbox.example",
			TerminalTemplates: []config.TerminalTemplate{{ID: "shell", Name: "Shell"}}})
		s.Projects = append(s.Projects, config.Project{ID: "hp", HostID: "h1", Path: "/home/remote/p", AllowedTemplates: []string{"*"}})
	}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	rr := &control.RouteRegistrar{Mux: mux, Transport: control.TransportSocket}
	RegisterTemplateRoutes(rr, store, &TemplateOps{Store: store})
	RegisterHostTemplateRoutes(rr, &HostTemplateOps{Store: store})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestHostTemplateRoutes_CatalogCreateAndRefusals(t *testing.T) {
	srv := newHostTemplateRoutesServer(t)
	catalog := func() string {
		t.Helper()
		resp, body := doJSON(t, "GET", srv.URL+"/api/terminal/templates?project=hp", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("catalog: status = %d, body = %s", resp.StatusCode, body)
		}
		var got []config.TerminalTemplate
		mustUnmarshal(t, body, &got)
		return hostTemplateIDs(got)
	}
	if got := catalog(); got != "shell" {
		t.Fatalf("hosted project catalog = %q, want only the host's shell (no console templates)", got)
	}

	tool := config.TerminalTemplate{ID: "tool", Name: "Tool", Command: "/opt/tool"}
	if resp, body := doJSON(t, "POST", srv.URL+"/api/hosts/h1/templates", tool); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status = %d, body = %s", resp.StatusCode, body)
	}
	resp, body := doJSON(t, "GET", srv.URL+"/api/hosts/h1/templates", nil)
	var listed []config.TerminalTemplate
	mustUnmarshal(t, body, &listed)
	if resp.StatusCode != http.StatusOK || hostTemplateIDs(listed) != "shell,tool" {
		t.Fatalf("list after create: status = %d, body = %s", resp.StatusCode, body)
	}
	if got := catalog(); got != "shell,tool" {
		t.Fatalf("catalog after create = %q, want shell,tool", got)
	}

	sandboxed := config.TerminalTemplate{ID: "boxed", Name: "Boxed", Sandbox: ptr(true)}
	if resp, body := doJSON(t, "POST", srv.URL+"/api/hosts/h1/templates", sandboxed); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("sandboxed host template: status = %d, want 400 (%s)", resp.StatusCode, body)
	}
	optedOut := json.RawMessage(`{"id":"optout","name":"Opted out","sandbox":false}`)
	if resp, body := doJSON(t, "POST", srv.URL+"/api/hosts/h1/templates", optedOut); resp.StatusCode != http.StatusCreated {
		t.Errorf("host template with sandbox false: status = %d, want 201 (%s)", resp.StatusCode, body)
	}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/hosts/nope/templates", nil},
		{"POST", "/api/hosts/nope/templates", tool},
	} {
		if resp, body := doJSON(t, c.method, srv.URL+c.path, c.body); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, want 404 (%s)", c.method, c.path, resp.StatusCode, body)
		}
	}
}
