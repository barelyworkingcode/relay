package main

// Client-side tests for `relay sandbox`. The client is the real code path
// (sandboxMain / attachTerminal) run as a child of this test binary with a real
// pseudo-terminal for stdin and stdout, against a fake relay listening on the
// sandboxed config dir's bridge socket. A child process is the only honest way
// to see signals, exit codes and terminal modes.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

const (
	sbxHelperEnv = "RELAY_SBX_HELPER"
	sbxHomeEnv   = "RELAY_SBX_HOME"
	sbxArgsEnv   = "RELAY_SBX_ARGS"
	sbxModeEnv   = "RELAY_SBX_MODE"
	sbxOutEnv    = "RELAY_SBX_OUT"
)

// TestHelperSandboxClient is the child process, not a test: it returns at once
// unless the parent test started it.
func TestHelperSandboxClient(t *testing.T) {
	if os.Getenv(sbxHelperEnv) != "1" {
		return
	}
	bridge.SetConfigDirForTest(os.Getenv(sbxHomeEnv))
	var args []string
	_ = json.Unmarshal([]byte(os.Getenv(sbxArgsEnv)), &args)
	switch os.Getenv(sbxModeEnv) {
	case "attach-recording":
		conn, err := net.Dial("unix", bridge.SocketPath())
		if err != nil {
			os.Exit(90)
		}
		rec := &sbxDeadlineRecorder{Conn: conn}
		code := attachTerminal(rec, bufio.NewReader(rec))
		_ = os.WriteFile(os.Getenv(sbxOutEnv), []byte(strconv.Itoa(int(rec.calls.Load()))), 0o600)
		os.Exit(code)
	case "send-then-exit":
		conn, err := net.Dial("unix", bridge.SocketPath())
		if err != nil {
			os.Exit(90)
		}
		cwd, _ := os.Getwd()
		arg, _ := json.Marshal(bridge.SandboxAttachRequest{Template: "shell", Cwd: cwd})
		line, _ := json.Marshal(bridge.BridgeRequest{Type: bridge.ReqSandboxAttach, Arguments: arg})
		if _, err := conn.Write(append(line, '\n')); err != nil {
			os.Exit(90)
		}
		os.Exit(1)
	default:
		os.Exit(sandboxMain(args))
	}
}

// --- fake relay ----------------------------------------------------------

type sbxBridge struct {
	t      *testing.T
	ln     net.Listener
	reqs   chan *sbxRequest
	probes atomic.Int32
}

type sbxRequest struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
	Req  bridge.BridgeRequest
	Args bridge.SandboxAttachRequest
	got  []bridge.StreamFrame
}

// startSandboxRelay listens where the client will look: bridge.SocketPath() of
// the sandboxed config dir. A connection that closes without a request line is
// a reachability probe (serviceReachable), not a request.
func startFakeRelay(t *testing.T, home string) *sbxBridge {
	t.Helper()
	ln, err := net.Listen("unix", filepath.Join(home, "relay.sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	b := &sbxBridge{t: t, ln: ln, reqs: make(chan *sbxRequest, 8)}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				r := bufio.NewReader(c)
				line, err := r.ReadBytes('\n')
				if err != nil && len(line) == 0 {
					b.probes.Add(1)
					_ = c.Close()
					return
				}
				req := &sbxRequest{t: t, conn: c, r: r}
				_ = json.Unmarshal(line, &req.Req)
				_ = json.Unmarshal(req.Req.Arguments, &req.Args)
				b.reqs <- req
			}()
		}
	}()
	t.Cleanup(func() {
		close(b.reqs)
		for r := range b.reqs {
			_ = r.conn.Close()
		}
	})
	return b
}

func (b *sbxBridge) expectRequest() *sbxRequest {
	b.t.Helper()
	select {
	case r := <-b.reqs:
		return r
	case <-time.After(sbxWait):
		b.t.Fatal("the client never sent a request")
		return nil
	}
}

// listenIdle listens where the client will look and never accepts, so every
// connection the client makes stays queued for bytesSentTo.
func listenIdle(t *testing.T, home string) *net.UnixListener {
	t.Helper()
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(home, "relay.sock"), Net: "unix"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// bytesSentTo drains ln and returns what each queued connection carried, in
// accept order; an empty entry is a reachability probe.
//
// Subtle: the drain is final only because the client has exited and three
// kernel facts hold. A Unix connect() has queued the server socket before it
// returns; a queued connection whose peer has closed stays queued; and a
// non-blocking accept() returning EAGAIN means the queue is empty.
func (p *sbxProc) bytesSentTo(ln *net.UnixListener) [][]byte {
	p.t.Helper()
	select {
	case <-p.exited:
	default:
		p.t.Fatal("bytesSentTo needs an exited client; a live one can still connect")
	}
	raw, err := ln.SyscallConn()
	if err != nil {
		p.t.Fatalf("listener SyscallConn: %v", err)
	}
	var sent [][]byte
	var drainErr error
	ctlErr := raw.Control(func(lfd uintptr) {
		for {
			syscall.ForkLock.RLock()
			fd, _, err := syscall.Accept(int(lfd))
			if err == nil {
				syscall.CloseOnExec(fd)
			}
			syscall.ForkLock.RUnlock()
			switch {
			case errors.Is(err, syscall.EINTR):
				continue
			case errors.Is(err, syscall.EAGAIN):
				return
			case err != nil:
				drainErr = fmt.Errorf("accept: %w", err)
				return
			}
			b, err := readQueuedConn(fd)
			if err != nil {
				drainErr = err
				return
			}
			sent = append(sent, b)
		}
	})
	if ctlErr != nil {
		p.t.Fatalf("listener Control: %v", ctlErr)
	}
	if drainErr != nil {
		p.t.Fatalf("drain the idle listener: %v", drainErr)
	}
	return sent
}

// readQueuedConn reads an accepted connection to EOF and closes it. Darwin
// hands out accepted sockets with the listener's O_NONBLOCK, so it is cleared
// first; the peer has exited, so EOF comes.
func readQueuedConn(fd int) ([]byte, error) {
	f := os.NewFile(uintptr(fd), "queued-conn")
	defer f.Close()
	if err := syscall.SetNonblock(fd, false); err != nil {
		return nil, fmt.Errorf("set blocking on accepted fd %d: %w", fd, err)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read accepted fd %d: %w", fd, err)
	}
	return b, nil
}

func (r *sbxRequest) write(v any) {
	r.t.Helper()
	b, _ := json.Marshal(v)
	if _, err := r.conn.Write(append(b, '\n')); err != nil {
		r.t.Fatalf("fake relay write: %v", err)
	}
}

func (r *sbxRequest) ack() {
	data, _ := json.Marshal(bridge.SandboxAttachResult{SessionID: "s-1", ProjectName: "Main", TemplateID: r.Args.Template, Cols: 80, Rows: 24})
	r.write(bridge.BridgeResponse{Type: bridge.RespAttached, Data: data})
}

func (r *sbxRequest) refuse(ref *bridge.SandboxRefusal) {
	data, _ := json.Marshal(ref)
	r.write(bridge.BridgeResponse{Type: bridge.RespError, Code: -32602, Message: ref.Message, Data: data})
}

func (r *sbxRequest) output(s string) {
	r.write(bridge.StreamFrame{Type: bridge.StreamOutput, Data: []byte(s)})
}

func (r *sbxRequest) exit(code int) {
	r.write(bridge.StreamFrame{Type: bridge.StreamExit, Code: code})
}

// readUntil reads client frames until pred accepts one, returning every frame
// read so far.
func (r *sbxRequest) readUntil(pred func(bridge.StreamFrame) bool) []bridge.StreamFrame {
	r.t.Helper()
	for {
		_ = r.conn.SetReadDeadline(time.Now().Add(sbxWait))
		line, err := r.r.ReadBytes('\n')
		if err != nil {
			r.t.Fatalf("waiting for a client frame: %v (frames so far %+v)", err, r.got)
		}
		var f bridge.StreamFrame
		if json.Unmarshal(line, &f) != nil {
			continue
		}
		r.got = append(r.got, f)
		if pred(f) {
			return r.got
		}
	}
}

// waitReady returns once the client has entered raw mode: it sends its size
// only after doing so.
func (r *sbxRequest) waitReady() {
	r.t.Helper()
	r.readUntil(func(f bridge.StreamFrame) bool { return f.Type == bridge.StreamResize })
}

// waitEOF asserts the client closes the stream, which is what tells relay to
// end the session.
func (r *sbxRequest) waitEOF() {
	r.t.Helper()
	for {
		_ = r.conn.SetReadDeadline(time.Now().Add(sbxWait))
		if _, err := r.r.ReadBytes('\n'); err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				r.t.Fatal("the client never closed the stream")
			}
			return
		}
	}
}

// --- the client process ------------------------------------------------------

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

type sbxRun struct {
	home       string
	args       []string
	env        []string
	dir        string
	stdinPipe  bool
	stdoutPipe bool
	mode       string
	out        string
	// ptyReadDelay holds each terminal read back before it reaches ptyOut.
	// It sits after the read on purpose. Measured on macOS: while the client
	// is session leader on this pty (Setsid/Setctty in startSandboxClient),
	// its exit does not finish until the master has read its output, so a
	// delay before the read cannot make the client exit first.
	ptyReadDelay time.Duration
}

type sbxProc struct {
	t             *testing.T
	cmd           *exec.Cmd
	master, slave *os.File
	stderr        *lockedBuf
	ptyOut        *lockedBuf
	pipeOut       *lockedBuf
	exited        chan struct{}
	termBefore    unix.Termios
	ptyReadDelay  time.Duration
}

func termiosOf(t *testing.T, f *os.File) unix.Termios {
	t.Helper()
	tm, err := unix.IoctlGetTermios(int(f.Fd()), unix.TIOCGETA)
	if err != nil {
		t.Fatalf("tcgetattr: %v", err)
	}
	return *tm
}

func startSandboxClient(t *testing.T, run sbxRun) *sbxProc {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	if err := pty.Setsize(master, &pty.Winsize{Rows: 33, Cols: 90}); err != nil {
		t.Fatalf("Setsize: %v", err)
	}
	args, _ := json.Marshal(run.args)
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperSandboxClient$")
	var env []string
	for _, e := range os.Environ() {
		// A test run from inside a relay session must not make the client
		// believe it is in one.
		if strings.HasPrefix(e, "RELAY_SESSION_ID=") {
			continue
		}
		env = append(env, e)
	}
	env = append(env, sbxHelperEnv+"=1", sbxHomeEnv+"="+run.home, sbxArgsEnv+"="+string(args), sbxModeEnv+"="+run.mode, sbxOutEnv+"="+run.out)
	cmd.Env = append(env, run.env...)
	cmd.Dir = run.dir

	p := &sbxProc{t: t, cmd: cmd, master: master, slave: slave, stderr: &lockedBuf{}, ptyOut: &lockedBuf{}, exited: make(chan struct{}), ptyReadDelay: run.ptyReadDelay}
	cmd.Stderr = p.stderr
	if run.stdinPipe {
		cmd.Stdin = strings.NewReader("")
	} else {
		cmd.Stdin = slave
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	}
	if run.stdoutPipe {
		p.pipeOut = &lockedBuf{}
		cmd.Stdout = p.pipeOut
	} else {
		cmd.Stdout = slave
	}
	p.termBefore = termiosOf(t, master)

	go p.readMaster()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start client: %v", err)
	}
	go func() { _ = cmd.Wait(); close(p.exited) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-p.exited
		_ = master.Close()
		_ = slave.Close()
	})
	return p
}

// wait returns the client's exit code, or -1 if a signal killed it.
func (p *sbxProc) wait() int {
	p.t.Helper()
	select {
	case <-p.exited:
		return p.cmd.ProcessState.ExitCode()
	case <-time.After(sbxWait):
		p.t.Fatalf("the client did not exit; stderr: %q", p.stderr.String())
		return 0
	}
}

func (p *sbxProc) readMaster() {
	buf := make([]byte, 4096)
	for {
		n, err := p.master.Read(buf)
		time.Sleep(p.ptyReadDelay)
		if n > 0 {
			_, _ = p.ptyOut.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// waitPtyOutput is needed even after wait: cmd.Wait joins exec's own copy
// goroutines (stderr, pipeOut) but not readMaster, which fills ptyOut.
func (p *sbxProc) waitPtyOutput(want string) bool {
	deadline := time.Now().Add(sbxWait)
	for !strings.Contains(p.ptyOut.String(), want) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

func (p *sbxProc) requireTerminalRestored() {
	p.t.Helper()
	now := termiosOf(p.t, p.master)
	if now.Lflag != p.termBefore.Lflag || now.Iflag != p.termBefore.Iflag || now.Oflag != p.termBefore.Oflag {
		p.t.Fatalf("terminal left modified: lflag %x->%x iflag %x->%x oflag %x->%x",
			p.termBefore.Lflag, now.Lflag, p.termBefore.Iflag, now.Iflag, p.termBefore.Oflag, now.Oflag)
	}
}

func (p *sbxProc) requireRawNow() {
	p.t.Helper()
	now := termiosOf(p.t, p.master)
	if now.Lflag&(unix.ICANON|unix.ECHO|unix.ISIG) != 0 {
		p.t.Fatalf("terminal is not in raw mode while attached: lflag %x", now.Lflag)
	}
}

type sbxFixture struct {
	home         string
	rel          *sbxBridge
	cwd          string
	ptyReadDelay time.Duration
}

func newSbxFixture(t *testing.T) *sbxFixture {
	t.Helper()
	home := mkEmptySandboxRelayHome(t)
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &sbxFixture{home: home, rel: startFakeRelay(t, home), cwd: real}
}

func (f *sbxFixture) run(t *testing.T, args ...string) *sbxProc {
	return startSandboxClient(t, sbxRun{home: f.home, args: args, dir: f.cwd, ptyReadDelay: f.ptyReadDelay})
}

// --- 1: the request -----------------------------------------------------------

func TestSandboxClient_AsksForTheTemplateInTheRealWorkingDirectoryAtTheTerminalsSize(t *testing.T) {
	f := newSbxFixture(t)
	link := filepath.Join(mkShortTempDir(t, "sbxcwd-"), "via-link")
	if err := os.Symlink(f.cwd, link); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"claude-code"},
		{"claude-code", "--project", "Main"},
		{"--project", "Main", "claude-code"},
	} {
		p := startSandboxClient(t, sbxRun{home: f.home, args: args, dir: link})
		req := f.rel.expectRequest()
		if req.Req.Type != bridge.ReqSandboxAttach {
			t.Errorf("%v: request type %q, want %q", args, req.Req.Type, bridge.ReqSandboxAttach)
		}
		if req.Args.Template != "claude-code" || req.Args.Cwd != f.cwd {
			t.Errorf("%v: request %+v, want template claude-code in the symlink-resolved %s", args, req.Args, f.cwd)
		}
		wantProject := ""
		if len(args) > 1 {
			wantProject = "Main"
		}
		if req.Args.Project != wantProject {
			t.Errorf("%v: project = %q, want %q", args, req.Args.Project, wantProject)
		}
		if req.Args.Cols != 90 || req.Args.Rows != 33 {
			t.Errorf("%v: size = %dx%d, want the terminal's 90x33", args, req.Args.Cols, req.Args.Rows)
		}
		req.ack()
		req.waitReady()
		req.exit(0)
		if code := p.wait(); code != 0 {
			t.Errorf("%v: exit %d, want 0; stderr %q", args, code, p.stderr.String())
		}
	}
}

// --- 8: exit codes and the end of the stream -------------------------------------

func TestSandboxClient_ExitsWithTheToolsExitCode(t *testing.T) {
	for _, code := range []int{0, 1, 2, 42, 130, 255} {
		f := newSbxFixture(t)
		p := f.run(t, "shell")
		req := f.rel.expectRequest()
		req.ack()
		req.waitReady()
		req.output("hi\r\n")
		req.exit(code)
		if got := p.wait(); got != code {
			t.Errorf("tool exit %d: client exited %d", code, got)
		}
		if !p.waitPtyOutput("hi") {
			t.Errorf("tool exit %d: terminal saw %q, want the output", code, p.ptyOut.String())
		}
		p.requireTerminalRestored()
	}
}

func TestSandboxClient_ExitsWithTheToolsExitCode_SlowTerminalReader(t *testing.T) {
	for _, code := range []int{0, 1, 2, 42, 130, 255} {
		f := newSbxFixture(t)
		f.ptyReadDelay = 200 * time.Millisecond
		p := f.run(t, "shell")
		req := f.rel.expectRequest()
		req.ack()
		req.waitReady()
		req.output("hi\r\n")
		req.exit(code)
		if got := p.wait(); got != code {
			t.Errorf("tool exit %d: client exited %d", code, got)
		}
		if !p.waitPtyOutput("hi") {
			t.Errorf("tool exit %d: terminal saw %q, want the output", code, p.ptyOut.String())
		}
		p.requireTerminalRestored()
	}
}

func TestSandboxClient_ExitStatusOfASignalledShimIsANonZeroFailure(t *testing.T) {
	f := newSbxFixture(t)
	p := f.run(t, "shell")
	req := f.rel.expectRequest()
	req.ack()
	req.waitReady()
	req.exit(-1)
	if code := p.wait(); code == 0 {
		t.Fatal("a session whose shim was signalled exited 0")
	}
}

func TestSandboxClient_StreamEndingWithoutAnExitStatusExitsOneWithAMessage(t *testing.T) {
	f := newSbxFixture(t)
	p := f.run(t, "shell")
	req := f.rel.expectRequest()
	req.ack()
	req.waitReady()
	req.output("partial")
	_ = req.conn.Close()

	if code := p.wait(); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(p.stderr.String(), "the session ended (connection dropped)") {
		t.Fatalf("stderr = %q, want a message naming the connection as dropped", p.stderr.String())
	}
	p.requireTerminalRestored()
}

// --- 10: terminal mode ------------------------------------------------------------

func TestSandboxClient_TerminalIsRawWhileAttachedAndRestoredOnNormalExit(t *testing.T) {
	f := newSbxFixture(t)
	p := f.run(t, "shell")
	req := f.rel.expectRequest()
	req.ack()
	req.waitReady()
	p.requireRawNow()
	req.exit(0)
	p.wait()
	p.requireTerminalRestored()
}

func TestSandboxClient_SignalEndsTheStreamAndRestoresTheTerminal(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			f := newSbxFixture(t)
			p := f.run(t, "shell")
			req := f.rel.expectRequest()
			req.ack()
			req.waitReady()
			p.requireRawNow()

			_ = p.cmd.Process.Signal(sig)
			req.waitEOF()
			if code := p.wait(); code == 0 {
				t.Fatal("client exited 0 after being signalled")
			}
			p.requireTerminalRestored()
		})
	}
}

func TestSandboxClient_TerminalIsNeverPutInRawModeWhenRelayRefuses(t *testing.T) {
	f := newSbxFixture(t)
	p := f.run(t, "shell")
	req := f.rel.expectRequest()
	req.refuse(&bridge.SandboxRefusal{Reason: bridge.SandboxReasonUnknownTemplate, Message: "there is no terminal template \"shell\""})
	if code := p.wait(); code == 0 {
		t.Fatal("exit 0 on a refusal")
	}
	p.requireTerminalRestored()
}

// --- 6/10: what the client does with the bytes ---------------------------------

func TestSandboxClient_ForwardsTypedBytesAndResizes(t *testing.T) {
	f := newSbxFixture(t)
	p := f.run(t, "shell")
	req := f.rel.expectRequest()
	req.ack()
	req.waitReady()

	if _, err := p.master.Write([]byte("ls\x03\r")); err != nil {
		t.Fatal(err)
	}
	var typed []byte
	req.readUntil(func(f bridge.StreamFrame) bool {
		if f.Type == bridge.StreamInput {
			typed = append(typed, f.Data...)
		}
		return len(typed) >= 4
	})
	if string(typed) != "ls\x03\r" {
		t.Fatalf("relay received %q, want the raw bytes (Ctrl-C is an ordinary byte) %q", typed, "ls\x03\r")
	}

	if err := pty.Setsize(p.master, &pty.Winsize{Rows: 50, Cols: 120}); err != nil {
		t.Fatal(err)
	}
	var resize bridge.StreamFrame
	req.readUntil(func(f bridge.StreamFrame) bool {
		resize = f
		return f.Type == bridge.StreamResize && f.Cols == 120 && f.Rows == 50
	})
	if resize.Cols != 120 || resize.Rows != 50 {
		t.Fatalf("resize frame = %+v, want 120x50", resize)
	}
	req.exit(0)
	p.wait()
}

// --- 9: no deadline anywhere on the client's attach path ---------------------------

func TestSandboxClient_AttachedConnectionIsNeverGivenADeadline(t *testing.T) {
	f := newSbxFixture(t)
	out := filepath.Join(f.home, "deadline-count")
	p := startSandboxClient(t, sbxRun{home: f.home, dir: f.cwd, mode: "attach-recording", out: out})
	// attachTerminal sends no request line: its first frame (the terminal's
	// size, sent once raw mode is on) is what the fake relay registers.
	c := f.rel.expectRequest()
	c.output("one")
	c.output("two")
	c.exit(0)
	if code := p.wait(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read count: %v", err)
	}
	if strings.TrimSpace(string(b)) != "0" {
		t.Fatalf("attachTerminal applied %s deadline call(s) to its connection, want 0", b)
	}
}

// --- 3: preflight refusals ----------------------------------------------------------

func TestSandboxClient_PreflightRefusalsSendNothing(t *testing.T) {
	cases := []struct {
		name string
		run  func(f *sbxFixture) sbxRun
		want string
	}{
		{"inside a relay session", func(f *sbxFixture) sbxRun {
			return sbxRun{home: f.home, args: []string{"shell"}, dir: f.cwd, env: []string{"RELAY_SESSION_ID=sess-1"}}
		}, "session"},
		{"stdin is not a terminal", func(f *sbxFixture) sbxRun {
			return sbxRun{home: f.home, args: []string{"shell"}, dir: f.cwd, stdinPipe: true}
		}, "terminal"},
		{"stdout is not a terminal", func(f *sbxFixture) sbxRun {
			return sbxRun{home: f.home, args: []string{"shell"}, dir: f.cwd, stdoutPipe: true}
		}, "terminal"},
		{"no template named", func(f *sbxFixture) sbxRun {
			return sbxRun{home: f.home, args: nil, dir: f.cwd}
		}, "template"},
		{"empty template", func(f *sbxFixture) sbxRun {
			return sbxRun{home: f.home, args: []string{""}, dir: f.cwd}
		}, "template"},
		{"two templates", func(f *sbxFixture) sbxRun {
			return sbxRun{home: f.home, args: []string{"shell", "pi"}, dir: f.cwd}
		}, ""},
		{"unknown flag", func(f *sbxFixture) sbxRun {
			return sbxRun{home: f.home, args: []string{"shell", "--frobnicate"}, dir: f.cwd}
		}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &sbxFixture{home: mkEmptySandboxRelayHome(t), cwd: realDir(t, t.TempDir())}
			ln := listenIdle(t, f.home)
			p := startSandboxClient(t, c.run(f))
			if code := p.wait(); code == 0 {
				t.Fatal("exit 0 on a refused invocation")
			}
			for i, b := range p.bytesSentTo(ln) {
				if len(b) != 0 {
					t.Fatalf("connection %d carried %q; a refused invocation must send relay nothing", i, b)
				}
			}
			msg := strings.ToLower(p.stderr.String())
			if strings.TrimSpace(msg) == "" {
				t.Fatal("refused without a message")
			}
			if !strings.Contains(msg, c.want) {
				t.Errorf("stderr %q does not mention %q", msg, c.want)
			}
			p.requireTerminalRestored()
		})
	}
}

func TestSandboxClient_RelayNotRunningIsAPlainRefusal(t *testing.T) {
	home := mkEmptySandboxRelayHome(t) // nobody listens on its bridge socket
	cwd := realDir(t, t.TempDir())
	p := startSandboxClient(t, sbxRun{home: home, args: []string{"shell"}, dir: cwd})
	if code := p.wait(); code == 0 {
		t.Fatal("exit 0 with relay not running")
	}
	if msg := strings.ToLower(p.stderr.String()); !strings.Contains(msg, "not running") {
		t.Fatalf("stderr %q does not say relay is not running", msg)
	}
	p.requireTerminalRestored()
}

// --- 3/4: what the client shows of relay's refusals -------------------------------------

func TestSandboxClient_ShowsRelaysRefusalAndExitsNonZero(t *testing.T) {
	cases := []struct {
		name string
		ref  bridge.SandboxRefusal
	}{
		{"not inside a project", bridge.SandboxRefusal{Reason: bridge.SandboxReasonNoProject, Message: "/x is not inside any registered relay project"}},
		{"unknown template", bridge.SandboxRefusal{Reason: bridge.SandboxReasonUnknownTemplate, Message: "there is no terminal template \"zzz\""}},
		{"template not allowed", bridge.SandboxRefusal{Reason: bridge.SandboxReasonTemplateDenied, Message: "terminal template \"pi\" is not allowed for project \"Main\""}},
		{"ambiguous project", bridge.SandboxRefusal{Reason: bridge.SandboxReasonProjectAmbiguous, Projects: []string{"Alpha (a)", "Beta (b)"}, Message: "inside more than one project (Alpha (a), Beta (b)); choose one with --project"}},
		{"inside a session", bridge.SandboxRefusal{Reason: bridge.SandboxReasonInsideSession, Message: "cannot be run from inside a relay session"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newSbxFixture(t)
			p := f.run(t, "shell")
			req := f.rel.expectRequest()
			ref := c.ref
			req.refuse(&ref)
			if code := p.wait(); code == 0 {
				t.Fatal("exit 0 on a refusal")
			}
			if !strings.Contains(p.stderr.String(), c.ref.Message) {
				t.Fatalf("stderr %q does not show relay's message %q", p.stderr.String(), c.ref.Message)
			}
		})
	}
}

func TestSandboxClient_ShowsAnErrorResponseThatCarriesNoStructuredRefusal(t *testing.T) {
	f := newSbxFixture(t)
	p := f.run(t, "shell")
	req := f.rel.expectRequest()
	req.write(bridge.BridgeResponse{Type: bridge.RespError, Code: -32603, Message: "sandbox: could not start the session"})
	if code := p.wait(); code == 0 {
		t.Fatal("exit 0 on an error")
	}
	if !strings.Contains(p.stderr.String(), "could not start the session") {
		t.Fatalf("stderr %q hides relay's message", p.stderr.String())
	}
}

func TestSandboxClient_RelayHangingUpBeforeAnsweringIsAFailureWithAMessage(t *testing.T) {
	f := newSbxFixture(t)
	p := f.run(t, "shell")
	req := f.rel.expectRequest()
	_ = req.conn.Close()
	if code := p.wait(); code == 0 {
		t.Fatal("exit 0")
	}
	if strings.TrimSpace(p.stderr.String()) == "" {
		t.Fatal("no message")
	}
	p.requireTerminalRestored()
}

func TestSandboxClient_BytesSentToSeesARequestFromAClientThatExited(t *testing.T) {
	home := mkEmptySandboxRelayHome(t)
	ln := listenIdle(t, home)
	p := startSandboxClient(t, sbxRun{home: home, dir: realDir(t, t.TempDir()), mode: "send-then-exit"})
	if code := p.wait(); code != 1 {
		t.Fatalf("send-then-exit exited %d, want 1; stderr: %q", code, p.stderr.String())
	}
	sent := p.bytesSentTo(ln)
	if len(sent) != 1 {
		t.Fatalf("saw %d connection(s), want 1: %q", len(sent), sent)
	}
	var req bridge.BridgeRequest
	if err := json.Unmarshal(sent[0], &req); err != nil || req.Type != bridge.ReqSandboxAttach {
		t.Fatalf("the connection carried %q, want one %s request line", sent[0], bridge.ReqSandboxAttach)
	}
}
