package main

// ADR-011 decision 5: source replaces the hardcoded field name, and the grant
// question becomes "would this leave the MCP with no usable tools?"

import (
	"encoding/json"
	"strings"
	"testing"
)

// fsmcpSurface is fsMCP once it declares v2: one project_path field with no
// applies_to, which governs every tool it has.
func fsmcpSurface() McpSurface {
	return McpSurface{
		Schema:        json.RawMessage(fsmcpV2Schema),
		SchemaVersion: 2,
		Tools:         []string{"fs_read", "fs_write", "fs_list", "fs_bash"},
	}
}

// macmcpSurface is macMCP's worked example: three restrict fields, only one of
// them project_path, and that one governs exactly two of the tools.
func macmcpSurface() McpSurface {
	return McpSurface{
		Schema:        json.RawMessage(macmcpSchema),
		SchemaVersion: 2,
		Tools: []string{
			"mail_search", "mail_get_email", "mail_send", "mail_move",
			"mail_save_attachment", "mail_get_source",
			"capture_screenshot", "contacts_list_groups", "messages_send",
		},
	}
}

func remoteProjectGranting(ids ...string) *Project {
	return &Project{ID: "p1", Name: "Profile", Kind: ProjectKindRemote, AllowedMcpIDs: ids}
}

func TestValidateProjectGrants_RefusesAnMcpWhoseEveryToolNeedsTheProjectPath(t *testing.T) {
	// The old rule refused fsMCP because it declared a field called
	// "allowed_dirs". The new one refuses it because a profile has no path,
	// every fs tool is governed by the field that path would fill, and so the
	// grant would buy nothing at all. Same answer, derived instead of encoded.
	s := &Settings{}
	err := s.ValidateProjectGrants(remoteProjectGranting("fsmcp"), McpSurfaces{"fsmcp": fsmcpSurface()})
	if err == nil {
		t.Fatal("a profile was granted an MCP whose every tool needs a project path")
	}
	for _, want := range []string{"fsmcp", v1AllowedDirsField, "no usable tools"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should say %q; got: %v", want, err)
		}
	}
}

func TestValidateProjectGrants_PermitsAnMcpThatKeepsUsableTools(t *testing.T) {
	// macMCP's write_dirs governs mail_save_attachment and mail_get_source
	// only, so the MCP stays grantable and precisely those two lose their
	// filesystem write. This is ADR-011 finding 1's fix arriving as a
	// consequence of the model rather than as a special case.
	s := &Settings{}
	if err := s.ValidateProjectGrants(remoteProjectGranting("macmcp"), McpSurfaces{"macmcp": macmcpSurface()}); err != nil {
		t.Fatalf("a profile was refused macMCP, which retains 7 usable tools: %v", err)
	}
	// And both together still fail on the one that cannot work.
	err := s.ValidateProjectGrants(remoteProjectGranting("macmcp", "fsmcp"),
		McpSurfaces{"macmcp": macmcpSurface(), "fsmcp": fsmcpSurface()})
	if err == nil || !strings.Contains(err.Error(), "fsmcp") {
		t.Fatalf("the refusal should name fsmcp and not macmcp; got: %v", err)
	}
}

func TestValidateProjectGrants_PermitsWhenTheToolSurfaceIsUnknown(t *testing.T) {
	// This is a coherence check an operator sees at edit time, not the
	// boundary — SyncProjectToken's guard and CallTool's presence check are.
	// Refusing on missing information would make an MCP that is merely not
	// running un-grantable.
	surface := macmcpSurface()
	surface.Tools = nil
	s := &Settings{}
	if err := s.ValidateProjectGrants(remoteProjectGranting("macmcp"), McpSurfaces{"macmcp": surface}); err != nil {
		t.Fatalf("a grant was refused because relay had not connected to the MCP: %v", err)
	}
	if err := s.ValidateProjectGrants(remoteProjectGranting("macmcp"), nil); err != nil {
		t.Fatalf("a grant was refused with no surfaces at all: %v", err)
	}
}

func TestValidateProjectGrants_LocalProjectsAreExempt(t *testing.T) {
	local := &Project{ID: "p1", Path: "/tmp/x", AllowedMcpIDs: []string{"fsmcp"}}
	s := &Settings{}
	if err := s.ValidateProjectGrants(local, McpSurfaces{"fsmcp": fsmcpSurface()}); err != nil {
		t.Fatalf("a local project was refused a path-scoped MCP: %v", err)
	}
}

func TestSyncProjectToken_DerivesEveryProjectPathFieldTheSchemaDeclares(t *testing.T) {
	// Relay writes the path because the SCHEMA asked for it, not because relay
	// recognised the name. The field here is called write_dirs and relay has
	// never heard of it.
	s := &Settings{ExternalMcps: []ExternalMcp{{ID: "macmcp"}}}
	proj := &Project{ID: "p1", Path: "/tmp/project", AllowedMcpIDs: []string{"macmcp"}}
	s.SyncProjectToken(proj, McpSurfaces{"macmcp": macmcpSurface()})

	values := contextValues(proj.Context["macmcp"])
	if string(values["write_dirs"]) != `["/tmp/project"]` {
		t.Fatalf("write_dirs = %s, want the project path", values["write_dirs"])
	}
	// The operator-supplied fields are NOT invented. Relay has no answer to
	// "which mailbox" and must not guess one.
	if _, invented := values["mail_accounts"]; invented {
		t.Errorf("relay invented a value for an operator-supplied scope field: %s", values["mail_accounts"])
	}
}

func TestSyncProjectToken_DerivationDoesNotClobberOperatorSetFields(t *testing.T) {
	// The old code replaced the whole context blob, which was harmless while
	// relay derived exactly one field and destructive as soon as an operator
	// can set others beside it.
	s := &Settings{ExternalMcps: []ExternalMcp{{ID: "macmcp"}}}
	proj := &Project{
		ID: "p1", Path: "/tmp/project", AllowedMcpIDs: []string{"macmcp"},
		Context: map[string]json.RawMessage{
			"macmcp": json.RawMessage(`{"mail_accounts":["Bob"],"mail_mailboxes":["INBOX"]}`),
		},
	}
	s.SyncProjectToken(proj, McpSurfaces{"macmcp": macmcpSurface()})

	values := contextValues(proj.Context["macmcp"])
	if string(values["mail_accounts"]) != `["Bob"]` {
		t.Errorf("an operator-set scope was lost on resync: %v", values)
	}
	if string(values["write_dirs"]) != `["/tmp/project"]` {
		t.Errorf("the derived field was not written beside it: %v", values)
	}
}

func TestSyncProjectToken_ARemoteRecordNeverGetsAProjectPathField(t *testing.T) {
	// Defence in depth, stated generically. ValidateProjectGrants is supposed
	// to refuse a grant this could apply to; this guard is what keeps a bypass
	// of that check from turning a silent widening into a loud failure.
	s := &Settings{ExternalMcps: []ExternalMcp{{ID: "macmcp"}}}
	proj := &Project{ID: "p1", Kind: ProjectKindRemote, AllowedMcpIDs: []string{"macmcp"}}
	s.SyncProjectToken(proj, McpSurfaces{"macmcp": macmcpSurface()})
	if raw, ok := proj.Context["macmcp"]; ok {
		t.Fatalf("a profile was handed a derived scope: %s", raw)
	}
}

func TestSyncProjectToken_DisablesFsBashForAnyPathScopedMcp(t *testing.T) {
	// DEFERRED by ADR-011: this is still a hardcoded tool name. It is now keyed
	// off "this MCP scopes something to the project path" rather than off the
	// field's name, which is the most domain-blind form available without the
	// schema change.
	s := &Settings{ExternalMcps: []ExternalMcp{{ID: "fsmcp"}}}
	proj := &Project{ID: "p1", Path: "/tmp/project", AllowedMcpIDs: []string{"fsmcp"}}
	s.SyncProjectToken(proj, McpSurfaces{"fsmcp": fsmcpSurface()})
	if len(proj.DisabledTools["fsmcp"]) != 1 || proj.DisabledTools["fsmcp"][0] != v1FsBashTool {
		t.Fatalf("fs_bash was not auto-disabled: %v", proj.DisabledTools)
	}
	// Idempotent across resyncs.
	s.SyncProjectToken(proj, McpSurfaces{"fsmcp": fsmcpSurface()})
	if len(proj.DisabledTools["fsmcp"]) != 1 {
		t.Fatalf("a resync duplicated the entry: %v", proj.DisabledTools)
	}
}

func TestSyncProjectToken_PrunesTheNewAllowlistsForRevokedMcps(t *testing.T) {
	// A stale entry for an MCP the project no longer grants reads as a grant
	// and is not one.
	s := &Settings{ExternalMcps: []ExternalMcp{{ID: "macmcp"}, {ID: "fsmcp"}}}
	proj := &Project{
		ID: "p1", Path: "/tmp/project", AllowedMcpIDs: []string{"macmcp"},
		Access:       map[string]string{"macmcp": AccessWrite, "fsmcp": AccessWrite},
		AllowedTools: map[string][]string{"macmcp": {"mail_*"}, "fsmcp": {"fs_*"}},
	}
	s.SyncProjectToken(proj, McpSurfaces{"macmcp": macmcpSurface()})
	if _, stale := proj.Access["fsmcp"]; stale {
		t.Error("an access mode survived for an MCP the project no longer grants")
	}
	if _, stale := proj.AllowedTools["fsmcp"]; stale {
		t.Error("an allowlist survived for an MCP the project no longer grants")
	}
	if _, kept := proj.AllowedTools["macmcp"]; !kept {
		t.Error("the granted MCP's allowlist was pruned")
	}
}

func TestSyncProjectToken_V1SchemaStillGetsTheAllowedDirsBranch(t *testing.T) {
	// One release of compatibility, unchanged: an MCP that declares no
	// contextSchemaVersion is handled exactly as it was.
	s := &Settings{ExternalMcps: []ExternalMcp{{ID: "fsmcp"}}}
	proj := &Project{ID: "p1", Path: "/tmp/project", AllowedMcpIDs: []string{"fsmcp"}}
	s.SyncProjectToken(proj, McpSurfaces{"fsmcp": {Schema: json.RawMessage(`{"allowed_dirs":{"type":"array"}}`)}})
	if string(proj.Context["fsmcp"]) != `{"allowed_dirs":["/tmp/project"]}` {
		t.Fatalf("v1 derivation changed shape: %s", proj.Context["fsmcp"])
	}
}

func TestUpdateProjectAllowedTools_DropsEntriesForUngrantedMcps(t *testing.T) {
	s := &Settings{Projects: []Project{{ID: "p1", Kind: ProjectKindRemote, AllowedMcpIDs: []string{"macmcp"}}}}
	s.UpdateProjectAllowedTools("p1", map[string][]string{
		"macmcp": {"mail_*", "mail_*", ""},
		"fsmcp":  {"fs_read"},
	})
	proj, _ := s.findProjectByID("p1")
	if len(proj.AllowedTools) != 1 {
		t.Fatalf("allowlist kept an entry for an ungranted MCP: %v", proj.AllowedTools)
	}
	if got := proj.AllowedTools["macmcp"]; len(got) != 1 || got[0] != "mail_*" {
		t.Fatalf("duplicates and blanks were not cleaned: %v", got)
	}
	s.UpdateProjectAllowedTools("p1", nil)
	if proj, _ := s.findProjectByID("p1"); proj.AllowedTools != nil {
		t.Fatalf("clearing the allowlist left %v", proj.AllowedTools)
	}
}
