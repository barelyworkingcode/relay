package main

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

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

// file_dirs is source: "project_path"; an access profile has no path, so no
// value can ever exist for it.
func TestListTools_WithholdsAToolThisGrantCanNeverCall(t *testing.T) {
	r := fullScopedProfile(t, ProjectKindRemote, map[string]json.RawMessage{
		"mail_accounts":  json.RawMessage(`["Bob"]`),
		"mail_mailboxes": json.RawMessage(`["INBOX"]`),
	})

	listed := listedToolNames(t, r)
	if slices.Contains(listed, "mail_save_attachment") {
		t.Errorf("ListTools advertised a tool CallTool always denies: %v", listed)
	}
	for _, want := range []string{"mail_search", "mail_get_email", "mail_send"} {
		if !slices.Contains(listed, want) {
			t.Errorf("%s went missing from the listing: %v", want, listed)
		}
	}

	if got := bucketToolNames(t, r); slices.Contains(got, "mail_save_attachment") {
		t.Errorf("ListSkillBuckets advertised it: %v", got)
	}

	if _, err := r.CallTool(context.Background(), "mail_save_attachment", json.RawMessage(`{}`), testToken); err == nil {
		t.Fatal("CallTool allowed the tool the listings withheld")
	}
}

// A `denied` naming a missing field is more diagnostic to an operator than
// silent absence, and the gap closes the moment somebody types a value — so a
// transient gap stays listed and loud, and only an unsatisfiable one is
// withheld.
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

// A local project has a path; SyncProjectToken derives file_dirs, so nothing
// is withheld from it.
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

// Naming two of three restrictions while omitting the one that decides the
// call is worse than saying nothing.
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

// macMCP has dozens of tools, so a broad grant always answers "some tools
// survive"; only a grant naming solely the unsatisfiable tool exposes this.
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

	proj.AllowedTools = map[string][]string{"macmcp": {"mail_*"}}
	if err := s.ValidateProjectGrants(proj, McpSurfaces{"macmcp": macmcpSurface()}); err != nil {
		t.Fatalf("a grant retaining usable tools was refused: %v", err)
	}
}

// A profile that has granted an MCP and not yet named tools holds none —
// asking "are the zero tools you named all dead" is vacuously no, and would
// wrongly permit the grant decision 5 is built on refusing.
func TestValidateProjectGrants_AnIncompleteGrantIsJudgedAgainstTheWholeSurface(t *testing.T) {
	s := &Settings{}
	proj := remoteProjectGranting("fsmcp")
	if err := s.ValidateProjectGrants(proj, McpSurfaces{"fsmcp": fsmcpSurface()}); err == nil {
		t.Fatal("a profile with no allowed_tools yet was granted an MCP no tool of which can work")
	}
	proj.AllowedTools = map[string][]string{"fsmcp": {"nosuch_tool"}}
	if err := s.ValidateProjectGrants(proj, McpSurfaces{"fsmcp": fsmcpSurface()}); err == nil {
		t.Fatal("an allowlist matching no tool was read as a grant of no governed tools")
	}
}
