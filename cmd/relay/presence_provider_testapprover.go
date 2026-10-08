//go:build testapprover

package main

import (
	"log/slog"

	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/presence/testapprover"
)

func newPresenceProvider() presence.Provider {
	slog.Warn("test approver build: presence prompts are answered by a program, not a person; only project.grant is approved")
	return testapprover.New()
}
