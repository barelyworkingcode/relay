package main

// Hermetic integration test for the relay ↔ relayLLM boundary.
//
// Uses FakeRelayLLMService — a stub service preloaded with relayLLM's
// declared manifest from test/fixtures/manifests/relayllm.json — to
// verify that:
//   1. A service registering relayLLM's manifest gets every declared
//      route into the dispatcher's table.
//   2. Inbound HTTP for any of those routes lands on the upstream
//      socket with the inbound Authorization stripped and the service-
//      declared token injected.
//   3. The /ws upgrade path works the same way.
//
// Drift mitigation: this test reads the manifest from a JSON file that
// relayLLM's own test suite is expected to assert against. If a route
// is added on either side without updating the file, one side breaks
// loudly.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"relaygo/bridge"
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

func TestIntegration_FakeRelayLLM_InjectsServiceToken(t *testing.T) {
	mkSandboxRelayHome(t)
	registry := NewEnhancedServiceRegistry(nil)
	fake := NewFakeRelayLLMService(t)
	_ = registry.RegisterManifest(fake.ServiceID(), fake.Socket(), fake.Token(), fake.Manifest())

	dispatcher := NewFrontendDispatcher(registry)
	srv := httptest.NewServer(dispatcher)
	defer srv.Close()

	const leak = "FRONTEND-EVE-TOKEN-NEVER-LEAK-TO-RELAYLLM"
	req, _ := http.NewRequest("POST", srv.URL+"/api/sessions/", strings.NewReader(`{"msg":"hi"}`))
	req.Header.Set("Authorization", "Bearer "+leak)
	resp, err := http.DefaultClient.Do(req)
	assertNoErr(t, err, "POST")
	resp.Body.Close()

	got := fake.LastRequest()
	if got == nil {
		t.Fatal("upstream never reached")
	}
	if strings.Contains(got.Headers.Get("Authorization"), leak) {
		t.Fatalf("Eve token leaked to relayLLM: %q", got.Headers.Get("Authorization"))
	}
	want := "Bearer " + fake.Token()
	if got.Headers.Get("Authorization") != want {
		t.Fatalf("upstream Authorization = %q; want %q", got.Headers.Get("Authorization"), want)
	}
}

func TestIntegration_FakeRelayLLM_WebSocketUpgradeAndForward(t *testing.T) {
	mkSandboxRelayHome(t)
	registry := NewEnhancedServiceRegistry(nil)

	// Replace the default echo handler with a WS upgrade so /ws works.
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	fake := NewFakeService(t, FakeServiceOptions{
		ServiceID: "relayLLM",
		Manifest:  loadManifestFixture(t, "relayllm.json"),
		Handler: func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/ws" {
				c, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer c.Close()
				_, msg, err := c.ReadMessage()
				if err != nil {
					return
				}
				_ = c.WriteMessage(websocket.TextMessage, []byte("relayllm-echo:"+string(msg)))
				return
			}
			w.WriteHeader(http.StatusOK)
		},
	})
	_ = registry.RegisterManifest(fake.ServiceID(), fake.Socket(), fake.Token(), fake.Manifest())

	dispatcher := NewFrontendDispatcher(registry)
	srv := httptest.NewServer(dispatcher)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
	conn, _, err := dialer.Dial(wsURL, nil)
	assertNoErr(t, err, "WS dial")
	defer conn.Close()
	_ = conn.WriteMessage(websocket.TextMessage, []byte("ping"))
	_, msg, err := conn.ReadMessage()
	assertNoErr(t, err, "WS read")
	if string(msg) != "relayllm-echo:ping" {
		t.Fatalf("WS payload mismatched; got %q", msg)
	}
}

func TestIntegration_FakeRelayLLM_RegistersViaBridge(t *testing.T) {
	mkSandboxRelayHome(t)

	// Real bridge + real appRouter so RegisterManifest goes through the
	// production path (service token check, manifest validation, registry
	// write, onChange fire).
	enhanced := NewEnhancedServiceRegistry(nil)
	store := NewSettingsStoreAt(bridge.ConfigDir())
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	router := &appRouter{
		store:    store,
		tools:    NewExternalMcpManager(nil),
		services: &fakeServiceReloader{},
		enhanced: enhanced,
	}
	// Plant a service token so the registration call can authenticate.
	const svcTokenPlain = "svc-token-fake-relayllm"
	router.serviceTokens.Register(hashToken(svcTokenPlain))

	srv, err := bridge.NewBridgeServer(context.Background(), router)
	assertNoErr(t, err, "NewBridgeServer")
	go func() { _ = srv.Serve() }()
	defer srv.Close()
	_ = dialUnixWithTimeout(t, bridge.SocketPath(), 2*time.Second).Close()

	fake := NewFakeRelayLLMService(t)
	client := bridge.NewClient(svcTokenPlain)
	if err := fake.Register(client); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// The router should have persisted the manifest.
	rec := enhanced.Get(fake.ServiceID())
	if rec == nil {
		t.Fatal("manifest not persisted in registry")
	}
	if rec.InternalSocket != fake.Socket() {
		t.Fatalf("socket mismatch: %s vs %s", rec.InternalSocket, fake.Socket())
	}
}
