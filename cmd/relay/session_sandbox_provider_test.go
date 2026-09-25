package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
)

// buildTree creates entries under root. A key ending in "/" is a directory, a
// value starting with "->" is a symlink to the rest, anything else is a 0755
// file. "$ROOT" in a value is replaced with root.
func buildTree(t *testing.T, root string, entries map[string]string) {
	t.Helper()
	for rel, content := range entries {
		p := filepath.Join(root, rel)
		content = strings.ReplaceAll(content, "$ROOT", root)
		var err error
		switch target, isLink := strings.CutPrefix(content, "->"); {
		case strings.HasSuffix(rel, "/"):
			err = os.MkdirAll(p, 0o755)
		case isLink:
			if err = os.MkdirAll(filepath.Dir(p), 0o755); err == nil {
				err = os.Symlink(target, p)
			}
		default:
			if err = os.MkdirAll(filepath.Dir(p), 0o755); err == nil {
				err = os.WriteFile(p, []byte(content), 0o755)
			}
		}
		if err != nil {
			t.Fatalf("build fixture %s: %v", rel, err)
		}
	}
}

// realTempDir is t.TempDir() in the kernel's spelling, so fixture paths and
// the paths relay derives from them agree on /var versus /private/var.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return dir
}

func setSeam[T any](t *testing.T, seam *T, value T) {
	t.Helper()
	original := *seam
	t.Cleanup(func() { *seam = original })
	*seam = value
}

func claudeTempDirUnder(root string) string {
	return filepath.Join(root, "claude-"+strconv.Itoa(os.Getuid()))
}

const machO = "\xcf\xfa\xed\xfe native"

func TestProviderInstallGrants(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tree      map[string]string
		binary    string            // joined to the fixture root when it contains a slash
		lookPath  map[string]string // command name -> path under the fixture root
		read      []string          // exactly these subtrees
		files     []string          // ReadFiles must hold these
		mayFiles  []string          // ReadFiles may also hold these, and nothing else
		lookups   []string          // exactly these names reach lookPath
		wantErr   bool
		ignoreErr bool
	}{
		{name: "no binary"},
		{name: "relative binary", binary: "claude", wantErr: true},
		{
			name: "homebrew cask",
			tree: map[string]string{
				"brew/bin/claude":                      "->../Caskroom/claude-code/1.0/claude",
				"brew/Caskroom/claude-code/1.0/claude": machO,
				"brew/Cellar/":                         "",
				"brew/opt":                             "->../elsewhere",
				"elsewhere/":                           "",
				"brew/etc/openssl@3/openssl.cnf":       "cnf",
				"brew/etc/openssl@1.1/openssl.cnf/":    "",
				"brew/etc/profile.d/tokens.sh":         "x",
				"brew/var/db/data":                     "x",
			},
			binary:   "brew/bin/claude",
			read:     []string{"brew/Cellar", "brew/Caskroom"},
			files:    []string{"brew/bin/claude", "brew/etc/openssl@3/openssl.cnf"},
			mayFiles: []string{"brew/Caskroom/claude-code/1.0/claude"},
		},
		{
			name: "homebrew formula through env node",
			tree: map[string]string{
				"brew/bin/pi":                  "->../Cellar/pi/1.0/bin/pi",
				"brew/Cellar/pi/1.0/bin/pi":    "#!/usr/bin/env -S node --no-warnings\n",
				"brew/opt/node":                "->../Cellar/node/22",
				"brew/Cellar/node/22/bin/node": machO,
			},
			binary:   "brew/bin/pi",
			lookPath: map[string]string{"node": "brew/opt/node/bin/node"},
			read:     []string{"brew/Cellar", "brew/opt"},
			files:    []string{"brew/bin/pi", "brew/opt/node/bin/node"},
			mayFiles: []string{"brew/Cellar/pi/1.0/bin/pi", "brew/Cellar/node/22/bin/node"},
			lookups:  []string{"node"},
		},
		{
			name: "npm global install takes the outermost node_modules",
			tree: map[string]string{
				"npm/bin/claude": "->../lib/node_modules/@acme/cli/node_modules/core/cli.js",
				"npm/lib/node_modules/@acme/cli/node_modules/core/cli.js": "#!/usr/bin/env node\n",
				"node/bin/node": machO,
			},
			binary:   "npm/bin/claude",
			lookPath: map[string]string{"node": "node/bin/node"},
			read:     []string{"npm/lib/node_modules"},
			files:    []string{"npm/bin/claude", "node/bin/node"},
			mayFiles: []string{"npm/lib/node_modules/@acme/cli/node_modules/core/cli.js"},
			lookups:  []string{"node"},
		},
		{
			name: "link chain to an absolute interpreter with a path-like env name",
			tree: map[string]string{
				"tools/claude":    "->hop1",
				"tools/hop1":      "->run.sh",
				"tools/run.sh":    "#!$ROOT/interp/bin/tool -x\n",
				"interp/bin/tool": "#!/usr/bin/env ../evil\n",
			},
			binary:   "tools/claude",
			files:    []string{"tools/claude", "tools/hop1", "interp/bin/tool"},
			mayFiles: []string{"tools/run.sh"},
		},
		{
			name:   "system interpreter needs nothing",
			tree:   map[string]string{"tools/claude": "#!/bin/sh\n"},
			binary: "tools/claude",
			files:  []string{"tools/claude"},
		},
		{
			name: "at most four files",
			tree: map[string]string{
				"chain/a": "#!$ROOT/chain/b\n", "chain/b": "#!$ROOT/chain/c\n", "chain/c": "#!$ROOT/chain/d\n",
				"chain/d": "#!$ROOT/chain/e\n", "chain/e": "#!$ROOT/chain/f\n", "chain/f": machO,
			},
			binary:    "chain/a",
			files:     []string{"chain/a", "chain/b", "chain/c", "chain/d"},
			ignoreErr: true,
		},
		{
			name:     "target is not a regular file",
			tree:     map[string]string{"tools/claude": "->dir", "tools/dir/": ""},
			binary:   "tools/claude",
			mayFiles: []string{"tools/claude"},
			wantErr:  true,
		},
		{
			name: "more than eight symlink hops",
			tree: map[string]string{
				"l/0": "->1", "l/1": "->2", "l/2": "->3", "l/3": "->4", "l/4": "->5",
				"l/5": "->6", "l/6": "->7", "l/7": "->8", "l/8": "->9", "l/9": machO,
			},
			binary:   "l/0",
			mayFiles: []string{"l/0", "l/1", "l/2", "l/3", "l/4", "l/5", "l/6", "l/7", "l/8", "l/9"},
			wantErr:  true,
		},
		{
			name: "relative link inside a symlinked parent resolves against the real parent",
			tree: map[string]string{
				"alias":           "->real/bin",
				"real/bin/claude": "->../lib/cli",
				"real/lib/cli":    machO,
				"lib/cli":         machO,
			},
			binary:   "alias/claude",
			files:    []string{"alias/claude", "real/lib/cli"},
			mayFiles: []string{"real/bin/claude"},
		},
		{
			name: "a Cellar outside homebrewPrefixes gets no prefix grant",
			tree: map[string]string{
				"other/bin/claude":               "->../Cellar/claude/1.0/claude",
				"other/Cellar/claude/1.0/claude": machO,
			},
			binary:   "other/bin/claude",
			files:    []string{"other/bin/claude"},
			mayFiles: []string{"other/Cellar/claude/1.0/claude"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := realTempDir(t)
			buildTree(t, root, tc.tree)
			setSeam(t, &homebrewPrefixes, []string{filepath.Join(root, "brew")})
			under := func(rels []string) []string {
				out := make([]string, 0, len(rels))
				for _, r := range rels {
					out = append(out, filepath.Join(root, r))
				}
				return out
			}
			binary := tc.binary
			if strings.Contains(binary, "/") {
				binary = filepath.Join(root, binary)
			}
			var lookups []string
			lookPath := func(name string) (string, error) {
				lookups = append(lookups, name)
				if rel, ok := tc.lookPath[name]; ok {
					return filepath.Join(root, rel), nil
				}
				return "", exec.ErrNotFound
			}

			got, err := providerInstallGrants(binary, lookPath)

			if !tc.ignoreErr && (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			wantRead := under(tc.read)
			gotRead := slices.Clone(got.Read)
			slices.Sort(wantRead)
			slices.Sort(gotRead)
			if !slices.Equal(gotRead, wantRead) || len(slices.Compact(slices.Clone(gotRead))) != len(gotRead) {
				t.Errorf("Read = %v, want exactly %v", got.Read, wantRead)
			}
			allowed := append(under(tc.files), under(tc.mayFiles)...)
			for _, f := range under(tc.files) {
				if !slices.Contains(got.ReadFiles, f) {
					t.Errorf("ReadFiles lacks %s: %v", f, got.ReadFiles)
				}
			}
			for _, f := range got.ReadFiles {
				if !slices.Contains(allowed, f) {
					t.Errorf("ReadFiles holds %s, outside %v", f, allowed)
				}
			}
			if !slices.Equal(lookups, tc.lookups) {
				t.Errorf("lookPath calls = %v, want %v", lookups, tc.lookups)
			}
		})
	}
}

func TestClaudeTempGrant(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, root, dir string) error
		reason string
	}{
		{"absent is created", func(*testing.T, string, string) error { return nil }, ""},
		{"existing 0700", func(_ *testing.T, _, dir string) error { return os.Mkdir(dir, 0o700) }, ""},
		{"symlink", func(_ *testing.T, root, dir string) error {
			owned := filepath.Join(root, "owned")
			if err := os.Mkdir(owned, 0o700); err != nil {
				return err
			}
			return os.Symlink(owned, dir)
		}, "symlink"},
		{"0777", func(_ *testing.T, _, dir string) error {
			if err := os.Mkdir(dir, 0o700); err != nil {
				return err
			}
			return os.Chmod(dir, 0o777)
		}, "group_or_other_writable"},
		{"regular file", func(_ *testing.T, _, dir string) error { return os.WriteFile(dir, nil, 0o600) }, "not_dir"},
		{"root missing", func(t *testing.T, root, _ string) error {
			setSeam(t, &claudeTempRoot, filepath.Join(root, "missing"))
			return nil
		}, "mkdir_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := realTempDir(t)
			setSeam(t, &claudeTempRoot, root)
			if err := tc.setup(t, root, claudeTempDirUnder(root)); err != nil {
				t.Fatalf("setup: %v", err)
			}

			dir, reason := claudeTempGrant()

			if reason != tc.reason {
				t.Fatalf("reason = %q, want %q (dir %q)", reason, tc.reason, dir)
			}
			if tc.reason != "" {
				if dir != "" {
					t.Fatalf("dir = %q alongside reason %q, want empty", dir, reason)
				}
				return
			}
			if want := claudeTempDirUnder(root); dir != want {
				t.Fatalf("dir = %q, want %q", dir, want)
			}
			info, err := os.Lstat(dir)
			if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
				t.Fatalf("granted dir is not a 0700 directory: %v, %v", info, err)
			}
		})
	}
}

// unwritableFixtureRoot is a fixture directory outside every write grant a
// session gets by default: t.TempDir() sits under $TMPDIR, which every session
// may write.
func unwritableFixtureRoot(t *testing.T) string {
	t.Helper()
	tmp, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatalf("resolve /tmp: %v", err)
	}
	root, err := os.MkdirTemp(tmp, "relay-install-")
	if err != nil {
		t.Fatalf("create fixture root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

// npmInstallFixture is a provider installed outside every template path: a
// link in bin/ to a script inside lib/node_modules.
func npmInstallFixture(t *testing.T) (link, target, nodeModules string) {
	t.Helper()
	root := unwritableFixtureRoot(t)
	buildTree(t, root, map[string]string{
		"bin/claude":                        "->../lib/node_modules/@acme/cli/cli.sh",
		"lib/node_modules/@acme/cli/cli.sh": "#!/bin/sh\necho ok\n",
	})
	return filepath.Join(root, "bin", "claude"),
		filepath.Join(root, "lib", "node_modules", "@acme", "cli", "cli.sh"),
		filepath.Join(root, "lib", "node_modules")
}

// setKindTemplates replaces the seeded claude-code and pi templates.
func setKindTemplates(t *testing.T, store config.SettingsStore, replacements ...config.TerminalTemplate) {
	t.Helper()
	if err := store.With(func(s *config.Settings) {
		s.TerminalTemplates = slices.DeleteFunc(s.TerminalTemplates, func(tt config.TerminalTemplate) bool {
			return tt.ID == "claude-code" || tt.ID == "pi"
		})
		s.TerminalTemplates = append(s.TerminalTemplates, replacements...)
	}); err != nil {
		t.Fatalf("store.With: %v", err)
	}
}

func TestAuthorizeLaunch_ProviderOutsideEveryTemplateIsGranted(t *testing.T) {
	noDeveloperTools(t)
	link, target, nodeModules := npmInstallFixture(t)
	// Every kind gets the fixture, so the claude/pi gate is sandboxSpecForLaunch's.
	setSeam(t, &providerBinary, func(string) string { return link })
	tempRoot := realTempDir(t)
	setSeam(t, &claudeTempRoot, tempRoot)
	store := newLaunchTestStore(t)
	setKindTemplates(t, store)
	proj := addLaunchTestProject(t, store, nil)

	installLines := []string{`(literal "` + link + `")`, `(literal "` + target + `")`, `(subpath "` + nodeModules + `")`}
	tempLine := `(subpath "` + claudeTempDirUnder(tempRoot) + `")`
	for _, tc := range []struct {
		kind, template       string
		wantInstall, wantTmp bool
	}{
		{KindClaude, "", true, true},
		{KindPi, "", true, false},
		{KindChat, "", false, false},
		{KindPTY, "shell", false, false},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			_, body := launchWithSandbox(t, LaunchRequest{
				Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: tc.kind, TemplateID: tc.template,
			}, store)
			body = dropMetadataBlock(body)
			writeAt := strings.Index(body, "(allow file-read* file-write*")
			readPart, writePart := body[:writeAt], body[writeAt:]
			for _, line := range installLines {
				if got := strings.Contains(readPart, line); got != tc.wantInstall {
					t.Errorf("read block holds %s = %v, want %v\n%s", line, got, tc.wantInstall, body)
				}
			}
			if got := strings.Contains(writePart, tempLine); got != tc.wantTmp {
				t.Errorf("write block holds %s = %v, want %v\n%s", tempLine, got, tc.wantTmp, body)
			}
		})
	}
}

func TestAuthorizeLaunch_TemplateDenyBeatsInstallGrant(t *testing.T) {
	noDeveloperTools(t)
	link, _, nodeModules := npmInstallFixture(t)
	setSeam(t, &providerBinary, func(string) string { return link })
	setSeam(t, &claudeTempRoot, realTempDir(t))
	store := newLaunchTestStore(t)
	setKindTemplates(t, store, config.TerminalTemplate{
		ID: "claude-code", Name: "Claude Code", Command: "claude", Sandbox: true, Deny: []string{nodeModules},
	})
	proj := addLaunchTestProject(t, store, nil)

	_, body := launchWithSandbox(t, LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindClaude,
	}, store)

	grant := `(subpath "` + nodeModules + `")`
	grantAt := strings.Index(body, grant)
	denyAt := strings.Index(body, "(deny file-read* file-write*\n")
	if grantAt < 0 || denyAt < grantAt || !strings.Contains(body[denyAt:], grant) {
		t.Fatalf("want the install grant %s, then a deny block naming it\n%s", grant, body)
	}
}

func TestAuthorizeLaunch_UnresolvedProviderLaunchesWithoutInstallGrant(t *testing.T) {
	noDeveloperTools(t)
	setSeam(t, &claudeTempRoot, realTempDir(t))
	store := newLaunchTestStore(t)
	setKindTemplates(t, store)
	proj := addLaunchTestProject(t, store, nil)
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindClaude}

	profileFor := func(binary string) string {
		setSeam(t, &providerBinary, func(string) string { return binary })
		_, body := launchWithSandbox(t, req, store)
		return body
	}
	if bare, none := profileFor("claude"), profileFor(""); bare != none {
		t.Fatalf("a bare binary name changed the profile.\nbare:\n%s\nnone:\n%s", bare, none)
	}
}

func TestAuthorizeLaunch_InstallGrantRefusedWhenChainIsSessionWritable(t *testing.T) {
	noDeveloperTools(t)
	root := unwritableFixtureRoot(t)
	buildTree(t, root, map[string]string{
		"bin/claude":  "->../lib/cli.sh",
		"lib/cli.sh":  "#!$ROOT/interp/tool\n",
		"interp/tool": machO,
		"other/":      "",
	})
	setSeam(t, &providerBinary, func(string) string { return filepath.Join(root, "bin", "claude") })
	setSeam(t, &claudeTempRoot, realTempDir(t))
	installLines := []string{
		`(literal "` + filepath.Join(root, "bin", "claude") + `")`,
		`(literal "` + filepath.Join(root, "lib", "cli.sh") + `")`,
		`(literal "` + filepath.Join(root, "interp", "tool") + `")`,
	}
	for _, tc := range []struct {
		name, writable string
		wantInstall    bool
	}{
		{"binary link inside a read_write grant", "bin", false},
		{"link target inside a read_write grant", "lib", false},
		{"interpreter inside a read_write grant", "interp", false},
		{"no writable overlap", "other", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newLaunchTestStore(t)
			setKindTemplates(t, store, config.TerminalTemplate{
				ID: "claude-code", Name: "Claude Code", Command: "claude", Sandbox: true,
				ReadWrite: []string{filepath.Join(root, tc.writable)},
			})
			proj := addLaunchTestProject(t, store, nil)

			_, body := launchWithSandbox(t, LaunchRequest{
				Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindClaude,
			}, store)

			body = dropMetadataBlock(body)
			readPart := body[:strings.Index(body, "(allow file-read* file-write*")]
			for _, line := range installLines {
				if got := strings.Contains(readPart, line); got != tc.wantInstall {
					t.Errorf("read block holds %s = %v, want %v\n%s", line, got, tc.wantInstall, body)
				}
			}
		})
	}
}

func TestAuthorizeLaunch_ClaudeTempDirSwappedForSymlinkIsRefused(t *testing.T) {
	tempRoot := realTempDir(t)
	setSeam(t, &claudeTempRoot, tempRoot)
	elsewhere := filepath.Join(tempRoot, "elsewhere")
	if err := os.Mkdir(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	// developerTools runs after the temp dir grant is checked and before the
	// profile is written, so the swap lands inside that window.
	setSeam(t, &developerTools, func() string {
		dir := claudeTempDirUnder(tempRoot)
		if err := os.Remove(dir); err != nil {
			t.Errorf("remove temp dir: %v", err)
		}
		if err := os.Symlink(elsewhere, dir); err != nil {
			t.Errorf("swap in symlink: %v", err)
		}
		return ""
	})
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)

	_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), newLaunchTestLedger(t), LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindClaude,
	})

	if refusal == nil {
		t.Fatal("launch authorized after the claude temp dir was swapped for a symlink")
	}
}
