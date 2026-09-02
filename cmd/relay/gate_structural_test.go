package main

// AC-15 (ADR-017 implementation spec §6.7): this file proves ONE property,
// and it is narrower than its name suggests -- mutation containment, not
// gate coverage. It parses every non-test .go file in each directory listed
// in gsScannedDirs and finds every call to store.With, store.WithDeclinable, withDeclinable, and
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
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/presence"
)

// gatedMutatorNames is §6.7's list verbatim, plus the generic settings-write
// entry points (With, WithDeclinable) every mutation goes
// through. Matching is by identifier name, not by resolved type — go/ast
// alone cannot tell a *Settings receiver from an unrelated one — which is
// why the allowlist below also has to admit files that call these SAME
// names for a reason unconnected to a gated operation (the WebAuthn login
// ceremony's own mintAPICredentialFor, the frontend-token migration's own With, and so
// on): the test's job is to make every call site visible and reviewed, not
// to prove each one is a gated act.
var gatedMutatorNames = map[string]bool{
	"With":                       true,
	"WithDeclinable":             true,
	"mintAPICredentialFor":       true,
	"addAPICredential":           true,
	"removeAPICredential":        true,
	"RotateProjectToken":         true,
	"UpsertExternalMcp":          true,
	"RemoveExternalMcp":          true,
	"UpsertService":              true,
	"RemoveService":              true,
	"addEnrolment":               true,
	"removeEnrolment":            true,
	"mintBootstrapCode":          true,
	"ApplyCreate":                true,
	"ApplyUpdate":                true,
	"UpdateProjectAllowedTools":  true,
	"UpdateProjectAccess":        true,
	"updateProjectContext":       true,
	"updateProjectMcps":          true,
	"SetProjectAllowCwdAuth":     true,
	"UpdateProjectAllowExternal": true,
	"updateProjectKind":          true,
	"updateProjectPath":          true,
}

// gateAllowlistedFiles is the exact, reviewed set of files permitted to call
// a name in gatedMutatorNames. Grown only by a change that says why in the
// same diff — that is what keeps this a check rather than ceremony.
var gateAllowlistedFiles = map[string]string{
	// The six gated cores (ADR-017 implementation spec S5): each holds a
	// presence.Gate field and calls Require before touching the store.
	"cmd/relay/credential_ops.go": "the CredentialOps core: Gate.Require runs before Mint/Revoke touch the store",
	"cmd/relay/project_ops.go": "the ProjectOps core: Gate.Require runs before Create/Update/RotateToken touch the store; " +
		"NarrowForEnrolment also calls project.ApplyUpdate and withDeclinable, but is deliberately UNGATED (ADR-018 " +
		"decision 4) — internal/project/narrowing.go's NarrowsOnly makes a widening unrepresentable before this file is ever " +
		"reached, so it is not one of the acts ADR-017 decision 3 gates, and gating a route a VM can reach would put " +
		"a presence prompt on the host's screen that the caller cannot see and the human did not ask for",
	"cmd/relay/mcp_ops.go": "the McpOps core: Gate.Require runs before Add and StartOAuth touch the store; Remove is deliberately " +
		"ungated (ADR-018 step 3 -- removal narrows, re-registering under the same id still hits Add's gate) and still " +
		"calls requireIssuanceAuditor",
	"cmd/relay/service_ops.go": "the ServiceOps core: Gate.Require runs before Create/Update touch the store; Remove is deliberately " +
		"ungated (ADR-018 step 3 -- removal narrows, and stopping a running service is already ungated configure via " +
		"POST /api/services/{id}/stop) and still calls requireIssuanceAuditor",
	"cmd/relay/enrolment_ops.go": "the EnrolmentOps core: Gate.Require runs before Create/Update/Revoke touch the store; SetRemoteConfig's own With is a separate, ungated op",
	"cmd/relay/login_ops.go":     "the LoginOps core: Gate.Require runs before MintBootstrap/RevokePasskey touch the store",

	// Where the mutators themselves, and the free functions a core
	// delegates to, are defined.
	// The persisted mutators themselves (With, WithDeclinable, and the
	// s.* methods) are declared in internal/config, which this scan does
	// not read: package main can only reach them through that package's
	// exported surface, so every crossing still appears here as a call
	// site in one of the files below.
	"internal/project/scope.go":       "defines the updateProject* grant-shape mutators; they are unexported there, so internal/project/apply.go below is the only file that can reach them at all",
	"cmd/relay/api_credential.go":     "defines mintAPICredentialFor, addAPICredential, removeAPICredential, and mintAPICredential/revokeAPICredentialIf (the store.With they run inside), which CredentialOps.Mint/Revoke call after the gate",
	"internal/enrolment/enrolment.go": "defines the package's Create/Update/Revoke and calls addEnrolment/removeEnrolment/config.WithDeclinable from inside them",
	"internal/project/apply.go":       "defines ApplyCreate/ApplyUpdate, the pair ProjectOps calls after the gate; they call the updateProject* grant-shape mutators and the config.Settings UpdateProject* methods as their own sub-mutations",

	// Legitimately ungated mutations that share a name with a gated
	// mutator (§6.7's matching is by identifier, not by resolved type):
	// none of these are project.grant, credential.mint or any other op in
	// presence.GatedOps.
	"cmd/relay/project_routes.go":  "DELETE /api/projects is not gated (deleting a project is not in presence.GatedOps); create/update/rotate_token go through ops.Create/Update/RotateToken, not store.With, directly",
	"cmd/relay/ipc_handlers.go":    "withSettings/withSettingsNotify are the generic IPC mutation helper every ungated IPC handler (autostart toggle, disabled_tools, remote config, ...) shares",
	"cmd/relay/trayapp.go":         "the frontend-token migration's one-time store.With call; not a gated op",
	"cmd/relay/frontend_server.go": "ensureFrontendTokenIsCredential's config.WithDeclinable call: the same frontend-token migration as trayapp.go's, run from NewFrontendServer's own setup path; not a gated op",
	"cmd/relay/login_routes.go":    "the WebAuthn ceremony's own mintAPICredentialFor (a signed assertion is a different presence factor from this gate) and config.WithDeclinable (POST /relay/login/verify is unauthenticated by design, ADR-016 decision 5)",

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
	return repoRoot(t)
}

// gsScannedDirs are the directories this scan walks, module-root-relative,
// which is also how gateAllowlistedFiles is keyed.
//
// This is DISCOVERED, not listed: cmd/relay plus every package under
// internal. A hand-maintained list is the wrong shape for this guard --
// anything it forgets reports zero violations forever, which reads exactly
// like compliance. Any package that can import config can call a gated
// mutator, so the default has to be that a new package is covered.
//
// internal/config is the one exclusion: it DECLARES the mutators, so its own
// calls to them are the definitions every other package is being checked
// against, not a door into them.
func gsScannedDirs(t *testing.T, root string) []string {
	t.Helper()
	dirs := []string{filepath.Join("cmd", "relay")}
	internalRoot := filepath.Join(root, "internal")
	err := filepath.WalkDir(internalRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() == "testdata" {
			// Fixture trees (build scripts, planted files) are not package
			// directories and carry no .go sources at all; including one
			// would trip the sanity check below, not extend the scan.
			return fs.SkipDir
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if rel == filepath.Join("internal", "config") {
			return fs.SkipDir
		}
		if path != internalRoot {
			dirs = append(dirs, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking internal/: %v", err)
	}
	return dirs
}

// gsScannedFiles returns every non-test .go file directly in each scanned
// directory, named by its path relative to the module root.
func gsScannedFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	for _, rel := range gsScannedDirs(t, root) {
		dir := filepath.Join(root, rel)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading %q: %v", dir, err)
		}
		found := 0
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			out = append(out, filepath.Join(rel, name))
			found++
		}
		if found == 0 {
			t.Fatalf("found no source files under %q; the scan is misconfigured", dir)
		}
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

	for _, name := range gsScannedFiles(t, root) {
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
//
// Case is load-bearing here. A mutator declared as a package-level function
// in main is lowercase; one still declared as a method on an internal/config
// type keeps its exported name. Matching is by identifier, so a name pinned
// in the wrong case matches nothing and silently gates nothing.
var wantGatedMutatorNames = []string{
	"With", "WithDeclinable",
	"mintAPICredentialFor",
	"addAPICredential", "removeAPICredential",
	"RotateProjectToken",
	"UpsertExternalMcp", "RemoveExternalMcp",
	"UpsertService", "RemoveService",
	"addEnrolment", "removeEnrolment",
	"mintBootstrapCode",
	"ApplyCreate", "ApplyUpdate",
	"UpdateProjectAllowedTools", "UpdateProjectAccess", "updateProjectContext",
	"updateProjectMcps", "SetProjectAllowCwdAuth", "UpdateProjectAllowExternal",
	"updateProjectKind", "updateProjectPath",
}

// wantGateAllowlistedFiles pins gateAllowlistedFiles' key set the same way.
var wantGateAllowlistedFiles = []string{
	"cmd/relay/credential_ops.go", "cmd/relay/project_ops.go", "cmd/relay/mcp_ops.go", "cmd/relay/service_ops.go",
	"cmd/relay/enrolment_ops.go", "cmd/relay/login_ops.go",
	"internal/project/scope.go", "cmd/relay/api_credential.go", "internal/enrolment/enrolment.go", "internal/project/apply.go",
	"cmd/relay/project_routes.go", "cmd/relay/ipc_handlers.go", "cmd/relay/trayapp.go", "cmd/relay/frontend_server.go", "cmd/relay/login_routes.go",
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
	// relaySourceDir, not gsModuleRoot: every requireGate call site is in
	// package main, because requireGate itself is. The mutation-containment
	// scan above reaches further only because the mutators it hunts do.
	sites := scanRequireGateCallSites(t, relaySourceDir(t))

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
