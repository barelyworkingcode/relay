//go:build !windows

package mcpbroker

import (
	"context"
	"github.com/barelyworkingcode/relay/internal/config"
	"os"
	"sync"
	"testing"
	"time"
)

func connOf(m *Manager, id string) Connection {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.conns[id]
}

func waitForNewConn(t *testing.T, m *Manager, id string, prev Connection) *externalMcpConn {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if c := connOf(m, id); c != nil && c != prev {
			conn, ok := c.(*externalMcpConn)
			if !ok {
				t.Fatalf("connection for %s is %T, want *externalMcpConn", id, c)
			}
			return conn
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no respawned connection for %q within the deadline: a dead external MCP stays dead", id)
	return nil
}

// A respawn must come back fully — handshake re-run, tools rediscovered,
// context schema re-read — because serving calls before the schema is known
// would be a worse bug than the outage it recovers from.
func TestSupervisor_RespawnsAChildThatDies(t *testing.T) {
	bin := buildTestMcpBinary(t)
	m := NewManager(nil)
	t.Cleanup(m.StopAll)
	ctx := context.Background()

	cfg := stdioMcp("mcp-dies", bin)
	// v2 context schema, so the assertion below is about the schema being
	// rediscovered, not about it never having existed.
	cfg.Env = config.SecretMapFromPlain(map[string]string{"RELAY_TESTMCP_CONTEXT": "v2"})
	if err := m.startOne(ctx, &cfg); err != nil {
		t.Fatalf("startOne: %v", err)
	}

	first := connOf(m, "mcp-dies")
	if first == nil {
		t.Fatal("no connection after startOne")
	}
	if s := m.McpSurfaceFor("mcp-dies"); len(s.Schema) == 0 || s.SchemaVersion != 2 {
		t.Fatalf("before the crash the surface should carry the declared schema, got %+v", s)
	}

	// "exit" is testmcp's own RPC for crashing itself mid-request.
	if _, err := first.SendRequest(ctx, "exit", nil); err == nil {
		t.Fatal("expected the in-flight call to fail when the child exits")
	}

	second := waitForNewConn(t, m, "mcp-dies", first)

	if got := len(m.Tools("mcp-dies")); got != 1 {
		t.Errorf("respawned MCP exposes %d tools, want 1: the handshake was not re-run", got)
	}
	if s := m.McpSurfaceFor("mcp-dies"); len(s.Schema) == 0 || s.SchemaVersion != 2 {
		t.Errorf("respawned MCP surface = %+v, want the rediscovered v2 schema: "+
			"a connection relay will dispatch to with no schema passes every scope check "+
			"and strips every context key", s)
	}

	res, err := second.SendRequest(ctx, "echo", map[string]any{"marker": "alive"})
	if err != nil {
		t.Fatalf("call on the respawned child: %v", err)
	}
	if got := markerOf(t, res); got != "alive" {
		t.Errorf("marker = %q, want alive", got)
	}
}

func TestSupervisor_CallToolWorksAgainAfterACrash(t *testing.T) {
	bin := buildTestMcpBinary(t)
	m := NewManager(nil)
	t.Cleanup(m.StopAll)
	ctx := context.Background()

	cfg := stdioMcp("mcp-crash", bin)
	if err := m.startOne(ctx, &cfg); err != nil {
		t.Fatalf("startOne: %v", err)
	}
	first := connOf(m, "mcp-crash")

	if _, err := first.SendRequest(ctx, "exit", nil); err == nil {
		t.Fatal("expected the in-flight call to fail when the child exits")
	}
	waitForNewConn(t, m, "mcp-crash", first)

	if _, err := m.CallTool(ctx, "mcp-crash", "echo", nil, nil); err != nil {
		t.Fatalf("CallTool after respawn: %v", err)
	}
}

func TestSupervisor_StopDoesNotRespawn(t *testing.T) {
	bin := buildTestMcpBinary(t)
	shortenRestartPolicy(t, 8)
	m := NewManager(nil)
	t.Cleanup(m.StopAll)
	ctx := context.Background()

	cfg := stdioMcp("mcp-stopped", bin)
	if err := m.startOne(ctx, &cfg); err != nil {
		t.Fatalf("startOne: %v", err)
	}
	m.Stop("mcp-stopped")

	// Long enough for the whole restart budget to have run, several times over.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if connOf(m, "mcp-stopped") != nil {
			t.Fatal("a stopped MCP was respawned by its supervisor")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if m.IsConnected("mcp-stopped") {
		t.Error("stopped MCP is connected again")
	}
}

func TestSupervisor_AbandonsACrashLoopAndSaysSo(t *testing.T) {
	bin := buildTestMcpBinary(t)
	shortenRestartPolicy(t, 3)

	m := NewManager(nil)
	t.Cleanup(m.StopAll)

	var (
		mu     sync.Mutex
		states []string
	)
	done := make(chan struct{})
	var once sync.Once
	m.SetHealthObserver(func(ev HealthEvent) {
		mu.Lock()
		states = append(states, ev.State)
		mu.Unlock()
		if ev.State == HealthAbandoned {
			once.Do(func() { close(done) })
		}
	})

	// The config's binary path is captured at install time, so copying it and
	// then deleting the copy produces a crash loop without a binary that
	// actually crashes: a first successful handshake, then every respawn
	// attempt fails because the path is gone.
	dir := t.TempDir()
	doomed := dir + "/testmcp-copy"
	copyFile(t, bin, doomed)

	cfg := stdioMcp("mcp-loop", doomed)
	if err := m.startOne(context.Background(), &cfg); err != nil {
		t.Fatalf("startOne: %v", err)
	}
	first := connOf(m, "mcp-loop")
	removeFile(t, doomed)

	if _, err := first.SendRequest(context.Background(), "exit", nil); err == nil {
		t.Fatal("expected the in-flight call to fail when the child exits")
	}

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		mu.Lock()
		got := append([]string(nil), states...)
		mu.Unlock()
		t.Fatalf("a child that cannot be respawned was never abandoned; states seen: %v", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(states) == 0 || states[0] != HealthDown {
		t.Errorf("first reported state = %v, want %q first", states, HealthDown)
	}
	failed := 0
	for _, st := range states {
		if st == HealthRestartFailed {
			failed++
		}
	}
	if failed != MCPRestartMaxAttempts {
		t.Errorf("%d failed attempts reported, want %d (the budget)", failed, MCPRestartMaxAttempts)
	}
}

// shortenRestartPolicy collapses the backoff so a crash loop can be driven to
// its cap inside a test. Vars, not consts, exactly so this is possible —
// the same seam MCPRequestTimeout already provides (ADR-002).
func shortenRestartPolicy(t *testing.T, attempts int) {
	t.Helper()
	base, max, cap_, window := MCPRestartBaseDelay, MCPRestartMaxDelay, MCPRestartMaxAttempts, MCPRestartStableWindow
	MCPRestartBaseDelay = 5 * time.Millisecond
	MCPRestartMaxDelay = 20 * time.Millisecond
	MCPRestartMaxAttempts = attempts
	MCPRestartStableWindow = time.Hour
	t.Cleanup(func() {
		MCPRestartBaseDelay, MCPRestartMaxDelay = base, max
		MCPRestartMaxAttempts, MCPRestartStableWindow = cap_, window
	})
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

func removeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove %s: %v", path, err)
	}
}

func TestSupervisor_ReconcileRecoversAnAbandonedMcp(t *testing.T) {
	bin := buildTestMcpBinary(t)
	shortenRestartPolicy(t, 2)

	m := NewManager(nil)
	t.Cleanup(m.StopAll)
	ctx := context.Background()

	abandoned := make(chan struct{})
	var once sync.Once
	m.SetHealthObserver(func(ev HealthEvent) {
		if ev.State == HealthAbandoned {
			once.Do(func() { close(abandoned) })
		}
	})

	dir := t.TempDir()
	doomed := dir + "/testmcp-copy"
	copyFile(t, bin, doomed)

	cfg := stdioMcp("mcp-abandoned", doomed)
	if err := m.startOne(ctx, &cfg); err != nil {
		t.Fatalf("startOne: %v", err)
	}
	first := connOf(m, "mcp-abandoned")
	removeFile(t, doomed)
	if _, err := first.SendRequest(ctx, "exit", nil); err == nil {
		t.Fatal("expected the in-flight call to fail when the child exits")
	}

	select {
	case <-abandoned:
	case <-time.After(20 * time.Second):
		t.Fatal("the child was never abandoned")
	}

	copyFile(t, bin, doomed)
	m.Reconcile(ctx, []config.ExternalMcp{stdioMcp("mcp-abandoned", doomed)})

	if !m.IsConnected("mcp-abandoned") {
		t.Fatal("reconcile did not restart an abandoned MCP")
	}
	if c := connOf(m, "mcp-abandoned"); c == first {
		t.Fatal("reconcile left the dead connection in place")
	}
	if _, err := m.CallTool(ctx, "mcp-abandoned", "echo", nil, nil); err != nil {
		t.Fatalf("CallTool after recovery: %v", err)
	}
}

// bridge.BridgeServer hands each handler a PER-CONNECTION context that dies
// the moment the client disconnects — `relay mcp register` is one such client
// and it exits immediately. Supervision tied to that context would apply to
// some MCPs and not others depending on which command last touched them.
func TestSupervisor_OutlivesTheReloadCallersContext(t *testing.T) {
	bin := buildTestMcpBinary(t)
	m := NewManager(nil)
	t.Cleanup(m.StopAll)

	cfg := stdioMcp("mcp-reloaded", bin)
	if err := m.startOne(context.Background(), &cfg); err != nil {
		t.Fatalf("startOne: %v", err)
	}

	reqCtx, cancel := context.WithCancel(context.Background())
	if err := m.Reload(reqCtx, "mcp-reloaded", &cfg); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	cancel()

	first := connOf(m, "mcp-reloaded")
	if first == nil {
		t.Fatal("no connection after Reload")
	}
	if _, err := first.SendRequest(context.Background(), "exit", nil); err == nil {
		t.Fatal("expected the in-flight call to fail when the child exits")
	}
	waitForNewConn(t, m, "mcp-reloaded", first)
}
