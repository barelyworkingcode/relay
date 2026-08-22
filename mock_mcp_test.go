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

// simpleTools creates a []mcp.Tool from a list of tool names carrying NO
// readOnlyHint — which under ADR-011 decision 2 means "mutating", because an
// absent hint is not a claim that a tool is safe — and an explicit
// openWorldHint: false, which under decision 2c is the claim that it stays on
// this host.
//
// The two are spelled differently on purpose, and the asymmetry is the
// design's rather than this helper's. Silence on readOnlyHint is a usable
// fixture: it is the ordinary tool a local project (default write) calls all
// day, and it is what the mode tests need to be silent about. Silence on
// openWorldHint is NOT usable, because that hint defaults to true in the MCP
// specification, so an unannotated fixture is refused to every grant that has
// not been given allow_external — which would turn every test of auditing,
// budgets, scope injection and skill buckets into a test of decision 2c.
//
// The absent-hint case therefore has its own fixtures rather than being the
// ambient default here: see openWorldTools below and
// TestOpenWorldHint_AbsentMeansOpenWorld.
func simpleTools(names ...string) []mcp.Tool {
	tools := make([]mcp.Tool, len(names))
	for i, name := range names {
		tools[i] = mcp.Tool{Name: name, Annotations: json.RawMessage(`{"openWorldHint":false}`)}
	}
	return tools
}

// openWorldTools is simpleTools with the outbound claim reversed: tools that
// declare openWorldHint: true, which is web_fetch's shape and mail_send's.
func openWorldTools(names ...string) []mcp.Tool {
	tools := make([]mcp.Tool, len(names))
	for i, name := range names {
		tools[i] = mcp.Tool{Name: name, Annotations: json.RawMessage(`{"openWorldHint":true}`)}
	}
	return tools
}

// unannotatedTools carry no annotations at all: mutating by decision 2 and
// open-world by decision 2c, which is what every tool of every MCP that has
// not yet annotated itself looks like.
func unannotatedTools(names ...string) []mcp.Tool {
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
		tools[i].Annotations = json.RawMessage(`{"readOnlyHint":true,"openWorldHint":false}`)
	}
	return tools
}
