package provider

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// TestBuildChatMCPManager_ToolChildSpawnedWithAncestryOnlyIdentity spawns a
// real child through the MCP manager buildChatMCPManager returns and
// inspects that child's actual environment and argv, written to disk by the
// child itself — not the config-builder function in isolation. Chat's own
// tool child follows the exact pattern ClaudeProvider.relayMCPConfig
// documents: run as this process's own child (a C3 member of the session's
// root), authenticated by process ancestry alone. No shim, no launch
// secret, no bearer of any kind travels in its config or env — see
// chat_base.go's buildChatMCPManager doc comment.
func TestBuildChatMCPManager_ToolChildSpawnedWithAncestryOnlyIdentity(t *testing.T) {
	dir := shortTempDir(t)
	script := writeEnvArgvDumpScript(t, dir)

	envOut := filepath.Join(dir, "env.out")
	argvOut := filepath.Join(dir, "argv.out")
	t.Setenv("RH_TEST_OUT_ENV", envOut)
	t.Setenv("RH_TEST_OUT_ARGV", argvOut)

	// A leaked ambient credential in this test process's own env must never
	// reach the child, even though the MCP package's own childBaseEnv only
	// strips by name.
	t.Setenv("RELAY_PROJECT_TOKEN", "leaked-project-token")

	settings, _ := json.Marshal(map[string]any{"useRelayTools": true})
	session := &sessionstypes.Session{ID: "chat-sess-1", Settings: settings}

	mgr := buildChatMCPManager(ChatConfig{RelayMCPCommand: script}, session)
	if mgr == nil {
		t.Fatal("buildChatMCPManager: expected a manager (useRelayTools is set)")
	}
	defer mgr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// The dump script never speaks MCP's JSON-RPC handshake, so Start
	// returning an error here is expected — only the spawn itself, and what
	// it was spawned with, is under test.
	_ = mgr.Start(ctx)

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
