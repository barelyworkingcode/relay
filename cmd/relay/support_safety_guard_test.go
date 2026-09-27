package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

func TestSuiteHome_IsolatesHomeAndDefaultConfigDir(t *testing.T) {
	if suiteHome == "" || !strings.HasPrefix(suiteHome, "/tmp/") {
		t.Fatalf("suiteHome = %q, want a root under /tmp/", suiteHome)
	}
	if got := os.Getenv("HOME"); got != suiteHome {
		t.Fatalf("HOME = %q, want suiteHome %q", got, suiteHome)
	}
	if got := bridge.DefaultConfigDir(); !strings.HasPrefix(got, suiteHome+"/") {
		t.Fatalf("DefaultConfigDir() = %q, want it under %q", got, suiteHome)
	}
}

func writeTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestIsolationViolations(t *testing.T) {
	cases := []struct {
		name string
		// setup returns the tripwire and resolved dir; "" keeps the defaults.
		setup func(t *testing.T, root, tripwire string) (string, string)
		// want is a path the single message must name; "" means clean.
		want func(root, tripwire, resolved string) string
	}{
		{"clean", nil, nil},
		{"tripwire holds settings.json", func(t *testing.T, _, tw string) (string, string) {
			writeTestFile(t, filepath.Join(tw, "settings.json"))
			return "", ""
		}, func(_, _, _ string) string { return "settings.json" }},
		{"tripwire holds only logs", func(t *testing.T, _, tw string) (string, string) {
			writeTestFile(t, filepath.Join(tw, "logs", "x.log"))
			return "", ""
		}, func(_, _, _ string) string { return filepath.Join("logs", "x.log") }},
		{"tripwire holds only run", func(t *testing.T, _, tw string) (string, string) {
			writeTestFile(t, filepath.Join(tw, "run", "x"))
			return "", ""
		}, func(_, _, _ string) string { return filepath.Join("run", "x") }},
		{"tripwire holds only relay.sock", func(t *testing.T, _, tw string) (string, string) {
			writeTestFile(t, filepath.Join(tw, "relay.sock"))
			return "", ""
		}, func(_, _, _ string) string { return "relay.sock" }},
		{"resolved differs from tripwire", func(t *testing.T, _, _ string) (string, string) {
			return "", filepath.Join(t.TempDir(), "relay")
		}, func(_, _, resolved string) string { return resolved }},
		{"tripwire outside root", func(t *testing.T, _, _ string) (string, string) {
			tw := filepath.Join(t.TempDir(), "relay")
			return tw, tw
		}, func(_, tw, _ string) string { return tw }},
		{"tripwire under a lookalike prefix of root", func(t *testing.T, root, _ string) (string, string) {
			tw := filepath.Join(root+"x", "relay")
			return tw, tw
		}, func(_, tw, _ string) string { return tw }},
		{"entries elsewhere under root", func(t *testing.T, root, _ string) (string, string) {
			writeTestFile(t, filepath.Join(root, "Library", "Application Support", "go", "telemetry", "x"))
			return "", ""
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tripwire := filepath.Join(root, "Library", "Application Support", "relay")
			resolved := tripwire
			if tc.setup != nil {
				tw, res := tc.setup(t, root, tripwire)
				if tw != "" {
					tripwire = tw
				}
				if res != "" {
					resolved = res
				}
			}
			v := isolationViolations(root, tripwire, resolved)
			if tc.want == nil {
				if len(v) != 0 {
					t.Fatalf("violations = %q, want none", v)
				}
				return
			}
			want := tc.want(root, tripwire, resolved)
			if len(v) != 1 || !strings.Contains(v[0], want) {
				t.Fatalf("violations = %q, want exactly one naming %q", v, want)
			}
		})
	}
}

func listenRelaySock(t *testing.T, dir string) {
	t.Helper()
	ln, err := net.Listen("unix", filepath.Join(dir, "relay.sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
}

func TestRealDirWatch(t *testing.T) {
	past := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		subdir bool // watch dir/relay, absent at watch time
		before func(t *testing.T, dir string)
		after  func(t *testing.T, dir string)
		// want is a path the violation must name; "" means clean.
		want     func(dir string) string
		wantLive bool
	}{
		{name: "file added, no listener",
			after: func(t *testing.T, dir string) { writeTestFile(t, filepath.Join(dir, "added.json")) },
			want:  func(dir string) string { return filepath.Join(dir, "added.json") }},
		{name: "mtime changed",
			before: func(t *testing.T, dir string) { writeTestFile(t, filepath.Join(dir, "settings.json")) },
			after: func(t *testing.T, dir string) {
				if err := os.Chtimes(filepath.Join(dir, "settings.json"), past, past); err != nil {
					t.Fatal(err)
				}
			},
			want: func(dir string) string { return filepath.Join(dir, "settings.json") }},
		{name: "dir absent then created", subdir: true,
			after: func(t *testing.T, dir string) {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			},
			want: func(dir string) string { return dir }},
		{name: "listener up at watch", wantLive: true,
			before: func(t *testing.T, dir string) { listenRelaySock(t, dir) },
			after:  func(t *testing.T, dir string) { writeTestFile(t, filepath.Join(dir, "added.json")) }},
		{name: "listener only after watch",
			after: func(t *testing.T, dir string) {
				listenRelaySock(t, dir)
				writeTestFile(t, filepath.Join(dir, "added.json"))
			},
			want: func(dir string) string { return filepath.Join(dir, "added.json") }},
		{name: "only logs and run changed",
			before: func(t *testing.T, dir string) {
				for _, sub := range []string{"logs", "run"} {
					if err := os.Mkdir(filepath.Join(dir, sub), 0o700); err != nil {
						t.Fatal(err)
					}
				}
			},
			after: func(t *testing.T, dir string) {
				writeTestFile(t, filepath.Join(dir, "logs", "relay.log"))
				writeTestFile(t, filepath.Join(dir, "run", "svc.pid"))
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := mkShortTempDir(t, "realdir-watch-")
			if tc.subdir {
				dir = filepath.Join(dir, "relay")
			}
			if tc.before != nil {
				tc.before(t, dir)
			}
			w := watchRealDir(dir)
			if w.liveOwner != tc.wantLive {
				t.Fatalf("liveOwner = %v, want %v", w.liveOwner, tc.wantLive)
			}
			tc.after(t, dir)
			v := w.violations()
			if tc.want == nil {
				if len(v) != 0 {
					t.Fatalf("violations = %q, want none", v)
				}
				return
			}
			want := tc.want(dir)
			if len(v) == 0 || !strings.Contains(strings.Join(v, "\n"), want) {
				t.Fatalf("violations = %q, want one naming %q", v, want)
			}
		})
	}
}

func TestLiveRelayAnswers(t *testing.T) {
	if liveRelayAnswers(t.TempDir()) {
		t.Fatal("liveRelayAnswers = true with no listener, want false")
	}
	dir := mkShortTempDir(t, "live-relay-")
	listenRelaySock(t, dir)
	if !liveRelayAnswers(dir) {
		t.Fatal("liveRelayAnswers = false with a listener on relay.sock, want true")
	}
}

const suiteHomeChildExitEnv = "GO_WANT_SUITE_HOME_CHILD_EXIT"

// TestSuiteHomeChildHelper is not a real test: only a child spawned by
// TestSuiteHome_ChildAdoptsParentRoot sets its guard. It leaves through
// os.Exit so TestMain's cleanup never runs, as helpers calling main() do.
func TestSuiteHomeChildHelper(t *testing.T) {
	code := os.Getenv(suiteHomeChildExitEnv)
	if code == "" {
		return
	}
	n, err := strconv.Atoi(code)
	if err != nil {
		os.Exit(2)
	}
	fmt.Printf("suiteHome=%s\n", suiteHome)
	os.Exit(n)
}

func suiteRootNames(t *testing.T) map[string]bool {
	t.Helper()
	matches, err := filepath.Glob("/tmp/relay-suite-home-*")
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool, len(matches))
	for _, m := range matches {
		names[m] = true
	}
	return names
}

func TestSuiteHome_ChildAdoptsParentRoot(t *testing.T) {
	for _, code := range []int{0, 1} {
		t.Run(fmt.Sprintf("exit %d", code), func(t *testing.T) {
			before := suiteRootNames(t)
			cmd := exec.Command(os.Args[0], "-test.run=^TestSuiteHomeChildHelper$")
			cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%d", suiteHomeChildExitEnv, code))
			out, err := cmd.Output()
			got := 0
			var stderr []byte
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				got, stderr = exitErr.ExitCode(), exitErr.Stderr
			} else if err != nil {
				t.Fatalf("run child: %v", err)
			}
			if got != code {
				t.Fatalf("child exit = %d, want %d; stdout:\n%s\nstderr:\n%s", got, code, out, stderr)
			}
			if want := "suiteHome=" + suiteHome + "\n"; !strings.Contains(string(out), want) {
				t.Fatalf("child stdout = %q, want it to contain %q", out, want)
			}
			for name := range suiteRootNames(t) {
				if !before[name] {
					t.Errorf("new suite root %s left behind by the child", name)
				}
			}
		})
	}
}
