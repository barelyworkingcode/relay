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
func (f ContextField) Governs(toolName string) bool {
	if len(f.AppliesTo) == 0 {
		return true
	}
	for _, pattern := range f.AppliesTo {
		if pattern == "" {
			continue
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

	fields := parseContextFields(top)
	if !anyRestricts(fields) {
		if nested, ok := top["properties"]; ok {
			var props map[string]json.RawMessage
			if err := json.Unmarshal(nested, &props); err == nil {
				if alt := parseContextFields(props); anyRestricts(alt) {
					fields = alt
				}
			}
		}
	}

	cs.Fields = fields
	cs.byName = make(map[string]ContextField, len(fields))
	for _, f := range fields {
		cs.byName[f.Name] = f
	}
	return cs
}

func parseContextFields(obj map[string]json.RawMessage) []ContextField {
	names := make([]string, 0, len(obj))
	for name := range obj {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]ContextField, 0, len(names))
	for _, name := range names {
		var f ContextField
		// A fragment that is not an object declares nothing relay can act on
		// (a bare "type": "object" sibling key, say). Skipped rather than
		// carried as an empty field.
		if err := json.Unmarshal(obj[name], &f); err != nil {
			continue
		}
		f.Name = name
		out = append(out, f)
	}
	return out
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
