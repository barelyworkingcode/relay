package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/outsidesessions"
	"github.com/barelyworkingcode/relay/internal/sessions/adopt"
	sessiontypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// maxAdoptBodyBytes bounds an adopt request: two ids, a flag and a model name.
const maxAdoptBodyBytes = 4 << 10

const maxAdoptModelChars = 200

// consoleMachineKey is the key of the machine relay runs on in the outside
// inventory; an SSH host's key is its host id.
const consoleMachineKey = "console"

// Codes the adopt door adds to a reason code (adopt.Reason*), AuthorizeLaunch's
// own refusals and the audit-unavailable code.
const (
	adoptOutsideNotFound = "outside_session_not_found"
	adoptInvalidBody     = "invalid_body"
	adoptBodyTooLarge    = "body_too_large"
	adoptManaged         = "managed"
	adoptBusy            = "busy"
	adoptModelUnknown    = "model_unknown"
	adoptInProgress      = "adopt_in_progress"
	adoptChanged         = "changed"
	adoptUnavailable     = "unavailable"
	adoptStopFailed      = "stop_failed"
	adoptLaunchFailed    = "launch_failed"
	adoptAuditOff        = "audit_unavailable"
)

var outsideIDPattern = regexp.MustCompile(`^outside:[0-9a-f]{32}$`)

// adoptBody is the whole of what the door reads. Exactly one selector is set.
type adoptBody struct {
	OutsideID       string `json:"outsideId"`
	ClaudeSessionID string `json:"claudeSessionId"`
	Interrupt       bool   `json:"interrupt"`
	Model           string `json:"model"`
}

type adoptResponse struct {
	SessionID       string `json:"sessionId"`
	ClaudeSessionID string `json:"claudeSessionId"`
	Interrupted     bool   `json:"interrupted"`
	RecoverySent    bool   `json:"recoverySent"`
}

// AdoptRequest is what the door hands the core. Caller is the resolved
// credential; nothing in the body names a credential, a path or an origin.
type AdoptRequest struct {
	Caller          LaunchCaller
	OutsideID       string
	ClaudeSessionID string
	Interrupt       bool
	Model           string
}

type AdoptResult struct {
	SessionID       string
	ClaudeSessionID string
	Interrupted     bool
	RecoverySent    bool
}

// AdoptRefusal is an adopt the core did not carry out. Status is the HTTP
// status the door answers with.
type AdoptRefusal struct {
	Status        int
	Code, Message string
}

func (r *AdoptRefusal) Error() string { return r.Message }

// adoptOps is the one core behind the adopt door: it ends an outside Claude
// process a person started in a terminal and resumes its conversation as a
// headless relay session, under the project's sandbox and policy.
type adoptOps struct {
	deps    sessionRouteDeps
	outside *outsidesessions.Inventory
	claude  adopt.Adopter
	clock   serverClock
	// inflight holds the conversation ids with an adopt running; the value is
	// unused.
	inflight sync.Map
	// console is the machine a claudeSessionId selector probes once when the
	// inventory has not seen the process yet.
	console outsidesessions.Machine
}

func newAdoptOps(deps sessionRouteDeps, outside *outsidesessions.Inventory, clock serverClock) *adoptOps {
	if clock == nil {
		clock = wallClock{}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// An empty home makes every Discover fail closed.
		home = ""
	}
	return &adoptOps{
		deps: deps, outside: outside, clock: clock,
		claude:  adopt.NewClaude(adopt.ClaudeConfig{Home: home, Clock: clock}),
		console: outsideMachine(deps.store, deps, nil, consoleOutsideSpool()),
	}
}

// handleAdopt implements POST /api/sessions/adopt.
func (a *adoptOps) handleAdopt(w http.ResponseWriter, r *http.Request) {
	parseRefusal := func(status int, code, message string) {
		logging.BeginEvent(r.Context(), "session.adopt").End(logging.OutcomeDenied, code, errors.New(code))
		writeChiefOfStaffError(w, status, code, message)
	}
	body, refusal := decodeAdoptBody(w, r)
	if refusal != nil {
		parseRefusal(refusal.Status, refusal.Code, refusal.Message)
		return
	}
	res, refusal := a.adoptOutside(r.Context(), AdoptRequest{
		Caller:    resolveLaunchCaller(r, a.deps.store),
		OutsideID: body.OutsideID, ClaudeSessionID: body.ClaudeSessionID,
		Interrupt: body.Interrupt, Model: body.Model,
	})
	if refusal != nil {
		writeChiefOfStaffError(w, refusal.Status, refusal.Code, refusal.Message)
		return
	}
	writeJSON(w, http.StatusCreated, adoptResponse{
		SessionID: res.SessionID, ClaudeSessionID: res.ClaudeSessionID,
		Interrupted: res.Interrupted, RecoverySent: res.RecoverySent,
	})
}

func decodeAdoptBody(w http.ResponseWriter, r *http.Request) (adoptBody, *AdoptRefusal) {
	var body adoptBody
	invalid := func(message string) (adoptBody, *AdoptRefusal) {
		return body, &AdoptRefusal{Status: http.StatusBadRequest, Code: adoptInvalidBody, Message: message}
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAdoptBodyBytes))
	dec.DisallowUnknownFields()
	err := dec.Decode(&body)
	if err == nil {
		// One JSON value and nothing after it.
		if _, extra := dec.Token(); extra != io.EOF {
			err = errors.New("trailing data")
			if extra != nil {
				var tooBig *http.MaxBytesError
				if errors.As(extra, &tooBig) {
					err = extra
				}
			}
		}
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return body, &AdoptRefusal{Status: http.StatusRequestEntityTooLarge, Code: adoptBodyTooLarge, Message: "request body is larger than 4 KiB"}
		}
		return invalid(`body must be JSON {"outsideId" or "claudeSessionId", "interrupt", "model"}`)
	}
	switch {
	case (body.OutsideID == "") == (body.ClaudeSessionID == ""):
		return invalid("send outsideId or claudeSessionId, not both and not neither")
	case body.OutsideID != "" && !outsideIDPattern.MatchString(body.OutsideID):
		return invalid("outsideId is not an outside session id")
	case body.ClaudeSessionID != "" && !outsidesessions.IsConversationID(body.ClaudeSessionID):
		return invalid("claudeSessionId is not a UUID")
	case !validAdoptModel(body.Model):
		return invalid("model is not a model name")
	}
	return body, nil
}

func validAdoptModel(model string) bool {
	if utf8.RuneCountInString(model) > maxAdoptModelChars || !utf8.ValidString(model) {
		return false
	}
	return !strings.ContainsFunc(model, unicode.IsControl)
}

// adoptState is what the event line reports; fields fill in as the core learns
// them.
type adoptState struct {
	outsideID, claudeID, projectID, sessionID string
	interrupted                               bool
}

func (s *adoptState) endEvent(ev *logging.Event, refusal *AdoptRefusal) {
	ev.Set("outside_id", s.outsideID).Set("claude_session_id", s.claudeID).
		Set("project_id", s.projectID).Set("session_id", s.sessionID).Set("interrupted", s.interrupted)
	if refusal == nil {
		ev.End(logging.OutcomeOK, "", nil)
		return
	}
	outcome, reason := refusalOutcome(refusal.Status, refusal.Code)
	ev.End(outcome, reason, errors.New(refusal.Code))
}

// adoptOutside judges, audits, stops and resumes, in this order: audit ready,
// the row, a fresh match of it against the agent's registry, the project, the
// managed list, the idle check, the model, launch authorization, the adopt
// intent, the interrupt (when the session is busy), the stop, the launch and
// the recovery message. Nothing is signalled before AuthorizeLaunch passed and
// the intent rows are durable, so a refusal up to there leaves the process as
// it was.
func (a *adoptOps) adoptOutside(ctx context.Context, req AdoptRequest) (res *AdoptResult, refusal *AdoptRefusal) {
	d := a.deps
	ev := logging.BeginEvent(ctx, "session.adopt")
	st := &adoptState{outsideID: req.OutsideID, claudeID: req.ClaudeSessionID}
	defer func() { st.endEvent(ev, refusal) }()

	refuse := func(status int, code, message string) *AdoptRefusal {
		return &AdoptRefusal{Status: status, Code: code, Message: message}
	}
	if !d.auditor.Ready() {
		return nil, refuse(http.StatusServiceUnavailable, adoptAuditOff, "auditing is off; no session can be adopted")
	}
	if !d.ready() || d.sessions == nil {
		return nil, refuse(http.StatusBadGateway, adoptUnavailable, "the session host is not available")
	}

	obs, refusal := a.resolveRow(ctx, req)
	if refusal != nil {
		return nil, refusal
	}
	row := obs.Session
	st.outsideID, st.claudeID = row.ID, obs.ClaudeID
	if _, taken := a.inflight.LoadOrStore(obs.ClaudeID, struct{}{}); taken {
		return nil, refuse(http.StatusConflict, adoptInProgress, "an adopt of this conversation is already running")
	}
	defer a.inflight.Delete(obs.ClaudeID)

	cand, refusal := a.freshCandidate(ctx, obs)
	if refusal != nil {
		return nil, refusal
	}

	var proj *config.Project
	if row.ProjectID != nil {
		proj, _ = config.FindProjectByID(config.FreshSettings(d.store), *row.ProjectID)
	}
	if proj == nil || proj.IsRemote() || proj.IsHosted() {
		return nil, refuse(http.StatusConflict, adopt.ReasonNoProject, "the session is not in a registered local project")
	}
	st.projectID = proj.ID

	managed, err := d.host().ManagedClaudeIdentities(ctx)
	if err != nil {
		return nil, refuse(http.StatusBadGateway, adoptUnavailable, "the session host is not available")
	}
	if managed[obs.ClaudeID] {
		return nil, refuse(http.StatusConflict, adoptManaged, "relay already holds this conversation")
	}

	ready, err := a.claude.Check(ctx, cand)
	switch {
	case errors.Is(err, adopt.ErrChanged):
		return nil, refuse(http.StatusConflict, adoptChanged, "the process or its registry entry changed")
	case err != nil:
		slog.WarnContext(ctx, "session adopt: idle check failed", "outside_id", row.ID, "error", err)
		return nil, refuse(http.StatusBadGateway, adoptUnavailable, "the session could not be inspected")
	}
	busy := !ready.Idle
	if busy && !req.Interrupt {
		return nil, refuse(http.StatusConflict, adoptBusy,
			fmt.Sprintf("the session is not idle (%s); ask to interrupt to take it over anyway", ready.Veto))
	}

	model := req.Model
	if model == "" {
		model = ready.Tail.ModelFamily
	}
	if model == "" {
		return nil, refuse(http.StatusConflict, adoptModelUnknown, "the model of the session is unknown; send model")
	}

	result, lr := AuthorizeLaunch(d.store, d.modelKeys, d.sessions, LaunchRequest{
		Caller: req.Caller, ProjectID: proj.ID, Kind: KindClaude, Directory: row.Directory,
		Name: row.Name, Model: model,
		ClientSettings: json.RawMessage(`{"headless":true,"agent":true}`),
	})
	if lr != nil {
		d.auditor.Record(lr.Audit)
		return nil, refuse(lr.Status, lr.Code, lr.Message)
	}
	result.Spec.ResumeConversation = obs.ClaudeID
	launched := false
	defer func() {
		if !launched {
			d.discardLaunch(result)
		}
	}()

	actor := callerAuditActor(req.Caller)
	base := map[string]any{
		"outside_id": row.ID, "claude_session_id": obs.ClaudeID, "project_id": proj.ID,
	}
	adoptArgs := func(extra map[string]any) json.RawMessage {
		args := map[string]any{}
		for k, v := range base {
			args[k] = v
		}
		args["directory"] = capAuditText(row.Directory, maxAuditDirectoryRunes)
		args["interrupt"] = req.Interrupt
		args["model"] = model
		for k, v := range extra {
			args[k] = v
		}
		raw, _ := json.Marshal(args)
		return raw
	}
	adoptIntent := audit.AuditEvent{
		ID: audit.NewAuditID(), TS: time.Now(), Event: audit.AuditEventSessionAdopt, Phase: audit.AuditPhaseIntent,
		Actor: actor, Args: adoptArgs(nil), Outcome: audit.AuditOutcomePending,
	}
	if err := d.auditor.RecordDurable(adoptIntent); err != nil {
		return nil, refuse(http.StatusServiceUnavailable, adoptAuditOff, "the audit log could not record the adopt")
	}
	start := time.Now()
	var signalsSent []adopt.Signal
	complete := func(outcome, errCode string, extra map[string]any) {
		completion := adoptIntent
		completion.Phase = audit.AuditPhaseCompletion
		completion.DurMs = time.Since(start).Milliseconds()
		completion.Outcome, completion.Error = outcome, errCode
		args := map[string]any{"interrupted": st.interrupted, "signals": signalRows(signalsSent)}
		for k, v := range extra {
			args[k] = v
		}
		completion.Args = adoptArgs(args)
		d.auditor.Record(completion)
	}
	fail := func(status int, code, message string, extra map[string]any) (*AdoptResult, *AdoptRefusal) {
		complete(audit.AuditOutcomeError, code, extra)
		return nil, refuse(status, code, message)
	}

	// Deliberate: past the intent rows the work finishes whatever happens to
	// the request ctx. A client that leaves must not strand a half-stopped
	// process with no session to hold its conversation.
	hold := context.WithoutCancel(ctx)

	if busy {
		interruptIntent := audit.AuditEvent{
			ID: audit.NewAuditID(), TS: time.Now(), Event: audit.AuditEventSessionInterrupt, Phase: audit.AuditPhaseIntent,
			Actor: actor, Args: mustJSON(base), Outcome: audit.AuditOutcomePending,
		}
		if err := d.auditor.RecordDurable(interruptIntent); err != nil {
			return fail(http.StatusServiceUnavailable, adoptAuditOff, "the audit log could not record the interrupt", nil)
		}
		interruptStart := time.Now()
		sent, err := a.claude.Interrupt(hold, cand)
		signalsSent = append(signalsSent, sent...)
		st.interrupted = len(sent) > 0
		completion := interruptIntent
		completion.Phase = audit.AuditPhaseCompletion
		completion.DurMs = time.Since(interruptStart).Milliseconds()
		completion.Outcome = audit.AuditOutcomeOK
		if err != nil {
			completion.Outcome, completion.Error = audit.AuditOutcomeError, adoptCodeFor(err)
		}
		completion.Args = mustJSON(withKeys(base, map[string]any{"signals": signalRows(sent), "exited": !a.claude.Alive(cand)}))
		d.auditor.Record(completion)
		if err != nil {
			status, code, message := adoptStopRefusal(err)
			return fail(status, code, message, nil)
		}
	}

	spec, err := a.claude.TakeOver(hold, cand)
	signalsSent = append(signalsSent, spec.Signals...)
	st.interrupted = st.interrupted || hasSignal(signalsSent, "INT")
	if err != nil {
		status, code, message := adoptStopRefusal(err)
		return fail(status, code, message, nil)
	}

	if _, err := d.launchOnHost(hold, result); err != nil {
		slog.WarnContext(ctx, "session adopt: launch failed after the process ended", "outside_id", row.ID, "error", err)
		launched = true // launchOnHost undid what it minted
		d.auditor.Record(newSessionLaunchAuditEvent(result.AuditFields, audit.AuditOutcomeError, err.Error()))
		return fail(http.StatusBadGateway, adoptLaunchFailed, "the process ended but relay could not start the session; the conversation is intact and can be resumed", nil)
	}
	launched = true
	if !d.commitLaunch(hold, result) {
		d.auditor.Record(newSessionLaunchAuditEvent(result.AuditFields, audit.AuditOutcomeError, "project no longer exists"))
		return fail(http.StatusBadGateway, adoptLaunchFailed, "the process ended but the project is gone; the conversation is intact", nil)
	}
	d.auditor.Record(newSessionLaunchAuditEvent(result.AuditFields, audit.AuditOutcomeOK, ""))
	st.sessionID = result.SessionID

	recoverySent := false
	if hasSignal(signalsSent, "INT") {
		status, code, _, _ := d.sendOriginText(hold, actor, result.SessionID, adopt.RecoveryText(ready.Tail), sessiontypes.OriginRelayRecovery, "")
		recoverySent = status == http.StatusAccepted
		if !recoverySent {
			slog.WarnContext(ctx, "session adopt: recovery message not delivered", "session", result.SessionID, "reason", code)
		}
	}
	complete(audit.AuditOutcomeOK, "", map[string]any{"session_id": result.SessionID, "recovery_sent": recoverySent})
	return &AdoptResult{
		SessionID: result.SessionID, ClaudeSessionID: obs.ClaudeID,
		Interrupted: st.interrupted, RecoverySent: recoverySent,
	}, nil
}

// resolveRow finds the live console row the request names. A selector that
// matches an ended row answers `ended`; one that matches nothing answers 404.
func (a *adoptOps) resolveRow(ctx context.Context, req AdoptRequest) (outsidesessions.Observation, *AdoptRefusal) {
	notFound := &AdoptRefusal{Status: http.StatusNotFound, Code: adoptOutsideNotFound, Message: "no live outside session matches"}
	ended := &AdoptRefusal{Status: http.StatusConflict, Code: adopt.ReasonEnded, Message: "the session has ended; there is no process to adopt"}

	if req.OutsideID != "" {
		obs, key, ok := a.outside.Live(req.OutsideID)
		if !ok {
			if a.snapshotHas(func(s outsidesessions.Session) bool { return s.ID == req.OutsideID && s.State == "ended" }) {
				return obs, ended
			}
			return obs, notFound
		}
		return obs, adoptableRow(obs, key)
	}

	rows := a.outside.LiveByClaudeID(consoleMachineKey, req.ClaudeSessionID)
	if len(rows) == 0 {
		a.outside.Probe(ctx, a.console)
		rows = a.outside.LiveByClaudeID(consoleMachineKey, req.ClaudeSessionID)
	}
	switch len(rows) {
	case 0:
		if a.snapshotHas(func(s outsidesessions.Session) bool {
			return s.ClaudeSessionID == req.ClaudeSessionID && s.State == "ended"
		}) {
			return outsidesessions.Observation{}, ended
		}
		return outsidesessions.Observation{}, notFound
	case 1:
		return rows[0], adoptableRow(rows[0], consoleMachineKey)
	}
	return rows[0], &AdoptRefusal{Status: http.StatusConflict, Code: adopt.ReasonAmbiguous, Message: "more than one live process holds this conversation"}
}

func (a *adoptOps) snapshotHas(match func(outsidesessions.Session) bool) bool {
	for _, s := range a.outside.Snapshot().Sessions {
		if match(s) {
			return true
		}
	}
	return false
}

// adoptableRow refuses a row the inventory marks not adoptable, with the
// reason the row carries, and a row of any machine but the console.
func adoptableRow(obs outsidesessions.Observation, machineKey string) *AdoptRefusal {
	switch {
	case machineKey != consoleMachineKey:
		return &AdoptRefusal{Status: http.StatusConflict, Code: adopt.ReasonHost, Message: "sessions on an SSH host cannot be adopted"}
	case !obs.Session.Adoptable:
		return &AdoptRefusal{Status: http.StatusConflict, Code: obs.Session.AdoptReason, Message: "the session cannot be adopted: " + obs.Session.AdoptReason}
	}
	return nil
}

// freshCandidate reads the agent's registry again and matches the row's
// process by pid, conversation id and kernel start time. The inventory's row
// is up to one probe old; the process it names may not be the same one.
func (a *adoptOps) freshCandidate(ctx context.Context, obs outsidesessions.Observation) (adopt.Candidate, *AdoptRefusal) {
	cands, err := a.claude.Discover(ctx)
	if err != nil {
		slog.WarnContext(ctx, "session adopt: registry read failed", "outside_id", obs.Session.ID, "error", err)
		return adopt.Candidate{}, &AdoptRefusal{Status: http.StatusBadGateway, Code: adoptUnavailable, Message: "the agent registry could not be read"}
	}
	for _, c := range cands {
		if c.PID != obs.PID || c.ConversationID != obs.ClaudeID {
			continue
		}
		if c.Cwd != obs.Session.Directory || fmt.Sprintf("%d.%06d", c.ProcStart.Unix(), c.ProcStart.Nanosecond()/1000) != obs.Lifetime {
			break
		}
		if !c.Adoptable {
			return c, &AdoptRefusal{Status: http.StatusConflict, Code: c.Reason, Message: "the session cannot be adopted: " + c.Reason}
		}
		return c, nil
	}
	return adopt.Candidate{}, &AdoptRefusal{Status: http.StatusConflict, Code: adoptChanged, Message: "the process or its registry entry changed"}
}

// adoptStopRefusal maps a ladder error to the door's refusal.
func adoptStopRefusal(err error) (int, string, string) {
	switch {
	case errors.Is(err, adopt.ErrChanged):
		return http.StatusConflict, adoptChanged, "the process or its registry entry changed"
	case errors.Is(err, adopt.ErrStopFailed):
		return http.StatusBadGateway, adoptStopFailed, "the process did not exit after SIGKILL; nothing was launched"
	}
	return http.StatusBadGateway, adoptUnavailable, "the process could not be stopped"
}

func adoptCodeFor(err error) string {
	_, code, _ := adoptStopRefusal(err)
	return code
}

func hasSignal(sent []adopt.Signal, name string) bool {
	for _, s := range sent {
		if s.Name == name {
			return true
		}
	}
	return false
}

func signalRows(sent []adopt.Signal) []map[string]string {
	rows := make([]map[string]string, 0, len(sent))
	for _, s := range sent {
		rows = append(rows, map[string]string{"signal": s.Name, "at": s.At.UTC().Format(time.RFC3339Nano)})
	}
	return rows
}

func withKeys(base, extra map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func mustJSON(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}
