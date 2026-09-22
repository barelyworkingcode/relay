package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// The attach path has to survive a machine sleeping under an attached
// terminal, and relay has no injectable clock on it, so the property is pinned
// on the source: none of these identifiers may appear in the code that pumps
// bytes. Comments are not scanned; the AST is.
var (
	// Forbidden as a method on any receiver: they arm a connection deadline or
	// a keepalive.
	forbiddenMethods = map[string]bool{
		"SetDeadline": true, "SetReadDeadline": true, "SetWriteDeadline": true,
		"SetPongHandler": true, "SetPingHandler": true, "WriteControl": true,
	}
	// Forbidden as package-qualified calls. context.AfterFunc is not among
	// them: it fires on cancellation, not on a clock.
	forbiddenQualified = map[string]map[string]bool{
		"time":    {"After": true, "AfterFunc": true, "NewTimer": true, "NewTicker": true, "Tick": true, "Sleep": true},
		"context": {"WithTimeout": true, "WithDeadline": true, "WithTimeoutCause": true, "WithDeadlineCause": true},
	}
)

func scanForWallClock(t *testing.T, path, onlyFunc string) (scanned int) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	visit := func(n ast.Node) {
		ast.Inspect(n, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			scanned++
			pkg := ""
			if id, ok := sel.X.(*ast.Ident); ok {
				pkg = id.Name
			}
			if forbiddenMethods[sel.Sel.Name] || forbiddenQualified[pkg][sel.Sel.Name] {
				t.Errorf("%s: %s uses %s; the attach path must carry no deadline or timer", path, fset.Position(sel.Pos()), sel.Sel.Name)
			}
			return true
		})
	}
	if onlyFunc == "" {
		visit(file)
		return scanned
	}
	found := false
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == onlyFunc {
			found = true
			visit(fd)
		}
	}
	if !found {
		t.Fatalf("%s has no func %s: the guard scans nothing", path, onlyFunc)
	}
	return scanned
}

func TestSandboxAttachPath_ContainsNoDeadlineOrWallClockTimer(t *testing.T) {
	// A ratchet against an empty scan: each file must contribute selectors.
	for _, path := range []string{
		"sandbox_attach.go",
		"sandbox_cmd.go",
		"../../internal/bridge/sandbox.go",
	} {
		if n := scanForWallClock(t, path, ""); n == 0 {
			t.Errorf("%s: scan saw no selector expressions; the guard is not looking at anything", path)
		}
	}
	// DialWS may bound the upgrade handshake (HandshakeTimeout is a field, not
	// a selector) and nothing else.
	if n := scanForWallClock(t, "sessionhost_client.go", "DialWS"); n == 0 {
		t.Error("DialWS scan saw nothing")
	}
}
