package main

// HTTP coverage for the ADR-014 audit slice: RegisterAuditRoutes shares
// audit.AuditOps with the Tool Calls tab's IPC handlers (ipc_audit.go), so these
// tests focus on the envelope — status codes, filter wiring, and (most
// importantly) that redaction survives the HTTP door exactly as it does the
// IPC one.
//
// TestAuditRoutes_RedactionNeverLeaksCredentialArgs is the one that matters:
// it drives a real tool call with credential-like arguments through the
// actual router instrumentation (audit_call.go), not a hand-crafted "already
// redacted" fixture, so it would catch a route that reads the raw log file
// instead of going through audit.AuditRecorder.Query.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
)

func newAuditRoutesServer(t *testing.T, rec *audit.AuditRecorder) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	RegisterAuditRoutes(&control.RouteRegistrar{CredentialID: APICredentialIDFromContext, Mux: mux, Transport: control.TransportSocket}, &audit.AuditOps{Audit: rec})
	return httptest.NewServer(mux)
}

func seedAuditEvents(t *testing.T, rec *audit.AuditRecorder, events []audit.AuditEvent) {
	t.Helper()
	for _, ev := range events {
		rec.Record(ev)
	}
	rec.Flush()
}

var auditFixture = []audit.AuditEvent{
	{ID: "ev1", Event: audit.AuditEventCallTool, Outcome: audit.AuditOutcomeOK,
		Actor: audit.AuditActor{Kind: audit.AuditActorProject, ProjectID: "p1", ProjectName: "relay"},
		McpID: "fsmcp", Tool: "read_file"},
	{ID: "ev2", Event: audit.AuditEventCallTool, Outcome: audit.AuditOutcomeDenied,
		Actor: audit.AuditActor{Kind: audit.AuditActorProject, ProjectID: "p2", ProjectName: "sandbox"},
		McpID: "macmcp", Tool: "send_mail", Error: "access denied"},
	{ID: "ev3", Event: audit.AuditEventCallTool, Outcome: audit.AuditOutcomeOK,
		Actor: audit.AuditActor{Kind: audit.AuditActorRemote, ProjectID: "p1", ClientID: "hermes-mail"},
		McpID: "macmcp", Tool: "mail_search"},
}

func TestAuditRoutes_QueryHappyPath(t *testing.T) {
	rec := newTestAudit(t, nil)
	seedAuditEvents(t, rec, auditFixture)
	srv := newAuditRoutesServer(t, rec)
	defer srv.Close()

	resp, body := doJSON(t, "GET", srv.URL+"/api/audit", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body %s", resp.StatusCode, body)
	}
	var events []audit.AuditEvent
	if err := json.Unmarshal(body, &events); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d: %+v", len(events), events)
	}
}

func TestAuditRoutes_FiltersActuallyFilter(t *testing.T) {
	rec := newTestAudit(t, nil)
	seedAuditEvents(t, rec, auditFixture)
	srv := newAuditRoutesServer(t, rec)
	defer srv.Close()

	cases := []struct {
		name  string
		query string
		want  []string // event ids expected, order-independent
	}{
		{"project_id", "project_id=p2", []string{"ev2"}},
		{"outcome", "outcome=denied", []string{"ev2"}},
		{"kind", "kind=remote", []string{"ev3"}},
		{"mcp_id", "mcp_id=macmcp", []string{"ev2", "ev3"}},
		{"text", "text=sandbox", []string{"ev2"}},
		{"limit", "limit=1", []string{"ev3"}}, // newest-first: ev3 was recorded last
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, body := doJSON(t, "GET", srv.URL+"/api/audit?"+c.query, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d, body %s", resp.StatusCode, body)
			}
			var events []audit.AuditEvent
			if err := json.Unmarshal(body, &events); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(events) != len(c.want) {
				t.Fatalf("query %q: got %d events, want %d: %+v", c.query, len(events), len(c.want), events)
			}
			got := map[string]bool{}
			for _, e := range events {
				got[e.ID] = true
			}
			for _, id := range c.want {
				if !got[id] {
					t.Errorf("query %q: missing expected event %q, got %+v", c.query, id, events)
				}
			}
		})
	}
}

func TestAuditRoutes_BadLimitIs400(t *testing.T) {
	rec := newTestAudit(t, nil)
	srv := newAuditRoutesServer(t, rec)
	defer srv.Close()

	resp, body := doJSON(t, "GET", srv.URL+"/api/audit?limit=not-a-number", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, body %s", resp.StatusCode, body)
	}

	resp, body = doJSON(t, "GET", srv.URL+"/api/audit?limit=-5", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative limit: status %d, body %s", resp.StatusCode, body)
	}
}

func TestAuditRoutes_BadOutcomeIs400(t *testing.T) {
	rec := newTestAudit(t, nil)
	srv := newAuditRoutesServer(t, rec)
	defer srv.Close()

	resp, body := doJSON(t, "GET", srv.URL+"/api/audit?outcome=sideways", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, body %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "sideways") {
		t.Errorf("refusal must name the offending value: %s", body)
	}
}

func TestAuditRoutes_LogPathReturnsThePath(t *testing.T) {
	rec := newTestAudit(t, nil)
	srv := newAuditRoutesServer(t, rec)
	defer srv.Close()

	resp, body := doJSON(t, "GET", srv.URL+"/api/audit/log", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body %s", resp.StatusCode, body)
	}
	var view auditPathView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.Path != rec.Path() {
		t.Errorf("path = %q, want %q", view.Path, rec.Path())
	}
}

func TestAuditRoutes_ExportWritesAFileAndReturnsItsPath(t *testing.T) {
	rec := newTestAudit(t, nil)
	seedAuditEvents(t, rec, auditFixture)
	srv := newAuditRoutesServer(t, rec)
	defer srv.Close()

	resp, body := doJSON(t, "POST", srv.URL+"/api/audit/export", map[string]interface{}{})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d, body %s", resp.StatusCode, body)
	}
	var view auditPathView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.Path == "" {
		t.Fatal("export response carries no path")
	}
	data, err := os.ReadFile(view.Path)
	if err != nil {
		t.Fatalf("read exported file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != len(auditFixture) {
		t.Fatalf("exported %d lines, want %d", len(lines), len(auditFixture))
	}
}

// The destination is fixed to the audit log's own directory and the filename
// is generated server-side — auditQueryFields carries no path field at all,
// so there is no channel through which a caller-supplied "path" can reach the
// filesystem. This proves the absence rather than a specific rejection: an
// attempted override in the request body is silently ignored because nothing
// on the decode side ever reads it.
func TestAuditRoutes_ExportDestinationIsNotCallerInfluenced(t *testing.T) {
	rec := newTestAudit(t, nil)
	seedAuditEvents(t, rec, auditFixture)
	srv := newAuditRoutesServer(t, rec)
	defer srv.Close()

	resp, body := doJSON(t, "POST", srv.URL+"/api/audit/export", map[string]interface{}{
		"path": "../../../../etc/passwd",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d, body %s", resp.StatusCode, body)
	}
	var view auditPathView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantDir := filepath.Dir(rec.Path())
	if filepath.Dir(view.Path) != wantDir {
		t.Fatalf("export landed in %q, want the audit log's own directory %q", view.Path, wantDir)
	}
}

func TestAuditRoutes_DisabledRecorderDoesNotPanic(t *testing.T) {
	srv := newAuditRoutesServer(t, nil)
	defer srv.Close()

	resp, body := doJSON(t, "GET", srv.URL+"/api/audit", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("query: status %d, body %s", resp.StatusCode, body)
	}
	var events []audit.AuditEvent
	if err := json.Unmarshal(body, &events); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected no events with auditing disabled, got %+v", events)
	}

	resp, body = doJSON(t, "GET", srv.URL+"/api/audit/log", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("log path with auditing disabled: status %d, body %s", resp.StatusCode, body)
	}

	resp, body = doJSON(t, "POST", srv.URL+"/api/audit/export", map[string]interface{}{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("export with auditing disabled: status %d, body %s", resp.StatusCode, body)
	}
}

// The load-bearing test in this file. redactArgs runs inside the real router
// instrumentation (audit_call.go), not as a hand-crafted fixture, so this
// would catch a route that bypassed audit.AuditRecorder.Query to read the log file
// (or the ring) directly.
func TestAuditRoutes_RedactionNeverLeaksCredentialArgs(t *testing.T) {
	mock := newMockConn("macmcp", localTools("send_mail"),
		okHandler(`{"content":[{"type":"text","text":"sent"}]}`))
	r, rec := auditedRouter(t, map[string]config.Permission{"macmcp": config.PermOn}, nil,
		map[string]*mockMcpConn{"macmcp": mock}, nil)

	const secret = "sk-live-hunter2-do-not-leak"
	args := json.RawMessage(`{"to":"a@b.com","api_key":"` + secret + `"}`)
	if _, err := r.CallTool(context.Background(), "send_mail", args, testToken); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	rec.Flush()

	srv := newAuditRoutesServer(t, rec)
	defer srv.Close()

	resp, body := doJSON(t, "GET", srv.URL+"/api/audit", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("query: status %d, body %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), secret) {
		t.Fatalf("the HTTP response leaked a credential-like argument value: %s", body)
	}
	if !strings.Contains(string(body), audit.AuditRedactedValue) {
		t.Errorf("expected the redaction marker %q in the response: %s", audit.AuditRedactedValue, body)
	}

	resp, body = doJSON(t, "POST", srv.URL+"/api/audit/export", map[string]interface{}{})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("export: status %d, body %s", resp.StatusCode, body)
	}
	var exported auditPathView
	if err := json.Unmarshal(body, &exported); err != nil {
		t.Fatalf("decode export response: %v", err)
	}
	data, err := os.ReadFile(exported.Path)
	if err != nil {
		t.Fatalf("read exported file: %v", err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatalf("the exported file leaked a credential-like argument value: %s", data)
	}
}
