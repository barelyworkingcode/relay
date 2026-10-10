package coverage

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// mapOf builds an AreaMap from code globs; goals maps a goal ID to its areas.
func mapOf(code map[string][]string, goals map[string][]string, journeyOnly ...string) AreaMap {
	am := AreaMap{Code: code, JourneyOnly: map[string]bool{}, GoalAreas: goals}
	for _, j := range journeyOnly {
		am.JourneyOnly[j] = true
	}
	return am
}

func rowOf(id string, tests ...string) Row {
	r := Row{ID: id}
	for _, n := range tests {
		r.Tests = append(r.Tests, TestItem{Kind: "e2e", Name: n})
	}
	return r
}

// has reports whether findings hold one of the rule that names where.
func has(fs []Finding, rule, where string) bool {
	for _, f := range fs {
		if f.Rule == rule && f.Where == where {
			return true
		}
	}
	return false
}

func TestMapRulesPureInputs(t *testing.T) {
	t.Parallel()
	am := mapOf(
		map[string][]string{
			"alpha":      {"cmd/alpha/**"},
			"beta":       {"cmd/beta/**"},
			"jo":         {"cmd/jo/**"},
			"every-test": {"e2e/harness/**"},
			"no-test":    {"scripts/**", "cmd/tool/**"},
		},
		map[string][]string{"G1": {"alpha", "every-test"}, "G2": {"no-test"}},
		"jo",
	)
	fm := FeatureMap{Rows: []Row{rowOf("G1.01", "TestA"), rowOf("G2.01", "TestB")}}
	tests := map[string]bool{"TestA": true, "TestB": true}
	tracked := []string{
		"cmd/alpha/a.go", "cmd/orphan/o.go", "cmd/orphan/o_test.go", "docs/a.md",
		"cmd/orphan/testdata/x.json", "test/fixtures/f.json", "scripts/s.sh",
	}
	fs := MapFindings(am, fm, tests, tracked, []string{"cmd/alpha/a.go", "cmd/tool/t.go"}, []string{"e2e/other/o_test.go: TestOther"})

	// M1: the unmapped code file is named; every test-only path is not.
	if !has(fs, "M1", "cmd/orphan/o.go") {
		t.Errorf("M1 missed the unmapped code file: %v", fs)
	}
	for _, f := range fs {
		if f.Rule == "M1" && f.Where != "cmd/orphan/o.go" {
			t.Errorf("M1 reported %s, which is mapped or test-only", f.Where)
		}
	}
	// M2: a row test whose goal has only reserved areas; a stray Test.
	if !has(fs, "M2", "TestB") {
		t.Errorf("M2 missed the row test with no area: %v", fs)
	}
	if has(fs, "M2", "TestA") {
		t.Errorf("M2 reported TestA, which resolves to alpha")
	}
	if !has(fs, "M2", "e2e/other/o_test.go: TestOther") {
		t.Errorf("M2 missed the Test outside the allowed packages: %v", fs)
	}
	// M3: beta selects nothing; alpha, reserved and journey-only areas pass.
	if !has(fs, "M3", "beta") {
		t.Errorf("M3 missed the area with no feature test: %v", fs)
	}
	for _, ok := range []string{"alpha", "every-test", "no-test", "jo"} {
		if has(fs, "M3", ok) {
			t.Errorf("M3 reported %s, which is exempt or selects a test", ok)
		}
	}
	// M4: an app file mapped only to no-test.
	if !has(fs, "M4", "cmd/tool/t.go") {
		t.Errorf("M4 missed the app file mapped only to no-test: %v", fs)
	}
	if has(fs, "M4", "cmd/alpha/a.go") {
		t.Errorf("M4 reported an app file mapped to a real area")
	}
}

func TestMapRulesM3IgnoresMissingTest(t *testing.T) {
	t.Parallel()
	// A row names TestGone, which no feature file defines: the area selects nothing.
	am := mapOf(map[string][]string{"alpha": {"a/**"}}, map[string][]string{"G1": {"alpha"}})
	fm := FeatureMap{Rows: []Row{rowOf("G1.01", "TestGone")}}
	if fs := MapFindings(am, fm, map[string]bool{}, nil, nil, nil); !has(fs, "M3", "alpha") {
		t.Errorf("M3 missed an area whose only row test does not exist: %v", fs)
	}
}

const areasDoc = "## Areas\n\n```yaml\n%s```\n\n### G1 · Goal\n\n%s\n"

func TestParseAreaMapSyntax(t *testing.T) {
	t.Parallel()
	good := "areas:\n  alpha:\n    code: [a/**, b/*.go]\n    journey-only: true\n"
	cases := []struct {
		name, block, goal, want string
	}{
		{"bad glob", "areas:\n  alpha:\n    code: [a/[]\n", "Areas: alpha.", "glob"},
		{"mid ** glob", "areas:\n  alpha:\n    code: [a/**/b.go]\n", "Areas: alpha.", "glob"},
		{"bad area name", "areas:\n  Alpha_1:\n    code: [a/**]\n", "Areas: Alpha_1.", "area name"},
		{"unknown key", "areas:\n  alpha:\n    code: [a/**]\n    colour: red\n", "Areas: alpha.", "unknown key"},
		{"key twice", "areas:\n  alpha:\n    code: [a/**]\n    code: [b/**]\n", "Areas: alpha.", "given twice"},
		{"tab indent", "areas:\n  alpha:\n\tcode: [a/**]\n", "Areas: alpha.", "tab"},
		{"undefined area", good, "Areas: alpha, ghost.", "ghost"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, fs := ParseAreaMap(sprintDoc(c.block, c.goal))
			for _, f := range fs {
				if strings.Contains(f.Msg, c.want) {
					return
				}
			}
			t.Errorf("no finding mentions %q: %v", c.want, fs)
		})
	}
	am, fs := ParseAreaMap(sprintDoc(good, "Areas: alpha."))
	if len(fs) != 0 {
		t.Errorf("valid map reported findings: %v", fs)
	}
	if got := am.AreasOf("a/x/y.go"); len(got) != 1 || got[0] != "alpha" || !am.JourneyOnly["alpha"] {
		t.Errorf("valid map parsed wrong: areas %v journey-only %v", got, am.JourneyOnly)
	}
}

func sprintDoc(block, goal string) string {
	return strings.Replace(strings.Replace(areasDoc, "%s", block, 1), "%s", goal, 1)
}

func TestGlobMatch(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		glob, path string
		want       bool
	}{
		{"cmd/*.go", "cmd/a.go", true},
		{"cmd/*.go", "cmd/sub/a.go", false},
		{"cmd/**", "cmd/a/b/c.go", true},
		{"cmd/**", "cmd", false},
		{"cmd/**", "other/a.go", false},
	} {
		got, err := GlobMatch(c.glob, c.path)
		if err != nil || got != c.want {
			t.Errorf("GlobMatch(%q, %q) = %v, %v; want %v", c.glob, c.path, got, err, c.want)
		}
	}
	for _, g := range []string{"a/**/b.go", "**/a.go", "**"} {
		if _, err := GlobMatch(g, "a/x/b.go"); err == nil {
			t.Errorf("GlobMatch(%q) accepted a misplaced **", g)
		}
	}
}

func TestIsTestOnly(t *testing.T) {
	t.Parallel()
	for p, want := range map[string]bool{
		"x_test.go":                true,
		"a.md":                     true,
		"pkg/testdata/f.json":      true,
		"testdata/f.json":          true,
		"test/fixtures/f.json":     true,
		"cmd/main.go":              false,
		"testdata/../cmd/main.go":  false,
		"test/fixtures/../../x.go": false,
		"../x_test.go":             false,
		"pkg/mytestdata/f.json":    false,
	} {
		if got := IsTestOnly(p); got != want {
			t.Errorf("IsTestOnly(%q) = %v, want %v", p, got, want)
		}
	}
}

// TestCheckMapOnGitCheckout drives the rules through the real entry point:
// git ls-files for tracked paths, go list for app files, a scan for stray Tests.
func TestCheckMapOnGitCheckout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module acme.test/app\n\ngo 1.21\n")
	write("cmd/relay/main.go", "package main\n\nfunc main() {}\n")
	write("cmd/relaysessions/main.go", "package main\n\nfunc main() {}\n")
	write("orphan/code.go", "package orphan\n")
	write("orphan/code_test.go", "package orphan\n")
	write("orphan/notes.md", "# notes\n")
	write("orphan/testdata/x.json", "{}\n")
	write("e2e/go.mod", "module acme.test/e2e\n\ngo 1.21\n")
	write("e2e/features/sub/x_test.go", "package sub\n\nimport \"testing\"\n\nfunc TestSub(t *testing.T) {}\n")
	write("e2e/contract/deep/x_test.go", "package deep\n\nimport \"testing\"\n\nfunc TestDeep(t *testing.T) {}\n")
	write("e2e/other/o_test.go", "package other\n\nimport \"testing\"\n\nfunc TestOther(t *testing.T) {}\n")
	write("docs/FEATURES.md", sprintDoc("areas:\n  app:\n    code: [cmd/relaysessions/**]\n  no-test:\n    code: [cmd/relay/**, go.mod, e2e/**, docs/**]\n", "Areas: app."))
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = cleanGitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	fs, err := CheckMap(root)
	if err != nil {
		t.Fatal(err)
	}
	if !has(fs, "M1", "orphan/code.go") {
		t.Errorf("M1 missed the unmapped tracked file: %v", fs)
	}
	for _, f := range fs {
		if f.Rule == "M1" && f.Where != "orphan/code.go" {
			t.Errorf("M1 reported %s, which is mapped or test-only", f.Where)
		}
	}
	if !has(fs, "M2", "e2e/other/o_test.go: TestOther") {
		t.Errorf("M2 missed the stray Test: %v", fs)
	}
	for _, name := range []string{"e2e/features/sub/x_test.go: TestSub", "e2e/contract/deep/x_test.go: TestDeep"} {
		if !has(fs, "M2", name) {
			t.Errorf("M2 missed the Test in a subpackage: %s: %v", name, fs)
		}
	}
	if !has(fs, "M4", "cmd/relay/main.go") {
		t.Errorf("M4 missed the app file mapped only to no-test: %v", fs)
	}
	if has(fs, "M4", "cmd/relaysessions/main.go") {
		t.Errorf("M4 reported an app file mapped to a real area")
	}
}

// A tree with no .git skips the map rules with a note, not a failure.
func TestCheckSkipsMapRulesWithoutGit(t *testing.T) {
	t.Parallel()
	all, err := Check(fixtureRepo, loadDoors(t))
	if err != nil {
		t.Fatal(err)
	}
	if !hasRule(all, "info", "map rules M1 to M4 skipped") {
		t.Errorf("no info finding for the skipped map rules: %v", all)
	}
	for _, f := range Failures(all) {
		if strings.HasPrefix(f.Rule, "M") {
			t.Errorf("map rule failed on a non-git tree: %s", f)
		}
	}
}
