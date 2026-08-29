package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func fsmcpSurface() McpSurface {
	return McpSurface{
		Schema:        json.RawMessage(fsmcpV2Schema),
		SchemaVersion: 2,
		Tools:         []string{"fs_read", "fs_write", "fs_list", "fs_bash"},
	}
}

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
	s := &Settings{}
	if err := s.ValidateProjectGrants(remoteProjectGranting("macmcp"), McpSurfaces{"macmcp": macmcpSurface()}); err != nil {
		t.Fatalf("a profile was refused macMCP, which retains 7 usable tools: %v", err)
	}
	err := s.ValidateProjectGrants(remoteProjectGranting("macmcp", "fsmcp"),
		McpSurfaces{"macmcp": macmcpSurface(), "fsmcp": fsmcpSurface()})
	if err == nil || !strings.Contains(err.Error(), "fsmcp") {
		t.Fatalf("the refusal should name fsmcp and not macmcp; got: %v", err)
	}
}

// This is a coherence check an operator sees at edit time, not the boundary —
// SyncProjectToken's guard and CallTool's presence check are — so a missing
// surface is permitted rather than refused: refusing here would make an MCP
// that is merely not running un-grantable.
func TestValidateProjectGrants_PermitsWhenTheToolSurfaceIsUnknown(t *testing.T) {
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

// Relay writes the path because the SCHEMA asked for it, not because relay
// recognised the field name: the field here is called file_dirs and relay
// has never heard of it.
func TestSyncProjectToken_DerivesEveryProjectPathFieldTheSchemaDeclares(t *testing.T) {
	s := &Settings{ExternalMcps: []ExternalMcp{{ID: "macmcp"}}}
	proj := &Project{ID: "p1", Path: "/tmp/project", AllowedMcpIDs: []string{"macmcp"}}
	s.SyncProjectToken(proj, McpSurfaces{"macmcp": macmcpSurface()})

	values := contextValues(proj.Context["macmcp"])
	if string(values["file_dirs"]) != `["/tmp/project"]` {
		t.Fatalf("file_dirs = %s, want the project path", values["file_dirs"])
	}
	if _, invented := values["mail_accounts"]; invented {
		t.Errorf("relay invented a value for an operator-supplied scope field: %s", values["mail_accounts"])
	}
}

func TestSyncProjectToken_DerivationDoesNotClobberOperatorSetFields(t *testing.T) {
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
	if string(values["file_dirs"]) != `["/tmp/project"]` {
		t.Errorf("the derived field was not written beside it: %v", values)
	}
}

// Defence in depth: ValidateProjectGrants is supposed to refuse a grant this
// could apply to; this guard is what keeps a bypass of that check from
// turning a silent widening into a loud failure.
func TestSyncProjectToken_ARemoteRecordNeverGetsAProjectPathField(t *testing.T) {
	s := &Settings{ExternalMcps: []ExternalMcp{{ID: "macmcp"}}}
	proj := &Project{ID: "p1", Kind: ProjectKindRemote, AllowedMcpIDs: []string{"macmcp"}}
	s.SyncProjectToken(proj, McpSurfaces{"macmcp": macmcpSurface()})
	if raw, ok := proj.Context["macmcp"]; ok {
		t.Fatalf("a profile was handed a derived scope: %s", raw)
	}
}

// fs_bash is DEFERRED by ADR-011 as still a hardcoded tool name — it is keyed
// off "this MCP scopes something to the project path" rather than off the
// field's name, the most domain-blind form available without a schema change.
func TestSyncProjectToken_DisablesFsBashForAnyPathScopedMcp(t *testing.T) {
	s := &Settings{ExternalMcps: []ExternalMcp{{ID: "fsmcp"}}}
	proj := &Project{ID: "p1", Path: "/tmp/project", AllowedMcpIDs: []string{"fsmcp"}}
	s.SyncProjectToken(proj, McpSurfaces{"fsmcp": fsmcpSurface()})
	if len(proj.DisabledTools["fsmcp"]) != 1 || proj.DisabledTools["fsmcp"][0] != v1FsBashTool {
		t.Fatalf("fs_bash was not auto-disabled: %v", proj.DisabledTools)
	}
	s.SyncProjectToken(proj, McpSurfaces{"fsmcp": fsmcpSurface()})
	if len(proj.DisabledTools["fsmcp"]) != 1 {
		t.Fatalf("a resync duplicated the entry: %v", proj.DisabledTools)
	}
}

func TestSyncProjectToken_PrunesTheNewAllowlistsForRevokedMcps(t *testing.T) {
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
