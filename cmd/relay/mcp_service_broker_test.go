package main

// mcp register/unregister and service register/unregister/restart are
// brokered (ADR-017 decision 2). These tests prove the CLI dispatches into
// the real McpOps/ServiceOps cores over a real bridge connection — not
// merely that it can reach the transport — the same shape
// TestEnrolUpdate_CLIDispatchesTheParsedRequestThroughTheBroker proves for
// enrolment.update.

import "testing"

func TestMcpRegisterAndUnregister_CLIDispatchThroughTheBroker(t *testing.T) {
	store := newCLISandboxStore(t)
	serveBroker(t, newBrokerRouter(t, store, nil))

	mcpRegister([]string{
		"--name", "Probe MCP",
		"--command", buildTestMcpBinary(t),
	})

	mcps := store.Get().ExternalMcps
	if len(mcps) != 1 {
		t.Fatalf("want 1 registered mcp, got %d", len(mcps))
	}
	if mcps[0].DisplayName != "Probe MCP" {
		t.Fatalf("registered mcp = %+v, want display name %q", mcps[0], "Probe MCP")
	}
	id := mcps[0].ID

	mcpUnregister([]string{"--id", id})

	if len(store.Get().ExternalMcps) != 0 {
		t.Fatalf("mcp %q was not removed by unregister", id)
	}
}

func TestServiceRestart_CLIDispatchesThroughTheBroker(t *testing.T) {
	store := newCLISandboxStore(t)
	serveBroker(t, newBrokerRouter(t, store, nil))

	serviceRegister([]string{
		"--name", "restartable",
		"--command", "/bin/true",
	})

	// serviceRestart is not gated (§6.4) and touches no settings; the proof
	// here is that it reaches appRouter.ReloadService rather than refusing
	// as an unknown admin_op or an unwired core.
	serviceRestart([]string{"--id", "restartable"})
}
