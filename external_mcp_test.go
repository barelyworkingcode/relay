package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"relaygo/mcp"
)

// ---------------------------------------------------------------------------
// mcpHandshake tests
// ---------------------------------------------------------------------------

func TestMcpHandshake_Success(t *testing.T) {
	callCount := 0
	mock := &mockMcpConn{
		sendRequestFunc: func(_ context.Context, method string, params interface{}) (json.RawMessage, error) {
			callCount++
			switch method {
			case mcp.MethodInitialize:
				return json.RawMessage(`{"serverInfo":{"name":"test-server","version":"0.1"}}`), nil
			case mcp.MethodToolsList:
				return json.RawMessage(`{"tools":[{"name":"fs_read","description":"Read a file","inputSchema":{}},{"name":"fs_write","description":"Write a file","inputSchema":{}}]}`), nil
			default:
				return nil, fmt.Errorf("unexpected method: %s", method)
			}
		},
	}

	result, err := mcpHandshake(context.Background(), mock)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if len(result.Tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(result.Tools))
	}
	if result.Tools[0].Name != "fs_read" {
		t.Errorf("expected first tool name 'fs_read', got %q", result.Tools[0].Name)
	}
	if result.Tools[1].Name != "fs_write" {
		t.Errorf("expected second tool name 'fs_write', got %q", result.Tools[1].Name)
	}
	if len(result.ToolInfos) != 2 {
		t.Fatalf("expected 2 ToolInfos, got %d", len(result.ToolInfos))
	}
	if result.ToolInfos[0].Name != "fs_read" {
		t.Errorf("expected first ToolInfo name 'fs_read', got %q", result.ToolInfos[0].Name)
	}
	if result.ToolInfos[0].Category != "Fs" {
		t.Errorf("expected category 'Fs', got %q", result.ToolInfos[0].Category)
	}
	// Verify notifications/initialized was sent.
	if len(mock.notifications) != 1 || mock.notifications[0] != mcp.MethodInitialized {
		t.Errorf("expected notifications/initialized, got %v", mock.notifications)
	}
	if callCount != 2 {
		t.Errorf("expected 2 SendRequest calls, got %d", callCount)
	}
}

func TestMcpHandshake_InitializeFailure(t *testing.T) {
	mock := &mockMcpConn{
		sendRequestFunc: func(_ context.Context, method string, params interface{}) (json.RawMessage, error) {
			if method == mcp.MethodInitialize {
				return nil, fmt.Errorf("connection refused")
			}
			return nil, fmt.Errorf("unexpected method: %s", method)
		},
	}

	result, err := mcpHandshake(context.Background(), mock)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if result != nil {
		t.Fatal("expected nil result on error")
	}
	expected := "MCP handshake failed: connection refused"
	if err.Error() != expected {
		t.Errorf("expected error %q, got %q", expected, err.Error())
	}
	// notifications/initialized should NOT have been sent since initialize failed.
	if len(mock.notifications) != 0 {
		t.Errorf("expected no notifications on initialize failure, got %v", mock.notifications)
	}
}

func TestMcpHandshake_ToolsListFailure(t *testing.T) {
	mock := &mockMcpConn{
		sendRequestFunc: func(_ context.Context, method string, params interface{}) (json.RawMessage, error) {
			switch method {
			case mcp.MethodInitialize:
				return json.RawMessage(`{"serverInfo":{"name":"test"}}`), nil
			case mcp.MethodToolsList:
				return nil, fmt.Errorf("tools list timeout")
			default:
				return nil, fmt.Errorf("unexpected method: %s", method)
			}
		},
	}

	result, err := mcpHandshake(context.Background(), mock)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if result != nil {
		t.Fatal("expected nil result on error")
	}
	expected := "tools/list failed: tools list timeout"
	if err.Error() != expected {
		t.Errorf("expected error %q, got %q", expected, err.Error())
	}
	// notifications/initialized should still have been sent (it fires before tools/list).
	if len(mock.notifications) != 1 || mock.notifications[0] != mcp.MethodInitialized {
		t.Errorf("expected notifications/initialized even on tools/list failure, got %v", mock.notifications)
	}
}

func TestMcpHandshake_ContextSchemaExtracted(t *testing.T) {
	mock := &mockMcpConn{
		sendRequestFunc: func(_ context.Context, method string, params interface{}) (json.RawMessage, error) {
			switch method {
			case mcp.MethodInitialize:
				return json.RawMessage(`{"serverInfo":{"name":"ctx-server","contextSchema":{"type":"object","properties":{"allowed_dirs":{"type":"array"}}}}}`), nil
			case mcp.MethodToolsList:
				return json.RawMessage(`{"tools":[]}`), nil
			default:
				return nil, fmt.Errorf("unexpected method: %s", method)
			}
		},
	}

	result, err := mcpHandshake(context.Background(), mock)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.ContextSchema == nil {
		t.Fatal("expected non-nil ContextSchema")
	}
	var schema map[string]interface{}
	if err := json.Unmarshal(result.ContextSchema, &schema); err != nil {
		t.Fatalf("failed to unmarshal ContextSchema: %v", err)
	}
	if schema["type"] != "object" {
		t.Errorf("expected schema type 'object', got %v", schema["type"])
	}
}

// ---------------------------------------------------------------------------
// extractContextSchema tests
// ---------------------------------------------------------------------------

func TestExtractContextSchema(t *testing.T) {
	tests := []struct {
		name     string
		input    json.RawMessage
		wantNil  bool
		wantType string // expected "type" field if non-nil
	}{
		{"nil input", nil, true, ""},
		{"valid with schema", json.RawMessage(`{"serverInfo":{"name":"test","contextSchema":{"type":"object","properties":{"dirs":{"type":"array"}}}}}`), false, "object"},
		{"no contextSchema", json.RawMessage(`{"serverInfo":{"name":"test","version":"1.0"}}`), true, ""},
		{"malformed JSON", json.RawMessage(`{not valid json`), true, ""},
		{"empty serverInfo", json.RawMessage(`{"serverInfo":{}}`), true, ""},
		{"no serverInfo", json.RawMessage(`{"protocolVersion":"2024-11-05"}`), true, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := extractContextSchema(tc.input)
			if tc.wantNil {
				if result != nil {
					t.Errorf("expected nil, got %s", string(result))
				}
				return
			}
			if result == nil {
				t.Fatal("expected non-nil result")
			}
			var schema map[string]interface{}
			if err := json.Unmarshal(result, &schema); err != nil {
				t.Fatalf("failed to unmarshal: %v", err)
			}
			if schema["type"] != tc.wantType {
				t.Errorf("expected type %q, got %v", tc.wantType, schema["type"])
			}
		})
	}
}

// ---------------------------------------------------------------------------
// toolCategory tests
// ---------------------------------------------------------------------------

func TestToolCategory(t *testing.T) {
	tests := []struct {
		name     string
		tool     mcp.Tool
		expected string
	}{
		{"explicit category", mcp.Tool{Name: "foo_bar", Category: "CustomCat"}, "CustomCat"},
		{"derived from underscore", mcp.Tool{Name: "foo_bar"}, "Foo"},
		{"no underscore", mcp.Tool{Name: "foobar"}, ""},
		{"leading underscore", mcp.Tool{Name: "_bar"}, ""},
		{"multiple underscores", mcp.Tool{Name: "abc_def_ghi"}, "Abc"},
		{"uppercase prefix", mcp.Tool{Name: "ABC_action"}, "Abc"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := toolCategory(tc.tool)
			if got != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ExternalMcpManager tests
// ---------------------------------------------------------------------------

func TestManager_ToolsUnknownID(t *testing.T) {
	mgr := NewExternalMcpManager(nil)
	tools := mgr.Tools("nonexistent")
	if tools != nil {
		t.Errorf("expected nil for unknown ID, got %v", tools)
	}
}

func TestManager_FindToolOwnerUnknown(t *testing.T) {
	mgr := NewExternalMcpManager(nil)
	id, cfg := mgr.FindToolOwner("no_such_tool")
	if id != "" {
		t.Errorf("expected empty id, got %q", id)
	}
	if cfg != nil {
		t.Errorf("expected nil config, got %v", cfg)
	}
}

func TestManager_ToolsWithConnection(t *testing.T) {
	mgr := NewExternalMcpManager(nil)
	expectedTools := []mcp.Tool{
		{Name: "fs_read", Description: "Read a file"},
		{Name: "fs_write", Description: "Write a file"},
	}
	addMockConn(mgr, "test-mcp", &mockMcpConn{
		tools:  expectedTools,
		config: ExternalMcp{ID: "test-mcp", DisplayName: "Test MCP"},
	})

	tools := mgr.Tools("test-mcp")
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	if tools[0].Name != "fs_read" {
		t.Errorf("expected 'fs_read', got %q", tools[0].Name)
	}
	if tools[1].Name != "fs_write" {
		t.Errorf("expected 'fs_write', got %q", tools[1].Name)
	}
}

func TestManager_FindToolOwnerWithConnection(t *testing.T) {
	mgr := NewExternalMcpManager(nil)
	addMockConn(mgr, "net-mcp", &mockMcpConn{
		tools:  simpleTools("net_fetch"),
		config: ExternalMcp{ID: "net-mcp", DisplayName: "Net MCP", Command: "/usr/bin/net-mcp"},
	})

	id, cfg := mgr.FindToolOwner("net_fetch")
	if id != "net-mcp" {
		t.Errorf("expected id 'net-mcp', got %q", id)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if cfg.DisplayName != "Net MCP" {
		t.Errorf("expected DisplayName 'Net MCP', got %q", cfg.DisplayName)
	}
}

func TestManager_Stop(t *testing.T) {
	mgr := NewExternalMcpManager(nil)
	mock := &mockMcpConn{tools: simpleTools("test_tool")}
	addMockConn(mgr, "stop-me", mock)

	mgr.Stop("stop-me")

	if !mock.closed {
		t.Error("expected connection to be closed")
	}
	if tools := mgr.Tools("stop-me"); tools != nil {
		t.Errorf("expected nil tools after stop, got %v", tools)
	}
}

func TestManager_StopNonexistent(t *testing.T) {
	mgr := NewExternalMcpManager(nil)
	// Should not panic.
	mgr.Stop("does-not-exist")
}

func TestManager_StopAll(t *testing.T) {
	mgr := NewExternalMcpManager(nil)
	mock1 := &mockMcpConn{tools: simpleTools("a_tool")}
	mock2 := &mockMcpConn{tools: simpleTools("b_tool")}
	addMockConn(mgr, "mcp-1", mock1)
	addMockConn(mgr, "mcp-2", mock2)

	mgr.StopAll()

	if !mock1.closed {
		t.Error("expected mock1 to be closed")
	}
	if !mock2.closed {
		t.Error("expected mock2 to be closed")
	}
	if mgr.Tools("mcp-1") != nil || mgr.Tools("mcp-2") != nil {
		t.Error("expected no tools after StopAll")
	}
}

func TestManager_CallToolNotConnected(t *testing.T) {
	mgr := NewExternalMcpManager(nil)
	_, err := mgr.CallTool(context.Background(), "missing", "some_tool", nil, nil)
	if err == nil {
		t.Fatal("expected error for unconnected MCP")
	}
	expected := "external MCP 'missing' not connected"
	if err.Error() != expected {
		t.Errorf("expected %q, got %q", expected, err.Error())
	}
}

func TestManager_CallToolSuccess(t *testing.T) {
	mgr := NewExternalMcpManager(nil)
	addMockConn(mgr, "echo-mcp", newMockConn("echo-mcp", simpleTools("echo_tool"),
		func(_ context.Context, method string, _ interface{}) (json.RawMessage, error) {
			if method != mcp.MethodToolsCall {
				return nil, fmt.Errorf("unexpected method: %s", method)
			}
			return json.RawMessage(`{"content":[{"type":"text","text":"hello"}]}`), nil
		}))

	result, err := mgr.CallTool(context.Background(), "echo-mcp", "echo_tool", json.RawMessage(`{"msg":"hi"}`), nil)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	content, ok := parsed["content"].([]interface{})
	if !ok || len(content) == 0 {
		t.Fatal("expected non-empty content array")
	}
}

func TestManager_CallToolWithMeta(t *testing.T) {
	mgr := NewExternalMcpManager(nil)
	var capturedParams map[string]interface{}
	addMockConn(mgr, "fs-mcp", newMockConn("fs-mcp", simpleTools("fs_tool"),
		func(_ context.Context, _ string, params interface{}) (json.RawMessage, error) {
			if p, ok := params.(map[string]interface{}); ok {
				capturedParams = p
			}
			return json.RawMessage(`{"content":[]}`), nil
		}))

	meta := json.RawMessage(`{"allowed_dirs":["/tmp"]}`)
	_, err := mgr.CallTool(context.Background(), "fs-mcp", "fs_tool", json.RawMessage(`{"path":"/tmp/f"}`), meta)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if capturedParams == nil {
		t.Fatal("expected captured params")
	}
	if capturedParams["name"] != "fs_tool" {
		t.Errorf("expected name 'fs_tool', got %v", capturedParams["name"])
	}
	if capturedParams["_meta"] == nil {
		t.Error("expected _meta to be injected into params")
	}
}

func TestManager_MultipleConnectionsFindCorrectOwner(t *testing.T) {
	mgr := NewExternalMcpManager(nil)
	addMockConn(mgr, "mcp-alpha", &mockMcpConn{
		tools:  simpleTools("alpha_tool"),
		config: ExternalMcp{ID: "mcp-alpha", DisplayName: "Alpha"},
	})
	addMockConn(mgr, "mcp-beta", &mockMcpConn{
		tools:  simpleTools("beta_tool"),
		config: ExternalMcp{ID: "mcp-beta", DisplayName: "Beta"},
	})

	id, cfg := mgr.FindToolOwner("beta_tool")
	if id != "mcp-beta" {
		t.Errorf("expected 'mcp-beta', got %q", id)
	}
	if cfg == nil || cfg.DisplayName != "Beta" {
		t.Errorf("expected Beta config, got %v", cfg)
	}

	id, cfg = mgr.FindToolOwner("alpha_tool")
	if id != "mcp-alpha" {
		t.Errorf("expected 'mcp-alpha', got %q", id)
	}
	if cfg == nil || cfg.DisplayName != "Alpha" {
		t.Errorf("expected Alpha config, got %v", cfg)
	}
}
