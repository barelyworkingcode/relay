// Package shim implements `relay-sessions exec` (plan-broker-and-sessions.md
// contract C6): the small process relay-sessions' host spawns for every
// terminal or provider session. It reads an optional launch secret off fd 3,
// optionally becomes a PTY session leader, optionally proves its identity to
// relay over the bridge socket, then spawns the real target (shell, claude,
// pi, …) as its own child — never execing into it, so the shim's pid keeps
// meaning "this session's root" for C3's ancestry walk (SH §4.2) — forwards
// signals to the target's process group, and reports each step on fd 4 as
// newline-delimited JSON so the host never has to guess what happened from
// the exit code alone.
package shim

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// Exit codes C6 assigns a specific meaning to. Anything else (bare exec.Cmd
// plumbing failures) uses ExitInternal.
const (
	ExitIdentityFailure = 78  // bad/missing secret, or a refused Hello
	ExitSpawnENOENT     = 127 // target executable not found
	ExitSpawnOther      = 126 // spawn failed for any other reason
	ExitInternal        = 70  // usage or internal error (EX_SOFTWARE-ish)
)

// identityFD is fd 3: the secret pipe's read end, present iff --identity.
// Fixed by the fd table in C6, not a flag — mirrors bridge.LaunchFD's own
// fixed-fd convention for the same reason (a moving target here would make
// the host's ExtraFiles wiring and this file's constant drift independently).
const identityFD = 3

// envLaunchFD names the inherited descriptor holding the launch secret, in
// the shim's own environment (set by the host iff --identity). Spelled out
// locally rather than imported from internal/bridge: see hello.go's doc
// comment for why this package doesn't reach into internal/bridge this wave.
const envLaunchFD = "RELAY_LAUNCH_FD"

const envBridgeSocket = "RELAY_BRIDGE_SOCKET"

// StatusEvent is one line relay-sessions exec writes to fd 4. Every event
// C6 defines is one of these with a different subset of fields populated;
// the zero value of an unused field is omitted rather than sent as 0/"" so a
// reader can tell "not applicable" from "genuinely zero".
type StatusEvent struct {
	Event  string `json:"event"`
	PID    int    `json:"pid,omitempty"`
	Errno  int    `json:"errno,omitempty"`
	Status int    `json:"status,omitempty"`
	Signal int    `json:"signal,omitempty"`
}

// Config is relay-sessions exec's parsed command line:
//
//	relay-sessions exec --session-id <id> [--identity] [--pty] \
//	  [--sandbox-profile <abs path>] --status-fd 4 -- <target argv…>
type Config struct {
	SessionID      string
	Identity       bool
	PTY            bool
	SandboxProfile string
	StatusFD       int
	Argv           []string
}

// ParseArgs parses the exec subcommand's flags. args is os.Args[2:] (i.e.
// with "relay-sessions" and "exec" already stripped).
func ParseArgs(args []string) (Config, error) {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sessionID := fs.String("session-id", "", "session id (bound to the target's argv[0] identity, if --identity)")
	identity := fs.Bool("identity", false, "read a launch secret off fd 3 and Hello before spawning")
	pty := fs.Bool("pty", false, "become a PTY session leader on fd 0 before spawning")
	sandboxProfile := fs.String("sandbox-profile", "", "absolute path to an SBPL profile; wraps the target in sandbox-exec")
	statusFD := fs.Int("status-fd", 4, "fd to write newline-delimited status JSON to")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if *sessionID == "" {
		return Config{}, errors.New("--session-id is required")
	}
	if *sandboxProfile != "" && !isAbs(*sandboxProfile) {
		return Config{}, fmt.Errorf("--sandbox-profile must be an absolute path, got %q", *sandboxProfile)
	}
	argv := fs.Args()
	if len(argv) == 0 {
		return Config{}, errors.New("no target argv given (expected `-- <argv...>`)")
	}
	return Config{
		SessionID:      *sessionID,
		Identity:       *identity,
		PTY:            *pty,
		SandboxProfile: *sandboxProfile,
		StatusFD:       *statusFD,
		Argv:           argv,
	}, nil
}

func isAbs(p string) bool { return len(p) > 0 && p[0] == '/' }

// Run executes the exec subcommand end to end and returns the process exit
// code. It never calls os.Exit itself, so its deferred fd/process cleanup
// always runs; the caller (cmd/relaysessions/main.go) is the one that exits.
func Run(args []string) int {
	cfg, err := ParseArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay-sessions exec: %v\n", err)
		return ExitInternal
	}
	return runConfig(cfg)
}

func runConfig(cfg Config) int {
	status, err := newStatusWriter(cfg.StatusFD)
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay-sessions exec: status fd: %v\n", err)
		return ExitInternal
	}
	defer status.close()

	if cfg.Identity {
		secret, ok := readIdentitySecret()
		_ = os.Unsetenv(envLaunchFD)
		if !ok {
			status.emit(StatusEvent{Event: "bad_secret"})
			return ExitIdentityFailure
		}

		bridgeSock := os.Getenv(envBridgeSocket)
		if bridgeSock == "" {
			status.emit(StatusEvent{Event: "hello_refused"})
			return ExitIdentityFailure
		}
		if _, err := sendProjectSessionHello(bridgeSock, cfg.SessionID, secret); err != nil {
			status.emit(StatusEvent{Event: "hello_refused"})
			return ExitIdentityFailure
		}
	}

	if cfg.PTY {
		if _, err := syscall.Setsid(); err != nil {
			fmt.Fprintf(os.Stderr, "relay-sessions exec: setsid: %v\n", err)
			return ExitInternal
		}
		if err := unix.IoctlSetInt(0, unix.TIOCSCTTY, 0); err != nil {
			fmt.Fprintf(os.Stderr, "relay-sessions exec: TIOCSCTTY: %v\n", err)
			return ExitInternal
		}
	}

	if cfg.Identity {
		status.emit(StatusEvent{Event: "hello_ok", PID: os.Getpid()})
	} else {
		status.emit(StatusEvent{Event: "no_identity", PID: os.Getpid()})
	}

	argv := cfg.Argv
	if cfg.SandboxProfile != "" {
		argv = append([]string{"/usr/bin/sandbox-exec", "-f", cfg.SandboxProfile, "--"}, argv...)
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Env: nil inherits the shim's own environ, which by this point no
	// longer has RELAY_LAUNCH_FD (unset above whenever --identity was set).
	// fds 3 and 4 are never in ExtraFiles, so the target gets neither: 3 was
	// fully closed by readIdentitySecret before we ever got here, and 4 is
	// CLOEXEC (set in newStatusWriter), so exec(2) closes it in the child.
	attr := &syscall.SysProcAttr{Setpgid: true}
	if cfg.PTY {
		attr.Foreground = true
		attr.Ctty = 0 // fd 0 in the shim's own table: the PTY slave, ctty'd above.
	}
	cmd.SysProcAttr = attr

	if err := cmd.Start(); err != nil {
		errno := 0
		var se syscall.Errno
		if errors.As(err, &se) {
			errno = int(se)
		}
		notFound := errors.Is(err, exec.ErrNotFound) || errors.Is(err, syscall.ENOENT)
		if notFound && errno == 0 {
			errno = int(syscall.ENOENT)
		}
		status.emit(StatusEvent{Event: "spawn_failed", Errno: errno})
		if notFound {
			return ExitSpawnENOENT
		}
		return ExitSpawnOther
	}

	targetPID := cmd.Process.Pid
	status.emit(StatusEvent{Event: "started", PID: targetPID})

	sigCh := make(chan os.Signal, 8)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer signal.Stop(sigCh)
	forwardDone := make(chan struct{})
	go func() {
		defer close(forwardDone)
		for sig := range sigCh {
			ss, ok := sig.(syscall.Signal)
			if !ok {
				continue
			}
			// Negative pid: signal the whole process group Setpgid put the
			// target in, not just the target itself, so a shell's own
			// children (SH §4.2) see the same signal a real terminal would
			// deliver to its foreground group.
			_ = syscall.Kill(-targetPID, ss)
		}
	}()

	waitErr := cmd.Wait()
	signal.Stop(sigCh)
	close(sigCh)
	<-forwardDone

	exitStatus, exitSignal, exitCode := decodeWait(waitErr)
	status.emit(StatusEvent{Event: "exit", Status: exitStatus, Signal: exitSignal})
	return exitCode
}

// decodeWait turns cmd.Wait()'s error into the target's own wait(2) status
// (for the status event) and this shim's own exit code: the target's code
// verbatim on a normal exit, or 128+signal on a signalled one (C6's own
// rule, matching a shell's $?).
func decodeWait(waitErr error) (status, sig, exitCode int) {
	if waitErr == nil {
		return 0, 0, 0
	}
	var ee *exec.ExitError
	if !errors.As(waitErr, &ee) {
		// cmd.Wait() failed for a reason that has nothing to do with the
		// target's own exit (e.g. an I/O error reaping it) — there is no
		// wait(2) status to report.
		return 0, 0, ExitInternal
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok {
		return 0, 0, ExitInternal
	}
	if ws.Signaled() {
		s := int(ws.Signal())
		return 0, s, 128 + s
	}
	return ws.ExitStatus(), 0, ws.ExitStatus()
}

// readIdentitySecret implements C6 step 1: read fd 3 to EOF (at most 65
// bytes so an over-long pipe is detected rather than silently truncated),
// close it — unconditionally, so the target never inherits an open fd 3
// regardless of whether the secret was valid — and accept only exactly 64
// lowercase hex characters.
func readIdentitySecret() (secret string, ok bool) {
	f := os.NewFile(uintptr(identityFD), "relay-launch-secret")
	if f == nil {
		return "", false
	}
	buf, readErr := io.ReadAll(io.LimitReader(f, 65))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil {
		return "", false
	}
	if !isLaunchSecret(string(buf)) {
		return "", false
	}
	return string(buf), true
}

func isLaunchSecret(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

type statusWriter struct{ f *os.File }

// newStatusWriter wraps fd (always present per the C6 fd table) and marks it
// close-on-exec: the shim itself must keep writing to it right up to step 8,
// but the target spawned in step 5 must not inherit it — an open fd 4 in the
// target would let EOF on it (the host's "did the shim crash" signal) be
// delayed by a process the host never asked about.
func newStatusWriter(fd int) (*statusWriter, error) {
	f := os.NewFile(uintptr(fd), "relay-status")
	if f == nil {
		return nil, fmt.Errorf("status fd %d is not open", fd)
	}
	syscall.CloseOnExec(fd)
	return &statusWriter{f: f}, nil
}

func (w *statusWriter) emit(ev StatusEvent) {
	line, err := json.Marshal(ev)
	if err != nil {
		return
	}
	line = append(line, '\n')
	_, _ = w.f.Write(line)
}

func (w *statusWriter) close() { _ = w.f.Close() }
