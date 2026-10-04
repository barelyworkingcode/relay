package logging_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// The checker supports exactly the keywords the line schema uses. Any other
// keyword is an error, so a schema edit cannot silently go unchecked.

func decodeNumbers(raw string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func checkSchema(schema map[string]any, v any, path string) error {
	for k := range schema {
		switch k {
		case "$schema", "$id", "title", "description":
		case "type", "required", "enum", "pattern", "maxLength", "minLength", "minimum", "additionalProperties", "properties":
		default:
			return fmt.Errorf("%s: unsupported schema keyword %q", path, k)
		}
	}
	if t, ok := schema["type"].(string); ok {
		if err := checkType(t, v); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	if enum, ok := schema["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if reflect.DeepEqual(e, v) {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("%s: %v not in enum %v", path, v, enum)
		}
	}
	switch val := v.(type) {
	case string:
		n := utf8.RuneCountInString(val)
		if p, ok := schema["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(val) {
			return fmt.Errorf("%s: %q does not match %s", path, val, p)
		}
		if m, ok := schema["maxLength"].(json.Number); ok {
			if lim, _ := m.Int64(); int64(n) > lim {
				return fmt.Errorf("%s: %d runes exceeds maxLength %d", path, n, lim)
			}
		}
		if m, ok := schema["minLength"].(json.Number); ok {
			if lim, _ := m.Int64(); int64(n) < lim {
				return fmt.Errorf("%s: %d runes under minLength %d", path, n, lim)
			}
		}
	case json.Number:
		if m, ok := schema["minimum"].(json.Number); ok {
			f, _ := val.Float64()
			lim, _ := m.Float64()
			if f < lim {
				return fmt.Errorf("%s: %v under minimum %v", path, val, lim)
			}
		}
	case map[string]any:
		return checkObject(schema, val, path)
	}
	return nil
}

func checkObject(schema map[string]any, obj map[string]any, path string) error {
	if req, ok := schema["required"].([]any); ok {
		for _, r := range req {
			if _, present := obj[r.(string)]; !present {
				return fmt.Errorf("%s: missing required key %q", path, r)
			}
		}
	}
	props, _ := schema["properties"].(map[string]any)
	for name, sub := range props {
		if val, present := obj[name]; present {
			if err := checkSchema(sub.(map[string]any), val, path+"."+name); err != nil {
				return err
			}
		}
	}
	for name, val := range obj {
		if _, declared := props[name]; declared {
			continue
		}
		switch ap := schema["additionalProperties"].(type) {
		case bool:
			if !ap {
				return fmt.Errorf("%s: additional property %q", path, name)
			}
		case map[string]any:
			if err := checkSchema(ap, val, path+"."+name); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkType(t string, v any) error {
	ok := false
	switch t {
	case "string":
		_, ok = v.(string)
	case "object":
		_, ok = v.(map[string]any)
	case "integer":
		if n, isNum := v.(json.Number); isNum {
			_, err := n.Int64()
			ok = err == nil
		}
	default:
		return fmt.Errorf("unsupported type %q", t)
	}
	if !ok {
		return fmt.Errorf("value %v is not of type %s", v, t)
	}
	return nil
}

func loadSchema(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/logging-schema.json")
	if err != nil {
		t.Fatal(err)
	}
	v, err := decodeNumbers(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return v.(map[string]any)
}

func validateLine(t *testing.T, line string) error {
	t.Helper()
	v, err := decodeNumbers(line)
	if err != nil {
		t.Fatalf("line is not JSON: %v: %q", err, line)
	}
	return checkSchema(loadSchema(t), v, "$")
}

func mustValidate(t *testing.T, line string) {
	t.Helper()
	if err := validateLine(t, line); err != nil {
		t.Errorf("line violates schema: %v\n%s", err, line)
	}
}

func TestCheckerKeywords(t *testing.T) {
	cases := []struct {
		name, schema, value string
		ok                  bool
	}{
		{"type string ok", `{"type":"string"}`, `"a"`, true},
		{"type string bad", `{"type":"string"}`, `1`, false},
		{"type integer ok", `{"type":"integer"}`, `3`, true},
		{"type integer rejects fraction", `{"type":"integer"}`, `1.5`, false},
		{"type object bad", `{"type":"object"}`, `"x"`, false},
		{"required ok", `{"required":["a"]}`, `{"a":1}`, true},
		{"required missing", `{"required":["a"]}`, `{"b":1}`, false},
		{"enum ok", `{"enum":["x","y"]}`, `"y"`, true},
		{"enum bad", `{"enum":["x","y"]}`, `"z"`, false},
		{"pattern ok", `{"pattern":"^a+$"}`, `"aaa"`, true},
		{"pattern bad", `{"pattern":"^a+$"}`, `"ab"`, false},
		{"maxLength counts runes ok", `{"maxLength":2}`, `"éé"`, true},
		{"maxLength bad", `{"maxLength":2}`, `"abc"`, false},
		{"minLength ok", `{"minLength":1}`, `"a"`, true},
		{"minLength bad", `{"minLength":1}`, `""`, false},
		{"minimum ok", `{"minimum":0}`, `0`, true},
		{"minimum bad", `{"minimum":0}`, `-1`, false},
		{"properties checks declared", `{"properties":{"a":{"type":"string"}}}`, `{"a":1}`, false},
		{"properties allows absent", `{"properties":{"a":{"type":"string"}}}`, `{}`, true},
		{"property named like a meta keyword is a property", `{"properties":{"title":{"type":"string"}}}`, `{"title":5}`, false},
		{"additionalProperties false rejects extra", `{"properties":{"a":{}},"additionalProperties":false}`, `{"a":1,"b":2}`, false},
		{"additionalProperties true allows extra", `{"properties":{"a":{}},"additionalProperties":true}`, `{"a":1,"b":2}`, true},
		{"meta keywords ignored", `{"$schema":"x","$id":"y","title":"t","description":"d","type":"string"}`, `"a"`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var schema map[string]any
			s, err := decodeNumbers(c.schema)
			if err != nil {
				t.Fatal(err)
			}
			schema = s.(map[string]any)
			v, err := decodeNumbers(c.value)
			if err != nil {
				t.Fatal(err)
			}
			if got := checkSchema(schema, v, "$") == nil; got != c.ok {
				t.Errorf("valid = %v, want %v", got, c.ok)
			}
		})
	}
}

func TestCheckerRejectsUnknownKeyword(t *testing.T) {
	for _, schema := range []string{
		`{"format":"date-time"}`,
		`{"oneOf":[{}]}`,
		`{"$ref":"#/x"}`,
		`{"properties":{"a":{"const":1}}}`,
		`{"type":"object","patternProperties":{}}`,
	} {
		s, err := decodeNumbers(schema)
		if err != nil {
			t.Fatal(err)
		}
		v, _ := decodeNumbers(`{"a":1}`)
		err = checkSchema(s.(map[string]any), v, "$")
		if err == nil || !strings.Contains(err.Error(), "unsupported schema keyword") {
			t.Errorf("schema %s: err = %v, want unsupported keyword error", schema, err)
		}
	}
}

func TestSchemaFileOnlyUsesSupportedKeywords(t *testing.T) {
	// The real schema, checked against a trivial value, trips on any keyword
	// the checker does not know.
	v, _ := decodeNumbers(`{}`)
	err := checkSchema(loadSchema(t), v, "$")
	if err != nil && strings.Contains(err.Error(), "unsupported schema keyword") {
		t.Fatal(err)
	}
}

func TestSchemaCopyMatchesDocs(t *testing.T) {
	a, err := os.ReadFile("testdata/logging-schema.json")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile("../../docs/logging-schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("internal/logging/testdata/logging-schema.json differs from docs/logging-schema.json")
	}
}
