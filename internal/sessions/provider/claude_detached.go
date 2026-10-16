package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/barelyworkingcode/relay/internal/logging"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
	"github.com/barelyworkingcode/relay/internal/sshhost"
)

// A detached Claude session keeps its agent on the host (docs/ssh-hosts.md,
// "Detached sessions"). Relay's ssh link carries input to in.jsonl and reads
// out.jsonl by byte offset, so a dropped link ends neither the agent nor the
// events it writes while relay is away.

// Backoff bounds for reopening the link; vars so a test can shorten them.
var (
	detachedBackoffMin = time.Second
	detachedBackoffMax = 10 * time.Second
)

const (
	detachedStartTimeout = 60 * time.Second
	detachedSendTimeout  = 30 * time.Second
	detachedStopTimeout  = 30 * time.Second
	detachedStopTries    = 3
	detachedEventsMax    = 200
	detachedEventsWait   = 30 * time.Second
	// detachedQuickFail is how long a read must run, delivering nothing, to
	// count as a dropped link rather than a read that failed at once.
	detachedQuickFail = 2 * time.Second
)

type detachedRun struct {
	spec   *sessionstypes.HostSpec
	dir    sshhost.RunDir
	ctx    context.Context
	cancel context.CancelFunc

	// offset is the byte offset of the next line start to consume. Only whole
	// lines advance it, which is what makes a reopened read deliver each event
	// once and in order.
	offset   atomic.Int64
	linkLost atomic.Bool
	// finished is set once the runner is known to be gone, so Kill has nothing
	// to stop.
	finished atomic.Bool

	stopOnce sync.Once
	// stopped closes when the host stop has returned; the final process_exited
	// of a killed spawn waits on it so the exit follows the real end.
	stopped chan struct{}
	done    chan struct{}
}

func hostExec(ctx context.Context, spec *sessionstypes.HostSpec, remote string) *exec.Cmd {
	argv := append(slices.Clone(spec.SSHArgv[1:]), "-T", "--", remote)
	cmd := exec.CommandContext(ctx, spec.SSHArgv[0], argv...)
	cmd.Env = childBaseEnv()
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

// runHost runs one remote command to completion and returns its stdout and
// the first line of its stderr.
func runHost(ctx context.Context, spec *sessionstypes.HostSpec, remote string, stdin io.Reader) (string, string, error) {
	cmd := hostExec(ctx, spec, remote)
	var out, errb bytes.Buffer
	cmd.Stdin = stdin
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	msg := strings.TrimSpace(errb.String())
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	if err != nil && msg == "" {
		msg = err.Error()
	}
	return out.String(), msg, err
}

func (p *ClaudeProvider) startDetached(host *sessionstypes.HostSpec, args []string) error {
	if sshhost.IsWindowsOS(host.OS) {
		return fmt.Errorf("host %q is a Windows host: detach_sessions is not available there", host.Name)
	}
	if len(host.SSHArgv) == 0 {
		return fmt.Errorf("host %q has no ssh_argv", host.Name)
	}
	program := p.cfg.Launcher.Program(host.ClaudePath)
	env := map[string]string{"RELAY_SESSION_ID": p.session.ID}
	remote := sshhost.RunnerStartIn(host.OS, p.directory, sshhost.RunDir{Session: p.session.ID}, append(slices.Clone(program), args...), env)

	ctx, cancel := context.WithTimeout(context.Background(), detachedStartTimeout)
	defer cancel()
	stdout, msg, err := runHost(ctx, host, remote, nil)
	if err != nil {
		return fmt.Errorf("start detached claude on host %q: %s", host.Name, msg)
	}
	gen, err := sshhost.ParseRunStart(stdout)
	if err != nil {
		return fmt.Errorf("start detached claude on host %q: %w", host.Name, err)
	}

	runCtx, runCancel := context.WithCancel(context.Background())
	det := &detachedRun{
		spec:    host,
		dir:     sshhost.RunDir{Session: p.session.ID, Generation: gen},
		ctx:     runCtx,
		cancel:  runCancel,
		stopped: make(chan struct{}),
		done:    make(chan struct{}),
	}

	p.cmd, p.stdin, p.targetPID = nil, nil, 0
	p.det = det
	p.exitMu.Lock()
	p.spawnGen++
	spawnGen := p.spawnGen
	p.exitMu.Unlock()
	p.alive.Store(true)
	p.stopIdle = make(chan struct{})
	p.stopIdleOnce = sync.Once{}
	p.waitDone = make(chan struct{})
	p.drainTimeout = providerDrainTimeout
	p.killed = &atomic.Bool{}
	p.exit = &spawnExit{done: make(chan struct{})}
	p.touchActivity()

	spawn := &claudeSpawnState{}
	go p.followDetached(det, spawn, spawnGen, p.killed, p.exit)
	go p.idleWatcher(p.stopIdle)

	slog.Info("claude detached agent started", "session", p.session.ID, "host", host.ID, "generation", gen)
	return nil
}

// followDetached owns the spawn from start to end: it reads the agent's events,
// rides out link drops, and delivers process_exited the way waitForExit does.
func (p *ClaudeProvider) followDetached(det *detachedRun, spawn *claudeSpawnState, gen uint64, killed *atomic.Bool, exit *spawnExit) {
	defer close(det.done)
	defer close(exit.done)

	code, tail := p.pumpDetached(det, spawn)
	p.alive.Store(false)
	exit.code = code
	if killed.Load() {
		<-det.stopped
	}

	p.exitMu.Lock()
	defer p.exitMu.Unlock()
	if p.spawnGen != gen {
		slog.Debug("provider exit from a superseded spawn dropped", "session", p.session.ID, "kind", "claude", "exitCode", code)
		return
	}
	slog.Info("claude process exited", "session", p.session.ID, "exitCode", code)
	wasKilled := killed.Load()
	if !wasKilled {
		warnProviderExit(p.session.ID, "claude", code, tail)
	}
	p.handler("process_exited", providerExitData(code, tail, wasKilled))
}

func (p *ClaudeProvider) pumpDetached(det *detachedRun, spawn *claudeSpawnState) (int, []string) {
	// backoff grows across reads that deliver nothing and resets only after a
	// read that delivered lines, so a read that fails at once cannot spin.
	backoff := detachedBackoffMin
	grow := func() {
		if backoff *= 2; backoff > detachedBackoffMax {
			backoff = detachedBackoffMax
		}
	}
	for {
		before, began := det.offset.Load(), time.Now()
		p.readDetached(det, spawn)
		if det.ctx.Err() != nil {
			return -1, nil
		}
		delivered := det.offset.Load() > before
		if delivered {
			backoff = detachedBackoffMin
		}
		st, err := p.detachedStatus(det)
		if err == nil && st.Phase != sshhost.RunAlive {
			return p.finishDetached(det, spawn, st)
		}
		if err == nil && !delivered && time.Since(began) < detachedQuickFail {
			// The read failed at once while the host answers: not a link
			// drop, so no event; wait and try again.
			if !sleepCtx(det.ctx, backoff) {
				return -1, nil
			}
			grow()
			continue
		}

		// The runner is alive or the host cannot be asked: either way the
		// link is what ended the read.
		p.emitHostLink(det, "lost")
		det.linkLost.Store(true)
		for {
			if !sleepCtx(det.ctx, backoff) {
				return -1, nil
			}
			grow()
			if st, err = p.detachedStatus(det); err == nil {
				break
			}
		}
		det.linkLost.Store(false)
		p.emitHostLink(det, "restored")
		if st.Phase != sshhost.RunAlive {
			return p.finishDetached(det, spawn, st)
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (p *ClaudeProvider) emitHostLink(det *detachedRun, link string) {
	logging.BeginEvent(context.Background(), "session.host_link").
		Set("session_id", p.session.ID).Set("host_id", det.spec.ID).Set("link", link).
		End(logging.OutcomeOK, "", nil)
}

func (p *ClaudeProvider) detachedStatus(det *detachedRun) (sshhost.RunState, error) {
	ctx, cancel := context.WithTimeout(det.ctx, detachedSendTimeout)
	defer cancel()
	stdout, msg, err := runHost(ctx, det.spec, sshhost.RunnerStatus(det.spec.OS, det.dir), nil)
	if err != nil {
		return sshhost.RunState{}, errors.New(msg)
	}
	return sshhost.ParseRunState(stdout)
}

// finishDetached delivers what the agent wrote last, then reports its exit.
func (p *ClaudeProvider) finishDetached(det *detachedRun, spawn *claudeSpawnState, st sshhost.RunState) (int, []string) {
	det.finished.Store(true)
	ctx, cancel := context.WithTimeout(det.ctx, detachedSendTimeout)
	defer cancel()
	p.consumeLines(ctx, det, spawn, hostExec(ctx, det.spec, sshhost.RunnerReadOnce(det.spec.OS, det.dir, det.offset.Load())))

	code := -1
	if st.Phase == sshhost.RunExited {
		code = st.Code
	}
	var tail []string
	if code != 0 {
		if out, _, err := runHost(ctx, det.spec, sshhost.RunnerStderrTail(det.spec.OS, det.dir), nil); err == nil {
			tail = stderrTailOf(out)
		}
	}
	return code, tail
}

func stderrTailOf(text string) []string {
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			cut, _ := truncateOnRune(l, stderrLineMaxBytes)
			lines = append(lines, cut)
		}
	}
	if len(lines) > stderrTailLines {
		lines = lines[len(lines)-stderrTailLines:]
	}
	return lines
}

func (p *ClaudeProvider) readDetached(det *detachedRun, spawn *claudeSpawnState) {
	cmd := hostExec(det.ctx, det.spec, sshhost.RunnerRead(det.spec.OS, det.dir, det.offset.Load()))
	p.consumeLines(det.ctx, det, spawn, cmd)
}

// closeOnDone closes r when ctx ends, so a read blocked on a pipe that a
// grandchild of the killed command still holds open returns. The returned
// func stops the watcher.
func closeOnDone(ctx context.Context, r io.Closer) func() {
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = r.Close()
		case <-stop:
		}
	}()
	return func() { close(stop) }
}

// consumeLines runs cmd and feeds each complete line of its stdout to
// processLine. A partial last line is dropped: the offset stays at its start,
// so the next read begins there.
func (p *ClaudeProvider) consumeLines(ctx context.Context, det *detachedRun, spawn *claudeSpawnState, cmd *exec.Cmd) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		slog.Warn("claude: detached read did not start", "session", p.session.ID, "error", err)
		return
	}
	defer closeOnDone(ctx, stdout)()
	br := bufio.NewReaderSize(stdout, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			break
		}
		body := bytes.TrimRight(line, "\r\n")
		if len(body) > 0 {
			spawn.sawStdout.Store(true)
			p.processLine(json.RawMessage(body), spawn)
		}
		det.offset.Add(int64(len(line)))
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// sendDetached appends one line (data ends in a newline) to the agent's input.
// Deliberate: no retry. An ssh failure after the command started may still
// have landed the line, and a second append would deliver it twice.
func (p *ClaudeProvider) sendDetached(det *detachedRun, data []byte) error {
	if det.linkLost.Load() {
		return fmt.Errorf("the link to host %q is down; the agent keeps running, send again when it returns", det.spec.Name)
	}
	line := bytes.TrimSuffix(data, []byte("\n"))
	ctx, cancel := context.WithTimeout(det.ctx, detachedSendTimeout)
	defer cancel()
	if _, msg, err := runHost(ctx, det.spec, sshhost.RunnerSend(det.spec.OS, det.dir), bytes.NewReader(line)); err != nil {
		return fmt.Errorf("send to claude on host %q: %s", det.spec.Name, msg)
	}
	return nil
}

// Release detaches this provider from its agent and leaves the agent running
// on the host: the local reader stops and process_exited follows, nothing is
// sent to the host. A relay or relay-sessions shutdown uses it, because a
// detached agent outlives relay. Kill, by contrast, ends the agent.
func (p *ClaudeProvider) Release() {
	if p.perms != nil {
		p.perms.DenyAllForSession(p.session.ID, "session stopped")
	}
	p.root.Store(nil)
	if det := p.det; det != nil {
		p.killDetached(det, false)
		return
	}
	p.Kill()
}

func (p *ClaudeProvider) killDetached(det *detachedRun, stopAgent bool) {
	det.stopOnce.Do(func() {
		if p.killed != nil {
			p.killed.Store(true)
		}
		p.alive.Store(false)
		if p.stopIdle != nil {
			p.stopIdleOnce.Do(func() { close(p.stopIdle) })
		}
		det.cancel()
		if stopAgent && !det.finished.Load() {
			stopDetachedRunner(p.session.ID, det.spec, det.dir)
		}
		close(det.stopped)
	})
	<-det.done
	if p.exit != nil {
		<-p.exit.done
	}
	slog.Info("claude process killed", "session", p.session.ID)
}

// stopRunner asks the host to end the agent's process group. A host that
// cannot be reached leaves the agent running; that is logged, since nothing
// else will tell the operator.
func stopDetachedRunner(session string, spec *sessionstypes.HostSpec, dir sshhost.RunDir) {
	var msg string
	for i := 0; i < detachedStopTries; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), detachedStopTimeout)
		_, m, err := runHost(ctx, spec, sshhost.RunnerStop(spec.OS, dir), nil)
		cancel()
		if err == nil {
			return
		}
		msg = m
		time.Sleep(time.Second)
	}
	slog.Warn("claude: detached agent could not be stopped; it may still run on the host", "session", session, "host", spec.ID, "error", msg)
}

// removeDetachedRun ends the agent and deletes its run directories, which
// hold tool output.
func (p *ClaudeProvider) removeDetachedRun(host *sessionstypes.HostSpec) {
	p.Kill()
	DeleteDetachedRun(p.session.ID, host)
}

// DeleteDetachedRun ends the latest agent of a detached session and deletes
// its run directories on the host. It needs no live provider: a session that
// is dormant after a restart still has an agent to end. A host that cannot be
// reached leaves both in place; that is logged.
func DeleteDetachedRun(session string, host *sessionstypes.HostSpec) {
	if host == nil || !host.Detach || len(host.SSHArgv) == 0 || sshhost.IsWindowsOS(host.OS) {
		return
	}
	stopDetachedRunner(session, host, sshhost.RunDir{Session: session})
	ctx, cancel := context.WithTimeout(context.Background(), detachedStopTimeout)
	defer cancel()
	if _, msg, err := runHost(ctx, host, sshhost.RunnerRemove(host.OS, session), nil); err != nil {
		slog.Warn("claude: detached run directory not removed", "session", session, "host", host.ID, "error", msg)
	}
}

// Events returns up to max complete events the agent wrote after the cursor
// and the cursor to pass next. The cursor is "<generation>:<byte offset of a
// line start>"; "" starts at the current generation's beginning.
func (p *ClaudeProvider) Events(ctx context.Context, after string, max int) (lines []json.RawMessage, next string, err error) {
	det := p.det
	if det == nil {
		return nil, "", errors.New("session is not a detached host session")
	}
	d := det.dir
	var offset int64
	if after != "" {
		gen, off, ok := strings.Cut(after, ":")
		n, perr := strconv.ParseInt(off, 10, 64)
		if !ok || gen == "" || perr != nil || n < 0 {
			return nil, "", fmt.Errorf("invalid event cursor %q", after)
		}
		d.Generation, offset = gen, n
	}
	if max <= 0 || max > detachedEventsMax {
		max = detachedEventsMax
	}

	rctx, cancel := context.WithTimeout(ctx, detachedEventsWait)
	defer cancel()
	cmd := hostExec(rctx, det.spec, sshhost.RunnerReadOnce(det.spec.OS, d, offset))
	stdout, perr := cmd.StdoutPipe()
	if perr != nil {
		return nil, "", perr
	}
	if perr = cmd.Start(); perr != nil {
		return nil, "", fmt.Errorf("read events on host %q: %w", det.spec.Name, perr)
	}
	defer closeOnDone(rctx, stdout)()
	br := bufio.NewReaderSize(stdout, 64*1024)
	var rerr error
	for len(lines) < max {
		var line []byte
		if line, rerr = br.ReadBytes('\n'); rerr != nil {
			break
		}
		offset += int64(len(line))
		if body := bytes.TrimRight(line, "\r\n"); len(body) > 0 {
			lines = append(lines, json.RawMessage(body))
		}
	}
	if rerr != io.EOF {
		_ = cmd.Process.Kill()
	}
	if werr := cmd.Wait(); werr != nil && rerr == io.EOF && len(lines) == 0 {
		return nil, "", fmt.Errorf("read events on host %q: %w", det.spec.Name, werr)
	}
	if rctx.Err() != nil && len(lines) == 0 {
		return nil, "", fmt.Errorf("read events on host %q: %w", det.spec.Name, rctx.Err())
	}
	return lines, d.Generation + ":" + strconv.FormatInt(offset, 10), nil
}
