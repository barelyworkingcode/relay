package harness

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// ioctl requests for a BSD pseudo-terminal master; the names are darwin's.
const (
	tiocPtyGrant = 0x20007454
	tiocPtyUnlk  = 0x20007452
	tiocPtyGName = 0x40807453
)

// ttyDrainWait bounds how long Wait lets the master's reader finish after the
// child exits; a grandchild that kept the slave open would otherwise hold it.
const ttyDrainWait = 5 * time.Second

// TTYProc is the CLI running on a pseudo-terminal: stdin, stdout and stderr
// are the PTY slave, and the child is a session leader with the PTY as its
// controlling terminal.
type TTYProc struct {
	t       *testing.T
	cmd     *exec.Cmd
	trace   string
	master  *os.File
	out     *notifyBuffer
	exited  chan struct{} // the child was reaped
	drained chan struct{} // the master reader returned
	limit   time.Time
	once    sync.Once
}

// StartCLITTY runs the CLI on a pseudo-terminal in dir, for the verbs that
// refuse without a terminal (relay sandbox, relay drop-in). It returns before
// the process exits.
func (i *Instance) StartCLITTY(o CLIOpts, dir string, args ...string) *TTYProc {
	i.t.Helper()
	full, trace := i.cliArgs(o, args)
	master, slave := openPTY(i.t)

	cmd := exec.Command(bundle.Relay, full...)
	cmd.Env = i.env
	cmd.Dir = dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		_ = master.Close()
		_ = slave.Close()
		i.t.Fatalf("starting %s on a pseudo-terminal: %v", bundle.Relay, err)
	}
	// The parent's slave must close, or the master never sees the hang-up.
	_ = slave.Close()

	p := &TTYProc{
		t: i.t, cmd: cmd, trace: trace, master: master,
		out: newNotifyBuffer(nil), exited: make(chan struct{}), drained: make(chan struct{}),
		limit: time.Now().Add(o.deadline()),
	}
	go func() {
		_ = cmd.Wait()
		close(p.exited)
	}()
	go func() {
		defer close(p.drained)
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				_, _ = p.out.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	i.t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-p.exited
		p.closeMaster()
	})
	return p
}

// Write sends bytes to the child's terminal input.
func (p *TTYProc) Write(b []byte) {
	p.t.Helper()
	if _, err := p.master.Write(b); err != nil {
		p.t.Fatalf("writing to the pseudo-terminal of %v: %v", p.cmd.Args, err)
	}
}

// Hangup closes the master: the child, as session leader, gets SIGHUP.
func (p *TTYProc) Hangup() { p.closeMaster() }

func (p *TTYProc) closeMaster() { p.once.Do(func() { _ = p.master.Close() }) }

// Wait blocks until the process exits and returns what the PTY carried in
// Stdout. The process is killed and t fails at the deadline of the CLIOpts it
// started with.
func (p *TTYProc) Wait() Result {
	p.t.Helper()
	tm := time.NewTimer(time.Until(p.limit))
	defer tm.Stop()
	select {
	case <-p.exited:
	case <-tm.C:
		_ = p.cmd.Process.Kill()
		<-p.exited
		out, _ := p.out.snapshot()
		p.t.Fatalf("waiting for exit: process %v passed its deadline and was killed\noutput: %s", p.cmd.Args, tailString(out, 2000))
	}
	drain := time.NewTimer(ttyDrainWait)
	defer drain.Stop()
	select {
	case <-p.drained:
	case <-drain.C:
		p.closeMaster()
		<-p.drained
	}
	out, _ := p.out.snapshot()
	return Result{Code: p.cmd.ProcessState.ExitCode(), Stdout: out, Trace: p.trace}
}

// openPTY returns a master and slave pair. The master is non-blocking so the
// runtime's poller owns it: closing it then ends a Read in flight. (Calling
// Fd on an os.File would put it back in blocking mode, where Close waits for
// the Read.) The slave is inherited by the child as fd 0, 1 and 2.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	fd, err := syscall.Open("/dev/ptmx", syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("opening /dev/ptmx: %v", err)
	}
	fail := func(what string, err error) {
		_ = syscall.Close(fd)
		t.Fatalf("%s: %v", what, err)
	}
	if err := ptyIoctl(fd, tiocPtyGrant, 0); err != nil {
		fail("TIOCPTYGRANT", err)
	}
	if err := ptyIoctl(fd, tiocPtyUnlk, 0); err != nil {
		fail("TIOCPTYUNLK", err)
	}
	var name [128]byte
	if err := ptyIoctl(fd, tiocPtyGName, uintptr(unsafe.Pointer(&name[0]))); err != nil {
		fail("TIOCPTYGNAME", err)
	}
	n := 0
	for n < len(name) && name[n] != 0 {
		n++
	}
	s, err := os.OpenFile(string(name[:n]), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		fail(fmt.Sprintf("opening the pseudo-terminal slave %q", name[:n]), err)
	}
	if err := syscall.SetNonblock(fd, true); err != nil {
		_ = s.Close()
		fail("setting the master non-blocking", err)
	}
	return os.NewFile(uintptr(fd), "ptmx"), s
}

func ptyIoctl(fd int, req, arg uintptr) error {
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, arg)
	if e != 0 {
		return fmt.Errorf("ioctl %#x: %w", req, e)
	}
	return nil
}
