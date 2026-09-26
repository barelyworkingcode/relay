package main

// R-S8 hermetic tests: what AuthorizeLaunch actually writes to disk for a
// sandboxed session, and what it deliberately does not write for one that is
// not sandboxed.

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/service"
	"github.com/barelyworkingcode/relay/internal/sessions/sandbox"
)

const (
	unlinkAncestorHead = "(deny file-write-unlink file-clone\n"
	socketFileRuleHead = "(deny file-write-unlink\n"
)

// normalizeSandboxProfile replaces every machine-specific path in a
// generated profile with a stable placeholder. Order matters: a directory
// that contains another must be replaced after it, or it eats the prefix.
func normalizeSandboxProfile(t *testing.T, body, projectA, projectB, home, eveData string) string {
	t.Helper()
	type sub struct{ from, to string }
	subs := []sub{
		{sandboxRealPath(t, projectA), "<PROJECT>"},
		{sandboxRealPath(t, projectB), "<OTHER_PROJECT>"},
		{sandboxRealPath(t, eveData), "<EVE_DATA>"},
		{sandboxRealPath(t, home), "<HOME>"},
		{sandboxRealPath(t, bridge.ConfigDir()), "<RELAY_DIR>"},
		{sandboxRealPath(t, os.TempDir()), "<DARWIN_TMP>"},
	}
	roots := make([]string, 0, len(subs))
	for _, s := range subs {
		roots = append(roots, s.from)
	}
	placeholders := func(p string) string {
		for _, s := range subs {
			p = strings.ReplaceAll(p, s.from, s.to)
		}
		return p
	}
	return dropMetadataBlock(placeholders(normalizeUnlinkAncestorBlock(body, roots, placeholders)))
}

// normalizeUnlinkAncestorBlock rewrites the ancestor unlink-and-clone block
// into what a golden can hold. The ancestors of a fixture root are temp
// directories with random names, so they are dropped; the rest are spelled
// with placeholders and byte-sorted again, because a placeholder does not sort
// where the path it replaces did.
func normalizeUnlinkAncestorBlock(body string, roots []string, placeholders func(string) string) string {
	at := strings.Index(body, unlinkAncestorHead)
	if at < 0 {
		return body
	}
	terms, rest := profileBlockTerms(body[at+len(unlinkAncestorHead):])
	var kept []string
	for _, term := range terms {
		p := strings.TrimSuffix(strings.TrimPrefix(term, `(literal "`), `")`)
		if !strictAncestorOfAny(p, roots) {
			kept = append(kept, `(literal "`+placeholders(p)+`")`)
		}
	}
	slices.Sort(kept)
	var b strings.Builder
	b.WriteString(body[:at])
	if len(kept) > 0 {
		b.WriteString(unlinkAncestorHead)
		for i, term := range kept {
			b.WriteString("  " + term)
			if i == len(kept)-1 {
				b.WriteString(")")
			}
			b.WriteString("\n")
		}
	}
	b.WriteString(rest)
	return b.String()
}

func strictAncestorOfAny(p string, roots []string) bool {
	for _, r := range roots {
		if strings.HasPrefix(r, p+"/") {
			return true
		}
	}
	return false
}

// profileBlockTerms reads the indented terms at the start of body, the text
// after a block's head line, and returns them with the text after the block.
func profileBlockTerms(body string) (terms []string, rest string) {
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

// allowBlocks is every allow block in a profile, heads and terms, and nothing
// else: the text a grant can appear in.
func allowBlocks(body string) string {
	var b strings.Builder
	for rest := body; ; {
		at := strings.Index(rest, "\n(allow ")
		if at < 0 {
			return b.String()
		}
		head, after, _ := strings.Cut(rest[at+1:], "\n")
		_, rest = profileBlockTerms(after)
		b.WriteString(head + "\n" + after[:len(after)-len(rest)])
	}
}

// dropBlock removes the block whose head line is head, if the profile has one.
func dropBlock(body, head string) string {
	at := strings.Index(body, head)
	if at < 0 {
		return body
	}
	_, rest := profileBlockTerms(body[at+len(head):])
	return body[:at] + rest
}

// dropMetadataBlock removes the ancestor-metadata block from a profile. Its
// entries are the parent directories of every grant, and a grant under a test
// temp directory has parents whose names are random, so they cannot sit in a
// golden. The block's construction is pinned in the sandbox package's own tests.
func dropMetadataBlock(body string) string {
	start := strings.Index(body, "(allow file-read-metadata")
	if start < 0 {
		return body
	}
	end := strings.Index(body[start:], "))\n")
	if end < 0 {
		return body
	}
	return body[:start] + body[start+end+3:]
}

// noDeveloperTools makes the profile independent of the developer tools the
// test machine has installed.
func noDeveloperTools(t *testing.T) {
	t.Helper()
	original := developerTools
	t.Cleanup(func() { developerTools = original })
	developerTools = func() string { return "" }
}

// setModelEndpoint gives a test store C8's default block, so the golden
// covers C7's tcp_loopback_allow rather than the "no TCP listener at all"
// shape a store that has never decided has.
func setModelEndpoint(t *testing.T, store config.SettingsStore) {
	t.Helper()
	if err := store.With(func(s *config.Settings) {
		s.ModelEndpoint = &config.ModelEndpointConfig{Listen: "127.0.0.1:8180"}
	}); err != nil {
		t.Fatalf("store.With: %v", err)
	}
}

// addEveTestService registers eve the way eve's own setup does — a working
// directory holding the checkout, no --data flag — and returns the data
// directory relay must derive from that record.
func addEveTestService(t *testing.T, store config.SettingsStore) string {
	t.Helper()
	checkout := t.TempDir()
	data := filepath.Join(checkout, "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatalf("mkdir eve data: %v", err)
	}
	if err := store.With(func(s *config.Settings) {
		s.AddService(config.ServiceConfig{
			ID:         eveServiceID,
			Command:    "node",
			Args:       []string{"--env-file=.env", "server.js"},
			WorkingDir: checkout,
		})
	}); err != nil {
		t.Fatalf("store.With: %v", err)
	}
	return data
}

func sandboxRealPath(t *testing.T, p string) string {
	t.Helper()
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

// launchWithSandbox runs one authorized launch and returns the profile it
// wrote.
func launchWithSandbox(t *testing.T, req LaunchRequest, store config.SettingsStore) (*LaunchResult, string) {
	t.Helper()
	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), newLaunchTestLedger(t), req)
	if refusal != nil {
		t.Fatalf("refused: %+v", refusal)
	}
	if result.Spec.Sandbox == nil || result.Spec.Sandbox.ProfilePath == "" {
		t.Fatalf("no sandbox profile for a %s launch", req.Kind)
	}
	body, err := os.ReadFile(result.Spec.Sandbox.ProfilePath)
	if err != nil {
		t.Fatalf("read profile: %v", err)
	}
	return result, string(body)
}

// TestAuthorizeLaunch_SandboxProfileGoldenPerKind pins the profile text for
// every kind C7 sandboxes by default, one golden each. What a session reaches
// is now a property of its template and its kind, so the profiles differ on
// purpose: a shell holds the home directory, Claude Code and pi hold only what
// they need, and chat has no template and holds nothing but its project. A
// profile that starts rendering differently is a change somebody has to justify,
// and this is where it shows up as a diff.
func TestAuthorizeLaunch_SandboxProfileGoldenPerKind(t *testing.T) {
	for _, tc := range []struct {
		name, golden string
		req          LaunchRequest
	}{
		{"claude", "golden_sandbox_profile_claude.sb", LaunchRequest{Kind: KindClaude}},
		{"pi", "golden_sandbox_profile_pi.sb", LaunchRequest{Kind: KindPi}},
		{"chat", "golden_sandbox_profile_chat.sb", LaunchRequest{Kind: KindChat, Model: "gpt-5"}},
		{"shell", "golden_sandbox_profile_shell.sb", LaunchRequest{Kind: KindPTY, TemplateID: "shell"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			golden, err := os.ReadFile(filepath.Join("testdata", tc.golden))
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			noDeveloperTools(t)
			store := newLaunchTestStore(t)
			proj := addLaunchTestProject(t, store, nil)
			other := addLaunchTestProject(t, store, func(p *config.Project) { p.ID = "p2"; p.Name = "Other" })
			eveData := addEveTestService(t, store)
			setModelEndpoint(t, store)
			home := t.TempDir()
			t.Setenv("HOME", home)

			req := tc.req
			req.Caller = bearerCaller(control.ClassExecute)
			req.ProjectID = proj.ID
			_, body := launchWithSandbox(t, req, store)

			got := normalizeSandboxProfile(t, body, proj.Path, other.Path, home, eveData)
			if got != string(golden) {
				t.Fatalf("profile drifted from the golden file.\ngot:\n%s\nwant:\n%s", got, golden)
			}
		})
	}
}

// TestAuthorizeLaunch_SandboxProfileIsWrittenWhereTheShimLooks pins C7's
// "profiles/<session_id>.sb, mode 0600" and the absolute-path requirement
// the shim's own --sandbox-profile validation enforces.
func TestAuthorizeLaunch_SandboxProfileIsWrittenWhereTheShimLooks(t *testing.T) {
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)

	result, _ := launchWithSandbox(t, LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindClaude,
	}, store)

	path := result.Spec.Sandbox.ProfilePath
	want := filepath.Join(bridge.ConfigDir(), "sessions", "profiles", result.SessionID+".sb")
	if path != want {
		t.Fatalf("profile path = %q, want %q", path, want)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("profile path %q is not absolute", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

// TestAuthorizeLaunch_SandboxProfileContents asserts the rules whose absence
// would be invisible in a golden that drifted with them: the project it owns,
// the hook socket a session must reach, and the Keychains SP2 found must stay
// readable (blocking them logs Claude Code out, so their disappearance is a
// regression, not a tightening).
func TestAuthorizeLaunch_SandboxProfileContents(t *testing.T) {
	noDeveloperTools(t)
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	home := t.TempDir()
	t.Setenv("HOME", home)

	_, body := launchWithSandbox(t, LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindClaude,
	}, store)

	relayDir := sandboxRealPath(t, bridge.ConfigDir())
	for _, want := range []string{
		`(subpath "` + sandboxRealPath(t, proj.Path) + `")`,
		`(subpath "` + filepath.Join(sandboxRealPath(t, home), "Library", "Keychains") + `")`,
		`(remote unix-socket (path-literal "` + filepath.Join(relayDir, filepath.Base(service.RelaySessionsHookSocketPath(bridge.ConfigDir()))) + `"))`,
		`(remote unix-socket (path-literal "` + filepath.Join(relayDir, "relay.sock") + `"))`,
		`(remote unix-socket (path-regex #"^` + relayDir + `/"))`,
		`(deny process-exec* (require-any (file-mode #o4000) (file-mode #o2000)))`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("profile is missing %s\ngot:\n%s", want, body)
		}
	}
}

// TestAuthorizeLaunch_SandboxGrantsOnlyWhatItNames is the point of the model:
// the profile denies files by default and names no directory to deny, so what
// a session cannot reach is everything the profile does not grant. Another
// project, relay's own directory, eve's data and the home directory itself are
// absent, and the one deny names no path. (The shell template is the exception
// that proves it: it names the home directory, so it holds ~/.ssh.)
func TestAuthorizeLaunch_SandboxGrantsOnlyWhatItNames(t *testing.T) {
	noDeveloperTools(t)
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	other := addLaunchTestProject(t, store, func(p *config.Project) { p.ID = "p2"; p.Name = "Other" })
	eveData := addEveTestService(t, store)
	home := t.TempDir()
	t.Setenv("HOME", home)

	_, body := launchWithSandbox(t, LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindClaude,
	}, store)

	if !strings.Contains(body, "(deny file-read* file-write*)\n") {
		t.Errorf("profile lacks the one bare file deny:\n%s", body)
	}
	// Ancestor metadata (stat, never contents) names the home directory as the
	// parent of ~/.cache, and the unlink denies name it and the socket deny
	// dirs; none of those is a grant.
	fileRules := body[:strings.Index(body, "(deny network-outbound")]
	for _, head := range []string{unlinkAncestorHead, socketFileRuleHead} {
		fileRules = dropBlock(fileRules, head)
	}
	fileRules = dropMetadataBlock(fileRules)
	homeReal := sandboxRealPath(t, home)
	for name, path := range map[string]string{
		"another project":    sandboxRealPath(t, other.Path),
		"relay's directory":  sandboxRealPath(t, bridge.ConfigDir()),
		"eve's data":         sandboxRealPath(t, eveData),
		"the home directory": homeReal,
		"~/.ssh":             filepath.Join(homeReal, ".ssh"),
	} {
		if strings.Contains(fileRules, `"`+path+`"`) {
			t.Errorf("profile grants %s (%s):\n%s", name, path, fileRules)
		}
	}
}

// TestSandboxSpecForLaunch_PiSessionsIsReadWriteAllowed asserts a pi session is
// granted exactly the one leaf it must be able to write its own transcript
// into (<config dir>/sessions/pi-sessions), that no other kind is, and that
// nothing grants the config dir itself: it is unreachable because it is never
// named.
func TestSandboxSpecForLaunch_PiSessionsIsReadWriteAllowed(t *testing.T) {
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	settings := store.Get()
	relayDir := bridge.ConfigDir()
	wantGrant := filepath.Join(relayDir, "sessions", "pi-sessions")

	spec, err := sandboxSpecForLaunch(settings, &proj, proj.Path, KindPi, nil)
	if err != nil {
		t.Fatalf("sandboxSpecForLaunch: %v", err)
	}
	found := false
	for _, p := range spec.ReadWrite {
		if p == wantGrant {
			found = true
		}
		if p == relayDir {
			t.Fatalf("ReadWrite grants the whole config dir %q", relayDir)
		}
	}
	if !found {
		t.Fatalf("ReadWrite = %v, want to contain %q", spec.ReadWrite, wantGrant)
	}
	for _, p := range spec.Read {
		if p == relayDir {
			t.Fatalf("Read grants the whole config dir %q", relayDir)
		}
	}

	noDeveloperTools(t)
	body, err := sandbox.Render(spec)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	relayReal := sandboxRealPath(t, relayDir)
	if strings.Contains(allowBlocks(body), `(subpath "`+relayReal+`")`) {
		t.Fatalf("rendered profile grants the whole config dir:\n%s", body)
	}
	// pi-sessions/ itself never exists on disk in this test, so it is
	// resolved (sandbox.resolve's own "walk up to the nearest existing
	// ancestor" rule) as relayReal's own resolved form plus the literal
	// suffix, not independently re-resolved here.
	if !strings.Contains(body, `(subpath "`+filepath.Join(relayReal, "sessions", "pi-sessions")+`")`) {
		t.Fatalf("rendered profile does not grant pi-sessions:\n%s", body)
	}

	for _, kind := range []string{KindClaude, KindChat, KindPTY} {
		other, err := sandboxSpecForLaunch(settings, &proj, proj.Path, kind, nil)
		if err != nil {
			t.Fatalf("sandboxSpecForLaunch(%s): %v", kind, err)
		}
		for _, p := range other.ReadWrite {
			if p == wantGrant {
				t.Errorf("a %s session was granted pi's transcript directory", kind)
			}
		}
	}
}

// TestSandboxTemplate_FoldersBecomeGrants pins where a tool's directories
// come from: the `read` and `read_write` lists of the template being launched,
// ~ expanded, each in its own direction.
func TestSandboxTemplate_FoldersBecomeGrants(t *testing.T) {
	noDeveloperTools(t)
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := store.With(func(s *config.Settings) {
		s.TerminalTemplates = append(s.TerminalTemplates, config.TerminalTemplate{
			ID: "custom", Name: "Custom", Sandbox: ptr(true),
			Read:      []string{"~/.hermes/node", "/private/tmp/relay-extra-read"},
			ReadWrite: []string{"~/scratch"},
		})
	}); err != nil {
		t.Fatalf("store.With: %v", err)
	}

	_, body := launchWithSandbox(t, LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "custom",
	}, store)

	homeReal := sandboxRealPath(t, home)
	writeAt := strings.Index(body, "(allow file-read* file-write*")
	if writeAt < 0 {
		t.Fatalf("no read-write block:\n%s", body)
	}
	readPart, writePart := body[:writeAt], body[writeAt:]
	for _, want := range []string{
		`(subpath "` + filepath.Join(homeReal, ".hermes", "node") + `")`,
		`(subpath "/private/tmp/relay-extra-read")`,
	} {
		if !strings.Contains(readPart, want) {
			t.Errorf("read block lacks %s\n%s", want, body)
		}
		if strings.Contains(writePart, want) {
			t.Errorf("a read grant %s is in the write block\n%s", want, body)
		}
	}
	if want := `(subpath "` + filepath.Join(homeReal, "scratch") + `")`; !strings.Contains(writePart, want) {
		t.Errorf("write block lacks %s\n%s", want, body)
	}
	// Another template's folders do not leak in: the custom one names no
	// ~/.claude, and the profile holds none.
	if strings.Contains(body, ".claude") {
		t.Errorf("a template that names no ~/.claude was granted it\n%s", body)
	}
}

// TestSandboxTemplate_ShellHoldsTheHomeDirectory pins a decision, not an
// accident: the shell template grants ~ read-write, so a shell session can reach
// ~/.ssh. The claude-code template does not.
func TestSandboxTemplate_ShellHoldsTheHomeDirectory(t *testing.T) {
	noDeveloperTools(t)
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	home := t.TempDir()
	t.Setenv("HOME", home)
	homeReal := sandboxRealPath(t, home)

	_, shell := launchWithSandbox(t, LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "shell",
	}, store)
	if want := `(subpath "` + homeReal + `")`; !strings.Contains(shell, want) {
		t.Errorf("the shell template does not grant the home directory:\n%s", shell)
	}

	_, claude := launchWithSandbox(t, LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "claude-code",
	}, store)
	if strings.Contains(dropBlock(dropMetadataBlock(claude), unlinkAncestorHead), `"`+homeReal+`"`) {
		t.Errorf("the claude-code template grants the home directory:\n%s", claude)
	}
}

// TestSandboxTemplate_FilesAndDirectoriesAreTreatedApart pins how an entry is
// classified: an existing regular file is a file grant (read-write ones keep
// their atomic-write siblings), anything else is a subtree, and a missing
// read-write directory is created while a missing file name is not.
func TestSandboxTemplate_FilesAndDirectoriesAreTreatedApart(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.Mkdir(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	dirs, files, err := addTemplateGrants(nil, nil, "read_write",
		[]string{"~/.claude.json", "~/.claude", "~/go/pkg", "~/.later.json"}, home)
	if err != nil {
		t.Fatalf("addTemplateGrants: %v", err)
	}
	if len(files) != 1 || files[0] != filepath.Join(home, ".claude.json") {
		t.Errorf("files = %v, want only the existing regular file", files)
	}
	wantDirs := []string{filepath.Join(home, ".claude"), filepath.Join(home, "go", "pkg"), filepath.Join(home, ".later.json")}
	if !slices.Equal(dirs, wantDirs) {
		t.Errorf("dirs = %v, want %v", dirs, wantDirs)
	}

	ensureGrantDirs(dirs)
	if info, err := os.Stat(filepath.Join(home, "go", "pkg")); err != nil || !info.IsDir() {
		t.Errorf("a missing read-write directory was not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".later.json")); err == nil {
		t.Error("a missing name with an extension was created as a directory; the tool expecting a file would break")
	}
}

// TestSandboxTemplate_RefuseAnEntryRelayCannotPlace pins fail-closed at launch:
// an entry that is relative, empty or the whole filesystem refuses rather than
// being dropped, which would leave the operator believing a folder was
// reachable when it was not.
func TestSandboxTemplate_RefuseAnEntryRelayCannotPlace(t *testing.T) {
	for name, entry := range map[string]string{
		"relative":                        "tools/bin",
		"empty":                           "",
		"whole filesystem":                "/",
		"whole filesystem, spelled oddly": "/tmp/..",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := addTemplateGrants(nil, nil, "read", []string{entry}, "/private/tmp/home"); err == nil {
				t.Fatalf("addTemplateGrants accepted %q", entry)
			}
		})
	}
}

// TestTemplateForKind pins that a claude, pi or chat session reads its folders
// from the template named for its kind, and that a missing template is not a
// refusal: the session gets only what every session gets.
func TestTemplateForKind(t *testing.T) {
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	settings := store.Get()

	if got := templateForKind(settings, &proj, KindClaude); got == nil || got.ID != "claude-code" {
		t.Errorf("claude session template = %+v, want claude-code", got)
	}
	if got := templateForKind(settings, &proj, KindPi); got == nil || got.ID != "pi" {
		t.Errorf("pi session template = %+v, want pi", got)
	}
	if got := templateForKind(settings, &proj, KindChat); got != nil {
		t.Errorf("chat session template = %+v, want none (no `chat` template is seeded)", got)
	}
	if got := templateForKind(settings, &proj, KindPTY); got != nil {
		t.Errorf("a terminal has no kind template, got %+v", got)
	}
}

func TestDeveloperToolsRoot(t *testing.T) {
	for in, want := range map[string]string{
		"/Applications/Xcode.app/Contents/Developer":       "/Applications/Xcode.app/Contents",
		"/Applications/Xcode-beta.app/Contents/Developer":  "/Applications/Xcode-beta.app/Contents",
		"/Library/Developer/CommandLineTools":              "/Library/Developer/CommandLineTools",
		"/Users/me/tools/Xcode.app/Contents/Developer/usr": "/Users/me/tools/Xcode.app/Contents",
	} {
		if got := developerToolsRoot(in); got != want {
			t.Errorf("developerToolsRoot(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAuthorizeLaunch_ShellTemplateIsSandboxed pins that a plain Shell
// terminal is confined: it shipped with sandbox off, so a terminal opened from
// eve could write anywhere the user could.
func TestAuthorizeLaunch_ShellTemplateIsSandboxed(t *testing.T) {
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), newLaunchTestLedger(t), LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "shell",
	})
	if refusal != nil {
		t.Fatalf("refused: %+v", refusal)
	}
	if result.Spec.Sandbox == nil || result.Spec.Sandbox.ProfilePath == "" {
		t.Fatalf("the built-in shell template got no sandbox profile: %+v", result.Spec.Sandbox)
	}
	if !result.AuditFields.Sandbox {
		t.Error("audit says a shell launch is unsandboxed")
	}
}

// A console template that never mentions the sandbox is confined: only an
// explicit sandbox: false opts out.
func TestAuthorizeLaunch_TemplateWithoutSandboxKeyIsSandboxed(t *testing.T) {
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	if err := store.With(func(s *config.Settings) {
		s.TerminalTemplates = append(s.TerminalTemplates, config.TerminalTemplate{ID: "unmarked", Name: "Unmarked"})
	}); err != nil {
		t.Fatalf("store.With: %v", err)
	}
	result, _ := launchWithSandbox(t, LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "unmarked",
	}, store)
	if !result.AuditFields.Sandbox {
		t.Error("audit says a template without a sandbox key launched unsandboxed")
	}
}

// TestAuthorizeLaunch_NoProfileWhenNotSandboxed covers the ways a launch is
// deliberately unconfined: a pty template that says sandbox: false, and an SSH
// project whose target runs on another machine entirely, whatever its
// template omits.
func TestAuthorizeLaunch_NoProfileWhenNotSandboxed(t *testing.T) {
	t.Run("pty template with sandbox off", func(t *testing.T) {
		store := newLaunchTestStore(t)
		proj := addLaunchTestProject(t, store, nil)
		result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), newLaunchTestLedger(t), LaunchRequest{
			Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "plain",
		})
		if refusal != nil {
			t.Fatalf("refused: %+v", refusal)
		}
		if result.Spec.Sandbox != nil {
			t.Fatalf("a template with sandbox off got a profile: %+v", result.Spec.Sandbox)
		}
		if result.AuditFields.Sandbox {
			t.Error("audit says sandbox for a template that opted out")
		}
		if entries, err := os.ReadDir(sessionProfilesDir()); err == nil && len(entries) != 0 {
			t.Errorf("an unsandboxed launch wrote %d profile(s)", len(entries))
		}
	})

	t.Run("ssh-hosted pty template without a sandbox key", func(t *testing.T) {
		store := newLaunchTestStore(t)
		proj := addLaunchTestHostedProject(t, store, []config.TerminalTemplate{{ID: "shell", Name: "Shell"}})
		result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), newLaunchTestLedger(t), LaunchRequest{
			Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "shell",
		})
		if refusal != nil {
			t.Fatalf("refused: %+v", refusal)
		}
		if result.Spec.Sandbox != nil {
			t.Fatalf("a hosted terminal got a local profile: %+v", result.Spec.Sandbox)
		}
		if result.AuditFields.Sandbox {
			t.Error("audit claims a hosted terminal is sandboxed")
		}
		if entries, err := os.ReadDir(sessionProfilesDir()); err == nil && len(entries) != 0 {
			t.Errorf("a hosted launch wrote %d profile(s)", len(entries))
		}
	})

	t.Run("ssh-hosted project", func(t *testing.T) {
		store := newLaunchTestStore(t)
		proj := addLaunchTestProject(t, store, func(p *config.Project) { p.HostID = "host-1" })
		if err := store.With(func(s *config.Settings) {
			s.Hosts = append(s.Hosts, config.Host{ID: "host-1", Name: "far", Target: "someone@far.local", TerminalTemplates: []config.TerminalTemplate{{ID: "claude-code", Name: "Claude Code", Command: "claude"}}})
		}); err != nil {
			t.Fatalf("store.With: %v", err)
		}
		result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), newLaunchTestLedger(t), LaunchRequest{
			Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindClaude,
		})
		if refusal != nil {
			t.Fatalf("refused: %+v", refusal)
		}
		if result.Spec.Sandbox != nil {
			t.Fatalf("an SSH session got a local profile: %+v", result.Spec.Sandbox)
		}
		if result.AuditFields.Sandbox {
			t.Error("audit claims an SSH session is sandboxed")
		}
	})
}

// TestEveDataDir_ComesFromEvesRegisteredRecord covers every shape of eve's
// service record, including the two that yield nothing: a path relay cannot
// derive must produce no rule at all, because a subpath deny on a directory
// eve never writes reads as enforcement and is not.
func TestEveDataDir_ComesFromEvesRegisteredRecord(t *testing.T) {
	for _, tc := range []struct {
		name string
		svc  *config.ServiceConfig
		want string
	}{
		{"default beside the checkout", &config.ServiceConfig{ID: eveServiceID, WorkingDir: "/private/tmp/eve"}, "/private/tmp/eve/data"},
		{"absolute --data", &config.ServiceConfig{ID: eveServiceID, WorkingDir: "/private/tmp/eve", Args: []string{"server.js", "--data", "/private/tmp/evedata"}}, "/private/tmp/evedata"},
		{"relative --data resolves against the working dir", &config.ServiceConfig{ID: eveServiceID, WorkingDir: "/private/tmp/eve", Args: []string{"--data", "state"}}, "/private/tmp/eve/state"},
		{"no working dir to anchor the default", &config.ServiceConfig{ID: eveServiceID}, ""},
		{"--data with no value", &config.ServiceConfig{ID: eveServiceID, Args: []string{"--data"}}, ""},
		{"eve not registered", nil, ""},
		{"another service is not eve", &config.ServiceConfig{ID: "relay-llm", WorkingDir: "/private/tmp/relayllm"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := &config.Settings{}
			if tc.svc != nil {
				settings.Services = []config.ServiceConfig{*tc.svc}
			}
			if got := eveDataDir(settings); got != tc.want {
				t.Fatalf("eveDataDir = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAuthorizeLaunch_EveDenyNamesARealDirectory is the assertion a socket
// denial has to pass to be worth emitting: the directory it names exists, and
// it is the one eve's own registered record puts its auth material in. Eve's
// files need no rule of their own; nothing grants them.
func TestAuthorizeLaunch_EveDenyNamesARealDirectory(t *testing.T) {
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	eveData := addEveTestService(t, store)

	_, body := launchWithSandbox(t, LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindClaude,
	}, store)

	if want := `(remote unix-socket (path-regex #"^` + sandboxRealPath(t, eveData) + `/"))`; !strings.Contains(body, want) {
		t.Errorf("profile is missing %s\ngot:\n%s", want, body)
	}
	if strings.Contains(body, filepath.Join("Application Support", "eve")) {
		t.Errorf("profile names an Application Support directory eve does not use:\n%s", body)
	}

	t.Run("no record, no rule", func(t *testing.T) {
		store := newLaunchTestStore(t)
		proj := addLaunchTestProject(t, store, nil)
		_, body := launchWithSandbox(t, LaunchRequest{
			Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindClaude,
		}, store)
		if strings.Contains(body, "eve") {
			t.Errorf("a machine with no eve registered got an eve rule:\n%s", body)
		}
	})
}

// TestAuthorizeLaunch_DeniesRelayAPIListenerPort covers SH §5.2's third TCP
// denial, the one whose port exists only when an operator binds it.
func TestAuthorizeLaunch_DeniesRelayAPIListenerPort(t *testing.T) {
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	t.Setenv(EnvAPIListen, "127.0.0.1:8791")

	_, body := launchWithSandbox(t, LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindClaude,
	}, store)

	want := "(deny network-outbound\n" +
		"  (remote ip \"localhost:3000\")\n" +
		"  (remote ip \"localhost:8181\")\n" +
		"  (remote ip \"localhost:8791\"))\n"
	if !strings.Contains(body, want) {
		t.Fatalf("profile does not deny relay's own API port.\nwant:\n%s\ngot:\n%s", want, body)
	}
}

// TestAuthorizeLaunch_MintFailureLeavesNoProfile pins the ordering that
// makes the profile the last thing a launch creates: a refusal after it was
// written would leave a file no session owns and no cleanup path visits.
func TestAuthorizeLaunch_MintFailureLeavesNoProfile(t *testing.T) {
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)

	original := mintModelKey
	t.Cleanup(func() { mintModelKey = original })
	mintModelKey = func(*ModelKeyTable, string, string) (string, error) {
		return "", errors.New("no entropy")
	}

	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), newLaunchTestLedger(t), LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPi,
	})
	if result != nil || refusal == nil {
		t.Fatalf("launch was authorized despite a failed mint: %+v", result)
	}
	if refusal.Code != "model_key_mint_failed" {
		t.Fatalf("refusal code = %q, want model_key_mint_failed", refusal.Code)
	}
	entries, err := os.ReadDir(sessionProfilesDir())
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read profiles dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("a refused launch left %d profile(s) behind", len(entries))
	}
}

func TestModelEndpointLoopbackPort(t *testing.T) {
	if _, ok := modelEndpointLoopbackPort(&config.Settings{}); ok {
		t.Error("an absent model_endpoint block allowed a port")
	}
	if _, ok := modelEndpointLoopbackPort(&config.Settings{ModelEndpoint: &config.ModelEndpointConfig{}}); ok {
		t.Error("a disabled model endpoint allowed a port")
	}
	port, ok := modelEndpointLoopbackPort(&config.Settings{ModelEndpoint: &config.ModelEndpointConfig{Listen: "127.0.0.1:9999"}})
	if !ok || port != 9999 {
		t.Errorf("port = %d, ok = %v, want 9999", port, ok)
	}
}

// A template's deny list reaches the profile as a deny block after every
// grant, so a `~` read-write grant cannot reopen ~/.ssh.
func TestSandboxTemplate_DenyCarvesAHoleOutOfAHomeGrant(t *testing.T) {
	noDeveloperTools(t)
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := store.With(func(s *config.Settings) {
		s.TerminalTemplates = append(s.TerminalTemplates, config.TerminalTemplate{
			ID: "custom", Name: "Custom", Sandbox: ptr(true),
			ReadWrite: []string{"~"},
			Deny:      []string{"~/.ssh"},
		})
	}); err != nil {
		t.Fatalf("store.With: %v", err)
	}

	_, body := launchWithSandbox(t, LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "custom",
	}, store)

	want := `(subpath "` + filepath.Join(sandboxRealPath(t, home), ".ssh") + `")`
	denyAt := strings.Index(body, want)
	if denyAt < 0 {
		t.Fatalf("profile lacks %s\n%s", want, body)
	}
	if head := strings.LastIndex(body[:denyAt], "\n("); !strings.HasPrefix(body[head+1:], "(deny file-read* file-write*\n") {
		t.Errorf("%s is not under a read-and-write deny block\n%s", want, body)
	}
	if grantAt := strings.Index(body, "(allow file-read* file-write*"); grantAt < 0 || grantAt > denyAt {
		t.Errorf("read-write grant is not before the deny block:\n%s", body)
	}
}
