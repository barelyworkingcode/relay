package audit

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func approvalFixture() PresenceApproval {
	return PresenceApproval{
		Op:         "project.grant",
		Subject:    "verify-1a2b",
		Via:        "http",
		Actor:      AuditActor{Kind: AuditActorControl, Auth: AuditAuthToken, CredID: "cred-1"},
		Start:      time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC),
		Dur:        5 * time.Millisecond,
		PresenceID: "pid-1",
		Approver:   "testapprover",
	}
}

func rawRow(t *testing.T, ev AuditEvent) map[string]json.RawMessage {
	t.Helper()
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPresenceApprovalEvent_Shape(t *testing.T) {
	ev := PresenceApprovalEvent(approvalFixture())
	if ev.Event != AuditEventControlDecision || ev.Outcome != AuditOutcomeOK || ev.Error != "" ||
		ev.Method != "project.grant" || ev.Subject != "verify-1a2b" || ev.Via != "http" ||
		ev.PresenceID != "pid-1" || ev.PresenceApprover != "testapprover" || ev.Path != "" {
		t.Fatalf("approval event = %+v", ev)
	}
	if ev.Actor.CredID != "cred-1" {
		t.Errorf("actor = %+v, want the requester", ev.Actor)
	}
	raw := rawRow(t, ev)
	if string(raw["presence_approver"]) != `"testapprover"` || string(raw["presence_id"]) != `"pid-1"` {
		t.Errorf("wire presence_approver = %s, presence_id = %s", raw["presence_approver"], raw["presence_id"])
	}
}

func TestPresenceRefusalEvent_CarriesApproverOnlyWhenSet(t *testing.T) {
	base := PresenceRefusal{Op: "credential.mint", Via: "cli", Start: time.Now(), Reason: "presence was refused"}
	if _, ok := rawRow(t, PresenceRefusalEvent(base))["presence_approver"]; ok {
		t.Error("a refusal by a person carries presence_approver; it must be absent")
	}
	base.Approver = "testapprover"
	ev := PresenceRefusalEvent(base)
	if ev.PresenceApprover != "testapprover" || ev.Outcome != AuditOutcomeDenied {
		t.Errorf("refusal event = %+v, want denied by testapprover", ev)
	}
}

func TestRecordPresenceApproval_IsDurableAndFailsClosed(t *testing.T) {
	rec := newTestAudit(t, nil)
	if err := rec.RecordPresenceApproval(approvalFixture()); err != nil {
		t.Fatalf("RecordPresenceApproval: %v", err)
	}
	// RecordDurable has returned: the row is written without a Flush.
	data, err := os.ReadFile(rec.Path())
	if err != nil || !strings.Contains(string(data), `"presence_approver":"testapprover"`) {
		t.Fatalf("log after durable record = %q, err %v", data, err)
	}

	if err := rec.CloseWriterForTest(); err != nil {
		t.Fatal(err)
	}
	if err := rec.RecordPresenceApproval(approvalFixture()); err == nil {
		t.Fatal("RecordPresenceApproval with a broken sink returned nil; an unrecorded approval must be an error")
	}
}
