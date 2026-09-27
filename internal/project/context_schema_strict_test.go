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

// Absent applies_to, an empty "" entry (alone or beside a pattern) and an
// uncompilable glob all govern everything: the fail-closed reading, since
// more tools then require a value.
func TestContextField_AnAbsentEmptyOrMalformedAppliesToGovernsEverything(t *testing.T) {
	parsed := func(t *testing.T, raw, name string) ContextField {
		t.Helper()
		cs := parseStrict(t, raw)
		if !cs.Usable() {
			t.Fatalf("unexpected refusal: %s", cs.MalformedReason())
		}
		f, ok := cs.Field(name)
		if !ok {
			t.Fatalf("field %q missing", name)
		}
		return f
	}
	for _, tc := range []struct {
		name       string
		field      func(t *testing.T) ContextField
		governs    []string
		governsAll []string
	}{
		{
			name: `applies_to [""]`,
			field: func(t *testing.T) ContextField {
				return parsed(t, `{"mail_accounts":{"type":"array","scope":"restrict","source":"operator","applies_to":[""]}}`, "mail_accounts")
			},
			governs:    []string{"mail_search", "capture_screenshot", "web_fetch", ""},
			governsAll: []string{"mail_search", "web_fetch"},
		},
		{
			name: `a stray "" beside "mail_*"`,
			field: func(t *testing.T) ContextField {
				return parsed(t, `{"f":{"type":"array","scope":"restrict","source":"operator","applies_to":["mail_*",""]}}`, "f")
			},
			governs: []string{"capture_screenshot"},
		},
		{
			name:       "absent applies_to",
			field:      func(t *testing.T) ContextField { return parsed(t, fsmcpV2Schema, V1AllowedDirsField) },
			governs:    []string{"fs_read", "fs_bash", "anything_at_all"},
			governsAll: []string{"fs_read", "fs_write"},
		},
		{
			name: "an uncompilable glob",
			field: func(t *testing.T) ContextField {
				return ContextField{Name: "x", Scope: ContextScopeRestrict, AppliesTo: []string{"mail_[unterminated"}}
			},
			governs: []string{"nothing_like_it"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.field(t)
			for _, tool := range tc.governs {
				if !f.Governs(tool) {
					t.Errorf("%s does not govern %q", tc.name, tool)
				}
			}
			if tc.governsAll != nil && !f.GovernsAll(tc.governsAll) {
				t.Errorf("%s did not govern all of %v", tc.name, tc.governsAll)
			}
		})
	}
}
