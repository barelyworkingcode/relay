package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/sessions/ledger"
)

type swapDirection string

const (
	swapToTerminal swapDirection = "to_terminal"
	swapToHeadless swapDirection = "to_headless"
)

const swapAuditUnavailable = "audit_unavailable"

// swapLink ties a drop-in terminal to the session it holds. Host is the SSH
// host's name, empty for this Mac.
type swapLink struct {
	SessionID, TerminalID, Host string
	Caller                      LaunchCaller
	TerminalGone, SessionGone   bool
}

// swapTable is relay's in-memory record of which terminal holds which session.
// A restart drops it; the held session then reads dormant and needs a resume.
// Its zero value and a nil table are inert.
type swapTable struct {
	mu     sync.Mutex
	now    func() time.Time
	links  map[string]*swapLink // by terminal id
	starts map[string]time.Time // intent time by audit id
}

func newSwapTable() *swapTable {
	return &swapTable{now: time.Now, links: map[string]*swapLink{}, starts: map[string]time.Time{}}
}

func (t *swapTable) link(l swapLink) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.links == nil {
		t.links = map[string]*swapLink{}
	}
	t.links[l.TerminalID] = &l
}

// exited marks the terminal or the session named id as reported gone. It
// answers the link and true once both halves are in, and forgets it then, so
// the resume starts once.
func (t *swapTable) exited(id string) (swapLink, bool) {
	if t == nil {
		return swapLink{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for terminalID, l := range t.links {
		switch id {
		case l.TerminalID:
			l.TerminalGone = true
		case l.SessionID:
			l.SessionGone = true
		default:
			continue
		}
		if l.TerminalGone && l.SessionGone {
			delete(t.links, terminalID)
			return *l, true
		}
	}
	return swapLink{}, false
}

func (t *swapTable) clock() time.Time {
	if t == nil || t.now == nil {
		return time.Now()
	}
	return t.now()
}

func (t *swapTable) noteStart(auditID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.starts == nil {
		t.starts = map[string]time.Time{}
	}
	t.starts[auditID] = t.clock()
}

func (t *swapTable) takeStart(auditID string) (time.Time, bool) {
	if t == nil {
		return time.Time{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.starts[auditID]
	delete(t.starts, auditID)
	return s, ok
}

type swapArgs struct {
	SessionID  string `json:"session_id"`
	Direction  string `json:"direction"`
	ProjectID  string `json:"project_id,omitempty"`
	HostID     string `json:"host_id,omitempty"`
	TerminalID string `json:"terminal_id,omitempty"`
}

// swapIntent writes the swap's intent row and answers only once it is
// durable. A swap that cannot be recorded does not start.
func (d sessionRouteDeps) swapIntent(actor audit.AuditActor, dir swapDirection, sessionID, projectID, hostID string) (string, error) {
	if !d.auditor.Ready() {
		return "", errors.New(swapAuditUnavailable)
	}
	args, _ := json.Marshal(swapArgs{SessionID: sessionID, Direction: string(dir), ProjectID: projectID, HostID: hostID})
	id := audit.NewAuditID()
	if err := d.auditor.RecordDurable(audit.AuditEvent{
		ID: id, TS: d.swaps.clock(), Event: audit.AuditEventSessionSwap, Phase: audit.AuditPhaseIntent,
		Actor: actor, Args: args, Outcome: audit.AuditOutcomePending,
	}); err != nil {
		return "", err
	}
	d.swaps.noteStart(id)
	return id, nil
}

// swapCompletion writes the completion row that pairs with swapIntent's id.
func (d sessionRouteDeps) swapCompletion(auditID string, dir swapDirection, sessionID, terminalID, outcome, code string) {
	args, _ := json.Marshal(swapArgs{SessionID: sessionID, Direction: string(dir), TerminalID: terminalID})
	now := d.swaps.clock()
	ev := audit.AuditEvent{
		ID: auditID, TS: now, Event: audit.AuditEventSessionSwap, Phase: audit.AuditPhaseCompletion,
		Args: args, Outcome: outcome, Error: code,
	}
	if start, ok := d.swaps.takeStart(auditID); ok {
		ev.DurMs = now.Sub(start).Milliseconds()
	}
	d.auditor.Record(ev)
}

// swapHostID is the SSH host's id for a hosted project, empty otherwise.
func (d sessionRouteDeps) swapHostID(projectID string) string {
	if proj, _ := config.FindProjectByID(config.FreshSettings(d.store), projectID); proj != nil && proj.IsHosted() {
		return proj.HostID
	}
	return ""
}

// armSwap records that terminalID holds the session and, when the ledger
// already reads dormant, marks that half and starts the resume if the
// terminal is gone too. Linking first and reading the ledger after closes the
// window in which the session's exit report lands between the two.
func (d sessionRouteDeps) armSwap(l swapLink) {
	d.swaps.link(l)
	if d.sessions == nil {
		return
	}
	if rec, ok := d.sessions.Get(l.SessionID); ok && rec.State == ledger.StateDormant {
		d.startHandBack(l.SessionID)
	}
}

// startHandBack reports the session half of a swap and, when that completes
// the pair, resumes in its own goroutine.
func (d sessionRouteDeps) startHandBack(id string) {
	if l, ok := d.swaps.exited(id); ok {
		go d.handBackResume(context.Background(), l)
	}
}

// handBackResume resumes the session a closed drop-in terminal held, on the
// drop-in caller's behalf. The to_headless intent is durable before the
// resume; without it the session stays dormant.
func (d sessionRouteDeps) handBackResume(ctx context.Context, l swapLink) {
	host := l.Host
	if host == "" {
		host = "console"
	}
	ev := logging.BeginEvent(ctx, "session.hand_back").
		Set("session_id", l.SessionID).Set("terminal_id", l.TerminalID).Set("host", host)

	rec, ok := d.sessions.Get(l.SessionID)
	if !ok {
		ev.End(logging.OutcomeDenied, "session_not_found", errors.New("session_not_found"))
		return
	}
	auditID, err := d.swapIntent(callerAuditActor(l.Caller), swapToHeadless, l.SessionID, rec.ProjectID, d.swapHostID(rec.ProjectID))
	if err != nil {
		ev.End(logging.OutcomeDenied, swapAuditUnavailable, err)
		return
	}
	out := d.resumeSession(ctx, l.Caller, l.SessionID)
	if out.Status == http.StatusOK {
		d.swapCompletion(auditID, swapToHeadless, l.SessionID, l.TerminalID, audit.AuditOutcomeOK, "")
		ev.End(logging.OutcomeOK, "", nil)
		return
	}
	code := "resume_failed"
	if body, ok := out.Body.(map[string]string); ok && body["error"] != "" {
		code = body["error"]
	}
	outcome, reason := refusalOutcome(out.Status, code)
	auditOutcome := audit.AuditOutcomeDenied
	if out.Status >= http.StatusInternalServerError {
		auditOutcome = audit.AuditOutcomeError
	}
	d.swapCompletion(auditID, swapToHeadless, l.SessionID, l.TerminalID, auditOutcome, code)
	ev.End(outcome, reason, errors.New(code))
}
