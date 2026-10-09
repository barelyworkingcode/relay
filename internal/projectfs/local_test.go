package projectfs_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/barelyworkingcode/relay/internal/projectfs"
)

func TestCleanRel(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""}, {"/", ""}, {".", ""}, {"./", ""}, {"//", ""},
		{"a", "a"}, {"/a", "a"}, {"a/", "a"}, {"a//b", "a/b"}, {"a/./b", "a/b"}, {"./a/.", "a"},
		{"a..b", "a..b"}, {"...", "..."}, {"..a", "..a"}, {"a/b.c/d", "a/b.c/d"},
	} {
		got, err := projectfs.CleanRel(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("CleanRel(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, tc := range []struct{ in, code string }{
		{"..", projectfs.CodeTraversal}, {"../a", projectfs.CodeTraversal}, {"a/..", projectfs.CodeTraversal},
		{"a/../b", projectfs.CodeTraversal}, {"/../a", projectfs.CodeTraversal}, {"a/b/../../..", projectfs.CodeTraversal},
		{"a\x00b", projectfs.CodeInvalid}, {"\x00", projectfs.CodeInvalid},
	} {
		got, err := projectfs.CleanRel(tc.in)
		if projectfs.CodeOf(err) != tc.code || got != "" {
			t.Errorf("CleanRel(%q) = %q, %v; want code %s", tc.in, got, err, tc.code)
		}
	}
}

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"a", "a b", "...", "a.b", ".hidden", "é"} {
		if err := projectfs.ValidateName(ok); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", "/a", "a/", "a\x00b"} {
		if err := projectfs.ValidateName(bad); projectfs.CodeOf(err) != projectfs.CodeInvalid {
			t.Errorf("ValidateName(%q) = %v, want INVALID", bad, err)
		}
	}
}

func TestNewLocalRefusesAnUnusableRoot(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	put(t, file, "x")
	for name, tc := range map[string]struct{ root, code string }{
		"empty":   {"", projectfs.CodeNotAvailable},
		"missing": {filepath.Join(dir, "nope"), projectfs.CodeENOENT},
		"a file":  {file, projectfs.CodeENOTDIR},
	} {
		_, err := projectfs.NewLocal(tc.root)
		if projectfs.CodeOf(err) != tc.code {
			t.Errorf("%s: want %s, got %v", name, tc.code, err)
		}
	}
}

// The project root may itself sit behind a link (a project under /tmp); only
// what lies below it is held to the never-follow rule.
func TestLocalRootBehindALinkIsUsable(t *testing.T) {
	real := t.TempDir()
	put(t, filepath.Join(real, "a.txt"), "A")
	link := filepath.Join(t.TempDir(), "rootlink")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	b, err := projectfs.NewLocal(link)
	if err != nil {
		t.Fatal(err)
	}
	if content, _, err := b.Read(ctxT(t), "a.txt", 0); err != nil || content != "A" {
		t.Errorf("read through a root link = %q, %v", content, err)
	}
}

func TestLocalCaseOnlyRenameOfTheSameEntry(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "Notes.md"), "n")
	b, err := projectfs.NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	p, err := b.Rename(ctxT(t), "Notes.md", "notes.md")
	if err != nil || p != "notes.md" {
		t.Fatalf("case-only rename = %q, %v", p, err)
	}
	names, _ := os.ReadDir(root)
	if len(names) != 1 || names[0].Name() != "notes.md" {
		t.Errorf("directory holds %v after the rename", names)
	}
}
