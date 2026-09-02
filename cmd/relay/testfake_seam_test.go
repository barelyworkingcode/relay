package main

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A package that fakes a security boundary for tests must never be reachable
// from a shipped binary. Each one guards itself with an init that panics
// outside a test binary; this proves the stronger property, that nothing
// links them at all, because Go links only what is imported.
//
// The set is DISCOVERED from that init guard rather than listed. A named
// list is the wrong shape here: the fake it forgets is the one that ships,
// and it would report success while doing so.
func tfsModuleRoot(t *testing.T) string {
	t.Helper()
	return repoRoot(t)
}

// tfsTestOnlyPackages returns the import path of every package under internal
// whose non-test sources refuse to run outside a test binary.
func tfsTestOnlyPackages(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if !strings.Contains(string(src), "testing.Testing()") || !strings.Contains(string(src), "must never ship") {
			return nil
		}
		rel, relErr := filepath.Rel(root, filepath.Dir(path))
		if relErr != nil {
			return relErr
		}
		imp := "github.com/barelyworkingcode/relay/" + filepath.ToSlash(rel)
		for _, seen := range out {
			if seen == imp {
				return nil
			}
		}
		out = append(out, imp)
		return nil
	})
	if err != nil {
		t.Fatalf("walking internal/: %v", err)
	}
	return out
}

func TestTestOnlyFakesAreNotLinkedIntoTheBinary(t *testing.T) {
	root := tfsModuleRoot(t)
	fakes := tfsTestOnlyPackages(t, root)

	// A guard that found nothing would pass forever. Both known fakes are
	// named here so that deleting the marker from one is a failure rather
	// than a silent exemption.
	if len(fakes) < 2 {
		t.Fatalf("discovered %d test-only package(s), want at least the presence and login fakes: %v", len(fakes), fakes)
	}
	for _, want := range []string{
		"github.com/barelyworkingcode/relay/internal/presence/presencetest",
		"github.com/barelyworkingcode/relay/internal/login/loginfake",
	} {
		if !slicesContains(fakes, want) {
			t.Errorf("%s no longer marks itself test-only; its init guard is what this scan finds it by", want)
		}
	}

	fset := token.NewFileSet()
	for _, path := range nonTestGoFiles(t, root) {
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly|parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			for _, fake := range fakes {
				if p == fake {
					rel, _ := filepath.Rel(root, path)
					t.Errorf("%s imports %s, which must never reach a shipped binary", rel, fake)
				}
			}
		}
	}
}

func slicesContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func nonTestGoFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "dist" || d.Name() == "web" {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking module: %v", err)
	}
	return out
}
