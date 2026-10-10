package coverage

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
)

const selectBase = "testdata/select/base"

// selFix is a fixture repository under construction: the base tree is copied
// from testdata/select/base, committed, then changed and committed as head.
type selFix struct {
	t    *testing.T
	root string
}

func (f selFix) git(args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", f.root, "-c", "user.name=Acme", "-c", "user.email=acme@example.test", "-c", "commit.gpgsign=false", "-c", "maintenance.auto=false", "-c", "gc.auto=0"}, args...)...)
	// Deliberate: the fixture's git ignores the machine's global and system
	// config (hooks, maintenance), so no detached process can still be writing
	// under .git when t.TempDir cleanup runs.
	cmd.Env = append(cleanGitEnv(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f selFix) write(rel, body string) {
	f.t.Helper()
	p := filepath.Join(f.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f selFix) remove(rel string) {
	f.t.Helper()
	if err := os.Remove(filepath.Join(f.root, filepath.FromSlash(rel))); err != nil {
		f.t.Fatal(err)
	}
}

func (f selFix) edit(rel, old, repl string) {
	f.t.Helper()
	p := filepath.Join(f.root, filepath.FromSlash(rel))
	b, err := os.ReadFile(p)
	if err != nil {
		f.t.Fatal(err)
	}
	if !strings.Contains(string(b), old) {
		f.t.Fatalf("%s holds no %q", rel, old)
	}
	f.write(rel, strings.Replace(string(b), old, repl, 1))
}

// newSelFix builds the fixture repository: base commit, then change, then
// head commit. It returns the repository and the two commit ids.
func newSelFix(t *testing.T, change func(selFix)) (selFix, string, string) {
	t.Helper()
	f := selFix{t: t, root: t.TempDir()}
	err := filepath.WalkDir(selectBase, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(selectBase, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		f.write(filepath.ToSlash(rel), string(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	f.git("init", "-q")
	f.git("add", "-A")
	f.git("commit", "-q", "-m", "base")
	base := f.git("rev-parse", "HEAD")
	change(f)
	f.git("add", "-A")
	f.git("commit", "-q", "--allow-empty", "-m", "head")
	return f, base, f.git("rev-parse", "HEAD")
}

const (
	tAlpha1 = "TestAlphaOne"
	tAlpha2 = "TestAlphaTwo"
	tBeta1  = "TestBetaOne"
	tGamma1 = "TestGammaOne"
	tHelper = "TestHelperOnly"
	tCover  = SelfTest
	// selTotal is the number of feature tests the base fixture defines.
	selTotal = 6
)

func TestSelectByChange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		change       func(selFix)
		full         bool
		areas, tests []string
		unmapped     []string
		narrows      bool
	}{
		{
			name:   "code path selects its area's row tests only",
			change: func(f selFix) { f.write("alpha/a.go", "package alpha\n\nconst Name = \"changed\"\n") },
			areas:  []string{"alpha"},
			tests:  []string{tAlpha1, tAlpha2, tCover},
		},
		{
			name: "changed features test file selects every test it defines",
			change: func(f selFix) {
				f.edit("e2e/features/alpha_test.go", "func TestAlphaTwo(t *testing.T) {\n\tt.Parallel()\n", "func TestAlphaTwo(t *testing.T) {\n\tt.Parallel()\n\t_ = 1\n")
			},
			tests: []string{tAlpha1, tAlpha2, tCover},
		},
		{
			name:   "docs-only diff selects only the map check",
			change: func(f selFix) { f.write("docs/notes.md", "# Notes\n\nmore\n") },
			tests:  []string{tCover},
		},
		{
			name:   "test data under a testdata directory selects only the map check",
			change: func(f selFix) { f.write("alpha/testdata/x.json", "{}\n") },
			tests:  []string{tCover},
		},
		{
			name:   "no-test path selects only the map check",
			change: func(f selFix) { f.write("scripts/check.sh", "#!/bin/sh\nfalse\n") },
			areas:  []string{"no-test"},
			tests:  []string{tCover},
		},
		{
			name: "init in a changed test file runs the full package",
			change: func(f selFix) {
				f.edit("e2e/features/gamma_test.go", "func TestGammaOne", "func init() {}\n\nfunc TestGammaOne")
			},
			full: true,
		},
		{
			name:   "declaration another file uses in a changed test file runs the full package",
			change: func(f selFix) { f.edit("e2e/features/helpers_test.go", "return 1", "return 2") },
			full:   true,
		},
		{
			// Nothing at head declares helperValue and no head file uses it: only the base tree shows the removed declaration.
			name: "declaration removed from a changed test file runs the full package",
			change: func(f selFix) {
				f.edit("e2e/features/helpers_test.go", "func helperValue() int { return 1 }\n\n", "")
				f.edit("e2e/features/beta_test.go", "\t_ = helperValue()\n", "")
			},
			full: true,
		},
		{
			name:   "every-test path runs the full package",
			change: func(f selFix) { f.write("e2e/harness/h.go", "package harness\n\nconst Root = \"changed\"\n") },
			areas:  []string{"every-test"},
			full:   true,
		},
		{
			name:     "code path in no area is unmapped",
			change:   func(f selFix) { f.write("newpkg/n.go", "package newpkg\n") },
			unmapped: []string{"newpkg/n.go"},
			tests:    []string{tCover},
		},
		{
			// The path is mapped by the base map only: the head map moves gamma's code.
			name: "area only in the base map still selects its tests",
			change: func(f selFix) {
				f.edit("docs/FEATURES.md", "code: [gamma/**]", "code: [gamma2/**]")
				f.write("gamma/g.go", "package gamma\n\nconst Name = \"changed\"\n")
			},
			areas:   []string{"gamma"},
			tests:   []string{tGamma1, tCover},
			narrows: true, // the head map no longer reaches gamma/g.go
		},
		{
			// The path is mapped by the head map only: the head map extends beta's code.
			name: "area only in the head map selects its tests",
			change: func(f selFix) {
				f.edit("docs/FEATURES.md", "code: [beta/**,", "code: [beta/**, newpkg/**,")
				f.write("newpkg/n.go", "package newpkg\n")
			},
			areas: []string{"beta"},
			tests: []string{tBeta1, tHelper, tCover},
		},
		{
			// TestHelperOnly joins gamma's rows only in the head map.
			name: "test an area gains in the head map is selected",
			change: func(f selFix) {
				f.edit("docs/FEATURES.md", "| `e2e:TestGammaOne` |\n", "| `e2e:TestGammaOne` |\n| `G3.02` | Two | x | n/a | n/a | none | none | `event:gamma.two=ok` | `e2e:TestHelperOnly` |\n")
				f.write("gamma/g.go", "package gamma\n\nconst Name = \"changed\"\n")
			},
			areas: []string{"gamma"},
			tests: []string{tGamma1, tHelper, tCover},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f, base, head := newSelFix(t, c.change)
			sel, err := Select(f.root, base, head)
			if err != nil {
				t.Fatal(err)
			}
			if sel.Total != selTotal {
				t.Errorf("Total = %d, want %d", sel.Total, selTotal)
			}
			if sel.Full != c.full {
				t.Errorf("Full = %v, want %v", sel.Full, c.full)
			}
			if c.full {
				if got := RunPattern(sel); got != "." {
					t.Errorf("RunPattern = %q, want %q", got, ".")
				}
				if len(sel.Tests) != selTotal {
					t.Errorf("Tests = %v, want all %d", sel.Tests, selTotal)
				}
			} else {
				if !reflect.DeepEqual(sel.Tests, sortedCopy(c.tests)) {
					t.Errorf("Tests = %v, want %v", sel.Tests, c.tests)
				}
				want := "^(" + strings.Join(sortedCopy(c.tests), "|") + ")$"
				if got := RunPattern(sel); got != want {
					t.Errorf("RunPattern = %q, want %q", got, want)
				}
			}
			if !reflect.DeepEqual(emptyIfNil(sel.Areas), emptyIfNil(c.areas)) {
				t.Errorf("Areas = %v, want %v", sel.Areas, c.areas)
			}
			if !reflect.DeepEqual(emptyIfNil(sel.Unmapped), emptyIfNil(c.unmapped)) {
				t.Errorf("Unmapped = %v, want %v", sel.Unmapped, c.unmapped)
			}
			if (len(sel.Narrowed) != 0) != c.narrows {
				t.Errorf("Narrowed = %v, want narrowing: %v", sel.Narrowed, c.narrows)
			}
		})
	}
}

func sortedCopy(in []string) []string {
	set := map[string]bool{}
	for _, s := range in {
		set[s] = true
	}
	return sortedKeys(set)
}

func emptyIfNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func TestSelectBadRevisionIsAnError(t *testing.T) {
	t.Parallel()
	f, _, head := newSelFix(t, func(selFix) {})
	if _, err := Select(f.root, "0000000000000000000000000000000000000001", head); err == nil {
		t.Error("Select accepted a base revision that does not exist")
	}
}

// narrowingChanges are map changes that make a file select fewer tests, or
// make an area unreachable by headless tests.
var narrowingChanges = map[string]func(selFix){
	"row removed": func(f selFix) {
		f.edit("docs/FEATURES.md", "| `G1.02` | Two | x | n/a | n/a | none | none | `event:alpha.two=ok` | `e2e:TestAlphaTwo` |\n", "")
	},
	"area gains journey-only": func(f selFix) {
		f.edit("docs/FEATURES.md", "    code: [gamma/**]\n", "    code: [gamma/**]\n    journey-only: true\n")
	},
}

func TestSelectReportsNarrowing(t *testing.T) {
	t.Parallel()
	for name, change := range narrowingChanges {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, base, head := newSelFix(t, change)
			sel, err := Select(f.root, base, head)
			if err != nil {
				t.Fatal(err)
			}
			if len(sel.Narrowed) == 0 {
				t.Error("Narrowed is empty for a map change that narrows")
			}
		})
	}
	t.Run("row removed names the lost test", func(t *testing.T) {
		t.Parallel()
		f, base, head := newSelFix(t, narrowingChanges["row removed"])
		sel, err := Select(f.root, base, head)
		if err != nil {
			t.Fatal(err)
		}
		if got := sel.Narrowed["alpha/a.go"]; !reflect.DeepEqual(got, []string{tAlpha2}) {
			t.Errorf("Narrowed[alpha/a.go] = %v, want [%s]; all: %v", got, tAlpha2, sel.Narrowed)
		}
	})
}

var (
	selectBinOnce sync.Once
	selectBinPath string
	selectBinErr  error
	selectBinDir  string
)

// TestMain removes the directory selectBin builds into.
func TestMain(m *testing.M) {
	code := m.Run()
	if selectBinDir != "" {
		os.RemoveAll(selectBinDir)
	}
	os.Exit(code)
}

// selectBin builds the selecttests command once per test process.
func selectBin(t *testing.T) string {
	t.Helper()
	selectBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "selecttests-bin")
		if err != nil {
			selectBinErr = err
			return
		}
		selectBinDir = dir
		selectBinPath = filepath.Join(dir, "selecttests")
		cmd := exec.Command("go", "build", "-o", selectBinPath, "./cmd/selecttests")
		if out, err := cmd.CombinedOutput(); err != nil {
			selectBinErr = errors.New("go build: " + err.Error() + ": " + string(out))
		}
	})
	if selectBinErr != nil {
		t.Fatal(selectBinErr)
	}
	return selectBinPath
}

// runSelect runs the selecttests command and returns its exit code and stdout.
func runSelect(t *testing.T, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(selectBin(t), args...)
	cmd.Env = cleanGitEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatal(err)
		}
		code = ee.ExitCode()
	}
	return code, stdout.String()
}

func lineWith(out, prefix string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}

func TestSelectTestsCommand(t *testing.T) {
	t.Parallel()

	t.Run("selected change prints the run pattern and writes summary and env", func(t *testing.T) {
		t.Parallel()
		f, base, head := newSelFix(t, func(f selFix) { f.write("alpha/a.go", "package alpha\n\nconst Name = \"changed\"\n") })
		out := t.TempDir()
		envFile, summary := filepath.Join(out, "env"), filepath.Join(out, "summary.md")
		code, stdout := runSelect(t, "--repo", f.root, "--base", base, "--head", head, "--env", envFile, "--summary", summary)
		if code != 0 {
			t.Fatalf("exit %d, want 0: %s", code, stdout)
		}
		pattern := "^(" + strings.Join([]string{tAlpha1, tAlpha2, tCover}, "|") + ")$"
		if got := lineWith(stdout, "RUN "); got != "RUN "+pattern {
			t.Errorf("RUN line = %q, want %q", got, "RUN "+pattern)
		}
		if got := lineWith(stdout, "TESTS "); got != "TESTS 3/6" {
			t.Errorf("TESTS line = %q, want %q", got, "TESTS 3/6")
		}
		if got := lineWith(stdout, "AREAS "); !strings.Contains(got, "alpha") || strings.Contains(got, "beta") {
			t.Errorf("AREAS line = %q, want alpha only", got)
		}
		env, err := os.ReadFile(envFile)
		if err != nil {
			t.Fatal(err)
		}
		if string(env) != "SELECT_RUN="+pattern+"\n" {
			t.Errorf("env file = %q, want SELECT_RUN=%s", env, pattern)
		}
		sum, err := os.ReadFile(summary)
		if err != nil {
			t.Fatal(err)
		}
		s := string(sum)
		for _, want := range []string{"alpha", tAlpha1, tAlpha2, "3 of 6"} {
			if !strings.Contains(s, want) {
				t.Errorf("summary lacks %q: %s", want, s)
			}
		}
		if !regexp.MustCompile(`(?i)not run\D*3`).MatchString(s) {
			t.Errorf("summary lacks the count not run (3): %s", s)
		}
	})

	t.Run("full selects everything", func(t *testing.T) {
		t.Parallel()
		f, _, _ := newSelFix(t, func(selFix) {})
		envFile := filepath.Join(t.TempDir(), "env")
		code, stdout := runSelect(t, "--repo", f.root, "--full", "--env", envFile)
		if code != 0 {
			t.Fatalf("exit %d, want 0: %s", code, stdout)
		}
		if got := lineWith(stdout, "RUN "); got != "RUN ." {
			t.Errorf("RUN line = %q, want %q", got, "RUN .")
		}
		if got := lineWith(stdout, "TESTS "); got != "TESTS 6/6" {
			t.Errorf("TESTS line = %q, want %q", got, "TESTS 6/6")
		}
		if env, _ := os.ReadFile(envFile); string(env) != "SELECT_RUN=.\n" {
			t.Errorf("env file = %q, want SELECT_RUN=.", env)
		}
	})

	t.Run("docs-only change runs only the map check", func(t *testing.T) {
		t.Parallel()
		f, base, head := newSelFix(t, func(f selFix) { f.write("docs/notes.md", "# Notes\n\nmore\n") })
		code, stdout := runSelect(t, "--repo", f.root, "--base", base, "--head", head)
		if code != 0 {
			t.Fatalf("exit %d, want 0: %s", code, stdout)
		}
		if got := lineWith(stdout, "RUN "); got != "RUN ^("+tCover+")$" {
			t.Errorf("RUN line = %q, want only %s", got, tCover)
		}
	})

	t.Run("unmapped deleted path prints UNMAPPED and no RUN", func(t *testing.T) {
		t.Parallel()
		// legacy/old.go matches no area at base or head: the work tree is
		// clean at head, so only the selection can see it.
		g, _, _ := newSelFix(t, func(selFix) {})
		g.write("legacy/old.go", "package legacy\n")
		g.git("add", "-A")
		g.git("commit", "-q", "-m", "add legacy")
		base := g.git("rev-parse", "HEAD")
		g.remove("legacy/old.go")
		g.git("add", "-A")
		g.git("commit", "-q", "-m", "remove legacy")
		head := g.git("rev-parse", "HEAD")
		code, stdout := runSelect(t, "--repo", g.root, "--base", base, "--head", head)
		if code != 1 {
			t.Fatalf("exit %d, want 1: %s", code, stdout)
		}
		if got := lineWith(stdout, "UNMAPPED "); got != "UNMAPPED legacy/old.go" {
			t.Errorf("UNMAPPED line = %q: %s", got, stdout)
		}
		if got := lineWith(stdout, "RUN "); got != "" {
			t.Errorf("RUN line present with an unmapped path: %q", got)
		}
	})

	t.Run("added unmapped code file fails before any test runs", func(t *testing.T) {
		t.Parallel()
		f, base, head := newSelFix(t, func(f selFix) { f.write("newpkg/n.go", "package newpkg\n") })
		code, stdout := runSelect(t, "--repo", f.root, "--base", base, "--head", head)
		if code != 1 {
			t.Fatalf("exit %d, want 1: %s", code, stdout)
		}
		if !strings.Contains(stdout, "newpkg/n.go") || lineWith(stdout, "RUN ") != "" {
			t.Errorf("want newpkg/n.go named and no RUN line: %s", stdout)
		}
	})

	for name, change := range narrowingChanges {
		t.Run("narrowing "+name+" fails without the label and passes with it", func(t *testing.T) {
			t.Parallel()
			f, base, head := newSelFix(t, change)
			for _, labels := range []string{"", `[]`, `["other-label"]`} {
				code, stdout := runSelect(t, "--repo", f.root, "--base", base, "--head", head, "--labels-json", labels)
				if code != 1 {
					t.Errorf("labels %q: exit %d, want 1: %s", labels, code, stdout)
				}
				if lineWith(stdout, "NARROWED ") == "" || lineWith(stdout, "RUN ") != "" {
					t.Errorf("labels %q: want a NARROWED line and no RUN line: %s", labels, stdout)
				}
			}
			code, stdout := runSelect(t, "--repo", f.root, "--base", base, "--head", head, "--labels-json", `["x","map-narrowing-approved"]`)
			if code != 0 {
				t.Errorf("approved: exit %d, want 0: %s", code, stdout)
			}
			if lineWith(stdout, "RUN ") == "" {
				t.Errorf("approved: no RUN line: %s", stdout)
			}
		})
	}

	t.Run("deleting a test with its row is not narrowing", func(t *testing.T) {
		t.Parallel()
		f, base, head := newSelFix(t, func(f selFix) {
			f.edit("e2e/features/alpha_test.go", "func TestAlphaTwo(t *testing.T) {\n\tt.Parallel()\n}\n", "")
			narrowingChanges["row removed"](f)
		})
		code, stdout := runSelect(t, "--repo", f.root, "--base", base, "--head", head)
		if code != 0 {
			t.Fatalf("exit %d, want 0: %s", code, stdout)
		}
		if got := lineWith(stdout, "NARROWED "); got != "" {
			t.Errorf("NARROWED line for a deleted test: %q", got)
		}
	})

	t.Run("bad revision and bad usage exit 2", func(t *testing.T) {
		t.Parallel()
		f, _, head := newSelFix(t, func(selFix) {})
		for name, args := range map[string][]string{
			"bad base":      {"--repo", f.root, "--base", "0000000000000000000000000000000000000001", "--head", head},
			"bad head":      {"--repo", f.root, "--base", head, "--head", "0000000000000000000000000000000000000001"},
			"no repo":       {"--full"},
			"no base":       {"--repo", f.root},
			"base and full": {"--repo", f.root, "--base", head, "--full"},
		} {
			if code, out := runSelect(t, args...); code != 2 {
				t.Errorf("%s: exit %d, want 2: %s", name, code, out)
			}
		}
	})
}
