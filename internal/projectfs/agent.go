package projectfs

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/sshhost"
)

//go:embed agent/fsagent.js
var fsAgentSource string

const (
	agentProtocolVersion = 2
	// HostRequestTimeout bounds every host request, including the wait for a
	// connecting agent to become ready.
	HostRequestTimeout = 30 * time.Second
	helloTimeout       = 10 * time.Second
	reconnectMin       = time.Second
	reconnectMax       = 30 * time.Second
	stderrTail         = 2048
)

// agentConn is one running agent process. done closes when cmd.Wait returns.
type agentConn struct {
	stdin   io.WriteCloser
	writeMu sync.Mutex

	mu      sync.Mutex
	pending map[int64]chan []byte
	dead    bool
	done    chan struct{}
}

func (c *agentConn) register(id int64) (chan []byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dead {
		return nil, false
	}
	ch := make(chan []byte, 1)
	c.pending[id] = ch
	return ch, true
}

func (c *agentConn) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *agentConn) deliver(id int64, line []byte) {
	c.mu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ch != nil {
		ch <- line
	}
}

func (c *agentConn) markDead() {
	c.mu.Lock()
	c.dead = true
	c.pending = map[int64]chan []byte{}
	c.mu.Unlock()
}

// rootWatch is one watched root on one host: the sinks sharing a single
// agent-side watcher. ready closes once the first watch request is answered.
type rootWatch struct {
	sinks map[int64]func(Event)
	ready chan struct{}
	err   error
}

// hostAgent owns one host's agent process: spawn, hello, request multiplexing,
// watch re-arming and reconnect with backoff.
type hostAgent struct {
	pool *HostPool
	fp   string
	ctx  context.Context
	stop context.CancelFunc

	mu      sync.Mutex
	host    config.Host
	status  string
	errMsg  string
	changed chan struct{}
	conn    *agentConn
	watches map[string]*rootWatch

	nextID     atomic.Int64
	nextSinkID atomic.Int64
}

func newHostAgent(pool *HostPool, h config.Host, fp string) *hostAgent {
	ctx, cancel := context.WithCancel(context.Background())
	a := &hostAgent{
		pool: pool, fp: fp, ctx: ctx, stop: cancel, host: h,
		status: StatusConnecting, changed: make(chan struct{}),
		watches: map[string]*rootWatch{},
	}
	if h.Probe == nil || h.Probe.NodePath == "" {
		a.status = StatusUnreachable
		a.errMsg = fmt.Sprintf("host %q has no node: run a probe", h.Name)
		return a
	}
	return a
}

func (a *hostAgent) start() {
	a.pool.emit(a.snapshot())
	if a.status == StatusUnreachable {
		return
	}
	go a.run()
}

func (a *hostAgent) snapshot() HostStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return HostStatus{HostID: a.host.ID, Name: a.host.Name, Status: a.status, Error: a.errMsg}
}

func (a *hostAgent) setName(name string) {
	a.mu.Lock()
	a.host.Name = name
	a.mu.Unlock()
}

func (a *hostAgent) setStatus(status, errMsg string) {
	a.mu.Lock()
	if a.status == status && a.errMsg == errMsg {
		a.mu.Unlock()
		return
	}
	a.status, a.errMsg = status, errMsg
	close(a.changed)
	a.changed = make(chan struct{})
	a.mu.Unlock()
	a.pool.emit(a.snapshot())
}

func (a *hostAgent) unreachableErr() *Error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.errMsg != "" && a.status == StatusUnreachable {
		return Errf(CodeHostUnreachable, a.errMsg)
	}
	return Errf(CodeHostUnreachable, fmt.Sprintf("host %q unreachable", a.host.Name))
}

// run connects, serves until the process exits, then backs off on the
// injected clock and connects again, until the agent is dropped.
func (a *hostAgent) run() {
	delay := reconnectMin
	for {
		reason, connected := a.runOnce()
		if a.ctx.Err() != nil {
			return
		}
		if connected {
			delay = reconnectMin
		}
		slog.Info("host file agent down", "host_id", a.hostID(), "reason", reason)
		a.setStatus(StatusUnreachable, reason)
		select {
		case <-a.pool.clk.After(delay):
		case <-a.ctx.Done():
			return
		}
		if delay *= 2; delay > reconnectMax {
			delay = reconnectMax
		}
		a.setStatus(StatusConnecting, "")
	}
}

func (a *hostAgent) hostID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.host.ID
}

func (a *hostAgent) hostName() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.host.Name
}

type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > stderrTail {
		t.buf = t.buf[len(t.buf)-stderrTail:]
	}
	t.mu.Unlock()
	return len(p), nil
}

func (t *tailBuffer) lastLine() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := strings.Split(strings.TrimSpace(string(t.buf)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// runOnce runs one agent process to its exit. connected reports whether the
// hello reply arrived, which resets the backoff.
func (a *hostAgent) runOnce() (reason string, connected bool) {
	a.mu.Lock()
	h := a.host
	a.mu.Unlock()
	argv, err := a.pool.argv(h, sshhost.NodeLauncher(h.Probe.NodePath, fsAgentSource))
	if err != nil {
		return "spawn failed: " + err.Error(), false
	}
	cmd := exec.CommandContext(a.ctx, argv[0], argv[1:]...)
	// ssh's ControlPersist master can hold inherited pipes after the client
	// exits; without a bound, Wait would block on them.
	cmd.WaitDelay = 2 * time.Second
	stderr := &tailBuffer{}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "spawn failed: " + err.Error(), false
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "spawn failed: " + err.Error(), false
	}
	if err := cmd.Start(); err != nil {
		return "spawn failed: " + err.Error(), false
	}
	conn := &agentConn{stdin: stdin, pending: map[int64]chan []byte{}, done: make(chan struct{})}

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		a.readLoop(conn, stdout)
	}()
	go func() {
		<-readerDone
		_ = cmd.Wait()
		conn.markDead()
		close(conn.done)
	}()
	kill := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}

	exited := func() string {
		<-conn.done
		if line := stderr.lastLine(); line != "" {
			return line
		}
		return fmt.Sprintf("host %q unreachable", a.hostName())
	}

	hello, err := a.exchange(a.ctx, conn, a.pool.clk.After(helloTimeout), "hello", map[string]any{"version": agentProtocolVersion})
	if err != nil {
		kill()
		reason := exited()
		if CodeOf(err) == CodeTimeout {
			reason = fmt.Sprintf("host %q did not answer hello", a.hostName())
		}
		return reason, false
	}
	var hr struct {
		Version int `json:"version"`
	}
	_ = json.Unmarshal(hello, &hr)
	if hr.Version != agentProtocolVersion {
		kill()
		_ = exited()
		return fmt.Sprintf("host %q agent speaks protocol %d, relay needs %d", a.hostName(), hr.Version, agentProtocolVersion), false
	}

	a.rearm(conn)
	a.mu.Lock()
	a.conn = conn
	a.mu.Unlock()
	a.setStatus(StatusConnected, "")

	select {
	case <-conn.done:
	case <-a.ctx.Done():
		kill()
		<-conn.done
	}
	a.mu.Lock()
	if a.conn == conn {
		a.conn = nil
	}
	a.mu.Unlock()
	if line := stderr.lastLine(); line != "" {
		return line, true
	}
	return fmt.Sprintf("host %q unreachable", a.hostName()), true
}

// rearm re-sends watch for every root that still has a sink, before the agent
// is announced as connected, so a connected status means watches are live.
func (a *hostAgent) rearm(conn *agentConn) {
	a.mu.Lock()
	var roots []string
	for root, rw := range a.watches {
		select {
		case <-rw.ready:
			if rw.err == nil && len(rw.sinks) > 0 {
				roots = append(roots, root)
			}
		default:
		}
	}
	a.mu.Unlock()
	for _, root := range roots {
		timeout := a.pool.clk.After(HostRequestTimeout)
		if _, err := a.exchange(a.ctx, conn, timeout, "watch", map[string]any{"root": root}); err != nil {
			slog.Warn("host watch re-arm failed", "host_id", a.hostID(), "root", root, "error", err)
		}
	}
}

type agentHead struct {
	ID    int64  `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error"`
	Code  string `json:"code"`
	Size  int64  `json:"size"`
	Event string `json:"event"`
	Root  string `json:"root"`
	Path  string `json:"path"`
	Kind  string `json:"kind"`
}

func (a *hostAgent) readLoop(conn *agentConn, r io.Reader) {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			a.handleLine(conn, line)
		}
		if err != nil {
			return
		}
	}
}

func (a *hostAgent) handleLine(conn *agentConn, line []byte) {
	var head agentHead
	if err := json.Unmarshal(line, &head); err != nil {
		return
	}
	if head.Event != "" {
		if head.Event == "fs" {
			a.dispatch(head.Root, Event{Path: head.Path, Kind: normalizeKind(head.Kind)})
		}
		return
	}
	conn.deliver(head.ID, line)
}

func normalizeKind(k string) string {
	if k == KindRename {
		return KindRename
	}
	return KindChange
}

func (a *hostAgent) dispatch(root string, ev Event) {
	a.mu.Lock()
	rw := a.watches[root]
	var sinks []func(Event)
	if rw != nil {
		for _, s := range rw.sinks {
			sinks = append(sinks, s)
		}
	}
	a.mu.Unlock()
	for _, s := range sinks {
		s(ev)
	}
}

// exchange sends one request on conn and waits for its reply, the timeout
// channel, ctx, or the process exit. A reply with ok:false becomes *Error.
func (a *hostAgent) exchange(ctx context.Context, conn *agentConn, timeout <-chan time.Time, op string, params map[string]any) (json.RawMessage, error) {
	id := a.nextID.Add(1)
	ch, ok := conn.register(id)
	if !ok {
		return nil, a.unreachableErr()
	}
	req := make(map[string]any, len(params)+2)
	for k, v := range params {
		req[k] = v
	}
	req["id"] = id
	req["op"] = op
	body, err := json.Marshal(req)
	if err != nil {
		conn.forget(id)
		return nil, Errf(CodeError, "encode request: "+err.Error())
	}
	conn.writeMu.Lock()
	_, werr := conn.stdin.Write(append(body, '\n'))
	conn.writeMu.Unlock()
	if werr != nil {
		conn.forget(id)
		return nil, a.unreachableErr()
	}
	select {
	case line := <-ch:
		return decodeReply(line)
	case <-conn.done:
		select {
		case line := <-ch:
			return decodeReply(line)
		default:
		}
		return nil, a.unreachableErr()
	case <-ctx.Done():
		conn.forget(id)
		a.cancelRemote(conn, op, id)
		return nil, ctx.Err()
	case <-timeout:
		conn.forget(id)
		a.cancelRemote(conn, op, id)
		return nil, Errf(CodeTimeout, fmt.Sprintf("host %q request timed out", a.hostName()))
	}
}

// cancelRemote tells the agent to stop a long search; the reply is dropped.
func (a *hostAgent) cancelRemote(conn *agentConn, op string, target int64) {
	if op != "search" {
		return
	}
	body, _ := json.Marshal(map[string]any{"id": a.nextID.Add(1), "op": "cancel", "target": target})
	conn.writeMu.Lock()
	_, _ = conn.stdin.Write(append(body, '\n'))
	conn.writeMu.Unlock()
}

func decodeReply(line []byte) (json.RawMessage, error) {
	var head agentHead
	if err := json.Unmarshal(line, &head); err != nil {
		return nil, Errf(CodeError, "malformed agent reply")
	}
	if !head.OK {
		return nil, &Error{Code: wireCode(head.Code), Msg: head.Error, Size: head.Size}
	}
	return line, nil
}

// wireCode keeps an agent code only when it is one the contract defines.
func wireCode(c string) string {
	switch c {
	case CodeInvalid, CodeTraversal, CodeSymlink, CodeEACCES, CodeENOENT, CodeEISDIR,
		CodeENOTDIR, CodeEEXIST, CodeTooLarge, CodeGitMissing, CodeTimeout, CodeUnsupported:
		return c
	}
	return CodeError
}

// call waits for the agent to be connected, then sends one request. The
// 30 s budget covers the wait and the round trip.
func (a *hostAgent) call(ctx context.Context, op string, params map[string]any) (json.RawMessage, error) {
	timeout := a.pool.clk.After(HostRequestTimeout)
	for {
		a.mu.Lock()
		status, changed, conn := a.status, a.changed, a.conn
		a.mu.Unlock()
		switch status {
		case StatusConnected:
			if conn == nil {
				return nil, a.unreachableErr()
			}
			return a.exchange(ctx, conn, timeout, op, params)
		case StatusUnreachable:
			return nil, a.unreachableErr()
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout:
			return nil, Errf(CodeTimeout, fmt.Sprintf("host %q request timed out", a.hostName()))
		}
	}
}

// addWatch registers sink for root and returns once the agent has acked a
// live watcher. The stop func releases the sink and, for the last one, the
// agent-side watcher.
func (a *hostAgent) addWatch(ctx context.Context, root string, sink func(Event)) (func(), error) {
	sinkID := a.nextSinkID.Add(1)
	a.mu.Lock()
	rw := a.watches[root]
	first := rw == nil
	if first {
		rw = &rootWatch{sinks: map[int64]func(Event){}, ready: make(chan struct{})}
		a.watches[root] = rw
	}
	rw.sinks[sinkID] = sink
	a.mu.Unlock()

	if first {
		_, err := a.call(ctx, "watch", map[string]any{"root": root})
		a.mu.Lock()
		rw.err = err
		if err != nil && a.watches[root] == rw {
			delete(a.watches, root)
		}
		a.mu.Unlock()
		close(rw.ready)
		if err != nil {
			return nil, err
		}
	} else {
		select {
		case <-rw.ready:
		case <-ctx.Done():
			a.dropSink(root, rw, sinkID)
			return nil, ctx.Err()
		}
		if rw.err != nil {
			return nil, rw.err
		}
	}

	var once sync.Once
	return func() { once.Do(func() { a.dropSink(root, rw, sinkID) }) }, nil
}

func (a *hostAgent) dropSink(root string, rw *rootWatch, sinkID int64) {
	a.mu.Lock()
	delete(rw.sinks, sinkID)
	last := len(rw.sinks) == 0 && a.watches[root] == rw
	if last {
		delete(a.watches, root)
	}
	conn := a.conn
	a.mu.Unlock()
	if last && conn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), HostRequestTimeout)
		defer cancel()
		_, _ = a.exchange(ctx, conn, a.pool.clk.After(HostRequestTimeout), "unwatch", map[string]any{"root": root})
	}
}

func (a *hostAgent) shutdown() {
	a.stop()
}
