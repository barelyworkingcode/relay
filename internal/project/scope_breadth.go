package project

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
)

// Classification is a property of the string an operator typed, never of the
// field name (ADR-011 decision 3 refuses relay a registry of known field
// names): a false positive on a non-path field whose value happens to be
// literally "/" or "~" is accepted on purpose -- the answer is loud and
// wrong rather than quiet and wrong, and no value is refused on the
// strength of it.
const (
	ScopeBreadthBounded = ""
	ScopeBreadthHome    = "home"
	ScopeBreadthRoot    = "root"
	// ScopeBreadthWildcard is a resource-scope field's value of exactly
	// ["*"] (ADR-011 addendum, "A star and an empty array") -- a live,
	// per-call "every value this field could name", not a path, but the
	// same class of fact as ScopeBreadthRoot: a value that is not really a
	// confinement and must never be classified as one merely because
	// `disclose` was set to hide it.
	ScopeBreadthWildcard = "wildcard"
)

// scopeBreadthPhrase is the one phrasing of each finding, shared by the
// client scope note, the audit line, the operator CLI and the Settings UI --
// four surfaces independently wording the same fact is how an operator
// learns to read one of them as less serious than another.
func scopeBreadthPhrase(kind string) string {
	switch kind {
	case ScopeBreadthRoot:
		return "unrestricted (the whole filesystem)"
	case ScopeBreadthHome:
		return "a whole home directory"
	case ScopeBreadthWildcard:
		return "unrestricted (every value, resolved fresh on every call -- including one added after this grant was made)"
	}
	return ""
}

// ScopeEntryBreadth classifies one entry of a scope value.
//
// The test is structural, never a lookup of the running user's own home: a
// grant of ANOTHER user's home directory is exactly as broad as a grant of
// this one's, and a rule that consulted os.UserHomeDir would call the first
// bounded.
//
// filepath.Clean runs first because "/", "//", "/.." and
// "/Users/admin/../.." are one value spelled four ways, and a check that
// only knew the first spelling would be a check an operator could walk past
// by accident.
func ScopeEntryBreadth(entry string) string {
	v := strings.TrimSpace(entry)
	if v == "" {
		return ScopeBreadthBounded
	}
	// "~" is a home directory to every shell and to a good many MCPs. Relay
	// does not expand it and cannot know whether the MCP receiving it will,
	// so it is classified on the reading that matters if anything does.
	if v == "~" || v == "~/" {
		return ScopeBreadthHome
	}
	if !strings.HasPrefix(v, "/") {
		return ScopeBreadthBounded
	}
	clean := filepath.Clean(v)
	if clean == "/" {
		return ScopeBreadthRoot
	}
	// /Users, /home are every account's home (broader); /Users/<x>, /home/<x>
	// are one account's. Both get the same warning rather than a quieter one.
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	if len(parts) <= 2 && (parts[0] == "Users" || parts[0] == "home") {
		return ScopeBreadthHome
	}
	return ScopeBreadthBounded
}

func scopeValueEntries(raw json.RawMessage) []string {
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return list
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []string{s}
	}
	return nil
}

// ScopeValueBreadth returns the widest breadth any entry has, not the
// narrowest: a list is a union, so ["/Users/me/proj", "/"] reaches
// everything, and reporting only the first entry would describe the
// confinement the operator meant instead of the one in force.
//
// The wildcard is checked against the WHOLE value first, not per entry: it is
// recognised only as the array's sole element (ADR-011 addendum, "A star and
// an empty array" -- the same rule `ContextField.ValidateValue` enforces at
// save time, so a mixed array can never reach here already stored). A
// resource-scope field is never path-shaped, so there is no risk of this
// racing the root/home classification below for the same value.
func ScopeValueBreadth(raw json.RawMessage) string {
	entries := scopeValueEntries(raw)
	if len(entries) == 1 && entries[0] == ContextWildcardValue {
		return ScopeBreadthWildcard
	}
	widest := ScopeBreadthBounded
	for _, e := range entries {
		switch ScopeEntryBreadth(e) {
		case ScopeBreadthRoot:
			return ScopeBreadthRoot
		case ScopeBreadthHome:
			widest = ScopeBreadthHome
		}
	}
	return widest
}

// ScopeBreadthWarnings appends to a rendering of the scope values, never
// replaces one -- the coordinates are what the operator is entitled to, and
// a breadth warning is the second sentence, not a substitute for the first.
func ScopeBreadthWarnings(scope map[string]json.RawMessage) []string {
	names := make([]string, 0, len(scope))
	for name := range scope {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		if phrase := scopeBreadthPhrase(ScopeValueBreadth(scope[name])); phrase != "" {
			out = append(out, name+" is "+phrase)
		}
	}
	return out
}
