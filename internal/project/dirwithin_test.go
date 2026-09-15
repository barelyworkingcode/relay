package project

import (
	"path/filepath"
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
