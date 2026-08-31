package main

// AC-15 (ADR-017 implementation spec §6.7): this file proves ONE property,
// and it is narrower than its name suggests -- mutation containment, not
// gate coverage. It parses every non-test .go file in package main and
// finds every call to store.With, store.WithDeclinable, withDeclinable, and
// to each mutator in the gated set (§6.7's own list). Every call site found
// must be in a file on gateAllowlistedFiles below; anything else fails the
// suite by name. That answers "a NEW route registered without the gate
// cannot reach a gated operation" -- it says nothing about whether the
// operation core it lands in actually calls the gate. That second property
// -- gate COVERAGE -- is proven separately, by
// TestGate_RequireGateCallSitesAreComplete below, derived from source via
// gate_ast_scan_test.go's scan rather than reviewed into this file's
// allowlist. Neither test stands in for the other: this one does not
// weaken when an op retires from the gated set (`McpOps.Remove` still calls
// `RemoveExternalMcp` from `mcp_ops.go`, still allowlisted, still the only
// door to that mutator); the other one is what would catch the retirement
// being wrong.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/presence"
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
	"project_ops.go": "the ProjectOps core: Gate.Require runs before Create/Update/RotateToken touch the store; " +
		"NarrowForEnrolment also calls applyProjectUpdate and withDeclinable, but is deliberately UNGATED (ADR-018 " +
		"decision 4) — grant_narrowing.go's narrowsOnly makes a widening unrepresentable before this file is ever " +
		"reached, so it is not one of the acts ADR-017 decision 3 gates, and gating a route a VM can reach would put " +
		"a presence prompt on the host's screen that the caller cannot see and the human did not ask for",
	"mcp_ops.go": "the McpOps core: Gate.Require runs before Add and StartOAuth touch the store; Remove is deliberately " +
		"ungated (ADR-018 step 3 -- removal narrows, re-registering under the same id still hits Add's gate) and still " +
		"calls requireIssuanceAuditor",
	"service_ops.go": "the ServiceOps core: Gate.Require runs before Create/Update touch the store; Remove is deliberately " +
		"ungated (ADR-018 step 3 -- removal narrows, and stopping a running service is already ungated configure via " +
		"POST /api/services/{id}/stop) and still calls requireIssuanceAuditor",
	"enrolment_ops.go": "the EnrolmentOps core: Gate.Require runs before Create/Update/Revoke touch the store; SetRemoteConfig's own With is a separate, ungated op",
	"login_ops.go":     "the LoginOps core: Gate.Require runs before MintBootstrap/RevokePasskey touch the store",

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
	return relaySourceDir(t)
}

// gsPackageMainFiles returns every non-test .go file directly in the relay
// command source directory. Its subpackages carry their own structural guards.
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

// wantGatedMutatorNames pins gatedMutatorNames' key set (§4.1's "does not
// shrink" rule, most pointedly for RemoveExternalMcp and RemoveService: a
// retired op's mutator stays listed, because the mutation-containment
// property this file proves does not depend on whether the op that reaches
// it still needs a prompt). A count floor is not enough here — dropping any
// ONE entry still leaves the set well above a loose minimum, and produces
// zero violations from TestGate_NoDoorReachesAGatedMutationOutsideItsCore
// today, since nothing outside the allowlisted cores currently calls any
// mutator by name. Pinning the literal set is what makes a silent drop
// visible.
var wantGatedMutatorNames = []string{
	"With", "WithDeclinable", "withDeclinable",
	"MintFor",
	"AddAPICredential", "RemoveAPICredential",
	"RotateProjectToken",
	"UpsertExternalMcp", "RemoveExternalMcp",
	"UpsertService", "RemoveService",
	"AddEnrolment", "RemoveEnrolment",
	"mintBootstrapCode",
	"applyProjectCreate", "applyProjectUpdate",
	"UpdateProjectAllowedTools", "UpdateProjectAccess", "UpdateProjectContext",
	"UpdateProjectMcps", "SetProjectAllowCwdAuth", "UpdateProjectAllowExternal",
	"UpdateProjectKind", "UpdateProjectPath",
}

// wantGateAllowlistedFiles pins gateAllowlistedFiles' key set the same way.
var wantGateAllowlistedFiles = []string{
	"credential_ops.go", "project_ops.go", "mcp_ops.go", "service_ops.go",
	"enrolment_ops.go", "login_ops.go",
	"settings.go", "settings_store.go", "api_credential.go", "enrolment.go", "project_apply.go",
	"project_routes.go", "ipc_handlers.go", "trayapp.go", "frontend_server.go", "login_routes.go",
}

// TestGate_MutatorAndAllowlistSetsHaveNotShrunk is AC-11: a
// mutation-containment guard that keeps passing while its two sets quietly
// shrink is worse than no guard, because "no violations found" stops
// meaning anything once there is nothing left to violate. Compares both
// maps' key sets to the pinned literals above, in both directions, so a
// drop and a stray addition are equally visible.
func TestGate_MutatorAndAllowlistSetsHaveNotShrunk(t *testing.T) {
	if n := len(gatedMutatorNames); n < 14 {
		t.Errorf("gatedMutatorNames has %d entries, want at least 14", n)
	}
	if n := len(gateAllowlistedFiles); n < 13 {
		t.Errorf("gateAllowlistedFiles has %d entries, want at least 13", n)
	}

	if len(gatedMutatorNames) != len(wantGatedMutatorNames) {
		t.Errorf("gatedMutatorNames has %d entries, wantGatedMutatorNames has %d", len(gatedMutatorNames), len(wantGatedMutatorNames))
	}
	for _, name := range wantGatedMutatorNames {
		if !gatedMutatorNames[name] {
			t.Errorf("wantGatedMutatorNames has %q, missing from gatedMutatorNames", name)
		}
	}
	for name := range gatedMutatorNames {
		if !containsString(wantGatedMutatorNames, name) {
			t.Errorf("gatedMutatorNames has %q, missing from wantGatedMutatorNames", name)
		}
	}

	if len(gateAllowlistedFiles) != len(wantGateAllowlistedFiles) {
		t.Errorf("gateAllowlistedFiles has %d entries, wantGateAllowlistedFiles has %d", len(gateAllowlistedFiles), len(wantGateAllowlistedFiles))
	}
	for _, name := range wantGateAllowlistedFiles {
		if _, ok := gateAllowlistedFiles[name]; !ok {
			t.Errorf("wantGateAllowlistedFiles has %q, missing from gateAllowlistedFiles", name)
		}
	}
	for name := range gateAllowlistedFiles {
		if !containsString(wantGateAllowlistedFiles, name) {
			t.Errorf("gateAllowlistedFiles has %q, missing from wantGateAllowlistedFiles", name)
		}
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// wantGatedOps pins presence.GatedOps itself (§4.1.3): a second, independent
// literal that must equal it element-wise, in the same order. A change to
// either the package's own list or this expectation then shows up as a diff
// a reviewer reads, rather than the two silently moving together.
var wantGatedOps = []string{
	"credential.mint",
	"credential.revoke",
	"enrolment.create",
	"enrolment.update",
	"enrolment.revoke",
	"enrolment.sign",
	"login.bootstrap.mint",
	"login.passkey.revoke",
	"mcp.register",
	"mcp.oauth.start",
	"service.register",
	"project.rotate_token",
	"project.grant",
	"sealed.reset",
}

func TestGate_GatedOpsMatchesPinnedList(t *testing.T) {
	if len(presence.GatedOps) != len(wantGatedOps) {
		t.Fatalf("presence.GatedOps has %d entries, wantGatedOps has %d", len(presence.GatedOps), len(wantGatedOps))
	}
	for i, op := range wantGatedOps {
		if presence.GatedOps[i] != op {
			t.Errorf("presence.GatedOps[%d] = %q, want %q", i, presence.GatedOps[i], op)
		}
	}
}

// wantGateCallSites pins the op -> {receiver.method} map every requireGate
// call site in the tree must produce (§4.1.2's fourth direction): the
// surviving gated set written down at the method level, not just the op
// level, so a future retirement is a diff to THIS map rather than a
// subtraction nothing notices. service.register and project.grant each
// have two call sites (Create and Update share one op, per §1.3's argument
// for why enrolment.update -- and by the same reasoning project.grant and
// service.register -- gate as a whole request rather than per field).
var wantGateCallSites = map[string][]string{
	"credential.mint":   {"CredentialOps.Mint"},
	"credential.revoke": {"CredentialOps.Revoke"},
	"enrolment.create":  {"EnrolmentOps.Create"},
	// Approve reuses this exact op and digest shape rather than adding
	// "enrolment.approve" — the second door into issuance ADR-018 forbids
	// (spec §3, §11.4) — so this op has two call sites, the same shape
	// service.register and project.grant already have below.
	"enrolment.sign":       {"EnrolmentOps.Approve", "EnrolmentOps.Sign"},
	"enrolment.update":     {"EnrolmentOps.Update"},
	"enrolment.revoke":     {"EnrolmentOps.Revoke"},
	"login.bootstrap.mint": {"LoginOps.MintBootstrap"},
	"login.passkey.revoke": {"LoginOps.RevokePasskey"},
	"mcp.register":         {"McpOps.Add"},
	"mcp.oauth.start":      {"McpOps.StartOAuth"},
	"service.register":     {"ServiceOps.Create", "ServiceOps.Update"},
	"project.rotate_token": {"ProjectOps.RotateToken"},
	"project.grant":        {"ProjectOps.Create", "ProjectOps.Update"},
	"sealed.reset":         {"resetSealedStore"},
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestGate_RequireGateCallSitesAreComplete is §4.1.2's gate-coverage guard,
// derived from source rather than reviewed into gate_structural_test.go's
// allowlist above (which proves a different property -- see this file's
// doc comment). It asserts all four directions at once:
//
//  1. every op in presence.GatedOps has at least one requireGate call site
//     (catches deleting the call while leaving the string);
//  2. every requireGate op literal is in presence.GatedOps (catches a typo
//     or an orphaned op -- ErrUnknownOp only catches these at runtime);
//  3. the op argument is always a string literal, never computed -- enforced
//     by scanRequireGateCallSites itself, which fails the suite by name on
//     anything else rather than skipping it;
//  4. the resulting op -> {receiver.method} map equals wantGateCallSites
//     exactly, so the surviving set is written down and any future
//     retirement is a two-place, reviewable diff rather than a silent
//     subtraction.
func TestGate_RequireGateCallSitesAreComplete(t *testing.T) {
	root := gsModuleRoot(t)
	sites := scanRequireGateCallSites(t, root)

	gotByOp := map[string][]string{}
	for _, s := range sites {
		gotByOp[s.op] = append(gotByOp[s.op], s.method)
	}
	for op := range gotByOp {
		sort.Strings(gotByOp[op])
	}

	gated := map[string]bool{}
	for _, op := range presence.GatedOps {
		gated[op] = true
	}

	// Direction 1.
	for op := range gated {
		if len(gotByOp[op]) == 0 {
			t.Errorf("presence.GatedOps has %q with no requireGate call site", op)
		}
	}
	// Direction 2.
	for op := range gotByOp {
		if !gated[op] {
			t.Errorf("requireGate is called with op %q, which is not in presence.GatedOps", op)
		}
	}

	// Direction 4.
	if len(gotByOp) != len(wantGateCallSites) {
		t.Fatalf("found requireGate call sites for %d op(s), wantGateCallSites has %d", len(gotByOp), len(wantGateCallSites))
	}
	for op, wantMethods := range wantGateCallSites {
		want := append([]string(nil), wantMethods...)
		sort.Strings(want)
		if got := gotByOp[op]; !equalStringSlices(got, want) {
			t.Errorf("requireGate call sites for %q = %v, want %v", op, got, want)
		}
	}
}
