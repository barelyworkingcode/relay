package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
	"github.com/barelyworkingcode/relay/internal/service"
)

// relaySessionsTestBinary builds the real cmd/relaysessions binary once per
// test run -- the same build this package's own production code ships and
// BuiltinRelaySessionsService launches, never a stand-in.
var (
	relaySessionsBinOnce sync.Once
	relaySessionsBinPath string
	relaySessionsBinErr  error
)

func relaySessionsTestBinary(t *testing.T) string {
	t.Helper()
	relaySessionsBinOnce.Do(func() {
		dir, err := os.MkdirTemp("/tmp", "relaysessions-bin-")
		if err != nil {
			relaySessionsBinErr = err
			return
		}
		path := filepath.Join(dir, "relay-sessions")
		cmd := exec.Command("go", "build", "-o", path, "./cmd/relaysessions")
		cmd.Dir = repoRoot(t)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			relaySessionsBinErr = err
			return
		}
		relaySessionsBinPath = path
	})
	if relaySessionsBinErr != nil {
		t.Fatalf("build cmd/relaysessions: %v", relaySessionsBinErr)
	}
	return relaySessionsBinPath
}

// TestRealRelaySessionsBinary_RegistersItsManifest drives the actual,
// unmodified `relay-sessions service` binary through a real service.Registry
// launch -- byte-for-byte the shape BuiltinRelaySessionsService gives it in
// production (internal/service/builtin_sessions.go: "service",
// "-internal-socket", "-hook-socket", nothing else) -- against a real bridge
// server, and confirms it both Hellos and registers a manifest relay's
// EnhancedServiceRegistry can resolve: the fix for the real gap where a
// production relay-sessions launch never told relay how to reach it, so
// every real /api/terminals or /api/sessions launch 502'd regardless of how
// healthy the host process was.
func TestRealRelaySessionsBinary_RegistersItsManifest(t *testing.T) {
	relaySessionsBin := relaySessionsTestBinary(t)
	store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")

	launches := service.NewLaunches()
	enhanced := NewEnhancedServiceRegistry(nil)
	router := &appRouter{
		store: store, tools: mcpbroker.NewManager(nil), services: &fakeServiceReloader{},
		enhanced: enhanced, launches: launches, onChange: func() {},
	}
	bsrv, err := bridge.NewBridgeServer(context.Background(), router)
	assertNoErr(t, err, "NewBridgeServer")
	go func() { _ = bsrv.Serve() }()
	t.Cleanup(bsrv.Close)
	_ = dialUnixWithTimeout(t, bridge.SocketPath(), 2*time.Second).Close()

	sockDir := mkShortTempDir(t, "relaysessions-manifest-")
	logDir := mkShortTempDir(t, "relaysessions-manifest-log-")
	registry := service.NewRegistry()
	registry.Launches = launches
	registry.Enhanced = enhanced
	registry.OpenLog = func(id string) (io.WriteCloser, error) {
		return os.OpenFile(filepath.Join(logDir, id+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	}
	t.Cleanup(registry.StopAll)

	internalSock := filepath.Join(sockDir, "internal.sock")

	// Byte-for-byte BuiltinRelaySessionsService's own Args
	// (internal/service/builtin_sessions.go) -- production's real launch
	// shape.
	cfg := &config.ServiceConfig{
		ID: config.RelaySessionsServiceID, DisplayName: "Session Host", Command: relaySessionsBin,
		Args: []string{
			"service",
			"-internal-socket", internalSock,
			"-hook-socket", filepath.Join(sockDir, "hook.sock"),
			"-relay-mcp-command", filepath.Join(sockDir, "relay"),
		},
		Capabilities: []config.ServiceCapability{config.ServiceCapabilityManifest, config.ServiceCapabilitySessions},
	}
	assertNoErr(t, registry.Start(cfg), "start the real relay-sessions binary")

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := launches.Bound(config.RelaySessionsServiceID); ok {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, ok := launches.Bound(config.RelaySessionsServiceID); !ok {
		t.Fatal("the real relay-sessions binary never even Hello'd onto the bridge")
	}

	var es *EnhancedService
	for time.Now().Before(deadline) {
		if es = enhanced.Get(config.RelaySessionsServiceID); es != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if es == nil {
		t.Fatal("the real relay-sessions binary never registered its manifest")
	}
	if es.InternalSocket != internalSock {
		t.Fatalf("registered internal socket = %q, want %q", es.InternalSocket, internalSock)
	}
	if es.InternalToken == "" {
		t.Fatal("registered manifest carries no internal token")
	}
	wantRoutes := make(map[string]bool, len(config.RelaySessionsManifestRoutes))
	for _, r := range config.RelaySessionsManifestRoutes {
		wantRoutes[r] = true
	}
	if len(es.Manifest.Routes) != len(wantRoutes) {
		t.Fatalf("registered routes = %v, want exactly %v", es.Manifest.Routes, wantRoutes)
	}
	for _, r := range es.Manifest.Routes {
		if !wantRoutes[r] {
			t.Fatalf("unexpected registered route %q", r)
		}
	}

	// The real dependent lookup sessionhost_client.go's resolve() makes: with
	// a manifest registered and a bound launch identity, a caller reaching
	// for relay-sessions must no longer be refused as unavailable.
	client := &sessionHostClient{enhanced: enhanced, launches: launches}
	if _, _, err := client.resolve(); err != nil {
		t.Fatalf("sessionHostClient.resolve() after registration: %v", err)
	}

	// A real round trip through the registered socket and bearer, not just
	// their presence: handleTerminate answers 204 for an unknown session id,
	// so this only succeeds if InternalToken is the token the real
	// relay-sessions binary actually checks Authorization against, and this
	// test process (the bridge's owner, standing in for relay) passes
	// checkInternalPeer's RelayPID check. A manifest that registered a
	// non-matching token would fail here with a 403, even though every
	// assertion above it already passed.
	if err := client.Terminate(context.Background(), "no-such-session", "probe"); err != nil {
		t.Fatalf("Terminate through the registered socket+bearer: %v", err)
	}

	// The rest of this test drives the real eve-facing surface through a
	// real NewFrontendDispatcher over this same live registry — dispatcher
	// lookup, reverse proxy, bearer injection, the real (unmocked)
	// relay-sessions process, its real handler — the exact chain that was
	// silently 404ing before this fix (background section of the plan this
	// test covers). internal/sessions/hostapi/mount_test.go already proves
	// the mount itself works directly against the socket; this proves the
	// dispatcher in front of it forwards correctly and, just as important,
	// that /launch stays unreachable through that same dispatcher.
	dispatcher := NewFrontendDispatcher(enhanced)
	dispatcherSrv := httptest.NewServer(dispatcher)
	t.Cleanup(dispatcherSrv.Close)

	modelsResp, err := http.Get(dispatcherSrv.URL + "/api/models")
	assertNoErr(t, err, "GET /api/models through the dispatcher")
	defer modelsResp.Body.Close()
	if modelsResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(modelsResp.Body)
		t.Fatalf("GET /api/models through dispatcher = %d, want 200 (body=%s)", modelsResp.StatusCode, body)
	}
	var modelsBody map[string]any
	if err := json.NewDecoder(modelsResp.Body).Decode(&modelsBody); err != nil {
		t.Fatalf("decode /api/models body: %v", err)
	}
	if _, ok := modelsBody["models"]; !ok {
		t.Fatalf("/api/models body missing \"models\": %+v", modelsBody)
	}

	wsURL := "ws" + strings.TrimPrefix(dispatcherSrv.URL, "http") + "/ws"
	wsDialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	wsConn, wsResp, err := wsDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("WS dial through dispatcher: %v", err)
	}
	defer wsConn.Close()
	if wsResp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("WS handshake through dispatcher = %d, want 101", wsResp.StatusCode)
	}
	if err := wsConn.WriteJSON(map[string]any{"type": "terminal_list"}); err != nil {
		t.Fatalf("write terminal_list through dispatcher: %v", err)
	}
	_ = wsConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var wsGot map[string]any
	if err := wsConn.ReadJSON(&wsGot); err != nil {
		t.Fatalf("read terminal_list reply through dispatcher: %v", err)
	}
	if wsGot["type"] != "terminal_list" {
		t.Fatalf("reply type through dispatcher = %v, want terminal_list (%+v)", wsGot["type"], wsGot)
	}

	// /launch and /terminate are relay-sessions' peer-verified internal API,
	// never eve-facing: they are absent from relay-sessions' own manifest
	// (bridge.Manifest.Validate refuses them there outright), so
	// LookupByPath must never resolve either through this dispatcher — a
	// 404 from the dispatcher's own "no service registered for this path",
	// never a 403/405 from the host behind it. This is the entire safety
	// argument for exposing the rest of the internal socket's mux through
	// the manifest surface at all.
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		req, err := http.NewRequest(method, dispatcherSrv.URL+"/launch", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		assertNoErr(t, err, method+" /launch through the dispatcher")
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s /launch through dispatcher = %d, want 404 (never reachable through the eve-facing proxy path)", method, resp.StatusCode)
		}
	}
}
