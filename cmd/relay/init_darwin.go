//go:build darwin

package main

import "runtime"

func init() {
	// macOS requires all UI work on the main thread.
	runtime.LockOSThread()
}
