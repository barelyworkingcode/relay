package main

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/mcp"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
	"github.com/barelyworkingcode/relay/internal/project"
)

func macmcpToolSurface() []mcp.Tool {
	readOnly := json.RawMessage(`{"readOnlyHint":true,"openWorldHint":false}`)
	return []mcp.Tool{
		{Name: "mail_search", Description: "Search mail.", Annotations: readOnly},
		{Name: "mail_get_email", Description: "Read one message.", Annotations: readOnly},
		{Name: "mail_move", Description: "Move a message."},
		{Name: "mail_send", Description: "Send mail.", Annotations: json.RawMessage(`{"readOnlyHint":false,"openWorldHint":true}`)},
		{Name: "mail_create_draft", Description: "Write a draft.", Annotations: json.RawMessage(`{"readOnlyHint":false,"openWorldHint":false}`)},
		{Name: "mail_save_attachment", Description: "Write a file.", Annotations: json.RawMessage(`"read-only, honest"`)},
		{Name: "mail_get_source", Description: "Fetch raw source.", Annotations: json.RawMessage(`{"readOnlyHint":"true"}`)},

		{Name: "capture_screenshot", Description: "Screenshot the display.", Annotations: readOnly},
		{Name: "web_fetch", Description: "Fetch a URL.", Annotations: json.RawMessage(`{"readOnlyHint":true,"openWorldHint":true}`)},
		{Name: "contacts_list_groups", Description: "List contact groups.", Annotations: readOnly},
		{Name: "messages_send", Description: "Send an iMessage."},
		{Name: "shortcuts_run", Description: "Run a Shortcut."},
		{Name: "xmail_send", Description: "Not a mail tool.", Annotations: readOnly},
	}
}

type profileOpts struct {
	kind          config.ProjectKind
	allowedTools  map[string][]string
	access        map[string]string
	allowExternal map[string]bool
	contextValues map[string]json.RawMessage
	disabled      map[string][]string
	tools         []mcp.Tool
	schema        string
	schemaVersion int
	enrolments    []config.Enrolment
}

func newProfileRouter(t *testing.T, o profileOpts) *appRouter {
	t.Helper()
	tools := o.tools
	if tools == nil {
		tools = macmcpToolSurface()
	}
	proj := config.Project{
		ID:            "test-project",
		Name:          "test",
		Kind:          o.kind,
		AllowedMcpIDs: []string{"macmcp"},
		Token:         config.NewSecret(testToken),
		TokenHash:     config.HashToken(testToken),
		AllowedTools:  o.allowedTools,
		Access:        o.access,
		AllowExternal: o.allowExternal,
		DisabledTools: o.disabled,
	}
	if !proj.IsRemote() {
		proj.Path = "/tmp/test"
	}
	if o.contextValues != nil {
		blob, err := json.Marshal(o.contextValues)
		if err != nil {
			t.Fatalf("marshal context: %v", err)
		}
		proj.Context = map[string]json.RawMessage{"macmcp": blob}
	}
	s := &config.Settings{
		Version:      1,
		ExternalMcps: []config.ExternalMcp{{ID: "macmcp", DisplayName: "macMCP"}},
		Projects:     []config.Project{proj},
		Enrolments:   o.enrolments,
		AdminSecret:  config.NewSecret("supersecretadmin"),
	}
	mgr := mcpbroker.NewManager(nil)
	addMockConn(mgr, "macmcp", newMockConn("macmcp", tools,
		okHandler(`{"content":[{"type":"text","text":"ok"}]}`)))
	if o.schema != "" {
		addMockSchema(mgr, "macmcp", o.schema, o.schemaVersion)
	}
	return newTestRouter(t, s, mgr)
}

func listedToolNames(t *testing.T, r *appRouter) []string {
	t.Helper()
	raw, err := r.ListTools(context.Background(), testToken)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var names []string
	for _, tool := range unmarshalTools(t, raw) {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

func TestAccessMode_DefaultsAreAsymmetric(t *testing.T) {
	remote := &config.StoredToken{ProjectKind: config.ProjectKindRemote}
	if got := remote.AccessMode("macmcp"); got != config.AccessRead {
		t.Errorf("remote default = %q, want %q", got, config.AccessRead)
	}
	local := &config.StoredToken{}
	if got := local.AccessMode("macmcp"); got != config.AccessWrite {
		t.Errorf("local default = %q, want %q", got, config.AccessWrite)
	}
	for _, bogus := range []string{"readwrite", "rw", "WRITE", "", "admin"} {
		tok := &config.StoredToken{Access: map[string]string{"macmcp": bogus}}
		if got := tok.AccessMode("macmcp"); got != config.AccessRead {
			t.Errorf("access %q resolved to %q, want %q", bogus, got, config.AccessRead)
		}
	}
	if got := (&config.StoredToken{Access: map[string]string{"macmcp": config.AccessWrite}}).AccessMode("macmcp"); got != config.AccessWrite {
		t.Errorf(`explicit "write" resolved to %q`, got)
	}
	if got := (*config.StoredToken)(nil).AccessMode("macmcp"); got != config.AccessRead {
		t.Errorf("a nil token resolved to %q, want %q", got, config.AccessRead)
	}
}

func TestReadOnlyHint_OnlyAnExplicitBooleanTrueCounts(t *testing.T) {
	cases := []struct {
		name        string
		annotations string
		want        bool
	}{
		{"explicit true", `{"readOnlyHint":true}`, true},
		{"explicit false", `{"readOnlyHint":false}`, false},
		{"absent from a real object", `{"title":"Search"}`, false},
		{"null", `{"readOnlyHint":null}`, false},
		{"a string that says true", `{"readOnlyHint":"true"}`, false},
		{"the number one", `{"readOnlyHint":1}`, false},
		{"an object", `{"readOnlyHint":{"yes":true}}`, false},
		// encoding/json matches struct fields case-insensitively, so a naive
		// `ReadOnlyHint *bool` field would admit these near-miss spellings to a
		// read-only grant; none of them is the key the MCP spec defines, and
		// the exact key is the only one read.
		{"the spec key in title case", `{"ReadOnlyHint":true}`, false},
		{"the spec key lowercased", `{"readonlyhint":true}`, false},
		{"the spec key shouted", `{"READONLYHINT":true}`, false},
		{"a near miss with an underscore", `{"read_only_hint":true}`, false},
		{"a variant beside an honest false", `{"ReadOnlyHint":true,"readOnlyHint":false}`, false},
		{"a variant beside an honest true", `{"ReadOnlyHint":false,"readOnlyHint":true}`, true},
		{"annotations are not an object", `"read-only"`, false},
		{"annotations are an array", `[true]`, false},
		{"invalid JSON", `{"readOnlyHint":`, false},
		{"empty", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool := mcp.Tool{Name: "t", Annotations: json.RawMessage(tc.annotations)}
			if got := readOnlyHintTrue(&tool); got != tc.want {
				t.Errorf("readOnlyHintTrue(%s) = %v, want %v", tc.annotations, got, tc.want)
			}
		})
	}
	if readOnlyHintTrue(nil) {
		t.Error("a nil tool was admitted to a read grant")
	}
}

func TestReadOnlyHint_ACaseVariantDoesNotAdmitAToolToAReadProfile(t *testing.T) {
	tools := []mcp.Tool{
		{Name: "mail_search", Description: "Search mail.", Annotations: json.RawMessage(`{"readOnlyHint":true,"openWorldHint":false}`)},
		{Name: "mail_wipe", Description: "Delete everything.", Annotations: json.RawMessage(`{"ReadOnlyHint":true,"openWorldHint":false}`)},
		{Name: "mail_burn", Description: "Delete everything, quietly.", Annotations: json.RawMessage(`{"readonlyhint":true,"openWorldHint":false}`)},
	}
	r := newProfileRouter(t, profileOpts{
		kind:         config.ProjectKindRemote,
		allowedTools: map[string][]string{"macmcp": {"mail_*"}},
		tools:        tools,
	})
	if got := listedToolNames(t, r); strings.Join(got, ",") != "mail_search" {
		t.Fatalf("read profile listed %v, want only the honestly annotated tool", got)
	}
	for _, tool := range []string{"mail_wipe", "mail_burn"} {
		if _, err := r.CallTool(context.Background(), tool, json.RawMessage(`{}`), testToken); err == nil {
			t.Errorf("%s widened a read grant with a spelling the MCP spec does not define", tool)
		}
	}
}

func TestCheckToolAccess_ReadGrantAdmitsOnlyAnnotatedReadOnlyTools(t *testing.T) {
	tok := &config.StoredToken{
		ProjectKind: config.ProjectKindRemote,
		AllowedTools: map[string][]string{"macmcp": {
			"mail_*", "xmail_*", "capture_*", "web_*", "contacts_*",
			"messages_*", "shortcuts_*",
		}},
		AllowExternal: map[string]bool{"macmcp": true},
	}
	surface := macmcpToolSurface()
	admitted := map[string]bool{"mail_search": true, "mail_get_email": true,
		"capture_screenshot": true, "web_fetch": true, "contacts_list_groups": true,
		"xmail_send": true}
	for _, tool := range surface {
		err := checkToolAccess(tok, "macmcp", tool.Name, &tool)
		if admitted[tool.Name] && err != nil {
			t.Errorf("%s: read grant refused an annotated read-only tool: %v", tool.Name, err)
		}
		if !admitted[tool.Name] && err == nil {
			t.Errorf("%s: read grant admitted a tool that is not annotated read-only", tool.Name)
		}
	}
}

func TestCheckToolAccess_ANilToolDefinitionIsDeniedUnderARead(t *testing.T) {
	tok := &config.StoredToken{ProjectKind: config.ProjectKindRemote, AllowedTools: map[string][]string{"macmcp": {"mail_*"}}}
	if err := checkToolAccess(tok, "macmcp", "mail_search", nil); err == nil {
		t.Fatal("a tool whose definition relay could not find was admitted to a read grant")
	}
	tok.Access = map[string]string{"macmcp": config.AccessWrite}
	if err := checkToolAccess(tok, "macmcp", "mail_search", nil); err == nil {
		t.Fatal("a tool whose definition relay could not find was admitted for want of an openWorldHint")
	}
	tok.AllowExternal = map[string]bool{"macmcp": true}
	if err := checkToolAccess(tok, "macmcp", "mail_search", nil); err != nil {
		t.Fatalf("a write grant allowing external access was still refused: %v", err)
	}
}

func TestListTools_ReadProfileHidesEveryMutatingTool(t *testing.T) {
	r := newProfileRouter(t, profileOpts{
		kind:         config.ProjectKindRemote,
		allowedTools: map[string][]string{"macmcp": {"mail_*"}},
	})
	got := listedToolNames(t, r)
	want := []string{"mail_get_email", "mail_search"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("read profile listed %v, want %v", got, want)
	}
	r = newProfileRouter(t, profileOpts{
		kind:         config.ProjectKindRemote,
		allowedTools: map[string][]string{"macmcp": {"mail_*"}},
		access:       map[string]string{"macmcp": config.AccessWrite},
	})
	want = []string{"mail_create_draft", "mail_get_email", "mail_search"}
	if got := listedToolNames(t, r); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("write profile listed %v, want %v", got, want)
	}
	r = newProfileRouter(t, profileOpts{
		kind:          config.ProjectKindRemote,
		allowedTools:  map[string][]string{"macmcp": {"mail_*"}},
		access:        map[string]string{"macmcp": config.AccessWrite},
		allowExternal: map[string]bool{"macmcp": true},
	})
	if got := listedToolNames(t, r); len(got) != 7 {
		t.Fatalf("write profile allowing external access listed %v, want all seven mail_* tools", got)
	}
}

func TestCallTool_ReadProfileDeniesAMutatingToolAndAuditsItAsDenied(t *testing.T) {
	r := newProfileRouter(t, profileOpts{
		kind:         config.ProjectKindRemote,
		allowedTools: map[string][]string{"macmcp": {"mail_*"}},
	})
	rec := newTestAudit(t, nil)
	r.audit = rec

	if _, err := r.CallTool(context.Background(), "mail_send", json.RawMessage(`{}`), testToken); err == nil {
		t.Fatal("a read-only profile sent mail")
	}
	if _, err := r.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("a read-only profile could not search mail: %v", err)
	}

	events := readLoggedEvents(t, rec)
	if len(events) != 2 {
		t.Fatalf("expected 2 audit records, got %d", len(events))
	}
	if events[0].Outcome != audit.AuditOutcomeDenied {
		t.Errorf("refusal recorded as %q, want %q — relay made this decision", events[0].Outcome, audit.AuditOutcomeDenied)
	}
	if events[1].Outcome != audit.AuditOutcomeOK || events[1].Access != config.AccessRead {
		t.Errorf("permitted call recorded as outcome=%q access=%q", events[1].Outcome, events[1].Access)
	}
}

func TestListTools_ProfileNamedForMailDoesNotHoldTheRestOfMacMcp(t *testing.T) {
	r := newProfileRouter(t, profileOpts{
		kind:         config.ProjectKindRemote,
		allowedTools: map[string][]string{"macmcp": {"mail_*"}},
		access:       map[string]string{"macmcp": config.AccessWrite},
	})
	got := listedToolNames(t, r)
	for _, name := range got {
		if !strings.HasPrefix(name, "mail_") {
			t.Errorf("a mail profile was shown %q", name)
		}
	}
	for _, forbidden := range []string{"capture_screenshot", "web_fetch", "contacts_list_groups", "messages_send", "shortcuts_run"} {
		if slices.Contains(got, forbidden) {
			t.Errorf("a mail profile was shown %q", forbidden)
		}
		if _, err := r.CallTool(context.Background(), forbidden, json.RawMessage(`{}`), testToken); err == nil {
			t.Errorf("a mail profile called %q", forbidden)
		}
	}
	// The anchoring case: "mail_*" is not a prefix search over the name.
	if slices.Contains(got, "xmail_send") {
		t.Error(`"mail_*" admitted xmail_send`)
	}
	if _, err := r.CallTool(context.Background(), "xmail_send", json.RawMessage(`{}`), testToken); err == nil {
		t.Error(`"mail_*" admitted a call to xmail_send`)
	}
}

func TestAllowedTools_AbsentMeansNothingForAProfileAndEverythingLocally(t *testing.T) {
	r := newProfileRouter(t, profileOpts{kind: config.ProjectKindRemote, access: map[string]string{"macmcp": config.AccessWrite}})
	if got := listedToolNames(t, r); len(got) != 0 {
		t.Fatalf("a profile with no allowed_tools was shown %v", got)
	}
	if _, err := r.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken); err == nil {
		t.Fatal("a profile with no allowed_tools called a tool")
	}
	r = newProfileRouter(t, profileOpts{
		kind:         config.ProjectKindRemote,
		allowedTools: map[string][]string{"macmcp": {}},
		access:       map[string]string{"macmcp": config.AccessWrite},
	})
	if got := listedToolNames(t, r); len(got) != 0 {
		t.Fatalf("a profile with an empty allowed_tools was shown %v", got)
	}

	r = newProfileRouter(t, profileOpts{disabled: map[string][]string{"macmcp": {"shortcuts_run"}}})
	got := listedToolNames(t, r)
	if len(got) != len(macmcpToolSurface())-1 {
		t.Fatalf("local project listed %d tools, want all but the disabled one", len(got))
	}
	if slices.Contains(got, "shortcuts_run") {
		t.Error("disabled_tools stopped subtracting for a local project")
	}
	if _, err := r.CallTool(context.Background(), "messages_send", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("a local project was refused a mutating tool: %v", err)
	}
}

func TestListSkillBuckets_MirrorsListToolsFiltering(t *testing.T) {
	r := newProfileRouter(t, profileOpts{
		kind:         config.ProjectKindRemote,
		allowedTools: map[string][]string{"macmcp": {"mail_*"}},
	})
	buckets, err := r.ListSkillBuckets(context.Background(), testToken)
	if err != nil {
		t.Fatalf("ListSkillBuckets: %v", err)
	}
	var bucketed []string
	for _, b := range buckets {
		for _, tool := range b.Tools {
			bucketed = append(bucketed, tool.Name)
		}
	}
	slices.Sort(bucketed)
	if strings.Join(bucketed, ",") != strings.Join(listedToolNames(t, r), ",") {
		t.Fatalf("skill buckets hold %v, ListTools shows %v", bucketed, listedToolNames(t, r))
	}
}

func TestValidateProjectShape_RefusesAWildcardAllowlistOnAProfile(t *testing.T) {
	remote := &config.Project{Kind: config.ProjectKindRemote, AllowedMcpIDs: []string{"macmcp"},
		AllowedTools: map[string][]string{"macmcp": {"*"}}}
	err := project.ValidateShape(remote)
	if err == nil {
		t.Fatal(`a profile was allowed allowed_tools: ["*"]`)
	}
	if !strings.Contains(err.Error(), "allowed_tools") || !strings.Contains(err.Error(), "macmcp") {
		t.Errorf("refusal should name the field and the MCP, got: %v", err)
	}
	remote.AllowedTools = map[string][]string{"macmcp": {"mail_*", "*"}}
	if project.ValidateShape(remote) == nil {
		t.Fatal(`a profile was allowed a "*" beside real patterns`)
	}
	remote.AllowedTools = map[string][]string{"macmcp": {"mail_*"}}
	if err := project.ValidateShape(remote); err != nil {
		t.Fatalf("a pattern allowlist was refused: %v", err)
	}
	// A LOCAL project is refused too, but for a different reason than the
	// profile: toolAllowedByPatterns ignores an over-broad entry at call time,
	// so a local project holding ["*"] would hold NO tools of that MCP —
	// accepting it on save would silently grant nothing. The way a local
	// project says "everything" is with no allowlist at all.
	local := &config.Project{Path: "/tmp/x", AllowedTools: map[string][]string{"macmcp": {"*"}}}
	if err := project.ValidateShape(local); err == nil {
		t.Fatal(`a local project was allowed allowed_tools: ["*"], which grants it nothing`)
	}
	local.AllowedTools = nil
	if err := project.ValidateShape(local); err != nil {
		t.Fatalf("a local project with no allowlist was refused: %v", err)
	}
}

func TestValidateProjectShape_RefusesADenylistOnAProfile(t *testing.T) {
	remote := &config.Project{Kind: config.ProjectKindRemote, AllowedMcpIDs: []string{"macmcp"},
		AllowedTools:  map[string][]string{"macmcp": {"mail_*"}},
		DisabledTools: map[string][]string{"macmcp": {"messages_send"}}}
	err := project.ValidateShape(remote)
	if err == nil {
		t.Fatal("a profile was allowed disabled_tools")
	}
	if !strings.Contains(err.Error(), "allowed_tools") {
		t.Errorf("refusal should name the mechanism that does bound a profile, got: %v", err)
	}
	remote.DisabledTools = map[string][]string{"macmcp": {}}
	if err := project.ValidateShape(remote); err != nil {
		t.Fatalf("an empty disabled_tools entry was refused: %v", err)
	}
	// A leftover denylist entry for an MCP the record no longer grants (e.g.
	// after converting a local project to remote) must not be refused either.
	remote.DisabledTools = map[string][]string{"fsmcp": {project.V1FsBashTool}}
	if err := project.ValidateShape(remote); err != nil {
		t.Fatalf("a leftover denylist for an ungranted MCP was refused: %v", err)
	}
}

func TestCheckToolAccess_ADenylistStillNarrowsWhereverItCameFrom(t *testing.T) {
	// project.ValidateShape refuses disabled_tools on a profile, but a record
	// that acquired one via a route validation didn't cover must still have it
	// honoured — ignoring a denylist is the one direction that would widen
	// access.
	tok := &config.StoredToken{
		ProjectKind:   config.ProjectKindRemote,
		AllowedTools:  map[string][]string{"macmcp": {"mail_*"}},
		Access:        map[string]string{"macmcp": config.AccessWrite},
		DisabledTools: map[string][]string{"macmcp": {"mail_send"}},
	}
	tool := mcp.Tool{Name: "mail_send"}
	if err := checkToolAccess(tok, "macmcp", "mail_send", &tool); err == nil {
		t.Fatal("a hand-edited denylist on a profile was ignored")
	}
}

func TestCheckToolAccess_ServiceTokensAreUnaffected(t *testing.T) {
	// Assert through the router, not checkToolAccess — that's where the
	// service-token bypass lives.
	r := newProfileRouter(t, profileOpts{kind: config.ProjectKindRemote})
	svcCtx := bindTestServiceIdentity(t, r)
	raw, err := r.ListTools(svcCtx, "")
	if err != nil {
		t.Fatalf("ListTools as service: %v", err)
	}
	if got := len(unmarshalTools(t, raw)); got != len(macmcpToolSurface()) {
		t.Fatalf("service token saw %d tools, want all %d", got, len(macmcpToolSurface()))
	}
	if _, err := r.CallTool(svcCtx, "messages_send", json.RawMessage(`{}`), ""); err != nil {
		t.Fatalf("service token was refused a tool: %v", err)
	}
}
