//go:build relaytest

package main

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

// seamsActive reports whether the test seams may act on configDir. They never
// act on the default config dir: a test build swapped in for the real app must
// behave as the real app does there. A path that does not resolve is treated as
// the default, so a failure to look never widens anything. The one exception
// is a default dir that does not exist: configDir exists and resolves, so it
// cannot be that dir, and a machine with no default dir (a CI runner) still
// never reaches the login keychain.
func seamsActive(configDir string) bool {
	dir, err := filepath.EvalSymlinks(configDir)
	if err != nil {
		return false
	}
	def, err := filepath.EvalSymlinks(bridge.DefaultConfigDir())
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	return dir != def
}
