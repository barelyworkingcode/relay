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
			case "initialize":
				return json.RawMessage(`{"serverInfo":{"name":"test-server","version":"0.1"}}`), nil
			case "tools/list":
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
	if len(mock.notifications) != 1 || mock.notifications[0] != "notifications/initialized" {
		t.Errorf("expected notifications/initialized, got %v", mock.notifications)
	}
	if callCount != 2 {
		t.Errorf("expected 2 SendRequest calls, got %d", callCount)
	}
}

func TestMcpHandshake_InitializeFailure(t *testing.T) {
	mock := &mockMcpConn{
		sendRequestFunc: func(_ context.Context, method string, params interface{}) (json.RawMessage, error) {
			if method == "initialize" {
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
			case "initialize":
				return json.RawMessage(`{"serverInfo":{"name":"test"}}`), nil
			case "tools/list":
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
	if len(mock.notifications) != 1 || mock.notifications[0] != "notifications/initialized" {
		t.Errorf("expected notifications/initialized even on tools/list failure, got %v", mock.notifications)
	}
}

func TestMcpHandshake_ContextSchemaExtracted(t *testing.T) {
	mock := &mockMcpConn{
		sendRequestFunc: func(_ context.Context, method string, params interface{}) (json.RawMessage, error) {
			switch method {
			case "initialize":
				return json.RawMessage(`{"serverInfo":{"name":"ctx-server","contextSchema":{"type":"object","properties":{"allowed_dirs":{"type":"array"}}}}}`), nil
			case "tools/list":
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

func TestExtractContextSchema_NilInput(t *testing.T) {
	result := extractContextSchema(nil)
	if result != nil {
		t.Errorf("expected nil for nil input, got %s", string(result))
	}
}

func TestExtractContextSchema_ValidResponse(t *testing.T) {
	resp := json.RawMessage(`{"serverInfo":{"name":"test","contextSchema":{"type":"object","properties":{"dirs":{"type":"array"}}}}}`)
	result := extractContextSchema(resp)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	var schema map[string]interface{}
	if err := json.Unmarshal(result, &schema); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if schema["type"] != "object" {
		t.Errorf("expected type 'object', got %v", schema["type"])
	}
}

func TestExtractContextSchema_NoContextSchema(t *testing.T) {
	resp := json.RawMessage(`{"serverInfo":{"name":"test","version":"1.0"}}`)
	result := extractContextSchema(resp)
	if result != nil {
		t.Errorf("expected nil when no contextSchema, got %s", string(result))
	}
}

func TestExtractContextSchema_MalformedJSON(t *testing.T) {
	resp := json.RawMessage(`{not valid json`)
	result := extractContextSchema(resp)
	if result != nil {
		t.Errorf("expected nil for malformed JSON, got %s", string(result))
	}
}

func TestExtractContextSchema_EmptyServerInfo(t *testing.T) {
	resp := json.RawMessage(`{"serverInfo":{}}`)
	result := extractContextSchema(resp)
	if result != nil {
		t.Errorf("expected nil for empty serverInfo, got %s", string(result))
	}
}

func TestExtractContextSchema_NoServerInfo(t *testing.T) {
	resp := json.RawMessage(`{"protocolVersion":"2024-11-05"}`)
	result := extractContextSchema(resp)
	if result != nil {
		t.Errorf("expected nil for missing serverInfo, got %s", string(result))
	}
}

// ---------------------------------------------------------------------------
// toolCategory tests
// ---------------------------------------------------------------------------

func TestToolCategory_ExplicitCategory(t *testing.T) {
	tool := mcp.Tool{Name: "foo_bar", Category: "CustomCat"}
	got := toolCategory(tool)
	if got != "CustomCat" {
		t.Errorf("expected 'CustomCat', got %q", got)
	}
}

func TestToolCategory_DerivedFromUnderscore(t *testing.T) {
	tool := mcp.Tool{Name: "foo_bar"}
	got := toolCategory(tool)
	if got != "Foo" {
		t.Errorf("expected 'Foo', got %q", got)
	}
}

func TestToolCategory_NoUnderscore(t *testing.T) {
	tool := mcp.Tool{Name: "foobar"}
	got := toolCategory(tool)
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestToolCategory_LeadingUnderscore(t *testing.T) {
	tool := mcp.Tool{Name: "_bar"}
	got := toolCategory(tool)
	if got != "" {
		t.Errorf("expected empty string for leading underscore, got %q", got)
	}
}

func TestToolCategory_MultipleUnderscores(t *testing.T) {
	tool := mcp.Tool{Name: "abc_def_ghi"}
	got := toolCategory(tool)
	if got != "Abc" {
		t.Errorf("expected 'Abc', got %q", got)
	}
}

func TestToolCategory_UppercasePrefix(t *testing.T) {
	tool := mcp.Tool{Name: "ABC_action"}
	got := toolCategory(tool)
	if got != "Abc" {
		t.Errorf("expected 'Abc', got %q", got)
	}
}

// ---------------------------------------------------------------------------
// ExternalMcpManager tests
// ---------------------------------------------------------------------------

func TestManager_ToolsUnknownID(t *testing.T) {
	mgr := NewExternalMcpManager(nil, nil)
	tools := mgr.Tools("nonexistent")
	if tools != nil {
		t.Errorf("expected nil for unknown ID, got %v", tools)
	}
}

func TestManager_FindToolOwnerUnknown(t *testing.T) {
	mgr := NewExternalMcpManager(nil, nil)
	id, cfg := mgr.FindToolOwner("no_such_tool")
	if id != "" {
		t.Errorf("expected empty id, got %q", id)
	}
	if cfg != nil {
		t.Errorf("expected nil config, got %v", cfg)
	}
}

func TestManager_ToolsWithConnection(t *testing.T) {
	mgr := NewExternalMcpManager(nil, nil)
	expectedTools := []mcp.Tool{
		{Name: "fs_read", Description: "Read a file"},
		{Name: "fs_write", Description: "Write a file"},
	}
	mock := &mockMcpConn{
		tools: expectedTools,
		config: ExternalMcp{
			ID:          "test-mcp",
			DisplayName: "Test MCP",
		},
	}

	mgr.mu.Lock()
	mgr.conns["test-mcp"] = mock
	mgr.mu.Unlock()

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
	mgr := NewExternalMcpManager(nil, nil)
	mock := &mockMcpConn{
		tools: []mcp.Tool{
			{Name: "net_fetch", Description: "Fetch URL"},
		},
		config: ExternalMcp{
			ID:          "net-mcp",
			DisplayName: "Net MCP",
			Command:     "/usr/bin/net-mcp",
		},
	}

	mgr.mu.Lock()
	mgr.conns["net-mcp"] = mock
	mgr.mu.Unlock()

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
	mgr := NewExternalMcpManager(nil, nil)
	mock := &mockMcpConn{
		tools: []mcp.Tool{{Name: "test_tool"}},
	}

	mgr.mu.Lock()
	mgr.conns["stop-me"] = mock
	mgr.mu.Unlock()

	mgr.Stop("stop-me")

	if !mock.closed {
		t.Error("expected connection to be closed")
	}

	tools := mgr.Tools("stop-me")
	if tools != nil {
		t.Errorf("expected nil tools after stop, got %v", tools)
	}
}

func TestManager_StopNonexistent(t *testing.T) {
	mgr := NewExternalMcpManager(nil, nil)
	// Should not panic.
	mgr.Stop("does-not-exist")
}

func TestManager_StopAll(t *testing.T) {
	mgr := NewExternalMcpManager(nil, nil)
	mock1 := &mockMcpConn{tools: []mcp.Tool{{Name: "a_tool"}}}
	mock2 := &mockMcpConn{tools: []mcp.Tool{{Name: "b_tool"}}}

	mgr.mu.Lock()
	mgr.conns["mcp-1"] = mock1
	mgr.conns["mcp-2"] = mock2
	mgr.mu.Unlock()

	mgr.StopAll()

	if !mock1.closed {
		t.Error("expected mock1 to be closed")
	}
	if !mock2.closed {
		t.Error("expected mock2 to be closed")
	}

	// Verify the manager has no connections left.
	tools1 := mgr.Tools("mcp-1")
	tools2 := mgr.Tools("mcp-2")
	if tools1 != nil || tools2 != nil {
		t.Error("expected no tools after StopAll")
	}
}

func TestManager_CallToolNotConnected(t *testing.T) {
	mgr := NewExternalMcpManager(nil, nil)
	_, err := mgr.CallTool(context.Background(),"missing", "some_tool", nil, nil)
	if err == nil {
		t.Fatal("expected error for unconnected MCP")
	}
	expected := "external MCP 'missing' not connected"
	if err.Error() != expected {
		t.Errorf("expected %q, got %q", expected, err.Error())
	}
}

func TestManager_CallToolSuccess(t *testing.T) {
	mgr := NewExternalMcpManager(nil, nil)
	mock := &mockMcpConn{
		tools: []mcp.Tool{{Name: "echo_tool"}},
		config: ExternalMcp{
			ID:          "echo-mcp",
			DisplayName: "Echo MCP",
		},
		sendRequestFunc: func(_ context.Context, method string, params interface{}) (json.RawMessage, error) {
			if method != "tools/call" {
				return nil, fmt.Errorf("unexpected method: %s", method)
			}
			return json.RawMessage(`{"content":[{"type":"text","text":"hello"}]}`), nil
		},
	}

	mgr.mu.Lock()
	mgr.conns["echo-mcp"] = mock
	mgr.mu.Unlock()

	result, err := mgr.CallTool(context.Background(),"echo-mcp", "echo_tool", json.RawMessage(`{"msg":"hi"}`), nil)
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
	mgr := NewExternalMcpManager(nil, nil)
	var capturedParams map[string]interface{}
	mock := &mockMcpConn{
		tools: []mcp.Tool{{Name: "fs_tool"}},
		sendRequestFunc: func(_ context.Context, method string, params interface{}) (json.RawMessage, error) {
			if p, ok := params.(map[string]interface{}); ok {
				capturedParams = p
			}
			return json.RawMessage(`{"content":[]}`), nil
		},
	}

	mgr.mu.Lock()
	mgr.conns["fs-mcp"] = mock
	mgr.mu.Unlock()

	meta := json.RawMessage(`{"allowed_dirs":["/tmp"]}`)
	_, err := mgr.CallTool(context.Background(),"fs-mcp", "fs_tool", json.RawMessage(`{"path":"/tmp/f"}`), meta)
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
	mgr := NewExternalMcpManager(nil, nil)
	mock1 := &mockMcpConn{
		tools:  []mcp.Tool{{Name: "alpha_tool"}},
		config: ExternalMcp{ID: "mcp-alpha", DisplayName: "Alpha"},
	}
	mock2 := &mockMcpConn{
		tools:  []mcp.Tool{{Name: "beta_tool"}},
		config: ExternalMcp{ID: "mcp-beta", DisplayName: "Beta"},
	}

	mgr.mu.Lock()
	mgr.conns["mcp-alpha"] = mock1
	mgr.conns["mcp-beta"] = mock2
	mgr.mu.Unlock()

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
