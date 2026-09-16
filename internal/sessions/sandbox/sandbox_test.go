package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goldenSpec is C7's full shape with every path chosen so that it does not
// exist on any machine: resolve then leaves it exactly as written, which is
// what makes the golden file below stable outside this developer's disk.
func goldenSpec() Spec {
	return Spec{
		WriteAllowDirs: []string{
			"/private/tmp/relay-sandbox-golden/projects/widget",
			"/private/tmp/relay-sandbox-golden/home/.cache",
			"/private/tmp/relay-sandbox-golden/home/go/pkg",
			"/private/tmp/cc-socks",
			"/dev",
		},
		WriteAllowFiles: []string{"/private/tmp/relay-sandbox-golden/home/.claude.json"},
		ReadDeny: []string{
			"/private/tmp/relay-sandbox-golden/relay",
			"/private/tmp/relay-sandbox-golden/projects/other",
		},
		UnixConnectDenyDirs:  []string{"/private/tmp/relay-sandbox-golden/relay"},
		UnixConnectDenyPaths: []string{"/private/tmp/relay-sandbox-golden/relayllm/router.sock"},
		UnixConnectAllow: []string{
			"/private/tmp/relay-sandbox-golden/relay/relay.sock",
			"/private/tmp/relay-sandbox-golden/relay/model.sock",
		},
		TCPLoopbackDeny:  []int{3000, 8181},
		TCPLoopbackAllow: []int{8180},
		DenySetIDExec:    true,
	}
}

func TestRender_Golden(t *testing.T) {
	got, err := Render(goldenSpec())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "golden_profile.sb"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if got != string(want) {
		t.Fatalf("profile drifted from the golden file.\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestRender_AllowsWinOverDenies pins SBPL's own rule-ordering semantics, the
// one property C7's socket rules depend on: the last matching rule decides,
// so an allowed socket inside a denied directory is reachable only because
// its allow is emitted after the deny (SP2 row 6).
func TestRender_AllowsWinOverDenies(t *testing.T) {
	got, err := Render(goldenSpec())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	denyWrite := strings.Index(got, "(deny file-write*)")
	allowWrite := strings.Index(got, "(allow file-write*")
	readDeny := strings.Index(got, "(deny file-read* file-write*")
	unixDeny := strings.Index(got, `(path-regex #"^/private/tmp/relay-sandbox-golden/relay/")`)
	unixAllow := strings.Index(got, `(path-literal "/private/tmp/relay-sandbox-golden/relay/relay.sock")`)
	tcpDeny := strings.Index(got, `(remote ip "localhost:3000")`)
	tcpAllow := strings.Index(got, `(remote ip "localhost:8180")`)
	for _, step := range []struct {
		name          string
		first, second int
	}{
		{"file-write deny before allow", denyWrite, allowWrite},
		{"write allow before read deny", allowWrite, readDeny},
		{"unix deny before unix allow", unixDeny, unixAllow},
		{"tcp deny before tcp allow", tcpDeny, tcpAllow},
	} {
		if step.first < 0 || step.second < 0 || step.first >= step.second {
			t.Errorf("%s: order is %d then %d", step.name, step.first, step.second)
		}
	}
}

func TestRender_EscapesQuotesBackslashesSpacesAndUnicode(t *testing.T) {
	got, err := Render(Spec{WriteAllowDirs: []string{
		`/private/tmp/relay-sandbox-golden/odd "quoted" dir`,
		`/private/tmp/relay-sandbox-golden/back\slash`,
		"/private/tmp/relay-sandbox-golden/unicode-café-δοκιμή",
	}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{
		`(subpath "/private/tmp/relay-sandbox-golden/odd \"quoted\" dir")`,
		`(subpath "/private/tmp/relay-sandbox-golden/back\\slash")`,
		`(subpath "/private/tmp/relay-sandbox-golden/unicode-café-δοκιμή")`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("profile does not contain %s\ngot:\n%s", want, got)
		}
	}
}

// TestRender_FileEntryCoversAtomicSiblings pins SP2 change 2: without the
// siblings, Claude Code's writes to ~/.claude.json are denied and its state
// is silently lost. The regex metacharacters in the path itself are escaped,
// so ".claude.json" cannot match ".claudeXjson".
func TestRender_FileEntryCoversAtomicSiblings(t *testing.T) {
	got, err := Render(Spec{WriteAllowFiles: []string{"/private/tmp/relay-sandbox-golden/home/.claude.json"}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := `(regex #"^/private/tmp/relay-sandbox-golden/home/\.claude\.json(\.lock|\.tmp\.[^/]*|\.backup)?$")`
	if !strings.Contains(got, want) {
		t.Fatalf("profile does not contain %s\ngot:\n%s", want, got)
	}
}

// TestRender_NeverReportsOnADeny pins SP2's finding that `(with report)` is a
// syntax error on a deny rule — a profile carrying one does not load at all.
func TestRender_NeverReportsOnADeny(t *testing.T) {
	got, err := Render(goldenSpec())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(got, "with report") {
		t.Fatalf("profile carries a (with report):\n%s", got)
	}
}

func TestRender_RefusesPathsItCannotSpell(t *testing.T) {
	cases := []struct {
		name string
		spec Spec
	}{
		{"quote in a file entry", Spec{WriteAllowFiles: []string{`/private/tmp/odd "q" dir/.claude.json`}}},
		{"quote in a socket directory", Spec{UnixConnectDenyDirs: []string{`/private/tmp/odd "q" dir`}}},
		{"newline in a directory", Spec{WriteAllowDirs: []string{"/private/tmp/line\nbreak"}}},
		{"newline in a read deny", Spec{ReadDeny: []string{"/private/tmp/line\nbreak"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Render(tc.spec); err == nil {
				t.Fatal("Render accepted a path it cannot express")
			}
		})
	}
}

func TestWrite_ProfileFileShape(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profiles")
	path, err := Write(dir, "11111111-1111-1111-1111-111111111111", goldenSpec())
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if want := filepath.Join(dir, "11111111-1111-1111-1111-111111111111.sb"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.HasPrefix(string(body), "(version 1)\n") {
		t.Fatalf("profile does not start with (version 1):\n%s", body)
	}
}

// TestWrite_RefusesASessionIDThatNamesAnotherFile is the fail-closed half of
// "<profiles dir>/<session id>.sb": the id is a string, and a caller that
// ever passes one through from a request must not be able to steer the write.
func TestWrite_RefusesASessionIDThatNamesAnotherFile(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"", "..", "../escape", "a/b", ".hidden", "with space"} {
		if _, err := Write(dir, id, Spec{}); err == nil {
			t.Errorf("Write accepted session id %q", id)
		}
	}
}

func TestWrite_FailsClosedWithoutSandboxExec(t *testing.T) {
	original := sandboxExecPath
	sandboxExecPath = filepath.Join(t.TempDir(), "no-such-sandbox-exec")
	t.Cleanup(func() { sandboxExecPath = original })

	dir := t.TempDir()
	if _, err := Write(dir, "session-1", goldenSpec()); err == nil {
		t.Fatal("Write returned a profile path with sandbox-exec missing")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("a refused Write left %d file(s) behind", len(entries))
	}
}

func TestRemove_IsIdempotent(t *testing.T) {
	path, err := Write(t.TempDir(), "session-1", Spec{})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := Remove(path); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
	if err := Remove(""); err != nil {
		t.Fatalf("Remove(\"\"): %v", err)
	}
}

// TestResolve pins SP2's realpath requirement in both directions: an
// existing path that is reached through a symlink resolves, and one that
// does not exist yet resolves as far as its deepest existing ancestor.
func TestResolve(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	if got, want := resolve(link), filepath.Join(resolvedRoot, "real"); got != want {
		t.Errorf("resolve(symlink) = %q, want %q", got, want)
	}
	if got, want := resolve(filepath.Join(link, "not", "created", "yet")), filepath.Join(resolvedRoot, "real", "not", "created", "yet"); got != want {
		t.Errorf("resolve(missing under a symlink) = %q, want %q", got, want)
	}
	if got := resolve("/private/tmp/relay-sandbox-golden/nothing/here"); got != "/private/tmp/relay-sandbox-golden/nothing/here" {
		t.Errorf("resolve(missing) = %q, want it unchanged", got)
	}
}
