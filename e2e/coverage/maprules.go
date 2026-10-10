package coverage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// CheckMap runs the map rules (syntax, M1 to M4) on the checkout at repoRoot.
// Tracked files come from git and the app build files from go list, so the
// root must be a git work tree holding the relay module. The error is for
// inputs it cannot read; a rule miss is a Finding.
func CheckMap(repoRoot string) ([]Finding, error) {
	features, err := os.ReadFile(filepath.Join(repoRoot, "docs", "FEATURES.md"))
	if err != nil {
		return nil, fmt.Errorf("reading docs/FEATURES.md: %w", err)
	}
	fm, _ := parseFeatureMap(string(features))
	src, err := scanGo(filepath.Join(repoRoot, "e2e"))
	if err != nil {
		return nil, err
	}
	return checkMap(repoRoot, string(features), fm, src.featureTests)
}

func checkMap(repoRoot, features string, fm FeatureMap, featureTests map[string]bool) ([]Finding, error) {
	am, fs := ParseAreaMap(features)
	tracked, err := trackedFiles(repoRoot)
	if err != nil {
		return nil, err
	}
	app, err := appBuildFiles(repoRoot)
	if err != nil {
		return nil, err
	}
	stray, err := strayTests(filepath.Join(repoRoot, "e2e"))
	if err != nil {
		return nil, err
	}
	return append(fs, MapFindings(am, fm, featureTests, tracked, app, stray)...), nil
}

// MapFindings applies M1 to M4 to already-gathered inputs. tracked is every
// tracked path, app the app build files, stray the "path: Test" names defined
// outside the allowed e2e packages.
func MapFindings(am AreaMap, fm FeatureMap, featureTests map[string]bool, tracked, app, stray []string) []Finding {
	var out []Finding
	add := func(rule, where, format string, a ...any) {
		out = append(out, Finding{rule, where, fmt.Sprintf(format, a...)})
	}

	// M1: every tracked file that is not test-only matches a glob.
	unmapped := map[string]bool{}
	for _, p := range tracked {
		if IsTestOnly(p) {
			continue
		}
		if len(am.AreasOf(p)) == 0 {
			unmapped[p] = true
			add("M1", p, "tracked code file matches no area glob")
		}
	}

	// M2: every row test resolves to a non-reserved area.
	testAreas := map[string]map[string]bool{}
	for _, r := range fm.Rows {
		goal := r.ID
		if i := strings.IndexByte(goal, '.'); i >= 0 {
			goal = goal[:i]
		}
		for _, t := range r.Tests {
			if t.Kind != "e2e" {
				continue
			}
			set := testAreas[t.Name]
			if set == nil {
				set = map[string]bool{}
				testAreas[t.Name] = set
			}
			for _, a := range am.GoalAreas[goal] {
				if !IsReserved(a) {
					set[a] = true
				}
			}
		}
	}
	for name, set := range testAreas {
		if len(set) == 0 {
			add("M2", name, "row test resolves to no area: its goal has no Areas: line naming a non-reserved area")
		}
	}
	for _, s := range stray {
		add("M2", s, "Test defined outside e2e/features, e2e/contract and e2e/coverage")
	}

	// M3: every non-reserved area that is not journey-only selects a test.
	selected := map[string]bool{}
	for name, set := range testAreas {
		if !featureTests[name] {
			continue
		}
		for a := range set {
			selected[a] = true
		}
	}
	for name := range am.Code {
		if IsReserved(name) || am.JourneyOnly[name] || selected[name] {
			continue
		}
		add("M3", name, "area selects no feature test: no goal listing it has a row with an existing e2e: test")
	}

	// M4: every app build file maps to an area other than no-test.
	for _, p := range app {
		if unmapped[p] {
			continue
		}
		real := false
		for _, a := range am.AreasOf(p) {
			real = real || a != AreaNoTest
		}
		if !real {
			add("M4", p, "app build file maps only to %s or to no area", AreaNoTest)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Rule != out[j].Rule {
			return out[i].Rule < out[j].Rule
		}
		return out[i].Where < out[j].Where
	})
	return out
}

func trackedFiles(repoRoot string) ([]string, error) {
	cmd := exec.Command("git", "-C", repoRoot, "ls-files", "-z")
	cmd.Env = cleanGitEnv()
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files in %s: %w", repoRoot, err)
	}
	var out []string
	for _, p := range strings.Split(string(b), "\x00") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out, nil
}

// cleanGitEnv drops GIT_* so a hook or test environment cannot point git at
// another repository.
func cleanGitEnv() []string {
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GIT_") {
			env = append(env, e)
		}
	}
	return env
}

type listedPkg struct {
	Dir        string
	ImportPath string
	Module     *struct {
		Main bool
		Dir  string
	}
	GoFiles, CgoFiles, CFiles, CXXFiles, MFiles, HFiles, SFiles, SwigFiles, SwigCXXFiles, SysoFiles, EmbedFiles []string
}

// appBuildFiles lists the files of the module's own packages that the two app
// binaries build from, test build included. Cgo, C, Objective-C, headers and
// embedded files count with the Go files.
func appBuildFiles(repoRoot string) ([]string, error) {
	var outputs [][]byte
	// The release build (no tags) holds files the test build never compiles.
	for _, tags := range []string{"relaytest", ""} {
		args := []string{"list", "-deps", "-json"}
		if tags != "" {
			args = append(args, "-tags", tags)
		}
		args = append(args, "./cmd/relay", "./cmd/relaysessions")
		cmd := exec.Command("go", args...)
		cmd.Dir = repoRoot
		cmd.Env = cleanGitEnv()
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		b, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("go list (tags %q) in %s: %w: %s", tags, repoRoot, err, strings.TrimSpace(stderr.String()))
		}
		outputs = append(outputs, b)
	}
	root, err := filepath.Abs(repoRoot)
	if err == nil {
		root, err = filepath.EvalSymlinks(root)
	}
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, b := range outputs {
		dec := json.NewDecoder(bytes.NewReader(b))
		for dec.More() {
			var p listedPkg
			if err := dec.Decode(&p); err != nil {
				return nil, fmt.Errorf("decoding go list output: %w", err)
			}
			if p.Module == nil || !p.Module.Main {
				continue
			}
			dir, err := filepath.EvalSymlinks(p.Dir)
			if err != nil {
				return nil, err
			}
			for _, group := range [][]string{p.GoFiles, p.CgoFiles, p.CFiles, p.CXXFiles, p.MFiles, p.HFiles, p.SFiles, p.SwigFiles, p.SwigCXXFiles, p.SysoFiles, p.EmbedFiles} {
				for _, f := range group {
					rel, err := filepath.Rel(root, filepath.Join(dir, f))
					if err != nil {
						return nil, err
					}
					seen[filepath.ToSlash(rel)] = true
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// strayTests names each Test function defined in the e2e module outside the
// features, contract and coverage packages ("path: TestName").
func strayTests(e2eDir string) ([]string, error) {
	var out []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(e2eDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(e2eDir, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if d.Name() == "testdata" || (strings.HasPrefix(d.Name(), ".") && p != e2eDir) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, "_test.go") {
			return nil
		}
		if dir := path.Dir(rel); dir == "features" || dir == "contract" || strings.HasPrefix(rel, "coverage/") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parsing %s: %w", rel, err)
		}
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv == nil && isTestFunc(fd) {
				out = append(out, "e2e/"+rel+": "+fd.Name.Name)
			}
		}
		return nil
	})
	return out, err
}
