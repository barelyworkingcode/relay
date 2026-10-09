package main

import (
	"context"
	"errors"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/jsonrpc"
	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/presence"
)

// endEvent closes an event with the outcome the error maps to.
func endEvent(ev *logging.Event, err error) {
	outcome, reason := eventOutcome(err)
	ev.End(outcome, reason, err)
}

// eventOutcome maps an operation's error to the outcome and stable reason code
// its event line carries. Order matters: the typed refusals are checked before
// the generic context and fallback cases.
func eventOutcome(err error) (logging.Outcome, string) {
	if err == nil {
		return logging.OutcomeOK, ""
	}
	var launch *LaunchRefusal
	var dropIn *DropInRefusal
	switch {
	case errors.Is(err, presence.ErrRefused):
		return logging.OutcomeDenied, "presence_refused"
	case errors.Is(err, presence.ErrNoSession):
		return logging.OutcomeDenied, "presence_no_session"
	case errors.Is(err, presence.ErrUnavailable), errors.Is(err, errPresenceGateNotWired):
		return logging.OutcomeDenied, "presence_unavailable"
	case errors.Is(err, presence.ErrGrantInvalid):
		return logging.OutcomeDenied, "presence_invalid"
	case errors.Is(err, control.ErrClassNotGranted), errors.Is(err, control.ErrOutsideScope), errors.Is(err, control.ErrUnknownScope):
		return logging.OutcomeDenied, "not_granted"
	case errors.Is(err, control.ErrNoCredential):
		return logging.OutcomeDenied, "unauthorized"
	case errors.As(err, &launch):
		return refusalOutcome(launch.Status, launch.Code)
	case errors.As(err, &dropIn):
		return refusalOutcome(dropIn.Status, dropIn.Code)
	}
	switch bridge.ErrorCode(err) {
	case jsonrpc.CodeUnauthorized:
		return logging.OutcomeDenied, "unauthorized"
	case jsonrpc.CodeInvalidParams:
		return logging.OutcomeError, "invalid"
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return logging.OutcomeError, "timeout"
	case errors.Is(err, context.Canceled):
		return logging.OutcomeError, "cancelled"
	}
	return logging.OutcomeError, "internal"
}

// refusalOutcome keeps a refusal's own code. A refusal below status 500 is a
// decision (denied); at 500 and above something failed (error).
func refusalOutcome(status int, code string) (logging.Outcome, string) {
	if status >= 500 {
		return logging.OutcomeError, code
	}
	return logging.OutcomeDenied, code
}
