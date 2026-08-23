package main

// Reading a context schema fails CLOSED.
//
// Three findings, one mechanism. A field fragment relay cannot read used to be
// dropped in silence — which stops relay requiring a value for it, stops relay
// governing the tools it names, and makes filterKnownContextFields strip the
// operator's value off the wire, all with nothing said to anybody. And what
// counted as "cannot read" was decided by encoding/json's case-INSENSITIVE
// struct matching, so {"Scope":"restrict"} was a restriction and
// {"scope":"RESTRICT"} silently was not.
//
// So: an exact keyword is read, a keyword relay has never heard of is ignored
// (decision 3 requires that — a later vocabulary has to be able to land), a
// NEAR MISS of one is an error, and any error makes the whole schema unusable.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func parseStrict(t *testing.T, raw string) ContextSchema {
	t.Helper()
	return ParseContextSchema(json.RawMessage(raw), 2)
}

// TestParseContextSchema_ANearMissKeywordKeyIsRefusedRatherThanGuessedAt is the
// S3 shape: the same discipline readOnlyHintTrue applies to an annotation,
// applied where the direction of the failure is the opposite one. Accepting
// "Scope" silently agrees with a spelling no schema document defines; ignoring
// "scope": "RESTRICT" silently disagrees with what a reviewer reading the MCP's
// published schema would see. Neither may be decided in silence.
func TestParseContextSchema_ANearMissKeywordKeyIsRefusedRatherThanGuessedAt(t *testing.T) {
	for _, key := range []string{"Scope", "SCOPE", "Applies_To", "APPLIES_TO", "Source", "Enumerable", "Depends_On"} {
		raw := `{"mail_accounts":{"type":"array","` + key + `":"restrict"}}`
		cs := parseStrict(t, raw)
		if cs.Usable() {
			t.Errorf("key %q was read as an ordinary unknown key; the schema should be unusable", key)
			continue
		}
		if !strings.Contains(cs.MalformedReason(), key) {
			t.Errorf("key %q: reason does not name it: %s", key, cs.MalformedReason())
		}
	}
}

func TestParseContextSchema_ANearMissKeywordValueIsRefusedToo(t *testing.T) {
	cases := map[string]string{
		"scope":         `{"f":{"scope":"RESTRICT"}}`,
		"source":        `{"f":{"scope":"restrict","source":"Project_Path"}}`,
		"source-casing": `{"f":{"scope":"restrict","source":"OPERATOR"}}`,
	}
	for name, raw := range cases {
		cs := parseStrict(t, raw)
		if cs.Usable() {
			t.Errorf("%s: %s parsed as usable; a value a case off from a keyword is a typo of THIS vocabulary, not a member of a future one", name, raw)
		}
	}
}

// TestParseContextSchema_ATypeSlipIsReportedRatherThanDroppingTheField is the
// S2 shape, in the spelling the finding used.
func TestParseContextSchema_ATypeSlipIsReportedRatherThanDroppingTheField(t *testing.T) {
	raw := `{
	  "mail_accounts": {"type":"array","scope":"restrict","source":"operator","applies_to":"mail_*"},
	  "mail_mailboxes": {"type":"array","scope":"restrict","source":"operator","applies_to":["mail_*"]}
	}`
	cs := parseStrict(t, raw)
	if cs.Usable() {
		t.Fatal(`applies_to written as a string dropped mail_accounts and applied the rest`)
	}
	if !strings.Contains(cs.MalformedReason(), "mail_accounts") ||
		!strings.Contains(cs.MalformedReason(), "applies_to") {
		t.Errorf("reason names neither the field nor the keyword: %s", cs.MalformedReason())
	}
	// The other field parsed fine, and that is exactly why the verdict cannot
	// be per-field: the fragment that failed may have been the one governing
	// everything, so "here is what I understood" is a claim about what was not.
	if len(cs.RestrictFields()) != 1 {
		t.Fatalf("restrict fields = %d, want the one that parsed", len(cs.RestrictFields()))
	}
	if cs.Usable() {
		t.Fatal("a partially-read schema reported itself usable")
	}
}

// TestParseContextSchema_AnUnknownKeywordIsStillIgnored is the boundary the
// rule above must not cross. Decision 3 dropped `ui` from the vocabulary while
// fsMCP still ships one, and every keyword added later arrives at an older
// relay looking exactly like it.
func TestParseContextSchema_AnUnknownKeywordIsStillIgnored(t *testing.T) {
	raw := `{"allowed_dirs":{
	  "type":"array","items":{"type":"string"},
	  "scope":"restrict","source":"project_path",
	  "ui":"directory-list","title":"Directories","x-relay-future":{"a":1}
	}}`
	cs := parseStrict(t, raw)
	if !cs.Usable() {
		t.Fatalf("an unknown keyword made the schema unusable: %s", cs.MalformedReason())
	}
	if len(cs.ProjectPathFields()) != 1 {
		t.Fatalf("the field itself was lost: %+v", cs.Fields)
	}
	// An unrecognised VALUE is ignored the same way: it is not a restriction,
	// and it is not an error either.
	cs = parseStrict(t, `{"f":{"type":"array","scope":"advisory"}}`)
	if !cs.Usable() {
		t.Fatalf(`scope: "advisory" was refused rather than ignored: %s`, cs.MalformedReason())
	}
	if len(cs.RestrictFields()) != 0 {
		t.Fatal(`scope: "advisory" was read as a restriction`)
	}
}

// TestParseContextSchema_NonObjectSiblingsAreNotMalformed keeps the nested
// JSON-Schema tolerance alive. The `"type": "object"` beside `"properties"` is
// not a declaration relay failed to read; it is not a declaration.
func TestParseContextSchema_NonObjectSiblingsAreNotMalformed(t *testing.T) {
	nested := `{"type":"object","properties":` + macmcpSchema + `}`
	cs := parseStrict(t, nested)
	if !cs.Usable() {
		t.Fatalf("the nested form was refused: %s", cs.MalformedReason())
	}
	if len(cs.RestrictFields()) != 3 {
		t.Fatalf("nested restrict fields = %d, want 3", len(cs.RestrictFields()))
	}
}

// TestParseContextSchema_AMalformedNestedFieldIsStillReported closes the one
// way a bad fragment could hide: inside a nested document whose only restrict
// field is the bad one. From the flat reading that presents as a document with
// no restrictions at all — which is precisely the silence being removed — so
// the rescue is adopted on a malformed reading as well as on a restricting one.
func TestParseContextSchema_AMalformedNestedFieldIsStillReported(t *testing.T) {
	nested := `{"type":"object","properties":{"mail_accounts":{"type":"array","Scope":"restrict"}}}`
	cs := parseStrict(t, nested)
	if cs.Usable() {
		t.Fatalf("a malformed field inside a nested document was not reported: %+v", cs)
	}
}

// TestContextField_AnEmptyAppliesToEntryGovernsEverything is S4. applies_to:
// [""] used to make a field that declares itself a restriction govern no tool
// at all, while still being reported as declared to the operator, to the client
// and to the audit log — a restriction that restricts nothing, which is the one
// thing scope: "restrict" is documented as unable to mean.
func TestContextField_AnEmptyAppliesToEntryGovernsEverything(t *testing.T) {
	cs := parseStrict(t, `{"mail_accounts":{"type":"array","scope":"restrict","source":"operator","applies_to":[""]}}`)
	if !cs.Usable() {
		t.Fatalf("unexpected refusal: %s", cs.MalformedReason())
	}
	f, ok := cs.Field("mail_accounts")
	if !ok {
		t.Fatal("field missing")
	}
	for _, tool := range []string{"mail_search", "capture_screenshot", "web_fetch", ""} {
		if !f.Governs(tool) {
			t.Errorf(`applies_to [""] does not govern %q`, tool)
		}
	}
	if !f.GovernsAll([]string{"mail_search", "web_fetch"}) {
		t.Error(`applies_to [""] did not govern every tool`)
	}

	// And one stray "" beside a real pattern widens the restriction rather
	// than voiding the list: the entry names no tool, so it takes the same
	// reading an unparseable pattern does.
	cs = parseStrict(t, `{"f":{"type":"array","scope":"restrict","source":"operator","applies_to":["mail_*",""]}}`)
	f, _ = cs.Field("f")
	if !f.Governs("capture_screenshot") {
		t.Error(`a stray "" beside "mail_*" narrowed the field instead of widening it`)
	}
}

// TestCallTool_RefusesEveryToolOfAnMcpWhoseSchemaCannotBeRead is the
// consequence at the chokepoint, and the reason Usable is total rather than
// per-field: with the fragment merely dropped, this call succeeds, the stored
// mail_accounts value is stripped from _meta on the way out, and neither the
// caller nor the operator nor the MCP author is told anything.
func TestCallTool_RefusesEveryToolOfAnMcpWhoseSchemaCannotBeRead(t *testing.T) {
	broken := `{"mail_accounts":{"type":"array","scope":"restrict","source":"operator","applies_to":"mail_*"}}`
	r := newProfileRouter(t, profileOpts{
		kind:          ProjectKindRemote,
		allowedTools:  map[string][]string{"macmcp": {"mail_*", "web_fetch"}},
		access:        map[string]string{"macmcp": AccessWrite},
		allowExternal: map[string]bool{"macmcp": true},
		contextValues: map[string]json.RawMessage{"mail_accounts": json.RawMessage(`["Bob"]`)},
		schema:        broken,
		schemaVersion: 2,
	})
	for _, tool := range []string{"mail_search", "web_fetch"} {
		_, err := r.CallTool(context.Background(), tool, json.RawMessage(`{}`), testToken)
		if err == nil {
			t.Fatalf("%s ran against a schema relay could not read", tool)
		}
		if !strings.Contains(err.Error(), "cannot read") {
			t.Errorf("%s: refusal does not say why: %v", tool, err)
		}
	}
}
