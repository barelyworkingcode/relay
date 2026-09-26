package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/project"
)

// newSwitchableSurfaceServer serves the project routes over whatever
// *surfaces holds at request time, so a test can take an MCP offline between
// two requests.
func newSwitchableSurfaceServer(t *testing.T, surfaces *project.McpSurfaces) (string, config.SettingsStore) {
	t.Helper()
	store := sealedSettingsStoreAt(t.TempDir())
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	store.With(func(s *config.Settings) {
		s.ExternalMcps = []config.ExternalMcp{{ID: "macmcp", DisplayName: "macMCP"}}
	})
	ops := &ProjectOps{Store: store, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
	mux := http.NewServeMux()
	RegisterProjectRoutes(&control.RouteRegistrar{CredentialID: APICredentialIDFromContext, Mux: mux, Transport: control.TransportSocket},
		store, ops, schemaProviderFunc(func() project.McpSurfaces { return *surfaces }), nil, nil, nil, nil)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL, store
}

// createLocalMacmcpProject POSTs a console project granting macmcp with an
// operator mail_accounts value and checks relay derived file_dirs onto it.
func createLocalMacmcpProject(t *testing.T, base string, store config.SettingsStore) string {
	t.Helper()
	resp, body := doJSON(t, "POST", base+"/api/projects", map[string]interface{}{
		"name":            "Acme",
		"path":            t.TempDir(),
		"allowed_mcp_ids": []string{"macmcp"},
		"context":         map[string]interface{}{"macmcp": map[string]interface{}{"mail_accounts": []string{"Alice"}}},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d: %s", resp.StatusCode, body)
	}
	var created projectView
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	stored, _ := config.FindProjectByID(store.Get(), created.ID)
	if _, ok := project.ContextValues(stored.Context["macmcp"])["file_dirs"]; !ok {
		t.Fatalf("precondition: file_dirs was not derived onto the local project: %s", stored.Context["macmcp"])
	}
	return created.ID
}

var convertToRemoteBody = map[string]interface{}{
	"kind": "remote", "path": "", "disabled_tools": map[string][]string{},
}

func TestProjectRoutes_ConvertLocalToRemoteDropsDerivedField(t *testing.T) {
	mkSandboxRelayHome(t)
	base, store := newV2ProjectRoutesServer(t)
	id := createLocalMacmcpProject(t, base, store)

	resp, body := doJSON(t, "PUT", base+"/api/projects/"+id, convertToRemoteBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("convert: status %d: %s", resp.StatusCode, body)
	}

	stored, _ := config.FindProjectByID(store.Get(), id)
	if !stored.IsRemote() {
		t.Fatal("project did not convert to remote")
	}
	values := project.ContextValues(stored.Context["macmcp"])
	if raw, ok := values["file_dirs"]; ok {
		t.Fatalf("stored context still holds file_dirs after conversion: %s", raw)
	}
	if string(values["mail_accounts"]) != `["Alice"]` {
		t.Errorf("the operator field did not survive: %s", stored.Context["macmcp"])
	}
}

func TestProjectRoutes_ConvertRefusedWhileMcpNotConnected(t *testing.T) {
	mkSandboxRelayHome(t)
	surfaces := project.McpSurfaces{"macmcp": macmcpSurface()}
	base, store := newSwitchableSurfaceServer(t, &surfaces)
	id := createLocalMacmcpProject(t, base, store)
	before, _ := config.FindProjectByID(store.Get(), id)
	beforePath, beforeCtx := before.Path, string(before.Context["macmcp"])

	surfaces = project.McpSurfaces{}
	resp, body := doJSON(t, "PUT", base+"/api/projects/"+id, convertToRemoteBody)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("conversion was not refused while macmcp is not connected: status %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "macmcp") || !strings.Contains(string(body), "not connected") {
		t.Errorf("refusal must name macmcp and say it is not connected: %s", body)
	}

	after, _ := config.FindProjectByID(store.Get(), id)
	if after.IsRemote() || after.Path != beforePath || string(after.Context["macmcp"]) != beforeCtx {
		t.Errorf("a refused conversion changed the record: kind=%q path=%q context=%s",
			after.Kind, after.Path, after.Context["macmcp"])
	}
}
