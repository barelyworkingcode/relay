package main

// R-S8 hermetic tests: what AuthorizeLaunch actually writes to disk for a
// sandboxed session, and what it deliberately does not write for one that is
// not sandboxed.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/service"
	"github.com/barelyworkingcode/relay/internal/sessions/sandbox"
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
	for _, s := range subs {
		body = strings.ReplaceAll(body, s.from, s.to)
	}
	return dropMetadataBlock(body)
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
// every kind C7 sandboxes by default. All four compare against ONE golden on
// purpose: C7's rules are a property of the session's project and this host,
// never of the kind — a kind that starts rendering differently is a change
// somebody has to justify, and this is where it shows up as a diff.
func TestAuthorizeLaunch_SandboxProfileGoldenPerKind(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("testdata", "golden_sandbox_profile.sb"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}

	for _, tc := range []struct {
		name string
		req  LaunchRequest
	}{
		{"claude", LaunchRequest{Kind: KindClaude}},
		{"pi", LaunchRequest{Kind: KindPi}},
		{"chat", LaunchRequest{Kind: KindChat}},
		{"rh", LaunchRequest{Kind: KindPTY, TemplateID: "rh"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
// absent, and the one deny names no path.
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
	// parent of ~/.cache; that is not a grant of it.
	fileRules := dropMetadataBlock(body[:strings.Index(body, "(deny network-outbound")])
	homeReal := sandboxRealPath(t, home)
	for name, path := range map[string]string{
		"another project":    sandboxRealPath(t, other.Path),
		"relay's directory":  sandboxRealPath(t, bridge.ConfigDir()),
		"eve's data":         sandboxRealPath(t, eveData),
		"the home directory": homeReal,
		"~/.ssh":             filepath.Join(homeReal, ".ssh"),
	} {
		if strings.Contains(strings.ReplaceAll(fileRules, `(subpath "`+filepath.Join(sandboxRealPath(t, bridge.ConfigDir()), "sessions", "pi-sessions")+`")`, ""), `"`+path+`"`) {
			t.Errorf("profile grants %s (%s):\n%s", name, path, fileRules)
		}
	}
}

// TestSandboxSpecForLaunch_PiSessionsIsReadWriteAllowed asserts
// sandboxSpecForLaunch grants exactly the one leaf a sandboxed pi session must
// be able to write its own transcript into (<config dir>/sessions/pi-sessions),
// and that nothing grants the config dir itself: it is unreachable because it
// is never named.
func TestSandboxSpecForLaunch_PiSessionsIsReadWriteAllowed(t *testing.T) {
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)

	settings := store.Get()
	spec, err := sandboxSpecForLaunch(settings, &proj, proj.Path)
	if err != nil {
		t.Fatalf("sandboxSpecForLaunch: %v", err)
	}

	relayDir := bridge.ConfigDir()
	wantGrant := filepath.Join(relayDir, "sessions", "pi-sessions")
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
	if strings.Contains(body, `(subpath "`+relayReal+`")`) {
		t.Fatalf("rendered profile grants the whole config dir:\n%s", body)
	}
	// pi-sessions/ itself never exists on disk in this test, so it is
	// resolved (sandbox.resolve's own "walk up to the nearest existing
	// ancestor" rule) as relayReal's own resolved form plus the literal
	// suffix, not independently re-resolved here.
	if !strings.Contains(body, `(subpath "`+filepath.Join(relayReal, "sessions", "pi-sessions")+`")`) {
		t.Fatalf("rendered profile does not grant pi-sessions:\n%s", body)
	}
}

// TestSandboxSettings_AddGrantsFromSettingsJSON pins where a tool that lives
// outside the fixed set gets its directory: `sandbox` in settings.json, ~
// expanded, read or read-write as named.
func TestSandboxSettings_AddGrantsFromSettingsJSON(t *testing.T) {
	noDeveloperTools(t)
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := store.With(func(s *config.Settings) {
		s.Sandbox = &config.SandboxConfig{
			Read:      []string{"~/.hermes/node", "/private/tmp/relay-extra-read"},
			ReadWrite: []string{"~/scratch"},
		}
	}); err != nil {
		t.Fatalf("store.With: %v", err)
	}

	_, body := launchWithSandbox(t, LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindClaude,
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
}

// TestSandboxSettings_RefuseAnEntryRelayCannotPlace pins fail-closed: a grant
// that is relative, empty, or the whole filesystem refuses the launch rather
// than being dropped, which would leave the operator believing a directory was
// reachable when it was not.
func TestSandboxSettings_RefuseAnEntryRelayCannotPlace(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.SandboxConfig
	}{
		{"relative read", config.SandboxConfig{Read: []string{"tools/bin"}}},
		{"empty read-write", config.SandboxConfig{ReadWrite: []string{""}}},
		{"whole filesystem", config.SandboxConfig{Read: []string{"/"}}},
		{"whole filesystem, spelled oddly", config.SandboxConfig{ReadWrite: []string{"/tmp/.."}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newLaunchTestStore(t)
			proj := addLaunchTestProject(t, store, nil)
			cfg := tc.cfg
			if err := store.With(func(s *config.Settings) { s.Sandbox = &cfg }); err != nil {
				t.Fatalf("store.With: %v", err)
			}
			result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), newLaunchTestLedger(t), LaunchRequest{
				Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindClaude,
			})
			if result != nil || refusal == nil {
				t.Fatalf("launch was authorized with sandbox settings %+v: %+v", tc.cfg, result)
			}
			if refusal.Code != "sandbox_unavailable" {
				t.Fatalf("refusal code = %q, want sandbox_unavailable", refusal.Code)
			}
		})
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

// TestAuthorizeLaunch_NoProfileWhenNotSandboxed covers the two ways a launch
// is deliberately unconfined: a pty template that never opted in, and an SSH
// project whose target runs on another machine entirely.
func TestAuthorizeLaunch_NoProfileWhenNotSandboxed(t *testing.T) {
	t.Run("pty template with sandbox off", func(t *testing.T) {
		store := newLaunchTestStore(t)
		proj := addLaunchTestProject(t, store, nil)
		result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), newLaunchTestLedger(t), LaunchRequest{
			Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "opencode",
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

	t.Run("ssh-hosted project", func(t *testing.T) {
		store := newLaunchTestStore(t)
		proj := addLaunchTestProject(t, store, func(p *config.Project) { p.HostID = "host-1" })
		if err := store.With(func(s *config.Settings) {
			s.Hosts = append(s.Hosts, config.Host{ID: "host-1", Name: "far", Target: "someone@far.local"})
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
