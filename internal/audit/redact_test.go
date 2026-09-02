package audit

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactArgs_RedactsCredentialKeys(t *testing.T) {
	in := json.RawMessage(`{
		"path": "/tmp/x",
		"api_key": "sk-live-1234",
		"nested": {"Authorization": "Bearer abc", "keep": 1},
		"list": [{"password": "hunter2"}, {"ok": true}]
	}`)
	out, _, truncated := RedactArgs(in, 4096, nil)
	if truncated {
		t.Fatal("small args were reported as truncated")
	}
	s := string(out)
	for _, secret := range []string{"sk-live-1234", "Bearer abc", "hunter2"} {
		if strings.Contains(s, secret) {
			t.Errorf("redacted output still contains %q: %s", secret, s)
		}
	}
	if !strings.Contains(s, "/tmp/x") || !strings.Contains(s, `"keep":1`) {
		t.Errorf("redaction removed non-credential values: %s", s)
	}
}

func TestRedactArgs_HonorsExtraKeys(t *testing.T) {
	in := json.RawMessage(`{"patient_name":"Jane","path":"/tmp/x"}`)
	out, _, _ := RedactArgs(in, 4096, []string{"patient"})
	if strings.Contains(string(out), "Jane") {
		t.Errorf("configured redact key was ignored: %s", out)
	}
	if !strings.Contains(string(out), "/tmp/x") {
		t.Errorf("configured redact key over-matched: %s", out)
	}
}

func TestRedactArgs_TruncatesOversizedArgs(t *testing.T) {
	big, err := json.Marshal(map[string]string{"blob": strings.Repeat("x", 500)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, size, truncated := RedactArgs(big, 100, nil)
	if !truncated {
		t.Fatal("oversized args were not flagged as truncated")
	}
	if size != len(big) {
		t.Errorf("recorded size = %d, want the original %d", size, len(big))
	}
	var v interface{}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("truncated args are not valid JSON: %v (%s)", err, out)
	}
	if _, ok := v.(string); !ok {
		t.Errorf("truncated args should be stored as a JSON string, got %T", v)
	}
}

func TestRedactArgs_MalformedJSONIsStoredAsText(t *testing.T) {
	out, _, _ := RedactArgs(json.RawMessage(`{not json`), 4096, nil)
	var v interface{}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("malformed args produced an unparseable record: %v", err)
	}
	if s, ok := v.(string); !ok || !strings.Contains(s, "not json") {
		t.Errorf("malformed args lost their content: %v", v)
	}
}

func TestTruncateRunes_DoesNotSplitMultibyte(t *testing.T) {
	// "é" is two bytes; cutting at 3 must drop it rather than halve it.
	got := TruncateRunes("aéb", 3)
	if got != "aé" {
		t.Errorf("TruncateRunes = %q, want %q", got, "aé")
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !json.Valid(encoded) {
		t.Error("truncated string does not encode as valid JSON")
	}
}

// The following four tests pin ADR-012: RedactArgs walks JSON as bytes and
// never decodes into a Go interface{}, so an unpaired UTF-16 surrogate, key
// order, duplicate keys and number spelling all survive untouched — every
// case below is something a decode/re-encode round trip would silently
// rewrite.

func TestRedactArgs_CopiesEverythingItIsNotRedacting(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"lone surrogate survives", `{"content":"a\ud800b"}`, `{"content":"a\ud800b"}`},
		{"key order survives", `{"zebra":1,"apple":2}`, `{"zebra":1,"apple":2}`},
		{"duplicate keys survive", `{"dir":"/safe","dir":"/etc"}`, `{"dir":"/safe","dir":"/etc"}`},
		{"number spelling survives", `{"n":1.0,"big":12345678901234567890}`, `{"n":1.0,"big":12345678901234567890}`},
		{"whitespace is the one rewrite", "{\n  \"a\" : 1\n}", `{"a":1}`},
		{"nested values survive", `{"o":{"content":"x\udc00"},"a":[1,"y\ud800"]}`, `{"o":{"content":"x\udc00"},"a":[1,"y\ud800"]}`},
		{"a bare scalar survives", `"a\ud800b"`, `"a\ud800b"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, size, truncated := RedactArgs(json.RawMessage(tc.in), 4096, nil)
			if truncated {
				t.Fatal("unexpectedly truncated")
			}
			if size != len(tc.in) {
				t.Errorf("size = %d, want %d (the size recorded is the size received)", size, len(tc.in))
			}
			if string(got) != tc.want {
				t.Errorf("RedactArgs =\n got  %s\n want %s", got, tc.want)
			}
		})
	}
}

func TestRedactArgs_StillReplacesCredentialValues(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"top level", `{"api_key":"sk-1","path":"/tmp"}`, `{"api_key":"[redacted]","path":"/tmp"}`},
		{"case and substring", `{"MyPassWord":"hunter2"}`, `{"MyPassWord":"[redacted]"}`},
		{"nested object", `{"cfg":{"token":"t","host":"h"}}`, `{"cfg":{"token":"[redacted]","host":"h"}}`},
		{"inside an array", `{"list":[{"secret":"s"},{"ok":1}]}`, `{"list":[{"secret":"[redacted]"},{"ok":1}]}`},
		{"whole subtree", `{"credentials":{"a":1,"b":2}}`, `{"credentials":"[redacted]"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, _ := RedactArgs(json.RawMessage(tc.in), 4096, nil)
			if string(got) != tc.want {
				t.Errorf("RedactArgs =\n got  %s\n want %s", got, tc.want)
			}
		})
	}

	got, _, _ := RedactArgs(json.RawMessage(`{"mailbox":"INBOX"}`), 4096, []string{"mailbox"})
	if string(got) != `{"mailbox":"[redacted]"}` {
		t.Errorf("extra redact key ignored: %s", got)
	}
}

func TestRedactArgs_StaysBoundedAndParseable(t *testing.T) {
	big := `{"content":"` + strings.Repeat("x", 5000) + `"}`
	got, size, truncated := RedactArgs(json.RawMessage(big), 128, nil)
	if !truncated {
		t.Fatal("an over-cap payload was not marked truncated")
	}
	if size != len(big) {
		t.Errorf("size = %d, want the full %d: the record says how much was sent, not how much was kept", size, len(big))
	}
	if len(got) > 160 {
		t.Errorf("capped record is %d bytes, well past the 128-byte cap", len(got))
	}
	var s string
	if err := json.Unmarshal(got, &s); err != nil {
		t.Fatalf("a truncated record must still be a valid JSON string, got %s: %v", got, err)
	}

	got, _, _ = RedactArgs(json.RawMessage(`{"broken":`), 4096, nil)
	if err := json.Unmarshal(got, &s); err != nil || s != `{"broken":` {
		t.Errorf("malformed arguments = %s, want them recorded verbatim as a JSON string", got)
	}
}

func TestRedactArgs_EdgeShapesStillParse(t *testing.T) {
	for _, in := range []string{
		`{}`,
		`[]`,
		`{"a":{}}`,
		`{"a\ud800b":1}`,
		`{"a\"b":1,"c\\":2}`,
		`{"api_ke\u0079A":"s"}`,
		`[[{"token":"t"}],{"n":[1,2]}]`,
		`null`,
		`{"deep":{"deeper":{"deepest":{"password":"p","ok":"o"}}}}`,
	} {
		got, _, truncated := RedactArgs(json.RawMessage(in), 4096, nil)
		if truncated {
			t.Errorf("%s: unexpectedly truncated", in)
			continue
		}
		if !json.Valid(got) {
			t.Errorf("%s: produced invalid JSON: %s", in, got)
		}
	}

	// An escape inside a key is compared case-insensitively as the DECODED
	// key, and written back as the bytes it arrived as.
	got, _, _ := RedactArgs(json.RawMessage(`{"api_ke\u0079A":"s"}`), 4096, nil)
	if string(got) != `{"api_ke\u0079A":"[redacted]"}` {
		t.Errorf("escaped key = %s, want the key's own bytes with the value redacted", got)
	}
	got, _, _ = RedactArgs(json.RawMessage(`{"deep":{"deeper":{"password":"p","ok":"o"}}}`), 4096, nil)
	if string(got) != `{"deep":{"deeper":{"password":"[redacted]","ok":"o"}}}` {
		t.Errorf("deep redaction = %s", got)
	}
}
