package main

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
)

// The claude-code template is the only one seeded, so home reaches the
// writable roots only by always being one.
func TestAuthorizeLaunch_RefusesClaudeReadGrantThroughLinkPlantedInHome(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write every directory, so no link is in a locked one")
	}
	noDeveloperTools(t)
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	home := sandboxRealPath(t, t.TempDir())
	t.Setenv("HOME", home)
	if err := store.With(func(s *config.Settings) {
		s.TerminalTemplates = slices.DeleteFunc(testTerminalTemplates(), func(tt config.TerminalTemplate) bool { return tt.ID != "claude-code" })
	}); err != nil {
		t.Fatalf("store.With: %v", err)
	}
	secret, share := filepath.Join(home, "secret"), filepath.Join(home, ".local", "share")
	for _, d := range []string{secret, share} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	link := filepath.Join(share, "claude")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	logs := captureWarnRecords(t)

	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), newLaunchTestLedger(t), LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindClaude,
	})
	if refusal == nil {
		t.Fatalf("launch %s was authorized with a read grant through %s", result.SessionID, link)
	}
	if refusal.Status != 400 || refusal.Code != "sandbox_unavailable" {
		t.Errorf("refusal = %d %q, want 400 sandbox_unavailable", refusal.Status, refusal.Code)
	}
	if want := `read grant "` + link + `" follows symlink "` + link + `"`; !strings.Contains(refusal.Message, want) {
		t.Errorf("refusal message %q does not name the grant and the link (%s)", refusal.Message, want)
	}
	if refusal.Audit.Event != audit.AuditEventSessionLaunch || refusal.Audit.Outcome != audit.AuditOutcomeError {
		t.Errorf("audit = %s/%s, want %s/%s", refusal.Audit.Event, refusal.Audit.Outcome, audit.AuditEventSessionLaunch, audit.AuditOutcomeError)
	}

	var warns []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var rec map[string]any
		if line != "" && json.Unmarshal([]byte(line), &rec) == nil {
			if msg, _ := rec["msg"].(string); strings.HasSuffix(msg, "read grant refused") {
				warns = append(warns, rec)
			}
		}
	}
	if len(warns) != 1 || warns[0]["level"] != "WARN" {
		t.Errorf("got %v, want exactly one WARN ending \"read grant refused\":\n%s", warns, logs.String())
	}

	entries, err := os.ReadDir(sessionProfilesDir())
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read profiles dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a refused launch left %d profile(s) behind", len(entries))
	}
}

func TestSandboxWritableRoots_Membership(t *testing.T) {
	mkSandboxRelayHome(t)
	t.Setenv("TMPDIR", t.TempDir())
	home, elsewhere := t.TempDir(), t.TempDir()
	at := func(name string) string { return filepath.Join(elsewhere, name) }
	settings := &config.Settings{
		TerminalTemplates: []config.TerminalTemplate{
			{ID: "shell", Name: "Shell", ReadWrite: []string{"~/acme-shell"}},
			{ID: "boxed", Name: "Boxed", Sandbox: ptr(true), ReadWrite: []string{at("boxed")}, Read: []string{"~/read-only"}},
			{ID: "plain", Name: "Plain", Sandbox: ptr(false), ReadWrite: []string{"~/plain"}},
			{ID: "claude-code", Name: "Claude Code", Sandbox: ptr(false), ReadWrite: []string{"~/.claude"}},
			{ID: "pi", Name: "pi", Sandbox: ptr(false), ReadWrite: []string{"~/.pi"}},
			{ID: "chat", Name: "Chat", Sandbox: ptr(false), ReadWrite: []string{at("chat")}},
		},
		Projects: []config.Project{
			{ID: "p1", Name: "Local", Path: at("local")},
			{ID: "p2", Name: "Remote", Kind: config.ProjectKindRemote, Path: at("remote")},
			{ID: "p3", Name: "Hosted", HostID: "devbox", Path: at("hosted")},
		},
	}

	roots, err := sandboxWritableRoots(settings, home)
	if err != nil {
		t.Fatalf("sandboxWritableRoots: %v", err)
	}
	var got []string
	for _, r := range roots {
		got = append(got, filepath.Clean(r))
	}

	want := []string{home, os.TempDir(), "/dev", sessionPiSessionsDir(),
		filepath.Join(home, "acme-shell"), at("boxed"), filepath.Join(home, ".claude"), filepath.Join(home, ".pi"), at("chat"), at("local")}
	if darwin := darwinUserTempDir(); darwin != "" {
		want = append(want, darwin)
	}
	for _, w := range want {
		if !slices.Contains(got, filepath.Clean(w)) {
			t.Errorf("writable roots lack %s\ngot %v", w, got)
		}
	}
	for _, absent := range []string{filepath.Join(home, "plain"), filepath.Join(home, "read-only"), at("remote"), at("hosted")} {
		if slices.Contains(got, absent) {
			t.Errorf("writable roots hold %s\ngot %v", absent, got)
		}
	}
}

// A template the launch path drops, here for a relative read_write entry, is
// dropped from W with it: its valid entries too, since the launch path drops
// the whole template.
func TestSandboxWritableRoots_TakeTheLaunchPathsTemplates(t *testing.T) {
	noDeveloperTools(t)
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	home := sandboxRealPath(t, t.TempDir())
	t.Setenv("HOME", home)
	if err := store.With(func(s *config.Settings) {
		s.TerminalTemplates = append(s.TerminalTemplates, config.TerminalTemplate{
			ID: "broken", Name: "Broken", Sandbox: ptr(true), ReadWrite: []string{"~/broken-sibling", "relative/dir"},
		})
	}); err != nil {
		t.Fatalf("store.With: %v", err)
	}
	settings := store.Get()

	roots, err := sandboxWritableRoots(settings, home)
	if err != nil {
		t.Fatalf("sandboxWritableRoots: %v", err)
	}
	checked := 0
	for _, tmpl := range config.EffectiveTerminalTemplates(settings) {
		if !tmpl.Sandboxed() && !slices.Contains(slices.Collect(maps.Values(kindTemplateIDs)), tmpl.ID) {
			continue
		}
		for _, e := range tmpl.ReadWrite {
			want := filepath.Clean(e)
			if e == "~" || strings.HasPrefix(e, "~/") {
				want = filepath.Join(home, e[1:])
			}
			if !slices.Contains(roots, want) {
				t.Errorf("writable roots lack %s from template %q\ngot %v", want, tmpl.ID, roots)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no kept template had a read_write entry to check")
	}
	for _, dropped := range []string{"relative/dir", filepath.Join(home, "broken-sibling")} {
		if slices.Contains(roots, dropped) {
			t.Errorf("writable roots hold %s from a template the launch path drops", dropped)
		}
	}

	launchWithSandbox(t, LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindClaude}, store)
}

func TestAuthorizeLaunch_RefusesWritableRootThroughSymlink(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write every directory, so no link is in a locked one")
	}
	mkdir := func(t *testing.T, d string) {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	link := func(t *testing.T, target, l string) {
		if err := os.Symlink(target, l); err != nil {
			t.Fatalf("symlink: %v", err)
		}
	}
	for _, tc := range []struct {
		name string
		// setup returns the template's read_write entries, the project path
		// ("" keeps the default), and the refused entry and link ("" when the
		// launch must proceed).
		setup func(t *testing.T, home string) (rw []string, projPath, entry, via string)
	}{
		{"read_write entry with a symlinked intermediate component", func(t *testing.T, home string) ([]string, string, string, string) {
			mkdir(t, filepath.Join(home, "real", "proj"))
			link(t, filepath.Join(home, "real"), filepath.Join(home, "work"))
			entry := filepath.Join(home, "work", "proj")
			return []string{entry}, "", entry, filepath.Join(home, "work")
		}},
		{"read_write entry that is itself a symlink", func(t *testing.T, home string) ([]string, string, string, string) {
			mkdir(t, filepath.Join(home, "real"))
			entry := filepath.Join(home, "proj")
			link(t, filepath.Join(home, "real"), entry)
			return []string{entry}, "", entry, entry
		}},
		{"read_write entry through a root-owned link that is not /tmp, /var or /etc", func(t *testing.T, home string) ([]string, string, string, string) {
			if fi, err := os.Lstat("/private/var/select/sh"); err != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Skip("no /var/select/sh link on this machine")
			}
			return []string{"/var/select/sh"}, "", "/var/select/sh", "/private/var/select/sh"
		}},
		{"project path that is a symlink", func(t *testing.T, home string) ([]string, string, string, string) {
			mkdir(t, filepath.Join(home, "real-proj"))
			entry := filepath.Join(home, "acme")
			link(t, filepath.Join(home, "real-proj"), entry)
			return nil, entry, entry, entry
		}},
		{"read_write entry through /tmp", func(t *testing.T, home string) ([]string, string, string, string) {
			dir, err := os.MkdirTemp("/tmp", "relay-rw-root-")
			if err != nil {
				t.Fatalf("temp dir: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			return []string{dir}, "", "", ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noDeveloperTools(t)
			store := newLaunchTestStore(t)
			home := sandboxRealPath(t, t.TempDir())
			t.Setenv("HOME", home)
			rw, projPath, entry, via := tc.setup(t, home)
			proj := addLaunchTestProject(t, store, func(p *config.Project) {
				if projPath != "" {
					p.Path = projPath
				}
			})
			if err := store.With(func(s *config.Settings) {
				s.TerminalTemplates = append(s.TerminalTemplates, config.TerminalTemplate{
					ID: "acme", Name: "Acme", Sandbox: ptr(true), ReadWrite: rw,
				})
			}); err != nil {
				t.Fatalf("store.With: %v", err)
			}
			req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "acme"}
			if entry == "" {
				launchWithSandbox(t, req, store)
				return
			}
			logs := captureWarnRecords(t)

			result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), newLaunchTestLedger(t), req)
			if refusal == nil {
				t.Fatalf("launch %s was authorized with a writable root %s through %s", result.SessionID, entry, via)
			}
			if refusal.Status != 400 || refusal.Code != "sandbox_unavailable" {
				t.Errorf("refusal = %d %q, want 400 sandbox_unavailable", refusal.Status, refusal.Code)
			}
			if want := `grant "` + entry + `" follows symlink "` + via + `"; use the real path`; !strings.Contains(refusal.Message, want) {
				t.Errorf("refusal message %q lacks %s", refusal.Message, want)
			}
			if refusal.Audit.Event != audit.AuditEventSessionLaunch || refusal.Audit.Outcome != audit.AuditOutcomeError {
				t.Errorf("audit = %s/%s, want %s/%s", refusal.Audit.Event, refusal.Audit.Outcome, audit.AuditEventSessionLaunch, audit.AuditOutcomeError)
			}
			if n := len(strings.Split(strings.TrimSpace(logs.String()), "\n")); logs.String() == "" || n != 1 {
				t.Errorf("want exactly one Warn, got:\n%s", logs.String())
			}
			entries, err := os.ReadDir(sessionProfilesDir())
			if err != nil && !os.IsNotExist(err) {
				t.Fatalf("read profiles dir: %v", err)
			}
			if len(entries) != 0 {
				t.Errorf("a refused launch left %d profile(s) behind", len(entries))
			}
		})
	}
}

// A home two levels below `/` makes `~/../..` the whole filesystem, which no
// launch can grant. The home need not exist.
func TestSandboxWritableRoots_SkipsAnEntryNoLaunchCanGrant(t *testing.T) {
	noDeveloperTools(t)
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	t.Setenv("HOME", "/nonexistent-relay-test/devbox")
	if err := store.With(func(s *config.Settings) {
		s.TerminalTemplates = append(s.TerminalTemplates, config.TerminalTemplate{
			ID: "acme", Name: "Acme", Sandbox: ptr(true), ReadWrite: []string{"~/../.."},
		})
	}); err != nil {
		t.Fatalf("store.With: %v", err)
	}
	logs := captureWarnRecords(t)

	launchWithSandbox(t, LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindClaude}, store)

	named := 0
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "acme") && strings.Contains(line, "~/../..") {
			named++
		}
	}
	if named == 0 {
		t.Errorf("no Warn names template acme and entry ~/../..:\n%s", logs.String())
	}
}
