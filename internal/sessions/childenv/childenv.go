// Package childenv holds what the session host's base-environment builders
// (terminal, provider, mcp) and relay's launch path share about a child's
// environment: what a spawned child must not inherit from relay-sessions' own
// environment beyond relay's credentials, and what a sandboxed Claude Code
// launch carries, including the folder that holds its login.
//
// It is a leaf on purpose: those packages keep their own copies of the relay
// credential list rather than depend on each other, and this rule needs the
// same property, so it imports only the standard library.
package childenv

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

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

// EnvClaudeTokenFD names the descriptor Claude Code reads its long-term OAuth
// token from.
//
// Deliberate: the name is CLAUDE_CODE_-prefixed, so IsParentClaudeSession drops
// a copy from relay's own environment. The host adds relay's value after that
// scrub, only to a launch that carries the token. The scrub itself does not
// change.
const EnvClaudeTokenFD = "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR"

// EnvDisableAutoUpdater is the Claude Code variable that turns its self-updater off.
const EnvDisableAutoUpdater = "DISABLE_AUTOUPDATER"

// EnvClaudeConfigDir is the Claude Code variable that names its config folder,
// and with it the place the login is kept.
const EnvClaudeConfigDir = "CLAUDE_CONFIG_DIR"

// SandboxClaudeDir is <os.UserConfigDir()>/relay/sandbox-claude: the same
// "relay" folder bridge.DefaultConfigDir names. Deliberately not bridge.ConfigDir:
// it does not move with --config-dir, so every relay instance under one HOME
// (Relay.app, each verify stack) shares one login. An os.UserConfigDir error is
// returned wrapped, never replaced by a fallback.
func SandboxClaudeDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve application support directory: %w", err)
	}
	return filepath.Join(base, "relay", "sandbox-claude"), nil
}

// EnvClaudeSandboxed is the Claude Code variable that says the process runs
// inside an external sandbox; Claude then skips its folder-trust check.
const EnvClaudeSandboxed = "CLAUDE_CODE_SANDBOXED"

// SandboxedClaude is the environment a sandboxed Claude Code launch carries.
//
// Deliberate: the sandbox grants the Claude install directory read-only, so
// the updater could only fail at every start. Writing it would let a sandboxed
// agent replace the binary the next unsandboxed launch runs. The person
// updates Claude from an unsandboxed terminal instead.
//
// Deliberate: the sandbox refuses the login keychain write that a token
// refresh needs, so Claude keeps its login in configDir, a folder the profile
// grants read-write, instead.
//
// Deliberate: EnvClaudeSandboxed is not here. The `claude` session kind runs
// --print, which shows no trust prompt, and the variable would change which
// project config a headless session loads. Only the terminal launch sets it.
func SandboxedClaude(configDir string) map[string]string {
	return map[string]string{EnvDisableAutoUpdater: "1", EnvClaudeConfigDir: configDir}
}
