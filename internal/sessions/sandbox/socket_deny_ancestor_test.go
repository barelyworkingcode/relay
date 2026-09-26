package sandbox

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const socketFileHead = "(deny file-write-unlink\n"

func socketFileTerm(dir string) string {
	return `(require-all (subpath "` + dir + `") (vnode-type SOCKET))`
}

func socketRegexTerm(dir string) string {
	return `(remote unix-socket (path-regex #"^` + regexp.QuoteMeta(dir) + `/"))`
}

// socketFileBlock is the terms of the profile's one socket-file block and the
// offset of its head, or nil and -1 when it has none.
func socketFileBlock(t *testing.T, profile string) (terms []string, at int) {
	t.Helper()
	switch n := strings.Count(profile, socketFileHead); n {
	case 0:
		return nil, -1
	case 1:
	default:
		t.Fatalf("profile has %d socket-file blocks, want at most one:\n%s", n, profile)
	}
	at = strings.Index(profile, socketFileHead)
	terms, _ = blockTerms(profile[at+len(socketFileHead):])
	return terms, at
}

func TestRender_EachSocketDenyDirGetsASocketFileRule(t *testing.T) {
	got, err := Render(Spec{UnixConnectDenyDirs: []string{"/tmp/relay-sandbox-golden/zeta", "/tmp/relay-sandbox-golden/alpha"}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	terms, at := socketFileBlock(t, got)
	want := []string{socketFileTerm("/private/tmp/relay-sandbox-golden/zeta"), socketFileTerm("/private/tmp/relay-sandbox-golden/alpha")}
	if !slices.Equal(terms, want) {
		t.Fatalf("socket-file block = %q\nwant              %q\n%s", terms, want, got)
	}
	_, ancestors := denyAncestorBlock(t, got)
	if outbound := strings.Index(got, "(deny network-outbound"); ancestors >= at || outbound < at {
		t.Errorf("socket-file block at %d is not between the ancestor block (%d) and the socket denies (%d):\n%s", at, ancestors, outbound, got)
	}

	bare, err := Render(Spec{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(bare, "vnode-type") {
		t.Errorf("Spec{} renders a socket-file rule:\n%s", bare)
	}
}

func TestRender_SocketDenyDirAndItsAncestorsJoinTheAncestorBlock(t *testing.T) {
	const g = "/private/tmp/relay-sandbox-golden"
	got, err := Render(Spec{ReadWrite: []string{g + "/home"}, UnixConnectDenyDirs: []string{g + "/home/lib/relay"}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	terms, at := denyAncestorBlock(t, got)
	want := literals("/private", "/private/tmp", g, g+"/home", g+"/home/lib", g+"/home/lib/relay", "/usr", "/usr/local")
	if !slices.Equal(terms, want) {
		t.Errorf("ancestor block = %q\nwant            %q\n%s", terms, want, got)
	}
	regex := strings.Index(got, `(remote unix-socket (path-regex #"^/private/tmp/relay-sandbox-golden/home/lib/relay/"))`)
	if grant := strings.Index(got, "(allow file-read* file-write*"); grant < 0 || grant > at || regex < at {
		t.Errorf("order is read-write grant %d, ancestor block %d, socket regex %d:\n%s", grant, at, regex, got)
	}
}

func TestRender_SocketDenyAncestorsAreNamedOnceInPathOrder(t *testing.T) {
	const g = "/private/tmp/relay-sandbox-golden"
	got, err := Render(Spec{
		Deny:                []string{g + "/home/.ssh"},
		UnixConnectDenyDirs: []string{g + "/home/sock x", g + "/home/sock", g + "/home/sock", g + "/home"},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	terms, _ := denyAncestorBlock(t, got)
	want := literals("/private", "/private/tmp", g, g+"/home", g+"/home/sock", g+"/home/sock x", "/usr", "/usr/local")
	if !slices.Equal(terms, want) {
		t.Errorf("ancestor block = %q\nwant            %q\n%s", terms, want, got)
	}
}

func TestRender_SocketDenyDirSpellingsComeFromOneWalk(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) (entry, resolved string, want, never []string)
	}{
		{"on-disk letter case", func(t *testing.T) (string, string, []string, []string) {
			root := realTempDir(t)
			real := filepath.Join(root, "Home", "Lib", "Relay")
			mkdirs(t, real)
			wrong := filepath.Join(root, "home", "lib", "relay")
			if _, err := os.Stat(wrong); err != nil {
				t.Skip("this volume is case-sensitive")
			}
			return wrong, real,
				literals(filepath.Join(root, "Home"), filepath.Join(root, "Home", "Lib"), real),
				literals(filepath.Join(root, "home"), filepath.Join(root, "home", "lib"), wrong)
		}},
		{"missing tail named as written", func(t *testing.T) (string, string, []string, []string) {
			root := realTempDir(t)
			mkdirs(t, filepath.Join(root, "Home"))
			entry := filepath.Join(root, "Home", "not", "yet", "relay")
			return entry, entry,
				literals(filepath.Join(root, "Home"), filepath.Join(root, "Home", "not"), filepath.Join(root, "Home", "not", "yet"), entry),
				nil
		}},
		{"reached through /tmp", func(t *testing.T) (string, string, []string, []string) {
			const g = "/private/tmp/relay-sandbox-golden"
			return "/tmp/relay-sandbox-golden/relay", g + "/relay",
				literals("/private", "/private/tmp", g, g+"/relay"),
				literals("/tmp", "/tmp/relay-sandbox-golden", "/tmp/relay-sandbox-golden/relay")
		}},
		{"final component is a user-writable link", func(t *testing.T) (string, string, []string, []string) {
			root := realTempDir(t)
			target, link := filepath.Join(root, "data", "relay"), filepath.Join(root, "home", "relay")
			mkdirs(t, target, filepath.Dir(link))
			symlink(t, target, link)
			return link, target,
				literals(root, filepath.Join(root, "data"), target, filepath.Join(root, "home"), link),
				nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry, resolved, want, never := tc.setup(t)
			got, err := Render(Spec{UnixConnectDenyDirs: []string{entry}})
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
			if re := socketRegexTerm(resolved); !strings.Contains(got, re) {
				t.Errorf("profile lacks %s\n%s", re, got)
			}
			if sock, _ := socketFileBlock(t, got); !slices.Equal(sock, []string{socketFileTerm(resolved)}) {
				t.Errorf("socket-file block = %q, want only %s", sock, socketFileTerm(resolved))
			}
		})
	}
}

func TestRender_SocketDenyDirSwappedAfterTheWalkKeepsTheWalkedSpelling(t *testing.T) {
	root := realTempDir(t)
	config, decoy := filepath.Join(root, "home", ".config"), filepath.Join(root, "other", ".config")
	dir := filepath.Join(config, "relay")
	mkdirs(t, dir, filepath.Join(decoy, "relay"))
	fired := swapAfterWalk(t, dir, func() { replaceWithLink(t, config, decoy) })

	got, err := Render(Spec{UnixConnectDenyDirs: []string{dir}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !*fired {
		t.Fatal("beforeSpell never saw the socket deny dir")
	}
	terms, _ := denyAncestorBlock(t, got)
	for _, w := range literals(config, dir) {
		if !slices.Contains(terms, w) {
			t.Errorf("ancestor block lacks the walked spelling %s: %q", w, terms)
		}
	}
	for _, w := range []string{socketRegexTerm(dir), socketFileTerm(dir)} {
		if !strings.Contains(got, w) {
			t.Errorf("profile lacks the walked spelling %s\n%s", w, got)
		}
	}
	if strings.Contains(got, filepath.Join(root, "other")) {
		t.Errorf("profile names the swapped-in link's target\n%s", got)
	}
}

func TestRender_SocketDenyDirItCannotSpellRefuses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry func(t *testing.T) string
	}{
		{"control character in the dir", func(t *testing.T) string {
			return "/private/tmp/relay-sandbox-golden/bad\x01dir/relay"
		}},
		{"control character in a final link's own name", func(t *testing.T) string {
			root := realTempDir(t)
			target, link := filepath.Join(root, "data", "relay"), filepath.Join(root, "home", "bad\x01link")
			mkdirs(t, target, filepath.Dir(link))
			symlink(t, target, link)
			return link
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Render(Spec{UnixConnectDenyDirs: []string{tc.entry(t)}})
			if err == nil {
				t.Fatalf("Render accepted a socket deny dir it cannot spell:\n%s", got)
			}
			if !strings.HasPrefix(err.Error(), "unix_connect_deny: ") {
				t.Errorf("error = %q, want the unix_connect_deny: prefix", err)
			}
		})
	}
}

func TestRender_OnlySocketDenyDirsJoinTheAncestorBlock(t *testing.T) {
	const g = "/private/tmp/relay-sandbox-golden"
	for name, spec := range map[string]Spec{
		"empty spec": {},
		"socket deny paths and allows": {
			UnixConnectDenyPaths: []string{g + "/relayllm/router.sock"},
			UnixConnectAllow:     []string{g + "/relay/relay.sock"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Render(spec)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if terms, _ := denyAncestorBlock(t, got); !slices.Equal(terms, literals("/usr", "/usr/local")) {
				t.Errorf("ancestor block = %q, want only /usr and /usr/local", terms)
			}
			if sock, _ := socketFileBlock(t, got); sock != nil {
				t.Errorf("socket-file block = %q, want none", sock)
			}
		})
	}
}
