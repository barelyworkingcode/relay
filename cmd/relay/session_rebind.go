package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/jsonrpc"
	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/membership"
	"github.com/barelyworkingcode/relay/internal/peertoken"
	"github.com/barelyworkingcode/relay/internal/service"
	"github.com/barelyworkingcode/relay/internal/sessions/ledger"
	"github.com/barelyworkingcode/relay/internal/sessions/runner"
)

// The reasons a rebind is refused after the caller is admitted. They go to the
// event line and the audit row only; the caller learns nothing but the refusal.
const (
	rebindNotLive        = "not_live"
	rebindProjectGone    = "project_gone"
	rebindUnsandboxed    = "unsandboxed"
	rebindNoRootRecord   = "no_root_record"
	rebindRunnerMismatch = "runner_mismatch"
	rebindRootGone       = "root_gone"
	rebindRootNotInTree  = "root_not_in_tree"
	rebindAlreadyBound   = "already_bound"
	rebindBindFailed     = "bind_failed"
	rebindAuditOff       = "audit_unavailable"
	rebindUnauthorized   = "unauthorized"
)

// errSessionRebindRefused is the one answer every refusal after the caller's
// own check gives, so a caller cannot tell which check held.
var errSessionRebindRefused = jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, errors.New("session rebind refused"))

// rebindRunnerRoots is the process table the tree check walks: the claimed
// runner is the only root, and relay's own pid stops the walk.
type rebindRunnerRoots struct {
	runner membership.Root
}

func (r rebindRunnerRoots) RootByPID(pid int) (membership.Root, bool) {
	return r.runner, pid == r.runner.PID
}

func (r rebindRunnerRoots) HostPID() int { return os.Getpid() }

// rebindArgs is the args both audit rows of one rebind carry.
type rebindArgs struct {
	SessionID string `json:"session_id"`
	ProjectID string `json:"project_id,omitempty"`
	RunnerPID int    `json:"runner_pid,omitempty"`
	RootPID   int32  `json:"root_pid,omitempty"`
}

// SessionRebind binds a local session's surviving root process to a fresh
// launch identity after a relay restart. It takes no secret and mints none:
// the process identity relay recorded at launch, and the kernel's view of the
// runner's tree, are the whole claim. The gate runs in a fixed order and the
// first failure stops it; the intent row is on disk before any check reads the
// kernel, and a rebind that cannot be recorded binds nothing.
func (r *appRouter) SessionRebind(ctx context.Context, req bridge.SessionRebindRequest, token string) error {
	ev := logging.BeginEvent(ctx, "session.rebind").Set("session_id", req.SessionID)

	if _, err := r.requireServiceIdentity(ctx, token, service.OpSessionRebind); err != nil {
		r.audit.Record(audit.AuditEvent{
			ID: audit.NewAuditID(), TS: time.Now(), Event: audit.AuditEventSessionRebind,
			Actor:   audit.AuditActor{Kind: audit.AuditActorUnknown, Auth: audit.AuditAuthNone},
			Args:    mustJSON(map[string]string{"session_id": req.SessionID}),
			Outcome: audit.AuditOutcomeUnauthorized,
		})
		ev.End(logging.OutcomeDenied, rebindUnauthorized, err)
		return err
	}

	var rec ledger.Record
	var held bool
	if r.sessions != nil {
		rec, held = r.sessions.Get(req.SessionID)
	}
	args := rebindArgs{SessionID: req.SessionID, ProjectID: rec.ProjectID, RunnerPID: req.RunnerPID}
	if rec.Root != nil {
		args.RootPID = rec.Root.PID
	}
	if held {
		ev.Set("project_id", rec.ProjectID)
	}
	actor := audit.AuditActor{
		Kind: audit.AuditActorService, Auth: audit.AuditAuthNone,
		ServiceID: config.RelaySessionsServiceID, SessionID: req.SessionID,
	}
	intent := audit.AuditEvent{
		ID: audit.NewAuditID(), TS: time.Now(), Event: audit.AuditEventSessionRebind, Phase: audit.AuditPhaseIntent,
		Actor: actor, Args: mustJSON(args), Outcome: audit.AuditOutcomePending,
	}
	if err := r.audit.RecordDurable(intent); err != nil {
		ev.End(logging.OutcomeError, rebindAuditOff, err)
		return errSessionRebindRefused
	}
	start := time.Now()

	launch, code := r.rebindRoot(req, rec, held)

	completion := intent
	completion.Phase = audit.AuditPhaseCompletion
	completion.DurMs = time.Since(start).Milliseconds()
	completion.Outcome = audit.AuditOutcomeOK
	outcome := logging.OutcomeOK
	switch code {
	case "":
	case rebindBindFailed:
		completion.Outcome, completion.Error, outcome = audit.AuditOutcomeError, code, logging.OutcomeError
	default:
		completion.Outcome, completion.Error, outcome = audit.AuditOutcomeDenied, code, logging.OutcomeDenied
	}
	// Deliberate: a completion row that cannot be written leaves the binding in
	// place. The intent row is the one that gates the act.
	if err := r.audit.RecordDurable(completion); err != nil {
		slog.Warn("session rebind: completion row not written", "session", req.SessionID, "error", err)
	}

	if code != "" {
		slog.Warn("session rebind refused", "session", req.SessionID, "reason", code)
		ev.End(outcome, code, errSessionRebindRefused)
		return errSessionRebindRefused
	}
	r.sessionAccounts.track(req.SessionID, sessionAccount{
		projectID: rec.ProjectID, launch: launch,
		sandboxProfile: filepath.Join(sessionProfilesDir(), req.SessionID+".sb"),
	})
	ev.End(logging.OutcomeOK, "", nil)
	return nil
}

// rebindRoot runs gate steps c to j and returns the new launch, or the code of
// the first step that failed.
func (r *appRouter) rebindRoot(req bridge.SessionRebindRequest, rec ledger.Record, held bool) (*service.Launch, string) {
	if !held || rec.State != ledger.StateLive || rec.Kind != KindClaude || rec.Detached != nil {
		return nil, rebindNotLive
	}
	// An unsandboxed session has no launch identity to bind, and its record has
	// no project to be gone: it keeps its own reason.
	if rec.Unsandboxed {
		return nil, rebindUnsandboxed
	}
	if r.store == nil || r.launches == nil {
		return nil, rebindProjectGone
	}
	// Authorization reads are fresh: the project may have been deleted or made
	// remote while relay was down.
	proj, _ := config.FindProjectByID(config.FreshSettings(r.store), rec.ProjectID)
	if proj == nil || proj.IsRemote() || proj.IsHosted() {
		return nil, rebindProjectGone
	}
	if rec.Root == nil || rec.Root.PID <= 1 {
		return nil, rebindNoRootRecord
	}

	src := membership.NewSource()
	st, err := runner.LocalStatus(filepath.Join(bridge.ConfigDir(), "sessions", "runners"), req.SessionID, "")
	if err != nil || st.Phase != runner.PhaseAlive || st.State.PID != req.RunnerPID {
		return nil, rebindRunnerMismatch
	}
	runnerInfo, ok := src.Info(req.RunnerPID)
	if !ok || runnerInfo.StartSec != req.RunnerStartSec || runnerInfo.StartUsec != req.RunnerStartUsec {
		return nil, rebindRunnerMismatch
	}

	rootInfo, ok := src.Info(int(rec.Root.PID))
	if !ok || rootInfo.StartSec != rec.Root.StartSec || rootInfo.StartUsec != rec.Root.StartUsec {
		return nil, rebindRootGone
	}

	// The runner itself is never a root: a record naming it would bind the
	// runner, and the walk would match it at once.
	if int(rec.Root.PID) == req.RunnerPID {
		return nil, rebindRootNotInTree
	}
	roots := rebindRunnerRoots{runner: membership.Root{
		SessionID: req.SessionID, PID: req.RunnerPID, StartSec: runnerInfo.StartSec, StartUsec: runnerInfo.StartUsec,
	}}
	if sid, inTree := membership.Resolve(src, roots, int(rec.Root.PID), time.Now()); !inTree || sid != req.SessionID {
		return nil, rebindRootNotInTree
	}

	launch, err := r.launches.Rebind(service.Identity{
		Kind: service.IdentityKindProjectSession, Name: req.SessionID, SessionID: req.SessionID,
		ProjectID: rec.ProjectID, ParentLaunch: config.RelaySessionsServiceID,
	}, peertoken.Process{PID: rec.Root.PID, PIDVersion: rec.Root.PIDVersion}, rec.Root.StartSec, rec.Root.StartUsec)
	switch {
	case errors.Is(err, service.ErrRebindAlreadyBound):
		return nil, rebindAlreadyBound
	case err != nil:
		return nil, rebindBindFailed
	}
	return launch, ""
}
