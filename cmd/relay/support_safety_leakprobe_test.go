//go:build leakprobe

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

// TestLeakProbe_WritesDefaultConfigDir leaks on purpose: it passes, and
// TestMain must turn the package red with SANDBOX VIOLATION.
func TestLeakProbe_WritesDefaultConfigDir(t *testing.T) {
	bridge.SetConfigDirForTest("")
	dir := bridge.ConfigDir()
	// This is deliberate: a plain string prefix, never EvalSymlinks, so the
	// probe can only ever write inside the isolated suite HOME.
	if suiteHome == "" || !strings.HasPrefix(dir, suiteHome+"/") {
		t.Fatalf("config dir %q is not under the isolated suite HOME %q; refusing to write", dir, suiteHome)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
}
