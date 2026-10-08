//go:build !testapprover

package main

import "github.com/barelyworkingcode/relay/internal/presence"

func newPresenceProvider() presence.Provider {
	return presence.NewLocalAuthProvider()
}
