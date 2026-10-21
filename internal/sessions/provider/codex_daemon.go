package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/barelyworkingcode/relay/internal/sessions/codexdaemon"
	"github.com/barelyworkingcode/relay/internal/sessions/runner"
)

// codexUnsubscribeWait bounds the best-effort goodbye when a daemon-attached
// provider ends.
const codexUnsubscribeWait = 2 * time.Second

// handlerTurnStarted tells the manager a turn began that no send of its own
// started: a terminal client of the daemon started it.
const handlerTurnStarted = "turn_started"

// maxDaemonListPages bounds the thread-list paging of one start.
const maxDaemonListPages = 256

// startDaemon attaches this provider to a thread the codex daemon holds. It
// never starts a program: no runner, launcher, sandbox profile or launch
// identity is used, and there is no fallback to a codex of relay's own. The
// attach sends thread/resume with the thread id alone, so the thread keeps
// every setting it runs under.
func (p *CodexProvider) startDaemon() error {
	if p.session.GetHost() != nil {
		return errors.New("codex: a thread attached through the daemon is local only")
	}
	p.stateMu.Lock()
	thread := p.threadID
	p.stateMu.Unlock()
	if parsed, err := uuid.Parse(thread); err != nil || len(thread) != 36 || parsed.String() != thread {
		return errors.New("codex: the attached thread id is not a UUID")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("codex: no home folder: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexHandshakeTimeout)
	defer cancel()
	conn, err := codexdaemon.Dial(ctx, home)
	if err != nil {
		return fmt.Errorf("attach to the codex daemon: %w", err)
	}

	p.cmd, p.stdin = nil, nil
	p.exitMu.Lock()
	p.spawnGen++
	gen := p.spawnGen
	p.exitMu.Unlock()
	waitDone := make(chan struct{})
	p.waitDone = waitDone
	p.drainTimeout = providerDrainTimeout
	killed := &atomic.Bool{}
	p.killed = killed
	exit := &spawnExit{done: make(chan struct{})}
	p.exit = exit
	p.daemon.Store(conn)
	p.alive.Store(true)
	go p.readDaemon(conn, gen, waitDone, exit, killed)

	deadline := time.After(codexHandshakeTimeout)
	if err := p.requireLoaded(thread, deadline); err != nil {
		p.abortDaemonStart(conn)
		return err
	}
	if err := p.resumeThread(thread, deadline); err != nil {
		p.abortDaemonStart(conn)
		return err
	}
	slog.Info("codex daemon thread attached", "session", p.session.ID, "model", p.slug)
	return nil
}

// abortDaemonStart ends a start that failed after the connection opened. The
// generation moves on first so the reader's exit stays silent.
func (p *CodexProvider) abortDaemonStart(conn *codexdaemon.Conn) {
	p.exitMu.Lock()
	p.spawnGen++
	p.exitMu.Unlock()
	p.alive.Store(false)
	_ = conn.Close()
	p.daemon.CompareAndSwap(conn, nil)
}

// readDaemon feeds every line the daemon sends to processLine until the socket
// closes. A closed socket is the provider's exit; there is no reconnect loop.
func (p *CodexProvider) readDaemon(conn *codexdaemon.Conn, gen uint64, waitDone chan struct{}, exit *spawnExit, killed *atomic.Bool) {
	for {
		raw, err := conn.Recv(context.Background())
		if err != nil {
			break
		}
		p.processLine(raw)
	}
	_ = conn.Close()
	p.daemon.CompareAndSwap(conn, nil)
	p.runExited(gen, killed, exit, waitDone, 0, nil)
}

// requireLoaded fails unless the daemon still holds the thread.
func (p *CodexProvider) requireLoaded(thread string, deadline <-chan time.Time) error {
	params := map[string]any{}
	for pages := 0; pages < maxDaemonListPages; pages++ {
		raw, err := p.call("thread/loaded/list", params, deadline)
		if err != nil {
			return err
		}
		var page struct {
			Data       []string `json:"data"`
			NextCursor *string  `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return fmt.Errorf("thread/loaded/list: %w", err)
		}
		for _, id := range page.Data {
			if id == thread {
				return nil
			}
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			break
		}
		params = map[string]any{"cursor": *page.NextCursor}
	}
	return errors.New("the codex thread is no longer running in codex")
}

// resumeThread subscribes to the thread. The reply is judged on the reader
// goroutine, which owns the turn state it seeds.
func (p *CodexProvider) resumeThread(thread string, deadline <-chan time.Time) error {
	done := make(chan error, 1)
	id, err := p.request(context.Background(), "thread/resume", map[string]any{"threadId": thread}, runner.MessageControl, 0, func(r codexReply) {
		done <- p.seedFromResume(thread, r)
	})
	if err != nil {
		return err
	}
	select {
	case err := <-done:
		return err
	case <-deadline:
		p.dropPending(id)
		return errors.New("thread/resume: timed out")
	case <-p.waitDone:
		p.dropPending(id)
		return errors.New("thread/resume: the codex daemon closed the connection")
	}
}

// seedFromResume checks the reply names the thread and its folder, then takes
// the model and an open turn from it.
func (p *CodexProvider) seedFromResume(thread string, r codexReply) error {
	if r.err != "" {
		return fmt.Errorf("thread/resume: %s", r.err)
	}
	var res struct {
		Cwd    string `json:"cwd"`
		Model  string `json:"model"`
		Thread struct {
			ID    string `json:"id"`
			Cwd   string `json:"cwd"`
			Model string `json:"model"`
			Turns []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"turns"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(r.result, &res); err != nil || res.Thread.ID != thread {
		return errors.New("thread/resume: the reply does not name the thread")
	}
	folders := 0
	for _, cwd := range []string{res.Thread.Cwd, res.Cwd} {
		if cwd == "" {
			continue
		}
		folders++
		if !sameFolder(cwd, p.directory) {
			return errors.New("thread/resume: the thread works in a different folder than the session")
		}
	}
	if folders == 0 {
		return errors.New("thread/resume: the reply names no folder")
	}
	if model := firstNonEmpty(res.Model, res.Thread.Model); model != "" {
		p.slug = model
	}
	if n := len(res.Thread.Turns); n > 0 && res.Thread.Turns[n-1].Status == "inProgress" {
		p.stateMu.Lock()
		p.turnID = res.Thread.Turns[n-1].ID
		p.attachedTurnOpen = true
		p.stateMu.Unlock()
		p.turnActive = true
		p.session.SetProcessing(true)
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// sameFolder compares two folders cleaned, with symlinks resolved where they
// can be.
func sameFolder(a, b string) bool {
	resolve := func(p string) string {
		p = filepath.Clean(p)
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return p
	}
	return filepath.IsAbs(a) && filepath.IsAbs(b) && resolve(a) == resolve(b)
}

// forOwnThread reports whether a notification belongs to this provider's
// thread. A notification that names no thread passes; another thread's does not.
func (p *CodexProvider) forOwnThread(params json.RawMessage) bool {
	var ev struct {
		ThreadID string `json:"threadId"`
	}
	if json.Unmarshal(params, &ev) != nil || ev.ThreadID == "" {
		return true
	}
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	return ev.ThreadID == p.threadID
}

// codexStatusType is a thread status's type: a string or an object with one.
func codexStatusType(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &o)
	return o.Type
}

// translateDaemonThread follows the thread's own status: active is a turn
// open, idle is idle, systemError is errored. A closed thread ends the
// connection, which is the provider's exit.
func (p *CodexProvider) translateDaemonThread(method string, params json.RawMessage) {
	if method == "thread/closed" {
		slog.Info("codex thread closed", "session", p.session.ID)
		if conn := p.daemon.Load(); conn != nil {
			_ = conn.Close()
		}
		return
	}
	var ev struct {
		Status json.RawMessage `json:"status"`
	}
	if json.Unmarshal(params, &ev) != nil {
		return
	}
	switch codexStatusType(ev.Status) {
	case "active":
		if !p.turnActive {
			p.resetTurnState()
			p.turnActive = true
			p.emitter.SystemInit(p.slug, p.directory, nil, nil)
			p.emitter.MessageStart("")
			p.handler(handlerTurnStarted, nil)
		}
	case "idle":
		if p.turnActive {
			p.translateTurnCompleted(nil)
		}
	case "systemError":
		slog.Warn("codex thread reports a system error", "session", p.session.ID)
		data, _ := json.Marshal(map[string]string{"error": "the codex thread reported a system error"})
		p.handler("error", data)
		if p.turnActive {
			p.translateTurnCompleted(nil)
		}
	}
}

// detachDaemon ends this connection and only it: a best-effort
// thread/unsubscribe, then close. It sends no turn/interrupt, deletes no
// thread and removes no folder. released leaves the session's board state as it
// was, as a relay-sessions shutdown does.
func (p *CodexProvider) detachDaemon(conn *codexdaemon.Conn, released bool) {
	if released {
		p.released.Store(true)
	}
	if p.killed != nil {
		p.killed.Store(true)
	}
	if p.alive.Load() {
		p.stateMu.Lock()
		thread := p.threadID
		p.stateMu.Unlock()
		if _, err := p.call("thread/unsubscribe", map[string]any{"threadId": thread}, time.After(codexUnsubscribeWait)); err != nil {
			slog.Debug("codex unsubscribe not confirmed", "session", p.session.ID, "error", err)
		}
	}
	p.alive.Store(false)
	_ = conn.Close()
	if p.exit != nil {
		<-p.exit.done
	}
	slog.Info("codex daemon attachment ended", "session", p.session.ID)
}

// ownTurnRequest reports whether a server request is for the turn this provider
// started on its thread. Any other request, a terminal's turn included, belongs
// to the person at that terminal: relay sends no answer, so codex's own prompt
// stays open. A request that names no turn counts only while relay's turn is the
// open one. A request that names no thread, or another one, is never answered.
func (p *CodexProvider) ownTurnRequest(env codexEnvelope) bool {
	var req struct {
		ThreadID       string `json:"threadId"`
		ConversationID string `json:"conversationId"`
		TurnID         string `json:"turnId"`
	}
	_ = json.Unmarshal(env.Params, &req)
	p.stateMu.Lock()
	thread, mine := p.threadID, p.relayTurn
	p.stateMu.Unlock()
	named := firstNonEmpty(req.ThreadID, req.ConversationID)
	own := mine != "" && thread != "" && named == thread && (req.TurnID == "" || req.TurnID == mine)
	if !own {
		slog.Info("codex: server request left for the terminal", "session", p.session.ID, "method", env.Method, "turn", req.TurnID)
	}
	return own
}
