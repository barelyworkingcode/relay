package main

// ADR-011 decisions 2 and 2b: the two allowlists relay can enforce by itself —
// which tools a grant names, and which operations it may perform. Both fail
// closed, and the negative cases are the point of this file: an unannotated
// tool is refused, a malformed annotations blob is refused, a profile with no
// allowed_tools holds nothing, and a "*" never reaches a profile at all.

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"relaygo/mcp"
)

// macmcpToolSurface is a scale model of the real thing: mail tools beside the
// twelve other domains macMCP bundles with them. The annotations are the ones
// macMCP actually publishes, which is what makes finding 9's measurement
// reproducible here — capture_screenshot, web_fetch and contacts_list_groups
// are all honestly read-only, so the access mode alone leaves every one of
// them reachable from a profile named for a mailbox.
func macmcpToolSurface() []mcp.Tool {
	readOnly := json.RawMessage(`{"readOnlyHint":true}`)
	return []mcp.Tool{
		{Name: "mail_search", Description: "Search mail.", Annotations: readOnly},
		{Name: "mail_get_email", Description: "Read one message.", Annotations: readOnly},
		// No annotations at all — macMCP omits them on mail_move and
		// mail_mark_read today, and an absent hint is not a claim of safety.
		{Name: "mail_move", Description: "Move a message."},
		// Explicitly false.
		{Name: "mail_send", Description: "Send mail.", Annotations: json.RawMessage(`{"readOnlyHint":false}`)},
		// A blob that parses as JSON but not as annotations. Must deny, and
		// must not panic — these are server-supplied bytes relay has carried
		// unread until now.
		{Name: "mail_save_attachment", Description: "Write a file.", Annotations: json.RawMessage(`"read-only, honest"`)},
		// A hint of the wrong type inside a well-formed object.
		{Name: "mail_get_source", Description: "Fetch raw source.", Annotations: json.RawMessage(`{"readOnlyHint":"true"}`)},

		{Name: "capture_screenshot", Description: "Screenshot the display.", Annotations: readOnly},
		{Name: "web_fetch", Description: "Fetch a URL.", Annotations: readOnly},
		{Name: "contacts_list_groups", Description: "List contact groups.", Annotations: readOnly},
		{Name: "messages_send", Description: "Send an iMessage."},
		{Name: "shortcuts_run", Description: "Run a Shortcut."},
		// The anchoring case: "mail_*" must not reach it.
		{Name: "xmail_send", Description: "Not a mail tool.", Annotations: readOnly},
	}
}

type profileOpts struct {
	kind          ProjectKind
	allowedTools  map[string][]string
	access        map[string]string
	contextValues map[string]json.RawMessage
	disabled      map[string][]string
	tools         []mcp.Tool
	schema        string
	schemaVersion int
}

// newProfileRouter builds a router whose single project grants "macmcp" and
// carries exactly the ADR-011 fields under test.
func newProfileRouter(t *testing.T, o profileOpts) *appRouter {
	t.Helper()
	tools := o.tools
	if tools == nil {
		tools = macmcpToolSurface()
	}
	proj := Project{
		ID:            "test-project",
		Name:          "test",
		Kind:          o.kind,
		AllowedMcpIDs: []string{"macmcp"},
		Token:         testToken,
		TokenHash:     hashToken(testToken),
		AllowedTools:  o.allowedTools,
		Access:        o.access,
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
	s := &Settings{
		Version:      1,
		ExternalMcps: []ExternalMcp{{ID: "macmcp", DisplayName: "macMCP"}},
		Projects:     []Project{proj},
		AdminSecret:  "supersecretadmin",
	}
	mgr := NewExternalMcpManager(nil)
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

// ---------------------------------------------------------------------------
// Decision 2: the access mode
// ---------------------------------------------------------------------------

func TestAccessMode_DefaultsAreAsymmetric(t *testing.T) {
	// This asymmetry is the decision, not an oversight: a grant to another
	// machine that nobody has said anything about must not mutate, while every
	// local project written before this field existed must keep working.
	remote := &StoredToken{ProjectKind: ProjectKindRemote}
	if got := remote.AccessMode("macmcp"); got != AccessRead {
		t.Errorf("remote default = %q, want %q", got, AccessRead)
	}
	local := &StoredToken{}
	if got := local.AccessMode("macmcp"); got != AccessWrite {
		t.Errorf("local default = %q, want %q", got, AccessWrite)
	}
	// A hand-edited value that is not exactly "write" narrows rather than
	// widens; there is no third mode for it to mean.
	for _, bogus := range []string{"readwrite", "rw", "WRITE", "", "admin"} {
		tok := &StoredToken{Access: map[string]string{"macmcp": bogus}}
		if got := tok.AccessMode("macmcp"); got != AccessRead {
			t.Errorf("access %q resolved to %q, want %q", bogus, got, AccessRead)
		}
	}
	if got := (&StoredToken{Access: map[string]string{"macmcp": AccessWrite}}).AccessMode("macmcp"); got != AccessWrite {
		t.Errorf(`explicit "write" resolved to %q`, got)
	}
	if got := (*StoredToken)(nil).AccessMode("macmcp"); got != AccessRead {
		t.Errorf("a nil token resolved to %q, want %q", got, AccessRead)
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

func TestCheckToolAccess_ReadGrantAdmitsOnlyAnnotatedReadOnlyTools(t *testing.T) {
	// The allowlist is enumerated rather than wildcarded so this test measures
	// the MODE and nothing else. A pattern wide enough to admit every tool
	// ("*_*", "**") is refused by both the editor and the matcher now — that is
	// F1's fix, and writing one here would have this test passing on a grant
	// nobody can save.
	tok := &StoredToken{
		ProjectKind: ProjectKindRemote,
		AllowedTools: map[string][]string{"macmcp": {
			"mail_*", "xmail_*", "capture_*", "web_*", "contacts_*",
			"messages_*", "shortcuts_*",
		}},
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
	// Relay could not find the definition, so it cannot verify the hint. That
	// is not a reason to admit.
	tok := &StoredToken{ProjectKind: ProjectKindRemote, AllowedTools: map[string][]string{"macmcp": {"mail_*"}}}
	if err := checkToolAccess(tok, "macmcp", "mail_search", nil); err == nil {
		t.Fatal("a tool whose definition relay could not find was admitted to a read grant")
	}
	// And a write grant is unaffected: the mode check is the only thing that
	// reads annotations.
	tok.Access = map[string]string{"macmcp": AccessWrite}
	if err := checkToolAccess(tok, "macmcp", "mail_search", nil); err != nil {
		t.Fatalf("a write grant was refused for want of an annotation: %v", err)
	}
}

func TestListTools_ReadProfileHidesEveryMutatingTool(t *testing.T) {
	r := newProfileRouter(t, profileOpts{
		kind:         ProjectKindRemote,
		allowedTools: map[string][]string{"macmcp": {"mail_*"}},
	})
	got := listedToolNames(t, r)
	want := []string{"mail_get_email", "mail_search"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("read profile listed %v, want %v", got, want)
	}
	// The same profile in write mode sees the mutating mail tools too.
	r = newProfileRouter(t, profileOpts{
		kind:         ProjectKindRemote,
		allowedTools: map[string][]string{"macmcp": {"mail_*"}},
		access:       map[string]string{"macmcp": AccessWrite},
	})
	if got := listedToolNames(t, r); len(got) != 6 {
		t.Fatalf("write profile listed %v, want all six mail_* tools", got)
	}
}

func TestCallTool_ReadProfileDeniesAMutatingToolAndAuditsItAsDenied(t *testing.T) {
	r := newProfileRouter(t, profileOpts{
		kind:         ProjectKindRemote,
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
	if events[0].Outcome != AuditOutcomeDenied {
		t.Errorf("refusal recorded as %q, want %q — relay made this decision", events[0].Outcome, AuditOutcomeDenied)
	}
	if events[1].Outcome != AuditOutcomeOK || events[1].Access != AccessRead {
		t.Errorf("permitted call recorded as outcome=%q access=%q", events[1].Outcome, events[1].Access)
	}
}

// ---------------------------------------------------------------------------
// Decision 2b: allowed_tools
// ---------------------------------------------------------------------------

// TestListTools_ProfileNamedForMailDoesNotHoldTheRestOfMacMcp is finding 9's
// measurement turned into an assertion. The live "Hermes Mail" enrolment was
// authorized for capture_screenshot, capture_audio, shortcuts_run, web_fetch
// and contacts_list_groups; every one of those is honestly read-only, so the
// access mode does not touch them and only an allowlist does.
func TestListTools_ProfileNamedForMailDoesNotHoldTheRestOfMacMcp(t *testing.T) {
	r := newProfileRouter(t, profileOpts{
		kind:         ProjectKindRemote,
		allowedTools: map[string][]string{"macmcp": {"mail_*"}},
		access:       map[string]string{"macmcp": AccessWrite},
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
	// A profile with no allowlist holds no tools: a grant to another machine
	// must be an enumeration someone typed.
	r := newProfileRouter(t, profileOpts{kind: ProjectKindRemote, access: map[string]string{"macmcp": AccessWrite}})
	if got := listedToolNames(t, r); len(got) != 0 {
		t.Fatalf("a profile with no allowed_tools was shown %v", got)
	}
	if _, err := r.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken); err == nil {
		t.Fatal("a profile with no allowed_tools called a tool")
	}
	// An explicitly empty list is the same as an absent one; there is no
	// reading under which saving an empty allowlist meant "everything".
	r = newProfileRouter(t, profileOpts{
		kind:         ProjectKindRemote,
		allowedTools: map[string][]string{"macmcp": {}},
		access:       map[string]string{"macmcp": AccessWrite},
	})
	if got := listedToolNames(t, r); len(got) != 0 {
		t.Fatalf("a profile with an empty allowed_tools was shown %v", got)
	}

	// A LOCAL project is unchanged: no allowlist means every tool, with
	// disabled_tools still subtracting.
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
	// The skill renderer reads this, and a bucket that named a tool ListTools
	// hides would advertise a capability the caller does not have.
	r := newProfileRouter(t, profileOpts{
		kind:         ProjectKindRemote,
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
	// ADR-009 decision 4's reasoning one level down: registering a new tool is
	// the same event as registering a new MCP, at finer grain and far more
	// often.
	remote := &Project{Kind: ProjectKindRemote, AllowedMcpIDs: []string{"macmcp"},
		AllowedTools: map[string][]string{"macmcp": {"*"}}}
	err := validateProjectShape(remote)
	if err == nil {
		t.Fatal(`a profile was allowed allowed_tools: ["*"]`)
	}
	if !strings.Contains(err.Error(), "allowed_tools") || !strings.Contains(err.Error(), "macmcp") {
		t.Errorf("refusal should name the field and the MCP, got: %v", err)
	}
	// A "*" hidden among real patterns is the same wildcard.
	remote.AllowedTools = map[string][]string{"macmcp": {"mail_*", "*"}}
	if validateProjectShape(remote) == nil {
		t.Fatal(`a profile was allowed a "*" beside real patterns`)
	}
	// Patterns are fine — the layers compose, so a future mail_delete_everything
	// is still refused by the mode and still confined by the scope.
	remote.AllowedTools = map[string][]string{"macmcp": {"mail_*"}}
	if err := validateProjectShape(remote); err != nil {
		t.Fatalf("a pattern allowlist was refused: %v", err)
	}
	// A LOCAL project is refused it too, and for a reason that is not the
	// profile's. The call-time matcher ignores an over-broad entry (see
	// toolAllowedByPatterns), so a local project holding ["*"] holds NO tools
	// of that MCP — an allowlist that reads as "everything" and grants
	// nothing. Accepting it on save would be validation and enforcement
	// disagreeing again, which is the whole of F2. The way a local project
	// says "everything" is to have no list at all, which it did before this
	// field existed and still does.
	local := &Project{Path: "/tmp/x", AllowedTools: map[string][]string{"macmcp": {"*"}}}
	if err := validateProjectShape(local); err == nil {
		t.Fatal(`a local project was allowed allowed_tools: ["*"], which grants it nothing`)
	}
	local.AllowedTools = nil
	if err := validateProjectShape(local); err != nil {
		t.Fatalf("a local project with no allowlist was refused: %v", err)
	}
}

func TestValidateProjectShape_RefusesADenylistOnAProfile(t *testing.T) {
	// An inert control is worse than none: it reads on the screen as a
	// boundary and decides nothing allowed_tools has not already decided.
	remote := &Project{Kind: ProjectKindRemote, AllowedMcpIDs: []string{"macmcp"},
		AllowedTools:  map[string][]string{"macmcp": {"mail_*"}},
		DisabledTools: map[string][]string{"macmcp": {"messages_send"}}}
	err := validateProjectShape(remote)
	if err == nil {
		t.Fatal("a profile was allowed disabled_tools")
	}
	if !strings.Contains(err.Error(), "allowed_tools") {
		t.Errorf("refusal should name the mechanism that does bound a profile, got: %v", err)
	}
	// An empty entry is not a denylist.
	remote.DisabledTools = map[string][]string{"macmcp": {}}
	if err := validateProjectShape(remote); err != nil {
		t.Fatalf("an empty disabled_tools entry was refused: %v", err)
	}
	// Nor is a leftover for an MCP the record no longer grants — that is what
	// converting a local fsMCP project to remote produces, and SyncProjectToken
	// prunes it moments later.
	remote.DisabledTools = map[string][]string{"fsmcp": {v1FsBashTool}}
	if err := validateProjectShape(remote); err != nil {
		t.Fatalf("a leftover denylist for an ungranted MCP was refused: %v", err)
	}
}

func TestCheckToolAccess_ADenylistStillNarrowsWhereverItCameFrom(t *testing.T) {
	// validateProjectShape refuses disabled_tools on a profile, but a record
	// that acquired one by a route validation did not cover must still have it
	// honoured: ignoring a denylist is the one direction that widens.
	tok := &StoredToken{
		ProjectKind:   ProjectKindRemote,
		AllowedTools:  map[string][]string{"macmcp": {"mail_*"}},
		Access:        map[string]string{"macmcp": AccessWrite},
		DisabledTools: map[string][]string{"macmcp": {"mail_send"}},
	}
	tool := mcp.Tool{Name: "mail_send"}
	if err := checkToolAccess(tok, "macmcp", "mail_send", &tool); err == nil {
		t.Fatal("a hand-edited denylist on a profile was ignored")
	}
}

func TestCheckToolAccess_ServiceTokensAreUnaffected(t *testing.T) {
	// Service tokens bypass checkToolAccess entirely in the router, exactly as
	// before. Assert it through the router rather than the helper, since that
	// is where the bypass lives.
	r := newProfileRouter(t, profileOpts{kind: ProjectKindRemote})
	svcToken := "ssssssssssssssssssssssssssssssssssssssssssssssssssssssssssssssss"
	r.serviceTokens.Register(hashToken(svcToken))
	raw, err := r.ListTools(context.Background(), svcToken)
	if err != nil {
		t.Fatalf("ListTools as service: %v", err)
	}
	if got := len(unmarshalTools(t, raw)); got != len(macmcpToolSurface()) {
		t.Fatalf("service token saw %d tools, want all %d", got, len(macmcpToolSurface()))
	}
	if _, err := r.CallTool(context.Background(), "messages_send", json.RawMessage(`{}`), svcToken); err != nil {
		t.Fatalf("service token was refused a tool: %v", err)
	}
}
