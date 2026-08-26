package main

// Issue #35: a tool name is not unique across MCPs, and the id CallTool
// resolves it to selects far more than a dispatch target — the `_meta`
// resource scope, the disabled-tools list, the live schema the scope is
// checked against, and the audit's mcp_id. Resolving the name against a Go map
// therefore let the runtime's map seed choose which confinement a call ran
// under.
//
// This fixture reaches the REAL ExternalMcpManager through setupRouter, which
// inserts the connections from a map — so the enumeration order is Go's, fresh
// on every run, and cannot be dictated from here. That is the right shape for
// the property below (it must hold whatever order the runtime picks) and it is
// why the call is repeated rather than run once: a single call against a map
// that favours the first-inserted key ~7 times in 8 proves very little. The
// order-controlled cases live in router_tool_collision_test.go, whose provider
// keeps an ordered slice.

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

// A service token admits every MCP, which makes it the grant most likely to be
// ambiguous — so it is held to the same rule rather than allowed to pick.
func TestCallTool_ServiceTokenIsRefusedOnAmbiguityToo(t *testing.T) {
	const runs = 20
	for i := 0; i < runs; i++ {
		var served string
		r := collidingRouter(t, map[string]Permission{"mcp-a": PermOn, "mcp-b": PermOn},
			[]string{"mcp-a", "mcp-b"}, &served)
		const svcToken = "svc-token-for-ambiguity-test-0011223344556677"
		r.serviceTokens.Register(hashToken(svcToken))

		_, err := r.CallTool(context.Background(), "fs_read", nil, svcToken)
		if err == nil {
			t.Fatalf("run %d: expected a service token to be refused on an ambiguous name", i)
		}
		for _, want := range []string{"fs_read", "mcp-a", "mcp-b"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("run %d: refusal must name %q, got %q", i, want, err.Error())
			}
		}
		if served != "" {
			t.Errorf("run %d: an ambiguous service-token call reached %q", i, served)
		}
		if _, err := r.CallTool(context.Background(), "only_mcp-b", nil, svcToken); err != nil {
			t.Fatalf("run %d: expected the unshared tool to still work, got %v", i, err)
		}
	}
}
