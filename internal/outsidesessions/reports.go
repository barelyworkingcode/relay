package outsidesessions

import (
	"context"
	"encoding/json"
	"time"
)

type Report struct {
	Event            string    `json:"hook_event_name"`
	SessionID        string    `json:"session_id"`
	Cwd              string    `json:"cwd"`
	TranscriptPath   string    `json:"transcript_path"`
	NotificationType string    `json:"notification_type"`
	At               time.Time `json:"-"`
}

func ParseReport(data []byte, at time.Time) (Report, bool) {
	var r Report
	if len(data) > 65536 || json.Unmarshal(data, &r) != nil || !sessionIdentity.MatchString(r.SessionID) || r.Cwd == "" {
		return r, false
	}
	r.At = at
	return r, true
}
func reportState(r Report) string {
	switch r.Event {
	case "UserPromptSubmit", "PreToolUse", "PostToolUse":
		return "running"
	case "PermissionRequest":
		return "asking"
	case "Notification":
		if r.NotificationType == "permission_prompt" {
			return "asking"
		}
	case "Stop", "SessionStart":
		return "idle"
	}
	return ""
}
func (i *Inventory) drain(ctx context.Context, m Machine) {
	if m.Reports == nil {
		return
	}
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	reports, err := m.Reports(bounded)
	if err != nil || len(reports) == 0 {
		return
	} // Reports never seed an identity: independently refresh live evidence first.
	if !i.Probe(bounded, m) {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	changed := false
	for _, r := range reports {
		state := reportState(r)
		if state == "" {
			continue
		}
		for id, e := range i.entries {
			if e.machine.Key != m.Key || e.observation.ClaudeID != r.SessionID || e.observation.Session.Directory != r.Cwd || r.At.Before(e.hookAt) || r.At.Before(e.liveAt.Add(-time.Minute)) || r.At.After(i.opts.Clock.Now().Add(time.Second)) {
				continue
			}
			e.observation.Session.State = state
			e.observation.Session.Source = "hook"
			e.observation.Session.UpdatedAt = stamp(i.opts.Clock.Now())
			e.hookAt = r.At
			i.entries[id] = e
			changed = true
		}
	}
	if changed {
		i.publishLocked()
	}
}
