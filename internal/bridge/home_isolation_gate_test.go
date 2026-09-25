package bridge

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// configPathHelpers are the calls that resolve a path under the real config
// dir when HOME is not isolated. The scan is source text: a package that
// reaches them only through another package is not seen.
var configPathHelpers = []string{
	"bridge.ConfigDir(",
	"bridge.SocketPath(",
	"bridge.ModelSocketPath(",
	"os.UserConfigDir(",
}

const homeIsolationCall = `os.Setenv("HOME"`

// homeIsolationExempt lists package dirs (relative to the module root) that
// resolve config paths in tests but isolate them some other way.
var homeIsolationExempt = map[string]string{
	"cmd/relay": "isolates per test with mkSandboxRelayHome and runs its own end-of-run guard on the real ConfigDir",
	"internal/sshhost": "its TestMain points bridge.SetConfigDirForTest at a temp dir, and the package reaches the " +
		"config dir only through bridge.ConfigDir, which honours that override",
}

func TestHomeIsolationGate_PackagesResolvingConfigPathsIsolateHome(t *testing.T) {
	root := moduleRoot(t)
	matched := 0
	var failures []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if path != root && (name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)

		files, hasTests := goFilesIn(t, path)
		if !hasTests || !mentionsConfigPathHelper(files) {
			return nil
		}
		matched++
		if _, ok := homeIsolationExempt[rel]; ok {
			return nil
		}
		if !testMainSetsHome(t, files) {
			failures = append(failures, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk module at %s: %v", root, err)
	}

	if matched == 0 {
		t.Fatalf("scan of %s found no package resolving config paths; the scan is broken", root)
	}
	sort.Strings(failures)
	for _, rel := range failures {
		t.Errorf("%s: tests resolve config paths but no func TestMain calls %s; point HOME at a temp dir before m.Run()", rel, homeIsolationCall)
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found above the test's working directory")
		}
		dir = parent
	}
}

// goFilesIn returns the contents of every .go file in dir, keyed by path,
// and whether any of them is a _test.go file.
func goFilesIn(t *testing.T, dir string) (map[string]string, bool) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	files := map[string]string{}
	hasTests := false
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		files[p] = string(b)
		if strings.HasSuffix(e.Name(), "_test.go") {
			hasTests = true
		}
	}
	return files, hasTests
}

func mentionsConfigPathHelper(files map[string]string) bool {
	for _, src := range files {
		for _, h := range configPathHelpers {
			if strings.Contains(src, h) {
				return true
			}
		}
	}
	return false
}

func testMainSetsHome(t *testing.T, files map[string]string) bool {
	t.Helper()
	for path, src := range files {
		if !strings.HasSuffix(path, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != "TestMain" || fn.Body == nil {
				continue
			}
			body := src[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset]
			if strings.Contains(body, homeIsolationCall) {
				return true
			}
		}
	}
	return false
}
