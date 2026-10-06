package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/sessions/sandbox"
)

// dropInTemplateID is the terminal template a drop-in launches: the project's
// own Claude terminal, so its sandbox, model key and host handling apply.
const dropInTemplateID = "claude-code"

const (
	defaultDropInCols = 120
	defaultDropInRows = 40
)

// Refusal codes the core adds to AuthorizeLaunch's own.
const (
	dropInSessionNotFound = "session_not_found"
	dropInNotClaude       = "not_claude"
	dropInUnavailable     = "unavailable"
	dropInLaunchFailed    = "launch_failed"
)

type DropInRequest struct {
	Caller     LaunchCaller
	SessionID  string
	Cols, Rows int
}

type DropInResult struct {
	SessionID       string
	ClaudeSessionID string
	// Host is the SSH host's name, empty for this machine.
	Host        string
	ProjectName string
	Launch      *LaunchResult
	// TerminalBody is the host's create body for the terminal, the object
	// POST /api/terminals returns.
	TerminalBody json.RawMessage
}

// DropInRefusal is a drop-in the core did not carry out. Status is the HTTP
// status the HTTP door answers with; the bridge door uses Code and Message.
type DropInRefusal struct {
	Status        int
	Code, Message string
}

func (r *DropInRefusal) Error() string { return r.Message }

// dropIn stops a headless Claude session and launches a terminal that resumes
// its conversation, for both doors. AuthorizeLaunch runs before anything is
// stopped, so a caller that may not launch cannot disturb a session. Once the
// host holds the session, every failure hands it back.
func (d sessionRouteDeps) dropIn(ctx context.Context, req DropInRequest) (res *DropInResult, refusal *DropInRefusal) {
	start := time.Now()
	host, terminalID := "", ""
	defer func() {
		d.logDropIn(ctx, req.SessionID, host, terminalID, refusal, time.Since(start))
	}()

	if !d.ready() || d.sessions == nil {
		return nil, &DropInRefusal{Status: http.StatusBadGateway, Code: dropInUnavailable, Message: "the session host is not available"}
	}
	rec, ok := d.sessions.Get(req.SessionID)
	if !ok {
		return nil, &DropInRefusal{Status: http.StatusNotFound, Code: dropInSessionNotFound, Message: fmt.Sprintf("no session %s", req.SessionID)}
	}
	if rec.Kind != KindClaude {
		return nil, &DropInRefusal{Status: http.StatusConflict, Code: dropInNotClaude,
			Message: fmt.Sprintf("only Claude sessions can be taken over; this is a %s session", rec.Kind)}
	}
	if proj, _ := config.FindProjectByID(config.FreshSettings(d.store), rec.ProjectID); proj != nil && proj.IsHosted() {
		if h, _ := config.FindHostByID(config.FreshSettings(d.store), proj.HostID); h != nil {
			host = h.Name
		}
	}

	cols, rows := req.Cols, req.Rows
	if cols <= 0 || rows <= 0 {
		cols, rows = defaultDropInCols, defaultDropInRows
	}
	name := "session"
	if stored, err := decodeStoredSessionRequest(rec); err == nil && stored.Name != "" {
		name = stored.Name
	}
	launchReq := LaunchRequest{
		Caller:     req.Caller,
		ProjectID:  rec.ProjectID,
		Kind:       KindPTY,
		TemplateID: dropInTemplateID,
		Directory:  rec.Directory,
		Name:       name + " (drop-in)",
		Cols:       cols,
		Rows:       rows,
	}
	result, lr := AuthorizeLaunch(d.store, d.modelKeys, d.sessions, launchReq)
	if lr != nil {
		d.auditor.Record(lr.Audit)
		return nil, &DropInRefusal{Status: lr.Status, Code: lr.Code, Message: lr.Message}
	}
	terminalID = result.SessionID

	// Deliberate: past this point relay-sessions completes the work whatever
	// happens to the request ctx, and a cancelled request must still hand the
	// session back. Only the handoff's own wait follows ctx.
	hold := context.WithoutCancel(ctx)
	claudeID, hr, err := d.host().Handoff(ctx, req.SessionID)
	switch {
	case hr != nil:
		d.discardLaunch(result)
		d.auditor.Record(newSessionLaunchAuditEvent(result.AuditFields, audit.AuditOutcomeDenied, hr.Message))
		status := http.StatusConflict
		if hr.Error == dropInSessionNotFound {
			status = http.StatusNotFound
		}
		return nil, &DropInRefusal{Status: status, Code: hr.Error, Message: hr.Message}
	case err != nil:
		d.discardLaunch(result)
		d.handBack(hold, req.SessionID)
		d.auditor.Record(newSessionLaunchAuditEvent(result.AuditFields, audit.AuditOutcomeError, err.Error()))
		return nil, &DropInRefusal{Status: http.StatusBadGateway, Code: dropInUnavailable, Message: "the session host is not available"}
	}

	if _, err := uuid.Parse(claudeID); err != nil {
		d.discardLaunch(result)
		d.handBack(hold, req.SessionID)
		d.auditor.Record(newSessionLaunchAuditEvent(result.AuditFields, audit.AuditOutcomeError, "host returned a conversation id that is not a UUID"))
		return nil, &DropInRefusal{Status: http.StatusBadGateway, Code: dropInLaunchFailed, Message: "relay could not start the terminal; the session is idle again"}
	}
	result.Spec.Argv = append(result.Spec.Argv, "--resume", claudeID)
	result.Spec.DropInFor = req.SessionID

	resp, err := d.launchOnHost(hold, result)
	if err == nil && !d.commitLaunch(hold, result) {
		err = errors.New("project no longer exists")
	} else if err != nil {
		slog.WarnContext(ctx, "session drop-in: terminal launch failed", "session", req.SessionID, "error", err)
	}
	if err != nil {
		d.handBack(hold, req.SessionID)
		d.auditor.Record(newSessionLaunchAuditEvent(result.AuditFields, audit.AuditOutcomeError, err.Error()))
		return nil, &DropInRefusal{Status: http.StatusBadGateway, Code: dropInLaunchFailed, Message: "relay could not start the terminal; the session is idle again"}
	}
	d.auditor.Record(newSessionLaunchAuditEvent(result.AuditFields, audit.AuditOutcomeOK, ""))
	return &DropInResult{
		SessionID: req.SessionID, ClaudeSessionID: claudeID, Host: host,
		ProjectName: result.AuditFields.ProjectName, Launch: result, TerminalBody: resp.Body,
	}, nil
}

// discardLaunch undoes what AuthorizeLaunch minted for a launch that never
// reached the host.
func (d sessionRouteDeps) discardLaunch(result *LaunchResult) {
	if result.Spec.ModelKey != "" {
		d.modelKeys.RevokeKey(result.Spec.ModelKey)
	}
	if err := sandbox.Remove(sandboxProfilePath(result)); err != nil {
		slog.Warn("session drop-in: sandbox profile removal failed", "session", result.SessionID, "error", err)
	}
}

func (d sessionRouteDeps) handBack(ctx context.Context, sessionID string) {
	if err := d.host().Handback(ctx, sessionID); err != nil {
		slog.Warn("session drop-in: handback failed", "session", sessionID, "error", err)
	}
}

// logDropIn writes the one session.drop_in line per request that reached the
// core. A refusal below 500 is the caller's to fix (denied); 5xx is relay's.
func (d sessionRouteDeps) logDropIn(ctx context.Context, sessionID, host, terminalID string, refusal *DropInRefusal, took time.Duration) {
	level, status, code := slog.LevelInfo, "ok", ""
	switch {
	case refusal == nil:
	case refusal.Status >= http.StatusInternalServerError:
		level, status, code = slog.LevelError, "error", refusal.Code
	default:
		level, status, code = slog.LevelWarn, "denied", refusal.Code
	}
	if host == "" {
		host = "console"
	}
	if refusal != nil {
		terminalID = ""
	}
	slog.LogAttrs(ctx, level, "session drop-in",
		slog.String("op", "session.drop_in"),
		slog.String("status", status),
		slog.Int64("duration_ms", took.Milliseconds()),
		slog.String("error", code),
		slog.String("trace_id", logging.TraceFromContext(ctx)),
		slog.String("session_id", sessionID),
		slog.String("host", host),
		slog.String("terminal_id", terminalID),
	)
}

type dropInWireBody struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

type dropInResponseBody struct {
	SessionID       string          `json:"sessionId"`
	ClaudeSessionID string          `json:"claudeSessionId"`
	Host            string          `json:"host,omitempty"`
	Terminal        json.RawMessage `json:"terminal"`
}

// handleDropIn implements POST /api/sessions/{id}/drop-in. The request can
// stay open up to hostapi.HandoffWait while the current turn ends.
func (d sessionRouteDeps) handleDropIn(w http.ResponseWriter, r *http.Request) {
	if d.sessionRoutesUnavailable(w) {
		return
	}
	var body dropInWireBody
	if r.ContentLength != 0 && !decodeSessionBody(w, r, &body) {
		return
	}
	res, refusal := d.dropIn(r.Context(), DropInRequest{
		Caller: resolveLaunchCaller(r, d.store), SessionID: r.PathValue("id"), Cols: body.Cols, Rows: body.Rows,
	})
	if refusal != nil {
		writeJSON(w, refusal.Status, map[string]string{"error": refusal.Code, "message": refusal.Message})
		return
	}
	terminal := res.TerminalBody
	if len(terminal) == 0 {
		terminal = json.RawMessage("{}")
	}
	writeJSON(w, http.StatusCreated, dropInResponseBody{
		SessionID: res.SessionID, ClaudeSessionID: res.ClaudeSessionID, Host: res.Host, Terminal: terminal,
	})
}

var _ bridge.DropInAttacher = (*appRouter)(nil)

// DropInAttach implements bridge.DropInAttacher: the same core as the HTTP
// route, then a relay-side viewer joined to the new terminal so the CLI's
// bytes can be relayed. It leaves the session handed back on any error.
func (r *appRouter) DropInAttach(ctx context.Context, req bridge.DropInAttachRequest) (bridge.DropInAttachment, error) {
	d := r.sessionDeps
	pid := bridge.CallerPIDFromContext(ctx)
	proc, parent := audit.ProcessNames(pid)
	res, refusal := d.dropIn(ctx, DropInRequest{
		Caller:    LaunchCaller{Operator: &OperatorCaller{PID: pid, Proc: proc, Parent: parent}},
		SessionID: req.SessionID,
		Cols:      req.Cols,
		Rows:      req.Rows,
	})
	if refusal != nil {
		return nil, &bridge.SandboxRefusal{Reason: refusal.Code, Message: refusal.Message}
	}

	terminalID := res.Launch.SessionID
	att, err := joinSandboxSession(ctx, d, terminalID, res.ProjectName)
	if err != nil {
		slog.WarnContext(ctx, "session drop-in: attach failed; ending the terminal", "session", req.SessionID, "terminal", terminalID, "error", err)
		// Ending the terminal is what hands the session back.
		endSandboxSession(ctx, d, terminalID, "drop_in_attach_failed")
		return nil, &bridge.SandboxRefusal{Reason: dropInUnavailable, Message: "relay started the terminal but could not attach to it; it has been ended"}
	}
	return &dropInAttachment{
		sandboxAttachment: att,
		result: bridge.DropInAttachResult{
			SessionID: res.SessionID, ClaudeSessionID: res.ClaudeSessionID, TerminalID: terminalID, Host: res.Host,
			Cols: att.result.Cols, Rows: att.result.Rows,
		},
	}, nil
}

// dropInAttachment is a sandbox attachment, which already pumps bytes and ends
// its terminal when the client goes, with a drop-in's own ack.
type dropInAttachment struct {
	*sandboxAttachment
	result bridge.DropInAttachResult
}

func (a *dropInAttachment) Result() bridge.DropInAttachResult { return a.result }
