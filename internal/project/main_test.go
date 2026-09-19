package project

import (
	"fmt"
	"os"
	"testing"
)

// TestMain points HOME at a throwaway directory for the whole package, so a
// test that builds a settings store (rooted at bridge.ConfigDir) or resolves
// the home directory cannot touch the real
// ~/Library/Application Support/relay.
//
// This is deliberate rather than per-test: the packages of a `go test ./...`
// run in parallel, and cmd/relay's sandbox guard watches the real directory
// for the whole run, so it cannot say which package escaped.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "project-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "project tests: create sandbox home:", err)
		os.Exit(1)
	}
	os.Setenv("HOME", home)
	os.Setenv("XDG_CONFIG_HOME", home+"/.config")
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
