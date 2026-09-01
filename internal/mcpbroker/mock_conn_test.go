package mcpbroker

// The stand-in Connection the manager tests drive. It bypasses the JSON-RPC
// framing, the pending-request ID map and the stdin write path entirely —
// external_mcp_stdio_test.go covers those against the real cmd/testmcp peer.
//
// cmd/relay carries its own copy (mock_mcp_test.go): a _test.go file's
// symbols do not cross a package boundary, and the router, audit and
// project-scoping tests over there need the same fake behind a *Manager.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/mcp"
)

type mockMcpConn struct {
	sendRequestFunc    func(ctx context.Context, method string, params interface{}) (json.RawMessage, error)
	sendNotificationFn func(method string)
	closeFn            func()
	tools              []mcp.Tool
	config             config.ExternalMcp
	notifications      []string
	closed             bool
}

func (m *mockMcpConn) SendRequest(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	if m.sendRequestFunc != nil {
		return m.sendRequestFunc(ctx, method, params)
	}
	return nil, fmt.Errorf("unexpected SendRequest call: %s", method)
}

func (m *mockMcpConn) SendNotification(method string) {
	m.notifications = append(m.notifications, method)
	if m.sendNotificationFn != nil {
		m.sendNotificationFn(method)
	}
}

func (m *mockMcpConn) Close() {
	m.closed = true
	if m.closeFn != nil {
		m.closeFn()
	}
}

func (m *mockMcpConn) GetTools() []mcp.Tool          { return m.tools }
func (m *mockMcpConn) SetTools(tools []mcp.Tool)     { m.tools = tools }
func (m *mockMcpConn) GetConfig() config.ExternalMcp { return m.config }

// decodedToolParams renders the tools/call params a mock connection was handed
// as decoded Go values, for tests that want to assert on `_meta`.
//
// Production never decodes call params — relay forwards a caller's arguments
// as bytes rather than re-encoding them — so there is no shared decode path
// to call into; a test that wants a Go map has to do this itself.
func decodedToolParams(params interface{}) map[string]interface{} {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// Through the exported seam rather than the fields beside it, so both
// suites install a fake connection exactly one way.
func addMockConn(mgr *Manager, id string, mock *mockMcpConn) {
	mgr.SetConnectionForTest(id, mock)
}

func addMockSchema(mgr *Manager, id, schema string, version int) {
	mgr.SetContextSchemaForTest(id, json.RawMessage(schema), version)
}

func newMockConn(id string, tools []mcp.Tool, sendFn func(context.Context, string, interface{}) (json.RawMessage, error)) *mockMcpConn {
	return &mockMcpConn{
		tools:           tools,
		config:          config.ExternalMcp{ID: id},
		sendRequestFunc: sendFn,
	}
}

// simpleTools creates a []mcp.Tool from a list of tool names, with no
// annotations at all: an absent readOnlyHint is not a claim of safety, and an
// absent openWorldHint is not a claim of containment, so this reads as
// "mutating" and "open-world" rather than as an unset default.
func simpleTools(names ...string) []mcp.Tool {
	tools := make([]mcp.Tool, len(names))
	for i, name := range names {
		tools[i] = mcp.Tool{Name: name}
	}
	return tools
}
