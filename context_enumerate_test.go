//go:build !windows

package main

// ADR-011 decision 6, relay's half: asking a connected MCP what a scope
// field's real values are, and — the part that actually matters — behaving
// differently for each way that can fail.
//
// The failure modes are the subject of most of this file because the hazard is
// asymmetric. A picker that renders an empty list when the call FAILED tells an
// operator there are no mailboxes on a machine full of them, and the profile
// they then save confines nothing they intended. So every non-ok status must
// arrive with a nil Values, and each must be distinguishable from the others.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"relaygo/jsonrpc"
	"relaygo/mcp"
)

// enumSurfaces is what relay knows about the MCP in these tests: macMCP's
// worked example, the same declaration cmd/testmcp publishes.
func enumSurfaces() McpSurfaces {
	return McpSurfaces{"macmcp": {
		SchemaVersion: contextSchemaV2,
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

// fakeEnumerator records what relay actually sent and answers with whatever the
// test wants back. The recording is half the point: the request shape is pinned
// with macMCP, and a client that quietly sent the whole form or an empty
// dependency list would still look correct from the outside.
type fakeEnumerator struct {
	calls  []fakeEnumCall
	result ContextEnumResult
}

type fakeEnumCall struct {
	mcpID  string
	field  string
	values map[string]json.RawMessage
}

func (f *fakeEnumerator) EnumerateContextField(_ context.Context, mcpID, field string, values map[string]json.RawMessage) ContextEnumResult {
	f.calls = append(f.calls, fakeEnumCall{mcpID: mcpID, field: field, values: values})
	res := f.result
	res.McpID, res.Field = mcpID, field
	return res
}

func okEnum(values ...string) *fakeEnumerator {
	out := make([]ContextEnumValue, 0, len(values))
	for _, v := range values {
		raw, _ := json.Marshal(v)
		out = append(out, ContextEnumValue{Value: raw, Label: v})
	}
	return &fakeEnumerator{result: ContextEnumResult{Status: EnumStatusOK, Values: out}}
}

// ---------------------------------------------------------------------------
// What relay refuses on its own, before any MCP is asked
// ---------------------------------------------------------------------------

// Decision 6 honours enumeration "for those fields only" — the ones declaring
// enumerable: true. Everything else is refused here, and the MCP is never
// contacted, so a field that is merely typed into a URL cannot become a probe.
func TestEnumerate_RelaysOwnRefusalsNeverReachTheMcp(t *testing.T) {
	cases := []struct {
		name, mcpID, field, wantStatus string
	}{
		{"an MCP relay has never connected to", "ghostmcp", "mail_accounts", EnumStatusUnknownMcp},
		{"a field the MCP does not declare", "macmcp", "nosuch", EnumStatusNotEnumerable},
		{"a declared field that is not enumerable", "macmcp", "mail_note", EnumStatusNotEnumerable},
		{"a field relay derives from the project path", "macmcp", "file_dirs", EnumStatusNotEnumerable},
		{"no field at all", "macmcp", "", EnumStatusNotEnumerable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			enum := okEnum("Alice")
			res := enumerateScopeField(context.Background(), enumSurfaces(), enum, c.mcpID, c.field, nil)
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

// No enumerator wired (a test context, or a build without the MCP manager) is
// "could not answer right now", not "there are none" and not a 404: the field
// still exists and the editor's fallback is still the text box.
func TestEnumerate_NoProviderIsUnavailableNotEmpty(t *testing.T) {
	res := enumerateScopeField(context.Background(), enumSurfaces(), nil, "macmcp", "mail_accounts", nil)
	if res.Status != EnumStatusUnavailable || res.Values != nil {
		t.Fatalf("got %q values=%v, want unavailable with no list", res.Status, res.Values)
	}
}

// ---------------------------------------------------------------------------
// The request shape, which is pinned with macMCP
// ---------------------------------------------------------------------------

// Only the fields THIS field declares in depends_on are sent, and an empty
// choice is not sent at all.
//
// The second half is the one with teeth. The picker's normal initial state is
// mail_mailboxes open with mail_accounts still unchosen, and a request carrying
// {"mail_accounts": []} invites the server to read it as "match nothing" —
// which is an empty picker at exactly the moment an operator opens one, and
// indistinguishable from a host with no mailboxes. macMCP fixed the server side
// of this before shipping; omitting it is the client side.
func TestEnumerate_SendsOnlyDeclaredDependenciesAndDropsEmptyOnes(t *testing.T) {
	chosen := map[string]json.RawMessage{
		"mail_accounts":   json.RawMessage(`["Bob"]`),
		"mail_note":       json.RawMessage(`"not a dependency of this field"`),
		"unrelated_field": json.RawMessage(`["x"]`),
	}
	enum := okEnum("INBOX")
	enumerateScopeField(context.Background(), enumSurfaces(), enum, "macmcp", "mail_mailboxes", chosen)
	if len(enum.calls) != 1 {
		t.Fatalf("want one call, got %d", len(enum.calls))
	}
	got := enum.calls[0].values
	if len(got) != 1 || string(got["mail_accounts"]) != `["Bob"]` {
		t.Fatalf("relay sent %v, want exactly the declared dependency", got)
	}

	// Every shape of "unchosen" is the same request: no values at all.
	for _, empty := range []string{`[]`, `null`, `""`, `{}`} {
		t.Run("unchosen "+empty, func(t *testing.T) {
			enum := okEnum("INBOX")
			enumerateScopeField(context.Background(), enumSurfaces(), enum, "macmcp", "mail_mailboxes",
				map[string]json.RawMessage{"mail_accounts": json.RawMessage(empty)})
			if v := enum.calls[0].values; len(v) != 0 {
				t.Fatalf("an unchosen dependency was sent as %v; the server may read that as 'match nothing'", v)
			}
		})
	}

	// A field with no depends_on is never told about anything, whatever the
	// operator has already picked elsewhere.
	enum = okEnum("Alice", "Bob")
	enumerateScopeField(context.Background(), enumSurfaces(), enum, "macmcp", "mail_accounts", chosen)
	if v := enum.calls[0].values; len(v) != 0 {
		t.Fatalf("a field declaring no depends_on was sent %v", v)
	}
}

// ---------------------------------------------------------------------------
// Classifying what the MCP answers
// ---------------------------------------------------------------------------

// mgrWithConn wires a mock connection into a real manager so the classification
// runs against ExternalMcpManager.EnumerateContextField itself — the latch and
// the parse live there, not in the seam above it.
func mgrWithConn(t *testing.T, sendFn func(context.Context, string, interface{}) (json.RawMessage, error)) (*ExternalMcpManager, *int) {
	t.Helper()
	calls := 0
	m := NewExternalMcpManager(nil)
	t.Cleanup(m.StopAll)
	addMockConn(m, "macmcp", newMockConn("macmcp", nil, func(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
		calls++
		return sendFn(ctx, method, params)
	}))
	return m, &calls
}

func rpcErrConn(code int, msg string) func(context.Context, string, interface{}) (json.RawMessage, error) {
	return func(context.Context, string, interface{}) (json.RawMessage, error) {
		return nil, formatJSONRPCError(&jsonrpc.Error{Code: code, Message: msg})
	}
}

// -32601: the MCP does not implement enumeration. Degrade to text entry, and
// stop asking — silently and permanently, for the life of the connection.
func TestEnumerate_MethodNotFoundDegradesAndLatches(t *testing.T) {
	m, calls := mgrWithConn(t, rpcErrConn(jsonrpc.CodeMethodNotFound, "method not found"))

	res := m.EnumerateContextField(context.Background(), "macmcp", "mail_accounts", nil)
	if res.Status != EnumStatusUnsupported {
		t.Fatalf("status = %q, want %q", res.Status, EnumStatusUnsupported)
	}
	if res.Values != nil {
		t.Error("an MCP that does not enumerate was reported as having no values")
	}
	if *calls != 1 {
		t.Fatalf("want one request, got %d", *calls)
	}

	// Asked again — for a different field, which is how a panel opens — and
	// answered from the latch without a second round trip.
	res = m.EnumerateContextField(context.Background(), "macmcp", "mail_mailboxes", nil)
	if res.Status != EnumStatusUnsupported {
		t.Fatalf("second call status = %q", res.Status)
	}
	if *calls != 1 {
		t.Fatalf("relay re-asked an MCP that already said it does not implement the method (%d requests)", *calls)
	}

	// The latch belongs to the CONNECTION, not to the MCP's id: a reconnect is
	// a new process and possibly a new build.
	m.Stop("macmcp")
	m.mu.RLock()
	latched := m.enumUnsupported["macmcp"]
	m.mu.RUnlock()
	if latched {
		t.Error("the -32601 latch survived the connection that asserted it")
	}
}

// -32602: relay asked for a field the MCP will not enumerate. That is a RELAY
// bug — relay is meant to ask only for fields declaring enumerable: true — and
// it is surfaced rather than degraded, because degrading hides it behind a
// text box that looks like it was meant to be there.
func TestEnumerate_InvalidParamsIsSurfacedNotDegraded(t *testing.T) {
	m, _ := mgrWithConn(t, rpcErrConn(jsonrpc.CodeInvalidParams, "no enumerable field named mail_mailboxes"))
	res := m.EnumerateContextField(context.Background(), "macmcp", "mail_mailboxes", nil)

	if res.Status != EnumStatusInvalidField {
		t.Fatalf("status = %q, want %q", res.Status, EnumStatusInvalidField)
	}
	if !strings.Contains(res.Error, "no enumerable field named mail_mailboxes") {
		t.Errorf("the MCP's own reason was thrown away: %q", res.Error)
	}
	// It must NOT latch: this says nothing about whether the MCP implements
	// the method, only that relay asked it the wrong question.
	m.mu.RLock()
	latched := m.enumUnsupported["macmcp"]
	m.mu.RUnlock()
	if latched {
		t.Error("a bad request from relay was recorded as the MCP not implementing enumeration")
	}
}

// Everything else is "could not answer right now": keep text entry, offer a
// retry, never claim there are none.
//
// The -32000 row is not hypothetical. macMCP answers in JSON-RPC's
// implementation-defined server-error range when Mail itself will not answer,
// which is exactly the transient condition a retry fixes — so relay recognises
// exactly two codes and treats every other one this way, rather than listing
// the ones it has seen.
func TestEnumerate_EverythingElseIsRetryable(t *testing.T) {
	cases := []struct {
		name string
		send func(context.Context, string, interface{}) (json.RawMessage, error)
	}{
		{"-32000, the server-error range macMCP uses for a mail read that failed",
			rpcErrConn(-32000, "could not read mailboxes: Mail timed out")},
		{"-32099, the other end of that range", rpcErrConn(-32099, "server error")},
		{"-32603 internal error", rpcErrConn(jsonrpc.CodeInternalError, "internal error")},
		{"-32700 parse error", rpcErrConn(jsonrpc.CodeParseError, "parse error")},
		{"a transport failure with no JSON-RPC error at all", func(context.Context, string, interface{}) (json.RawMessage, error) {
			return nil, errors.New("read response: EOF")
		}},
		{"an answer that is not an enumeration", func(context.Context, string, interface{}) (json.RawMessage, error) {
			return json.RawMessage(`"suddenly a string"`), nil
		}},
		{"an answer about a different field", func(context.Context, string, interface{}) (json.RawMessage, error) {
			return json.RawMessage(`{"field":"mail_accounts","values":[{"value":"Bob"}]}`), nil
		}},
		{"an offered entry with nothing to store", func(context.Context, string, interface{}) (json.RawMessage, error) {
			return json.RawMessage(`{"field":"mail_mailboxes","values":[{"value":"INBOX"},{"label":"orphan"}]}`), nil
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, _ := mgrWithConn(t, c.send)
			res := m.EnumerateContextField(context.Background(), "macmcp", "mail_mailboxes", nil)
			if res.Status != EnumStatusUnavailable {
				t.Fatalf("status = %q, want %q (%s)", res.Status, EnumStatusUnavailable, res.Error)
			}
			if res.Values != nil {
				t.Fatalf("a failed call came back with a value list: %v", res.Values)
			}
			if res.Error == "" {
				t.Error("a failure with no message leaves the operator nothing to act on")
			}
			m.mu.RLock()
			latched := m.enumUnsupported["macmcp"]
			m.mu.RUnlock()
			if latched {
				t.Error("a transient failure permanently disabled the picker for this MCP")
			}
		})
	}
}

// An MCP that is registered but not connected cannot answer, and saying so is
// not the same as saying there is nothing to list.
func TestEnumerate_DisconnectedMcpIsUnavailable(t *testing.T) {
	m := NewExternalMcpManager(nil)
	t.Cleanup(m.StopAll)
	res := m.EnumerateContextField(context.Background(), "macmcp", "mail_accounts", nil)
	if res.Status != EnumStatusUnavailable || res.Values != nil {
		t.Fatalf("got %q values=%v", res.Status, res.Values)
	}
}

// An empty list IS an answer, and it is the one case where rendering "there
// are none" is correct — so it must be representable, and distinguishable in
// the JSON from every failure above.
func TestEnumerate_EmptyListIsAnAnswerAndSerializesApartFromAFailure(t *testing.T) {
	m, _ := mgrWithConn(t, func(context.Context, string, interface{}) (json.RawMessage, error) {
		return json.RawMessage(`{"field":"mail_mailboxes","values":[]}`), nil
	})
	res := m.EnumerateContextField(context.Background(), "macmcp", "mail_mailboxes", nil)
	if !res.OK() {
		t.Fatalf("status = %q, want ok", res.Status)
	}
	if res.Values == nil || len(res.Values) != 0 {
		t.Fatalf("an empty answer did not survive as an empty list: %v", res.Values)
	}
	body, _ := json.Marshal(res)
	if !strings.Contains(string(body), `"values":[]`) {
		t.Errorf(`"there are none" must serialize as [] : %s`, body)
	}

	failed := ContextEnumResult{McpID: "macmcp", Field: "mail_mailboxes", Status: EnumStatusUnavailable}
	body, _ = json.Marshal(failed)
	if !strings.Contains(string(body), `"values":null`) {
		t.Errorf(`"nobody could look" must serialize as null, not []: %s`, body)
	}
}

// ---------------------------------------------------------------------------
// Against the real stdio peer
// ---------------------------------------------------------------------------

// The whole path — spawn, handshake, a declared v2 schema read off serverInfo,
// a context/enumerate over the wire, the answer parsed back — with nothing
// mocked. The mocks above pin the classification; this pins that relay and an
// MCP agree on the request and the result shape at all.
func startEnumPeer(t *testing.T, mode string) *ExternalMcpManager {
	t.Helper()
	bin := buildTestMcpBinary(t)
	m := NewExternalMcpManager(nil)
	t.Cleanup(m.StopAll)
	cfg := stdioMcp("macmcp", bin)
	cfg.Env = map[string]string{"RELAY_TESTMCP_CONTEXT": mode}
	if err := m.startOne(context.Background(), &cfg); err != nil {
		t.Fatalf("start testmcp: %v", err)
	}
	return m
}

func enumValueStrings(t *testing.T, res ContextEnumResult) []string {
	t.Helper()
	out := make([]string, 0, len(res.Values))
	for _, v := range res.Values {
		var s string
		if err := json.Unmarshal(v.Value, &s); err != nil {
			t.Fatalf("value %s is not a string: %v", v.Value, err)
		}
		out = append(out, s)
	}
	return out
}

func TestEnumerate_LiveStdioPeer(t *testing.T) {
	m := startEnumPeer(t, "v2")
	surfaces := m.AllMcpSurfaces()

	// The schema arrived off the wire, and relay reads the two enumerable
	// fields out of it without knowing what either one means.
	views := surfaces.Schema("macmcp").ScopeFieldViews()
	if len(views) != 3 {
		t.Fatalf("want 3 restrict fields from the peer's serverInfo, got %d", len(views))
	}

	accounts := enumerateScopeField(context.Background(), surfaces, m, "macmcp", "mail_accounts", nil)
	if !accounts.OK() {
		t.Fatalf("mail_accounts: %q %s", accounts.Status, accounts.Error)
	}
	if got := fmt.Sprint(enumValueStrings(t, accounts)); got != "[Alice Bob]" {
		t.Errorf("mail_accounts = %s, want [Alice Bob]", got)
	}
	if accounts.Values[0].Label == "" {
		t.Error("the label the MCP sent for display did not survive")
	}

	// Dependency order: the mailbox list is read WITHIN the chosen accounts.
	within := enumerateScopeField(context.Background(), surfaces, m, "macmcp", "mail_mailboxes",
		map[string]json.RawMessage{"mail_accounts": json.RawMessage(`["Bob"]`)})
	if !within.OK() {
		t.Fatalf("mail_mailboxes within Bob: %q %s", within.Status, within.Error)
	}
	if got := fmt.Sprint(enumValueStrings(t, within)); got != "[Bob/INBOX]" {
		t.Errorf("mail_mailboxes within Bob = %s, want [Bob/INBOX]", got)
	}

	// And with the dependency UNSET it lists across every account rather than
	// coming back empty — the picker's opening state must not look like a host
	// with no mailboxes.
	for _, unset := range []map[string]json.RawMessage{
		nil,
		{"mail_accounts": json.RawMessage(`[]`)},
	} {
		across := enumerateScopeField(context.Background(), surfaces, m, "macmcp", "mail_mailboxes", unset)
		if !across.OK() {
			t.Fatalf("mail_mailboxes across all: %q %s", across.Status, across.Error)
		}
		if got := fmt.Sprint(enumValueStrings(t, across)); got != "[Alice/INBOX Bob/INBOX]" {
			t.Errorf("with the dependency unset (%v) the list was %s, want every account's", unset, got)
		}
	}
}

// The same peer built without the method: relay asks once, is told -32601, and
// the operator surface degrades to the text box it always had.
func TestEnumerate_LiveStdioPeerWithoutTheMethod(t *testing.T) {
	m := startEnumPeer(t, "unsupported")
	res := enumerateScopeField(context.Background(), m.AllMcpSurfaces(), m, "macmcp", "mail_accounts", nil)
	if res.Status != EnumStatusUnsupported {
		t.Fatalf("status = %q, want %q (%s)", res.Status, EnumStatusUnsupported, res.Error)
	}
	if res.Values != nil {
		t.Error("an MCP without the method was reported as having no accounts")
	}
}

// A live peer asked about a field it does not enumerate answers -32602, and
// that reaches the surface as a relay bug rather than as an empty picker.
// Called on the manager directly because enumerateScopeField would (correctly)
// refuse first — this is the belt behind that brace.
func TestEnumerate_LiveStdioPeerRefusesAnUndeclaredField(t *testing.T) {
	m := startEnumPeer(t, "v2")
	res := m.EnumerateContextField(context.Background(), "macmcp", "invented_field", nil)
	if res.Status != EnumStatusInvalidField {
		t.Fatalf("status = %q, want %q (%s)", res.Status, EnumStatusInvalidField, res.Error)
	}
	if res.Values != nil {
		t.Error("a refused request came back with a value list")
	}
}

// A peer that dies mid-request is a transport failure, not an empty mailbox
// list. This is the one path where nothing answers at all.
func TestEnumerate_LiveStdioPeerThatDies(t *testing.T) {
	m := startEnumPeer(t, "v2")
	m.mu.RLock()
	conn := m.conns["macmcp"]
	m.mu.RUnlock()
	conn.Close()

	res := m.EnumerateContextField(context.Background(), "macmcp", "mail_accounts", nil)
	if res.Status != EnumStatusUnavailable {
		t.Fatalf("status = %q, want %q (%s)", res.Status, EnumStatusUnavailable, res.Error)
	}
	if res.Values != nil {
		t.Error("a dead connection was reported as an empty account list")
	}
}

// ---------------------------------------------------------------------------
// The wire: HTTP and IPC, which ADR-004 keeps co-equal
// ---------------------------------------------------------------------------

func TestEnumerateRoute_HTTP(t *testing.T) {
	cases := []struct {
		name       string
		mcpID      string
		body       string
		enum       ContextEnumerator
		wantCode   int
		wantStatus string
		wantValues string // the raw JSON of the values field
	}{
		{
			name: "a real answer", mcpID: "macmcp",
			body: `{"field":"mail_accounts"}`, enum: okEnum("Alice", "Bob"),
			wantCode: http.StatusOK, wantStatus: EnumStatusOK,
			wantValues: `"values":[{"value":"Alice","label":"Alice"},{"value":"Bob","label":"Bob"}]`,
		},
		{
			name: "an answer with nothing in it", mcpID: "macmcp",
			body: `{"field":"mail_accounts"}`, enum: okEnum(),
			wantCode: http.StatusOK, wantStatus: EnumStatusOK, wantValues: `"values":[]`,
		},
		{
			name: "an MCP that does not implement enumeration", mcpID: "macmcp",
			body: `{"field":"mail_accounts"}`,
			enum: &fakeEnumerator{result: ContextEnumResult{Status: EnumStatusUnsupported, Error: "no such method"}},
			// 200: a true, final answer ABOUT the MCP. The caller renders a
			// text box, permanently.
			wantCode: http.StatusOK, wantStatus: EnumStatusUnsupported, wantValues: `"values":null`,
		},
		{
			name: "the MCP refusing the request relay built", mcpID: "macmcp",
			body: `{"field":"mail_accounts"}`,
			enum: &fakeEnumerator{result: ContextEnumResult{Status: EnumStatusInvalidField, Error: "not enumerable"}},
			// 502: the failure is on relay's side of the operator.
			wantCode: http.StatusBadGateway, wantStatus: EnumStatusInvalidField, wantValues: `"values":null`,
		},
		{
			name: "the MCP not answering right now", mcpID: "macmcp",
			body:     `{"field":"mail_accounts"}`,
			enum:     &fakeEnumerator{result: ContextEnumResult{Status: EnumStatusUnavailable, Error: "Mail timed out"}},
			wantCode: http.StatusServiceUnavailable, wantStatus: EnumStatusUnavailable, wantValues: `"values":null`,
		},
		{
			name: "an MCP relay has never connected to", mcpID: "ghostmcp",
			body: `{"field":"mail_accounts"}`, enum: okEnum("Alice"),
			wantCode: http.StatusNotFound, wantStatus: EnumStatusUnknownMcp, wantValues: `"values":null`,
		},
		{
			name: "a field the MCP never said it could enumerate", mcpID: "macmcp",
			body: `{"field":"file_dirs"}`, enum: okEnum("Alice"),
			wantCode: http.StatusBadRequest, wantStatus: EnumStatusNotEnumerable, wantValues: `"values":null`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := NewSettingsStoreAt(mkEmptySandboxRelayHome(t))
			if err := store.EnsureInitialized(); err != nil {
				t.Fatalf("EnsureInitialized: %v", err)
			}
			mux := http.NewServeMux()
			RegisterProjectRoutes(mux, store, schemaProviderFunc(enumSurfaces), nil, c.enum, nil, nil)
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

// The dependency values an operator has already chosen reach the MCP through
// the HTTP surface too — a picker in eve fills in dependency order or it fills
// in the wrong order.
func TestEnumerateRoute_HTTPCarriesTheChosenDependencies(t *testing.T) {
	store := NewSettingsStoreAt(mkEmptySandboxRelayHome(t))
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	enum := okEnum("INBOX")
	mux := http.NewServeMux()
	RegisterProjectRoutes(mux, store, schemaProviderFunc(enumSurfaces), nil, enum, nil, nil)
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
// behind the same bearer token as every other project route, on the same 0600
// socket. Unauthenticated is 401 before the handler runs.
func TestEnumerateRoute_RequiresTheFrontendToken(t *testing.T) {
	_, sock := newTestFrontendServer(t, "the-token")
	client := dialFrontendHTTP(sock)

	req, _ := http.NewRequest("POST", "http://unix/api/mcps/macmcp/enumerate",
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

// And it is not reachable from the remote listener at all. That dispatch table
// is ListTools and CallTool; this adds a method to the FRONTEND surface and to
// the tray's IPC channel, neither of which a remote client can address.
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

// The IPC half. Same function underneath, same result shape, emitted verbatim
// so the tray picker can tell an empty answer from a failed one exactly as eve
// can (ADR-004: the two editors are co-equal).
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
	var res ContextEnumResult
	if err := json.Unmarshal(args[0].(json.RawMessage), &res); err != nil {
		t.Fatalf("event payload: %v", err)
	}
	if !res.OK() || res.Field != "mail_mailboxes" || len(res.Values) != 2 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(enum.calls) != 1 || string(enum.calls[0].values["mail_accounts"]) != `["Bob"]` {
		t.Fatalf("the chosen dependency did not reach the MCP: %+v", enum.calls)
	}

	// A failure reaches the UI as a failure, with no value list to mistake for
	// an empty one.
	ipc.Enumerate = &fakeEnumerator{result: ContextEnumResult{Status: EnumStatusUnavailable, Error: "Mail timed out"}}
	ipcEnumerateScopeField(ipc, json.RawMessage(`{"type":"enumerate_scope_field","mcp_id":"macmcp","field":"mail_accounts"}`))
	args, _ = findEvent(ui, "onScopeFieldEnumerated")
	res = ContextEnumResult{}
	if err := json.Unmarshal(args[0].(json.RawMessage), &res); err != nil {
		t.Fatalf("event payload: %v", err)
	}
	if res.Status != EnumStatusUnavailable || res.Values != nil {
		t.Fatalf("a failure reached the UI as %+v", res)
	}
}

// No enumerator wired at all (the IPC context a narrow test builds, or a mode
// where the manager is absent) must not panic and must not claim emptiness.
func TestEnumerate_IPCWithNoProvider(t *testing.T) {
	ipc, _, ui, _ := newProjectsIPC(t)
	ipc.Tools = &fakeTools{surfaces: enumSurfaces()}
	ipc.Enumerate = nil

	ipcEnumerateScopeField(ipc, json.RawMessage(`{"type":"enumerate_scope_field","mcp_id":"macmcp","field":"mail_accounts"}`))
	args, ok := findEvent(ui, "onScopeFieldEnumerated")
	if !ok {
		t.Fatal("no event emitted")
	}
	var res ContextEnumResult
	_ = json.Unmarshal(args[0].(json.RawMessage), &res)
	if res.Status != EnumStatusUnavailable || res.Values != nil {
		t.Fatalf("got %+v", res)
	}
}
