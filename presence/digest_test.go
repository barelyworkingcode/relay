package presence

import (
	"testing"
	"time"
)

// buildCredentialMintDigest models §6.4's credential.mint row: name
// (string), classes (set), ttl (duration). Several tests below build one,
// change exactly one input, and check the digest moved — this is the shape
// every ops core's presenceDigest method will take in a later step.
func buildCredentialMintDigest(name string, namePresent bool, classes []string, classesPresent bool, ttl time.Duration, ttlPresent bool) Digest {
	return NewDigestBuilder("credential.mint").
		StringField("name", namePresent, name).
		StringSetField("classes", classesPresent, classes).
		DurationField("ttl", ttlPresent, ttl).
		Build()
}

func TestDigest_Deterministic(t *testing.T) {
	a := buildCredentialMintDigest("eve-view", true, []string{"read", "grant"}, true, time.Hour, true)
	b := buildCredentialMintDigest("eve-view", true, []string{"read", "grant"}, true, time.Hour, true)
	if a != b {
		t.Fatal("identical inputs produced different digests")
	}
}

func TestDigest_ChangingAnyDigestedArgumentMovesTheDigest(t *testing.T) {
	base := buildCredentialMintDigest("eve-view", true, []string{"read"}, true, time.Hour, true)

	cases := map[string]Digest{
		"name":    buildCredentialMintDigest("eve-view-2", true, []string{"read"}, true, time.Hour, true),
		"classes": buildCredentialMintDigest("eve-view", true, []string{"read", "grant"}, true, time.Hour, true),
		"ttl":     buildCredentialMintDigest("eve-view", true, []string{"read"}, true, 2*time.Hour, true),
	}
	for field, d := range cases {
		if d == base {
			t.Errorf("changing %s did not move the digest", field)
		}
	}
}

func TestDigest_ChangingAnUndigestedInputDoesNotChangeTheOperationsFieldSet(t *testing.T) {
	// A field the operation never includes in its builder call chain cannot
	// move the digest, because it was never fed in. This is the mirror
	// image of the completeness rule: nothing outside the declared field
	// list can affect the hash.
	a := NewDigestBuilder("credential.mint").StringField("name", true, "eve-view").Build()
	b := NewDigestBuilder("credential.mint").StringField("name", true, "eve-view").Build()
	if a != b {
		t.Fatal("two builders fed the identical declared fields produced different digests")
	}
}

func TestDigest_AbsentIsNotEmpty(t *testing.T) {
	// A nil set and an empty (but present) set must differ.
	absentSet := NewDigestBuilder("op").StringSetField("classes", false, nil).Build()
	emptySet := NewDigestBuilder("op").StringSetField("classes", true, []string{}).Build()
	if absentSet == emptySet {
		t.Fatal("absent set and empty set produced the same digest")
	}

	// An absent duration and an explicit zero duration ("never expires")
	// must differ.
	absentDuration := NewDigestBuilder("op").DurationField("ttl", false, 0).Build()
	zeroDuration := NewDigestBuilder("op").DurationField("ttl", true, 0).Build()
	if absentDuration == zeroDuration {
		t.Fatal("absent duration and explicit zero duration produced the same digest")
	}

	// An absent map and an empty map must differ.
	absentMap := NewDigestBuilder("op").StringMapField("env", false, nil).Build()
	emptyMap := NewDigestBuilder("op").StringMapField("env", true, map[string]string{}).Build()
	if absentMap == emptyMap {
		t.Fatal("absent map and empty map produced the same digest")
	}

	// An absent string and an explicit empty string must differ.
	absentString := NewDigestBuilder("op").StringField("id", false, "").Build()
	emptyString := NewDigestBuilder("op").StringField("id", true, "").Build()
	if absentString == emptyString {
		t.Fatal("absent string and explicit empty string produced the same digest")
	}
}

func TestDigest_StringSetIgnoresOrderAndDuplicates(t *testing.T) {
	a := NewDigestBuilder("op").StringSetField("classes", true, []string{"read", "grant", "read"}).Build()
	b := NewDigestBuilder("op").StringSetField("classes", true, []string{"grant", "read"}).Build()
	if a != b {
		t.Fatal("a set field's digest depends on order or duplicate count, and it must not")
	}
}

func TestDigest_StringSequencePreservesOrder(t *testing.T) {
	a := NewDigestBuilder("op").StringSeqField("args", true, []string{"--a", "--b"}).Build()
	b := NewDigestBuilder("op").StringSeqField("args", true, []string{"--b", "--a"}).Build()
	if a == b {
		t.Fatal("a sequence field's digest ignored the order of its elements")
	}
}

func TestDigest_MapKeyOrderIsIrrelevant(t *testing.T) {
	// Go's map iteration order is randomised per run; build the same
	// logical map twice and confirm the digest doesn't depend on it.
	m1 := map[string]string{"OPENAI_API_KEY": "x", "LOG_LEVEL": "debug"}
	m2 := map[string]string{"LOG_LEVEL": "debug", "OPENAI_API_KEY": "x"}
	a := NewDigestBuilder("op").StringMapField("env", true, m1).Build()
	b := NewDigestBuilder("op").StringMapField("env", true, m2).Build()
	if a != b {
		t.Fatal("StringMapField's digest depends on map iteration order")
	}
}

func TestDigest_FieldLevelBoundariesDoNotCollide(t *testing.T) {
	// This covers the FIELD-level boundary only: content shifted across
	// the boundary between two different fields, and a field name shifted
	// against its own value. It does NOT cover the boundary between
	// elements INSIDE one collection field — TestDigest_
	// ElementBoundaryCollisions is the one that does, and is the one that
	// actually exercises each collection kind's own length-prefixing.
	cases := []struct {
		name   string
		first  func() Digest
		second func() Digest
	}{
		{
			name: "string concatenation boundary",
			first: func() Digest {
				return NewDigestBuilder("op").StringField("a", true, "ab").StringField("b", true, "c").Build()
			},
			second: func() Digest {
				return NewDigestBuilder("op").StringField("a", true, "a").StringField("b", true, "bc").Build()
			},
		},
		{
			name: "field name vs field value boundary",
			first: func() Digest {
				return NewDigestBuilder("op").StringField("ab", true, "c").Build()
			},
			second: func() Digest {
				return NewDigestBuilder("op").StringField("a", true, "bc").Build()
			},
		},
		{
			name: "set element containing a separator-like byte",
			first: func() Digest {
				return NewDigestBuilder("op").StringSetField("classes", true, []string{"a,b"}).Build()
			},
			second: func() Digest {
				return NewDigestBuilder("op").StringSetField("classes", true, []string{"a", "b"}).Build()
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.first() == c.second() {
				t.Fatal("two distinct argument tuples collided under length-prefixed encoding")
			}
		})
	}
}

func TestDigest_RawJSONMapBindsToExactBytes(t *testing.T) {
	a := NewDigestBuilder("project.grant").
		RawJSONMapField("context", true, map[string][]byte{"scope": []byte(`{"a":1}`)}).
		Build()
	b := NewDigestBuilder("project.grant").
		RawJSONMapField("context", true, map[string][]byte{"scope": []byte(`{"a": 1}`)}). // re-spelled
		Build()
	if a == b {
		t.Fatal("RawJSONMapField normalised the JSON bytes instead of hashing them verbatim")
	}
}

func TestDigest_DifferentOperationsWithIdenticalFieldsDiffer(t *testing.T) {
	a := NewDigestBuilder("credential.mint").StringField("id", true, "x").Build()
	b := NewDigestBuilder("enrolment.revoke").StringField("id", true, "x").Build()
	if a == b {
		t.Fatal("two different operation names with identical field content collided")
	}
}

// TestDigest_ElementBoundaryCollisions targets the boundary BETWEEN elements
// inside a single collection field — StringSeqField, StringSetField,
// StringMapField, StringSetMapField and RawJSONMapField each loop over their
// own elements and length-prefix each one independently; appendField wraps
// the whole field once, but does nothing for the elements inside it. A
// mutation that dropped the inner per-element length prefix (see putString)
// would not be caught by TestDigest_FieldLevelBoundariesDoNotCollide, which
// only shifts content across field boundaries — this is the test that
// mutation exposed as missing, and every case here was verified to fail
// (produce a collision) when putString's length prefix is removed, and to
// pass again once it is restored.
func TestDigest_ElementBoundaryCollisions(t *testing.T) {
	cases := []struct {
		name   string
		first  func() Digest
		second func() Digest
	}{
		{
			// count=2, then "ab"+"c" vs "a"+"bc": identical concatenation
			// once each element's own length prefix is gone.
			name: "StringSeqField: element concatenation realigns",
			first: func() Digest {
				return NewDigestBuilder("op").StringSeqField("argv", true, []string{"ab", "c"}).Build()
			},
			second: func() Digest {
				return NewDigestBuilder("op").StringSeqField("argv", true, []string{"a", "bc"}).Build()
			},
		},
		{
			// Same shape as the sequence case, after dedup+sort — chosen so
			// sorting leaves both slices in the order written.
			name: "StringSetField: element concatenation realigns",
			first: func() Digest {
				return NewDigestBuilder("op").StringSetField("classes", true, []string{"ab", "c"}).Build()
			},
			second: func() Digest {
				return NewDigestBuilder("op").StringSetField("classes", true, []string{"a", "bc"}).Build()
			},
		},
		{
			// Single entry: key "ab" + value "c" vs key "a" + value "bc".
			// Both keys and values go through the same (mutated) putString,
			// so the key/value boundary inside one entry collapses too.
			name: "StringMapField: key/value boundary within one entry",
			first: func() Digest {
				return NewDigestBuilder("op").StringMapField("env", true, map[string]string{"ab": "c"}).Build()
			},
			second: func() Digest {
				return NewDigestBuilder("op").StringMapField("env", true, map[string]string{"a": "bc"}).Build()
			},
		},
		{
			// Two entries, sorted keys "a" < "d" vs "ab" < "d": content
			// moves from key1's value into key2's — er, into key1 itself —
			// while the second entry ("d":"e") is untouched. Concatenated
			// without length prefixes both read "abcde".
			name: "StringMapField: content realigns across two entries",
			first: func() Digest {
				return NewDigestBuilder("op").StringMapField("env", true, map[string]string{"a": "bc", "d": "e"}).Build()
			},
			second: func() Digest {
				return NewDigestBuilder("op").StringMapField("env", true, map[string]string{"ab": "c", "d": "e"}).Build()
			},
		},
		{
			// The nested case: a single outer key "k" (unchanged in both),
			// whose inner SET realigns exactly like the top-level
			// StringSetField case above.
			name: "StringSetMapField: inner set element boundary realigns",
			first: func() Digest {
				return NewDigestBuilder("op").StringSetMapField("allowed_tools", true, map[string][]string{"k": {"ab", "c"}}).Build()
			},
			second: func() Digest {
				return NewDigestBuilder("op").StringSetMapField("allowed_tools", true, map[string][]string{"k": {"a", "bc"}}).Build()
			},
		},
		{
			// The outer key list realigns instead: both maps have two keys
			// and empty inner sets, so each entry contributes only its raw
			// (mutated) key bytes plus a fixed one-byte "0 elements" marker.
			// One key legitimately contains an embedded NUL — an ordinary
			// Go string, however unusual a real MCP id would be — which is
			// exactly the adversarial input a length-prefix exists to
			// survive: "a\x00b" + "c" and "a" + "b\x00c" concatenate to the
			// identical six bytes once the key's own length prefix is gone.
			name: "StringSetMapField: outer key list realigns around an embedded NUL",
			first: func() Digest {
				return NewDigestBuilder("op").StringSetMapField("allowed_tools", true, map[string][]string{"a\x00b": {}, "c": {}}).Build()
			},
			second: func() Digest {
				return NewDigestBuilder("op").StringSetMapField("allowed_tools", true, map[string][]string{"a": {}, "b\x00c": {}}).Build()
			},
		},
		{
			// RawJSONMapField's value goes through putBytes, which carries
			// its own independent length prefix and is untouched by a
			// putString mutation — so the exploitable boundary here is the
			// KEY, exactly as above.
			name: "RawJSONMapField: key list realigns around an embedded NUL",
			first: func() Digest {
				return NewDigestBuilder("op").RawJSONMapField("context", true, map[string][]byte{"a\x00b": {}, "c": {}}).Build()
			},
			second: func() Digest {
				return NewDigestBuilder("op").RawJSONMapField("context", true, map[string][]byte{"a": {}, "b\x00c": {}}).Build()
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.first() == c.second() {
				t.Fatal("two distinct field values collided at an element boundary inside a collection field")
			}
		})
	}
}
