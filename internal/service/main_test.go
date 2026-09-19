package service

import (
	"fmt"
	"os"
	"testing"
)

// TestMain points HOME at a throwaway directory for the whole package, so a
// test that starts a Registry service (which writes a pidfile under
// bridge.ConfigDir) cannot reach the real ~/Library/Application Support/relay.
//
// This is deliberate rather than per-test: the packages of a `go test ./...`
// run in parallel, and cmd/relay's sandbox guard watches the real directory
// for the whole run, so a single test here that forgets withTempConfigDir
// creates the directory under it. Tests that call withTempConfigDir still get
// their own directory; t.Setenv layers over this one.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "service-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "service tests: create sandbox home:", err)
		os.Exit(1)
	}
	os.Setenv("HOME", home)
	os.Setenv("XDG_CONFIG_HOME", home+"/.config")
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
