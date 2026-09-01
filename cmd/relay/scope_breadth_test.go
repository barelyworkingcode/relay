package main

import (
	"encoding/json"
	"github.com/barelyworkingcode/relay/internal/config"
	"strings"
	"testing"
)

// disclose: count because allowed_dirs' value is host topology.
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

// Deliberate: a count is not a measure of confinement — "1 value" is true of
// /Users/me/project and equally true of "/".
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
	// Deliberate: the bounded grant's client-facing note is unchanged — fixing
	// the operator-side problem must not weaken client disclosure.
	if folder != `Scope: Directories this client may read, search and modify within — confined to 1 value.` {
		t.Errorf("the bounded grant's note changed: %q", folder)
	}
}

// Deliberate: a client learns it's at a filesystem root the moment it lists
// one, so withholding that fact buys nothing.
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

// Deliberate: a home directory IS confined (not false to say "1 value"), and
// naming it to a remote client would disclose topology the operator never
// chose to reveal. It's loud on operator surfaces instead.
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

// Subtle: a multi-entry value is a union — reporting just the first entry
// would describe the confinement the operator meant, not the one in force.
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

// The coordinates stay printed; the warning is appended beside them — the
// operator is entitled to both.
func TestAuditAuthorityLine_NamesAnUnrestrictedScope(t *testing.T) {
	allowExternal := false
	line, ok := auditAuthorityLine(AuditEvent{
		Access:        config.AccessWrite,
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
		Access:        config.AccessWrite,
		AllowExternal: &allowExternal,
		Scope:         map[string]json.RawMessage{"allowed_dirs": json.RawMessage(`["/Users/me/project"]`)},
	})
	if strings.Contains(bounded, "unrestricted") {
		t.Errorf("a bounded grant was flagged: %q", bounded)
	}
}

func TestGrantView_ShowsTheRealValueAndFlagsTheRoot(t *testing.T) {
	s := &config.Settings{
		Version:      1,
		ExternalMcps: []config.ExternalMcp{{ID: "fsmcp", DisplayName: "fsMCP"}},
	}
	profile := config.Project{
		ID: "probe", Name: "Probe", Kind: config.ProjectKindRemote,
		AllowedMcpIDs: []string{"fsmcp"},
		AllowedTools:  map[string][]string{"fsmcp": {"fs_*"}},
		Context: map[string]json.RawMessage{
			"fsmcp": json.RawMessage(`{"allowed_dirs":["/"]}`),
		},
	}
	var out strings.Builder
	printGrantViews(&out, []grantView{newGrantView(s, profile)})
	got := out.String()

	// disclose never governs the operator surface — this is the operator's own
	// machine and their own grant.
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
	s := &config.Settings{Version: 1, ExternalMcps: []config.ExternalMcp{{ID: "fsmcp"}}}
	local := config.Project{
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
	projects := []config.Project{
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
