package main

import (
	"context"
	"encoding/json"
	"fmt"

	"relaygo/mcp"
)

// mockMcpConn implements McpConnection for testing.
type mockMcpConn struct {
	sendRequestFunc    func(ctx context.Context, method string, params interface{}) (json.RawMessage, error)
	sendNotificationFn func(method string)
	closeFn            func()
	tools              []mcp.Tool
	config             ExternalMcp
	notifications      []string // track received notifications
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

func (m *mockMcpConn) GetTools() []mcp.Tool      { return m.tools }
func (m *mockMcpConn) SetTools(tools []mcp.Tool) { m.tools = tools }
func (m *mockMcpConn) GetConfig() ExternalMcp    { return m.config }

// decodedToolParams renders the tools/call params a mock connection was handed
// as decoded Go values, for tests that want to assert on `_meta`.
//
// Production hands SendRequest a map[string]json.RawMessage and every member of
// it is bytes, because relay forwards a caller's arguments rather than
// re-encoding them (ADR-012). A test that wants a Go map has to do the decode
// itself, and that is the point: nothing on the call path does it any more.
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

// ---------------------------------------------------------------------------
// Test helpers — reduce lock/unlock boilerplate in router and manager tests
// ---------------------------------------------------------------------------

// addMockConn registers a mock connection in the manager under lock.
// Eliminates the repeated mgr.mu.Lock() / mgr.conns[id] = mock / mgr.mu.Unlock() pattern.
func addMockConn(mgr *ExternalMcpManager, id string, mock *mockMcpConn) {
	mgr.mu.Lock()
	mgr.conns[id] = mock
	mgr.mu.Unlock()
}

// addMockSchema registers a context schema and its declared version for an MCP
// under lock, which is what a real handshake does in finalizeConnection.
func addMockSchema(mgr *ExternalMcpManager, id, schema string, version int) {
	mgr.mu.Lock()
	mgr.schemas[id] = json.RawMessage(schema)
	mgr.schemaVersions[id] = version
	mgr.mu.Unlock()
}

// newMockConn creates a mockMcpConn with the given ID and tools.
// Optionally accepts a sendRequestFunc for tool call interception.
func newMockConn(id string, tools []mcp.Tool, sendFn func(context.Context, string, interface{}) (json.RawMessage, error)) *mockMcpConn {
	return &mockMcpConn{
		tools:           tools,
		config:          ExternalMcp{ID: id},
		sendRequestFunc: sendFn,
	}
}

// simpleTools creates a []mcp.Tool from a list of tool names, with NO
// annotations — which under ADR-011 decision 2 means "mutating", because an
// absent readOnlyHint is not a claim that a tool is safe, and under decision 2c
// means "open-world", because MCP's default for openWorldHint is true and
// silence there is not a claim of containment either.
//
// Both silences are usable in a LOCAL project's fixture, and that is the point
// rather than an accident: a local project defaults to write and to allowed,
// so an unannotated tool is exactly the ordinary tool such a project calls all
// day. A remote-profile fixture has to say what it means on both axes — see
// macmcpToolSurface, which does.
func simpleTools(names ...string) []mcp.Tool {
	tools := make([]mcp.Tool, len(names))
	for i, name := range names {
		tools[i] = mcp.Tool{Name: name}
	}
	return tools
}

// localTools is simpleTools plus the claim that these tools stay on this host:
// openWorldHint: false, still silent on readOnlyHint. For a fixture that has to
// be callable from a PROFILE without an outbound grant.
func localTools(names ...string) []mcp.Tool {
	tools := simpleTools(names...)
	for i := range tools {
		tools[i].Annotations = json.RawMessage(`{"openWorldHint":false}`)
	}
	return tools
}

// openWorldTools declares the opposite: tools that reach outside the host,
// which is web_fetch's shape and mail_send's.
func openWorldTools(names ...string) []mcp.Tool {
	tools := simpleTools(names...)
	for i := range tools {
		tools[i].Annotations = json.RawMessage(`{"openWorldHint":true}`)
	}
	return tools
}

// readOnlyTools is simpleTools with both hints declared honestly: read-only
// and staying on this host. Deliberately a SEPARATE helper rather than a
// default on simpleTools: the whole of decision 2 is that an unannotated tool
// is refused to a read-only grant, so a helper that quietly annotated
// everything would make that untestable by making it unreachable.
//
// It carries openWorldHint too because its only consumer is a remote-profile
// fixture, and a profile defaults to refusing an open-world tool (decision 2c)
// — so a tool annotated on one axis only would be unreachable there for a
// reason that has nothing to do with what the test is measuring.
func readOnlyTools(names ...string) []mcp.Tool {
	tools := simpleTools(names...)
	for i := range tools {
		tools[i].Annotations = json.RawMessage(`{"readOnlyHint":true,"openWorldHint":false}`)
	}
	return tools
}
