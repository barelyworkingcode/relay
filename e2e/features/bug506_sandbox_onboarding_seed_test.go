package features

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// A fresh folder gets relay's own fixed .claude.json, never a copy.
func TestBug506SandboxClaudeDirOnboardingSeeded(t *testing.T) {
	t.Parallel()
	i, pid := b181Start(t, nil)
	b181Fixture(t, i)
	b181SessionEnv(t, i, pid)

	w := b181Dir(i)
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
	if string(b) != b506Seed {
		t.Fatalf(".claude.json holds %q, want %q", b, b506Seed)
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
