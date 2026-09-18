package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"sync"

	"github.com/barelyworkingcode/relay/internal/sessions/events"
	"github.com/barelyworkingcode/relay/internal/sessions/permission"
	"github.com/barelyworkingcode/relay/internal/sessions/provider"
	"github.com/barelyworkingcode/relay/internal/sessions/session"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// SessionHandlers registers the session-related WS message types onto a
// Hub, and implements sessionstypes.EventSink so session.Manager can be
// pointed at it directly (Manager.SetEventSink(sh)). Tracks viewer<->
// connection binding itself, mirroring TerminalHandlers — the Hub carries
// no session knowledge (ws.go's own doc comment).
type SessionHandlers struct {
	hub   *Hub
	mgr   *session.Manager
	perms *permission.PermissionManager

	mu      sync.Mutex
	viewers map[string]map[uint64]*Conn // sessionID -> connID -> Conn
	bound   map[uint64]map[string]bool  // connID -> set of sessionIDs it joined
}

// NewSessionHandlers registers every session-related handler on hub and
// returns the handlers value; the caller still owns
// mgr.SetEventSink(handlers) since only it knows whether this Hub is the
// sole sink or one of several.
func NewSessionHandlers(hub *Hub, mgr *session.Manager, perms *permission.PermissionManager) *SessionHandlers {
	sh := &SessionHandlers{
		hub:     hub,
		mgr:     mgr,
		perms:   perms,
		viewers: make(map[string]map[uint64]*Conn),
		bound:   make(map[uint64]map[string]bool),
	}

	hub.RegisterHandler(events.WSMsgJoinSession, sh.handleJoinSession)
	hub.RegisterHandler(events.WSMsgSendMessage, sh.handleSendMessage)
	hub.RegisterHandler(events.WSMsgEndSession, sh.handleEndSession)
	hub.RegisterHandler(events.WSMsgRenameSession, sh.handleRenameSession)
	hub.RegisterHandler(events.WSMsgSetSessionFolder, sh.handleSetSessionFolder)
	hub.RegisterHandler(events.WSMsgDeleteSession, sh.handleDeleteSession)
	hub.RegisterHandler(events.WSMsgLeaveSession, sh.handleLeaveSession)
	hub.RegisterHandler(events.WSMsgStopGeneration, sh.handleStopGeneration)
	hub.RegisterHandler(events.WSMsgClearSession, sh.handleClearSession)
	hub.RegisterHandler(events.WSMsgPermissionResponse, sh.handlePermissionResponse)
	hub.RegisterHandler(events.WSMsgSetPermissionMode, sh.handleSetPermissionMode)
	hub.OnDisconnect(sh.handleDisconnect)
	return sh
}

// SendToSession implements sessionstypes.EventSink: broadcast msg to every
// connection currently joined to sessionID.
func (sh *SessionHandlers) SendToSession(sessionID string, msg map[string]any) {
	sh.mu.Lock()
	viewers := make([]*Conn, 0, len(sh.viewers[sessionID]))
	for _, c := range sh.viewers[sessionID] {
		viewers = append(viewers, c)
	}
	sh.mu.Unlock()
	if len(viewers) == 0 {
		return
	}
	data := mustJSON(msg)
	for _, c := range viewers {
		c.Write(data)
	}
}

func (sh *SessionHandlers) handleJoinSession(c *Conn, raw []byte) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(raw, &req)
	if req.SessionID == "" {
		return
	}

	sess, ok := sh.mgr.Get(req.SessionID)
	if !ok {
		sendWSError(c, "session not found: "+req.SessionID)
		return
	}

	sh.mu.Lock()
	if sh.viewers[req.SessionID] == nil {
		sh.viewers[req.SessionID] = make(map[uint64]*Conn)
	}
	sh.viewers[req.SessionID][c.ID] = c
	if sh.bound[c.ID] == nil {
		sh.bound[c.ID] = make(map[string]bool)
	}
	sh.bound[c.ID][req.SessionID] = true
	sh.mu.Unlock()

	// One locked snapshot for every field RenameSession/SetSessionFolder/
	// Manager.persist can mutate concurrently (matches Manager.summarize's
	// own single-snapshot pattern) — reading them individually, each under
	// its own lock or no lock at all, would let a rename or folder change
	// land mid-read and mix an old and a new field into one response.
	sess.Lock()
	providerState := sess.ProviderState
	directory := sess.Directory
	model := sess.Model
	name := sess.Name
	folder := sess.Folder
	headless := sess.Headless
	stats := sess.Stats
	messages := make([]sessionstypes.Message, len(sess.Messages))
	copy(messages, sess.Messages)
	sess.Unlock()

	// Claude CLI's own JSONL transcript has both user + assistant turns and
	// is the more complete replay when available; fall back to the
	// session's own in-memory/persisted Messages otherwise (relayLLM's
	// handleJoinSession, ported unchanged).
	var history []sessionstypes.Message
	var claudeSessionID string
	if providerState != nil {
		var ps struct {
			ClaudeSessionID string `json:"claudeSessionId"`
		}
		_ = json.Unmarshal(providerState, &ps)
		claudeSessionID = ps.ClaudeSessionID
	}
	if claudeSessionID != "" {
		if h, err := provider.ReadClaudeHistory(directory, sess.GetHost(), claudeSessionID); err == nil && len(h) > 0 {
			history = h
		} else if err != nil {
			slog.Debug("claude history unavailable, using session messages", "session", req.SessionID, "error", err)
		}
	}
	if history == nil {
		history = messages
	}

	p := sess.Provider()
	c.Write(mustJSON(map[string]any{
		"type":            events.WSMsgSessionJoined,
		"sessionId":       sess.ID,
		"projectId":       sess.ProjectID,
		"directory":       directory,
		"model":           model,
		"name":            name,
		"folder":          folder,
		"history":         history,
		"stats":           stats,
		"headless":        headless,
		"protocolVersion": events.ProtocolVersion,
		"host":            sess.GetHost(),
		// live is SH-6's addition: a caller must be able to tell, from
		// session_joined alone, whether send_message will work right now
		// or would come back resume_required — relayLLM never had a
		// concept of "joined but not live" since it respawned silently.
		"live": p != nil && p.Alive(),
	}))
}

func (sh *SessionHandlers) handleSendMessage(c *Conn, raw []byte) {
	var req struct {
		SessionID string                         `json:"sessionId"`
		Text      string                         `json:"text"`
		Files     []sessionstypes.FileAttachment `json:"files"`
	}
	_ = json.Unmarshal(raw, &req)
	if req.SessionID == "" {
		sendWSError(c, "sessionId required")
		return
	}

	err := sh.mgr.SendMessage(req.SessionID, req.Text, req.Files)
	if err == nil {
		return
	}
	if errors.Is(err, session.ErrResumeRequired) {
		sendResumeRequired(c, req.SessionID)
		return
	}
	sendWSError(c, err.Error())
}

func (sh *SessionHandlers) handleEndSession(c *Conn, raw []byte) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(raw, &req)
	if req.SessionID == "" {
		sendWSError(c, "sessionId required")
		return
	}
	sh.mgr.EndSession(req.SessionID)
}

func (sh *SessionHandlers) handleRenameSession(c *Conn, raw []byte) {
	var req struct {
		SessionID string `json:"sessionId"`
		Name      string `json:"name"`
	}
	_ = json.Unmarshal(raw, &req)
	if req.SessionID == "" {
		sendWSError(c, "sessionId required")
		return
	}
	if err := sh.mgr.RenameSession(req.SessionID, req.Name); err != nil {
		sendWSError(c, err.Error())
	}
}

func (sh *SessionHandlers) handleSetSessionFolder(c *Conn, raw []byte) {
	var req struct {
		SessionID string `json:"sessionId"`
		Folder    string `json:"folder"`
	}
	_ = json.Unmarshal(raw, &req)
	if req.SessionID == "" {
		sendWSError(c, "sessionId required")
		return
	}
	if err := sh.mgr.SetSessionFolder(req.SessionID, req.Folder); err != nil {
		sendWSError(c, err.Error())
	}
}

func (sh *SessionHandlers) handleDeleteSession(c *Conn, raw []byte) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(raw, &req)
	if req.SessionID == "" {
		sendWSError(c, "sessionId required")
		return
	}
	sh.mgr.DeleteSession(req.SessionID)

	sh.mu.Lock()
	delete(sh.viewers, req.SessionID)
	if sh.bound[c.ID] != nil {
		delete(sh.bound[c.ID], req.SessionID)
	}
	sh.mu.Unlock()

	sh.hub.Broadcast(mustJSON(map[string]any{"type": events.WSMsgSessionEnded, "sessionId": req.SessionID}))
}

func (sh *SessionHandlers) handleLeaveSession(c *Conn, raw []byte) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(raw, &req)
	if req.SessionID == "" {
		return
	}
	sh.removeViewer(c.ID, req.SessionID)
}

func (sh *SessionHandlers) handleStopGeneration(c *Conn, raw []byte) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(raw, &req)
	if req.SessionID == "" {
		sendWSError(c, "sessionId required")
		return
	}
	if err := sh.mgr.StopGeneration(req.SessionID); err != nil {
		sendWSError(c, err.Error())
	}
}

func (sh *SessionHandlers) handleClearSession(c *Conn, raw []byte) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(raw, &req)
	if req.SessionID == "" {
		sendWSError(c, "sessionId required")
		return
	}
	err := sh.mgr.ClearSession(req.SessionID)
	if err == nil {
		return
	}
	if errors.Is(err, session.ErrResumeRequired) {
		sendResumeRequired(c, req.SessionID)
		return
	}
	sendWSError(c, err.Error())
}

func (sh *SessionHandlers) handleSetPermissionMode(c *Conn, raw []byte) {
	var req struct {
		SessionID string `json:"sessionId"`
		Mode      string `json:"mode"`
	}
	_ = json.Unmarshal(raw, &req)
	if req.SessionID == "" {
		sendWSError(c, "sessionId required")
		return
	}
	sess, ok := sh.mgr.Get(req.SessionID)
	if !ok {
		sendWSError(c, "session not found")
		return
	}
	claude, ok := sess.Provider().(*provider.ClaudeProvider)
	if !ok {
		sendWSError(c, "permission mode toggle not supported for this provider")
		return
	}
	if err := claude.SetPermissionMode(req.Mode); err != nil {
		if errors.Is(err, provider.ErrRestartNeedsResume) {
			sendResumeRequired(c, req.SessionID)
			return
		}
		sendWSError(c, err.Error())
		return
	}
	sess.Lock()
	mode := sess.PermissionMode
	sess.Unlock()
	sh.SendToSession(req.SessionID, map[string]any{"type": events.WSMsgModeChanged, "sessionId": req.SessionID, "mode": mode})
}

// handlePermissionResponse resolves a pending Claude Code tool-approval
// prompt. Before doing that, it checks that c has itself joined the pending
// request's own session (join_session, tracked in sh.bound the same way
// every other per-session action here is scoped) — otherwise any connection
// on relay's single frontend trust domain could resolve, sight unseen, a
// tool-approval prompt for a session it never joined. This is the one
// scoping check this in-band control_request/control_response path has;
// broader per-caller session ownership is out of scope (see the guarded
// wrapper's own comment in hostapi/server.go for why).
func (sh *SessionHandlers) handlePermissionResponse(c *Conn, raw []byte) {
	if sh.perms == nil {
		return
	}
	var req struct {
		PermissionID string `json:"permissionId"`
		Approved     bool   `json:"approved"`
		Reason       string `json:"reason"`
	}
	_ = json.Unmarshal(raw, &req)

	sessionID, ok := sh.perms.PendingSessionID(req.PermissionID)
	if !ok {
		// Already resolved, timed out, or never existed: Resolve's own
		// lookup would report this exact same no-op, so there is nothing
		// left to do.
		return
	}
	sh.mu.Lock()
	joined := sh.bound[c.ID][sessionID]
	sh.mu.Unlock()
	if !joined {
		sendWSError(c, "permission response refused: this connection has not joined session "+sessionID)
		return
	}

	decision := "deny"
	if req.Approved {
		decision = "allow"
	}
	sh.perms.Resolve(req.PermissionID, permission.PermissionDecision{Decision: decision, Reason: req.Reason})
}

func (sh *SessionHandlers) handleDisconnect(connID uint64) {
	sh.mu.Lock()
	ids := make([]string, 0, len(sh.bound[connID]))
	for id := range sh.bound[connID] {
		ids = append(ids, id)
	}
	delete(sh.bound, connID)
	sh.mu.Unlock()

	for _, id := range ids {
		sh.removeViewer(connID, id)
	}
}

func (sh *SessionHandlers) removeViewer(connID uint64, sessionID string) {
	sh.mu.Lock()
	if sh.viewers[sessionID] != nil {
		delete(sh.viewers[sessionID], connID)
		if len(sh.viewers[sessionID]) == 0 {
			delete(sh.viewers, sessionID)
		}
	}
	if sh.bound[connID] != nil {
		delete(sh.bound[connID], sessionID)
	}
	sh.mu.Unlock()
}

// sendResumeRequired sends SH-6's resume_required error frame — a distinct,
// typed refusal (not a generic sendWSError string) so a client can drive
// its own resume UI rather than parsing a message. This frame has no
// relayLLM ancestor: relayLLM never refused to respawn, so it never needed
// one.
func sendResumeRequired(c *Conn, sessionID string) {
	c.Write(mustJSON(map[string]any{
		"type":      events.WSMsgError,
		"code":      "resume_required",
		"sessionId": sessionID,
	}))
}
