package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/sealed"
)

// testSealKeyID/testSealKey are a fixed, non-secret AES-256 key used by
// every hermetic test that needs a working sealer — never the real login
// keychain, and never derived from anything random, so a failure
// reproduces byte for byte.
var (
	testSealKeyID = "0123456789abcdef"
	testSealKey   = bytes.Repeat([]byte{0x42}, 32)
)

// testSealer returns a Sealer over the fixed test key. It needs no *testing.T
// and no cleanup: it is pure in-memory AES-GCM, not a keychain item.
func testSealer() sealed.Sealer {
	s, err := sealed.NewAESSealer(testSealKeyID, testSealKey)
	if err != nil {
		panic(err)
	}
	return s
}

// sealedSettingsStoreAt is the hermetic-suite stand-in for the tray's own
// NewSettingsStoreSealed: a store that can actually write, backed by
// testSealer rather than the login keychain.
func sealedSettingsStoreAt(dir string) *FileSettingsStore {
	return NewSettingsStoreSealed(dir, testSealer())
}

// mkEmptySandboxRelayHome creates a fresh temp dir and points bridge's
// ConfigDir override plus HOME/XDG_CONFIG_HOME at it, so a test cannot
// touch the real user config directory even indirectly.
func mkEmptySandboxRelayHome(t *testing.T) string {
	t.Helper()
	dir := mkShortTempDir(t, "relay-home-empty-")
	applyOverride(t, dir)
	return dir
}

func applyOverride(t *testing.T, dir string) {
	t.Helper()
	bridge.SetConfigDirForTest(dir)
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Cleanup(func() { bridge.SetConfigDirForTest("") })
}

// mkShortTempDir creates a tempdir under /tmp (short paths) and registers
// cleanup. Use instead of t.TempDir() whenever the dir might hold a Unix
// socket — macOS caps sun_path at 104 chars.
func mkShortTempDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", prefix)
	if err != nil {
		t.Fatalf("mkShortTempDir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func sdSettingsPath(dir string) string { return filepath.Join(dir, "settings.json") }

func sdRead(t *testing.T, dir string) []byte {
	t.Helper()
	data, err := os.ReadFile(sdSettingsPath(dir))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	return data
}
