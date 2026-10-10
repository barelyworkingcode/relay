package features

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"relaye2e/harness"
)

func TestChiefOfStaffSendAuditOffWritesEvent(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Settings:    map[string]json.RawMessage{"audit": json.RawMessage(`{"enabled": false}`)},
		Credentials: []harness.CredentialSpec{{Name: "cos", Classes: []string{"proxy"}}},
	})
	c := i.SocketHTTP(i.Credential("cos"))
	trace := harness.NewTrace(t)
	resp := c.Do("POST", "/api/chief-of-staff/messages",
		map[string]string{"sessionId": "acme-session", "text": "status please"},
		harness.ReqOpts{Trace: trace, Header: http.Header{"X-Relay-Scope": {"chief-of-staff"}}})
	if resp.Status != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503; body: %s", resp.Status, resp.Body)
	}
	var body struct {
		Error string `json:"error"`
	}
	resp.JSON(t, &body)
	if body.Error != "audit_unavailable" {
		t.Fatalf("error %q, want audit_unavailable", body.Error)
	}
	i.WaitEvent(harness.EventQuery{Key: "chief_of_staff.send", Trace: trace,
		Fields: map[string]any{"status": "denied", "reason": "audit_unavailable"}}, 30*time.Second)
}

func TestChiefOfStaffSendRefusalsWriteEvent(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: []harness.CredentialSpec{{Name: "cos", Classes: []string{"proxy"}}},
	})
	c := i.SocketHTTP(i.Credential("cos"))
	scope := http.Header{"X-Relay-Scope": {"chief-of-staff"}}
	cases := []struct {
		name string
		body any
		want int
		code string
	}{
		{"blank_text", map[string]string{"sessionId": "acme-session", "text": "  "}, http.StatusBadRequest, "text_required"},
		{"missing_session", map[string]string{"text": "hello"}, http.StatusBadRequest, "session_id_required"},
		{"bad_body", []byte("not json"), http.StatusBadRequest, "invalid_body"},
		{"too_large", []byte(`{"sessionId":"acme-session","text":"` + strings.Repeat("x", 70<<10) + `"}`), http.StatusRequestEntityTooLarge, "body_too_large"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			trace := harness.NewTrace(t)
			resp := c.Do("POST", "/api/chief-of-staff/messages", tc.body, harness.ReqOpts{Trace: trace, Header: scope})
			if resp.Status != tc.want {
				t.Fatalf("status %d, want %d; body: %s", resp.Status, tc.want, resp.Body)
			}
			var body struct {
				Error string `json:"error"`
			}
			resp.JSON(t, &body)
			if body.Error != tc.code {
				t.Fatalf("error %q, want %s", body.Error, tc.code)
			}
			i.WaitEvent(harness.EventQuery{Key: "chief_of_staff.send", Trace: trace,
				Fields: map[string]any{"status": "error", "reason": "invalid"}}, 30*time.Second)
			if n := len(i.Events(harness.EventQuery{Key: "chief_of_staff.send", Trace: trace})); n != 1 {
				t.Fatalf("%d chief_of_staff.send events on the trace, want 1", n)
			}
		})
	}
}
