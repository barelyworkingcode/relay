//go:build !windows

package main

// Subtle: every non-ok enumeration result must carry a nil Values — nil vs []
// is what separates "could not answer" from "there are none."

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/mcp"
	"github.com/barelyworkingcode/relay/internal/project"
)

func enumSurfaces() project.McpSurfaces {
	return project.McpSurfaces{"macmcp": {
		SchemaVersion: project.ContextSchemaV2,
		Tools:         []string{"mail_search", "mail_save_attachment"},
		Schema: json.RawMessage(`{
			"mail_accounts": {"type":"array","items":{"type":"string"},
				"scope":"restrict","source":"operator","applies_to":["mail_*"],"enumerable":true},
			"mail_mailboxes": {"type":"array","items":{"type":"string"},
				"scope":"restrict","source":"operator","applies_to":["mail_*"],"enumerable":true,
				"depends_on":["mail_accounts"]},
			"mail_note": {"type":"string","scope":"restrict","source":"operator","applies_to":["mail_*"]},
			"file_dirs": {"type":"array","items":{"type":"string"},
				"scope":"restrict","source":"project_path","applies_to":["mail_save_attachment"]}
		}`),
	}}
}

// Records calls so a client that silently sent the whole form (or an empty
// dependency list) doesn't look correct from the outside.
type fakeEnumerator struct {
	calls  []fakeEnumCall
	result project.ContextEnumResult
}

type fakeEnumCall struct {
	mcpID  string
	field  string
	values map[string]json.RawMessage
}

func (f *fakeEnumerator) EnumerateContextField(_ context.Context, mcpID, field string, values map[string]json.RawMessage) project.ContextEnumResult {
	f.calls = append(f.calls, fakeEnumCall{mcpID: mcpID, field: field, values: values})
	res := f.result
	res.McpID, res.Field = mcpID, field
	return res
}

func okEnum(values ...string) *fakeEnumerator {
	out := make([]project.ContextEnumValue, 0, len(values))
	for _, v := range values {
		raw, _ := json.Marshal(v)
		out = append(out, project.ContextEnumValue{Value: raw, Label: v})
	}
	return &fakeEnumerator{result: project.ContextEnumResult{Status: project.EnumStatusOK, Values: out}}
}

// Deliberate: a refused field never reaches the MCP, so a name typed into the
// URL cannot become a probe.
func TestEnumerate_RelaysOwnRefusalsNeverReachTheMcp(t *testing.T) {
	cases := []struct {
		name, mcpID, field, wantStatus string
	}{
		{"an MCP relay has never connected to", "ghostmcp", "mail_accounts", project.EnumStatusUnknownMcp},
		{"a field the MCP does not declare", "macmcp", "nosuch", project.EnumStatusNotEnumerable},
		{"a declared field that is not enumerable", "macmcp", "mail_note", project.EnumStatusNotEnumerable},
		{"a field relay derives from the project path", "macmcp", "file_dirs", project.EnumStatusNotEnumerable},
		{"no field at all", "macmcp", "", project.EnumStatusNotEnumerable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			enum := okEnum("Alice")
			res := project.EnumerateScopeField(context.Background(), enumSurfaces(), enum, c.mcpID, c.field, nil)
			if res.Status != c.wantStatus {
				t.Errorf("status = %q, want %q (%s)", res.Status, c.wantStatus, res.Error)
			}
			if len(enum.calls) != 0 {
				t.Errorf("relay asked the MCP anyway: %+v", enum.calls)
			}
			if res.Values != nil {
				t.Errorf("a refusal carried a value list, which renders as 'there are none': %v", res.Values)
			}
			if res.Error == "" {
				t.Error("a refusal with no reason is one an operator answers by guessing")
			}
		})
	}
}

func TestEnumerate_NoProviderIsUnavailableNotEmpty(t *testing.T) {
	res := project.EnumerateScopeField(context.Background(), enumSurfaces(), nil, "macmcp", "mail_accounts", nil)
	if res.Status != project.EnumStatusUnavailable || res.Values != nil {
		t.Fatalf("got %q values=%v, want unavailable with no list", res.Status, res.Values)
	}
}

// Subtle: an empty dependency value is dropped, not sent as []. Sending
// {"mail_accounts":[]} could be read server-side as "match nothing", making
// the picker's initial state look like a mailbox-less host.
func TestEnumerate_SendsOnlyDeclaredDependenciesAndDropsEmptyOnes(t *testing.T) {
	chosen := map[string]json.RawMessage{
		"mail_accounts":   json.RawMessage(`["Bob"]`),
		"mail_note":       json.RawMessage(`"not a dependency of this field"`),
		"unrelated_field": json.RawMessage(`["x"]`),
	}
	enum := okEnum("INBOX")
	project.EnumerateScopeField(context.Background(), enumSurfaces(), enum, "macmcp", "mail_mailboxes", chosen)
	if len(enum.calls) != 1 {
		t.Fatalf("want one call, got %d", len(enum.calls))
	}
	got := enum.calls[0].values
	if len(got) != 1 || string(got["mail_accounts"]) != `["Bob"]` {
		t.Fatalf("relay sent %v, want exactly the declared dependency", got)
	}

	for _, empty := range []string{`[]`, `null`, `""`, `{}`} {
		t.Run("unchosen "+empty, func(t *testing.T) {
			enum := okEnum("INBOX")
			project.EnumerateScopeField(context.Background(), enumSurfaces(), enum, "macmcp", "mail_mailboxes",
				map[string]json.RawMessage{"mail_accounts": json.RawMessage(empty)})
			if v := enum.calls[0].values; len(v) != 0 {
				t.Fatalf("an unchosen dependency was sent as %v; the server may read that as 'match nothing'", v)
			}
		})
	}

	enum = okEnum("Alice", "Bob")
	project.EnumerateScopeField(context.Background(), enumSurfaces(), enum, "macmcp", "mail_accounts", chosen)
	if v := enum.calls[0].values; len(v) != 0 {
		t.Fatalf("a field declaring no depends_on was sent %v", v)
	}
}

func TestEnumerateRoute_HTTP(t *testing.T) {
	cases := []struct {
		name       string
		mcpID      string
		body       string
		enum       project.ContextEnumerator
		wantCode   int
		wantStatus string
		wantValues string
	}{
		{
			name: "a real answer", mcpID: "macmcp",
			body: `{"field":"mail_accounts"}`, enum: okEnum("Alice", "Bob"),
			wantCode: http.StatusOK, wantStatus: project.EnumStatusOK,
			wantValues: `"values":[{"value":"Alice","label":"Alice"},{"value":"Bob","label":"Bob"}]`,
		},
		{
			name: "an answer with nothing in it", mcpID: "macmcp",
			body: `{"field":"mail_accounts"}`, enum: okEnum(),
			wantCode: http.StatusOK, wantStatus: project.EnumStatusOK, wantValues: `"values":[]`,
		},
		{
			name: "an MCP that does not implement enumeration", mcpID: "macmcp",
			body: `{"field":"mail_accounts"}`,
			enum: &fakeEnumerator{result: project.ContextEnumResult{Status: project.EnumStatusUnsupported, Error: "no such method"}},
			// 200: this is a true final answer about the MCP, not a
			// failure — the caller renders a text box permanently.
			wantCode: http.StatusOK, wantStatus: project.EnumStatusUnsupported, wantValues: `"values":null`,
		},
		{
			name: "the MCP refusing the request relay built", mcpID: "macmcp",
			body: `{"field":"mail_accounts"}`,
			enum: &fakeEnumerator{result: project.ContextEnumResult{Status: project.EnumStatusInvalidField, Error: "not enumerable"}},
			// 502: the failure is on relay's side of the operator.
			wantCode: http.StatusBadGateway, wantStatus: project.EnumStatusInvalidField, wantValues: `"values":null`,
		},
		{
			name: "the MCP not answering right now", mcpID: "macmcp",
			body:     `{"field":"mail_accounts"}`,
			enum:     &fakeEnumerator{result: project.ContextEnumResult{Status: project.EnumStatusUnavailable, Error: "Mail timed out"}},
			wantCode: http.StatusServiceUnavailable, wantStatus: project.EnumStatusUnavailable, wantValues: `"values":null`,
		},
		{
			name: "an MCP relay has never connected to", mcpID: "ghostmcp",
			body: `{"field":"mail_accounts"}`, enum: okEnum("Alice"),
			wantCode: http.StatusNotFound, wantStatus: project.EnumStatusUnknownMcp, wantValues: `"values":null`,
		},
		{
			name: "a field the MCP never said it could enumerate", mcpID: "macmcp",
			body: `{"field":"file_dirs"}`, enum: okEnum("Alice"),
			wantCode: http.StatusBadRequest, wantStatus: project.EnumStatusNotEnumerable, wantValues: `"values":null`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
			if err := store.EnsureInitialized(); err != nil {
				t.Fatalf("EnsureInitialized: %v", err)
			}
			mux := http.NewServeMux()
			RegisterProjectRoutes(&control.RouteRegistrar{CredentialID: APICredentialIDFromContext, Mux: mux, Transport: control.TransportSocket}, store, &ProjectOps{Store: store}, schemaProviderFunc(enumSurfaces), nil, c.enum, nil, nil)
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)

			resp, body := doJSON(t, "POST", srv.URL+"/api/mcps/"+c.mcpID+"/enumerate", json.RawMessage(c.body))
			if resp.StatusCode != c.wantCode {
				t.Errorf("HTTP %d, want %d: %s", resp.StatusCode, c.wantCode, body)
			}
			if !strings.Contains(string(body), `"status":"`+c.wantStatus+`"`) {
				t.Errorf("body does not carry status %q: %s", c.wantStatus, body)
			}
			if !strings.Contains(string(body), c.wantValues) {
				t.Errorf("body does not carry %s: %s", c.wantValues, body)
			}
		})
	}
}

func TestEnumerateRoute_HTTPCarriesTheChosenDependencies(t *testing.T) {
	store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	enum := okEnum("INBOX")
	mux := http.NewServeMux()
	RegisterProjectRoutes(&control.RouteRegistrar{CredentialID: APICredentialIDFromContext, Mux: mux, Transport: control.TransportSocket}, store, &ProjectOps{Store: store}, schemaProviderFunc(enumSurfaces), nil, enum, nil, nil)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, body := doJSON(t, "POST", srv.URL+"/api/mcps/macmcp/enumerate",
		json.RawMessage(`{"field":"mail_mailboxes","values":{"mail_accounts":["Bob"]}}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
	}
	if len(enum.calls) != 1 || string(enum.calls[0].values["mail_accounts"]) != `["Bob"]` {
		t.Fatalf("the chosen dependency did not reach the MCP: %+v", enum.calls)
	}
}

// Enumeration is disclosure — every mail account on this machine — so it sits
// behind the same authentication as any other project route.
func TestEnumerateRoute_RequiresAuthentication(t *testing.T) {
	_, sock := newTestFrontendServer(t, "the-token")
	client := dialFrontendHTTP(sock)

	req, _ := http.NewRequest(http.MethodPost, "http://unix/api/mcps/macmcp/enumerate",
		strings.NewReader(`{"field":"mail_accounts"}`))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated enumerate = %d, want 401", resp.StatusCode)
	}
}

// remoteHandlers has exactly two entries: ListTools and CallTool.
func TestEnumerate_IsNotAnythingARemoteClientCanCall(t *testing.T) {
	for _, name := range []string{mcp.MethodContextEnumerate, MsgEnumerateScopeField, "enumerate"} {
		if _, ok := remoteHandlers[name]; ok {
			t.Fatalf("%q reached the remote dispatch table", name)
		}
	}
	if len(remoteHandlers) != 2 {
		t.Fatalf("the remote dispatch table grew to %d entries; enumeration must not be one of them", len(remoteHandlers))
	}
}

func TestEnumerate_IPC(t *testing.T) {
	ipc, _, ui, _ := newProjectsIPC(t)
	ipc.Tools = &fakeTools{surfaces: enumSurfaces()}
	enum := okEnum("Alice", "Bob")
	ipc.Enumerate = enum

	ipcEnumerateScopeField(ipc, json.RawMessage(`{"type":"enumerate_scope_field","mcp_id":"macmcp","field":"mail_mailboxes","values":{"mail_accounts":["Bob"]}}`))

	args, ok := findEvent(ui, "onScopeFieldEnumerated")
	if !ok {
		t.Fatal("no onScopeFieldEnumerated event")
	}
	var res project.ContextEnumResult
	if err := json.Unmarshal(args[0].(json.RawMessage), &res); err != nil {
		t.Fatalf("event payload: %v", err)
	}
	if !res.OK() || res.Field != "mail_mailboxes" || len(res.Values) != 2 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(enum.calls) != 1 || string(enum.calls[0].values["mail_accounts"]) != `["Bob"]` {
		t.Fatalf("the chosen dependency did not reach the MCP: %+v", enum.calls)
	}

	ipc.Enumerate = &fakeEnumerator{result: project.ContextEnumResult{Status: project.EnumStatusUnavailable, Error: "Mail timed out"}}
	ipcEnumerateScopeField(ipc, json.RawMessage(`{"type":"enumerate_scope_field","mcp_id":"macmcp","field":"mail_accounts"}`))
	args, _ = findEvent(ui, "onScopeFieldEnumerated")
	res = project.ContextEnumResult{}
	if err := json.Unmarshal(args[0].(json.RawMessage), &res); err != nil {
		t.Fatalf("event payload: %v", err)
	}
	if res.Status != project.EnumStatusUnavailable || res.Values != nil {
		t.Fatalf("a failure reached the UI as %+v", res)
	}
}

func TestEnumerate_IPCWithNoProvider(t *testing.T) {
	ipc, _, ui, _ := newProjectsIPC(t)
	ipc.Tools = &fakeTools{surfaces: enumSurfaces()}
	ipc.Enumerate = nil

	ipcEnumerateScopeField(ipc, json.RawMessage(`{"type":"enumerate_scope_field","mcp_id":"macmcp","field":"mail_accounts"}`))
	args, ok := findEvent(ui, "onScopeFieldEnumerated")
	if !ok {
		t.Fatal("no event emitted")
	}
	var res project.ContextEnumResult
	_ = json.Unmarshal(args[0].(json.RawMessage), &res)
	if res.Status != project.EnumStatusUnavailable || res.Values != nil {
		t.Fatalf("got %+v", res)
	}
}
