package audit

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/control"
)

// acc2LongLegitimatePath is a realistic proxied-service path: several
// UUID-bearing resource segments deep, the shape a manifest-registered
// session/message/attachment route actually produces. It stays well under
// auditMaxControlPathBytes.
const acc2LongLegitimatePath = "/api/sessions/550e8400-e29b-41d4-a716-446655440000/messages/660e8400-e29b-41d4-a716-446655440000/attachments/770e8400-e29b-41d4-a716-446655440000/download"

// acc2ReadGenerations reads the current audit file plus every rotated
// generation up to and including n, concatenating their bytes. A missing
// generation (rotation never reached it) is skipped rather than failing the
// read.

// TestControlAudit_LongPathBoundsSerializedRecordSize is the core amplification
// proof: a caller-chosen path of attacker-picked size must not scale the
// on-disk record. The bound is asserted on the serialised record, not on the
// path string alone, because the vulnerability was about total row size (and
// therefore retention capacity), not specifically the Path field's own length.
func TestControlAudit_LongPathBoundsSerializedRecordSize(t *testing.T) {
	rec := newTestAudit(t, nil)

	hugePath := "/" + strings.Repeat("A", 400_000)
	rec.RecordDecision(control.ControlDecision{
		Method:    "GET",
		Path:      hugePath,
		Class:     control.ClassRead,
		Transport: control.TransportSocket,
		CredID:    "cred-attacker",
		Allowed:   false,
		Reason:    "class not granted",
	})

	ev := onlyEvent(t, readLoggedEvents(t, rec))

	if len(ev.Path) > auditMaxControlPathBytes {
		t.Fatalf("recorded path is %d bytes, want <= %d", len(ev.Path), auditMaxControlPathBytes)
	}
	if !ev.PathTruncated {
		t.Error("path_truncated = false for a path far over the cap")
	}

	encoded, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	// Generous relative to the cap (allows for JSON escaping and every other
	// field on the record) but three orders of magnitude below the 400 KB the
	// caller actually sent — that gap is the amplification the cap removes.
	const maxRecordBytes = 4096
	if len(encoded) > maxRecordBytes {
		t.Fatalf("serialised record is %d bytes for a %d-byte attacker path, want <= %d",
			len(encoded), len(hugePath), maxRecordBytes)
	}
}

// TestControlAudit_RealisticLongPathIsNotTruncated pins the other half of
// picking a cap: a genuine deep proxied-service path must survive untouched.
func TestControlAudit_RealisticLongPathIsNotTruncated(t *testing.T) {
	rec := newTestAudit(t, nil)

	rec.RecordDecision(control.ControlDecision{
		Method:    "GET",
		Path:      acc2LongLegitimatePath,
		Class:     control.ClassConfigure,
		Transport: control.TransportSocket,
		CredID:    "cred-eve",
		Allowed:   true,
	})

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Path != acc2LongLegitimatePath {
		t.Errorf("path = %q, want %q (unmodified)", ev.Path, acc2LongLegitimatePath)
	}
	if ev.PathTruncated {
		t.Error("path_truncated = true for a path under the cap")
	}
}

// TestControlAudit_PathTruncationIsVisibleInTheRecord proves an operator can
// tell a truncated path from a genuine one from the record alone — the
// marker, not the path's length or content, is what must carry that fact.
func TestControlAudit_PathTruncationIsVisibleInTheRecord(t *testing.T) {
	rec := newTestAudit(t, nil)

	hugePath := "/" + strings.Repeat("Z", 10_000)
	rec.RecordDecision(control.ControlDecision{
		Method: "POST", Path: hugePath, Class: control.ClassExecute,
		Transport: control.TransportSocket, CredID: "cred-attacker", Allowed: false, Reason: "class not granted",
	})
	rec.RecordDecision(control.ControlDecision{
		Method: "GET", Path: acc2LongLegitimatePath, Class: control.ClassRead,
		Transport: control.TransportSocket, CredID: "cred-eve", Allowed: true,
	})

	events := readLoggedEvents(t, rec)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	var truncated, genuine AuditEvent
	for _, ev := range events {
		if ev.Path == acc2LongLegitimatePath {
			genuine = ev
		} else {
			truncated = ev
		}
	}

	if !truncated.PathTruncated {
		t.Error("the cut path does not carry path_truncated = true")
	}
	if genuine.PathTruncated {
		t.Error("the untouched path carries path_truncated = true")
	}

	// The marker must actually be present on the wire, not just true in the
	// Go struct: omitempty would hide a false value, which is correct, but a
	// true value must render.
	truncEncoded, err := json.Marshal(truncated)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(truncEncoded), `"path_truncated":true`) {
		t.Errorf("truncated record does not render path_truncated: %s", truncEncoded)
	}
	genuineEncoded, err := json.Marshal(genuine)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(genuineEncoded), `path_truncated`) {
		t.Errorf("genuine record renders path_truncated at all: %s", genuineEncoded)
	}
}

// TestControlAudit_MethodCapBoundsSerializedRecordAndIsVisible covers the
// other caller-shaped field on this path: Method is read off the request
// line before any class check runs, exactly like Path, and a garbage-token
// method matching the unclassed-turned-configure catch-all route reaches
// this record the same way a maximal path does.
func TestControlAudit_MethodCapBoundsSerializedRecordAndIsVisible(t *testing.T) {
	rec := newTestAudit(t, nil)

	hugeMethod := strings.Repeat("M", 50_000)
	rec.RecordDecision(control.ControlDecision{
		Method: hugeMethod, Path: "/", Class: control.ClassConfigure,
		Transport: control.TransportSocket, CredID: "cred-attacker", Allowed: false, Reason: "class not granted",
	})
	rec.RecordDecision(control.ControlDecision{
		Method: http.MethodDelete, Path: "/api/enrolments/enr_1", Class: control.ClassGrant,
		Transport: control.TransportSocket, CredID: "cred-op", Allowed: true,
	})

	events := readLoggedEvents(t, rec)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	var truncated, genuine AuditEvent
	for _, ev := range events {
		if ev.Method == http.MethodDelete {
			genuine = ev
		} else {
			truncated = ev
		}
	}

	if len(truncated.Method) > auditMaxControlMethodBytes {
		t.Fatalf("recorded method is %d bytes, want <= %d", len(truncated.Method), auditMaxControlMethodBytes)
	}
	if !truncated.MethodTruncated {
		t.Error("method_truncated = false for a method far over the cap")
	}
	if genuine.MethodTruncated {
		t.Error("method_truncated = true for an ordinary HTTP method")
	}
	if genuine.Method != http.MethodDelete {
		t.Errorf("method = %q, want DELETE (unmodified)", genuine.Method)
	}

	encoded, err := json.Marshal(truncated)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const maxRecordBytes = 1024
	if len(encoded) > maxRecordBytes {
		t.Fatalf("serialised record is %d bytes for a %d-byte attacker method, want <= %d",
			len(encoded), len(hugeMethod), maxRecordBytes)
	}
}
