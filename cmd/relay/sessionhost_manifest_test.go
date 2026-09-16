package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

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
	wantRoutes := map[string]bool{"/api/terminals/": true, "/api/sessions/": true}
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
}
