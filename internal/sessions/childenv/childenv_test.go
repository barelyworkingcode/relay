package childenv

import "testing"

func TestIsParentClaudeSession(t *testing.T) {
	for kv, want := range map[string]bool{
		// the launching session's identity, transport and markers
		"CLAUDECODE=1":                       true,
		"CLAUDE_CODE_CHILD_SESSION=1":        true,
		"CLAUDE_CODE_SESSION_ID=abc":         true,
		"CLAUDE_CODE_BRIDGE_SESSION_ID=abc":  true,
		"CLAUDE_CODE_MESSAGING_SOCKET=/x/y":  true,
		"CLAUDE_CODE_MESSAGING_TOKEN=secret": true,
		"CLAUDE_CODE_ENTRYPOINT=cli":         true,
		"CLAUDE_CODE_EXECPATH=/bin/claude":   true,
		"CLAUDE_CODE_SESSION_ATTENDED=1":     true,
		"CLAUDE_CODE_SOMETHING_NEW_LATER=1":  true,
		"CLAUDE_PID=123":                     true,
		"CLAUDE_EFFORT=high":                 true,
		"CLAUDE_CODE_CHILD_SESSION=":         true,
		// configuration for a Claude Code the operator means to run
		"CLAUDE_CONFIG_DIR=/Users/me/.claude": false,
		"ANTHROPIC_API_KEY=sk-x":              false,
		"ANTHROPIC_BASE_URL=http://x":         false,
		// names that only look similar
		"CLAUDECODE_EXTRA=1":    false,
		"MY_CLAUDE_CODE_FLAG=1": false,
		"CLAUDE=1":              false,
		"PATH=/usr/bin":         false,
		"HOME=/Users/me":        false,
		// a value that contains a scrubbed name does not make the entry one
		"NOTE=CLAUDECODE=1": false,
	} {
		if got := IsParentClaudeSession(kv); got != want {
			t.Errorf("IsParentClaudeSession(%q) = %v, want %v", kv, got, want)
		}
	}
}
