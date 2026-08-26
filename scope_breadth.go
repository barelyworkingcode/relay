package main

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// How much of the host one scope value actually reaches (issue #41)
// ---------------------------------------------------------------------------
//
// A COUNT IS NOT A MEASURE OF CONFINEMENT. `disclose: "count"` (issue #33)
// closed a real disclosure — relay used to render an MCP's allowed directories
// into every tool description and a live agent read the sandbox root out of one
// — but it rendered `allowed_dirs: ["/"]` and `allowed_dirs: ["/Users/me/proj"]`
// as the same eleven bytes: "confined to 1 value". One of those confines a
// client to a folder and the other hands it /etc/passwd and ~/.ssh, and the
// first verification step relay's own operator guide names could not tell them
// apart.
//
// So relay asks a second question of a value beside "how many entries" — "how
// much does an entry reach" — and every surface that renders a scope asks it.
//
// This is deliberately a question about the VALUE and never about the field
// name. ADR-011 decision 3 refuses relay a registry of known field names, and
// this does not smuggle one back in: `allowed_dirs`, `file_dirs` and a field
// named by an MCP written next year are all read the same way, because the
// classification is a property of the string an operator typed. The cost is a
// false positive on a non-path field whose value happens to be literally "/"
// or "~" — a mail account so named would be announced as unrestricted. That is
// the fail direction to take: the answer is loud and wrong rather than quiet
// and wrong, and no value is refused on the strength of it.

// The breadth kinds, widest last, so a comparison in that order is the
// "widest wins" rule every caller needs.
const (
	// scopeBreadthBounded is an ordinary value: it names something inside the
	// host rather than the host.
	scopeBreadthBounded = ""
	// scopeBreadthHome is a home directory tree. Not unrestricted — it does
	// not reach /etc or another user — but it is the operator's own mail,
	// keys, browser profile and every project on the machine, which is not
	// what "confined to 1 value" brings to mind either.
	scopeBreadthHome = "home"
	// scopeBreadthRoot is a filesystem root: the whole machine.
	scopeBreadthRoot = "root"
)

// scopeBreadthPhrase is the ONE phrasing of each finding, shared by the client
// scope note, the audit line, the operator CLI and (mirrored) the Settings UI.
// One constant per kind because four surfaces saying the same fact four ways
// is how an operator learns to read one of them as less serious than another.
func scopeBreadthPhrase(kind string) string {
	switch kind {
	case scopeBreadthRoot:
		return "unrestricted (the whole filesystem)"
	case scopeBreadthHome:
		return "a whole home directory"
	}
	return ""
}

// scopeEntryBreadth classifies one entry of a scope value.
//
// The test is structural, never a lookup of the running user's own home: a
// grant of ANOTHER user's home directory is exactly as broad as a grant of
// this one's, and a rule that consulted os.UserHomeDir would call the first
// bounded. It is also pure, which is what lets the same rule be asserted in a
// unit test and mirrored in the Settings UI's JavaScript.
//
// Cleaning first is the point rather than an optimisation: "/", "//", "/.." and
// "/Users/admin/../.." are one value spelled four ways, and a check that only
// knew the first spelling would be a check an operator could walk past by
// accident.
func scopeEntryBreadth(entry string) string {
	v := strings.TrimSpace(entry)
	if v == "" {
		return scopeBreadthBounded
	}
	// "~" is a home directory to every shell and to a good many MCPs. Relay
	// does not expand it and cannot know whether the MCP receiving it will,
	// so it is classified on the reading that matters if anything does.
	if v == "~" || v == "~/" {
		return scopeBreadthHome
	}
	if !strings.HasPrefix(v, "/") {
		return scopeBreadthBounded
	}
	clean := filepath.Clean(v)
	if clean == "/" {
		return scopeBreadthRoot
	}
	// /Users, /home, /Users/<someone>, /home/<someone>. The two-deep forms are
	// one account's whole home; the one-deep forms are every account's, which
	// is broader still and gets the same warning rather than a quieter one.
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	if len(parts) <= 2 && (parts[0] == "Users" || parts[0] == "home") {
		return scopeBreadthHome
	}
	return scopeBreadthBounded
}

// scopeValueEntries reads a stored scope value as the list of things it names.
// Array-of-string and string are the two shapes ValidateValue admits; anything
// else names nothing this can classify, and says so by returning nothing.
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

// scopeValueBreadth returns the WIDEST breadth any entry of a value has.
// Widest rather than narrowest because a list is a union: ["/Users/me/proj",
// "/"] reaches everything, and a rendering that reported the first entry would
// describe the confinement the operator meant instead of the one in force.
func scopeValueBreadth(raw json.RawMessage) string {
	widest := scopeBreadthBounded
	for _, e := range scopeValueEntries(raw) {
		switch scopeEntryBreadth(e) {
		case scopeBreadthRoot:
			return scopeBreadthRoot
		case scopeBreadthHome:
			widest = scopeBreadthHome
		}
	}
	return widest
}

// scopeBreadthWarnings names every field of an injected scope whose value
// reaches further than a folder, each with its phrase, in field-name order.
// It is what an operator surface appends to a truthful rendering of the values
// themselves — it never replaces one, because the coordinates are the thing the
// operator is entitled to (issue #41) and a warning is the second sentence.
func scopeBreadthWarnings(scope map[string]json.RawMessage) []string {
	names := make([]string, 0, len(scope))
	for name := range scope {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		if phrase := scopeBreadthPhrase(scopeValueBreadth(scope[name])); phrase != "" {
			out = append(out, name+" is "+phrase)
		}
	}
	return out
}
