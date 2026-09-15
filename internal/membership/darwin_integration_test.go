//go:build darwin

package membership

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// killAndWait only signals the direct child (cmd.Process); the
	// grandchild is a separate process the child spawned and is never
	// waited on by anyone once the child dies, so it must be signalled here
	// too or it leaks past the test.
	t.Cleanup(func() { _ = syscall.Kill(grandchildPID, syscall.SIGTERM) })

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
	if !errors.Is(err, ErrExited) {
		t.Fatalf("WatchExit(mismatched start): err = %v; want errors.Is(err, ErrExited)", err)
	}
}

func TestDarwin_WatchExitAlreadyExited(t *testing.T) {
	bin := buildTestMembershipBinary(t)
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "watched.pid")

	cmd := exec.Command(bin, "-mode=chain", "-pidfiles="+pidfile)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := readPID(t, pidfile)
	src := NewSource()
	want, ok := src.Info(pid)
	if !ok {
		t.Fatal("Info(watched) failed")
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		if _, isExit := err.(*exec.ExitError); !isExit {
			t.Fatalf("wait: %v", err)
		}
	}

	cancel, err := WatchExit(pid, want, func() {})
	if cancel != nil {
		cancel()
	}
	// Either the kqueue registration itself refuses a dead pid (ESRCH), or
	// (if the kernel already recycled the number) the post-registration
	// recheck catches the mismatch — both map to ErrExited.
	if !errors.Is(err, ErrExited) {
		t.Fatalf("WatchExit on an already-exited pid: err = %v; want errors.Is(err, ErrExited)", err)
	}
}

func TestDarwin_WatchExitCancelThenExitNeverFires(t *testing.T) {
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

	var fired atomic.Bool
	cancel, err := WatchExit(pid, want, func() { fired.Store(true) })
	if err != nil {
		t.Fatalf("WatchExit: %v", err)
	}
	cancel() // must not return until the watch has fully stopped

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal watched process: %v", err)
	}
	_ = cmd.Wait()

	// Give a buggy implementation a window to fire spuriously; cancel()
	// having already returned is the actual guarantee under test.
	time.Sleep(200 * time.Millisecond)
	if fired.Load() {
		t.Fatal("onExit fired after cancel(); want it to never fire")
	}
}

// TestDarwin_WatchExitConcurrentCancelChurn hammers WatchExit/cancel across
// many independent kqueues concurrently. It targets the class of bug where
// cancel() closes its kq while its watcher goroutine might still call
// kevent() on it (e.g. after an EINTR retry): the freed fd number can be
// handed to a different, concurrently-opened kqueue, letting a cancelled
// watch steal another watch's exit event (firing the wrong onExit, or
// silencing the real one). -race won't see the fd-reuse mistake itself, but
// it will catch any data race in the fix, and running it with -count>1
// gives the fd-churn timing many chances to line up badly if the fix
// regresses.
func TestDarwin_WatchExitConcurrentCancelChurn(t *testing.T) {
	bin := buildTestMembershipBinary(t)

	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			dir, err := os.MkdirTemp("", fmt.Sprintf("watch-churn-%d-", i))
			if err != nil {
				t.Errorf("mkdtemp %d: %v", i, err)
				return
			}
			defer func() { _ = os.RemoveAll(dir) }()
			pidfile := filepath.Join(dir, "watched.pid")

			cmd := exec.Command(bin, "-mode=chain", "-pidfiles="+pidfile)
			if err := cmd.Start(); err != nil {
				t.Errorf("start %d: %v", i, err)
				return
			}
			defer func() { _ = cmd.Wait() }()

			pid := readPID(t, pidfile)
			want, ok := NewSource().Info(pid)
			if !ok {
				t.Errorf("Info(%d) failed", i)
				_ = cmd.Process.Kill()
				return
			}

			fired := make(chan struct{})
			cancel, err := WatchExit(pid, want, func() { close(fired) })
			if err != nil {
				t.Errorf("WatchExit %d: %v", i, err)
				_ = cmd.Process.Kill()
				return
			}

			if i%2 == 0 {
				// Cancel immediately: this is the fd-churn case — kq opens
				// and closes as fast as possible while every other
				// goroutine in this test is doing the same.
				cancel()
				_ = cmd.Process.Signal(syscall.SIGTERM)
			} else {
				// Let it fire for real, racing every other goroutine's
				// cancel() calls for the same kind of kq churn.
				_ = cmd.Process.Signal(syscall.SIGTERM)
				select {
				case <-fired:
				case <-time.After(5 * time.Second):
					t.Errorf("watch %d: onExit did not fire", i)
				}
				cancel()
			}
		}(i)
	}
	wg.Wait()
}

// TestDarwin_CancelAfterFireDoesNotStealAnotherWatch reproduces a security
// review finding against this package: watch A fires and closes its kq,
// watch B opens immediately afterward (on this platform, very reliably
// reusing A's just-freed fd number), and a stale cancelA() call — made
// after A already fired — must not touch B's kqueue even though the fd
// number was handed straight back out. Before the kqMu fix, that trigger
// reached B's kqueue instead (every watch shares the same EVFILT_USER
// wakeIdent), and B silently stopped without ever seeing its own exit; this
// test failed 5/5 runs against that code (go test -run
// TestDarwin_CancelAfterFireDoesNotStealAnotherWatch -count=5) and the
// existing concurrent-churn test above did not catch it, since it never
// exercises a watch firing to completion before a stale cancel of it runs.
func TestDarwin_CancelAfterFireDoesNotStealAnotherWatch(t *testing.T) {
	bin := buildTestMembershipBinary(t)

	spawn := func(name string) (int, *exec.Cmd) {
		dir := t.TempDir()
		pidfile := filepath.Join(dir, name+".pid")
		cmd := exec.Command(bin, "-mode=chain", "-pidfiles="+pidfile)
		if err := cmd.Start(); err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
		return readPID(t, pidfile), cmd
	}

	pidA, cmdA := spawn("a")
	wantA, ok := NewSource().Info(pidA)
	if !ok {
		t.Fatal("Info(A) failed")
	}
	firedA := make(chan struct{})
	cancelA, err := WatchExit(pidA, wantA, func() { close(firedA) })
	if err != nil {
		t.Fatalf("WatchExit(A): %v", err)
	}
	if err := cmdA.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal A: %v", err)
	}
	select {
	case <-firedA:
	case <-time.After(5 * time.Second):
		t.Fatal("A did not fire")
	}
	_ = cmdA.Wait()

	// B opens right after A's kq closes, while that fd number is freshest.
	pidB, cmdB := spawn("b")
	t.Cleanup(func() { killAndWait(t, cmdB) })
	wantB, ok := NewSource().Info(pidB)
	if !ok {
		t.Fatal("Info(B) failed")
	}
	firedB := make(chan struct{})
	cancelB, err := WatchExit(pidB, wantB, func() { close(firedB) })
	if err != nil {
		t.Fatalf("WatchExit(B): %v", err)
	}
	defer cancelB()

	// The stale call under test: A is done, but nothing stops a caller from
	// calling its cancel func anyway (e.g. a defer that always runs).
	cancelA()

	if err := cmdB.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal B: %v", err)
	}
	select {
	case <-firedB:
	case <-time.After(5 * time.Second):
		t.Fatal("B did not fire: a stale cancelA() after A's own exit stole B's watch")
	}
}

func TestDarwin_WatchExitCancelFromInsideOnExit(t *testing.T) {
	bin := buildTestMembershipBinary(t)
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "watched.pid")

	cmd := exec.Command(bin, "-mode=chain", "-pidfiles="+pidfile)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = cmd.Wait() }()

	pid := readPID(t, pidfile)
	want, ok := NewSource().Info(pid)
	if !ok {
		t.Fatal("Info(watched) failed")
	}

	done := make(chan struct{})
	// cancelCh hands the cancel func to onExit safely across goroutines: a
	// channel send/receive is a happens-before edge, and a bare closure
	// variable assigned after WatchExit returns is not — the watcher
	// goroutine that runs onExit is already alive at that point, since
	// WatchExit starts it before returning.
	cancelCh := make(chan func(), 1)
	cancel, err := WatchExit(pid, want, func() {
		// A reentrant cancel() must return without deadlocking on its own
		// completion — stopped can't close until this call returns.
		(<-cancelCh)()
		close(done)
	})
	if err != nil {
		t.Fatalf("WatchExit: %v", err)
	}
	cancelCh <- cancel
	defer cancel()

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal watched process: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("onExit calling cancel() reentrantly deadlocked")
	}
}
