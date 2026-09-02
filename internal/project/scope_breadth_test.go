package project

import (
	"encoding/json"
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
	return ScopeNoteFor(cs, map[string]json.RawMessage{
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

// mailAccountsValueSchema is disclose: "value", the default -- macMCP's own
// mail_accounts is not path-shaped, so this exercises the wildcard breadth
// classification on a resource-scope field rather than piggy-backing on
// fsmcp's filesystem fixture.
const mailAccountsValueSchema = `{
  "mail_accounts": {
    "type": "array", "items": {"type": "string"},
    "description": "Mail accounts this client may read from or send as",
    "scope": "restrict", "source": "operator"
  }
}`

func mailAccountsNoteFor(t *testing.T, value string) string {
	t.Helper()
	cs := ParseContextSchema(json.RawMessage(mailAccountsValueSchema), 2)
	if !cs.Usable() {
		t.Fatalf("fixture schema unusable: %s", cs.MalformedReason())
	}
	return ScopeNoteFor(cs, map[string]json.RawMessage{
		"mail_accounts": json.RawMessage(value),
	}, "mail_send")
}

// Deliberate, the ADR-011 addendum's own reasoning ("A star and an empty
// array"): a client learns it reaches every account the moment it lists
// them, so withholding "unrestricted" from the scope note buys nothing and
// costs the one warning that matters most for this value.
func TestScopeNote_AWildcardOutranksEveryDiscloseSetting(t *testing.T) {
	for _, disclose := range []string{`"value"`, `"count"`, `"none"`} {
		schema := `{"mail_accounts":{"type":"array","description":"Accounts","scope":"restrict","source":"operator","disclose":` + disclose + `}}`
		cs := ParseContextSchema(json.RawMessage(schema), 2)
		if !cs.Usable() {
			t.Fatalf("fixture schema unusable: %s", cs.MalformedReason())
		}
		note := ScopeNoteFor(cs, map[string]json.RawMessage{"mail_accounts": json.RawMessage(`["*"]`)}, "mail_send")
		if !strings.Contains(note, "unrestricted") {
			t.Errorf("disclose %s hid that the grant is unrestricted: %q", disclose, note)
		}
		if strings.Contains(note, "confined to") {
			t.Errorf("disclose %s still called it a confinement: %q", disclose, note)
		}
	}
	// A bounded, named grant is unaffected by the wildcard's special-casing.
	bounded := mailAccountsNoteFor(t, `["Bob"]`)
	if strings.Contains(bounded, "unrestricted") {
		t.Errorf("a named account grant read as unrestricted: %q", bounded)
	}
}

func TestScopeBreadth_WildcardClassification(t *testing.T) {
	if got := ScopeValueBreadth(json.RawMessage(`["*"]`)); got != ScopeBreadthWildcard {
		t.Errorf("[\"*\"] classified as %q, want wildcard", got)
	}
	// Recognised only as the array's sole element (ADR-011 addendum) --
	// relay's own save-time validation refuses this combination outright, so
	// it should never reach here already stored, but the classifier must not
	// call it unrestricted if it somehow does.
	if got := ScopeValueBreadth(json.RawMessage(`["*","Bob"]`)); got == ScopeBreadthWildcard {
		t.Errorf("[\"*\",\"Bob\"] classified as the wildcard; it is a mixed value, not a grant of everything")
	}
	if got := ScopeValueBreadth(json.RawMessage(`["Bob"]`)); got != ScopeBreadthBounded {
		t.Errorf("a plain named value classified as %q", got)
	}
}

func TestScopeBreadth_Classification(t *testing.T) {
	cases := []struct{ entry, want string }{
		{"/", ScopeBreadthRoot},
		{"//", ScopeBreadthRoot},
		{"/..", ScopeBreadthRoot},
		{"/Users/admin/../..", ScopeBreadthRoot},
		{"  /  ", ScopeBreadthRoot},
		{"~", ScopeBreadthHome},
		{"~/", ScopeBreadthHome},
		{"/Users", ScopeBreadthHome},
		{"/Users/admin", ScopeBreadthHome},
		{"/Users/admin/", ScopeBreadthHome},
		{"/home/someone", ScopeBreadthHome},
		{"/Users/admin/source/project", ScopeBreadthBounded},
		{"/etc", ScopeBreadthBounded},
		{"/tmp/work", ScopeBreadthBounded},
		{"relative/path", ScopeBreadthBounded},
		{"Bob", ScopeBreadthBounded},
		{"", ScopeBreadthBounded},
	}
	for _, tc := range cases {
		if got := ScopeEntryBreadth(tc.entry); got != tc.want {
			t.Errorf("ScopeEntryBreadth(%q) = %q, want %q", tc.entry, got, tc.want)
		}
	}
}

// Subtle: a multi-entry value is a union — reporting just the first entry
// would describe the confinement the operator meant, not the one in force.
func TestScopeBreadth_AListIsAUnion(t *testing.T) {
	if got := ScopeValueBreadth(json.RawMessage(`["/Users/me/proj","/"]`)); got != ScopeBreadthRoot {
		t.Errorf("a list containing the filesystem root read as %q", got)
	}
	if got := ScopeValueBreadth(json.RawMessage(`["/Users/me/proj","/tmp"]`)); got != ScopeBreadthBounded {
		t.Errorf("a bounded list read as %q", got)
	}
	// A string-typed field is one entry, not zero.
	if got := ScopeValueBreadth(json.RawMessage(`"/"`)); got != ScopeBreadthRoot {
		t.Errorf("a scalar filesystem root read as %q", got)
	}
}
