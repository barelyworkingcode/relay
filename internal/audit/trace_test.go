package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const traceTestID = "0123456789abcdef0123456789abcdef"

func TestAuditEvent_TraceIDRoundTripsAndEmptyIsOmitted(t *testing.T) {
	withTrace, err := json.Marshal(AuditEvent{ID: "e1", TraceID: traceTestID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(withTrace), `"trace_id":"`+traceTestID+`"`) {
		t.Errorf("trace_id missing from %s", withTrace)
	}
	var back AuditEvent
	if err := json.Unmarshal(withTrace, &back); err != nil || back.TraceID != traceTestID {
		t.Errorf("round trip = %q, %v", back.TraceID, err)
	}

	without, err := json.Marshal(AuditEvent{ID: "e1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(without), "trace_id") {
		t.Errorf("empty trace_id is not omitted, so old records change shape: %s", without)
	}
}

func TestReadAuditTail_ReadsMixedOldAndNewRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "toolcalls.jsonl")
	old := `{"id":"old1","event":"tool_call"}`
	newer := `{"id":"new1","event":"tool_call","trace_id":"` + traceTestID + `"}`
	if err := os.WriteFile(path, []byte(old+"\n"+newer+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := ReadAuditTail(path, 1<<20)
	if len(got) != 2 {
		t.Fatalf("read %d records, want 2: %+v", len(got), got)
	}
	// Newest first.
	if got[0].ID != "new1" || got[0].TraceID != traceTestID {
		t.Errorf("new record = %+v", got[0])
	}
	if got[1].ID != "old1" || got[1].TraceID != "" {
		t.Errorf("old record = %+v", got[1])
	}
}
