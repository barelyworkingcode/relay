package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"relaygo/bridge"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newTestAudit returns a recorder writing into a temp dir, closed on cleanup.
func newTestAudit(t *testing.T, cfg *AuditConfig) *AuditRecorder {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit", "toolcalls.jsonl")
	rec, err := NewAuditRecorder(cfg, path)
	if err != nil {
		t.Fatalf("NewAuditRecorder: %v", err)
	}
	if rec != nil {
		t.Cleanup(rec.Close)
	}
	return rec
}

// auditedRouter is setupRouter plus an attached recorder.
func auditedRouter(t *testing.T, perms map[string]Permission, disabled map[string][]string, mocks map[string]*mockMcpConn, cfg *AuditConfig) (*appRouter, *AuditRecorder) {
	t.Helper()
	r := setupRouter(t, perms, disabled, nil, mocks)
	rec := newTestAudit(t, cfg)
	r.audit = rec
	return r, rec
}

// readLoggedEvents flushes and parses every event written to the log file.
func readLoggedEvents(t *testing.T, rec *AuditRecorder) []AuditEvent {
	t.Helper()
	rec.Flush()
	data, err := os.ReadFile(rec.Path())
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	var out []AuditEvent
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var ev AuditEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("audit log line is not valid JSON: %v\nline: %s", err, line)
		}
		out = append(out, ev)
	}
	return out
}

func onlyEvent(t *testing.T, events []AuditEvent) AuditEvent {
	t.Helper()
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 audit event, got %d: %+v", len(events), events)
	}
	return events[0]
}

// ---------------------------------------------------------------------------
// Router instrumentation
// ---------------------------------------------------------------------------

func TestAudit_RecordsSuccessfulCall(t *testing.T) {
	mock := newMockConn("fsmcp", simpleTools("read_file"), okHandler(`{"content":[{"type":"text","text":"hi"}]}`))
	r, rec := auditedRouter(t,
		map[string]Permission{"fsmcp": PermOn}, nil,
		map[string]*mockMcpConn{"fsmcp": mock}, nil)

	args := json.RawMessage(`{"path":"/tmp/notes.md"}`)
	if _, err := r.CallTool(context.Background(), "read_file", args, testToken); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Event != AuditEventCallTool {
		t.Errorf("event = %q, want %q", ev.Event, AuditEventCallTool)
	}
	if ev.Outcome != AuditOutcomeOK {
		t.Errorf("outcome = %q, want ok (error=%q)", ev.Outcome, ev.Error)
	}
	if ev.Tool != "read_file" || ev.McpID != "fsmcp" {
		t.Errorf("tool/mcp = %q/%q, want read_file/fsmcp", ev.Tool, ev.McpID)
	}
	// The project id must come from relay's own auth resolution, not the caller.
	if ev.Actor.ProjectID != "test-project" || ev.Actor.ProjectName != "test" {
		t.Errorf("actor project = %q/%q, want test-project/test", ev.Actor.ProjectID, ev.Actor.ProjectName)
	}
	if ev.Actor.Kind != AuditActorProject || ev.Actor.Auth != AuditAuthToken {
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

// A refused call is the record a security review is actually looking for, so
// it must be logged just as reliably as a successful one.
func TestAudit_RecordsDeniedCall(t *testing.T) {
	mock := newMockConn("fsmcp", simpleTools("read_file", "fs_bash"), okHandler(`{}`))
	r, rec := auditedRouter(t,
		map[string]Permission{"fsmcp": PermOn},
		map[string][]string{"fsmcp": {"fs_bash"}},
		map[string]*mockMcpConn{"fsmcp": mock}, nil)

	if _, err := r.CallTool(context.Background(), "fs_bash", json.RawMessage(`{"cmd":"ls"}`), testToken); err == nil {
		t.Fatal("expected a denial for a disabled tool")
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Outcome != AuditOutcomeDenied {
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
		map[string]Permission{"fsmcp": PermOn}, nil,
		map[string]*mockMcpConn{"fsmcp": mock}, nil)

	if _, err := r.CallTool(context.Background(), "read_file", json.RawMessage(`{}`), "not-a-real-token"); err == nil {
		t.Fatal("expected an auth failure for an unknown token")
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Outcome != AuditOutcomeUnauthorized {
		t.Errorf("outcome = %q, want unauthorized", ev.Outcome)
	}
	if ev.Actor.Kind != AuditActorUnknown {
		t.Errorf("actor kind = %q, want unknown", ev.Actor.Kind)
	}
	// A credential was presented, it just didn't resolve. That distinction is
	// the point of recording the attempt at all.
	if ev.Actor.Auth != AuditAuthToken {
		t.Errorf("actor auth = %q, want token", ev.Actor.Auth)
	}
	if ev.Actor.ProjectID != "" {
		t.Errorf("unauthenticated call attributed to project %q", ev.Actor.ProjectID)
	}
}

func TestAudit_RecordsUnknownTool(t *testing.T) {
	r, rec := auditedRouter(t, map[string]Permission{"fsmcp": PermOn}, nil, nil, nil)

	if _, err := r.CallTool(context.Background(), "no_such_tool", nil, testToken); err == nil {
		t.Fatal("expected an error for an unknown tool")
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Outcome != AuditOutcomeError || ev.Tool != "no_such_tool" {
		t.Errorf("got outcome=%q tool=%q, want error/no_such_tool", ev.Outcome, ev.Tool)
	}
}

// Directory auth has no deliberate credential hand-off to point at, so the log
// is its only audit trail — it must record both the method and the directory.
func TestAudit_RecordsDirectoryAuth(t *testing.T) {
	dir := t.TempDir()

	// Opt the project into directory auth and root it at a real directory
	// before the router is built: the store hands out settings by value.
	settings := makeSettings(map[string]Permission{"fsmcp": PermOn}, nil, nil)
	settings.Projects[0].Path = dir
	settings.Projects[0].AllowCwdAuth = true

	mgr := NewExternalMcpManager(nil)
	addMockConn(mgr, "fsmcp", newMockConn("fsmcp", simpleTools("read_file"), okHandler(`{}`)))
	r := newTestRouter(t, settings, mgr)
	rec := newTestAudit(t, nil)
	r.audit = rec

	ctx := bridge.WithCallerCwd(context.Background(), dir)
	if _, err := r.CallTool(ctx, "read_file", json.RawMessage(`{}`), ""); err != nil {
		t.Fatalf("CallTool with directory auth: %v", err)
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Actor.Auth != AuditAuthCwd {
		t.Errorf("actor auth = %q, want cwd", ev.Actor.Auth)
	}
	if ev.Actor.Cwd != dir {
		t.Errorf("actor cwd = %q, want %q", ev.Actor.Cwd, dir)
	}
	if ev.Actor.ProjectID != "test-project" {
		t.Errorf("actor project = %q, want test-project", ev.Actor.ProjectID)
	}
}

// A tool that fails inside the protocol returns a normal result with
// isError set; without the probe it would be logged as a plain success.
func TestAudit_RecordsProtocolLevelToolError(t *testing.T) {
	mock := newMockConn("fsmcp", simpleTools("read_file"),
		okHandler(`{"isError":true,"content":[{"type":"text","text":"no such file"}]}`))
	r, rec := auditedRouter(t,
		map[string]Permission{"fsmcp": PermOn}, nil,
		map[string]*mockMcpConn{"fsmcp": mock}, nil)

	if _, err := r.CallTool(context.Background(), "read_file", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if !ev.ResultIsError {
		t.Error("result_is_error = false, want true for an isError result")
	}
	// The flag alone is not enough: the outcome is what `relay audit --outcome`
	// and the Tool Calls filter select on, so a refusal recorded as "ok" is
	// invisible to every query an operator would actually run.
	if ev.Outcome != AuditOutcomeToolError {
		t.Errorf("outcome = %q, want %q", ev.Outcome, AuditOutcomeToolError)
	}
}

// An in-protocol refusal must be reachable by the query an operator runs, not
// merely recoverable by post-processing raw JSONL for result_is_error.
func TestAudit_ToolErrorIsFilterable(t *testing.T) {
	// Branch on the tool named in the request params: read_file refuses
	// in-protocol, list_dir succeeds, so one call of each lands in the log.
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
		map[string]Permission{"fsmcp": PermOn}, nil,
		map[string]*mockMcpConn{"fsmcp": mock}, nil)

	if _, err := r.CallTool(context.Background(), "list_dir", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("CallTool(list_dir): %v", err)
	}
	if _, err := r.CallTool(context.Background(), "read_file", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("CallTool(read_file): %v", err)
	}

	// Record is asynchronous; drain the queue before querying the ring.
	rec.Flush()

	got := rec.Query(AuditQuery{Outcome: AuditOutcomeToolError})
	if len(got) != 1 {
		t.Fatalf("outcome=tool_error matched %d events, want 1", len(got))
	}
	if got[0].Tool != "read_file" {
		t.Errorf("matched tool = %q, want read_file", got[0].Tool)
	}

	// The successful call must not be swept up by the new outcome.
	if ok := rec.Query(AuditQuery{Outcome: AuditOutcomeOK}); len(ok) != 1 || ok[0].Tool != "list_dir" {
		t.Errorf("outcome=ok matched %v, want exactly list_dir", ok)
	}
}

// List events are off by default: skill regeneration lists the tool surface for
// every project on every reconcile, which would bury the calls that matter.
func TestAudit_ListEventsOffByDefault(t *testing.T) {
	mock := newMockConn("fsmcp", simpleTools("read_file"), nil)
	r, rec := auditedRouter(t,
		map[string]Permission{"fsmcp": PermOn}, nil,
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
		map[string]Permission{"fsmcp": PermOn}, nil,
		map[string]*mockMcpConn{"fsmcp": mock},
		&AuditConfig{LogLists: &on})

	if _, err := r.ListTools(context.Background(), testToken); err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Event != AuditEventListTools {
		t.Errorf("event = %q, want %q", ev.Event, AuditEventListTools)
	}
	if ev.ToolCount != 2 {
		t.Errorf("tool_count = %d, want 2", ev.ToolCount)
	}
}

// A router with no recorder must behave exactly as it did before auditing
// existed — the nil-safe helpers are what make the instrumentation free.
func TestAudit_NilRecorderIsInert(t *testing.T) {
	mock := newMockConn("fsmcp", simpleTools("read_file"), okHandler(`{}`))
	r := setupRouter(t, map[string]Permission{"fsmcp": PermOn}, nil, nil,
		map[string]*mockMcpConn{"fsmcp": mock})
	r.audit = nil

	if _, err := r.CallTool(context.Background(), "read_file", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("CallTool with no recorder: %v", err)
	}
	if _, err := r.ListTools(context.Background(), testToken); err != nil {
		t.Fatalf("ListTools with no recorder: %v", err)
	}
}

func TestAudit_DisabledConfigYieldsNoRecorder(t *testing.T) {
	off := false
	rec := newTestAudit(t, &AuditConfig{Enabled: &off})
	if rec != nil {
		t.Fatal("disabled config produced a recorder")
	}
	if rec.Enabled() {
		t.Error("nil recorder reports Enabled")
	}
}

// ---------------------------------------------------------------------------
// Redaction and truncation
// ---------------------------------------------------------------------------

func TestRedactArgs_RedactsCredentialKeys(t *testing.T) {
	in := json.RawMessage(`{
		"path": "/tmp/x",
		"api_key": "sk-live-1234",
		"nested": {"Authorization": "Bearer abc", "keep": 1},
		"list": [{"password": "hunter2"}, {"ok": true}]
	}`)
	out, _, truncated := redactArgs(in, 4096, nil)
	if truncated {
		t.Fatal("small args were reported as truncated")
	}
	s := string(out)
	for _, secret := range []string{"sk-live-1234", "Bearer abc", "hunter2"} {
		if strings.Contains(s, secret) {
			t.Errorf("redacted output still contains %q: %s", secret, s)
		}
	}
	// Non-credential values must survive, or the log stops being useful.
	if !strings.Contains(s, "/tmp/x") || !strings.Contains(s, `"keep":1`) {
		t.Errorf("redaction removed non-credential values: %s", s)
	}
}

func TestRedactArgs_HonorsExtraKeys(t *testing.T) {
	in := json.RawMessage(`{"patient_name":"Jane","path":"/tmp/x"}`)
	out, _, _ := redactArgs(in, 4096, []string{"patient"})
	if strings.Contains(string(out), "Jane") {
		t.Errorf("configured redact key was ignored: %s", out)
	}
	if !strings.Contains(string(out), "/tmp/x") {
		t.Errorf("configured redact key over-matched: %s", out)
	}
}

func TestRedactArgs_TruncatesOversizedArgs(t *testing.T) {
	big, err := json.Marshal(map[string]string{"blob": strings.Repeat("x", 500)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, size, truncated := redactArgs(big, 100, nil)
	if !truncated {
		t.Fatal("oversized args were not flagged as truncated")
	}
	if size != len(big) {
		t.Errorf("recorded size = %d, want the original %d", size, len(big))
	}
	// Truncated or not, every line in the log must parse.
	var v interface{}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("truncated args are not valid JSON: %v (%s)", err, out)
	}
	if _, ok := v.(string); !ok {
		t.Errorf("truncated args should be stored as a JSON string, got %T", v)
	}
}

func TestRedactArgs_MalformedJSONIsStoredAsText(t *testing.T) {
	out, _, _ := redactArgs(json.RawMessage(`{not json`), 4096, nil)
	var v interface{}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("malformed args produced an unparseable record: %v", err)
	}
	if s, ok := v.(string); !ok || !strings.Contains(s, "not json") {
		t.Errorf("malformed args lost their content: %v", v)
	}
}

func TestTruncateRunes_DoesNotSplitMultibyte(t *testing.T) {
	// "é" is two bytes; cutting at 3 must drop it rather than halve it.
	got := truncateRunes("aéb", 3)
	if got != "aé" {
		t.Errorf("truncateRunes = %q, want %q", got, "aé")
	}
	if !json.Valid(mustJSON(t, got)) {
		t.Error("truncated string does not encode as valid JSON")
	}
}

// ---------------------------------------------------------------------------
// Ring, queue, and query
// ---------------------------------------------------------------------------

func TestAuditRing_EvictsOldestAndReturnsNewestFirst(t *testing.T) {
	ring := newAuditRing(3)
	for _, id := range []string{"a", "b", "c", "d"} {
		ring.add(AuditEvent{ID: id})
	}
	got := ring.snapshot()
	want := []string{"d", "c", "b"}
	if len(got) != len(want) {
		t.Fatalf("snapshot len = %d, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("snapshot[%d] = %q, want %q", i, got[i].ID, id)
		}
	}
}

func TestAuditRing_PartiallyFilled(t *testing.T) {
	ring := newAuditRing(5)
	ring.add(AuditEvent{ID: "a"})
	ring.add(AuditEvent{ID: "b"})
	got := ring.snapshot()
	if len(got) != 2 || got[0].ID != "b" || got[1].ID != "a" {
		t.Fatalf("snapshot = %+v, want [b a]", got)
	}
}

func TestAuditQuery_FiltersRing(t *testing.T) {
	rec := newTestAudit(t, nil)
	rec.Record(AuditEvent{ID: "1", Event: AuditEventCallTool, McpID: "fsmcp", Tool: "read_file", Outcome: AuditOutcomeOK,
		Actor: AuditActor{ProjectID: "p1", ProjectName: "alpha"}})
	rec.Record(AuditEvent{ID: "2", Event: AuditEventCallTool, McpID: "macmcp", Tool: "send_mail", Outcome: AuditOutcomeDenied,
		Actor: AuditActor{ProjectID: "p2", ProjectName: "beta"}})
	rec.Flush()

	if got := rec.Query(AuditQuery{ProjectID: "p1"}); len(got) != 1 || got[0].ID != "1" {
		t.Errorf("project filter returned %+v", got)
	}
	if got := rec.Query(AuditQuery{Outcome: AuditOutcomeDenied}); len(got) != 1 || got[0].ID != "2" {
		t.Errorf("outcome filter returned %+v", got)
	}
	if got := rec.Query(AuditQuery{McpID: "macmcp"}); len(got) != 1 || got[0].ID != "2" {
		t.Errorf("mcp filter returned %+v", got)
	}
	if got := rec.Query(AuditQuery{Text: "READ_FILE"}); len(got) != 1 || got[0].ID != "1" {
		t.Errorf("text filter should be case-insensitive, returned %+v", got)
	}
	if got := rec.Query(AuditQuery{Limit: 1}); len(got) != 1 {
		t.Errorf("limit ignored, returned %d events", len(got))
	}
	if got := rec.Query(AuditQuery{}); len(got) != 2 {
		t.Errorf("empty query returned %d events, want 2", len(got))
	}
}

// The deep path answers from the file, so it still works for history the ring
// has already evicted.
func TestAuditQuery_DeepReadsBeyondTheRing(t *testing.T) {
	rec := newTestAudit(t, &AuditConfig{RingSize: 2})
	for _, id := range []string{"1", "2", "3", "4"} {
		rec.Record(AuditEvent{ID: id, Event: AuditEventCallTool, Tool: "t" + id, Outcome: AuditOutcomeOK})
	}
	rec.Flush()

	if got := rec.Query(AuditQuery{Text: "t1"}); len(got) != 0 {
		t.Errorf("ring query found an evicted event: %+v", got)
	}
	got := rec.Query(AuditQuery{Text: "t1", Deep: true})
	if len(got) != 1 || got[0].ID != "1" {
		t.Errorf("deep query = %+v, want the evicted event 1", got)
	}
}

// The sink must never be able to stall a tool call: past the queue bound,
// events are dropped and counted rather than made to wait.
func TestAuditRecorder_DropsRatherThanBlocks(t *testing.T) {
	rec := newTestAudit(t, nil)
	// Wedge the writer goroutine so the queue can actually fill.
	var wg sync.WaitGroup
	wg.Add(1)
	blocked := make(chan struct{})
	rec.SetSink(func(AuditEvent) {
		close(blocked)
		wg.Wait()
	})
	rec.Record(AuditEvent{ID: "wedge"})
	<-blocked

	for i := 0; i < auditQueueSize+50; i++ {
		rec.Record(AuditEvent{ID: "flood"})
	}
	if rec.Dropped() == 0 {
		t.Error("a full queue did not drop any events")
	}
	rec.SetSink(nil)
	wg.Done()
}

func TestAuditRecorder_ConcurrentRecord(t *testing.T) {
	rec := newTestAudit(t, nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				rec.Record(AuditEvent{ID: "x", Event: AuditEventCallTool, Outcome: AuditOutcomeOK})
			}
		}()
	}
	wg.Wait()
	rec.Flush()
	// Concurrent readers must not race the writer.
	_ = rec.Query(AuditQuery{Limit: 10})
	if rec.Wrote()+rec.Dropped() != 160 {
		t.Errorf("wrote %d + dropped %d != 160", rec.Wrote(), rec.Dropped())
	}
}

// ---------------------------------------------------------------------------
// Config defaults
// ---------------------------------------------------------------------------

// An install predating this feature has no audit block at all and must start
// logging without a settings.json migration.
func TestAuditConfig_NilResolvesToEnabledDefaults(t *testing.T) {
	var cfg *AuditConfig
	got := cfg.resolve()
	if !got.Enabled || !got.LogArgs {
		t.Errorf("nil config resolved to enabled=%v log_args=%v, want both true", got.Enabled, got.LogArgs)
	}
	if got.LogLists {
		t.Error("list events should default off")
	}
	if got.MaxResultPreviewBytes != 0 {
		t.Errorf("result preview defaults to %d, want 0 (metadata only)", got.MaxResultPreviewBytes)
	}
	if got.MaxArgBytes != auditDefaultMaxArgBytes || got.RingSize != auditDefaultRingSize {
		t.Errorf("size defaults not applied: %+v", got)
	}
	if got.Generations != auditDefaultGenerations || got.MaxFileBytes != auditDefaultMaxFileBytes {
		t.Errorf("rotation defaults not applied: %+v", got)
	}
}

func TestAuditConfig_ExplicitFalseIsHonored(t *testing.T) {
	off := false
	got := (&AuditConfig{LogArgs: &off}).resolve()
	if got.LogArgs {
		t.Error("explicit log_args=false was overridden by the default")
	}
	if !got.Enabled {
		t.Error("unrelated field lost its default")
	}
}

func TestAudit_ArgsOmittedWhenLogArgsDisabled(t *testing.T) {
	off := false
	mock := newMockConn("fsmcp", simpleTools("read_file"), okHandler(`{}`))
	r, rec := auditedRouter(t,
		map[string]Permission{"fsmcp": PermOn}, nil,
		map[string]*mockMcpConn{"fsmcp": mock},
		&AuditConfig{LogArgs: &off})

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
		map[string]Permission{"fsmcp": PermOn}, nil,
		map[string]*mockMcpConn{"fsmcp": mock},
		&AuditConfig{MaxResultPreviewBytes: 8})

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

// ---------------------------------------------------------------------------
// End-to-end over a real bridge socket
// ---------------------------------------------------------------------------

// The caller attribution is only worth anything if it survives the real
// transport: the pid is read off the socket by the bridge server, not passed in
// by the caller, so it can only be verified end to end.
func TestAudit_CallerIdentityOverBridge(t *testing.T) {
	dir := mkSandboxRelayHome(t)
	store := NewSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	if err := store.With(func(s *Settings) {
		s.ExternalMcps = append(s.ExternalMcps, ExternalMcp{ID: "audite2e", DisplayName: "Audit E2E"})
		s.Projects = append(s.Projects, Project{
			ID:            "audit-e2e",
			Name:          "audit-e2e",
			Path:          dir,
			AllowedMcpIDs: []string{"audite2e"},
			Token:         testToken,
			TokenHash:     hashToken(testToken),
		})
	}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	mgr := NewExternalMcpManager(nil)
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
	if ev.Outcome != AuditOutcomeOK {
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
