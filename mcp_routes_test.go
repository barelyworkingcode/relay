package main

// HTTP coverage for the ADR-014 MCP-MUTATIONS slice: RegisterMcpRoutes shares
// McpOps with the MCP Servers tab's IPC handlers (ipc_mcps.go), so these
// tests focus on the envelope -- status codes, request/response shape, the
// SSRF guard, and the two routes that must NOT exist -- rather than
// re-proving discovery itself (external_mcp_manager_test.go and friends
// already cover that against the real cmd/testmcp peer).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Mounts both RegisterProjectRoutes and RegisterMcpRoutes on one mux, the
// same as production (frontend_server.go): a genuine pattern collision
// between the two would panic right here at registration, before any test
// runs a request.
func newMcpRoutesServer(t *testing.T) (*httptest.Server, SettingsStore) {
	t.Helper()
	store := newCLISandboxStore(t)
	ops := &McpOps{Store: store, Ctx: context.Background(), Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
	mux := http.NewServeMux()
	rr := &RouteRegistrar{Mux: mux, Transport: TransportSocket}
	RegisterProjectRoutes(rr, store, &ProjectOps{Store: store}, nil, nil, nil, nil, nil)
	RegisterMcpRoutes(rr, ops)
	return httptest.NewServer(mux), store
}

func TestMcpRoutes_CreateStdioHappyPath(t *testing.T) {
	srv, store := newMcpRoutesServer(t)
	defer srv.Close()
	bin := buildTestMcpBinary(t)

	resp, body := doJSON(t, "POST", srv.URL+"/api/mcps", map[string]interface{}{
		"display_name": "Test MCP",
		"command":      bin,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d, body %s", resp.StatusCode, body)
	}
	var created mcpView
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	if created.ID != "test-mcp" {
		t.Errorf("id = %q, want slugified display name", created.ID)
	}
	if created.Command != bin {
		t.Errorf("command = %q, want %q", created.Command, bin)
	}
	if created.AuthRequired {
		t.Error("a stdio MCP has no OAuth concept; auth_required must be false")
	}

	mcps := store.Get().ExternalMcps
	if len(mcps) != 1 || mcps[0].ID != "test-mcp" {
		t.Fatalf("expected 1 persisted mcp, got %+v", mcps)
	}
}

func TestMcpRoutes_CreateValidationErrors(t *testing.T) {
	srv, _ := newMcpRoutesServer(t)
	defer srv.Close()

	cases := []struct {
		name string
		body map[string]interface{}
	}{
		{"missing display name", map[string]interface{}{"command": "/bin/true"}},
		{"missing command for stdio", map[string]interface{}{"display_name": "No Command"}},
		{"missing URL for http", map[string]interface{}{"display_name": "No URL", "transport": "http"}},
		{"invalid URL for http", map[string]interface{}{"display_name": "Bad URL", "transport": "http", "url": "://not-a-url"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, body := doJSON(t, "POST", srv.URL+"/api/mcps", c.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status %d, want 400, body %s", resp.StatusCode, body)
			}
		})
	}
}

// The load-bearing security test: validateMcpURL is an SSRF guard, and
// Add() must run it exactly as ipcAddExternalMcp did -- this proves the HTTP
// door did not skip it. A scheme validateMcpURL rejects is chosen so the
// assertion cannot pass by relay attempting and failing to connect instead.
func TestMcpRoutes_CreateHTTPRejectsSSRFURL(t *testing.T) {
	srv, store := newMcpRoutesServer(t)
	defer srv.Close()

	resp, body := doJSON(t, "POST", srv.URL+"/api/mcps", map[string]interface{}{
		"display_name": "SSRF MCP",
		"transport":    "http",
		"url":          "ftp://internal.example.com/mcp",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400, body %s", resp.StatusCode, body)
	}
	if got := store.Get().ExternalMcps; len(got) != 0 {
		t.Fatalf("a URL the SSRF guard rejects must not persist: %+v", got)
	}
}

func TestMcpRoutes_CreateMalformedJSON(t *testing.T) {
	srv, _ := newMcpRoutesServer(t)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/api/mcps", "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
}

// A discovery failure (the stdio spawn itself failing) is the upstream
// MCP's fault, not relay's -- 502, not 500 -- and nothing must be persisted
// for a config that was never actually reachable.
func TestMcpRoutes_CreateDiscoveryFailureMaps502(t *testing.T) {
	srv, store := newMcpRoutesServer(t)
	defer srv.Close()

	resp, body := doJSON(t, "POST", srv.URL+"/api/mcps", map[string]interface{}{
		"display_name": "Broken MCP",
		"command":      "/nonexistent/does-not-exist-binary-zzz",
	})
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502, body %s", resp.StatusCode, body)
	}
	if got := store.Get().ExternalMcps; len(got) != 0 {
		t.Fatalf("a failed discovery must not persist a record: %+v", got)
	}
}

func TestMcpRoutes_DeleteHappyPathAnd404(t *testing.T) {
	srv, store := newMcpRoutesServer(t)
	defer srv.Close()
	bin := buildTestMcpBinary(t)

	_, body := doJSON(t, "POST", srv.URL+"/api/mcps", map[string]interface{}{
		"display_name": "Deletable MCP",
		"command":      bin,
	})
	var created mcpView
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}

	resp, body := doJSON(t, "DELETE", srv.URL+"/api/mcps/"+created.ID, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: status %d, body %s", resp.StatusCode, body)
	}
	if got := store.Get().ExternalMcps; len(got) != 0 {
		t.Fatalf("the record survived its own delete: %+v", got)
	}

	resp, body = doJSON(t, "DELETE", srv.URL+"/api/mcps/"+created.ID, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete: status %d, want 404, body %s", resp.StatusCode, body)
	}

	resp, body = doJSON(t, "DELETE", srv.URL+"/api/mcps/never-existed", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete of unknown id: status %d, want 404, body %s", resp.StatusCode, body)
	}
}

// The load-bearing test in this file, mirroring
// TestEnrolmentRoutes_CreateNeverLeaksKeyMaterial: ExternalMcp carries
// OAuthState (client secret, access/refresh tokens), and mcpView is an
// explicit allow-list precisely so none of it can ride out over the wire --
// on the create response, or on any other route that reads the same record
// back.
func TestMcpRoutes_NeverLeaksOAuthSecrets(t *testing.T) {
	srv, store := newMcpRoutesServer(t)
	defer srv.Close()
	bin := buildTestMcpBinary(t)

	resp, body := doJSON(t, "POST", srv.URL+"/api/mcps", map[string]interface{}{
		"display_name": "Secret MCP",
		"command":      bin,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d, body %s", resp.StatusCode, body)
	}
	var created mcpView
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}

	// Every field on the wire is one mcpViewOf can produce; an unexpected
	// field is the shape a secret would arrive in.
	var generic map[string]interface{}
	if err := json.Unmarshal(body, &generic); err != nil {
		t.Fatalf("decode as generic map: %v", err)
	}
	for k := range generic {
		switch k {
		case "id", "display_name", "transport", "command", "args", "env", "url", "tcc_services", "auth_required":
		default:
			t.Errorf("unexpected field %q in create response", k)
		}
	}

	// Simulate a completed OAuth flow (authenticate_mcp, IPC-only) landing
	// on this record after creation, then prove no route hands the secret
	// back out -- not the record's own list entry, not anywhere else this
	// mux answers a request about MCPs.
	const clientSecret = "s3cr3t-oauth-client-secret"
	const accessToken = "s3cr3t-oauth-access-token"
	const refreshToken = "s3cr3t-oauth-refresh-token"
	if err := store.With(func(s *Settings) {
		s.UpdateOAuthState(created.ID, &OAuthState{
			ClientID:     "some-oauth-client-id",
			ClientSecret: NewSecret(clientSecret),
			AccessToken:  NewSecret(accessToken),
			RefreshToken: NewSecret(refreshToken),
			TokenExpiry:  "2099-01-01T00:00:00Z",
		})
	}); err != nil {
		t.Fatalf("seed oauth state: %v", err)
	}

	resp, listBody := doJSON(t, "GET", srv.URL+"/api/mcps", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: status %d, body %s", resp.StatusCode, listBody)
	}
	raw := string(listBody)
	for _, secret := range []string{clientSecret, accessToken, refreshToken} {
		if strings.Contains(raw, secret) {
			t.Fatalf("OAuth secret %q reached the HTTP response: %s", secret, raw)
		}
	}
}

// Pins the deliberate exclusion (ADR-014 section 4): authenticate_mcp opens
// a browser on the host and reset_mcp_permissions fires TCC prompts from
// relay's own bundle, and neither can be implemented behind an API call.
// Someone adding a route for either later should have to delete this test
// on purpose.
func TestMcpRoutes_NoRouteForAuthenticateOrResetPermissions(t *testing.T) {
	srv, store := newMcpRoutesServer(t)
	defer srv.Close()
	bin := buildTestMcpBinary(t)

	_, body := doJSON(t, "POST", srv.URL+"/api/mcps", map[string]interface{}{
		"display_name": "Native Residue MCP",
		"command":      bin,
	})
	var created mcpView
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	if got := store.Get().ExternalMcps; len(got) != 1 {
		t.Fatalf("setup: expected 1 persisted mcp, got %+v", got)
	}

	paths := []string{
		"/api/mcps/" + created.ID + "/authenticate",
		"/api/mcps/" + created.ID + "/reset_permissions",
		"/api/mcps/" + created.ID + "/reset-permissions",
	}
	for _, path := range paths {
		for _, method := range []string{"POST", "GET"} {
			t.Run(method+" "+path, func(t *testing.T) {
				resp, respBody := doJSON(t, method, srv.URL+path, nil)
				if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
					t.Fatalf("status %d, want 404 or 405, body %s", resp.StatusCode, respBody)
				}
			})
		}
	}
}
