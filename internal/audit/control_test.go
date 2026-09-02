package audit

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/control"
)

func TestControlAudit_RecordsAllowedAndRefusedDistinguishably(t *testing.T) {
	rec := newTestAudit(t, nil)

	rec.RecordDecision(control.ControlDecision{
		Method:    "GET",
		Path:      "/api/projects",
		Class:     control.ClassRead,
		Transport: control.TransportSocket,
		CredID:    "cred-eve",
		Allowed:   true,
	})
	rec.RecordDecision(control.ControlDecision{
		Method:    "POST",
		Path:      "/api/mcps",
		Class:     control.ClassExecute,
		Transport: control.TransportTCP,
		CredID:    "cred-scheduler",
		Allowed:   false,
		Reason:    "class not granted",
	})

	events := readLoggedEvents(t, rec)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(events), events)
	}

	var allow, refuse AuditEvent
	for _, ev := range events {
		if ev.Event != AuditEventControlDecision {
			t.Errorf("event = %q, want %q", ev.Event, AuditEventControlDecision)
		}
		if ev.Outcome == AuditOutcomeOK {
			allow = ev
		} else {
			refuse = ev
		}
	}

	if allow.Outcome != AuditOutcomeOK {
		t.Fatalf("allowed decision outcome = %q, want %q", allow.Outcome, AuditOutcomeOK)
	}
	if allow.Error != "" {
		t.Errorf("allowed decision carries a reason: %q", allow.Error)
	}
	if refuse.Outcome != AuditOutcomeDenied {
		t.Fatalf("refused decision outcome = %q, want %q", refuse.Outcome, AuditOutcomeDenied)
	}
	if refuse.Error != "class not granted" {
		t.Errorf("refused decision reason = %q, want %q", refuse.Error, "class not granted")
	}
	if allow.Outcome == refuse.Outcome {
		t.Fatal("allowed and refused decisions are not distinguishable by outcome")
	}
}

func TestControlAudit_RecordCarriesClassTransportAndCredential(t *testing.T) {
	rec := newTestAudit(t, nil)

	rec.RecordDecision(control.ControlDecision{
		Method:    "PUT",
		Path:      "/api/remote",
		Class:     control.ClassConfigure,
		Transport: control.TransportSocket,
		CredID:    "cred-tray",
		Allowed:   false,
		Reason:    "no credential",
	})

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Method != "PUT" {
		t.Errorf("method = %q, want PUT", ev.Method)
	}
	if ev.Path != "/api/remote" {
		t.Errorf("path = %q, want /api/remote", ev.Path)
	}
	if ev.Class != string(control.ClassConfigure) {
		t.Errorf("class = %q, want %q", ev.Class, control.ClassConfigure)
	}
	if ev.Transport != string(control.TransportSocket) {
		t.Errorf("transport = %q, want %q", ev.Transport, control.TransportSocket)
	}
	if ev.Actor.CredID != "cred-tray" {
		t.Errorf("cred id = %q, want cred-tray", ev.Actor.CredID)
	}
	if ev.Actor.Kind != AuditActorControl {
		t.Errorf("actor kind = %q, want %q", ev.Actor.Kind, AuditActorControl)
	}
	if ev.Error != "no credential" {
		t.Errorf("reason = %q, want %q", ev.Error, "no credential")
	}
}

// TestControlAudit_NeverLeaksTokenMaterial is the one that matters: proves
// the recorded event is built from an allow-list of control.ControlDecision fields
// rather than anything wider. The plausible bearer token below is never
// passed to RecordDecision at all — control.ControlDecision has no field for one —
// so this also pins that the type stays that way.
func TestControlAudit_NeverLeaksTokenMaterial(t *testing.T) {
	rec := newTestAudit(t, nil)

	const plausibleToken = "relay_frontend_bearer_9f8e7d6c5b4a3210deadbeefcafefeed"
	const credID = "cred-4c1f9d"

	rec.RecordDecision(control.ControlDecision{
		Method:    "POST",
		Path:      "/api/services",
		Class:     control.ClassExecute,
		Transport: control.TransportSocket,
		CredID:    credID,
		Allowed:   false,
		Reason:    "class not granted",
	})

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Actor.CredID != credID {
		t.Fatalf("credential id = %q, want %q", ev.Actor.CredID, credID)
	}

	encoded, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	body := string(encoded)

	if strings.Contains(body, plausibleToken) {
		t.Fatal("a token that was never passed to RecordDecision appeared in the recorded event")
	}
	// "token" itself is not checked here: AuditActor.Auth legitimately holds
	// the string "token" as the auth-mechanism label (AuditAuthToken), same
	// as every ordinary tool-call record — that is not the leak this test
	// guards against.
	for _, needle := range []string{"Authorization", "Bearer "} {
		if strings.Contains(body, needle) {
			t.Fatalf("token-shaped material %q reached the recorded event: %s", needle, body)
		}
	}

	// Prove the record is an allow-list, not a struct dump: decode as a
	// generic map and require every field to be one RecordDecision is known
	// to produce.
	var generic map[string]interface{}
	if err := json.Unmarshal(encoded, &generic); err != nil {
		t.Fatalf("decode as generic map: %v", err)
	}
	allowed := map[string]bool{
		"id": true, "ts": true, "dur_ms": true, "event": true, "actor": true,
		"outcome": true, "error": true, "method": true, "path": true,
		"class": true, "transport": true, "scope": true,
	}
	for k := range generic {
		if !allowed[k] {
			t.Errorf("unexpected top-level field %q in control-decision record", k)
		}
	}
	actor, ok := generic["actor"].(map[string]interface{})
	if !ok {
		t.Fatal("actor field is not an object")
	}
	allowedActor := map[string]bool{"kind": true, "auth": true, "cred_id": true}
	for k := range actor {
		if !allowedActor[k] {
			t.Errorf("unexpected actor field %q in control-decision record", k)
		}
	}
}

func TestControlAudit_NilRecorderDoesNotPanic(t *testing.T) {
	var rec *AuditRecorder
	rec.RecordDecision(control.ControlDecision{
		Method:  "GET",
		Path:    "/api/projects",
		Class:   control.ClassRead,
		CredID:  "cred-x",
		Allowed: true,
	})
}

func TestControlAudit_NilAuditorInterfaceDoesNotPanic(t *testing.T) {
	var ca control.ControlAuditor = (*AuditRecorder)(nil)
	ca.RecordDecision(control.ControlDecision{
		Method:  "GET",
		Path:    "/api/projects",
		Class:   control.ClassRead,
		CredID:  "cred-x",
		Allowed: true,
	})
}

func TestControlAuditorOrNil_NilRecorderProducesNilInterface(t *testing.T) {
	var rec *AuditRecorder
	if got := ControlAuditorOrNil(rec); got != nil {
		t.Fatalf("ControlAuditorOrNil(nil) = %#v, want nil", got)
	}
}

func TestControlAudit_NewKindIsQueryableAndDistinctFromToolCalls(t *testing.T) {
	rec := newTestAudit(t, nil)

	rec.RecordDecision(control.ControlDecision{
		Method:    "POST",
		Path:      "/api/enrolments",
		Class:     control.ClassGrant,
		Transport: control.TransportSocket,
		CredID:    "cred-op",
		Allowed:   true,
	})
	rec.Record(AuditEvent{
		ID:      NewAuditID(),
		Event:   AuditEventCallTool,
		Outcome: AuditOutcomeOK,
		Tool:    "read_file",
		Actor:   AuditActor{Kind: AuditActorProject},
	})
	rec.Flush()

	byActorKind := rec.Query(AuditQuery{Kind: AuditActorControl})
	if len(byActorKind) != 1 {
		t.Fatalf("--kind control: got %d events, want 1", len(byActorKind))
	}
	if byActorKind[0].Event != AuditEventControlDecision {
		t.Errorf("event = %q, want %q", byActorKind[0].Event, AuditEventControlDecision)
	}

	byEvent := rec.Query(AuditQuery{Event: AuditEventControlDecision})
	if len(byEvent) != 1 {
		t.Fatalf("--event control_decision: got %d events, want 1", len(byEvent))
	}

	toolCalls := rec.Query(AuditQuery{Event: AuditEventCallTool})
	if len(toolCalls) != 1 {
		t.Fatalf("--event call_tool: got %d events, want 1", len(toolCalls))
	}
	for _, ev := range toolCalls {
		if ev.Event == AuditEventControlDecision {
			t.Fatal("control_decision event matched an --event call_tool filter")
		}
	}

	projectActors := rec.Query(AuditQuery{Kind: AuditActorProject})
	for _, ev := range projectActors {
		if ev.Actor.Kind == AuditActorControl {
			t.Fatal("control-plane actor matched an --kind project filter")
		}
	}
}
