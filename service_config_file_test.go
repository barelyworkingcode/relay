package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"relaygo/bridge"
)

// resolveConfigPath is the single security gate for the config editor. These
// tests pin its containment guarantees: a regular file within the allowed root
// resolves; anything that escapes the root (directly or via symlink), is not a
// regular file, or is oversize is rejected.

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestResolveConfigPath_HappyPath(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "settings.json")
	writeFile(t, cfg, "{}")

	got, err := resolveConfigPath(&bridge.ConfigDecl{Path: cfg}, root)
	if err != nil {
		t.Fatalf("resolveConfigPath: %v", err)
	}
	// EvalSymlinks may canonicalize /var → /private/var on macOS; compare resolved.
	want, _ := filepath.EvalSymlinks(cfg)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveConfigPath_RejectsRelative(t *testing.T) {
	_, err := resolveConfigPath(&bridge.ConfigDecl{Path: "settings.json"}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("want absolute-path error, got %v", err)
	}
}

func TestResolveConfigPath_RejectsOutsideRoot(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	cfg := filepath.Join(other, "settings.json")
	writeFile(t, cfg, "{}")

	_, err := resolveConfigPath(&bridge.ConfigDecl{Path: cfg}, root)
	if err == nil || !strings.Contains(err.Error(), "escapes allowed root") {
		t.Errorf("want escape error, got %v", err)
	}
}

func TestResolveConfigPath_RejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.json")
	writeFile(t, target, `{"secret":true}`)

	// A symlink INSIDE the root pointing at a file OUTSIDE it must be caught
	// by the EvalSymlinks + containment check.
	link := filepath.Join(root, "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	_, err := resolveConfigPath(&bridge.ConfigDecl{Path: link}, root)
	if err == nil || !strings.Contains(err.Error(), "escapes allowed root") {
		t.Errorf("symlink escape should be rejected, got %v", err)
	}
}

func TestResolveConfigPath_RejectsDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "adir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, err := resolveConfigPath(&bridge.ConfigDecl{Path: dir}, root)
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("want not-a-regular-file error, got %v", err)
	}
}

func TestResolveConfigPath_RejectsOversize(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "big.json")
	big := make([]byte, maxConfigFileBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	if err := os.WriteFile(cfg, big, 0o600); err != nil {
		t.Fatalf("write big: %v", err)
	}
	_, err := resolveConfigPath(&bridge.ConfigDecl{Path: cfg}, root)
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Errorf("want size-cap error, got %v", err)
	}
}

func TestResolveConfigPath_DefaultsToDeclaredDir(t *testing.T) {
	// With no explicit allowedRoot, the config file's own directory is the
	// root — so a plain regular file resolves out of the box.
	dir := t.TempDir()
	cfg := filepath.Join(dir, "settings.json")
	writeFile(t, cfg, "{}")

	got, err := resolveConfigPath(&bridge.ConfigDecl{Path: cfg}, "")
	if err != nil {
		t.Fatalf("resolveConfigPath with default root: %v", err)
	}
	want, _ := filepath.EvalSymlinks(cfg)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveConfigPath_DefaultRootStillRejectsSymlinkEscape(t *testing.T) {
	// Even with the default (declared-dir) root, a symlink whose target is
	// outside that dir must be rejected.
	dir := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.json")
	writeFile(t, target, "{}")
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	_, err := resolveConfigPath(&bridge.ConfigDecl{Path: link}, "")
	if err == nil || !strings.Contains(err.Error(), "escapes allowed root") {
		t.Errorf("default-root symlink escape should be rejected, got %v", err)
	}
}

func TestValidateConfigText_JSONCAndJSON(t *testing.T) {
	jsoncWithComments := "{\n  // a comment\n  \"a\": 1, /* inline */ \"b\": [1,2]\n}"
	if err := validateConfigText([]byte(jsoncWithComments), bridge.ConfigFormatJSONC); err != nil {
		t.Errorf("jsonc with comments should validate: %v", err)
	}
	// Default format is jsonc.
	if err := validateConfigText([]byte(jsoncWithComments), ""); err != nil {
		t.Errorf("default (jsonc) should validate: %v", err)
	}
	// Strict json must reject comments.
	if err := validateConfigText([]byte(jsoncWithComments), bridge.ConfigFormatJSON); err == nil {
		t.Errorf("strict json should reject comments")
	}
	// Malformed is rejected in either mode.
	if err := validateConfigText([]byte(`{"a":}`), bridge.ConfigFormatJSONC); err == nil {
		t.Errorf("malformed jsonc should be rejected")
	}
}

func TestWriteConfigFile_RoundTripPreservesBytes(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "settings.json")
	// Comments + key order must survive verbatim — relay writes the original
	// bytes, never a re-marshal.
	original := "{\n  // keep me\n  \"z\": 1,\n  \"a\": 2\n}\n"
	writeFile(t, cfg, original)

	real, err := resolveConfigPath(&bridge.ConfigDecl{Path: cfg}, root)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	edited := "{\n  // keep me\n  \"z\": 9,\n  \"a\": 2\n}\n"
	if err := writeConfigFile(real, []byte(edited), configFilePerm(real)); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != edited {
		t.Errorf("round-trip mismatch:\n got  %q\n want %q", string(got), edited)
	}
	// No leftover temp file.
	if _, err := os.Stat(cfg + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file should not remain: %v", err)
	}
	// Mode preserved (0600).
	info, _ := os.Stat(cfg)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perm widened: got %v, want 0600", info.Mode().Perm())
	}
}

func TestWriteConfigFile_RejectsOversize(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "settings.json")
	writeFile(t, cfg, "{}")
	big := make([]byte, maxConfigFileBytes+1)
	if err := writeConfigFile(cfg, big, 0o600); err == nil {
		t.Errorf("oversize write should be refused")
	}
}
