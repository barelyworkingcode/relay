package main

// Issue #42: a profile's scope value for a field the MCP's live contextSchema
// does not declare was silently dropped, the call dispatched unconfined, and
// the audit recorded `scope=(none declared)` — the reassuring one of two very
// different facts.
//
// TestCallTool_DeniesAScopeTheLiveSchemaCannotPlace is written against nothing
// but the router and the audit reader, so it compiles and FAILS on main: there
// the call succeeds, _meta reaches the MCP with allowed_dirs stripped, and the
// only thing that would have stopped an unconfined filesystem call is fsMCP's
// own fail-closed rule.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// quietV2Schema is a v2 declaration that scopes NOTHING. It is what relay
// holds after any of the reachable paths issue #42 lists: an MCP whose schema
// legitimately changed, a downgrade, a rebuild, a different binary at the same
// path, or a schema that failed to publish.
const quietV2Schema = `{}`

// unplaceableScopeRouter grants macmcp to an access profile whose stored scope
// names a field the live schema does not declare, and captures whatever
// reaches the MCP.
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
	proj := Project{
		ID: "probe", Name: "probe", Kind: ProjectKindRemote,
		AllowedMcpIDs: []string{"macmcp"}, Token: testToken, TokenHash: hashToken(testToken),
		AllowedTools: map[string][]string{"macmcp": {"mail_*"}},
		Access:       map[string]string{"macmcp": AccessWrite},
		Context: map[string]json.RawMessage{
			"macmcp": json.RawMessage(`{"allowed_dirs":["/Users/admin/project"]}`),
		},
	}
	s := &Settings{
		Version: 1, ExternalMcps: []ExternalMcp{{ID: "macmcp", DisplayName: "macMCP"}},
		Projects: []Project{proj}, AdminSecret: "supersecretadmin",
	}
	mgr := NewExternalMcpManager(nil)
	addMockConn(mgr, "macmcp", newMockConn("macmcp", macmcpToolSurface(), capture))
	addMockSchema(mgr, "macmcp", schema, version)
	return newTestRouter(t, s, mgr)
}

// TestCallTool_DeniesAScopeTheLiveSchemaCannotPlace is the reproduction.
//
// Relay already denies every call to an MCP that "publishes a context schema
// relay cannot read", because a scope it cannot understand is not one it can
// enforce. A scope the operator WROTE that relay cannot place is the same
// condition and gets the same answer. Do not rely on the MCP's own
// fail-closed behaviour: fsMCP happens to have one, relay cannot assume the
// next MCP does, and layer 5's whole point is that relay knows what it handed
// over.
func TestCallTool_DeniesAScopeTheLiveSchemaCannotPlace(t *testing.T) {
	var reached json.RawMessage
	r := unplaceableScopeRouter(t, quietV2Schema, 2, &reached)
	rec := newTestAudit(t, nil)
	r.audit = rec

	_, err := r.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken)
	if err == nil {
		t.Fatal("a call whose operator-configured scope relay could not place was dispatched")
	}
	// The message has to name the field and the MCP, so an operator sees
	// "your profile scopes allowed_dirs and this MCP no longer declares it"
	// rather than a generic error.
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
	if events[0].Outcome != AuditOutcomeDenied {
		t.Errorf("recorded as %q, want %q", events[0].Outcome, AuditOutcomeDenied)
	}
}

// TestCallTool_APlaceableScopeIsUnaffected is the control: the same grant
// against a schema that still declares the field runs exactly as before.
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

// TestCallTool_AnEmptyStoredKeyIsNotAnUnplaceableScope: an empty value is
// absent everywhere else in this model (hasScopeValue, checkScopePresence, the
// UI), and a leftover empty key asserts no confinement anybody could fail to
// deliver. Denying on one would turn a harmless remnant into an outage.
func TestCallTool_AnEmptyStoredKeyIsNotAnUnplaceableScope(t *testing.T) {
	for _, empty := range []string{`[]`, `null`, `""`, `{}`} {
		proj := Project{
			ID: "probe", Name: "probe", Kind: ProjectKindRemote,
			AllowedMcpIDs: []string{"macmcp"}, Token: testToken, TokenHash: hashToken(testToken),
			AllowedTools: map[string][]string{"macmcp": {"mail_*"}},
			Access:       map[string]string{"macmcp": AccessWrite},
			Context: map[string]json.RawMessage{
				"macmcp": json.RawMessage(`{"gone_field":` + empty + `}`),
			},
		}
		s := &Settings{
			Version: 1, ExternalMcps: []ExternalMcp{{ID: "macmcp", DisplayName: "macMCP"}},
			Projects: []Project{proj}, AdminSecret: "supersecretadmin",
		}
		mgr := NewExternalMcpManager(nil)
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

// TestCallTool_AV1SchemaIsUnaffectedByThePlacementCheck pins the boundary of
// the fix, which is also the boundary of the defect. The failure is
// DROP-AND-DISPATCH, and only the v2 branch drops: filterKnownContextFields
// returns a v1 blob verbatim, so the operator's value goes out on the wire
// under the name they wrote it. Widening the refusal to v1 would break the
// promise the version exists to keep — "handled exactly as it was before
// ADR-011" — for every MCP that ships no contextSchema at all.
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

// TestAudit_UnplacedScopeDoesNotShareAStringWithNoneDeclared is the second
// half of issue #42's requirement. `scope=(none declared)` is true of "this
// MCP declares no scope fields at all" and was ALSO what an unconfined
// dispatch produced. Those must not share a string, and the second must be
// loud.
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

	// An MCP that genuinely scopes nothing, with a grant that sets nothing,
	// keeps the quiet string — the two facts must stay distinguishable in
	// both directions.
	quiet, _ := auditAuthorityLine(AuditEvent{Access: AccessRead, AllowExternal: new(bool)})
	if !strings.Contains(quiet, "(none declared)") {
		t.Errorf("an MCP with no scope concept lost its own rendering: %q", quiet)
	}
	if strings.Contains(quiet, "SCOPE NOT APPLIED") {
		t.Errorf("the loud string leaked onto an ordinary record: %q", quiet)
	}
}

// TestListTools_WithholdsToolsWhoseGrantCarriesAnUnplaceableScope keeps the
// listing honest with dispatch. CallTool refuses every tool of this MCP under
// this grant unconditionally, and a listing that advertised them would write
// an uncallable tool into `relayremote list` and into a generated SKILL.md —
// the same invariant "ListTools stops advertising tools CallTool always
// denies" established for the schema-unusable case.
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
