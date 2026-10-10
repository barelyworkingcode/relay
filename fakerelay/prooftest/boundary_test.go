package prooftest

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/barelyworkingcode/relay/fakerelay"

// Criteria: one dependency; no import of relay; pure Go on Linux and macOS; no time.Sleep anywhere.
func TestImportBoundary(t *testing.T) {
	t.Parallel()

	mod, err := os.ReadFile("../go.mod")
	must(t, err, "read go.mod")
	var requires []string
	inBlock := false
	sc := bufio.NewScanner(strings.NewReader(string(mod)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "require (":
			inBlock = true
		case line == ")":
			inBlock = false
		case strings.HasPrefix(line, "require "):
			requires = append(requires, strings.Fields(line)[1])
		case inBlock && line != "":
			requires = append(requires, strings.Fields(line)[0])
		}
	}
	eq(t, requires, []string{"github.com/gorilla/websocket"}, "go.mod requires")

	fset := token.NewFileSet()
	err = filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		timeName := ""
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			switch {
			case p == "C":
				t.Errorf("%s imports C: not pure Go", path)
			case strings.HasPrefix(p, "github.com/barelyworkingcode/relay/") && p != modulePath && !strings.HasPrefix(p, modulePath+"/"):
				t.Errorf("%s imports relay package %s", path, p)
			case p == "time":
				timeName = "time"
				if imp.Name != nil {
					timeName = imp.Name.Name
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && timeName != "" {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == timeName && sel.Sel.Name == "Sleep" {
					t.Errorf("%s: time.Sleep at %s", path, fset.Position(sel.Pos()))
				}
			}
			return true
		})
		return nil
	})
	must(t, err, "walk module")

	for _, target := range []string{"linux/amd64", "linux/arm64", "darwin/arm64"} {
		goos, goarch, _ := strings.Cut(target, "/")
		cmd := exec.Command("go", "build", "-o", os.DevNull, "./cmd/fakerelay")
		cmd.Dir = ".."
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("CGO_ENABLED=0 build for %s: %v\n%s", target, err, out)
		}
	}
}
