package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/presence/presencetest"
	"github.com/barelyworkingcode/relay/internal/project"
)

type pmFixture struct {
	srv    *httptest.Server
	store  *config.FileSettingsStore
	dir    string
	events *atomic.Int64
	both   string
	work   string
	remote string
}

// pmRoutesServer seeds a both project, a work-only project and an access
// profile, and counts commit events on the queue the tray subscribes to.
func pmRoutesServer(t *testing.T, gate *presence.Gate) *pmFixture {
	t.Helper()
	mkSandboxRelayHome(t)
	dir, store := odwSandbox(t)
	store.With(func(s *config.Settings) {
		s.ExternalMcps = []config.ExternalMcp{{ID: "fsmcp", DisplayName: "fsMCP"}}
	})
	f := &pmFixture{store: store, dir: dir, events: &atomic.Int64{}}
	f.both = createTestProject(t, store, "Acme", t.TempDir(), []string{"fsmcp"}).ID
	f.work = createTestProject(t, store, "Acme work", t.TempDir(), []string{"fsmcp"}).ID
	store.With(func(s *config.Settings) {
		s.SetProjectMode(f.work, config.ProjectModeWork)
		p, err := project.CreateWithTokenKind(s, config.ProjectKindRemote, "Acme remote", "", nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("create access profile: %v", err)
		}
		f.remote = p.ID
	})
	ops := &ProjectOps{Store: store, Gate: gate, Issuance: enabledIssuanceRecorder(t), Queue: commitQueueFor(t, store, f.events)}
	mux := http.NewServeMux()
	RegisterProjectRoutes(&control.RouteRegistrar{CredentialID: APICredentialIDFromContext, Mux: mux, Transport: control.TransportSocket}, store, ops, schemaProviderFunc(testSchemas), nil, nil, nil, nil)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *pmFixture) putDefault(t *testing.T, mode string, body any) (int, map[string]string, string) {
	t.Helper()
	resp, raw := doJSON(t, "PUT", f.srv.URL+"/api/default_project/"+mode, body)
	var out map[string]string
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
	}
	return resp.StatusCode, out, string(raw)
}

func TestDefaultProject_RouteSetsClearsAndCommitsOncePerMutation(t *testing.T) {
	f := pmRoutesServer(t, allowGate(t))
	steps := []struct {
		mode, id   string
		home, work string
	}{
		{"home", f.both, f.both, ""},
		{"work", f.both, f.both, f.both},
		{"home", "", "", f.both},
	}
	for i, st := range steps {
		status, got, raw := f.putDefault(t, st.mode, map[string]string{"project_id": st.id})
		if status != http.StatusOK || got["home"] != st.home || got["work"] != st.work {
			t.Fatalf("step %d: %d %s, want 200 home=%q work=%q", i, status, raw, st.home, st.work)
		}
		if n := f.events.Load(); n != int64(i+1) {
			t.Fatalf("step %d: %d commit events, want %d", i, n, i+1)
		}
	}
	if block := f.store.Get().DefaultProject; block == nil || block.Home != "" {
		t.Errorf("stored block after clearing home = %+v", block)
	}
}

func TestDefaultProject_RouteRefusalsWriteNothing(t *testing.T) {
	f := pmRoutesServer(t, allowGate(t))
	cases := []struct {
		name, mode string
		body       any
	}{
		{"mode both", "both", map[string]string{"project_id": f.both}},
		{"mode unknown", "office", map[string]string{"project_id": f.both}},
		{"missing project_id", "home", map[string]string{}},
		{"unknown key", "home", map[string]string{"project_id": f.both, "mode": "home"}},
		{"no such project", "home", map[string]string{"project_id": "p-missing"}},
		{"access profile", "home", map[string]string{"project_id": f.remote}},
		{"mode mismatch", "home", map[string]string{"project_id": f.work}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := odwSnap(t, f.dir)
			events := f.events.Load()
			if status, _, raw := f.putDefault(t, c.mode, c.body); status != http.StatusBadRequest {
				t.Fatalf("status %d (%s), want 400", status, raw)
			}
			before.assertUntouched(t, f.dir, c.name)
			if n := f.events.Load(); n != events {
				t.Errorf("a refusal published %d commit events", n-events)
			}
		})
	}
}

func TestDefaultProject_RouteStoreFailureIs500(t *testing.T) {
	f := pmRoutesServer(t, allowGate(t))
	degraded := config.NewSettingsStoreDegraded(f.dir, errors.New("sealer unavailable"))
	ops := &ProjectOps{Store: degraded, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
	mux := http.NewServeMux()
	RegisterProjectRoutes(&control.RouteRegistrar{CredentialID: APICredentialIDFromContext, Mux: mux, Transport: control.TransportSocket}, degraded, ops, schemaProviderFunc(testSchemas), nil, nil, nil, nil)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if resp, raw := doJSON(t, "PUT", srv.URL+"/api/default_project/home", map[string]string{"project_id": f.both}); resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status %d (%s), want 500", resp.StatusCode, raw)
	}
}

func pmListProjects(t *testing.T, f *pmFixture) map[string]map[string]any {
	t.Helper()
	resp, raw := doJSON(t, "GET", f.srv.URL+"/api/projects", nil)
	var items []map[string]any
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &items) != nil {
		t.Fatalf("GET /api/projects: %d %s, want a JSON array", resp.StatusCode, raw)
	}
	byID := map[string]map[string]any{}
	for _, it := range items {
		byID[it["id"].(string)] = it
	}
	return byID
}

func TestProjectMode_ProjectListExposesModeAndDefaultFor(t *testing.T) {
	f := pmRoutesServer(t, allowGate(t))
	var edited string
	f.store.With(func(s *config.Settings) {
		for _, m := range config.DefaultModes {
			if err := s.SetDefaultProject(m, f.both); err != nil {
				t.Fatal(err)
			}
		}
		p, _ := config.FindProjectByID(s, f.remote)
		p.Mode = "office"
		edited = p.ID
	})
	byID := pmListProjects(t, f)
	for id, want := range map[string]string{f.both: "both", f.work: "work", edited: "both"} {
		if got := byID[id]["mode"]; got != want {
			t.Errorf("%s mode = %v, want %s", id, got, want)
		}
	}
	if got := byID[f.both]["default_for"]; !reflect.DeepEqual(got, []any{"home", "work"}) {
		t.Errorf("default_for = %v, want [home work]", got)
	}
	if _, has := byID[f.work]["default_for"]; has {
		t.Errorf("a project that is no default carries default_for: %v", byID[f.work])
	}
}

func TestProjectMode_HTTPCreateUpdateAndDeleteClearsDefault(t *testing.T) {
	f := pmRoutesServer(t, allowGate(t))
	resp, raw := doJSON(t, "POST", f.srv.URL+"/api/projects", map[string]any{
		"name": "Acme home", "path": t.TempDir(), "allowed_mcp_ids": []string{"fsmcp"}, "mode": "home",
	})
	var created struct{ ID, Mode string }
	if resp.StatusCode != http.StatusCreated || json.Unmarshal(raw, &created) != nil || created.Mode != "home" {
		t.Fatalf("create with mode home: %d %s", resp.StatusCode, raw)
	}

	if resp, raw := doJSON(t, "PUT", f.srv.URL+"/api/projects/"+created.ID, map[string]any{"mode": "office"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("update with an unknown mode: %d %s, want 400", resp.StatusCode, raw)
	}
	if p, _ := config.FindProjectByID(f.store.Get(), created.ID); p == nil || p.Mode != config.ProjectModeHome {
		t.Fatalf("a refused mode update changed the record: %+v", p)
	}

	if status, _, raw := f.putDefault(t, "home", map[string]string{"project_id": created.ID}); status != http.StatusOK {
		t.Fatalf("set home default: %d %s", status, raw)
	}
	if resp, raw := doJSON(t, "DELETE", f.srv.URL+"/api/projects/"+created.ID, nil); resp.StatusCode >= 300 {
		t.Fatalf("delete: %d %s", resp.StatusCode, raw)
	}
	if block := f.store.Get().DefaultProject; block == nil || block.Home != "" {
		t.Errorf("after deleting the home default the block is %+v, want non-nil with home cleared", block)
	}
}

func TestProjectMode_GateNeverConsulted(t *testing.T) {
	rec := presencetest.NewRecording(presence.ErrRefused)
	gate, err := presence.NewGate(rec)
	if err != nil {
		t.Fatal(err)
	}
	f := pmRoutesServer(t, gate)
	if resp, raw := doJSON(t, "PUT", f.srv.URL+"/api/projects/"+f.both, map[string]any{"mode": "home"}); resp.StatusCode != http.StatusOK {
		t.Errorf("mode-only update: %d %s", resp.StatusCode, raw)
	}
	if status, _, raw := f.putDefault(t, "home", map[string]string{"project_id": f.both}); status != http.StatusOK {
		t.Errorf("set default: %d %s", status, raw)
	}
	if n := rec.Calls(); n != 0 {
		t.Errorf("the presence gate was consulted %d time(s) for a label change", n)
	}
}

func TestProjectMode_NotInGrantDigestsOrGatedOps(t *testing.T) {
	work := config.ProjectMode(config.ProjectModeWork)
	create := project.CreateFields{Name: "Acme", Path: "/tmp/acme", AllowedMcpIDs: []string{"fsmcp"}}
	withMode := create
	withMode.Mode = work
	if projectCreateDigest(create) != projectCreateDigest(withMode) {
		t.Error("mode changes the project.grant create digest")
	}
	if projectUpdateDigest("p1", project.UpdateFields{}) != projectUpdateDigest("p1", project.UpdateFields{Mode: &work}) {
		t.Error("mode changes the project.grant update digest")
	}
	for _, op := range presence.GatedOps {
		if strings.Contains(op, "mode") || strings.Contains(op, "default") {
			t.Errorf("GatedOps gained %q", op)
		}
	}
}

func TestDefaultProject_RouteNeedsConfigureClass(t *testing.T) {
	store := newCLISandboxStore(t)
	srv := accNewServer(t, store, accLegacyToken)
	body := map[string]string{"project_id": ""}
	resp, raw := srv.socket(t, "PUT", "/api/default_project/home", accMint(t, store, "pm-reader", "read"), body)
	accAssertForbidden(t, resp, raw, "read-only credential setting a default project")
	// Also proves the route is mounted: the proxied catch-all would refuse a
	// configure credential too.
	resp, raw = srv.socket(t, "PUT", "/api/default_project/home", accMint(t, store, "pm-configurer", "configure"), body)
	accAssertReached(t, resp, raw, "configure credential setting a default project")
}
