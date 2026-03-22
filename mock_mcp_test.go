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

func (m *mockMcpConn) GetTools() []mcp.Tool   { return m.tools }
func (m *mockMcpConn) GetConfig() ExternalMcp { return m.config }
