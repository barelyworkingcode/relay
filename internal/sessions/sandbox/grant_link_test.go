package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// realTempDir is t.TempDir() in the kernel's spelling, so a path built under
// it is spelled the same by the caller and by any walk from `/`.
func realTempDir(t *testing.T) string {
	t.Helper()
	return evalSymlinks(t, t.TempDir())
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
}

func writeFile(t *testing.T, p string) {
	t.Helper()
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func linkedGrantText(grant, link string) string {
	return `read_write: grant "` + grant + `" follows symlink "` + link + `"; use the real path`
}

type linkedGrantCase struct {
	name  string
	setup func(t *testing.T) (spec Spec, grant, link string)
}

func linkedGrantCases() []linkedGrantCase {
	return []linkedGrantCase{
		{"directory replaced by a link to a sibling", func(t *testing.T) (Spec, string, string) {
			root := realTempDir(t)
			proj, sibling := filepath.Join(root, "proj"), filepath.Join(root, "sibling")
			mkdirs(t, sibling)
			symlink(t, sibling, proj)
			return Spec{ReadWrite: []string{proj}}, proj, proj
		}},
		{"file replaced by a link to a file", func(t *testing.T) (Spec, string, string) {
			root := realTempDir(t)
			f, target := filepath.Join(root, "state.json"), filepath.Join(root, "other.json")
			writeFile(t, target)
			symlink(t, target, f)
			return Spec{ReadWriteFiles: []string{f}}, f, f
		}},
		{"file replaced by renaming a .tmp link over it", func(t *testing.T) (Spec, string, string) {
			root := realTempDir(t)
			f, target := filepath.Join(root, "state.json"), filepath.Join(root, "other.json")
			writeFile(t, f)
			writeFile(t, target)
			tmp := f + ".tmp.1"
			symlink(t, target, tmp)
			if err := os.Rename(tmp, f); err != nil {
				t.Fatalf("rename: %v", err)
			}
			return Spec{ReadWriteFiles: []string{f}}, f, f
		}},
		{"intermediate directory replaced by a link", func(t *testing.T) (Spec, string, string) {
			root := realTempDir(t)
			acme, elsewhere := filepath.Join(root, "acme"), filepath.Join(root, "elsewhere")
			mkdirs(t, acme, filepath.Join(elsewhere, "b"))
			symlink(t, elsewhere, filepath.Join(acme, "a"))
			grant := filepath.Join(acme, "a", "b")
			return Spec{ReadWrite: []string{acme, grant}}, grant, filepath.Join(acme, "a")
		}},
		{"intermediate link with a tail that does not exist yet", func(t *testing.T) (Spec, string, string) {
			root := realTempDir(t)
			acme, elsewhere := filepath.Join(root, "acme"), filepath.Join(root, "elsewhere")
			mkdirs(t, acme, elsewhere)
			symlink(t, elsewhere, filepath.Join(acme, "a"))
			grant := filepath.Join(acme, "a", "new", "deeper")
			return Spec{ReadWrite: []string{grant}}, grant, filepath.Join(acme, "a")
		}},
		{"link directly in /private/tmp", func(t *testing.T) (Spec, string, string) {
			target := realTempDir(t)
			name, err := os.MkdirTemp("/private/tmp", "relay-grant-link-")
			if err != nil {
				t.Fatalf("temp name: %v", err)
			}
			if err := os.Remove(name); err != nil {
				t.Fatalf("remove: %v", err)
			}
			symlink(t, target, name)
			t.Cleanup(func() { _ = os.Remove(name) })
			return Spec{ReadWrite: []string{name}}, name, name
		}},
		{"dangling link", func(t *testing.T) (Spec, string, string) {
			root := realTempDir(t)
			link := filepath.Join(root, "proj")
			symlink(t, filepath.Join(root, "absent"), link)
			return Spec{ReadWrite: []string{link}}, link, link
		}},
		{"link in a directory the user owns but cannot write", func(t *testing.T) (Spec, string, string) {
			root := realTempDir(t)
			locked, target := filepath.Join(root, "readonly"), filepath.Join(root, "target")
			mkdirs(t, locked, target)
			link := filepath.Join(locked, "proj")
			symlink(t, target, link)
			if err := os.Chmod(locked, 0o555); err != nil {
				t.Fatalf("chmod: %v", err)
			}
			t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
			return Spec{ReadWrite: []string{link}}, link, link
		}},
	}
}

func skipAsRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root can write every directory, so no link is in a locked one")
	}
}

func TestRender_RefusesReadWriteGrantThroughUserLink(t *testing.T) {
	skipAsRoot(t)
	for _, tc := range linkedGrantCases() {
		t.Run(tc.name, func(t *testing.T) {
			spec, grant, link := tc.setup(t)
			got, err := Render(spec)
			if err == nil {
				t.Fatalf("Render accepted a read-write grant through %s:\n%s", link, got)
			}
			if want := linkedGrantText(grant, link); err.Error() != want {
				t.Fatalf("error = %q\nwant    %q", err.Error(), want)
			}
			var linked *LinkedGrantError
			if !errors.As(err, &linked) {
				t.Fatalf("error %v is not a *LinkedGrantError", err)
			}
			if linked.Grant != grant || linked.Link != link {
				t.Errorf("LinkedGrantError = %+v, want Grant %s, Link %s", *linked, grant, link)
			}
		})
	}
}

func TestRender_ReadWriteThroughSystemLinksStillRenders(t *testing.T) {
	type rendered struct {
		spec Spec
		want []string
	}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) rendered
	}{
		{"directory reached through /var", func(t *testing.T) rendered {
			root := t.TempDir()
			mkdirs(t, filepath.Join(root, "proj"))
			return rendered{Spec{ReadWrite: []string{filepath.Join(root, "proj")}},
				[]string{`(subpath "` + filepath.Join(evalSymlinks(t, root), "proj") + `")`}}
		}},
		{"file reached through /var", func(t *testing.T) rendered {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "state.json"))
			return rendered{Spec{ReadWriteFiles: []string{filepath.Join(root, "state.json")}},
				[]string{`(regex #"^` + strings.ReplaceAll(filepath.Join(evalSymlinks(t, root), "state.json"), ".", `\.`)}}
		}},
		{"directory reached through /tmp", func(t *testing.T) rendered {
			dir, err := os.MkdirTemp("/tmp", "relay-grant-tmp-")
			if err != nil {
				t.Fatalf("temp dir: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			return rendered{Spec{ReadWrite: []string{dir}},
				[]string{`(subpath "/private/tmp/` + filepath.Base(dir) + `")`}}
		}},
		{"/tmp itself", func(t *testing.T) rendered {
			return rendered{Spec{ReadWrite: []string{"/tmp"}},
				[]string{`(subpath "/private/tmp")`, `(literal "/tmp")`}}
		}},
		{"a tail that does not exist yet", func(t *testing.T) rendered {
			root := t.TempDir()
			return rendered{Spec{ReadWrite: []string{filepath.Join(root, "not", "yet")}},
				[]string{`(subpath "` + filepath.Join(evalSymlinks(t, root), "not", "yet") + `")`}}
		}},
		{"a wrongly cased directory", func(t *testing.T) rendered {
			dir := filepath.Join(realTempDir(t), "MixedCaseProject")
			mkdirs(t, dir)
			wrong := strings.ToLower(dir)
			if _, err := os.Stat(wrong); err != nil {
				t.Skip("this volume is case-sensitive")
			}
			return rendered{Spec{ReadWrite: []string{wrong}}, []string{`(subpath "` + dir + `")`}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.setup(t)
			got, err := Render(r.spec)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			for _, want := range r.want {
				if !strings.Contains(got, want) {
					t.Errorf("profile lacks %s\n%s", want, got)
				}
			}
		})
	}
}

func evalSymlinks(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return r
}

// swapAfterWalk runs swap once, when Render has walked target and not yet
// spelled it, and reports whether that moment came.
func swapAfterWalk(t *testing.T, target string, swap func()) *bool {
	t.Helper()
	fired := new(bool)
	original := beforeSpell
	t.Cleanup(func() { beforeSpell = original })
	beforeSpell = func(resolved string) {
		if resolved == target && !*fired {
			*fired = true
			swap()
		}
	}
	return fired
}

// replaceWithLink moves dir aside and puts a link to target in its place.
func replaceWithLink(t *testing.T, dir, target string) {
	t.Helper()
	if err := os.Rename(dir, dir+".moved"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	symlink(t, target, dir)
}

func TestRender_LinkSwappedInAfterTheWalkKeepsTheWalkedSpelling(t *testing.T) {
	for _, tc := range []struct {
		name     string
		swapping string
	}{
		{"final component", "proj"},
		{"intermediate component", "acme"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := realTempDir(t)
			grant, other := filepath.Join(root, "acme", "proj"), filepath.Join(root, "other")
			mkdirs(t, grant, filepath.Join(other, "proj"))
			swapped := filepath.Join(root, "acme")
			if tc.swapping == "proj" {
				swapped, other = grant, filepath.Join(other, "proj")
			}
			fired := swapAfterWalk(t, grant, func() { replaceWithLink(t, swapped, other) })

			got, err := Render(Spec{ReadWrite: []string{grant}})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if !*fired {
				t.Fatal("beforeSpell never saw the read-write grant")
			}
			if !strings.Contains(got, `(subpath "`+grant+`")`) {
				t.Errorf("profile lacks the walked spelling %s\n%s", grant, got)
			}
			if strings.Contains(got, filepath.Join(root, "other")) {
				t.Errorf("profile names the swapped-in link's target\n%s", got)
			}
		})
	}
}

func TestRender_ReadGrantOnADanglingLinkNeverNamesTheTarget(t *testing.T) {
	for _, tc := range []struct{ name, tail string }{
		{"the link itself", ""},
		{"a path below the link", "sub/tail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := realTempDir(t)
			link, target := filepath.Join(root, "planted"), filepath.Join(root, "absent-target")
			symlink(t, target, link)
			grant := filepath.Join(link, tc.tail)

			got, err := Render(Spec{Read: []string{grant}})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if !strings.Contains(got, `(subpath "`+grant+`")`) {
				t.Errorf("profile lacks the grant's own spelling %s\n%s", grant, got)
			}
			if strings.Contains(got, target) {
				t.Errorf("profile names the dangling link's target %s, which a later write could create\n%s", target, got)
			}
		})
	}
}

func TestRender_RefusesARelativeEntry(t *testing.T) {
	for name, spec := range map[string]Spec{
		"read":                   {Read: []string{"relative/dir"}},
		"deny":                   {Deny: []string{"relative/dir"}},
		"unix connect allow":     {UnixConnectAllow: []string{"relative/relay.sock"}},
		"unix connect deny dir":  {UnixConnectDenyDirs: []string{"relative/dir"}},
		"unix connect deny path": {UnixConnectDenyPaths: []string{"relative/relay.sock"}},
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := Render(spec); err == nil {
				t.Fatalf("Render accepted a relative %s entry:\n%s", name, got)
			}
		})
	}
}

func TestRender_LinkCycleOnAReadWriteGrantFailsWithoutBlamingALink(t *testing.T) {
	root := realTempDir(t)
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	symlink(t, b, a)
	symlink(t, a, b)

	got, err := Render(Spec{ReadWrite: []string{a}})
	if err == nil {
		t.Fatalf("Render accepted a read-write grant on a link cycle:\n%s", got)
	}
	var linked *LinkedGrantError
	if errors.As(err, &linked) {
		t.Fatalf("a link cycle was reported as a linked grant: %v", err)
	}
}
