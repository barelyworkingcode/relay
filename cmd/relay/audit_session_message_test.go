package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/audit"
)

// The rows are written as the log carries them, so this also pins the field
// names relay audit reads.
func TestAuditDetail_SessionMessageRowNamesSessionOriginAndPhase(t *testing.T) {
	for _, tc := range []struct {
		name, line, want string
	}{
		{
			"intent",
			`{"id":"a1","ts":"2026-10-05T14:03:07.123Z","event":"session_message","phase":"intent","actor":{"kind":"control","auth":"token","cred_id":"launch:service:chief"},"args":{"session_id":"s-3af1","origin":"chief-of-staff","text":"restart the job","text_bytes":15},"outcome":"pending"}`,
			"session=s-3af1 origin=chief-of-staff intent",
		},
		{
			"completion",
			`{"id":"a1","ts":"2026-10-05T14:03:07.141Z","dur_ms":18,"event":"session_message","phase":"completion","args":{"session_id":"s-3af1","origin":"chief-of-staff","text_bytes":15},"outcome":"ok"}`,
			"session=s-3af1 origin=chief-of-staff completion",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ev audit.AuditEvent
			if err := json.Unmarshal([]byte(tc.line), &ev); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := auditDetail(ev); !strings.HasPrefix(got, tc.want) {
				t.Errorf("DETAIL = %q, want it to start with %q", got, tc.want)
			}
		})
	}
}
