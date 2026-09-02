package project

// The configuration plane's own validation, kept in a file with no store
// call and no gated mutator name in it (gate_structural_test.go's allowlist
// is by filename, and this one is deliberately absent from it): the safety
// of a remote's self-narrowing comes from making a widening unrepresentable
// here, not from a presence prompt.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/barelyworkingcode/relay/internal/config"
	"maps"
	"slices"
	"strings"
)

// NarrowFields is the entire configuration surface a remote holds.
// Note what is not here: no path, no kind, no allow_cwd_auth, no
// disabled_tools, no context, no name, no id. Decoding is strict, so a
// client sending allow_cwd_auth gets a decode error at the door rather than
// a field silently ignored.
type NarrowFields struct {
	AllowedMcpIDs *[]string            `json:"allowed_mcp_ids,omitempty"`
	AllowedTools  *map[string][]string `json:"allowed_tools,omitempty"`
	Access        *map[string]string   `json:"access,omitempty"`
	AllowExternal *map[string]bool     `json:"allow_external,omitempty"`
}

// DecodeNarrowFields decodes a NarrowGrant request's Arguments
// strictly: an unknown key is a decode error naming it, not a value
// silently dropped. Empty/absent Arguments decode to the zero value — a
// narrowing request that touches nothing.
func DecodeNarrowFields(raw json.RawMessage) (NarrowFields, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return NarrowFields{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var f NarrowFields
	if err := dec.Decode(&f); err != nil {
		return NarrowFields{}, err
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
// pass NarrowsOnly's literal-name check against the unescaped name while
// the pattern actually stored, once escapes are honoured, matched a wider
// set than what was validated.
func hasGlobMeta(pattern string) bool {
	return strings.ContainsAny(pattern, "*?[\\")
}

// NarrowsOnly is the one check standing between a remote's own request and
// a widening of its stored grant. Every rule refuses with a message naming
// the field and the offending value; nothing here mutates stored.
func NarrowsOnly(stored config.Project, f NarrowFields) error {
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
			if mode != config.AccessRead {
				return fmt.Errorf("access for %q: narrowing may only request %q, not %q", mcpID, config.AccessRead, mode)
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

// NarrowingIsNoop reports whether every field f touches already reads,
// byte for byte, what stored holds — the request narrows to exactly what
// is already granted. NarrowsOnly having accepted f only means f is not a
// widening; a repeat of an already-applied narrowing (or a resend of the
// same request) is equally accepted by that test, and it is this function's
// job to tell the two apart. Only fields the request touches are compared:
// an absent pointer is "no change" everywhere else in this package too.
//
// The comparison is against NarrowUpdateFields' MERGED result, not f
// itself. f.AllowedTools (etc.) holds only the ids the request named; under
// merge semantics that is never what ends up stored for the whole map, so
// comparing it directly against stored's full map would read every request
// that omits an untouched id — which is every request — as a change, even
// a byte-for-byte resend. Comparing what would actually be written is what
// keeps this the same question ApplyUpdate's mutators are about to
// answer.
func NarrowingIsNoop(stored config.Project, f NarrowFields) bool {
	merged := NarrowUpdateFields(stored, f)
	if merged.AllowedMcpIDs != nil && !slices.Equal(*merged.AllowedMcpIDs, stored.AllowedMcpIDs) {
		return false
	}
	if merged.AllowedTools != nil && !maps.EqualFunc(*merged.AllowedTools, stored.AllowedTools, slices.Equal) {
		return false
	}
	if merged.Access != nil && !maps.Equal(*merged.Access, stored.Access) {
		return false
	}
	if merged.AllowExternal != nil && !maps.Equal(*merged.AllowExternal, stored.AllowExternal) {
		return false
	}
	return true
}

// NarrowUpdateFields carries a NarrowGrant request's already-validated
// fields into ApplyUpdate's patch shape. AllowedMcpIDs is a
// relabelling — NarrowsOnly already proved the requested list widens
// nothing. AllowedTools/Access/AllowExternal are NOT: the wire semantics
// for a set pointer is whole-map replace (ApplyUpdate's
// candidate.AllowedTools = *f.AllowedTools and siblings), but a remote
// only ever names the MCP ids it means to touch, so a map built from the
// request alone would drop every id it didn't mention — narrowing an MCP
// the caller never named, down to nothing, as a side effect of narrowing
// one it did. mergeNarrowedMap folds the request's per-key overrides onto
// what's already stored so an untouched id keeps its stored value; keys
// for an MCP falling out of the resulting allowed_mcp_ids are left for
// syncProjectToken's existing pruning rather than carried forward stale.
func NarrowUpdateFields(stored config.Project, f NarrowFields) UpdateFields {
	resultMcpIDs := stored.AllowedMcpIDs
	if f.AllowedMcpIDs != nil {
		resultMcpIDs = *f.AllowedMcpIDs
	}
	return UpdateFields{
		AllowedMcpIDs: f.AllowedMcpIDs,
		AllowedTools:  mergeNarrowedMap(stored.AllowedTools, f.AllowedTools, resultMcpIDs),
		Access:        mergeNarrowedMap(stored.Access, f.Access, resultMcpIDs),
		AllowExternal: mergeNarrowedMap(stored.AllowExternal, f.AllowExternal, resultMcpIDs),
	}
}

// mergeNarrowedMap merges a NarrowGrant request's per-MCP overrides onto
// what's stored: an id in keep but not in req keeps its stored value, an
// id in req is set to req's value, and an id outside keep is dropped
// (falling out of allowed_mcp_ids, handled here rather than left for
// ApplyUpdate to reconcile against a stale carried-forward entry).
// req == nil means the request doesn't touch this field at all, which
// must stay nil so ApplyUpdate's own nil-check leaves it alone —
// merging would turn "not in the request" into "set to a copy of
// stored," a write with nothing behind it.
func mergeNarrowedMap[V any](stored map[string]V, req *map[string]V, keep []string) *map[string]V {
	if req == nil {
		return nil
	}
	keepSet := make(map[string]bool, len(keep))
	for _, id := range keep {
		keepSet[id] = true
	}
	merged := make(map[string]V, len(stored))
	for id, v := range stored {
		if keepSet[id] {
			merged[id] = v
		}
	}
	for id, v := range *req {
		merged[id] = v
	}
	return &merged
}

// NarrowFieldNames lists the fields a NarrowGrant request touches,
// for the audit record and the caller's Changed list — presence, not
// content, so it reads the request directly rather than NarrowUpdateFields'
// merged (and therefore always-non-nil-when-touched, identically) result.
func NarrowFieldNames(f NarrowFields) []string {
	var names []string
	if f.AllowedMcpIDs != nil {
		names = append(names, "allowed_mcp_ids")
	}
	if f.AllowedTools != nil {
		names = append(names, "allowed_tools")
	}
	if f.Access != nil {
		names = append(names, "access")
	}
	if f.AllowExternal != nil {
		names = append(names, "allow_external")
	}
	return names
}
