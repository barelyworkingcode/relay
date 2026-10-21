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
	if m.Reports == nil || !i.shouldDrain(m) {
		return
	}
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	reports, err := m.Reports(bounded)
	if err != nil || len(reports) == 0 {
		return
	}
	if m.Lifetimes != nil {
		unmatched, ok := i.drainConfirmed(ctx, m, reports)
		if !ok {
			unmatched = reports
		}
		reports = unmatched
		if len(reports) == 0 {
			return
		}
	}
	// Reports never seed an identity: independently refresh live evidence first.
	// Deliberately ctx, not bounded: the read has already deleted the reports,
	// so a probe that ran out of the read's budget would lose them. For the
	// same reason a failed probe waits for a later one instead of dropping them.
	if !i.Probe(ctx, m) && !i.awaitReachable(ctx, m) {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.applyReportsLocked(m, reports, nil) {
		i.publishLocked()
	}
}

// drainConfirmed applies reports to rows whose pid still has the start time
// the probe saw, and removes rows whose pid is gone or restarted. It returns
// the reports that reached no confirmed row, for the full probe to rebuild and
// apply; ok is false when Lifetimes failed and nothing was applied.
func (i *Inventory) drainConfirmed(ctx context.Context, m Machine, reports []Report) (unmatched []Report, ok bool) {
	type target struct {
		pid      int
		lifetime string
	}
	targets := map[string]target{}
	matches := map[int][]string{}
	i.mu.Lock()
	for id, e := range i.entries {
		if e.machine.Key != m.Key {
			continue
		}
		for n, r := range reports {
			if e.observation.ClaudeID == r.SessionID && e.observation.Session.Directory == r.Cwd {
				targets[id] = target{e.observation.PID, e.observation.Lifetime}
				matches[n] = append(matches[n], id)
			}
		}
	}
	i.mu.Unlock()
	if len(targets) == 0 {
		return reports, true
	}
	seen := map[int]bool{}
	pids := []int{}
	for _, t := range targets {
		if !seen[t.pid] {
			seen[t.pid] = true
			pids = append(pids, t.pid)
		}
	}
	current, err := m.Lifetimes(ctx, pids)
	if err != nil {
		return nil, false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	confirmed := map[string]bool{}
	changed := false
	for id, t := range targets {
		e, present := i.entries[id]
		if !present || e.observation.PID != t.pid || e.observation.Lifetime != t.lifetime {
			continue
		}
		if now, alive := current[t.pid]; alive && now == t.lifetime {
			confirmed[id] = true
			continue
		}
		delete(i.entries, id)
		changed = true
	}
	for n, r := range reports {
		reached := false
		for _, id := range matches[n] {
			reached = reached || confirmed[id]
		}
		if !reached {
			unmatched = append(unmatched, r)
		}
	}
	if i.applyReportsLocked(m, reports, confirmed) {
		changed = true
	}
	if changed {
		i.publishLocked()
	}
	return unmatched, true
}

// awaitReachable waits, at most a minute, for a later probe to find the
// machine reachable again.
func (i *Inventory) awaitReachable(ctx context.Context, m Machine) bool {
	snapshots, cancel := i.Subscribe()
	defer cancel()
	deadline := i.opts.Clock.After(time.Minute)
	for {
		select {
		case <-ctx.Done():
			return false
		case <-deadline:
			return false
		case <-snapshots:
		}
		i.mu.Lock()
		reachable := i.machines[m.Key].Status == "reachable"
		i.mu.Unlock()
		if reachable {
			return true
		}
	}
}

// applyReportsLocked writes state, source, update time and hookAt, and nothing
// else: a hook report grants no authority. A nil only applies to every row of
// the machine; otherwise only to the ids in only.
func (i *Inventory) applyReportsLocked(m Machine, reports []Report, only map[string]bool) bool {
	changed := false
	for _, r := range reports {
		state := reportState(r)
		if state == "" {
			continue
		}
		for id, e := range i.entries {
			if only != nil && !only[id] {
				continue
			}
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
	return changed
}
