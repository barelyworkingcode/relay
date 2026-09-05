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

// TestPathMatchIsMeta_MatchesTheDocumentedGrammar is a sanity floor on the
// detector itself: path.Match's doc comment names exactly four
// metacharacters (*, ?, [, \) and no others among ordinary printable ASCII.
func TestPathMatchIsMeta_MatchesTheDocumentedGrammar(t *testing.T) {
	want := map[byte]bool{'*': true, '?': true, '[': true, '\\': true}
	for c := byte(0x21); c < 0x7f; c++ {
		if c == '/' || c == 'z' {
			continue
		}
		got := pathMatchIsMeta(c)
		if got != want[c] {
			t.Errorf("pathMatchIsMeta(%q) = %v, want %v", string(c), got, want[c])
		}
	}
}

// TestHasGlobMeta_CoversEveryPathMatchMetaCharacter is table-driven over a
// metacharacter set derived independently of hasGlobMeta's own
// implementation (see derivedPathMatchMetaChars): a future metacharacter
// hasGlobMeta forgets fails this test the same way '\' escaping the fix
// was meant to catch, instead of being missed the same way '\' itself was.
func TestHasGlobMeta_CoversEveryPathMatchMetaCharacter(t *testing.T) {
	metas := derivedPathMatchMetaChars(t)
	if len(metas) == 0 {
		t.Fatal("derivedPathMatchMetaChars found none; the detector itself is broken")
	}
	for _, c := range metas {
		pattern := "mail_" + string(c) + "x"
		t.Run(string(c), func(t *testing.T) {
			if !hasGlobMeta(pattern) {
				t.Errorf("hasGlobMeta(%q) = false, want true: %q is one of path.Match's metacharacters", pattern, string(c))
			}
		})
	}
}

// TestHasGlobMeta_OrdinaryCharactersAreNotFlagged is the negative control:
// hasGlobMeta must not start refusing plain tool-name characters.
func TestHasGlobMeta_OrdinaryCharactersAreNotFlagged(t *testing.T) {
	for _, pattern := range []string{"mail_search", "mail-search", "mail.search", "mail_search_v2", "MAIL_SEARCH"} {
		if hasGlobMeta(pattern) {
			t.Errorf("hasGlobMeta(%q) = true, want false: it has no path.Match metacharacter", pattern)
		}
	}
}

// ---------------------------------------------------------------------------
// Finding B: an allowed_tools / access / allow_external key naming an MCP
// NOT in the resulting allowed_mcp_ids must be refused rather than stored
// (SPEC-step2-cli-admin.md §4.2's last bullet) — an inert control reads on
// the screen as a boundary and is not one. Each of NarrowsOnly's three
// "is this MCP in the resulting set" guards has its own test: deleting any
// one of them must leave only its own test red, not the other two.
// ---------------------------------------------------------------------------

func TestNarrowsOnly_RefusesAllowedToolsKeyOnAnMcpNotInTheResultingSet(t *testing.T) {
	stored := config.Project{
		AllowedMcpIDs: []string{"macmcp", "other"},
		AllowedTools:  map[string][]string{"macmcp": {"mail_search"}, "other": {"other_tool"}},
	}
	narrowedIDs := []string{"macmcp"}
	tools := map[string][]string{"other": {"other_tool"}}
	err := NarrowsOnly(stored, NarrowFields{AllowedMcpIDs: &narrowedIDs, AllowedTools: &tools})
	if err == nil {
		t.Fatal("NarrowsOnly accepted an allowed_tools key for an MCP dropped from allowed_mcp_ids")
	}
	if !strings.Contains(err.Error(), "other") {
		t.Errorf("error = %q, want it to name the offending MCP %q", err, "other")
	}
}

func TestNarrowsOnly_RefusesAccessKeyOnAnMcpNotInTheResultingSet(t *testing.T) {
	stored := config.Project{
		AllowedMcpIDs: []string{"macmcp", "other"},
		Access:        map[string]string{"macmcp": config.AccessRead, "other": config.AccessRead},
	}
	narrowedIDs := []string{"macmcp"}
	access := map[string]string{"other": config.AccessRead}
	err := NarrowsOnly(stored, NarrowFields{AllowedMcpIDs: &narrowedIDs, Access: &access})
	if err == nil {
		t.Fatal("NarrowsOnly accepted an access key for an MCP dropped from allowed_mcp_ids")
	}
	if !strings.Contains(err.Error(), "other") {
		t.Errorf("error = %q, want it to name the offending MCP %q", err, "other")
	}
}

func TestNarrowsOnly_RefusesAllowExternalKeyOnAnMcpNotInTheResultingSet(t *testing.T) {
	stored := config.Project{
		AllowedMcpIDs: []string{"macmcp", "other"},
		AllowExternal: map[string]bool{"macmcp": false, "other": false},
	}
	narrowedIDs := []string{"macmcp"}
	allowExternal := map[string]bool{"other": false}
	err := NarrowsOnly(stored, NarrowFields{AllowedMcpIDs: &narrowedIDs, AllowExternal: &allowExternal})
	if err == nil {
		t.Fatal("NarrowsOnly accepted an allow_external key for an MCP dropped from allowed_mcp_ids")
	}
	if !strings.Contains(err.Error(), "other") {
		t.Errorf("error = %q, want it to name the offending MCP %q", err, "other")
	}
}

// ---------------------------------------------------------------------------
// Mounts narrowing (relayfs P1a): write->read only, no path change, no
// adding via narrow, and dropping an unlisted mount is always allowed.
// ---------------------------------------------------------------------------

func mountStoredProject(mounts ...config.MountGrant) config.Project {
	return config.Project{Kind: config.ProjectKindRemote, Mounts: mounts}
}

func TestNarrowsOnly_MountNarrowingWriteToReadIsAllowed(t *testing.T) {
	stored := mountStoredProject(config.MountGrant{ID: "mail", Path: "/tmp/mail", Access: config.AccessWrite})
	narrowed := []config.MountGrant{{ID: "mail", Path: "/tmp/mail", Access: config.AccessRead}}
	if err := NarrowsOnly(stored, NarrowFields{Mounts: &narrowed}); err != nil {
		t.Fatalf("narrowing write->read must be allowed, got: %v", err)
	}
}

func TestNarrowsOnly_MountWideningReadToWriteIsRefused(t *testing.T) {
	stored := mountStoredProject(config.MountGrant{ID: "mail", Path: "/tmp/mail", Access: config.AccessRead})
	narrowed := []config.MountGrant{{ID: "mail", Path: "/tmp/mail", Access: config.AccessWrite}}
	if err := NarrowsOnly(stored, NarrowFields{Mounts: &narrowed}); err == nil {
		t.Fatal("widening a mount from read to write via narrowing must be refused")
	}
}

func TestNarrowsOnly_MountRequestNamingAnUnstoredIDIsRefused(t *testing.T) {
	stored := mountStoredProject(config.MountGrant{ID: "mail", Path: "/tmp/mail"})
	narrowed := []config.MountGrant{
		{ID: "mail", Path: "/tmp/mail"},
		{ID: "calendar", Path: "/tmp/cal"},
	}
	err := NarrowsOnly(stored, NarrowFields{Mounts: &narrowed})
	if err == nil {
		t.Fatal("narrowing must not be able to add a mount id that is not already stored")
	}
	if !strings.Contains(err.Error(), "calendar") {
		t.Errorf("error = %q, want it to name the offending mount id %q", err, "calendar")
	}
}

func TestNarrowsOnly_MountRequestChangingPathIsRefused(t *testing.T) {
	stored := mountStoredProject(config.MountGrant{ID: "mail", Path: "/tmp/mail"})
	narrowed := []config.MountGrant{{ID: "mail", Path: "/tmp/mail-2"}}
	if err := NarrowsOnly(stored, NarrowFields{Mounts: &narrowed}); err == nil {
		t.Fatal("narrowing must not be able to change a listed mount's path")
	}
}

// Omitting a currently-stored mount entirely is how a narrow request drops
// it — that is narrowing, always allowed, and NarrowsOnly must not iterate
// the stored mounts complaining about ones missing from the request (only
// the request's own entries are checked against what is stored).
func TestNarrowsOnly_MountOmittingAStoredMountIsAllowed(t *testing.T) {
	stored := mountStoredProject(
		config.MountGrant{ID: "mail", Path: "/tmp/mail"},
		config.MountGrant{ID: "calendar", Path: "/tmp/cal"},
	)
	narrowed := []config.MountGrant{{ID: "mail", Path: "/tmp/mail"}}
	if err := NarrowsOnly(stored, NarrowFields{Mounts: &narrowed}); err != nil {
		t.Fatalf("dropping a stored mount by omitting it must be allowed, got: %v", err)
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

