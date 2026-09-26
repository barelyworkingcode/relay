package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// goldenSpec is C7's full shape with every path chosen so that it does not
// exist on any machine: resolve then leaves it exactly as written, which is
// what makes the golden file below stable outside this developer's disk.
func goldenSpec() Spec {
	return Spec{
		ReadWrite: []string{
			"/private/tmp/relay-sandbox-golden/projects/widget",
			"/private/tmp/relay-sandbox-golden/home/.cache",
			"/private/tmp/relay-sandbox-golden/home/go/pkg",
			"/private/tmp/cc-socks",
			"/dev",
		},
		ReadWriteFiles:       []string{"/private/tmp/relay-sandbox-golden/home/.claude.json"},
		Read:                 []string{"/private/tmp/relay-sandbox-golden/opt/homebrew"},
		ReadFiles:            []string{"/private/tmp/relay-sandbox-golden/home/.gitconfig"},
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

// TestRender_GrantsComeAfterTheDeny pins SBPL's own rule-ordering semantics,
// the one property every grant depends on: the last matching rule decides, so
// a grant is reachable only because it is emitted after the deny. A
// presence-only check would still pass on a profile that emitted a grant
// BEFORE the deny, which the deny would then override -- the rule sitting
// right there in the text, and the path denied anyway. The socket and
// loopback rules rely on the same order (SP2 row 6).
func TestRender_GrantsComeAfterTheDeny(t *testing.T) {
	got, err := Render(goldenSpec())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	deny := strings.Index(got, "(deny file-read* file-write*)")
	readGrant := strings.Index(got, "(allow file-read*\n")
	writeGrant := strings.Index(got, "(allow file-read* file-write*")
	metadata := strings.Index(got, "(allow file-read-metadata")
	unixDeny := strings.Index(got, `(path-regex #"^/private/tmp/relay-sandbox-golden/relay/")`)
	unixAllow := strings.Index(got, `(path-literal "/private/tmp/relay-sandbox-golden/relay/relay.sock")`)
	tcpDeny := strings.Index(got, `(remote ip "localhost:3000")`)
	tcpAllow := strings.Index(got, `(remote ip "localhost:8180")`)
	for _, step := range []struct {
		name          string
		first, second int
	}{
		{"file deny before read grants", deny, readGrant},
		{"read grants before read-write grants", readGrant, writeGrant},
		{"read-write grants before ancestor metadata", writeGrant, metadata},
		{"unix deny before unix allow", unixDeny, unixAllow},
		{"tcp deny before tcp allow", tcpDeny, tcpAllow},
	} {
		if step.first < 0 || step.second < 0 || step.first >= step.second {
			t.Errorf("%s: order is %d then %d", step.name, step.first, step.second)
		}
	}
}

// TestRender_NothingIsDeniedByName is the point of the model: without
// Spec.Deny the only paths a file deny names are the fixed baseline carve-outs
// under /usr, and the only unlink deny names their ancestors and the socket
// deny dir with its ancestors, plus that dir's socket files. Everything else
// is unreachable because no grant names it, not because a deny list
// remembered it.
func TestRender_NothingIsDeniedByName(t *testing.T) {
	got, err := Render(goldenSpec())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if n := strings.Count(got, "(deny file-read*"); n != 2 {
		t.Fatalf("profile has %d file-read deny blocks, want the bare deny and the baseline block:\n%s", n, got)
	}
	allowed := map[string]bool{usrLocalEtcTerm: true, usrLocalVarTerm: true}
	for _, term := range namedFileDenyTerms(got) {
		if !allowed[term] {
			t.Errorf("profile denies %s by name without a Spec.Deny:\n%s", term, got)
		}
	}
	if strings.Contains(got, "(deny file-write*") {
		t.Fatalf("profile carries a separate write deny:\n%s", got)
	}
	const unlinkDenies = "(deny file-write-unlink file-clone\n" +
		"  (literal \"/private\")\n" +
		"  (literal \"/private/tmp\")\n" +
		"  (literal \"/private/tmp/relay-sandbox-golden\")\n" +
		"  (literal \"/private/tmp/relay-sandbox-golden/relay\")\n" +
		"  (literal \"/usr\")\n" +
		"  (literal \"/usr/local\"))\n" +
		"(deny file-write-unlink\n" +
		"  (require-all (subpath \"/private/tmp/relay-sandbox-golden/relay\") (vnode-type SOCKET)))\n"
	if n := strings.Count(got, "(deny file-write-unlink"); n != 2 || !strings.Contains(got, unlinkDenies) {
		t.Errorf("profile's unlink denies are not exactly the ancestor block and the socket-file block:\n%s", got)
	}
}

// namedFileDenyTerms is every term under a path-naming file deny block.
func namedFileDenyTerms(profile string) []string {
	const head = "(deny file-read* file-write*\n"
	var terms []string
	for rest := profile; ; {
		at := strings.Index(rest, head)
		if at < 0 {
			return terms
		}
		var block []string
		block, rest = blockTerms(rest[at+len(head):])
		terms = append(terms, block...)
	}
}

// blockTerms reads the indented terms at the start of body, the text after a
// block's head line, and returns them with the text that follows the block.
func blockTerms(body string) (terms []string, rest string) {
	rest = body
	for strings.HasPrefix(rest, "  ") {
		line, after, _ := strings.Cut(rest, "\n")
		term := strings.TrimSpace(line)
		if !strings.HasPrefix(after, "  ") {
			term = strings.TrimSuffix(term, ")")
		}
		terms = append(terms, term)
		rest = after
	}
	return terms, rest
}

const (
	usrLocalEtcTerm = `(subpath "/usr/local/etc")`
	usrLocalVarTerm = `(subpath "/usr/local/var")`
)

// TestRender_BaselineDeniesUsrLocalConfigAndData pins the carve-out from the
// /usr read baseline: Intel Homebrew keeps service config and data under
// /usr/local/etc and /usr/local/var. The deny must follow the /usr allow or
// it covers nothing, and must precede Spec grants so an explicit grant under
// either subtree reopens it.
func TestRender_BaselineDeniesUsrLocalConfigAndData(t *testing.T) {
	got, err := Render(Spec{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	usr := strings.Index(got, `(subpath "/usr")`)
	if usr < 0 {
		t.Fatalf("baseline lacks /usr:\n%s", got)
	}
	head := strings.Index(got, "(deny file-read* file-write*\n")
	for _, term := range []string{usrLocalEtcTerm, usrLocalVarTerm} {
		at := strings.Index(got, term)
		if at < 0 || head < 0 || head > at {
			t.Errorf("no baseline read-and-write deny for %s:\n%s", term, got)
			continue
		}
		if at < usr {
			t.Errorf("baseline deny %s precedes the /usr allow (%d < %d), so /usr reopens it:\n%s", term, at, usr, got)
		}
	}
}

// TestRender_BaselineReopensOnlyTheHomebrewCABundleAndGitconfig pins the files
// every session may still read under the /usr/local deny, without which Intel
// Homebrew's curl, python and git lose TLS verification and their system
// gitconfig. The CA bundle is named at both its link and its target, as fixed
// literals: nothing else beneath the deny reopens.
func TestRender_BaselineReopensOnlyTheHomebrewCABundleAndGitconfig(t *testing.T) {
	const specGrant = `(literal "/private/tmp/relay-sandbox-golden/home/.gitconfig")`
	got, err := Render(Spec{ReadFiles: []string{"/private/tmp/relay-sandbox-golden/home/.gitconfig"}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	const denyHead, reopenHead = "(deny file-read* file-write*\n", "(allow file-read*\n"
	at := strings.Index(got, denyHead)
	if at < 0 {
		t.Fatalf("no baseline deny block:\n%s", got)
	}
	denied, after := blockTerms(got[at+len(denyHead):])
	if !slices.Contains(denied, usrLocalEtcTerm) || !slices.Contains(denied, usrLocalVarTerm) {
		t.Fatalf("first path deny block is not the baseline deny: %q\n%s", denied, got)
	}
	if !strings.HasPrefix(after, reopenHead) {
		t.Fatalf("no read-only allow block right after the baseline deny:\n%s", got)
	}
	reopened, rest := blockTerms(after[len(reopenHead):])
	want := []string{
		`(literal "/usr/local/etc/ca-certificates/cert.pem")`,
		`(literal "/usr/local/etc/gitconfig")`,
		`(literal "/usr/local/etc/openssl@3/cert.pem")`,
	}
	slices.Sort(reopened)
	if !slices.Equal(reopened, want) {
		t.Errorf("reopen block = %q, want exactly %q\n%s", reopened, want, got)
	}
	if !strings.Contains(rest, specGrant) {
		t.Errorf("Spec grant %s does not follow the reopen block:\n%s", specGrant, got)
	}
	for _, dir := range []string{"/usr/local/etc", "/usr/local/var"} {
		if n := strings.Count(got, `(subpath "`+dir); n != 1 {
			t.Errorf("%d subpath terms name %s or beneath it, want only its deny:\n%s", n, dir, got)
		}
	}
}

func TestRender_SpecGrantReopensTheUsrLocalDeny(t *testing.T) {
	cases := []struct {
		name  string
		spec  Spec
		grant string
	}{
		{"read file", Spec{ReadFiles: []string{"/usr/local/etc/openssl@3/openssl.cnf"}}, `(literal "/usr/local/etc/openssl@3/openssl.cnf")`},
		{"read subtree", Spec{Read: []string{"/usr/local/etc"}}, usrLocalEtcTerm},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Render(tc.spec)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			// The /usr/local/var term is unique here: it marks the baseline
			// deny even when the grant spells the same term as the etc deny.
			deny := strings.Index(got, usrLocalVarTerm)
			if deny < 0 {
				t.Fatalf("no baseline deny for /usr/local/var:\n%s", got)
			}
			if tc.grant == usrLocalEtcTerm && strings.Count(got, tc.grant) != 2 {
				t.Fatalf("want %s once as the baseline deny and once as the grant:\n%s", tc.grant, got)
			}
			if grant := strings.LastIndex(got, tc.grant); grant < deny {
				t.Errorf("grant %s at %d does not follow the baseline deny at %d:\n%s", tc.grant, grant, deny, got)
			}
		})
	}
}

// TestRender_BaselineIsSystemOnly pins what every session gets without being
// asked, and what it must never include.
func TestRender_BaselineIsSystemOnly(t *testing.T) {
	got, err := Render(Spec{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{`(literal "/")`, `(literal "/var")`, `(literal "/etc")`, `(subpath "/usr")`, `(subpath "/private/etc")`} {
		if !strings.Contains(got, want) {
			t.Errorf("baseline lacks %s\n%s", want, got)
		}
	}
	for _, never := range []string{"/Users", "/Volumes", "/opt", ".ssh", `(subpath "/")`, `(subpath "/private")`, `(subpath "/private/var")`} {
		if strings.Contains(got, never) {
			t.Errorf("baseline names %q\n%s", never, got)
		}
	}
}

// TestRender_ReadOnlyGrantIsNotWritable pins the two directions apart: a Read
// entry is emitted only in the read block, a ReadWrite entry in both.
func TestRender_ReadOnlyGrantIsNotWritable(t *testing.T) {
	got, err := Render(Spec{
		Read:      []string{"/private/tmp/relay-sandbox-golden/ro"},
		ReadFiles: []string{"/private/tmp/relay-sandbox-golden/ro.conf"},
		ReadWrite: []string{"/private/tmp/relay-sandbox-golden/rw"},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	writeAt := strings.Index(got, "(allow file-read* file-write*")
	if writeAt < 0 {
		t.Fatalf("no read-write block:\n%s", got)
	}
	readBlock, writeBlock := got[:writeAt], got[writeAt:]
	for _, ro := range []string{`(subpath "/private/tmp/relay-sandbox-golden/ro")`, `(literal "/private/tmp/relay-sandbox-golden/ro.conf")`} {
		if !strings.Contains(readBlock, ro) {
			t.Errorf("read block lacks %s\n%s", ro, got)
		}
		if strings.Contains(writeBlock, ro) {
			t.Errorf("a read-only grant %s is in the write block\n%s", ro, got)
		}
	}
	if !strings.Contains(writeBlock, `(subpath "/private/tmp/relay-sandbox-golden/rw")`) {
		t.Errorf("write block lacks the read-write grant\n%s", got)
	}
}

// TestRender_AncestorsGetMetadataButNotContents pins that reaching a granted
// directory needs `stat` on the path above it, and that this never widens to
// a listing or a read.
func TestRender_AncestorsGetMetadataButNotContents(t *testing.T) {
	got, err := Render(Spec{ReadWrite: []string{"/private/tmp/relay-sandbox-golden/projects/widget"}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	at := strings.Index(got, "(allow file-read-metadata")
	if at < 0 {
		t.Fatalf("no metadata block:\n%s", got)
	}
	meta := got[at:]
	for _, want := range []string{
		`(literal "/private/tmp/relay-sandbox-golden/projects")`,
		`(literal "/private/tmp/relay-sandbox-golden")`,
	} {
		if !strings.Contains(meta, want) {
			t.Errorf("metadata block lacks %s\n%s", want, meta)
		}
	}
	if strings.Contains(got[:at], `"/private/tmp/relay-sandbox-golden/projects")`) {
		t.Errorf("an ancestor was granted contents, not just metadata\n%s", got)
	}
}

// TestRender_SymlinkedGrantAlsoNamesTheLink pins that a grant whose final
// component is a symlink is readable as the link and as its target: a process
// follows the link by reading it first. A grant that merely passes through a
// symlinked parent names only the resolved path.
func TestRender_SymlinkedGrantAlsoNamesTheLink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	link := filepath.Join(root, "link")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	got, err := Render(Spec{Read: []string{link}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(got, `(subpath "`+resolve(link)+`")`) {
		t.Errorf("profile lacks the target subtree\n%s", got)
	}
	// The link sits under the kernel's spelling of its parent, not the
	// caller's: t.TempDir is reached through /var, which is /private/var.
	if want := filepath.Join(resolve(root), "link"); !strings.Contains(got, `(literal "`+want+`")`) {
		t.Errorf("profile lacks the link itself, %s\n%s", want, got)
	}
}

func TestRender_EscapesQuotesBackslashesSpacesAndUnicode(t *testing.T) {
	got, err := Render(Spec{ReadWrite: []string{
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
	got, err := Render(Spec{ReadWriteFiles: []string{"/private/tmp/relay-sandbox-golden/home/.claude.json"}})
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
		{"quote in a file entry", Spec{ReadWriteFiles: []string{`/private/tmp/odd "q" dir/.claude.json`}}},
		{"quote in a socket directory", Spec{UnixConnectDenyDirs: []string{`/private/tmp/odd "q" dir`}}},
		{"newline in a read-write directory", Spec{ReadWrite: []string{"/private/tmp/line\nbreak"}}},
		{"newline in a read directory", Spec{Read: []string{"/private/tmp/line\nbreak"}}},
		{"newline in a read file", Spec{ReadFiles: []string{"/private/tmp/line\nbreak"}}},
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

// TestResolve_CorrectsLetterCase pins the reason resolve asks the kernel for
// the path rather than trusting the caller's spelling. A project path stored
// as `/users/me/Proj` opens fine on a case-insensitive volume, but Seatbelt
// matches the kernel's spelling, so a grant written that way matches nothing
// and the session is locked out of its own directory.
func TestResolve_CorrectsLetterCase(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	dir := filepath.Join(root, "MixedCaseProject")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	wrong := strings.ToLower(dir)
	if _, err := os.Stat(wrong); err != nil {
		t.Skip("this volume is case-sensitive; the case problem does not exist here")
	}
	if got := resolve(wrong); got != dir {
		t.Errorf("resolve(%q) = %q, want the on-disk spelling %q", wrong, got, dir)
	}
	if got, want := resolve(filepath.Join(wrong, "not", "yet")), filepath.Join(dir, "not", "yet"); got != want {
		t.Errorf("resolve(missing under a wrongly-cased dir) = %q, want %q", got, want)
	}
}

// TestRender_SymlinkedParentNamesOnlyTheResolvedPath pins the other half: a
// caller's spelling that differs only in a symlinked parent (/var for
// /private/var, ~ through a link) adds no literal, because Seatbelt never sees
// that spelling.
func TestRender_SymlinkedParentNamesOnlyTheResolvedPath(t *testing.T) {
	root := t.TempDir() // reached through /var, which is /private/var
	dir := filepath.Join(root, "proj")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got, err := Render(Spec{ReadWrite: []string{dir}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(got, `(literal "`+dir+`")`) && dir != resolve(dir) {
		t.Errorf("profile names the unresolved spelling %s\n%s", dir, got)
	}
}

func TestRender_DenyComesAfterEveryGrantAndNamesItsPath(t *testing.T) {
	s := goldenSpec()
	s.Read = append(s.Read, "/usr/local/etc")
	s.ReadFiles = append(s.ReadFiles, "/usr/local/etc/openssl@3/openssl.cnf")
	s.Deny = []string{"/private/tmp/relay-sandbox-golden/home/.ssh"}
	got, err := Render(s)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	deny := strings.Index(got, `(subpath "/private/tmp/relay-sandbox-golden/home/.ssh")`)
	if deny < 0 {
		t.Fatalf("no deny names the path:\n%s", got)
	}
	if head := strings.LastIndex(got[:deny], "\n("); !strings.HasPrefix(got[head+1:], "(deny file-read* file-write*\n") {
		t.Errorf("the path is not under a read-and-write deny:\n%s", got)
	}
	for _, earlier := range []string{
		"(allow file-read*\n",
		"(allow file-read* file-write*",
		"(allow file-read-metadata",
		usrLocalEtcTerm,
		usrLocalVarTerm,
		`(literal "/usr/local/etc/openssl@3/openssl.cnf")`,
	} {
		if i := strings.LastIndex(got, earlier); i < 0 || i > deny {
			t.Errorf("%q is not before the Spec.Deny term (%d, %d)", earlier, i, deny)
		}
	}
}

func TestRender_DenyRefusesAPathItCannotExpress(t *testing.T) {
	if _, err := Render(Spec{Deny: []string{"/tmp/bad\npath"}}); err == nil {
		t.Fatal("a control character in a deny path rendered")
	}
}
