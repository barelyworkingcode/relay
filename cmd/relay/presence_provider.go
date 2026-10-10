//go:build !relaytest

package main

import (
	"context"

	"github.com/barelyworkingcode/relay/internal/presence"
)

// newPresenceProvider ignores configDir: a release build has exactly one
// provider, the person at the console.
func newPresenceProvider(configDir string) presence.Provider {
	return presence.NewLocalAuthProvider()
}

// withTestCallerSession leaves ctx alone: a release build takes the caller's
// session from the kernel and from nowhere else.
func withTestCallerSession(ctx context.Context) context.Context { return ctx }
