package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"relaygo/mcp"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// testToken is a known plaintext token used across router tests.
const testToken = "aaaaaabbbbbbccccccddddddeeeeee0011223344556677889900aabbccddeeff"

// makeSettings builds a Settings with one token whose hash matches testToken.
// The token has the given permissions and disabled tools.
func makeSettings(perms map[string]Permission, disabled map[string][]string, context map[string]json.RawMessage) *Settings {
	hash := hashToken(testToken)
	tok := StoredToken{
		Name:          "test-token",
		Hash:          hash,
		Prefix:        testToken[:tokenDisplayLen],
		Suffix:        testToken[len(testToken)-tokenDisplayLen:],
		CreatedAt:     "2025-01-01T00:00:00Z",
		Permissions:   perms,
		DisabledTools: disabled,
		Context:       context,
	}
	return &Settings{
		Version:      1,
		Tokens:       []StoredToken{tok},
		ExternalMcps: []ExternalMcp{},
		Services:     []ServiceConfig{},
		AdminSecret:  "supersecretadmin",
	}
}

// newTestRouter creates an appRouter with the given settings and ExternalMcpManager.
func newTestRouter(t *testing.T, s *Settings, mgr *ExternalMcpManager) *appRouter {
	t.Helper()
	store := &FileSettingsStore{cache: s}
	return &appRouter{
		store:    store,
		tools:    mgr,
		services: NewServiceRegistry(),
		onChange: func() {},
	}
}

// setupRouter builds a test router with the given MCPs registered in both settings
// and the manager. Each entry maps mcp-id to its mock connection.
func setupRouter(t *testing.T, perms map[string]Permission, disabled map[string][]string, ctx map[string]json.RawMessage, mocks map[string]*mockMcpConn) *appRouter {
	t.Helper()
	s := makeSettings(perms, disabled, ctx)
	for id := range mocks {
		s.ExternalMcps = append(s.ExternalMcps, ExternalMcp{ID: id, DisplayName: strings.ToUpper(id)})
	}
	mgr := NewExternalMcpManager(nil, nil)
	for id, mock := range mocks {
		addMockConn(mgr, id, mock)
	}
	return newTestRouter(t, s, mgr)
}

// okHandler returns a sendRequestFunc that always succeeds with the given JSON.
func okHandler(result string) func(context.Context, string, interface{}) (json.RawMessage, error) {
	return func(_ context.Context, _ string, _ interface{}) (json.RawMessage, error) {
		return json.RawMessage(result), nil
	}
}

// unmarshalTools parses a JSON tool list result and fails the test on error.
func unmarshalTools(t *testing.T, raw json.RawMessage) []mcp.Tool {
	t.Helper()
	var tools []mcp.Tool
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatalf("failed to unmarshal tools: %v", err)
	}
	return tools
}

// ---------------------------------------------------------------------------
// resolveAuth
// ---------------------------------------------------------------------------

func TestResolveAuth_ValidToken(t *testing.T) {
	s := makeSettings(nil, nil, nil)
	r := newTestRouter(t, s, NewExternalMcpManager(nil, nil))

	stored, settings, err := r.resolveAuth(testToken)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if stored == nil {
		t.Fatal("expected non-nil StoredToken")
	}
	if stored.Name != "test-token" {
		t.Errorf("expected token name 'test-token', got %q", stored.Name)
	}
	if settings == nil {
		t.Fatal("expected non-nil Settings")
	}
}

func TestResolveAuth_InvalidToken(t *testing.T) {
	s := makeSettings(nil, nil, nil)
	r := newTestRouter(t, s, NewExternalMcpManager(nil, nil))

	_, _, err := r.resolveAuth("completely-wrong-token")
	if err == nil {
		t.Fatal("expected error for invalid token")
	}
	if err != ErrInvalidToken {
		t.Errorf("expected ErrInvalidToken, got %v", err)
	}
}

func TestResolveAuth_NoTokens(t *testing.T) {
	s := &Settings{
		Version:      1,
		Tokens:       []StoredToken{},
		ExternalMcps: []ExternalMcp{},
		Services:     []ServiceConfig{},
	}
	r := newTestRouter(t, s, NewExternalMcpManager(nil, nil))

	_, _, err := r.resolveAuth(testToken)
	if err == nil {
		t.Fatal("expected error when no tokens configured")
	}
	if err != ErrNoTokens {
		t.Errorf("expected ErrNoTokens, got %v", err)
	}
}

func TestResolveAuth_EmptyToken(t *testing.T) {
	s := makeSettings(nil, nil, nil)
	r := newTestRouter(t, s, NewExternalMcpManager(nil, nil))

	_, _, err := r.resolveAuth("")
	if err == nil {
		t.Fatal("expected error for empty token")
	}
	if err != ErrNoToken {
		t.Errorf("expected ErrNoToken, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// ListTools
// ---------------------------------------------------------------------------

func TestListTools_ReturnsPermittedTools(t *testing.T) {
	r := setupRouter(t,
		map[string]Permission{"mcp-a": PermOn}, nil, nil,
		map[string]*mockMcpConn{
			"mcp-a": newMockConn("mcp-a", []mcp.Tool{
				{Name: "tool_one", Description: "First tool"},
				{Name: "tool_two", Description: "Second tool"},
			}, nil),
		},
	)

	result, err := r.ListTools(context.Background(), testToken)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	tools := unmarshalTools(t, result)
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	if tools[0].Name != "tool_one" {
		t.Errorf("expected 'tool_one', got %q", tools[0].Name)
	}
	if tools[1].Name != "tool_two" {
		t.Errorf("expected 'tool_two', got %q", tools[1].Name)
	}
}

func TestListTools_ExcludesPermOffMcp(t *testing.T) {
	r := setupRouter(t,
		map[string]Permission{"mcp-a": PermOn, "mcp-b": PermOff}, nil, nil,
		map[string]*mockMcpConn{
			"mcp-a": newMockConn("mcp-a", simpleTools("alpha_tool"), nil),
			"mcp-b": newMockConn("mcp-b", simpleTools("beta_tool"), nil),
		},
	)

	result, err := r.ListTools(context.Background(), testToken)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	tools := unmarshalTools(t, result)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool (beta excluded), got %d", len(tools))
	}
	if tools[0].Name != "alpha_tool" {
		t.Errorf("expected 'alpha_tool', got %q", tools[0].Name)
	}
}

func TestListTools_ExcludesDisabledTools(t *testing.T) {
	r := setupRouter(t,
		map[string]Permission{"mcp-a": PermOn},
		map[string][]string{"mcp-a": {"tool_two"}}, nil,
		map[string]*mockMcpConn{
			"mcp-a": newMockConn("mcp-a", []mcp.Tool{
				{Name: "tool_one", Description: "Allowed"},
				{Name: "tool_two", Description: "Disabled"},
				{Name: "tool_three", Description: "Also allowed"},
			}, nil),
		},
	)

	result, err := r.ListTools(context.Background(), testToken)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	tools := unmarshalTools(t, result)
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools (tool_two disabled), got %d", len(tools))
	}
	for _, tool := range tools {
		if tool.Name == "tool_two" {
			t.Error("tool_two should have been excluded (disabled)")
		}
	}
}

func TestListTools_EmptyForTokenWithNoPermittedMcps(t *testing.T) {
	r := setupRouter(t,
		map[string]Permission{"mcp-a": PermOff}, nil, nil,
		map[string]*mockMcpConn{
			"mcp-a": newMockConn("mcp-a", simpleTools("blocked_tool"), nil),
		},
	)

	result, err := r.ListTools(context.Background(), testToken)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	var tools []json.RawMessage
	if err := json.Unmarshal(result, &tools); err != nil {
		t.Fatalf("failed to unmarshal tools: %v", err)
	}
	if len(tools) != 0 {
		t.Errorf("expected 0 tools, got %d", len(tools))
	}
}

func TestListTools_InvalidToken(t *testing.T) {
	s := makeSettings(nil, nil, nil)
	r := newTestRouter(t, s, NewExternalMcpManager(nil, nil))

	_, err := r.ListTools(context.Background(), "bad-token")
	if err == nil {
		t.Fatal("expected error for invalid token")
	}
}

// ---------------------------------------------------------------------------
// CallTool
// ---------------------------------------------------------------------------

func TestCallTool_Success(t *testing.T) {
	r := setupRouter(t,
		map[string]Permission{"mcp-a": PermOn}, nil, nil,
		map[string]*mockMcpConn{
			"mcp-a": newMockConn("mcp-a", simpleTools("do_thing"),
				func(_ context.Context, method string, _ interface{}) (json.RawMessage, error) {
					if method == "tools/call" {
						return json.RawMessage(`{"content":[{"type":"text","text":"done"}]}`), nil
					}
					return nil, fmt.Errorf("unexpected method: %s", method)
				}),
		},
	)

	result, err := r.CallTool(context.Background(), "do_thing", json.RawMessage(`{"x":1}`), testToken)
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

func TestCallTool_UnknownTool(t *testing.T) {
	r := setupRouter(t,
		map[string]Permission{"mcp-a": PermOn}, nil, nil,
		map[string]*mockMcpConn{
			"mcp-a": newMockConn("mcp-a", simpleTools("real_tool"), nil),
		},
	)

	_, err := r.CallTool(context.Background(), "nonexistent_tool", nil, testToken)
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
	if !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("expected 'unknown tool' in error, got %q", err.Error())
	}
}

func TestCallTool_DisabledMcp(t *testing.T) {
	r := setupRouter(t,
		map[string]Permission{"mcp-a": PermOff}, nil, nil,
		map[string]*mockMcpConn{
			"mcp-a": newMockConn("mcp-a", simpleTools("blocked_tool"), nil),
		},
	)

	_, err := r.CallTool(context.Background(), "blocked_tool", nil, testToken)
	if err == nil {
		t.Fatal("expected error for disabled MCP")
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("expected 'access denied' in error, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "disabled for this token") {
		t.Errorf("expected 'disabled for this token' in error, got %q", err.Error())
	}
}

func TestCallTool_DisabledTool(t *testing.T) {
	r := setupRouter(t,
		map[string]Permission{"mcp-a": PermOn},
		map[string][]string{"mcp-a": {"forbidden_tool"}}, nil,
		map[string]*mockMcpConn{
			"mcp-a": newMockConn("mcp-a", simpleTools("forbidden_tool"), nil),
		},
	)

	_, err := r.CallTool(context.Background(), "forbidden_tool", nil, testToken)
	if err == nil {
		t.Fatal("expected error for disabled tool")
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("expected 'access denied' in error, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "tool") {
		t.Errorf("expected 'tool' in error message, got %q", err.Error())
	}
}

func TestCallTool_InjectsMetaContext(t *testing.T) {
	var capturedParams map[string]interface{}
	r := setupRouter(t,
		map[string]Permission{"mcp-a": PermOn}, nil,
		map[string]json.RawMessage{
			"mcp-a": json.RawMessage(`{"allowed_dirs":["/tmp","/home"]}`),
		},
		map[string]*mockMcpConn{
			"mcp-a": newMockConn("mcp-a", simpleTools("fs_read"),
				func(_ context.Context, _ string, params interface{}) (json.RawMessage, error) {
					if p, ok := params.(map[string]interface{}); ok {
						capturedParams = p
					}
					return json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`), nil
				}),
		},
	)

	_, err := r.CallTool(context.Background(), "fs_read", json.RawMessage(`{"path":"/tmp/f"}`), testToken)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if capturedParams == nil {
		t.Fatal("expected captured params from SendRequest")
	}
	if capturedParams["_meta"] == nil {
		t.Fatal("expected _meta to be injected into tool call params")
	}
	meta, ok := capturedParams["_meta"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected _meta to be a map, got %T", capturedParams["_meta"])
	}
	dirs, ok := meta["allowed_dirs"].([]interface{})
	if !ok || len(dirs) != 2 {
		t.Errorf("expected allowed_dirs with 2 entries, got %v", meta["allowed_dirs"])
	}
}

func TestCallTool_NoMetaWhenContextNotSet(t *testing.T) {
	var capturedParams map[string]interface{}
	r := setupRouter(t,
		map[string]Permission{"mcp-a": PermOn}, nil, nil,
		map[string]*mockMcpConn{
			"mcp-a": newMockConn("mcp-a", simpleTools("fs_read"),
				func(_ context.Context, _ string, params interface{}) (json.RawMessage, error) {
					if p, ok := params.(map[string]interface{}); ok {
						capturedParams = p
					}
					return json.RawMessage(`{"content":[]}`), nil
				}),
		},
	)

	_, err := r.CallTool(context.Background(), "fs_read", nil, testToken)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if capturedParams != nil && capturedParams["_meta"] != nil {
		t.Errorf("expected no _meta when context is not set, got %v", capturedParams["_meta"])
	}
}

func TestCallTool_InvalidToken(t *testing.T) {
	s := makeSettings(nil, nil, nil)
	r := newTestRouter(t, s, NewExternalMcpManager(nil, nil))

	_, err := r.CallTool(context.Background(), "any_tool", nil, "wrong-token")
	if err == nil {
		t.Fatal("expected error for invalid token")
	}
}

func TestCallTool_RoutesToCorrectMcp(t *testing.T) {
	var calledMcp string
	r := setupRouter(t,
		map[string]Permission{"mcp-a": PermOn, "mcp-b": PermOn}, nil, nil,
		map[string]*mockMcpConn{
			"mcp-a": newMockConn("mcp-a", simpleTools("alpha_do"),
				func(_ context.Context, _ string, _ interface{}) (json.RawMessage, error) {
					calledMcp = "mcp-a"
					return json.RawMessage(`{"content":[]}`), nil
				}),
			"mcp-b": newMockConn("mcp-b", simpleTools("beta_do"),
				func(_ context.Context, _ string, _ interface{}) (json.RawMessage, error) {
					calledMcp = "mcp-b"
					return json.RawMessage(`{"content":[]}`), nil
				}),
		},
	)

	_, err := r.CallTool(context.Background(), "beta_do", nil, testToken)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if calledMcp != "mcp-b" {
		t.Errorf("expected call routed to 'mcp-b', got %q", calledMcp)
	}
}

// ---------------------------------------------------------------------------
// ValidateAdmin
// ---------------------------------------------------------------------------

func TestValidateAdmin_CorrectSecret(t *testing.T) {
	s := makeSettings(nil, nil, nil)
	r := newTestRouter(t, s, NewExternalMcpManager(nil, nil))

	err := r.ValidateAdmin("supersecretadmin")
	if err != nil {
		t.Fatalf("expected no error for correct admin secret, got %v", err)
	}
}

func TestValidateAdmin_WrongSecret(t *testing.T) {
	s := makeSettings(nil, nil, nil)
	r := newTestRouter(t, s, NewExternalMcpManager(nil, nil))

	err := r.ValidateAdmin("wrongsecret")
	if err == nil {
		t.Fatal("expected error for wrong admin secret")
	}
	if !strings.Contains(err.Error(), "admin authentication failed") {
		t.Errorf("expected 'admin authentication failed' in error, got %q", err.Error())
	}
}

func TestValidateAdmin_EmptySecret(t *testing.T) {
	s := makeSettings(nil, nil, nil)
	r := newTestRouter(t, s, NewExternalMcpManager(nil, nil))

	err := r.ValidateAdmin("")
	if err == nil {
		t.Fatal("expected error for empty admin secret")
	}
	if !strings.Contains(err.Error(), "admin authentication failed") {
		t.Errorf("expected 'admin authentication failed' in error, got %q", err.Error())
	}
}
