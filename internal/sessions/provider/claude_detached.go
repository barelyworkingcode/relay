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
	"github.com/barelyworkingcode/relay/internal/sessions/events"
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
	// detachedQuickFail is how long a reopened read must stay open, delivering
	// nothing, before the link counts as restored.
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
	// linkMu orders linkLost with restored: restored is non-nil exactly while
	// the link is marked lost, and closes when it is restored.
	linkMu   sync.Mutex
	restored chan struct{}
	// lineSig is nudged when a line arrives, so a reopened read can prove
	// itself without waiting out its timer.
	lineSig chan struct{}
	// finished is set once the runner is known to be gone, so Kill has nothing
	// to stop.
	finished atomic.Bool

	// requests holds the ids of control requests already answered or raised, so
	// an attach that replays old output raises none twice.
	reqMu    sync.Mutex
	requests map[string]struct{}

	stopOnce sync.Once
	// stopped closes when the host stop has returned; the final process_exited
	// of a killed spawn waits on it so the exit follows the real end.
	stopped chan struct{}
	done    chan struct{}
}

// markLost marks the link lost and reports whether this call did it.
func (d *detachedRun) markLost() bool {
	d.linkMu.Lock()
	defer d.linkMu.Unlock()
	if d.linkLost.Load() {
		return false
	}
	d.restored = make(chan struct{})
	d.linkLost.Store(true)
	return true
}

// markRestored clears the lost mark, wakes every send waiting on it, and
// reports whether this call did it.
func (d *detachedRun) markRestored() bool {
	d.linkMu.Lock()
	defer d.linkMu.Unlock()
	if !d.linkLost.Load() {
		return false
	}
	d.linkLost.Store(false)
	close(d.restored)
	d.restored = nil
	return true
}

func (d *detachedRun) restoredSignal() <-chan struct{} {
	d.linkMu.Lock()
	defer d.linkMu.Unlock()
	return d.restored
}

// claimRequest reports whether the control request in raw has not been
// answered or raised before, and marks it as handled.
func (d *detachedRun) claimRequest(raw json.RawMessage) bool {
	return d.claimRequestID(controlRequestID(raw))
}

func (d *detachedRun) claimRequestID(id string) bool {
	if id == "" {
		return true
	}
	d.reqMu.Lock()
	defer d.reqMu.Unlock()
	if _, seen := d.requests[id]; seen {
		return false
	}
	if d.requests == nil {
		d.requests = map[string]struct{}{}
	}
	d.requests[id] = struct{}{}
	return true
}

// controlRequestID is the request id of a control_request or control_response
// line, compacted so the same id compares equal whatever its spacing.
func controlRequestID(raw json.RawMessage) string {
	var line struct {
		RequestID json.RawMessage `json:"request_id"`
		Response  struct {
			RequestID json.RawMessage `json:"request_id"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &line) != nil {
		return ""
	}
	id := line.RequestID
	if len(id) == 0 {
		id = line.Response.RequestID
	}
	var buf bytes.Buffer
	if len(id) == 0 || json.Compact(&buf, id) != nil {
		return ""
	}
	return buf.String()
}

// detachedCursor is where a detached session's saved state says relay stopped
// reading: the byte offset of a line start in one generation's output. Only
// relay-sessions reads the output, so it saves the cursor with the
// conversation's state, at every save.
type detachedCursor struct {
	Generation string `json:"generation"`
	Offset     int64  `json:"cursor"`
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
	if p.cfg.Resume || p.cfg.AttachOnly {
		ctx, cancel := context.WithTimeout(context.Background(), detachedStartTimeout)
		st, err := DetachedRunState(ctx, host, p.session.ID)
		cancel()
		if err != nil {
			return fmt.Errorf("ask host %q whether the agent runs: %w", host.Name, err)
		}
		if st.Phase == sshhost.RunAlive {
			return p.attachDetached(host, st.Generation)
		}
		if p.cfg.AttachOnly {
			return fmt.Errorf("%w on host %q", ErrNoDetachedRun, host.Name)
		}
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

	p.mu.Lock()
	p.restoredRun = nil
	p.mu.Unlock()
	det, spawn, spawnGen := p.beginDetached(host, sshhost.RunDir{Session: p.session.ID, Generation: gen})
	p.runDetached(det, spawn, spawnGen)
	slog.Info("claude detached agent started", "session", p.session.ID, "host", host.ID, "generation", gen)
	return nil
}

// beginDetached installs the state of a spawn that follows det's run and
// marks the provider alive. It starts no goroutine: runDetached does, so an
// attach can read what the agent wrote while relay was away first.
func (p *ClaudeProvider) beginDetached(host *sessionstypes.HostSpec, dir sshhost.RunDir) (*detachedRun, *claudeSpawnState, uint64) {
	runCtx, runCancel := context.WithCancel(context.Background())
	det := &detachedRun{
		spec:    host,
		dir:     dir,
		ctx:     runCtx,
		cancel:  runCancel,
		stopped: make(chan struct{}),
		done:    make(chan struct{}),
		lineSig: make(chan struct{}, 1),
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
	return det, &claudeSpawnState{}, spawnGen
}

func (p *ClaudeProvider) runDetached(det *detachedRun, spawn *claudeSpawnState, spawnGen uint64) {
	go p.followDetached(det, spawn, spawnGen, p.killed, p.exit)
	go p.idleWatcher(p.stopIdle)
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

// pumpDetached reads the agent's events until it exits or the spawn is killed.
// A read that ends while the runner lives marks the link lost (one `lost`
// event per outage). A reopened read clears the mark, with `restored`, once it
// has delivered a line or stayed open detachedQuickFail, so a read that keeps
// failing at once produces one `lost` and no churn. The backoff grows across
// reads that deliver nothing and resets after one that delivered lines.
func (p *ClaudeProvider) pumpDetached(det *detachedRun, spawn *claudeSpawnState) (int, []string) {
	backoff := detachedBackoffMin
	grow := func() {
		if backoff *= 2; backoff > detachedBackoffMax {
			backoff = detachedBackoffMax
		}
	}
	for {
		select {
		case <-det.lineSig:
		default:
		}
		before := det.offset.Load()
		readDone := make(chan struct{})
		watched := make(chan struct{})
		go func() {
			defer close(watched)
			p.confirmRestored(det, readDone)
		}()
		p.readDetached(det, spawn)
		close(readDone)
		<-watched
		if det.ctx.Err() != nil {
			return -1, nil
		}
		if det.offset.Load() > before {
			backoff = detachedBackoffMin
		}

		st, err := p.detachedStatus(det)
		for err == nil && st.Phase == sshhost.RunAlive || err != nil {
			if det.markLost() {
				p.emitHostLink(det, "lost")
			}
			if !sleepCtx(det.ctx, backoff) {
				return -1, nil
			}
			grow()
			if err == nil {
				break
			}
			st, err = p.detachedStatus(det)
		}
		if err == nil && st.Phase != sshhost.RunAlive {
			if det.markRestored() {
				p.emitHostLink(det, "restored")
			}
			return p.finishDetached(det, spawn, st)
		}
	}
}

// confirmRestored clears a lost mark once the read has proved itself: a line
// arrived or it stayed open long enough. It ends when the read does.
func (p *ClaudeProvider) confirmRestored(det *detachedRun, readDone <-chan struct{}) {
	if !det.linkLost.Load() {
		return
	}
	t := time.NewTimer(detachedQuickFail)
	defer t.Stop()
	select {
	case <-det.lineSig:
	case <-t.C:
	case <-readDone:
		return
	case <-det.ctx.Done():
		return
	}
	if det.markRestored() {
		p.emitHostLink(det, "restored")
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
		select {
		case det.lineSig <- struct{}{}:
		default:
		}
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// DefaultLinkWait is how long a send waits for a lost host link to return.
const DefaultLinkWait = 30 * time.Second

// MaxLinkWait is the longest wait a caller may ask for. It stays under the
// bridge's ten-minute inactivity limit on the operator's call.
const MaxLinkWait = 9 * time.Minute

// MaxMessageLinkWait is the longest `message` waits for the link. The reply
// wait that follows can take five minutes, and the sum stays under the
// bridge's ten-minute limit.
const MaxMessageLinkWait = 4 * time.Minute

// ClampMessageLinkWait is ClampLinkWait for the message route.
func ClampMessageLinkWait(d time.Duration) time.Duration {
	return min(ClampLinkWait(d), MaxMessageLinkWait)
}

// ClampLinkWait returns DefaultLinkWait for a zero or negative d and MaxLinkWait
// for one above the limit.
func ClampLinkWait(d time.Duration) time.Duration {
	switch {
	case d <= 0:
		return DefaultLinkWait
	case d > MaxLinkWait:
		return MaxLinkWait
	}
	return d
}

var (
	// ErrHostLinkDown means the link to a detached session's host stayed down
	// for the whole wait; nothing was sent and the agent keeps running.
	ErrHostLinkDown = errors.New("the host link is down")
	// ErrSendUnknown means the connection ended during the send, so the line
	// may or may not have reached the agent. It is never retried.
	ErrSendUnknown = errors.New("the outcome of the send is unknown")
	// ErrNotDetached means the session has no detached agent to replay.
	ErrNotDetached = errors.New("session is not a detached host session")
	// ErrNoDetachedRun means an attach found no running agent for the session.
	ErrNoDetachedRun = errors.New("no running detached agent")
	// ErrInvalidCursor means an events cursor was not one this package issued.
	ErrInvalidCursor = errors.New("invalid event cursor")
)

// sshConnectFailure reports a first stderr line that proves ssh never reached
// the host's shell, so the command did not run and a later attempt cannot
// deliver twice.
func sshConnectFailure(msg string) bool {
	return strings.HasPrefix(msg, "ssh: connect to host") || strings.HasPrefix(msg, "ssh: Could not resolve hostname")
}

// awaitLink returns once the link is not marked lost. It gives up when the
// timer fires, the caller's ctx ends, the runner is gone or the spawn is
// killed. A caller that has left gets ErrHostLinkDown and nothing is written.
func (p *ClaudeProvider) awaitLink(ctx context.Context, det *detachedRun, wait time.Duration, timer *time.Timer) error {
	down := fmt.Errorf("%w: the message was not sent; retry when host %q is reachable", ErrHostLinkDown, det.spec.Name)
	if ctx.Err() != nil {
		return down
	}
	for det.linkLost.Load() {
		if wait <= 0 {
			return down
		}
		ch := det.restoredSignal()
		if ch == nil {
			continue
		}
		select {
		case <-ch:
		case <-timer.C:
			return down
		case <-ctx.Done():
			return down
		case <-det.done:
			return fmt.Errorf("claude on host %q has exited", det.spec.Name)
		case <-det.ctx.Done():
			return fmt.Errorf("the session on host %q was stopped", det.spec.Name)
		}
	}
	return nil
}

// sendDetached appends one line (data ends in a newline) to the agent's input.
// It waits up to wait for a lost link to return, then writes once.
// Deliberate: no retry after the command ran. An ssh failure after the
// command started may still have landed the line, and a second append would
// deliver it twice. A failure that proves ssh never connected is the one
// retry: the link is marked lost and the wait begins again.
func (p *ClaudeProvider) sendDetached(callerCtx context.Context, det *detachedRun, data []byte, wait time.Duration) error {
	line := bytes.TrimSuffix(data, []byte("\n"))
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		if err := p.awaitLink(callerCtx, det, wait, timer); err != nil {
			return err
		}
		if callerCtx.Err() != nil {
			return fmt.Errorf("%w: the message was not sent; retry when host %q is reachable", ErrHostLinkDown, det.spec.Name)
		}
		// Deliberate: the write itself does not follow the caller's ctx. A
		// line already on its way cannot be recalled, so it is left to finish.
		ctx, cancel := context.WithTimeout(det.ctx, detachedSendTimeout)
		_, msg, err := runHost(ctx, det.spec, sshhost.RunnerSend(det.spec.OS, det.dir), bytes.NewReader(line))
		cut := ctx.Err() != nil
		cancel()
		switch {
		case err == nil:
			return nil
		case sshConnectFailure(msg):
			if det.markLost() {
				p.emitHostLink(det, "lost")
			}
		case cut || sendOutcomeUnknown(err):
			return fmt.Errorf("%w: the connection to host %q ended during the send; the message may have been delivered, so read the session's events before sending again", ErrSendUnknown, det.spec.Name)
		default:
			return fmt.Errorf("send to claude on host %q: %s", det.spec.Name, msg)
		}
	}
}

// sendOutcomeUnknown is true when the failure is ssh's own (exit 255) or the
// command was killed, so the remote append may have run.
func sendOutcomeUnknown(err error) bool {
	var ee *exec.ExitError
	return !errors.As(err, &ee) || ee.ExitCode() == 255 || ee.ExitCode() < 0
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
		return nil, "", ErrNotDetached
	}
	d := det.dir
	var offset int64
	if after != "" {
		gen, off, ok := strings.Cut(after, ":")
		n, perr := strconv.ParseInt(off, 10, 64)
		if !ok || gen == "" || perr != nil || n < 0 {
			return nil, "", fmt.Errorf("%w %q", ErrInvalidCursor, after)
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

// DetachedHost reports whether a session on host runs its agent detached.
func DetachedHost(host *sessionstypes.HostSpec) bool {
	return host != nil && host.Detach && len(host.SSHArgv) > 0 && !sshhost.IsWindowsOS(host.OS)
}

// DetachedRunState asks host about the latest run of session's agent. A host
// that cannot answer is an error, never "not running".
func DetachedRunState(ctx context.Context, host *sessionstypes.HostSpec, session string) (sshhost.RunState, error) {
	stdout, msg, err := runHost(ctx, host, sshhost.RunnerStatus(host.OS, sshhost.RunDir{Session: session}), nil)
	if err != nil {
		return sshhost.RunState{}, errors.New(msg)
	}
	return sshhost.ParseRunState(stdout)
}

// attachProbeScript takes $1 session and $2 generation and prints what an
// attach needs in one round trip, one tagged line each: "S <n>", the end of
// the last complete line of out.jsonl; "T <line>", its last line; "O <line>",
// every control_request line; "I <line>", every control_response line of
// in.jsonl. Deliberate: a file that ends in a half-written line puts S at
// that line's start, because a cursor must sit on a line start.
const attachProbeScript = `case "$1" in ''|*[!A-Za-z0-9_-]*) echo "relay: invalid session id" >&2; exit 2;; esac
case "$2" in ''|*[!A-Za-z0-9_-]*) echo "relay: invalid generation" >&2; exit 2;; esac
[ -n "$HOME" ] || { echo "relay: HOME is not set on the host" >&2; exit 2; }
d="$HOME/.relay/agents/$1/$2"
f="$d/out.jsonl"
[ -f "$f" ] || { echo "relay: no agent output in $d" >&2; exit 5; }
n=$(wc -c < "$f" | tr -d ' ')
if [ "$n" -gt 0 ] && [ "$(tail -c 1 "$f" | od -An -tx1 | tr -d ' \n')" != 0a ]; then
  n=$((n - $(tail -n 1 "$f" | wc -c | tr -d ' ')))
fi
echo "S $n"
echo "T $(tail -n 1 "$f")"
grep -a '"control_request"' "$f" | sed 's/^/O /'
if [ -f "$d/in.jsonl" ]; then grep -a '"control_response"' "$d/in.jsonl" | sed 's/^/I /'; fi
exit 0
`

type attachProbe struct {
	size     int64
	last     string
	requests []json.RawMessage
	answered []string
}

func probeAttach(ctx context.Context, host *sessionstypes.HostSpec, dir sshhost.RunDir) (attachProbe, error) {
	remote := sshhost.RemoteCommandForOS(host.OS, "", []string{"sh", "-c", attachProbeScript, "relay-agent", dir.Session, dir.Generation}, nil)
	stdout, msg, err := runHost(ctx, host, remote, nil)
	if err != nil {
		return attachProbe{}, errors.New(msg)
	}
	var pr attachProbe
	sawSize := false
	for _, l := range strings.Split(stdout, "\n") {
		if len(l) < 2 || l[1] != ' ' {
			continue
		}
		body := strings.TrimRight(l[2:], "\r")
		switch l[0] {
		case 'S':
			if n, perr := strconv.ParseInt(strings.TrimSpace(body), 10, 64); perr == nil && n >= 0 {
				pr.size, sawSize = n, true
			}
		case 'T':
			pr.last = body
		case 'O':
			if lineType(body) == "control_request" {
				pr.requests = append(pr.requests, json.RawMessage(body))
			}
		case 'I':
			if lineType(body) == "control_response" {
				if id := controlRequestID(json.RawMessage(body)); id != "" {
					pr.answered = append(pr.answered, id)
				}
			}
		}
	}
	if !sawSize {
		return attachProbe{}, fmt.Errorf("unexpected attach answer %q", firstLineOf(stdout))
	}
	return pr, nil
}

func lineType(line string) string {
	var env struct {
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(line), &env) != nil {
		return ""
	}
	return env.Type
}

func firstLineOf(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// turnOpen reports whether the agent's last output line says a turn is in
// progress: it is mid-answer, running a tool or waiting on a permission. A
// result or system line, or none, is an idle agent.
func turnOpen(lastLine string) bool {
	switch lineType(lastLine) {
	case events.EvtAssistant, "user", "control_request":
		return true
	}
	return false
}

// attachDetached follows the agent already running in generation gen. It
// reads, before it returns, what the agent wrote since the saved cursor, so a
// message sent next meets only its own reply, and it raises the permission
// requests nobody answered: the agent is blocked on each. A cursor that is
// absent, from another generation or past the output starts at the end of the
// output.
func (p *ClaudeProvider) attachDetached(host *sessionstypes.HostSpec, gen string) error {
	dir := sshhost.RunDir{Session: p.session.ID, Generation: gen}
	ctx, cancel := context.WithTimeout(context.Background(), detachedStartTimeout)
	defer cancel()
	probe, err := probeAttach(ctx, host, dir)
	if err != nil {
		return fmt.Errorf("attach to the agent on host %q: %w", host.Name, err)
	}

	offset := probe.size
	p.mu.Lock()
	saved := p.restoredRun
	p.mu.Unlock()
	if saved != nil && saved.Generation == gen && saved.Offset >= 0 && saved.Offset <= probe.size {
		offset = saved.Offset
	}

	det, spawn, spawnGen := p.beginDetached(host, dir)
	det.offset.Store(offset)
	for _, id := range probe.answered {
		det.claimRequestID(id)
	}
	if turnOpen(probe.last) {
		p.session.SetProcessing(true)
	}
	for _, raw := range probe.requests {
		if det.claimRequest(raw) {
			p.handleControlRequest(raw)
		}
	}
	p.consumeLines(ctx, det, spawn, hostExec(ctx, host, sshhost.RunnerReadOnce(host.OS, dir, offset)))
	p.runDetached(det, spawn, spawnGen)

	slog.Info("claude detached agent attached", "session", p.session.ID, "host", host.ID, "generation", gen, "cursor", offset)
	return nil
}
