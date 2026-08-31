//go:build !windows

package main

// Uses the real cmd/testmcp stdio peer so spawn, handshake, and connection
// bookkeeping run for real, not through mocks.

import (
	"context"
	"testing"
)

func stdioMcp(id, bin string) ExternalMcp {
	return ExternalMcp{ID: id, DisplayName: id, Transport: "stdio", Command: bin}
}

func TestExternalMcpManager_Reconcile_StopsRemovedAndStartsAdded(t *testing.T) {
	bin := buildTestMcpBinary(t)
	m := NewExternalMcpManager(nil)
	t.Cleanup(m.StopAll)
	ctx := context.Background()

	if err := m.startOne(ctx, ptr(stdioMcp("mcp-old", bin))); err != nil {
		t.Fatalf("startOne mcp-old: %v", err)
	}
	if !m.IsConnected("mcp-old") {
		t.Fatal("mcp-old should be connected after startOne")
	}

	m.Reconcile(ctx, []ExternalMcp{stdioMcp("mcp-new", bin)})

	if m.IsConnected("mcp-old") {
		t.Error("mcp-old should have been stopped by reconcile")
	}
	if !m.IsConnected("mcp-new") {
		t.Error("mcp-new should have been started by reconcile")
	}
}

func TestExternalMcpManager_Reconcile_RetainsUnchanged(t *testing.T) {
	bin := buildTestMcpBinary(t)
	m := NewExternalMcpManager(nil)
	t.Cleanup(m.StopAll)
	ctx := context.Background()

	if err := m.startOne(ctx, ptr(stdioMcp("mcp-keep", bin))); err != nil {
		t.Fatalf("startOne mcp-keep: %v", err)
	}

	m.Reconcile(ctx, []ExternalMcp{stdioMcp("mcp-keep", bin), stdioMcp("mcp-add", bin)})

	if !m.IsConnected("mcp-keep") {
		t.Error("retained MCP should still be connected after reconcile")
	}
	if !m.IsConnected("mcp-add") {
		t.Error("added MCP should be connected after reconcile")
	}
}

func TestExternalMcpManager_Reload_RestartsConnection(t *testing.T) {
	bin := buildTestMcpBinary(t)
	m := NewExternalMcpManager(nil)
	t.Cleanup(m.StopAll)
	ctx := context.Background()
	cfg := stdioMcp("mcp-x", bin)

	if err := m.startOne(ctx, &cfg); err != nil {
		t.Fatalf("startOne: %v", err)
	}
	if got := len(m.Tools("mcp-x")); got != 1 {
		t.Fatalf("want 1 tool before reload, got %d", got)
	}

	if err := m.Reload(ctx, "mcp-x", &cfg); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !m.IsConnected("mcp-x") {
		t.Error("MCP should be connected after Reload")
	}
	if got := len(m.Tools("mcp-x")); got != 1 {
		t.Errorf("want 1 tool after reload, got %d", got)
	}
}

func ptr[T any](v T) *T { return &v }
