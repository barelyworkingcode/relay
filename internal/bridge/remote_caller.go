package bridge

import "context"

// RemoteCaller is the connection-attested identity of a client that reached
// relay over the remote listener (ADR-010): the enrolled client the
// certificate resolved to, that certificate's fingerprint, and the peer
// address. Every field is derived from the connection, never from a request
// body, and there is deliberately no constructor that takes one — a remote
// caller's identity is attested the same way a local caller's pid is, rather
// than asserted.
//
// The fingerprint is carried in full, not truncated: it is the one thing
// that can still answer "which key made this call" after the enrolment
// naming that key has been deleted.
type RemoteCaller struct {
	ClientID    string
	Fingerprint string
	RemoteAddr  string
}

// attested reports whether this identity came from a verified certificate.
// The fingerprint is the attestation — a client id alone is just a string
// someone chose — so an identity without one is never admitted to the
// context at all.
func (c RemoteCaller) attested() bool { return c.Fingerprint != "" }

type remoteCallerCtxKey struct{}

// WithRemoteCaller carries the identity behind a remote (mTLS) connection.
// Carried in the context, not a parameter, for the same reason as the peer
// pid and the caller-asserted cwd: ToolRouter is implemented across repos and
// must not grow a parameter for every piece of caller metadata (ADR-008
// Consequences).
//
// An unattested identity returns ctx unchanged. A caller that is not on the
// remote listener must never acquire remote identity by any route, because
// that identity is what switches auditing from ADR-008's fail-open path to
// ADR-010's fail-closed one.
func WithRemoteCaller(ctx context.Context, c RemoteCaller) context.Context {
	if !c.attested() {
		return ctx
	}
	return context.WithValue(ctx, remoteCallerCtxKey{}, c)
}

// RemoteCallerFromContext's bool reports whether the call arrived over the
// remote listener at all — a different question from whether any particular
// field is populated.
func RemoteCallerFromContext(ctx context.Context) (RemoteCaller, bool) {
	c, ok := ctx.Value(remoteCallerCtxKey{}).(RemoteCaller)
	return c, ok
}
