package project

import (
	"encoding/json"
	"strings"
	"testing"
)

func parseStrict(t *testing.T, raw string) ContextSchema {
	t.Helper()
	return ParseContextSchema(json.RawMessage(raw), 2)
}

func TestParseContextSchema_ANearMissKeywordKeyIsRefusedRatherThanGuessedAt(t *testing.T) {
	for _, key := range []string{"Scope", "SCOPE", "Applies_To", "APPLIES_TO", "Source", "Enumerable", "Depends_On", "Disclose", "DISCLOSE"} {
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
		"scope":          `{"f":{"scope":"RESTRICT"}}`,
		"source":         `{"f":{"scope":"restrict","source":"Project_Path"}}`,
		"source-casing":  `{"f":{"scope":"restrict","source":"OPERATOR"}}`,
		"disclose":       `{"f":{"scope":"restrict","disclose":"Count"}}`,
		"disclose-value": `{"f":{"scope":"restrict","disclose":"VALUE"}}`,
	}
	for name, raw := range cases {
		cs := parseStrict(t, raw)
		if cs.Usable() {
			t.Errorf("%s: %s parsed as usable; a value a case off from a keyword is a typo of THIS vocabulary, not a member of a future one", name, raw)
		}
	}
}

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
	// The verdict is deliberately whole-schema rather than per-field, even
	// though the other field parsed fine: the fragment that failed may have
	// been the one governing everything, so reporting "here is what I
	// understood" would be a claim about what was not understood.
	if len(cs.RestrictFields()) != 1 {
		t.Fatalf("restrict fields = %d, want the one that parsed", len(cs.RestrictFields()))
	}
	if cs.Usable() {
		t.Fatal("a partially-read schema reported itself usable")
	}
}

// "ui" and "x-relay-future" stand in for a keyword this relay doesn't know
// about yet: fsMCP already ships a "ui" field outside the vocabulary, and any
// keyword added later will look exactly the same to an older relay.
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
	cs = parseStrict(t, `{"f":{"type":"array","scope":"advisory"}}`)
	if !cs.Usable() {
		t.Fatalf(`scope: "advisory" was refused rather than ignored: %s`, cs.MalformedReason())
	}
	if len(cs.RestrictFields()) != 0 {
		t.Fatal(`scope: "advisory" was read as a restriction`)
	}

	cs = parseStrict(t, `{"f":{"type":"array","scope":"restrict","disclose":"summary"}}`)
	if !cs.Usable() {
		t.Fatalf(`disclose: "summary" was refused rather than ignored: %s`, cs.MalformedReason())
	}
	f, _ := cs.Field("f")
	if got := f.Disclosure(); got != ContextDiscloseValue {
		t.Errorf(`disclose: "summary" resolved to %q, want the default %q`, got, ContextDiscloseValue)
	}
}

// The `"type": "object"` sibling beside `"properties"` is not a declaration
// relay failed to read — it is not a declaration at all.
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

// A bad fragment could otherwise hide inside a nested document whose only
// restrict field is the bad one: the flat reading alone would present that as
// a document with no restrictions at all, so the nested rescue has to apply
// to a malformed reading, not just to a restricting one.
func TestParseContextSchema_AMalformedNestedFieldIsStillReported(t *testing.T) {
	nested := `{"type":"object","properties":{"mail_accounts":{"type":"array","Scope":"restrict"}}}`
	cs := parseStrict(t, nested)
	if cs.Usable() {
		t.Fatalf("a malformed field inside a nested document was not reported: %+v", cs)
	}
}

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

	cs = parseStrict(t, `{"f":{"type":"array","scope":"restrict","source":"operator","applies_to":["mail_*",""]}}`)
	f, _ = cs.Field("f")
	if !f.Governs("capture_screenshot") {
		t.Error(`a stray "" beside "mail_*" narrowed the field instead of widening it`)
	}
}
