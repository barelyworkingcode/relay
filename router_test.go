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
		Prefix:        testToken[:6],
		Suffix:        testToken[len(testToken)-6:],
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

// setCache installs s into the package-level settingsCache and registers
// cleanup to nil it out when the test finishes.
func setCache(t *testing.T, s *Settings) {
	t.Helper()
	settingsMu.Lock()
	settingsCache = s
	settingsMu.Unlock()
	t.Cleanup(func() {
		settingsMu.Lock()
		settingsCache = nil
		settingsMu.Unlock()
	})
}

// newTestRouter creates an appRouter with the given ExternalMcpManager, installs
// settings into the cache, and returns the appRouter.
func newTestRouter(t *testing.T, s *Settings, mgr *ExternalMcpManager) *appRouter {
	t.Helper()
	setCache(t, s)
	return &appRouter{
		tools:    mgr,
		services: NewServiceRegistry(),
		onChange: func() {},
	}
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
	perms := map[string]Permission{"mcp-a": PermOn}
	s := makeSettings(perms, nil, nil)
	s.ExternalMcps = []ExternalMcp{
		{ID: "mcp-a", DisplayName: "MCP A"},
	}

	mgr := NewExternalMcpManager(nil, nil)
	mgr.mu.Lock()
	mgr.conns["mcp-a"] = &mockMcpConn{
		tools: []mcp.Tool{
			{Name: "tool_one", Description: "First tool"},
			{Name: "tool_two", Description: "Second tool"},
		},
		config: ExternalMcp{ID: "mcp-a"},
	}
	mgr.mu.Unlock()

	r := newTestRouter(t, s, mgr)
	result, err := r.ListTools(context.Background(), testToken)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	var tools []mcp.Tool
	if err := json.Unmarshal(result, &tools); err != nil {
		t.Fatalf("failed to unmarshal tools: %v", err)
	}
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
	perms := map[string]Permission{
		"mcp-a": PermOn,
		"mcp-b": PermOff,
	}
	s := makeSettings(perms, nil, nil)
	s.ExternalMcps = []ExternalMcp{
		{ID: "mcp-a", DisplayName: "MCP A"},
		{ID: "mcp-b", DisplayName: "MCP B"},
	}

	mgr := NewExternalMcpManager(nil, nil)
	mgr.mu.Lock()
	mgr.conns["mcp-a"] = &mockMcpConn{
		tools:  []mcp.Tool{{Name: "alpha_tool"}},
		config: ExternalMcp{ID: "mcp-a"},
	}
	mgr.conns["mcp-b"] = &mockMcpConn{
		tools:  []mcp.Tool{{Name: "beta_tool"}},
		config: ExternalMcp{ID: "mcp-b"},
	}
	mgr.mu.Unlock()

	r := newTestRouter(t, s, mgr)
	result, err := r.ListTools(context.Background(), testToken)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	var tools []mcp.Tool
	if err := json.Unmarshal(result, &tools); err != nil {
		t.Fatalf("failed to unmarshal tools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool (beta excluded), got %d", len(tools))
	}
	if tools[0].Name != "alpha_tool" {
		t.Errorf("expected 'alpha_tool', got %q", tools[0].Name)
	}
}

func TestListTools_ExcludesDisabledTools(t *testing.T) {
	perms := map[string]Permission{"mcp-a": PermOn}
	disabled := map[string][]string{"mcp-a": {"tool_two"}}
	s := makeSettings(perms, disabled, nil)
	s.ExternalMcps = []ExternalMcp{
		{ID: "mcp-a", DisplayName: "MCP A"},
	}

	mgr := NewExternalMcpManager(nil, nil)
	mgr.mu.Lock()
	mgr.conns["mcp-a"] = &mockMcpConn{
		tools: []mcp.Tool{
			{Name: "tool_one", Description: "Allowed"},
			{Name: "tool_two", Description: "Disabled"},
			{Name: "tool_three", Description: "Also allowed"},
		},
		config: ExternalMcp{ID: "mcp-a"},
	}
	mgr.mu.Unlock()

	r := newTestRouter(t, s, mgr)
	result, err := r.ListTools(context.Background(), testToken)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	var tools []mcp.Tool
	if err := json.Unmarshal(result, &tools); err != nil {
		t.Fatalf("failed to unmarshal tools: %v", err)
	}
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
	perms := map[string]Permission{"mcp-a": PermOff}
	s := makeSettings(perms, nil, nil)
	s.ExternalMcps = []ExternalMcp{
		{ID: "mcp-a", DisplayName: "MCP A"},
	}

	mgr := NewExternalMcpManager(nil, nil)
	mgr.mu.Lock()
	mgr.conns["mcp-a"] = &mockMcpConn{
		tools:  []mcp.Tool{{Name: "blocked_tool"}},
		config: ExternalMcp{ID: "mcp-a"},
	}
	mgr.mu.Unlock()

	r := newTestRouter(t, s, mgr)
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
	perms := map[string]Permission{"mcp-a": PermOn}
	s := makeSettings(perms, nil, nil)
	s.ExternalMcps = []ExternalMcp{
		{ID: "mcp-a", DisplayName: "MCP A"},
	}

	mgr := NewExternalMcpManager(nil, nil)
	mgr.mu.Lock()
	mgr.conns["mcp-a"] = &mockMcpConn{
		tools:  []mcp.Tool{{Name: "do_thing"}},
		config: ExternalMcp{ID: "mcp-a"},
		sendRequestFunc: func(_ context.Context, method string, params interface{}) (json.RawMessage, error) {
			if method == "tools/call" {
				return json.RawMessage(`{"content":[{"type":"text","text":"done"}]}`), nil
			}
			return nil, fmt.Errorf("unexpected method: %s", method)
		},
	}
	mgr.mu.Unlock()

	r := newTestRouter(t, s, mgr)
	result, err := r.CallTool(context.Background(),"do_thing", json.RawMessage(`{"x":1}`), testToken)
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
	perms := map[string]Permission{"mcp-a": PermOn}
	s := makeSettings(perms, nil, nil)
	s.ExternalMcps = []ExternalMcp{
		{ID: "mcp-a", DisplayName: "MCP A"},
	}

	mgr := NewExternalMcpManager(nil, nil)
	mgr.mu.Lock()
	mgr.conns["mcp-a"] = &mockMcpConn{
		tools:  []mcp.Tool{{Name: "real_tool"}},
		config: ExternalMcp{ID: "mcp-a"},
	}
	mgr.mu.Unlock()

	r := newTestRouter(t, s, mgr)
	_, err := r.CallTool(context.Background(),"nonexistent_tool", nil, testToken)
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
	if !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("expected 'unknown tool' in error, got %q", err.Error())
	}
}

func TestCallTool_DisabledMcp(t *testing.T) {
	perms := map[string]Permission{"mcp-a": PermOff}
	s := makeSettings(perms, nil, nil)
	s.ExternalMcps = []ExternalMcp{
		{ID: "mcp-a", DisplayName: "MCP A"},
	}

	mgr := NewExternalMcpManager(nil, nil)
	mgr.mu.Lock()
	mgr.conns["mcp-a"] = &mockMcpConn{
		tools:  []mcp.Tool{{Name: "blocked_tool"}},
		config: ExternalMcp{ID: "mcp-a"},
	}
	mgr.mu.Unlock()

	r := newTestRouter(t, s, mgr)
	_, err := r.CallTool(context.Background(),"blocked_tool", nil, testToken)
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
	perms := map[string]Permission{"mcp-a": PermOn}
	disabled := map[string][]string{"mcp-a": {"forbidden_tool"}}
	s := makeSettings(perms, disabled, nil)
	s.ExternalMcps = []ExternalMcp{
		{ID: "mcp-a", DisplayName: "MCP A"},
	}

	mgr := NewExternalMcpManager(nil, nil)
	mgr.mu.Lock()
	mgr.conns["mcp-a"] = &mockMcpConn{
		tools:  []mcp.Tool{{Name: "forbidden_tool"}},
		config: ExternalMcp{ID: "mcp-a"},
	}
	mgr.mu.Unlock()

	r := newTestRouter(t, s, mgr)
	_, err := r.CallTool(context.Background(),"forbidden_tool", nil, testToken)
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
	perms := map[string]Permission{"mcp-a": PermOn}
	ctx := map[string]json.RawMessage{
		"mcp-a": json.RawMessage(`{"allowed_dirs":["/tmp","/home"]}`),
	}
	s := makeSettings(perms, nil, ctx)
	s.ExternalMcps = []ExternalMcp{
		{ID: "mcp-a", DisplayName: "MCP A"},
	}

	var capturedParams map[string]interface{}
	mgr := NewExternalMcpManager(nil, nil)
	mgr.mu.Lock()
	mgr.conns["mcp-a"] = &mockMcpConn{
		tools:  []mcp.Tool{{Name: "fs_read"}},
		config: ExternalMcp{ID: "mcp-a"},
		sendRequestFunc: func(_ context.Context, method string, params interface{}) (json.RawMessage, error) {
			if p, ok := params.(map[string]interface{}); ok {
				capturedParams = p
			}
			return json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`), nil
		},
	}
	mgr.mu.Unlock()

	r := newTestRouter(t, s, mgr)
	_, err := r.CallTool(context.Background(),"fs_read", json.RawMessage(`{"path":"/tmp/f"}`), testToken)
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
	perms := map[string]Permission{"mcp-a": PermOn}
	// No context set for the token.
	s := makeSettings(perms, nil, nil)
	s.ExternalMcps = []ExternalMcp{
		{ID: "mcp-a", DisplayName: "MCP A"},
	}

	var capturedParams map[string]interface{}
	mgr := NewExternalMcpManager(nil, nil)
	mgr.mu.Lock()
	mgr.conns["mcp-a"] = &mockMcpConn{
		tools:  []mcp.Tool{{Name: "fs_read"}},
		config: ExternalMcp{ID: "mcp-a"},
		sendRequestFunc: func(_ context.Context, method string, params interface{}) (json.RawMessage, error) {
			if p, ok := params.(map[string]interface{}); ok {
				capturedParams = p
			}
			return json.RawMessage(`{"content":[]}`), nil
		},
	}
	mgr.mu.Unlock()

	r := newTestRouter(t, s, mgr)
	_, err := r.CallTool(context.Background(),"fs_read", nil, testToken)
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

	_, err := r.CallTool(context.Background(),"any_tool", nil, "wrong-token")
	if err == nil {
		t.Fatal("expected error for invalid token")
	}
}

func TestCallTool_RoutesToCorrectMcp(t *testing.T) {
	perms := map[string]Permission{"mcp-a": PermOn, "mcp-b": PermOn}
	s := makeSettings(perms, nil, nil)
	s.ExternalMcps = []ExternalMcp{
		{ID: "mcp-a", DisplayName: "MCP A"},
		{ID: "mcp-b", DisplayName: "MCP B"},
	}

	var calledMcp string
	mgr := NewExternalMcpManager(nil, nil)
	mgr.mu.Lock()
	mgr.conns["mcp-a"] = &mockMcpConn{
		tools:  []mcp.Tool{{Name: "alpha_do"}},
		config: ExternalMcp{ID: "mcp-a"},
		sendRequestFunc: func(_ context.Context, method string, params interface{}) (json.RawMessage, error) {
			calledMcp = "mcp-a"
			return json.RawMessage(`{"content":[]}`), nil
		},
	}
	mgr.conns["mcp-b"] = &mockMcpConn{
		tools:  []mcp.Tool{{Name: "beta_do"}},
		config: ExternalMcp{ID: "mcp-b"},
		sendRequestFunc: func(_ context.Context, method string, params interface{}) (json.RawMessage, error) {
			calledMcp = "mcp-b"
			return json.RawMessage(`{"content":[]}`), nil
		},
	}
	mgr.mu.Unlock()

	r := newTestRouter(t, s, mgr)

	_, err := r.CallTool(context.Background(),"beta_do", nil, testToken)
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
