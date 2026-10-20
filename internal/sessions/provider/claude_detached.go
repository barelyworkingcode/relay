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
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/membership"
	"github.com/barelyworkingcode/relay/internal/sessions/runner"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
	"github.com/barelyworkingcode/relay/internal/sshhost"
)

// A detached Claude session keeps its agent on the host (docs/ssh-hosts.md,
// "Detached sessions"). Relay's ssh link carries input to the runner's inbox
// and reads out.jsonl by byte offset, so a dropped link ends neither the agent nor the
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
	// host is nil for a run on this machine.
	host *sessionstypes.HostSpec
	gen  string
	// conn reaches the run directory; connFor reaches another generation's.
	conn    runConn
	connFor func(gen string) runConn
	ctx     context.Context
	cancel  context.CancelFunc

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
	// deferring holds new control requests in deferred instead of raising
	// them; an attach sets it, because nothing can show a prompt until the
	// manager has published the session.
	deferring bool
	deferred  []json.RawMessage

	stopOnce sync.Once
	// stopped closes when the host stop has returned; the final process_exited
	// of a killed spawn waits on it so the exit follows the real end.
	stopped chan struct{}
	done    chan struct{}
}

// where names the machine the agent runs on, for messages.
func (d *detachedRun) where() string {
	if d.host == nil {
		return "this machine"
	}
	return fmt.Sprintf("host %q", d.host.Name)
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

// deferRaise holds raw for a later raise and reports whether it did.
func (d *detachedRun) deferRaise(raw json.RawMessage) bool {
	d.reqMu.Lock()
	defer d.reqMu.Unlock()
	if !d.deferring {
		return false
	}
	d.deferred = append(d.deferred, raw)
	return true
}

// endDeferring stops holding requests and returns the ones held.
func (d *detachedRun) endDeferring() []json.RawMessage {
	d.reqMu.Lock()
	defer d.reqMu.Unlock()
	d.deferring = false
	held := d.deferred
	d.deferred = nil
	return held
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

// claudeRunnerPatterns are the strings the runner matches in Claude's stdout:
// the result line ends a turn, and a can_use_tool request is a permission
// prompt.
var claudeRunnerPatterns = runner.Patterns{
	TurnEnded: `"type":"result"`,
	Asking:    `"subtype":"can_use_tool"`,
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
			return p.attachRun(host, hostConnFor(host, p.session.ID), st.Generation)
		}
		if st.Phase == sshhost.RunLegacy {
			// A run from before the message runner has no inbox to send to: it
			// is stopped once and the session resumes on its conversation.
			stopRun(p.session.ID, fmt.Sprintf("host %q", host.Name), hostConnFor(host, p.session.ID)(st.Generation))
		}
		if p.cfg.AttachOnly {
			return fmt.Errorf("%w on host %q", ErrNoDetachedRun, host.Name)
		}
	}
	program := p.cfg.Launcher.Program(host.ClaudePath)
	env := map[string]string{"RELAY_SESSION_ID": p.session.ID}
	remote := sshhost.RunnerStartIn(host.OS, p.directory, sshhost.RunDir{Session: p.session.ID}, append(slices.Clone(program), args...), env, claudeRunnerPatterns)

	ctx, cancel := context.WithTimeout(context.Background(), detachedStartTimeout)
	defer cancel()
	stdout, msg, err := runHost(ctx, host, remote, nil)
	if err != nil {
		return fmt.Errorf("start detached claude on host %q: %s (hosts[].detach_sessions: false runs this host's sessions under ssh -T)", host.Name, msg)
	}
	gen, err := sshhost.ParseRunStart(stdout)
	if err != nil {
		return fmt.Errorf("start detached claude on host %q: %w (hosts[].detach_sessions: false runs this host's sessions under ssh -T)", host.Name, err)
	}

	p.mu.Lock()
	p.restoredRun = nil
	p.mu.Unlock()
	det, spawn, spawnGen := p.beginDetached(host, hostConnFor(host, p.session.ID), gen)
	p.runDetached(det, spawn, spawnGen)
	slog.Info("claude detached agent started", "session", p.session.ID, "host", host.ID, "generation", gen)
	return nil
}

// beginDetached installs the state of a spawn that follows det's run and
// marks the provider alive. It starts no goroutine: runDetached does, so an
// attach can read what the agent wrote while relay was away first.
func (p *ClaudeProvider) beginDetached(host *sessionstypes.HostSpec, connFor func(gen string) runConn, gen string) (*detachedRun, *claudeSpawnState, uint64) {
	runCtx, runCancel := context.WithCancel(context.Background())
	det := &detachedRun{
		host:    host,
		gen:     gen,
		conn:    connFor(gen),
		connFor: connFor,
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
		for err == nil && st.Phase == runner.PhaseAlive || err != nil {
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
		if err == nil && st.Phase != runner.PhaseAlive {
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
	if det.host == nil {
		return
	}
	logging.BeginEvent(context.Background(), "session.host_link").
		Set("session_id", p.session.ID).Set("host_id", det.host.ID).Set("link", link).
		End(logging.OutcomeOK, "", nil)
}

func (p *ClaudeProvider) detachedStatus(det *detachedRun) (runner.Status, error) {
	ctx, cancel := context.WithTimeout(det.ctx, detachedSendTimeout)
	defer cancel()
	return det.conn.Status(ctx)
}

// finishDetached delivers what the agent wrote last, then reports its exit.
func (p *ClaudeProvider) finishDetached(det *detachedRun, spawn *claudeSpawnState, st runner.Status) (int, []string) {
	det.finished.Store(true)
	ctx, cancel := context.WithTimeout(det.ctx, detachedSendTimeout)
	defer cancel()
	p.consumeLines(ctx, det, spawn, false)

	code := -1
	if st.Phase == runner.PhaseExited {
		code = st.Code
	}
	var tail []string
	if code != 0 {
		if out, err := det.conn.StderrTail(ctx); err == nil {
			tail = p.redactTail(stderrTailOf(out))
		}
	}
	return code, tail
}

// redactTail masks the launch's secrets in each tail line. The secrets are
// empty after a relay restart; the shape-based masks still apply then.
func (p *ClaudeProvider) redactTail(lines []string) []string {
	secrets := []string{p.identitySecret(), p.cfg.ClaudeToken}
	for i, l := range lines {
		lines[i] = redactStderrLine(l, secrets)
	}
	return lines
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
	p.consumeLines(det.ctx, det, spawn, true)
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

// consumeLines reads the output from the saved offset, following it while the
// runner lives when follow is set, and feeds each complete line to
// processLine. A partial last line is dropped: the offset stays at its start,
// so the next read begins there.
func (p *ClaudeProvider) consumeLines(ctx context.Context, det *detachedRun, spawn *claudeSpawnState, follow bool) {
	stdout, err := det.conn.Read(ctx, det.offset.Load(), follow)
	if err != nil {
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
		// Deliberate: the offset moves before the line is handled, so a state
		// save that the line triggers (a result saves one) records the cursor
		// after that line.
		start := det.offset.Add(int64(len(line))) - int64(len(line))
		body := bytes.TrimRight(line, "\r\n")
		if spawn.countResultsFrom > 0 && start >= spawn.countResultsFrom && bytes.Contains(body, []byte(`"type":"result"`)) {
			spawn.resultsSeen.Add(1)
		}
		if len(body) > 0 {
			spawn.sawStdout.Store(true)
			p.processLine(json.RawMessage(body), spawn)
		}
		select {
		case det.lineSig <- struct{}{}:
		default:
		}
	}
	_ = stdout.Close()
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
	down := fmt.Errorf("%w: the message was not sent; retry when %s is reachable", ErrHostLinkDown, det.where())
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
			return fmt.Errorf("claude on %s has exited", det.where())
		case <-det.ctx.Done():
			return fmt.Errorf("the session on %s was stopped", det.where())
		}
	}
	return nil
}

// sendDetached queues one line (data ends in a newline) in the runner's inbox
// and returns once the runner has written it to the agent. It waits up to wait
// for a lost link to return, then sends once.
// Deliberate: no retry after the command ran. A failure after the command
// started, or a missing receipt, may still have landed the message, and a
// second one would deliver it twice. Only a failure that proves ssh never
// connected is retried: the link is marked lost and the wait begins again.
func (p *ClaudeProvider) sendDetached(callerCtx context.Context, det *detachedRun, data []byte, wait time.Duration) error {
	line := bytes.TrimSuffix(data, []byte("\n"))
	timer := time.NewTimer(wait)
	defer timer.Stop()
	// A control_response answers a request and starts no turn.
	kind := runner.MessageTurn
	if lineType(string(line)) == "control_response" {
		kind = runner.MessageControl
	}
	for {
		if err := p.awaitLink(callerCtx, det, wait, timer); err != nil {
			return err
		}
		if callerCtx.Err() != nil {
			return fmt.Errorf("%w: the message was not sent; retry when %s is reachable", ErrHostLinkDown, det.where())
		}
		err := det.conn.Send(det.ctx, kind, line)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, errNoConnect):
			if det.markLost() {
				p.emitHostLink(det, "lost")
			}
		case errors.Is(err, errSendUnknown):
			return fmt.Errorf("%w: the connection to %s ended during the send; the message may have been delivered, so read the session's events before sending again", ErrSendUnknown, det.where())
		case errors.Is(err, runner.ErrNotRunning):
			return fmt.Errorf("the agent is not running on %s", det.where())
		case errors.Is(err, runner.ErrNoReceipt):
			return fmt.Errorf("%w: %s did not confirm delivery in time; the message is queued and may still be delivered, so read the session's events before sending again", ErrSendUnknown, det.where())
		default:
			return fmt.Errorf("send to claude on %s: %w", det.where(), err)
		}
	}
}

// RunnerSend's exit codes.
const (
	runnerNotRunning = 5
	runnerNoReceipt  = 7
)

func exitCodeOf(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// sendOutcomeUnknown is true when the failure is ssh's own (exit 255) or the
// command was killed, so the remote write may have run.
func sendOutcomeUnknown(err error) bool {
	var ee *exec.ExitError
	return !errors.As(err, &ee) || ee.ExitCode() == 255 || ee.ExitCode() < 0
}

// Release detaches this provider from its agent and leaves the agent running
// on the host: the local reader stops and process_exited follows, nothing is
// sent to the host. A relay or relay-sessions shutdown uses it, because a
// detached agent outlives relay. Kill, by contrast, ends the agent.
func (p *ClaudeProvider) Release() {
	p.root.Store(nil)
	if det := p.det; det != nil {
		p.released.Store(true)
		// Deliberate: no denial of waiting permission prompts. The agent is
		// still blocked on each, and the next attach raises it again.
		p.killDetached(det, false)
		return
	}
	p.Kill()
}

// Released reports whether Release detached this provider from a running
// agent.
func (p *ClaudeProvider) Released() bool { return p.released.Load() }

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
			stopRun(p.session.ID, det.where(), det.conn)
		}
		close(det.stopped)
	})
	<-det.done
	if p.exit != nil {
		<-p.exit.done
	}
	slog.Info("claude process killed", "session", p.session.ID)
}

// hostConnFor reaches generations of session's run directory on host.
func hostConnFor(host *sessionstypes.HostSpec, session string) func(gen string) runConn {
	return func(gen string) runConn {
		return sshConn{host: host, dir: sshhost.RunDir{Session: session, Generation: gen}}
	}
}

// localConnFor reaches generations of session's run directory under base.
func localConnFor(base, session string) func(gen string) runConn {
	return func(gen string) runConn {
		return localConn{run: runner.LocalRun{Base: base, Session: session, Generation: gen}}
	}
}

// stopRun asks the runner to end the agent. An agent that cannot be reached
// keeps running; that is logged, since nothing else will tell the operator.
func stopRun(session, where string, conn runConn) {
	var err error
	for i := 0; i < detachedStopTries; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), detachedStopTimeout)
		err = conn.Stop(ctx)
		cancel()
		if err == nil {
			return
		}
		time.Sleep(time.Second)
	}
	slog.Warn("claude: detached agent could not be stopped; it may still run", "session", session, "where", where, "error", err)
}

// StopDetachedRun ends the latest agent of a detached session without
// deleting its run directories. It needs no live provider.
func StopDetachedRun(session string, host *sessionstypes.HostSpec) {
	if DetachedHost(host) {
		stopRun(session, fmt.Sprintf("host %q", host.Name), hostConnFor(host, session)(""))
	}
}

// StopLocalRun is StopDetachedRun for a run on this machine.
func StopLocalRun(base, session string) {
	stopRun(session, "this machine", localConnFor(base, session)(""))
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
	StopDetachedRun(session, host)
	ctx, cancel := context.WithTimeout(context.Background(), detachedStopTimeout)
	defer cancel()
	if err := hostConnFor(host, session)("").Remove(ctx); err != nil {
		slog.Warn("claude: detached run directory not removed", "session", session, "host", host.ID, "error", err)
	}
}

// DeleteLocalRun is DeleteDetachedRun for a run on this machine.
func DeleteLocalRun(base, session string) {
	StopLocalRun(base, session)
	if err := runner.RemoveLocal(base, session); err != nil {
		slog.Warn("claude: local run directory not removed", "session", session, "error", err)
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
	gen := det.gen
	var offset int64
	if after != "" {
		g, off, ok := strings.Cut(after, ":")
		n, perr := strconv.ParseInt(off, 10, 64)
		if !ok || g == "" || perr != nil || n < 0 {
			return nil, "", fmt.Errorf("%w %q", ErrInvalidCursor, after)
		}
		gen, offset = g, n
	}
	if max <= 0 || max > detachedEventsMax {
		max = detachedEventsMax
	}

	rctx, cancel := context.WithTimeout(ctx, detachedEventsWait)
	defer cancel()
	stdout, perr := det.connFor(gen).Read(rctx, offset, false)
	if perr != nil {
		return nil, "", fmt.Errorf("read events on %s: %w", det.where(), perr)
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
	if werr := stdout.Close(); werr != nil && rerr == io.EOF && len(lines) == 0 {
		return nil, "", fmt.Errorf("read events on %s: %w", det.where(), werr)
	}
	if rctx.Err() != nil && len(lines) == 0 {
		return nil, "", fmt.Errorf("read events on %s: %w", det.where(), rctx.Err())
	}
	return lines, gen + ":" + strconv.FormatInt(offset, 10), nil
}

// TakeAttach returns, once, whether the attach found a turn in progress and
// a func that raises the permission requests nobody answered. The manager
// calls it after it publishes the session: the attention board and the
// viewers can show a prompt only then. It returns nil for a provider that did
// not attach.
func (p *ClaudeProvider) TakeAttach() (turnOpen bool, raise func()) {
	p.mu.Lock()
	turnOpen = p.attachedTurnOpen
	p.attachedTurnOpen = false
	det := p.det
	p.mu.Unlock()
	// Deliberate: the hold ends here, not when the attach returns. A request
	// the follow loop delivers before the manager has published the session
	// joins the held ones and is raised with the session in place.
	var reqs []json.RawMessage
	if det != nil {
		reqs = det.endDeferring()
	}
	return turnOpen, func() {
		for _, raw := range reqs {
			p.handleControlRequest(raw)
		}
	}
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
// the last complete line of out.jsonl; "U <n>", the turn messages in sent/ and
// inbox/; "R <n>", the result lines in out.jsonl; "O <line>", every
// control_request line; "I <line>", every control message in sent/ and inbox/.
// Deliberate: a file that ends in a half-written line puts S at that line's
// start, because a cursor must sit on a line start.
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
u=0
for m in "$d"/sent/*.json "$d"/inbox/*.json; do [ -f "$m" ] && u=$((u+1)); done
echo "U $u"
echo "R $(grep -a -c '"type":"result"' "$f")"
grep -a '"control_request"' "$f" | sed 's/^/O /'
for m in "$d"/sent/*.ctl "$d"/inbox/*.ctl; do [ -f "$m" ] && sed 's/^/I /' "$m"; done
exit 0
`

type attachProbe struct {
	size     int64
	users    int
	results  int
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
		case 'U':
			pr.users, _ = strconv.Atoi(strings.TrimSpace(body))
		case 'R':
			pr.results, _ = strconv.Atoi(strings.TrimSpace(body))
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

// attachRun follows the agent already running in generation gen. It
// reads, before it returns, what the agent wrote since the saved cursor, so a
// message sent next meets only its own reply, and it raises the permission
// requests nobody answered: the agent is blocked on each. A cursor that is
// absent, from another generation or past the output starts at the end of the
// output.
func (p *ClaudeProvider) attachRun(host *sessionstypes.HostSpec, connFor func(gen string) runConn, gen string) error {
	ctx, cancel := context.WithTimeout(context.Background(), detachedStartTimeout)
	defer cancel()
	probe, err := connFor(gen).Probe(ctx)
	if err != nil {
		if host == nil {
			return fmt.Errorf("attach to the local agent: %w", err)
		}
		return fmt.Errorf("attach to the agent on host %q: %w", host.Name, err)
	}

	offset := probe.size
	p.mu.Lock()
	saved := p.restoredRun
	p.mu.Unlock()
	if saved != nil && saved.Generation == gen && saved.Offset >= 0 && saved.Offset <= probe.size {
		offset = saved.Offset
	}

	det, spawn, spawnGen := p.beginDetached(host, connFor, gen)
	det.offset.Store(offset)
	for _, id := range probe.answered {
		det.claimRequestID(id)
	}
	det.deferring = true
	spawn.countResultsFrom = probe.size
	for _, raw := range probe.requests {
		if det.claimRequest(raw) {
			det.deferred = append(det.deferred, raw)
		}
	}
	p.consumeLines(ctx, det, spawn, false)
	// Results the read delivered from past the probe end a turn the probe
	// counted as open; older ones it replayed from the cursor do not count.
	turnOpen := probe.users > probe.results+int(spawn.resultsSeen.Load())
	p.session.SetProcessing(turnOpen)
	p.mu.Lock()
	p.attachedTurnOpen = turnOpen
	p.mu.Unlock()
	p.runDetached(det, spawn, spawnGen)

	slog.Info("claude detached agent attached", "session", p.session.ID, "where", det.where(), "generation", gen, "cursor", offset)
	return nil
}

// startLocal runs the agent on this machine under the runner, so it outlives
// relay-sessions. A resume or attach finds a runner that still lives first and
// follows it: a second agent must never share the conversation.
func (p *ClaudeProvider) startLocal(spawn *claudeSpawnState) error {
	base := p.cfg.RunnerBase
	if base == "" {
		return errors.New("claude: no runner directory is configured for local sessions")
	}
	if p.cfg.Resume || p.cfg.AttachOnly {
		st, err := runner.LocalStatus(base, p.session.ID, "")
		if err != nil {
			return fmt.Errorf("read the local runner state: %w", err)
		}
		if st.Phase == runner.PhaseAlive {
			return p.attachLocal(st)
		}
		if p.cfg.AttachOnly {
			return fmt.Errorf("%w on this machine", ErrNoDetachedRun)
		}
	}
	if err := p.ensureHookConfig(); err != nil {
		slog.Warn("claude: hook config write failed", "session", p.session.ID, "directory", p.directory, "error", err)
	}

	run, err := runner.AllocLocal(base, p.session.ID)
	if err != nil {
		return err
	}
	started := false
	defer func() {
		if !started {
			_ = os.RemoveAll(run.Dir())
		}
	}()

	// Deliberate: a sandboxed agent reads these files from inside its sandbox,
	// and no profile grants the runner directory, so its files stay in the temp
	// directory the profile does grant. An unsandboxed agent keeps them in its
	// run directory, which removes them with the session.
	fileDir, tracked := run.Dir(), false
	if p.cfg.SandboxProfile != "" {
		fileDir, tracked = os.TempDir(), true
	}
	writeFile := func(pattern string, data []byte) (string, error) {
		path, err := writeSpawnFile(fileDir, pattern, data)
		if err == nil && tracked {
			p.addSpawnFile(path)
		}
		return path, err
	}
	var mcpConfigPath, sysPromptPath string
	if cfg := p.relayMCPConfig(); cfg != nil {
		data, err := json.Marshal(cfg)
		if err != nil {
			return fmt.Errorf("marshal mcp config: %w", err)
		}
		if mcpConfigPath, err = writeFile("claude-mcp-*.json", data); err != nil {
			return fmt.Errorf("write mcp config: %w", err)
		}
		spawn.relayMCPWritten = true
	} else if useRelayTools(p.session.Settings) {
		warnRelayToolsUnavailable(p.session.ID, "claude", "relay_mcp_command_unset")
	}
	if p.session.SystemPrompt != "" {
		var err error
		if sysPromptPath, err = writeFile("claude-sysprompt-*.txt", []byte(p.session.SystemPrompt)); err != nil {
			return fmt.Errorf("write system prompt: %w", err)
		}
	}

	args := p.buildClaudeArgs(mcpConfigPath, sysPromptPath)
	program := p.cfg.Launcher.Program(resolveClaudePath(p.cfg.Binary))
	shimmed := shimSpec{
		Binary:         p.cfg.ShimBinary,
		SessionID:      p.session.ID,
		BridgeSocket:   p.cfg.BridgeSocket,
		SandboxProfile: p.cfg.SandboxProfile,
		Identity:       p.cfg.Identity,
		ClaudeToken:    p.cfg.ClaudeToken,
	}
	fds := &spawnFDs{}
	defer fds.closeUnlessStarted()
	var argv []string
	switch {
	case shimmed.ClaudeToken != "" && !shimmed.wanted():
		return errors.New("claude token requires the shim")
	case shimmed.wanted() && shimmed.Binary == "":
		return ErrShimRequired
	case shimmed.Identity != nil && shimmed.BridgeSocket == "":
		return ErrNoBridgeSocket
	case shimmed.wanted():
		cmd, statusR, extraFiles, berr := buildShimCmd(shimmed, program[0], append(slices.Clone(program[1:]), args...))
		if berr != nil {
			return berr
		}
		argv, fds.statusR, fds.extraFiles = cmd.Args, statusR, extraFiles
	default:
		if _, err := exec.LookPath(program[0]); err != nil {
			return fmt.Errorf("failed to start claude: %w", err)
		}
		argv = append(slices.Clone(program), args...)
	}
	env, err := p.buildClaudeEnv(ensurePath(childBaseEnv()))
	if err != nil {
		return err
	}

	st, err := run.Start(runner.LocalSpec{Argv: argv, Env: env, Dir: p.directory, ExtraFiles: fds.extraFiles, Patterns: claudeRunnerPatterns})
	if err != nil {
		return fmt.Errorf("failed to start claude: %w", err)
	}
	fds.started = true
	// Load-bearing: without closing the parent's own copies, the status pipe
	// (and, when present, the identity secret pipe) never reaches EOF.
	for _, f := range fds.extraFiles {
		_ = f.Close()
	}
	if fds.statusR != nil {
		outcome, _ := readShimStatus(fds.statusR)
		_ = fds.statusR.Close()
		var failure error
		switch {
		case outcome.identityRefused:
			failure = fmt.Errorf("%w: session %s", ErrIdentityRefused, p.session.ID)
		case !outcome.started || outcome.spawnFailed:
			failure = fmt.Errorf("%w: errno=%d", ErrSpawnFailed, outcome.spawnErrno)
		}
		if failure != nil {
			p.logRefusedStart(run)
			stopRun(p.session.ID, "this machine", localConn{run: run})
			return failure
		}
	}
	started = true

	root := p.pinRoot(st.PID) // st.PID is the pid cmd.Start returned
	p.mu.Lock()
	p.restoredRun, p.runnerRoot = nil, root
	p.mu.Unlock()
	p.root.Store(root)
	det, state, spawnGen := p.beginDetached(nil, localConnFor(base, p.session.ID), run.Generation)
	state.relayMCPWritten = spawn.relayMCPWritten
	p.runDetached(det, state, spawnGen)
	slog.Info("claude process started", "session", p.session.ID, "model", p.model, "runner", st.PID, "generation", run.Generation)
	return nil
}

// logRefusedStart logs what the agent wrote to stderr before the shim refused
// it, with the launch's secrets masked.
func (p *ClaudeProvider) logRefusedStart(run runner.LocalRun) {
	tail, err := localConn{run: run}.StderrTail(context.Background())
	if err != nil || tail == "" {
		return
	}
	var sawStdout atomic.Bool
	logProviderStderr(io.NopCloser(strings.NewReader(tail)), p.session.ID, "claude", &sawStdout, p.identitySecret(), p.cfg.ClaudeToken)
}

// pinRoot pins pid's start time as the permission-hook root: the runner is the
// ancestor of the shim and every process under it. It returns nil when the
// start time is unreadable, and the hook then denies.
func (p *ClaudeProvider) pinRoot(pid int) *sessionstypes.ProcessRoot {
	info, ok := membership.NewSource().Info(pid)
	if !ok {
		slog.Warn("claude: process start time unreadable; session has no process root", "session", p.session.ID, "pid", pid)
		return nil
	}
	return &sessionstypes.ProcessRoot{PID: pid, StartSec: info.StartSec, StartUsec: info.StartUsec}
}

// attachLocal follows the runner that still lives. It records the process root
// only when runner.json's pid and the kernel's start time of that pid both
// match the root saved when the runner started: a recycled pid, or a runner
// nobody saved, leaves the session without a root and the hook denies.
func (p *ClaudeProvider) attachLocal(st runner.Status) error {
	p.mu.Lock()
	saved := p.restoredRoot
	p.mu.Unlock()
	var root *sessionstypes.ProcessRoot
	if saved != nil && saved.PID == st.State.PID {
		if pinned := p.pinRoot(st.State.PID); pinned != nil && *pinned == *saved {
			root = pinned
		}
	}
	if root == nil {
		slog.Warn("claude: local runner does not match the saved process root; session has no process root", "session", p.session.ID, "runner", st.State.PID)
	}
	p.mu.Lock()
	p.runnerRoot = root
	p.mu.Unlock()
	p.root.Store(root)
	if err := p.attachRun(nil, localConnFor(p.cfg.RunnerBase, p.session.ID), st.Generation); err != nil {
		p.root.Store(nil)
		return err
	}
	return nil
}

// SendEvent tells the runner something happened that the agent's output does
// not show. Only "asking" is defined, and a host session answers nil: its
// asks arrive on stdout.
func (p *ClaudeProvider) SendEvent(ctx context.Context, typ string) error {
	det := p.det
	if det == nil {
		return nil
	}
	return det.conn.SendEvent(ctx, typ)
}
