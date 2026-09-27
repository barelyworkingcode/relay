package main

// Drift mitigation: this test reads the manifest from a JSON file that
// relayLLM's own test suite is expected to assert against. If a route
// is added on either side without updating the file, one side breaks
// loudly.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIntegration_FakeRelayLLM_DispatchesEveryDeclaredRoute(t *testing.T) {
	mkSandboxRelayHome(t)
	registry := NewEnhancedServiceRegistry(nil)
	fake := NewFakeRelayLLMService(t)
	assertManifestHasRoutes(t, fake.Manifest(),
		"/api/sessions/", "/api/models", "/api/permission", "/api/status", "/ws")

	err := registry.RegisterManifest(fake.ServiceID(), fake.Socket(), fake.Token(), fake.Manifest())
	assertNoErr(t, err, "RegisterManifest")

	dispatcher := NewFrontendDispatcher(registry)
	srv := httptest.NewServer(dispatcher)
	defer srv.Close()

	// Hit every prefix and exact route declared by the manifest. For
	// prefix routes (trailing /), append a sub-segment so the longest-
	// prefix logic actually exercises the prefix branch.
	for _, route := range fake.Manifest().Routes {
		if route == "/ws" {
			continue // WS upgrade exercised separately
		}
		path := route
		if strings.HasSuffix(route, "/") {
			path += "probe"
		}
		t.Run("dispatch "+path, func(t *testing.T) {
			resp, err := http.Get(srv.URL + path)
			assertNoErr(t, err, "GET %s", path)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("expected 200 on %s; got %d", path, resp.StatusCode)
			}
		})
	}
}
