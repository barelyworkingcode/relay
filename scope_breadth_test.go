package main

// Issue #41: a grant of "/" rendered identically to a one-folder grant on the
// verification step the operator guide points at first.
//
// The headline regression is TestScopeNote_AFilesystemRootIsNotConfinedToOneValue,
// which is written against nothing but ParseContextSchema and scopeNoteFor —
// both unchanged in signature — so it compiles and FAILS on main.

import (
	"encoding/json"
	"strings"
	"testing"
)

// fsmcpCountSchema is fsMCP's declaration as it exists after issue #33: the
// value is host topology, so the client's scope note gets the count and not
// the paths.
const fsmcpCountSchema = `{
  "allowed_dirs": {
    "type": "array", "items": {"type": "string"},
    "description": "Directories this client may read, search and modify within",
    "scope": "restrict", "source": "operator",
    "disclose": "count"
  }
}`

func noteFor(t *testing.T, schema, value string) string {
	t.Helper()
	cs := ParseContextSchema(json.RawMessage(schema), 2)
	if !cs.Usable() {
		t.Fatalf("fixture schema unusable: %s", cs.MalformedReason())
	}
	return scopeNoteFor(cs, map[string]json.RawMessage{
		"allowed_dirs": json.RawMessage(value),
	}, "fs_read")
}

// TestScopeNote_AFilesystemRootIsNotConfinedToOneValue is issue #41's headline
// comparison, and the whole of it: on main these two calls return the SAME
// string.
//
// `disclose: "count"` was a correct fix for a real disclosure. What it missed
// is that a count is not a measure of confinement — "1 value" is true of
// /Users/me/project and equally true of "/", and the first verification step
// relay's own operator guide names could therefore not fail.
func TestScopeNote_AFilesystemRootIsNotConfinedToOneValue(t *testing.T) {
	root := noteFor(t, fsmcpCountSchema, `["/"]`)
	folder := noteFor(t, fsmcpCountSchema, `["/Users/me/project"]`)

	if root == folder {
		t.Fatalf("a grant of \"/\" and a grant of one folder render identically: %q", root)
	}
	if strings.Contains(root, "confined to 1 value") {
		t.Errorf("a grant of the whole filesystem is described as a confinement: %q", root)
	}
	if !strings.Contains(root, "unrestricted") {
		t.Errorf("the note does not say the grant is unrestricted: %q", root)
	}
	// The bounded grant is unchanged, byte for byte. Issue #41 is explicit
	// that the client-side rendering must not be weakened to fix an
	// operator-side problem: the count is still all a confined client gets.
	if folder != `Scope: Directories this client may read, search and modify within — confined to 1 value.` {
		t.Errorf("the bounded grant's note changed: %q", folder)
	}
}

// TestScopeNote_AnUnrestrictedValueOutranksEveryDiscloseSetting pins the rule
// that the finding is not a disclosure. A client learns it is at a filesystem
// root the moment it lists one, so withholding it buys nothing and costs the
// client the one fact about its own limits that matters.
func TestScopeNote_AnUnrestrictedValueOutranksEveryDiscloseSetting(t *testing.T) {
	for _, disclose := range []string{`"value"`, `"count"`, `"none"`} {
		schema := `{"allowed_dirs":{"type":"array","description":"Dirs","scope":"restrict","source":"operator","disclose":` + disclose + `}}`
		note := noteFor(t, schema, `["/"]`)
		if !strings.Contains(note, "unrestricted") {
			t.Errorf("disclose %s hid that the grant is unrestricted: %q", disclose, note)
		}
		if strings.Contains(note, "confined to") {
			t.Errorf("disclose %s still called it a confinement: %q", disclose, note)
		}
	}
	// disclose: "value" keeps showing the coordinates as well — the warning is
	// a second sentence, never a replacement for the value.
	if note := noteFor(t, `{"allowed_dirs":{"type":"array","description":"Dirs","scope":"restrict","source":"operator"}}`, `["/"]`); !strings.Contains(note, `/`) {
		t.Errorf("disclose: \"value\" stopped showing the value: %q", note)
	}
}

// TestScopeNote_AHomeDirectoryStaysCountedForTheClient is the deliberate half
// of the split. A home directory IS confined, so "confined to 1 value" is not
// false; and naming it would tell a remote client the sandbox is somebody's
// home, which is exactly the topology `disclose` exists to withhold. It is
// loud on every OPERATOR surface instead — see TestScopeBreadth_Classification
// and the `relay grant` and Settings tests.
func TestScopeNote_AHomeDirectoryStaysCountedForTheClient(t *testing.T) {
	note := noteFor(t, fsmcpCountSchema, `["/Users/admin"]`)
	if !strings.Contains(note, "confined to 1 value") {
		t.Errorf("a home directory's client-facing note changed: %q", note)
	}
	if strings.Contains(note, "home") {
		t.Errorf("the note disclosed that the root is a home directory: %q", note)
	}
}

func TestScopeBreadth_Classification(t *testing.T) {
	cases := []struct{ entry, want string }{
		{"/", scopeBreadthRoot},
		{"//", scopeBreadthRoot},
		{"/..", scopeBreadthRoot},
		{"/Users/admin/../..", scopeBreadthRoot},
		{"  /  ", scopeBreadthRoot},
		{"~", scopeBreadthHome},
		{"~/", scopeBreadthHome},
		{"/Users", scopeBreadthHome},
		{"/Users/admin", scopeBreadthHome},
		{"/Users/admin/", scopeBreadthHome},
		{"/home/someone", scopeBreadthHome},
		{"/Users/admin/source/project", scopeBreadthBounded},
		{"/etc", scopeBreadthBounded},
		{"/tmp/work", scopeBreadthBounded},
		{"relative/path", scopeBreadthBounded},
		{"Bob", scopeBreadthBounded},
		{"", scopeBreadthBounded},
	}
	for _, tc := range cases {
		if got := scopeEntryBreadth(tc.entry); got != tc.want {
			t.Errorf("scopeEntryBreadth(%q) = %q, want %q", tc.entry, got, tc.want)
		}
	}
}

// TestScopeBreadth_AListIsAUnion pins the direction of the answer for a
// multi-entry value: ["/Users/me/proj", "/"] reaches everything, and a
// rendering that reported the first entry would describe the confinement the
// operator meant rather than the one in force.
func TestScopeBreadth_AListIsAUnion(t *testing.T) {
	if got := scopeValueBreadth(json.RawMessage(`["/Users/me/proj","/"]`)); got != scopeBreadthRoot {
		t.Errorf("a list containing the filesystem root read as %q", got)
	}
	if got := scopeValueBreadth(json.RawMessage(`["/Users/me/proj","/tmp"]`)); got != scopeBreadthBounded {
		t.Errorf("a bounded list read as %q", got)
	}
	// A string-typed field is one entry, not zero.
	if got := scopeValueBreadth(json.RawMessage(`"/"`)); got != scopeBreadthRoot {
		t.Errorf("a scalar filesystem root read as %q", got)
	}
}

// TestAuditAuthorityLine_NamesAnUnrestrictedScope: `relay audit --authority`
// is the surface issue #41 names as already truthful, and it stays truthful —
// the coordinates are still printed. The warning is appended beside them,
// because the operator is entitled to both.
func TestAuditAuthorityLine_NamesAnUnrestrictedScope(t *testing.T) {
	allowExternal := false
	line, ok := auditAuthorityLine(AuditEvent{
		Access:        AccessWrite,
		AllowExternal: &allowExternal,
		Scope:         map[string]json.RawMessage{"allowed_dirs": json.RawMessage(`["/"]`)},
	})
	if !ok {
		t.Fatal("no authority line for a record that has one")
	}
	if !strings.Contains(line, `allowed_dirs=["/"]`) {
		t.Errorf("the authority line stopped printing the real value: %q", line)
	}
	if !strings.Contains(line, "unrestricted") {
		t.Errorf("the authority line does not flag the filesystem root: %q", line)
	}

	bounded, _ := auditAuthorityLine(AuditEvent{
		Access:        AccessWrite,
		AllowExternal: &allowExternal,
		Scope:         map[string]json.RawMessage{"allowed_dirs": json.RawMessage(`["/Users/me/project"]`)},
	})
	if strings.Contains(bounded, "unrestricted") {
		t.Errorf("a bounded grant was flagged: %q", bounded)
	}
}

// ---------------------------------------------------------------------------
// `relay grant` — the operator-side check the guide now points at
// ---------------------------------------------------------------------------

func TestGrantView_ShowsTheRealValueAndFlagsTheRoot(t *testing.T) {
	s := &Settings{
		Version:      1,
		ExternalMcps: []ExternalMcp{{ID: "fsmcp", DisplayName: "fsMCP"}},
	}
	profile := Project{
		ID: "probe", Name: "Probe", Kind: ProjectKindRemote,
		AllowedMcpIDs: []string{"fsmcp"},
		AllowedTools:  map[string][]string{"fsmcp": {"fs_*"}},
		Context: map[string]json.RawMessage{
			"fsmcp": json.RawMessage(`{"allowed_dirs":["/"]}`),
		},
	}
	var out strings.Builder
	printGrantViews(&out, []grantView{newGrantView(s, profile)})
	got := out.String()

	// The coordinates, in full, whatever the field's disclose says. This is
	// the operator's own machine and their own grant.
	if !strings.Contains(got, `allowed_dirs = ["/"]`) {
		t.Errorf("the operator surface did not print the real value:\n%s", got)
	}
	if !strings.Contains(got, "UNRESTRICTED (THE WHOLE FILESYSTEM)") {
		t.Errorf("the operator surface did not flag the filesystem root:\n%s", got)
	}
	// The two asymmetric defaults are resolved through StoredToken's own
	// methods, so this command cannot drift from what the router decides.
	if !strings.Contains(got, "access=read") || !strings.Contains(got, "outbound=blocked") {
		t.Errorf("an access profile's defaults were not resolved:\n%s", got)
	}
}

func TestGrantView_ABoundedGrantCarriesNoWarning(t *testing.T) {
	s := &Settings{Version: 1, ExternalMcps: []ExternalMcp{{ID: "fsmcp"}}}
	local := Project{
		ID: "proj", Name: "Proj", Path: "/Users/me/project",
		AllowedMcpIDs: []string{"fsmcp"},
		Context: map[string]json.RawMessage{
			"fsmcp": json.RawMessage(`{"allowed_dirs":["/Users/me/project"]}`),
		},
	}
	var out strings.Builder
	printGrantViews(&out, []grantView{newGrantView(s, local)})
	got := out.String()
	if strings.Contains(got, "**") {
		t.Errorf("a one-folder grant was flagged:\n%s", got)
	}
	if !strings.Contains(got, "access=write") || !strings.Contains(got, "outbound=allowed") {
		t.Errorf("a local project's defaults were not resolved:\n%s", got)
	}
	if !strings.Contains(got, "all tools") {
		t.Errorf("a local project with no allowlist should hold every tool:\n%s", got)
	}
}

func TestSelectGrantRecords_ResolvesByIdAndByName(t *testing.T) {
	projects := []Project{
		{ID: "b-id", Name: "A name"},
		{ID: "a-id", Name: "B name"},
	}
	if got := selectGrantRecords(projects, "a-id"); len(got) != 1 || got[0].ID != "a-id" {
		t.Errorf("selecting by id returned %+v", got)
	}
	if got := selectGrantRecords(projects, "A name"); len(got) != 1 || got[0].ID != "b-id" {
		t.Errorf("selecting by name returned %+v", got)
	}
	if got := selectGrantRecords(projects, "nope"); got != nil {
		t.Errorf("an unknown selector returned %+v", got)
	}
	// Everything, in name order, so a run-it-over-the-whole-machine sweep
	// reads the same twice.
	all := selectGrantRecords(projects, "")
	if len(all) != 2 || all[0].Name != "A name" {
		t.Errorf("the unfiltered listing is not in name order: %+v", all)
	}
}
