package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/gorilla/websocket"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/project"
	"github.com/barelyworkingcode/relay/internal/sessions/events"
)

// The terminal WebSocket dialect relay-sessions speaks (internal/sessions/api).
// Only the fields this viewer reads or writes are named.
type wsTerminalFrame struct {
	Type       string `json:"type"`
	TerminalID string `json:"terminalId,omitempty"`
	Data       string `json:"data,omitempty"`
	Cols       int    `json:"cols,omitempty"`
	Rows       int    `json:"rows,omitempty"`
	ExitCode   int    `json:"exitCode,omitempty"`
	State      string `json:"state,omitempty"`
	Scrollback string `json:"scrollback,omitempty"`
	Message    string `json:"message,omitempty"`
}

// maxQueuedOutputBytes bounds output waiting for a client that is not
// reading. Past it the session is ended rather than dropping bytes silently:
// a terminal with a hole in its output is worse than a closed one.
const maxQueuedOutputBytes = 32 << 20

// streamChunkBytes keeps every output frame far below the bridge's frame
// ceiling however large a scrollback replay is.
const streamChunkBytes = 64 << 10

// resolveSandboxProject maps a directory to the one local project holding it.
// Several holding projects is a refusal unless selector (id or name) names
// exactly one of them: a directory that two projects claim has no safe default,
// and a selector never reaches a project that does not hold the directory.
// Remote and SSH-hosted projects are skipped: their path is not on this disk.
func resolveSandboxProject(settings *config.Settings, cwd, selector string) (*config.Project, *bridge.SandboxRefusal) {
	cwd = filepath.Clean(cwd)
	if !filepath.IsAbs(cwd) {
		return nil, &bridge.SandboxRefusal{Reason: bridge.SandboxReasonInvalidRequest, Message: "the working directory must be an absolute path"}
	}
	// Resolved once here, never empty: DirWithin answers true for an empty
	// directory.
	dir := realpath(cwd)

	var holding []*config.Project
	for i := range settings.Projects {
		p := &settings.Projects[i]
		if p.IsRemote() || p.IsHosted() || p.Path == "" {
			continue
		}
		if project.DirWithin(dir, p.Path) {
			holding = append(holding, p)
		}
	}
	if len(holding) == 0 {
		return nil, &bridge.SandboxRefusal{Reason: bridge.SandboxReasonNoProject,
			Message: fmt.Sprintf("%s is not inside any registered relay project", dir)}
	}

	picked := holding
	if selector != "" {
		picked = slices.DeleteFunc(slices.Clone(holding), func(p *config.Project) bool {
			return p.ID != selector && p.Name != selector
		})
		if len(picked) == 0 {
			return nil, &bridge.SandboxRefusal{Reason: bridge.SandboxReasonProjectMismatch, Projects: projectLabels(holding),
				Message: fmt.Sprintf("project %q does not contain %s; the projects that do are: %s", selector, dir, strings.Join(projectLabels(holding), ", "))}
		}
	}
	if len(picked) > 1 {
		return nil, &bridge.SandboxRefusal{Reason: bridge.SandboxReasonProjectAmbiguous, Projects: projectLabels(picked),
			Message: fmt.Sprintf("%s is inside more than one project (%s); choose one with --project <name-or-id>", dir, strings.Join(projectLabels(picked), ", "))}
	}
	return picked[0], nil
}

func projectLabels(ps []*config.Project) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, fmt.Sprintf("%s (%s)", p.Name, p.ID))
	}
	sort.Strings(out)
	return out
}

var _ bridge.SandboxAttacher = (*appRouter)(nil)

// SandboxAttach implements bridge.SandboxAttacher: it launches a terminal
// through the same core the HTTP route uses, then joins it as a viewer so the
// caller's bytes can be relayed. It leaves no session running on any error.
func (r *appRouter) SandboxAttach(ctx context.Context, req bridge.SandboxAttachRequest) (bridge.SandboxAttachment, error) {
	d := r.sessionDeps
	if !d.ready() || d.sessions == nil {
		return nil, &bridge.SandboxRefusal{Reason: bridge.SandboxReasonUnavailable, Message: "the session host is not available"}
	}

	settings := config.FreshSettings(r.store)
	proj, refusal := resolveSandboxProject(settings, req.Cwd, req.Project)
	if refusal != nil {
		return nil, refusal
	}

	pid := bridge.CallerPIDFromContext(ctx)
	proc, parent := audit.ProcessNames(pid)
	launchReq := LaunchRequest{
		Caller:     LaunchCaller{Operator: &OperatorCaller{PID: pid, Proc: proc, Parent: parent}},
		ProjectID:  proj.ID,
		Kind:       KindPTY,
		Directory:  req.Cwd,
		TemplateID: req.Template,
		Cols:       req.Cols,
		Rows:       req.Rows,
	}
	result, _, launchRefusal, err := d.launch(ctx, launchReq)
	switch {
	case launchRefusal != nil:
		return nil, sandboxLaunchRefusal(settings, proj, req.Template, launchRefusal)
	case errors.Is(err, errSessionHostUnavailable):
		return nil, &bridge.SandboxRefusal{Reason: bridge.SandboxReasonUnavailable, Message: "the session host is not running"}
	case err != nil:
		return nil, &bridge.SandboxRefusal{Reason: bridge.SandboxReasonUnavailable, Message: "relay could not start the session; see relay's log"}
	}

	// From here the session is running and relay owns ending it.
	att, err := joinSandboxSession(ctx, d, result.SessionID, proj.Name)
	if err != nil {
		slog.Warn("sandbox: attach failed; ending the session", "session", result.SessionID, "error", err)
		endSandboxSession(ctx, d, result.SessionID, "sandbox_attach_failed")
		return nil, &bridge.SandboxRefusal{Reason: bridge.SandboxReasonUnavailable, Message: "relay started the session but could not attach to it; it has been ended"}
	}
	att.result.TemplateID = req.Template
	return att, nil
}

func sandboxLaunchRefusal(settings *config.Settings, proj *config.Project, template string, lr *LaunchRefusal) *bridge.SandboxRefusal {
	if lr.Code != "template_not_allowed" {
		return &bridge.SandboxRefusal{Reason: bridge.SandboxReasonLaunchRefused, Message: lr.Message}
	}
	known := slices.ContainsFunc(config.EffectiveTerminalTemplates(settings), func(t config.TerminalTemplate) bool { return t.ID == template })
	if !known {
		return &bridge.SandboxRefusal{Reason: bridge.SandboxReasonUnknownTemplate,
			Message: fmt.Sprintf("there is no terminal template %q", template)}
	}
	return &bridge.SandboxRefusal{Reason: bridge.SandboxReasonTemplateDenied,
		Message: fmt.Sprintf("terminal template %q is not allowed for project %q; add it to the project's allowed templates in Relay's Projects settings", template, proj.Name)}
}

// sandboxAttachment is a launched session with relay's viewer already joined.
type sandboxAttachment struct {
	deps      sessionRouteDeps
	sessionID string
	result    bridge.SandboxAttachResult

	ws  *websocket.Conn
	wmu sync.Mutex // gorilla allows one concurrent writer

	// early holds what arrived between the join request and its answer:
	// join registers the viewer before snapshotting scrollback, so output can
	// appear both there and live, and scrollback has to be written first.
	scrollback []byte
	early      []wsEvent

	terminateOnce sync.Once

	// hostClosed is written by writeClient and read only after Serve's
	// goroutines have finished.
	hostClosed bool
}

// endSandboxSession is the one way relay ends a sandbox session it started:
// the host terminate, then the identity and model-key bookkeeping the launch
// minted, which a session ended this way would otherwise leave to a
// SessionExited report that may never come.
func endSandboxSession(ctx context.Context, d sessionRouteDeps, sessionID, reason string) {
	if err := d.host().Terminate(context.WithoutCancel(ctx), sessionID, reason); err != nil {
		slog.Warn("sandbox: terminate failed", "session", sessionID, "reason", reason, "error", err)
	}
	if acc, ok := d.accounting.take(sessionID); ok {
		acc.end(d.modelKeys)
	}
}

func joinSandboxSession(ctx context.Context, d sessionRouteDeps, sessionID, projectName string) (*sandboxAttachment, error) {
	host := d.host()
	ws, err := host.DialWS(ctx)
	if err != nil {
		return nil, err
	}
	// The handshake wait has no timer: it ends when the answer arrives, the
	// host drops the connection, or relay shuts down.
	stop := context.AfterFunc(ctx, func() { _ = ws.Close() })
	defer stop()

	a := &sandboxAttachment{deps: d, sessionID: sessionID, ws: ws}
	a.result = bridge.SandboxAttachResult{SessionID: sessionID, ProjectName: projectName}
	if err := a.writeWS(wsTerminalFrame{Type: events.WSMsgJoinTerminal, TerminalID: sessionID}); err != nil {
		_ = ws.Close()
		return nil, err
	}
	for {
		var f wsTerminalFrame
		if err := ws.ReadJSON(&f); err != nil {
			_ = ws.Close()
			return nil, fmt.Errorf("join: %w", err)
		}
		if f.Type == events.WSMsgError {
			_ = ws.Close()
			return nil, fmt.Errorf("join refused: %s", f.Message)
		}
		if f.TerminalID != sessionID {
			continue
		}
		switch f.Type {
		case events.WSMsgTerminalJoined:
			sb, err := base64.StdEncoding.DecodeString(f.Scrollback)
			if err != nil {
				_ = ws.Close()
				return nil, fmt.Errorf("join: scrollback: %w", err)
			}
			a.scrollback = sb
			a.result.Cols, a.result.Rows = f.Cols, f.Rows
			return a, nil
		default:
			if ev, ok := decodeWSEvent(f); ok {
				a.early = append(a.early, ev)
			}
		}
	}
}

func (a *sandboxAttachment) Result() bridge.SandboxAttachResult { return a.result }

func (a *sandboxAttachment) Abort() {
	_ = a.ws.Close()
	a.terminate(context.Background(), "sandbox_attach_failed")
}

func (a *sandboxAttachment) writeWS(f wsTerminalFrame) error {
	a.wmu.Lock()
	defer a.wmu.Unlock()
	return a.ws.WriteJSON(f)
}

type wsEvent struct {
	output []byte
	exit   bool
	code   int
	closed bool
}

func decodeWSEvent(f wsTerminalFrame) (wsEvent, bool) {
	switch f.Type {
	case events.WSMsgTerminalOutput:
		data, err := base64.StdEncoding.DecodeString(f.Data)
		if err != nil {
			return wsEvent{}, false
		}
		return wsEvent{output: data}, true
	case events.WSMsgTerminalExit:
		return wsEvent{exit: true, code: f.ExitCode}, true
	case events.WSMsgTerminalClosed:
		return wsEvent{closed: true}, true
	}
	return wsEvent{}, false
}

// outputQueue decouples the WebSocket reader from the client writer: the host
// cuts a viewer that stops reading, so the reader must never wait on a slow
// terminal.
type outputQueue struct {
	mu       sync.Mutex
	cond     *sync.Cond
	items    []wsEvent
	bytes    int
	closed   bool
	overflow bool
}

func newOutputQueue() *outputQueue {
	q := &outputQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *outputQueue) push(ev wsEvent) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if q.bytes+len(ev.output) > maxQueuedOutputBytes {
		q.overflow, q.closed = true, true
		q.cond.Broadcast()
		return
	}
	q.items = append(q.items, ev)
	q.bytes += len(ev.output)
	q.cond.Signal()
}

// pop blocks for the next event; ok is false once the queue is closed and
// drained.
func (q *outputQueue) pop() (wsEvent, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.items) == 0 {
		return wsEvent{}, false
	}
	ev := q.items[0]
	q.items = q.items[1:]
	q.bytes -= len(ev.output)
	return ev, true
}

func (q *outputQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.cond.Broadcast()
}

// Serve relays until the client goes away, the session ends or relay shuts
// down. There is no deadline anywhere on this path: a machine that sleeps
// leaves the Unix sockets and the WebSocket intact, and only an explicit
// disconnect or an exit ends the session.
func (a *sandboxAttachment) Serve(ctx context.Context, fc *bridge.FrameConn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	q := newOutputQueue()
	for _, ev := range a.early {
		q.push(ev)
	}
	stop := context.AfterFunc(ctx, func() {
		q.close()
		_ = a.ws.Close()
		_ = fc.Close()
	})
	defer stop()

	var sessionGone bool
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		// Closes the queue rather than cancelling: what the host sent last,
		// possibly the exit, must still reach the client.
		defer wg.Done()
		defer q.close()
		a.readWS(q)
	}()
	go func() {
		defer wg.Done()
		defer cancel()
		sessionGone = a.writeClient(fc, q)
	}()
	go func() {
		defer wg.Done()
		defer cancel()
		a.readClient(fc)
	}()
	<-ctx.Done()
	wg.Wait()

	switch {
	case sessionGone:
	case a.hostClosed:
		// Deliberate: the host already dropped the session, but the launch's
		// identity and model-key bookkeeping still needs ending, and Terminate
		// of an unknown id is a no-op.
		a.terminate(ctx, "sandbox_session_closed")
	default:
		a.terminate(ctx, "sandbox_client_gone")
	}
}

func (a *sandboxAttachment) terminate(ctx context.Context, reason string) {
	a.terminateOnce.Do(func() { endSandboxSession(ctx, a.deps, a.sessionID, reason) })
}

func (a *sandboxAttachment) readWS(q *outputQueue) {
	for {
		var f wsTerminalFrame
		if err := a.ws.ReadJSON(&f); err != nil {
			return
		}
		if f.TerminalID != a.sessionID {
			continue
		}
		if ev, ok := decodeWSEvent(f); ok {
			q.push(ev)
		}
	}
}

// writeClient sends scrollback, then queued output, to the client and reports
// whether the session is known to be over (so no terminate is needed).
func (a *sandboxAttachment) writeClient(fc *bridge.FrameConn, q *outputQueue) (sessionGone bool) {
	send := func(data []byte) bool {
		for len(data) > 0 {
			n := min(len(data), streamChunkBytes)
			if err := fc.WriteValue(bridge.StreamFrame{Type: bridge.StreamOutput, Data: data[:n]}); err != nil {
				return false
			}
			data = data[n:]
		}
		return true
	}
	if !send(a.scrollback) {
		return false
	}
	for {
		ev, ok := q.pop()
		if !ok {
			if q.overflow {
				slog.Warn("sandbox: client not reading; ending the session", "session", a.sessionID)
			}
			return false
		}
		switch {
		case ev.exit:
			_ = fc.WriteValue(bridge.StreamFrame{Type: bridge.StreamExit, Code: ev.code})
			return true
		case ev.closed:
			a.hostClosed = true
			return false
		default:
			if !send(ev.output) {
				return false
			}
		}
	}
}

func (a *sandboxAttachment) readClient(fc *bridge.FrameConn) {
	for {
		var f bridge.StreamFrame
		if err := fc.ReadValue(&f); err != nil {
			if !errors.Is(err, io.EOF) {
				slog.Debug("sandbox: client stream ended", "session", a.sessionID, "error", err)
			}
			return
		}
		var out wsTerminalFrame
		switch f.Type {
		case bridge.StreamInput:
			out = wsTerminalFrame{Type: events.WSMsgTerminalInput, TerminalID: a.sessionID, Data: base64.StdEncoding.EncodeToString(f.Data)}
		case bridge.StreamResize:
			if f.Cols <= 0 || f.Rows <= 0 {
				continue
			}
			out = wsTerminalFrame{Type: events.WSMsgTerminalResize, TerminalID: a.sessionID, Cols: f.Cols, Rows: f.Rows}
		default:
			continue
		}
		if err := a.writeWS(out); err != nil {
			return
		}
	}
}
