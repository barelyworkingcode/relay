package main

// Deliberate: setupRouter inserts connections from a map, so enumeration
// order is Go's and fresh on every run — the property below must hold
// whatever order the runtime picks. The call is repeated rather than run
// once because a single call against a map that favors the first-inserted
// key ~7 times in 8 proves very little. Order-controlled cases live in
// router_tool_collision_test.go, whose provider keeps an ordered slice.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/mcp"
)

func collidingRouter(t *testing.T, perms map[string]config.Permission, order []string, served *string) *appRouter {
	t.Helper()
	mocks := map[string]*mockMcpConn{}
	for _, id := range order {
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
		r := collidingRouter(t, map[string]config.Permission{"mcp-a": config.PermOn, "mcp-b": config.PermOn},
			[]string{"mcp-a", "mcp-b"}, &served)
		const svcToken = "svc-token-for-ambiguity-test-0011223344556677"
		r.serviceTokens.Register(config.HashToken(svcToken))

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
