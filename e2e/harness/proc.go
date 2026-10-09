package harness

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// NewTrace returns a fresh trace ID that satisfies relay's [A-Za-z0-9_-]{8,64}.
func NewTrace(t *testing.T) string {
	t.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("reading random bytes for a trace: %v", err)
	}
	return "e2e" + hex.EncodeToString(b)
}

// Result is a finished process.
type Result struct {
	Code           int
	Stdout, Stderr []byte
	Trace          string
}

// JSON decodes stdout into v and fails t if it does not decode.
func (r Result) JSON(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Stdout, v); err != nil {
		t.Fatalf("decoding stdout as JSON: %v\nstdout: %s\nstderr: %s", err, r.Stdout, r.Stderr)
	}
}

// notifyBuffer is a write-only buffer that wakes readers when bytes arrive and
// can tee to a file.
type notifyBuffer struct {
	mu      sync.Mutex
	buf     []byte
	changed chan struct{}
	tee     io.Writer
}

func newNotifyBuffer(tee io.Writer) *notifyBuffer {
	return &notifyBuffer{changed: make(chan struct{}), tee: tee}
}

func (b *notifyBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.buf = append(b.buf, p...)
	old := b.changed
	b.changed = make(chan struct{})
	b.mu.Unlock()
	close(old)
	if b.tee != nil {
		b.tee.Write(p)
	}
	return len(p), nil
}

func (b *notifyBuffer) snapshot() ([]byte, chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf...), b.changed
}

// Proc is a running process whose stdout can be read line by line.
type Proc struct {
	t        *testing.T
	cmd      *exec.Cmd
	trace    string
	out      *notifyBuffer
	errb     *notifyBuffer
	done     chan struct{}
	waitErr  error
	limit    time.Time // zero: no limit
	readOff  int
	onResult func(Result)
}

type procSpec struct {
	bin       string
	args      []string
	env       []string
	dir       string
	stdin     []byte
	deadline  time.Duration
	stdoutTee io.Writer
	stderrTo  *os.File // nil: captured in memory
	trace     string
}

func startProc(t *testing.T, s procSpec) *Proc {
	t.Helper()
	p := &Proc{t: t, trace: s.trace, out: newNotifyBuffer(s.stdoutTee), done: make(chan struct{})}
	cmd := exec.Command(s.bin, s.args...)
	cmd.Env = s.env
	cmd.Dir = s.dir
	cmd.Stdout = p.out
	if s.stderrTo != nil {
		cmd.Stderr = s.stderrTo
	} else {
		p.errb = newNotifyBuffer(nil)
		cmd.Stderr = p.errb
	}
	if s.stdin != nil {
		cmd.Stdin = bytes.NewReader(s.stdin)
	}
	// A grandchild that inherits the pipes must not hold Wait forever.
	cmd.WaitDelay = 5 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", s.bin, err)
	}
	p.cmd = cmd
	if s.deadline > 0 {
		p.limit = time.Now().Add(s.deadline)
	}
	go func() {
		p.waitErr = cmd.Wait()
		close(p.done)
	}()
	return p
}

func (p *Proc) exitCode() int {
	// ProcessState is written by Wait; it is readable only once done is closed.
	select {
	case <-p.done:
		return p.cmd.ProcessState.ExitCode()
	default:
		return -1
	}
}

func (p *Proc) result() Result {
	out, _ := p.out.snapshot()
	var errOut []byte
	if p.errb != nil {
		errOut, _ = p.errb.snapshot()
	}
	return Result{Code: p.exitCode(), Stdout: out, Stderr: errOut, Trace: p.trace}
}

func (p *Proc) limitTimer() (<-chan time.Time, func()) {
	if p.limit.IsZero() {
		return nil, func() {}
	}
	tm := time.NewTimer(time.Until(p.limit))
	return tm.C, func() { tm.Stop() }
}

func (p *Proc) killForDeadline(what string) {
	p.cmd.Process.Kill()
	<-p.done
	r := p.result()
	p.t.Fatalf("%s: process %v passed its deadline and was killed\nstdout: %s\nstderr: %s",
		what, p.cmd.Args, tailString(r.Stdout, 2000), tailString(r.Stderr, 2000))
}

// Wait blocks until the process exits and returns its result. The process is
// killed and t fails at the process's deadline.
func (p *Proc) Wait() Result {
	p.t.Helper()
	lim, stop := p.limitTimer()
	defer stop()
	select {
	case <-p.done:
	case <-lim:
		p.killForDeadline("waiting for exit")
	}
	r := p.result()
	if p.onResult != nil {
		p.onResult(r)
	}
	return r
}

// Signal sends sig to the process.
func (p *Proc) Signal(sig os.Signal) {
	p.t.Helper()
	if err := p.cmd.Process.Signal(sig); err != nil && !strings.Contains(err.Error(), "already finished") {
		p.t.Fatalf("signalling %v: %v", p.cmd.Args, err)
	}
}

// nextLine returns the next complete stdout line. ok is false when the process
// ended and no more lines remain. t fails at the given deadline.
func (p *Proc) nextLine(deadline time.Duration, what string) (line []byte, ok bool) {
	p.t.Helper()
	tm := time.NewTimer(deadline)
	defer tm.Stop()
	lim, stop := p.limitTimer()
	defer stop()
	for {
		data, changed := p.out.snapshot()
		if idx := bytes.IndexByte(data[p.readOff:], '\n'); idx >= 0 {
			line = data[p.readOff : p.readOff+idx]
			p.readOff += idx + 1
			return line, true
		}
		select {
		case <-changed:
		case <-p.done:
			data, _ = p.out.snapshot()
			if p.readOff < len(data) {
				line = data[p.readOff:]
				p.readOff = len(data)
				return line, true
			}
			return nil, false
		case <-tm.C:
			p.t.Fatalf("%s: no stdout line from %v within %v\nstderr: %s", what, p.cmd.Args, deadline, tailString(p.result().Stderr, 2000))
		case <-lim:
			p.killForDeadline(what)
		}
	}
}

// FirstLine returns the first stdout line. t fails at the deadline or if the
// process exits without printing one.
func (p *Proc) FirstLine(deadline time.Duration) []byte {
	p.t.Helper()
	line, ok := p.nextLine(deadline, "waiting for the first stdout line")
	if !ok {
		r := p.result()
		p.t.Fatalf("%v exited %d before printing a line\nstderr: %s", p.cmd.Args, r.Code, tailString(r.Stderr, 2000))
	}
	return line
}

func tailString(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return "..." + string(b[len(b)-n:])
}

func tailLines(b []byte, n int) string {
	s := strings.TrimRight(string(b), "\n")
	if s == "" {
		return "(empty)"
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return fmt.Sprintf("(cut: last %d of %d lines)\n%s", n, len(lines), strings.Join(lines[len(lines)-n:], "\n"))
}
