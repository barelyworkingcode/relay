package features

import (
	"net/http"
	"testing"
	"time"

	"relaye2e/harness"
)

const b319Deadline = 60 * time.Second

func b319Start(t *testing.T) (*harness.Instance, string) {
	t.Helper()
	i := harness.Start(t, harness.Options{
		Presence:    approveGrant,
		Credentials: []harness.CredentialSpec{{Name: "runner", Classes: []string{"execute"}}},
	})
	i.WaitSessionHost(b319Deadline)
	id := startAgentSession(t, i, "claude-code", "sonnet")
	r := i.CLI("session", "stop", "--id", id, "--json")
	if r.Code != 0 {
		t.Fatalf("session stop exited %d: %s", r.Code, r.Stderr)
	}
	i.WaitEvent(harness.EventQuery{Key: "session.delete", Fields: map[string]any{"session_id": id, "status": "ok"}}, b319Deadline)
	return i, id
}

func TestBug319ResumeStoppedSessionIsNotFound(t *testing.T) {
	t.Parallel()
	i, id := b319Start(t)
	resp := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/sessions/"+id+"/resume", nil)
	if resp.Status != http.StatusNotFound {
		t.Fatalf("resume of a stopped session answered %d (%s), want 404", resp.Status, resp.Body)
	}
	var body struct {
		Error string `json:"error"`
	}
	resp.JSON(t, &body)
	if body.Error == "" {
		t.Fatalf("resume 404 body %s carries no error message", resp.Body)
	}
	b319AssertNotFoundRow(t, i, id)
}

func TestBug319ResumeStoppedDormantSessionIsNotFound(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Presence:    approveGrant,
		Credentials: []harness.CredentialSpec{{Name: "runner", Classes: []string{"execute"}}},
	})
	i.WaitSessionHost(b319Deadline)
	id := startAgentSession(t, i, "claude-code", "sonnet")
	i.Restart()
	i.WaitSessionHost(b319Deadline)
	if r := i.CLI("session", "stop", "--id", id, "--json"); r.Code != 0 {
		t.Fatalf("session stop exited %d: %s", r.Code, r.Stderr)
	}
	i.WaitEvent(harness.EventQuery{Key: "session.delete", Fields: map[string]any{"session_id": id, "status": "ok"}}, b319Deadline)
	resp := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/sessions/"+id+"/resume", nil)
	if resp.Status != http.StatusNotFound {
		t.Fatalf("resume of a stopped dormant session answered %d (%s), want 404", resp.Status, resp.Body)
	}
	b319AssertNotFoundRow(t, i, id)
}

func b319AssertNotFoundRow(t *testing.T, i *harness.Instance, id string) {
	t.Helper()
	for _, row := range i.Audit(harness.AuditQuery{Event: "session_resume", Outcome: "not_found"}) {
		if args, ok := row["args"].(map[string]any); ok && args["session_id"] == id {
			return
		}
	}
	t.Fatalf("no session_resume audit row with outcome not_found for session %s", id)
}
