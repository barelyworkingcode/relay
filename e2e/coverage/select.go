package coverage

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// AlwaysRun lists the feature tests every selection includes: the map check
// itself.
var AlwaysRun = []string{SelfTest}

const (
	featuresDir  = "e2e/features"
	featuresMD   = "docs/FEATURES.md"
	journeyOnlyV = "journey-only"
)

// Selection is the answer of Select: which feature tests a change reaches.
type Selection struct {
	Full     bool                // run every feature test
	Areas    []string            // areas the changed code paths belong to
	Tests    []string            // feature tests to run, sorted
	Unmapped []string            // changed code paths that match no area
	Narrowed map[string][]string // path (or "area:<name>") to the tests it no longer selects
	Total    int                 // feature tests defined at head
}

// RunPattern is the -run value for the features package: "." for a full run,
// else an anchored alternation of the selected tests and AlwaysRun.
func RunPattern(s Selection) string {
	if s.Full {
		return "."
	}
	set := map[string]bool{}
	for _, t := range s.Tests {
		set[t] = true
	}
	for _, t := range AlwaysRun {
		set[t] = true
	}
	names := sortedKeys(set)
	for i, n := range names {
		names[i] = regexp.QuoteMeta(n)
	}
	return "^(" + strings.Join(names, "|") + ")$"
}

// Select picks the feature tests the change from base to head reaches. The
// area maps are read from both commits with git, so a PR cannot narrow its
// own run. Any git failure is an error; nothing falls back to a full run.
// repoRoot's work tree should hold head (or a merge of it): the app build
// file set comes from it.
func Select(repoRoot, base, head string) (Selection, error) {
	g := gitRepo{root: repoRoot}
	changed, err := g.changedPaths(base, head)
	if err != nil {
		return Selection{}, err
	}
	baseMap, baseFM, err := g.maps(base)
	if err != nil {
		return Selection{}, err
	}
	headMap, headFM, err := g.maps(head)
	if err != nil {
		return Selection{}, err
	}
	baseFiles, err := g.featuresPackage(base)
	if err != nil {
		return Selection{}, err
	}
	headFiles, err := g.featuresPackage(head)
	if err != nil {
		return Selection{}, err
	}
	headTests := headFiles.testNames()
	baseByArea := testsByArea(baseMap, baseFM)
	headByArea := testsByArea(headMap, headFM)

	sel := Selection{Total: len(headTests)}
	tests := map[string]bool{}
	areas := map[string]bool{}
	unmapped := map[string]bool{}
	var appFiles map[string]bool // computed on first need

	for _, p := range changed {
		// Rule 1: a changed feature test file runs every test it defines.
		if path.Dir(p) == featuresDir && strings.HasSuffix(p, "_test.go") {
			if f, ok := headFiles[p]; ok {
				for _, n := range f.tests() {
					tests[n] = true
				}
			}
			if sharedDeclaration(baseFiles, p) || sharedDeclaration(headFiles, p) {
				sel.Full = true
			}
			continue
		}
		// Rule 2: any other test-only path selects nothing, unless it is a
		// file the app binaries build from.
		if IsTestOnly(p) {
			if strings.HasPrefix(p, "e2e/") {
				continue
			}
			if appFiles == nil {
				if appFiles, err = g.appFileSet(); err != nil {
					return Selection{}, err
				}
			}
			if !appFiles[p] {
				continue
			}
		}
		// Rule 3: a code path takes the areas of both maps.
		names := unionStrings(baseMap.AreasOf(p), headMap.AreasOf(p))
		if len(names) == 0 {
			unmapped[p] = true
			continue
		}
		for _, a := range names {
			areas[a] = true
			switch a {
			case AreaEveryTest:
				sel.Full = true
			case AreaNoTest:
			default:
				for t := range baseByArea[a] {
					tests[t] = true
				}
				for t := range headByArea[a] {
					tests[t] = true
				}
			}
		}
	}

	// Rule 4: the map check always runs.
	for _, t := range AlwaysRun {
		tests[t] = true
	}
	sel.Narrowed, err = narrowed(g, head, baseMap, headMap, baseByArea, headByArea, headTests)
	if err != nil {
		return Selection{}, err
	}

	if sel.Full {
		tests = headTests
	}
	for t := range tests {
		if headTests[t] {
			sel.Tests = append(sel.Tests, t)
		}
	}
	sort.Strings(sel.Tests)
	sel.Areas = sortedKeys(areas)
	sel.Unmapped = sortedKeys(unmapped)
	return sel, nil
}

// SelectFull is the selection of a run that skips nothing: every feature test
// defined at head.
func SelectFull(repoRoot, head string) (Selection, error) {
	g := gitRepo{root: repoRoot}
	files, err := g.featuresPackage(head)
	if err != nil {
		return Selection{}, err
	}
	all := files.testNames()
	return Selection{Full: true, Tests: sortedKeys(all), Total: len(all)}, nil
}

// narrowed applies rule 5. For every tracked code file at head, the tests the
// base map selects and the head map does not are a narrowing; so is an area
// that gains journey-only.
func narrowed(g gitRepo, head string, baseMap, headMap AreaMap, baseByArea, headByArea map[string]map[string]bool, headTests map[string]bool) (map[string][]string, error) {
	tracked, err := g.trackedAt(head)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	selectedBy := func(m AreaMap, byArea map[string]map[string]bool, p string) map[string]bool {
		set := map[string]bool{}
		for _, a := range m.AreasOf(p) {
			if a == AreaEveryTest {
				return headTests
			}
			if a != AreaNoTest {
				for t := range byArea[a] {
					set[t] = true
				}
			}
		}
		return set
	}
	for _, p := range tracked {
		if IsTestOnly(p) {
			continue
		}
		was := selectedBy(baseMap, baseByArea, p)
		now := selectedBy(headMap, headByArea, p)
		var lost []string
		for t := range was {
			if headTests[t] && !now[t] {
				lost = append(lost, t)
			}
		}
		if len(lost) > 0 {
			sort.Strings(lost)
			out[p] = lost
		}
	}
	for a := range headMap.JourneyOnly {
		if !baseMap.JourneyOnly[a] {
			out["area:"+a] = []string{journeyOnlyV}
		}
	}
	return out, nil
}

// testsByArea maps each non-reserved area to the feature tests named by rows
// of the goals that list it. A "deny" item is still a test.
func testsByArea(am AreaMap, fm FeatureMap) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, r := range fm.Rows {
		goal := r.ID
		if i := strings.IndexByte(goal, '.'); i >= 0 {
			goal = goal[:i]
		}
		for _, a := range am.GoalAreas[goal] {
			if IsReserved(a) {
				continue
			}
			for _, t := range r.Tests {
				if t.Kind != "e2e" {
					continue
				}
				if out[a] == nil {
					out[a] = map[string]bool{}
				}
				out[a][t.Name] = true
			}
		}
	}
	return out
}

// pkgFile is one parsed Go file of the features package.
type pkgFile struct {
	ast *ast.File
}

func (f pkgFile) tests() []string {
	var out []string
	for _, d := range f.ast.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && isTestFunc(fd) {
			out = append(out, fd.Name.Name)
		}
	}
	return out
}

type pkgFiles map[string]pkgFile // key is the repo-relative path

func (p pkgFiles) testNames() map[string]bool {
	out := map[string]bool{}
	for _, f := range p {
		for _, n := range f.tests() {
			out[n] = true
		}
	}
	return out
}

// sharedDeclaration reports whether the file at changed holds an init, a
// TestMain, or a top-level name another file of the package mentions. The
// check over-approximates on purpose: any identifier of that name counts, so a
// shadowing local widens the run and never narrows it.
func sharedDeclaration(files pkgFiles, changed string) bool {
	f, ok := files[changed]
	if !ok {
		return false
	}
	names := map[string]bool{}
	add := func(id *ast.Ident) {
		if id.Name != "_" {
			names[id.Name] = true
		}
	}
	for _, d := range f.ast.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil && (d.Name.Name == "init" || d.Name.Name == "TestMain") {
				return true
			}
			if d.Recv == nil && isTestFunc(d) {
				continue
			}
			add(d.Name)
		case *ast.GenDecl:
			for _, s := range d.Specs {
				switch s := s.(type) {
				case *ast.TypeSpec:
					add(s.Name)
				case *ast.ValueSpec:
					for _, id := range s.Names {
						add(id)
					}
				}
			}
		}
	}
	if len(names) == 0 {
		return false
	}
	for p, other := range files {
		if p == changed {
			continue
		}
		found := false
		ast.Inspect(other.ast, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && names[id.Name] {
				found = true
			}
			return !found
		})
		if found {
			return true
		}
	}
	return false
}

// gitRepo reads a repository through the git CLI with GIT_* cleared.
type gitRepo struct{ root string }

func (g gitRepo) run(args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", g.root}, args...)...)
	cmd.Env = cleanGitEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s in %s: %w: %s", strings.Join(args, " "), g.root, err, strings.TrimSpace(stderr.String()))
	}
	return b, nil
}

func splitNUL(b []byte) []string {
	var out []string
	for _, s := range strings.Split(string(b), "\x00") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// changedPaths lists the paths changed from the merge base of base and head to
// head. Renames are not detected, so a rename names both paths.
func (g gitRepo) changedPaths(base, head string) ([]string, error) {
	b, err := g.run("diff", "--no-renames", "--name-only", "-z", base+"..."+head)
	if err != nil {
		return nil, err
	}
	return splitNUL(b), nil
}

func (g gitRepo) trackedAt(rev string) ([]string, error) {
	b, err := g.run("ls-tree", "-r", "-z", "--name-only", rev)
	if err != nil {
		return nil, err
	}
	return splitNUL(b), nil
}

func (g gitRepo) maps(rev string) (AreaMap, FeatureMap, error) {
	b, err := g.run("show", rev+":"+featuresMD)
	if err != nil {
		return AreaMap{}, FeatureMap{}, err
	}
	am, _ := ParseAreaMap(string(b))
	fm, _ := parseFeatureMap(string(b))
	return am, fm, nil
}

// featuresPackage parses every Go file directly under e2e/features at rev.
func (g gitRepo) featuresPackage(rev string) (pkgFiles, error) {
	b, err := g.run("ls-tree", "-z", "--name-only", rev, featuresDir+"/")
	if err != nil {
		return nil, err
	}
	out := pkgFiles{}
	fset := token.NewFileSet()
	for _, p := range splitNUL(b) {
		if !strings.HasSuffix(p, ".go") {
			continue
		}
		src, err := g.run("show", rev+":"+p)
		if err != nil {
			return nil, err
		}
		f, err := parser.ParseFile(fset, p, src, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parsing %s at %s: %w", p, rev, err)
		}
		out[p] = pkgFile{f}
	}
	return out, nil
}

// appFileSet lists the files the app binaries build from, from the work tree.
// A root with no go.mod (a fixture repository) has none.
func (g gitRepo) appFileSet() (map[string]bool, error) {
	set := map[string]bool{}
	if _, err := os.Stat(filepath.Join(g.root, "go.mod")); err != nil {
		return set, nil
	}
	files, err := appBuildFiles(g.root)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		set[f] = true
	}
	return set, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func unionStrings(a, b []string) []string {
	set := map[string]bool{}
	for _, s := range a {
		set[s] = true
	}
	for _, s := range b {
		set[s] = true
	}
	return sortedKeys(set)
}
