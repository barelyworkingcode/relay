package main

// ADR-011 decisions 4, 7 and 8, at the chokepoint: the call-time presence
// re-check, what the audit record says about the authority a call ran with,
// and the scope note a client is told its own limits through.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"relaygo/bridge"
)

// scopedSchema is macMCP's declaration with the two operator fields only, so a
// test can turn a scope requirement on without also needing a project path.
const scopedSchema = `{
  "mail_accounts": {
    "type": "array", "items": {"type": "string"},
    "description": "Mail accounts this client may read from or send as",
    "scope": "restrict", "source": "operator",
    "applies_to": ["mail_*"], "enumerable": true
  }
}`

func scopedProfile(t *testing.T, kind ProjectKind, values map[string]json.RawMessage) *appRouter {
	t.Helper()
	return newProfileRouter(t, profileOpts{
		kind:          kind,
		allowedTools:  map[string][]string{"macmcp": {"mail_*", "web_fetch"}},
		access:        map[string]string{"macmcp": AccessWrite},
		contextValues: values,
		schema:        scopedSchema,
		schemaVersion: 2,
	})
}

// TestCallTool_DeniesWhenTheLiveSchemaDeclaresAScopeTheGrantDoesNotSupply is
// the case the whole third defence exists for: the grant was validated against
// a schema that had no restrict field, the MCP was upgraded, and nothing
// re-ran validation. Only a check against the LIVE schema catches it.
func TestCallTool_DeniesWhenTheLiveSchemaDeclaresAScopeTheGrantDoesNotSupply(t *testing.T) {
	r := scopedProfile(t, ProjectKindRemote, nil)
	rec := newTestAudit(t, nil)
	r.audit = rec

	if _, err := r.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken); err == nil {
		t.Fatal("a call governed by an unsupplied scope field was allowed")
	} else if !strings.Contains(err.Error(), "mail_accounts") {
		t.Errorf("refusal should name the field the grant is missing, got: %v", err)
	}

	// A tool the field does not govern is untouched: applies_to is what
	// selects, and web_fetch is outside "mail_*".
	if _, err := r.CallTool(context.Background(), "web_fetch", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("an ungoverned tool was refused for want of a scope: %v", err)
	}

	events := readLoggedEvents(t, rec)
	if len(events) != 2 || events[0].Outcome != AuditOutcomeDenied {
		t.Fatalf("scope refusal recorded as %+v", events[0].Outcome)
	}
}

func TestCallTool_DeniesAnEmptyScopeValueTheSameAsAnAbsentOne(t *testing.T) {
	// Absent and empty are both refusals: "no restriction" is deliberately not
	// expressible as emptiness, so a stored [] must not read as a grant that
	// confines nothing.
	for _, empty := range []string{`[]`, `null`, `""`, `{}`} {
		r := scopedProfile(t, ProjectKindRemote, map[string]json.RawMessage{
			"mail_accounts": json.RawMessage(empty),
		})
		if _, err := r.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken); err == nil {
			t.Errorf("scope value %s was accepted as a restriction", empty)
		}
	}
}

func TestCallTool_AllowsWhenTheScopeIsSupplied(t *testing.T) {
	r := scopedProfile(t, ProjectKindRemote, map[string]json.RawMessage{
		"mail_accounts": json.RawMessage(`["Bob"]`),
	})
	if _, err := r.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("a properly scoped call was refused: %v", err)
	}
}

// TestCallTool_ThePresenceCheckIsNotRemoteOnly pins ADR-011 decision 4's
// deliberate part. The asymmetric default in decision 2 is not extended here,
// because a mode has a defensible default in each direction and a scope has
// none — there is no answer to "which mailbox" relay could pick and be right
// about.
func TestCallTool_ThePresenceCheckIsNotRemoteOnly(t *testing.T) {
	local := scopedProfile(t, ProjectKindLocal, nil)
	if _, err := local.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken); err == nil {
		t.Fatal("a local project reached a scope-declaring tool with no scope set")
	}
	local = scopedProfile(t, ProjectKindLocal, map[string]json.RawMessage{
		"mail_accounts": json.RawMessage(`["Bob"]`),
	})
	if _, err := local.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("a local project with a scope set was still refused: %v", err)
	}
}

func TestCallTool_AV1SchemaImposesNoPresenceRequirement(t *testing.T) {
	// A declaration that never opted into the vocabulary declared no scope
	// keywords, so there is nothing here to be present.
	r := newProfileRouter(t, profileOpts{
		allowedTools:  map[string][]string{"macmcp": {"mail_*"}},
		schema:        scopedSchema,
		schemaVersion: 0,
	})
	if _, err := r.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("a v1 schema imposed a presence requirement: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Decision 7: what the record says about the authority
// ---------------------------------------------------------------------------

func TestAudit_RecordsTheModeAndOnlyTheDeclaredRestrictFields(t *testing.T) {
	// _meta is a general channel and a future MCP may pass an API key through
	// it. Logging Context[extID] wholesale would make the audit file the place
	// credentials go to be archived.
	r := scopedProfile(t, ProjectKindRemote, map[string]json.RawMessage{
		"mail_accounts": json.RawMessage(`["Bob"]`),
		"api_key":       json.RawMessage(`"sk-do-not-log-me"`),
	})
	rec := newTestAudit(t, nil)
	r.audit = rec

	if _, err := r.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	events := readLoggedEvents(t, rec)
	if len(events) != 1 {
		t.Fatalf("expected 1 record, got %d", len(events))
	}
	ev := events[0]
	if ev.Access != AccessWrite {
		t.Errorf("access recorded as %q, want %q", ev.Access, AccessWrite)
	}
	if string(ev.Scope["mail_accounts"]) != `["Bob"]` {
		t.Errorf("scope did not record the injected value: %v", ev.Scope)
	}
	if _, leaked := ev.Scope["api_key"]; leaked {
		t.Error("a context field the schema does not declare as a restriction was archived in the audit log")
	}
	if _, leaked := ev.Scope["project_id"]; leaked {
		t.Error("project_id was recorded as a resource scope")
	}
	// Serialize the whole line: the on-disk contract is what external tooling
	// greps, and a credential must not appear anywhere in it.
	line, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	if strings.Contains(string(line), "sk-do-not-log-me") {
		t.Fatalf("a credential reached the audit line: %s", line)
	}
}

func TestAudit_ScopeViolationIsAFieldAndNotAnOutcome(t *testing.T) {
	// ADR-008 already places this case: tool_error means the call completed
	// and the MCP answered no. Promoting it to an outcome would inflate a
	// small enum that --outcome, the CLI table and the UI pill all key on.
	cases := []struct {
		name   string
		result string
		want   bool
	}{
		{"the marker", `{"content":[],"isError":true,"_meta":{"scope_violation":true}}`, true},
		{"namespaced", `{"content":[],"isError":true,"_meta":{"relay/scope_violation":true}}`, true},
		{"false", `{"content":[],"isError":true,"_meta":{"scope_violation":false}}`, false},
		{"a string", `{"content":[],"isError":true,"_meta":{"scope_violation":"yes"}}`, false},
		{"no marker", `{"content":[],"isError":true}`, false},
		{"marker without isError", `{"content":[],"_meta":{"scope_violation":true}}`, false},
		{"_meta is not an object", `{"content":[],"isError":true,"_meta":7}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newProfileRouter(t, profileOpts{
				allowedTools: map[string][]string{"macmcp": {"mail_*"}},
				tools:        macmcpToolSurface(),
			})
			addMockConn(r.tools.(*ExternalMcpManager), "macmcp",
				newMockConn("macmcp", macmcpToolSurface(), okHandler(tc.result)))
			rec := newTestAudit(t, nil)
			r.audit = rec

			if _, err := r.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken); err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			ev := readLoggedEvents(t, rec)[0]
			if ev.ScopeViolation != tc.want {
				t.Errorf("scope_violation = %v, want %v", ev.ScopeViolation, tc.want)
			}
			// Whatever the marker says, an in-protocol refusal is a tool_error
			// and nothing else.
			if strings.Contains(tc.result, `"isError":true`) && ev.Outcome != AuditOutcomeToolError {
				t.Errorf("outcome = %q, want %q", ev.Outcome, AuditOutcomeToolError)
			}
		})
	}
}

func TestAudit_RemoteIntentCarriesTheAuthorityBeforeTheMcpRuns(t *testing.T) {
	// The intent record is the one written before the call. An authority
	// recorded only on the completion would be missing from exactly the record
	// that survives a crash mid-call.
	f := newRemoteFixture(t, remoteFixtureOpts{})
	assertNoErr(t, f.store.With(func(s *Settings) {
		proj, _ := s.findProjectByID(f.project.ID)
		proj.Context = map[string]json.RawMessage{
			"macmcp": json.RawMessage(`{"mail_accounts":["Bob"]}`),
		}
	}), "set the profile's scope")
	addMockSchema(f.mgr, "macmcp", scopedSchema, 2)

	c := f.dial()
	if resp := c.roundTrip(`{"type":"CallTool","name":"mail_search"}`); resp.Type != bridge.RespResult {
		t.Fatalf("scoped remote call refused: %s %s", resp.Type, resp.Message)
	}
	events := readLoggedEvents(t, f.audit)
	var intent *AuditEvent
	for i := range events {
		if events[i].Phase == AuditPhaseIntent {
			intent = &events[i]
		}
	}
	if intent == nil {
		t.Fatal("no intent record")
	}
	if intent.Access != AccessRead {
		t.Errorf("intent access = %q, want %q (a profile defaults to read)", intent.Access, AccessRead)
	}
	if string(intent.Scope["mail_accounts"]) != `["Bob"]` {
		t.Errorf("intent scope = %v", intent.Scope)
	}
}

// ---------------------------------------------------------------------------
// Decision 8: the scope note
// ---------------------------------------------------------------------------

func TestListTools_AppendsTheScopeNoteToGovernedToolsOnly(t *testing.T) {
	r := scopedProfile(t, ProjectKindRemote, map[string]json.RawMessage{
		"mail_accounts": json.RawMessage(`["Bob"]`),
	})
	raw, err := r.ListTools(context.Background(), testToken)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	seen := map[string]string{}
	for _, tool := range unmarshalTools(t, raw) {
		seen[tool.Name] = tool.Description
	}
	mail, ok := seen["mail_search"]
	if !ok {
		t.Fatal("mail_search was not listed")
	}
	if !strings.HasPrefix(mail, "Search mail.") {
		t.Errorf("the tool's own description was lost: %q", mail)
	}
	for _, want := range []string{scopeNotePrefix, "Mail accounts this client may read from or send as", "Bob"} {
		if !strings.Contains(mail, want) {
			t.Errorf("scope note %q missing %q", mail, want)
		}
	}
	if web, ok := seen["web_fetch"]; ok && strings.Contains(web, scopeNotePrefix) {
		t.Errorf("an ungoverned tool got a scope note: %q", web)
	}
}

func TestListSkillBuckets_CarriesTheSameNoteWithoutDoubling(t *testing.T) {
	r := scopedProfile(t, ProjectKindRemote, map[string]json.RawMessage{
		"mail_accounts": json.RawMessage(`["Bob"]`),
	})
	// Both list paths read the same live tool objects. Calling one after the
	// other is exactly the sequence that would double-append if the note were
	// written onto shared state instead of onto each path's own copy.
	if _, err := r.ListTools(context.Background(), testToken); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	buckets, err := r.ListSkillBuckets(context.Background(), testToken)
	if err != nil {
		t.Fatalf("ListSkillBuckets: %v", err)
	}
	found := false
	for _, b := range buckets {
		for _, tool := range b.Tools {
			if tool.Name != "mail_search" {
				continue
			}
			found = true
			if n := strings.Count(tool.Description, scopeNotePrefix); n != 1 {
				t.Errorf("scope note appears %d times: %q", n, tool.Description)
			}
		}
	}
	if !found {
		t.Fatal("mail_search was not bucketed")
	}
	// And the live tool list itself was not mutated by either pass.
	for _, tool := range r.tools.Tools("macmcp") {
		if strings.Contains(tool.Description, scopeNotePrefix) {
			t.Fatalf("a listing wrote its note back onto the MCP's own tool list: %q", tool.Description)
		}
	}
}
