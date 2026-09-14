//go:build darwin

package membership

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var (
	testMembershipBinOnce sync.Once
	testMembershipBinPath string
	testMembershipBinErr  error
)

// buildTestMembershipBinary builds cmd/testmembership on demand, following
// cmd/relay's buildTestServiceBinary pattern: one build shared by every test
// in the process, into a throwaway /tmp dir rather than the module tree.
func buildTestMembershipBinary(t *testing.T) string {
	t.Helper()
	testMembershipBinOnce.Do(func() {
		dir, err := os.MkdirTemp("/tmp", "testmembership-bin-")
		if err != nil {
			testMembershipBinErr = err
			return
		}
		path := filepath.Join(dir, "testmembership")
		cmd := exec.Command("go", "build", "-o", path, "./cmd/testmembership")
		cmd.Dir = repoRoot(t)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			testMembershipBinErr = err
			return
		}
		testMembershipBinPath = path
	})
	if testMembershipBinErr != nil {
		t.Fatalf("build cmd/testmembership: %v", testMembershipBinErr)
	}
	return testMembershipBinPath
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed — cannot locate darwin_integration_test.go")
	}
	for dir := filepath.Dir(file); ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find module root containing go.mod")
		}
	}
}

// selfAsRoot registers the test process's own pid and real start time as a
// fake session root, using the same darwin Source production code uses.
// Every process this test spawns is a real child (or further descendant) of
// the test binary itself, so the test binary's own ancestry entry is the
// one true root of every chain below.
func selfAsRoot(t *testing.T, sessionID string) (Root, Source) {
	t.Helper()
	src := NewSource()
	self, ok := src.Info(os.Getpid())
	if !ok {
		t.Fatal("Info(self) failed — proc_pidinfo unavailable")
	}
	return Root{SessionID: sessionID, PID: self.PID, StartSec: self.StartSec, StartUsec: self.StartUsec}, src
}

// waitForFile polls until path exists and is non-empty, or fails the test.
func waitForFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil && len(b) > 0 {
			return string(b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
	return ""
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	s := waitForFile(t, path)
	pid, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		t.Fatalf("parse pidfile %s: %v", path, err)
	}
	return pid
}

func killAndWait(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	_ = cmd.Wait()
}

func TestDarwin_ChildAndGrandchildAreMembers(t *testing.T) {
	bin := buildTestMembershipBinary(t)
	dir := t.TempDir()
	childPidfile := filepath.Join(dir, "child.pid")
	grandchildPidfile := filepath.Join(dir, "grandchild.pid")

	root, src := selfAsRoot(t, "sess-real")
	roots := newFakeRoots(999999999).add(root)

	cmd := exec.Command(bin, "-mode=chain", "-pidfiles="+childPidfile+","+grandchildPidfile)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start chain: %v", err)
	}
	t.Cleanup(func() { killAndWait(t, cmd) })

	childPID := readPID(t, childPidfile)
	grandchildPID := readPID(t, grandchildPidfile)

	acceptedAt := time.Now().Add(time.Second)

	if id, ok := Resolve(src, roots, childPID, acceptedAt); !ok || id != "sess-real" {
		t.Fatalf("child Resolve() = %q, %v; want sess-real, true", id, ok)
	}
	if id, ok := Resolve(src, roots, grandchildPID, acceptedAt); !ok || id != "sess-real" {
		t.Fatalf("grandchild Resolve() = %q, %v; want sess-real, true", id, ok)
	}
}

func TestDarwin_DoubleForkIsNotAMember(t *testing.T) {
	bin := buildTestMembershipBinary(t)
	dir := t.TempDir()
	detachedPidfile := filepath.Join(dir, "detached.pid")

	root, src := selfAsRoot(t, "sess-real")
	roots := newFakeRoots(999999999).add(root)

	forker := exec.Command(bin, "-mode=doublefork", "-pidfile="+detachedPidfile)
	if err := forker.Start(); err != nil {
		t.Fatalf("start doublefork: %v", err)
	}
	// The forker exits on its own almost immediately, after spawning the
	// detached target and calling setsid; waiting for it is what lets the
	// target's reparent-to-launchd actually happen before we check it.
	if err := forker.Wait(); err != nil {
		t.Fatalf("forker exited with error: %v", err)
	}

	detachedPID := readPID(t, detachedPidfile)
	t.Cleanup(func() { _ = syscall.Kill(detachedPID, syscall.SIGTERM) })

	info, ok := src.Info(detachedPID)
	if !ok {
		t.Fatal("Info(detached) failed")
	}
	deadline := time.Now().Add(5 * time.Second)
	for info.PPID != 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		info, ok = src.Info(detachedPID)
		if !ok {
			t.Fatal("Info(detached) failed while waiting for reparent")
		}
	}
	if info.PPID != 1 {
		t.Fatalf("detached process ppid = %d after waiting; want 1 (reparented to launchd)", info.PPID)
	}

	acceptedAt := time.Now().Add(time.Second)
	if id, ok := Resolve(src, roots, detachedPID, acceptedAt); ok {
		t.Fatalf("Resolve(detached) = %q, true; want not-a-member", id)
	}
}

func TestDarwin_WatchExitFires(t *testing.T) {
	bin := buildTestMembershipBinary(t)
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "watched.pid")

	cmd := exec.Command(bin, "-mode=chain", "-pidfiles="+pidfile)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = cmd.Wait() }()

	pid := readPID(t, pidfile)
	src := NewSource()
	want, ok := src.Info(pid)
	if !ok {
		t.Fatal("Info(watched) failed")
	}

	fired := make(chan struct{})
	cancel, err := WatchExit(pid, want, func() { close(fired) })
	if err != nil {
		t.Fatalf("WatchExit: %v", err)
	}
	defer cancel()

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal watched process: %v", err)
	}

	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("onExit did not fire within 5s")
	}
}

func TestDarwin_WatchExitRefusesStartMismatch(t *testing.T) {
	bin := buildTestMembershipBinary(t)
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "watched.pid")

	cmd := exec.Command(bin, "-mode=chain", "-pidfiles="+pidfile)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { killAndWait(t, cmd) })

	pid := readPID(t, pidfile)
	src := NewSource()
	real, ok := src.Info(pid)
	if !ok {
		t.Fatal("Info(watched) failed")
	}
	// A caller holding stale info about this pid — e.g. from before it was
	// recycled — must be refused rather than silently watching whatever now
	// occupies the pid.
	stale := real
	stale.StartSec = real.StartSec + 1

	cancel, err := WatchExit(pid, stale, func() {})
	if err == nil {
		if cancel != nil {
			cancel()
		}
		t.Fatal("WatchExit succeeded with a mismatched start time; want an error")
	}
}
