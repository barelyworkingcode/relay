package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// TestClaudeProvider_RealSpawn_NoSecretsInRealEnvOrArgv spawns a real child
// process through ClaudeProvider.Start and inspects that process's actual
// environment and argv, written to disk by the child itself — not the
// env/argv-builder functions in isolation. This is the standard
// internal/sessions/terminal's own review held env stripping to; this
// package's required hermetic test list names the same property.
func TestClaudeProvider_RealSpawn_NoSecretsInRealEnvOrArgv(t *testing.T) {
	scratch := t.TempDir()
	projectDir := t.TempDir()
	binDir := t.TempDir()

	script := writeEnvArgvDumpScript(t, binDir)
	envOut := filepath.Join(scratch, "env.out")
	argvOut := filepath.Join(scratch, "argv.out")
	t.Setenv("RH_TEST_OUT_ENV", envOut)
	t.Setenv("RH_TEST_OUT_ARGV", argvOut)

	// A leaked ambient credential in this test process's own env must never
	// reach the child, even though childBaseEnv only strips by name.
	t.Setenv("RELAY_PROJECT_TOKEN", "leaked-project-token")

	settings, _ := json.Marshal(map[string]any{"useRelayTools": true})
	session := &sessionstypes.Session{
		ID:        "claude-sess-1",
		Model:     "sonnet",
		Directory: projectDir,
		Settings:  settings,
	}
	p := NewClaudeProvider(session, func(string, json.RawMessage) {}, ClaudeConfig{
		Binary:          script,
		HookSocket:      "/tmp/relay-sessions-hook.sock",
		HookCommandPath: "/abs/relay-sessions",
		BridgeSocket:    "/tmp/relay-bridge.sock",
		ModelSocket:     "/tmp/relay-model.sock",
		RelayMCPCommand: "/usr/local/bin/relay",
	}, nil)
	defer p.Kill()

	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitForFile(t, envOut)
	waitForFile(t, argvOut)

	env := readLines(t, envOut)
	argv := readLines(t, argvOut)

	assertNoForbiddenSecrets(t, "real claude child env", env)
	assertNoForbiddenSecrets(t, "real claude child argv", argv)

	for _, e := range env {
		if strings.HasPrefix(e, "RELAY_PROJECT_TOKEN=") {
			t.Fatalf("ambient RELAY_PROJECT_TOKEN reached the real child: %s", e)
		}
	}

	wantEnvPrefixes := []string{
		"RELAY_SESSIONS_HOOK_SOCKET=/tmp/relay-sessions-hook.sock",
		"RELAY_SESSION_ID=claude-sess-1",
		"RELAY_BRIDGE_SOCKET=/tmp/relay-bridge.sock",
		"RELAY_MODEL_SOCKET=/tmp/relay-model.sock",
	}
	for _, want := range wantEnvPrefixes {
		found := false
		for _, e := range env {
			if e == want {
				found = true
			}
		}
		if !found {
			t.Errorf("real child env missing %q; got %v", want, env)
		}
	}

	// --mcp-config must be a 0600 file path, never inline JSON, in the real argv.
	mcpPath := argAfter(argv, "--mcp-config")
	if mcpPath == "" {
		t.Fatal("--mcp-config missing from real argv")
	}
	assertFileMode0600(t, mcpPath)
	mcpData, err := os.ReadFile(mcpPath)
	if err != nil {
		t.Fatalf("read mcp config: %v", err)
	}
	if !strings.Contains(string(mcpData), `"relay"`) {
		t.Errorf("mcp config missing relay server entry: %s", mcpData)
	}
	assertNoForbiddenSecrets(t, "mcp config file", []string{string(mcpData)})

	// System prompt must come from a 0600 file, never inline.
	session.SystemPrompt = "this is the system prompt"
	p2 := NewClaudeProvider(session, func(string, json.RawMessage) {}, ClaudeConfig{Binary: script}, nil)
	defer p2.Kill()
	envOut2 := filepath.Join(scratch, "env2.out")
	argvOut2 := filepath.Join(scratch, "argv2.out")
	t.Setenv("RH_TEST_OUT_ENV", envOut2)
	t.Setenv("RH_TEST_OUT_ARGV", argvOut2)
	if err := p2.Start(); err != nil {
		t.Fatalf("Start (system prompt case): %v", err)
	}
	waitForFile(t, argvOut2)
	argv2 := readLines(t, argvOut2)
	sysPromptPath := argAfter(argv2, "--append-system-prompt-file")
	if sysPromptPath == "" {
		t.Fatal("--append-system-prompt-file missing from real argv")
	}
	assertFileMode0600(t, sysPromptPath)
	data, err := os.ReadFile(sysPromptPath)
	if err != nil {
		t.Fatalf("read system prompt file: %v", err)
	}
	if string(data) != "this is the system prompt" {
		t.Errorf("system prompt file content = %q", data)
	}
	for _, a := range argv2 {
		if a == "this is the system prompt" {
			t.Fatal("system prompt leaked inline into real argv")
		}
	}

	// The hook command written to .claude/settings.local.json must be
	// exactly "<abs> hook", per C6.
	settingsData, err := os.ReadFile(filepath.Join(projectDir, ".claude", "settings.local.json"))
	if err != nil {
		t.Fatalf("read hook settings: %v", err)
	}
	if !strings.Contains(string(settingsData), `"/abs/relay-sessions hook"`) {
		t.Errorf("hook settings missing expected command: %s", settingsData)
	}
}

// argAfter returns the value following flag in argv, or "".
func argAfter(argv []string, flag string) string {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

func assertFileMode0600(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("%s mode = %o, want 0600", path, info.Mode().Perm())
	}
}
