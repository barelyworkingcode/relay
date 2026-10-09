package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/enrolment"
	"github.com/barelyworkingcode/relay/internal/jsonrpc"
	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/service"
)

// Domain sentinels: an operation's core marks a failure with one of these so
// the event carries a stable reason instead of "internal".
var (
	errEventNotFound  = errors.New("not found")
	errEventInvalid   = errors.New("invalid request")
	errEventConflict  = errors.New("conflict")
	errEventThrottled = errors.New("throttled")
	errEventUpstream  = errors.New("upstream failure")
)

// eventError carries a caller-facing message unchanged while classing the
// failure for its event. cause, when set, stays reachable to errors.Is.
type eventError struct {
	kind  error
	cause error
}

func (e *eventError) Error() string {
	if e.cause != nil {
		return e.cause.Error()
	}
	return e.kind.Error()
}

func (e *eventError) Unwrap() []error {
	if e.cause != nil {
		return []error{e.kind, e.cause}
	}
	return []error{e.kind}
}

func notFoundf(format string, a ...any) error {
	return &eventError{kind: errEventNotFound, cause: fmt.Errorf(format, a...)}
}

// upstreamErr classes a failure of something relay called. A cancelled or
// timed-out call keeps its own reason.
func upstreamErr(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &eventError{kind: errEventUpstream, cause: err}
}

// foundOrErr turns a "found == false" result with no error into errEventNotFound.
func foundOrErr(err error, found bool) error {
	if err == nil && !found {
		return errEventNotFound
	}
	return err
}

// asInvalid marks a core's error as a caller mistake unless eventOutcome
// already names it or it matches one of the listed internal failures.
func asInvalid(err error, internal ...error) error {
	if err == nil {
		return nil
	}
	for _, i := range internal {
		if errors.Is(err, i) {
			return err
		}
	}
	if _, reason := eventOutcome(err); reason != "internal" {
		return err
	}
	return fmt.Errorf("%w: %w", errEventInvalid, err)
}

// endEventHTTP closes a read's event with the outcome its response status
// maps to.
func endEventHTTP(ev *logging.Event, status int, msg string) {
	outcome, reason := logging.OutcomeForHTTPStatus(status)
	var err error
	if outcome != logging.OutcomeOK {
		err = errors.New(msg)
	}
	ev.End(outcome, reason, err)
}

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
	case errors.Is(err, errIssuanceAuditingRequired):
		return logging.OutcomeDenied, "audit_unavailable"
	case errors.Is(err, errEventNotFound), errors.Is(err, errMcpNotFound),
		errors.Is(err, enrolment.ErrNotFound), errors.Is(err, errEnrolmentRequestNotFound), errors.Is(err, errPasskeyNotFound), errors.Is(err, errEvePasskeyUnknown):
		return logging.OutcomeError, "not_found"
	case errors.Is(err, errEventInvalid), errors.Is(err, errMcpInvalid), errors.Is(err, enrolment.ErrInvalid):
		return logging.OutcomeError, "invalid"
	case errors.Is(err, errEnrolmentRequestsNotWired), errors.Is(err, errLoginOpsUnavailable),
		errors.Is(err, errEveEnrolmentOpsUnavailable), errors.Is(err, errEvePasskeyOpsUnavailable):
		return logging.OutcomeError, "unavailable"
	case errors.Is(err, errEventThrottled):
		return logging.OutcomeDenied, "throttled"
	case errors.Is(err, errEventUpstream):
		return logging.OutcomeError, "upstream"
	case errors.Is(err, service.ErrHelloRefused):
		return logging.OutcomeDenied, "unauthorized"
	case errors.Is(err, errMcpDiscovery):
		return logging.OutcomeError, "upstream"
	case errors.Is(err, errEventConflict), errors.Is(err, errEnrolmentRequestExpired), errors.Is(err, errEnrolmentRequestRefused),
		errors.Is(err, errEnrolmentRequestSASIncomplete), errors.Is(err, errRemoteConfigChangedDuringApproval),
		errors.Is(err, errEvePasskeyAlreadyPending), errors.Is(err, errEvePasskeyLast), errors.Is(err, errEveEnrolmentClosed):
		return logging.OutcomeError, "conflict"
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

// eventResponseWriter runs finish with the response status before the first
// header or body byte leaves, so a handler with many return paths still ends
// its event before the caller gets its answer.
type eventResponseWriter struct {
	http.ResponseWriter
	finish func(status int)
	done   bool
}

func newEventResponseWriter(w http.ResponseWriter, finish func(status int)) *eventResponseWriter {
	return &eventResponseWriter{ResponseWriter: w, finish: finish}
}

func (e *eventResponseWriter) end(status int) {
	if !e.done {
		e.done = true
		e.finish(status)
	}
}

func (e *eventResponseWriter) WriteHeader(status int) {
	e.end(status)
	e.ResponseWriter.WriteHeader(status)
}

func (e *eventResponseWriter) Write(b []byte) (int, error) {
	e.end(http.StatusOK)
	return e.ResponseWriter.Write(b)
}

func (e *eventResponseWriter) Unwrap() http.ResponseWriter { return e.ResponseWriter }
