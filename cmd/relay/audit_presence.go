package main

import (
	"context"
	"os"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/bridge"
)

// requireGate records a refusal from presenceAttempt and never reads it for
// anything else: it must not become an input to the gate's decision.
type presenceAttempt struct {
	auditor IssuanceAuditor // nil records nothing
	via     string
	credID  string // set only for auditViaHTTP
	subject string
}

type presenceRefusalRecorder interface {
	Enabled() bool
	RecordPresenceRefusal(audit.PresenceRefusal)
}

var _ presenceRefusalRecorder = (*audit.AuditRecorder)(nil)

// pendingPresenceRefusal holds what must be captured before the prompt: the
// caller may leave while it is up, and once it has, neither its session nor
// its pid can be resolved any more.
type pendingPresenceRefusal struct {
	rec   presenceRefusalRecorder
	actor audit.AuditActor
	start time.Time
	via   string
	sub   string
}

// begin returns nil when there is nowhere to record, so a core with auditing
// off pays for no actor lookup.
func (a presenceAttempt) begin(ctx context.Context) *pendingPresenceRefusal {
	rec, ok := a.auditor.(presenceRefusalRecorder)
	if !ok || !rec.Enabled() {
		return nil
	}
	return &pendingPresenceRefusal{
		rec:   rec,
		actor: presenceRequester(ctx, a.via, a.credID),
		start: time.Now(),
		via:   a.via,
		sub:   a.subject,
	}
}

func (p *pendingPresenceRefusal) refused(op string, err error) {
	if p == nil || err == nil {
		return
	}
	p.rec.RecordPresenceRefusal(audit.PresenceRefusal{
		Op:      op,
		Subject: p.sub,
		Via:     p.via,
		Actor:   p.actor,
		Start:   p.start,
		Dur:     time.Since(p.start),
		Reason:  err.Error(),
	})
}

// presenceRequester attributes a refused prompt to whoever asked. It is
// attribution only and must never feed an authorization decision.
func presenceRequester(ctx context.Context, via, credID string) audit.AuditActor {
	if via == auditViaHTTP {
		return audit.AuditActor{Kind: audit.AuditActorControl, Auth: audit.AuditAuthToken, CredID: credID}
	}
	pid := bridge.CallerPIDFromContext(ctx)
	if member, ok := bridge.ConnMembershipFromContext(ctx).Session(); ok {
		actor := audit.AuditActor{Kind: audit.AuditActorProjectSession, Auth: audit.AuditAuthSession, ProjectID: member.ProjectID, SessionID: member.SessionID}
		if pid > 0 {
			actor.PID = pid
			actor.Proc, actor.Parent = audit.ProcessNames(pid)
		}
		return actor
	}
	if pid <= 0 {
		pid = os.Getpid()
	}
	proc, parent := audit.ProcessNames(pid)
	return audit.AuditActor{Kind: audit.AuditActorOperator, Auth: audit.AuditAuthNone, PID: pid, Proc: proc, Parent: parent}
}
