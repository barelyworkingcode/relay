package main

import (
	"context"
	"errors"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/service"
)

// interruptMethods names the wire method each adopted agent's interrupt uses.
var interruptMethods = map[string]string{KindPi: "abort", KindCodex: "turn/interrupt"}

// The acknowledgement certifies persistence of intent, not delivery of abort.
// The session host owns delivery and keeps the turn unchanged on refusal.
func (r *appRouter) SessionInterrupt(ctx context.Context, req bridge.SessionInterruptRequest, token string) (err error) {
	ev := logging.BeginEvent(ctx, "session.interrupt").Set("session_id", req.SessionID)
	defer func() { endEvent(ev, err) }()
	if _, err = r.requireServiceIdentity(ctx, token, service.OpSessionInterrupt); err != nil {
		return err
	}
	if r.sessions == nil {
		return errors.New("session interrupt: no session ledger")
	}
	rec, ok := r.sessions.Get(req.SessionID)
	if !ok || (rec.Kind != KindPi && rec.Kind != KindCodex) {
		return errors.New("session interrupt: no Pi or codex session")
	}
	if r.audit == nil || !r.audit.Ready() {
		return errors.New("session interrupt: audit unavailable")
	}
	return r.audit.RecordDurable(audit.AuditEvent{ID: audit.NewAuditID(), TS: time.Now(), Event: audit.AuditEventSessionInterrupt, Phase: audit.AuditPhaseIntent, Actor: audit.AuditActor{Kind: audit.AuditActorService, Auth: audit.AuditAuthNone, ServiceID: config.RelaySessionsServiceID, SessionID: req.SessionID}, Outcome: audit.AuditOutcomePending, Args: mustJSON(map[string]string{"session_id": req.SessionID, "project_id": rec.ProjectID, "agent": rec.Kind, "method": interruptMethods[rec.Kind]})})
}
