package project

// Headline rule, inherited from cmd/relay's own support_test.go: every test
// that touches settings MUST start with mkEmptySandboxRelayHome(t), which
// redirects bridge.ConfigDir() away from the operator's real one.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/sealed"
)

// testSealKeyID/testSealKey are a fixed, non-secret AES-256 key used by
// every hermetic test that needs a working sealer — never the real login
// keychain (headline rule above: no test may touch it), and never derived
// from anything random, so a failure reproduces byte for byte.
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
func sealedSettingsStoreAt(dir string) *config.FileSettingsStore {
	return config.NewSettingsStoreSealed(dir, testSealer())
}

func mkEmptySandboxRelayHome(t *testing.T) string {
	t.Helper()
	dir := mkShortTempDir(t, "relay-home-empty-")
	bridge.SetConfigDirForTest(dir)
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Cleanup(func() { bridge.SetConfigDirForTest("") })
	return dir
}

// mkShortTempDir creates a tempdir under /tmp (short paths) and registers
// cleanup. Use instead of t.TempDir() whenever the dir holds a Unix
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

func assertNoErr(t *testing.T, err error, format string, args ...any) {
	t.Helper()
	if err != nil {
		args = append(args, err)
		t.Fatalf(format+": %v", args...)
	}
}
