package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sessionsmcp "github.com/barelyworkingcode/relay/internal/sessions/mcp"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// TestBuildChatMCPManager_ToolChildSpawnedThroughShimWithIdentity spawns a
// real tool child through the MCP manager buildChatMCPManager returns,
// against a real relay-sessions binary and a fake bridge standing in for
// relay's own bridge socket, and inspects the *actual* spawned process's
// environment and argv, written to disk by the process itself.
//
// Unlike a plain child of this test process, this tool child has no other
// process to serve as its session's project_session root (a chat session
// runs no external CLI at all) -- it must become that root itself, which
// only happens if it is genuinely spawned through the shim: the fake
// bridge receiving a matching Hello is proof the shim ran and proved this
// launch's real identity, not just proof some process eventually execed
// the dump script.
func TestBuildChatMCPManager_ToolChildSpawnedThroughShimWithIdentity(t *testing.T) {
	relaySessionsBin := buildRelaySessionsBinary(t)
	bridgeSock, received := startFakeBridge(t)

	dir := shortTempDir(t)
	script := writeEnvArgvDumpScript(t, dir)

	envOut := filepath.Join(dir, "env.out")
	argvOut := filepath.Join(dir, "argv.out")
	t.Setenv("RH_TEST_OUT_ENV", envOut)
	t.Setenv("RH_TEST_OUT_ARGV", argvOut)

	// A leaked ambient credential in this test process's own env must never
	// reach the tool child, even though the shim's own env is built from
	// childBaseEnv, not this process's raw os.Environ().
	t.Setenv("RELAY_PROJECT_TOKEN", "leaked-project-token")

	secret := strings.Repeat("c", 64)
	settings, _ := json.Marshal(map[string]any{"useRelayTools": true})
	session := &sessionstypes.Session{ID: "chat-sess-shim-1", Settings: settings}

	cfg := ChatConfig{
		RelayMCPCommand: script,
		ShimBinary:      relaySessionsBin,
		BridgeSocket:    bridgeSock,
		Identity:        &sessionsmcp.IdentitySpec{Secret: secret},
	}

	mgr := buildChatMCPManager(cfg, session)
	if mgr == nil {
		t.Fatal("buildChatMCPManager: expected a manager (useRelayTools is set)")
	}
	defer mgr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The dump script never speaks MCP's JSON-RPC handshake, so Start
	// returning an error here is expected -- only the spawn itself, what it
	// was spawned with, and the identity it presented, are under test.
	_ = mgr.Start(ctx)

	select {
	case hello := <-received:
		if hello.Type != "Hello" || hello.Kind != "project_session" || hello.Name != session.ID || hello.Token != secret {
			t.Fatalf("hello = %+v, want {Hello project_session %s %s}", hello, session.ID, secret)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fake bridge never received a Hello -- tool child was not spawned through the shim")
	}

	waitForFile(t, envOut)
	waitForFile(t, argvOut)

	argv := readLines(t, argvOut)
	if len(argv) != 1 || argv[0] != "mcp" {
		t.Fatalf("argv = %v, want [mcp]", argv)
	}

	env := readLines(t, envOut)
	assertNoForbiddenSecrets(t, "chat MCP tool child env", env)
	for _, e := range env {
		if e == "RELAY_PROJECT_TOKEN=leaked-project-token" {
			t.Fatalf("ambient RELAY_PROJECT_TOKEN reached the tool child: %s", e)
		}
	}

	var sawSessionID bool
	for _, e := range env {
		if e == "RELAY_SESSION_ID="+session.ID {
			sawSessionID = true
		}
	}
	if !sawSessionID {
		t.Fatalf("env = %v, want RELAY_SESSION_ID=%s (the shim's own env, inherited unchanged by its target)", env, session.ID)
	}
}

// TestBuildChatMCPManager_SessionSettingsCannotNameACommand is the required
// F1 regression test: a settings payload shaped exactly like the old,
// removed mcpServers mechanism -- naming an arbitrary command, args and env
// -- must have no effect at all. The only command the tool child ever
// runs is cfg.RelayMCPCommand, relay's own fixed entry.
func TestBuildChatMCPManager_SessionSettingsCannotNameACommand(t *testing.T) {
	dir := shortTempDir(t)
	marker := filepath.Join(dir, "pwned")

	exploit, _ := json.Marshal(map[string]any{
		"useRelayTools": true,
		"mcpServers": map[string]any{
			"evil": map[string]any{
				"command": "/bin/sh",
				"args":    []string{"-c", "touch " + marker},
				"env":     map[string]string{"X": "1"},
			},
		},
	})
	session := &sessionstypes.Session{ID: "chat-sess-exploit", Settings: exploit}

	mgr := buildChatMCPManager(ChatConfig{RelayMCPCommand: "/usr/local/bin/relay"}, session)
	if mgr == nil {
		t.Fatal("buildChatMCPManager: expected a manager (useRelayTools is set)")
	}
	defer mgr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// /usr/local/bin/relay is very unlikely to exist as the relay MCP
	// command in a test environment; Start erroring is fine -- what matters
	// is that "evil" was never among the servers it tried to connect to.
	_ = mgr.Start(ctx)

	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("session settings' mcpServers entry executed a caller-named command")
	}
}

// TestBuildChatMCPManager_NoOptInIsNil confirms a session that never set
// useRelayTools gets no MCP manager at all -- no child is ever spawned
// speculatively.
func TestBuildChatMCPManager_NoOptInIsNil(t *testing.T) {
	session := &sessionstypes.Session{ID: "chat-sess-2"}
	if mgr := buildChatMCPManager(ChatConfig{RelayMCPCommand: "/usr/local/bin/relay"}, session); mgr != nil {
		t.Fatal("expected nil manager when the session never opted into relay tools")
	}
}
