package api

import (
	"encoding/base64"
	"encoding/json"
	"sync"

	"github.com/barelyworkingcode/relay/internal/sessions/events"
	"github.com/barelyworkingcode/relay/internal/sessions/terminal"
)

// TerminalHandlers registers the terminal_* WS message types onto a Hub. It
// tracks viewer↔connection binding itself (terminalID -> set of connection
// IDs, and the reverse) — the Hub deliberately carries no session/terminal
// knowledge (ws.go's own doc comment), so nothing upstream does this for a
// handler package.
type TerminalHandlers struct {
	hub *Hub
	mgr *terminal.Manager

	mu      sync.Mutex
	viewers map[string]map[uint64]*Conn // terminalID -> connID -> Conn
	bound   map[uint64]map[string]bool  // connID -> set of terminalIDs it joined
}

// NewTerminalHandlers registers every terminal_* handler on hub and returns
// the handlers value, so a caller can still reach mgr-backed broadcast
// helpers (SendOutput/SendExit) directly if it owns the Manager's
// SetOutputHandler/SetExitHandler wiring.
func NewTerminalHandlers(hub *Hub, mgr *terminal.Manager) *TerminalHandlers {
	th := &TerminalHandlers{
		hub:     hub,
		mgr:     mgr,
		viewers: make(map[string]map[uint64]*Conn),
		bound:   make(map[uint64]map[string]bool),
	}

	// C11: the WS-based terminal_create frame is retired as a create
	// mechanism (creation is now POST /api/terminals). This handler must
	// stay registered rather than simply absent — an explicit refusal, not
	// a silently dropped message, is what tells an old or buggy client its
	// request went nowhere.
	hub.RegisterHandler(events.WSMsgTerminalCreate, th.handleTerminalCreateRetired)
	// terminal_templates is retired the same way terminal_create is: the
	// template catalog moved to relay itself (GET /api/terminal/templates,
	// cmd/relay/template_routes.go), reachable over HTTP, not this Hub.
	// Registering an explicit refusal rather than leaving this type
	// unregistered is what turns "the Shell Launcher's New tab hangs on
	// Loading forever" into a loud, visible error for any client that still
	// sends this frame.
	hub.RegisterHandler(events.WSMsgTerminalTemplates, th.handleTerminalTemplatesRetired)
	hub.RegisterHandler(events.WSMsgJoinTerminal, th.handleJoinTerminal)
	hub.RegisterHandler(events.WSMsgTerminalReconnect, th.handleTerminalReconnect)
	hub.RegisterHandler(events.WSMsgLeaveTerminal, th.handleLeaveTerminal)
	hub.RegisterHandler(events.WSMsgTerminalInput, th.handleTerminalInput)
	hub.RegisterHandler(events.WSMsgTerminalResize, th.handleTerminalResize)
	hub.RegisterHandler(events.WSMsgTerminalClose, th.handleTerminalClose)
	hub.RegisterHandler(events.WSMsgTerminalList, th.handleTerminalList)
	hub.OnDisconnect(th.handleDisconnect)
	return th
}

func (th *TerminalHandlers) handleTerminalCreateRetired(c *Conn, _ []byte) {
	sendWSError(c, "terminal_create over WebSocket is retired; POST /api/terminals instead")
}

func (th *TerminalHandlers) handleTerminalTemplatesRetired(c *Conn, _ []byte) {
	sendWSError(c, "terminal_templates over WebSocket is retired; GET /api/terminal/templates instead")
}

func (th *TerminalHandlers) handleJoinTerminal(c *Conn, raw []byte) {
	var req struct {
		TerminalID string `json:"terminalId"`
	}
	_ = json.Unmarshal(raw, &req)
	if sess, ok := th.lookup(c, req.TerminalID); ok {
		th.join(c, sess)
	}
}

// handleTerminalReconnect re-joins a terminal after a client reload: the
// fresh connection is registered as a viewer and gets terminal_joined with
// scrollback, exactly like join_terminal. The client's current size is
// applied first so the scrollback it replays and the output that follows
// match its viewport — but only when it actually differs, because a
// same-size resize is not a no-op on Windows (ConPTY clears the screen).
func (th *TerminalHandlers) handleTerminalReconnect(c *Conn, raw []byte) {
	var req struct {
		TerminalID string `json:"terminalId"`
		Cols       uint16 `json:"cols"`
		Rows       uint16 `json:"rows"`
	}
	_ = json.Unmarshal(raw, &req)
	sess, ok := th.lookup(c, req.TerminalID)
	if !ok {
		return
	}
	if req.Cols > 0 && req.Rows > 0 {
		if cols, rows := sess.Size(); cols != req.Cols || rows != req.Rows {
			// Best-effort: a stopped terminal has no PTY to resize, and
			// its viewer still needs the join (scrollback + terminal_exit).
			_ = sess.Resize(req.Cols, req.Rows)
		}
	}
	th.join(c, sess)
}

// lookup resolves a terminal id for join_terminal and terminal_reconnect.
// An empty id is silently ignored; an unknown one gets an error frame.
func (th *TerminalHandlers) lookup(c *Conn, terminalID string) (*terminal.Session, bool) {
	if terminalID == "" {
		return nil, false
	}
	sess, ok := th.mgr.Get(terminalID)
	if !ok {
		sendWSError(c, "terminal not found: "+terminalID)
		return nil, false
	}
	return sess, true
}

func (th *TerminalHandlers) handleLeaveTerminal(c *Conn, raw []byte) {
	var req struct {
		TerminalID string `json:"terminalId"`
	}
	_ = json.Unmarshal(raw, &req)
	if req.TerminalID == "" {
		return
	}
	th.leave(c, req.TerminalID)
}

func (th *TerminalHandlers) handleTerminalInput(c *Conn, raw []byte) {
	var req struct {
		TerminalID string `json:"terminalId"`
		Data       string `json:"data"` // base64-encoded
	}
	_ = json.Unmarshal(raw, &req)
	if req.TerminalID == "" {
		return
	}
	decoded, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil {
		sendWSError(c, "invalid base64 data")
		return
	}
	if err := th.mgr.Write(req.TerminalID, decoded); err != nil {
		sendWSError(c, err.Error())
	}
}

func (th *TerminalHandlers) handleTerminalResize(c *Conn, raw []byte) {
	var req struct {
		TerminalID string `json:"terminalId"`
		Cols       uint16 `json:"cols"`
		Rows       uint16 `json:"rows"`
	}
	_ = json.Unmarshal(raw, &req)
	if req.TerminalID == "" {
		return
	}
	if err := th.mgr.Resize(req.TerminalID, req.Cols, req.Rows); err != nil {
		sendWSError(c, err.Error())
	}
}

func (th *TerminalHandlers) handleTerminalClose(_ *Conn, raw []byte) {
	var req struct {
		TerminalID string `json:"terminalId"`
	}
	_ = json.Unmarshal(raw, &req)
	if req.TerminalID == "" {
		return
	}
	th.mgr.Close(req.TerminalID)

	th.mu.Lock()
	delete(th.viewers, req.TerminalID)
	th.mu.Unlock()

	th.hub.Broadcast(mustJSON(map[string]any{
		"type":       events.WSMsgTerminalClosed,
		"terminalId": req.TerminalID,
	}))
}

func (th *TerminalHandlers) handleTerminalList(c *Conn, _ []byte) {
	c.Write(mustJSON(map[string]any{
		"type":      events.WSMsgTerminalList,
		"terminals": th.mgr.ListSummary(),
	}))
}

// handleDisconnect removes a closed connection from every terminal's viewer
// set and, for each terminal it drops to zero viewers, notifies the
// manager so the idle timer can start — the same cleanup handleLeaveTerminal
// does explicitly, but for a client that vanished without saying goodbye.
func (th *TerminalHandlers) handleDisconnect(connID uint64) {
	th.mu.Lock()
	ids := make([]string, 0, len(th.bound[connID]))
	for id := range th.bound[connID] {
		ids = append(ids, id)
	}
	delete(th.bound, connID)
	th.mu.Unlock()

	for _, id := range ids {
		th.removeViewer(connID, id)
	}
}

func (th *TerminalHandlers) join(c *Conn, sess *terminal.Session) {
	tid := sess.ID
	th.mu.Lock()
	if th.viewers[tid] == nil {
		th.viewers[tid] = make(map[uint64]*Conn)
	}
	th.viewers[tid][c.ID] = c
	viewerCount := len(th.viewers[tid])
	if th.bound[c.ID] == nil {
		th.bound[c.ID] = make(map[string]bool)
	}
	th.bound[c.ID][tid] = true
	th.mu.Unlock()

	th.mgr.NotifyViewerChange(tid, viewerCount)

	scrollback := sess.ScrollbackBytes()
	state, exitCode := sess.Snapshot()
	cols, rows := sess.Size()
	c.Write(mustJSON(map[string]any{
		"type":       events.WSMsgTerminalJoined,
		"terminalId": tid,
		"templateId": sess.TemplateID,
		"name":       sess.Name,
		"directory":  sess.Directory,
		"state":      state,
		"cols":       cols,
		"rows":       rows,
		"scrollback": base64.StdEncoding.EncodeToString(scrollback),
		"host":       sess.Host.Chip(),
	}))
	if state == "stopped" {
		c.Write(mustJSON(map[string]any{
			"type":       events.WSMsgTerminalExit,
			"terminalId": tid,
			"exitCode":   exitCode,
		}))
	}
}

func (th *TerminalHandlers) leave(c *Conn, terminalID string) {
	th.mu.Lock()
	bound := th.bound[c.ID][terminalID]
	th.mu.Unlock()
	if !bound {
		return
	}
	th.removeViewer(c.ID, terminalID)

	th.mu.Lock()
	delete(th.bound[c.ID], terminalID)
	th.mu.Unlock()
}

func (th *TerminalHandlers) removeViewer(connID uint64, terminalID string) {
	th.mu.Lock()
	viewers := th.viewers[terminalID]
	delete(viewers, connID)
	remaining := len(viewers)
	if remaining == 0 {
		delete(th.viewers, terminalID)
	}
	th.mu.Unlock()
	th.mgr.NotifyViewerChange(terminalID, remaining)
}

// BroadcastOutput sends a terminal's output chunk to every connection
// currently joined to it, base64-encoded. Intended to be wired as
// terminal.Manager's own output callback by whatever unit constructs both
// (a later integration unit; this package only defines the shape).
func (th *TerminalHandlers) BroadcastOutput(terminalID string, data []byte) {
	th.mu.Lock()
	viewers := make([]*Conn, 0, len(th.viewers[terminalID]))
	for _, c := range th.viewers[terminalID] {
		viewers = append(viewers, c)
	}
	th.mu.Unlock()
	if len(viewers) == 0 {
		return
	}
	msg := mustJSON(map[string]any{
		"type":       events.WSMsgTerminalOutput,
		"terminalId": terminalID,
		"data":       base64.StdEncoding.EncodeToString(data),
	})
	for _, c := range viewers {
		c.Write(msg)
	}
}

// BroadcastExit tells every viewer a terminal exited. Intended to be wired
// as terminal.Manager's own exit callback, mirroring BroadcastOutput.
func (th *TerminalHandlers) BroadcastExit(terminalID string, exitCode int) {
	th.mu.Lock()
	viewers := make([]*Conn, 0, len(th.viewers[terminalID]))
	for _, c := range th.viewers[terminalID] {
		viewers = append(viewers, c)
	}
	th.mu.Unlock()
	msg := mustJSON(map[string]any{
		"type":       events.WSMsgTerminalExit,
		"terminalId": terminalID,
		"exitCode":   exitCode,
	})
	for _, c := range viewers {
		c.Write(msg)
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func sendWSError(c *Conn, message string) {
	c.Write(mustJSON(map[string]any{
		"type":    events.WSMsgError,
		"message": message,
	}))
}
