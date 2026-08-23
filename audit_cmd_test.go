package main

// The gap this file closes: access, scope, allow_external and scope_violation
// were recorded on every AuditEvent (ADR-011 decision 7) but reachable only
// through --json and a grep. `relay audit`'s table is the thing a human
// actually reads, and these tests pin what it now shows without breaking the
// shape a script parsing the eight existing columns already depends on.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

// ---------------------------------------------------------------------------
// auditDetail: the scope_violation marker
// ---------------------------------------------------------------------------

func TestAuditDetail_MarksScopeViolationEvenWithNoOtherDetail(t *testing.T) {
	ev := AuditEvent{Outcome: AuditOutcomeToolError, ScopeViolation: true}
	if got := auditDetail(ev); got != "scope_violation: true" {
		t.Errorf("auditDetail = %q, want a bare scope_violation marker", got)
	}
}

func TestAuditDetail_ScopeViolationMarkerLeadsExistingDetail(t *testing.T) {
	ev := AuditEvent{Outcome: AuditOutcomeToolError, ScopeViolation: true, Error: "no such mailbox"}
	got := auditDetail(ev)
	if !strings.HasPrefix(got, "scope_violation: true") {
		t.Errorf("auditDetail = %q, want the marker first", got)
	}
	if !strings.Contains(got, "no such mailbox") {
		t.Errorf("auditDetail = %q, want the underlying detail preserved", got)
	}
}

// An ordinary tool_error — the overwhelming majority of them — must render
// exactly as it did before this feature existed: nothing here may make a
// non-violation look like one.
func TestAuditDetail_OrdinaryToolErrorIsUnmarked(t *testing.T) {
	ev := AuditEvent{Outcome: AuditOutcomeToolError, Args: json.RawMessage(`{"path":"/tmp/x"}`)}
	got := auditDetail(ev)
	if strings.Contains(got, "scope_violation") {
		t.Errorf("auditDetail = %q, an ordinary tool_error must not mention scope_violation", got)
	}
	if got != `{"path":"/tmp/x"}` {
		t.Errorf("auditDetail = %q, want the unmodified args", got)
	}
}

func TestAuditDetail_DeniedRecordShowsWhichLayerRefused(t *testing.T) {
	// This is the layer-naming the CLI is asked to preserve: relay's own
	// refusal messages already say which check fired, and auditDetail must
	// not truncate or otherwise obscure that sentence.
	ev := AuditEvent{Outcome: AuditOutcomeDenied,
		Error: "access denied: tool 'capture_screenshot' is not in the allowed tools for MCP 'macmcp'"}
	got := auditDetail(ev)
	if got != "access denied: tool 'capture_screenshot' is not in the allowed tools for MCP 'macmcp'" {
		t.Errorf("auditDetail = %q, want the refusal message verbatim", got)
	}
}

// ---------------------------------------------------------------------------
// auditScopeSummary / auditAuthorityLine
// ---------------------------------------------------------------------------

func TestAuditScopeSummary_AbsentEmptyAndPopulatedReadDifferently(t *testing.T) {
	if got := auditScopeSummary(nil); got != "(none declared)" {
		t.Errorf("nil scope = %q", got)
	}
	if got := auditScopeSummary(map[string]json.RawMessage{}); got != "(declared, none injected)" {
		t.Errorf("empty scope = %q", got)
	}
	got := auditScopeSummary(map[string]json.RawMessage{
		"mail_mailboxes": json.RawMessage(`["INBOX"]`),
		"mail_accounts":  json.RawMessage(`["Bob"]`),
	})
	if got != `mail_accounts=["Bob"],mail_mailboxes=["INBOX"]` {
		t.Errorf("populated scope = %q, want stable key-sorted order", got)
	}
}

func TestAuditAuthorityLine_OmittedWhenNothingWasRecorded(t *testing.T) {
	// A service token, a list event, or a refusal before an MCP resolved: none
	// of these ever reach setAuthority, so Access stays "".
	if _, ok := auditAuthorityLine(AuditEvent{}); ok {
		t.Error("authority line rendered for a record with no recorded authority")
	}
}

func TestAuditAuthorityLine_RendersModeOutboundAndScope(t *testing.T) {
	ev := AuditEvent{Access: AccessRead, AllowExternal: boolPtr(false),
		Scope: map[string]json.RawMessage{"mail_accounts": json.RawMessage(`["Bob"]`)}}
	line, ok := auditAuthorityLine(ev)
	if !ok {
		t.Fatal("authority line was omitted for a record that carried authority")
	}
	for _, want := range []string{"access=read", "outbound=blocked", `scope=mail_accounts=["Bob"]`} {
		if !strings.Contains(line, want) {
			t.Errorf("authority line = %q, missing %q", line, want)
		}
	}
}

func TestAuditAuthorityLine_AllowExternalNilIsNotApplicable(t *testing.T) {
	// AllowExternal is a pointer specifically so "not recorded" and "recorded
	// false" don't collide; the CLI must keep that distinction visible too.
	ev := AuditEvent{Access: AccessWrite}
	line, ok := auditAuthorityLine(ev)
	if !ok {
		t.Fatal("authority line omitted")
	}
	if !strings.Contains(line, "outbound=n/a") {
		t.Errorf("authority line = %q, want outbound=n/a for a nil AllowExternal", line)
	}
}

// ---------------------------------------------------------------------------
// writeAuditTable: default shape is unchanged; --authority is additive
// ---------------------------------------------------------------------------

func TestWriteAuditTable_DefaultShapeUnchanged(t *testing.T) {
	events := []AuditEvent{
		{Outcome: AuditOutcomeDenied, Tool: "capture_screenshot", McpID: "macmcp",
			Access: AccessRead, AllowExternal: boolPtr(false),
			Error: "access denied: tool 'capture_screenshot' is not in the allowed tools for MCP 'macmcp'"},
	}
	var buf bytes.Buffer
	writeAuditTable(&buf, events, false)
	out := buf.String()

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("without --authority, got %d lines, want 2 (header + one row):\n%s", len(lines), out)
	}
	if !strings.HasPrefix(lines[0], "TIME\tOUTCOME\tPROJECT\tMCP\tTOOL\tMS\tCALLER\tDETAIL") {
		t.Errorf("header changed: %q", lines[0])
	}
	if strings.Contains(out, "authority:") {
		t.Errorf("authority line leaked into default output:\n%s", out)
	}
}

func TestWriteAuditTable_AuthorityFlagAddsALinePerConfinedCall(t *testing.T) {
	events := []AuditEvent{
		{Outcome: AuditOutcomeDenied, Tool: "capture_screenshot", McpID: "macmcp",
			Access: AccessRead, AllowExternal: boolPtr(false),
			Error: "access denied: tool 'capture_screenshot' is not in the allowed tools for MCP 'macmcp'"},
	}
	var buf bytes.Buffer
	writeAuditTable(&buf, events, true)
	out := buf.String()

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("with --authority, got %d lines, want 3 (header + row + authority line):\n%s", len(lines), out)
	}
	if !strings.Contains(lines[2], "authority: access=read") {
		t.Errorf("authority line = %q", lines[2])
	}
}

// A service-token call carries no authority at all, and --authority must not
// invent placeholders for it.
func TestWriteAuditTable_AuthorityFlagSkipsRecordsWithNoAuthority(t *testing.T) {
	events := []AuditEvent{
		{Outcome: AuditOutcomeOK, Tool: "list_services", Actor: AuditActor{Kind: AuditActorService}},
	}
	var buf bytes.Buffer
	writeAuditTable(&buf, events, true)
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Errorf("service-token call grew an authority line it has nothing to fill in: %q", lines)
	}
}
