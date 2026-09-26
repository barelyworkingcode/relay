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
