package project

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
)

// A field name is an opaque map key to relay, start to finish: the vocabulary
// describes each field's role in the permission model, never its meaning, so
// there is deliberately no registry mapping known names to known handling.
// Vocabulary reference: docs/context-schema.md. Rationale: ADR-011 decision 3.

// ContextSchemaV2 is the first version carrying the v2 keywords; absent or
// lower means v1, handled by the allowed_dirs compatibility branch.
const ContextSchemaV2 = 2

const (
	// ContextScopeRestrict: there is no "absent" keyword for unrestricted
	// (ADR-011 decision 4) -- scope: "restrict" MEANS fail closed.
	ContextScopeRestrict = "restrict"

	ContextSourceOperator = "operator"

	// ContextSourceProjectPath: absent for an access profile (no path), so
	// by decision 4 the tools it governs refuse.
	ContextSourceProjectPath = "project_path"

	// ContextDiscloseValue is the default when disclose is absent.
	ContextDiscloseValue = "value"

	// ContextDiscloseCount renders a set field's SHAPE, never its content
	// (docs/context-schema.md).
	ContextDiscloseCount = "count"

	ContextDiscloseNone = "none"

	// ContextWildcardValue is the literal, single-element value an
	// operator-set restrict field can hold to mean "every value this field
	// could name, resolved fresh by the MCP on every call" -- never a
	// snapshot relay takes. ADR-011 decision 3 originally rejected a stored
	// wildcard outright; the addendum ("A star and an empty array") revisits
	// that for resource-scope fields specifically. Relay does not resolve
	// it, does not know what it means, and validates it exactly like any
	// other non-empty string -- the one exception is that it may not be
	// combined with a named value in the same array (see ValidateValue),
	// because a mixed array cannot be reviewed as "everything" and is not
	// treated as the wildcard by any MCP that implements this.
	ContextWildcardValue = "*"
)

// V1AllowedDirsField is the ONE domain-specific name left in relay
// (TestNoDomainSpecificFieldNames asserts it); v2 derives it instead.
const V1AllowedDirsField = "allowed_dirs"

// V1FsBashTool is the second domain-specific string, deliberately deferred
// rather than fixed (ADR-011: out of scope, not resource scoping).
const V1FsBashTool = "fs_bash"

// ContextField is one declared field of a v2 contextSchema: a JSON-Schema-
// ish validation fragment plus the ADR-011 keywords for what it's FOR.
type ContextField struct {
	// Name is the map key the field was declared under; opaque to relay.
	Name string `json:"-"`

	// A deliberate JSON-Schema SUBSET: array-of-string and string are what
	// the model needs, and a full implementation would be a second
	// validator to keep correct for no gain (see ValidateValue).
	Type        string          `json:"type,omitempty"`
	Items       json.RawMessage `json:"items,omitempty"`
	Description string          `json:"description,omitempty"`

	Scope      string   `json:"scope,omitempty"`
	Source     string   `json:"source,omitempty"`
	AppliesTo  []string `json:"applies_to,omitempty"`
	Enumerable bool     `json:"enumerable,omitempty"`
	DependsOn  []string `json:"depends_on,omitempty"`

	// Disclose governs only the CLIENT-FACING scope note; every other
	// surface always sees the real value.
	Disclose string `json:"disclose,omitempty"`
}

// The keyword keys, spelled as docs/context-schema.md defines.
const (
	ctxKeyType        = "type"
	ctxKeyItems       = "items"
	ctxKeyDescription = "description"
	ctxKeyScope       = "scope"
	ctxKeySource      = "source"
	ctxKeyAppliesTo   = "applies_to"
	ctxKeyEnumerable  = "enumerable"
	ctxKeyDependsOn   = "depends_on"
	ctxKeyDisclose    = "disclose"
)

// contextKeywordBySpelling maps a lower-cased keyword back to its one true
// spelling, so a near miss can be told from an unknown key.
var contextKeywordBySpelling = func() map[string]string {
	out := map[string]string{}
	for _, k := range []string{
		ctxKeyType, ctxKeyItems, ctxKeyDescription, ctxKeyScope,
		ctxKeySource, ctxKeyAppliesTo, ctxKeyEnumerable, ctxKeyDependsOn,
		ctxKeyDisclose,
	} {
		out[strings.ToLower(k)] = k
	}
	return out
}()

// UnmarshalJSON reads keywords under their EXACT key. Unlike
// readOnlyHintTrue, a near-miss here is an ERROR: it fails OPEN otherwise.
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
		func() error { return get(ctxKeyDisclose, &f.Disclose) },
	} {
		if err := step(); err != nil {
			return err
		}
	}
	// Items is carried as raw bytes; itemType() decodes it defensively, so
	// an unmodeled shape is declined rather than refused.
	if v, ok := raw[ctxKeyItems]; ok && string(v) != "null" {
		f.Items = v
	}

	// A value differing from a keyword only in case is refused the same as
	// a near-miss key above; an unrecognised value is ignored per decision 3.
	if err := nearMissValue(ctxKeyScope, f.Scope, ContextScopeRestrict); err != nil {
		return err
	}
	if err := nearMissValue(ctxKeySource, f.Source, ContextSourceOperator, ContextSourceProjectPath); err != nil {
		return err
	}
	return nearMissValue(ctxKeyDisclose, f.Disclose, ContextDiscloseValue, ContextDiscloseCount, ContextDiscloseNone)
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

func (f ContextField) Restricts() bool { return f.Scope == ContextScopeRestrict }

func (f ContextField) FromProjectPath() bool { return f.Source == ContextSourceProjectPath }

// FromOperator: a restrict-field with no declared source is treated as
// operator-supplied -- the reading relay cannot invent a value for.
func (f ContextField) FromOperator() bool {
	return f.Source == ContextSourceOperator || (f.Source == "" && f.Restricts())
}

// Disclosure reads absent or unrecognised as ContextDiscloseValue -- no
// separate "unknown" branch, so the rule cannot drift between two places.
func (f ContextField) Disclosure() string {
	switch f.Disclose {
	case ContextDiscloseCount, ContextDiscloseNone:
		return f.Disclose
	default:
		return ContextDiscloseValue
	}
}

// Governs: absent/empty applies_to, a malformed glob, and an empty ""
// entry all govern EVERYTHING, the fail-closed reading (ADR-011).
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

// matchToolPattern is THE tool-name matcher, shared so applies_to and
// allowed_tools cannot diverge; the error is returned since the two
// callers fail closed in OPPOSITE directions.
func matchToolPattern(pattern, toolName string) (bool, error) {
	return path.Match(pattern, toolName)
}

// toolAllowedByPatterns: an unparseable or over-broad pattern selects
// nothing, the opposite of ContextField.Governs.
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

// toolPatternProbes are synthetic names no MCP exposes, so a pattern
// whose only literal content is an ordinary character or two matches one.
var toolPatternProbes = []string{
	"zqx_abcdefghijklmnopqrstuvwxyz_0123456789",
	"ZQX-ABCDEFGHIJKLMNOPQRSTUVWXYZ.0123456789",
	"z",
}

// overBroadToolPattern: a pattern selects by shape, not name (ADR-011
// decision 2b), if it needs no literal character, or matches a probe.
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

// toolPatternLiteral strips wildcards and classes; the class scanner
// mirrors path.Match's own rules so what this reads as a class matches it.
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

// GovernsAll is ADR-011 decision 5's question. An empty tool list answers
// false, not vacuously true (an MCP relay never connected to).
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

// ValidateValue requires the KEY to be present with a well-formed value
// (ADR-011 decision 4: an absent key means every call it governs is
// refused) but, since the addendum "A star and an empty array", no longer
// refuses an explicit empty array outright for one that is `type: "array"`.
//
// An empty array used to be indistinguishable from "an operator forgot this
// field" and was refused for exactly that reason. It is not indistinguishable
// any more: relay's own editor only ever writes one through an explicit
// "confirm nothing to grant" action (never as a default, never silently), and
// a well-behaved MCP resolves it to "the confined set is empty" rather than
// treating it as absent. A hand-written `[]` from any other source reads the
// same way -- there is no way to tell them apart on the wire, and there does
// not need to be: both are "an operator (or a caller acting as one) looked
// and confirmed there is nothing here", which is the fact this validator's
// job is to let through, not to interrogate the origin of.
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
		if f.itemType() != "string" {
			return nil
		}
		strs := make([]string, 0, len(arr))
		for i, el := range arr {
			var s string
			if err := json.Unmarshal(el, &s); err != nil {
				return fmt.Errorf("%s[%d]: expected a string", f.Name, i)
			}
			if strings.TrimSpace(s) == "" {
				return fmt.Errorf("%s[%d]: expected a non-empty string", f.Name, i)
			}
			strs = append(strs, s)
		}
		// "*" is recognised only as the array's sole element (ADR-011
		// addendum). Mixed with a named value it cannot be reviewed as
		// "everything", so it is refused here rather than silently stored
		// as an inert literal that folds and matches nothing real.
		if len(strs) > 1 {
			for _, s := range strs {
				if s == ContextWildcardValue {
					return fmt.Errorf(
						"%s: \"*\" cannot be combined with a named value -- \"*\" means every value, on its "+
							"own; remove the named entries, or remove \"*\" and list the values instead", f.Name,
					)
				}
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

	// No declared type, or one outside the subset: presence is still
	// required, and an empty container is still not presence.
	if trimmed == "[]" || trimmed == "{}" || trimmed == `""` {
		return fmt.Errorf("%s: a non-empty value is required", f.Name)
	}
	return nil
}

// ContextSchema is a parsed contextSchema declaration. Fields is populated
// only for v2; a v1 schema keeps its Raw form (read via schemaHasField).
type ContextSchema struct {
	Version int
	Raw     json.RawMessage

	Fields []ContextField
	byName map[string]ContextField

	// Malformed names every declaration relay could not read. Non-empty
	// makes the whole schema UNUSABLE rather than partially applied.
	Malformed []string
}

// Usable is false when any field fragment failed to decode; the refusal is
// total (whole MCP, every grant, local projects included) -- ADR-011.
func (cs ContextSchema) Usable() bool { return len(cs.Malformed) == 0 }

func (cs ContextSchema) MalformedReason() string {
	return strings.Join(cs.Malformed, "; ")
}

func (cs ContextSchema) V2() bool { return cs.Version >= ContextSchemaV2 }

func (cs ContextSchema) Field(name string) (ContextField, bool) {
	f, ok := cs.byName[name]
	return f, ok
}

// RestrictFields returns fields in NAME order, not declaration order, so a
// scope note or audit line does not reshuffle between identical calls.
func (cs ContextSchema) RestrictFields() []ContextField {
	out := make([]ContextField, 0, len(cs.Fields))
	for _, f := range cs.Fields {
		if f.Restricts() {
			out = append(out, f)
		}
	}
	return out
}

func (cs ContextSchema) GoverningFields(toolName string) []ContextField {
	out := make([]ContextField, 0, len(cs.Fields))
	for _, f := range cs.RestrictFields() {
		if f.Governs(toolName) {
			out = append(out, f)
		}
	}
	return out
}

func (cs ContextSchema) ProjectPathFields() []ContextField {
	out := make([]ContextField, 0, len(cs.Fields))
	for _, f := range cs.RestrictFields() {
		if f.FromProjectPath() {
			out = append(out, f)
		}
	}
	return out
}

// v1DerivedField is v1's allowed_dirs in the v2 vocabulary -- the existing
// compatibility branch, not the field-name registry decision 3 rejects.
var v1DerivedField = ContextField{
	Name:   V1AllowedDirsField,
	Type:   "array",
	Scope:  ContextScopeRestrict,
	Source: ContextSourceProjectPath,
}

// derivedScopeFields returns restrict fields relay DERIVES rather than an
// operator supplying -- "can this ever be satisfied", not "has it yet".
func derivedScopeFields(cs ContextSchema) []ContextField {
	if cs.V2() {
		return cs.ProjectPathFields()
	}
	if schemaHasField(cs.Raw, V1AllowedDirsField) {
		return []ContextField{v1DerivedField}
	}
	return nil
}

// UnsatisfiableScopeField: a value that can NEVER be supplied, distinct
// from "not set yet". Remote-kind only; v1's absent allowed_dirs is
// UNRESTRICTED to fsMCP.
func UnsatisfiableScopeField(cs ContextSchema, isRemote bool, toolName string) (ContextField, bool) {
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

// AuditedScopeFields returns fields whose injected values belong on an
// audit record: every v2 restriction, or v1's derived field.
func AuditedScopeFields(cs ContextSchema) []ContextField {
	if cs.V2() {
		return cs.RestrictFields()
	}
	return derivedScopeFields(cs)
}

func (cs ContextSchema) OperatorFields() []ContextField {
	out := make([]ContextField, 0, len(cs.Fields))
	for _, f := range cs.RestrictFields() {
		if f.FromOperator() {
			out = append(out, f)
		}
	}
	return out
}

// ParseContextSchema parses the FLAT form (docs/context-schema.md); the
// nested JSON-Schema form is tolerated only as a rescue.
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
				// Adopted also when it found a fragment it could NOT read:
				// otherwise the malformed restrict field looks like no
				// restriction at all.
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

// parseContextFields: a non-object fragment is skipped, not reported --
// that silence is what makes the nested tolerance above work.
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

// isJSONObject checks only the first non-whitespace byte; an object
// malformed INSIDE must still reach the decode above to report its reason.
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

// ContextValues yields an empty map, not an error, for an absent, null, or
// non-object blob.
func ContextValues(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

// FilterKnownContextFields drops any stored key the MCP's LIVE schema no
// longer declares. v2 only: a v1 blob is always fully replaced.
func FilterKnownContextFields(base json.RawMessage, cs ContextSchema) json.RawMessage {
	if !cs.V2() {
		return base
	}
	values := ContextValues(base)
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

// Names every field this grant SETS a value for that the MCP's LIVE schema no
// longer declares. Dropping such a key on the way to the wire and dispatching
// anyway would assert a confinement in the profile without delivering it, so
// this refuses instead — the same answer Usable gives for a schema relay
// cannot read (ADR-011).
//
// Only a SET value counts; an empty one asserts no confinement. Every stored
// key is asked, not only those that were restrictions when written, because
// once the declaration is gone relay cannot tell which it was. V2 only: a v1
// blob is forwarded verbatim, which is the MCP's half of the bargain. A v2
// schema that decodes to no fields lands here too, since it would otherwise
// pass every scope-presence check while stripping every stored key.
func UnplaceableContextFields(cs ContextSchema, values map[string]json.RawMessage) []string {
	if !cs.V2() {
		return nil
	}
	var out []string
	for name := range values {
		if !HasScopeValue(values, name) {
			continue
		}
		if _, ok := cs.Field(name); ok {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func QuoteNames(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, strconv.Quote(n))
	}
	return strings.Join(quoted, ", ")
}

// HasScopeValue does NOT re-run ValidateValue -- emptiness must be caught
// here since a schema can grow a field after a grant was written.
//
// `[]` reads as absent here on purpose, and still does after the ADR-011
// addendum ("A star and an empty array"): this function backs two things
// that are correctly conservative about it --
// `unplaceableContextFields` ("an empty key asserts no confinement anybody
// could fail to deliver", which stays true: there is nothing left to
// enforce once the field is gone from the schema, `[]` or not) and
// `dependencyValues`'s enumerate-filter semantics, which are a DIFFERENT
// axis (a picker query, not an authorisation) and were never about decision
// 4 to begin with. Neither is where "is this field's own value a live
// authorisation" is decided -- see `HasScopeAssertion` for that question.
func HasScopeValue(values map[string]json.RawMessage, name string) bool {
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

// HasScopeAssertion is HasScopeValue's sibling for the one question ADR-011's
// addendum ("A star and an empty array") actually changes: whether a
// restrict field's own value is a live authorisation an operator (or a
// client acting on their behalf) is answerable for -- present-and-empty
// counts now, distinct from the key being absent altogether, which still
// does not. Two callers need exactly this: `checkScopePresence` (relay's own
// call-time gate -- a confirmed-empty field must not be denied the way an
// unset one is) and `ScopeNoteFor` (the client's "Scope: ..." note must not
// claim a tool is refused when a confirmed-empty grant lets it succeed
// emptily).
func HasScopeAssertion(values map[string]json.RawMessage, name string) bool {
	raw, ok := values[name]
	if !ok {
		return false
	}
	trimmed := strings.TrimSpace(string(raw))
	switch trimmed {
	case "", "null", "{}", `""`:
		return false
	}
	return true
}

// McpSurface is everything relay knows at runtime about one MCP bearing on
// permission derivation.
type McpSurface struct {
	Schema        json.RawMessage
	SchemaVersion int
	Tools         []string

	// Root is the resolved --root relay spawned this MCP with, if any --
	// from relay's own spawn config, not from anything the MCP declared.
	Root string
}

// McpSurfaces: a nil map or missing entry means relay has not connected --
// treated as "no schema" everywhere.
type McpSurfaces map[string]McpSurface

func (m McpSurfaces) Schema(mcpID string) ContextSchema {
	s := m[mcpID]
	return ParseContextSchema(s.Schema, s.SchemaVersion)
}

// ScopeNotePrefix marks a note relay appended, so ListTools and
// ListSkillBuckets rebuilding the same tool must not double-append.
const ScopeNotePrefix = "Scope: "

// scopeValueWithheld is shared by disclose: "none" and by disclose:
// "count" on a scalar, so the two cannot drift apart in phrasing.
const scopeValueWithheld = "set, value withheld"

// ScopeNoteFor: EVERY governing field is named, including one with no
// value -- omitting the one that disqualifies the tool would read as
// complete when it is not (decision 8); the unset message ignores disclose.
func ScopeNoteFor(cs ContextSchema, values map[string]json.RawMessage, toolName string) string {
	if !cs.V2() {
		return ""
	}
	var parts []string
	for _, f := range cs.GoverningFields(toolName) {
		label := f.Description
		if label == "" {
			label = f.Name
		}
		if !HasScopeAssertion(values, f.Name) {
			parts = append(parts, fmt.Sprintf("%s — no value is set for %q, so every call to this tool is refused", label, f.Name))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s — %s", label, renderScopeDisclosure(f, values[f.Name])))
	}
	if len(parts) == 0 {
		return ""
	}
	return ScopeNotePrefix + strings.Join(parts, "; ") + "."
}

// renderScopeDisclosure: a value reaching a filesystem ROOT, or a
// resource-scope field's wildcard, is named regardless of disclose. A HOME
// directory gets the opposite treatment deliberately -- naming it discloses
// host topology the client does not otherwise learn. The wildcard is grouped
// with root rather than with home: it discloses nothing about this host a
// client could not already learn by calling the field's own enumerator, and
// withholding "this reaches everything, including what is added later" would
// hide the one fact a client confined by disclose is most entitled to.
func renderScopeDisclosure(f ContextField, raw json.RawMessage) string {
	breadth := ScopeValueBreadth(raw)
	alwaysNamed := breadth == ScopeBreadthRoot || breadth == ScopeBreadthWildcard
	switch f.Disclosure() {
	case ContextDiscloseCount:
		if alwaysNamed {
			return scopeBreadthPhrase(breadth)
		}
		return renderScopeCount(raw)
	case ContextDiscloseNone:
		if alwaysNamed {
			return scopeBreadthPhrase(breadth)
		}
		return scopeValueWithheld
	default:
		if alwaysNamed {
			return scopeBreadthPhrase(breadth) + ": " + renderScopeValue(raw)
		}
		return renderScopeValue(raw)
	}
}

// renderScopeCount describes a value's SHAPE (entry count), never its
// content. A scalar renders identically to disclose: "none".
func renderScopeCount(raw json.RawMessage) string {
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		unit := "value"
		if len(list) != 1 {
			unit = "values"
		}
		return fmt.Sprintf("confined to %d %s", len(list), unit)
	}
	return scopeValueWithheld
}

// renderScopeValue prints arrays of strings as "a, b"; anything else falls
// back to its compact JSON, honest about a shape relay does not model.
//
// A present, empty array is the confirmed-empty grant (ADR-011 addendum, "A
// star and an empty array") and gets its own phrase rather than falling
// through to the literal characters "[]" -- which is what an operator would
// otherwise read on the client's own tools/list, indistinguishable from a
// rendering bug.
func renderScopeValue(raw json.RawMessage) string {
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		if len(list) == 0 {
			return "confirmed empty -- confined to nothing"
		}
		return strings.Join(list, ", ")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

func AppendScopeNote(desc, note string) string {
	if note == "" || strings.Contains(desc, note) {
		return desc
	}
	if desc == "" {
		return note
	}
	return desc + " " + note
}

// ScopeFieldView projects one restrict field for the Settings UI's
// permission panel; Source is normalised here so FromOperator's rule
// lives in one place. No Disclose: the operator sees the real value.
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

// ScopeFields returns an empty slice, not a missing key, for an MCP that
// declares none, so a UI can tell that from "never heard of this MCP".
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
