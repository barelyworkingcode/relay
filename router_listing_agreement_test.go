package main

// ListTools, ListSkillBuckets and CallTool have to agree.
//
// ListTools applied layers 1-4 and then only ANNOTATED scope; the presence
// check ran in CallTool alone. So a tool whose grant can never supply a
// governing field was listed, written into the SKILL.md `relayremote skill`
// generates, and refused on every call — with the appended note naming the
// fields that DID have values and never the one that was the reason:
//
//	relayremote list                          -> lists mail_save_attachment
//	relayremote call --tool mail_save_attachment
//	  -> access denied: MCP 'macmcp' scopes tool 'mail_save_attachment'
//	     by "file_dirs" and this grant supplies no value for it
//
// ADR-011 decision 8 exists so a client is told its own limits. This was
// precisely the limit it was not told, in all three surfaces at once.

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// macmcpSchemaSurface is the worked example beside a tool list that includes
// both governed and ungoverned tools, so a listing can be checked for the
// difference rather than for a count.
func fullScopedProfile(t *testing.T, kind ProjectKind, values map[string]json.RawMessage) *appRouter {
	t.Helper()
	return newProfileRouter(t, profileOpts{
		kind:          kind,
		allowedTools:  map[string][]string{"macmcp": {"mail_*"}},
		access:        map[string]string{"macmcp": AccessWrite},
		allowExternal: map[string]bool{"macmcp": true},
		contextValues: values,
		schema:        macmcpSchema,
		schemaVersion: 2,
	})
}

func bucketToolNames(t *testing.T, r *appRouter) []string {
	t.Helper()
	buckets, err := r.ListSkillBuckets(context.Background(), testToken)
	if err != nil {
		t.Fatalf("ListSkillBuckets: %v", err)
	}
	var names []string
	for _, b := range buckets {
		for _, tool := range b.Tools {
			names = append(names, tool.Name)
		}
	}
	slices.Sort(names)
	return names
}

// TestListTools_WithholdsAToolThisGrantCanNeverCall is the finding. file_dirs
// is source: "project_path"; an access profile has no path, so no value for it
// can ever exist, so mail_save_attachment can never work under this grant.
func TestListTools_WithholdsAToolThisGrantCanNeverCall(t *testing.T) {
	r := fullScopedProfile(t, ProjectKindRemote, map[string]json.RawMessage{
		"mail_accounts":  json.RawMessage(`["Bob"]`),
		"mail_mailboxes": json.RawMessage(`["INBOX"]`),
	})

	listed := listedToolNames(t, r)
	if slices.Contains(listed, "mail_save_attachment") {
		t.Errorf("ListTools advertised a tool CallTool always denies: %v", listed)
	}
	// The tools the field does not govern are untouched: this withholds
	// exactly what is unsatisfiable, not the MCP.
	for _, want := range []string{"mail_search", "mail_get_email", "mail_send"} {
		if !slices.Contains(listed, want) {
			t.Errorf("%s went missing from the listing: %v", want, listed)
		}
	}

	// The skill renderer reads ListSkillBuckets, which is the artefact the
	// agent actually plans from.
	if got := bucketToolNames(t, r); slices.Contains(got, "mail_save_attachment") {
		t.Errorf("ListSkillBuckets advertised it: %v", got)
	}

	// And the three surfaces agree, which is the property rather than the
	// individual assertions.
	if _, err := r.CallTool(context.Background(), "mail_save_attachment", json.RawMessage(`{}`), testToken); err == nil {
		t.Fatal("CallTool allowed the tool the listings withheld")
	}
}

// TestListTools_KeepsAToolWhoseValueIsMerelyUnset is the case that must NOT
// regress, and it is the reason the withholding is narrow. A `denied` naming a
// missing field is more diagnostic to an operator than silent absence, and the
// gap closes the moment somebody types a value — so a transient gap stays
// listed and loud, and only an unsatisfiable one is withheld.
func TestListTools_KeepsAToolWhoseValueIsMerelyUnset(t *testing.T) {
	r := fullScopedProfile(t, ProjectKindRemote, nil)
	listed := listedToolNames(t, r)
	if !slices.Contains(listed, "mail_search") {
		t.Fatalf("a tool whose scope is merely unset was withheld: %v", listed)
	}
	if _, err := r.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken); err == nil {
		t.Fatal("the loud denial for an unset scope was lost")
	} else if !strings.Contains(err.Error(), "mail_accounts") {
		t.Errorf("the denial stopped naming the missing field: %v", err)
	}
}

// TestListTools_ALocalProjectKeepsItsPathScopedTools is the other half of
// "unsatisfiable for this record's KIND": a local project has a path,
// SyncProjectToken derives the value, and nothing is withheld from it.
func TestListTools_ALocalProjectKeepsItsPathScopedTools(t *testing.T) {
	r := fullScopedProfile(t, ProjectKindLocal, map[string]json.RawMessage{
		"mail_accounts":  json.RawMessage(`["Bob"]`),
		"mail_mailboxes": json.RawMessage(`["INBOX"]`),
		"file_dirs":      json.RawMessage(`["/tmp/test"]`),
	})
	if listed := listedToolNames(t, r); !slices.Contains(listed, "mail_save_attachment") {
		t.Fatalf("a local project lost its path-scoped tool: %v", listed)
	}
}

// TestScopeNote_NamesEveryGoverningFieldIncludingTheUnsetOne is the third
// surface. The note reaches the client through ListTools, `relay mcp call
// --list` and the generated SKILL.md, and naming two of three restrictions
// while omitting the one that decides the call is worse than saying nothing.
func TestScopeNote_NamesEveryGoverningFieldIncludingTheUnsetOne(t *testing.T) {
	r := fullScopedProfile(t, ProjectKindRemote, map[string]json.RawMessage{
		"mail_accounts": json.RawMessage(`["Bob"]`),
	})
	raw, err := r.ListTools(context.Background(), testToken)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var desc string
	for _, tool := range unmarshalTools(t, raw) {
		if tool.Name == "mail_search" {
			desc = tool.Description
		}
	}
	if desc == "" {
		t.Fatal("mail_search was not listed")
	}
	if !strings.Contains(desc, "Bob") {
		t.Errorf("the note lost the value that IS set: %q", desc)
	}
	if !strings.Contains(desc, "mail_mailboxes") {
		t.Errorf("the note omits the field with no value, which is the one that refuses the call: %q", desc)
	}
}

// TestValidateProjectGrants_AsksAboutTheGrantedToolsNotTheMcpsWhole Surface is
// the save-time half of the same disagreement. macMCP's file_dirs governs
// mail_save_attachment alone, so over 47 tools the answer is always "some tools
// survive" — but a grant that NAMES only that tool survives with nothing.
func TestValidateProjectGrants_AsksAboutTheGrantedToolsNotTheWholeSurface(t *testing.T) {
	s := &Settings{}
	proj := remoteProjectGranting("macmcp")
	proj.AllowedTools = map[string][]string{"macmcp": {"mail_save_attachment"}}

	err := s.ValidateProjectGrants(proj, McpSurfaces{"macmcp": macmcpSurface()})
	if err == nil {
		t.Fatal("a profile whose every granted tool needs a project path saved cleanly and can call nothing")
	}
	for _, want := range []string{"macmcp", "file_dirs", "no usable tools"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should say %q; got: %v", want, err)
		}
	}

	// A grant that keeps something usable is still permitted: the check
	// narrowed, it did not become a refusal of path-scoped MCPs.
	proj.AllowedTools = map[string][]string{"macmcp": {"mail_*"}}
	if err := s.ValidateProjectGrants(proj, McpSurfaces{"macmcp": macmcpSurface()}); err != nil {
		t.Fatalf("a grant retaining usable tools was refused: %v", err)
	}
}

// TestValidateProjectGrants_AnIncompleteGrantIsJudgedAgainstTheWholeSurface
// pins the fallback, which is the fail-closed direction rather than a
// shortcut. A profile that has granted an MCP and not yet named tools holds
// none — asking "are the zero tools you named all dead" is vacuously no, and
// would permit the fsMCP grant decision 5 is built on refusing.
func TestValidateProjectGrants_AnIncompleteGrantIsJudgedAgainstTheWholeSurface(t *testing.T) {
	s := &Settings{}
	proj := remoteProjectGranting("fsmcp")
	if err := s.ValidateProjectGrants(proj, McpSurfaces{"fsmcp": fsmcpSurface()}); err == nil {
		t.Fatal("a profile with no allowed_tools yet was granted an MCP no tool of which can work")
	}
	// Same when the patterns name nothing the MCP exposes.
	proj.AllowedTools = map[string][]string{"fsmcp": {"nosuch_tool"}}
	if err := s.ValidateProjectGrants(proj, McpSurfaces{"fsmcp": fsmcpSurface()}); err == nil {
		t.Fatal("an allowlist matching no tool was read as a grant of no governed tools")
	}
}
