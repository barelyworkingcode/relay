package contract

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"testing"
)

// TestSurfacesCovered fails when a Surface constant is used by no
// Scenario{Surface: ...} literal in this package's test files. It reads the
// source, so a scenario that never runs still counts only if it is written.
func TestSurfacesCovered(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	declared := surfaceConstants(t, fset)
	if len(declared) != len(Surfaces) {
		t.Fatalf("surfaces.go declares %d Surface constants but Surfaces lists %d", len(declared), len(Surfaces))
	}
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatalf("listing test files: %v", err)
	}
	used := map[string]bool{}
	for _, f := range files {
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", f, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isScenarioType(lit.Type) {
				return true
			}
			for _, el := range lit.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Surface" {
					if name := identName(kv.Value); name != "" {
						used[name] = true
					}
				}
			}
			return true
		})
	}
	var missing []string
	for _, name := range declared {
		if !used[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("no Scenario{Surface: ...} literal uses %d of %d surfaces: %v", len(missing), len(declared), missing)
	}
}

func surfaceConstants(t *testing.T, fset *token.FileSet) []string {
	t.Helper()
	file, err := parser.ParseFile(fset, "surfaces.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing surfaces.go: %v", err)
	}
	var names []string
	for _, d := range file.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, s := range gd.Specs {
			vs := s.(*ast.ValueSpec)
			if id, ok := vs.Type.(*ast.Ident); ok && id.Name == "Surface" {
				for _, n := range vs.Names {
					names = append(names, n.Name)
				}
			}
		}
	}
	return names
}

func isScenarioType(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name == "Scenario"
	case *ast.SelectorExpr:
		return x.Sel.Name == "Scenario"
	}
	return false
}

func identName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	}
	return ""
}
