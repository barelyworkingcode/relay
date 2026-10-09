//go:build relaytest

package main

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/presence/testapprover"
)

// testApprover is the one approver newPresenceProvider built, kept so
// withTestCallerSession can read the same outcome file. Set once per serve.
var testApprover atomic.Pointer[testapprover.Approver]

func newPresenceProvider(configDir string) presence.Provider {
	active := seamsActive(configDir)
	if active {
		slog.Warn("test build: presence prompts are answered by test-presence.json, not a person", "config_dir", configDir)
	} else {
		slog.Warn("test build on the default config dir: presence prompts are answered by a program, not a person; only project.grant is approved")
	}
	a := testapprover.New(configDir, active)
	testApprover.Store(a)
	return a
}

// withTestCallerSession lets the outcome file stand in for the caller's
// console-session fact, which the kernel cannot give a test run over SSH or in
// CI. It only supplies the fact: Gate.Request still applies the refusal rule
// to it, so a caller with no console session is refused as before.
func withTestCallerSession(ctx context.Context) context.Context {
	a := testApprover.Load()
	if a == nil {
		return ctx
	}
	console, ok := a.ConsoleSession()
	if !ok {
		return ctx
	}
	return presence.WithCallerSession(ctx, presence.CallerSession{GraphicAccess: console})
}
