package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

// TestMain enforces the headline rule from ADR-001: no test may mutate the
// real user config directory (~/Library/Application Support/relay/).
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

	providerBinary = func(string) string { return "" }
	claudeTmp, err := os.MkdirTemp("", "relay-claude-tmp-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create claude temp root: %v\n", err)
		os.Exit(1)
	}
	claudeTempRoot = claudeTmp

	code := m.Run()
	os.RemoveAll(claudeTmp)

	// Reset any override the suite may have left behind before re-reading.
	bridge.SetConfigDirForTest("")
	after, afterOK := snapshotDir(realDir)

	if beforeOK != afterOK {
		fmt.Fprintf(os.Stderr, "\n\nSANDBOX VIOLATION: real ConfigDir existence changed during test run\n  path: %s\n  before: existed=%v  after: existed=%v\n  %s\n  fix: ensure every test calls mkSandboxRelayHome(t) before touching settings/pidfiles/logs/sockets\n\n", realDir, beforeOK, afterOK, runningRelayNote(realDir))
		os.Exit(1)
	}
	if beforeOK && !before.equal(after) {
		diff := before.diff(after)
		fmt.Fprintf(os.Stderr, "\n\nSANDBOX VIOLATION: real ConfigDir was modified during test run\n  path: %s\n  before: %s\n  after:  %s\n%s  %s\n  fix: ensure every test calls mkSandboxRelayHome(t) before touching settings/pidfiles/logs/sockets\n\n", realDir, before, after, diff, runningRelayNote(realDir))
		os.Exit(1)
	}

	os.Exit(code)
}

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
			// A walk error means the entry is unreadable, not that the walk
			// should abort; returning err here would do the latter.
			return nil //nolint:nilerr // deliberate: skip the entry, keep walking
		}
		if shouldIgnoreForSafetySnapshot(dir, path) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil //nolint:nilerr // deliberate: skip the entry, keep walking
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

// runningRelayNote names a live tray app as a possible cause of a sandbox
// violation.
//
// This is deliberate: a running relay legitimately rewrites settings.json on
// its own schedule, which otherwise reads identically to a real sandbox
// escape and sends the reader hunting for a test bug that does not exist.
// The comparison above stays exactly as strict either way — this only makes
// its failure explain itself.
//
// This is subtle: the dial can only narrow the message from "possible" to
// "confirmed", never suppress it — a socket that fails to answer (relay not
// running, or any other reason) still leaves the generic line in place.
func runningRelayNote(configDir string) string {
	generic := "possible cause: a running relay writes to this directory on its own schedule (e.g. settings.json), unrelated to the code under test — stop relay and rerun"
	conn, err := net.DialTimeout("unix", filepath.Join(configDir, "relay.sock"), 200*time.Millisecond)
	if err != nil {
		return generic
	}
	conn.Close()
	return "likely cause: a relay instance is running right now (its bridge socket answered) and writes to this directory on its own schedule — stop it and rerun"
}

// TestRunningRelayNote_NoSocketNamesGenericPossibleCause covers the message
// alone, not TestMain: no relay.sock in dir means nothing answered the dial,
// so the guard's failure output must still name a running relay as a
// possible cause rather than saying nothing about it.
func TestRunningRelayNote_NoSocketNamesGenericPossibleCause(t *testing.T) {
	got := runningRelayNote(t.TempDir())
	if !strings.Contains(got, "possible cause") || !strings.Contains(got, "running relay") {
		t.Fatalf("note = %q, want it to name a running relay as a possible cause", got)
	}
}

// TestRunningRelayNote_LiveSocketNamesRelayAsLikelyCause is the bonus half:
// a real listener on relay.sock narrows the message from "possible" to
// "confirmed", which is the only direction the dial is allowed to move it.
func TestRunningRelayNote_LiveSocketNamesRelayAsLikelyCause(t *testing.T) {
	dir := mkShortTempDir(t, "running-relay-note") // net.Listen needs sun_path under 104 chars
	ln, err := net.Listen("unix", filepath.Join(dir, "relay.sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	got := runningRelayNote(dir)
	if !strings.Contains(got, "likely cause") || !strings.Contains(got, "running right now") {
		t.Fatalf("note = %q, want it to name a running relay as the likely, confirmed cause", got)
	}
}
