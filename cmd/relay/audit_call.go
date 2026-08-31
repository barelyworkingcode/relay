package main

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

// Every method on auditCall tolerates a nil receiver, so the router
// instrumentation reads as straight-line code with no `if r.audit != nil`
// noise at each step — a router built without a recorder simply does nothing.
type auditCall struct {
	rec   *AuditRecorder
	start time.Time
	ev    AuditEvent

	// remote marks a caller that arrived over the remote listener, which is the
	// only thing that switches this event from ADR-008's one fail-open record
	// to ADR-010's fail-closed intent + completion pair.
	remote bool

	// intentWritten records that the pre-call half is already on disk, so the
	// final record is labelled as its completion rather than as a standalone
	// event.
	intentWritten bool
}

// Returns nil when auditing is off or this event kind isn't being recorded,
// so no work is done building an event that would be thrown away.
func (r *appRouter) beginAudit(ctx context.Context, event string) *auditCall {
	if !r.audit.Enabled() {
		return nil
	}
	if event != AuditEventCallTool && !r.audit.LogLists() {
		return nil
	}
	a := &auditCall{
		rec:   r.audit,
		start: time.Now(),
		ev: AuditEvent{
			ID:    newAuditID(),
			Event: event,
			Actor: AuditActor{Kind: AuditActorUnknown, Auth: AuditAuthNone},
		},
	}
	// A remote caller's identity is attested by its certificate, the network
	// equivalent of the peer pid and strictly stronger (a pid is reusable and
	// racy, a fingerprint is neither); this is an either/or rather than two
	// independent lookups, which is what guarantees PID/Proc/Parent stay
	// *absent* for a remote call instead of filled with a locally-meaningless
	// number.
	if rc, ok := bridge.RemoteCallerFromContext(ctx); ok {
		a.remote = true
		a.ev.Actor.Kind = AuditActorRemote
		a.ev.Actor.Auth = AuditAuthMTLS
		a.ev.Actor.ClientID = rc.ClientID
		a.ev.Actor.Fingerprint = rc.Fingerprint
		a.ev.Actor.RemoteAddr = rc.RemoteAddr
		return a
	}
	// Resolve now, while the caller is certainly still alive: a `relay mcp
	// call` child exits as soon as it has its answer, so resolving after the
	// call would frequently find nothing.
	if pid := bridge.CallerPIDFromContext(ctx); pid > 0 {
		a.ev.Actor.PID = pid
		a.ev.Actor.Proc, a.ev.Actor.Parent = ProcessNames(pid)
	}
	return a
}

// Arguments are redacted and capped here rather than at write time so the raw
// values never sit in the queue waiting to be persisted.
func (a *auditCall) setTool(name string, args json.RawMessage) {
	if a == nil {
		return
	}
	a.ev.Tool = name
	if !a.rec.cfg.LogArgs {
		return
	}
	a.ev.Args, a.ev.ArgsBytes, a.ev.ArgsTruncated = redactArgs(args, a.rec.cfg.MaxArgBytes, a.rec.cfg.RedactKeys)
}

// Known only after tool-owner lookup, which is why it's separate from setTool.
func (a *auditCall) setMcp(id string) {
	if a == nil {
		return
	}
	a.ev.McpID = id
}

// token is the credential the caller presented, used only to distinguish a
// token hand-off from directory auth — the value itself is never recorded.
func (a *auditCall) setActor(ctx context.Context, stored *StoredToken, settings *Settings, token string) {
	if a == nil || stored == nil {
		return
	}
	// A remote caller's kind and auth come from the connection, not from what
	// auth resolution found, so they are left alone here — but the project
	// fields below are still filled in: the caller is remote *and* acting as
	// a project grant.
	if a.remote {
		a.setProject(stored, settings)
		return
	}
	switch {
	case stored.Name == serviceTokenName:
		a.ev.Actor.Kind = AuditActorService
		a.ev.Actor.Auth = AuditAuthService
	case token == "":
		a.ev.Actor.Kind = AuditActorProject
		a.ev.Actor.Auth = AuditAuthCwd
		a.ev.Actor.Cwd = bridge.CallerCwdFromContext(ctx)
	default:
		a.ev.Actor.Kind = AuditActorProject
		a.ev.Actor.Auth = AuditAuthToken
	}
	a.setProject(stored, settings)
}

// Shared by every actor kind: whichever way a caller was identified, the
// project it is acting as comes from relay's own auth resolution.
func (a *auditCall) setProject(stored *StoredToken, settings *Settings) {
	a.ev.Actor.ProjectID = stored.ProjectID
	a.ev.Actor.ProjectName = projectNameFor(stored, settings)
}

func (a *auditCall) setUnauthenticated(ctx context.Context, token string) {
	if a == nil {
		return
	}
	// A remote caller that failed to resolve a grant is still a known
	// certificate on a known connection: downgrading it to "unknown" would
	// discard the only attribution the record has.
	if a.remote {
		return
	}
	a.ev.Actor.Kind = AuditActorUnknown
	if token == "" {
		a.ev.Actor.Auth = AuditAuthNone
		a.ev.Actor.Cwd = bridge.CallerCwdFromContext(ctx)
	} else {
		a.ev.Actor.Auth = AuditAuthToken
	}
}

// Called at the point CallTool assembles _meta, which is before intent()
// writes the pre-call record, so a remote call's intent line carries the
// authority it is about to run with rather than only the completion doing so.
//
// allowExternal is taken by value and stored by pointer: every caller of this
// function knows the answer, so the nil the field can hold means "nobody
// recorded an authority for this event" and can never be produced here.
func (a *auditCall) setAuthority(access string, allowExternal bool, scope map[string]json.RawMessage) {
	if a == nil {
		return
	}
	a.ev.Access = access
	a.ev.AllowExternal = &allowExternal
	a.ev.Scope = scope
}

// A no-op when root is empty, so it is always safe to call with whatever
// McpSurfaceFor returned.
func (a *auditCall) setMcpRoot(root string) {
	if a == nil || root == "" {
		return
	}
	a.ev.McpRoot = root
}

// Records the fields this grant set a value for that the MCP's live schema
// does not declare. Called immediately before the refusal they cause, so the
// record that says the call was denied is also the record that says why the
// grant could not be applied.
func (a *auditCall) setUnplacedScope(fields []string) {
	if a == nil {
		return
	}
	a.ev.ScopeUnplaced = fields
}

func (a *auditCall) setToolCount(n int) {
	if a == nil {
		return
	}
	a.ev.ToolCount = n
}

// Blocks until the pre-call record is on disk, returning an error when it
// could not be written; the router turns that error into a refusal, and the
// MCP is never invoked.
//
// The ordering is the whole point (ADR-010 decision 5). An event written
// after the call — which is what every local call still does, because
// doneResult needs the result bytes — makes "refuse a call that cannot be
// logged" mean nothing: by the time the write fails the data has already left
// the MCP.
//
// An intent with no matching completion is a signal worth alerting on, not
// noise to reconcile away: it means relay invoked an MCP and never learned
// the outcome — a crash, a kill, or a hang.
//
// A local caller returns nil immediately, and so does a nil *auditCall (the
// case when auditing is off): an operator who turns the audit log off has
// turned off the thing the refusal was protecting, and the Tool Calls tab
// says so rather than pretending otherwise.
func (a *auditCall) intent() error {
	if a == nil || !a.remote {
		return nil
	}
	ev := a.ev
	ev.Phase = AuditPhaseIntent
	ev.TS = a.start.UTC()
	ev.Outcome = AuditOutcomePending
	if err := a.rec.RecordDurable(ev); err != nil {
		return err
	}
	a.intentWritten = true
	return nil
}

func (a *auditCall) done(outcome string, err error) {
	if a == nil {
		return
	}
	if a.intentWritten {
		// Same id as the intent: that pairing is what makes the two lines one
		// call rather than two events that happen to look alike.
		a.ev.Phase = AuditPhaseCompletion
	}
	a.ev.DurMs = time.Since(a.start).Milliseconds()
	a.ev.TS = a.start.UTC()
	a.ev.Outcome = outcome
	if err != nil {
		a.ev.Error = err.Error()
	}
	a.rec.Record(a.ev)
}

// A preview is recorded only when the operator opted into one.
func (a *auditCall) doneResult(result json.RawMessage, err error) {
	if a == nil {
		return
	}
	if err != nil {
		a.done(AuditOutcomeError, err)
		return
	}
	a.ev.ResultBytes = len(result)
	a.ev.ResultIsError = resultIsError(result)
	a.ev.ScopeViolation = a.ev.ResultIsError && resultIsScopeViolation(result)
	if n := a.rec.cfg.MaxResultPreviewBytes; n > 0 && len(result) > 0 {
		a.ev.ResultPreview = truncateRunes(string(result), n)
	}
	// A tool that refuses in-protocol returns a normal result with isError
	// set: not AuditOutcomeError, but not a success either — recording it as
	// "ok" would hide every application-level refusal (an fsMCP read outside
	// allowed_dirs, say) from `relay audit --outcome ...`.
	if a.ev.ResultIsError {
		a.done(AuditOutcomeToolError, nil)
		return
	}
	a.done(AuditOutcomeOK, nil)
}

func resultIsError(result json.RawMessage) bool {
	if len(result) == 0 {
		return false
	}
	var probe struct {
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(result, &probe); err != nil {
		return false
	}
	return probe.IsError
}

// The wire shape, which is the whole of the contract:
//
//	{"content": [...], "isError": true,
//	 "_meta": {"scope_violation": true}}
//
// Honoured ONLY when isError is true — a successful call cannot claim to have
// refused anything — and only when the value is boolean true; anything else
// (absent, a string, a malformed blob) leaves the flag off and degrades to
// "an ordinary tool_error".
//
// It is deliberately a marker relay TRUSTS rather than a message relay
// parses: distinguishing a scope refusal from any other isError by reading
// the error text would be the ADR-006 line — domain knowledge inside relay.
// The flag is what the MCP said about itself, and it changes no outcome and
// gates no decision; it exists so alerting has something to select on.
const scopeViolationMarker = "scope_violation"

// The namespaced spelling MCP convention uses for _meta keys. Both are
// accepted because the cost of accepting two is nil — the flag gates
// nothing — and the cost of accepting the wrong one is a signal that
// silently never appears.
const scopeViolationMarkerNamespaced = "relay/scope_violation"

func resultIsScopeViolation(result json.RawMessage) bool {
	if len(result) == 0 {
		return false
	}
	var probe struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(result, &probe); err != nil {
		return false
	}
	raw, ok := probe.Meta[scopeViolationMarker]
	if !ok {
		if raw, ok = probe.Meta[scopeViolationMarkerNamespaced]; !ok {
			return false
		}
	}
	var flag bool
	if err := json.Unmarshal(raw, &flag); err != nil {
		return false
	}
	return flag
}

// Prefers the live settings lookup; falls back to the "project:<name>" form
// already carried on the StoredToken when the project has since been deleted.
func projectNameFor(stored *StoredToken, settings *Settings) string {
	if settings != nil && stored.ProjectID != "" {
		if proj, _ := settings.findProjectByID(stored.ProjectID); proj != nil {
			return proj.Name
		}
	}
	return strings.TrimPrefix(stored.Name, "project:")
}

// mcpSupervisionEvent builds the audit record for one external-MCP liveness
// transition (ADR-012). It is the only audit event relay writes about itself
// rather than about a caller, so the actor is `relay` and every
// caller-derived field is left absent rather than zero-filled.
//
// A failed individual restart attempt produces NO record: it is a step inside
// an outage the mcp_down row already opened, and one line per retry would
// bury the two lines that bound it.
func mcpSupervisionEvent(ev McpHealthEvent) (AuditEvent, bool) {
	out := AuditEvent{
		ID:          newAuditID(),
		TS:          time.Now(),
		McpID:       ev.ID,
		Supervision: ev.State,
		Actor: AuditActor{
			Kind: AuditActorRelay,
			Auth: AuditAuthNone,
			// Named so the CALLER column says who wrote the row rather than a
			// dash, which on every other line means "could not attribute".
			Proc: "relay",
		},
	}
	if ev.Err != nil {
		out.Error = ev.Err.Error()
	}
	// 0 on the mcp_down row because the outage duration is not yet known.
	out.DurMs = ev.Downtime.Milliseconds()

	switch ev.State {
	case McpHealthDown:
		out.Event, out.Outcome = AuditEventMcpDown, AuditOutcomeError
	case McpHealthRestarted:
		out.Event, out.Outcome = AuditEventMcpUp, AuditOutcomeOK
	case McpHealthAbandoned:
		out.Event, out.Outcome = AuditEventMcpDown, AuditOutcomeError
	default:
		return AuditEvent{}, false
	}
	return out, true
}

func (r *AuditRecorder) RecordMcpSupervision(ev McpHealthEvent) {
	if !r.Enabled() {
		return
	}
	if out, ok := mcpSupervisionEvent(ev); ok {
		r.Record(out)
	}
}
