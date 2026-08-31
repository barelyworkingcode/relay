package main

import "time"

// Compile-time proof that *AuditRecorder satisfies capability.go's
// ControlAuditor, so a signature drift on either side fails the build rather
// than surfacing only when the two packages are wired together.
var _ ControlAuditor = (*AuditRecorder)(nil)

// ControlDecision.Path and .Method are read straight off the request line
// (r.URL.Path, r.Method) before RouteRegistrar.authorize has resolved a
// credential, let alone checked its class — a caller holding no class at all,
// or the wrong one, still reaches this record on every refusal. Bounded here,
// generously past any real relay or proxied-service route: a control-plane
// path never needs more than a handful of path segments and ids, and no real
// or WebDAV-style HTTP method approaches this length. What matters is that
// neither bound is anywhere near what the caller could otherwise put there —
// arguments quoted by an attacker are not a source of confinement, distance
// from the attacker's reach is.
const (
	auditMaxControlPathBytes   = 1024
	auditMaxControlMethodBytes = 32
)

// Caps s on a rune boundary, reusing audit.go's truncateRunes rather than a
// second truncation routine for the same on-disk contract (ArgsTruncated):
// the returned bool is that contract's marker, true exactly when s did not
// fit and was cut.
func capControlString(s string, maxBytes int) (string, bool) {
	if len(s) <= maxBytes {
		return s, false
	}
	return truncateRunes(s, maxBytes), true
}

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
	method, methodTruncated := capControlString(d.Method, auditMaxControlMethodBytes)
	path, pathTruncated := capControlString(d.Path, auditMaxControlPathBytes)
	// A remote-listener decision names an enrolment's certificate, not a
	// control-plane credential — there is no CredID for it to name at all,
	// the same attested-identity pair the tool-call audit path already
	// carries for a remote caller (audit_call.go).
	actor := AuditActor{Kind: AuditActorControl, Auth: AuditAuthToken, CredID: d.CredID}
	if d.Transport == TransportTCP && d.ClientID != "" {
		actor = AuditActor{Kind: AuditActorRemote, Auth: AuditAuthMTLS, ClientID: d.ClientID, Fingerprint: d.Fingerprint}
	}
	r.Record(AuditEvent{
		ID:              newAuditID(),
		TS:              time.Now().UTC(),
		Event:           AuditEventControlDecision,
		Method:          method,
		MethodTruncated: methodTruncated,
		Path:            path,
		PathTruncated:   pathTruncated,
		Class:           string(d.Class),
		Transport:       string(d.Transport),
		Outcome:         outcome,
		Error:           reason,
		Actor:           actor,
	})
}
