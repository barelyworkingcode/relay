package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/sessions/childenv"
)

// sandboxClaudeLinks are the ~/.claude entries a sandboxed Claude sees through
// its relay-owned config folder. Never .credentials.json (the login itself) or
// .claude.json (account data, possibly an API key): the folder holds its own,
// and relay seeds a fixed .claude.json instead.
// projects and sessions are directories relay and the outside-session probe
// read under ~/.claude, so they are created in the target when absent.
var sandboxClaudeLinks = []string{
	"settings.json", "CLAUDE.md", "skills", "agents", "commands", "projects", "sessions",
}

// sandboxClaudeLinkDirs are the entries whose target is made when missing.
var sandboxClaudeLinkDirs = map[string]bool{"projects": true, "sessions": true}

// sandboxClaudeSeed is the whole .claude.json relay writes into a folder that
// has none. 31 bytes, no trailing newline.
var sandboxClaudeSeed = []byte(`{"hasCompletedOnboarding":true}`)

// isClaudeLaunch reports whether the launch runs Claude Code: the claude
// session kind, or a terminal started from the claude-code template.
func isClaudeLaunch(kind string, tmpl *config.TerminalTemplate) bool {
	if kind == KindClaude {
		return true
	}
	return kind == KindPTY && tmpl != nil && tmpl.ID == kindTemplateIDs[KindClaude]
}

// ensureSandboxClaudeDir creates the sandbox Claude folder and seeds it with
// links into ~/.claude and the onboarding .claude.json, then returns its path.
// It never reads, copies or opens a file in ~/.claude, and never overwrites an
// entry already in the folder.
func ensureSandboxClaudeDir() (string, error) {
	dir, err := childenv.SandboxClaudeDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", fmt.Errorf("sandbox claude dir %s: %w", dir, err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("sandbox claude dir %s: %w", dir, err)
		}
		// Lstat, not Stat: a link here could point the grant anywhere.
		fi, lerr := os.Lstat(dir)
		if lerr != nil || !fi.IsDir() {
			return "", fmt.Errorf("sandbox claude dir %s: not a directory", dir)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("sandbox claude dir %s: resolve home directory: %w", dir, err)
	}
	for _, name := range sandboxClaudeLinks {
		link := filepath.Join(dir, name)
		if _, err := os.Lstat(link); err == nil {
			continue
		}
		target := filepath.Join(home, ".claude", name)
		if sandboxClaudeLinkDirs[name] {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return "", fmt.Errorf("sandbox claude dir %s: link %s: %w", dir, name, err)
			}
		} else if _, err := os.Lstat(target); err != nil {
			continue
		}
		if err := os.Symlink(target, link); err != nil && !errors.Is(err, syscall.EEXIST) {
			return "", fmt.Errorf("sandbox claude dir %s: link %s: %w", dir, name, err)
		}
	}
	if err := seedSandboxClaudeState(dir); err != nil {
		return "", fmt.Errorf("sandbox claude dir %s: seed .claude.json: %w", dir, err)
	}
	return dir, nil
}

// seedSandboxClaudeState writes sandboxClaudeSeed to <dir>/.claude.json when
// that name has no entry. It returns nil when an entry exists or another
// launch published one first. The publish is a hard link, deliberately not a
// rename or a create on the final name: link(2) fails with EEXIST and never
// follows an existing entry, while rename would replace a file a concurrent
// launch or a person put there, and the folder is writable by sandboxed agents.
func seedSandboxClaudeState(dir string) error {
	final := filepath.Join(dir, ".claude.json")
	if _, err := os.Lstat(final); !errors.Is(err, os.ErrNotExist) {
		return nil
	}
	tmp, err := os.CreateTemp(dir, ".claude.json.seed-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(sandboxClaudeSeed); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(name, final); err != nil && !errors.Is(err, syscall.EEXIST) {
		return err
	}
	return nil
}
