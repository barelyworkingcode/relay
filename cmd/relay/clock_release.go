//go:build !relaytest

package main

import "time"

func newServerClock(configDir string) serverClock { return wallClock{} }

// cliNow is the time a CLI verb judges against; a release build has only
// wall time.
func cliNow(command string) time.Time { return time.Now() }
