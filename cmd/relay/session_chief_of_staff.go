package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
	sessiontypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// maxChiefOfStaffBodyBytes bounds a Chief of Staff message request.
const maxChiefOfStaffBodyBytes = 64 << 10

type chiefOfStaffMessageBody struct {
	SessionID string `json:"sessionId"`
	Text      string `json:"text"`
}

type chiefOfStaffMessageResult struct {
	SessionID string `json:"sessionId"`
	Origin    string `json:"origin"`
	At        string `json:"at"`
}

func writeChiefOfStaffError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": code, "message": message})
}

// handleChiefOfStaffMessage is the only door that stamps the Chief of Staff
// origin on a message. The origin is the constant below and is never read from
// the body, and no other decoder in relay accepts one, so a claim made
// anywhere else records as the person.
//
// Delivery is fail-closed on the audit trail: the intent row is durable
// before anything is sent, and a recorder that cannot write refuses the
// request. The completion row is best effort, as for every other intent and
// completion pair. The text reaches the log only through audit.log_args.
func (d sessionRouteDeps) handleChiefOfStaffMessage(w http.ResponseWriter, r *http.Request) {
	const origin = sessiontypes.OriginChiefOfStaff

	var body chiefOfStaffMessageBody
	r.Body = http.MaxBytesReader(w, r.Body, maxChiefOfStaffBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeChiefOfStaffError(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body is larger than 64 KiB")
			return
		}
		writeChiefOfStaffError(w, http.StatusBadRequest, "invalid_body", "body must be JSON {\"sessionId\",\"text\"}")
		return
	}
	if body.SessionID == "" {
		writeChiefOfStaffError(w, http.StatusBadRequest, "session_id_required", "sessionId is required")
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeChiefOfStaffError(w, http.StatusBadRequest, "text_required", "text is required")
		return
	}
	if !d.auditor.Ready() {
		writeChiefOfStaffError(w, http.StatusServiceUnavailable, "audit_unavailable", "auditing is off; the Chief of Staff cannot send")
		return
	}

	status, code, message, at := d.sendChiefOfStaffText(r.Context(), callerAuditActor(resolveLaunchCaller(r, d.store)), body.SessionID, body.Text)
	if status == http.StatusAccepted {
		writeJSON(w, http.StatusAccepted, chiefOfStaffMessageResult{SessionID: body.SessionID, Origin: origin, At: at})
		return
	}
	writeChiefOfStaffError(w, status, code, message)
}

// deliverChiefOfStaffText sends text to a session as the Chief of Staff and
// records the intent and completion rows. The caller has already checked that
// auditing is ready. It answers 202 with empty code and message on success.
func (d sessionRouteDeps) deliverChiefOfStaffText(ctx context.Context, actor audit.AuditActor, sessionID, text string) (status int, code, message string) {
	status, code, message, _ = d.sendChiefOfStaffText(ctx, actor, sessionID, text)
	return status, code, message
}

func (d sessionRouteDeps) sendChiefOfStaffText(ctx context.Context, actor audit.AuditActor, sessionID, text string) (status int, wireCode, message, at string) {
	const origin = sessiontypes.OriginChiefOfStaff
	start := time.Now()
	ev := logging.BeginEvent(ctx, "chief_of_staff.send").Set("session_id", sessionID).Set("origin", origin)

	id := audit.NewAuditID()
	intent := audit.AuditEvent{
		ID: id, TS: time.Now(), Event: audit.AuditEventSessionMessage, Phase: audit.AuditPhaseIntent,
		Actor:   actor,
		Args:    d.sessionMessageArgs(sessionID, text, origin, true),
		Outcome: audit.AuditOutcomePending,
	}
	if err := d.auditor.RecordDurable(intent); err != nil {
		ev.End(logging.OutcomeDenied, "audit_unavailable", err)
		return http.StatusServiceUnavailable, "audit_unavailable", "the audit log could not record the message", ""
	}

	resp, hostErr, err := d.host().Send(context.WithoutCancel(ctx), hostapi.SendRequest{SessionID: sessionID, Text: text, Origin: origin})

	outcome, errCode := audit.AuditOutcomeOK, ""
	status, wireCode, message = http.StatusAccepted, "", ""
	switch {
	case err != nil:
		outcome, errCode = audit.AuditOutcomeError, "session_host_unavailable"
		status, wireCode, message = http.StatusBadGateway, "session_host_unavailable", "the session host could not be reached"
	case hostErr != nil:
		switch hostErr.Error {
		case hostapi.ErrSessionNotFound:
			outcome, errCode = audit.AuditOutcomeNotFound, ""
			status, wireCode, message = http.StatusNotFound, "session_not_found", "session not found"
		case hostapi.ErrAlreadyProcessing:
			outcome, errCode = audit.AuditOutcomeError, hostapi.ErrAlreadyProcessing
			status, wireCode, message = http.StatusConflict, hostapi.ErrAlreadyProcessing, "the session is already processing a message"
		case hostapi.ErrResumeRequired:
			outcome, errCode = audit.AuditOutcomeError, hostapi.ErrResumeRequired
			status, wireCode, message = http.StatusConflict, hostapi.ErrResumeRequired, "the session is not running; resume it first"
		case hostapi.ErrDroppedIn:
			outcome, errCode = audit.AuditOutcomeError, hostapi.ErrDroppedIn
			status, wireCode, message = http.StatusConflict, hostapi.ErrDroppedIn, "a terminal holds this session; close it first"
		case hostapi.ErrSendFailed:
			outcome, errCode = audit.AuditOutcomeError, hostapi.ErrSendFailed
			status, wireCode, message = http.StatusBadGateway, "session_host_unavailable", "the session host could not deliver the message"
		default:
			outcome, errCode = audit.AuditOutcomeError, "session_host_unavailable"
			status, wireCode, message = http.StatusBadGateway, "session_host_unavailable", "the session host refused the message"
		}
	}

	completion := intent
	completion.Phase = audit.AuditPhaseCompletion
	completion.DurMs = time.Since(start).Milliseconds()
	completion.Args = d.sessionMessageArgs(sessionID, text, origin, false)
	completion.Outcome = outcome
	completion.Error = errCode
	d.auditor.Record(completion)

	if status == http.StatusAccepted {
		ev.End(logging.OutcomeOK, "", nil)
		return status, "", "", resp.At
	}
	logErr := wireCode
	if errCode != "" {
		logErr = errCode
	}
	ev.End(logging.OutcomeError, sendFailureReason(status), errors.New(logErr))
	return status, wireCode, message, ""
}

// sessionMessageArgs builds a session_message row's args. The text is carried
// only on the intent row and only when audit.log_args is on, cut to
// max_arg_bytes; the byte count is always recorded.
func (d sessionRouteDeps) sessionMessageArgs(sessionID, body, origin string, withText bool) json.RawMessage {
	args := map[string]any{
		"session_id": sessionID,
		"origin":     origin,
		"text_bytes": len(body),
	}
	if withText && d.auditor.LogArgs() {
		text, truncated := d.auditor.CapText(body)
		args["text"] = text
		if truncated {
			args["text_truncated"] = true
		}
	}
	raw, _ := json.Marshal(args)
	return raw
}

// sendFailureReason names why a send to the Chief of Staff session failed from
// the status the caller is answered with.
func sendFailureReason(status int) string {
	switch status {
	case http.StatusNotFound:
		return "not_found"
	case http.StatusConflict:
		return "conflict"
	case http.StatusBadGateway:
		return "upstream"
	case http.StatusServiceUnavailable:
		return "unavailable"
	}
	return "internal"
}
