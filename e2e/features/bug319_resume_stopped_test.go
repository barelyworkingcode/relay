package features

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
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

func TestBug319ResumeHTTPDeletedDormantSessionIsNotFound(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Presence: approveGrant,
		Credentials: []harness.CredentialSpec{
			{Name: "runner", Classes: []string{"execute"}},
			{Name: "proxier", Classes: []string{"proxy"}},
		},
	})
	i.WaitSessionHost(b319Deadline)
	id := startAgentSession(t, i, "claude-code", "sonnet")
	i.Restart()
	i.WaitSessionHost(b319Deadline)
	del := i.SocketHTTP(i.Credential("proxier")).Do("DELETE", "/api/sessions/"+id, nil)
	if del.Status != http.StatusNoContent {
		t.Fatalf("DELETE /api/sessions/%s answered %d (%s), want 204", id, del.Status, del.Body)
	}
	i.WaitEvent(harness.EventQuery{Key: "session.delete", Fields: map[string]any{"session_id": id, "status": "ok"}}, b319Deadline)
	resp := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/sessions/"+id+"/resume", nil)
	if resp.Status != http.StatusNotFound {
		t.Fatalf("resume of an HTTP-deleted dormant session answered %d (%s), want 404", resp.Status, resp.Body)
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

func b319NoSessionEndRow(t *testing.T, i *harness.Instance, id string) {
	t.Helper()
	for _, row := range i.Audit(harness.AuditQuery{Event: "session_end"}) {
		if args, ok := row["args"].(map[string]any); ok && args["session_id"] == id {
			t.Fatalf("session_end audit row names %s, which no ledger record backs: %v", id, row)
		}
	}
}

func TestBug319DeleteUnknownSessionWritesNoSessionEnd(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: []harness.CredentialSpec{{Name: "proxier", Classes: []string{"proxy"}}},
	})
	i.WaitSessionHost(b319Deadline)
	const id = "7c1d2a54-3b6e-4f08-9a1d-5e2f60b4c8d3"
	del := i.SocketHTTP(i.Credential("proxier")).Do("DELETE", "/api/sessions/"+id, nil)
	if del.Status != http.StatusNoContent {
		t.Fatalf("DELETE of an unknown session answered %d (%s), want 204", del.Status, del.Body)
	}
	i.WaitEvent(harness.EventQuery{Key: "session.exited", Fields: map[string]any{"session_id": id}}, b319Deadline)
	b319NoSessionEndRow(t, i, id)
}

func TestBug319DeleteRunningTerminalIDLeavesTerminalAlone(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Presence: approveGrant,
		Settings: map[string]json.RawMessage{
			"terminal_templates": json.RawMessage(`[{"id":"hold","name":"Hold","command":"/bin/sleep"}]`),
		},
		Credentials: []harness.CredentialSpec{{Name: "proxier", Classes: []string{"proxy"}}},
	})
	i.WaitSessionHost(b319Deadline)
	dir := filepath.Join(i.Home, "work", "acme")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	body, _ := json.Marshal(map[string]any{"name": "Acme hold", "path": dir, "allowed_templates": []string{"hold"}})
	var p project
	i.CLIWith(harness.CLIOpts{Stdin: body}, "project", "create", "--file", "-", "--json").JSON(t, &p)
	var term struct {
		TerminalID string `json:"terminalId"`
	}
	i.MustCLI("terminal", "start", "--project", p.ID, "--template", "hold", "--extra-arg", "3600", "--json").JSON(t, &term)

	del := i.SocketHTTP(i.Credential("proxier")).Do("DELETE", "/api/sessions/"+term.TerminalID, nil)
	if del.Status != http.StatusNoContent {
		t.Fatalf("DELETE of a terminal id answered %d (%s), want 204", del.Status, del.Body)
	}
	i.WaitEvent(harness.EventQuery{Key: "session.exited", Fields: map[string]any{"session_id": term.TerminalID}}, b319Deadline)
	b319NoSessionEndRow(t, i, term.TerminalID)
	if r := i.CLI("terminal", "log", "--id", term.TerminalID); r.Code != 0 {
		t.Fatalf("terminal log exited %d after the session delete: %s", r.Code, r.Stderr)
	}
}
