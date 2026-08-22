package main

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"relaygo/bridge"
)

// auditCall accumulates one audit event over the life of a router call.
//
// Every method tolerates a nil receiver, so the router instrumentation reads as
// straight-line code with no `if r.audit != nil` noise at each step — and a
// router built without a recorder (most tests, and any install that disabled
// auditing) simply does nothing.
type auditCall struct {
	rec   *AuditRecorder
	start time.Time
	ev    AuditEvent

	// remote marks a caller that arrived over the remote listener, which is the
	// only thing that switches this event from ADR-008's one fail-open record
	// to ADR-010's fail-closed intent + completion pair. It is set from the
	// connection-attested identity in the context and from nothing else.
	remote bool

	// intentWritten records that the pre-call half is already on disk, so the
	// final record is labelled as its completion rather than as a standalone
	// event. A refusal that happens before the intent is written (an unknown
	// tool, a denied grant) never reaches an MCP, so it stays a single record.
	intentWritten bool
}

// beginAudit starts an event, capturing the caller's kernel-attested pid.
// Returns nil when auditing is off or this event kind isn't being recorded, so
// no work is done building an event that would be thrown away.
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
	// A remote caller's identity is attested by its certificate, which is the
	// network equivalent of the peer pid and strictly stronger: a pid is
	// reusable and racy, a fingerprint is neither. The two are mutually
	// exclusive by construction — pid attribution means nothing across a
	// network — so this is an either/or rather than two independent lookups,
	// which is also what guarantees PID/Proc/Parent stay *absent* for a remote
	// call instead of being filled with a locally-meaningless number.
	if rc, ok := bridge.RemoteCallerFromContext(ctx); ok {
		a.remote = true
		a.ev.Actor.Kind = AuditActorRemote
		a.ev.Actor.Auth = AuditAuthMTLS
		a.ev.Actor.ClientID = rc.ClientID
		a.ev.Actor.Fingerprint = rc.Fingerprint
		a.ev.Actor.RemoteAddr = rc.RemoteAddr
		return a
	}
	// Resolve the caller's process now, while it is certainly still alive: a
	// `relay mcp call` child exits as soon as it has its answer, so resolving
	// after the call would frequently find nothing.
	if pid := bridge.CallerPIDFromContext(ctx); pid > 0 {
		a.ev.Actor.PID = pid
		a.ev.Actor.Proc, a.ev.Actor.Parent = ProcessNames(pid)
	}
	return a
}

// setTool records the requested tool and its arguments. Arguments are redacted
// and capped here rather than at write time so the raw values never sit in the
// queue waiting to be persisted.
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

// setMcp records which MCP owns the tool. Known only after tool-owner lookup,
// which is why it's separate from setTool.
func (a *auditCall) setMcp(id string) {
	if a == nil {
		return
	}
	a.ev.McpID = id
}

// setActor fills in the authenticated identity. token is the credential the
// caller presented (used only to distinguish a token hand-off from directory
// auth — the value itself is never recorded).
func (a *auditCall) setActor(ctx context.Context, stored *StoredToken, settings *Settings, token string) {
	if a == nil || stored == nil {
		return
	}
	// A remote caller's kind and auth come from the connection, not from what
	// auth resolution found, so they are left alone here — but the project
	// fields below are still filled in. The caller is remote *and* acting as a
	// project grant, and a record that dropped either half would answer only
	// one of the two questions worth asking of it.
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

// setProject records the resolved grant. Shared by every actor kind: whichever
// way a caller was identified, the project it is acting as comes from relay's
// own auth resolution.
func (a *auditCall) setProject(stored *StoredToken, settings *Settings) {
	a.ev.Actor.ProjectID = stored.ProjectID
	a.ev.Actor.ProjectName = projectNameFor(stored, settings)
}

// setUnauthenticated records the shape of a failed auth attempt. There is no
// project to name, but whether a credential was presented at all — and from
// what directory — is exactly what a review of failed calls needs.
func (a *auditCall) setUnauthenticated(ctx context.Context, token string) {
	if a == nil {
		return
	}
	// A remote caller that failed to resolve a grant is still a known
	// certificate on a known connection: downgrading it to "unknown" would
	// discard the only attribution the record has, and a refused remote call is
	// exactly the record worth keeping attributable.
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

// setAuthority records the access mode in force and the scope actually
// injected. Called at the point CallTool assembles _meta, which is before
// intent() writes the pre-call record, so a remote call's intent line carries
// the authority it is about to run with rather than only the completion doing
// so.
func (a *auditCall) setAuthority(access string, scope map[string]json.RawMessage) {
	if a == nil {
		return
	}
	a.ev.Access = access
	a.ev.Scope = scope
}

// setToolCount records how many tools a list call exposed.
func (a *auditCall) setToolCount(n int) {
	if a == nil {
		return
	}
	a.ev.ToolCount = n
}

// intent writes the pre-call record for a remote caller and blocks until it is
// on disk, returning an error when it could not be written. The router turns
// that error into a refusal, and the MCP is never invoked.
//
// The ordering is the whole point (ADR-010 decision 5). An event written after
// the call — which is what every local call still does, because doneResult
// needs the result bytes — makes "refuse a call that cannot be logged" mean
// nothing: by the time the write fails the mailbox has been read and the data
// has left the MCP. Refusing afterwards is theatre.
//
// An intent with no matching completion is a signal worth alerting on, not
// noise to reconcile away: it means relay invoked an MCP and never learned the
// outcome — a crash, a kill, or a hang. Deleting or pairing those away would
// discard the only evidence that a call went in and nothing came back.
//
// A local caller returns nil immediately, and so does a nil *auditCall — which
// is the case when auditing is switched off entirely. That is a deliberate
// limit on this guarantee: an operator who turns the audit log off has turned
// off the thing the refusal was protecting, and the Tool Calls tab says so
// rather than pretending otherwise.
func (a *auditCall) intent() error {
	if a == nil || !a.remote {
		return nil
	}
	ev := a.ev
	ev.Phase = AuditPhaseIntent
	ev.TS = a.start.UTC()
	// The result is not knowable yet; "pending" says so in the field every
	// reader of this log already looks at.
	ev.Outcome = AuditOutcomePending
	if err := a.rec.RecordDurable(ev); err != nil {
		return err
	}
	a.intentWritten = true
	return nil
}

// done finalizes the event with an outcome and optional error, then hands it to
// the recorder.
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

// doneResult finalizes a completed tool call, recording result metadata (and a
// preview only when the operator opted into one).
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
	// A tool that refuses in-protocol returns a normal result with isError set.
	// The call completed, so this is not AuditOutcomeError, but it is not a
	// success either — recording it as "ok" would hide every application-level
	// refusal (an fsMCP read outside allowed_dirs, say) from `relay audit
	// --outcome ...` and from the Tool Calls filter.
	if a.ev.ResultIsError {
		a.done(AuditOutcomeToolError, nil)
		return
	}
	a.done(AuditOutcomeOK, nil)
}

// resultIsError reports whether an MCP CallToolResult carries isError: true.
// A tool that fails *within* the protocol returns a normal result with that
// flag set, so without this check every application-level failure would be
// logged as a success.
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

// scopeViolationMarker is the _meta key an MCP sets on an error result to say
// that the refusal was a resource-scope refusal rather than any other kind.
//
// The wire shape, which is the whole of the contract:
//
//	{"content": [...], "isError": true,
//	 "_meta": {"scope_violation": true}}
//
// _meta on a result is the symmetric channel to the _meta relay injects on a
// request. It is honoured ONLY when isError is true — a successful call cannot
// claim to have refused anything — and only when the value is boolean true.
// Everything else (absent, a string, a number, a malformed blob) leaves the
// flag off, because this is an optional signal an MCP volunteers and a garbled
// one must degrade to "an ordinary tool_error", never to an error about the
// marker.
//
// It is deliberately a marker relay TRUSTS rather than a message relay parses.
// Distinguishing a scope refusal from any other isError by reading the error
// text would be the ADR-006 line — domain knowledge inside relay — and this
// stays honest about which it is: the flag is what the MCP said about itself,
// and it changes no outcome and gates no decision. It exists so alerting has
// something to select on.
const scopeViolationMarker = "scope_violation"

// scopeViolationMarkerNamespaced is the same signal under MCP's convention for
// a namespaced _meta key. Both spellings are accepted because the cost of
// accepting two is nil — the flag gates nothing — and the cost of accepting
// the wrong one is a signal that silently never appears.
const scopeViolationMarkerNamespaced = "relay/scope_violation"

// resultIsScopeViolation reports whether an MCP error result carries the
// marker. Nil-safe and malformed-safe by construction: every failure to decode
// answers false.
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

// projectNameFor resolves a display name for the audited project. Prefers the
// live settings lookup; falls back to the "project:<name>" form already carried
// on the StoredToken when the project has since been deleted.
func projectNameFor(stored *StoredToken, settings *Settings) string {
	if settings != nil && stored.ProjectID != "" {
		if proj, _ := settings.findProjectByID(stored.ProjectID); proj != nil {
			return proj.Name
		}
	}
	return strings.TrimPrefix(stored.Name, "project:")
}
