package main

// The configuration plane's own validation, kept in a file with no store
// call and no gated mutator name in it (gate_structural_test.go's allowlist
// is by filename, and this one is deliberately absent from it): the safety
// of a remote's self-narrowing comes from making a widening unrepresentable
// here, not from a presence prompt.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// remoteNarrowFields is the entire configuration surface a remote holds.
// Note what is not here: no path, no kind, no allow_cwd_auth, no
// disabled_tools, no context, no name, no id. Decoding is strict, so a
// client sending allow_cwd_auth gets a decode error at the door rather than
// a field silently ignored.
type remoteNarrowFields struct {
	AllowedMcpIDs *[]string            `json:"allowed_mcp_ids,omitempty"`
	AllowedTools  *map[string][]string `json:"allowed_tools,omitempty"`
	Access        *map[string]string   `json:"access,omitempty"`
	AllowExternal *map[string]bool     `json:"allow_external,omitempty"`
}

// decodeRemoteNarrowFields decodes a NarrowGrant request's Arguments
// strictly: an unknown key is a decode error naming it, not a value
// silently dropped. Empty/absent Arguments decode to the zero value — a
// narrowing request that touches nothing.
func decodeRemoteNarrowFields(raw json.RawMessage) (remoteNarrowFields, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return remoteNarrowFields{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var f remoteNarrowFields
	if err := dec.Decode(&f); err != nil {
		return remoteNarrowFields{}, err
	}
	return f, nil
}

// hasGlobMeta reports whether pattern is anything path.Match would treat as
// more than a literal tool name — the four characters it gives special
// meaning: *, ?, [ and \. \ is easy to miss because it doesn't look like a
// wildcard: as an escape it changes what NAME a pattern matches without
// ever opening a class or repeating anything, so a pattern built entirely
// from \-escapes still reads, to a human, as a literal string — while
// path.Match reads it as a pattern whose matched set differs from the
// literal string's. Treating it as ordinary here let a requested pattern
// pass narrowsOnly's literal-name check against the unescaped name while
// the pattern actually stored, once escapes are honoured, matched a wider
// set than what was validated.
func hasGlobMeta(pattern string) bool {
	return strings.ContainsAny(pattern, "*?[\\")
}

// narrowsOnly is the one check standing between a remote's own request and
// a widening of its stored grant. Every rule refuses with a message naming
// the field and the offending value; nothing here mutates stored.
func narrowsOnly(stored Project, f remoteNarrowFields) error {
	resultMcpIDs := stored.AllowedMcpIDs
	if f.AllowedMcpIDs != nil {
		for _, id := range *f.AllowedMcpIDs {
			if !slices.Contains(stored.AllowedMcpIDs, id) {
				return fmt.Errorf("allowed_mcp_ids: %q is not already granted; narrowing cannot add an MCP", id)
			}
		}
		resultMcpIDs = *f.AllowedMcpIDs
	}

	if f.AllowedTools != nil {
		for _, mcpID := range sortedKeys(*f.AllowedTools) {
			if !slices.Contains(resultMcpIDs, mcpID) {
				return fmt.Errorf("allowed_tools: %q is not in the resulting allowed_mcp_ids", mcpID)
			}
			storedPatterns := stored.AllowedTools[mcpID]
			for _, pattern := range (*f.AllowedTools)[mcpID] {
				if slices.Contains(storedPatterns, pattern) {
					continue
				}
				// A literal tool name already matched by a stored pattern
				// (e.g. requesting "mail_search" against a stored "mail_*")
				// narrows the matched set; a glob is refused outright even
				// when it looks shorter, since relay cannot compare two
				// patterns' reach without enumerating every future tool
				// name against both.
				if !hasGlobMeta(pattern) && toolAllowedByPatterns(storedPatterns, pattern) {
					continue
				}
				return fmt.Errorf("allowed_tools for %q: %q is not already granted; narrowing cannot widen a tool pattern", mcpID, pattern)
			}
		}
	}

	if f.Access != nil {
		for _, mcpID := range sortedKeys(*f.Access) {
			if !slices.Contains(resultMcpIDs, mcpID) {
				return fmt.Errorf("access: %q is not in the resulting allowed_mcp_ids", mcpID)
			}
			mode := (*f.Access)[mcpID]
			if mode != AccessRead {
				return fmt.Errorf("access for %q: narrowing may only request %q, not %q", mcpID, AccessRead, mode)
			}
		}
	}

	if f.AllowExternal != nil {
		for _, mcpID := range sortedKeys(*f.AllowExternal) {
			if !slices.Contains(resultMcpIDs, mcpID) {
				return fmt.Errorf("allow_external: %q is not in the resulting allowed_mcp_ids", mcpID)
			}
			if (*f.AllowExternal)[mcpID] {
				return fmt.Errorf("allow_external for %q: narrowing may only request false", mcpID)
			}
		}
	}

	return nil
}
