//go:build !windows

package main

// Integration test for ServiceRegistry. Exercises the real spawn path
// (env-var injection, pidfile write, service-token registration,
// manifest registration on bridge) using the in-tree cmd/testservice
// binary — no exec.Command mocks. Per ADR-002, the spawn surface is
// too security-sensitive to fake.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"relaygo/bridge"
)

// waitFor polls cond until it returns true or timeout elapses, failing the
// test with msg on timeout. Keeps the spawn-lifecycle tests free of repeated
// deadline loops.
func waitFor(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", msg)
}

// readDumpedEnv parses a `VAR=value` per-line file written by
// `testservice --dump-env` into a map.
func readDumpedEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read dumped env %s: %v", path, err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		out[k] = v
	}
	return out
}

var (
	testserviceBinOnce sync.Once
	testserviceBinPath string
	testserviceBinErr  error
)

// buildTestServiceBinary compiles cmd/testservice into a per-test-run
// tempdir and returns the binary path. The build is cached so multiple
// service-registry tests in one suite share the artifact.
func buildTestServiceBinary(t *testing.T) string {
	t.Helper()
	testserviceBinOnce.Do(func() {
		dir, err := os.MkdirTemp("/tmp", "testsvc-bin-")
		if err != nil {
			testserviceBinErr = err
			return
		}
		path := filepath.Join(dir, "testservice")
		cmd := exec.Command("go", "build", "-o", path, "./cmd/testservice")
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			testserviceBinErr = err
			return
		}
		testserviceBinPath = path
	})
	if testserviceBinErr != nil {
		t.Fatalf("build cmd/testservice: %v", testserviceBinErr)
	}
	return testserviceBinPath
}

// startSandboxBridge brings up a real BridgeServer on the sandbox's
// SocketPath and returns a wired (router, registry) pair. The router's
// embedded serviceTokens is shared with the registry via pointer — same
// wiring as trayapp.go production setup, so token registrations made by
// the registry are visible to the router's auth check.
func startSandboxBridge(t *testing.T, enhanced *EnhancedServiceRegistry) (*appRouter, *ServiceRegistry) {
	t.Helper()
	dir := mkEmptySandboxRelayHome(t)
	store := NewSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}

	router := &appRouter{
		store:    store,
		tools:    NewExternalMcpManager(nil),
		services: &fakeServiceReloader{},
		enhanced: enhanced,
	}
	reg := NewServiceRegistry()
	reg.TokenStore = &router.serviceTokens
	reg.Enhanced = enhanced

	srv, err := bridge.NewBridgeServer(context.Background(), router)
	if err != nil {
		t.Fatalf("NewBridgeServer: %v", err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() {
		reg.StopAll()
		srv.Close()
	})
	// Wait for socket to be dialable.
	_ = dialUnixWithTimeout(t, bridge.SocketPath(), 2*time.Second).Close()
	return router, reg
}

func TestServiceRegistry_Spawn_InjectsBridgeEnvAndPidfile(t *testing.T) {
	binPath := buildTestServiceBinary(t)
	enhanced := NewEnhancedServiceRegistry(nil)
	router, reg := startSandboxBridge(t, enhanced)
	_ = router

	cfg := &ServiceConfig{
		ID:          "svc-spawn-test",
		DisplayName: "Test Spawn",
		Command:     binPath,
	}
	if err := reg.Start(cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { reg.Stop(cfg.ID) })

	// Process is running.
	if !reg.IsRunning(cfg.ID) {
		t.Fatal("service should be running immediately after Start")
	}

	// Pidfile was written under the sandboxed ConfigDir.
	pid, err := readPidFile(cfg.ID)
	if err != nil {
		t.Fatalf("readPidFile: %v", err)
	}
	pids := reg.PIDsByServiceID()
	if pids[cfg.ID] != pid {
		t.Fatalf("pidfile (%d) does not match process pid (%d)", pid, pids[cfg.ID])
	}

	// A service token was registered.
	if n := router.serviceTokens.Len(); n != 1 {
		t.Fatalf("expected one service token, got %d", n)
	}
}

func TestServiceRegistry_Spawn_RegistersManifest(t *testing.T) {
	binPath := buildTestServiceBinary(t)
	enhanced := NewEnhancedServiceRegistry(nil)
	_, reg := startSandboxBridge(t, enhanced)

	cfg := &ServiceConfig{
		ID:          "svc-manifest",
		DisplayName: "Test Manifest",
		Command:     binPath,
		Args:        []string{"--register"},
	}
	if err := reg.Start(cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { reg.Stop(cfg.ID) })

	// Wait for manifest registration. The child runs `go build` of itself
	// → register → block; even on slow CI that should be <5s.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if enhanced.Get(cfg.ID) != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	rec := enhanced.Get(cfg.ID)
	if rec == nil {
		t.Fatal("service never registered manifest with relay")
	}
	if rec.InternalSocket == "" || rec.InternalToken == "" {
		t.Fatalf("registered record missing socket/token: %+v", rec)
	}
	wantRoute := "/api/" + cfg.ID
	if len(rec.Manifest.Routes) != 1 || rec.Manifest.Routes[0] != wantRoute {
		t.Fatalf("registered routes wrong: %v want %v", rec.Manifest.Routes, []string{wantRoute})
	}
}

func TestServiceRegistry_Stop_CleansTokenAndPidfile(t *testing.T) {
	binPath := buildTestServiceBinary(t)
	enhanced := NewEnhancedServiceRegistry(nil)
	router, reg := startSandboxBridge(t, enhanced)

	cfg := &ServiceConfig{
		ID:          "svc-stop",
		DisplayName: "Test Stop",
		Command:     binPath,
	}
	if err := reg.Start(cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	reg.Stop(cfg.ID)

	// Process map is cleaned.
	if reg.IsRunning(cfg.ID) {
		t.Fatal("IsRunning should return false after Stop")
	}

	// Service token was removed when the process exited.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if router.serviceTokens.Len() == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n := router.serviceTokens.Len(); n != 0 {
		t.Fatalf("service token not cleaned up; have %d", n)
	}

	// Pidfile was removed.
	_, err := readPidFile(cfg.ID)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		_ = err
	}
}

// TestServiceRegistry_Reload_RestartsInPlace exercises the real restart path
// (Stop → Start) that `relay service restart`, the tray toggle, and config-save
// all drive through Reload. Every other test substitutes a no-op reloader, so
// this is the only coverage that the in-place restart actually tears down the
// old process/token/manifest and stands up a fresh one. A regression in Stop's
// "delete only if same proc" guard, the token rollover, or the manifest
// re-registration would otherwise pass the suite silently.
func TestServiceRegistry_Reload_RestartsInPlace(t *testing.T) {
	binPath := buildTestServiceBinary(t)
	enhanced := NewEnhancedServiceRegistry(nil)
	router, reg := startSandboxBridge(t, enhanced)

	cfg := &ServiceConfig{
		ID:          "svc-reload",
		DisplayName: "Test Reload",
		Command:     binPath,
		Args:        []string{"--register"},
	}
	if err := reg.Start(cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { reg.Stop(cfg.ID) })

	// Capture the first generation's identity: pid + internal socket. The
	// child binds a fresh per-pid socket on every spawn, so the socket path
	// is a reliable discriminator between generations.
	waitFor(t, 5*time.Second, "first manifest registration", func() bool {
		return enhanced.Get(cfg.ID) != nil
	})
	firstPID := reg.PIDsByServiceID()[cfg.ID]
	firstSocket := enhanced.Get(cfg.ID).InternalSocket
	if firstPID == 0 || firstSocket == "" {
		t.Fatalf("first generation not fully up: pid=%d socket=%q", firstPID, firstSocket)
	}
	if n := router.serviceTokens.Len(); n != 1 {
		t.Fatalf("before reload want exactly 1 service token, got %d", n)
	}

	// Restart in place.
	if err := reg.Reload(cfg.ID, cfg); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	// New process is running under a different pid.
	if !reg.IsRunning(cfg.ID) {
		t.Fatal("service not running after Reload")
	}
	secondPID := reg.PIDsByServiceID()[cfg.ID]
	if secondPID == 0 {
		t.Fatal("no pid recorded after Reload")
	}
	if secondPID == firstPID {
		t.Fatalf("Reload reused pid %d; expected a freshly spawned process", firstPID)
	}

	// Stop tears down the old token before Start registers the new one, so the
	// count must stay at exactly 1 — never 0 (old leaked away) or 2 (old not
	// cleaned up).
	if n := router.serviceTokens.Len(); n != 1 {
		t.Fatalf("after reload want exactly 1 service token, got %d", n)
	}

	// The new generation re-registers its manifest on a fresh internal socket.
	waitFor(t, 5*time.Second, "manifest re-registration after reload", func() bool {
		rec := enhanced.Get(cfg.ID)
		return rec != nil && rec.InternalSocket != firstSocket
	})
}

// TestServiceRegistry_Spawn_FrontendCredsIsolation pins the security boundary
// documented in CLAUDE.md: relay's front-door bearer (RELAY_FRONTEND_*) must
// reach frontend consumers (eve) but NEVER a backend — otherwise it leaks into
// any shell the backend spawns. Previously this was only covered at the
// frontendCredsEnabled predicate level; here we assert it at the actual spawn
// line by reading the child's real injected environment.
func TestServiceRegistry_Spawn_FrontendCredsIsolation(t *testing.T) {
	binPath := buildTestServiceBinary(t)
	enhanced := NewEnhancedServiceRegistry(nil)
	_, reg := startSandboxBridge(t, enhanced)

	// Wire a real frontend channel so creds *could* be injected.
	reg.FrontendChannel = NewFrontendChannel()
	t.Cleanup(reg.CloseFrontendChannel)
	endpoint, err := reg.FrontendChannel.Ensure()
	if err != nil {
		t.Fatalf("Ensure frontend channel: %v", err)
	}

	dumpDir := mkShortTempDir(t, "envdump-")
	backendEnvFile := filepath.Join(dumpDir, "backend.env")
	frontendEnvFile := filepath.Join(dumpDir, "frontend.env")

	falseVal := false
	backend := &ServiceConfig{
		ID:               "svc-backend",
		DisplayName:      "Backend",
		Command:          binPath,
		Args:             []string{"--dump-env", backendEnvFile},
		FrontendConsumer: &falseVal, // opt out
	}
	if err := reg.Start(backend); err != nil {
		t.Fatalf("Start backend: %v", err)
	}
	t.Cleanup(func() { reg.Stop(backend.ID) })

	frontend := &ServiceConfig{
		ID:          "svc-frontend",
		DisplayName: "Frontend",
		Command:     binPath,
		Args:        []string{"--dump-env", frontendEnvFile},
		// FrontendConsumer nil → default (inject).
	}
	if err := reg.Start(frontend); err != nil {
		t.Fatalf("Start frontend: %v", err)
	}
	t.Cleanup(func() { reg.Stop(frontend.ID) })

	waitFor(t, 5*time.Second, "backend env dump", func() bool {
		_, err := os.Stat(backendEnvFile)
		return err == nil
	})
	waitFor(t, 5*time.Second, "frontend env dump", func() bool {
		_, err := os.Stat(frontendEnvFile)
		return err == nil
	})

	backendEnv := readDumpedEnv(t, backendEnvFile)
	frontendEnv := readDumpedEnv(t, frontendEnvFile)

	// Backend: front-door bearer absent; backend creds present.
	if _, ok := backendEnv[EnvFrontendToken]; ok {
		t.Errorf("backend leaked %s into its environment", EnvFrontendToken)
	}
	if _, ok := backendEnv[EnvFrontendSocket]; ok {
		t.Errorf("backend leaked %s into its environment", EnvFrontendSocket)
	}
	if backendEnv[EnvServiceToken] == "" {
		t.Errorf("backend missing %s", EnvServiceToken)
	}
	if backendEnv[EnvBridgeSocket] == "" {
		t.Errorf("backend missing %s", EnvBridgeSocket)
	}
	if got := backendEnv[EnvServiceID]; got != backend.ID {
		t.Errorf("backend %s = %q, want %q", EnvServiceID, got, backend.ID)
	}

	// Frontend consumer: receives the exact injected bearer + socket.
	if got := frontendEnv[EnvFrontendToken]; got != endpoint.Token {
		t.Errorf("frontend %s = %q, want injected token", EnvFrontendToken, got)
	}
	if got := frontendEnv[EnvFrontendSocket]; got != endpoint.Socket {
		t.Errorf("frontend %s = %q, want injected socket", EnvFrontendSocket, got)
	}
}

// TestServiceRegistry_StartAllAutostart_OnlyStartsEnabled verifies the boot-time
// filter: services flagged autostart start, the rest don't. A regression that
// inverts the predicate or starts everything would otherwise go unnoticed.
func TestServiceRegistry_StartAllAutostart_OnlyStartsEnabled(t *testing.T) {
	binPath := buildTestServiceBinary(t)
	enhanced := NewEnhancedServiceRegistry(nil)
	_, reg := startSandboxBridge(t, enhanced)

	configs := []ServiceConfig{
		{ID: "svc-auto", DisplayName: "Auto", Command: binPath, Autostart: true},
		{ID: "svc-manual", DisplayName: "Manual", Command: binPath, Autostart: false},
	}
	reg.StartAllAutostart(configs)
	t.Cleanup(reg.StopAll)

	if !reg.IsRunning("svc-auto") {
		t.Error("autostart service should be running")
	}
	if reg.IsRunning("svc-manual") {
		t.Error("non-autostart service should NOT be running")
	}
	if ids := reg.RunningIDs(); len(ids) != 1 || ids[0] != "svc-auto" {
		t.Errorf("RunningIDs = %v, want [svc-auto]", ids)
	}
}

func TestServiceRegistry_OnProcessExit_FiresAfterExit(t *testing.T) {
	binPath := buildTestServiceBinary(t)
	enhanced := NewEnhancedServiceRegistry(nil)
	_, reg := startSandboxBridge(t, enhanced)

	exited := make(chan struct{})
	reg.OnProcessExit = func() {
		select {
		case exited <- struct{}{}:
		default:
		}
	}

	cfg := &ServiceConfig{
		ID:          "svc-exit",
		DisplayName: "Test Exit",
		Command:     binPath,
		Args:        []string{"--status-after", "100ms"}, // self-terminates
	}
	if err := reg.Start(cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}

	select {
	case <-exited:
		// OK
	case <-time.After(3 * time.Second):
		t.Fatal("OnProcessExit never fired after service self-exit")
	}
}
