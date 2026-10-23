package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/service"
	"github.com/barelyworkingcode/relay/internal/sessions/ledger"
	"github.com/barelyworkingcode/relay/internal/sessions/sandbox"
)

// SessionExited implements the bridge.ToolRouter half of C5's advisory,
// tokenless SessionExited report: relay-sessions telling relay one of its
// sessions is gone. requireServiceIdentity (router.go) is the only
// authorization this needs -- the sessions capability config restricts to
// the built-in relay-sessions service (internal/config/models.go's
// sanitizeIfBuiltin), so a second name check here would be redundant with
// an already-enforced invariant, not a second layer of defense.
func (r *appRouter) SessionExited(ctx context.Context, req bridge.SessionExitedRequest, token string) (err error) {
	ev := logging.BeginEvent(ctx, "session.exited").Set("session_id", req.SessionID)
	defer func() { endEvent(ev, err) }()
	if _, err := r.requireServiceIdentity(ctx, token, service.OpSessionExited); err != nil {
		return err
	}

	// A delete report names whatever id the caller passed. Only a session
	// the ledger holds may be torn down by it: a terminal id or an invented
	// one must leave accounts and audit untouched.
	if req.Reason == "deleted" && r.sessions != nil {
		if _, ok := r.sessions.Get(req.SessionID); !ok {
			return nil
		}
	}

	if acc, ok := r.sessionAccounts.take(req.SessionID); ok {
		acc.end(r.modelKeys)
	} else if r.sessions != nil {
		// Deliberate: a session that outlived a relay restart has no account to
		// carry its sandbox profile, so the exit report is the only trigger left
		// to delete it. Only an id the ledger holds names a file.
		if _, held := r.sessions.Get(req.SessionID); held {
			if err := sandbox.Remove(filepath.Join(sessionProfilesDir(), req.SessionID+".sb")); err != nil {
				slog.Warn("session exited: sandbox profile removal failed", "session", req.SessionID, "error", err)
			}
		}
	}

	if r.sessions != nil {
		switch req.Reason {
		case "deleted":
			if err := r.sessions.Remove(req.SessionID); err != nil {
				slog.Warn("session exited: ledger remove failed", "session", req.SessionID, "error", err)
			}
		default:
			// Only a claude-code terminal has a ledger record; every other
			// terminal has none, so "no such record" here is the ordinary pty
			// case, not a failure worth logging.
			if rec, ok := r.sessions.Get(req.SessionID); ok && !r.detachedAgentStillRuns(rec) {
				if _, err := r.sessions.SetState(req.SessionID, ledger.StateDormant); err != nil {
					slog.Warn("session exited: ledger update failed", "session", req.SessionID, "error", err)
				}
			}
		}
	}

	r.sessionDeps.startHandBack(req.SessionID)

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
// were provisioned for (internal/audit/audit.go's AuditEventSessionEnd).
// The actor is a service actor (relay-sessions itself reported this,
// tokenlessly), never a project or a session: SessionExited names the
// session that ended, not the caller's own scope.
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

// detachedAgentStillRuns reports whether rec is a detached host session or a
// local runner session whose agent runs after its provider released it, as a
// relay-sessions restart leaves it. relay-sessions attaches that agent again as
// it starts, so the record stays live and project-delete cleanup still finds
// it.
func (r *appRouter) detachedAgentStillRuns(rec ledger.Record) bool {
	if rec.Detached == nil && runnerKind(rec.Kind) {
		return localRunnerAlive(bridge.ConfigDir(), rec.SessionID)
	}
	if rec.Detached == nil || r.sessionDeps.store == nil {
		return false
	}
	return detachedRunnerAlive(r.sessionDeps.store.Get(), rec)
}
