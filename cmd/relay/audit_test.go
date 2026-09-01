package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
)

func newTestAudit(t *testing.T, cfg *config.AuditConfig) *audit.AuditRecorder {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit", "toolcalls.jsonl")
	rec, err := audit.NewAuditRecorder(cfg, path, openAuditWriter)
	if err != nil {
		t.Fatalf("NewAuditRecorder: %v", err)
	}
	if rec != nil {
		t.Cleanup(rec.Close)
	}
	return rec
}

func auditedRouter(t *testing.T, perms map[string]config.Permission, disabled map[string][]string, mocks map[string]*mockMcpConn, cfg *config.AuditConfig) (*appRouter, *audit.AuditRecorder) {
	t.Helper()
	r := setupRouter(t, perms, disabled, nil, mocks)
	rec := newTestAudit(t, cfg)
	r.audit = rec
	return r, rec
}

func readLoggedEvents(t *testing.T, rec *audit.AuditRecorder) []audit.AuditEvent {
	t.Helper()
	rec.Flush()
	data, err := os.ReadFile(rec.Path())
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	var out []audit.AuditEvent
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var ev audit.AuditEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("audit log line is not valid JSON: %v\nline: %s", err, line)
		}
		out = append(out, ev)
	}
	return out
}

func onlyEvent(t *testing.T, events []audit.AuditEvent) audit.AuditEvent {
	t.Helper()
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 audit event, got %d: %+v", len(events), events)
	}
	return events[0]
}

func TestAudit_RecordsSuccessfulCall(t *testing.T) {
	mock := newMockConn("fsmcp", simpleTools("read_file"), okHandler(`{"content":[{"type":"text","text":"hi"}]}`))
	r, rec := auditedRouter(t,
		map[string]config.Permission{"fsmcp": config.PermOn}, nil,
		map[string]*mockMcpConn{"fsmcp": mock}, nil)

	args := json.RawMessage(`{"path":"/tmp/notes.md"}`)
	if _, err := r.CallTool(context.Background(), "read_file", args, testToken); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Event != audit.AuditEventCallTool {
		t.Errorf("event = %q, want %q", ev.Event, audit.AuditEventCallTool)
	}
	if ev.Outcome != audit.AuditOutcomeOK {
		t.Errorf("outcome = %q, want ok (error=%q)", ev.Outcome, ev.Error)
	}
	if ev.Tool != "read_file" || ev.McpID != "fsmcp" {
		t.Errorf("tool/mcp = %q/%q, want read_file/fsmcp", ev.Tool, ev.McpID)
	}
	if ev.Actor.ProjectID != "test-project" || ev.Actor.ProjectName != "test" {
		t.Errorf("actor project = %q/%q, want test-project/test", ev.Actor.ProjectID, ev.Actor.ProjectName)
	}
	if ev.Actor.Kind != audit.AuditActorProject || ev.Actor.Auth != audit.AuditAuthToken {
		t.Errorf("actor kind/auth = %q/%q, want project/token", ev.Actor.Kind, ev.Actor.Auth)
	}
	if string(ev.Args) != `{"path":"/tmp/notes.md"}` {
		t.Errorf("args = %s, want the original object", ev.Args)
	}
	if ev.ResultBytes == 0 {
		t.Error("result_bytes = 0, want the size of the tool result")
	}
	if ev.ID == "" || ev.TS.IsZero() {
		t.Errorf("event id/timestamp not set: id=%q ts=%v", ev.ID, ev.TS)
	}
}

func TestAudit_RecordsDeniedCall(t *testing.T) {
	mock := newMockConn("fsmcp", simpleTools("read_file", "fs_bash"), okHandler(`{}`))
	r, rec := auditedRouter(t,
		map[string]config.Permission{"fsmcp": config.PermOn},
		map[string][]string{"fsmcp": {"fs_bash"}},
		map[string]*mockMcpConn{"fsmcp": mock}, nil)

	if _, err := r.CallTool(context.Background(), "fs_bash", json.RawMessage(`{"cmd":"ls"}`), testToken); err == nil {
		t.Fatal("expected a denial for a disabled tool")
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Outcome != audit.AuditOutcomeDenied {
		t.Errorf("outcome = %q, want denied", ev.Outcome)
	}
	if ev.Tool != "fs_bash" || ev.McpID != "fsmcp" {
		t.Errorf("tool/mcp = %q/%q, want fs_bash/fsmcp", ev.Tool, ev.McpID)
	}
	if ev.Actor.ProjectID != "test-project" {
		t.Errorf("denied call lost the project attribution: %+v", ev.Actor)
	}
	if !strings.Contains(ev.Error, "disabled") {
		t.Errorf("error = %q, want the denial reason", ev.Error)
	}
}

func TestAudit_RecordsUnauthorizedCall(t *testing.T) {
	mock := newMockConn("fsmcp", simpleTools("read_file"), okHandler(`{}`))
	r, rec := auditedRouter(t,
		map[string]config.Permission{"fsmcp": config.PermOn}, nil,
		map[string]*mockMcpConn{"fsmcp": mock}, nil)

	if _, err := r.CallTool(context.Background(), "read_file", json.RawMessage(`{}`), "not-a-real-token"); err == nil {
		t.Fatal("expected an auth failure for an unknown token")
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Outcome != audit.AuditOutcomeUnauthorized {
		t.Errorf("outcome = %q, want unauthorized", ev.Outcome)
	}
	if ev.Actor.Kind != audit.AuditActorUnknown {
		t.Errorf("actor kind = %q, want unknown", ev.Actor.Kind)
	}
	// A credential was presented but did not resolve, which is why Auth is
	// still recorded as "token" even though the outcome is unauthorized.
	if ev.Actor.Auth != audit.AuditAuthToken {
		t.Errorf("actor auth = %q, want token", ev.Actor.Auth)
	}
	if ev.Actor.ProjectID != "" {
		t.Errorf("unauthenticated call attributed to project %q", ev.Actor.ProjectID)
	}
}

func TestAudit_RecordsUnknownTool(t *testing.T) {
	r, rec := auditedRouter(t, map[string]config.Permission{"fsmcp": config.PermOn}, nil, nil, nil)

	if _, err := r.CallTool(context.Background(), "no_such_tool", nil, testToken); err == nil {
		t.Fatal("expected an error for an unknown tool")
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Outcome != audit.AuditOutcomeError || ev.Tool != "no_such_tool" {
		t.Errorf("got outcome=%q tool=%q, want error/no_such_tool", ev.Outcome, ev.Tool)
	}
}

func TestAudit_RecordsDirectoryAuth(t *testing.T) {
	dir := t.TempDir()

	// The store hands out settings by value, so the project must be opted
	// into directory auth before the router is built from it.
	settings := makeSettings(map[string]config.Permission{"fsmcp": config.PermOn}, nil, nil)
	settings.Projects[0].Path = dir
	settings.Projects[0].AllowCwdAuth = true

	mgr := mcpbroker.NewManager(nil)
	addMockConn(mgr, "fsmcp", newMockConn("fsmcp", simpleTools("read_file"), okHandler(`{}`)))
	r := newTestRouter(t, settings, mgr)
	rec := newTestAudit(t, nil)
	r.audit = rec

	ctx := bridge.WithCallerCwd(context.Background(), dir)
	if _, err := r.CallTool(ctx, "read_file", json.RawMessage(`{}`), ""); err != nil {
		t.Fatalf("CallTool with directory auth: %v", err)
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Actor.Auth != audit.AuditAuthCwd {
		t.Errorf("actor auth = %q, want cwd", ev.Actor.Auth)
	}
	if ev.Actor.Cwd != dir {
		t.Errorf("actor cwd = %q, want %q", ev.Actor.Cwd, dir)
	}
	if ev.Actor.ProjectID != "test-project" {
		t.Errorf("actor project = %q, want test-project", ev.Actor.ProjectID)
	}
}

func TestAudit_RecordsProtocolLevelToolError(t *testing.T) {
	mock := newMockConn("fsmcp", simpleTools("read_file"),
		okHandler(`{"isError":true,"content":[{"type":"text","text":"no such file"}]}`))
	r, rec := auditedRouter(t,
		map[string]config.Permission{"fsmcp": config.PermOn}, nil,
		map[string]*mockMcpConn{"fsmcp": mock}, nil)

	if _, err := r.CallTool(context.Background(), "read_file", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if !ev.ResultIsError {
		t.Error("result_is_error = false, want true for an isError result")
	}
	if ev.Outcome != audit.AuditOutcomeToolError {
		t.Errorf("outcome = %q, want %q", ev.Outcome, audit.AuditOutcomeToolError)
	}
}

func TestAudit_ToolErrorIsFilterable(t *testing.T) {
	mock := newMockConn("fsmcp", simpleTools("read_file", "list_dir"),
		func(_ context.Context, _ string, params interface{}) (json.RawMessage, error) {
			raw, err := json.Marshal(params)
			if err != nil {
				return nil, err
			}
			if bytes.Contains(raw, []byte(`"read_file"`)) {
				return json.RawMessage(`{"isError":true,"content":[{"type":"text","text":"path /etc/shadow is outside allowed directories"}]}`), nil
			}
			return json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`), nil
		})
	r, rec := auditedRouter(t,
		map[string]config.Permission{"fsmcp": config.PermOn}, nil,
		map[string]*mockMcpConn{"fsmcp": mock}, nil)

	if _, err := r.CallTool(context.Background(), "list_dir", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("CallTool(list_dir): %v", err)
	}
	if _, err := r.CallTool(context.Background(), "read_file", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("CallTool(read_file): %v", err)
	}

	// Record is asynchronous; drain the queue before querying.
	rec.Flush()

	got := rec.Query(audit.AuditQuery{Outcome: audit.AuditOutcomeToolError})
	if len(got) != 1 {
		t.Fatalf("outcome=tool_error matched %d events, want 1", len(got))
	}
	if got[0].Tool != "read_file" {
		t.Errorf("matched tool = %q, want read_file", got[0].Tool)
	}

	if ok := rec.Query(audit.AuditQuery{Outcome: audit.AuditOutcomeOK}); len(ok) != 1 || ok[0].Tool != "list_dir" {
		t.Errorf("outcome=ok matched %v, want exactly list_dir", ok)
	}
}

func TestAudit_ListEventsOffByDefault(t *testing.T) {
	mock := newMockConn("fsmcp", simpleTools("read_file"), nil)
	r, rec := auditedRouter(t,
		map[string]config.Permission{"fsmcp": config.PermOn}, nil,
		map[string]*mockMcpConn{"fsmcp": mock}, nil)

	if _, err := r.ListTools(context.Background(), testToken); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	rec.Flush()
	if n := rec.Wrote(); n != 0 {
		t.Errorf("recorded %d events with log_lists off, want 0", n)
	}
}

func TestAudit_ListEventsWhenEnabled(t *testing.T) {
	on := true
	mock := newMockConn("fsmcp", simpleTools("read_file", "write_file"), nil)
	r, rec := auditedRouter(t,
		map[string]config.Permission{"fsmcp": config.PermOn}, nil,
		map[string]*mockMcpConn{"fsmcp": mock},
		&config.AuditConfig{LogLists: &on})

	if _, err := r.ListTools(context.Background(), testToken); err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Event != audit.AuditEventListTools {
		t.Errorf("event = %q, want %q", ev.Event, audit.AuditEventListTools)
	}
	if ev.ToolCount != 2 {
		t.Errorf("tool_count = %d, want 2", ev.ToolCount)
	}
}

func TestAudit_NilRecorderIsInert(t *testing.T) {
	mock := newMockConn("fsmcp", simpleTools("read_file"), okHandler(`{}`))
	r := setupRouter(t, map[string]config.Permission{"fsmcp": config.PermOn}, nil, nil,
		map[string]*mockMcpConn{"fsmcp": mock})
	r.audit = nil

	if _, err := r.CallTool(context.Background(), "read_file", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("CallTool with no recorder: %v", err)
	}
	if _, err := r.ListTools(context.Background(), testToken); err != nil {
		t.Fatalf("ListTools with no recorder: %v", err)
	}
}

func TestAudit_ArgsOmittedWhenLogArgsDisabled(t *testing.T) {
	off := false
	mock := newMockConn("fsmcp", simpleTools("read_file"), okHandler(`{}`))
	r, rec := auditedRouter(t,
		map[string]config.Permission{"fsmcp": config.PermOn}, nil,
		map[string]*mockMcpConn{"fsmcp": mock},
		&config.AuditConfig{LogArgs: &off})

	if _, err := r.CallTool(context.Background(), "read_file", json.RawMessage(`{"path":"/secret"}`), testToken); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if len(ev.Args) != 0 {
		t.Errorf("args recorded despite log_args=false: %s", ev.Args)
	}
	if ev.Tool != "read_file" {
		t.Errorf("tool name should still be recorded, got %q", ev.Tool)
	}
}

func TestAudit_ResultPreviewOptIn(t *testing.T) {
	mock := newMockConn("fsmcp", simpleTools("read_file"), okHandler(`{"content":"hello world"}`))
	r, rec := auditedRouter(t,
		map[string]config.Permission{"fsmcp": config.PermOn}, nil,
		map[string]*mockMcpConn{"fsmcp": mock},
		&config.AuditConfig{MaxResultPreviewBytes: 8})

	if _, err := r.CallTool(context.Background(), "read_file", nil, testToken); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.ResultPreview == "" {
		t.Fatal("result preview not recorded despite opt-in")
	}
	if len(ev.ResultPreview) > 8 {
		t.Errorf("result preview = %q, longer than the configured cap", ev.ResultPreview)
	}
}

func TestAudit_CallerIdentityOverBridge(t *testing.T) {
	dir := mkSandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	if err := store.With(func(s *config.Settings) {
		s.ExternalMcps = append(s.ExternalMcps, config.ExternalMcp{ID: "audite2e", DisplayName: "Audit E2E"})
		s.Projects = append(s.Projects, config.Project{
			ID:            "audit-e2e",
			Name:          "audit-e2e",
			Path:          dir,
			AllowedMcpIDs: []string{"audite2e"},
			Token:         config.NewSecret(testToken),
			TokenHash:     config.HashToken(testToken),
		})
	}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	mgr := mcpbroker.NewManager(nil)
	addMockConn(mgr, "audite2e", newMockConn("audite2e", simpleTools("probe"), okHandler(`{"content":[]}`)))
	rec := newTestAudit(t, nil)
	r := &appRouter{
		store:    store,
		tools:    mgr,
		services: &fakeServiceReloader{},
		enhanced: NewEnhancedServiceRegistry(nil),
		onChange: func() {},
		audit:    rec,
	}

	bs, err := bridge.NewBridgeServer(context.Background(), r)
	if err != nil {
		t.Fatalf("NewBridgeServer: %v", err)
	}
	go bs.Serve()
	t.Cleanup(bs.Close)

	client := bridge.NewClient(testToken)
	if _, err := client.CallTool("probe", json.RawMessage(`{"q":1}`)); err != nil {
		t.Fatalf("CallTool over bridge: %v", err)
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Outcome != audit.AuditOutcomeOK {
		t.Fatalf("outcome = %q (%s)", ev.Outcome, ev.Error)
	}
	// Client and server are the same process here, so the peer pid is ours.
	if ev.Actor.PID != os.Getpid() {
		t.Errorf("actor pid = %d, want %d", ev.Actor.PID, os.Getpid())
	}
	if ev.Actor.Proc == "" {
		t.Error("actor process name not resolved from the peer pid")
	}
	if ev.Actor.ProjectID != "audit-e2e" {
		t.Errorf("actor project = %q, want audit-e2e", ev.Actor.ProjectID)
	}
}
