package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"relaygo/bridge"
)

// TestMain enforces the headline rule from ADR-001: no test may mutate the
// real user config directory (~/Library/Application Support/relay/). We
// snapshot the real ConfigDir's mtime before the suite runs and verify it
// hasn't changed after. If the directory doesn't exist, we record absence
// and require it to still not exist after.
//
// This catches three classes of bug at the suite level:
//   - A new test forgot to call mkSandboxRelayHome(t).
//   - A helper bypassed bridge.ConfigDir() and called os.UserConfigDir directly.
//   - SetConfigDirForTest("") was called too early (clearing the override
//     mid-test) so a later write landed in the real dir.
func TestMain(m *testing.M) {
	// Capture the REAL ConfigDir (override is empty at this point) so the
	// snapshot is meaningful even if a stray Set call below leaks.
	bridge.SetConfigDirForTest("")
	realDir := bridge.ConfigDir()

	before, beforeOK := snapshotDir(realDir)

	code := m.Run()

	// Reset any override the suite may have left behind before re-reading.
	bridge.SetConfigDirForTest("")
	after, afterOK := snapshotDir(realDir)

	if beforeOK != afterOK {
		fmt.Fprintf(os.Stderr, "\n\nSANDBOX VIOLATION: real ConfigDir existence changed during test run\n  path: %s\n  before: existed=%v  after: existed=%v\n  fix: ensure every test calls mkSandboxRelayHome(t) before touching settings/pidfiles/logs/sockets\n\n", realDir, beforeOK, afterOK)
		os.Exit(1)
	}
	if beforeOK && !before.equal(after) {
		diff := before.diff(after)
		fmt.Fprintf(os.Stderr, "\n\nSANDBOX VIOLATION: real ConfigDir was modified during test run\n  path: %s\n  before: %s\n  after:  %s\n%s  fix: ensure every test calls mkSandboxRelayHome(t) before touching settings/pidfiles/logs/sockets\n\n", realDir, before, after, diff)
		os.Exit(1)
	}

	os.Exit(code)
}

// dirSnapshot fingerprints a directory tree's mtime + entry set. Cheap
// enough to take twice per test run; precise enough to catch any write.
type dirSnapshot struct {
	rootMtime time.Time
	entries   map[string]time.Time
}

func (s dirSnapshot) String() string {
	return fmt.Sprintf("mtime=%s entries=%d", s.rootMtime.Format(time.RFC3339Nano), len(s.entries))
}

func (s dirSnapshot) equal(other dirSnapshot) bool {
	if !s.rootMtime.Equal(other.rootMtime) {
		return false
	}
	if len(s.entries) != len(other.entries) {
		return false
	}
	for path, mt := range s.entries {
		if otherMt, ok := other.entries[path]; !ok || !mt.Equal(otherMt) {
			return false
		}
	}
	return true
}

// diff lists the per-entry differences between two snapshots in a
// human-readable form. Empty string means equal.
func (s dirSnapshot) diff(other dirSnapshot) string {
	var b []byte
	for path, mt := range s.entries {
		omt, ok := other.entries[path]
		if !ok {
			b = append(b, fmt.Sprintf("  - REMOVED: %s\n", path)...)
			continue
		}
		if !mt.Equal(omt) {
			b = append(b, fmt.Sprintf("  - MUTATED: %s (%s → %s)\n", path,
				mt.Format(time.RFC3339Nano), omt.Format(time.RFC3339Nano))...)
		}
	}
	for path := range other.entries {
		if _, ok := s.entries[path]; !ok {
			b = append(b, fmt.Sprintf("  - ADDED:   %s\n", path)...)
		}
	}
	return string(b)
}

// snapshotDir returns (snapshot, true) if dir exists, (zero, false) otherwise.
// Recursive but bounded — the real relay ConfigDir on a dev machine has
// O(10s) of files; on CI it doesn't exist, in which case beforeOK=false.
//
// IGNORED paths: anything inside logs/ or run/ subdirs (a running tray app
// continuously appends to log files and rotates pidfiles — these mutate
// during the test run even without test contamination). What matters for
// safety is settings.json and the top-level entry set.
func snapshotDir(dir string) (dirSnapshot, bool) {
	info, err := os.Stat(dir)
	if err != nil {
		return dirSnapshot{}, false
	}
	snap := dirSnapshot{
		rootMtime: info.ModTime(),
		entries:   make(map[string]time.Time),
	}
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if shouldIgnoreForSafetySnapshot(dir, path) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		snap.entries[path] = fi.ModTime()
		return nil
	})
	return snap, true
}

// shouldIgnoreForSafetySnapshot returns true for paths that a live
// externally-running relay (the user's tray app) routinely mutates —
// log files, pidfiles, sockets — but tests have no business touching.
// Filtering these out makes the safety guard catch what matters
// (settings.json corruption, new project dirs, new MCP entries) without
// flagging benign churn from the user's running relay.
func shouldIgnoreForSafetySnapshot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) == 0 {
		return false
	}
	switch parts[0] {
	case "logs", "run":
		return true
	}
	// Unix sockets are filesystem entries but their mtime is meaningless.
	if strings.HasSuffix(path, ".sock") {
		return true
	}
	return false
}
