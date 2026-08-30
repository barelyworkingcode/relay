package main

// The shared AST scan behind three structural guards (ADR-018 step 3, §4.1.2):
// gate_structural_test.go's gate-coverage guard, ipc_thread_structural_test.go's
// gatedIPCMethods equality check, and the enrolment.sign regression this file
// used to carry alone as requireGateOpLiteralIn. All three need the same fact
// -- which method encloses which requireGate/requireIssuanceAuditor call --
// so it is computed once here rather than reviewed once per file.
//
// Matching is by identifier name, the same limitation gate_structural_test.go
// and ipc_thread_structural_test.go already carry: go/ast alone cannot
// resolve a *McpOps receiver from an unrelated type of the same name. Nothing
// in this module has that collision today.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func gateASTModuleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("gate_ast_scan_test.go: could not determine this file's location")
	}
	root := filepath.Dir(file)
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("gate_ast_scan_test.go: computed module root %q has no go.mod: %v", root, err)
	}
	return root
}

// gateASTFiles returns every non-test .go file directly in root -- the same
// package-main source set gate_structural_test.go's mutation-containment
// scan walks.
func gateASTFiles(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading %q: %v", root, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		t.Fatalf("found no package-main source files under %q; the scan is misconfigured", root)
	}
	return out
}

// funcRecvTypeName strips a leading pointer star, if any, so "*EnrolmentOps"
// and "EnrolmentOps" both report as "EnrolmentOps".
func funcRecvTypeName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// enclosingMethodName names a top-level func or method declaration the way
// every call site in this file is keyed: "Receiver.Method" for a method,
// the bare function name for a free function (resetSealedStore's shape --
// sealed.reset has no core, only a tray-only free function, ADR-017 §5.6
// clause 5).
func enclosingMethodName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	return funcRecvTypeName(fd.Recv.List[0].Type) + "." + fd.Name.Name
}

// gateCallSite is one requireGate(...) call site found by scanning package
// main's non-test source (§4.1.2): the op literal it gates, the method (or
// free function) enclosing it, and the file it is in.
type gateCallSite struct {
	op     string
	method string
	file   string
}

// scanRequireGateCallSites generalises requireGateOpLiteralIn (formerly a
// single-file, single-method helper in enrolment_ops_sign_test.go) into a
// scan of the whole package: the guards built on this need the complete set
// of call sites to prove coverage is derived from source, not merely
// reviewed into a comment that can go stale.
//
// This is also a security check in its own right, not just a data
// collector: a computed (non-literal) op argument would defeat both this
// scan and Gate.Require's own redemption check (Require and Redeem use the
// identical string for both, so a wrong-but-still-gated op succeeds exactly
// as if it had been asked for correctly -- nothing observable from outside
// Require distinguishes the two). A non-literal op argument fails the suite
// by name here rather than being silently skipped.
func scanRequireGateCallSites(t *testing.T, root string) []gateCallSite {
	t.Helper()
	fset := token.NewFileSet()
	var sites []gateCallSite
	for _, name := range gateASTFiles(t, root) {
		path := filepath.Join(root, name)
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			method := enclosingMethodName(fd)
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				ident, ok := call.Fun.(*ast.Ident)
				if !ok || ident.Name != "requireGate" {
					return true
				}
				if len(call.Args) < 3 {
					t.Fatalf("%s: requireGate call in %s has %d args, want at least 3", name, method, len(call.Args))
				}
				lit, ok := call.Args[2].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("%s: requireGate's op argument in %s is not a string literal (%T) -- a computed op name would defeat this scan and Gate.Require's own redemption check", name, method, call.Args[2])
				}
				op, uerr := strconv.Unquote(lit.Value)
				assertNoErr(t, uerr, "unquote op literal %s", lit.Value)
				sites = append(sites, gateCallSite{op: op, method: method, file: name})
				return true
			})
		}
	}
	return sites
}

// scanRequireIssuanceAuditorCallSites is scanRequireGateCallSites' sibling
// for requireIssuanceAuditor (§4.2's fifth guard): it takes no op argument,
// so this returns only the enclosing method for each call site. Every
// method with a requireGate call also has one of these (§3.4), so this
// scan's result is always a superset of scanRequireGateCallSites' methods.
func scanRequireIssuanceAuditorCallSites(t *testing.T, root string) []string {
	t.Helper()
	fset := token.NewFileSet()
	var methods []string
	for _, name := range gateASTFiles(t, root) {
		path := filepath.Join(root, name)
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			method := enclosingMethodName(fd)
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				ident, ok := call.Fun.(*ast.Ident)
				if !ok || ident.Name != "requireIssuanceAuditor" {
					return true
				}
				methods = append(methods, method)
				return false
			})
		}
	}
	return methods
}
