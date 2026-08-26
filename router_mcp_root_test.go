package main

// fsMCP v3 integration, R2: relay knows the directory it spawned a stdio MCP
// with even when the MCP itself publishes no contextSchema at all, and the
// audit record must carry that fact separately from Scope so
// "scope=(none declared)" stays true and meaningful.

import (
	"context"
	"encoding/json"
	"testing"

	"relaygo/mcp"
)

// fsmcpV3ToolSurface is a scale model of fsMCP v3's own annotations: every
// tool is honestly read-only or mutating, and closed-world -- these tools
// never leave the host -- which is what a real fs_list/fs_read grant looks
// like and is needed here so the outbound-grant check (ADR-011 decision 2c)
// does not itself refuse the call before McpRoot ever has a chance to be
// recorded.
func fsmcpV3ToolSurface() []mcp.Tool {
	return []mcp.Tool{
		{Name: "fs_list", Description: "List a directory.",
			Annotations: json.RawMessage(`{"readOnlyHint":true,"openWorldHint":false}`)},
		{Name: "fs_read", Description: "Read a file.",
			Annotations: json.RawMessage(`{"readOnlyHint":true,"openWorldHint":false}`)},
	}
}

// rootedProfile builds a router granting one MCP that declares NO context
// schema — matching fsMCP v3, which publishes none — whose live connection
// carries a ResolvedRoot the way connectStdio populates it after a real
// --root spawn.
func rootedProfile(t *testing.T, root string) *appRouter {
	t.Helper()
	proj := Project{
		ID: "test-project", Name: "test", Kind: ProjectKindRemote,
		AllowedMcpIDs: []string{"fsmcp3"}, Token: testToken, TokenHash: hashToken(testToken),
		AllowedTools: map[string][]string{"fsmcp3": {"fs_*"}},
		Access:       map[string]string{"fsmcp3": AccessWrite},
	}
	s := &Settings{
		Version:      1,
		ExternalMcps: []ExternalMcp{{ID: "fsmcp3", DisplayName: "fsMCP v3"}},
		Projects:     []Project{proj},
		AdminSecret:  "supersecretadmin",
	}
	mgr := NewExternalMcpManager(nil)
	conn := newMockConn("fsmcp3", fsmcpV3ToolSurface(),
		okHandler(`{"content":[{"type":"text","text":"ok"}]}`))
	conn.config.ResolvedRoot = root
	addMockConn(mgr, "fsmcp3", conn)
	// Deliberately no addMockSchema call: fsMCP v3 declares no contextSchema,
	// so McpSurfaceFor(...).Schema stays nil and Scope must stay nil too.
	return newTestRouter(t, s, mgr)
}

func TestAudit_RecordsMcpRootAsSeparateFactFromScope(t *testing.T) {
	const root = "/Users/admin/source/barelyworkingcode/testfolder"
	r := rootedProfile(t, root)
	rec := newTestAudit(t, nil)
	r.audit = rec

	if _, err := r.CallTool(context.Background(), "fs_list", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	events := readLoggedEvents(t, rec)
	if len(events) != 1 {
		t.Fatalf("expected 1 record, got %d", len(events))
	}
	ev := events[0]
	if ev.Scope != nil {
		t.Errorf("a schema-less MCP recorded a non-nil scope: %v", ev.Scope)
	}
	if ev.McpRoot != root {
		t.Errorf("mcp_root = %q, want %q", ev.McpRoot, root)
	}
}

// TestAudit_NoRootWhenRelayDidNotSpawnOne pins the negative: an MCP relay
// connected to without a --root leaves McpRoot empty, never a placeholder
// that would read as a real directory.
func TestAudit_NoRootWhenRelayDidNotSpawnOne(t *testing.T) {
	r := rootedProfile(t, "")
	rec := newTestAudit(t, nil)
	r.audit = rec

	if _, err := r.CallTool(context.Background(), "fs_read", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	events := readLoggedEvents(t, rec)
	if len(events) != 1 {
		t.Fatalf("expected 1 record, got %d", len(events))
	}
	if events[0].McpRoot != "" {
		t.Errorf("mcp_root = %q, want empty when relay spawned no --root", events[0].McpRoot)
	}
}
