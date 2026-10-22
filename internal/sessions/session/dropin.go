package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/attention"
	"github.com/barelyworkingcode/relay/internal/sessions/events"
	"github.com/barelyworkingcode/relay/internal/sessions/provider"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// ErrDroppedIn is returned while a terminal holds the session.
var ErrDroppedIn = errors.New("session: a terminal holds this session")

// HandoffError is a refusal Handoff names with a stable Code; callers branch
// on Code, never on Message.
type HandoffError struct{ Code, Message string }

func (e *HandoffError) Error() string { return e.Message }

// HandoffTarget is what a terminal needs to open the conversation a stopped
// headless session held.
type HandoffTarget struct {
	// Agent is "claude", "pi" or "codex".
	Agent string
	// ConversationID is the Claude session id, the Pi session uuid or the
	// Codex thread uuid.
	ConversationID string
	// CodexAttach is true when the codex thread is held through the shared
	// daemon.
	CodexAttach bool
	// PiSessionArgs is exactly ["--session", <file or uuid>, "--session-dir",
	// <absolute dir>]; Pi only.
	PiSessionArgs []string
}

type openTool struct{ id, name string }

// toolBoard tracks the open tool calls of each claude session. Each session's
// slice is in open order, so the first entry is the earliest open tool.
type toolBoard struct {
	mu    sync.Mutex
	tools map[string][]openTool
	wake  map[string]chan struct{}
}

// snapshot returns the earliest open tool's name ("" when none) and a channel
// closed by the next tool opening or clear for sid.
func (b *toolBoard) snapshot(sid string) (string, <-chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.wake == nil {
		b.wake = map[string]chan struct{}{}
	}
	ch := b.wake[sid]
	if ch == nil {
		ch = make(chan struct{})
		b.wake[sid] = ch
	}
	if list := b.tools[sid]; len(list) > 0 {
		return list[0].name, ch
	}
	return "", ch
}

func (b *toolBoard) notifyLocked(sid string) {
	if ch := b.wake[sid]; ch != nil {
		close(ch)
		delete(b.wake, sid)
	}
}

func (b *toolBoard) open(sid, id, name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tools == nil {
		b.tools = map[string][]openTool{}
	}
	b.tools[sid] = append(b.tools[sid], openTool{id: id, name: name})
	b.notifyLocked(sid)
}

func (b *toolBoard) closeTool(sid, id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	list := b.tools[sid]
	for i, t := range list {
		if t.id == id {
			b.tools[sid] = append(list[:i:i], list[i+1:]...)
			break
		}
	}
	if len(b.tools[sid]) == 0 {
		delete(b.tools, sid)
	}
}

// clear drops every open tool for sid and wakes waiters: the turn is over.
func (b *toolBoard) clear(sid string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.tools, sid)
	b.notifyLocked(sid)
}

// trackTools opens and closes tool calls from a canonical event. Claude only.
func (m *Manager) trackTools(sess *sessionstypes.Session, data json.RawMessage) {
	if sess.ProviderType != KindClaude {
		return
	}
	var ev struct {
		Type             string `json:"type"`
		Subtype          string `json:"subtype"`
		ContentBlockStop bool   `json:"content_block_stop"`
		ToolUseID        string `json:"tool_use_id"`
		ContentBlock     *struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	switch {
	case ev.Type == events.EvtAssistant && ev.ContentBlockStop && ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use":
		m.tools.open(sess.ID, ev.ContentBlock.ID, ev.ContentBlock.Name)
	case ev.Type == events.EvtResult && ev.Subtype == events.ResultToolResultSubtype:
		m.tools.closeTool(sess.ID, ev.ToolUseID)
	}
}

// Held reports whether a terminal holds id.
func (m *Manager) Held(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	slot := m.slots[id]
	return slot != nil && slot.held
}

// Handoff stops id's headless Claude, Pi or Codex process so a terminal can
// resume its conversation, and holds the session until HandBack. A Claude
// session refuses at once when a tool call is open; every kind otherwise waits
// up to wait for the current turn to end. beforeStop runs just before the
// process is killed. The provider and slot stay installed, so the process's
// exit still reaches the exit handler. For an attached Codex thread the stop
// only ends relay's own daemon connection.
func (m *Manager) Handoff(ctx context.Context, id string, wait time.Duration, beforeStop func()) (HandoffTarget, error) {
	// A detached agent that relay or this host left running is attached first,
	// so the stop below ends it: a terminal and a second agent must not share
	// the conversation.
	if err := m.reattachIfDetached(id); err != nil {
		if errors.Is(err, ErrAttachFailed) {
			return HandoffTarget{}, &HandoffError{"attach_failed", "the host answered and the session's agent is running, but relay could not attach to it, so it cannot be stopped for the terminal; try again"}
		}
		return HandoffTarget{}, &HandoffError{"host_unavailable", "the host of this session did not answer, so its agent cannot be stopped for the terminal; try again when the host is reachable"}
	}
	sess, ok := m.Get(id)
	if !ok {
		return HandoffTarget{}, ErrSessionNotFound
	}
	switch sess.ProviderType {
	case KindClaude:
	case KindPi, KindCodex:
		if sess.GetHost() != nil {
			return HandoffTarget{}, &HandoffError{"not_local", "Pi and Codex sessions drop in only on this Mac"}
		}
	default:
		return HandoffTarget{}, &HandoffError{"not_claude", fmt.Sprintf("only Claude, Pi and Codex sessions can be taken over; this is a %s session", sess.ProviderType)}
	}
	if !sess.Headless {
		return HandoffTarget{}, &HandoffError{"not_headless", "this session is not headless; continue it in eve"}
	}
	deadline := m.cfg.clockOrDefault().After(wait)
	for {
		if m.Held(id) {
			return HandoffTarget{}, errDroppedIn()
		}
		// The wake channel is taken before the claim attempt so a turn that
		// ends in between is not missed.
		tool, wake := m.tools.snapshot(id)
		if tool != "" && sess.ProviderType == KindClaude {
			return HandoffTarget{}, &HandoffError{"tool_running", fmt.Sprintf("a tool is running (%s); wait for it to finish or stop the turn, then try again", tool)}
		}
		if sess.TryStartProcessing() {
			break
		}
		select {
		case <-wake:
		case <-deadline:
			return HandoffTarget{}, &HandoffError{"turn_timeout", fmt.Sprintf("the current turn did not end within %d s; stop it or try again", int(wait/time.Second))}
		case <-ctx.Done():
			return HandoffTarget{}, ctx.Err()
		}
	}

	// Read before m.mu: a provider's state lock can be held across a blocking
	// stdin write.
	target, err := m.handoffTarget(sess)
	if err != nil {
		sess.SetProcessing(false)
		return HandoffTarget{}, &HandoffError{"no_conversation", fmt.Sprintf("the session's conversation cannot be opened in a terminal: %v", err)}
	}
	if target.ConversationID == "" {
		sess.SetProcessing(false)
		return HandoffTarget{}, &HandoffError{"no_conversation", "the session has not run a turn yet; there is nothing to take over"}
	}

	m.mu.Lock()
	slot := m.slots[id]
	if slot == nil || slot.sess != sess || slot.held {
		held := slot != nil && slot.held
		m.mu.Unlock()
		if held {
			return HandoffTarget{}, errDroppedIn()
		}
		sess.SetProcessing(false)
		return HandoffTarget{}, ErrSessionNotFound
	}
	slot.held = true
	m.mu.Unlock()

	if beforeStop != nil {
		beforeStop()
	}
	m.signal(sess, attention.DroppedIn)
	if p := sess.Provider(); p != nil && p.Alive() {
		p.Kill()
	}
	return target, nil
}

func errDroppedIn() error {
	return &HandoffError{"dropped_in", "a terminal already has this session; close it first"}
}

// claudeConversationID reads the conversation id from the live provider, else
// from the persisted provider state.
func claudeConversationID(sess *sessionstypes.Session) string {
	var id string
	read := func(state json.RawMessage) {
		var s struct {
			ClaudeSessionID string `json:"claudeSessionId"`
		}
		if id == "" && len(state) > 0 && json.Unmarshal(state, &s) == nil {
			id = s.ClaudeSessionID
		}
	}
	if p := sess.Provider(); p != nil {
		read(p.GetState())
	}
	sess.Lock()
	state := sess.ProviderState
	sess.Unlock()
	read(state)
	return id
}

// handoffTarget reads the conversation from the live provider, else from the
// persisted provider state. An empty ConversationID means the session has not
// run a turn yet; an error means the state names a conversation no terminal
// can safely open.
func (m *Manager) handoffTarget(sess *sessionstypes.Session) (HandoffTarget, error) {
	var states []json.RawMessage
	if p := sess.Provider(); p != nil {
		states = append(states, p.GetState())
	}
	sess.Lock()
	states = append(states, sess.ProviderState)
	sess.Unlock()

	t := HandoffTarget{Agent: sess.ProviderType}
	for _, state := range states {
		var s struct {
			ClaudeSessionID string `json:"claudeSessionId"`
			PiSessionID     string `json:"piSessionId"`
			ThreadID        string `json:"threadId"`
			DaemonAttach    bool   `json:"daemonAttach"`
		}
		if len(state) == 0 || json.Unmarshal(state, &s) != nil {
			continue
		}
		switch sess.ProviderType {
		case KindClaude:
			t.ConversationID = s.ClaudeSessionID
		case KindPi:
			t.ConversationID = s.PiSessionID
			if t.ConversationID != "" {
				args, err := provider.PiTerminalSessionArgs(state, m.cfg.Pi.DataDir)
				if err != nil {
					return HandoffTarget{}, err
				}
				t.PiSessionArgs = args
			}
		case KindCodex:
			t.ConversationID = s.ThreadID
			t.CodexAttach = s.DaemonAttach
		}
		if t.ConversationID != "" {
			return t, nil
		}
	}
	return t, nil
}

// HandBack releases the hold on id and reports it idle. Idempotent; an unknown
// id is a no-op.
func (m *Manager) HandBack(id string) {
	m.mu.Lock()
	slot := m.slots[id]
	if slot == nil || !slot.held {
		m.mu.Unlock()
		return
	}
	slot.held = false
	sess := slot.sess
	m.mu.Unlock()
	if sess == nil {
		return
	}
	sess.SetProcessing(false)
	m.signal(sess, attention.HandedBack)
}
