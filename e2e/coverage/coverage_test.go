package coverage

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	fixtureRepo  = "testdata/repo"
	fixtureDoors = "testdata/doors.json"
)

func loadDoors(t *testing.T) DoorsDoc {
	t.Helper()
	data, err := os.ReadFile(fixtureDoors)
	if err != nil {
		t.Fatal(err)
	}
	d, err := ParseDoors(data)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// copyFixture copies the fixture tree so a test can break one file of it.
func copyFixture(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(fixtureRepo, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(fixtureRepo, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o755)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func replaceIn(t *testing.T, root, rel, old, repl string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), old) {
		t.Fatalf("%s does not contain %q", rel, old)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(data), old, repl, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendTo(t *testing.T, root, rel, text string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, root string) []Finding {
	t.Helper()
	fs, err := Check(root, loadDoors(t))
	if err != nil {
		t.Fatal(err)
	}
	return Failures(fs)
}

func hasRule(fs []Finding, rule, containing string) bool {
	for _, f := range fs {
		if f.Rule == rule && strings.Contains(f.Where+" "+f.Msg, containing) {
			return true
		}
	}
	return false
}

func TestFixtureIsClean(t *testing.T) {
	t.Parallel()
	all, err := Check(fixtureRepo, loadDoors(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range Failures(all) {
		t.Errorf("%s", f)
	}
	if !hasRule(all, "info", "G1.04") {
		t.Errorf("the screen-only row is not reported as info: %v", all)
	}
}

func TestRulesFireOnBrokenInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		file       string
		old, repl  string
		rule, text string
	}{
		{"R1 door without a row", "docs/FEATURES.md", "`cli:relay status`", "none", "R1", "cli:relay status"},
		{"R2 unknown door", "docs/FEATURES.md", "`http:GET /api/projects`", "`http:GET /api/nothing`", "R2", "GET /api/nothing"},
		{"R2 unknown door in a proof", "docs/FEATURES.md", "`code:http:POST /api/projects#403`", "`code:http:POST /api/x#403`", "R2", "POST /api/x"},
		{"R3 no test", "docs/FEATURES.md", "`e2e:TestProjectList`", "`journey:some-journey`", "R3", "G1.01"},
		{"R3 pending no longer listed", "docs/FEATURES.md", "`pending:#289`", "`pending:#291`", "R3", "pending:#291"},
		{"R3 screen-only off an exception row", "docs/FEATURES.md", "| exception: tray menu |", "| `relay serve` |", "R3", "G1.04"},
		{"R3 ci off the CI list", "docs/FEATURES.md", "**Rows proven by CI:** `G14.01`.", "No rows.", "R3", "G14.01"},
		{"R4 e2e item names a missing function", "docs/FEATURES.md", "`e2e:TestProjectList`", "`e2e:TestProjectListed`", "R4", "TestProjectListed"},
		{"R5 gated row without refusal proof", "docs/FEATURES.md", " `event:project.create=denied/presence_refused` `code:http:POST /api/projects#403`", " `event:project.create=ok#project_id`", "R5", "no refusal proof"},
		{"R5 missing deny test", "docs/FEATURES.md", " `deny e2e:TestProjectCreateDeniedCLI@cli:relay project create`", "", "R5", "cli:relay project create"},
		{"R6 unknown event", "docs/FEATURES.md", "`event:project.list=ok#count`", "`event:project.listed=ok#count`", "R6", "project.listed"},
		{"R6 event outside section 7", "docs/FEATURES.md", "`event:project.list=ok#count`", "`event:not.in.seven`", "R6", "not.in.seven"},
		{"R7 duplicate ID", "docs/FEATURES.md", "`G1.03`", "`G1.02`", "R7", "used by another row"},
		{"R7 retired ID", "docs/FEATURES.md", "`G1.03`", "`G1.90`", "R7", "Retired"},
		{"R7 bad ID", "docs/FEATURES.md", "`G1.03`", "`G01.3`", "R7", "does not match"},
		{"R8 promise maps to a row without refusal", "docs/FEATURES.md", "| `G1.02` |\n| `TMT.1`", "| `G1.01` |\n| `TMT.1`", "R8", "TM4.1"},
		{"R8 promise names a missing row", "docs/FEATURES.md", "| `G1.02` |\n| `TMT.1`", "| `G1.77` |\n| `TMT.1`", "R8", "G1.77"},
		{"R9 code alone", "docs/FEATURES.md", "`event:project.list=ok#count` `out:http:GET /api/projects#.projects[]`", "`code:http:GET /api/projects#200`", "R9", "G1.01"},
		{"Q6 route heading missing", "docs/routes.md", "### POST /api/projects", "### POST /api/project", "Q6", "POST /api/projects"},
		{"Q6 cli heading missing", "docs/cli.md", "### `status`", "### `state`", "Q6", "relay status"},
		{"H1 no Parallel first", "e2e/features/g1_test.go", "func TestProjectList(t *testing.T) {\n\tt.Parallel()\n", "func TestProjectList(t *testing.T) {\n\t_ = 1\n\tt.Parallel()\n", "H1", "TestProjectList"},
		{"H2 sleep", "e2e/features/g1_test.go", "import \"testing\"", "import (\n\t\"testing\"\n\ttm \"time\"\n)\n\nvar _ = tm.Sleep", "H2", "time.Sleep"},
		{"H3 import", "e2e/features/g1_test.go", "import \"testing\"", "import (\n\t_ \"github.com/barelyworkingcode/relay/internal/x\"\n\t\"testing\"\n)", "H3", "internal/x"},
		{"H3 go.mod", "e2e/go.mod", "go 1.25.0", "go 1.25.0\n\nrequire github.com/barelyworkingcode/relay v0.0.0", "H3", "go.mod"},
		{"pending list widened", "e2e/coverage/pending.txt", "289\n", "289\n287\n", "pending", "#287"},
		{"syntax: unknown proof kind", "docs/FEATURES.md", "`event:project.list=ok#count`", "`log:project.list`", "syntax", "unknown proof kind"},
		{"syntax: deny without a door", "docs/FEATURES.md", "`deny e2e:TestProjectCreateDenied@http:POST /api/projects`", "`deny e2e:TestProjectCreateDenied`", "syntax", "needs @<door>"},
		{"syntax: empty power door", "docs/FEATURES.md", "| `relay status` | `http:GET", "|  | `http:GET", "syntax", "Power door"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := copyFixture(t)
			replaceIn(t, root, tc.file, tc.old, tc.repl)
			got := run(t, root)
			if !hasRule(got, tc.rule, tc.text) {
				t.Errorf("expected %s mentioning %q, got %v", tc.rule, tc.text, got)
			}
		})
	}
}

func TestOrphanTestAndSelfTestExemption(t *testing.T) {
	t.Parallel()
	root := copyFixture(t)
	appendTo(t, root, "e2e/features/g1_test.go", "\nfunc TestOrphan(t *testing.T) {\n\tt.Parallel()\n}\n")
	got := run(t, root)
	if !hasRule(got, "R4", "TestOrphan") {
		t.Errorf("an unnamed test is not reported: %v", got)
	}
	if hasRule(got, "R4", SelfTest) {
		t.Errorf("%s must be exempt from R4: %v", SelfTest, got)
	}
}

func TestDuplicateTestNameAcrossFiles(t *testing.T) {
	t.Parallel()
	root := copyFixture(t)
	appendTo(t, root, "e2e/features/g2_test.go", "package features\n\nimport \"testing\"\n\nfunc TestProjectList(t *testing.T) {\n\tt.Parallel()\n}\n")
	if got := run(t, root); !hasRule(got, "R4", "unique") {
		t.Errorf("a duplicated test name is not reported: %v", got)
	}
}

func TestLivePendingDefersDenyHalfOnly(t *testing.T) {
	t.Parallel()
	root := copyFixture(t)
	replaceIn(t, root, "docs/FEATURES.md", " `deny e2e:TestProjectCreateDeniedCLI@cli:relay project create`", "")
	replaceIn(t, root, "docs/FEATURES.md", "`e2e:TestProjectCreate` ", "`e2e:TestProjectCreate` `pending:#290` ")
	replaceIn(t, root, "e2e/features/g1_test.go", "func TestProjectCreateDeniedCLI(t *testing.T) {\n\tt.Parallel()\n}\n\n", "")
	got := run(t, root)
	if hasRule(got, "R5", "deny test item") {
		t.Errorf("a live pending item must defer the deny half: %v", got)
	}
	// The refusal proof half still applies.
	replaceIn(t, root, "docs/FEATURES.md", " `event:project.create=denied/presence_refused` `code:http:POST /api/projects#403`", "")
	if got := run(t, root); !hasRule(got, "R5", "no refusal proof") {
		t.Errorf("the refusal proof half must still apply: %v", got)
	}
}

func TestCIScriptMustExist(t *testing.T) {
	t.Parallel()
	root := copyFixture(t)
	if err := os.Remove(filepath.Join(root, "scripts", "check-test-build.sh")); err != nil {
		t.Fatal(err)
	}
	if got := run(t, root); !hasRule(got, "R3", "does not exist") {
		t.Errorf("a missing ci script is not reported: %v", got)
	}
}

func TestDoorsSchemaRefused(t *testing.T) {
	t.Parallel()
	if _, err := ParseDoors([]byte(`{"schema":2,"doors":[]}`)); err == nil {
		t.Error("schema 2 was accepted")
	}
	if _, err := Check(fixtureRepo, DoorsDoc{Schema: 2}); err == nil {
		t.Error("Check accepted schema 2")
	}
}

func TestUnreadableInputIsAnError(t *testing.T) {
	t.Parallel()
	root := copyFixture(t)
	if err := os.Remove(filepath.Join(root, "docs", "routes.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := Check(root, loadDoors(t)); err == nil {
		t.Error("a missing routes.md was not an error")
	}
}

func TestParseProofAndTestItems(t *testing.T) {
	t.Parallel()
	ok := []string{"event:a.b=denied/not_granted#field", "audit:control_decision=denied", "out:cli:relay status#.a.b[]", "out:http:GET /x#.", "code:cli:relay x#2"}
	for _, s := range ok {
		if _, err := parseProof(s); err != nil {
			t.Errorf("parseProof(%q): %v", s, err)
		}
	}
	bad := []string{"event:nodot", "event:a.b=maybe", "out:cli:relay x#a", "code:http:GET /x#2xx", "code:bogus:x#1", "out:cli:relay x"}
	for _, s := range bad {
		if _, err := parseProof(s); err == nil {
			t.Errorf("parseProof(%q) accepted", s)
		}
	}
	for _, s := range []string{"e2e:TestA", "deny e2e:TestA@cli:relay x", "journey:abc", "pending:#12", "ci:scripts/x.sh", "screen-only"} {
		if _, err := parseTestItem(s); err != nil {
			t.Errorf("parseTestItem(%q): %v", s, err)
		}
	}
	for _, s := range []string{"e2e:testA", "e2e:Test", "pending:12", "deny e2e:TestA", "tests:TestA"} {
		if _, err := parseTestItem(s); err == nil {
			t.Errorf("parseTestItem(%q) accepted", s)
		}
	}
}
