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
	a.ev.Actor.Kind = AuditActorUnknown
	if token == "" {
		a.ev.Actor.Auth = AuditAuthNone
		a.ev.Actor.Cwd = bridge.CallerCwdFromContext(ctx)
	} else {
		a.ev.Actor.Auth = AuditAuthToken
	}
}

// setToolCount records how many tools a list call exposed.
func (a *auditCall) setToolCount(n int) {
	if a == nil {
		return
	}
	a.ev.ToolCount = n
}

// done finalizes the event with an outcome and optional error, then hands it to
// the recorder.
func (a *auditCall) done(outcome string, err error) {
	if a == nil {
		return
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
	if n := a.rec.cfg.MaxResultPreviewBytes; n > 0 && len(result) > 0 {
		a.ev.ResultPreview = truncateRunes(string(result), n)
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
