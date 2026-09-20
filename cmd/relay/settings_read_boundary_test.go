package main

import (
	"github.com/barelyworkingcode/relay/internal/config"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Production code reads settings through config.FreshSettings (any decision)
// or config.DisplaySettings (rendering), and only the tray's startup builds a
// store. The health poll reads no settings at all. This guard matches source text, so a rename can leave it scanning
// nothing; settingsBoundaryKnownFindings proves it still finds the paths it
// exists to police.

type settingsAccess struct {
	key  string // file:function:kind
	kind string // "read" or "construct"
}

// settingsAccessAllowlist is the complete set of production functions that
// may touch a store directly. Grown only by a change that says why here.
var settingsAccessAllowlist = map[string]string{
	"cmd/relay/trayapp.go:runTrayApp:read":      "startup: the boot snapshot and audit recorder are taken before the queue or any listener serves",
	"cmd/relay/trayapp.go:runTrayApp:construct": "the one production store construction; the ownership lock is already held",
}

// settingsBoundaryKnownFindings must each be found, so an empty or mis-aimed
// scan cannot pass.
var settingsBoundaryKnownFindings = []string{
	"cmd/relay/trayapp.go:runTrayApp:read",
	"cmd/relay/trayapp.go:runTrayApp:construct",
}

var storeReadMethods = map[string]bool{"Get": true, "Reload": true, "ReloadIfChanged": true}

var storeConstructors = map[string]bool{
	"NewSettingsStore": true, "NewSettingsStoreAt": true, "NewSettingsStoreSealed": true,
	"NewSettingsStoreWithCache": true, "NewSettingsStoreDegraded": true, "ResolveSealedStore": true,
}

func isStoreReceiver(x ast.Expr) bool {
	name := ""
	switch v := x.(type) {
	case *ast.Ident:
		name = v.Name
	case *ast.SelectorExpr:
		name = v.Sel.Name
	}
	return strings.HasSuffix(strings.ToLower(name), "store")
}

func scanSettingsAccess(fset *token.FileSet, file *ast.File, rel string) []settingsAccess {
	var out []settingsAccess
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		seen := map[string]bool{}
		add := func(kind string) {
			key := rel + ":" + funcDisplayName(fn) + ":" + kind
			if !seen[key] {
				seen[key] = true
				out = append(out, settingsAccess{key: key, kind: kind})
			}
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CallExpr:
				sel, ok := v.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if storeReadMethods[sel.Sel.Name] && len(v.Args) == 0 && isStoreReceiver(sel.X) {
					add("read")
				}
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "config" && storeConstructors[sel.Sel.Name] {
					add("construct")
				}
			case *ast.CompositeLit:
				if sel, ok := v.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "FileSettingsStore" {
					add("construct")
				}
			}
			return true
		})
	}
	return out
}

func scanProductionSettingsAccess(t *testing.T) []settingsAccess {
	t.Helper()
	root := repoRoot(t)
	var out []settingsAccess
	fset := token.NewFileSet()
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				if rel == "internal/config" {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			out = append(out, scanSettingsAccess(fset, file, rel)...)
			return nil
		})
		assertNoErr(t, err, "walk "+top)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

func TestProductionSettingsReadsAndConstructionStayBehindTheBoundary(t *testing.T) {
	found := map[string]bool{}
	for _, a := range scanProductionSettingsAccess(t) {
		found[a.key] = true
		if _, ok := settingsAccessAllowlist[a.key]; !ok {
			t.Errorf("%s reaches a settings store directly; read through config.FreshSettings (decisions) or config.DisplaySettings (rendering), and construct stores only at tray startup, or allowlist it with a reason", a.key)
		}
	}
	for _, want := range settingsBoundaryKnownFindings {
		if !found[want] {
			t.Errorf("the scan no longer finds %s; it is aimed at the wrong thing", want)
		}
	}
	for key := range settingsAccessAllowlist {
		if !found[key] {
			t.Errorf("allowlist entry %s no longer matches a direct store access; remove it", key)
		}
	}
}

func TestSettingsBoundaryScanBitesOnEachViolation(t *testing.T) {
	const src = `package p
import "x/config"
func readsCached(o *O) { _ = o.Store.Get() }
func reloads(r *R) { _ = r.store.Reload() }
func polls(a *A) { _ = a.store.ReloadIfChanged() }
func builds() { _ = config.NewSettingsStoreAt("d") }
func resolves() { _, _ = config.ResolveSealedStore("d", nil) }
func literal() { _ = &config.FileSettingsStore{} }
func clean(o *O) { _ = config.FreshSettings(o.Store); _ = o.sessions.Get() }
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p.go", src, 0)
	assertNoErr(t, err, "parse fixture")
	got := map[string]bool{}
	for _, a := range scanSettingsAccess(fset, file, "p.go") {
		got[a.key] = true
	}
	for _, want := range []string{
		"p.go:readsCached:read", "p.go:reloads:read", "p.go:polls:read",
		"p.go:builds:construct", "p.go:resolves:construct", "p.go:literal:construct",
	} {
		if !got[want] {
			t.Errorf("scan missed %s", want)
		}
	}
	if len(got) != 6 {
		t.Errorf("scan reported %v, want exactly the six violations", got)
	}
}

func TestTrayOwnershipIsAcquiredBeforeStoreAndBridgeSetup(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(relaySourceDir(t), "trayapp.go"), nil, 0)
	assertNoErr(t, err, "parse trayapp.go")
	pos := map[string]token.Pos{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "runTrayApp" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					if _, seen := pos[sel.Sel.Name]; !seen {
						pos[sel.Sel.Name] = call.Pos()
					}
				}
			}
			return true
		})
	}
	lock, ok := pos["AcquireTrayOwnership"]
	if !ok {
		t.Fatal("runTrayApp no longer calls AcquireTrayOwnership")
	}
	for _, later := range []string{"ResolveSealedStore", "NewBridgeServer"} {
		p, ok := pos[later]
		if !ok {
			t.Fatalf("runTrayApp no longer calls %s; the ordering guard is aimed at the wrong thing", later)
		}
		if lock > p {
			t.Errorf("AcquireTrayOwnership must precede %s in runTrayApp", later)
		}
	}
}

func TestDecisionReadsSeeACommittedChangeWithoutWaitingForThePoll(t *testing.T) {
	_ = mkSandboxRelayHome(t)
	dir := mkEmptySandboxRelayHome(t)
	reader := sealedSettingsStoreAt(dir)
	assertNoErr(t, reader.EnsureInitialized(), "initialize")
	assertNoErr(t, reader.With(func(s *config.Settings) {
		s.ExternalMcps = []config.ExternalMcp{{ID: "fsmcp", DisplayName: "fsMCP"}}
	}), "seed mcp")
	proj := createTestProject(t, reader, "Narrowed", t.TempDir(), []string{"fsmcp"})
	if !modelAllowedForProject(reader, proj.ID, "opus") {
		t.Fatal("precondition: a wildcard project allows opus")
	}

	// A second store stands in for any writer that is not the reader's own
	// cache; the reader's cache still holds the wildcard grant.
	writer := sealedSettingsStoreAt(dir)
	assertNoErr(t, writer.With(func(s *config.Settings) {
		s.UpdateProjectModels(proj.ID, []string{"haiku"})
	}), "narrow models")

	if modelAllowedForProject(reader, proj.ID, "opus") {
		t.Error("the model guard decided from a stale cache: opus is no longer allowed")
	}
}
