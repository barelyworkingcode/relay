package mcpbroker

// A schema version relay reports is a version it holds a schema for.
//
// A surface reporting a version with no schema is the most dangerous state
// this type can hold, because project.ParseContextSchema(nil, 2) is a v2 schema with
// ZERO fields and nothing about it looks wrong: checkScopePresence finds no
// restrict field and passes every tool, project.FilterKnownContextFields finds
// nothing declared and strips every stored context key off the wire, and
// scopeFromMeta records no scope. Relay removes the confinement and reports
// that none was needed.

import (
	"testing"

	"github.com/barelyworkingcode/relay/internal/project"
)

// A minimal v2 context schema. cmd/relay's router_scope_test.go carries an
// identical const for the same reason the mock connection is duplicated: a
// _test.go file's symbols do not cross a package boundary.
const scopedSchema = `{
  "mail_accounts": {
    "type": "array", "items": {"type": "string"},
    "description": "Mail accounts this client may read from or send as",
    "scope": "restrict", "source": "operator",
    "applies_to": ["mail_*"], "enumerable": true
  }
}`

func TestMcpSurface_StopClearsTheVersionWithTheSchema(t *testing.T) {
	mgr := NewManager(nil)
	addMockConn(mgr, "macmcp", newMockConn("macmcp", simpleTools("mail_search"), nil))
	addMockSchema(mgr, "macmcp", scopedSchema, 2)

	if s := mgr.McpSurfaceFor("macmcp"); !surfaceHasSchema(s) || s.SchemaVersion != 2 {
		t.Fatalf("before Stop: %+v", s)
	}

	mgr.Stop("macmcp")

	// This is the state a reload whose handshake carries no schema lands in.
	if s := mgr.McpSurfaceFor("macmcp"); s.SchemaVersion != 0 {
		t.Fatalf("Stop left a schema version behind: %+v", s)
	}
	if s := mgr.AllMcpSurfaces()["macmcp"]; s.SchemaVersion != 0 {
		t.Fatalf("AllMcpSurfaces reported a version after Stop: %+v", s)
	}
}

// TestMcpSurface_AVersionIsNeverReportedWithoutItsSchema is the second half,
// and it is what makes the next leak of this kind unable to reach a caller: a
// missing schema decides the surface on its own rather than the two maps having
// to stay in agreement.
func TestMcpSurface_AVersionIsNeverReportedWithoutItsSchema(t *testing.T) {
	mgr := NewManager(nil)
	addMockConn(mgr, "macmcp", newMockConn("macmcp", simpleTools("mail_search"), nil))
	mgr.mu.Lock()
	mgr.schemaVersions["macmcp"] = 2
	mgr.mu.Unlock()

	for name, s := range map[string]project.McpSurface{
		"McpSurfaceFor":   mgr.McpSurfaceFor("macmcp"),
		"AllMcpSurfaces":  mgr.AllMcpSurfaces()["macmcp"],
		"empty-schema":    mgr.McpSurfaceFor("never-connected"),
		"AllMcpSurfaces2": mgr.AllMcpSurfaces()["never-connected"],
	} {
		if s.SchemaVersion != 0 {
			t.Errorf("%s reported version %d with no schema", name, s.SchemaVersion)
		}
		cs := project.ParseContextSchema(s.Schema, s.SchemaVersion)
		if cs.V2() {
			t.Errorf("%s parsed as v2 with no schema: a zero-field v2 schema passes every scope check and strips every context key", name)
		}
	}
}

// A surface with no bytes is what "relay knows nothing about this MCP"
// looks like.
func surfaceHasSchema(s project.McpSurface) bool { return len(s.Schema) > 0 }
