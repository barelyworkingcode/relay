package project

// narrowing.go's own tests: hasGlobMeta must recognise every
// character path.Match's grammar treats specially, because NarrowsOnly's
// literal-tool-name branch trusts hasGlobMeta to tell a plain name from
// anything path.Match would parse further.

import (
	"github.com/barelyworkingcode/relay/internal/config"
	"path"
	"slices"
	"strings"
	"testing"
)

// pathMatchIsMeta answers, empirically rather than by copying hasGlobMeta's
// own string, whether c changes what path.Match matches when it appears in
// a pattern. A pattern built as "a"+c+"b" is compared against two
// candidate names: the identical string, and the same shape with c
// replaced by an unrelated ordinary character ('z'). An ordinary character
// matches only the identical name; a character path.Match gives meaning to
// either fails to match its own literal rendering (e.g. an unterminated
// class, or "\b" which really means "b") or also matches the substituted
// name (e.g. "?" and "*", which match anything in that position). Deriving
// the table this way — from path.Match's observed behaviour — is what
// would have caught '\' being forgotten in "*?[": a table copied from
// hasGlobMeta's own source cannot catch hasGlobMeta's own omission.
func pathMatchIsMeta(c byte) bool {
	if c == 'z' {
		panic("probe character 'z' cannot be the character under test")
	}
	pattern := "a" + string(c) + "b"
	identical := "a" + string(c) + "b"
	substituted := "azb"

	matchedIdentical, err := path.Match(pattern, identical)
	if err != nil {
		return true // does not even compile as a pattern over its own literal rendering
	}
	if !matchedIdentical {
		return true // e.g. "\" c consumes two pattern chars into one matched char
	}
	matchedSubstituted, err := path.Match(pattern, substituted)
	if err != nil {
		return true
	}
	return matchedSubstituted // matches something other than its own literal rendering
}

// derivedPathMatchMetaChars scans every printable, non-separator ASCII
// character and returns the ones pathMatchIsMeta flags — the table
// TestHasGlobMeta_CoversEveryPathMatchMetaCharacter is driven from.
func derivedPathMatchMetaChars(t *testing.T) []byte {
	t.Helper()
	var metas []byte
	for c := byte(0x21); c < 0x7f; c++ {
		if c == '/' || c == 'z' {
			continue // '/' is path.Match's separator, not a pattern metacharacter; 'z' is the probe's own substitute
		}
		if pathMatchIsMeta(c) {
			metas = append(metas, c)
		}
	}
	return metas
}

// TestHasGlobMeta_CoversEveryPathMatchMetaCharacter is table-driven over a
// metacharacter set derived independently of hasGlobMeta's own
// implementation (see derivedPathMatchMetaChars): a future metacharacter
// hasGlobMeta forgets fails this test the same way '\' escaping the fix
// was meant to catch, instead of being missed the same way '\' itself was.
func TestHasGlobMeta_CoversEveryPathMatchMetaCharacter(t *testing.T) {
	metas := derivedPathMatchMetaChars(t)
	if string(metas) != `*?[\` {
		t.Fatalf("derivedPathMatchMetaChars = %q, want the documented grammar %q", metas, `*?[\`)
	}
	for _, c := range metas {
		pattern := "mail_" + string(c) + "x"
		t.Run(string(c), func(t *testing.T) {
			if !hasGlobMeta(pattern) {
				t.Errorf("hasGlobMeta(%q) = false, want true: %q is one of path.Match's metacharacters", pattern, string(c))
			}
		})
	}
	for _, pattern := range []string{"mail_search", "mail-search", "mail.search", "mail_search_v2", "MAIL_SEARCH"} {
		t.Run(pattern, func(t *testing.T) {
			if hasGlobMeta(pattern) {
				t.Errorf("hasGlobMeta(%q) = true, want false: it has no path.Match metacharacter", pattern)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Finding B: an allowed_tools / access / allow_external key naming an MCP
// NOT in the resulting allowed_mcp_ids must be refused rather than stored
// (SPEC-step2-cli-admin.md §4.2's last bullet) — an inert control reads on
// the screen as a boundary and is not one. Each of NarrowsOnly's three
// "is this MCP in the resulting set" guards has its own row: deleting any
// one of them must leave only its own row red, not the other two.
// ---------------------------------------------------------------------------

func TestNarrowsOnly_RefusesAKeyOnAnMcpNotInTheResultingSet(t *testing.T) {
	narrowedIDs := []string{"macmcp"}
	for _, tc := range []struct {
		name   string
		stored config.Project
		fields NarrowFields
	}{
		{
			name: "allowed_tools",
			stored: config.Project{
				AllowedMcpIDs: []string{"macmcp", "other"},
				AllowedTools:  map[string][]string{"macmcp": {"mail_search"}, "other": {"other_tool"}},
			},
			fields: NarrowFields{AllowedMcpIDs: &narrowedIDs, AllowedTools: &map[string][]string{"other": {"other_tool"}}},
		},
		{
			name: "access",
			stored: config.Project{
				AllowedMcpIDs: []string{"macmcp", "other"},
				Access:        map[string]string{"macmcp": config.AccessRead, "other": config.AccessRead},
			},
			fields: NarrowFields{AllowedMcpIDs: &narrowedIDs, Access: &map[string]string{"other": config.AccessRead}},
		},
		{
			name: "allow_external",
			stored: config.Project{
				AllowedMcpIDs: []string{"macmcp", "other"},
				AllowExternal: map[string]bool{"macmcp": false, "other": false},
			},
			fields: NarrowFields{AllowedMcpIDs: &narrowedIDs, AllowExternal: &map[string]bool{"other": false}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := NarrowsOnly(tc.stored, tc.fields)
			if err == nil {
				t.Fatalf("NarrowsOnly accepted an %s key for an MCP dropped from allowed_mcp_ids", tc.name)
			}
			if !strings.Contains(err.Error(), "other") {
				t.Errorf("error = %q, want it to name the offending MCP %q", err, "other")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Mounts narrowing (relayfs P1a): write->read only, no path change, no
// adding via narrow, and dropping an unlisted mount is always allowed.
// ---------------------------------------------------------------------------

func mountStoredProject(mounts ...config.MountGrant) config.Project {
	return config.Project{Kind: config.ProjectKindRemote, Mounts: mounts}
}

// A request's own entries are checked against what is stored; a stored mount
// the request omits is being dropped, which is narrowing and always allowed.
func TestNarrowsOnly_Mounts(t *testing.T) {
	mail := config.MountGrant{ID: "mail", Path: "/tmp/mail"}
	withAccess := func(m config.MountGrant, access string) config.MountGrant {
		m.Access = access
		return m
	}
	for _, tc := range []struct {
		name      string
		stored    config.Project
		requested []config.MountGrant
		refusal   []string // nil: the request must be allowed
	}{
		{
			name:      "write to read is allowed",
			stored:    mountStoredProject(withAccess(mail, config.AccessWrite)),
			requested: []config.MountGrant{withAccess(mail, config.AccessRead)},
		},
		{
			name:      "read to write is refused",
			stored:    mountStoredProject(withAccess(mail, config.AccessRead)),
			requested: []config.MountGrant{withAccess(mail, config.AccessWrite)},
			refusal:   []string{"mail"},
		},
		{
			name:      "an unstored id is refused",
			stored:    mountStoredProject(mail),
			requested: []config.MountGrant{mail, {ID: "calendar", Path: "/tmp/cal"}},
			refusal:   []string{"calendar", "cannot add"},
		},
		{
			name:      "changing a path is refused",
			stored:    mountStoredProject(mail),
			requested: []config.MountGrant{{ID: "mail", Path: "/tmp/mail-2"}},
			refusal:   []string{"mail"},
		},
		{
			name:      "omitting a stored mount is allowed",
			stored:    mountStoredProject(mail, config.MountGrant{ID: "calendar", Path: "/tmp/cal"}),
			requested: []config.MountGrant{mail},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := NarrowsOnly(tc.stored, NarrowFields{Mounts: &tc.requested})
			if tc.refusal == nil {
				if err != nil {
					t.Fatalf("expected the narrowing to be allowed, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected a refusal, got none")
			}
			for _, want := range tc.refusal {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}

func TestNarrowUpdateFields_MountsReplacesWithTheRequestSetDroppingOmittedOnes(t *testing.T) {
	stored := mountStoredProject(
		config.MountGrant{ID: "mail", Path: "/tmp/mail"},
		config.MountGrant{ID: "calendar", Path: "/tmp/cal"},
	)
	narrowed := []config.MountGrant{{ID: "mail", Path: "/tmp/mail"}}
	got := NarrowUpdateFields(stored, NarrowFields{Mounts: &narrowed})
	if got.Mounts == nil || len(*got.Mounts) != 1 || (*got.Mounts)[0].ID != "mail" {
		t.Fatalf("NarrowUpdateFields.Mounts = %v, want exactly [mail] (calendar dropped)", got.Mounts)
	}
}

// Re-listing every stored mount with an identical path and access — even
// when access is spelled differently (stored has no Access set at all, the
// request explicitly sends "read") — must read as a no-op, never as a
// change: AccessMode() is the comparison, not the raw string.
func TestNarrowingIsNoop_MountsIdenticalRelistIsNoopEvenWithDifferentAccessSpelling(t *testing.T) {
	stored := mountStoredProject(config.MountGrant{ID: "mail", Path: "/tmp/mail"}) // Access absent
	narrowed := []config.MountGrant{{ID: "mail", Path: "/tmp/mail", Access: config.AccessRead}}
	if !NarrowingIsNoop(stored, NarrowFields{Mounts: &narrowed}) {
		t.Fatal("re-listing an identical mount, with access respelled from absent to \"read\", must be a no-op")
	}
}

func TestNarrowingIsNoop_MountAccessNarrowingIsNotANoop(t *testing.T) {
	stored := mountStoredProject(config.MountGrant{ID: "mail", Path: "/tmp/mail", Access: config.AccessWrite})
	narrowed := []config.MountGrant{{ID: "mail", Path: "/tmp/mail", Access: config.AccessRead}}
	if NarrowingIsNoop(stored, NarrowFields{Mounts: &narrowed}) {
		t.Fatal("narrowing write->read is an actual change and must not read as a no-op")
	}
}

func TestNarrowFieldNames_IncludesMountsOnlyWhenSet(t *testing.T) {
	if names := NarrowFieldNames(NarrowFields{}); slices.Contains(names, "mounts") {
		t.Fatalf("NarrowFieldNames(%+v) = %v, must not include \"mounts\" when f.Mounts is nil", NarrowFields{}, names)
	}
	mounts := []config.MountGrant{{ID: "mail", Path: "/tmp/mail"}}
	names := NarrowFieldNames(NarrowFields{Mounts: &mounts})
	if !slices.Contains(names, "mounts") {
		t.Fatalf("NarrowFieldNames = %v, want it to include \"mounts\" when f.Mounts is set", names)
	}
}
