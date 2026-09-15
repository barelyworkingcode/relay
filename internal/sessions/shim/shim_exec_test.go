package shim_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/shim"
)

// execOpts drives one `relay-sessions exec` invocation under test.
type execOpts struct {
	sessionID    string
	identity     bool
	secret       string // raw bytes written to fd 3; only meaningful if identity
	bridgeSocket string
	pty          bool
	ptySlave     *os.File
	targetArgv   []string
	extraEnv     []string
}

type execHandle struct {
	t      *testing.T
	cmd    *exec.Cmd
	events chan shim.StatusEvent
	stderr *bytes.Buffer
}

func startExec(t *testing.T, opts execOpts) *execHandle {
	t.Helper()
	relaySessionsBin, _ := buildBinaries(t)

	args := []string{"exec", "--session-id", opts.sessionID}
	if opts.identity {
		args = append(args, "--identity")
	}
	if opts.pty {
		args = append(args, "--pty")
	}

	var extraFiles []*os.File
	statusFD := 3
	if opts.identity {
		secretR, secretW, err := os.Pipe()
		if err != nil {
			t.Fatalf("secret pipe: %v", err)
		}
		if _, err := secretW.WriteString(opts.secret); err != nil {
			t.Fatalf("write secret: %v", err)
		}
		if err := secretW.Close(); err != nil {
			t.Fatalf("close secret write end: %v", err)
		}
		extraFiles = append(extraFiles, secretR)
		statusFD = 4
	}
	statusR, statusW, err := os.Pipe()
	if err != nil {
		t.Fatalf("status pipe: %v", err)
	}
	extraFiles = append(extraFiles, statusW)
	args = append(args, "--status-fd", strconv.Itoa(statusFD), "--")
	args = append(args, opts.targetArgv...)

	cmd := exec.Command(relaySessionsBin, args...)
	cmd.ExtraFiles = extraFiles

	stderrBuf := &bytes.Buffer{}
	if opts.pty {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = opts.ptySlave, opts.ptySlave, opts.ptySlave
	} else {
		cmd.Stdin = nil
		cmd.Stdout = nil
		cmd.Stderr = stderrBuf
	}

	env := os.Environ()
	if opts.bridgeSocket != "" {
		env = append(env, "RELAY_BRIDGE_SOCKET="+opts.bridgeSocket)
	}
	env = append(env, opts.extraEnv...)
	cmd.Env = env

	if err := cmd.Start(); err != nil {
		t.Fatalf("start relay-sessions exec: %v", err)
	}
	for _, f := range extraFiles {
		_ = f.Close()
	}

	events := make(chan shim.StatusEvent, 16)
	go func() {
		defer close(events)
		defer func() { _ = statusR.Close() }()
		sc := bufio.NewScanner(statusR)
		for sc.Scan() {
			var ev shim.StatusEvent
			if json.Unmarshal(sc.Bytes(), &ev) == nil {
				events <- ev
			}
		}
	}()

	return &execHandle{t: t, cmd: cmd, events: events, stderr: stderrBuf}
}

// waitForEvent drains h.events until name appears (returning it), the
// channel closes, or timeout elapses (fataling the test either way).
func (h *execHandle) waitForEvent(name string, timeout time.Duration) shim.StatusEvent {
	h.t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-h.events:
			if !ok {
				h.t.Fatalf("status stream closed before %q event", name)
			}
			if ev.Event == name {
				return ev
			}
		case <-deadline:
			h.t.Fatalf("timed out waiting for %q event", name)
		}
	}
}

// drainRest reads every remaining event until the stream closes.
func (h *execHandle) drainRest(timeout time.Duration) []shim.StatusEvent {
	h.t.Helper()
	var out []shim.StatusEvent
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-h.events:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			h.t.Fatalf("timed out draining status stream")
		}
	}
}

func (h *execHandle) allEvents(timeout time.Duration) []shim.StatusEvent {
	return h.drainRest(timeout)
}

func (h *execHandle) wait() int {
	h.t.Helper()
	err := h.cmd.Wait()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if ok := asExitError(err, &ee); ok {
		return ee.ExitCode()
	}
	h.t.Fatalf("cmd.Wait: unexpected error: %v", err)
	return -1
}

func asExitError(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*target = ee
	}
	return ok
}

func eventNames(evs []shim.StatusEvent) []string {
	names := make([]string, len(evs))
	for i, e := range evs {
		names[i] = e.Event
	}
	return names
}

// --- Tests -----------------------------------------------------------

func TestExec_UsageErrors(t *testing.T) {
	relaySessionsBin, _ := buildBinaries(t)

	t.Run("missing session-id", func(t *testing.T) {
		cmd := exec.Command(relaySessionsBin, "exec", "--status-fd", "4", "--", "/bin/echo")
		out, err := cmd.CombinedOutput()
		code := exitCodeOf(t, err)
		if code != shim.ExitInternal {
			t.Fatalf("exit code = %d, want %d; output: %s", code, shim.ExitInternal, out)
		}
	})

	t.Run("no target argv", func(t *testing.T) {
		cmd := exec.Command(relaySessionsBin, "exec", "--session-id", "s1", "--status-fd", "4")
		out, err := cmd.CombinedOutput()
		code := exitCodeOf(t, err)
		if code != shim.ExitInternal {
			t.Fatalf("exit code = %d, want %d; output: %s", code, shim.ExitInternal, out)
		}
	})
}

func exitCodeOf(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	t.Fatalf("unexpected error type: %v", err)
	return -1
}

// TestExec_FD3ClosedInTarget proves the shim closes the secret pipe before
// spawning the target: with --identity and a fake bridge that accepts the
// Hello, the target must see fd 3 as EBADF, never an open, readable pipe.
func TestExec_FD3ClosedInTarget(t *testing.T) {
	_, testTargetBin := buildBinaries(t)
	bridgeSock, received := startFakeBridge(t, fakeBridgeOK)
	dir := mkShortTempDir(t, "fd3-")
	fd3CheckFile := filepath.Join(dir, "fd3.txt")
	secret := strings.Repeat("a", 64)

	h := startExec(t, execOpts{
		sessionID:    "sess-fd3",
		identity:     true,
		secret:       secret,
		bridgeSocket: bridgeSock,
		targetArgv:   []string{testTargetBin, "-fd3-check", fd3CheckFile, "-sleep", "5ms", "-exit-code", "0"},
	})
	h.waitForEvent("exit", 10*time.Second)
	_ = h.wait()
	wantHello(t, <-received, "sess-fd3", secret)

	got, err := os.ReadFile(fd3CheckFile)
	if err != nil {
		t.Fatalf("read fd3 check file: %v", err)
	}
	if string(got) != "EBADF" {
		t.Fatalf("fd 3 in target = %q, want EBADF", got)
	}
}

// TestExec_BadSecret_Exit78 covers C6 step 1: anything but exactly 64
// lowercase hex on fd 3 must exit 78 and the target must never start at all
// (proved by the marker file it would otherwise create being absent). A
// real, working fake bridge is deliberately present and listening on
// RELAY_BRIDGE_SOCKET for every case: without one, this test would also
// pass via the unrelated "no bridge socket" failure path (C6 step 3) even
// if step 1's own hex validation were deleted entirely, since both paths
// share the same exit code. Asserting the specific bad_secret status event
// — which only step 1 ever emits — closes that gap, and the bridge must
// never even see a connection (checked via the received channel staying
// empty), since a bad secret must fail before step 3 dials anything.
func TestExec_BadSecret_Exit78(t *testing.T) {
	_, testTargetBin := buildBinaries(t)
	bridgeSock, received := startFakeBridge(t, fakeBridgeOK)
	dir := mkShortTempDir(t, "badsecret-")
	marker := filepath.Join(dir, "marker.json")

	cases := map[string]string{
		"too_short":  "abc123",
		"uppercase":  strings.Repeat("A", 64),
		"too_long":   strings.Repeat("a", 65),
		"non_hex":    strings.Repeat("g", 64),
		"empty":      "",
		"has_prefix": "0x" + strings.Repeat("a", 62),
	}
	for name, secret := range cases {
		t.Run(name, func(t *testing.T) {
			_ = os.Remove(marker)
			h := startExec(t, execOpts{
				sessionID:    "sess-badsecret-" + name,
				identity:     true,
				secret:       secret,
				bridgeSocket: bridgeSock,
				targetArgv:   []string{testTargetBin, "-marker", marker},
			})
			ev := h.waitForEvent("bad_secret", 10*time.Second)
			if ev.Event != "bad_secret" {
				t.Fatalf("got event %q, want bad_secret", ev.Event)
			}
			code := h.wait()
			if code != shim.ExitIdentityFailure {
				t.Fatalf("exit code = %d, want %d", code, shim.ExitIdentityFailure)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatalf("target ran and wrote a marker despite a bad secret")
			}
			select {
			case got := <-received:
				t.Fatalf("bridge received a Hello (%+v) despite a bad secret", got)
			default:
			}
		})
	}
}

// TestExec_HelloRefused_Exit78 covers C6 step 3's failure path: a valid
// secret but a bridge that refuses the Hello must still exit 78 with the
// target never started.
func TestExec_HelloRefused_Exit78(t *testing.T) {
	_, testTargetBin := buildBinaries(t)
	bridgeSock, received := startFakeBridge(t, fakeBridgeRefuse)
	dir := mkShortTempDir(t, "refused-")
	marker := filepath.Join(dir, "marker.json")
	secret := strings.Repeat("b", 64)

	h := startExec(t, execOpts{
		sessionID:    "sess-refused",
		identity:     true,
		secret:       secret,
		bridgeSocket: bridgeSock,
		targetArgv:   []string{testTargetBin, "-marker", marker},
	})
	ev := h.waitForEvent("hello_refused", 10*time.Second)
	if ev.Event != "hello_refused" {
		t.Fatalf("got event %q", ev.Event)
	}
	code := h.wait()
	if code != shim.ExitIdentityFailure {
		t.Fatalf("exit code = %d, want %d", code, shim.ExitIdentityFailure)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("target ran despite a refused Hello")
	}
	wantHello(t, <-received, "sess-refused", secret)
}

// TestExec_HelloRefused_NoBridgeSocket covers the same failure path when
// RELAY_BRIDGE_SOCKET is simply unset/empty: the shim must fail exactly the
// same way as a real refusal, not hang or crash.
func TestExec_HelloRefused_NoBridgeSocket(t *testing.T) {
	_, testTargetBin := buildBinaries(t)
	h := startExec(t, execOpts{
		sessionID:  "sess-nobridge",
		identity:   true,
		secret:     strings.Repeat("c", 64),
		targetArgv: []string{testTargetBin},
		extraEnv:   []string{"RELAY_BRIDGE_SOCKET="},
	})
	h.waitForEvent("hello_refused", 10*time.Second)
	code := h.wait()
	if code != shim.ExitIdentityFailure {
		t.Fatalf("exit code = %d, want %d", code, shim.ExitIdentityFailure)
	}
}

// TestExec_StatusEventOrder_WithIdentity checks the full success sequence
// step by step matches C6: hello_ok, started, exit.
func TestExec_StatusEventOrder_WithIdentity(t *testing.T) {
	_, testTargetBin := buildBinaries(t)
	bridgeSock, received := startFakeBridge(t, fakeBridgeOK)
	secret := strings.Repeat("d", 64)

	h := startExec(t, execOpts{
		sessionID:    "sess-order-id",
		identity:     true,
		secret:       secret,
		bridgeSocket: bridgeSock,
		targetArgv:   []string{testTargetBin, "-sleep", "5ms", "-exit-code", "0"},
	})
	evs := h.allEvents(10 * time.Second)
	h.wait()
	wantHello(t, <-received, "sess-order-id", secret)
	got := eventNames(evs)
	want := []string{"hello_ok", "started", "exit"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("event order = %v, want %v", got, want)
	}
	if evs[0].PID == 0 {
		t.Fatal("hello_ok missing pid")
	}
	if evs[1].PID == 0 {
		t.Fatal("started missing pid")
	}
}

// TestExec_StatusEventOrder_NoIdentity checks the no-identity sequence:
// no_identity, started, exit.
func TestExec_StatusEventOrder_NoIdentity(t *testing.T) {
	_, testTargetBin := buildBinaries(t)
	h := startExec(t, execOpts{
		sessionID:  "sess-order-noid",
		targetArgv: []string{testTargetBin, "-sleep", "5ms", "-exit-code", "0"},
	})
	evs := h.allEvents(10 * time.Second)
	h.wait()
	got := eventNames(evs)
	want := []string{"no_identity", "started", "exit"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("event order = %v, want %v", got, want)
	}
}

// TestExec_TargetExitCodePassesThrough proves the shim's own exit code is
// the target's own on a normal exit, and the exit event carries it.
func TestExec_TargetExitCodePassesThrough(t *testing.T) {
	_, testTargetBin := buildBinaries(t)
	h := startExec(t, execOpts{
		sessionID:  "sess-exitcode",
		targetArgv: []string{testTargetBin, "-sleep", "20ms", "-exit-code", "42"},
	})
	exitEv := h.waitForEvent("exit", 10*time.Second)
	code := h.wait()
	if code != 42 {
		t.Fatalf("shim exit code = %d, want 42", code)
	}
	if exitEv.Status != 42 || exitEv.Signal != 0 {
		t.Fatalf("exit event = %+v, want status=42 signal=0", exitEv)
	}
}

// TestExec_TargetKilledBySignal proves 128+signal: a target killed by an
// uncatchable signal (SIGKILL) must produce shim exit 128+9 and a matching
// exit event.
func TestExec_TargetKilledBySignal(t *testing.T) {
	_, testTargetBin := buildBinaries(t)
	dir := mkShortTempDir(t, "killsig-")
	marker := filepath.Join(dir, "marker.json")

	h := startExec(t, execOpts{
		sessionID:  "sess-killsig",
		targetArgv: []string{testTargetBin, "-marker", marker},
	})
	startedEv := h.waitForEvent("started", 10*time.Second)
	if startedEv.PID == 0 {
		t.Fatal("started event missing pid")
	}
	waitForFile(t, marker, 5*time.Second)

	if err := syscall.Kill(startedEv.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill target: %v", err)
	}

	exitEv := h.waitForEvent("exit", 10*time.Second)
	code := h.wait()
	const wantCode = 128 + 9
	if code != wantCode {
		t.Fatalf("shim exit code = %d, want %d", code, wantCode)
	}
	if exitEv.Signal != 9 {
		t.Fatalf("exit event signal = %d, want 9", exitEv.Signal)
	}
}

// TestExec_SignalForwarding proves SIGHUP/SIGINT/SIGTERM/SIGQUIT sent to the
// shim reach the target's process group, per C6 step 7.
func TestExec_SignalForwarding(t *testing.T) {
	_, testTargetBin := buildBinaries(t)

	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT} {
		sig := sig
		t.Run(sig.String(), func(t *testing.T) {
			dir := mkShortTempDir(t, "sigfwd-")
			marker := filepath.Join(dir, "sig.txt")

			h := startExec(t, execOpts{
				sessionID:  "sess-sigfwd-" + sig.String(),
				targetArgv: []string{testTargetBin, "-signal-marker", marker, "-exit-code", "17"},
			})
			shimPID := h.cmd.Process.Pid
			h.waitForEvent("started", 10*time.Second)
			waitForNoFile(t, marker, 2*time.Second) // target must not have exited on its own yet

			if err := syscall.Kill(shimPID, sig); err != nil {
				t.Fatalf("signal shim: %v", err)
			}

			exitEv := h.waitForEvent("exit", 10*time.Second)
			code := h.wait()
			if code != 17 {
				t.Fatalf("shim exit code = %d, want 17 (target's own voluntary exit)", code)
			}
			if exitEv.Status != 17 {
				t.Fatalf("exit event status = %d, want 17", exitEv.Status)
			}
			got, err := os.ReadFile(marker)
			if err != nil {
				t.Fatalf("target never observed the forwarded signal: %v", err)
			}
			if !strings.Contains(string(got), sig.String()) {
				t.Fatalf("signal marker = %q, want to contain %q", got, sig.String())
			}
		})
	}
}

// TestExec_SpawnFailed_ENOENT covers C6 step 5's failure path: a target
// executable that doesn't exist must produce a spawn_failed status event and
// exit 127.
func TestExec_SpawnFailed_ENOENT(t *testing.T) {
	h := startExec(t, execOpts{
		sessionID:  "sess-spawnfail",
		targetArgv: []string{"/no/such/binary-relay-sessions-test"},
	})
	ev := h.waitForEvent("spawn_failed", 10*time.Second)
	code := h.wait()
	if code != shim.ExitSpawnENOENT {
		t.Fatalf("exit code = %d, want %d", code, shim.ExitSpawnENOENT)
	}
	if ev.Errno == 0 {
		t.Fatal("spawn_failed event carries no errno")
	}
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s to appear", path)
}

func waitForNoFile(t *testing.T, path string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("%s appeared before the signal was sent", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
