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
		{"operator field removed beside a derived field still differs",
			blobs("macmcp", `{"mail_accounts":["Alice"],"file_dirs":["/work/acme"]}`),
			blobs(), mac, true},
	})
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
