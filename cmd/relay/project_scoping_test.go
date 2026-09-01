package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/mcp"
	"github.com/barelyworkingcode/relay/internal/project"
	"github.com/barelyworkingcode/relay/internal/service"
)

// testSchemas declares fsmcp's v1 allowed_dirs field, the trigger for the
// filesystem-scoping branch of the project package's scope derivation.
func testSchemas() project.McpSurfaces {
	return project.McpSurfaces{
		"fsmcp": {Schema: json.RawMessage(`{"allowed_dirs":{"type":"array"}}`)},
	}
}

func TestProjectTokenScoping(t *testing.T) {
	tmpDir := t.TempDir()

	s := &config.Settings{
		Version: 1,
		ExternalMcps: []config.ExternalMcp{
			{ID: "fsmcp", DisplayName: "fsMCP"},
			{ID: "macmcp", DisplayName: "macMCP"},
		},
		Services:    []config.ServiceConfig{},
		Projects:    []config.Project{},
		AdminSecret: config.NewSecret("test-admin"),
	}

	proj, err := project.CreateWithToken(s, "ScopeTest", tmpDir, []string{"fsmcp"}, nil, nil, testSchemas())
	if err != nil {
		t.Fatalf("CreateProjectWithToken failed: %v", err)
	}

	mgr := NewExternalMcpManager(nil)
	addMockConn(mgr, "fsmcp", newMockConn("fsmcp", []mcp.Tool{
		{Name: "fs_read", Description: "Read file"},
		{Name: "fs_write", Description: "Write file"},
		{Name: "fs_bash", Description: "Run bash"},
	}, func(_ context.Context, _ string, _ interface{}) (json.RawMessage, error) {
		return json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`), nil
	}))
	addMockConn(mgr, "macmcp", newMockConn("macmcp", simpleTools("capture_screenshot"),
		func(_ context.Context, _ string, _ interface{}) (json.RawMessage, error) {
			return json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`), nil
		}))

	store := storeWithCache(t.TempDir(), testSealer(), s)
	r := &appRouter{
		store:    store,
		tools:    mgr,
		services: service.NewRegistry(),
		onChange: func() {},
	}

	projTok, _ := proj.Token.Reveal()
	result, err := r.ListTools(context.Background(), projTok)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	tools := unmarshalTools(t, result)
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools (fs_read, fs_write — fs_bash disabled), got %d: %v", len(tools), toolNamesOf(tools))
	}
	for _, tool := range tools {
		if tool.Name == "fs_bash" {
			t.Error("fs_bash should be excluded (disabled)")
		}
		if tool.Name == "capture_screenshot" {
			t.Error("capture_screenshot should be excluded (macmcp not in project)")
		}
	}

	_, err = r.CallTool(context.Background(), "fs_read", json.RawMessage(`{"path":"/tmp"}`), projTok)
	if err != nil {
		t.Fatalf("expected fs_read to succeed, got: %v", err)
	}

	_, err = r.CallTool(context.Background(), "capture_screenshot", nil, projTok)
	if err == nil {
		t.Fatal("expected error calling tool from disallowed MCP")
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("expected 'access denied', got %q", err.Error())
	}

	_, err = r.CallTool(context.Background(), "fs_bash", json.RawMessage(`{"command":"ls"}`), projTok)
	if err == nil {
		t.Fatal("expected error calling disabled tool fs_bash")
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("expected 'access denied', got %q", err.Error())
	}
}

func toolNamesOf(tools []mcp.Tool) []string {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	return names
}
