package project

import (
	"encoding/json"
	"maps"
	"reflect"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
)

type contextWideningRow struct {
	name      string
	stored    map[string]json.RawMessage
	requested map[string]json.RawMessage
	surfaces  McpSurfaces
	widens    bool
}

func blobs(kv ...string) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = json.RawMessage(kv[i+1])
	}
	return out
}

func runContextWideningRows(t *testing.T, rows []contextWideningRow) {
	t.Helper()
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			stored := config.Project{ID: "p1", Name: "Acme", Path: "/work/acme", Context: r.stored}
			requested := r.requested
			got := UpdateWidensGrant(stored, UpdateFields{Context: &requested}, r.surfaces)
			want := []string(nil)
			if r.widens {
				want = []string{"context"}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("UpdateWidensGrant = %v, want %v", got, want)
			}
		})
	}
}

func TestUpdateWidensGrant_ContextDerivedFields(t *testing.T) {
	mac := McpSurfaces{"macmcp": macmcpSurface()}
	fs := McpSurfaces{"fsmcp": fsmcpSurface()}
	macV1 := McpSurfaces{"macmcp": {Schema: json.RawMessage(macmcpSchema), SchemaVersion: 1}}
	fsV1 := McpSurfaces{"fsmcp": {Schema: json.RawMessage(fsmcpV2Schema), SchemaVersion: 1}}

	runContextWideningRows(t, []contextWideningRow{
		{"stored derived field beside an operator field is ignored",
			blobs("macmcp", `{"mail_accounts":["Alice"],"file_dirs":["/work/acme"]}`),
			blobs("macmcp", `{"mail_accounts":["Alice"]}`), mac, false},
		{"stored entry holding only a derived field is ignored",
			blobs("fsmcp", `{"allowed_dirs":["/work/acme"]}`),
			blobs(), fs, false},
		{"stored derived-only entry against a requested empty entry",
			blobs("fsmcp", `{"allowed_dirs":["/work/acme"]}`),
			blobs("fsmcp", `{}`), fs, false},
		{"nil surfaces strips nothing",
			blobs("macmcp", `{"mail_accounts":["Alice"],"file_dirs":["/work/acme"]}`),
			blobs("macmcp", `{"mail_accounts":["Alice"]}`), nil, true},
		{"unknown schema strips nothing",
			blobs("fsmcp", `{"allowed_dirs":["/work/acme"]}`),
			blobs(), McpSurfaces{"fsmcp": {Tools: []string{"fs_read"}}}, true},
		{"surfaces for another MCP strip nothing",
			blobs("fsmcp", `{"allowed_dirs":["/work/acme"]}`),
			blobs(), mac, true},
		{"v1 schema strips nothing",
			blobs("macmcp", `{"mail_accounts":["Alice"],"file_dirs":["/work/acme"]}`),
			blobs("macmcp", `{"mail_accounts":["Alice"]}`), macV1, true},
		{"v1 allowed_dirs strips nothing",
			blobs("fsmcp", `{"allowed_dirs":["/work/acme"]}`),
			blobs(), fsV1, true},
		{"stale stored key not in the schema still differs",
			blobs("macmcp", `{"mail_accounts":["Alice"],"retired_field":["x"],"file_dirs":["/work/acme"]}`),
			blobs("macmcp", `{"mail_accounts":["Alice"]}`), mac, true},
		{"request carrying the derived field still differs",
			blobs("macmcp", `{"mail_accounts":["Alice"],"file_dirs":["/work/acme"]}`),
			blobs("macmcp", `{"mail_accounts":["Alice"],"file_dirs":["/work/acme"]}`), mac, true},
		{"request adding a derived field where none was stored",
			blobs("fsmcp", `{}`),
			blobs("fsmcp", `{"allowed_dirs":["/"]}`), fs, true},
		{"changed operator field beside a derived field still widens",
			blobs("macmcp", `{"mail_accounts":["Alice"],"file_dirs":["/work/acme"]}`),
			blobs("macmcp", `{"mail_accounts":["Alice","Bob"]}`), mac, true},
		{"operator field removed beside a derived field is left unset, so it narrows",
			blobs("macmcp", `{"mail_accounts":["Alice"],"file_dirs":["/work/acme"]}`),
			blobs(), mac, false},
	})
}

const mailSchemaWithUnscopedField = `{
  "mail_accounts": {"type": "array", "items": {"type": "string"}, "scope": "restrict", "source": "operator"},
  "tags": {"type": "array", "items": {"type": "string"}, "description": "Labels shown to the operator"}
}`

const mailSchemaWithMalformedField = `{
  "mail_accounts": {"type": "array", "items": {"type": "string"}, "scope": "restrict", "source": "operator"},
  "broken": {"type": "array", "Scope": "restrict"}
}`

func TestUpdateWidensGrant_ContextNarrowing(t *testing.T) {
	mac := McpSurfaces{"macmcp": macmcpSurface()}
	macV1 := McpSurfaces{"macmcp": {Schema: json.RawMessage(macmcpSchema), SchemaVersion: 1}}
	unscoped := McpSurfaces{"macmcp": {Schema: json.RawMessage(mailSchemaWithUnscopedField), SchemaVersion: 2}}
	malformed := McpSurfaces{"macmcp": {Schema: json.RawMessage(mailSchemaWithMalformedField), SchemaVersion: 2}}

	if cs := unscoped.Schema("macmcp"); !cs.Usable() {
		t.Fatalf("unscoped fixture is unusable: %s", cs.MalformedReason())
	} else if f, ok := cs.Field("tags"); !ok || f.Restricts() {
		t.Fatalf("unscoped fixture: tags declared=%v restricts=%v, want a declared non-restrict field", ok, f.Restricts())
	}
	if malformed.Schema("macmcp").Usable() {
		t.Fatal("malformed fixture parsed as usable")
	}

	pair := `{"mail_accounts":["Alice","Bob"]}`
	runContextWideningRows(t, []contextWideningRow{
		{"removing one account narrows", blobs("macmcp", pair), blobs("macmcp", `{"mail_accounts":["Alice"]}`), mac, false},
		{"removing the field narrows",
			blobs("macmcp", `{"mail_accounts":["Alice"],"mail_mailboxes":["INBOX"]}`),
			blobs("macmcp", `{"mail_accounts":["Alice"]}`), mac, false},
		{"removing the whole MCP entry narrows", blobs("macmcp", pair), blobs(), mac, false},
		{"requested null narrows", blobs("macmcp", pair), blobs("macmcp", `{"mail_accounts":null}`), mac, false},
		{"requested empty string narrows", blobs("macmcp", pair), blobs("macmcp", `{"mail_accounts":""}`), mac, false},
		{"requested empty array against a stored list narrows", blobs("macmcp", pair), blobs("macmcp", `{"mail_accounts":[]}`), mac, false},
		{"requested empty array against an absent field widens",
			blobs("macmcp", `{"mail_mailboxes":["INBOX"]}`),
			blobs("macmcp", `{"mail_mailboxes":["INBOX"],"mail_accounts":[]}`), mac, true},
		{"a value where the stored field is null widens",
			blobs("macmcp", `{"mail_accounts":null}`), blobs("macmcp", `{"mail_accounts":["Alice"]}`), mac, true},
		{"reordering narrows", blobs("macmcp", pair), blobs("macmcp", `{"mail_accounts":["Bob","Alice"]}`), mac, false},
		{"de-duplicating narrows",
			blobs("macmcp", `{"mail_accounts":["Alice","Alice"]}`), blobs("macmcp", `{"mail_accounts":["Alice"]}`), mac, false},
		{"adding an element widens",
			blobs("macmcp", `{"mail_accounts":["Alice"]}`), blobs("macmcp", pair), mac, true},
		{"swapping an element for another widens",
			blobs("macmcp", `{"mail_accounts":["Alice"]}`), blobs("macmcp", `{"mail_accounts":["Bob"]}`), mac, true},
		{"one field narrowing beside another widening widens",
			blobs("macmcp", `{"mail_accounts":["Alice","Bob"],"mail_mailboxes":["INBOX"]}`),
			blobs("macmcp", `{"mail_accounts":["Alice"],"mail_mailboxes":["INBOX","Archive"]}`), mac, true},
		{"unchanged wildcard does not widen",
			blobs("macmcp", `{"mail_accounts":["*"]}`), blobs("macmcp", `{"mail_accounts":["*"]}`), mac, false},
		{"a list to the wildcard widens",
			blobs("macmcp", `{"mail_accounts":["Alice"]}`), blobs("macmcp", `{"mail_accounts":["*"]}`), mac, true},
		{"a mixed stored list to the wildcard widens",
			blobs("macmcp", `{"mail_accounts":["*","Bob"]}`), blobs("macmcp", `{"mail_accounts":["*"]}`), mac, true},
		{"the wildcard to a list narrows",
			blobs("macmcp", `{"mail_accounts":["*"]}`), blobs("macmcp", `{"mail_accounts":["Alice"]}`), mac, false},
		{"the wildcard to an empty array narrows",
			blobs("macmcp", `{"mail_accounts":["*"]}`), blobs("macmcp", `{"mail_accounts":[]}`), mac, false},
		{"a non-array value against a stored list widens",
			blobs("macmcp", pair), blobs("macmcp", `{"mail_accounts":"Alice"}`), mac, true},

		{"nil surfaces compare strictly", blobs("macmcp", pair), blobs("macmcp", `{"mail_accounts":["Alice"]}`), nil, true},
		{"a v1 schema compares strictly", blobs("macmcp", pair), blobs("macmcp", `{"mail_accounts":["Alice"]}`), macV1, true},
		{"an unusable schema compares strictly", blobs("macmcp", pair), blobs("macmcp", `{"mail_accounts":["Alice"]}`), malformed, true},
		{"a restrict field beside a non-restrict field still narrows",
			blobs("macmcp", pair), blobs("macmcp", `{"mail_accounts":["Alice"]}`), unscoped, false},
		{"a non-restrict field compares strictly",
			blobs("macmcp", `{"tags":["a","b"]}`), blobs("macmcp", `{"tags":["a"]}`), unscoped, true},
		{"removing a stale key compares strictly",
			blobs("macmcp", `{"mail_accounts":["Alice"],"retired_field":["x"]}`),
			blobs("macmcp", `{"mail_accounts":["Alice"]}`), mac, true},
	})

	for _, p := range []config.Project{
		{ID: "p1", Name: "Acme", Kind: config.ProjectKindRemote, Path: "/work/acme"},
		{ID: "p1", Name: "Acme", HostID: "devbox", Path: "/work/acme"},
	} {
		name := "remote"
		if p.IsHosted() {
			name = "hosted"
		}
		t.Run("a narrowed project_path field on a "+name+" project compares strictly", func(t *testing.T) {
			p.Context = blobs("macmcp", `{"mail_accounts":["Alice"],"file_dirs":["/work/acme","/work/other"]}`)
			requested := blobs("macmcp", `{"mail_accounts":["Alice"],"file_dirs":["/work/acme"]}`)
			got := UpdateWidensGrant(p, UpdateFields{Context: &requested}, mac)
			if want := []string{"context"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("UpdateWidensGrant = %v, want %v", got, want)
			}
		})
	}
}

func TestUpdateWidensGrant_ContextDerivedFieldMustMatchDerivation(t *testing.T) {
	mac := McpSurfaces{"macmcp": macmcpSurface()}
	fs := McpSurfaces{"fsmcp": fsmcpSurface()}
	local := config.Project{ID: "p1", Name: "Acme", Path: "/work/acme"}
	remote := config.Project{ID: "p1", Name: "Acme", Kind: config.ProjectKindRemote, Path: "/work/acme"}
	hosted := config.Project{ID: "p1", Name: "Acme", HostID: "devbox", Path: "/work/acme"}

	rows := []struct {
		name      string
		stored    config.Project
		context   map[string]json.RawMessage
		requested map[string]json.RawMessage
		surfaces  McpSurfaces
	}{
		{"stored derived value that differs from the path derivation", local,
			blobs("macmcp", `{"mail_accounts":["Alice"],"file_dirs":["/"]}`),
			blobs("macmcp", `{"mail_accounts":["Alice"]}`), mac},
		{"stored derived value with an extra root", local,
			blobs("fsmcp", `{"allowed_dirs":["/work/acme","/etc"]}`),
			blobs(), fs},
		{"stored derived value as a bare string where a list is derived", local,
			blobs("fsmcp", `{"allowed_dirs":"/work/acme"}`),
			blobs(), fs},
		{"remote project with a derived-looking field", remote,
			blobs("macmcp", `{"mail_accounts":["Alice"],"file_dirs":["/work/acme"]}`),
			blobs("macmcp", `{"mail_accounts":["Alice"]}`), mac},
		{"hosted project with a derived-looking field", hosted,
			blobs("fsmcp", `{"allowed_dirs":["/work/acme"]}`),
			blobs(), fs},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			stored := r.stored
			stored.Context = r.context
			requested := r.requested
			got := UpdateWidensGrant(stored, UpdateFields{Context: &requested}, r.surfaces)
			if want := []string{"context"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("UpdateWidensGrant = %v, want %v", got, want)
			}
		})
	}
}

func TestUpdateWidensGrant_ContextEmptyValues(t *testing.T) {
	runContextWideningRows(t, []contextWideningRow{
		{"null entry equals missing", blobs("macmcp", `null`), blobs(), nil, false},
		{"empty-object entry equals missing", blobs(), blobs("macmcp", `{}`), nil, false},
		{"zero-length entry equals missing", blobs("macmcp", ``), blobs(), nil, false},
		{"null entry equals empty-object entry", blobs("macmcp", `null`), blobs("macmcp", ` { } `), nil, false},
		{"whitespace and key order are not a change",
			blobs("macmcp", `{"mail_accounts":["Alice"],"mail_mailboxes":["INBOX"]}`),
			blobs("macmcp", `{ "mail_mailboxes": ["INBOX"], "mail_accounts": ["Alice"] }`), nil, false},
		{"field null vs empty array", blobs("macmcp", `{"mail_accounts":null}`), blobs("macmcp", `{"mail_accounts":[]}`), nil, true},
		{"field empty array vs null", blobs("macmcp", `{"mail_accounts":[]}`), blobs("macmcp", `{"mail_accounts":null}`), nil, true},
		{"field null vs missing", blobs("macmcp", `{"mail_accounts":null}`), blobs(), nil, true},
		{"field empty array vs missing", blobs("macmcp", `{"mail_accounts":[]}`), blobs("macmcp", `{}`), nil, true},
		{"field empty string vs missing", blobs("macmcp", `{"id":""}`), blobs(), nil, true},
		{"field empty string vs null", blobs("macmcp", `{"id":""}`), blobs("macmcp", `{"id":null}`), nil, true},
		{"whitespace inside a value", blobs("macmcp", `{"mail_accounts":["Alice"]}`), blobs("macmcp", `{"mail_accounts":["Alice "]}`), nil, true},
		{"type inside a value", blobs("macmcp", `{"limit":"123"}`), blobs("macmcp", `{"limit":123}`), nil, true},
		{"array order", blobs("macmcp", `{"mail_accounts":["Alice","Bob"]}`), blobs("macmcp", `{"mail_accounts":["Bob","Alice"]}`), nil, true},
		{"empty array entry is not dropped", blobs(), blobs("macmcp", `[]`), nil, true},
		{"undecodable requested blob", blobs(), blobs("macmcp", `{bad`), nil, true},
		{"undecodable stored blob is kept", blobs("macmcp", `{bad`), blobs(), nil, true},
		{"undecodable blobs that differ", blobs("macmcp", `{bad`), blobs("macmcp", `{worse`), nil, true},
		{"undecodable stored blob kept with v2 surfaces",
			blobs("macmcp", `{"file_dirs":`), blobs(), McpSurfaces{"macmcp": macmcpSurface()}, true},
	})
}

func TestComparableContext_NeverMutatesItsInput(t *testing.T) {
	surfaces := McpSurfaces{"macmcp": macmcpSurface(), "fsmcp": fsmcpSurface()}
	stored := blobs(
		"macmcp", `{"mail_accounts":["Alice"],"file_dirs":["/work/acme"]}`,
		"fsmcp", `{"allowed_dirs":["/work/acme"]}`,
		"emptymcp", `{}`,
		"nullmcp", `null`,
		"badmcp", `{bad`,
	)
	requested := blobs("macmcp", `{"mail_accounts":["Alice"]}`, "emptymcp", `{}`)
	storedBefore := cloneBlobs(stored)
	requestedBefore := cloneBlobs(requested)

	owner := config.Project{ID: "p1", Name: "Acme", Path: "/work/acme", Context: stored}
	out := comparableContext(stored, &owner, surfaces)
	if out == nil {
		t.Fatal("comparableContext returned nil, want a new map")
	}
	out["injected"] = json.RawMessage(`{}`)
	if _, leaked := stored["injected"]; leaked {
		t.Fatal("comparableContext returned its input map")
	}
	if got := comparableContext(nil, &owner, surfaces); got == nil {
		t.Fatal("comparableContext(nil) returned nil, want a new map")
	}

	UpdateWidensGrant(owner, UpdateFields{Context: &requested}, surfaces)

	if !reflect.DeepEqual(stored, storedBefore) {
		t.Fatalf("stored context mutated:\nbefore %s\nafter  %s", storedBefore, stored)
	}
	if !reflect.DeepEqual(requested, requestedBefore) {
		t.Fatalf("requested context mutated:\nbefore %s\nafter  %s", requestedBefore, requested)
	}
}

func cloneBlobs(in map[string]json.RawMessage) map[string]json.RawMessage {
	out := maps.Clone(in)
	for k, v := range out {
		out[k] = append(json.RawMessage(nil), v...)
	}
	return out
}
