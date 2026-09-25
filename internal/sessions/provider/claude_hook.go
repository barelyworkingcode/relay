package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// hookCommand is C6's hook command for a relay-sessions binary: "<binary> hook".
func hookCommand(binary string) string {
	return binary + " hook"
}

// resolveHookCommand returns C6's exact hook command string: the absolute
// path to the currently-running relay-sessions binary, followed by its
// "hook" subcommand. relayLLM resolved a separate hook binary
// (cmd/hook/hook next to its own executable); relay-sessions is a single
// binary with a hook mode instead (cmd/relaysessions/main.go, R-S5), so the
// resolved path is this process's own os.Executable(), not a sibling file.
func resolveHookCommand() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	return hookCommand(exe), nil
}

// ensureHookConfig writes <directory>/.claude/settings.local.json to
// register relay-sessions' hook mode as Claude Code's PreToolUse hook.
// Without this, Claude has no idea the hook exists and never invokes it.
// Ported from relayLLM's internal/session/session.go ensureHookConfig/
// resolveHookPath; ClaudeProvider.Start is the closest equivalent this
// package has to relayLLM's SessionManager.Create, since no session-manager
// layer exists here yet (R-S7c).
func (p *ClaudeProvider) ensureHookConfig() error {
	if p.cfg.HookSocket == "" {
		return nil
	}
	command := p.cfg.HookCommandPath
	if command != "" {
		command = hookCommand(command)
	} else {
		cmd, err := resolveHookCommand()
		if err != nil {
			return fmt.Errorf("resolve hook command: %w", err)
		}
		command = cmd
	}

	claudeDir := filepath.Join(p.directory, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		return fmt.Errorf("create .claude dir: %w", err)
	}

	settingsPath := filepath.Join(claudeDir, "settings.local.json")

	var settings map[string]interface{}
	if data, err := os.ReadFile(settingsPath); err == nil {
		_ = json.Unmarshal(data, &settings)
	}
	if settings == nil {
		settings = make(map[string]interface{})
	}

	hooks, _ := settings["hooks"].(map[string]interface{})
	if hooks == nil {
		hooks = make(map[string]interface{})
	}
	hooks["PreToolUse"] = []interface{}{
		map[string]interface{}{
			"matcher": "",
			"hooks": []interface{}{
				map[string]interface{}{
					"type":    "command",
					"command": command,
					"timeout": 120,
				},
			},
		},
	}
	settings["hooks"] = hooks

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}

	return os.WriteFile(settingsPath, data, 0o644)
}
