package main

import (
	"context"
	"encoding/json"
	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
	"strings"
	"testing"
)

// What relay holds when an MCP's schema changed, downgraded, rebuilt under a
// different binary, or failed to publish.
const quietV2Schema = `{}`

func unplaceableScopeRouter(t *testing.T, schema string, version int, captured *json.RawMessage) *appRouter {
	t.Helper()
	capture := func(_ context.Context, _ string, params interface{}) (json.RawMessage, error) {
		if m := decodedToolParams(params); m != nil {
			if raw, err := json.Marshal(m["_meta"]); err == nil {
				*captured = raw
			}
		}
		return json.RawMessage(`{"content":[{"type":"text","text":"served"}]}`), nil
	}
	proj := config.Project{
		ID: "probe", Name: "probe", Kind: config.ProjectKindRemote,
		AllowedMcpIDs: []string{"macmcp"}, Token: config.NewSecret(testToken), TokenHash: config.HashToken(testToken),
		AllowedTools: map[string][]string{"macmcp": {"mail_*"}},
		Access:       map[string]string{"macmcp": config.AccessWrite},
		Context: map[string]json.RawMessage{
			"macmcp": json.RawMessage(`{"allowed_dirs":["/Users/admin/project"]}`),
		},
	}
	s := &config.Settings{
		Version: 1, ExternalMcps: []config.ExternalMcp{{ID: "macmcp", DisplayName: "macMCP"}},
		Projects: []config.Project{proj}, AdminSecret: config.NewSecret("supersecretadmin"),
	}
	mgr := mcpbroker.NewManager(nil)
	addMockConn(mgr, "macmcp", newMockConn("macmcp", macmcpToolSurface(), capture))
	addMockSchema(mgr, "macmcp", schema, version)
	return newTestRouter(t, s, mgr)
}

// Deliberate: a scope the operator wrote that relay cannot place in the live
// schema gets the same fail-closed answer as a schema relay cannot read —
// relay must not rely on the MCP's own fail-closed behavior.
func TestCallTool_DeniesAScopeTheLiveSchemaCannotPlace(t *testing.T) {
	var reached json.RawMessage
	r := unplaceableScopeRouter(t, quietV2Schema, 2, &reached)
	rec := newTestAudit(t, nil)
	r.audit = rec

	_, err := r.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken)
	if err == nil {
		t.Fatal("a call whose operator-configured scope relay could not place was dispatched")
	}
	if !strings.Contains(err.Error(), "allowed_dirs") || !strings.Contains(err.Error(), "macmcp") {
		t.Errorf("the refusal names neither the field nor the MCP: %v", err)
	}
	if reached != nil {
		t.Errorf("the MCP was invoked anyway, with _meta = %s", reached)
	}

	events := readLoggedEvents(t, rec)
	if len(events) != 1 {
		t.Fatalf("expected one record, got %d", len(events))
	}
	if events[0].Outcome != audit.AuditOutcomeDenied {
		t.Errorf("recorded as %q, want %q", events[0].Outcome, audit.AuditOutcomeDenied)
	}
}

func TestCallTool_APlaceableScopeIsUnaffected(t *testing.T) {
	const declares = `{
	  "allowed_dirs": {"type":"array","description":"Dirs","scope":"restrict","source":"operator"}
	}`
	var reached json.RawMessage
	r := unplaceableScopeRouter(t, declares, 2, &reached)
	if _, err := r.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("a grant relay CAN place was refused: %v", err)
	}
	if !strings.Contains(string(reached), "allowed_dirs") {
		t.Errorf("the operator's scope did not reach the MCP: %s", reached)
	}
}

// Subtle: an empty value is absent everywhere else in this model
// (project.HasScopeValue, checkScopePresence, the UI) — denying here would turn a
// harmless remnant into an outage.
func TestCallTool_AnEmptyStoredKeyIsNotAnUnplaceableScope(t *testing.T) {
	for _, empty := range []string{`[]`, `null`, `""`, `{}`} {
		proj := config.Project{
			ID: "probe", Name: "probe", Kind: config.ProjectKindRemote,
			AllowedMcpIDs: []string{"macmcp"}, Token: config.NewSecret(testToken), TokenHash: config.HashToken(testToken),
			AllowedTools: map[string][]string{"macmcp": {"mail_*"}},
			Access:       map[string]string{"macmcp": config.AccessWrite},
			Context: map[string]json.RawMessage{
				"macmcp": json.RawMessage(`{"gone_field":` + empty + `}`),
			},
		}
		s := &config.Settings{
			Version: 1, ExternalMcps: []config.ExternalMcp{{ID: "macmcp", DisplayName: "macMCP"}},
			Projects: []config.Project{proj}, AdminSecret: config.NewSecret("supersecretadmin"),
		}
		mgr := mcpbroker.NewManager(nil)
		serve := func(_ context.Context, _ string, _ interface{}) (json.RawMessage, error) {
			return json.RawMessage(`{"content":[{"type":"text","text":"served"}]}`), nil
		}
		addMockConn(mgr, "macmcp", newMockConn("macmcp", macmcpToolSurface(), serve))
		addMockSchema(mgr, "macmcp", quietV2Schema, 2)
		r := newTestRouter(t, s, mgr)
		if _, err := r.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken); err != nil {
			t.Errorf("an empty stored value %s was treated as an undeliverable scope: %v", empty, err)
		}
	}
}

// Deliberate: only the v2 branch drops (project.FilterKnownContextFields returns a v1
// blob verbatim) — widening the refusal to v1 would break the "handled
// exactly as before ADR-011" promise for MCPs with no contextSchema.
func TestCallTool_AV1SchemaIsUnaffectedByThePlacementCheck(t *testing.T) {
	var reached json.RawMessage
	r := unplaceableScopeRouter(t, "", 0, &reached)
	if _, err := r.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("a v1 MCP was refused: %v", err)
	}
	if !strings.Contains(string(reached), "allowed_dirs") {
		t.Errorf("a v1 call no longer carries the stored context verbatim: %s", reached)
	}
}

// Deliberate: "scope=(none declared)" must not also be what an unconfined
// dispatch produces — those are different facts and must not share a string.
func TestAudit_UnplacedScopeDoesNotShareAStringWithNoneDeclared(t *testing.T) {
	var reached json.RawMessage
	r := unplaceableScopeRouter(t, quietV2Schema, 2, &reached)
	rec := newTestAudit(t, nil)
	r.audit = rec
	if _, err := r.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken); err == nil {
		t.Fatal("precondition: the call should have been denied")
	}
	events := readLoggedEvents(t, rec)
	if len(events) != 1 {
		t.Fatalf("expected one record, got %d", len(events))
	}
	ev := events[0]
	if len(ev.ScopeUnplaced) != 1 || ev.ScopeUnplaced[0] != "allowed_dirs" {
		t.Fatalf("the record does not name the field that could not be placed: %+v", ev.ScopeUnplaced)
	}

	line, ok := auditAuthorityLine(ev)
	if !ok {
		t.Fatal("no authority line on a denied record")
	}
	if !strings.Contains(line, "SCOPE NOT APPLIED") || !strings.Contains(line, "allowed_dirs") {
		t.Errorf("the authority line is not loud about the unapplied scope: %q", line)
	}

	// The reverse direction: a genuinely scope-nothing MCP keeps the quiet
	// string.
	quiet, _ := auditAuthorityLine(audit.AuditEvent{Access: config.AccessRead, AllowExternal: new(bool)})
	if !strings.Contains(quiet, "(none declared)") {
		t.Errorf("an MCP with no scope concept lost its own rendering: %q", quiet)
	}
	if strings.Contains(quiet, "SCOPE NOT APPLIED") {
		t.Errorf("the loud string leaked onto an ordinary record: %q", quiet)
	}
}

// Same invariant as the schema-unusable case: ListTools must not advertise a
// tool CallTool refuses unconditionally — it would write an uncallable tool
// into relayremote list and a generated SKILL.md.
func TestListTools_WithholdsToolsWhoseGrantCarriesAnUnplaceableScope(t *testing.T) {
	var reached json.RawMessage
	r := unplaceableScopeRouter(t, quietV2Schema, 2, &reached)

	raw, err := r.ListTools(context.Background(), testToken)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var tools []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(tools) != 0 {
		t.Errorf("tools CallTool refuses unconditionally were advertised: %+v", tools)
	}
}
