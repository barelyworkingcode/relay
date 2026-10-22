package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/outsidesessions"
	"github.com/barelyworkingcode/relay/internal/sessions/adopt"
	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
	"github.com/barelyworkingcode/relay/internal/sessions/ledger"
	sessiontypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// adoptHostLinkLost is the code of a host adopt whose result never arrived: the
// session shows as dormant, and a resume settles it.
const adoptHostLinkLost = "host_link_lost"

// hostTailBytes is the transcript tail the door reads over SSH, as the local
// check reads.
const hostTailBytes = 256 << 10

// hostRefusedPrefix is the start of the error launchOnHost builds from a
// relay-sessions refusal. Subtle: hostRefusalCode reads the code out of that
// text, so the two change together.
const hostRefusedPrefix = "relay-sessions refused: "

// hostRefusalCode is the error code relay-sessions answered /launch with, or ""
// when the call itself failed (no answer arrived).
func hostRefusalCode(err error) string {
	rest, ok := strings.CutPrefix(err.Error(), hostRefusedPrefix)
	if !ok {
		return ""
	}
	code, _, _ := strings.Cut(rest, ":")
	return code
}

// readHostTail reads the end of the row's transcript over SSH. whole is true
// when the read reached the file's start.
func readHostTail(ctx context.Context, h *config.Host, transcript string) (tail []byte, size int64, whole bool, err error) {
	if transcript == "" {
		return nil, 0, false, outsidesessions.ErrUnavailable
	}
	out, err := outsideRun(ctx, h, outsideRangeScript(transcript, -1, hostTailBytes))
	if err != nil {
		return nil, 0, false, err
	}
	tail, start, err := parseOutsideRange(out, -1, hostTailBytes)
	if err != nil {
		return nil, 0, false, err
	}
	return tail, start + int64(len(tail)), start == 0, nil
}

// hostChildTest chooses how the host script tests the Claude process's
// children, from what the tail says about the last turn. Fail closed: a
// boundary the tail cannot give, or a start the row does not state, tests every
// child.
func hostChildTest(tail []byte, whole bool, since string, newest time.Time) (test string, fromMs, toMs int64) {
	procStart, err := time.Parse(time.RFC3339Nano, since)
	if err != nil {
		return "all", 0, 0
	}
	from, known := adopt.TurnWindow(tail, whole, procStart)
	switch {
	case !known || newest.Before(from):
		return "all", 0, 0
	case from.IsZero():
		return "none", 0, 0
	}
	return "window", from.UnixMilli(), newest.UnixMilli()
}

// adoptHost adopts a Claude row of an SSH host. The transition itself is one
// script on the host (internal/sshhost/adopt.go); this door judges the row,
// writes the audit intents and the dormant record, then reads the answer.
// Nothing reaches the host before the intents and the record are durable.
func (a *adoptOps) adoptHost(ctx context.Context, req AdoptRequest, obs outsidesessions.Observation, hostID string, st *adoptState) (res *AdoptResult, refusal *AdoptRefusal) {
	d := a.deps
	row := obs.Session
	refuse := func(status int, code, message string) *AdoptRefusal {
		return &AdoptRefusal{Status: status, Code: code, Message: message}
	}
	unsupported := refuse(http.StatusConflict, adopt.ReasonHost, "sessions on this host cannot be adopted")

	settings := config.FreshSettings(d.store)
	h, _ := config.FindHostByID(settings, hostID)
	var proj *config.Project
	if row.ProjectID != nil {
		proj, _ = config.FindProjectByID(settings, *row.ProjectID)
	}
	if observationAgent(obs) != "claude" || h == nil || !hostAdopts(settings, h, proj) {
		return nil, unsupported
	}
	st.projectID = proj.ID

	managed, err := d.host().ManagedClaudeIdentities(ctx)
	if err != nil {
		return nil, refuse(http.StatusBadGateway, adoptUnavailable, "the session host is not available")
	}
	if managed[obs.ClaudeID] {
		return nil, refuse(http.StatusConflict, adoptManaged, "relay already holds this conversation")
	}

	tailBytes, size, whole, err := readHostTail(ctx, h, obs.Transcript)
	if err != nil {
		slog.WarnContext(ctx, "session adopt: host transcript unreadable", "outside_id", row.ID, "host_id", hostID, "error", err)
		return nil, refuse(http.StatusBadGateway, adoptUnavailable, "the session could not be inspected")
	}
	tail := adopt.ReadTail(tailBytes)
	veto := ""
	switch {
	case row.State != "idle":
		veto = adopt.VetoRegistryBusy
	case tail.OpenTool != nil:
		veto = adopt.VetoOpenToolCall
	}
	busy := veto != ""
	if busy && !req.Interrupt {
		return nil, refuse(http.StatusConflict, adoptBusy, fmt.Sprintf("the session is not idle (%s); ask to interrupt to take it over anyway", veto))
	}
	interrupt := busy

	// Deliberate: a host adopt never answers model_unknown. An empty model
	// starts the host's own default.
	model := req.Model
	if model == "" {
		model = tail.ModelFamily
	}

	result, lr := AuthorizeLaunch(d.store, d.modelKeys, d.sessions, LaunchRequest{
		Caller: req.Caller, ProjectID: proj.ID, Kind: KindClaude, Directory: row.Directory,
		Name: row.Name, Model: model, offer: d.offerFor(ctx),
		ClientSettings: json.RawMessage(`{"headless":true,"agent":true}`),
	})
	if lr != nil {
		d.auditor.Record(lr.Audit)
		return nil, refuse(lr.Status, lr.Code, lr.Message)
	}
	launched := false
	defer func() {
		if !launched {
			d.discardLaunch(result)
		}
	}()

	childTest, fromMs, toMs := hostChildTest(tailBytes, whole, row.Since, tail.LastRecord)
	result.Spec.ResumeConversation = obs.ClaudeID
	result.Spec.AdoptHost = &hostapi.AdoptHostSpec{
		PID: obs.PID, Lifetime: obs.Lifetime, Interrupt: interrupt, TranscriptSize: size,
		ChildTest: childTest, WindowFromMs: fromMs, WindowToMs: toMs,
	}

	actor := callerAuditActor(req.Caller)
	base := map[string]any{
		"outside_id": row.ID, "claude_session_id": obs.ClaudeID, "project_id": proj.ID, "host_id": hostID,
	}
	adoptIntent := audit.AuditEvent{
		ID: audit.NewAuditID(), TS: time.Now(), Event: audit.AuditEventSessionAdopt, Phase: audit.AuditPhaseIntent,
		Actor: actor, Args: mustJSON(withKeys(base, map[string]any{
			"directory": capAuditText(row.Directory, maxAuditDirectoryRunes), "interrupt": req.Interrupt, "model": model,
		})), Outcome: audit.AuditOutcomePending,
	}
	if err := d.auditor.RecordDurable(adoptIntent); err != nil {
		return nil, refuse(http.StatusServiceUnavailable, adoptAuditOff, "the audit log could not record the adopt")
	}
	start := time.Now()
	completeAdopt := func(outcome, errCode string, extra map[string]any) {
		completion := adoptIntent
		completion.Phase = audit.AuditPhaseCompletion
		completion.DurMs = time.Since(start).Milliseconds()
		completion.Outcome, completion.Error = outcome, errCode
		var args map[string]any
		_ = json.Unmarshal(adoptIntent.Args, &args)
		completion.Args = mustJSON(withKeys(args, withKeys(map[string]any{"interrupted": interrupt}, extra)))
		d.auditor.Record(completion)
	}
	var interruptIntent *audit.AuditEvent
	completeAll := func(outcome, errCode string, extra map[string]any) {
		if interruptIntent != nil {
			completion := *interruptIntent
			completion.Phase = audit.AuditPhaseCompletion
			completion.DurMs = time.Since(start).Milliseconds()
			completion.Outcome, completion.Error = outcome, errCode
			completion.Args = mustJSON(withKeys(base, map[string]any{"interrupted": interrupt}))
			d.auditor.Record(completion)
		}
		completeAdopt(outcome, errCode, extra)
	}
	fail := func(status int, code, message string) (*AdoptResult, *AdoptRefusal) {
		completeAll(audit.AuditOutcomeError, code, nil)
		return nil, refuse(status, code, message)
	}
	if interrupt {
		ev := audit.AuditEvent{
			ID: audit.NewAuditID(), TS: time.Now(), Event: audit.AuditEventSessionInterrupt, Phase: audit.AuditPhaseIntent,
			Actor: actor, Args: mustJSON(base), Outcome: audit.AuditOutcomePending,
		}
		if err := d.auditor.RecordDurable(ev); err != nil {
			return fail(http.StatusServiceUnavailable, adoptAuditOff, "the audit log could not record the interrupt")
		}
		interruptIntent = &ev
	}

	// The dormant record is the session's only trace if the link drops, and the
	// pin a resume checks. commitLaunch writes the live record over it.
	pending := *result.Ledger
	pending.State = ledger.StateDormant
	pending.AdoptPending = &ledger.AdoptPending{ConversationID: obs.ClaudeID, PID: obs.PID, Lifetime: obs.Lifetime}
	if err := d.sessions.Put(pending); err != nil {
		slog.WarnContext(ctx, "session adopt: dormant record not written", "outside_id", row.ID, "error", err)
		return fail(http.StatusBadGateway, adoptUnavailable, "the session could not be recorded; nothing was changed")
	}

	// Deliberate: past the intent rows the work finishes whatever happens to
	// the request ctx, as a local adopt does.
	hold := context.WithoutCancel(ctx)
	_, err = d.launchOnHost(hold, result)
	launched = true // launchOnHost undid what it minted on an error
	if err != nil {
		return a.hostLaunchFailed(ctx, d, result, hostRefusalCode(err), err, h.Name, fail)
	}
	if !d.commitLaunch(hold, result) {
		_ = d.sessions.Remove(result.SessionID)
		d.auditor.Record(newSessionLaunchAuditEvent(result.AuditFields, audit.AuditOutcomeError, "project no longer exists"))
		return fail(http.StatusBadGateway, adoptLaunchFailed, "the process ended but the project is gone; the conversation is intact")
	}
	d.auditor.Record(newSessionLaunchAuditEvent(result.AuditFields, audit.AuditOutcomeOK, ""))
	st.sessionID = result.SessionID
	st.interrupted = interrupt

	recoverySent := false
	if interrupt {
		status, code, _, _ := d.sendOriginText(hold, actor, result.SessionID, adopt.RecoveryText(tail), sessiontypes.OriginRelayRecovery, "")
		recoverySent = status == http.StatusAccepted
		if !recoverySent {
			slog.WarnContext(ctx, "session adopt: recovery message not delivered", "session", result.SessionID, "reason", code)
		}
	}
	completeAll(audit.AuditOutcomeOK, "", map[string]any{"session_id": result.SessionID, "recovery_sent": recoverySent})
	return &AdoptResult{
		SessionID: result.SessionID, ClaudeSessionID: obs.ClaudeID,
		Interrupted: interrupt, RecoverySent: recoverySent,
	}, nil
}

// hostLaunchFailed maps relay-sessions' answer to a failed host adopt launch:
// a refusal the script made leaves the outside process as it was (or gone) and
// the record is removed; a start that failed after the stop, or a result that
// never arrived, keeps the dormant record so Resume can finish it.
func (a *adoptOps) hostLaunchFailed(ctx context.Context, d sessionRouteDeps, result *LaunchResult, code string, err error, hostName string,
	fail func(status int, code, message string) (*AdoptResult, *AdoptRefusal)) (*AdoptResult, *AdoptRefusal) {
	switch code {
	case hostapi.ErrAdoptBusy:
		_ = d.sessions.Remove(result.SessionID)
		return fail(http.StatusConflict, adoptBusy, "the session is not idle; ask to interrupt to take it over anyway")
	case hostapi.ErrAdoptChanged:
		_ = d.sessions.Remove(result.SessionID)
		return fail(http.StatusConflict, adoptChanged, "the process or its registry entry changed")
	case hostapi.ErrAdoptSetupFailed:
		_ = d.sessions.Remove(result.SessionID)
		return fail(http.StatusBadGateway, adoptUnavailable, "the host could not start the adopt; nothing was changed")
	case hostapi.ErrAdoptStopFailed:
		_ = d.sessions.Remove(result.SessionID)
		return fail(http.StatusBadGateway, adoptStopFailed, "the process did not exit after SIGKILL; nothing was launched")
	}
	slog.WarnContext(ctx, "session adopt: host launch did not complete", "session", result.SessionID, "code", code, "error", err)
	d.auditor.Record(newSessionLaunchAuditEvent(result.AuditFields, audit.AuditOutcomeError, err.Error()))
	if code == "" || code == hostapi.ErrAdoptLinkLost {
		return fail(http.StatusBadGateway, adoptHostLinkLost,
			fmt.Sprintf("the connection to %s dropped during the adopt; the session shows as dormant, and Resume continues it", hostName))
	}
	return fail(http.StatusBadGateway, adoptLaunchFailed, "the process ended but relay could not start the session; the conversation is intact and can be resumed")
}

// resumeAdoptPending resumes the dormant record of a host adopt that did not
// settle. It checks what a first adopt checks that can still change (audit, host,
// the managed list, launch authorization), then launches with no_signal: the
// host script starts the agent only if the pinned outside process is gone, and
// answers changed while it still runs. A live run of the session is attached by
// relay-sessions before any script runs.
func (d sessionRouteDeps) resumeAdoptPending(ctx context.Context, ev *logging.Event, caller LaunchCaller, rec ledger.Record) resumeOutcome {
	actor := callerAuditActor(caller)
	pin := rec.AdoptPending
	refuse := func(status int, code, message string) resumeOutcome {
		outcome, reason := refusalOutcome(status, code)
		ev.End(outcome, reason, errors.New(code))
		if code != adoptAuditOff {
			d.auditResume(actor, rec.SessionID, rec.ProjectID, rec.Kind, audit.AuditOutcomeDenied, code)
		}
		return resumeError(status, message, code)
	}
	if !d.auditor.Ready() {
		return refuse(http.StatusServiceUnavailable, adoptAuditOff, "auditing is off; no session can be resumed")
	}
	if !d.ready() || d.sessions == nil {
		return refuse(http.StatusBadGateway, adoptUnavailable, "the session host is not available")
	}
	managed, err := d.host().ManagedClaudeIdentities(ctx)
	if err != nil {
		return refuse(http.StatusBadGateway, adoptUnavailable, "the session host is not available")
	}
	// Deliberate: relay-sessions keeps a dormant host adopt's session saved, and
	// that session holds the conversation itself; only another session's hold
	// refuses.
	if managed[pin.ConversationID] && !d.hostKnowsSession(ctx, rec.SessionID) {
		return refuse(http.StatusConflict, adoptManaged, "relay already holds this conversation")
	}

	sessionReq, err := decodeStoredSessionRequest(rec)
	if err != nil {
		return refuse(http.StatusInternalServerError, "corrupt_record", "corrupt session record")
	}
	result, lr := AuthorizeLaunch(d.store, d.modelKeys, d.sessions, LaunchRequest{
		Caller: caller, SessionID: rec.SessionID, Resume: true,
		ProjectID: rec.ProjectID, Kind: rec.Kind, Directory: rec.Directory,
		Name: sessionReq.Name, Model: sessionReq.Model, ClientSettings: sessionReq.Settings,
		SystemPrompt: sessionReq.SystemPrompt, AppendClaudeMd: sessionReq.AppendClaudeMd,
		Entry: rec.Entry, offer: d.offerFor(ctx),
	})
	if lr != nil {
		d.auditor.Record(resumeAuditEvent(lr.Audit))
		endEvent(ev, lr)
		return resumeError(lr.Status, lr.Message, lr.Code)
	}
	result.Spec.ResumeConversation = pin.ConversationID
	result.Spec.AdoptHost = &hostapi.AdoptHostSpec{PID: pin.PID, Lifetime: pin.Lifetime, ChildTest: "none", NoSignal: true}

	// Deliberate: the host script finishes whatever happens to the request ctx.
	hold := context.WithoutCancel(ctx)
	if _, err := d.launchOnHost(hold, result); err != nil {
		code := hostRefusalCode(err)
		d.auditor.Record(resumeAuditEvent(newSessionLaunchAuditEvent(result.AuditFields, audit.AuditOutcomeError, err.Error())))
		if code == hostapi.ErrAdoptChanged {
			_ = d.sessions.Remove(rec.SessionID)
			d.deleteHostSession(hold, rec.SessionID)
			ev.End(logging.OutcomeDenied, adoptChanged, errors.New(adoptChanged))
			return resumeError(http.StatusConflict, "the terminal session still runs; adopt it again", adoptChanged)
		}
		slog.Warn("session resume: host adopt launch failed", "session", rec.SessionID, "code", code, "error", err)
		return resumeError(http.StatusBadGateway, "launch failed", "launch_failed")
	}
	if !d.commitLaunch(hold, result) {
		d.auditor.Record(resumeAuditEvent(newSessionLaunchAuditEvent(result.AuditFields, audit.AuditOutcomeError, "project no longer exists")))
		return resumeError(http.StatusBadGateway, "launch failed", "launch_failed")
	}
	d.auditor.Record(resumeAuditEvent(newSessionLaunchAuditEvent(result.AuditFields, audit.AuditOutcomeOK, "")))
	return resumeOutcome{Status: http.StatusOK, Body: resumeResponseBody{SessionID: result.SessionID, Resumed: true}}
}

// hostKnowsSession reports whether relay-sessions lists the session. An answer
// it cannot read is "no": the managed check then applies.
func (d sessionRouteDeps) hostKnowsSession(ctx context.Context, id string) bool {
	resp, err := d.host().do(ctx, http.MethodGet, "/api/sessions", nil)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, maxSessionHostResponseBytes)).Decode(&body) != nil {
		return false
	}
	for _, s := range body.Sessions {
		if s.ID == id {
			return true
		}
	}
	return false
}

// deleteHostSession removes relay-sessions' saved copy of a session whose
// record relay dropped, so the outside row is adoptable again.
func (d sessionRouteDeps) deleteHostSession(ctx context.Context, id string) {
	resp, err := d.host().do(ctx, http.MethodDelete, "/api/sessions/"+id, nil)
	if err != nil {
		slog.Warn("session resume: saved session not deleted", "session", id, "error", err)
		return
	}
	_ = resp.Body.Close()
}
