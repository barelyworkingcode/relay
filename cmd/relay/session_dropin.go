package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"

	"github.com/google/uuid"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/sessions/codexdaemon"
	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
	"github.com/barelyworkingcode/relay/internal/sessions/ledger"
	"github.com/barelyworkingcode/relay/internal/sessions/sandbox"
	sessiontypes "github.com/barelyworkingcode/relay/internal/sessions/types"
	"github.com/barelyworkingcode/relay/internal/sshhost"
)

const (
	defaultDropInCols = 120
	defaultDropInRows = 40
)

// Refusal codes the core adds to AuthorizeLaunch's own.
const (
	dropInSessionNotFound = "session_not_found"
	dropInNotClaude       = "not_claude"
	dropInNotLocal        = "not_local"
	dropInUnavailable     = "unavailable"
	dropInLaunchFailed    = "launch_failed"
	dropInAuditOff        = "audit_unavailable"
)

type DropInRequest struct {
	Caller     LaunchCaller
	SessionID  string
	Cols, Rows int
}

type DropInResult struct {
	SessionID string
	// Agent is the session's kind; ConversationID is its Claude session id, Pi
	// session uuid or Codex thread uuid.
	Agent          string
	ConversationID string
	// ClaudeSessionID is set for Claude only.
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

// dropIn stops a headless Claude, Pi or Codex session and launches a terminal
// that resumes its conversation, for both doors. AuthorizeLaunch runs before anything is
// stopped, so a caller that may not launch cannot disturb a session. Once the
// host holds the session, every failure hands it back.
func (d sessionRouteDeps) dropIn(ctx context.Context, req DropInRequest) (res *DropInResult, refusal *DropInRefusal) {
	ev := logging.BeginEvent(ctx, "session.drop_in")
	host, hostID, terminalID := "", "", ""
	defer func() {
		endDropInEvent(ev, req.SessionID, host, terminalID, refusal)
	}()

	if !d.ready() || d.sessions == nil {
		return nil, &DropInRefusal{Status: http.StatusBadGateway, Code: dropInUnavailable, Message: "the session host is not available"}
	}
	rec, ok := d.sessions.Get(req.SessionID)
	if !ok {
		return nil, &DropInRefusal{Status: http.StatusNotFound, Code: dropInSessionNotFound, Message: fmt.Sprintf("no session %s", req.SessionID)}
	}
	if rec.Kind != KindClaude && rec.Kind != KindPi && rec.Kind != KindCodex {
		return nil, &DropInRefusal{Status: http.StatusConflict, Code: dropInNotClaude,
			Message: fmt.Sprintf("only Claude, Pi and Codex sessions can be taken over; this is a %s session", rec.Kind)}
	}
	hosted := false
	if proj, _ := config.FindProjectByID(config.FreshSettings(d.store), rec.ProjectID); proj != nil && proj.IsHosted() {
		hosted = true
		if h, _ := config.FindHostByID(config.FreshSettings(d.store), proj.HostID); h != nil {
			host = h.Name
			hostID = h.ID
		}
	}
	if hosted && rec.Kind != KindClaude {
		return nil, &DropInRefusal{Status: http.StatusConflict, Code: dropInNotLocal, Message: "Pi and Codex sessions drop in only on this Mac"}
	}
	// An attached thread's terminal talks to the daemon, so the daemon must be
	// reachable before anything stops.
	var daemon codexdaemon.Identity
	attached := rec.Kind == KindCodex && rec.CodexThread != ""
	if attached {
		id, err := resolveCodexDaemon()
		if err != nil {
			slog.WarnContext(ctx, "session drop-in: codex daemon not reachable", "session", req.SessionID, "error", err)
			return nil, &DropInRefusal{Status: http.StatusBadGateway, Code: dropInUnavailable, Message: "the codex daemon is not reachable; the session was not taken over"}
		}
		daemon = id
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
		TemplateID: kindTemplateIDs[rec.Kind], // the project's own template for the kind: its sandbox, model key and host handling apply
		Directory:  rec.Directory,
		Name:       name + " (drop-in)",
		Cols:       cols,
		Rows:       rows,

		DropInAgent: rec.Kind,
	}
	result, lr := AuthorizeLaunch(d.store, d.modelKeys, d.sessions, launchReq)
	if lr != nil {
		d.auditor.Record(lr.Audit)
		return nil, &DropInRefusal{Status: lr.Status, Code: lr.Code, Message: lr.Message}
	}
	terminalID = result.SessionID

	// The intent row is durable before anything stops; a swap relay cannot
	// record does not start.
	actor := callerAuditActor(req.Caller)
	swapID, err := d.swapIntent(actor, swapToTerminal, req.SessionID, rec.ProjectID, hostID, swapAgent(rec.Kind))
	if err != nil {
		d.discardLaunch(result)
		return nil, &DropInRefusal{Status: http.StatusServiceUnavailable, Code: dropInAuditOff,
			Message: "auditing is off or cannot record; the session was not taken over"}
	}
	complete := func(outcome, code string) {
		d.swapCompletion(swapID, swapToTerminal, req.SessionID, terminalID, swapAgent(rec.Kind), outcome, code)
	}
	link := swapLink{SessionID: req.SessionID, TerminalID: terminalID, Host: host, Caller: req.Caller}

	// Deliberate: past this point relay-sessions completes the work whatever
	// happens to the request ctx, and a cancelled request must still hand the
	// session back. Only the handoff's own wait follows ctx.
	hold := context.WithoutCancel(ctx)
	target, hr, err := d.host().Handoff(ctx, req.SessionID)
	switch {
	case hr != nil:
		d.discardLaunch(result)
		d.auditor.Record(newSessionLaunchAuditEvent(result.AuditFields, audit.AuditOutcomeDenied, hr.Message))
		complete(audit.AuditOutcomeDenied, hr.Error)
		status := http.StatusConflict
		if hr.Error == dropInSessionNotFound {
			status = http.StatusNotFound
		}
		return nil, &DropInRefusal{Status: status, Code: hr.Error, Message: hr.Message}
	case err != nil:
		d.discardLaunch(result)
		d.handBack(hold, req.SessionID)
		d.auditor.Record(newSessionLaunchAuditEvent(result.AuditFields, audit.AuditOutcomeError, err.Error()))
		complete(audit.AuditOutcomeError, dropInUnavailable)
		return nil, &DropInRefusal{Status: http.StatusBadGateway, Code: dropInUnavailable, Message: "the session host is not available"}
	}

	extra, reason := dropInTerminalArgs(rec, target, daemon, result.Spec.Sandbox != nil)
	if reason != "" {
		d.discardLaunch(result)
		d.handBack(hold, req.SessionID)
		d.auditor.Record(newSessionLaunchAuditEvent(result.AuditFields, audit.AuditOutcomeError, reason))
		complete(audit.AuditOutcomeError, dropInLaunchFailed)
		link.TerminalGone = true
		d.armSwap(link)
		return nil, &DropInRefusal{Status: http.StatusBadGateway, Code: dropInLaunchFailed, Message: "relay could not start the terminal; the session is idle again"}
	}
	pane, onPane := []string(nil), false
	if rec.Kind == KindClaude {
		pane, onPane = d.paneDropInArgv(req.SessionID, rec.ProjectID, result, target.ConversationID)
	}
	if onPane {
		result.Spec.Argv = pane
		result.Spec.Name = launchReq.Name
	} else {
		result.Spec.Argv = append(result.Spec.Argv, extra...)
	}
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
		complete(audit.AuditOutcomeError, dropInLaunchFailed)
		link.TerminalGone = true
		d.armSwap(link)
		return nil, &DropInRefusal{Status: http.StatusBadGateway, Code: dropInLaunchFailed, Message: "relay could not start the terminal; the session is idle again"}
	}
	d.auditor.Record(newSessionLaunchAuditEvent(result.AuditFields, audit.AuditOutcomeOK, ""))
	complete(audit.AuditOutcomeOK, "")
	d.armSwap(link)
	claudeID := ""
	if rec.Kind == KindClaude {
		claudeID = target.ConversationID
	}
	return &DropInResult{
		SessionID: req.SessionID, Agent: rec.Kind, ConversationID: target.ConversationID, ClaudeSessionID: claudeID, Host: host,
		ProjectName: result.AuditFields.ProjectName, Launch: result, TerminalBody: resp.Body,
	}, nil
}

// resolveCodexDaemon finds the codex daemon's control socket by the same safety
// rules the attach uses.
func resolveCodexDaemon() (codexdaemon.Identity, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return codexdaemon.Identity{}, err
	}
	return codexdaemon.Resolve(home)
}

// dropInTerminalArgs is the argv a drop-in appends to the template's own. A
// non-empty reason means the host's answer does not match the session, and the
// terminal must not start. Deliberate: a Relay-started Codex terminal runs
// without Codex's own sandbox when relay's profile confines it, because Codex
// cannot apply its seatbelt inside another; the headless runner does the same.
// An attached thread's terminal gets no setting at all: it keeps the policy
// the thread already runs under.
func dropInTerminalArgs(rec ledger.Record, t hostapi.HandoffResponse, daemon codexdaemon.Identity, sandboxed bool) (argv []string, reason string) {
	if t.Agent != rec.Kind {
		return nil, "host returned a different agent than the session's"
	}
	if _, err := uuid.Parse(t.ConversationID); err != nil {
		return nil, "host returned a conversation id that is not a UUID"
	}
	switch rec.Kind {
	case KindClaude:
		return []string{"--resume", t.ConversationID}, ""
	case KindPi:
		a := t.PiSessionArgs
		if len(a) != 4 || a[0] != "--session" || a[2] != "--session-dir" || !filepath.IsAbs(a[3]) ||
			(a[1] != t.ConversationID && !filepath.IsAbs(a[1])) {
			return nil, "host returned unexpected Pi session arguments"
		}
		return slices.Clone(a), ""
	case KindCodex:
		if t.CodexAttach != (rec.CodexThread != "") {
			return nil, "host and ledger disagree on whether the Codex thread is attached"
		}
		if t.CodexAttach {
			if t.ConversationID != rec.CodexThread {
				return nil, "host returned a different Codex thread than the session's"
			}
			return []string{"resume", "--remote", "unix://" + daemon.Physical, t.ConversationID}, ""
		}
		argv = []string{"resume", "--no-daemon"}
		if sandboxed {
			argv = append(argv, "--sandbox", "danger-full-access")
		}
		return append(argv, t.ConversationID), ""
	}
	return nil, "unsupported session kind"
}

// paneDropInArgv is the terminal's argv on a host whose detached agent runs as
// a tmux pane: the pane swaps to Claude's UI and the terminal attaches to it.
// It is false when the host has no tmux path or the template has no command,
// and the terminal then launches as it does without a pane. Deliberate: the UI
// is the template's command and args, never its persist wrapper, because the
// pane is already the tmux session.
func (d sessionRouteDeps) paneDropInArgv(sessionID, projectID string, result *LaunchResult, claudeID string) ([]string, bool) {
	var host sessiontypes.HostSpec
	if len(result.Spec.Host) == 0 || json.Unmarshal(result.Spec.Host, &host) != nil || !host.Detach || host.TmuxPath == "" {
		return nil, false
	}
	settings := config.FreshSettings(d.store)
	proj, _ := config.FindProjectByID(settings, projectID)
	if proj == nil || !proj.IsHosted() {
		return nil, false
	}
	tmpl, ok := findTemplate(settings, proj, kindTemplateIDs[KindClaude])
	if !ok {
		return nil, false
	}
	ui := resolveHostArgv(tmpl, proj.Path, projectID)
	if len(ui) == 0 {
		return nil, false
	}
	ui = append(ui, "--resume", claudeID)
	return sshhost.PaneDropInArgv(host.TmuxPath, sshhost.AgentPaneName(sessionID), result.Spec.Directory, ui, result.Spec.Env), true
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

// endDropInEvent writes the one session.drop_in event per request that
// reached the core. A refusal below 500 is the caller's to fix (denied); 5xx is
// relay's. The error text is the refusal's code.
func endDropInEvent(ev *logging.Event, sessionID, host, terminalID string, refusal *DropInRefusal) {
	if host == "" {
		host = "console"
	}
	if refusal != nil {
		terminalID = ""
	}
	ev.Set("session_id", sessionID).Set("host", host).Set("terminal_id", terminalID)
	if refusal == nil {
		ev.End(logging.OutcomeOK, "", nil)
		return
	}
	outcome, reason := refusalOutcome(refusal.Status, refusal.Code)
	if refusal.Code == dropInAuditOff {
		outcome = logging.OutcomeDenied
	}
	ev.End(outcome, reason, errors.New(refusal.Code))
}

type dropInWireBody struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

type dropInResponseBody struct {
	SessionID       string          `json:"sessionId"`
	Agent           string          `json:"agent"`
	ConversationID  string          `json:"conversationId"`
	ClaudeSessionID string          `json:"claudeSessionId,omitempty"`
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
		SessionID: res.SessionID, Agent: res.Agent, ConversationID: res.ConversationID,
		ClaudeSessionID: res.ClaudeSessionID, Host: res.Host, Terminal: terminal,
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
