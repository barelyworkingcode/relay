package provider

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/sessions/hook"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

func newTestClaudeProvider(session *sessionstypes.Session, cfg ClaudeConfig) *ClaudeProvider {
	return NewClaudeProvider(session, func(string, json.RawMessage) {}, cfg, nil)
}

func TestBuildClaudeArgs_MCPConfigAndSystemPromptAreFilePaths(t *testing.T) {
	p := newTestClaudeProvider(&sessionstypes.Session{Model: "sonnet"}, ClaudeConfig{})
	args := p.buildClaudeArgs("/tmp/mcp-config.json", "/tmp/sysprompt.txt")

	assertNoForbiddenSecrets(t, "claude argv", args)

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--mcp-config /tmp/mcp-config.json") {
		t.Errorf("argv missing --mcp-config file path: %v", args)
	}
	if !strings.Contains(joined, "--append-system-prompt-file /tmp/sysprompt.txt") {
		t.Errorf("argv missing --append-system-prompt-file: %v", args)
	}
	// The prompt/config content itself must never appear inline in argv.
	for _, a := range args {
		if a == "secret system prompt text" {
			t.Fatal("system prompt leaked into argv instead of a file path")
		}
	}
}

func TestBuildClaudeArgs_HostSessionUsesStdioPermissionPromptNotMCPConfig(t *testing.T) {
	p := newTestClaudeProvider(&sessionstypes.Session{
		Model: "sonnet",
		Host:  &sessionstypes.HostSpec{ID: "h1", Name: "host1", SSHArgv: []string{"ssh", "host1"}},
	}, ClaudeConfig{})
	args := p.buildClaudeArgs("/tmp/mcp-config.json", "")

	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--mcp-config") {
		t.Errorf("host session must never receive --mcp-config: %v", args)
	}
	if !strings.Contains(joined, "--permission-prompt-tool stdio") {
		t.Errorf("host session missing --permission-prompt-tool stdio: %v", args)
	}
}

func TestBuildClaudeArgs_BypassPermissionsAddsSkipFlag(t *testing.T) {
	p := newTestClaudeProvider(&sessionstypes.Session{Model: "sonnet", Headless: true}, ClaudeConfig{})
	args := p.buildClaudeArgs("", "")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--permission-mode bypassPermissions") {
		t.Errorf("missing --permission-mode bypassPermissions: %v", args)
	}
	if !strings.Contains(joined, "--dangerously-skip-permissions") {
		t.Errorf("missing --dangerously-skip-permissions: %v", args)
	}
}

func TestBuildClaudeEnv_CarriesHookSocketAndSessionID_NeverAToken(t *testing.T) {
	p := newTestClaudeProvider(&sessionstypes.Session{ID: "sess-1", Model: "sonnet"}, ClaudeConfig{
		HookSocket:   "/tmp/hook.sock",
		BridgeSocket: "/tmp/bridge.sock",
		ModelSocket:  "/tmp/model.sock",
	})
	env := p.buildClaudeEnv([]string{"PATH=/usr/bin"})

	assertNoForbiddenSecrets(t, "claude env", env)

	want := map[string]string{
		hook.EnvHookSocket:    "/tmp/hook.sock",
		"RELAY_SESSION_ID":    "sess-1",
		"RELAY_BRIDGE_SOCKET": "/tmp/bridge.sock",
		"RELAY_MODEL_SOCKET":  "/tmp/model.sock",
	}
	for k, v := range want {
		found := false
		for _, e := range env {
			if e == k+"="+v {
				found = true
			}
		}
		if !found {
			t.Errorf("env missing %s=%s; got %v", k, v, env)
		}
	}
}

func TestBuildClaudeEnv_OmitsHookSocketWhenUnset(t *testing.T) {
	p := newTestClaudeProvider(&sessionstypes.Session{ID: "sess-1", Model: "sonnet"}, ClaudeConfig{})
	env := p.buildClaudeEnv([]string{"PATH=/usr/bin"})
	for _, e := range env {
		if strings.HasPrefix(e, hook.EnvHookSocket+"=") {
			t.Fatalf("hook socket env set despite empty config: %v", env)
		}
	}
}

func TestRelayMCPConfig_DisabledWithoutSettingOrCommand(t *testing.T) {
	p := newTestClaudeProvider(&sessionstypes.Session{}, ClaudeConfig{})
	if cfg := p.relayMCPConfig(); cfg != nil {
		t.Fatalf("expected nil config, got %v", cfg)
	}

	settings, _ := json.Marshal(map[string]any{"useRelayTools": true})
	p2 := newTestClaudeProvider(&sessionstypes.Session{Settings: settings}, ClaudeConfig{})
	if cfg := p2.relayMCPConfig(); cfg != nil {
		t.Fatalf("expected nil config without RelayMCPCommand, got %v", cfg)
	}
}

func TestRelayMCPConfig_EnabledCarriesNoBearer(t *testing.T) {
	settings, _ := json.Marshal(map[string]any{"useRelayTools": true})
	p := newTestClaudeProvider(&sessionstypes.Session{Settings: settings}, ClaudeConfig{
		RelayMCPCommand: "/usr/local/bin/relay",
	})
	cfg := p.relayMCPConfig()
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	assertNoForbiddenSecrets(t, "relay mcp config", []string{string(data)})

	var parsed struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	server, ok := parsed.MCPServers["relay"]
	if !ok {
		t.Fatalf("missing relay entry: %s", data)
	}
	if server.Command != "/usr/local/bin/relay" {
		t.Errorf("command = %q", server.Command)
	}
	if len(server.Env) != 0 {
		t.Errorf("relay mcp config must carry no env at all (ancestry auth, not a token): %v", server.Env)
	}
}

func TestClaudeProvider_KillRemovesSpawnFiles(t *testing.T) {
	dir := t.TempDir()
	p := newTestClaudeProvider(&sessionstypes.Session{ID: "s1", Model: "sonnet"}, ClaudeConfig{})
	path, err := writeSpawnFile(dir, "mcp-*.json", []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	p.spawnFiles = append(p.spawnFiles, path)

	p.Kill()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("spawn file %s still present after Kill", path)
	}
}
