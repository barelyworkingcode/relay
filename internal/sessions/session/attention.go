package session

import (
	"encoding/json"
	"sync/atomic"

	"github.com/barelyworkingcode/relay/internal/sessions/attention"
	"github.com/barelyworkingcode/relay/internal/sessions/events"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// tracked reports whether sess gets an attention state. Headless sessions
// that are not agents are routine runs and stay invisible, as before.
func tracked(sess *sessionstypes.Session) bool {
	if sess == nil {
		return false
	}
	if sess.ProviderType != KindClaude && sess.ProviderType != KindPi && sess.ProviderType != KindCodex {
		return false
	}
	return !sess.Headless || sess.Agent
}

// sinkAdapter lets the Board exist before SetAttentionSink is called; frames
// before then are dropped.
type sinkAdapter struct {
	target atomic.Pointer[attention.Sink]
}

func (a *sinkAdapter) StateChanged(c attention.Change) {
	if s := a.target.Load(); s != nil {
		(*s).StateChanged(c)
	}
}

func (a *sinkAdapter) TurnDone(d attention.TurnDone) {
	if s := a.target.Load(); s != nil {
		(*s).TurnDone(d)
	}
}

// SetAttentionSink installs the receiver of session_state and turn_done
// notifications.
func (m *Manager) SetAttentionSink(s attention.Sink) {
	if s == nil {
		m.attnSink.target.Store(nil)
		return
	}
	m.attnSink.target.Store(&s)
}

func (m *Manager) signal(sess *sessionstypes.Session, sig attention.Signal) {
	if tracked(sess) {
		m.attn.Signal(sess.ID, sig)
	}
}

// onPermissionPending is the permission manager's pending observer. Sessions
// the manager does not hold are ignored.
func (m *Manager) onPermissionPending(sessionID string, pending int) {
	m.mu.Lock()
	slot := m.slots[sessionID]
	var sess *sessionstypes.Session
	if slot != nil {
		sess = slot.sess
	}
	m.mu.Unlock()
	if pending > 0 {
		m.signal(sess, attention.Asked)
	} else {
		m.signal(sess, attention.Answered)
	}
}

// classifyLLMEvent maps a canonical event to the signal it implies and, for
// an assistant text delta, the reply text.
func classifyLLMEvent(data json.RawMessage) (sig attention.Signal, text string) {
	var ev struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Delta   struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return attention.Activity, ""
	}
	switch {
	case ev.Type == events.EvtAssistant && ev.Delta.Type == events.DeltaText:
		return attention.Activity, ev.Delta.Text
	case ev.Type == events.EvtSystem && ev.Subtype == events.SystemQuestionSubtype:
		return attention.Asked, ""
	case ev.Type == events.EvtResult && ev.Subtype == events.ResultToolResultSubtype:
		return attention.Answered, ""
	}
	return attention.Activity, ""
}

// completionSignal reads message_complete's optional {"isError":true}.
func completionSignal(data json.RawMessage) attention.Signal {
	var d struct {
		IsError bool `json:"isError"`
	}
	if len(data) > 0 && json.Unmarshal(data, &d) == nil && d.IsError {
		return attention.TurnFailed
	}
	return attention.TurnEnded
}
