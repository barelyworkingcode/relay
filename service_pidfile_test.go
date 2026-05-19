//go:build !windows

package main

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// withTempConfigDir reroutes bridge.ConfigDir() to a per-test directory by
// pointing HOME at t.TempDir(). os.UserConfigDir on darwin/linux derives the
// config dir from HOME, so the pidfile helpers land in the temp tree and
// don't pollute the user's real ~/Library/Application Support/relay.
func withTempConfigDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp+"/.config")
	return tmp
}

func TestPidFile_RoundTrip(t *testing.T) {
	withTempConfigDir(t)

	if err := writePidFile("svc-a", 4242); err != nil {
		t.Fatalf("writePidFile: %v", err)
	}
	pid, err := readPidFile("svc-a")
	if err != nil {
		t.Fatalf("readPidFile: %v", err)
	}
	if pid != 4242 {
		t.Fatalf("expected pid 4242, got %d", pid)
	}

	removePidFile("svc-a")
	pid, err = readPidFile("svc-a")
	if err != nil {
		t.Fatalf("readPidFile after remove: %v", err)
	}
	if pid != 0 {
		t.Fatalf("expected pid 0 after remove, got %d", pid)
	}
}

// spawnSleeper starts a `sleep` process in its own process group, mirroring
// how ServiceRegistry.Start spawns services. Returns the cmd (so callers can
// Wait() it after SIGTERM — in production launchd reaps orphans, but in
// tests we own the child and zombies linger until reaped) and a cleanup
// that hard-kills it if the test fails before the reclaim runs.
func spawnSleeper(t *testing.T) (*exec.Cmd, func()) {
	t.Helper()
	cmd := exec.Command("sleep", "120")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	cleanup := func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	}
	return cmd, cleanup
}

func TestReclaimOrphans_KillsMatchingOrphan(t *testing.T) {
	withTempConfigDir(t)

	cmd, cleanup := spawnSleeper(t)
	defer cleanup()
	pid := cmd.Process.Pid

	if err := writePidFile("sleeper", pid); err != nil {
		t.Fatalf("writePidFile: %v", err)
	}

	r := NewServiceRegistry()
	r.ReclaimOrphans([]ServiceConfig{{ID: "sleeper", Command: "sleep"}})

	// Reap the now-terminated child so kill(pid, 0) below sees ESRCH.
	// In production launchd does this automatically once the tray exits.
	state, err := cmd.Process.Wait()
	if err != nil {
		t.Fatalf("wait sleeper: %v", err)
	}
	if state.ExitCode() == 0 && !state.Exited() {
		t.Fatalf("sleep should have been signaled, not exited cleanly")
	}

	if syscall.Kill(pid, 0) == nil {
		t.Fatalf("expected pid %d to be reaped, still alive", pid)
	}
	if leftover, _ := readPidFile("sleeper"); leftover != 0 {
		t.Fatalf("expected pidfile removed after reclaim, got pid %d", leftover)
	}
}

func TestReclaimOrphans_SkipsPidRecyclingMismatch(t *testing.T) {
	withTempConfigDir(t)

	// Spawn a real sleeper; its command line will contain "sleep", not
	// "totally-different-binary". The mismatch should cause ReclaimOrphans
	// to leave the process alone — guarding against killing a recycled pid
	// that happens to match an old service's recorded pid.
	cmd, cleanup := spawnSleeper(t)
	defer cleanup()
	pid := cmd.Process.Pid

	if err := writePidFile("ghost", pid); err != nil {
		t.Fatalf("writePidFile: %v", err)
	}

	r := NewServiceRegistry()
	r.ReclaimOrphans([]ServiceConfig{{ID: "ghost", Command: "/opt/totally-different-binary"}})

	time.Sleep(200 * time.Millisecond)
	if syscall.Kill(pid, 0) != nil {
		t.Fatalf("expected pid %d to survive recycling-guard, but it's dead", pid)
	}
	// Pidfile is still removed (it's stale — we don't own that pid anymore).
	if leftover, _ := readPidFile("ghost"); leftover != 0 {
		t.Fatalf("expected pidfile removed after reclaim attempt, got pid %d", leftover)
	}
}

func TestReclaimOrphans_StalePidfile(t *testing.T) {
	withTempConfigDir(t)

	// Spawn and immediately reap so the pid is guaranteed gone before
	// reclaim runs.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	stalePid := cmd.ProcessState.Pid()

	if err := writePidFile("dead", stalePid); err != nil {
		t.Fatalf("writePidFile: %v", err)
	}

	r := NewServiceRegistry()
	r.ReclaimOrphans([]ServiceConfig{{ID: "dead", Command: "true"}})

	if leftover, _ := readPidFile("dead"); leftover != 0 {
		t.Fatalf("expected stale pidfile removed, got pid %d", leftover)
	}
}

// Quick sanity that readPidFile returns 0 for an absent file and is not
// fooled by garbage content.
func TestReadPidFile_EdgeCases(t *testing.T) {
	withTempConfigDir(t)

	if pid, err := readPidFile("missing"); err != nil || pid != 0 {
		t.Fatalf("expected (0, nil) for missing, got (%d, %v)", pid, err)
	}

	path, err := pidFilePath("garbage")
	if err != nil {
		t.Fatalf("pidFilePath: %v", err)
	}
	if err := os.WriteFile(path, []byte("not-a-number"), 0600); err != nil {
		t.Fatalf("write garbage pidfile: %v", err)
	}
	if pid, err := readPidFile("garbage"); err == nil {
		t.Fatalf("expected error parsing %q, got pid %d", "not-a-number", pid)
	}

	// And confirm strconv.Atoi accepts the well-formed case we rely on.
	if _, err := strconv.Atoi("4242"); err != nil {
		t.Fatalf("strconv sanity: %v", err)
	}
}
