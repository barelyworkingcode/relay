package main

import (
	"net/http"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
)

// recordModelCall is app.modelEndpoint.AuditHook's real implementation
// (docs/model-endpoint.md's Audit section; spec-model-broker.md §7;
// plan-broker-and-sessions.md §2 C4, unit R-M1c). It replaces the slog.Debug
// stand-in trayapp.go used to carry: every finished model-endpoint call
// becomes one audit.AuditEvent, on the same fail-open policy tool calls use
// (audit_call.go's done/doneResult call rec.Record, never RecordDurable) —
// a model call is never delayed by, or refused because of, a full or broken
// audit sink.
func recordModelCall(rec *audit.AuditRecorder, ev ModelCallAudit) {
	if !rec.Enabled() {
		return
	}
	event := modelAuditEventKind(ev)
	// model_list follows list_tools/list_skills' own gate (audit_call.go's
	// beginAudit): a listing is asked for far more often than a call is
	// made, and logging every one by default would bury the calls that
	// matter, exactly the reasoning docs/audit-log.md gives for log_lists.
	if event == audit.AuditEventModelList && !rec.LogLists() {
		return
	}
	rec.Record(modelAuditEvent(event, ev))
}

// modelAuditEventKind classifies a finished call by the same test
// isModelListRequest uses at request time — GET on either spelling of the
// models-list route — rather than threading a bool through ModelCallAudit
// for a distinction its own Method/Path already carry.
func modelAuditEventKind(ev ModelCallAudit) string {
	if ev.Method == http.MethodGet && (ev.Path == "/v1/models" || ev.Path == "/models") {
		return audit.AuditEventModelList
	}
	return audit.AuditEventModelCall
}

// modelAuditEvent builds the on-disk record. Outcome is copied from
// ev.Outcome verbatim rather than remapped through a switch: ModelCallAudit's
// own doc comment already names its exact vocabulary (ok, denied, not_found,
// unauthorized, remote_project, route_not_found, host_unavailable,
// bad_request, body_too_large, trailing_data, error, client_abort,
// rate_limited), it never collides with a tool-call outcome's meaning, and
// audit.go's AuditOutcome* constants for the values this unit adds
// (NotFound, ClientAbort) already spell the same two strings — there is
// nothing a remapping step would add except a second place these could
// drift apart.
func modelAuditEvent(event string, ev ModelCallAudit) audit.AuditEvent {
	return audit.AuditEvent{
		ID:               audit.NewAuditID(),
		TS:               time.Now().UTC(),
		DurMs:            ev.DurationMS,
		Event:            event,
		Actor:            modelAuditActor(ev),
		Transport:        ev.Transport,
		Method:           ev.Method,
		Path:             ev.Path,
		ModelKeyLabel:    ev.ModelKeyLabel,
		Model:            ev.RequestedModel,
		ModelCanonical:   ev.CanonicalModel,
		ModelTarget:      ev.Target,
		Stream:           ev.Stream,
		RequestBytes:     ev.RequestBytes,
		ResponseBytes:    ev.ResponseBytes,
		PromptTokens:     ev.Usage.PromptTokens,
		CompletionTokens: ev.Usage.CompletionTokens,
		Status:           ev.Status,
		Outcome:          ev.Outcome,
	}
}

// modelAuditActor maps the model endpoint's own caller vocabulary onto
// audit.AuditActor. A caller that never resolved at all (Outcome
// "unauthorized", CallerKind empty) gets Kind Unknown rather than a
// caller-shaped guess — the same call docs/audit-log.md's setUnauthenticated
// makes for a tool call whose credential did not resolve.
func modelAuditActor(ev ModelCallAudit) audit.AuditActor {
	switch ev.CallerKind {
	case "project":
		// A session caller acts under the project's grant but is not the
		// project's bearer: C4 records it as its own actor kind, carrying
		// the session that vouched for it, so an audit reader can tell a
		// call made from inside a session from one made with the token.
		kind := audit.AuditActorProject
		if ev.SessionID != "" {
			kind = audit.AuditActorProjectSession
		}
		return audit.AuditActor{
			Kind:        kind,
			ProjectID:   ev.CallerName,
			ProjectName: ev.CallerProjectName,
			SessionID:   ev.SessionID,
			Auth:        modelAuditAuth(ev.Auth),
		}
	case "service":
		return audit.AuditActor{
			Kind:      audit.AuditActorService,
			Auth:      audit.AuditAuthService,
			ServiceID: ev.CallerName,
		}
	default:
		return audit.AuditActor{Kind: audit.AuditActorUnknown, Auth: modelAuditAuth(ev.Auth)}
	}
}

// modelAuditAuth translates ModelCallAudit.Auth's vocabulary ("token",
// "model_key", "identity", "session", or "" for nothing attempted) to
// audit.go's AuditAuth* constants. "identity" becomes AuditAuthService: a
// model.sock caller with no bearer header authenticates by launch identity
// exactly the way a tool-call service actor does (audit_call.go's setActor),
// so this is the same fact under the model endpoint's own name for it.
// "session" is the project_session path — a session's root process or a C3
// member of it — and is the same AuditAuthSession a tool call records.
func modelAuditAuth(auth string) string {
	switch auth {
	case "token":
		return audit.AuditAuthToken
	case "model_key":
		return audit.AuditAuthModelKey
	case "identity":
		return audit.AuditAuthService
	case "session":
		return audit.AuditAuthSession
	default:
		return audit.AuditAuthNone
	}
}
