package bridge

import (
	"context"
	"errors"

	"github.com/barelyworkingcode/relay/internal/jsonrpc"
)

// ErrSessionCaller is the refusal an operator-only door gives a caller that is
// inside a relay session or a sandbox.
var ErrSessionCaller = errors.New("this command cannot be run from inside a relay session or a sandbox; run it from your own terminal")

// RequireOperatorCaller admits a same-user peer on the 0600 socket that is not
// a member of a live relay session and not running under a Seatbelt profile.
// It is the CLI's identity: no credential class stands for it.
//
// Deliberate: the two checks and their order are handleSandboxAttach's. The
// membership answer is the kernel's, but ancestry is erased by reparenting, so
// the peer's own confinement is checked independently. A confinement probe
// that could not run (!ok) refuses the same as a confined peer.
func RequireOperatorCaller(ctx context.Context) error {
	if _, member := ConnMembershipFromContext(ctx).Session(); member {
		return jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, ErrSessionCaller)
	}
	if confined, ok := connConfinedFromContext(ctx); !ok || confined {
		return jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, ErrSessionCaller)
	}
	return nil
}
