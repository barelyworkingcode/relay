//go:build !windows

package mcpbroker

// The manager half of context/enumerate: how *Manager classifies an MCP's
// answer, what it latches, and what a real stdio peer produces. The policy
// half (project.EnumerateScopeField's own refusals) and the HTTP/IPC doors
// stay beside their code in cmd/relay's context_enumerate_test.go.
//
// Subtle: every non-ok enumeration result must carry a nil Values — nil vs []
// is what separates "could not answer" from "there are none."

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/jsonrpc"
	"github.com/barelyworkingcode/relay/internal/project"
)

func mgrWithConn(t *testing.T, sendFn func(context.Context, string, interface{}) (json.RawMessage, error)) (*Manager, *int) {
	t.Helper()
	calls := 0
	m := NewManager(nil)
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

func TestEnumerate_MethodNotFoundDegradesAndLatches(t *testing.T) {
	m, calls := mgrWithConn(t, rpcErrConn(jsonrpc.CodeMethodNotFound, "method not found"))

	res := m.EnumerateContextField(context.Background(), "macmcp", "mail_accounts", nil)
	if res.Status != project.EnumStatusUnsupported {
		t.Fatalf("status = %q, want %q", res.Status, project.EnumStatusUnsupported)
	}
	if res.Values != nil {
		t.Error("an MCP that does not enumerate was reported as having no values")
	}
	if *calls != 1 {
		t.Fatalf("want one request, got %d", *calls)
	}

	// A different field, same connection: still answered from the latch, no
	// second round trip.
	res = m.EnumerateContextField(context.Background(), "macmcp", "mail_mailboxes", nil)
	if res.Status != project.EnumStatusUnsupported {
		t.Fatalf("second call status = %q", res.Status)
	}
	if *calls != 1 {
		t.Fatalf("relay re-asked an MCP that already said it does not implement the method (%d requests)", *calls)
	}

	// Latch is scoped to the connection, not the MCP id — a reconnect may be a
	// different build that does implement the method.
	m.Stop("macmcp")
	m.mu.RLock()
	latched := m.enumUnsupported["macmcp"]
	m.mu.RUnlock()
	if latched {
		t.Error("the -32601 latch survived the connection that asserted it")
	}
}

// Deliberate: surfaced, not degraded — degrading would hide a relay bug
// behind a text box that looks intentional.
func TestEnumerate_InvalidParamsIsSurfacedNotDegraded(t *testing.T) {
	m, _ := mgrWithConn(t, rpcErrConn(jsonrpc.CodeInvalidParams, "no enumerable field named mail_mailboxes"))
	res := m.EnumerateContextField(context.Background(), "macmcp", "mail_mailboxes", nil)

	if res.Status != project.EnumStatusInvalidField {
		t.Fatalf("status = %q, want %q", res.Status, project.EnumStatusInvalidField)
	}
	if !strings.Contains(res.Error, "no enumerable field named mail_mailboxes") {
		t.Errorf("the MCP's own reason was thrown away: %q", res.Error)
	}
	// Must not latch — this is relay asking the wrong question, not the MCP
	// lacking the method.
	m.mu.RLock()
	latched := m.enumUnsupported["macmcp"]
	m.mu.RUnlock()
	if latched {
		t.Error("a bad request from relay was recorded as the MCP not implementing enumeration")
	}
}

// Deliberate: relay special-cases exactly two codes (-32601, -32602) and
// treats every other error — including macMCP's real -32000 server-error
// range — as retryable, rather than enumerating every code it has seen.
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
			if res.Status != project.EnumStatusUnavailable {
				t.Fatalf("status = %q, want %q (%s)", res.Status, project.EnumStatusUnavailable, res.Error)
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

func TestEnumerate_DisconnectedMcpIsUnavailable(t *testing.T) {
	m := NewManager(nil)
	t.Cleanup(m.StopAll)
	res := m.EnumerateContextField(context.Background(), "macmcp", "mail_accounts", nil)
	if res.Status != project.EnumStatusUnavailable || res.Values != nil {
		t.Fatalf("got %q values=%v", res.Status, res.Values)
	}
}

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

	failed := project.ContextEnumResult{McpID: "macmcp", Field: "mail_mailboxes", Status: project.EnumStatusUnavailable}
	body, _ = json.Marshal(failed)
	if !strings.Contains(string(body), `"values":null`) {
		t.Errorf(`"nobody could look" must serialize as null, not []: %s`, body)
	}
}

func startEnumPeer(t *testing.T, mode string) *Manager {
	t.Helper()
	bin := buildTestMcpBinary(t)
	m := NewManager(nil)
	t.Cleanup(m.StopAll)
	cfg := stdioMcp("macmcp", bin)
	cfg.Env = config.SecretMapFromPlain(map[string]string{"RELAY_TESTMCP_CONTEXT": mode})
	if err := m.startOne(context.Background(), &cfg); err != nil {
		t.Fatalf("start testmcp: %v", err)
	}
	return m
}

func enumValueStrings(t *testing.T, res project.ContextEnumResult) []string {
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

	views := surfaces.Schema("macmcp").ScopeFieldViews()
	if len(views) != 3 {
		t.Fatalf("want 3 restrict fields from the peer's serverInfo, got %d", len(views))
	}

	accounts := project.EnumerateScopeField(context.Background(), surfaces, m, "macmcp", "mail_accounts", nil)
	if !accounts.OK() {
		t.Fatalf("mail_accounts: %q %s", accounts.Status, accounts.Error)
	}
	if got := fmt.Sprint(enumValueStrings(t, accounts)); got != "[Alice Bob]" {
		t.Errorf("mail_accounts = %s, want [Alice Bob]", got)
	}
	if accounts.Values[0].Label == "" {
		t.Error("the label the MCP sent for display did not survive")
	}

	within := project.EnumerateScopeField(context.Background(), surfaces, m, "macmcp", "mail_mailboxes",
		map[string]json.RawMessage{"mail_accounts": json.RawMessage(`["Bob"]`)})
	if !within.OK() {
		t.Fatalf("mail_mailboxes within Bob: %q %s", within.Status, within.Error)
	}
	if got := fmt.Sprint(enumValueStrings(t, within)); got != "[Bob/INBOX]" {
		t.Errorf("mail_mailboxes within Bob = %s, want [Bob/INBOX]", got)
	}

	// Unset dependency lists across every account rather than empty — the
	// picker's opening state must not look mailbox-less.
	for _, unset := range []map[string]json.RawMessage{
		nil,
		{"mail_accounts": json.RawMessage(`[]`)},
	} {
		across := project.EnumerateScopeField(context.Background(), surfaces, m, "macmcp", "mail_mailboxes", unset)
		if !across.OK() {
			t.Fatalf("mail_mailboxes across all: %q %s", across.Status, across.Error)
		}
		if got := fmt.Sprint(enumValueStrings(t, across)); got != "[Alice/INBOX Bob/INBOX]" {
			t.Errorf("with the dependency unset (%v) the list was %s, want every account's", unset, got)
		}
	}
}

func TestEnumerate_LiveStdioPeerWithoutTheMethod(t *testing.T) {
	m := startEnumPeer(t, "unsupported")
	res := project.EnumerateScopeField(context.Background(), m.AllMcpSurfaces(), m, "macmcp", "mail_accounts", nil)
	if res.Status != project.EnumStatusUnsupported {
		t.Fatalf("status = %q, want %q (%s)", res.Status, project.EnumStatusUnsupported, res.Error)
	}
	if res.Values != nil {
		t.Error("an MCP without the method was reported as having no accounts")
	}
}

// Calls the manager directly — project.EnumerateScopeField would (correctly) refuse
// first; this is belt behind that brace.
func TestEnumerate_LiveStdioPeerRefusesAnUndeclaredField(t *testing.T) {
	m := startEnumPeer(t, "v2")
	res := m.EnumerateContextField(context.Background(), "macmcp", "invented_field", nil)
	if res.Status != project.EnumStatusInvalidField {
		t.Fatalf("status = %q, want %q (%s)", res.Status, project.EnumStatusInvalidField, res.Error)
	}
	if res.Values != nil {
		t.Error("a refused request came back with a value list")
	}
}

func TestEnumerate_LiveStdioPeerThatDies(t *testing.T) {
	m := startEnumPeer(t, "v2")
	m.mu.RLock()
	conn := m.conns["macmcp"]
	m.mu.RUnlock()
	conn.Close()

	res := m.EnumerateContextField(context.Background(), "macmcp", "mail_accounts", nil)
	if res.Status != project.EnumStatusUnavailable {
		t.Fatalf("status = %q, want %q (%s)", res.Status, project.EnumStatusUnavailable, res.Error)
	}
	if res.Values != nil {
		t.Error("a dead connection was reported as an empty account list")
	}
}
