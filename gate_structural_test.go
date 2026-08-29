package main

// AC-15 (ADR-017 implementation spec §6.7): the gate lives inside each
// operation core, never on a door, so a door that reaches around a core
// cannot invoke a gated operation even by accident. This file is the
// build-time proof: it parses every non-test .go file in package main and
// finds every call to store.With, store.WithDeclinable, withDeclinable, and
// to each mutator in the gated set (§6.7's own list). Every call site found
// must be in a file on gateAllowlistedFiles below; anything else fails the
// suite by name, which is the property that answers "a NEW route
// registered without the gate cannot reach a gated operation."

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

// gatedMutatorNames is §6.7's list verbatim, plus the generic settings-write
// entry points (With, WithDeclinable, withDeclinable) every mutation goes
// through. Matching is by identifier name, not by resolved type — go/ast
// alone cannot tell a *Settings receiver from an unrelated one — which is
// why the allowlist below also has to admit files that call these SAME
// names for a reason unconnected to a gated operation (the WebAuthn login
// ceremony's own MintFor, the frontend-token migration's own With, and so
// on): the test's job is to make every call site visible and reviewed, not
// to prove each one is a gated act.
var gatedMutatorNames = map[string]bool{
	"With":                       true,
	"WithDeclinable":             true,
	"withDeclinable":             true,
	"MintFor":                    true,
	"AddAPICredential":           true,
	"RemoveAPICredential":        true,
	"RotateProjectToken":         true,
	"UpsertExternalMcp":          true,
	"RemoveExternalMcp":          true,
	"UpsertService":              true,
	"RemoveService":              true,
	"AddEnrolment":               true,
	"RemoveEnrolment":            true,
	"mintBootstrapCode":          true,
	"applyProjectCreate":         true,
	"applyProjectUpdate":         true,
	"UpdateProjectAllowedTools":  true,
	"UpdateProjectAccess":        true,
	"UpdateProjectContext":       true,
	"UpdateProjectMcps":          true,
	"SetProjectAllowCwdAuth":     true,
	"UpdateProjectAllowExternal": true,
	"UpdateProjectKind":          true,
	"UpdateProjectPath":          true,
}

// gateAllowlistedFiles is the exact, reviewed set of files permitted to call
// a name in gatedMutatorNames. Grown only by a change that says why in the
// same diff — that is what keeps this a check rather than ceremony.
var gateAllowlistedFiles = map[string]string{
	// The six gated cores (ADR-017 implementation spec S5): each holds a
	// presence.Gate field and calls Require before touching the store.
	"credential_ops.go": "the CredentialOps core: Gate.Require runs before Mint/Revoke touch the store",
	"project_ops.go":    "the ProjectOps core: Gate.Require runs before Create/Update/RotateToken touch the store",
	"mcp_ops.go":        "the McpOps core: Gate.Require runs before Add/Remove/StartOAuth touch the store",
	"service_ops.go":    "the ServiceOps core: Gate.Require runs before Create/Update/Remove touch the store",
	"enrolment_ops.go":  "the EnrolmentOps core: Gate.Require runs before Create/Update/Revoke touch the store; SetRemoteConfig's own With is a separate, ungated op",
	"login_ops.go":      "the LoginOps core: Gate.Require runs before MintBootstrap/RevokePasskey touch the store",

	// Where the mutators themselves, and the free functions a core
	// delegates to, are defined.
	"settings.go":       "defines every s.* mutator method the cores and the free functions below call",
	"settings_store.go": "defines With / WithDeclinable / withDeclinable themselves",
	"api_credential.go": "defines MintFor, AddAPICredential, RemoveAPICredential, and mintAPICredential/revokeAPICredentialIf (the store.With they run inside), which CredentialOps.Mint/Revoke call after the gate",
	"enrolment.go":      "defines createEnrolment/updateEnrolment/revokeEnrolment and calls AddEnrolment/RemoveEnrolment/withDeclinable from inside them",
	"project_apply.go":  "defines applyProjectCreate/applyProjectUpdate, which call the UpdateProject* grant-shape mutators as their own sub-mutations",

	// Legitimately ungated mutations that share a name with a gated
	// mutator (§6.7's matching is by identifier, not by resolved type):
	// none of these are project.grant, credential.mint or any other op in
	// presence.GatedOps.
	"project_routes.go":  "DELETE /api/projects is not gated (deleting a project is not in presence.GatedOps); create/update/rotate_token go through ops.Create/Update/RotateToken, not store.With, directly",
	"ipc_handlers.go":    "withSettings/withSettingsNotify are the generic IPC mutation helper every ungated IPC handler (autostart toggle, disabled_tools, remote config, ...) shares",
	"trayapp.go":         "the frontend-token migration's one-time store.With call; not a gated op",
	"frontend_server.go": "ensureFrontendTokenIsCredential's withDeclinable call: the same frontend-token migration as trayapp.go's, run from NewFrontendServer's own setup path; not a gated op",
	"login_routes.go":    "the WebAuthn ceremony's own MintFor (a signed assertion is a different presence factor from this gate) and withDeclinable (POST /relay/login/verify is unauthenticated by design, ADR-016 decision 5)",

	// S6 brokered every mutating CLI command over admin_op (ADR-017
	// implementation spec §7): credential_cmd.go, mcp_cmd.go, service_cmd.go
	// and cli_helpers.go no longer call a gated mutator or store.With at
	// all — mintAPICredential/revokeAPICredentialIf moved to
	// api_credential.go, and upsertAndPrint/resolveAndRemove (the
	// store.With wrappers mcp_cmd.go and service_cmd.go called directly)
	// were deleted along with the direct-mutation code paths they served.
	// The four interim entries this comment used to document are gone;
	// do not re-add them.
}

func gsModuleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("gate_structural_test.go: could not determine this file's location")
	}
	root := filepath.Dir(file)
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("gate_structural_test.go: computed module root %q has no go.mod: %v", root, err)
	}
	return root
}

// gsPackageMainFiles returns every non-test .go file directly in root
// (package main lives at the module root; its subpackages -- bridge,
// sealed, presence, mcp, jsonrpc, cmd/*, web/gen -- are excluded because
// they are not package main and carry their own structural guards).
func gsPackageMainFiles(t *testing.T, root string) []string {
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

type gsViolation struct {
	file string
	name string
	line int
}

// TestGate_NoDoorReachesAGatedMutationOutsideItsCore is AC-15.
func TestGate_NoDoorReachesAGatedMutationOutsideItsCore(t *testing.T) {
	root := gsModuleRoot(t)
	fset := token.NewFileSet()
	var violations []gsViolation

	for _, name := range gsPackageMainFiles(t, root) {
		path := filepath.Join(root, name)
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var callee string
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				callee = fn.Name
			case *ast.SelectorExpr:
				callee = fn.Sel.Name
			default:
				return true
			}
			if !gatedMutatorNames[callee] {
				return true
			}
			if _, ok := gateAllowlistedFiles[name]; ok {
				return true
			}
			violations = append(violations, gsViolation{
				file: name,
				name: callee,
				line: fset.Position(call.Pos()).Line,
			})
			return true
		})
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
	fmt.Fprintf(&sb, "%d call site(s) reach a gated mutation outside an allowlisted file:\n", len(violations))
	for _, v := range violations {
		fmt.Fprintf(&sb, "  %s:%d calls %s\n", v.file, v.line, v.name)
	}
	sb.WriteString("a new route, IPC handler or bridge handler that reaches around a gated core must go through that core instead; if this call site is a deliberate, reviewed exception, add it to gateAllowlistedFiles and say why")
	t.Fatal(sb.String())
}

// TestGate_AllowlistNamesOnlyRealFiles catches the allowlist itself
// drifting from the tree: a stale entry (a file renamed or removed) would
// otherwise silently stop protecting anything.
func TestGate_AllowlistNamesOnlyRealFiles(t *testing.T) {
	root := gsModuleRoot(t)
	for name, reason := range gateAllowlistedFiles {
		if reason == "" {
			// A documentation-only key (see the comment beside SetRemoteConfig
			// above); it names no file to stat.
			continue
		}
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("gateAllowlistedFiles names %q, which does not exist: %v", name, err)
		}
	}
}
