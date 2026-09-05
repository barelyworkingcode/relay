package project

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/enrolment"
)

// ValidateMounts is ValidateShape's mount-specific half, factored into its
// own file for the same reason scope_breadth.go is its own file: the rule
// set is self-contained and long enough to want its own place. Called from
// ValidateShape for every project, local and remote — the very first check
// is what makes a local project's non-empty Mounts a refusal rather than a
// silently-ignored field.
func ValidateMounts(proj *config.Project) error {
	if len(proj.Mounts) == 0 {
		return nil
	}
	if !proj.IsRemote() {
		return fmt.Errorf("mounts are only valid on a remote project: a local project already reaches its directory through shells and fsMCP; the mount plane is for a client on another machine")
	}

	seenID := make(map[string]config.MountGrant, len(proj.Mounts))
	for _, m := range proj.Mounts {
		if !enrolment.SafeID(m.ID) {
			return fmt.Errorf("mount id %q is not a safe id (letters, digits, -, _, . only, not empty, not . or ..)", m.ID)
		}
		if _, dup := seenID[m.ID]; dup {
			return fmt.Errorf("mount id %q is used by more than one mount in this project", m.ID)
		}
		seenID[m.ID] = m

		// An unrecognised m.Access ("rw", "Write", …) is not a refusal:
		// MountGrant.AccessMode already reads anything other than exactly
		// "write" as read, so such a value silently narrows. Refusing here
		// would make ValidateMounts a second, stricter copy of that rule,
		// free to disagree with it later — so it is deliberately not checked.

		if err := validateMountPath(m.Path); err != nil {
			return fmt.Errorf("mount %q: %w", m.ID, err)
		}
	}

	if err := validateMountsDontOverlap(proj.Mounts); err != nil {
		return err
	}
	return nil
}

// validateMountPath checks the operator-authored path shape and the
// filesystem: absolute, exactly its own filepath.Clean form (a trailing
// slash is refused rather than silently cleaned — the operator wrote a value
// that doesn't equal its clean form, and this is caught once, loudly, at the
// point they wrote it), an existing directory, not a symlink, and not a
// breadth the operator must not be allowed to grant at all (a filesystem
// root — a whole home directory is allowed, with a warning surfaced
// elsewhere by MountBreadthWarnings).
func validateMountPath(path string) error {
	if path == "" {
		return fmt.Errorf("path is required")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("path must be absolute: %q", path)
	}
	if clean := filepath.Clean(path); clean != path {
		return fmt.Errorf("path must be its own clean form (got %q, want %q) — a trailing slash or a %q/%q segment is refused rather than silently cleaned", path, clean, ".", "..")
	}
	if ScopeEntryBreadth(path) == ScopeBreadthRoot {
		return fmt.Errorf("path %q resolves to a filesystem root, which a mount grant may not reach outright — the widest a mount may grant is a whole home directory", path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("path %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("path %q is a symlink, which a mount grant may not have as its root", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("path %q is not a directory", path)
	}
	return nil
}

// validateMountsDontOverlap refuses two mounts in one project sharing a path
// or nesting: an audit row's mcp_id must name one root unambiguously (a
// later piece's concern; this is where that invariant is made true by
// construction). Checked as a path-prefix relationship using
// filepath.Separator, not a string prefix, so "/a/bc" is not treated as
// nested under "/a/b". Every unordered pair is compared, not just adjacent
// ones in sorted order: a lexicographic neighbor is not a path-tree neighbor
// ("/a-b" sorts between "/a" and "/a/b", so an adjacent-only pass over
// "/a", "/a-b", "/a/b" would never compare "/a" against "/a/b"). O(n²) on
// purpose: a project's mount count is small, realistically single digits.
// The sort is only to make the first error deterministic.
func validateMountsDontOverlap(mounts []config.MountGrant) error {
	sorted := make([]config.MountGrant, len(mounts))
	copy(sorted, mounts)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			a, b := sorted[i], sorted[j]
			if a.Path == b.Path {
				return fmt.Errorf("mounts %q and %q share the same path %q", a.ID, b.ID, a.Path)
			}
			if strings.HasPrefix(a.Path, b.Path+string(filepath.Separator)) {
				return fmt.Errorf("mount %q (%q) is nested inside mount %q (%q)", a.ID, a.Path, b.ID, b.Path)
			}
			if strings.HasPrefix(b.Path, a.Path+string(filepath.Separator)) {
				return fmt.Errorf("mount %q (%q) is nested inside mount %q (%q)", b.ID, b.Path, a.ID, a.Path)
			}
		}
	}
	return nil
}

// FindMount looks up one mount by id within a project. The bool mirrors a
// map's comma-ok idiom.
func FindMount(proj *config.Project, id string) (config.MountGrant, bool) {
	for _, m := range proj.Mounts {
		if m.ID == id {
			return m, true
		}
	}
	return config.MountGrant{}, false
}

// MountBreadthWarnings reuses ScopeEntryBreadth/scopeBreadthPhrase — the same
// single source of truth scope_breadth.go uses for a context value — so a
// mount whose path is a whole home directory is described in exactly the
// same words on every operator surface. Returns nil when there is nothing to
// warn about. A root-breadth path never reaches here in practice (refused at
// validation), but this function does not assume that: it answers the
// classification question for whatever path it is given, independent of
// whether the directory still exists (a mount whose directory was deleted
// after validation is a warning surface's problem, not this function's).
func MountBreadthWarnings(m config.MountGrant) []string {
	if phrase := scopeBreadthPhrase(ScopeEntryBreadth(m.Path)); phrase != "" {
		return []string{phrase}
	}
	return nil
}
