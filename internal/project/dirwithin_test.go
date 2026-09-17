package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDirWithin_ResolvesSymlinks guards the macOS /var->/private/var (and
// /tmp) gotcha: a project stored under the symlink form must accept a
// directory expressed in the resolved form of the same location.
func TestDirWithin_ResolvesSymlinks(t *testing.T) {
	tmp := t.TempDir() // macOS: under /var/folders (a symlink to /private/var/folders)
	real, err := filepath.EvalSymlinks(tmp)
	if err != nil || real == tmp {
		t.Skip("temp dir has no symlink component on this platform")
	}
	// project = symlink form; dir = resolved-form subdir of the same place.
	if !DirWithin(filepath.Join(real, "sub"), tmp) {
		t.Errorf("symlink-equivalent subdir wrongly rejected: dir=%q project=%q", filepath.Join(real, "sub"), tmp)
	}
	if !DirWithin(filepath.Join(tmp, "sub"), real) {
		t.Errorf("symlink-equivalent subdir wrongly rejected (reverse): dir=%q project=%q", filepath.Join(tmp, "sub"), real)
	}
}

// Skipped on case-sensitive volumes, where a case variant is genuinely a
// different directory rather than an alias for the same one.
func TestDirWithin_CaseInsensitiveVolume(t *testing.T) {
	// A named element, not t.TempDir()'s numeric leaf — digits have no case.
	dir := filepath.Join(t.TempDir(), "ProjectDir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	variant := caseVariant(dir)
	if variant == "" {
		t.Skip("case-sensitive volume: no case variant resolves to the same directory")
	}

	if !DirWithin(dir, variant) {
		t.Errorf("dir %q not matched against case-variant project path %q", dir, variant)
	}
	if !DirWithin(filepath.Join(dir, "sub"), variant) {
		t.Errorf("subdirectory of %q not matched against %q", dir, variant)
	}
}

// TestDirWithin_RejectsSymlinkLeafEscape guards the identity fast path
// (dirWithinProjectByIdentity) specifically: a directory whose own leaf
// component is a symlink pointing outside the project must not be read as
// contained just because its literal (unresolved) parent happens to be the
// project path.
func TestDirWithin_RejectsSymlinkLeafEscape(t *testing.T) {
	proj := t.TempDir()
	outside := t.TempDir()
	escape := filepath.Join(proj, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if DirWithin(escape, proj) {
		t.Errorf("symlink leaf %q (-> %q) inside project %q was accepted as contained", escape, outside, proj)
	}
	// A symlink leaf pointing INSIDE the project must still be accepted.
	inside := filepath.Join(proj, "real-sub")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	link := filepath.Join(proj, "link-to-inside")
	if err := os.Symlink(inside, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if !DirWithin(link, proj) {
		t.Errorf("symlink leaf %q (-> %q) genuinely inside project %q was wrongly rejected", link, inside, proj)
	}
}

func TestDirWithin_RejectsOutsiders(t *testing.T) {
	proj := t.TempDir()
	other := t.TempDir()

	if DirWithin(other, proj) {
		t.Errorf("unrelated dir %q matched project %q", other, proj)
	}
	if DirWithin(filepath.Dir(proj), proj) {
		t.Errorf("parent of %q matched the project itself", proj)
	}
	// A path that doesn't exist yet still resolves textually.
	if !DirWithin(filepath.Join(proj, "not", "created", "yet"), proj) {
		t.Errorf("non-existent nested path should still match textually")
	}
}

// caseVariant returns a case-flipped form of dir's last element that stats to
// the same directory, or "" when the volume is case-sensitive.
func caseVariant(dir string) string {
	base := filepath.Base(dir)
	flipped := strings.ToUpper(base)
	if flipped == base {
		flipped = strings.ToLower(base)
	}
	if flipped == base {
		return ""
	}
	candidate := filepath.Join(filepath.Dir(dir), flipped)
	a, err1 := os.Stat(dir)
	b, err2 := os.Stat(candidate)
	if err1 != nil || err2 != nil || !os.SameFile(a, b) {
		return ""
	}
	return candidate
}

func TestDirWithin(t *testing.T) {
	cases := []struct {
		name    string
		dir     string
		project string
		want    bool
	}{
		{"empty dir is allowed (no cwd to validate)", "", "/home/p", true},
		{"exact match", "/home/p", "/home/p", true},
		{"subdir", "/home/p/sub/pkg", "/home/p", true},
		{"trailing-slash exact", "/home/p/", "/home/p", true},
		{"sibling escape", "/home/other", "/home/p", false},
		{"parent escape", "/home", "/home/p", false},
		{"dotdot traversal", "/home/p/../other", "/home/p", false},
		{"prefix-not-subdir", "/home/project2", "/home/project", false},
		{"empty project path", "/home/p", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DirWithin(tc.dir, tc.project); got != tc.want {
				t.Errorf("DirWithin(%q, %q) = %v, want %v", tc.dir, tc.project, got, tc.want)
			}
		})
	}
}
