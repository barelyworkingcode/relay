package features

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"relaye2e/harness"
)

const b506Seed = `{"hasCompletedOnboarding":true}`

// b506Plant makes the sandbox Claude folder.
func b506Plant(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating the folder: %v", err)
	}
}

// b506NoSeedTemp fails when a seed temp file is left in the folder.
func b506NoSeedTemp(t *testing.T, dir string) {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the folder: %v", err)
	}
	for _, e := range es {
		if strings.HasPrefix(e.Name(), ".claude.json.seed-") {
			t.Fatalf("the folder holds leftover %s", e.Name())
		}
	}
}

// b506Read returns the folder's .claude.json after checking it is a regular
// 0600 file.
func b506Read(t *testing.T, w string) []byte {
	t.Helper()
	p := filepath.Join(w, ".claude.json")
	st, err := os.Lstat(p)
	if err != nil {
		t.Fatalf("the folder's .claude.json: %v", err)
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0o600 {
		t.Fatalf(".claude.json is %v, want a regular file of mode 0600", st.Mode())
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("reading .claude.json: %v", err)
	}
	return b
}

// b506Projects decodes the seed and checks its two top-level keys. It returns
// the projects object.
func b506Projects(t *testing.T, b []byte) map[string]json.RawMessage {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatalf("decoding .claude.json %q: %v", b, err)
	}
	if len(top) != 2 {
		t.Fatalf(".claude.json holds %d top-level keys, want 2: %s", len(top), b)
	}
	if string(top["hasCompletedOnboarding"]) != "true" {
		t.Fatalf("hasCompletedOnboarding is %s, want true", top["hasCompletedOnboarding"])
	}
	var projects map[string]json.RawMessage
	if err := json.Unmarshal(top["projects"], &projects); err != nil {
		t.Fatalf("decoding projects %s: %v", top["projects"], err)
	}
	return projects
}

// b506WantTrust fails unless projects holds exactly one entry, keyed by want,
// whose value is exactly {"hasTrustDialogAccepted":true}.
func b506WantTrust(t *testing.T, projects map[string]json.RawMessage, want string) {
	t.Helper()
	if len(projects) != 1 {
		t.Fatalf("projects holds %d keys, want exactly one: %v", len(projects), projects)
	}
	v, ok := projects[want]
	if !ok {
		t.Fatalf("projects has no key %q: %v", want, projects)
	}
	var got map[string]any
	if err := json.Unmarshal(v, &got); err != nil {
		t.Fatalf("decoding the trust entry %s: %v", v, err)
	}
	if len(got) != 1 || got["hasTrustDialogAccepted"] != true {
		t.Fatalf("trust entry is %s, want only hasTrustDialogAccepted=true", v)
	}
}

// A fresh folder gets relay's own .claude.json, never a copy: onboarding plus
// one trust entry for the resolved launch directory.
func TestBug506SandboxClaudeDirOnboardingSeeded(t *testing.T) {
	t.Parallel()
	i, pid := b181Start(t, nil)
	b181Fixture(t, i)
	b181SessionEnv(t, i, pid)

	w := b181Dir(i)
	b := b506Read(t, w)
	if !bytes.HasPrefix(b, []byte(`{"hasCompletedOnboarding":true,"projects":{`)) {
		t.Fatalf(".claude.json starts %q", b)
	}
	if bytes.HasSuffix(b, []byte("\n")) {
		t.Fatalf(".claude.json ends with a newline")
	}
	proj := filepath.Join(i.Home, "work", "acme-updater")
	real, err := filepath.EvalSymlinks(proj)
	if err != nil {
		t.Fatalf("resolving %s: %v", proj, err)
	}
	projects := b506Projects(t, b)
	b506WantTrust(t, projects, real)
	if real != proj {
		if _, ok := projects[proj]; ok {
			t.Fatalf("projects is keyed by the unresolved path %q", proj)
		}
	}
	if _, err := os.Lstat(filepath.Join(w, ".credentials.json")); err == nil {
		t.Fatalf("the folder holds .credentials.json, want it absent")
	}
	es, err := os.ReadDir(w)
	if err != nil {
		t.Fatalf("reading the folder: %v", err)
	}
	for _, e := range es {
		if e.Type().IsRegular() {
			c, _ := os.ReadFile(filepath.Join(w, e.Name()))
			if bytes.Contains(c, []byte("acme-placeholder")) {
				t.Fatalf("%s holds bytes of a fixture file", e.Name())
			}
		}
	}
}

// A launch directory reached through a link is trusted by its resolved path
// only. The sandbox refuses a project path that is itself a link, so the link
// is a folder inside the project, named by the terminal's directory.
func TestBug506SandboxClaudeDirTrustsResolvedLink(t *testing.T) {
	t.Parallel()
	i, pid := b181Start(t, nil)
	proj := filepath.Join(i.Home, "work", "acme-updater")
	realDir := filepath.Join(proj, "acme-real")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", realDir, err)
	}
	link := filepath.Join(proj, "acme-link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatalf("linking: %v", err)
	}
	var term struct {
		TerminalID string `json:"terminalId"`
	}
	i.MustCLI("terminal", "start", "--project", pid, "--template", "claude-code", "--directory", link, "--json").JSON(t, &term)
	if term.TerminalID == "" {
		t.Fatalf("terminal start printed no terminalId")
	}

	want, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatalf("resolving %s: %v", realDir, err)
	}
	projects := b506Projects(t, b506Read(t, b181Dir(i)))
	b506WantTrust(t, projects, want)
	for k := range projects {
		if strings.Contains(k, "acme-link") {
			t.Fatalf("projects key %q holds the link name", k)
		}
	}
}

// A project at the home folder gets the onboarding key and no trust entry.
func TestBug506SandboxClaudeDirHomeNotTrusted(t *testing.T) {
	t.Parallel()
	i, _ := b181Start(t, nil)
	raw, err := json.Marshal(map[string]any{"name": "acme-home", "path": i.Home, "allowed_templates": []string{"claude-code"}})
	if err != nil {
		t.Fatalf("encoding the project: %v", err)
	}
	res := i.CLIWith(harness.CLIOpts{Stdin: raw}, "project", "create", "--file", "-", "--json")
	if res.Code != 0 {
		t.Fatalf("project create exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	var p struct {
		ID string `json:"id"`
	}
	res.JSON(t, &p)
	if p.ID == "" {
		t.Fatalf("project create printed no id: %s", res.Stdout)
	}
	g1sLaunchClaude(t, i, p.ID)
	if b := b506Read(t, b181Dir(i)); string(b) != b506Seed {
		t.Fatalf(".claude.json holds %q, want %q", b, b506Seed)
	}
}

// A .claude.json the folder already holds keeps its bytes and mode.
func TestBug506SandboxClaudeDirKeepsOwnClaudeJSON(t *testing.T) {
	t.Parallel()
	i, pid := b181Start(t, nil)
	b181Fixture(t, i)
	w := b181Dir(i)
	b506Plant(t, w)
	p := filepath.Join(w, ".claude.json")
	if err := os.WriteFile(p, []byte("acme-own"), 0o644); err != nil {
		t.Fatalf("planting .claude.json: %v", err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	for round := 1; round <= 2; round++ {
		g1sLaunchClaude(t, i, pid)
		st, err := os.Lstat(p)
		if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0o644 {
			t.Fatalf("launch %d: .claude.json is %v (%v), want a regular file of mode 0644", round, st, err)
		}
		if b, _ := os.ReadFile(p); string(b) != "acme-own" {
			t.Fatalf("launch %d: .claude.json holds %q, want acme-own", round, b)
		}
		b506NoSeedTemp(t, w)
	}
}

// A .claude.json that is a link stays that link; its target is not written.
func TestBug506SandboxClaudeDirKeepsLinkedClaudeJSON(t *testing.T) {
	t.Parallel()
	i, pid := b181Start(t, nil)
	b181Fixture(t, i)
	w := b181Dir(i)
	b506Plant(t, w)
	target := filepath.Join(i.Home, "acme-elsewhere.json")
	if err := os.WriteFile(target, []byte("acme-own"), 0o600); err != nil {
		t.Fatalf("writing the target: %v", err)
	}
	p := filepath.Join(w, ".claude.json")
	if err := os.Symlink(target, p); err != nil {
		t.Fatalf("planting the link: %v", err)
	}
	g1sLaunchClaude(t, i, pid)
	got, err := os.Readlink(p)
	if err != nil {
		t.Fatalf(".claude.json is no longer a link: %v", err)
	}
	if got != target {
		t.Fatalf(".claude.json points at %q, want %q", got, target)
	}
	if b, _ := os.ReadFile(target); string(b) != "acme-own" {
		t.Fatalf("the link target holds %q, want acme-own", b)
	}
	b506NoSeedTemp(t, w)
}
