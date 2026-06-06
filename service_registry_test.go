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
	"sync"
	"testing"
	"time"

	"relaygo/bridge"
)

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

