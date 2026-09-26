package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const denyAncestorHead = "(deny file-write-unlink file-clone\n"

// denyAncestorBlock is the terms of the profile's one ancestor block and the
// offset of its head.
func denyAncestorBlock(t *testing.T, profile string) (terms []string, at int) {
	t.Helper()
	if n := strings.Count(profile, denyAncestorHead); n != 1 {
		t.Fatalf("profile has %d ancestor unlink-and-clone blocks, want exactly one:\n%s", n, profile)
	}
	at = strings.Index(profile, denyAncestorHead)
	terms, _ = blockTerms(profile[at+len(denyAncestorHead):])
	return terms, at
}

func literals(paths ...string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, `(literal "`+p+`")`)
	}
	return out
}

func TestRender_DenyAncestorBlockNamesEveryAncestorOnceAfterTheDenies(t *testing.T) {
	const g = "/private/tmp/relay-sandbox-golden"
	s := goldenSpec()
	s.Read = append(s.Read, g+"/work/shared")
	s.Deny = []string{g + "/home/.config/gh", g + "/home/.ssh", g + "/work/shared/keys", "/usr/local/relay-sandbox-golden/keys"}
	got, err := Render(s)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	terms, at := denyAncestorBlock(t, got)
	want := literals("/private", "/private/tmp", g, g+"/home", g+"/home/.config", g+"/relay", g+"/work", g+"/work/shared", "/usr", "/usr/local", "/usr/local/relay-sandbox-golden")
	if !slices.Equal(terms, want) {
		t.Errorf("ancestor block = %q\nwant            %q\n%s", terms, want, got)
	}
	for _, step := range []struct {
		name          string
		first, second int
	}{
		{"read-write grants before the ancestor block", strings.Index(got, "(allow file-read* file-write*"), at},
		{"Spec.Deny block before the ancestor block", strings.Index(got, `(subpath "`+g+`/home/.ssh")`), at},
		{"ancestor block before the unix-socket rules", at, strings.Index(got, "(deny network-outbound")},
	} {
		if step.first < 0 || step.second < 0 || step.first >= step.second {
			t.Errorf("%s: order is %d then %d\n%s", step.name, step.first, step.second, got)
		}
	}
}

func TestRender_DenyAncestorsUseTheWalkedSpelling(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) (deny string, want, never []string)
	}{
		{"on-disk letter case", func(t *testing.T) (string, []string, []string) {
			root := realTempDir(t)
			mkdirs(t, filepath.Join(root, "Home", ".Config", "gh"))
			wrong := filepath.Join(root, "home", ".config", "gh")
			if _, err := os.Stat(wrong); err != nil {
				t.Skip("this volume is case-sensitive")
			}
			return wrong,
				literals(filepath.Join(root, "Home"), filepath.Join(root, "Home", ".Config")),
				literals(filepath.Join(root, "home"), filepath.Join(root, "home", ".config"))
		}},
		{"missing tail named as written", func(t *testing.T) (string, []string, []string) {
			root := realTempDir(t)
			mkdirs(t, filepath.Join(root, "Home"))
			return filepath.Join(root, "Home", "not", "yet", "gh"),
				literals(filepath.Join(root, "Home"), filepath.Join(root, "Home", "not"), filepath.Join(root, "Home", "not", "yet")),
				nil
		}},
		{"deny reached through /tmp", func(t *testing.T) (string, []string, []string) {
			return "/tmp", literals("/private"), literals("/")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deny, want, never := tc.setup(t)
			got, err := Render(Spec{Deny: []string{deny}})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			terms, _ := denyAncestorBlock(t, got)
			for _, w := range want {
				if !slices.Contains(terms, w) {
					t.Errorf("ancestor block lacks %s: %q", w, terms)
				}
			}
			for _, n := range never {
				if slices.Contains(terms, n) {
					t.Errorf("ancestor block names %s: %q", n, terms)
				}
			}
		})
	}
}

func TestRender_DenyAncestorsComeFromTheDenysOneWalk(t *testing.T) {
	root := realTempDir(t)
	config, decoy := filepath.Join(root, "home", ".config"), filepath.Join(root, "other", ".config")
	deny := filepath.Join(config, "gh")
	mkdirs(t, deny, filepath.Join(decoy, "gh"))
	fired := swapAfterWalk(t, deny, func() { replaceWithLink(t, config, decoy) })

	got, err := Render(Spec{Deny: []string{deny}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !*fired {
		t.Fatal("beforeSpell never saw the deny")
	}
	terms, _ := denyAncestorBlock(t, got)
	if want := literals(config)[0]; !slices.Contains(terms, want) {
		t.Errorf("ancestor block lacks the walked spelling %s: %q", want, terms)
	}
	if strings.Contains(got, filepath.Join(root, "other")) {
		t.Errorf("profile names the swapped-in link's target\n%s", got)
	}
}

func TestDenyAncestorTerms_RefusesAnAncestorItCannotSpell(t *testing.T) {
	terms, err := denyAncestorTerms([]string{"/private/tmp/ok/gh", "/private/tmp/bad\x01dir/gh"})
	if err == nil {
		t.Fatalf("denyAncestorTerms accepted an ancestor holding a control character: %q", terms)
	}
}

func linkedDenyText(prefix, deny, link string) string {
	return prefix + `deny "` + deny + `" follows symlink "` + link + `", which a sandboxed session could have made`
}

func TestRender_RefusesDenyThroughUserLink(t *testing.T) {
	skipAsRoot(t)
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, root string) (spec Spec, prefix, deny, link string)
	}{
		{"intermediate link", func(t *testing.T, root string) (Spec, string, string, string) {
			home, elsewhere := filepath.Join(root, "home"), filepath.Join(root, "elsewhere")
			mkdirs(t, home, filepath.Join(elsewhere, "gh"))
			symlink(t, elsewhere, filepath.Join(home, ".config"))
			deny := filepath.Join(home, ".config", "gh")
			return Spec{ReadWrite: []string{home}, Deny: []string{deny}}, "deny: ", deny, filepath.Join(home, ".config")
		}},
		{"final link", func(t *testing.T, root string) (Spec, string, string, string) {
			home, elsewhere := filepath.Join(root, "home"), filepath.Join(root, "elsewhere")
			mkdirs(t, filepath.Join(home, ".config"), elsewhere)
			deny := filepath.Join(home, ".config", "gh")
			symlink(t, elsewhere, deny)
			return Spec{ReadWrite: []string{home}, Deny: []string{deny}}, "deny: ", deny, deny
		}},
		{"dangling link", func(t *testing.T, root string) (Spec, string, string, string) {
			home := filepath.Join(root, "home")
			mkdirs(t, filepath.Join(home, ".config"))
			deny := filepath.Join(home, ".config", "gh")
			symlink(t, filepath.Join(root, "absent"), deny)
			return Spec{ReadWrite: []string{home}, Deny: []string{deny}}, "deny: ", deny, deny
		}},
		{"baseline carve-out that is a link", func(t *testing.T, root string) (Spec, string, string, string) {
			local, elsewhere := filepath.Join(root, "usr", "local"), filepath.Join(root, "elsewhere")
			mkdirs(t, local, elsewhere)
			etc := filepath.Join(local, "etc")
			symlink(t, elsewhere, etc)
			original := baselineDenyDirs
			t.Cleanup(func() { baselineDenyDirs = original })
			baselineDenyDirs = []string{etc}
			return Spec{}, "baseline deny: ", etc, etc
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := realTempDir(t)
			spec, prefix, deny, link := tc.setup(t, root)
			profiles := filepath.Join(root, "profiles")

			_, err := Write(profiles, "s1", spec)
			if err == nil {
				t.Fatalf("Write accepted a deny through %s", link)
			}
			if want := linkedDenyText(prefix, deny, link); err.Error() != want {
				t.Fatalf("error = %q\nwant    %q", err.Error(), want)
			}
			var linked *LinkedDenyError
			if !errors.As(err, &linked) {
				t.Fatalf("error %v is not a *LinkedDenyError", err)
			}
			if linked.Deny != deny || linked.Link != link {
				t.Errorf("LinkedDenyError = %+v, want Deny %s, Link %s", *linked, deny, link)
			}
			if entries, err := os.ReadDir(profiles); err == nil && len(entries) != 0 {
				t.Errorf("a refused Write left %d file(s) behind", len(entries))
			}
		})
	}
}
