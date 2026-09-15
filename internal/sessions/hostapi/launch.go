package hostapi

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/shim"
)

// helloWait is C5's own bound: "The host replies only after the shim's
// status fd reports hello_ok, no_identity or a failure, waiting at most
// 10 s." Applied to the whole identity-then-spawn status sequence, not just
// the first event, since both phases (Hello, then spawn) must complete
// before /launch has anything meaningful to answer with.
const helloWait = 10 * time.Second

// terminateGrace is C5's "SIGTERM to the shim, then SIGKILL after 3 s."
const terminateGrace = 3 * time.Second

// launchOutcome is what spawnShim learned from the shim's status events —
// enough for the HTTP handler to pick a response, never more.
type launchOutcome struct {
	identityRefused bool
	spawnFailed     bool
	spawnErrno      int
	started         bool
	targetPID       int // the shim's own child's pid (the "started" event) — never the session root; see sessionEntry's doc comment
}

// spawnShim implements C5's host-side half of a launch: build the shim's
// argv and fds per C6, start it as this process's own child (so it can
// later be sent SIGTERM/SIGKILL by pid, and reaped without leaking a
// zombie), and read its status events up to helloWait.
//
// It never touches PTY sessions: this skeleton has no real terminal host
// behind it yet (that is R-S6's job, "spawn through the shim Launcher" per
// the plan's own R-S6 dependency line), so req.PTY is rejected earlier, in
// the HTTP handler, before this is ever called.
func (s *Server) spawnShim(sessionID string, req LaunchRequest) (*exec.Cmd, *launchOutcome, error) {
	args := []string{"exec", "--session-id", sessionID}

	var extraFiles []*os.File
	statusFDNum := 3

	if req.Identity != nil {
		secretR, secretW, err := os.Pipe()
		if err != nil {
			return nil, nil, fmt.Errorf("identity pipe: %w", err)
		}
		if _, err := secretW.WriteString(req.Identity.Secret); err != nil {
			_ = secretR.Close()
			_ = secretW.Close()
			return nil, nil, fmt.Errorf("write identity secret: %w", err)
		}
		if err := secretW.Close(); err != nil {
			_ = secretR.Close()
			return nil, nil, fmt.Errorf("close identity pipe write end: %w", err)
		}
		args = append(args, "--identity")
		extraFiles = append(extraFiles, secretR)
		statusFDNum = 4
	}

	if req.Sandbox != nil && req.Sandbox.ProfilePath != "" {
		args = append(args, "--sandbox-profile", req.Sandbox.ProfilePath)
	}

	statusR, statusW, err := os.Pipe()
	if err != nil {
		for _, f := range extraFiles {
			_ = f.Close()
		}
		return nil, nil, fmt.Errorf("status pipe: %w", err)
	}
	extraFiles = append(extraFiles, statusW)
	args = append(args, "--status-fd", strconv.Itoa(statusFDNum), "--")
	args = append(args, req.Argv...)

	cmd := exec.Command(s.cfg.ShimBinary, args...)
	cmd.ExtraFiles = extraFiles
	// Skeleton: no real terminal/pty hosting behind this yet, so the target
	// gets no interactive stdio. nil connects each to /dev/null (os/exec).
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil

	if err := cmd.Start(); err != nil {
		_ = statusR.Close()
		for _, f := range extraFiles {
			_ = f.Close()
		}
		return nil, nil, fmt.Errorf("start shim: %w", err)
	}
	// The host's own copies of the fds now duplicated into the child: keep
	// only statusR (the host reads it). Closing the rest here, not before
	// Start, is what lets the write end actually reach the child.
	for _, f := range extraFiles {
		_ = f.Close()
	}

	outcome, readErr := readShimStatus(statusR)
	_ = statusR.Close()
	if readErr != nil && !outcome.started && !outcome.identityRefused && !outcome.spawnFailed {
		// Timed out or the pipe broke before any terminal event arrived —
		// C5 has no code for "the host itself gave up waiting", so this
		// folds into spawn_failed, the closest defined outcome.
		outcome.spawnFailed = true
	}
	return cmd, outcome, nil
}

// readShimStatus reads relay-sessions exec's fd-4 events (shim.StatusEvent)
// until a terminal one arrives or helloWait elapses.
func readShimStatus(f *os.File) (*launchOutcome, error) {
	out := &launchOutcome{}
	_ = f.SetReadDeadline(time.Now().Add(helloWait))

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ev shim.StatusEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		switch ev.Event {
		case "hello_refused", "bad_secret":
			out.identityRefused = true
			return out, nil
		case "hello_ok", "no_identity":
			// Identity phase done; keep reading for the spawn phase.
		case "spawn_failed":
			out.spawnFailed = true
			out.spawnErrno = ev.Errno
			return out, nil
		case "started":
			out.started = true
			out.targetPID = ev.PID
			return out, nil
		}
	}
	return out, sc.Err()
}

// reapShim waits for a spawned shim to exit so it never becomes a zombie,
// then marks its session ended. Since a session's root is the shim itself
// (sessionEntry's doc comment) and this host is the shim's real OS parent,
// wait(2) is authoritative for "is this session over" — no separate
// membership.WatchExit/kqueue watch of a grandchild is needed here, unlike
// a hook peer's own ancestry walk, which does have to reach past processes
// this host never directly parented.
func (s *Server) reapShim(sessionID string, cmd *exec.Cmd) {
	_ = cmd.Wait()
	s.table.markEnded(sessionID)
}

// terminateShim implements C5's POST /terminate: SIGTERM the shim (which
// forwards, per C6 step 7, to the target's own process group), then SIGKILL
// after terminateGrace if it hasn't exited. It also signals the target's
// own process group directly, belt-and-suspenders: a target that ignores
// SIGTERM must not be able to outlive a shim that a SIGKILL then removes
// before the shim's own forwarding (already best-effort, not guaranteed —
// see internal/sessions/shim's own documented signal-forwarding race) had a
// chance to matter. targetPID of 0 (not yet known, e.g. terminate racing a
// launch still in flight) skips the direct signal; the shim SIGTERM/SIGKILL
// still applies either way.
func terminateShim(shimPID, targetPID int) {
	_ = syscall.Kill(shimPID, syscall.SIGTERM)
	if targetPID > 0 {
		_ = syscall.Kill(-targetPID, syscall.SIGTERM)
	}
	go func() {
		time.Sleep(terminateGrace)
		// Signal 0 probes for existence without actually signalling —
		// ESRCH means it (and any zombie) is already gone, nothing to kill.
		if err := syscall.Kill(shimPID, 0); err == nil {
			_ = syscall.Kill(shimPID, syscall.SIGKILL)
		}
		if targetPID > 0 {
			_ = syscall.Kill(-targetPID, syscall.SIGKILL)
		}
	}()
}
