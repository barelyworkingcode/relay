// Package childenv holds the one rule the session host's three base-environment
// builders (terminal, provider, mcp) share about what a spawned child must not
// inherit from relay-sessions' own environment beyond relay's credentials.
//
// It is a leaf on purpose: those packages keep their own copies of the relay
// credential list rather than depend on each other, and this rule needs the
// same property, with no imports at all.
package childenv

import "strings"

// IsParentClaudeSession reports whether kv, a "NAME=value" entry from
// os.Environ(), belongs to the Claude Code session that happened to launch
// relay rather than to anything relay itself started.
//
// Relay is often launched from a terminal, and a build script run from inside a
// Claude Code session hands it that session's environment: its session id, its
// bridge and messaging socket and token, and the marker that says "I am a child
// session". A terminal relay spawns from that environment then believes it is a
// child of the launching session (Claude Code turns transcript saving off and
// may attach to the parent's bridge), and every session relay hosts can read
// the parent's messaging token.
//
// The names dropped are CLAUDECODE, CLAUDE_PID, CLAUDE_EFFORT and everything
// prefixed CLAUDE_CODE_. Not CLAUDE_CONFIG_DIR or any ANTHROPIC_* name: those
// configure a Claude Code the operator means to run, not a session that is
// already running. A template that wants one of the dropped names says so in
// env or env_passthrough, and it is applied after this base, so it survives.
func IsParentClaudeSession(kv string) bool {
	name, _, _ := strings.Cut(kv, "=")
	switch name {
	case "CLAUDECODE", "CLAUDE_PID", "CLAUDE_EFFORT":
		return true
	}
	return strings.HasPrefix(name, "CLAUDE_CODE_")
}
