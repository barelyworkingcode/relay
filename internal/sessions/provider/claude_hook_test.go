package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

func TestResolveHookCommand_IsExecutablePathPlusHookSubcommand(t *testing.T) {
	cmd, err := resolveHookCommand()
	if err != nil {
		t.Fatalf("resolveHookCommand: %v", err)
	}
	if !strings.HasSuffix(cmd, " hook") {
		t.Fatalf("hook command %q does not end in ' hook'", cmd)
	}
	binPath := strings.TrimSuffix(cmd, " hook")
	if !filepath.IsAbs(binPath) {
		t.Fatalf("hook command binary path %q is not absolute", binPath)
	}
}

func TestEnsureHookConfig_ConfiguredBinaryPathGetsHookSubcommand(t *testing.T) {
	dir := t.TempDir()
	p := newTestClaudeProvider(&sessionstypes.Session{ID: "s1", Model: "sonnet", Directory: dir}, ClaudeConfig{
		HookSocket:      "/tmp/hook.sock",
		HookCommandPath: "/abs/path/relay-sessions",
	})
	p.directory = dir

	if err := p.ensureHookConfig(); err != nil {
		t.Fatalf("ensureHookConfig: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.local.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}

	var settings struct {
		Hooks struct {
			PreToolUse []struct {
				Hooks []struct {
					Type    string `json:"type"`
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"PreToolUse"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	if len(settings.Hooks.PreToolUse) != 1 || len(settings.Hooks.PreToolUse[0].Hooks) != 1 {
		t.Fatalf("unexpected hooks shape: %s", data)
	}
	got := settings.Hooks.PreToolUse[0].Hooks[0].Command
	if got != "/abs/path/relay-sessions hook" {
		t.Errorf("hook command = %q, want %q", got, "/abs/path/relay-sessions hook")
	}
}

func TestEnsureHookConfig_NoopWithoutHookSocket(t *testing.T) {
	dir := t.TempDir()
	p := newTestClaudeProvider(&sessionstypes.Session{ID: "s1", Model: "sonnet", Directory: dir}, ClaudeConfig{})
	p.directory = dir

	if err := p.ensureHookConfig(); err != nil {
		t.Fatalf("ensureHookConfig: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude", "settings.local.json")); !os.IsNotExist(err) {
		t.Fatal("settings.local.json written despite no hook socket configured")
	}
}

func TestEnsureHookConfig_PreservesExistingSettings(t *testing.T) {
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := `{"otherSetting": "keep-me"}`
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.local.json"), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	p := newTestClaudeProvider(&sessionstypes.Session{ID: "s1", Model: "sonnet", Directory: dir}, ClaudeConfig{
		HookSocket:      "/tmp/hook.sock",
		HookCommandPath: "/abs/relay-sessions",
	})
	p.directory = dir

	if err := p.ensureHookConfig(); err != nil {
		t.Fatalf("ensureHookConfig: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(claudeDir, "settings.local.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "keep-me") {
		t.Errorf("existing settings not preserved: %s", data)
	}
	if !strings.Contains(string(data), "relay-sessions hook") {
		t.Errorf("hook not written: %s", data)
	}
}
