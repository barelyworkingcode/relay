package main

// Issue #35: a tool name is not unique across MCPs, and the id CallTool
// resolves it to selects far more than a dispatch target — the `_meta`
// resource scope, the disabled-tools list, the live schema the scope is
// checked against, and the audit's mcp_id. Resolving the name against a Go map
// therefore let the runtime's map seed choose which confinement a call ran
// under. Every test here uses BOTH insertion orders for the colliding MCPs,
// because one ordering can pass on a lucky seed and prove nothing.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"relaygo/mcp"
)

// collidingRouter builds a router where mcp-a and mcp-b both expose fs_read
// and each also has a tool only it owns. served receives the id of whichever
// MCP was actually invoked.
func collidingRouter(t *testing.T, perms map[string]Permission, order []string, served *string) *appRouter {
	t.Helper()
	mocks := map[string]*mockMcpConn{}
	for _, id := range order {
		id := id
		mocks[id] = newMockConn(id, simpleTools("fs_read", "only_"+id),
			func(_ context.Context, method string, _ interface{}) (json.RawMessage, error) {
				if method != mcp.MethodToolsCall {
					return nil, fmt.Errorf("unexpected method: %s", method)
				}
				*served = id
				return json.RawMessage(`{"content":[{"type":"text","text":"` + id + `"}]}`), nil
			})
	}
	return setupRouter(t, perms, nil, nil, mocks)
}

// bothConnOrders runs fn with each insertion order of the two colliding MCPs.
func bothConnOrders(t *testing.T, fn func(t *testing.T, order []string)) {
	t.Helper()
	for _, order := range [][]string{{"mcp-a", "mcp-b"}, {"mcp-b", "mcp-a"}} {
		t.Run(strings.Join(order, ","), func(t *testing.T) { fn(t, order) })
	}
}

// The reported symptom: a grant naming only B, calling a tool A and B both
// expose, was refused "MCP 'A' is disabled for this token". B owns the name, B
// is granted, B must serve it.
// A service token admits every MCP, which makes it the grant most likely to be
// ambiguous — so it is held to the same rule rather than allowed to pick.
func TestCallTool_ServiceTokenIsRefusedOnAmbiguityToo(t *testing.T) {
	bothConnOrders(t, func(t *testing.T, order []string) {
		var served string
		r := collidingRouter(t, map[string]Permission{"mcp-a": PermOn, "mcp-b": PermOn}, order, &served)
		const svcToken = "svc-token-for-ambiguity-test-0011223344556677"
		r.serviceTokens.Register(hashToken(svcToken))

		_, err := r.CallTool(context.Background(), "fs_read", nil, svcToken)
		if err == nil {
			t.Fatal("expected a service token to be refused on an ambiguous name")
		}
		for _, want := range []string{"fs_read", "mcp-a", "mcp-b"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("refusal must name %q, got %q", want, err.Error())
			}
		}
		if served != "" {
			t.Errorf("an ambiguous service-token call reached %q", served)
		}
		if _, err := r.CallTool(context.Background(), "only_mcp-b", nil, svcToken); err != nil {
			t.Fatalf("expected the unshared tool to still work, got %v", err)
		}
	})
}
