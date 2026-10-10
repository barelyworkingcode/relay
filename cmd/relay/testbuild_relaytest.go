//go:build relaytest

package main

import (
	"os"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

// seamsActive reports whether the test seams may act on configDir. They never
// act on the default config dir: a test build swapped in for the real app must
// behave as the real app does there. Identity is the file, not the spelling:
// the default volume is case-insensitive, so two paths that differ in case are
// one dir. A configDir that does not stat is never trusted. Any failure to stat
// the default dir leaves the seams on, deliberately: configDir stats and the
// default does not, so they cannot be the same dir, and a machine with no
// default dir (a CI runner) must still never reach the login keychain.
func seamsActive(configDir string) bool {
	dir, err := os.Stat(configDir)
	if err != nil {
		return false
	}
	def, err := os.Stat(bridge.DefaultConfigDir())
	if err != nil {
		return true
	}
	return !os.SameFile(def, dir)
}
