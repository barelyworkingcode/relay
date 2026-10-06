package main

import (
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

// suiteHome is the isolated HOME TestMain sets before m.Run; "" until then.
var suiteHome string

const suiteHomeEnv = "RELAY_TEST_SUITE_HOME"

const sandboxFixHint = "fix: ensure every test calls mkSandboxRelayHome(t) before touching settings/pidfiles/logs/sockets"

const maxTripwireEntries = 20

// TestMain enforces the headline rule from ADR-001: no test may read or
// mutate the real user config directory (~/Library/Application Support/relay/).
//
// The suite runs under an isolated HOME, so a test that skips
// mkSandboxRelayHome(t) resolves the config dir to a tripwire under that HOME.
// The tripwire must not exist after the run. The real config dir is also
// compared before and after, unless a relay answered on its socket at start.
func TestMain(m *testing.M) {
	// This is deliberate: tests swap HOME, and Go derives these caches from
	// HOME, so an unpinned child `go build` would start from an empty module
	// cache and hit the network.
	if err := pinGoToolchainCaches(); err != nil {
		fmt.Fprintf(os.Stderr, "pin Go toolchain caches: %v\n", err)
		os.Exit(1)
	}

	bridge.SetConfigDirForTest("")
	realDir := bridge.DefaultConfigDir()
	real := watchRealDir(realDir)

	// This is subtle: tests re-exec this binary as a helper child, which runs
	// TestMain again. The child adopts the parent's root so it shares the
	// tripwire the parent checks, and leaves removal to the parent, since the
	// child often exits inside main() before reaching cleanup.
	root := os.Getenv(suiteHomeEnv)
	ownsRoot := !isDir(root)
	if ownsRoot {
		// This is deliberate: /tmp, not $TMPDIR, so a test that binds relay.sock
		// under the isolated HOME stays inside the 104-byte socket path limit.
		var err error
		root, err = os.MkdirTemp("/tmp", fmt.Sprintf("relay-suite-home-%d-", os.Getpid()))
		if err != nil {
			fmt.Fprintf(os.Stderr, "create isolated suite HOME under /tmp: %v\n", err)
			os.Exit(1)
		}
	}
	removeOwnedRoot := func() error {
		if !ownsRoot {
			return nil
		}
		return os.RemoveAll(root)
	}
	if err := os.Setenv("HOME", root); err != nil {
		fmt.Fprintf(os.Stderr, "set HOME to isolated suite root %s: %v\n", root, err)
		_ = removeOwnedRoot()
		os.Exit(1)
	}
	if err := os.Setenv("XDG_CONFIG_HOME", filepath.Join(root, ".config")); err != nil {
		fmt.Fprintf(os.Stderr, "set XDG_CONFIG_HOME under isolated suite root %s: %v\n", root, err)
		_ = removeOwnedRoot()
		os.Exit(1)
	}
	if err := os.Setenv(suiteHomeEnv, root); err != nil {
		fmt.Fprintf(os.Stderr, "set %s to isolated suite root %s: %v\n", suiteHomeEnv, root, err)
		_ = removeOwnedRoot()
		os.Exit(1)
	}
	suiteHome = root
	tripwire := bridge.DefaultConfigDir()

	if v := isolationViolations(root, tripwire, bridge.ConfigDir()); len(v) > 0 {
		fmt.Fprintf(os.Stderr, "sandbox guard failed before the run:\n")
		for _, msg := range v {
			fmt.Fprintf(os.Stderr, "  %s\n", msg)
		}
		_ = removeOwnedRoot()
		os.Exit(1)
	}

	// This is deliberate: only the top-level process builds. Helper children
	// inherit the suite root and must not run `go build`, often inside their
	// parent's deadline.
	var binDir string
	if ownsRoot {
		var err error
		binDir, err = os.MkdirTemp("/tmp", "relay-bin-")
		if err == nil {
			err = buildTestBinaries(binDir)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "build test binaries: %v\n", err)
			if binDir != "" {
				_ = os.RemoveAll(binDir)
			}
			_ = removeOwnedRoot()
			os.Exit(1)
		}
		relayBinPath = filepath.Join(binDir, "relay")
	}

	code := m.Run()
	if binDir != "" {
		_ = os.RemoveAll(binDir)
	}

	bridge.SetConfigDirForTest("")
	v := isolationViolations(root, tripwire, bridge.ConfigDir())
	realViolations := real.violations()
	v = append(v, realViolations...)

	if real.liveOwner {
		fmt.Fprintf(os.Stderr, "sandbox guard: a relay answered on %s at start; real config dir not compared this run (isolated-HOME tripwire still checked)\n", filepath.Join(realDir, "relay.sock"))
	}

	if err := removeOwnedRoot(); err != nil {
		fmt.Fprintf(os.Stderr, "remove isolated suite HOME %s: %v\n", root, err)
		code = 1
	}

	if len(v) > 0 {
		var b strings.Builder
		b.WriteString("\n\nSANDBOX VIOLATION\n")
		for _, msg := range v {
			for _, line := range strings.Split(strings.TrimRight(msg, "\n"), "\n") {
				b.WriteString("  " + line + "\n")
			}
		}
		if len(realViolations) > 0 {
			b.WriteString("  " + runningRelayNote(realDir) + "\n")
		}
		b.WriteString("  " + sandboxFixHint + "\n\n")
		fmt.Fprint(os.Stderr, b.String())
		os.Exit(1)
	}

	os.Exit(code)
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// isolationViolations reports how the isolated HOME failed to contain the
// suite: the tripwire outside root, the resolved config dir moved off the
// tripwire, or anything created at the tripwire. Entries elsewhere under root
// (Go telemetry from child `go` commands, for one) are not violations.
func isolationViolations(root, tripwire, resolved string) []string {
	var v []string
	// This is subtle: plain string prefixes, never EvalSymlinks. /tmp is a
	// symlink on macOS and DefaultConfigDir returns $HOME literally, so root,
	// tripwire and resolved are only comparable in their unresolved form.
	if !strings.HasPrefix(tripwire, root+string(filepath.Separator)) {
		v = append(v, fmt.Sprintf("tripwire %s is not under the isolated suite HOME %s", tripwire, root))
	}
	if resolved != tripwire {
		v = append(v, fmt.Sprintf("config dir resolves to %s, not the tripwire %s: a test left HOME, XDG_CONFIG_HOME or the config dir override changed", resolved, tripwire))
	}
	if _, err := os.Lstat(tripwire); err == nil {
		v = append(v, tripwireLeakMessage(tripwire))
	}
	return v
}

func tripwireLeakMessage(tripwire string) string {
	var entries []string
	_ = filepath.WalkDir(tripwire, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // deliberate: skip the unreadable entry, keep walking
		}
		if path == tripwire {
			return nil
		}
		rel, relErr := filepath.Rel(tripwire, path)
		if relErr != nil {
			rel = path
		}
		entries = append(entries, rel)
		return nil
	})
	sort.Strings(entries)
	var b strings.Builder
	fmt.Fprintf(&b, "tripwire %s exists: a test wrote the default config dir without mkSandboxRelayHome", tripwire)
	for i, e := range entries {
		if i == maxTripwireEntries {
			fmt.Fprintf(&b, "\n  ... and %d more", len(entries)-maxTripwireEntries)
			break
		}
		fmt.Fprintf(&b, "\n  - %s", e)
	}
	return b.String()
}

func liveRelayAnswers(configDir string) bool {
	conn, err := net.DialTimeout("unix", filepath.Join(configDir, "relay.sock"), 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

type realDirWatch struct {
	dir       string
	liveOwner bool
	before    dirSnapshot
	beforeOK  bool
}

// watchRealDir decides once, before the run, whether a live relay owns dir.
//
// This is deliberate: liveness is never re-checked after the run, so a test
// that binds the real socket cannot switch off the comparison for its own leak.
func watchRealDir(dir string) realDirWatch {
	w := realDirWatch{dir: dir, liveOwner: liveRelayAnswers(dir)}
	if !w.liveOwner {
		w.before, w.beforeOK = snapshotDir(dir)
	}
	return w
}

// violations compares dir against the snapshot taken at watch time. It
// returns nil when a live relay owned dir at watch time: that relay rewrites
// settings.json on its own schedule, so a comparison would say nothing.
func (w realDirWatch) violations() []string {
	if w.liveOwner {
		return nil
	}
	after, afterOK := snapshotDir(w.dir)
	if w.beforeOK != afterOK {
		return []string{fmt.Sprintf("real ConfigDir existence changed during test run\n  path: %s\n  before: existed=%v  after: existed=%v", w.dir, w.beforeOK, afterOK)}
	}
	if w.beforeOK && !w.before.equal(after) {
		return []string{fmt.Sprintf("real ConfigDir was modified during test run\n  path: %s\n  before: %s\n  after:  %s\n%s", w.dir, w.before, after, strings.TrimRight(w.before.diff(after), "\n"))}
	}
	return nil
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
	if !liveRelayAnswers(configDir) {
		return generic
	}
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
