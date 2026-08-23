package main

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// The context-schema vocabulary (ADR-011 decision 3)
// ---------------------------------------------------------------------------
//
// Relay must store, inject, render and refuse a scoping value without knowing
// what it scopes. That is possible only because a v2 contextSchema describes
// each field's ROLE IN THE PERMISSION MODEL rather than its meaning:
//
//	scope       "restrict"                      this field narrows access
//	source      "operator" | "project_path"     who supplies the value
//	applies_to  []string of tool-name globs     which tools it governs
//	enumerable  bool                            the MCP can list valid values
//	depends_on  []string of field names         enumeration ordering
//
// Relay learns "this field restricts access, an operator sets it, and it
// governs mail_*". It never learns what a mailbox is. Every field name is an
// opaque map key from relay's side, start to finish. The rejected alternative
// is a registry inside relay mapping known field names to known handling —
// which is what schemaHasField(…, "allowed_dirs") is today in miniature, and
// it does not survive a second MCP.

// contextSchemaV2 is the first contextSchemaVersion that carries those
// keywords. An absent or lower version means v1 and is handled exactly as it
// was before ADR-011 — the literal allowed_dirs branch, for one release.
const contextSchemaV2 = 2

// Keyword values. Each is a small closed set; anything else is ignored rather
// than guessed at, because a keyword relay does not understand must not be
// able to widen anything.
const (
	// ContextScopeRestrict marks a field that narrows access. Its absence
	// means an ordinary context value relay injects and otherwise ignores.
	//
	// There is deliberately no "absent" keyword letting an MCP say a missing
	// value means unrestricted: a field that says it restricts and then
	// defaults open is not a restriction, and relay could never verify the
	// claim either way. scope: "restrict" MEANS fail closed.
	ContextScopeRestrict = "restrict"

	// ContextSourceOperator — an operator sets the value explicitly. Local
	// and remote alike; nothing about a mail account depends on the caller
	// having a filesystem.
	ContextSourceOperator = "operator"

	// ContextSourceProjectPath — relay derives the value from Project.Path.
	// An access profile (a remote-kind record) has no path, so such a field
	// is ABSENT for one, and by decision 4 the tools it governs refuse.
	ContextSourceProjectPath = "project_path"
)

// v1AllowedDirsField is the ONE domain-specific field name left anywhere in
// relay, and it is here so that fact is checkable: TestNoDomainSpecificFieldNames
// asserts the literal appears in exactly one non-test Go source. It exists only
// for the v1 compatibility branch — an MCP that declares no contextSchemaVersion,
// which is fsMCP as shipped today — and is scheduled for removal one release
// after every MCP relay serves declares v2.
//
// Under v2 the same MCP declares the same field with source: "project_path",
// and relay derives it because the SCHEMA asked for it, not because relay
// recognised the name.
const v1AllowedDirsField = "allowed_dirs"

// v1FsBashTool is the second domain-specific string, and it is DEFERRED
// rather than fixed: ADR-011 names moving this into the schema (as a
// default_disabled_tools declaration) as out of scope, because it is the same
// ADR-006 violation but it is not resource scoping. Until then relay keeps
// auto-disabling this one tool for filesystem-scoped MCPs, exactly as before.
const v1FsBashTool = "fs_bash"

// ContextField is one declared field of a v2 contextSchema: the ordinary
// JSON-Schema-ish fragment relay needs to validate a value, plus the ADR-011
// keywords that tell relay what the field is FOR.
type ContextField struct {
	// Name is the map key the field was declared under. It is opaque to
	// relay and is what gets written into _meta.
	Name string `json:"-"`

	// The value-shape fragment. A deliberate JSON-Schema SUBSET: array-of-string
	// and string are what the model needs, and a full implementation would be a
	// second validator to keep correct for no gain (see ValidateValue).
	Type        string          `json:"type,omitempty"`
	Items       json.RawMessage `json:"items,omitempty"`
	Description string          `json:"description,omitempty"`

	// The permission-model keywords.
	Scope      string   `json:"scope,omitempty"`
	Source     string   `json:"source,omitempty"`
	AppliesTo  []string `json:"applies_to,omitempty"`
	Enumerable bool     `json:"enumerable,omitempty"`
	DependsOn  []string `json:"depends_on,omitempty"`
}

// The keyword keys of a field fragment, in the spelling docs/context-schema.md
// defines. They are constants so the exactness below is one value each rather
// than a string literal someone later "tidies", which is the same reason
// mcpReadOnlyHintKey and mcpOpenWorldHintKey are constants.
const (
	ctxKeyType        = "type"
	ctxKeyItems       = "items"
	ctxKeyDescription = "description"
	ctxKeyScope       = "scope"
	ctxKeySource      = "source"
	ctxKeyAppliesTo   = "applies_to"
	ctxKeyEnumerable  = "enumerable"
	ctxKeyDependsOn   = "depends_on"
)

// contextKeywordBySpelling maps a keyword's lower-cased form back to the one
// spelling that is the keyword, so a NEAR MISS can be told from a key relay
// simply does not know.
var contextKeywordBySpelling = func() map[string]string {
	out := map[string]string{}
	for _, k := range []string{
		ctxKeyType, ctxKeyItems, ctxKeyDescription, ctxKeyScope,
		ctxKeySource, ctxKeyAppliesTo, ctxKeyEnumerable, ctxKeyDependsOn,
	} {
		out[strings.ToLower(k)] = k
	}
	return out
}()

// UnmarshalJSON decodes one field fragment by reading each keyword out of a
// map UNDER ITS EXACT KEY, rather than letting encoding/json match the struct's
// fields.
//
// This is readOnlyHintTrue's discipline applied where it was missing, and the
// reason it is needed here is the same: encoding/json matches struct fields
// CASE-INSENSITIVELY, so a plain decode of this struct accepted
// {"Scope":"restrict"} as a restriction — a key no schema document defines —
// while {"scope":"RESTRICT"} silently was not one. Two spellings a reviewer
// reading an MCP's published schema would read identically, decided opposite
// ways, with no signal either time.
//
// The direction that matters is NOT the same as the annotation hints', and
// that is why "read it exactly" is not the whole rule here. A near-miss
// readOnlyHint that relay ignores DENIES, so ignoring it is safe. A near-miss
// `scope` that relay ignores means relay stops requiring a value for a field
// the MCP believes is a restriction, stops governing the tools it names, and
// (through filterKnownContextFields, which drops any key the parsed schema no
// longer declares) strips the operator's value off the wire. Silence is the
// fail-OPEN direction on this side.
//
// So the rule is: an EXACT keyword is read, a key or value relay has never
// heard of is ignored exactly as decision 3 says it must be, and a NEAR MISS
// of a keyword — differing only in case — is an error. An error here makes the
// whole schema unusable (see ContextSchema.Malformed), which is loud, closed,
// and the only answer that does not require relay to guess which of two
// readings an MCP author meant.
func (f *ContextField) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*f = ContextField{}

	for key := range raw {
		if want, ok := contextKeywordBySpelling[strings.ToLower(key)]; ok && key != want {
			return fmt.Errorf("declares %q, which is not the keyword %q — keywords are read under their exact spelling, so a near miss is refused rather than guessed at", key, want)
		}
	}

	get := func(key string, dst any) error {
		v, ok := raw[key]
		if !ok || string(v) == "null" {
			return nil
		}
		if err := json.Unmarshal(v, dst); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		return nil
	}
	for _, step := range []func() error{
		func() error { return get(ctxKeyType, &f.Type) },
		func() error { return get(ctxKeyDescription, &f.Description) },
		func() error { return get(ctxKeyScope, &f.Scope) },
		func() error { return get(ctxKeySource, &f.Source) },
		func() error { return get(ctxKeyAppliesTo, &f.AppliesTo) },
		func() error { return get(ctxKeyEnumerable, &f.Enumerable) },
		func() error { return get(ctxKeyDependsOn, &f.DependsOn) },
	} {
		if err := step(); err != nil {
			return err
		}
	}
	// Items is carried through as raw bytes: itemType() is the only reader and
	// it decodes defensively, so a fragment relay does not model is a shape it
	// declines to describe rather than a schema it refuses.
	if v, ok := raw[ctxKeyItems]; ok && string(v) != "null" {
		f.Items = v
	}

	// A keyword VALUE gets the same treatment its key does. An unrecognised
	// value is ignored — decision 3's "anything else is ignored rather than
	// guessed at", which is what lets a later vocabulary land without breaking
	// this one — but a value that differs from a keyword only in case is a typo
	// of THIS vocabulary, not a member of a future one, and reading it as
	// "no scope" is the fail-open answer.
	if err := nearMissValue(ctxKeyScope, f.Scope, ContextScopeRestrict); err != nil {
		return err
	}
	return nearMissValue(ctxKeySource, f.Source, ContextSourceOperator, ContextSourceProjectPath)
}

func nearMissValue(key, got string, want ...string) error {
	for _, w := range want {
		if got == w {
			return nil
		}
		if strings.EqualFold(got, w) {
			return fmt.Errorf("%s is %q, which is not the keyword %q — keyword values are read under their exact spelling", key, got, w)
		}
	}
	return nil
}

// Restricts reports whether this field narrows access.
func (f ContextField) Restricts() bool { return f.Scope == ContextScopeRestrict }

// FromProjectPath reports whether relay derives this field's value from the
// project's path.
func (f ContextField) FromProjectPath() bool { return f.Source == ContextSourceProjectPath }

// FromOperator reports whether an operator supplies this field's value.
// A restrict-field with no declared source is treated as operator-supplied:
// that is the reading that leaves the value un-derivable by relay, which is
// the safe one — relay inventing a value for a field it does not understand
// is the failure this whole mechanism exists to prevent.
func (f ContextField) FromOperator() bool {
	return f.Source == ContextSourceOperator || (f.Source == "" && f.Restricts())
}

// Governs reports whether this field's applies_to selects the named tool.
//
// An ABSENT or empty applies_to governs every tool the MCP exposes. That is
// the domain-blind default: an MCP that offers no precision gets the widest
// reading, and one that offers precision gets exactly what it declared.
//
// A malformed glob governs everything too. path.Match rejects e.g. an
// unterminated character class, and the two readings of that are "governs
// nothing" and "governs everything" — the second is the fail-closed one
// (more tools require a value, and a grant whose MCP publishes a broken
// pattern is refused rather than silently unscoped), so it is the one taken.
//
// An EMPTY entry governs everything for exactly that reason, and it used to be
// skipped. applies_to: [""] therefore made a field that declares itself a
// restriction govern no tool at all, while still being reported as declared
// everywhere an operator or a client looks — a restriction that restricts
// nothing, which is the one thing scope: "restrict" is documented to be unable
// to mean (see ContextScopeRestrict: there is deliberately no keyword letting
// an MCP say a missing value is unrestricted, and a spelling that achieves it
// by accident is the same hole through a side door). "" names no tool, exactly
// as an unparseable pattern does, so it takes the same reading; and because
// one entry governing everything makes the whole list govern everything, a
// stray "" beside a real "mail_*" widens the restriction rather than voiding
// the list.
func (f ContextField) Governs(toolName string) bool {
	if len(f.AppliesTo) == 0 {
		return true
	}
	for _, pattern := range f.AppliesTo {
		if pattern == "" {
			return true
		}
		ok, err := matchToolPattern(pattern, toolName)
		if err != nil {
			return true
		}
		if ok {
			return true
		}
	}
	return false
}

// matchToolPattern is THE tool-name matcher. Both places that select tools by
// pattern go through it — a context field's applies_to (which tools a scope
// governs) and an access profile's allowed_tools (which tools it may call) —
// because two matchers with slightly different anchoring is how "mail_* does
// not admit xmail_send" ends up true in one place and false in the other.
//
// It is path.Match, which is ANCHORED: the pattern must match the whole name.
// So "mail_*" matches "mail_send" and not "xmail_send", and a pattern with no
// metacharacter is an exact match. Tool names contain no "/", so path.Match's
// one separator rule never comes into play.
//
// The error is returned rather than swallowed because the two callers must
// fail closed in OPPOSITE directions, and only they know which way that is: an
// unparseable applies_to governs everything (more tools need a value), an
// unparseable allowed_tools entry admits nothing.
func matchToolPattern(pattern, toolName string) (bool, error) {
	return path.Match(pattern, toolName)
}

// toolAllowedByPatterns reports whether any pattern in the list selects the
// tool. An unparseable pattern selects nothing — the fail-closed direction for
// an allowlist, and the opposite of ContextField.Governs, which is the
// fail-closed direction for a restriction.
//
// An OVER-BROAD pattern selects nothing either, and that is enforcement
// agreeing with validation rather than trusting it. validateToolPattern
// refuses one on save, but a record that acquired one by a route validation
// did not cover — a hand-edited settings.json, a restored backup, a migration
// that predates the rule — must not thereby hold every tool its MCPs expose.
// A grant is only as good as the weakest way into the file that holds it.
//
// Note the deliberate asymmetry with the denylist in checkToolAccess, which is
// honoured wherever it came from: ignoring a denylist is the direction that
// widens, and ignoring an over-broad allowlist entry is the direction that
// narrows. Both rules are "prefer the smaller grant"; they only look opposite.
func toolAllowedByPatterns(patterns []string, toolName string) bool {
	for _, pattern := range patterns {
		if pattern == "" {
			continue
		}
		if _, over := overBroadToolPattern(pattern); over {
			continue
		}
		if ok, err := matchToolPattern(pattern, toolName); err == nil && ok {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// What makes a tool pattern too broad to be an allowlist entry
// ---------------------------------------------------------------------------
//
// ADR-011 decision 2b refuses a bare "*" in allowed_tools, because registering
// a tool tomorrow would silently widen a grant made today. That refusal used
// to be a literal string compare against "*" while the matcher underneath was
// path.Match — and a tool name contains no "/", so "**", "?*", "*_*", "[a-z]*"
// and "*e*" all match EVERY tool of an MCP and none of them is the string "*".
// Measured: a read-only "mail" profile written with allowed_tools ["**"] held
// 26 tools across 11 of macMCP's domains, web_fetch among them, which restores
// the whole outbound channel decision 2b exists to remove.
//
// So the refusal cannot be a list of spellings — the next spelling slips
// through the same way. It has to be a property of the MATCHER, asked as: does
// this pattern select tools by NAME, or by SHAPE? Two questions answer that,
// and a pattern is refused if either does:
//
//  1. Does it require any literal character at all? A pattern built only from
//     "*", "?" and character classes constrains nothing about a name — it is a
//     statement about length and alphabet, and every tool name satisfies it.
//     "*", "**", "?*" and "[a-z]*" all fail here.
//
//  2. Does it match a name that is not a tool? A pattern whose literal content
//     is one or two ordinary characters ("*_*", "*e*") is naming a substring
//     every plausible identifier carries, which is selection by shape wearing
//     a letter as a disguise. Probing it against names no MCP exposes is what
//     tells the two apart, and it needs no knowledge of the MCP's tool list —
//     which relay does not have at validation time and must not depend on
//     anyway, since the whole point is the tool that does not exist yet.
//
// What survives is what an operator actually means: "mail_*", "mail_search",
// "capture_screen*". What does not is anything that would still match after
// the MCP grows a domain.

// toolPatternProbes are names no MCP exposes. They are deliberately not
// plausible tool names and deliberately wide — every letter of both cases,
// every digit, and the separators identifiers use — so that a pattern whose
// only literal content is an ordinary character or two matches one of them.
//
// The alphabet runs in order on purpose: no real tool-name fragment ("mail",
// "list", "get", "send") appears as a substring of it, so a pattern naming a
// real fragment is not caught by accident. They contain no "/" because a tool
// name contains none and path.Match's separator rule must stay out of this.
var toolPatternProbes = []string{
	"zqx_abcdefghijklmnopqrstuvwxyz_0123456789",
	"ZQX-ABCDEFGHIJKLMNOPQRSTUVWXYZ.0123456789",
	// One character, so "?" and "?*" are answered too.
	"z",
}

// overBroadToolPattern reports whether a pattern selects tools by shape rather
// than by name, and returns the reason in the voice a refusal can use.
//
// It is only ever asked about an ALLOWLIST entry. A context field's applies_to
// runs through the same matcher and is deliberately NOT filtered by this: a
// field that governs everything is a restriction that applies to everything,
// which is the fail-closed reading there (see ContextField.Governs). The same
// pattern is over-broad in one list and exactly right in the other.
func overBroadToolPattern(pattern string) (string, bool) {
	if toolPatternLiteral(pattern) == "" {
		return "it requires no literal character at all, so it selects every tool the MCP has by shape rather than naming any", true
	}
	for _, probe := range toolPatternProbes {
		if ok, err := matchToolPattern(pattern, probe); err == nil && ok {
			return fmt.Sprintf("it matches %q, which is no tool of any MCP — a pattern that matches a name like that is matching by structure, so it would take in whatever an MCP is given tomorrow", probe), true
		}
	}
	return "", false
}

// toolPatternLiteral returns the characters a pattern requires literally, with
// the wildcards, the character classes and path.Match's backslash escapes
// removed. A character class contributes nothing: it constrains which
// characters may appear at a position, never that any particular one does.
//
// The class scanner mirrors path.Match's own — "^" negates, and a "]" in the
// first position is a member rather than the terminator — so that what this
// reads as a class is what the matcher reads as a class. An unterminated class
// runs to the end of the pattern here; such a pattern does not compile and is
// refused before this answer is used for anything.
func toolPatternLiteral(pattern string) string {
	var lit strings.Builder
	for i := 0; i < len(pattern); {
		switch c := pattern[i]; c {
		case '*', '?':
			i++
		case '\\':
			i++
			if i < len(pattern) {
				lit.WriteByte(pattern[i])
				i++
			}
		case '[':
			i++
			if i < len(pattern) && pattern[i] == '^' {
				i++
			}
			for first := true; i < len(pattern); first = false {
				if pattern[i] == ']' && !first {
					i++
					break
				}
				if pattern[i] == '\\' {
					i++
				}
				i++
			}
		default:
			lit.WriteByte(c)
			i++
		}
	}
	return lit.String()
}

// GovernsAll reports whether this field governs every tool in the given list.
// This is the question ADR-011 decision 5 turns grant validation into: a field
// whose value cannot be supplied makes every tool it governs refuse, so a
// field that governs all of them leaves the MCP with nothing usable.
//
// An empty tool list answers false, not true. "This MCP exposes no tools" is
// what an MCP relay has never connected to looks like, and vacuous truth there
// would refuse a grant on the strength of missing information.
func (f ContextField) GovernsAll(toolNames []string) bool {
	if len(toolNames) == 0 {
		return false
	}
	for _, name := range toolNames {
		if !f.Governs(name) {
			return false
		}
	}
	return true
}

// itemType returns the declared type of an array's elements, or "".
func (f ContextField) itemType() string {
	if len(f.Items) == 0 {
		return ""
	}
	var items struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(f.Items, &items); err != nil {
		return ""
	}
	return items.Type
}

// ValidateValue checks a candidate value against the declared fragment.
//
// This is a JSON-Schema SUBSET on purpose — array-of-string and string are the
// shapes the model needs, and everything else is accepted as long as it is
// present and non-empty. What it will never do is accept an EMPTY value for a
// restrict-field: ADR-011 decision 4 makes absent and empty both refusals on
// all three sides, so "no restriction" is not expressible as emptiness and a
// stored [] would be a grant that reads as confined and is not.
func (f ContextField) ValidateValue(raw json.RawMessage) error {
	trimmed := strings.TrimSpace(string(raw))
	if len(trimmed) == 0 || trimmed == "null" {
		return fmt.Errorf("%s: a value is required (an absent value means every call it governs is refused)", f.Name)
	}

	switch f.Type {
	case "array":
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return fmt.Errorf("%s: expected an array of values", f.Name)
		}
		if len(arr) == 0 {
			return fmt.Errorf("%s: expected at least one value (an empty list means every call it governs is refused, which is not how to say \"no restriction\")", f.Name)
		}
		if f.itemType() != "string" {
			return nil
		}
		for i, el := range arr {
			var s string
			if err := json.Unmarshal(el, &s); err != nil {
				return fmt.Errorf("%s[%d]: expected a string", f.Name, i)
			}
			if strings.TrimSpace(s) == "" {
				return fmt.Errorf("%s[%d]: expected a non-empty string", f.Name, i)
			}
		}
		return nil

	case "string":
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Errorf("%s: expected a string", f.Name)
		}
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("%s: expected a non-empty string", f.Name)
		}
		return nil
	}

	// No declared type, or one outside the subset. Presence is still required;
	// an empty container still is not presence.
	if trimmed == "[]" || trimmed == "{}" || trimmed == `""` {
		return fmt.Errorf("%s: a non-empty value is required", f.Name)
	}
	return nil
}

// ContextSchema is a parsed contextSchema declaration.
//
// Fields is populated only for v2. A v1 schema keeps its Raw form and is read
// through schemaHasField, which is what "handled exactly as today" means: no
// v2 rule can fire on a declaration that never opted into the vocabulary, even
// if it happens to carry a key spelled like one of the keywords.
type ContextSchema struct {
	Version int
	Raw     json.RawMessage

	Fields []ContextField
	byName map[string]ContextField

	// Malformed names every declaration relay could not read, one entry per
	// field, each already carrying its reason. A non-empty list makes the
	// whole schema UNUSABLE rather than partially applied — see Usable.
	Malformed []string
}

// Usable reports whether relay can act on this declaration at all.
//
// It is false when any field fragment failed to decode, and the consequence is
// deliberately total: relay refuses every call to that MCP and lists none of
// its tools, for every grant.
//
// The alternative — what this used to do — was to drop the field that would
// not parse and apply the rest, silently. That is fail-open twice over for the
// one kind of field that matters. A restrict field relay does not hold is a
// field relay does not REQUIRE A VALUE FOR (checkScopePresence never asks about
// it) and does not GOVERN A TOOL BY (Governs is never consulted), and
// filterKnownContextFields then drops the operator's stored value on the way to
// the wire because the parsed schema no longer declares that name. One type
// slip in one fragment — `"applies_to": "mail_*"` written as a string — and
// relay stops enforcing a confinement, strips the value that expressed it, and
// says nothing to anybody: not to the operator, not to the client, not to the
// MCP author who made the typo.
//
// The whole schema rather than the one field, because a fragment relay could
// not read is a fragment relay cannot bound: the field it failed on may have
// been the one governing everything, and "apply the parts I understood" is a
// claim about the parts it did not. This is the same reading ParseContextSchema
// gives a malformed glob and ContextField.Governs gives an empty applies_to —
// when the declaration is unreadable, take the widest restriction, not the
// narrowest.
//
// It bites a local project too, and that is not an oversight: the signal an MCP
// author needs is one they cannot miss, and a rule that only fired for remote
// grants would let a broken declaration sit unnoticed on a developer's own
// machine until the day it was granted to a client. checkScopePresence declines
// the local/remote asymmetry for the same reason and says so at length.
func (cs ContextSchema) Usable() bool { return len(cs.Malformed) == 0 }

// MalformedReason renders what could not be read, for a refusal and for the log
// line finalizeConnection writes when the schema arrives.
func (cs ContextSchema) MalformedReason() string {
	return strings.Join(cs.Malformed, "; ")
}

// V2 reports whether this schema declared the ADR-011 vocabulary.
func (cs ContextSchema) V2() bool { return cs.Version >= contextSchemaV2 }

// Field returns the named field.
func (cs ContextSchema) Field(name string) (ContextField, bool) {
	f, ok := cs.byName[name]
	return f, ok
}

// RestrictFields returns every field declaring scope: "restrict", in name
// order. Name order rather than declaration order because a JSON object has no
// declaration order to preserve, and a stable one is what keeps a scope note
// and an audit line from reshuffling between two identical calls.
func (cs ContextSchema) RestrictFields() []ContextField {
	out := make([]ContextField, 0, len(cs.Fields))
	for _, f := range cs.Fields {
		if f.Restricts() {
			out = append(out, f)
		}
	}
	return out
}

// GoverningFields returns every restrict-field that governs the named tool.
func (cs ContextSchema) GoverningFields(toolName string) []ContextField {
	out := make([]ContextField, 0, len(cs.Fields))
	for _, f := range cs.RestrictFields() {
		if f.Governs(toolName) {
			out = append(out, f)
		}
	}
	return out
}

// ProjectPathFields returns every restrict-field relay derives from the
// project's path.
func (cs ContextSchema) ProjectPathFields() []ContextField {
	out := make([]ContextField, 0, len(cs.Fields))
	for _, f := range cs.RestrictFields() {
		if f.FromProjectPath() {
			out = append(out, f)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// The two questions that have to be asked of a v1 schema as well (ADR-011
// decisions 4, 5 and 7)
// ---------------------------------------------------------------------------
//
// ContextSchema.Fields is populated for v2 ONLY, deliberately: no v2 rule may
// fire on a declaration that never opted into the vocabulary. That invariant is
// right and is pinned by a test — but it left v1 with no call-time defence at
// all and no audit record, because every consumer spelled its own `!cs.V2()`
// early return and stopped there.
//
// The two rules below are the ones that must not stop there, and each is
// written once for both versions rather than twice. Everything else about v1
// is unchanged.

// v1DerivedField is v1's allowed_dirs said in the v2 vocabulary: a restriction
// whose value relay derives from the project's path, governing every tool
// (v1 has no applies_to, so there is nothing to narrow it with).
//
// This is not the "registry of known field names" decision 3 rejects. It is the
// v1 compatibility branch that already exists — see v1AllowedDirsField, which
// is the one domain-specific name left in relay and is scheduled for removal —
// expressed so that the rules below can be written once instead of once per
// version. Under v2 the same MCP declares the same field with
// source: "project_path" and none of this is consulted.
var v1DerivedField = ContextField{
	Name:   v1AllowedDirsField,
	Type:   "array",
	Scope:  ContextScopeRestrict,
	Source: ContextSourceProjectPath,
}

// derivedScopeFields returns every restrict field whose value relay DERIVES
// from the record rather than an operator supplying it.
//
// A record with no path cannot have one derived for it, which is what makes
// this the question "can this grant ever satisfy that field" rather than "has
// it yet" — see unsatisfiableScopeField.
func derivedScopeFields(cs ContextSchema) []ContextField {
	if cs.V2() {
		return cs.ProjectPathFields()
	}
	if schemaHasField(cs.Raw, v1AllowedDirsField) {
		return []ContextField{v1DerivedField}
	}
	return nil
}

// unsatisfiableScopeField returns a restrict field governing toolName whose
// value can NEVER be supplied for this record's kind, and is the difference
// between the two shapes of "this tool has no scope value".
//
//   - Not set YET — an operator field on a record that could hold one. The tool
//     stays listed and the call is refused loudly, because a `denied` naming
//     the missing field is more diagnostic to an operator than silent absence.
//   - Can NEVER be set — a source: "project_path" field on an access profile,
//     which has no path. SyncProjectToken will not derive one, the editor
//     refuses one typed by hand, and there is no configuration under which the
//     tool works. A client must not be shown a capability it cannot have.
//
// It answers only for a remote-kind record: a local project has a path, so
// SyncProjectToken derives the value and the field is always satisfiable.
//
// It is also the second defence decision 5 asks for, in the direction the
// first one cannot cover. SyncProjectToken never DERIVES such a value for a
// remote record — but nothing removes one written into settings.json by hand,
// and for a v1 MCP nothing at call time looked at it either: the value was
// injected and honoured. Refusing here is not "strip the value and let the
// presence check deny", because for v1 an absent allowed_dirs is exactly what
// fsMCP reads as UNRESTRICTED, which is ADR-009's original finding. The call
// is refused; nothing goes on the wire.
func unsatisfiableScopeField(cs ContextSchema, isRemote bool, toolName string) (ContextField, bool) {
	if !isRemote {
		return ContextField{}, false
	}
	for _, f := range derivedScopeFields(cs) {
		if f.Governs(toolName) {
			return f, true
		}
	}
	return ContextField{}, false
}

// auditedScopeFields returns the fields whose injected values belong on an
// audit record: every declared restriction under v2, and v1's derived field.
//
// ADR-011 decision 7's property is that the log answers what was attempted with
// what authority, and for a v1 MCP that answer used to be `scope: null` on
// every record — including a call relay had confined with a value it injected
// itself. "This MCP has no scope concept" and "this call carried one" were the
// same line.
func auditedScopeFields(cs ContextSchema) []ContextField {
	if cs.V2() {
		return cs.RestrictFields()
	}
	return derivedScopeFields(cs)
}

// OperatorFields returns every restrict-field an operator must supply. Phase-2
// operator surfaces (the editor, the enumeration picker) work from this list.
func (cs ContextSchema) OperatorFields() []ContextField {
	out := make([]ContextField, 0, len(cs.Fields))
	for _, f := range cs.RestrictFields() {
		if f.FromOperator() {
			out = append(out, f)
		}
	}
	return out
}

// ParseContextSchema turns a raw contextSchema plus its declared version into
// the parsed form.
//
// The shape is fixed as the FLAT form — {fieldName: {fragment}} — and
// documented (docs/context-schema.md), because issue #17 showed the ambiguity
// between that and the nested JSON-Schema form is live and its failure
// direction is fail-open. The nested form is still tolerated here, but only as
// a rescue: it is consulted when the flat reading found no restrict-field at
// all, and only adopted when the nested one does. Missing a restrict-field is
// the failure that matters — the grant is then permitted and nothing is
// enforced — so the tolerance runs in the fail-closed direction only.
func ParseContextSchema(raw json.RawMessage, version int) ContextSchema {
	cs := ContextSchema{Version: version, Raw: raw}
	if len(raw) == 0 || !cs.V2() {
		return cs
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return cs
	}

	fields, bad := parseContextFields(top)
	if !anyRestricts(fields) {
		if nested, ok := top["properties"]; ok {
			var props map[string]json.RawMessage
			if err := json.Unmarshal(nested, &props); err == nil {
				// The rescue is adopted when the nested reading finds a
				// restriction the flat one missed — and ALSO when the nested
				// reading found a fragment it could not read. The second is
				// the fail-closed half: a nested document whose one restrict
				// field is the malformed one presents, from out here, as a
				// document with no restrictions at all, which is exactly the
				// silence Usable exists to break.
				alt, altBad := parseContextFields(props)
				if anyRestricts(alt) || len(altBad) > 0 {
					fields = alt
					bad = append(bad, altBad...)
				}
			}
		}
	}

	cs.Malformed = bad
	cs.Fields = fields
	cs.byName = make(map[string]ContextField, len(fields))
	for _, f := range fields {
		cs.byName[f.Name] = f
	}
	return cs
}

// parseContextFields decodes each entry of a schema object into a field, and
// returns alongside them the entries it could NOT decode.
//
// The two failures have to be told apart, and telling them apart is the whole
// of the function:
//
//   - A fragment that is not a JSON object at all declares nothing relay could
//     act on. That is a sibling key of a nested JSON-Schema document — the
//     `"type": "object"` beside `"properties"` — and skipping it is what makes
//     ParseContextSchema's nested tolerance work at all. Not an error.
//   - A fragment that IS an object and still would not decode is a declaration
//     relay could not read: a type slip inside it (`"applies_to": "mail_*"`),
//     or a keyword spelled a case off (ContextField.UnmarshalJSON). Silently
//     dropping one of those is how relay stops enforcing a restriction, and
//     strips its value, with nobody told. It is reported, and Usable turns the
//     report into a refusal.
func parseContextFields(obj map[string]json.RawMessage) (fields []ContextField, malformed []string) {
	names := make([]string, 0, len(obj))
	for name := range obj {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]ContextField, 0, len(names))
	for _, name := range names {
		if !isJSONObject(obj[name]) {
			continue
		}
		var f ContextField
		if err := json.Unmarshal(obj[name], &f); err != nil {
			malformed = append(malformed, fmt.Sprintf("field %q %v", name, err))
			continue
		}
		f.Name = name
		out = append(out, f)
	}
	return out, malformed
}

// isJSONObject reports whether raw is a JSON object, by its first
// non-whitespace byte. json.Valid is not consulted: an object that is
// malformed INSIDE must reach the decode above so its reason can be reported,
// and this question is only ever "is this shaped like a declaration".
func isJSONObject(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return strings.HasPrefix(trimmed, "{")
}

func anyRestricts(fields []ContextField) bool {
	for _, f := range fields {
		if f.Restricts() {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Context values
// ---------------------------------------------------------------------------

// contextValues decodes a project's per-MCP context blob into its fields.
// A blob that is absent, null, or not an object yields an empty map rather
// than an error: every caller's next question is "is there a value for field
// X", and the answer for all three is no.
func contextValues(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

// filterKnownContextFields drops any key from a stored context blob that cs
// — the MCP's LIVE schema — does not currently declare, so a value stored
// under a field name an MCP has since renamed or dropped is never injected
// into _meta under that stale name. This is the call-time half of the stale-
// key problem SyncProjectToken's doc comment describes: relay does not
// rewrite settings.json when a schema changes underneath a stored grant
// (doing that from a possibly-empty live schema would be indistinguishable
// from an MCP that is merely down declaring nothing, and would delete an
// operator's values on the strength of that), so the safe place to enforce
// "no unknown key reaches the wire" is here, against the schema of an MCP
// CallTool has already confirmed is live.
//
// Restricted to v2: a v1 schema's context blob is always exactly
// {v1AllowedDirsField: [...]}, fully replaced by SyncProjectToken on every
// resync, so there is no drift to filter and nothing else can be stored there
// (validateProjectContextForMcp refuses it).
func filterKnownContextFields(base json.RawMessage, cs ContextSchema) json.RawMessage {
	if !cs.V2() {
		return base
	}
	values := contextValues(base)
	if values == nil {
		return base
	}
	kept := make(map[string]json.RawMessage, len(values))
	for name, v := range values {
		if _, ok := cs.Field(name); ok {
			kept[name] = v
		}
	}
	out, err := json.Marshal(kept)
	if err != nil {
		return base
	}
	return out
}

// hasScopeValue reports whether the context blob carries a usable value for
// the field — present, non-null, and non-empty, which is the whole of what
// decision 4 requires relay to check at call time. It deliberately does NOT
// re-run ValidateValue: a type mismatch is the operator surface's problem to
// refuse on save, whereas emptiness is the one that has to be caught here
// because a schema can grow a field after a grant was already written.
func hasScopeValue(values map[string]json.RawMessage, name string) bool {
	raw, ok := values[name]
	if !ok {
		return false
	}
	trimmed := strings.TrimSpace(string(raw))
	switch trimmed {
	case "", "null", "[]", "{}", `""`:
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// What relay knows about a live MCP
// ---------------------------------------------------------------------------

// McpSurface is everything relay knows at runtime about one MCP that bears on
// permission derivation: the contextSchema it declared at handshake, that
// schema's version, and the tools it currently exposes.
//
// The three travel together because every question ADR-011 asks needs at
// least two of them. "Would this grant leave the MCP with no usable tools"
// (decision 5) needs the schema AND the tool list; "is this a v1 schema"
// needs the schema AND the version. Passing them as three parallel maps is
// how the versions and the tool list would end up plumbed to some call sites
// and not others.
type McpSurface struct {
	Schema        json.RawMessage
	SchemaVersion int
	Tools         []string
}

// McpSurfaces maps MCP id to its surface. A nil map, or a missing entry,
// means relay has not connected to that MCP and knows nothing about it —
// which every consumer here treats as "no schema", matching the pre-ADR-011
// contract that a nil schemas map skips derivation rather than failing closed.
type McpSurfaces map[string]McpSurface

// Schema returns the parsed context schema for an MCP.
func (m McpSurfaces) Schema(mcpID string) ContextSchema {
	s := m[mcpID]
	return ParseContextSchema(s.Schema, s.SchemaVersion)
}

// ToolNames returns the tool names an MCP currently exposes.
func (m McpSurfaces) ToolNames(mcpID string) []string { return m[mcpID].Tools }

// ---------------------------------------------------------------------------
// The scope note (ADR-011 decision 8)
// ---------------------------------------------------------------------------

// scopeNotePrefix marks a note relay appended, so appending is idempotent.
// ListTools and ListSkillBuckets each build their own copy of a tool from the
// same live list, and the skill renderer reads the second — the two must not
// double-append, and the cheapest way to guarantee that is to make the second
// append a no-op rather than to reason about who calls whom.
const scopeNotePrefix = "Scope: "

// scopeNoteFor builds the one-sentence note describing how a tool is confined,
// from the schema field's OWN description and the operator's value. Returns ""
// when the tool is governed by nothing, or when nothing has a value.
//
// A client is told its own limits through ListTools because renderBucketSkillMd
// — the obvious place — is the wrong ONLY place: access profiles have no
// skills (validateProjectShape refuses GenerateSkill), so the agent this
// feature exists for would never see it.
func scopeNoteFor(cs ContextSchema, values map[string]json.RawMessage, toolName string) string {
	if !cs.V2() {
		return ""
	}
	var parts []string
	for _, f := range cs.GoverningFields(toolName) {
		raw, ok := values[f.Name]
		if !ok || !hasScopeValue(values, f.Name) {
			continue
		}
		label := f.Description
		if label == "" {
			label = f.Name
		}
		parts = append(parts, fmt.Sprintf("%s — %s", label, renderScopeValue(raw)))
	}
	if len(parts) == 0 {
		return ""
	}
	return scopeNotePrefix + strings.Join(parts, "; ") + "."
}

// renderScopeValue prints a scope value for a human reading a tool
// description. Arrays of strings become "a, b"; anything else is its compact
// JSON, which is honest about a shape relay does not model.
func renderScopeValue(raw json.RawMessage) string {
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil && len(list) > 0 {
		return strings.Join(list, ", ")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

// appendScopeNote adds the note to a description if it is not already there.
func appendScopeNote(desc, note string) string {
	if note == "" || strings.Contains(desc, note) {
		return desc
	}
	if desc == "" {
		return note
	}
	return desc + " " + note
}

// ---------------------------------------------------------------------------
// The operator's view of a schema (ADR-011 decision 6)
// ---------------------------------------------------------------------------

// ScopeFieldView is one declared scope: "restrict" field, projected for an
// operator surface: the Settings UI's per-MCP permission panel, and the same
// panel eve renders over the HTTP routes.
//
// It carries ONLY restrict fields. The panel is a permission editor, and an
// ordinary context value (a field with no `scope`) is something relay injects
// and otherwise ignores — showing it beside the values that decide what a
// client may reach would put two different things under one heading.
//
// Source is NORMALISED here rather than passed through: ContextField.FromOperator
// reads an absent source as operator-supplied, and that rule must be applied in
// exactly one place. A surface that re-derived it from a raw "" would be a
// second copy of the rule, free to disagree the day it changes.
type ScopeFieldView struct {
	Name        string   `json:"name"`
	Type        string   `json:"type,omitempty"`
	ItemType    string   `json:"item_type,omitempty"`
	Description string   `json:"description,omitempty"`
	Source      string   `json:"source"`
	AppliesTo   []string `json:"applies_to,omitempty"`
	Enumerable  bool     `json:"enumerable,omitempty"`
	DependsOn   []string `json:"depends_on,omitempty"`
}

// ScopeFieldViews projects a schema's restrict fields for an operator surface,
// in the same name order RestrictFields uses.
func (cs ContextSchema) ScopeFieldViews() []ScopeFieldView {
	fields := cs.RestrictFields()
	out := make([]ScopeFieldView, 0, len(fields))
	for _, f := range fields {
		source := ContextSourceOperator
		if f.FromProjectPath() {
			source = ContextSourceProjectPath
		}
		out = append(out, ScopeFieldView{
			Name:        f.Name,
			Type:        f.Type,
			ItemType:    f.itemType(),
			Description: f.Description,
			Source:      source,
			AppliesTo:   f.AppliesTo,
			Enumerable:  f.Enumerable,
			DependsOn:   f.DependsOn,
		})
	}
	return out
}

// ScopeFields returns the operator-facing scope fields for every MCP relay
// knows about, keyed by MCP id. An MCP that declares none gets an empty slice
// rather than a missing key, so a UI can tell "this MCP scopes nothing" from
// "relay has never heard of this MCP" — the second is the case where a panel
// must say it cannot show the fields rather than that there are none.
func (m McpSurfaces) ScopeFields() map[string][]ScopeFieldView {
	out := make(map[string][]ScopeFieldView, len(m))
	for id := range m {
		views := m.Schema(id).ScopeFieldViews()
		if views == nil {
			views = []ScopeFieldView{}
		}
		out[id] = views
	}
	return out
}
