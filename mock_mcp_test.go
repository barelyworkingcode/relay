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
// absent readOnlyHint is not a claim that a tool is safe.
func simpleTools(names ...string) []mcp.Tool {
	tools := make([]mcp.Tool, len(names))
	for i, name := range names {
		tools[i] = mcp.Tool{Name: name}
	}
	return tools
}

// readOnlyTools is simpleTools with an explicit annotations.readOnlyHint: true
// on every tool. Deliberately a SEPARATE helper rather than a default on
// simpleTools: the whole of decision 2 is that an unannotated tool is refused
// to a read-only grant, so a helper that quietly annotated everything would
// make that untestable by making it unreachable.
func readOnlyTools(names ...string) []mcp.Tool {
	tools := simpleTools(names...)
	for i := range tools {
		tools[i].Annotations = json.RawMessage(`{"readOnlyHint":true}`)
	}
	return tools
}
