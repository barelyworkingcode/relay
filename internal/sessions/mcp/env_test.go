package mcp

import (
	"strings"
	"testing"
)

// An MCP server child must not inherit the Claude Code session that launched
// relay: its session id, bridge, and messaging token are that session's.
func TestChildBaseEnv_DropsTheLaunchingClaudeSession(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "parent")
	t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "parent-secret")
	t.Setenv("HARMLESS_VAR", "kept")

	have := map[string]bool{}
	for _, kv := range childBaseEnv() {
		name, _, _ := strings.Cut(kv, "=")
		have[name] = true
	}
	for _, gone := range []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_MESSAGING_TOKEN"} {
		if have[gone] {
			t.Errorf("childBaseEnv carried the launching session's %s", gone)
		}
	}
	if !have["HARMLESS_VAR"] {
		t.Error("childBaseEnv dropped a non-session variable")
	}
}
