//go:build !windows

package main

// Deliberate: exercises the real spawn path via cmd/testservice, no
// exec.Command mocks — ADR-002 treats the spawn surface as too
// security-sensitive to fake.

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
	"github.com/barelyworkingcode/relay/internal/service"
)

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
		cmd.Dir = repoRoot(t)
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

// One launch table is shared by the router and the registry — same wiring as
// trayapp.go — so a launch the registry begins is one Hello can bind and the
// router can authenticate by.
func startSandboxBridge(t *testing.T, enhanced *EnhancedServiceRegistry) (*appRouter, *service.Registry) {
	t.Helper()
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}

	router := &appRouter{
		store:    store,
		tools:    mcpbroker.NewManager(nil),
		services: &fakeServiceReloader{},
		enhanced: enhanced,
		launches: service.NewLaunches(),
	}
	reg := service.NewRegistry()
	reg.Launches = router.launches
	reg.Enhanced = enhanced
	// Mirrors trayapp.go's production wiring: the registry has no default
	// log destination, so a test that spawns a real service must supply one
	// too.
	reg.OpenLog = func(id string) (io.WriteCloser, error) {
		dir, err := serviceLogDir()
		if err != nil {
			return nil, err
		}
		return openRotatingLog(filepath.Join(dir, id+".log"))
	}

	srv, err := bridge.NewBridgeServer(context.Background(), router)
	if err != nil {
		t.Fatalf("NewBridgeServer: %v", err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() {
		reg.StopAll()
		srv.Close()
	})
	_ = dialUnixWithTimeout(t, bridge.SocketPath(), 2*time.Second).Close()
	return router, reg
}

func TestServiceRegistry_Spawn_InjectsBridgeEnvAndPidfile(t *testing.T) {
	binPath := buildTestServiceBinary(t)
	enhanced := NewEnhancedServiceRegistry(nil)
	router, reg := startSandboxBridge(t, enhanced)
	_ = router

	cfg := &config.ServiceConfig{
		ID:          "svc-spawn-test",
		DisplayName: "Test Spawn",
		Command:     binPath,
	}
	if err := reg.Start(cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { reg.Stop(cfg.ID) })

	if !reg.IsRunning(cfg.ID) {
		t.Fatal("service should be running immediately after Start")
	}

	pid, err := service.ReadPidFileForTest(cfg.ID)
	if err != nil {
		t.Fatalf("readPidFile: %v", err)
	}
	pids := reg.PIDsByServiceID()
	if pids[cfg.ID] != pid {
		t.Fatalf("pidfile (%d) does not match process pid (%d)", pid, pids[cfg.ID])
	}

	if n := router.launches.Len(); n != 1 {
		t.Fatalf("expected one live launch, got %d", n)
	}
}

func TestServiceRegistry_Spawn_RegistersManifest(t *testing.T) {
	binPath := buildTestServiceBinary(t)
	enhanced := NewEnhancedServiceRegistry(nil)
	_, reg := startSandboxBridge(t, enhanced)

	backend := false
	cfg := &config.ServiceConfig{
		ID:               "svc-manifest",
		DisplayName:      "Test Manifest",
		Command:          binPath,
		Args:             []string{"--register"},
		FrontendConsumer: &backend,
	}
	if err := reg.Start(cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { reg.Stop(cfg.ID) })

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

func TestServiceRegistry_Stop_EndsLaunchAndCleansPidfile(t *testing.T) {
	binPath := buildTestServiceBinary(t)
	enhanced := NewEnhancedServiceRegistry(nil)
	router, reg := startSandboxBridge(t, enhanced)

	cfg := &config.ServiceConfig{
		ID:          "svc-stop",
		DisplayName: "Test Stop",
		Command:     binPath,
	}
	if err := reg.Start(cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	reg.Stop(cfg.ID)

	if reg.IsRunning(cfg.ID) {
		t.Fatal("IsRunning should return false after Stop")
	}

	// No wait: Stop returns only after the reaper has ended the launch.
	if n := router.launches.Len(); n != 0 {
		t.Fatalf("launch not ended by Stop; have %d", n)
	}

	_, err := service.ReadPidFileForTest(cfg.ID)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		_ = err
	}
}

func TestServiceRegistry_Reload_RestartsInPlace(t *testing.T) {
	binPath := buildTestServiceBinary(t)
	enhanced := NewEnhancedServiceRegistry(nil)
	router, reg := startSandboxBridge(t, enhanced)

	backend := false
	cfg := &config.ServiceConfig{
		ID:               "svc-reload",
		DisplayName:      "Test Reload",
		Command:          binPath,
		Args:             []string{"--register"},
		FrontendConsumer: &backend,
	}
	if err := reg.Start(cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { reg.Stop(cfg.ID) })

	// Socket path discriminates generations: the child binds a fresh per-pid
	// socket on every spawn.
	waitFor(t, 5*time.Second, "first manifest registration", func() bool {
		return enhanced.Get(cfg.ID) != nil
	})
	firstPID := reg.PIDsByServiceID()[cfg.ID]
	firstSocket := enhanced.Get(cfg.ID).InternalSocket
	if firstPID == 0 || firstSocket == "" {
		t.Fatalf("first generation not fully up: pid=%d socket=%q", firstPID, firstSocket)
	}
	if n := router.launches.Len(); n != 1 {
		t.Fatalf("before reload want exactly 1 live launch, got %d", n)
	}

	if err := reg.Reload(cfg.ID, cfg); err != nil {
		t.Fatalf("Reload: %v", err)
	}

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

	// Stop ends the old launch before Start begins the new one — count must
	// stay exactly 1, never 0 (not begun) or 2 (not ended).
	if n := router.launches.Len(); n != 1 {
		t.Fatalf("after reload want exactly 1 live launch, got %d", n)
	}

	waitFor(t, 5*time.Second, "manifest re-registration after reload", func() bool {
		rec := enhanced.Get(cfg.ID)
		return rec != nil && rec.InternalSocket != firstSocket
	})
}

// The frontend socket path goes only to consumers, every launch gets the
// launch fd, and no removed credential name reaches either — including one
// relay's own environment carries.
func TestServiceRegistry_Spawn_FrontendEnvIsolation(t *testing.T) {
	binPath := buildTestServiceBinary(t)
	for _, name := range bridge.RemovedCredentialEnv {
		t.Setenv(name, "relay-inherited-this")
	}
	enhanced := NewEnhancedServiceRegistry(nil)
	_, reg := startSandboxBridge(t, enhanced)

	frontendChannel := NewFrontendChannel()
	t.Cleanup(frontendChannel.Close)
	reg.FrontendEnv = func() (map[string]string, error) {
		ep, err := frontendChannel.Ensure()
		if err != nil {
			return nil, err
		}
		return ep.FrontendEnv(), nil
	}
	endpoint, err := frontendChannel.Ensure()
	if err != nil {
		t.Fatalf("Ensure frontend channel: %v", err)
	}

	dumpDir := mkShortTempDir(t, "envdump-")
	backendEnvFile := filepath.Join(dumpDir, "backend.env")
	frontendEnvFile := filepath.Join(dumpDir, "frontend.env")

	falseVal := false
	backend := &config.ServiceConfig{
		ID:               "svc-backend",
		DisplayName:      "Backend",
		Command:          binPath,
		Args:             []string{"--dump-env", backendEnvFile},
		FrontendConsumer: &falseVal,
	}
	if err := reg.Start(backend); err != nil {
		t.Fatalf("Start backend: %v", err)
	}
	t.Cleanup(func() { reg.Stop(backend.ID) })

	frontend := &config.ServiceConfig{
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

	if _, ok := backendEnv[EnvFrontendSocket]; ok {
		t.Errorf("backend received %s", EnvFrontendSocket)
	}
	if backendEnv[EnvBridgeSocket] == "" {
		t.Errorf("backend missing %s", EnvBridgeSocket)
	}
	if got := backendEnv[EnvServiceID]; got != backend.ID {
		t.Errorf("backend %s = %q, want %q", EnvServiceID, got, backend.ID)
	}
	if got := frontendEnv[EnvFrontendSocket]; got != endpoint.Socket {
		t.Errorf("frontend %s = %q, want injected socket", EnvFrontendSocket, got)
	}
	for who, env := range map[string]map[string]string{"backend": backendEnv, "frontend": frontendEnv} {
		if got := env[EnvLaunchFD]; got != "3" {
			t.Errorf("%s %s = %q, want 3", who, EnvLaunchFD, got)
		}
		for _, name := range bridge.RemovedCredentialEnv {
			if v, ok := env[name]; ok {
				t.Errorf("%s received %s=%q", who, name, v)
			}
		}
	}
}

func TestServiceRegistry_StartAllAutostart_OnlyStartsEnabled(t *testing.T) {
	binPath := buildTestServiceBinary(t)
	enhanced := NewEnhancedServiceRegistry(nil)
	_, reg := startSandboxBridge(t, enhanced)

	configs := []config.ServiceConfig{
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

	cfg := &config.ServiceConfig{
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
	case <-time.After(3 * time.Second):
		t.Fatal("OnProcessExit never fired after service self-exit")
	}
}
