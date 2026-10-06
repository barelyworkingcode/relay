package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// builtTool is the burn-in binary, built once before any case runs so the
// build never lands inside a case.
var builtTool string

func TestMain(m *testing.M) {
	// This is deliberate: git exports GIT_DIR, GIT_INDEX_FILE and friends to
	// hooks, and a test run from a hook inherits them. Every fixture git call,
	// the in-process selection and the built tool would then act on the
	// repository that ran the hook instead of the case's own temp repo.
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "GIT_") {
			os.Unsetenv(name)
		}
	}
	dir, err := os.MkdirTemp("", "burnin-tool-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkdir temp:", err)
		os.Exit(1)
	}
	builtTool = filepath.Join(dir, "burnin")
	build := exec.Command("go", "build", "-o", builtTool, ".")
	build.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "go build burnin: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// repo is a throwaway module example.com/acme under git.
type repo struct {
	t   *testing.T
	dir string
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	for _, tool := range []string{"git", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is required: %v", tool, err)
		}
	}
	t.Setenv("GOWORK", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	r := &repo{t: t, dir: t.TempDir()}
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(r.dir))
	r.git("init", "-q", "-b", "main")
	gitDir := r.git("rev-parse", "--absolute-git-dir")
	if want := filepath.Join(r.dir, ".git"); !sameDir(gitDir, want) {
		t.Fatalf("fixture git dir = %s, want %s", gitDir, want)
	}
	r.write("go.mod", "module example.com/acme\n\ngo 1.21\n")
	return r
}

// sameDir compares directories after resolving symlinks (macOS temp dirs sit
// behind /var -> /private/var).
func sameDir(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	full := append([]string{"-c", "user.name=Acme", "-c", "user.email=dev@example.com", "-c", "commit.gpgsign=false"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = r.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *repo) write(rel, content string) {
	r.t.Helper()
	p := filepath.Join(r.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *repo) remove(rel string) {
	r.t.Helper()
	if err := os.Remove(filepath.Join(r.dir, filepath.FromSlash(rel))); err != nil {
		r.t.Fatal(err)
	}
}

func (r *repo) commit() string {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-q", "--allow-empty", "-m", "step")
	return r.git("rev-parse", "HEAD")
}

// run executes the built tool in the repo and returns stdout, stderr, exit code.
func (r *repo) run(env []string, args ...string) (string, string, int) {
	r.t.Helper()
	cmd := exec.Command(builtTool, args...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), env...)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			r.t.Fatalf("run tool: %v", err)
		}
		code = ee.ExitCode()
	}
	return so.String(), se.String(), code
}

func fn(name, body string) string {
	return "func " + name + "(t *testing.T) {\n\t" + body + "\n}"
}

func tf(pkg string, funcs ...string) string {
	return "package " + pkg + "\n\nimport \"testing\"\n\n" + strings.Join(funcs, "\n\n") + "\n"
}

func pkgFile(name string) string { return "package " + name + "\n" }

func pt(importPath string, tests ...string) PackageTests {
	return PackageTests{ImportPath: "example.com/acme/" + importPath, Tests: tests}
}

func selection(t *testing.T, r *repo, base, head string) []PackageTests {
	t.Helper()
	got, err := changedTests(r.dir, base, head)
	if err != nil {
		t.Fatalf("changedTests: %v", err)
	}
	return got
}

func requireSelection(t *testing.T, got []PackageTests, want ...PackageTests) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selection = %+v, want %+v", got, want)
	}
}

func TestNothingToRunWhenNoTestFileChanges(t *testing.T) {
	r := newRepo(t)
	r.write("a/a.go", pkgFile("a"))
	r.write("a/a_test.go", tf("a", fn("TestA", "t.Log(1)")))
	r.write("z/z.go", pkgFile("z"))
	r.write("z/z_test.go", tf("z", fn("TestAlwaysFails", `t.Fatal("must not run")`)))
	base := r.commit()
	r.write("a/a.go", "package a\n\nconst Changed = 1\n")
	r.write("README.md", "notes\n")
	head := r.commit()

	requireSelection(t, selection(t, r, base, head))
	stdout, stderr, code := r.run(nil, base, head)
	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout+stderr, "burn-in: no added or changed TestXxx functions; nothing to run") {
		t.Fatalf("missing nothing-to-run line:\n%s%s", stdout, stderr)
	}
}

func TestHelperAndTestMainEditsRunNothing(t *testing.T) {
	r := newRepo(t)
	r.write("a/a.go", pkgFile("a"))
	src := func(helperRet, mainBody string) string {
		return "package a\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\n" +
			"func helper() int { return " + helperRet + " }\n\n" +
			"func TestMain(m *testing.M) {\n\t" + mainBody + "\n}\n\n" +
			fn("TestUse", "_ = helper()") + "\n"
	}
	r.write("a/a_test.go", src("1", "os.Exit(m.Run())"))
	base := r.commit()
	r.write("a/a_test.go", src("2", "code := m.Run()\n\tos.Exit(code)"))
	head := r.commit()

	requireSelection(t, selection(t, r, base, head))
}

func TestAddedAndChangedSelectedUnchangedNot(t *testing.T) {
	r := newRepo(t)
	r.write("a/a.go", pkgFile("a"))
	r.write("a/a_test.go", tf("a", fn("TestB", "t.Log(1)"), fn("TestC", "t.Log(3)")))
	base := r.commit()
	r.write("a/a_test.go", tf("a", fn("TestB", "t.Log(2)"), fn("TestC", "t.Log(3)")))
	r.write("a/new_test.go", tf("a", fn("TestA", "t.Log(0)")))
	head := r.commit()

	requireSelection(t, selection(t, r, base, head), pt("a", "TestA", "TestB"))
}

func TestDocCommentIgnoredSignatureCounts(t *testing.T) {
	r := newRepo(t)
	r.write("a/a.go", pkgFile("a"))
	r.write("a/a_test.go", tf("a", fn("TestS", "_ = 1")))
	base := r.commit()

	r.write("a/a_test.go", tf("a", "// TestS now has a doc comment.\n"+fn("TestS", "_ = 1")))
	docOnly := r.commit()
	requireSelection(t, selection(t, r, base, docOnly))

	r.write("a/a_test.go", tf("a", "func TestS(_ *testing.T) {\n\t_ = 1\n}"))
	sig := r.commit()
	requireSelection(t, selection(t, r, docOnly, sig), pt("a", "TestS"))
}

func TestBranchBehindMainSelectsOnlyItsOwnChange(t *testing.T) {
	r := newRepo(t)
	r.write("a/a.go", pkgFile("a"))
	r.write("a/a_test.go", tf("a", fn("TestB", "t.Log(1)"), fn("TestZ", "t.Log(2)")))
	fork := r.commit()

	r.git("checkout", "-q", "-b", "pr")
	r.write("a/a_test.go", tf("a", fn("TestB", "t.Log(10)"), fn("TestZ", "t.Log(2)")))
	prHead := r.commit()

	r.git("checkout", "-q", "main")
	r.write("a/a_test.go", tf("a", fn("TestB", "t.Log(1)"), fn("TestZ", "t.Log(20)")))
	mainTip := r.commit()
	if mainTip == fork {
		t.Fatal("main did not move past the fork point")
	}

	requireSelection(t, selection(t, r, mainTip, prHead), pt("a", "TestB"))
}

func TestMovesWithinPackageIgnoredAcrossPackageSelected(t *testing.T) {
	r := newRepo(t)
	r.write("a/a.go", pkgFile("a"))
	r.write("b/b.go", pkgFile("b"))
	r.write("a/x_test.go", tf("a", fn("TestD", "t.Log(1)"), fn("TestKeep", "t.Log(2)")))
	r.write("a/e_test.go", tf("a", fn("TestE", "t.Log(3)")))
	base := r.commit()
	r.write("a/x_test.go", tf("a", fn("TestKeep", "t.Log(2)")))
	r.write("a/y_test.go", tf("a", fn("TestD", "t.Log(1)")))
	r.remove("a/e_test.go")
	r.write("b/e_test.go", tf("b", fn("TestE", "t.Log(3)")))
	head := r.commit()

	requireSelection(t, selection(t, r, base, head), pt("b", "TestE"))
}

func TestRenamedTestIsNew(t *testing.T) {
	r := newRepo(t)
	r.write("a/a.go", pkgFile("a"))
	r.write("a/a_test.go", tf("a", fn("TestOld", "t.Log(1)")))
	base := r.commit()
	r.write("a/a_test.go", tf("a", fn("TestNew", "t.Log(1)")))
	head := r.commit()

	requireSelection(t, selection(t, r, base, head), pt("a", "TestNew"))
}

func TestDeletionsRunNothing(t *testing.T) {
	r := newRepo(t)
	r.write("a/a.go", pkgFile("a"))
	r.write("a/a_test.go", tf("a", fn("TestGone", "t.Log(1)"), fn("TestStay", "t.Log(2)")))
	r.write("a/b_test.go", tf("a", fn("TestFileGone", "t.Log(3)")))
	base := r.commit()
	r.write("a/a_test.go", tf("a", fn("TestStay", "t.Log(2)")))
	r.remove("a/b_test.go")
	head := r.commit()

	requireSelection(t, selection(t, r, base, head))
}

func TestTaggedFileExcludedAndCounted(t *testing.T) {
	r := newRepo(t)
	r.write("a/a.go", pkgFile("a"))
	base := r.commit()
	r.write("a/live_test.go", "//go:build live\n\n"+tf("a", fn("TestLive", "t.Log(1)")))
	head := r.commit()

	requireSelection(t, selection(t, r, base, head))
	stdout, stderr, code := r.run(nil, "-list", base, head)
	if code != 0 || stdout != "" {
		t.Fatalf("-list exit %d stdout %q, want 0 and empty\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "burn-in: 1 changed _test.go file(s) outside the default test build, not run") {
		t.Fatalf("missing excluded-file count on stderr:\n%s", stderr)
	}
}

func TestTestdataFileExcluded(t *testing.T) {
	r := newRepo(t)
	r.write("a/a.go", pkgFile("a"))
	base := r.commit()
	r.write("a/testdata/fixture_test.go", tf("a", fn("TestFixture", "t.Log(1)")))
	head := r.commit()

	requireSelection(t, selection(t, r, base, head))
}

func TestExternalTestPackageListedUnderImportPath(t *testing.T) {
	r := newRepo(t)
	r.write("a/a.go", pkgFile("a"))
	base := r.commit()
	r.write("a/ext_test.go", tf("a_test", fn("TestX", "t.Log(1)")))
	head := r.commit()

	requireSelection(t, selection(t, r, base, head), pt("a", "TestX"))
}

func TestOnlyRealTestFunctionsListed(t *testing.T) {
	r := newRepo(t)
	r.write("a/a.go", pkgFile("a"))
	base := r.commit()
	r.write("a/n_test.go", "package a\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\n"+
		"func ExampleF() {}\n\n"+
		"func BenchmarkF(b *testing.B) {}\n\n"+
		"func FuzzF(f *testing.F) {}\n\n"+
		"func TestMain(m *testing.M) { os.Exit(m.Run()) }\n\n"+
		fn("Testlower", "t.Log(1)")+"\n\n"+
		"type S struct{}\n\n"+
		"func (s S) TestM(t *testing.T) {}\n\n"+
		"func TestH(t *testing.B) {}\n")
	r.write("a/alias_test.go", "package a\n\nimport tt \"testing\"\n\nfunc TestAlias(t *tt.T) {}\n")
	head := r.commit()

	requireSelection(t, selection(t, r, base, head), pt("a", "TestAlias"))
}

func TestSelectionSortedAndArgsPinned(t *testing.T) {
	r := newRepo(t)
	r.write("a/a.go", pkgFile("a"))
	r.write("b/b.go", pkgFile("b"))
	base := r.commit()
	r.write("b/b_test.go", tf("b", fn("TestB", "t.Log(1)")))
	r.write("a/a_test.go", tf("a", fn("TestZ", "t.Log(1)"), fn("TestA", "t.Log(2)")))
	head := r.commit()

	got := selection(t, r, base, head)
	requireSelection(t, got, pt("a", "TestA", "TestZ"), pt("b", "TestB"))

	wantArgs := []string{"test", "-race", "-count=10", "-timeout", "20m", "-run", "^(TestA|TestZ)$", "example.com/acme/a"}
	if args := goTestArgs(got[0]); !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("goTestArgs = %q, want %q", args, wantArgs)
	}

	stdout, _, code := r.run(nil, "-list", base, head)
	wantList := "example.com/acme/a\t^(TestA|TestZ)$\nexample.com/acme/b\t^(TestB)$\n"
	if code != 0 || stdout != wantList {
		t.Fatalf("-list exit %d stdout %q, want %q", code, stdout, wantList)
	}
}

func TestRunRepeatsUnderRaceAndReportsEveryFailingPackage(t *testing.T) {
	r := newRepo(t)
	r.write("a/a.go", pkgFile("a"))
	r.write("a/a_test.go", tf("a", fn("TestFlaky", "t.Log(0)")))
	r.write("b/b.go", pkgFile("b"))
	r.write("c/c.go", pkgFile("c"))
	r.write("c/c_test.go", tf("c", fn("TestSteady", "t.Log(0)")))
	r.write("b/race_on_test.go", "//go:build race\n\npackage b\n\nconst raceOn = true\n")
	r.write("b/race_off_test.go", "//go:build !race\n\npackage b\n\nconst raceOn = false\n")
	recorder := func(extra string) string {
		return "package b\n\nimport (\n\t\"fmt\"\n\t\"os\"\n\t\"testing\"\n)\n\n" +
			"func TestRecorder(t *testing.T) {\n" +
			"\t" + extra + "\n" +
			"\tf, err := os.OpenFile(os.Getenv(\"BURNIN_RECORD\"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)\n" +
			"\tif err != nil {\n\t\tt.Fatal(err)\n\t}\n\tdefer f.Close()\n" +
			"\tfmt.Fprintf(f, \"race=%v\\n\", raceOn)\n}\n\n" +
			fn("TestAlwaysFails", `t.Fatal("unchanged test must not run")`) + "\n"
	}
	r.write("b/b_test.go", "package b\n\nimport \"testing\"\n\nfunc TestRecorder(t *testing.T) {}\n\n"+
		fn("TestAlwaysFails", `t.Fatal("unchanged test must not run")`)+"\n")
	base := r.commit()

	r.write("a/a_test.go", "package a\n\nimport \"testing\"\n\nvar flakyRuns int\n\n"+
		fn("TestFlaky", "flakyRuns++\n\tif flakyRuns == 3 {\n\t\tt.Fatal(\"third run\")\n\t}")+"\n")
	r.write("b/b_test.go", recorder("t.Log(1)"))
	r.write("c/c_test.go", tf("c", fn("TestSteady", `t.Fatal("always")`)))
	head := r.commit()

	record := filepath.Join(t.TempDir(), "record.txt")
	env := []string{"BURNIN_RECORD=" + record}
	stdout, stderr, code := r.run(env, base, head)
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s%s", code, stdout, stderr)
	}
	errPkgs := map[string]int{}
	var errLines int
	for _, line := range strings.Split(stdout+stderr, "\n") {
		if !strings.HasPrefix(line, "::error") {
			continue
		}
		errLines++
		for pkg, run := range map[string]string{"a": "TestFlaky", "c": "TestSteady"} {
			if strings.HasPrefix(line, "::error::burn-in: example.com/acme/"+pkg+" ") {
				errPkgs[pkg]++
				if !strings.Contains(line, "-run ^("+run+")$") {
					t.Fatalf("error line does not name the test: %q", line)
				}
			}
		}
	}
	if errPkgs["a"] != 1 || errPkgs["c"] != 1 || errLines != 2 {
		t.Fatalf("want one ::error line each for a and c; got %v, %d total\n%s%s", errPkgs, errLines, stdout, stderr)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), strings.Repeat("race=true\n", 10); got != want {
		t.Fatalf("recorder wrote %q, want ten race=true lines", got)
	}

	// Only b changes: everything passes, and the unchanged failing test stays unrun.
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	r.write("b/b_test.go", recorder("t.Log(2)"))
	head2 := r.commit()
	stdout, stderr, code = r.run(env, head, head2)
	if code != 0 {
		t.Fatalf("second run exit %d, want 0\n%s%s", code, stdout, stderr)
	}
	if strings.Contains(stdout+stderr, "::error") {
		t.Fatalf("second run reported an error:\n%s%s", stdout, stderr)
	}
}

func TestParseFailures(t *testing.T) {
	t.Run("head file fails closed", func(t *testing.T) {
		r := newRepo(t)
		r.write("a/a.go", pkgFile("a"))
		r.write("a/a_test.go", tf("a", fn("TestOk", "t.Log(1)")))
		base := r.commit()
		r.write("a/a_test.go", "package a\n\nimport \"testing\"\n\nfunc TestBad(t *testing.T) {\n")
		head := r.commit()

		if _, err := changedTests(r.dir, base, head); err == nil {
			t.Fatal("changedTests returned no error for an unparseable head file")
		}
		stdout, stderr, code := r.run(nil, base, head)
		if code != 1 || !strings.Contains(stdout+stderr, "::error::burn-in:") {
			t.Fatalf("exit %d, want 1 with an ::error line\n%s%s", code, stdout, stderr)
		}
	})
	t.Run("base file that does not parse makes head functions new", func(t *testing.T) {
		r := newRepo(t)
		r.write("a/a.go", pkgFile("a"))
		r.write("a/a_test.go", tf("a", fn("TestQ", "t.Log(1)"))+"\nfunc broken( {\n")
		base := r.commit()
		r.write("a/a_test.go", tf("a", fn("TestQ", "t.Log(1)"), fn("TestP", "t.Log(2)")))
		head := r.commit()

		requireSelection(t, selection(t, r, base, head), pt("a", "TestP", "TestQ"))
	})
}

func TestBadUsageExitsTwo(t *testing.T) {
	r := newRepo(t)
	r.write("a/a.go", pkgFile("a"))
	base := r.commit()
	notACommit := strings.Repeat("0", 40)
	for name, args := range map[string][]string{
		"no arguments":      nil,
		"one argument":      {base},
		"head not a commit": {base, notACommit},
		"base not a commit": {notACommit, base},
	} {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, code := r.run(nil, args...)
			if code != 2 {
				t.Fatalf("exit %d, want 2\n%s%s", code, stdout, stderr)
			}
		})
	}
}
