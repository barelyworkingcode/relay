package audit

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
)

// testOpenWriter is a plain append-only file, not a rotating one: these tests
// exercise the recorder engine, not log rotation (cmd/relay owns that and
// tests it in its own package).
func testOpenWriter(path string, maxBytes int64, generations int) (io.WriteCloser, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}

func newTestAudit(t *testing.T, cfg *config.AuditConfig) *AuditRecorder {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit", "toolcalls.jsonl")
	rec, err := NewAuditRecorder(cfg, path, testOpenWriter)
	if err != nil {
		t.Fatalf("NewAuditRecorder: %v", err)
	}
	if rec != nil {
		t.Cleanup(rec.Close)
	}
	return rec
}

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

func TestAudit_DisabledConfigYieldsNoRecorder(t *testing.T) {
	off := false
	rec := newTestAudit(t, &config.AuditConfig{Enabled: &off})
	if rec != nil {
		t.Fatal("disabled config produced a recorder")
	}
	if rec.Enabled() {
		t.Error("nil recorder reports Enabled")
	}
}

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

func TestAuditQuery_ScopeViolationFiltersOnTheFieldNotTheOutcome(t *testing.T) {
	rec := newTestAudit(t, nil)
	rec.Record(AuditEvent{ID: "1", Event: AuditEventCallTool, Tool: "mail_get_email",
		Outcome: AuditOutcomeToolError, ScopeViolation: true})
	rec.Record(AuditEvent{ID: "2", Event: AuditEventCallTool, Tool: "fs_read",
		Outcome: AuditOutcomeToolError, ScopeViolation: false})
	rec.Record(AuditEvent{ID: "3", Event: AuditEventCallTool, Tool: "mail_search",
		Outcome: AuditOutcomeOK})
	rec.Flush()

	if got := rec.Query(AuditQuery{Outcome: "scope_violation"}); len(got) != 1 || got[0].ID != "1" {
		t.Errorf("scope_violation filter = %+v, want only event 1", got)
	}
	if got := rec.Query(AuditQuery{Outcome: AuditOutcomeToolError}); len(got) != 2 {
		t.Errorf("tool_error filter returned %d events, want both tool_error records unaffected", len(got))
	}
	if got := rec.Query(AuditQuery{Outcome: AuditOutcomeOK}); len(got) != 1 || got[0].ID != "3" {
		t.Errorf("ok filter = %+v, want only event 3 — existing outcomes must be untouched", got)
	}
}

func TestAuditQuery_DeepReadsBeyondTheRing(t *testing.T) {
	rec := newTestAudit(t, &config.AuditConfig{RingSize: 2})
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

	for i := 0; i < AuditQueueSize+50; i++ {
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
	_ = rec.Query(AuditQuery{Limit: 10})
	if rec.Wrote()+rec.Dropped() != 160 {
		t.Errorf("wrote %d + dropped %d != 160", rec.Wrote(), rec.Dropped())
	}
}

func TestAuditConfig_NilResolvesToEnabledDefaults(t *testing.T) {
	var cfg *config.AuditConfig
	got := ResolveAuditConfig(cfg)
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
	got := ResolveAuditConfig(&config.AuditConfig{LogArgs: &off})
	if got.LogArgs {
		t.Error("explicit log_args=false was overridden by the default")
	}
	if !got.Enabled {
		t.Error("unrelated field lost its default")
	}
}
