package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/runner"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
	"github.com/barelyworkingcode/relay/internal/sshhost"
)

// runConn is one run directory, over ssh or on disk. The host and local Claude
// paths use it now; codex and pi use it when they move under the runner.
type runConn interface {
	Status(ctx context.Context) (runner.Status, error)
	Send(ctx context.Context, kind runner.MessageKind, line []byte) error
	SendEvent(ctx context.Context, typ string) error // a host answers nil: its asks come from stdout
	Read(ctx context.Context, offset int64, follow bool) (io.ReadCloser, error)
	Probe(ctx context.Context) (attachProbe, error)
	Stop(ctx context.Context) error
	Remove(ctx context.Context) error
	StderrTail(ctx context.Context) (string, error)
}

// Send outcomes a runConn reports besides the runner's own errors.
var (
	errSendUnknown = errors.New("the connection ended during the send")
	errNoConnect   = errors.New("ssh never reached the host")
)

// sshConn is a run directory on a host.
type sshConn struct {
	host *sessionstypes.HostSpec
	dir  sshhost.RunDir
}

func (c sshConn) Status(ctx context.Context) (runner.Status, error) {
	stdout, msg, err := runHost(ctx, c.host, sshhost.RunnerStatus(c.host.OS, c.dir), nil)
	if err != nil {
		return runner.Status{}, errors.New(msg)
	}
	st, err := sshhost.ParseRunState(stdout)
	if err != nil {
		return runner.Status{}, err
	}
	return runner.Status{Phase: runner.Phase(st.Phase), Generation: st.Generation, Code: st.Code}, nil
}

func (c sshConn) Send(ctx context.Context, kind runner.MessageKind, line []byte) error {
	// Deliberate: the write itself does not follow the caller's ctx. A line
	// already on its way cannot be recalled, so it is left to finish.
	ctx, cancel := context.WithTimeout(ctx, detachedSendTimeout)
	defer cancel()
	id := runner.NewMessageID(time.Now())
	_, msg, err := runHost(ctx, c.host, sshhost.RunnerSend(c.host.OS, c.dir, id, kind), bytes.NewReader(line))
	cut := ctx.Err() != nil
	switch {
	case err == nil:
		return nil
	case sshConnectFailure(msg):
		return errNoConnect
	case cut || sendOutcomeUnknown(err):
		return errSendUnknown
	case exitCodeOf(err) == runnerNotRunning:
		return runner.ErrNotRunning
	case exitCodeOf(err) == runnerNoReceipt:
		return runner.ErrNoReceipt
	}
	return errors.New(msg)
}

func (sshConn) SendEvent(context.Context, string) error { return nil }

func (c sshConn) Read(ctx context.Context, offset int64, follow bool) (io.ReadCloser, error) {
	remote := sshhost.RunnerReadOnce(c.host.OS, c.dir, offset)
	if follow {
		remote = sshhost.RunnerRead(c.host.OS, c.dir, offset)
	}
	cmd := hostExec(ctx, c.host, remote)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &cmdReader{ReadCloser: stdout, cmd: cmd}, nil
}

// cmdReader reads a command's stdout. Close ends the command; it returns the
// command's own error only when the output ended on its own.
type cmdReader struct {
	io.ReadCloser
	cmd    *exec.Cmd
	eof    atomic.Bool
	closed atomic.Bool
}

func (r *cmdReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err == io.EOF {
		r.eof.Store(true)
	}
	return n, err
}

func (r *cmdReader) Close() error {
	if !r.closed.CompareAndSwap(false, true) {
		return nil
	}
	if !r.eof.Load() {
		_ = r.cmd.Process.Kill()
	}
	werr := r.cmd.Wait()
	if r.eof.Load() {
		return werr
	}
	return nil
}

func (c sshConn) Probe(ctx context.Context) (attachProbe, error) {
	return probeAttach(ctx, c.host, c.dir)
}

func (c sshConn) Stop(ctx context.Context) error {
	_, msg, err := runHost(ctx, c.host, sshhost.RunnerStop(c.host.OS, c.dir), nil)
	if err != nil {
		return errors.New(msg)
	}
	return nil
}

func (c sshConn) Remove(ctx context.Context) error {
	_, msg, err := runHost(ctx, c.host, sshhost.RunnerRemove(c.host.OS, c.dir.Session), nil)
	if err != nil {
		return errors.New(msg)
	}
	if c.host.TmuxPath != "" {
		if _, msg, err := runHost(ctx, c.host, sshhost.KillAgentPane(c.host.OS, c.host.TmuxPath, sshhost.AgentPaneName(c.dir.Session)), nil); err != nil {
			return errors.New(msg)
		}
	}
	return nil
}

func (c sshConn) StderrTail(ctx context.Context) (string, error) {
	out, _, err := runHost(ctx, c.host, sshhost.RunnerStderrTail(c.host.OS, c.dir), nil)
	return out, err
}

// localConn is a run directory on this machine.
type localConn struct{ run runner.LocalRun }

const followPoll = 50 * time.Millisecond

func (c localConn) Status(context.Context) (runner.Status, error) {
	return runner.LocalStatus(c.run.Base, c.run.Session, c.run.Generation)
}

func (c localConn) Send(ctx context.Context, kind runner.MessageKind, line []byte) error {
	return c.run.Send(ctx, kind, line)
}

func (c localConn) SendEvent(ctx context.Context, typ string) error {
	return c.run.SendEvent(ctx, typ)
}

func (c localConn) Read(ctx context.Context, offset int64, follow bool) (io.ReadCloser, error) {
	f, err := c.run.Open(offset)
	if err != nil || !follow {
		return f, err
	}
	return &followReader{ctx: ctx, f: f, run: c.run}, nil
}

// followReader reads out.jsonl as it grows and ends once the runner is gone
// and the file is drained. Deliberate: a growing file has no wake-up, so it
// polls.
type followReader struct {
	ctx   context.Context
	f     *os.File
	run   runner.LocalRun
	polls int
}

func (r *followReader) Read(p []byte) (int, error) {
	for {
		n, err := r.f.Read(p)
		if n > 0 || err != io.EOF {
			return n, err
		}
		if r.gone() {
			if n, err = r.f.Read(p); n > 0 || err != io.EOF {
				return n, err
			}
			return 0, io.EOF
		}
		select {
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		case <-time.After(followPoll):
		}
	}
}

func (r *followReader) gone() bool {
	r.polls++
	if _, err := os.Stat(filepath.Join(r.run.Dir(), runner.ExitFile)); err == nil {
		return true
	}
	if r.polls%20 != 0 {
		return false
	}
	st, err := runner.LocalStatus(r.run.Base, r.run.Session, r.run.Generation)
	return err == nil && st.Phase != runner.PhaseAlive
}

func (r *followReader) Close() error { return r.f.Close() }

// Probe reads what attachProbeScript prints for a host, from the files.
func (c localConn) Probe(context.Context) (attachProbe, error) {
	dir := c.run.Dir()
	f, err := os.Open(filepath.Join(dir, runner.OutFile))
	if err != nil {
		return attachProbe{}, fmt.Errorf("no agent output in %s", dir)
	}
	defer f.Close()
	var pr attachProbe
	br := bufio.NewReaderSize(f, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			break
		}
		pr.size += int64(len(line))
		if bytes.Contains(line, []byte(`"type":"result"`)) {
			pr.results++
		}
		if body := bytes.TrimRight(line, "\r\n"); bytes.Contains(body, []byte(`"control_request"`)) && lineType(string(body)) == "control_request" {
			pr.requests = append(pr.requests, json.RawMessage(body))
		}
	}
	for _, d := range []string{runner.SentDir, runner.InboxDir} {
		turns, _ := filepath.Glob(filepath.Join(dir, d, "*.json"))
		pr.users += len(turns)
		controls, _ := filepath.Glob(filepath.Join(dir, d, "*.ctl"))
		for _, name := range controls {
			data, err := os.ReadFile(name)
			if err != nil {
				continue
			}
			for _, l := range strings.Split(string(data), "\n") {
				if lineType(l) == "control_response" {
					if id := controlRequestID(json.RawMessage(l)); id != "" {
						pr.answered = append(pr.answered, id)
					}
				}
			}
		}
	}
	return pr, nil
}

func (c localConn) Stop(ctx context.Context) error { return c.run.Stop(ctx) }

func (c localConn) Remove(context.Context) error {
	return runner.RemoveLocal(c.run.Base, c.run.Session)
}

func (c localConn) StderrTail(context.Context) (string, error) {
	f, err := os.Open(filepath.Join(c.run.Dir(), runner.StderrFile))
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	const tail = 4096
	off := max(st.Size()-tail, 0)
	buf := make([]byte, st.Size()-off)
	n, err := f.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		return "", err
	}
	return string(buf[:n]), nil
}
