package project

import (
	"encoding/json"
	"github.com/barelyworkingcode/relay/internal/config"
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

func remoteProjectGranting(ids ...string) *config.Project {
	return &config.Project{ID: "p1", Name: "Profile", Kind: config.ProjectKindRemote, AllowedMcpIDs: ids}
}

func TestValidateProjectGrants_RefusesAnMcpWhoseEveryToolNeedsTheProjectPath(t *testing.T) {
	err := ValidateGrants(remoteProjectGranting("fsmcp"), McpSurfaces{"fsmcp": fsmcpSurface()})
	if err == nil {
		t.Fatal("a profile was granted an MCP whose every tool needs a project path")
	}
	for _, want := range []string{"fsmcp", V1AllowedDirsField, "no usable tools"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should say %q; got: %v", want, err)
		}
	}
}

func TestValidateProjectGrants_PermitsAnMcpThatKeepsUsableTools(t *testing.T) {
	if err := ValidateGrants(remoteProjectGranting("macmcp"), McpSurfaces{"macmcp": macmcpSurface()}); err != nil {
		t.Fatalf("a profile was refused macMCP, which retains 7 usable tools: %v", err)
	}
	err := ValidateGrants(remoteProjectGranting("macmcp", "fsmcp"),
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
	if err := ValidateGrants(remoteProjectGranting("macmcp"), McpSurfaces{"macmcp": surface}); err != nil {
		t.Fatalf("a grant was refused because relay had not connected to the MCP: %v", err)
	}
	if err := ValidateGrants(remoteProjectGranting("macmcp"), nil); err != nil {
		t.Fatalf("a grant was refused with no surfaces at all: %v", err)
	}
}

func TestValidateProjectGrants_LocalProjectsAreExempt(t *testing.T) {
	local := &config.Project{ID: "p1", Path: "/tmp/x", AllowedMcpIDs: []string{"fsmcp"}}
	if err := ValidateGrants(local, McpSurfaces{"fsmcp": fsmcpSurface()}); err != nil {
		t.Fatalf("a local project was refused a path-scoped MCP: %v", err)
	}
}

// Relay writes the path because the SCHEMA asked for it, not because relay
// recognised the field name: the field here is called file_dirs and relay
// has never heard of it.
func TestSyncProjectToken_DerivesEveryProjectPathFieldTheSchemaDeclares(t *testing.T) {
	s := &config.Settings{ExternalMcps: []config.ExternalMcp{{ID: "macmcp"}}}
	proj := &config.Project{ID: "p1", Path: "/tmp/project", AllowedMcpIDs: []string{"macmcp"}}
	syncProjectToken(s, proj, McpSurfaces{"macmcp": macmcpSurface()})

	values := ContextValues(proj.Context["macmcp"])
	if string(values["file_dirs"]) != `["/tmp/project"]` {
		t.Fatalf("file_dirs = %s, want the project path", values["file_dirs"])
	}
	if _, invented := values["mail_accounts"]; invented {
		t.Errorf("relay invented a value for an operator-supplied scope field: %s", values["mail_accounts"])
	}
}

func TestSyncProjectToken_DerivationDoesNotClobberOperatorSetFields(t *testing.T) {
	s := &config.Settings{ExternalMcps: []config.ExternalMcp{{ID: "macmcp"}}}
	proj := &config.Project{
		ID: "p1", Path: "/tmp/project", AllowedMcpIDs: []string{"macmcp"},
		Context: map[string]json.RawMessage{
			"macmcp": json.RawMessage(`{"mail_accounts":["Bob"],"mail_mailboxes":["INBOX"]}`),
		},
	}
	syncProjectToken(s, proj, McpSurfaces{"macmcp": macmcpSurface()})

	values := ContextValues(proj.Context["macmcp"])
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
	s := &config.Settings{ExternalMcps: []config.ExternalMcp{{ID: "macmcp"}}}
	proj := &config.Project{ID: "p1", Kind: config.ProjectKindRemote, AllowedMcpIDs: []string{"macmcp"}}
	syncProjectToken(s, proj, McpSurfaces{"macmcp": macmcpSurface()})
	if raw, ok := proj.Context["macmcp"]; ok {
		t.Fatalf("a profile was handed a derived scope: %s", raw)
	}
}

// fs_bash is DEFERRED by ADR-011 as still a hardcoded tool name — it is keyed
// off "this MCP scopes something to the project path" rather than off the
// field's name, the most domain-blind form available without a schema change.
func TestSyncProjectToken_DisablesFsBashForAnyPathScopedMcp(t *testing.T) {
	s := &config.Settings{ExternalMcps: []config.ExternalMcp{{ID: "fsmcp"}}}
	proj := &config.Project{ID: "p1", Path: "/tmp/project", AllowedMcpIDs: []string{"fsmcp"}}
	syncProjectToken(s, proj, McpSurfaces{"fsmcp": fsmcpSurface()})
	if len(proj.DisabledTools["fsmcp"]) != 1 || proj.DisabledTools["fsmcp"][0] != V1FsBashTool {
		t.Fatalf("fs_bash was not auto-disabled: %v", proj.DisabledTools)
	}
	syncProjectToken(s, proj, McpSurfaces{"fsmcp": fsmcpSurface()})
	if len(proj.DisabledTools["fsmcp"]) != 1 {
		t.Fatalf("a resync duplicated the entry: %v", proj.DisabledTools)
	}
}

func TestSyncProjectToken_PrunesTheNewAllowlistsForRevokedMcps(t *testing.T) {
	s := &config.Settings{ExternalMcps: []config.ExternalMcp{{ID: "macmcp"}, {ID: "fsmcp"}}}
	proj := &config.Project{
		ID: "p1", Path: "/tmp/project", AllowedMcpIDs: []string{"macmcp"},
		Access:       map[string]string{"macmcp": config.AccessWrite, "fsmcp": config.AccessWrite},
		AllowedTools: map[string][]string{"macmcp": {"mail_*"}, "fsmcp": {"fs_*"}},
	}
	syncProjectToken(s, proj, McpSurfaces{"macmcp": macmcpSurface()})
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
	s := &config.Settings{ExternalMcps: []config.ExternalMcp{{ID: "fsmcp"}}}
	proj := &config.Project{ID: "p1", Path: "/tmp/project", AllowedMcpIDs: []string{"fsmcp"}}
	syncProjectToken(s, proj, McpSurfaces{"fsmcp": {Schema: json.RawMessage(`{"allowed_dirs":{"type":"array"}}`)}})
	if string(proj.Context["fsmcp"]) != `{"allowed_dirs":["/tmp/project"]}` {
		t.Fatalf("v1 derivation changed shape: %s", proj.Context["fsmcp"])
	}
}

func TestUpdateProjectAllowedTools_DropsEntriesForUngrantedMcps(t *testing.T) {
	s := &config.Settings{Projects: []config.Project{{ID: "p1", Kind: config.ProjectKindRemote, AllowedMcpIDs: []string{"macmcp"}}}}
	s.UpdateProjectAllowedTools("p1", map[string][]string{
		"macmcp": {"mail_*", "mail_*", ""},
		"fsmcp":  {"fs_read"},
	})
	proj, _ := config.FindProjectByID(s, "p1")
	if len(proj.AllowedTools) != 1 {
		t.Fatalf("allowlist kept an entry for an ungranted MCP: %v", proj.AllowedTools)
	}
	if got := proj.AllowedTools["macmcp"]; len(got) != 1 || got[0] != "mail_*" {
		t.Fatalf("duplicates and blanks were not cleaned: %v", got)
	}
	s.UpdateProjectAllowedTools("p1", nil)
	if proj, _ := config.FindProjectByID(s, "p1"); proj.AllowedTools != nil {
		t.Fatalf("clearing the allowlist left %v", proj.AllowedTools)
	}
}

// schemaHasField decides whether an MCP is filesystem-scoped, and a false
// negative fails OPEN: the grant is permitted, the second defence then declines
// to derive allowed_dirs for a remote project, the MCP receives nothing, and an
// MCP that reads an absent allowlist as "unrestricted" hands a client on another
// machine the whole host filesystem. So the answer must not depend on which of
// two equivalent spellings an MCP chose.
func TestSchemaHasField_DetectsBothSchemaShapes(t *testing.T) {
	tests := []struct {
		name   string
		schema string
		field  string
		want   bool
	}{
		{"flat, as fsMCP declares it", `{"allowed_dirs":{"type":"array"}}`, "allowed_dirs", true},
		{"nested under properties, the ordinary JSON-Schema shape",
			`{"type":"object","properties":{"allowed_dirs":{"type":"array"}}}`, "allowed_dirs", true},
		{"nested, field genuinely absent",
			`{"type":"object","properties":{"allowed_mailboxes":{"type":"array"}}}`, "allowed_dirs", false},
		{"flat, field genuinely absent", `{"allowed_mailboxes":{"type":"array"}}`, "allowed_dirs", false},
		{"a field literally named properties still matches flat first",
			`{"properties":{"type":"array"}}`, "properties", true},
		{"properties present but not an object", `{"properties":"nonsense"}`, "allowed_dirs", false},
		{"empty schema", ``, "allowed_dirs", false},
		{"malformed json", `{not valid`, "allowed_dirs", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := schemaHasField(json.RawMessage(tc.schema), tc.field); got != tc.want {
				t.Errorf("schemaHasField(%s, %q) = %v, want %v", tc.schema, tc.field, got, tc.want)
			}
		})
	}
}

// The same filesystem-scoped MCP must be refused a remote grant regardless of
// how it spelled its schema — the exact outcome ADR-009 decision 3 exists to
// prevent.
func TestValidateProjectGrants_RefusesFilesystemMcpInEitherSchemaShape(t *testing.T) {
	shapes := map[string]string{
		"flat":   `{"allowed_dirs":{"type":"array"}}`,
		"nested": `{"type":"object","properties":{"allowed_dirs":{"type":"array"}}}`,
	}
	for name, schema := range shapes {
		t.Run(name, func(t *testing.T) {
			s := &config.Settings{Projects: []config.Project{{
				ID: "p1", Name: "Remote", Kind: config.ProjectKindRemote, AllowedMcpIDs: []string{"fsmcp"},
			}}}
			err := ValidateGrants(&s.Projects[0], McpSurfaces{
				"fsmcp": {Schema: json.RawMessage(schema)},
			})
			if err == nil {
				t.Fatalf("%s schema: remote project was granted a filesystem-scoped MCP", name)
			}
			if !strings.Contains(err.Error(), "fsmcp") {
				t.Errorf("refusal should name the offending MCP, got: %v", err)
			}
		})
	}
}
