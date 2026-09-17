package terminal

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/barelyworkingcode/relay/internal/service"
	"github.com/barelyworkingcode/relay/internal/sessions/clock"
	"github.com/barelyworkingcode/relay/internal/sessions/shim"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const (
	terminalScrollbackSize = 100 * 1024
	terminalReadBufSize    = 4096

	// helloWait matches C5's own bound on the whole identity-then-spawn
	// status sequence: "The host replies only after the shim's status fd
	// reports hello_ok, no_identity or a failure, waiting at most 10 s."
	helloWait = 10 * time.Second

	// terminateGrace matches C5's "SIGTERM to the shim, then SIGKILL after 3 s."
	terminateGrace = 3 * time.Second
)

// Session is one PTY-backed terminal, spawned locally or (Host != nil) over
// ssh, through an internal/sessions/shim child.
type Session struct {
	ID         string
	TemplateID string
	Name       string
	Directory  string
	Host       *sessionstypes.HostSpec
	CreatedAt  string

	cmd *exec.Cmd
	// ptmx is an atomic.Pointer, not a plain field under s.mu: if Write had
	// to hold s.mu across its own pty write syscall (potentially blocking
	// indefinitely, in raw mode with a full input queue) in order to read
	// this field safely, Close's own acquisition of s.mu to swap it to nil
	// would queue up behind that same blocked write — and never get it,
	// since the write can't return until something closes the fd Close is
	// blocked trying to reach. atomic.Pointer lets Write Load() the current
	// *os.File without taking s.mu at all, so Close's swap-and-close (also
	// lock-free) can never be stuck behind it. Close swaps this to nil,
	// and closes the fd, before it touches anything else — see Close for
	// what actually unblocks a write already parked in the kernel; closing
	// this fd alone is not it.
	ptmx      atomic.Pointer[os.File]
	targetPID int // the shim's own child (the real terminal target), for Close's belt-and-suspenders pgid signal

	mu       sync.Mutex
	cols     uint16
	rows     uint16
	state    string // "running" or "stopped"
	exitCode int

	alive      atomic.Bool
	scrollback *scrollBuffer
	waitDone   chan struct{}

	logger *terminalLogger

	idleTimeout time.Duration
	idleCancel  chan struct{}
	idleOnce    sync.Once
	clock       clock.Clock

	onOutput func(id string, data []byte)
	onExit   func(id string, exitCode int)
	onIdle   func(id string)
}

// launchOutcome mirrors internal/sessions/hostapi/launch.go's own type of
// the same name: what the shim's status events (fd 4) told the caller.
// Duplicated rather than imported — hostapi's is unexported, and this
// package does not depend on hostapi at all (doc.go).
type launchOutcome struct {
	identityRefused bool
	spawnFailed     bool
	spawnErrno      int
	started         bool
	targetPID       int
}

// ErrSessionExists is returned by Manager.Create for a duplicate session id.
var ErrSessionExists = errors.New("terminal: session already exists")

// ErrIdentityRefused is returned when the shim's Hello was refused (C5's
// identity_refused, 502).
var ErrIdentityRefused = errors.New("terminal: identity refused")

// ErrSpawnFailed is returned when the shim could not start the target (C5's
// spawn_failed, 500) or never reported success within helloWait.
var ErrSpawnFailed = errors.New("terminal: spawn failed")

// ErrNoBridgeSocket is returned when spec.Identity is set but cfg carries no
// real bridge socket for this launch: with nothing legitimate to Hello
// against, proceeding would let whatever ends up in RELAY_BRIDGE_SOCKET —
// including a caller-supplied one — decide where the launch secret goes.
var ErrNoBridgeSocket = errors.New("terminal: identity requires a configured bridge socket")

// startSession spawns the shim for spec and blocks (up to helloWait) until
// its status events confirm the target is running, mirroring hostapi's own
// spawnShim contract but adding real pty wiring end to end.
//
// onExit/onIdle/onOutput are wired into the returned Session before its
// readLoop/waitForExit goroutines start, not by the caller mutating the
// struct afterward: a short-lived target (this package's own tests use
// "echo hi; exit 7") can reach waitForExit (or readLoop's first chunk)
// before Manager.Create would otherwise have had a chance to set
// session.onExit/onOutput, a real data race this signature closes by
// construction rather than by convention.
func startSession(spec CreateSpec, cfg Config, onExit func(id string, exitCode int), onIdle func(id string), onOutput func(id string, data []byte)) (*Session, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}
	if spec.Identity != nil && cfg.BridgeSocket == "" {
		return nil, ErrNoBridgeSocket
	}

	targetArgv := spec.Argv
	if spec.Host != nil {
		name, args, err := buildHostTargetArgv(spec.Host, spec.Directory, spec.Argv, spec.Env)
		if err != nil {
			return nil, err
		}
		targetArgv = append([]string{name}, args...)
	}

	cols, rows := spec.Cols, spec.Rows
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}

	ptmx, tty, err := pty.Open()
	if err != nil {
		return nil, fmt.Errorf("open pty: %w", err)
	}
	if err := pty.Setsize(ptmx, &pty.Winsize{Rows: rows, Cols: cols}); err != nil {
		_ = ptmx.Close()
		_ = tty.Close()
		return nil, fmt.Errorf("set pty size: %w", err)
	}

	cmd, statusR, extraFiles, startErr := buildShimCmd(spec, cfg, targetArgv, tty)
	if startErr != nil {
		_ = ptmx.Close()
		_ = tty.Close()
		return nil, startErr
	}

	if err := cmd.Start(); err != nil {
		_ = statusR.Close()
		for _, f := range extraFiles {
			_ = f.Close()
		}
		_ = ptmx.Close()
		_ = tty.Close()
		return nil, fmt.Errorf("start shim: %w", err)
	}
	for _, f := range extraFiles {
		_ = f.Close()
	}
	// The parent's copy of the slave: the shim (via fds 0-2) holds the real
	// reference. Closing it here, after Start, is what lets ptmx be the only
	// surviving end this process still reads/writes.
	_ = tty.Close()

	outcome, readErr := readShimStatus(statusR)
	_ = statusR.Close()
	if readErr != nil && !outcome.started && !outcome.identityRefused && !outcome.spawnFailed {
		outcome.spawnFailed = true
	}

	if outcome.identityRefused {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = ptmx.Close()
		return nil, ErrIdentityRefused
	}
	if !outcome.started || outcome.spawnFailed {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = ptmx.Close()
		return nil, fmt.Errorf("%w: errno=%d", ErrSpawnFailed, outcome.spawnErrno)
	}

	s := &Session{
		ID:          spec.SessionID,
		TemplateID:  spec.TemplateID,
		Name:        spec.Name,
		Directory:   spec.Directory,
		Host:        spec.Host,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		cmd:         cmd,
		targetPID:   outcome.targetPID,
		cols:        cols,
		rows:        rows,
		state:       "running",
		scrollback:  newScrollBuffer(terminalScrollbackSize),
		waitDone:    make(chan struct{}),
		idleTimeout: spec.IdleTimeout,
		clock:       cfg.clockOrDefault(),
		onExit:      onExit,
		onIdle:      onIdle,
		onOutput:    onOutput,
	}
	s.ptmx.Store(ptmx)
	s.alive.Store(true)

	if cfg.LogDir != "" {
		if lg, lerr := newTerminalLogger(cfg.LogDir, s.ID); lerr == nil {
			s.logger = lg
		}
		// Disk logging is best-effort: an open failure still lets the
		// terminal run, just without replay support.
	}

	go s.readLoop()
	go s.waitForExit()

	return s, nil
}

// buildShimCmd assembles the `relay-sessions exec ... --pty ... -- <target>`
// invocation: the identity secret pipe (fd 3, iff spec.Identity is set), the
// status pipe (always), and the shim's own env (buildShimEnv). tty is wired
// as the shim's stdio — deliberately via pty.Open()+Setsize rather than
// pty.StartWithSize: the latter would call Setsid itself before exec, and
// the shim's own --pty handling (C6 step 2) also calls Setsid — a second
// Setsid against an already-session-leader process fails, so exactly one of
// the two call sites may own it. Leaving cmd.SysProcAttr nil here is what
// leaves that call to the shim.
func buildShimCmd(spec CreateSpec, cfg Config, targetArgv []string, tty *os.File) (cmd *exec.Cmd, statusR *os.File, extraFiles []*os.File, err error) {
	args := []string{"exec", "--session-id", spec.SessionID}
	statusFDNum := 3

	if spec.Identity != nil {
		secretR, secretW, perr := os.Pipe()
		if perr != nil {
			return nil, nil, nil, fmt.Errorf("identity pipe: %w", perr)
		}
		if _, werr := secretW.WriteString(spec.Identity.Secret); werr != nil {
			_ = secretR.Close()
			_ = secretW.Close()
			return nil, nil, nil, fmt.Errorf("write identity secret: %w", werr)
		}
		if cerr := secretW.Close(); cerr != nil {
			_ = secretR.Close()
			return nil, nil, nil, fmt.Errorf("close identity pipe write end: %w", cerr)
		}
		args = append(args, "--identity")
		extraFiles = append(extraFiles, secretR)
		statusFDNum = 4
	}

	args = append(args, "--pty")
	if spec.Sandbox != nil && spec.Sandbox.ProfilePath != "" {
		args = append(args, "--sandbox-profile", spec.Sandbox.ProfilePath)
	}

	sr, sw, perr := os.Pipe()
	if perr != nil {
		for _, f := range extraFiles {
			_ = f.Close()
		}
		return nil, nil, nil, fmt.Errorf("status pipe: %w", perr)
	}
	extraFiles = append(extraFiles, sw)
	args = append(args, "--status-fd", strconv.Itoa(statusFDNum), "--")
	args = append(args, targetArgv...)

	cmd = exec.Command(cfg.ShimBinary, args...)
	cmd.ExtraFiles = extraFiles
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.Env = buildShimEnv(spec, cfg)
	return cmd, sr, extraFiles, nil
}

// relaySecretEnvKeys must never be inherited by a spawned session's shim: a
// stale credential from this host process's own environment (e.g. a
// RELAY_PROJECT_TOKEN some other launch left set) must not leak into a
// target the caller never granted it to. A narrow, intentional duplicate of
// internal/sessions/mcp's own relaySecretEnvKeys/childBaseEnv (that
// package's own comment names this package, R-S6, as the intended home for
// the shim Launcher's env handling) rather than a shared dependency across
// two otherwise-independent small packages.
var relaySecretEnvKeys = []string{
	"RELAY_SERVICE_TOKEN",
	"RELAY_MCP_TOKEN",
	"RELAY_FRONTEND_TOKEN",
	"RELAY_LAUNCH_FD",
	"RELAY_PROJECT_TOKEN",
	"RELAY_TOKEN",
	"RELAY_LLM_TOKEN",
	"RELAY_LLM_HOOK_TOKEN",
}

func childBaseEnv() []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		drop := false
		for _, k := range relaySecretEnvKeys {
			if strings.HasPrefix(kv, k+"=") {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// buildShimEnv assembles the env `relay-sessions exec` receives (C6:
// "Environment in: host-built. Must contain RELAY_BRIDGE_SOCKET and
// RELAY_SESSION_ID"). Base is childBaseEnv (never a stale relay
// credential), then spec.Env (C5's caller-supplied env — TERM/COLORTERM in
// the common case), then this package's own additions, with a later entry
// winning on key collision.
//
// RELAY_SESSIONS_HOOK_SOCKET is deliberately never added here: C5 names it
// "(claude)" only, and this package never builds a claude session — a pty
// target has no PreToolUse hook process to dial it. RELAY_BRIDGE_SOCKET and
// RELAY_MODEL_SOCKET carry no such qualifier in C5's table, so a plain
// interactive shell gets them too: they are what let a human manually run
// `rh` or a model-aware CLI from inside the terminal, the same capability an
// agent kind gets automatically. Both are added only when cfg carries a
// non-empty value — the socket paths themselves are this host process's own
// concern to resolve (cmd/relaysessions, a later wiring unit), not
// something a bare Config in a unit test should have to fabricate.
//
// Every RELAY_-prefixed key in spec.Env is dropped before the merge, the
// same prefix-not-denylist reasoning internal/config/templates.go's
// EnvPassthrough check already uses: a caller/template must never be able to
// *name* RELAY_BRIDGE_SOCKET (or any other RELAY_* key) and have that value
// reach the child, even by the host simply having nothing of its own to
// override it with.
func buildShimEnv(spec CreateSpec, cfg Config) []string {
	add := make(map[string]string, len(spec.Env)+3)
	for k, v := range spec.Env {
		if strings.HasPrefix(k, "RELAY_") {
			continue
		}
		add[k] = v
	}
	add["RELAY_SESSION_ID"] = spec.SessionID
	if cfg.BridgeSocket != "" {
		add["RELAY_BRIDGE_SOCKET"] = cfg.BridgeSocket
	}
	if cfg.ModelSocket != "" {
		add["RELAY_MODEL_SOCKET"] = cfg.ModelSocket
	}

	cmd := &exec.Cmd{Env: childBaseEnv()}
	service.MergeEnv(cmd, add)
	return cmd.Env
}

// readShimStatus reads relay-sessions exec's fd-4 events (shim.StatusEvent)
// until a terminal one arrives or helloWait elapses. Mirrors hostapi's own
// readShimStatus exactly (same event set, same bound).
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

func (s *Session) readLoop() {
	defer s.logger.Close() // nil-safe; flushes and syncs the log files.
	// Loaded once: s.ptmx is only ever set here (startSession, before this
	// goroutine starts) and swapped to nil by Close, never reassigned to a
	// different live file, so the *os.File obtained here stays the correct
	// one for this session's whole lifetime — Close()'ing it is exactly what
	// makes the blocked Read below return.
	f := s.ptmx.Load()
	buf := make([]byte, terminalReadBufSize)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])

			s.scrollback.Write(chunk)
			if s.logger != nil {
				s.logger.Write(chunk)
			}
			if s.onOutput != nil {
				s.onOutput(s.ID, chunk)
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *Session) waitForExit() {
	err := s.cmd.Wait()
	s.alive.Store(false)

	// The shim's own exit code is the target's on a normal exit, or
	// 128+signal on a signalled one (C6 step 8) — so reading it here reports
	// the target's fate, not merely whether the shim process itself
	// survived.
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
	}

	s.mu.Lock()
	s.state = "stopped"
	s.exitCode = exitCode
	s.mu.Unlock()

	close(s.waitDone)

	if s.onExit != nil {
		s.onExit(s.ID, exitCode)
	}
}

// Write sends input data to the terminal PTY. Deliberately does not hold
// s.mu across the actual pty write: a raw-mode target that has stopped
// reading stdin (Ctrl-Z'd, wedged, a hung TUI) makes this write block
// indefinitely once the pty's input queue fills, and Close must be able to
// reach in and close the fd — the only thing that unblocks it — without
// waiting on the same mutex this call would otherwise be holding.
func (s *Session) Write(data []byte) error {
	if !s.alive.Load() {
		return fmt.Errorf("terminal not running")
	}
	f := s.ptmx.Load()
	if f == nil {
		return fmt.Errorf("terminal not running")
	}
	_, err := f.Write(data)
	return err
}

// Resize changes the PTY window size.
func (s *Session) Resize(cols, rows uint16) error {
	f := s.ptmx.Load()
	if f == nil {
		return fmt.Errorf("terminal not running")
	}
	s.mu.Lock()
	s.cols = cols
	s.rows = rows
	s.mu.Unlock()
	return pty.Setsize(f, &pty.Winsize{Rows: rows, Cols: cols})
}

// Size returns the current PTY dimensions.
func (s *Session) Size() (cols, rows uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cols, s.rows
}

// Close gracefully shuts down the terminal: SIGTERM, wait terminateGrace,
// then SIGKILL (C5's own /terminate rule). Also signals the target's own
// process group directly, belt-and-suspenders — same reasoning as
// hostapi's terminateShim: a target that ignores SIGTERM must not outlive a
// SIGKILL that removes the shim before its own forwarding (best-effort, not
// guaranteed) had a chance to matter.
func (s *Session) Close() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	s.alive.Store(false)
	// Swapping and closing ptmx here, before CancelIdleTimer's s.mu
	// acquisition, is what avoids the lock-ordering hazard ptmx's own doc
	// comment names — but on this platform (macOS) the pty master fd is not
	// registered with the runtime's netpoller, so this close alone does not
	// wake a write already parked in the write(2) syscall: measured
	// directly, a parked write stayed blocked 5+ seconds after this line
	// ran in isolation. What actually unblocks it is the SIGTERM/SIGKILL
	// below, which Close always performs unconditionally rather than
	// gating on whether a write might currently be parked: killing the
	// target drops the pty slave's last reference, and it's that drop —
	// not this Close(2) — that finally delivers the parked write its EIO.
	// The residual case this doesn't cover: a grandchild outside the
	// target's own process group that independently holds the slave open
	// keeps the pty alive regardless of the kill, so the write (and its
	// goroutine, and this fd) can stay unreclaimed until that grandchild
	// also exits — Go's fd refcounting defers ptmx's real close(2) until
	// every in-flight I/O reference to it, including that parked write,
	// has dropped.
	if f := s.ptmx.Swap(nil); f != nil {
		_ = f.Close()
	}
	s.CancelIdleTimer()

	shimPID := s.cmd.Process.Pid
	_ = syscall.Kill(shimPID, syscall.SIGTERM)
	if s.targetPID > 0 {
		_ = syscall.Kill(-s.targetPID, syscall.SIGTERM)
	}

	select {
	case <-s.waitDone:
	case <-s.clock.After(terminateGrace):
		_ = syscall.Kill(shimPID, syscall.SIGKILL)
		if s.targetPID > 0 {
			_ = syscall.Kill(-s.targetPID, syscall.SIGKILL)
		}
		<-s.waitDone
	}
}

// Alive reports whether the terminal process is still running.
func (s *Session) Alive() bool { return s.alive.Load() }

// Snapshot returns the current state and exit code, safe for concurrent reads.
func (s *Session) Snapshot() (state string, exitCode int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, s.exitCode
}

// ScrollbackBytes returns the current scrollback buffer contents.
func (s *Session) ScrollbackBytes() []byte {
	if s.scrollback == nil {
		return nil
	}
	return s.scrollback.Bytes()
}

// CreatedBody renders this session's C5 201 body / WS terminal_created
// shape (types.go's CreatedBody doc comment).
func (s *Session) CreatedBody() CreatedBody {
	return CreatedBody{
		TerminalID: s.ID,
		TemplateID: s.TemplateID,
		Name:       s.Name,
		Directory:  s.Directory,
		Host:       s.Host.Chip(),
	}
}

// StartIdleTimer begins the idle countdown. If no viewer reconnects before
// it fires, onIdle is called (which should close the terminal).
func (s *Session) StartIdleTimer() {
	if s.idleTimeout <= 0 || !s.alive.Load() {
		return
	}
	s.CancelIdleTimer()

	s.mu.Lock()
	s.idleCancel = make(chan struct{})
	s.idleOnce = sync.Once{}
	cancel := s.idleCancel
	s.mu.Unlock()

	go func() {
		select {
		case <-cancel:
			return
		case <-s.clock.After(s.idleTimeout):
		}
		if s.onIdle != nil {
			s.onIdle(s.ID)
		}
	}()
}

// CancelIdleTimer stops a pending idle timer (e.g. when a viewer reconnects).
func (s *Session) CancelIdleTimer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idleCancel != nil {
		s.idleOnce.Do(func() { close(s.idleCancel) })
	}
}

// scrollBuffer is a simple ring buffer for terminal scrollback.
type scrollBuffer struct {
	mu   sync.Mutex
	data []byte
	size int
}

func newScrollBuffer(size int) *scrollBuffer {
	return &scrollBuffer{data: make([]byte, 0, size), size: size}
}

func (b *scrollBuffer) Write(p []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if len(b.data) > b.size {
		b.data = b.data[len(b.data)-b.size:]
	}
}

func (b *scrollBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]byte, len(b.data))
	copy(out, b.data)
	return out
}
