//go:build !darwin

package main

// ProcessNames is a no-op outside darwin, matching bridge.PeerPID: relay's tray
// host is macOS-only, and an unknown caller process is recorded as empty rather
// than treated as an error.
func ProcessNames(int) (proc, parent string) { return "", "" }
