package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
	"github.com/barelyworkingcode/relay/internal/project"
)

const traceTestID = "abcd1234efgh5678"

func TestMergeTraceID(t *testing.T) {
	cases := []struct {
		name, base, id, want string
	}{
		{"adds to an existing _meta", `{"project_id":"p1"}`, traceTestID, `{"project_id":"p1","trace_id":"` + traceTestID + `"}`},
		{"never creates _meta from nothing", ``, traceTestID, ``},
		{"never creates _meta from null", `null`, traceTestID, `null`},
		{"never fills an empty object", `{}`, traceTestID, `{}`},
		{"never replaces a present key", `{"trace_id":"theirs"}`, traceTestID, `{"trace_id":"theirs"}`},
		{"ignores an invalid id", `{"project_id":"p1"}`, "short", `{"project_id":"p1"}`},
		{"ignores an empty id", `{"project_id":"p1"}`, "", `{"project_id":"p1"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mergeTraceID(json.RawMessage(c.base), c.id)
			if string(got) != c.want {
				t.Errorf("mergeTraceID = %s, want %s", got, c.want)
			}
		})
	}
}

func TestTraceForMcp(t *testing.T) {
	settings := &config.Settings{ExternalMcps: []config.ExternalMcp{
		{ID: "stdio", Transport: "stdio", Command: "x"},
		{ID: "local", Transport: "http", URL: "http://localhost:9000/mcp"},
		{ID: "loop6", Transport: "http", URL: "http://[::1]:9000/mcp"},
		{ID: "remote", Transport: "http", URL: "https://example.com/mcp"},
		{ID: "lan", Transport: "http", URL: "http://10.0.0.1/mcp"},
		{ID: "lookalike", Transport: "http", URL: "http://localhost.example.com/mcp"},
		{ID: "broken", Transport: "http", URL: "http://%zz"},
	}}
	withTrace := logging.ContextWithTrace(context.Background(), traceTestID)
	noField := project.ParseContextSchema(nil, 0)
	declares := project.ParseContextSchema(json.RawMessage(`{"trace_id":{"type":"string","description":"theirs","source":"operator"}}`), 2)

	cases := []struct {
		name   string
		ctx    context.Context
		id     string
		schema project.ContextSchema
		want   string
	}{
		{"stdio carries it", withTrace, "stdio", noField, traceTestID},
		{"localhost carries it", withTrace, "local", noField, traceTestID},
		{"ipv6 loopback carries it", withTrace, "loop6", noField, traceTestID},
		{"example.com does not", withTrace, "remote", noField, ""},
		{"private address does not", withTrace, "lan", noField, ""},
		{"lookalike host does not", withTrace, "lookalike", noField, ""},
		{"unparseable URL does not", withTrace, "broken", noField, ""},
		{"unknown MCP does not", withTrace, "nope", noField, ""},
		{"no trace in ctx", context.Background(), "stdio", noField, ""},
		{"schema declaring trace_id", withTrace, "stdio", declares, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := traceForMcp(c.ctx, settings, c.id, c.schema); got != c.want {
				t.Errorf("traceForMcp = %q, want %q", got, c.want)
			}
		})
	}
}

// callMeta runs one CallTool and returns the _meta the MCP received.
func callMeta(t *testing.T, ctx context.Context, schema string, grant map[string]json.RawMessage) json.RawMessage {
	meta, _ := callMetaAndScope(t, ctx, schema, grant)
	return meta
}

func callMetaAndScope(t *testing.T, ctx context.Context, schema string, grant map[string]json.RawMessage) (json.RawMessage, string) {
	t.Helper()
	var meta json.RawMessage
	capture := func(_ context.Context, _ string, params interface{}) (json.RawMessage, error) {
		meta, _ = json.Marshal(decodedToolParams(params)["_meta"])
		return json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`), nil
	}
	r := newProfileRouter(t, profileOpts{
		kind:          config.ProjectKindRemote,
		allowedTools:  map[string][]string{"macmcp": {"mail_*"}},
		access:        map[string]string{"macmcp": config.AccessWrite},
		contextValues: grant,
		schema:        schema,
		schemaVersion: 2,
	})
	mgr := mcpbroker.NewManager(nil)
	addMockConn(mgr, "macmcp", newMockConn("macmcp", macmcpToolSurface(), capture))
	addMockSchema(mgr, "macmcp", schema, 2)
	r.tools = mgr
	rec := newTestAudit(t, nil)
	r.audit = rec
	if _, err := r.CallTool(ctx, "mail_search", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	scope, _ := json.Marshal(onlyEvent(t, readLoggedEvents(t, rec)).Scope)
	return meta, string(scope)
}

func metaFields(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("_meta %q: %v", raw, err)
	}
	return m
}

func TestCallToolMetaCarriesTraceIDAndKeepsTheRest(t *testing.T) {
	grant := map[string]json.RawMessage{"mail_accounts": json.RawMessage(`["Bob"]`)}
	sha := bridge.WithArgsSHA256(context.Background(), "deadbeef")

	plainRaw, plainScope := callMetaAndScope(t, sha, scopedSchema, grant)
	tracedRaw, tracedScope := callMetaAndScope(t, logging.ContextWithTrace(sha, traceTestID), scopedSchema, grant)
	plain, traced := metaFields(t, plainRaw), metaFields(t, tracedRaw)
	if plainScope != tracedScope || plainScope == "null" {
		t.Errorf("audit scope changed by trace: plain %s, traced %s", plainScope, tracedScope)
	}

	if _, has := plain["trace_id"]; has {
		t.Fatalf("_meta carries trace_id with no trace in ctx: %v", plain)
	}
	if string(traced["trace_id"]) != `"`+traceTestID+`"` {
		t.Fatalf("trace_id = %s, want %q", traced["trace_id"], traceTestID)
	}
	delete(traced, "trace_id")
	if len(traced) != len(plain) {
		t.Fatalf("other _meta keys changed: plain %v, traced %v", plain, traced)
	}
	for _, k := range []string{"project_id", "args_sha256", "mail_accounts"} {
		if string(plain[k]) == "" || string(traced[k]) != string(plain[k]) {
			t.Errorf("_meta[%s] plain %s, traced %s", k, plain[k], traced[k])
		}
	}
}

func TestCallToolSchemaFieldNamedTraceIDIsLeftAlone(t *testing.T) {
	const schema = `{"trace_id":{"type":"string","description":"the MCP's own","source":"operator"}}`
	ctx := logging.ContextWithTrace(context.Background(), traceTestID)

	got := metaFields(t, callMeta(t, ctx, schema, map[string]json.RawMessage{"trace_id": json.RawMessage(`"grant-value"`)}))
	if string(got["trace_id"]) != `"grant-value"` {
		t.Errorf("trace_id = %s, want the grant's value untouched", got["trace_id"])
	}
	got = metaFields(t, callMeta(t, ctx, schema, nil))
	if v, has := got["trace_id"]; has {
		t.Errorf("relay supplied trace_id %s to an MCP that declares its own", v)
	}
}
