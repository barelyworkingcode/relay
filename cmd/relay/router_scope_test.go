package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/project"
)

const scopedSchema = `{
  "mail_accounts": {
    "type": "array", "items": {"type": "string"},
    "description": "Mail accounts this client may read from or send as",
    "scope": "restrict", "source": "operator",
    "applies_to": ["mail_*"], "enumerable": true
  }
}`

func scopedProfile(t *testing.T, kind config.ProjectKind, values map[string]json.RawMessage) *appRouter {
	t.Helper()
	return newProfileRouter(t, profileOpts{
		kind:         kind,
		allowedTools: map[string][]string{"macmcp": {"mail_*", "web_fetch"}},
		access:       map[string]string{"macmcp": config.AccessWrite},
		// web_fetch is the scope layer's ungoverned control tool, so it needs
		// the outbound grant too or the access-mode check would refuse it
		// before the scope check is ever reached.
		allowExternal: map[string]bool{"macmcp": true},
		contextValues: values,
		schema:        scopedSchema,
		schemaVersion: 2,
	})
}

func TestCallTool_DeniesWhenTheLiveSchemaDeclaresAScopeTheGrantDoesNotSupply(t *testing.T) {
	r := scopedProfile(t, config.ProjectKindRemote, nil)
	rec := newTestAudit(t, nil)
	r.audit = rec

	if _, err := r.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken); err == nil {
		t.Fatal("a call governed by an unsupplied scope field was allowed")
	} else if !strings.Contains(err.Error(), "mail_accounts") {
		t.Errorf("refusal should name the field the grant is missing, got: %v", err)
	}

	if _, err := r.CallTool(context.Background(), "web_fetch", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("an ungoverned tool was refused for want of a scope: %v", err)
	}

	events := readLoggedEvents(t, rec)
	if len(events) != 2 || events[0].Outcome != AuditOutcomeDenied {
		t.Fatalf("scope refusal recorded as %+v", events[0].Outcome)
	}
}

func TestCallTool_DeniesAnEmptyScopeValueTheSameAsAnAbsentOne(t *testing.T) {
	for _, empty := range []string{`[]`, `null`, `""`, `{}`} {
		r := scopedProfile(t, config.ProjectKindRemote, map[string]json.RawMessage{
			"mail_accounts": json.RawMessage(empty),
		})
		if _, err := r.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken); err == nil {
			t.Errorf("scope value %s was accepted as a restriction", empty)
		}
	}
}

func TestCallTool_AllowsWhenTheScopeIsSupplied(t *testing.T) {
	r := scopedProfile(t, config.ProjectKindRemote, map[string]json.RawMessage{
		"mail_accounts": json.RawMessage(`["Bob"]`),
	})
	if _, err := r.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("a properly scoped call was refused: %v", err)
	}
}

func TestCallTool_ThePresenceCheckIsNotRemoteOnly(t *testing.T) {
	local := scopedProfile(t, config.ProjectKindLocal, nil)
	if _, err := local.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken); err == nil {
		t.Fatal("a local project reached a scope-declaring tool with no scope set")
	}
	local = scopedProfile(t, config.ProjectKindLocal, map[string]json.RawMessage{
		"mail_accounts": json.RawMessage(`["Bob"]`),
	})
	if _, err := local.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("a local project with a scope set was still refused: %v", err)
	}
}

func TestCallTool_AStaleContextKeyIsNeverInjectedIntoMeta(t *testing.T) {
	var capturedMeta json.RawMessage
	capture := func(_ context.Context, _ string, params interface{}) (json.RawMessage, error) {
		if m := decodedToolParams(params); m != nil {
			if raw, err := json.Marshal(m["_meta"]); err == nil {
				capturedMeta = raw
			}
		}
		return json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`), nil
	}

	proj := config.Project{
		ID: "test-project", Name: "test", Kind: config.ProjectKindRemote,
		AllowedMcpIDs: []string{"macmcp"}, Token: config.NewSecret(testToken), TokenHash: config.HashToken(testToken),
		AllowedTools: map[string][]string{"macmcp": {"mail_*"}},
		Access:       map[string]string{"macmcp": config.AccessWrite},
		// write_dirs is left over from a schema rename; the live schema below
		// no longer declares it.
		Context: map[string]json.RawMessage{
			"macmcp": json.RawMessage(`{"mail_accounts":["Bob"],"write_dirs":["/etc"]}`),
		},
	}
	s := &config.Settings{
		Version: 1, ExternalMcps: []config.ExternalMcp{{ID: "macmcp", DisplayName: "macMCP"}},
		Projects: []config.Project{proj}, AdminSecret: config.NewSecret("supersecretadmin"),
	}
	mgr := NewExternalMcpManager(nil)
	addMockConn(mgr, "macmcp", newMockConn("macmcp", macmcpToolSurface(), capture))
	addMockSchema(mgr, "macmcp", scopedSchema, 2)
	r := newTestRouter(t, s, mgr)

	if _, err := r.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken); err == nil {
		t.Fatal("a grant carrying a scope the live schema cannot place was dispatched")
	} else if !strings.Contains(err.Error(), "write_dirs") {
		t.Errorf("the refusal should name the field that could not be placed, got: %v", err)
	}
	if capturedMeta != nil {
		t.Errorf("the MCP was reached at all: _meta = %s", capturedMeta)
	}
}

func TestCallTool_AV1SchemaImposesNoPresenceRequirement(t *testing.T) {
	r := newProfileRouter(t, profileOpts{
		allowedTools:  map[string][]string{"macmcp": {"mail_*"}},
		schema:        scopedSchema,
		schemaVersion: 0,
	})
	if _, err := r.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("a v1 schema imposed a presence requirement: %v", err)
	}
}

const scopedSchemaWithSecret = `{
  "mail_accounts": {
    "type": "array", "items": {"type": "string"},
    "description": "Mail accounts this client may read from or send as",
    "scope": "restrict", "source": "operator",
    "applies_to": ["mail_*"], "enumerable": true
  },
  "api_key": {"type": "string", "description": "credential for the upstream"}
}`

func TestAudit_RecordsTheModeAndOnlyTheDeclaredRestrictFields(t *testing.T) {
	r := newProfileRouter(t, profileOpts{
		kind:          config.ProjectKindRemote,
		allowedTools:  map[string][]string{"macmcp": {"mail_*", "web_fetch"}},
		access:        map[string]string{"macmcp": config.AccessWrite},
		allowExternal: map[string]bool{"macmcp": true},
		schema:        scopedSchemaWithSecret,
		schemaVersion: 2,
		contextValues: map[string]json.RawMessage{
			"mail_accounts": json.RawMessage(`["Bob"]`),
			"api_key":       json.RawMessage(`"sk-do-not-log-me"`),
		},
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
	if ev.Access != config.AccessWrite {
		t.Errorf("access recorded as %q, want %q", ev.Access, config.AccessWrite)
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
	line, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	if strings.Contains(string(line), "sk-do-not-log-me") {
		t.Fatalf("a credential reached the audit line: %s", line)
	}
}

func TestAudit_ARefusalCarriesTheAuthorityItWasRefusedUnder(t *testing.T) {
	scoped := map[string]json.RawMessage{"mail_accounts": json.RawMessage(`["Bob"]`)}

	cases := []struct {
		name    string
		tool    string
		opts    profileOpts
		outcome string
	}{
		{
			name: "a tool the allowlist does not name", tool: "web_fetch",
			opts: profileOpts{kind: config.ProjectKindRemote,
				allowedTools:  map[string][]string{"macmcp": {"mail_*"}},
				contextValues: scoped, schema: scopedSchema, schemaVersion: 2},
			outcome: AuditOutcomeDenied,
		},
		{
			name: "a mutating tool under a read grant", tool: "mail_send",
			opts: profileOpts{kind: config.ProjectKindRemote,
				allowedTools:  map[string][]string{"macmcp": {"mail_*"}},
				contextValues: scoped, schema: scopedSchema, schemaVersion: 2},
			outcome: AuditOutcomeDenied,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newProfileRouter(t, tc.opts)
			rec := newTestAudit(t, nil)
			r.audit = rec

			if _, err := r.CallTool(context.Background(), tc.tool, json.RawMessage(`{}`), testToken); err == nil {
				t.Fatalf("%s was not refused", tc.tool)
			}
			ev := lastEvent(t, rec)
			if ev.Outcome != tc.outcome {
				t.Fatalf("outcome = %q, want %q", ev.Outcome, tc.outcome)
			}
			if ev.Access != config.AccessRead {
				t.Errorf("access = %q, want %q — a refusal must say what mode was in force", ev.Access, config.AccessRead)
			}
			if string(ev.Scope["mail_accounts"]) != `["Bob"]` {
				t.Errorf("scope = %v, want the value the grant carried", ev.Scope)
			}
		})
	}

	t.Run("a grant missing its scope value", func(t *testing.T) {
		r := newProfileRouter(t, profileOpts{kind: config.ProjectKindRemote,
			allowedTools: map[string][]string{"macmcp": {"mail_*"}},
			access:       map[string]string{"macmcp": config.AccessWrite},
			schema:       scopedSchema, schemaVersion: 2})
		rec := newTestAudit(t, nil)
		r.audit = rec

		if _, err := r.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken); err == nil {
			t.Fatal("a call with no scope value was allowed")
		}
		ev := lastEvent(t, rec)
		if ev.Outcome != AuditOutcomeDenied {
			t.Fatalf("outcome = %q, want %q", ev.Outcome, AuditOutcomeDenied)
		}
		if ev.Access != config.AccessWrite {
			t.Errorf("access = %q, want %q", ev.Access, config.AccessWrite)
		}
		if len(ev.Scope) != 0 {
			t.Errorf("scope = %v, want nothing recorded for a grant that carried none", ev.Scope)
		}
	})

	t.Run("a call over its enrolment budget", func(t *testing.T) {
		r := newProfileRouter(t, profileOpts{kind: config.ProjectKindRemote,
			allowedTools:  map[string][]string{"macmcp": {"mail_*"}},
			contextValues: scoped, schema: scopedSchema, schemaVersion: 2,
			enrolments: []config.Enrolment{{
				ClientID:    "hermes-mail",
				Fingerprint: budgetFingerprint("hermes-mail"),
				ProjectIDs:  []string{"test-project"},
				Budget:      config.EnrolmentBudget{WindowSeconds: 60, MaxCalls: 1, MaxResultBytes: 1 << 20},
			}}})
		rec := newTestAudit(t, nil)
		r.audit = rec

		ctx := budgetCtx("hermes-mail")
		if _, err := r.CallTool(ctx, "mail_search", json.RawMessage(`{}`), testToken); err != nil {
			t.Fatalf("the first call was refused: %v", err)
		}
		if _, err := r.CallTool(ctx, "mail_search", json.RawMessage(`{}`), testToken); err == nil {
			t.Fatal("the call over the budget succeeded")
		}
		ev := lastEvent(t, rec)
		if ev.Outcome != AuditOutcomeThrottled {
			t.Fatalf("outcome = %q, want %q", ev.Outcome, AuditOutcomeThrottled)
		}
		if ev.Access != config.AccessRead || string(ev.Scope["mail_accounts"]) != `["Bob"]` {
			t.Errorf("throttled record carried access=%q scope=%v, want the authority in force", ev.Access, ev.Scope)
		}
	})
}

func TestAudit_ScopeViolationIsAFieldAndNotAnOutcome(t *testing.T) {
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
			if strings.Contains(tc.result, `"isError":true`) && ev.Outcome != AuditOutcomeToolError {
				t.Errorf("outcome = %q, want %q", ev.Outcome, AuditOutcomeToolError)
			}
		})
	}
}

func TestAudit_RemoteIntentCarriesTheAuthorityBeforeTheMcpRuns(t *testing.T) {
	// The intent record is written before the call; an authority recorded
	// only on the completion would be missing from the record that survives a
	// crash mid-call.
	f := newRemoteFixture(t, remoteFixtureOpts{})
	assertNoErr(t, f.store.With(func(s *config.Settings) {
		proj, _ := config.FindProjectByID(s, f.project.ID)
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
	if intent.Access != config.AccessRead {
		t.Errorf("intent access = %q, want %q (a profile defaults to read)", intent.Access, config.AccessRead)
	}
	if string(intent.Scope["mail_accounts"]) != `["Bob"]` {
		t.Errorf("intent scope = %v", intent.Scope)
	}
}

func TestListTools_AppendsTheScopeNoteToGovernedToolsOnly(t *testing.T) {
	r := scopedProfile(t, config.ProjectKindRemote, map[string]json.RawMessage{
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
	for _, want := range []string{project.ScopeNotePrefix, "Mail accounts this client may read from or send as", "Bob"} {
		if !strings.Contains(mail, want) {
			t.Errorf("scope note %q missing %q", mail, want)
		}
	}
	if web, ok := seen["web_fetch"]; ok && strings.Contains(web, project.ScopeNotePrefix) {
		t.Errorf("an ungoverned tool got a scope note: %q", web)
	}
}

func TestListSkillBuckets_CarriesTheSameNoteWithoutDoubling(t *testing.T) {
	r := scopedProfile(t, config.ProjectKindRemote, map[string]json.RawMessage{
		"mail_accounts": json.RawMessage(`["Bob"]`),
	})
	// Both list paths read the same live tool objects; calling one after the
	// other is the sequence that would double-append if the note were written
	// onto shared state instead of onto each path's own copy.
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
			if n := strings.Count(tool.Description, project.ScopeNotePrefix); n != 1 {
				t.Errorf("scope note appears %d times: %q", n, tool.Description)
			}
		}
	}
	if !found {
		t.Fatal("mail_search was not bucketed")
	}
	for _, tool := range r.tools.Tools("macmcp") {
		if strings.Contains(tool.Description, project.ScopeNotePrefix) {
			t.Fatalf("a listing wrote its note back onto the MCP's own tool list: %q", tool.Description)
		}
	}
}

func TestScopeFromMeta_NoRestrictFieldDeclaredIsAbsent(t *testing.T) {
	v1 := project.ParseContextSchema(json.RawMessage(`{"anything":"here"}`), 1)
	if got := scopeFromMeta(v1, json.RawMessage(`{"anything":"here"}`)); got != nil {
		t.Errorf("v1 schema: scope = %#v, want nil (no scope concept declared)", got)
	}

	v2NoRestrict := project.ParseContextSchema(json.RawMessage(`{
		"note": {"type": "string", "source": "operator"}
	}`), 2)
	if got := scopeFromMeta(v2NoRestrict, json.RawMessage(`{"note":"hi"}`)); got != nil {
		t.Errorf("v2 schema with no restrict field: scope = %#v, want nil", got)
	}
}

func TestScopeFromMeta_RestrictFieldDeclaredButNothingInjectedIsEmptyNotNil(t *testing.T) {
	cs := project.ParseContextSchema(json.RawMessage(scopedSchema), 2)

	got := scopeFromMeta(cs, json.RawMessage(`{}`))
	if got == nil {
		t.Fatal("scope = nil, want a non-nil empty map — the field IS declared, it just carried no value")
	}
	if len(got) != 0 {
		t.Errorf("scope = %#v, want empty", got)
	}

	// omitempty on a map is defined by length, so it treats a nil and an
	// empty map identically — the distinction has to survive encoding/json,
	// not just live as a Go nil check.
	blob, err := json.Marshal(AuditEvent{Access: config.AccessRead, Scope: got})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(blob), `"scope":{}`) {
		t.Errorf("marshaled record = %s, want a literal \"scope\":{}", blob)
	}
}

func TestScopeFromMeta_PopulatedValueIsCarried(t *testing.T) {
	cs := project.ParseContextSchema(json.RawMessage(scopedSchema), 2)
	got := scopeFromMeta(cs, json.RawMessage(`{"mail_accounts":["Bob"],"unrelated":1}`))
	if string(got["mail_accounts"]) != `["Bob"]` {
		t.Errorf("scope = %v, want mail_accounts = [\"Bob\"]", got)
	}
	if _, leaked := got["unrelated"]; leaked {
		t.Errorf("scope = %v, leaked a field the schema never declared as restrict", got)
	}
}

// encoding/json renders a nil map as `null` and a non-nil empty map as `{}`
// as long as the field is not `omitempty` — scopeFromMeta depends on this to
// keep "no scope declared" and "declared but empty" distinguishable on the wire.
func TestAuditEvent_ScopeAbsentVsEmptyMarshalDifferently(t *testing.T) {
	absent, err := json.Marshal(AuditEvent{Access: "read"})
	if err != nil {
		t.Fatalf("marshal absent: %v", err)
	}
	if !strings.Contains(string(absent), `"scope":null`) {
		t.Errorf("absent scope marshaled as %s, want \"scope\":null", absent)
	}

	empty, err := json.Marshal(AuditEvent{Access: "read", Scope: map[string]json.RawMessage{}})
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	if !strings.Contains(string(empty), `"scope":{}`) {
		t.Errorf("empty scope marshaled as %s, want \"scope\":{}", empty)
	}

	populated, err := json.Marshal(AuditEvent{Access: "read", Scope: map[string]json.RawMessage{
		"mail_accounts": json.RawMessage(`["Bob"]`),
	}})
	if err != nil {
		t.Fatalf("marshal populated: %v", err)
	}
	if !strings.Contains(string(populated), `"scope":{"mail_accounts":["Bob"]}`) {
		t.Errorf("populated scope marshaled as %s", populated)
	}
}

func TestCallTool_RefusesEveryToolOfAnMcpWhoseSchemaCannotBeRead(t *testing.T) {
	broken := `{"mail_accounts":{"type":"array","scope":"restrict","source":"operator","applies_to":"mail_*"}}`
	r := newProfileRouter(t, profileOpts{
		kind:          config.ProjectKindRemote,
		allowedTools:  map[string][]string{"macmcp": {"mail_*", "web_fetch"}},
		access:        map[string]string{"macmcp": config.AccessWrite},
		allowExternal: map[string]bool{"macmcp": true},
		contextValues: map[string]json.RawMessage{"mail_accounts": json.RawMessage(`["Bob"]`)},
		schema:        broken,
		schemaVersion: 2,
	})
	for _, tool := range []string{"mail_search", "web_fetch"} {
		_, err := r.CallTool(context.Background(), tool, json.RawMessage(`{}`), testToken)
		if err == nil {
			t.Fatalf("%s ran against a schema relay could not read", tool)
		}
		if !strings.Contains(err.Error(), "cannot read") {
			t.Errorf("%s: refusal does not say why: %v", tool, err)
		}
	}
}
