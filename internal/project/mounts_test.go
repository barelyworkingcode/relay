package project

// mounts.go's own tests: ValidateMounts (kind gate, per-mount validation,
// overlap), validateMountPath (the path-shape/filesystem checks), FindMount
// and MountGrant.AccessMode.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
)

// mkMountDir returns a real, ordinary bounded directory under t.TempDir() —
// the shape most mount tests want as a starting point.
func mkMountDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// ---------------------------------------------------------------------------
// ValidateMounts: the kind gate.
// ---------------------------------------------------------------------------

func TestValidateMounts_RefusesNonEmptyMountsOnALocalProject(t *testing.T) {
	proj := &config.Project{
		Kind:   config.ProjectKindLocal,
		Mounts: []config.MountGrant{{ID: "m1", Path: mkMountDir(t)}},
	}
	if err := ValidateMounts(proj); err == nil {
		t.Fatal("expected refusal of non-empty Mounts on a kind:local project")
	}
}

// The zero value of Kind reads as local (project.go's own documented rule),
// so a project built with no Kind set at all must be refused exactly like an
// explicit kind:local one — an equality check against ProjectKindLocal would
// pass this by accident, but IsRemote() is what ValidateMounts actually
// calls, and that is what this pins.
func TestValidateMounts_RefusesNonEmptyMountsOnAZeroValueKindProject(t *testing.T) {
	proj := &config.Project{
		Mounts: []config.MountGrant{{ID: "m1", Path: mkMountDir(t)}},
	}
	if proj.Kind != "" {
		t.Fatalf("test setup: expected zero-value Kind, got %q", proj.Kind)
	}
	if err := ValidateMounts(proj); err == nil {
		t.Fatal("expected refusal of non-empty Mounts on a zero-value-kind (local) project")
	}
}

func TestValidateMounts_NilMountsIsFineOnEveryProjectKind(t *testing.T) {
	for _, kind := range []config.ProjectKind{"", config.ProjectKindLocal, config.ProjectKindRemote} {
		proj := &config.Project{Kind: kind}
		if err := ValidateMounts(proj); err != nil {
			t.Errorf("kind %q: absent Mounts should not be refused, got: %v", kind, err)
		}
	}
}

// ---------------------------------------------------------------------------
// ValidateMounts on a remote project: per-mount and cross-mount checks.
// ---------------------------------------------------------------------------

func TestValidateMounts_AcceptsAnOrdinaryBoundedDirectoryOnARemoteProject(t *testing.T) {
	proj := &config.Project{
		Kind:   config.ProjectKindRemote,
		Mounts: []config.MountGrant{{ID: "m1", Path: mkMountDir(t)}},
	}
	if err := ValidateMounts(proj); err != nil {
		t.Fatalf("expected an ordinary bounded directory to be accepted, got: %v", err)
	}
}

func TestValidateMounts_RefusesDuplicateMountIDs(t *testing.T) {
	proj := &config.Project{
		Kind: config.ProjectKindRemote,
		Mounts: []config.MountGrant{
			{ID: "dup", Path: mkMountDir(t)},
			{ID: "dup", Path: mkMountDir(t)},
		},
	}
	if err := ValidateMounts(proj); err == nil {
		t.Fatal("expected refusal of two mounts sharing an id")
	}
}

func TestValidateMounts_RefusesDuplicatePaths(t *testing.T) {
	dir := mkMountDir(t)
	proj := &config.Project{
		Kind: config.ProjectKindRemote,
		Mounts: []config.MountGrant{
			{ID: "m1", Path: dir},
			{ID: "m2", Path: dir},
		},
	}
	if err := ValidateMounts(proj); err == nil {
		t.Fatal("expected refusal of two mounts sharing a path")
	}
}

// Unsafe mount ids are refused through enrolment.SafeID: a slash would let a
// mount id escape into a path, an empty or "."/".." id has no safe meaning
// as a component, and a shell metacharacter has no business in a value that
// might land in a log line or a CLI flag with no quoting.
func TestValidateMounts_RefusesUnsafeMountIDs(t *testing.T) {
	for _, id := range []string{"", ".", "..", "a/b", "a b", "a;rm -rf /", "a$HOME", "a`b`"} {
		proj := &config.Project{
			Kind:   config.ProjectKindRemote,
			Mounts: []config.MountGrant{{ID: id, Path: mkMountDir(t)}},
		}
		if err := ValidateMounts(proj); err == nil {
			t.Errorf("mount id %q: expected refusal as an unsafe id", id)
		}
	}
}

// ---------------------------------------------------------------------------
// Nesting/overlap: validateMountsDontOverlap must compare every unordered
// pair, not just lexicographic neighbours — a regression test for a real
// review finding.
// ---------------------------------------------------------------------------

// TestValidateMounts_CatchesNestingAcrossALexicographicNonNeighbour is the
// exact bug that was found and fixed during review: "/a-b" sorts between
// "/a" and "/a/sub", so a check that only compared adjacent entries after
// sorting would never compare "/a" against "/a/sub" directly and would miss
// the nesting. Three REAL directories are created so os.Lstat inside
// validateMountPath has something legitimate to find, and the nesting must
// still be caught regardless of that ordering.
func TestValidateMounts_CatchesNestingAcrossALexicographicNonNeighbour(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a")
	aDashB := filepath.Join(root, "a-b")
	aSub := filepath.Join(a, "sub")
	for _, dir := range []string{a, aDashB, aSub} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	// Sanity check on the fixture itself: "/a-b" really does sort between
	// "/a" and "/a/sub" lexicographically, which is the whole point of the
	// regression.
	if a >= aDashB || aDashB >= aSub {
		t.Fatalf("fixture does not have the intended lexicographic order: %q < %q < %q", a, aDashB, aSub)
	}

	proj := &config.Project{
		Kind: config.ProjectKindRemote,
		Mounts: []config.MountGrant{
			{ID: "root-a", Path: a},
			{ID: "sibling", Path: aDashB},
			{ID: "nested-sub", Path: aSub},
		},
	}
	if err := ValidateMounts(proj); err == nil {
		t.Fatal("expected refusal: nested-sub is nested inside root-a, even though sibling sorts between them")
	}
}

func TestValidateMounts_UnrelatedDirectoriesDoNotOverlap(t *testing.T) {
	proj := &config.Project{
		Kind: config.ProjectKindRemote,
		Mounts: []config.MountGrant{
			{ID: "m1", Path: mkMountDir(t)},
			{ID: "m2", Path: mkMountDir(t)},
			{ID: "m3", Path: mkMountDir(t)},
		},
	}
	if err := ValidateMounts(proj); err != nil {
		t.Fatalf("three unrelated directories must not be treated as overlapping: %v", err)
	}
}

// ---------------------------------------------------------------------------
// validateMountPath: path shape and filesystem checks, tested directly.
// ---------------------------------------------------------------------------

func TestValidateMountPath_RefusesRelativePath(t *testing.T) {
	if err := validateMountPath("relative/dir"); err == nil {
		t.Fatal("expected refusal of a relative path")
	}
}

func TestValidateMountPath_RefusesTrailingSlashRatherThanCleaningIt(t *testing.T) {
	dir := mkMountDir(t)
	if err := validateMountPath(dir + "/"); err == nil {
		t.Fatal("expected refusal of a trailing-slash path; it must not be silently cleaned")
	}
}

func TestValidateMountPath_RefusesFilesystemRoot(t *testing.T) {
	if err := validateMountPath("/"); err == nil {
		t.Fatal("expected refusal of the filesystem root")
	}
}

func TestValidateMountPath_RefusesSymlink(t *testing.T) {
	target := mkMountDir(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := validateMountPath(link); err == nil {
		t.Fatal("expected refusal of a symlink as a mount root")
	}
}

func TestValidateMountPath_RefusesNonExistentPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if err := validateMountPath(missing); err == nil {
		t.Fatal("expected refusal of a non-existent path")
	}
}

func TestValidateMountPath_RefusesAFileNotADirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "plain-file")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := validateMountPath(file); err == nil {
		t.Fatal("expected refusal of a file (not a directory)")
	}
}

func TestValidateMountPath_AcceptsAnOrdinaryBoundedDirectory(t *testing.T) {
	if err := validateMountPath(mkMountDir(t)); err != nil {
		t.Fatalf("expected an ordinary bounded directory to be accepted, got: %v", err)
	}
}

// A two-segment /Users/<name> (or /home/<name>) path is a whole home
// directory: accepted by validateMountPath (only a filesystem root is
// refused outright), but flagged by MountBreadthWarnings for the operator
// surfaces to show loudly. os.UserHomeDir reads $HOME, which this test does
// not override, so it resolves to the real, pre-existing home directory —
// exactly the shape ScopeEntryBreadth classifies as "home".
func TestValidateMountPath_AcceptsHomeDirectoryPathButItIsFlaggedAsBreadth(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory available in this environment: %v", err)
	}
	if got := ScopeEntryBreadth(home); got != ScopeBreadthHome {
		t.Skipf("this environment's home directory %q is not two-segment /Users or /home shaped (breadth=%q); skipping", home, got)
	}

	if err := validateMountPath(home); err != nil {
		t.Fatalf("a home-directory path must be accepted (only a filesystem root is refused outright), got: %v", err)
	}

	warnings := MountBreadthWarnings(config.MountGrant{ID: "home", Path: home})
	if len(warnings) == 0 {
		t.Fatal("expected MountBreadthWarnings to flag a whole-home-directory mount")
	}
}

// ---------------------------------------------------------------------------
// FindMount
// ---------------------------------------------------------------------------

func TestFindMount_FoundAndNotFound(t *testing.T) {
	proj := &config.Project{
		Kind: config.ProjectKindRemote,
		Mounts: []config.MountGrant{
			{ID: "mail", Path: mkMountDir(t), Access: config.AccessWrite},
		},
	}
	got, ok := FindMount(proj, "mail")
	if !ok {
		t.Fatal("expected to find mount \"mail\"")
	}
	if got.ID != "mail" || got.Access != config.AccessWrite {
		t.Fatalf("FindMount returned %+v, want the stored mail mount", got)
	}

	_, ok = FindMount(proj, "does-not-exist")
	if ok {
		t.Fatal("expected FindMount to report false for an id that is not on the project")
	}
}

// ---------------------------------------------------------------------------
// MountGrant.AccessMode
// ---------------------------------------------------------------------------

func TestMountGrant_AccessMode(t *testing.T) {
	cases := []struct {
		access string
		want   string
	}{
		{"write", config.AccessWrite},
		{"", config.AccessRead},
		{"Write", config.AccessRead}, // a typo must narrow, never widen
		{"rw", config.AccessRead},
		{"read", config.AccessRead},
	}
	for _, c := range cases {
		m := config.MountGrant{ID: "m", Path: "/tmp/x", Access: c.access}
		if got := m.AccessMode(); got != c.want {
			t.Errorf("AccessMode() with Access=%q = %q, want %q", c.access, got, c.want)
		}
	}
}
