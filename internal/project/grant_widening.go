package project

// The operator-side counterpart of NarrowsOnly: NarrowsOnly answers "may a
// remote apply this to its own grant" (widening refused outright), and this
// file answers "does this operator edit widen the grant at all" — the
// question cmd/relay's presence gate needs so a rename, an unchanged resend,
// or a pure narrowing of allowed_mcp_ids/allowed_tools/access/allow_external/
// mounts never raises a prompt (ADR-018 decision 1: obtaining or widening a
// capability is privileged, using or narrowing one is not).

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"

	"github.com/barelyworkingcode/relay/internal/config"
)

// UpdateWidensGrant reports which of AC-16c's grant-shape fields actually
// widen what the project's token reaches once f is resolved against stored
// — never merely which fields the request happens to carry. A door like the
// Settings window that resends the whole record on every save must not
// manufacture a prompt out of a value that came back unchanged, and a pure
// narrowing (fewer MCPs, a smaller tool pattern, read in place of write)
// must not prompt either: only entries in the returned slice are grounds to
// gate, and its order is stable (matching projectUpdateGrantFieldNames' own
// field order) so a reason string built from it reads the same way every
// time.
func UpdateWidensGrant(stored config.Project, f UpdateFields) []string {
	var out []string
	if f.AllowedMcpIDs != nil && addsMcpID(stored.AllowedMcpIDs, *f.AllowedMcpIDs) {
		out = append(out, "allowed_mcp_ids")
	}
	if f.AllowedTools != nil && allowedToolsWidens(stored.AllowedTools, *f.AllowedTools) {
		out = append(out, "allowed_tools")
	}
	if f.Access != nil && accessWidens(stored, *f.Access) {
		out = append(out, "access")
	}
	if f.Context != nil && !contextEqual(stored.Context, *f.Context) {
		out = append(out, "context")
	}
	if f.AllowExternal != nil && allowExternalWidens(stored.AllowExternal, *f.AllowExternal) {
		out = append(out, "allow_external")
	}
	if f.AllowCwdAuth != nil && *f.AllowCwdAuth && !stored.AllowCwdAuth {
		out = append(out, "allow_cwd_auth")
	}
	if f.Kind != nil && normalizeKind(*f.Kind) != normalizeKind(stored.Kind) {
		out = append(out, "kind")
	}
	if f.HostID != nil && *f.HostID != stored.HostID {
		out = append(out, "host_id")
	}
	if f.Path != nil && *f.Path != stored.Path {
		out = append(out, "path")
	}
	if f.Mounts != nil && mountsWiden(stored, *f.Mounts) {
		out = append(out, "mounts")
	}
	return out
}

// normalizeKind reads Project.Kind's own documented rule -- "" and
// ProjectKindLocal are the same project, never distinguishable by comparing
// the raw string -- so a request that spells its own kind "local" against a
// record that predates the field (kind: "") is not a change.
func normalizeKind(k config.ProjectKind) config.ProjectKind {
	if k == "" {
		return config.ProjectKindLocal
	}
	return k
}

// addsMcpID reports whether requested contains an id absent from stored --
// set membership, not sequence equality, so reordering or de-duplicating an
// unchanged grant is not a widening, and dropping ids (narrowing) never is
// either.
func addsMcpID(stored, requested []string) bool {
	for _, id := range requested {
		if !slices.Contains(stored, id) {
			return true
		}
	}
	return false
}

// allowedToolsWidens mirrors NarrowsOnly's own literal reading of
// AllowedTools: "all tools" (an absent or empty entry on a local project)
// has no enumerable stored form to compare a requested pattern against, so
// an explicit pattern where none was stored before is new reach, not a
// narrowing of an implicit default -- exactly the reading
// TestProjectOps_WideningAllowedToolsIsGated already pins. A dropped map key
// (a stored, non-empty entry absent from requested) is a narrowing: no
// resulting pattern is wider than nothing.
func allowedToolsWidens(stored, requested map[string][]string) bool {
	for mcpID, patterns := range requested {
		storedPatterns := stored[mcpID]
		for _, pattern := range patterns {
			if slices.Contains(storedPatterns, pattern) {
				continue
			}
			if !hasGlobMeta(pattern) && toolAllowedByPatterns(storedPatterns, pattern) {
				continue
			}
			return true
		}
	}
	return false
}

// accessWidens is default-aware, unlike allowExternalWidens below: Access
// has no client-side "store only dissent from the default" discipline (the
// Settings form's access control pins whatever it last showed, including a
// value that only ever repeated the kind's own default), so treating any
// newly-explicit entry as a widening would gate a save that changed nothing
// a token can reach. Comparing against StoredToken.AccessMode's own
// asymmetric default -- write for a local project, read for a profile --
// answers the only question that matters: does the requested mode reach
// somewhere the stored mode didn't. Read in place of write, or a mode equal
// to what AccessMode already resolves to, is never counted.
func accessWidens(stored config.Project, requested map[string]string) bool {
	before := &config.StoredToken{ProjectKind: stored.Kind, Access: stored.Access}
	for mcpID, mode := range requested {
		if mode != config.AccessWrite {
			continue
		}
		if before.AccessMode(mcpID) == config.AccessRead {
			return true
		}
	}
	return false
}

// allowExternalWidens compares against stored's own literal value, never
// against ExternalAllowed's kind-aware default (unlike accessWidens above):
// setProjAllowExternal (web/src/app.js) already stores only dissent from the
// kind's default before this ever reaches the wire, so there is no
// "resent default" case here to guard against, and a resolved-default
// comparison would instead let a caller flip a local project's already-true
// default to explicit true, or vice versa, for free. A stored absent entry
// reads as literal false (Go's own map zero value), which is what keeps
// TestProjectOps_AllowExternalTurningOnIsGated true: a local project already
// defaults to allowed, and an operator (or a script) that asks for true
// explicitly, where nothing was stored, is still choosing to pin an
// outbound grant that a later conversion to a profile -- whose default is
// refused -- would otherwise silently carry forward as a bare boolean with
// no default left to fall back to.
func allowExternalWidens(stored, requested map[string]bool) bool {
	for id, v := range requested {
		if v && !stored[id] {
			return true
		}
	}
	return false
}

// mountsWiden mirrors NarrowsOnly's Mounts rule: a mount id absent from
// stored is new reach, a path change is refused there and treated as
// widening here (there is no "narrower path" reading), and read-to-write on
// an existing id is the one axis NarrowsOnly names explicitly. Dropping a
// stored mount id (present in stored, absent from requested) is narrowing —
// full replace semantics mean it is removed, never carried forward stale.
func mountsWiden(stored config.Project, requested []config.MountGrant) bool {
	for _, m := range requested {
		existing, ok := FindMount(&stored, m.ID)
		if !ok {
			return true
		}
		if m.Path != existing.Path {
			return true
		}
		if m.AccessMode() == config.AccessWrite && existing.AccessMode() == config.AccessRead {
			return true
		}
	}
	return false
}

// contextEqual compares two context maps by decoded value rather than by
// raw bytes: whitespace or key-order differences introduced by re-encoding
// (the Settings form round-trips every scope value through JSON) must not
// read as a change when nothing an MCP would observe actually moved. A key
// present on one side only, or a value that fails to decode identically, is
// treated as a difference — the conservative direction, since context can
// carry a resource scope and there is no narrower reading defined for it.
func contextEqual(a, b map[string]json.RawMessage) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok {
			return false
		}
		if bytes.Equal(bytes.TrimSpace(av), bytes.TrimSpace(bv)) {
			continue
		}
		var da, db any
		if err := json.Unmarshal(av, &da); err != nil {
			return false
		}
		if err := json.Unmarshal(bv, &db); err != nil {
			return false
		}
		if !reflect.DeepEqual(da, db) {
			return false
		}
	}
	return true
}
