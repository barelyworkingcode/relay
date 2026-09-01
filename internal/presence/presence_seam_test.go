package presence

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestMain lets AC-20 be checked after every test in this package's binary
// has run, regardless of source-file ordering: the real LocalAuthentication
// provider's constructor must never be reached anywhere in the hermetic
// suite. The developer-visible symptom of a regression here is a password
// dialog appearing during `go test`.
func TestMain(m *testing.M) {
	code := m.Run()
	if n := LocalAuthProviderConstructions(); n != 0 {
		fmt.Fprintf(os.Stderr, "presence: the real LocalAuthentication provider was constructed %d time(s) during this test run; the hermetic suite must use presencetest instead\n", n)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// moduleRoot finds the repository root from this test file's own location,
// so the seam guards below see the whole module regardless of the working
// directory `go test` was invoked from.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("presence: could not determine this test file's location")
	}
	for root := filepath.Dir(file); ; root = filepath.Dir(root) {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			return root
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatal("presence: could not find module root containing go.mod")
		}
	}
}

// nonTestSourceFiles returns every file under root with one of exts whose
// name does not end in _test.go — the set every seam guard here scans.
func nonTestSourceFiles(t *testing.T, root string, exts ...string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		for _, ext := range exts {
			if strings.HasSuffix(path, ext) {
				out = append(out, path)
				return nil
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("presence: walking %q: %v", root, err)
	}
	if len(out) == 0 {
		t.Fatalf("presence: found no source files under %q; the walk is misconfigured", root)
	}
	return out
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// TestPresence_SeamIsNotLinkedIntoTheBinary is AC-17: no non-test .go file
// in the module imports github.com/barelyworkingcode/relay/internal/presence/presencetest. Go links only what
// is imported, so a pass here is a proof the fake cannot reach a shipped
// binary — including transitively through another package.
func TestPresence_SeamIsNotLinkedIntoTheBinary(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	for _, path := range nonTestSourceFiles(t, root, ".go") {
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if p == "github.com/barelyworkingcode/relay/internal/presence/presencetest" {
				t.Errorf("%s imports github.com/barelyworkingcode/relay/internal/presence/presencetest, which must never reach a shipped binary", path)
			}
		}
	}
}

// TestPresencetest_HasAnInitGuard is AC-17b: presencetest defends itself
// with an init() that panics unless testing.Testing() — the last line of
// defence if the import guard above ever regresses.
func TestPresencetest_HasAnInitGuard(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, "internal", "presence", "presencetest", "presencetest.go")
	src := readFile(t, path)
	if !strings.Contains(src, "func init()") {
		t.Fatalf("%s has no init() function", path)
	}
	if !strings.Contains(src, "testing.Testing()") {
		t.Fatalf("%s does not call testing.Testing() from init(); it cannot defend itself if ever linked into a real binary", path)
	}
	if !strings.Contains(src, "panic(") {
		t.Fatalf("%s does not panic when the guard trips", path)
	}
}

// TestPresence_NeverReadsSSHEnvVars is AC-19: SSH_TTY and SSH_CONNECTION
// belong to the caller and are spoofable; relay must never read them for
// this determination.
func TestPresence_NeverReadsSSHEnvVars(t *testing.T) {
	root := moduleRoot(t)
	forbidden := []string{"SSH_TTY", "SSH_CONNECTION"}
	for _, path := range nonTestSourceFiles(t, root, ".go", ".h", ".m") {
		src := readFile(t, path)
		for _, f := range forbidden {
			if strings.Contains(src, f) {
				t.Errorf("%s references %s; the caller's environment must never be read for this determination (§6.6)", path, f)
			}
		}
	}
}

// TestPresence_NoGlobalSwitchExists is AC-18: there is no env var, settings
// field, or *ForTest global anywhere in this feature that disables or
// weakens the gate.
func TestPresence_NoGlobalSwitchExists(t *testing.T) {
	root := moduleRoot(t)
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`RELAY_\w*PRESENCE`),
		regexp.MustCompile(`skip_presence`),
		regexp.MustCompile(`presence_disabled`),
		regexp.MustCompile(`SetPresence\w*`),
	}
	for _, path := range nonTestSourceFiles(t, root, ".go", ".h", ".m") {
		src := readFile(t, path)
		for _, re := range patterns {
			if re.MatchString(src) {
				t.Errorf("%s matches %s; there must be no switch that disables or weakens the presence gate", path, re.String())
			}
		}
	}
}

// TestPresence_NeverCallsCanEvaluatePolicy is half of AC-19g:
// canEvaluatePolicy returned YES inside a LaunchDaemon with no session at
// all, so nothing may gate on it — not as a pre-flight check, not at all.
func TestPresence_NeverCallsCanEvaluatePolicy(t *testing.T) {
	root := moduleRoot(t)
	for _, path := range nonTestSourceFiles(t, root, ".go", ".h", ".m") {
		src := readFile(t, path)
		if strings.Contains(src, "canEvaluatePolicy") {
			t.Errorf("%s calls canEvaluatePolicy; it must never be used, as a pre-flight check or otherwise (§6.5.1)", path)
		}
	}
}

// TestPresence_ProbeTakesAnExplicitPeerFD is AC-19c's static half: the
// session probe's exported entry point takes the peer connection's file
// descriptor as an explicit argument and contains no call that would
// resolve relay's own process instead (os.Getpid, or the C getpid()). The
// dynamic half — that bridge/server.go actually calls it with the peer's fd
// and not relay's own — is wired in a later step and gets its own test then.
func TestPresence_ProbeTakesAnExplicitPeerFD(t *testing.T) {
	root := moduleRoot(t)
	forbidden := []string{"os.Getpid", "getpid("}
	for _, name := range []string{"session_darwin.go", "session_other.go"} {
		path := filepath.Join(root, "internal", "presence", name)
		src := readFile(t, path)
		for _, f := range forbidden {
			if strings.Contains(src, f) {
				t.Errorf("%s references %s; the probe must ask about the peer passed to it, never about relay's own process (§6.6)", path, f)
			}
		}
	}
}
