// Package migrate is a one-shot, idempotent copy of relayLLM's data
// directory into relay-sessions' own, run once at relay-sessions' first
// start (a later cmd/relaysessions wiring unit's job to call; this package
// only implements the copy itself, the same "define the operation, leave
// invoking it at startup to main wiring" split internal/sessions/session's
// sweep functions and internal/sessions/terminal's SweepTerminalLogs both
// follow).
//
// No file content is transformed: internal/sessions/provider's own read
// paths (claude_history.go reads Claude CLI's own
// ~/.claude/projects/<dir>/<sid>.jsonl, entirely outside either data
// directory; internal/sessions/session's Store reads relayLLM's
// sessions/*.json, which is byte-identical in shape to
// internal/sessions/types.Session — verified field-by-field, including
// JSON tags, against relayLLM/internal/types/session.go) all expect exactly
// what relayLLM already wrote.
package migrate

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// markerName is the file migrate writes once the copy has completed, and
// checks first on every call — the idempotency guard.
const markerName = ".migrated-from-relayllm"

// dirs is C5's named list: "copy relayLLM's sessions, pi-sessions and
// terminal_logs into it, then write the marker."
var dirs = []string{"sessions", "pi-sessions", "terminal_logs"}

// Run copies dirs from relayLLMDataDir into hostDataDir and writes the
// marker, unless the marker is already present (relayLLMDataDir need not
// exist in that case — Run returns immediately). A relayLLMDataDir that
// does not exist at all (relayLLM was never installed) is not an error:
// each named subdirectory simply has nothing to copy, and the marker is
// still written so a later start never re-checks.
func Run(relayLLMDataDir, hostDataDir string) error {
	markerPath := filepath.Join(hostDataDir, markerName)
	if _, err := os.Stat(markerPath); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("migrate: stat marker: %w", err)
	}

	if err := os.MkdirAll(hostDataDir, 0o700); err != nil {
		return fmt.Errorf("migrate: create host data dir: %w", err)
	}

	for _, name := range dirs {
		src := filepath.Join(relayLLMDataDir, name)
		dst := filepath.Join(hostDataDir, name)
		if err := copyTree(src, dst); err != nil {
			return fmt.Errorf("migrate: copy %s: %w", name, err)
		}
	}

	if err := os.WriteFile(markerPath, []byte{}, 0o600); err != nil {
		return fmt.Errorf("migrate: write marker: %w", err)
	}
	return nil
}

// Done reports whether Run has already completed for hostDataDir.
func Done(hostDataDir string) bool {
	_, err := os.Stat(filepath.Join(hostDataDir, markerName))
	return err == nil
}

// copyTree copies src into dst, preserving relative structure and file
// mode, skipping symlinks (neither relayLLM's session store nor pi's
// session dir nor terminal logs are ever expected to contain one; a real
// one here would be a caller error worth surfacing, not silently
// dereferencing). A missing src is not an error — nothing to copy.
func copyTree(src, dst string) error {
	info, err := os.Lstat(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s: not a directory", src)
	}

	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		switch {
		case d.Type()&fs.ModeSymlink != 0:
			return fmt.Errorf("%s: refusing to copy a symlink", path)
		case d.IsDir():
			fi, err := d.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(target, fi.Mode().Perm()|0o700)
		default:
			return copyFile(path, target, d)
		}
	})
}

func copyFile(src, dst string, d fs.DirEntry) error {
	fi, err := d.Info()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
