package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// PackageTests names the top-level test functions of one package that a change
// adds or alters.
type PackageTests struct {
	ImportPath string   // as go list reports it
	Tests      []string // sorted, unique, each a top-level TestXxx name
}

// changedTests returns, per package, the TestXxx functions whose declaration
// text differs between base and head or that do not exist at base.
func changedTests(repoDir, base, head string) ([]PackageTests, error) {
	pkgs, _, err := selectTests(repoDir, base, head)
	return pkgs, err
}

// selectTests also reports how many changed _test.go files the default test
// build leaves out, so -list can say what it skipped.
func selectTests(repoDir, base, head string) ([]PackageTests, int, error) {
	out, err := git(repoDir, "-c", "core.quotePath=false", "diff", "--name-only", "--no-renames",
		"--diff-filter=AM", base+"..."+head, "--", "*_test.go")
	if err != nil {
		return nil, 0, fmt.Errorf("git diff failed: %w (%s)", err, head)
	}
	changed := nonEmptyLines(out)
	if len(changed) == 0 {
		return nil, 0, nil
	}

	// The three-dot diff above compares against the merge-base, so the base
	// copy must come from there too: a function main changed after the fork
	// would otherwise look changed by the PR.
	mergeBase, err := git(repoDir, "merge-base", base, head)
	if err != nil {
		return nil, 0, fmt.Errorf("git merge-base failed: %w (%s, %s)", err, base, head)
	}
	mergeBase = strings.TrimSpace(mergeBase)

	inBuild, err := defaultBuildTestFiles(repoDir)
	if err != nil {
		return nil, 0, err
	}

	// A set of changed files per package; files outside the default build are
	// only counted.
	byPkg := map[string][]string{}
	excluded := 0
	for _, f := range changed {
		ip, ok := inBuild[f]
		if !ok {
			excluded++
			continue
		}
		byPkg[ip] = append(byPkg[ip], f)
	}

	var result []PackageTests
	for ip, files := range byPkg {
		tests, err := changedInPackage(repoDir, mergeBase, head, ip, files)
		if err != nil {
			return nil, 0, err
		}
		if len(tests) > 0 {
			result = append(result, PackageTests{ImportPath: ip, Tests: tests})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ImportPath < result[j].ImportPath })
	return result, excluded, nil
}

// defaultBuildTestFiles maps "<rel dir>/<file>" to the import path of the
// package whose default test build contains that file.
func defaultBuildTestFiles(repoDir string) (map[string]string, error) {
	mod, err := runIn(repoDir, "go", "list", "-m")
	if err != nil {
		return nil, fmt.Errorf("go list -m failed: %w (.)", err)
	}
	modPath := strings.TrimSpace(mod)
	out, err := runIn(repoDir, "go", "list", "-e", "-f",
		"{{.ImportPath}}\t{{join .TestGoFiles \" \"}}\t{{join .XTestGoFiles \" \"}}", "./...")
	if err != nil {
		return nil, fmt.Errorf("go list failed: %w (%s/...)", err, modPath)
	}
	files := map[string]string{}
	for _, line := range nonEmptyLines(out) {
		cols := strings.Split(line, "\t")
		if len(cols) != 3 {
			return nil, fmt.Errorf("go list output not understood (%s)", modPath)
		}
		ip := cols[0]
		dir := strings.TrimPrefix(strings.TrimPrefix(ip, modPath), "/")
		for _, f := range strings.Fields(cols[1] + " " + cols[2]) {
			files[path.Join(dir, f)] = ip
		}
	}
	return files, nil
}

func changedInPackage(repoDir, base, head, importPath string, files []string) ([]string, error) {
	baseSet, err := baseDeclarations(repoDir, base, path.Dir(files[0]))
	if err != nil {
		return nil, fmt.Errorf("reading base tests failed: %w (%s)", err, importPath)
	}
	found := map[string]bool{}
	for _, f := range files {
		src, err := git(repoDir, "show", head+":"+f)
		if err != nil {
			return nil, fmt.Errorf("reading head test file failed: %w (%s)", err, importPath)
		}
		decls, err := parseDecls([]byte(src))
		if err != nil {
			return nil, fmt.Errorf("parsing head test file failed: %w (%s)", err, importPath)
		}
		for _, d := range decls {
			if !d.isTest {
				continue
			}
			if !contains(baseSet[d.name], d.text) {
				found[d.name] = true
			}
		}
	}
	names := make([]string, 0, len(found))
	for n := range found {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

// baseDeclarations collects the declaration text of every function in every
// _test.go file directly in dir at base, so a function moved between files of
// one folder is not new. A base file that does not parse is left out, which
// makes its functions count as new: more runs, never fewer.
func baseDeclarations(repoDir, base, dir string) (map[string][]string, error) {
	spec := dir + "/"
	if dir == "." {
		spec = "."
	}
	listing, err := git(repoDir, "ls-tree", "--name-only", base, "--", spec)
	if err != nil {
		return nil, err
	}
	set := map[string][]string{}
	for _, p := range nonEmptyLines(listing) {
		if !strings.HasSuffix(p, "_test.go") || path.Dir(p) != path.Clean(dir) {
			continue
		}
		src, err := git(repoDir, "show", base+":"+p)
		if err != nil {
			return nil, err
		}
		decls, err := parseDecls([]byte(src))
		if err != nil {
			continue
		}
		for _, d := range decls {
			set[d.name] = append(set[d.name], d.text)
		}
	}
	return set, nil
}

type declaration struct {
	name   string
	text   string
	isTest bool
}

// parseDecls returns every receiver-less function in src. The text starts at
// the func keyword, so a doc comment change alone alters nothing.
func parseDecls(src []byte) ([]declaration, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	testingName := testingImportName(file)
	var decls []declaration
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv != nil {
			continue
		}
		start := fset.Position(fn.Pos()).Offset
		end := fset.Position(fn.End()).Offset
		decls = append(decls, declaration{
			name:   fn.Name.Name,
			text:   string(src[start:end]),
			isTest: testingName != "" && isTestFunc(fn, testingName),
		})
	}
	return decls, nil
}

// testingImportName is the file's local name for "testing", or "" when it is
// not imported under a usable name.
func testingImportName(file *ast.File) string {
	for _, imp := range file.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || p != "testing" {
			continue
		}
		if imp.Name == nil {
			return "testing"
		}
		if imp.Name.Name == "_" || imp.Name.Name == "." {
			return ""
		}
		return imp.Name.Name
	}
	return ""
}

func isTestFunc(fn *ast.FuncDecl, testingName string) bool {
	if fn.Type.TypeParams != nil || !isTestName(fn.Name.Name) {
		return false
	}
	params := fn.Type.Params.List
	if len(params) != 1 || len(params[0].Names) > 1 {
		return false
	}
	star, ok := params[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "T" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == testingName
}

// isTestName applies go test's own rule: "Test" alone, or followed by a rune
// that is not a lowercase letter.
func isTestName(name string) bool {
	rest, ok := strings.CutPrefix(name, "Test")
	if !ok {
		return false
	}
	if rest == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return !unicode.IsLower(r)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimRight(l, "\r"); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func git(repoDir string, args ...string) (string, error) {
	return runIn(repoDir, "git", args...)
}

// runIn runs a tool in dir and returns stdout; stderr goes into the error.
func runIn(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
