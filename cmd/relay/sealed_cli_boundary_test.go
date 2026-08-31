package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cliEntryPoints names every command dispatch function main.go can reach
// with no bridge round trip in between — the set §5.3.3 requires be
// structurally unable to reach the keychain, since each of these processes
// carries relay's own code identity and would otherwise silently satisfy
// the ACL the tray depends on (§5.3.1, §5.3.2).
var cliEntryPoints = []string{
	"runCredentialCommand",
	"runEnrolCommand",
	"runLoginCommand",
	"runMcpCommand",
	"runServiceCommand",
	"runGrantCommand",
	"runAuditCommand",
	"runMcpExec",
	"runMcpOrServer",
}

// forbiddenCalls maps a bare identifier this test must never find reachable
// to the fully-qualified name reported when it is. sealed.NewKeychainKeyring
// is the only place a real key ever comes from; Sealer.Unseal and
// Secret.Reveal are the only two ways a sealed value's plaintext ever
// becomes available at all. None of the three has another definition
// anywhere in this module (confirmed by grep at the time this test was
// written), so a bare-name match is precise, not merely convenient.
var forbiddenCalls = map[string]string{
	"NewKeychainKeyring": "sealed.NewKeychainKeyring",
	"Unseal":             "Sealer.Unseal",
	"Reveal":             "Secret.Reveal",
}

// TestSeal_NoCLIPathReachesTheKeychain builds package main's call graph from
// source (go/ast, not go/types — see buildCLICallGraph for why that
// boundary is precise enough here) and fails, naming the entry point and
// the call chain, if any CLI entry point can reach sealed.NewKeychainKeyring,
// Sealer.Unseal, or Secret.Reveal. This is what makes §5.3.3's structural
// requirement real rather than a comment (AC-29): brokering (decision 2)
// is not merely a tidiness argument without it, because the CLI binary
// satisfies the keychain ACL by code identity — the only thing standing
// between it and a silent unlock is that no code path tries.
func TestSeal_NoCLIPathReachesTheKeychain(t *testing.T) {
	calls, hits := buildCLICallGraph(t)

	for _, entry := range cliEntryPoints {
		if _, ok := calls[entry]; !ok {
			t.Fatalf("entry point %q not found in package main — has it been renamed?", entry)
		}
		if chain, target := reachesForbidden(entry, calls, hits); target != "" {
			t.Errorf("%s can reach %s via %s", entry, target, strings.Join(chain, " -> "))
		}
	}
}

// buildCLICallGraph parses every non-test source file in package main and
// returns two things: calls, a graph of PLAIN unqualified call edges
// (func() — which in Go can only name a package-level function or a
// locally shadowed one, never a method whose receiver type this pass does
// not know); and hits, which function bodies directly call one of
// forbiddenCalls' names as EITHER a plain call or a qualified/method call
// (pkg.Func(...), x.Method(...)).
//
// This is deliberate: a method call is not added as a graph edge, because
// go/ast alone cannot tell which type's "Unseal" or "Reveal" a given
// x.Unseal() resolves to, and merging every method sharing that bare name
// into one graph node would let an unrelated method elsewhere manufacture
// a path that does not exist in the compiled program (a false positive
// this test would rather not risk trading against a real miss). What
// matters for AC-29 is only whether an entry point's own reachable PLAIN
// calls ever land in a function that itself performs one of these three
// specific calls — and hit-detection (unlike edge-traversal) works
// correctly on a method call by name alone, since none of the three names
// exists anywhere else in this module.
func buildCLICallGraph(t *testing.T) (calls map[string][]string, hits map[string]string) {
	t.Helper()
	calls = map[string][]string{}
	hits = map[string]string{}

	fset := token.NewFileSet()
	root := relaySourceDir(t)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(root, name)
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			fname := fd.Name.Name
			var edges []string
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fn := call.Fun.(type) {
				case *ast.Ident:
					edges = append(edges, fn.Name)
					if target, bad := forbiddenCalls[fn.Name]; bad {
						hits[fname] = target
					}
				case *ast.SelectorExpr:
					if target, bad := forbiddenCalls[fn.Sel.Name]; bad {
						hits[fname] = target
					}
				}
				return true
			})
			calls[fname] = append(calls[fname], edges...)
		}
	}
	return calls, hits
}

// reachesForbidden does a breadth-first search over calls starting at
// start, and returns the first chain reaching a function recorded in hits.
func reachesForbidden(start string, calls map[string][]string, hits map[string]string) (chain []string, target string) {
	if t, ok := hits[start]; ok {
		return []string{start}, t
	}
	type frame struct {
		name string
		path []string
	}
	seen := map[string]bool{start: true}
	queue := []frame{{start, []string{start}}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range calls[cur.name] {
			if seen[next] {
				continue
			}
			seen[next] = true
			path := append(append([]string{}, cur.path...), next)
			if t, ok := hits[next]; ok {
				return path, t
			}
			queue = append(queue, frame{next, path})
		}
	}
	return nil, ""
}
