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
	"github.com/barelyworkingcode/relay/internal/sessions/runner"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
	"github.com/barelyworkingcode/relay/internal/sshhost"
)

// A runner session keeps its agent under the runner, on this machine or on a
// host (docs/ssh-hosts.md, "Detached sessions"). Relay's link carries input to
// the runner's inbox and reads out.jsonl by byte offset, so a dropped link or a
// restart ends neither the agent nor the events it writes while relay is away.
// runFollower is the provider-neutral half of that: a provider hands it hooks
// and reads its lines.

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

// probeSpec is what an attach probe counts. Both are fixed strings; "" never
// matches.
type probeSpec struct {
	// TurnEnded is the runner's TurnEnded pattern.
	TurnEnded string
	// Request marks a line that may wait for an answer.
	Request string
}

// attachProbe is what one round trip to a run directory shows. The provider
// reads controls and requests; the probe parses none of them.
type attachProbe struct {
	size     int64             // end of the last complete line of out.jsonl
	users    int               // .json messages in sent/ and inbox/
	results  int               // lines that hold probeSpec.TurnEnded
	requests []json.RawMessage // lines that hold probeSpec.Request
	controls []json.RawMessage // every line of every .ctl file in sent/ and inbox/
}

// runHooks is what a provider gives the follower.
type runHooks struct {
	// Kind names the provider in logs and error text.
	Kind string
	// Session is the session id, for logs and events.
	Session string
	// Line receives each complete, non-empty stdout line in order. The cursor is
	// already past it.
	Line func(body json.RawMessage, start int64)
	// Exited runs once when the follow loop ends: the runner is gone, or Stop
	// ran. killed is true after Stop.
	Exited func(code int, tail []string, killed bool)
	// Redact masks the launch's secrets in a stderr tail. Nil masks nothing.
	Redact func(lines []string) []string
}

type runFollower struct {
	// host is nil for a run on this machine.
	host *sessionstypes.HostSpec
	gen  string
	// conn reaches the run directory; connFor reaches another generation's.
	conn    runConn
	connFor func(gen string) runConn
	h       runHooks
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
	// finished is set once the runner is known to be gone, so Stop has nothing
	// to stop.
	finished atomic.Bool

	// requests holds the ids already answered or raised, so an attach that
	// replays old output raises none twice.
	reqMu    sync.Mutex
	requests map[string]struct{}

	stopOnce sync.Once
	stopping atomic.Bool
	// stopped closes when the stop has returned; the final exit of a stopped
	// follower waits on it so the exit follows the real end.
	stopped chan struct{}
	done    chan struct{}
}

func newRunFollower(host *sessionstypes.HostSpec, connFor func(gen string) runConn, gen string, offset int64, h runHooks) *runFollower {
	ctx, cancel := context.WithCancel(context.Background())
	f := &runFollower{
		host:    host,
		gen:     gen,
		conn:    connFor(gen),
		connFor: connFor,
		h:       h,
		ctx:     ctx,
		cancel:  cancel,
		stopped: make(chan struct{}),
		done:    make(chan struct{}),
		lineSig: make(chan struct{}, 1),
	}
	f.offset.Store(offset)
	return f
}

// Run starts the follow loop.
func (f *runFollower) Run() { go f.follow() }

// CatchUp reads to the end of the output once, without following it.
func (f *runFollower) CatchUp(ctx context.Context) { f.consume(ctx, false) }

// Cursor is where relay's reading stands.
func (f *runFollower) Cursor() detachedCursor {
	return detachedCursor{Generation: f.gen, Offset: f.offset.Load()}
}

// Claim reports whether id has not been answered or raised before, and marks
// it as handled. An empty id is always new.
func (f *runFollower) Claim(id string) bool {
	if id == "" {
		return true
	}
	f.reqMu.Lock()
	defer f.reqMu.Unlock()
	if _, seen := f.requests[id]; seen {
		return false
	}
	if f.requests == nil {
		f.requests = map[string]struct{}{}
	}
	f.requests[id] = struct{}{}
	return true
}

// SendEvent tells the runner something happened that the agent's output does
// not show.
func (f *runFollower) SendEvent(ctx context.Context, typ string) error {
	return f.conn.SendEvent(ctx, typ)
}

// where names the machine the agent runs on, for messages.
func (f *runFollower) where() string {
	if f.host == nil {
		return "this machine"
	}
	return fmt.Sprintf("host %q", f.host.Name)
}

// markLost marks the link lost and reports whether this call did it.
func (f *runFollower) markLost() bool {
	f.linkMu.Lock()
	defer f.linkMu.Unlock()
	if f.linkLost.Load() {
		return false
	}
	f.restored = make(chan struct{})
	f.linkLost.Store(true)
	return true
}

// markRestored clears the lost mark, wakes every send waiting on it, and
// reports whether this call did it.
func (f *runFollower) markRestored() bool {
	f.linkMu.Lock()
	defer f.linkMu.Unlock()
	if !f.linkLost.Load() {
		return false
	}
	f.linkLost.Store(false)
	close(f.restored)
	f.restored = nil
	return true
}

func (f *runFollower) restoredSignal() <-chan struct{} {
	f.linkMu.Lock()
	defer f.linkMu.Unlock()
	return f.restored
}

// detachedCursor is where a runner session's saved state says relay stopped
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

// follow owns the run from start to end: it reads the agent's lines, rides out
// link drops, and reports the exit through the Exited hook.
func (f *runFollower) follow() {
	defer close(f.done)

	code, tail := f.pump()
	killed := f.stopping.Load()
	if killed {
		<-f.stopped
	}
	f.h.Exited(code, tail, killed)
}

// pump reads the agent's lines until it exits or the follower is stopped.
// A read that ends while the runner lives marks the link lost (one `lost`
// event per outage). A reopened read clears the mark, with `restored`, once it
// has delivered a line or stayed open detachedQuickFail, so a read that keeps
// failing at once produces one `lost` and no churn. The backoff grows across
// reads that deliver nothing and resets after one that delivered lines.
func (f *runFollower) pump() (int, []string) {
	backoff := detachedBackoffMin
	grow := func() {
		if backoff *= 2; backoff > detachedBackoffMax {
			backoff = detachedBackoffMax
		}
	}
	for {
		select {
		case <-f.lineSig:
		default:
		}
		before := f.offset.Load()
		readDone := make(chan struct{})
		watched := make(chan struct{})
		go func() {
			defer close(watched)
			f.confirmRestored(readDone)
		}()
		f.consume(f.ctx, true)
		close(readDone)
		<-watched
		if f.ctx.Err() != nil {
			return -1, nil
		}
		if f.offset.Load() > before {
			backoff = detachedBackoffMin
		}

		st, err := f.status()
		for err == nil && st.Phase == runner.PhaseAlive || err != nil {
			if f.markLost() {
				f.emitHostLink("lost")
			}
			if !sleepCtx(f.ctx, backoff) {
				return -1, nil
			}
			grow()
			if err == nil {
				break
			}
			st, err = f.status()
		}
		if err == nil && st.Phase != runner.PhaseAlive {
			if f.markRestored() {
				f.emitHostLink("restored")
			}
			return f.finish(st)
		}
	}
}

// confirmRestored clears a lost mark once the read has proved itself: a line
// arrived or it stayed open long enough. It ends when the read does.
func (f *runFollower) confirmRestored(readDone <-chan struct{}) {
	if !f.linkLost.Load() {
		return
	}
	t := time.NewTimer(detachedQuickFail)
	defer t.Stop()
	select {
	case <-f.lineSig:
	case <-t.C:
	case <-readDone:
		return
	case <-f.ctx.Done():
		return
	}
	if f.markRestored() {
		f.emitHostLink("restored")
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

func (f *runFollower) emitHostLink(link string) {
	if f.host == nil {
		return
	}
	logging.BeginEvent(context.Background(), "session.host_link").
		Set("session_id", f.h.Session).Set("host_id", f.host.ID).Set("link", link).
		End(logging.OutcomeOK, "", nil)
}

func (f *runFollower) status() (runner.Status, error) {
	ctx, cancel := context.WithTimeout(f.ctx, detachedSendTimeout)
	defer cancel()
	return f.conn.Status(ctx)
}

// finish delivers what the agent wrote last, then reports its exit.
func (f *runFollower) finish(st runner.Status) (int, []string) {
	f.finished.Store(true)
	ctx, cancel := context.WithTimeout(f.ctx, detachedSendTimeout)
	defer cancel()
	f.consume(ctx, false)

	code := -1
	if st.Phase == runner.PhaseExited {
		code = st.Code
	}
	var tail []string
	if code != 0 {
		if out, err := f.conn.StderrTail(ctx); err == nil {
			tail = stderrTailOf(out)
			if f.h.Redact != nil {
				tail = f.h.Redact(tail)
			}
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

// consume reads the output from the saved offset, following it while the
// runner lives when follow is set, and feeds each complete line to the Line
// hook. A partial last line is dropped: the offset stays at its start, so the
// next read begins there.
func (f *runFollower) consume(ctx context.Context, follow bool) {
	stdout, err := f.conn.Read(ctx, f.offset.Load(), follow)
	if err != nil {
		slog.Warn(f.h.Kind+": detached read did not start", "session", f.h.Session, "error", err)
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
		// save that the line triggers (a turn end saves one) records the cursor
		// after that line.
		start := f.offset.Add(int64(len(line))) - int64(len(line))
		if body := bytes.TrimRight(line, "\r\n"); len(body) > 0 {
			f.h.Line(json.RawMessage(body), start)
		}
		select {
		case f.lineSig <- struct{}{}:
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
// timer fires, the caller's ctx ends, the runner is gone or the follower is
// stopped. A caller that has left gets ErrHostLinkDown and nothing is written.
func (f *runFollower) awaitLink(ctx context.Context, wait time.Duration, timer *time.Timer) error {
	down := fmt.Errorf("%w: the message was not sent; retry when %s is reachable", ErrHostLinkDown, f.where())
	if ctx.Err() != nil {
		return down
	}
	for f.linkLost.Load() {
		if wait <= 0 {
			return down
		}
		ch := f.restoredSignal()
		if ch == nil {
			continue
		}
		select {
		case <-ch:
		case <-timer.C:
			return down
		case <-ctx.Done():
			return down
		case <-f.done:
			return fmt.Errorf("%s on %s has exited", f.h.Kind, f.where())
		case <-f.ctx.Done():
			return fmt.Errorf("the session on %s was stopped", f.where())
		}
	}
	return nil
}

// Send queues one line (no trailing newline) in the runner's inbox and returns
// once the runner has written it to the agent. It waits up to wait for a lost
// link to return, then sends once.
// Deliberate: no retry after the command ran. A failure after the command
// started, or a missing receipt, may still have landed the message, and a
// second one would deliver it twice. Only a failure that proves ssh never
// connected is retried: the link is marked lost and the wait begins again.
func (f *runFollower) Send(callerCtx context.Context, kind runner.MessageKind, line []byte, wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		if err := f.awaitLink(callerCtx, wait, timer); err != nil {
			return err
		}
		if callerCtx.Err() != nil {
			return fmt.Errorf("%w: the message was not sent; retry when %s is reachable", ErrHostLinkDown, f.where())
		}
		err := f.conn.Send(f.ctx, kind, line)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, errNoConnect):
			if f.markLost() {
				f.emitHostLink("lost")
			}
		case errors.Is(err, errSendUnknown):
			return fmt.Errorf("%w: the connection to %s ended during the send; the message may have been delivered, so read the session's events before sending again", ErrSendUnknown, f.where())
		case errors.Is(err, runner.ErrNotRunning):
			return fmt.Errorf("the agent is not running on %s", f.where())
		case errors.Is(err, runner.ErrNoReceipt):
			return fmt.Errorf("%w: %s did not confirm delivery in time; the message is queued and may still be delivered, so read the session's events before sending again", ErrSendUnknown, f.where())
		default:
			return fmt.Errorf("send to %s on %s: %w", f.h.Kind, f.where(), err)
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

// Stop ends the follow loop and waits for it. stopAgent false is a release:
// nothing is sent to the runner and the agent keeps running.
func (f *runFollower) Stop(stopAgent bool) {
	f.stopOnce.Do(func() {
		f.stopping.Store(true)
		f.cancel()
		if stopAgent && !f.finished.Load() {
			stopRun(f.h.Kind, f.h.Session, f.where(), f.conn)
		}
		close(f.stopped)
	})
	<-f.done
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
func stopRun(kind, session, where string, conn runConn) {
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
	slog.Warn(kind+": detached agent could not be stopped; it may still run", "session", session, "where", where, "error", err)
}

// StopDetachedRun ends the latest agent of a detached session without
// deleting its run directories. It needs no live provider.
func StopDetachedRun(session string, host *sessionstypes.HostSpec) {
	if DetachedHost(host) {
		stopRun("claude", session, fmt.Sprintf("host %q", host.Name), hostConnFor(host, session)(""))
	}
}

// StopLocalRun is StopDetachedRun for a run on this machine.
func StopLocalRun(base, session string) {
	stopRun("claude", session, "this machine", localConnFor(base, session)(""))
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
func (f *runFollower) Events(ctx context.Context, after string, max int) (lines []json.RawMessage, next string, err error) {
	gen := f.gen
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
	stdout, perr := f.connFor(gen).Read(rctx, offset, false)
	if perr != nil {
		return nil, "", fmt.Errorf("read events on %s: %w", f.where(), perr)
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
		return nil, "", fmt.Errorf("read events on %s: %w", f.where(), werr)
	}
	if rctx.Err() != nil && len(lines) == 0 {
		return nil, "", fmt.Errorf("read events on %s: %w", f.where(), rctx.Err())
	}
	return lines, gen + ":" + strconv.FormatInt(offset, 10), nil
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

// attachProbeScript takes $1 session, $2 generation, $3 the turn-ended string
// and $4 the request marker, and prints what an attach needs in one round trip,
// one tagged line each: "S <n>", the end of the last complete line of
// out.jsonl; "U <n>", the turn messages in sent/ and inbox/; "R <n>", the lines
// that hold $3; "O <line>", every line that holds $4; "I <line>", every line of
// every control file in sent/ and inbox/. Both strings match as fixed strings,
// and an empty one matches nothing.
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
r=0
if [ -n "$3" ]; then r=$(grep -a -c -F -- "$3" "$f"); fi
echo "R $r"
if [ -n "$4" ]; then grep -a -F -- "$4" "$f" | sed 's/^/O /'; fi
for m in "$d"/sent/*.ctl "$d"/inbox/*.ctl; do [ -f "$m" ] && sed 's/^/I /' "$m"; done
exit 0
`

func probeAttach(ctx context.Context, host *sessionstypes.HostSpec, dir sshhost.RunDir, spec probeSpec) (attachProbe, error) {
	remote := sshhost.RemoteCommandForOS(host.OS, "", []string{"sh", "-c", attachProbeScript, "relay-agent", dir.Session, dir.Generation, spec.TurnEnded, spec.Request}, nil)
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
			pr.requests = append(pr.requests, json.RawMessage(body))
		case 'I':
			if body != "" {
				pr.controls = append(pr.controls, json.RawMessage(body))
			}
		}
	}
	if !sawSize {
		return attachProbe{}, fmt.Errorf("unexpected attach answer %q", firstLineOf(stdout))
	}
	return pr, nil
}

func firstLineOf(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
