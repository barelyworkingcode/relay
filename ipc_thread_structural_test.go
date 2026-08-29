package main

// TestIPC_GatedCoreCallsRunOffTheMainThread is the structural regression for
// defect 2 (ADR-017 implementation spec §6.5, §6.6): onSettingsIpc dispatches
// straight from the WKWebView callback on the Cocoa main thread
// (cocoa_darwin.go -> ipc_handlers.go's onSettingsIpc), and a gated core call
// blocks there inside Gate.Require, which itself blocks on
// LocalAuthentication's async completion handler -- a completion that needs
// the very run loop the blocked call owns to be pumped. That is a deadlock,
// not a slow path: trayapp.go's showLoginCode and confirmAndResetSealedStore
// carry the doc comment explaining it and the fix (hop off main with
// ctx.GoFunc / a.goFunc, hop back with DispatchToMain before touching the
// WebView).
//
// This parses every non-test ipc_*.go file with go/ast and walks each
// function body, tracking whether the node currently being visited is
// lexically inside the closure passed to a "ctx.GoFunc(...)" call. A call to
// one of the gated core methods below (gatedIPCMethods, mirrored from
// presence.GatedOps via §6.4's table) found OUTSIDE that closure fails the
// suite by name -- the same shape as gate_structural_test.go's AC-15 check
// for defect 1's sibling problem, applied to the main-thread boundary
// instead of the gate-bypass boundary.
//
// Matching is by identifier name (IPCContext field, then method), not by
// resolved type, for the same reason gate_structural_test.go's is: go/ast
// alone cannot tell one *ProjectOps receiver from an unrelated type that
// happens to have a field or method of the same name. Nothing in this
// module does, so that is not a live ambiguity today; if it ever becomes
// one, the fix is a narrower allowlist here, not weakening the check.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// gatedIPCMethods maps an IPCContext field name to the gated core methods
// reachable through it (ADR-017 implementation spec §6.4's table). A method
// is listed here if and only if its core calls requireGate/Gate.Require --
// including methods no current ipc_*.go handler reaches synchronously any
// more (ServiceOps.Remove, McpOps.StartOAuth), so a future handler that
// starts calling one directly is caught too. A method absent here because
// it is genuinely ungated (ServiceOps.Start/Stop/SetAutostart,
// LoginOps.Passkeys/Sessions/SignOut, EnrolmentOps.SetRemoteConfig,
// McpOps.ResetPermissions, ...) must stay absent: adding an ungated method
// would just make the test ignore real synchronous calls without proving
// anything.
var gatedIPCMethods = map[string]map[string]bool{
	"ProjectOps":   {"Create": true, "Update": true, "RotateToken": true},
	"McpOps":       {"Add": true, "Remove": true, "StartOAuth": true},
	"Ops":          {"Create": true, "Update": true, "Remove": true}, // ServiceOps
	"EnrolmentOps": {"Create": true, "Update": true, "Revoke": true},
	"LoginOps":     {"RevokePasskey": true},
}

func itsModuleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("ipc_thread_structural_test.go: could not determine this file's location")
	}
	root := filepath.Dir(file)
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("ipc_thread_structural_test.go: computed module root %q has no go.mod: %v", root, err)
	}
	return root
}

// itsIPCFiles returns every non-test ipc_*.go file directly in root -- the
// WKWebView IPC handler files onSettingsIpc's dispatch table (ipc_handlers.go)
// draws from.
func itsIPCFiles(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading %q: %v", root, err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "ipc_") || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		t.Fatal("ipc_thread_structural_test.go: found no ipc_*.go source files; the scan is misconfigured")
	}
	return out
}

type itsViolation struct {
	file string
	expr string
	line int
}

// itsVisitor walks one file's AST. ast.Walk threads visitor state down the
// tree for us: when Visit returns a NEW itsVisitor (with insideGoFunc set),
// that visitor -- and its insideGoFunc flag -- is what Walk uses to recurse
// into the node's children, so the flag applies only to the subtree under
// the "ctx.GoFunc(...)" call. A sibling statement outside that call's
// arguments is walked with the ORIGINAL visitor untouched, exactly the
// scoping ordinary lexical nesting has.
type itsVisitor struct {
	insideGoFunc bool
	fset         *token.FileSet
	file         string
	violations   *[]itsViolation
}

func (v *itsVisitor) Visit(n ast.Node) ast.Visitor {
	if n == nil {
		return nil
	}
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return v
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return v
	}
	// ctx.GoFunc(func() { ... }): everything inside the func literal
	// argument (and any call nested inside it, including a second GoFunc)
	// is off the main thread from here on.
	if sel.Sel.Name == "GoFunc" {
		return &itsVisitor{insideGoFunc: true, fset: v.fset, file: v.file, violations: v.violations}
	}
	if !v.insideGoFunc {
		if recv, ok := sel.X.(*ast.SelectorExpr); ok {
			if methods, known := gatedIPCMethods[recv.Sel.Name]; known && methods[sel.Sel.Name] {
				*v.violations = append(*v.violations, itsViolation{
					file: v.file,
					expr: recv.Sel.Name + "." + sel.Sel.Name,
					line: v.fset.Position(call.Pos()).Line,
				})
			}
		}
	}
	return v
}

func TestIPC_GatedCoreCallsRunOffTheMainThread(t *testing.T) {
	root := itsModuleRoot(t)
	fset := token.NewFileSet()
	var violations []itsViolation

	for _, name := range itsIPCFiles(t, root) {
		path := filepath.Join(root, name)
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		ast.Walk(&itsVisitor{fset: fset, file: name, violations: &violations}, f)
	}

	if len(violations) == 0 {
		return
	}
	sort.Slice(violations, func(i, j int) bool {
		if violations[i].file != violations[j].file {
			return violations[i].file < violations[j].file
		}
		return violations[i].line < violations[j].line
	})
	var sb strings.Builder
	sb.WriteString("a gated core call is reachable synchronously from the WKWebView main-thread IPC entry point (onSettingsIpc); Gate.Require would deadlock the tray the same way trayapp.go's showLoginCode doc comment describes. Wrap it in ctx.GoFunc, and DispatchToMain any UI touch that follows it:\n")
	for _, viol := range violations {
		fmt.Fprintf(&sb, "  %s:%d: ctx.%s(...)\n", viol.file, viol.line, viol.expr)
	}
	t.Fatal(sb.String())
}
