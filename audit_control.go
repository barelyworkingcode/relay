package main

import "time"

// Compile-time proof that *AuditRecorder satisfies capability.go's
// ControlAuditor, so a signature drift on either side fails the build rather
// than surfacing only when the two packages are wired together.
var _ ControlAuditor = (*AuditRecorder)(nil)

// RecordDecision is the control-plane counterpart to the router's tool-call
// instrumentation (ADR-015): every authorization decision on the frontend
// API, allowed or refused, becomes an event with the same standing as a
// tool-call denial. Goes through the same fail-open queue as every other
// local record (Record, not RecordDurable) — ADR-010's fail-closed rule is
// scoped to remote tool calls and does not extend here.
//
// ControlDecision carries no token, hash, or header value, only a
// credential id — there is nothing else this method could leak even by
// accident.
func (r *AuditRecorder) RecordDecision(d ControlDecision) {
	if !r.Enabled() {
		return
	}
	outcome := AuditOutcomeOK
	reason := ""
	if !d.Allowed {
		outcome = AuditOutcomeDenied
		reason = d.Reason
	}
	r.Record(AuditEvent{
		ID:        newAuditID(),
		TS:        time.Now().UTC(),
		Event:     AuditEventControlDecision,
		Method:    d.Method,
		Path:      d.Path,
		Class:     string(d.Class),
		Transport: string(d.Transport),
		Outcome:   outcome,
		Error:     reason,
		Actor: AuditActor{
			Kind:   AuditActorControl,
			Auth:   AuditAuthToken,
			CredID: d.CredID,
		},
	})
}
