package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/service"
	"github.com/barelyworkingcode/relay/internal/sessions/ledger"
)

// SessionExited implements the bridge.ToolRouter half of C5's advisory,
// tokenless SessionExited report: relay-sessions telling relay one of its
// sessions is gone. requireServiceIdentity (router.go) is the only
// authorization this needs -- the sessions capability config restricts to
// the built-in relay-sessions service (internal/config/models.go's
// sanitizeIfBuiltin), so a second name check here would be redundant with
// an already-enforced invariant, not a second layer of defense.
func (r *appRouter) SessionExited(ctx context.Context, req bridge.SessionExitedRequest, token string) error {
	if _, err := r.requireServiceIdentity(ctx, token, service.OpSessionExited); err != nil {
		return err
	}

	if acc, ok := r.sessionAccounts.take(req.SessionID); ok {
		acc.end(r.modelKeys)
	}

	if r.sessions != nil {
		switch req.Reason {
		case "deleted":
			if err := r.sessions.Remove(req.SessionID); err != nil {
				slog.Warn("session exited: ledger remove failed", "session", req.SessionID, "error", err)
			}
		default:
			// A terminal is never in the ledger (C5), so "no such record" here
			// is the ordinary pty case, not a failure worth logging.
			if _, ok := r.sessions.Get(req.SessionID); ok {
				if _, err := r.sessions.SetState(req.SessionID, ledger.StateDormant); err != nil {
					slog.Warn("session exited: ledger update failed", "session", req.SessionID, "error", err)
				}
			}
		}
	}

	r.recordSessionExited(req)
	return nil
}

// sessionExitedAuditArgs mirrors sessionLaunchAuditArgs' shape for a
// different event kind: session_id plus exactly what C5's report itself
// carries, nothing relay had to look up.
type sessionExitedAuditArgs struct {
	SessionID  string `json:"session_id,omitempty"`
	RootPID    int    `json:"root_pid,omitempty"`
	ExitStatus int    `json:"exit_status"`
	Reason     string `json:"reason,omitempty"`
}

// recordSessionExited records the session_end event C5's audit constants
// were provisioned for (internal/audit/audit.go's AuditEventSessionEnd) --
// nothing in this repo constructed one before this unit. The actor is a
// service actor (relay-sessions itself reported this, tokenlessly), never a
// project or a session: SessionExited names the session that ended, not the
// caller's own scope.
func (r *appRouter) recordSessionExited(req bridge.SessionExitedRequest) {
	args, _ := json.Marshal(sessionExitedAuditArgs{
		SessionID: req.SessionID, RootPID: req.RootPID, ExitStatus: req.ExitStatus, Reason: req.Reason,
	})
	r.audit.Record(audit.AuditEvent{
		ID:    audit.NewAuditID(),
		TS:    time.Now(),
		Event: audit.AuditEventSessionEnd,
		Actor: audit.AuditActor{
			Kind:      audit.AuditActorService,
			Auth:      audit.AuditAuthNone,
			ServiceID: config.RelaySessionsServiceID,
			SessionID: req.SessionID,
		},
		Outcome: audit.AuditOutcomeOK,
		Args:    args,
	})
}
