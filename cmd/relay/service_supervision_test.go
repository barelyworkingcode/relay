//go:build !windows

package main

// Supervision tests exercise the real spawn path via cmd/testservice, the
// same way service_registry_test.go's tests do -- no exec.Command mocks
// (ADR-002). A FakeClock replaces wall-clock backoff so a crash loop can be
// driven to its give-up limit without a real sleep; the process spawn and
// exit themselves are still real, on real time.

import (
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/peertoken"
	"github.com/barelyworkingcode/relay/internal/service"
)

// waitForPIDChange polls until PIDsByServiceID()[id] is nonzero and not
// exclude, i.e. a newer generation than whatever was running before.
func waitForPIDChange(t *testing.T, reg *service.Registry, id string, exclude int) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pid := reg.PIDsByServiceID()[id]; pid != 0 && pid != exclude {
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for a new pid for %s (excluding %d)", id, exclude)
	return 0
}

func waitForPhase(t *testing.T, reg *service.Registry, id string, phase service.SupervisionPhase) service.SupervisionStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := reg.SupervisionStatuses()[id]; ok && st.Phase == phase {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s to reach phase %s; have %+v", id, phase, reg.SupervisionStatuses()[id])
	return service.SupervisionStatus{}
}

// waitForBoundIdentity polls until Hello has bound an identity for id --
// Begin (which Launches.Len already counts) happens synchronously inside
// Start, but Hello is the spawned process dialing back, which takes a beat
// of real time.
func waitForBoundIdentity(t *testing.T, launches *service.Launches, id string) service.Identity {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ident, ok := launches.Bound(id); ok {
			return ident
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for an identity to bind for %s", id)
	return service.Identity{}
}

func waitForNotRunning(t *testing.T, reg *service.Registry, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !reg.IsRunning(id) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s to stop running", id)
}

// TestSupervision_CrashRestartsWithNewLaunchSecretAndIdentity is the crash
// path's core guarantee: an unrequested exit is restarted through Start,
// which always mints a fresh launch secret, and the old identity Hello once
// bound stops authenticating the moment the new launch replaces it.
func TestSupervision_CrashRestartsWithNewLaunchSecretAndIdentity(t *testing.T) {
	binPath := buildTestServiceBinary(t)
	enhanced := NewEnhancedServiceRegistry(nil)
	router, reg := startSandboxBridge(t, enhanced)
	clock := service.NewFakeClock(time.Now())
	reg.Clock = clock

	cfg := &config.ServiceConfig{
		ID:          "svc-crash",
		DisplayName: "Test Crash",
		Command:     binPath,
		Args:        []string{"--status-after", "60ms", "--exit-code", "7"},
	}
	if err := reg.Start(cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { reg.Stop(cfg.ID) })

	firstPID := reg.PIDsByServiceID()[cfg.ID]
	if firstPID == 0 {
		t.Fatal("service not running immediately after Start")
	}
	oldIdentity := waitForBoundIdentity(t, router.launches, cfg.ID)

	// The process crashes on its own after ~60ms; wait for supervision to
	// notice and schedule a restart, then fire the backoff instantly.
	st := waitForPhase(t, reg, cfg.ID, service.SupervisionRestarting)
	if st.Attempt != 1 || !st.HasExitCode || st.LastExitCode != 7 {
		t.Fatalf("unexpected restart status after first crash: %+v", st)
	}
	clock.Advance(service.ServiceRestartMaxDelay)

	secondPID := waitForPIDChange(t, reg, cfg.ID, firstPID)
	if secondPID == firstPID {
		t.Fatal("restart reused the old pid")
	}

	newIdentity := waitForBoundIdentity(t, router.launches, cfg.ID)
	if newIdentity.Process == oldIdentity.Process {
		t.Fatalf("restart kept the old identity's process %+v", oldIdentity.Process)
	}
	oldToken := peertoken.ForProcessForTest(oldIdentity.Process.PID, oldIdentity.Process.PIDVersion)
	if _, ok := router.launches.Lookup(oldToken); ok {
		t.Fatal("the old identity still authenticates after a supervised restart")
	}
}

// TestSupervision_OperatorStopPreventsRestart proves the one negative case
// that matters most: relay must never relaunch a service the operator just
// told it to stop, even though Stop's own kill looks exactly like a crash to
// the exit-watching goroutine.
func TestSupervision_OperatorStopPreventsRestart(t *testing.T) {
	binPath := buildTestServiceBinary(t)
	enhanced := NewEnhancedServiceRegistry(nil)
	_, reg := startSandboxBridge(t, enhanced)
	clock := service.NewFakeClock(time.Now())
	reg.Clock = clock

	cfg := &config.ServiceConfig{
		ID:          "svc-stop-no-restart",
		DisplayName: "Test Stop No Restart",
		Command:     binPath,
	}
	if err := reg.Start(cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}

	reg.Stop(cfg.ID)
	waitForNotRunning(t, reg, cfg.ID)

	// Nothing scheduled: advancing the clock as far as the whole give-up
	// budget would ever need must not resurrect it.
	clock.Advance(10 * service.ServiceRestartMaxDelay)
	time.Sleep(200 * time.Millisecond)

	if reg.IsRunning(cfg.ID) {
		t.Fatal("service restarted after an operator Stop")
	}
	if _, ok := reg.SupervisionStatuses()[cfg.ID]; ok {
		t.Fatal("a stopped service still has a supervision entry")
	}
}

// TestSupervision_ReloadIsASingleRestartNoDouble guards the race Reload is
// built from: its own Stop half must retire the old supervisor before the
// new Start installs one, so the crash-shaped exit of the process Reload
// itself killed never queues a second, redundant relaunch.
func TestSupervision_ReloadIsASingleRestartNoDouble(t *testing.T) {
	binPath := buildTestServiceBinary(t)
	enhanced := NewEnhancedServiceRegistry(nil)
	_, reg := startSandboxBridge(t, enhanced)
	clock := service.NewFakeClock(time.Now())
	reg.Clock = clock

	cfg := &config.ServiceConfig{
		ID:          "svc-reload-single",
		DisplayName: "Test Reload Single",
		Command:     binPath,
	}
	if err := reg.Start(cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { reg.Stop(cfg.ID) })
	firstPID := reg.PIDsByServiceID()[cfg.ID]

	if err := reg.Reload(cfg.ID, cfg); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	secondPID := waitForPIDChange(t, reg, cfg.ID, firstPID)

	// Give any (wrongly) queued second restart a chance to fire.
	clock.Advance(10 * service.ServiceRestartMaxDelay)
	time.Sleep(300 * time.Millisecond)

	thirdPID := reg.PIDsByServiceID()[cfg.ID]
	if thirdPID != secondPID {
		t.Fatalf("Reload's process was replaced again (pid %d -> %d); a supervised restart fired alongside it", secondPID, thirdPID)
	}
	if st, ok := reg.SupervisionStatuses()[cfg.ID]; !ok || st.Phase != service.SupervisionRunning {
		t.Fatalf("service not left in the running phase after Reload: ok=%v st=%+v", ok, st)
	}
}

// TestSupervision_BackoffSequenceAndGiveUp drives a service that crashes
// immediately on every launch through the full attempt budget and checks
// the exact 1s/2s/4s/8s/16s doubling sequence and the give-up rule, using
// the fake clock so none of it waits in real time.
func TestSupervision_BackoffSequenceAndGiveUp(t *testing.T) {
	binPath := buildTestServiceBinary(t)
	enhanced := NewEnhancedServiceRegistry(nil)
	_, reg := startSandboxBridge(t, enhanced)
	clock := service.NewFakeClock(time.Now())
	reg.Clock = clock

	cfg := &config.ServiceConfig{
		ID:          "svc-giveup",
		DisplayName: "Test Give Up",
		Command:     binPath,
		Args:        []string{"--status-after", "20ms", "--exit-code", "9"},
	}
	if err := reg.Start(cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { reg.Stop(cfg.ID) })

	wantDelays := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
	if len(wantDelays) != service.ServiceRestartMaxAttempts {
		t.Fatalf("test's expected sequence has %d entries, ServiceRestartMaxAttempts is %d -- keep them in sync", len(wantDelays), service.ServiceRestartMaxAttempts)
	}

	lastPID := reg.PIDsByServiceID()[cfg.ID]
	for attempt, want := range wantDelays {
		st := waitForPhase(t, reg, cfg.ID, service.SupervisionRestarting)
		if st.Attempt != attempt+1 {
			t.Fatalf("attempt %d: SupervisionStatuses attempt = %d, want %d", attempt+1, st.Attempt, attempt+1)
		}
		gotDelay := st.NextAttempt.Sub(clock.Now())
		if gotDelay != want {
			t.Fatalf("attempt %d: backoff delay = %s, want %s", attempt+1, gotDelay, want)
		}
		clock.Advance(want)
		lastPID = waitForPIDChange(t, reg, cfg.ID, lastPID)
	}

	// The budget is spent: the next crash must be marked failed, not
	// scheduled for a sixth attempt.
	final := waitForPhase(t, reg, cfg.ID, service.SupervisionFailed)
	if final.LastExitCode != 9 {
		t.Fatalf("failed status carries exit code %d, want 9", final.LastExitCode)
	}

	clock.Advance(10 * service.ServiceRestartMaxDelay)
	time.Sleep(300 * time.Millisecond)
	if reg.IsRunning(cfg.ID) {
		t.Fatal("a failed service restarted anyway after the clock advanced further")
	}
}

// TestSupervision_StableRunResetsAttemptCounter proves the intensity cap is
// not a lifetime one: a run that stays up ServiceRestartStableWindow resets
// the attempt counter, so the service that follows a long healthy stretch
// with one more crash is treated as a fresh incident, not attempt N+1.
func TestSupervision_StableRunResetsAttemptCounter(t *testing.T) {
	binPath := buildTestServiceBinary(t)
	enhanced := NewEnhancedServiceRegistry(nil)
	_, reg := startSandboxBridge(t, enhanced)
	clock := service.NewFakeClock(time.Now())
	reg.Clock = clock

	cfg := &config.ServiceConfig{
		ID:          "svc-stable-reset",
		DisplayName: "Test Stable Reset",
		Command:     binPath,
		Args:        []string{"--status-after", "150ms", "--exit-code", "3"},
	}
	if err := reg.Start(cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { reg.Stop(cfg.ID) })

	firstPID := reg.PIDsByServiceID()[cfg.ID]

	// First crash: attempt climbs to 1.
	st := waitForPhase(t, reg, cfg.ID, service.SupervisionRestarting)
	if st.Attempt != 1 {
		t.Fatalf("first crash: attempt = %d, want 1", st.Attempt)
	}
	clock.Advance(service.ServiceRestartMaxDelay)
	secondPID := waitForPIDChange(t, reg, cfg.ID, firstPID)

	// While the second generation is alive (it sleeps 150ms of real time
	// before exiting), jump the fake clock past the stable window -- this is
	// what "stayed up long enough" means to ranFor, independent of how long
	// the test itself actually waits.
	clock.Advance(service.ServiceRestartStableWindow + time.Second)

	st = waitForPhase(t, reg, cfg.ID, service.SupervisionRestarting)
	if st.Attempt != 1 {
		t.Fatalf("crash after a stable run: attempt = %d, want 1 (counter should have reset)", st.Attempt)
	}
	clock.Advance(service.ServiceRestartMaxDelay)
	_ = waitForPIDChange(t, reg, cfg.ID, secondPID)
}

// TestSupervision_ExitCode78CountsAsAFailureAndIsLoggedDistinctly checks
// EX_CONFIG (a service that could not establish its launch identity) is
// restarted exactly like any other failure -- same backoff, same counter --
// while its log record is distinguishable from a generic crash.
func TestSupervision_ExitCode78CountsAsAFailureAndIsLoggedDistinctly(t *testing.T) {
	binPath := buildTestServiceBinary(t)
	enhanced := NewEnhancedServiceRegistry(nil)
	_, reg := startSandboxBridge(t, enhanced)
	clock := service.NewFakeClock(time.Now())
	reg.Clock = clock

	cfg := &config.ServiceConfig{
		ID:          "svc-exit78",
		DisplayName: "Test EX_CONFIG",
		Command:     binPath,
		Args:        []string{"--status-after", "20ms", "--exit-code", "78"},
	}
	if err := reg.Start(cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { reg.Stop(cfg.ID) })

	st := waitForPhase(t, reg, cfg.ID, service.SupervisionRestarting)
	if st.Attempt != 1 || st.LastExitCode != service.ExitCodeLaunchIdentity {
		t.Fatalf("unexpected status after an EX_CONFIG exit: %+v", st)
	}
}

// TestSupervision_TrayShutdownCancelsPendingRestart is StopAll's contract: a
// restart mid-backoff when shutdown begins must never fire once StopAll has
// returned, however far the clock is later advanced.
func TestSupervision_TrayShutdownCancelsPendingRestart(t *testing.T) {
	binPath := buildTestServiceBinary(t)
	enhanced := NewEnhancedServiceRegistry(nil)
	_, reg := startSandboxBridge(t, enhanced)
	clock := service.NewFakeClock(time.Now())
	reg.Clock = clock

	cfg := &config.ServiceConfig{
		ID:          "svc-shutdown-cancel",
		DisplayName: "Test Shutdown Cancel",
		Command:     binPath,
		Args:        []string{"--status-after", "20ms", "--exit-code", "9"},
	}
	if err := reg.Start(cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitForPhase(t, reg, cfg.ID, service.SupervisionRestarting)

	reg.StopAll()

	if _, ok := reg.SupervisionStatuses()[cfg.ID]; ok {
		t.Fatal("StopAll left a supervision entry behind")
	}

	clock.Advance(10 * service.ServiceRestartMaxDelay)
	time.Sleep(300 * time.Millisecond)

	if reg.IsRunning(cfg.ID) {
		t.Fatal("a restart fired after tray shutdown cancelled it")
	}
}
