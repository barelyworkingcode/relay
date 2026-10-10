package features

import (
	"encoding/json"
	"net/http"
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
