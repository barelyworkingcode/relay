package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readFixture is a stand-in home H, which the tests put in W, and a stand-in
// Homebrew prefix B beside it, outside W, whose bin/tool reaches its keg
// through a relative link.
type readFixture struct {
	root, home, brew string
}

func newReadFixture(t *testing.T, root string) readFixture {
	t.Helper()
	f := readFixture{root: root, home: filepath.Join(root, "home"), brew: filepath.Join(root, "brew")}
	keg := filepath.Join(f.brew, "Cellar", "tool", "1", "bin")
	mkdirs(t, f.h("share"), f.h("secret"), f.h("other", "sub"), f.h("dotfiles"), filepath.Join(f.brew, "bin"), keg)
	writeFile(t, f.h("secret", "key"))
	writeFile(t, f.h("dotfiles", "zshrc"))
	writeFile(t, filepath.Join(keg, "tool"))
	symlink(t, "../Cellar/tool/1/bin/tool", filepath.Join(f.brew, "bin", "tool"))
	return f
}

func (f readFixture) h(parts ...string) string {
	return filepath.Join(append([]string{f.home}, parts...)...)
}

// varAliasRoot is a temp dir in both spellings: through /var and resolved.
func varAliasRoot(t *testing.T) (alias, real string) {
	t.Helper()
	alias = t.TempDir()
	real = evalSymlinks(t, alias)
	if alias == real {
		t.Skip("the temp dir has no second spelling on this machine")
	}
	return alias, real
}

// withBaselineReadDirs points the fixed read baseline at dirs for one test.
func withBaselineReadDirs(t *testing.T, dirs []string) {
	t.Helper()
	original := baselineReadDirs
	t.Cleanup(func() { baselineReadDirs = original })
	baselineReadDirs = dirs
}

func TestRender_RefusesReadGrantThroughLinkTheWritableRootsCover(t *testing.T) {
	skipAsRoot(t)
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) (spec Spec, grant, link string)
	}{
		{"directory entry is a link", func(t *testing.T) (Spec, string, string) {
			f := newReadFixture(t, realTempDir(t))
			app := f.h("share", "app")
			symlink(t, f.h("secret"), app)
			return Spec{Read: []string{app}, Writable: []string{f.home}}, app, app
		}},
		{"intermediate component is a link", func(t *testing.T) (Spec, string, string) {
			f := newReadFixture(t, realTempDir(t))
			app := f.h("share", "app")
			symlink(t, f.h("other"), app)
			grant := filepath.Join(app, "sub")
			return Spec{Read: []string{grant}, Writable: []string{f.home}}, grant, app
		}},
		{"read file entry is a dotfiles link", func(t *testing.T) (Spec, string, string) {
			f := newReadFixture(t, realTempDir(t))
			rc := f.h(".zshrc")
			symlink(t, f.h("dotfiles", "zshrc"), rc)
			return Spec{ReadFiles: []string{rc}, Writable: []string{f.home}}, rc, rc
		}},
		{"a later link on the walk is the covered one", func(t *testing.T) (Spec, string, string) {
			f := newReadFixture(t, realTempDir(t))
			app := f.h("share", "app")
			symlink(t, f.h("secret"), app)
			grant := filepath.Join(f.brew, "bin", "probe")
			symlink(t, filepath.Join(app, "key"), grant)
			return Spec{ReadFiles: []string{grant}, Writable: []string{f.home}}, grant, app
		}},
		{"baseline read dir", func(t *testing.T) (Spec, string, string) {
			f := newReadFixture(t, realTempDir(t))
			app := f.h("share", "app")
			symlink(t, f.h("secret"), app)
			withBaselineReadDirs(t, []string{app})
			return Spec{Writable: []string{f.home}}, app, app
		}},
		{"the launch's own read-write dir", func(t *testing.T) (Spec, string, string) {
			f := newReadFixture(t, realTempDir(t))
			app := f.h("share", "app")
			symlink(t, f.h("secret"), app)
			return Spec{ReadWrite: []string{f.home}, Read: []string{app}, Writable: []string{f.brew}}, app, app
		}},
		{"the launch's own read-write file", func(t *testing.T) (Spec, string, string) {
			f := newReadFixture(t, realTempDir(t))
			app := f.h("app")
			symlink(t, f.h("secret"), app)
			writeFile(t, f.h("state.json"))
			return Spec{ReadWriteFiles: []string{f.h("state.json")}, Read: []string{app}, Writable: []string{f.brew}}, app, app
		}},
		{"a file root covers its parent directory", func(t *testing.T) (Spec, string, string) {
			f := newReadFixture(t, realTempDir(t))
			app := f.h("app")
			symlink(t, f.h("secret"), app)
			writeFile(t, f.h(".claude.json"))
			return Spec{Read: []string{app}, Writable: []string{f.h(".claude.json")}}, app, app
		}},
		{"root spelled in another case", func(t *testing.T) (Spec, string, string) {
			f := newReadFixture(t, realTempDir(t))
			flipped := filepath.Join(f.root, "HOME")
			if _, err := os.Stat(flipped); err != nil {
				t.Skip("this volume is case-sensitive")
			}
			app := f.h("share", "app")
			symlink(t, f.h("secret"), app)
			return Spec{Read: []string{app}, Writable: []string{flipped}}, app, app
		}},
		{"root spelled through /var", func(t *testing.T) (Spec, string, string) {
			alias, real := varAliasRoot(t)
			f := newReadFixture(t, real)
			app := f.h("share", "app")
			symlink(t, f.h("secret"), app)
			return Spec{Read: []string{app}, Writable: []string{filepath.Join(alias, "home")}}, app, app
		}},
		{"grant spelled through /var", func(t *testing.T) (Spec, string, string) {
			alias, real := varAliasRoot(t)
			f := newReadFixture(t, real)
			app := f.h("share", "app")
			symlink(t, f.h("secret"), app)
			grant := filepath.Join(alias, "home", "share", "app")
			return Spec{Read: []string{grant}, Writable: []string{f.home}}, grant, app
		}},
		{"a root that is itself a link", func(t *testing.T) (Spec, string, string) {
			f := newReadFixture(t, realTempDir(t))
			root := filepath.Join(f.root, "rootlink")
			symlink(t, f.h("secret"), root)
			return Spec{Read: []string{root}, Writable: []string{root}}, root, root
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, grant, link := tc.setup(t)
			got, err := Render(spec)
			if err == nil {
				t.Fatalf("Render accepted a read grant through %s:\n%s", link, got)
			}
			want := `read grant "` + grant + `" follows symlink "` + link + `", which a sandboxed session could have made`
			if !strings.HasSuffix(err.Error(), want) {
				t.Fatalf("error = %q\nwant it to end %q", err.Error(), want)
			}
			var linked *LinkedReadError
			if !errors.As(err, &linked) {
				t.Fatalf("error %v is not a *LinkedReadError", err)
			}
			if linked.Grant != grant || linked.Link != link {
				t.Errorf("LinkedReadError = %+v, want Grant %s, Link %s", *linked, grant, link)
			}
		})
	}
}

func TestRender_ReadGrantsTheWritableRootsDoNotReachRenderAsBefore(t *testing.T) {
	skipAsRoot(t)
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) (spec Spec, want string)
	}{
		{"relative Homebrew chain outside the roots", func(t *testing.T) (Spec, string) {
			f := newReadFixture(t, realTempDir(t))
			keg := filepath.Join(f.brew, "Cellar", "tool", "1", "bin", "tool")
			return Spec{ReadFiles: []string{filepath.Join(f.brew, "bin", "tool")}, Writable: []string{f.home}}, `(literal "` + keg + `")`
		}},
		{"/tmp", func(t *testing.T) (Spec, string) {
			return Spec{Read: []string{"/tmp"}, Writable: []string{"/private/tmp"}}, `(subpath "/private/tmp")`
		}},
		{"/dev", func(t *testing.T) (Spec, string) {
			return Spec{Read: []string{"/dev"}, Writable: []string{"/dev"}}, `(subpath "/dev")`
		}},
		{"dangling link in a root", func(t *testing.T) (Spec, string) {
			f := newReadFixture(t, realTempDir(t))
			app := f.h("share", "app")
			symlink(t, f.h("absent"), app)
			return Spec{Read: []string{app}, Writable: []string{f.home}}, `(subpath "` + app + `")`
		}},
		{"missing and cycling roots", func(t *testing.T) (Spec, string) {
			f := newReadFixture(t, realTempDir(t))
			a, b := filepath.Join(f.root, "a"), filepath.Join(f.root, "b")
			symlink(t, b, a)
			symlink(t, a, b)
			return Spec{Read: []string{f.brew}, Writable: []string{f.h("missing"), a}}, `(subpath "` + f.brew + `")`
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, want := tc.setup(t)
			got, err := Render(spec)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			spec.Writable = nil
			before, err := Render(spec)
			if err != nil {
				t.Fatalf("Render without writable roots: %v", err)
			}
			if got != before {
				t.Errorf("writable roots changed the profile.\nwith:\n%s\nwithout:\n%s", got, before)
			}
			if !strings.Contains(got, want) {
				t.Errorf("profile lacks %s\n%s", want, got)
			}
		})
	}
}

func TestRender_RefusesARelativeWritableRoot(t *testing.T) {
	if got, err := Render(Spec{Writable: []string{"relative/dir"}}); err == nil {
		t.Errorf("Render accepted a relative writable root:\n%s", got)
	}
	if _, err := NewWritableSet([]string{"relative/dir"}); err == nil {
		t.Error("NewWritableSet accepted a relative root")
	}
}

func TestWritableSet_Covers(t *testing.T) {
	root := realTempDir(t)
	r := filepath.Join(root, "proj")
	mkdirs(t, filepath.Join(r, "a"), r+"2", filepath.Join(root, "sibling"))
	for _, p := range []string{filepath.Join(r, "x"), filepath.Join(r, "a", "b"), filepath.Join(r+"2", "x"), filepath.Join(root, "sibling", "x")} {
		writeFile(t, p)
	}
	set, err := NewWritableSet([]string{r})
	if err != nil {
		t.Fatalf("NewWritableSet: %v", err)
	}
	empty, err := NewWritableSet(nil)
	if err != nil {
		t.Fatalf("NewWritableSet(nil): %v", err)
	}
	for _, tc := range []struct {
		name string
		set  WritableSet
		path string
		want bool
	}{
		{"entry in the root", set, filepath.Join(r, "x"), true},
		{"entry deeper in the root", set, filepath.Join(r, "a", "b"), true},
		{"entry in a sibling sharing the root's prefix", set, filepath.Join(r+"2", "x"), false},
		{"entry in another sibling", set, filepath.Join(root, "sibling", "x"), false},
		{"empty set", empty, filepath.Join(r, "x"), false},
	} {
		if got := tc.set.Covers(tc.path); got != tc.want {
			t.Errorf("%s: Covers(%s) = %v, want %v", tc.name, tc.path, got, tc.want)
		}
	}
}

func TestRender_GoldenIgnoresWritableRoots(t *testing.T) {
	spec := goldenSpec()
	spec.Writable = []string{realTempDir(t), "/private/tmp"}
	got, err := Render(spec)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "golden_profile.sb"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if got != string(want) {
		t.Fatalf("writable roots changed the golden profile.\ngot:\n%s\nwant:\n%s", got, want)
	}
}
