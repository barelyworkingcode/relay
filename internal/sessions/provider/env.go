// Package provider hosts the Claude Code and pi CLI session providers:
// argv/env construction, process lifecycle, and translation of each CLI's
// wire format into the canonical events in internal/sessions/events.
//
// Every credential a spawned child needs arrives typed, through this
// package's own Config structs (a model key, a socket path) — never read
// back out of this process's ambient environment and never written into a
// child's argv. internal/sessions/terminal/session.go's buildShimEnv/
// childBaseEnv pair is the precedent this package follows.
package provider

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/barelyworkingcode/relay/internal/service"
	"github.com/barelyworkingcode/relay/internal/sessions/childenv"
)

// relaySecretEnvKeys is the canonical list every env builder under
// internal/sessions/ strips before adding its own values back. Byte-identical
// to internal/sessions/terminal/session.go's and internal/sessions/mcp/mcp.go's
// own copies — package boundaries under internal/sessions/ don't share an
// env-building dependency, so this is a deliberate triplication, not drift.
var relaySecretEnvKeys = []string{
	"RELAY_SERVICE_TOKEN",
	"RELAY_MCP_TOKEN",
	"RELAY_FRONTEND_TOKEN",
	"RELAY_LAUNCH_FD",
	"RELAY_PROJECT_TOKEN",
	"RELAY_TOKEN",
	"RELAY_LLM_TOKEN",
	"RELAY_LLM_HOOK_TOKEN",
}

// childBaseEnv returns os.Environ() with every relaySecretEnvKeys entry
// stripped, so no stale relay credential reaches a claude/pi child, and with
// the launching Claude Code session's own variables stripped too
// (childenv.IsParentClaudeSession).
func childBaseEnv() []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		if childenv.IsParentClaudeSession(kv) {
			continue
		}
		drop := false
		for _, k := range relaySecretEnvKeys {
			if strings.HasPrefix(kv, k+"=") {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// mergeEnv layers add onto base, add winning on key collision. Thin wrapper
// over internal/service.MergeEnv (already the shared helper for this exact
// shape elsewhere in the sessions tree) so callers here don't hand-roll the
// override-by-key walk themselves.
func mergeEnv(base []string, add map[string]string) []string {
	cmd := &exec.Cmd{Env: base}
	service.MergeEnv(cmd, add)
	return cmd.Env
}

// ensurePath adds ~/.local/bin to PATH if not already present, or sets a
// minimal PATH if none is inherited at all. Needed when relay-sessions itself
// runs from a launchd-style minimal environment that never sourced a shell
// profile.
func ensurePath(env []string) []string {
	home, _ := os.UserHomeDir()
	localBin := filepath.Join(home, ".local", "bin")

	for i, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			if !strings.Contains(e, localBin) {
				env[i] = e + ":" + localBin
			}
			return env
		}
	}
	return append(env, "PATH=/usr/local/bin:/usr/bin:/bin:"+localBin)
}

// resolveClaudePath finds the claude binary: configured path first, then
// well-known install locations, then PATH lookup, then the literal "claude"
// (exec.LookPath defers the error to spawn time).
func resolveClaudePath(configured string) string {
	if configured != "" {
		return configured
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".local", "bin", "claude"),
		filepath.Join(home, ".claude", "local", "claude"),
		"/usr/local/bin/claude",
		"/opt/homebrew/bin/claude",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	return "claude"
}

// resolvePiPath finds the pi binary, same priority as resolveClaudePath.
func resolvePiPath(configured string) string {
	if configured != "" {
		if strings.HasPrefix(configured, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				configured = filepath.Join(home, configured[2:])
			}
		}
		return configured
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".bun", "bin", "pi"),
		filepath.Join(home, ".local", "bin", "pi"),
		filepath.Join(home, ".npm-global", "bin", "pi"),
		"/opt/homebrew/bin/pi",
		"/usr/local/bin/pi",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	if p, err := exec.LookPath("pi"); err == nil {
		return p
	}
	return "pi"
}

// ResolveClaudePath is the claude binary relay-sessions launches when no path
// is configured, which is how relay-sessions always runs. Relay calls it to
// derive a sandbox grant for the same binary.
func ResolveClaudePath() string { return resolveClaudePath("") }

// ResolvePiPath is ResolveClaudePath for pi.
func ResolvePiPath() string { return resolvePiPath("") }

// hasArg reports whether args contains a flag matching name (case-insensitive).
func hasArg(args []string, name string) bool {
	for _, a := range args {
		if strings.EqualFold(a, name) {
			return true
		}
	}
	return false
}

// writeSpawnFile writes data to a fresh file under dir at mode 0600 and
// returns its path. Used for everything a CLI child reads by path instead of
// by argv (mcp-config, system prompt) so neither shows up in argv, which
// KERN_PROCARGS2 (and any co-resident same-uid process) can read regardless
// of this process's own file permissions.
func writeSpawnFile(dir, pattern string, data []byte) (string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	path := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}
