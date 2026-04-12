package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"relaygo/bridge"
)

// LLMChannel holds the shared bearer token and Unix socket path the
// orchestrator uses to bind Eve and relayLLM together.
//
// Both processes are spawned by the orchestrator, and both must agree on a
// credential pair before they can talk to each other. The orchestrator is the
// trusted issuer: it generates a fresh token + allocates a socket path on
// first need, then injects the SAME pair into both processes via environment
// variables at spawn time. Neither credential ever touches disk.
//
// The channel is per-orchestrator-lifetime: created lazily, persists across
// individual service restarts (so an Eve restart can rejoin a relayLLM that's
// still running), and torn down when the orchestrator exits.
//
// This is intentionally separate from the existing RELAY_MCP_TOKEN bridge
// channel — see relay/CLAUDE.md "Eve ↔ relayLLM internal channel" and
// eve/plans/cozy-honking-toast.md Section B for the rationale. Multiplexing
// the two would mean a leaked credential in one channel grants access to the
// other.
type LLMChannel struct {
	mu         sync.Mutex
	token      string
	socketPath string
}

// NewLLMChannel returns an empty (uninitialized) channel. Credentials are
// generated on the first Ensure() call.
func NewLLMChannel() *LLMChannel { return &LLMChannel{} }

// Ensure returns the bearer token and Unix socket path, generating them on
// first call. Safe to call concurrently from multiple Start() goroutines.
func (c *LLMChannel) Ensure() (token, socketPath string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.token != "" && c.socketPath != "" {
		return c.token, c.socketPath, nil
	}

	// The socket lives next to the existing bridge.sock so it inherits the
	// same 0700 parent directory and is easy to find from the relay config
	// dir if an operator needs to inspect it.
	dir := bridge.ConfigDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("create relay config dir: %w", err)
	}

	socketPath = filepath.Join(dir, fmt.Sprintf("relay-llm-%d.sock", os.Getpid()))
	// Remove any stale socket from a previous orchestrator instance whose
	// PID happened to be reused. Best-effort.
	_ = os.Remove(socketPath)

	c.token = generateRandomHex(32)
	c.socketPath = socketPath

	slog.Info("provisioned llm channel", "socket", socketPath)
	return c.token, c.socketPath, nil
}

// Close unlinks the socket file. Called from orchestrator cleanup. The token
// is left in the struct (the orchestrator process is exiting anyway) so any
// late spawn attempt during shutdown still finds a usable value rather than
// regenerating it; the file removal is the operative cleanup.
func (c *LLMChannel) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.socketPath == "" {
		return
	}
	if err := os.Remove(c.socketPath); err != nil && !os.IsNotExist(err) {
		slog.Warn("failed to unlink llm socket", "path", c.socketPath, "error", err)
	}
}

// participatesInLLMChannel reports whether a service should receive
// RELAY_LLM_TOKEN and RELAY_LLM_SOCKET at spawn time.
//
// Match is on the slugified service ID — registering with `relay service
// register --name Eve` produces id "eve", `--name relayLLM` produces "relayllm",
// `--name relay-LLM` produces "relay-llm". A new participant must be added
// here explicitly so the credential surface stays bounded.
func participatesInLLMChannel(id string) bool {
	switch id {
	case "eve", "relayllm", "relay-llm":
		return true
	default:
		return false
	}
}
