// Package sessions holds no code of its own — this file is a repo-wide
// guard, not a unit under test.
package sessions

import (
	"os/exec"
	"strings"
	"testing"
)

// forbiddenPrefix is what internal/sessions must never depend on. Once
// relay-sessions exists as its own binary, internal/sessions is the library
// it (and only it) imports; a dependency back on cmd/relay would mean the
// session host secretly needs the very process it is meant to be hosted
// apart from — a real architectural inversion, not a style nit.
const forbiddenPrefix = "github.com/barelyworkingcode/relay/cmd/relay"

// TestNoImportOfCmdRelay walks the actual build graph (go list -deps),
// including test-only imports, for every package under internal/sessions.
// It asserts against what the Go toolchain resolved, not a grep of import
// statements, so it keeps catching a violation introduced through an
// indirect dependency as later P2 units land code in this tree.
func TestNoImportOfCmdRelay(t *testing.T) {
	out, err := exec.Command("go", "list", "-test", "-deps", "./...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps ./...: %v\n%s", err, out)
	}

	for _, pkg := range strings.Fields(string(out)) {
		if pkg == forbiddenPrefix || strings.HasPrefix(pkg, forbiddenPrefix+"/") {
			t.Fatalf("internal/sessions/... depends on %s; internal/sessions must never import cmd/relay", pkg)
		}
	}
}
