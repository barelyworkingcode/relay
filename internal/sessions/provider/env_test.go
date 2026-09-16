package provider

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestChildBaseEnv_StripsRelaySecrets(t *testing.T) {
	for _, k := range relaySecretEnvKeys {
		t.Setenv(k, "leaked-value")
	}
	t.Setenv("HARMLESS_VAR", "kept")

	env := childBaseEnv()
	for _, e := range env {
		for _, k := range relaySecretEnvKeys {
			if strings.HasPrefix(e, k+"=") {
				t.Fatalf("childBaseEnv leaked %s: %s", k, e)
			}
		}
	}

	found := false
	for _, e := range env {
		if e == "HARMLESS_VAR=kept" {
			found = true
		}
	}
	if !found {
		t.Fatal("childBaseEnv dropped a non-secret var")
	}
}

func TestWriteSpawnFile_Mode0600(t *testing.T) {
	dir := t.TempDir()
	path, err := writeSpawnFile(dir, "test-*.json", []byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("writeSpawnFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 0600", info.Mode().Perm())
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("path %q not under %q", path, dir)
	}
}

func TestEnsurePath_AddsLocalBinWhenMissing(t *testing.T) {
	home, _ := os.UserHomeDir()
	localBin := filepath.Join(home, ".local", "bin")

	env := ensurePath([]string{"PATH=/usr/bin:/bin"})
	if !strings.Contains(env[0], localBin) {
		t.Fatalf("PATH not extended: %s", env[0])
	}
}

func TestEnsurePath_SetsMinimalPathWhenAbsent(t *testing.T) {
	env := ensurePath([]string{"OTHER=1"})
	found := false
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			found = true
		}
	}
	if !found {
		t.Fatal("no PATH set")
	}
}

// forbiddenSecretPatterns is the required hermetic-test shape from
// plan-broker-and-sessions.md's R-S7b row: no RELAY_*TOKEN, no
// RELAY_LLM_HOOK_TOKEN specifically (subsumed by the first), no bare 64-hex
// string, no rmk_-prefixed string, anywhere in a spawned child's env or argv.
var hex64Pattern = regexp.MustCompile(`\b[0-9a-fA-F]{64}\b`)

func assertNoForbiddenSecrets(t *testing.T, label string, entries []string) {
	t.Helper()
	for _, e := range entries {
		if strings.Contains(e, "RELAY_") && strings.Contains(e, "TOKEN") {
			t.Errorf("%s: forbidden RELAY_*TOKEN shape: %q", label, e)
		}
		if strings.Contains(e, "rmk_") {
			t.Errorf("%s: forbidden rmk_ model key shape: %q", label, e)
		}
		if hex64Pattern.MatchString(e) {
			t.Errorf("%s: forbidden 64-hex shape: %q", label, e)
		}
	}
}
