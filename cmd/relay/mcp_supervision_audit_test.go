//go:build !windows

package main

// The ADR-012 liveness records are written by main, not by the broker: the
// supervisor reports a transition, and mcpSupervisionEvent (audit_call.go)
// is what turns one into an mcp_down/mcp_up row and `relay audit` is what
// renders it. That whole span is what this test measures, which is why it
// stays here while the rest of the supervision tests moved into
// internal/mcpbroker with the supervisor.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
)

func TestSupervisor_DeathAndRecoveryAreAudited(t *testing.T) {
	bin := buildTestMcpBinary(t)
	dir := t.TempDir()
	rec, err := audit.NewAuditRecorder(&config.AuditConfig{}, dir+"/audit.jsonl", openAuditWriter)
	if err != nil {
		t.Fatalf("NewAuditRecorder: %v", err)
	}
	t.Cleanup(rec.Close)

	m := mcpbroker.NewManager(nil)
	t.Cleanup(m.StopAll)

	// Wait on the observer, not on the connection: the restart is published
	// before its health event is reported, so watching the connection table
	// would race the record this test is about.
	restarted := make(chan struct{})
	var once sync.Once
	m.SetHealthObserver(func(ev mcpbroker.HealthEvent) {
		recordMcpSupervision(rec, ev)
		if ev.State == mcpbroker.HealthRestarted {
			once.Do(func() { close(restarted) })
		}
	})

	cfg := config.ExternalMcp{ID: "mcp-audited", DisplayName: "mcp-audited", Transport: "stdio", Command: bin}
	m.StartAll(context.Background(), []config.ExternalMcp{cfg})
	if !m.IsConnected("mcp-audited") {
		t.Fatal("the child never came up")
	}
	first := m.ConnectionForTest("mcp-audited")
	if _, err := first.SendRequest(context.Background(), "exit", nil); err == nil {
		t.Fatal("expected the in-flight call to fail when the child exits")
	}
	select {
	case <-restarted:
	case <-time.After(15 * time.Second):
		t.Fatal("the child was never restarted")
	}
	rec.Flush()

	events := rec.Query(audit.AuditQuery{McpID: "mcp-audited"})
	var down, up *audit.AuditEvent
	for i := range events {
		switch events[i].Event {
		case audit.AuditEventMcpDown:
			down = &events[i]
		case audit.AuditEventMcpUp:
			up = &events[i]
		}
	}
	if down == nil {
		t.Fatalf("no mcp_down record for a child that died; got %d events", len(events))
	}
	if up == nil {
		t.Fatalf("no mcp_up record for a child that came back; got %d events", len(events))
	}
	if down.Outcome != audit.AuditOutcomeError || down.Supervision != mcpbroker.HealthDown {
		t.Errorf("mcp_down record = outcome %q supervision %q", down.Outcome, down.Supervision)
	}
	if down.Error == "" {
		t.Error("mcp_down record names no cause")
	}
	if up.Outcome != audit.AuditOutcomeOK || up.Supervision != mcpbroker.HealthRestarted {
		t.Errorf("mcp_up record = outcome %q supervision %q", up.Outcome, up.Supervision)
	}
	for _, ev := range []*audit.AuditEvent{down, up} {
		if ev.Actor.Kind != audit.AuditActorRelay {
			t.Errorf("%s actor kind = %q, want %q", ev.Event, ev.Actor.Kind, audit.AuditActorRelay)
		}
		if ev.Actor.ProjectID != "" {
			t.Errorf("%s attributes a project (%q) to a record about relay itself", ev.Event, ev.Actor.ProjectID)
		}
	}
	// The operator's table has no EVENT column, so the transition must survive
	// into the DETAIL cell or the row says nothing at all.
	if got := auditDetail(*down); !strings.HasPrefix(got, mcpbroker.HealthDown+": ") {
		t.Errorf("mcp_down detail = %q, want it to lead with the transition", got)
	}
	if got := auditDetail(*up); got != mcpbroker.HealthRestarted {
		t.Errorf("mcp_up detail = %q, want %q", got, mcpbroker.HealthRestarted)
	}
}
