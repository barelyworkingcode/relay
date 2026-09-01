package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/mcp"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
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

// The manager's connection and schema tables are unexported and stay that
// way; mcpbroker's own test seam is the only way in from here, and it
// panics outside a test binary.
func addMockConn(mgr *mcpbroker.Manager, id string, mock *mockMcpConn) {
	mgr.SetConnectionForTest(id, mock)
}

func addMockSchema(mgr *mcpbroker.Manager, id, schema string, version int) {
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
// "mutating" and "open-world" rather than as an unset default. That's usable
// in a LOCAL project's fixture, where it's exactly the ordinary tool such a
// project calls all day — a remote-profile fixture has to say what it means
// on both axes instead (see macmcpToolSurface).
func simpleTools(names ...string) []mcp.Tool {
	tools := make([]mcp.Tool, len(names))
	for i, name := range names {
		tools[i] = mcp.Tool{Name: name}
	}
	return tools
}

// localTools is simpleTools plus openWorldHint: false, still silent on
// readOnlyHint — for a fixture that must be callable from a profile with no
// outbound grant.
func localTools(names ...string) []mcp.Tool {
	tools := simpleTools(names...)
	for i := range tools {
		tools[i].Annotations = json.RawMessage(`{"openWorldHint":false}`)
	}
	return tools
}

func openWorldTools(names ...string) []mcp.Tool {
	tools := simpleTools(names...)
	for i := range tools {
		tools[i].Annotations = json.RawMessage(`{"openWorldHint":true}`)
	}
	return tools
}

// readOnlyTools is simpleTools with both hints declared honestly: read-only
// and staying on this host. Deliberately a separate helper rather than a
// default on simpleTools: an unannotated tool being refused to a read-only
// grant is exactly what several tests measure, so a helper that quietly
// annotated everything would make that case unreachable.
func readOnlyTools(names ...string) []mcp.Tool {
	tools := simpleTools(names...)
	for i := range tools {
		tools[i].Annotations = json.RawMessage(`{"readOnlyHint":true,"openWorldHint":false}`)
	}
	return tools
}
