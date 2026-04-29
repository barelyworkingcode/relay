package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"relaygo/bridge"
)

// Endpoint pairs a Unix socket path with a bearer token. Both sockets are
// 0600 in a 0700 parent dir; the token is defense-in-depth.
type Endpoint struct {
	Socket string
	Token  string
}

// LLMChannelCreds holds the credentials the orchestrator hands out to the
// processes wired into the front-door channel.
//
//	Eve / relayScheduler  ──→  Frontend (relay binds)  ──→  Internal (relayLLM binds)
type LLMChannelCreds struct {
	Frontend Endpoint
	Internal Endpoint
}

// Env vars injected into spawned services. Constants prevent silent typo
// drift across the four-process surface.
const (
	EnvFrontendSocket = "RELAY_FRONTEND_SOCKET"
	EnvFrontendToken  = "RELAY_FRONTEND_TOKEN"
	EnvInternalSocket = "RELAY_LLM_INTERNAL_SOCKET"
	EnvInternalToken  = "RELAY_LLM_INTERNAL_TOKEN"
)

// LLMChannel lazily provisions LLMChannelCreds on first Ensure() call and
// holds them for the orchestrator's lifetime. Safe for concurrent use.
type LLMChannel struct {
	mu    sync.Mutex
	creds LLMChannelCreds
	ready bool
}

func NewLLMChannel() *LLMChannel { return &LLMChannel{} }

// Ensure returns the credential set, generating it on first call.
func (c *LLMChannel) Ensure() (LLMChannelCreds, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.ready {
		return c.creds, nil
	}

	dir := bridge.ConfigDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return LLMChannelCreds{}, fmt.Errorf("create relay config dir: %w", err)
	}

	pid := os.Getpid()
	c.creds = LLMChannelCreds{
		Frontend: Endpoint{
			Socket: filepath.Join(dir, fmt.Sprintf("relay-frontend-%d.sock", pid)),
			Token:  generateRandomHex(32),
		},
		Internal: Endpoint{
			Socket: filepath.Join(dir, fmt.Sprintf("relay-llm-%d.sock", pid)),
			Token:  generateRandomHex(32),
		},
	}
	// Best-effort cleanup of stale sockets from a previous orchestrator
	// instance whose PID happened to be reused.
	_ = os.Remove(c.creds.Frontend.Socket)
	_ = os.Remove(c.creds.Internal.Socket)

	c.ready = true
	slog.Info("provisioned llm channel",
		"frontend_socket", c.creds.Frontend.Socket,
		"internal_socket", c.creds.Internal.Socket)
	return c.creds, nil
}

// Close unlinks both socket files. Tokens persist in-memory until the
// process exits.
func (c *LLMChannel) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range []string{c.creds.Frontend.Socket, c.creds.Internal.Socket} {
		if p == "" {
			continue
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			slog.Warn("failed to unlink llm channel socket", "path", p, "error", err)
		}
	}
}

// EnvFor returns the env vars to inject into a spawned service. Empty map
// means the service is not part of the channel. Match is on slugified ID.
// New participants must be added explicitly so the credential surface
// stays bounded.
func (c LLMChannelCreds) EnvFor(serviceID string) map[string]string {
	switch serviceID {
	case "relayllm", "relay-llm":
		return map[string]string{
			EnvInternalSocket: c.Internal.Socket,
			EnvInternalToken:  c.Internal.Token,
		}
	case "eve", "relayscheduler", "relay-scheduler":
		return map[string]string{
			EnvFrontendSocket: c.Frontend.Socket,
			EnvFrontendToken:  c.Frontend.Token,
		}
	}
	return nil
}
