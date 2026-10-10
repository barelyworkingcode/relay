package coverage

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const relayModule = "github.com/barelyworkingcode/relay"

type goScan struct {
	featureTests map[string]bool
	findings     []Finding
}

func isRelayPath(p string) bool { return p == relayModule || strings.HasPrefix(p, relayModule+"/") }

// scanGo parses every Go file under the e2e module (testdata directories are
// fixtures, not part of the module) and applies H1 to H3. It collects the test
// functions of e2e/features for R4.
func scanGo(e2eDir string) (goScan, error) {
	out := goScan{featureTests: map[string]bool{}}
	fset := token.NewFileSet()
	defs := map[string]string{}
	err := filepath.WalkDir(e2eDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(e2eDir, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if d.Name() == "testdata" || (strings.HasPrefix(d.Name(), ".") && path != e2eDir) {
				return filepath.SkipDir
			}
			return nil
		}
		if rel == "go.mod" {
			return out.checkGoMod(path)
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parsing %s: %w", rel, err)
		}
		isTest := strings.HasSuffix(path, "_test.go")
		inFeatures := strings.HasPrefix(rel, "features/")
		out.checkImports(f, fset, rel)
		out.checkSleep(f, fset, rel)
		if !isTest {
			return nil
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || !isTestFunc(fd) {
				continue
			}
			where := fmt.Sprintf("%s:%d %s", rel, fset.Position(fd.Pos()).Line, fd.Name.Name)
			if !startsParallel(fd) {
				out.findings = append(out.findings, Finding{"H1", where, "t.Parallel() is not the first statement"})
			}
			if inFeatures {
				if prev, dup := defs[fd.Name.Name]; dup {
					out.findings = append(out.findings, Finding{"R4", fd.Name.Name, fmt.Sprintf("defined in %s and %s; test names are unique across e2e", prev, rel)})
				}
				defs[fd.Name.Name] = rel
				out.featureTests[fd.Name.Name] = true
			}
		}
		return nil
	})
	return out, err
}

func (g *goScan) checkGoMod(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for n, line := range strings.Split(string(data), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		for _, tok := range strings.Fields(strings.NewReplacer("\"", " ", "`", " ").Replace(line)) {
			if isRelayPath(tok) {
				g.findings = append(g.findings, Finding{"H3", fmt.Sprintf("go.mod:%d", n+1), "e2e/go.mod names " + tok + "; the e2e module imports nothing of relay's"})
			}
		}
	}
	return nil
}

func (g *goScan) checkImports(f *ast.File, fset *token.FileSet, rel string) {
	for _, im := range f.Imports {
		p, err := strconv.Unquote(im.Path.Value)
		if err == nil && isRelayPath(p) {
			g.findings = append(g.findings, Finding{"H3", fmt.Sprintf("%s:%d", rel, fset.Position(im.Pos()).Line), "imports " + p})
		}
	}
}

// checkSleep finds calls of time.Sleep through the file's own name for the
// time package, so an alias does not hide one.
func (g *goScan) checkSleep(f *ast.File, fset *token.FileSet, rel string) {
	names := map[string]bool{}
	for _, im := range f.Imports {
		if p, _ := strconv.Unquote(im.Path.Value); p == "time" {
			name := "time"
			if im.Name != nil {
				name = im.Name.Name
			}
			names[name] = true
		}
	}
	if len(names) == 0 {
		return
	}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Sleep" {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && names[id.Name] {
			g.findings = append(g.findings, Finding{"H2", fmt.Sprintf("%s:%d", rel, fset.Position(sel.Pos()).Line), "time.Sleep; wait on an event, a ready line or a handle"})
		}
		return true
	})
}

// isTestFunc matches `func TestXxx(t *testing.T)`; TestMain takes *testing.M
// and Test followed by a lower-case letter is not a test.
func isTestFunc(fd *ast.FuncDecl) bool {
	name := fd.Name.Name
	if !strings.HasPrefix(name, "Test") || (len(name) > 4 && name[4] >= 'a' && name[4] <= 'z') {
		return false
	}
	return testingTParam(fd) != ""
}

// testingTParam returns the name of the single *testing.T parameter, or "".
func testingTParam(fd *ast.FuncDecl) string {
	ps := fd.Type.Params.List
	if len(ps) != 1 || len(ps[0].Names) != 1 {
		return ""
	}
	star, ok := ps[0].Type.(*ast.StarExpr)
	if !ok {
		return ""
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "T" {
		return ""
	}
	if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "testing" {
		return ""
	}
	return ps[0].Names[0].Name
}

func startsParallel(fd *ast.FuncDecl) bool {
	if fd.Body == nil || len(fd.Body.List) == 0 {
		return false
	}
	es, ok := fd.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := es.X.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Parallel" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == testingTParam(fd)
}
