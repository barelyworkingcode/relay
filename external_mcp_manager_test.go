//go:build !windows

package main

// Coverage for ExternalMcpManager.Reconcile and Reload — the start/stop diffing
// on the MCP-config-change hot path, previously untested. Uses the real
// cmd/testmcp stdio peer (which answers initialize + tools/list) so the spawn,
// handshake, and connection bookkeeping all run for real, not through mocks.

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

	// Seed a connection that the next reconcile must remove.
	if err := m.startOne(ctx, ptr(stdioMcp("mcp-old", bin))); err != nil {
		t.Fatalf("startOne mcp-old: %v", err)
	}
	if !m.IsConnected("mcp-old") {
		t.Fatal("mcp-old should be connected after startOne")
	}

	// Desired set drops mcp-old and introduces mcp-new.
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

	// Superset reconcile: mcp-keep stays connected, mcp-add starts.
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

// ptr returns the address of a copy of v.
func ptr[T any](v T) *T { return &v }
