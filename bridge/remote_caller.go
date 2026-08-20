package bridge

import "context"

// RemoteCaller is the connection-attested identity of a client that reached
// relay over the remote listener (ADR-010): the enrolled client the certificate
// resolved to, that certificate's fingerprint, and the peer address.
//
// Every field is derived from the connection. None of it is ever read out of a
// request body, and there is deliberately no constructor that takes one — the
// whole point of decision 6 is that a remote caller's identity is attested the
// same way a local caller's pid is, rather than asserted.
//
// The fingerprint is carried in full. Truncating it would save nothing and cost
// the one question the log has to answer after an enrolment is deleted: which
// *key* made this call, when the record naming that key is gone.
type RemoteCaller struct {
	ClientID    string
	Fingerprint string
	RemoteAddr  string
}

// attested reports whether this identity actually came from a verified
// certificate. The fingerprint is the attestation — a client id on its own is
// just a string someone chose — so an identity without one is not admitted to
// the context at all. This is what keeps "is this caller remote?" a question
// about the connection rather than about which fields happen to be populated.
func (c RemoteCaller) attested() bool { return c.Fingerprint != "" }

type remoteCallerCtxKey struct{}

// WithRemoteCaller returns ctx carrying the identity behind a remote (mTLS)
// connection. Carried in the context for the same reason as the peer pid and
// the caller-asserted cwd: ToolRouter is implemented across repos and must not
// grow a parameter for every piece of caller metadata (ADR-008 Consequences).
//
// An unattested identity returns ctx unchanged, mirroring WithCallerPID's
// treatment of a zero pid. A caller that is not on the remote listener must
// never be able to acquire remote identity by any route, because that identity
// is what switches auditing from ADR-008's fail-open path to ADR-010's
// fail-closed one.
func WithRemoteCaller(ctx context.Context, c RemoteCaller) context.Context {
	if !c.attested() {
		return ctx
	}
	return context.WithValue(ctx, remoteCallerCtxKey{}, c)
}

// RemoteCallerFromContext returns the identity set by WithRemoteCaller. The
// bool reports whether the call arrived over the remote listener at all, which
// is a different question from whether any particular field is populated.
func RemoteCallerFromContext(ctx context.Context) (RemoteCaller, bool) {
	c, ok := ctx.Value(remoteCallerCtxKey{}).(RemoteCaller)
	return c, ok
}
