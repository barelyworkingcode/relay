//go:build relaytest

package main

import (
	"path/filepath"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

// seamsActive reports whether the test seams may act on configDir. They never
// act on the default config dir: a test build swapped in for the real app must
// behave as the real app does there. A path that does not resolve is treated as
// the default, so a failure to look never widens anything.
func seamsActive(configDir string) bool {
	dir, err := filepath.EvalSymlinks(configDir)
	if err != nil {
		return false
	}
	def, err := filepath.EvalSymlinks(bridge.DefaultConfigDir())
	if err != nil {
		return false
	}
	return dir != def
}
