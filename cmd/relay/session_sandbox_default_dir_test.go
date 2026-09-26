package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/sessions/sandbox"
)

// defaultRelayDirUnderTempHome points HOME at a fresh temp dir and returns
// the relay dir the OS would pick under it. Deliberate: the sandbox relay
// home sets HOME to the override dir itself; a separate HOME keeps the
// default dir outside the override, as it is in production.
func defaultRelayDirUnderTempHome(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	appSupport, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("UserConfigDir: %v", err)
	}
	return filepath.Clean(filepath.Join(appSupport, "relay"))
}

func claudeSpecForTest(t *testing.T) sandbox.Spec {
	t.Helper()
	noDeveloperTools(t)
	spec, err := sandboxSpecForLaunch(&config.Settings{}, nil, t.TempDir(), KindClaude, nil)
	if err != nil {
		t.Fatalf("sandboxSpecForLaunch: %v", err)
	}
	return spec
}

func countCleaned(paths []string, want string) int {
	n := 0
	for _, p := range paths {
		if filepath.Clean(p) == want {
			n++
		}
	}
	return n
}

func TestSandboxSpec_ConfigDirOverrideAlsoDeniesDefaultDir(t *testing.T) {
	mkEmptySandboxRelayHome(t)
	defaultDir := defaultRelayDirUnderTempHome(t)
	if filepath.Clean(bridge.ConfigDir()) == defaultDir {
		t.Fatalf("setup: override %q equals the default dir", bridge.ConfigDir())
	}

	spec := claudeSpecForTest(t)

	if got := countCleaned(spec.UnixConnectDenyDirs, defaultDir); got != 1 {
		t.Errorf("default relay dir %q appears %d times in UnixConnectDenyDirs, want 1\ndeny dirs: %q",
			defaultDir, got, spec.UnixConnectDenyDirs)
	}
	for _, allow := range spec.UnixConnectAllow {
		if strings.HasPrefix(filepath.Clean(allow), defaultDir+string(filepath.Separator)) {
			t.Errorf("allow literal %q re-opens a socket under the default relay dir %q", allow, defaultDir)
		}
	}
}

func TestSandboxSpec_OverrideEqualToDefaultDirIsDeniedOnce(t *testing.T) {
	mkEmptySandboxRelayHome(t)
	defaultDir := defaultRelayDirUnderTempHome(t)
	if err := os.MkdirAll(defaultDir, 0o700); err != nil {
		t.Fatalf("mkdir default dir: %v", err)
	}
	bridge.SetConfigDirForTest(defaultDir)

	spec := claudeSpecForTest(t)

	if got := countCleaned(spec.UnixConnectDenyDirs, defaultDir); got != 1 {
		t.Errorf("relay dir %q appears %d times in UnixConnectDenyDirs, want 1\ndeny dirs: %q",
			defaultDir, got, spec.UnixConnectDenyDirs)
	}
}
