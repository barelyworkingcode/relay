package main

// Unit coverage for the v2 context-schema vocabulary (ADR-011 decision 3).
// Every question relay asks of a schema is answered here without a live MCP:
// the parse, the applies_to glob, the value validator, and the scope note.
// The enforcement that consumes them is covered in router_access_test.go and
// router_scope_test.go.

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// macmcpSchema is ADR-011's worked example, verbatim: two operator-supplied
// restrict fields governing mail_*, and one project_path field governing
// exactly two tools.
const macmcpSchema = `{
  "mail_accounts": {
    "type": "array", "items": {"type": "string"},
    "description": "Mail accounts this client may read from or send as",
    "scope": "restrict", "source": "operator",
    "applies_to": ["mail_*"], "enumerable": true
  },
  "mail_mailboxes": {
    "type": "array", "items": {"type": "string"},
    "description": "Mailbox paths within those accounts this client may reach",
    "scope": "restrict", "source": "operator",
    "applies_to": ["mail_*"], "enumerable": true,
    "depends_on": ["mail_accounts"]
  },
  "write_dirs": {
    "type": "array", "items": {"type": "string"},
    "description": "Directories this client may write files into",
    "scope": "restrict", "source": "project_path",
    "applies_to": ["mail_save_attachment", "mail_get_source"]
  }
}`

// fsmcpV2Schema is what fsMCP looks like once it declares v2: one
// project_path field with no applies_to at all, which governs everything.
const fsmcpV2Schema = `{
  "allowed_dirs": {
    "type": "array", "items": {"type": "string"},
    "description": "Directories this client may reach",
    "scope": "restrict", "source": "project_path"
  }
}`

func TestParseContextSchema_V1DeclaresNoFieldsWhateverItSays(t *testing.T) {
	// A schema that never opted into the vocabulary must be handled exactly as
	// it was before ADR-011 — including one that carries a key spelled like a
	// keyword. Reading v2 semantics off an un-versioned declaration would let
	// an MCP change how relay enforces without saying so.
	cs := ParseContextSchema(json.RawMessage(macmcpSchema), 0)
	if cs.V2() {
		t.Fatal("version 0 parsed as v2")
	}
	if len(cs.Fields) != 0 {
		t.Fatalf("v1 schema exposed %d fields; v1 is read through schemaHasField only", len(cs.Fields))
	}
	if len(cs.RestrictFields()) != 0 {
		t.Fatal("v1 schema produced restrict fields")
	}
}

func TestParseContextSchema_V2FlatFormIsTheContract(t *testing.T) {
	cs := ParseContextSchema(json.RawMessage(macmcpSchema), 2)
	if !cs.V2() {
		t.Fatal("version 2 did not parse as v2")
	}
	if got := len(cs.RestrictFields()); got != 3 {
		t.Fatalf("restrict fields = %d, want 3", got)
	}
	// Name order, so a scope note and an audit line do not reshuffle between
	// two identical calls.
	var names []string
	for _, f := range cs.RestrictFields() {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "mail_accounts,mail_mailboxes,write_dirs" {
		t.Fatalf("restrict fields out of name order: %v", names)
	}

	f, ok := cs.Field("mail_mailboxes")
	if !ok {
		t.Fatal("mail_mailboxes missing")
	}
	if !f.Restricts() || !f.FromOperator() || f.FromProjectPath() {
		t.Fatalf("mail_mailboxes keywords misread: %+v", f)
	}
	if !f.Enumerable || len(f.DependsOn) != 1 || f.DependsOn[0] != "mail_accounts" {
		t.Fatalf("enumerable/depends_on misread: %+v", f)
	}
	if len(cs.ProjectPathFields()) != 1 || cs.ProjectPathFields()[0].Name != "write_dirs" {
		t.Fatalf("project_path fields = %v", cs.ProjectPathFields())
	}
	if len(cs.OperatorFields()) != 2 {
		t.Fatalf("operator fields = %v", cs.OperatorFields())
	}
}

func TestParseContextSchema_NestedFormIsRescuedOnlyTowardsFailClosed(t *testing.T) {
	// Issue #17 showed the flat/nested ambiguity is live and that its failure
	// direction is fail-open: a restrict field relay does not see is a grant
	// permitted and nothing enforced. The nested shape is therefore consulted,
	// but only when the flat reading found no restriction at all.
	nested := `{"type":"object","properties":` + macmcpSchema + `}`
	cs := ParseContextSchema(json.RawMessage(nested), 2)
	if len(cs.RestrictFields()) != 3 {
		t.Fatalf("nested v2 schema yielded %d restrict fields, want 3", len(cs.RestrictFields()))
	}
	// And a flat schema that already declares a restriction is NOT re-read
	// under a "properties" key it happens to also carry.
	both := `{"mail_accounts":{"type":"array","scope":"restrict","source":"operator"},` +
		`"properties":{"decoy":{"type":"array","scope":"restrict","source":"project_path"}}}`
	cs = ParseContextSchema(json.RawMessage(both), 2)
	if _, ok := cs.Field("decoy"); ok {
		t.Fatal("nested rescue overrode a flat reading that already found a restriction")
	}
}

func TestParseContextSchema_SurvivesRubbish(t *testing.T) {
	for _, raw := range []string{``, `null`, `[]`, `{not json`, `{"a":3}`, `"a string"`} {
		cs := ParseContextSchema(json.RawMessage(raw), 2)
		if len(cs.RestrictFields()) != 0 {
			t.Fatalf("%q produced restrict fields", raw)
		}
	}
}

func TestContextField_GovernsReadsAppliesTo(t *testing.T) {
	cs := ParseContextSchema(json.RawMessage(macmcpSchema), 2)
	mail, _ := cs.Field("mail_accounts")
	dirs, _ := cs.Field("write_dirs")

	if !mail.Governs("mail_search") || !mail.Governs("mail_send") {
		t.Fatal("mail_* did not match a mail tool")
	}
	if mail.Governs("messages_send") {
		t.Fatal("mail_* matched a non-mail tool")
	}
	if !dirs.Governs("mail_get_source") || dirs.Governs("mail_search") {
		t.Fatal("an explicit applies_to list did not select exactly its two tools")
	}
}

func TestContextField_AbsentAppliesToGovernsEverything(t *testing.T) {
	// The domain-blind default: an MCP that offers no precision gets the
	// widest reading.
	cs := ParseContextSchema(json.RawMessage(fsmcpV2Schema), 2)
	f, _ := cs.Field(v1AllowedDirsField)
	for _, name := range []string{"fs_read", "fs_bash", "anything_at_all"} {
		if !f.Governs(name) {
			t.Fatalf("absent applies_to did not govern %q", name)
		}
	}
	if !f.GovernsAll([]string{"fs_read", "fs_write"}) {
		t.Fatal("absent applies_to did not govern all")
	}
}

func TestMatchToolPattern_IsAnchoredAndShared(t *testing.T) {
	// One matcher serves both applies_to and allowed_tools, so the anchoring
	// is asserted once, here, on the matcher itself.
	cases := []struct {
		pattern, tool string
		want          bool
	}{
		{"mail_*", "mail_send", true},
		{"mail_*", "mail_", true},
		{"mail_*", "xmail_send", false},    // not a substring match
		{"mail_*", "send_mail_now", false}, // nor a suffix one
		{"mail_send", "mail_send", true},   // no metacharacter: exact
		{"mail_send", "mail_sender", false},
		{"mail_send", "mail_", false},
		{"*", "anything", true},
	}
	for _, tc := range cases {
		got, err := matchToolPattern(tc.pattern, tc.tool)
		if err != nil {
			t.Fatalf("matchToolPattern(%q,%q): %v", tc.pattern, tc.tool, err)
		}
		if got != tc.want {
			t.Errorf("matchToolPattern(%q,%q) = %v, want %v", tc.pattern, tc.tool, got, tc.want)
		}
	}
}

func TestToolAllowedByPatterns_MalformedPatternAdmitsNothing(t *testing.T) {
	// The opposite fail-closed direction to ContextField.Governs, and the
	// reason matchToolPattern returns the error instead of deciding.
	if toolAllowedByPatterns([]string{"mail_[unterminated"}, "mail_send") {
		t.Fatal("a malformed allowlist pattern admitted a tool")
	}
	if toolAllowedByPatterns(nil, "mail_send") {
		t.Fatal("an empty allowlist admitted a tool")
	}
	if !toolAllowedByPatterns([]string{"nope_*", "mail_*"}, "mail_send") {
		t.Fatal("a later matching pattern was not reached")
	}
}

func TestContextField_MalformedGlobGovernsEverything(t *testing.T) {
	// The two readings of an uncompilable pattern are "governs nothing" and
	// "governs everything". The second is fail-closed — more tools require a
	// value, and a grant whose MCP publishes a broken pattern is refused
	// rather than silently unscoped — so it is the one taken.
	f := ContextField{Name: "x", Scope: ContextScopeRestrict, AppliesTo: []string{"mail_[unterminated"}}
	if !f.Governs("nothing_like_it") {
		t.Fatal("a malformed glob failed open")
	}
}

func TestContextField_GovernsAllIsFalseForAnUnknownToolSurface(t *testing.T) {
	// "This MCP exposes no tools" is what an MCP relay has never connected to
	// looks like. Vacuous truth there would refuse a grant on the strength of
	// missing information.
	f := ContextField{Name: "x", Scope: ContextScopeRestrict}
	if f.GovernsAll(nil) {
		t.Fatal("an empty tool list answered GovernsAll true")
	}
}

func TestContextField_ValidateValueRefusesEmptyInEveryShape(t *testing.T) {
	arr := ContextField{Name: "mail_accounts", Type: "array", Items: json.RawMessage(`{"type":"string"}`)}
	str := ContextField{Name: "root", Type: "string"}
	free := ContextField{Name: "anything"}

	refuse := []struct {
		f   ContextField
		val string
	}{
		{arr, ``},
		{arr, `null`},
		{arr, `[]`},
		{arr, `["ok",""]`},
		{arr, `["ok","  "]`},
		{arr, `[3]`},
		{arr, `"not an array"`},
		{str, `""`},
		{str, `null`},
		{str, `["a"]`},
		{free, `[]`},
		{free, `{}`},
		{free, `""`},
		{free, `null`},
	}
	for _, tc := range refuse {
		if err := tc.f.ValidateValue(json.RawMessage(tc.val)); err == nil {
			t.Errorf("%s accepted %q", tc.f.Name, tc.val)
		}
	}

	accept := []struct {
		f   ContextField
		val string
	}{
		{arr, `["Bob"]`},
		{arr, `["Alice","Bob"]`},
		{str, `"/tmp/project"`},
		{free, `{"a":1}`},
		{free, `3`},
	}
	for _, tc := range accept {
		if err := tc.f.ValidateValue(json.RawMessage(tc.val)); err != nil {
			t.Errorf("%s refused %q: %v", tc.f.Name, tc.val, err)
		}
	}
}

func TestHasScopeValue_EmptyIsAbsent(t *testing.T) {
	values := map[string]json.RawMessage{
		"present": json.RawMessage(`["Bob"]`),
		"empty":   json.RawMessage(`[]`),
		"null":    json.RawMessage(`null`),
		"blank":   json.RawMessage(`""`),
		"object":  json.RawMessage(`{}`),
	}
	if !hasScopeValue(values, "present") {
		t.Error("a real value read as absent")
	}
	for _, name := range []string{"empty", "null", "blank", "object", "missing"} {
		if hasScopeValue(values, name) {
			t.Errorf("%q read as a usable scope value", name)
		}
	}
}

func TestScopeNoteFor_UsesTheSchemasOwnDescription(t *testing.T) {
	cs := ParseContextSchema(json.RawMessage(macmcpSchema), 2)
	values := map[string]json.RawMessage{
		"mail_accounts":  json.RawMessage(`["Bob"]`),
		"mail_mailboxes": json.RawMessage(`["INBOX","Projects/Archive"]`),
	}
	note := scopeNoteFor(cs, values, "mail_search")
	for _, want := range []string{"Mail accounts this client may read from or send as", "Bob", "INBOX, Projects/Archive"} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q missing %q", note, want)
		}
	}
	// write_dirs governs neither of those tools and has no value here, so it
	// must not appear.
	if strings.Contains(note, "Directories") {
		t.Errorf("note mentioned an ungoverned field: %q", note)
	}
	if n := scopeNoteFor(cs, values, "messages_send"); n != "" {
		t.Errorf("an ungoverned tool got a note: %q", n)
	}
	if n := scopeNoteFor(cs, nil, "mail_search"); n != "" {
		t.Errorf("a grant with no values got a note: %q", n)
	}
	if n := scopeNoteFor(ParseContextSchema(json.RawMessage(macmcpSchema), 0), values, "mail_search"); n != "" {
		t.Errorf("a v1 schema got a note: %q", n)
	}
}

func TestAppendScopeNote_IsIdempotent(t *testing.T) {
	// ListTools and ListSkillBuckets each annotate their own copy of the same
	// live tool; the two must not double-append.
	note := "Scope: accounts — Bob."
	once := appendScopeNote("Search mail.", note)
	twice := appendScopeNote(once, note)
	if once != twice {
		t.Fatalf("double-appended: %q then %q", once, twice)
	}
	if appendScopeNote("desc", "") != "desc" {
		t.Fatal("an empty note changed the description")
	}
	if appendScopeNote("", note) != note {
		t.Fatal("an empty description did not become the note")
	}
}

// TestNoDomainSpecificFieldNamesRemainInRelay is ADR-011's closing consequence
// made checkable: "the only domain-specific string left in relay is the v1
// allowed_dirs compatibility branch, kept for one release with a deprecation
// line and a test asserting it is the last one."
//
// It reads STRING LITERALS out of the AST rather than grepping, so the many
// comments explaining the history do not count and cannot be gamed by
// rewording. fs_bash is the one other survivor and is deliberately named here
// too, with its deferral, so that adding a third needs a deliberate edit to
// this list.
func TestNoDomainSpecificFieldNamesRemainInRelay(t *testing.T) {
	allowed := map[string]struct {
		file string
		why  string
	}{
		v1AllowedDirsField: {"context_schema.go", "the v1 compatibility branch (ADR-011 decision 3), scheduled for removal"},
		v1FsBashTool:       {"context_schema.go", "the fs_bash auto-disable, DEFERRED by ADR-011 as not resource scoping"},
	}

	counts := map[string]int{}
	fset := token.NewFileSet()
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if name := info.Name(); name != "." && (strings.HasPrefix(name, ".") || name == "test" || name == "web" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			val, uerr := strconv.Unquote(lit.Value)
			if uerr != nil {
				return true
			}
			spec, watched := allowed[val]
			if !watched {
				return true
			}
			counts[val]++
			if filepath.Base(path) != spec.file {
				t.Errorf("domain-specific literal %q appears in %s; it belongs only in %s (%s)",
					val, path, spec.file, spec.why)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	for name, spec := range allowed {
		if counts[name] != 1 {
			t.Errorf("literal %q appears %d times in non-test sources, want exactly 1 (%s in %s)",
				name, counts[name], spec.why, spec.file)
		}
	}
}
