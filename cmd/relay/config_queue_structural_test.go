package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// queueScanAllowlist is the complete set of production functions permitted to
// write settings without naming the command queue themselves. Grown only by a
// change that says why here.
var queueScanAllowlist = map[string]string{
	"trayapp.go:runTrayApp":                                   "startup: settings initialisation before the queue serves anything",
	"api_credential.go:retireLegacyFrontendCredentialOnStart": "startup: retires a reserved credential before the queue serves anything",
	"api_credential.go:mintAPICredential":                     "helper the caller runs inside its own queued step (CredentialOps.Mint)",
	"api_credential.go:revokeAPICredentialIf":                 "helper the caller runs inside its own queued step (CredentialOps.Revoke, LoginOps.SignOut)",
	"login_ops.go:mintLoginBootstrap":                         "helper LoginOps.MintBootstrap runs inside its queued step",
	"login_ops.go:revokePasskey":                              "helper LoginOps.RevokePasskey and RemoveUnrecordedPasskey run inside their queued steps",
	"service_ops.go:(*ServiceOps).commitCreate":               "commit half of Create/Register, called from inside the queued step",
	"service_ops.go:(*ServiceOps).commitUpdate":               "commit half of Update/Register, called from inside the queued step",
	"service_ops.go:(*ServiceOps).remove":                     "body of Remove, called from inside the queued step",
	"service_ops.go:(*ServiceOps).setAutostart":               "body of SetAutostart, called from inside the queued step",
}

// queueScanKnownQueued are call sites that must be found and accepted as
// queued, so an empty or mis-aimed scan cannot pass.
var queueScanKnownQueued = []string{
	"login_ops.go:(*LoginOps).RegisterPasskey",
	"login_ops.go:(*LoginOps).MintLoginSession",
	"project_ops.go:(*ProjectOps).SetDisabledTools",
	"mcp_ops.go:(*McpOps).PersistOAuthState",
	"template_ops.go:(*TemplateOps).put",
}

type storeWrite struct {
	key    string // file:function
	queued bool
}

func funcDisplayName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	switch t := fn.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return "(*" + id.Name + ")." + fn.Name.Name
		}
	case *ast.Ident:
		return "(" + t.Name + ")." + fn.Name.Name
	}
	return fn.Name.Name
}

// isStoreWrite matches config.WithDeclinable(...) and X.With(...) where X is
// the store: an identifier or field named store/Store.
func isStoreWrite(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if id, ok := sel.X.(*ast.Ident); ok && id.Name == "config" && sel.Sel.Name == "WithDeclinable" {
		return true
	}
	if sel.Sel.Name != "With" {
		return false
	}
	switch x := sel.X.(type) {
	case *ast.Ident:
		return strings.EqualFold(x.Name, "store")
	case *ast.SelectorExpr:
		return strings.EqualFold(x.Sel.Name, "store")
	}
	return false
}

// namesTheQueue reports whether the function body submits to the queue: a
// call to runQueued/runCommitted, or Do/DoCommitted on a Queue field. Merely
// mentioning a Queue (wiring it into a struct) does not count.
func namesTheQueue(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return !found
		}
		switch f := call.Fun.(type) {
		case *ast.Ident:
			found = f.Name == "runQueued" || f.Name == "runCommitted"
		case *ast.SelectorExpr:
			found = f.Sel.Name == "runQueued" || f.Sel.Name == "runCommitted" ||
				((f.Sel.Name == "Do" || f.Sel.Name == "DoCommitted") && selectorEndsIn(f.X, "Queue"))
		}
		return !found
	})
	return found
}

func selectorEndsIn(x ast.Expr, name string) bool {
	sel, ok := x.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == name
}

func scanStoreWrites(t *testing.T) []storeWrite {
	t.Helper()
	dir := relaySourceDir(t)
	entries, err := os.ReadDir(dir)
	assertNoErr(t, err, "read cmd/relay")
	var out []storeWrite
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		assertNoErr(t, err, "parse "+name)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			writes := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && isStoreWrite(call) {
					writes = true
				}
				return !writes
			})
			if writes {
				out = append(out, storeWrite{key: name + ":" + funcDisplayName(fn), queued: namesTheQueue(fn)})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

func TestProductionSettingsWritesNameTheCommandQueue(t *testing.T) {
	writes := scanStoreWrites(t)
	byKey := map[string]storeWrite{}
	for _, w := range writes {
		byKey[w.key] = w
	}
	for _, want := range queueScanKnownQueued {
		w, ok := byKey[want]
		if !ok || !w.queued {
			t.Fatalf("the scan no longer finds %s as a queued settings write (found=%v); it is aimed at the wrong thing", want, ok)
		}
	}
	for _, w := range writes {
		if w.queued {
			continue
		}
		if _, ok := queueScanAllowlist[w.key]; !ok {
			t.Errorf("%s writes settings outside the command queue; run it through an Ops core's runQueued or allowlist it with a reason", w.key)
		}
	}
	for key := range queueScanAllowlist {
		if w, ok := byKey[key]; !ok || w.queued {
			t.Errorf("allowlist entry %s no longer matches an unqueued settings write; remove it", key)
		}
	}
}
