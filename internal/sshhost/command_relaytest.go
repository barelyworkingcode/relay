//go:build relaytest

package sshhost

import "sync/atomic"

var testCommand atomic.Pointer[string]

// SetTestCommand makes every ssh_argv start with path. An empty path restores
// "ssh".
func SetTestCommand(path string) {
	if path == "" {
		testCommand.Store(nil)
		return
	}
	testCommand.Store(&path)
}

func sshCommand() string {
	if p := testCommand.Load(); p != nil {
		return *p
	}
	return "ssh"
}
