package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

// Owner-only 0600 in a 0700 parent dir; the token is defense-in-depth on top
// of the FS permissions.
type Endpoint struct {
	Socket string
	Token  string
}

type FrontendChannel struct {
	mu       sync.Mutex
	endpoint Endpoint
	ready    bool
}

// Re-exported aliases of the canonical names declared in the bridge package
// so existing call sites keep their import paths stable.
const (
	EnvFrontendSocket     = bridge.EnvFrontendSocket
	EnvFrontendToken      = bridge.EnvFrontendToken
	EnvBridgeSocket       = bridge.EnvBridgeSocket
	EnvServiceID          = bridge.EnvServiceID
	EnvServiceToken       = bridge.EnvServiceToken
	EnvServiceTokenLegacy = bridge.EnvServiceTokenLegacy
	EnvMcpCommand         = bridge.EnvMcpCommand
)

func NewFrontendChannel() *FrontendChannel { return &FrontendChannel{} }

func (c *FrontendChannel) Ensure() (Endpoint, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.ready {
		return c.endpoint, nil
	}

	dir := bridge.ConfigDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Endpoint{}, fmt.Errorf("create relay config dir: %w", err)
	}

	token, err := generateRandomHex(32)
	if err != nil {
		return Endpoint{}, fmt.Errorf("generate frontend token: %w", err)
	}
	pid := os.Getpid()
	c.endpoint = Endpoint{
		Socket: filepath.Join(dir, fmt.Sprintf("relay-frontend-%d.sock", pid)),
		Token:  token,
	}
	// Best-effort cleanup of a stale socket from a previous orchestrator
	// instance whose PID happened to be reused.
	_ = os.Remove(c.endpoint.Socket)

	c.ready = true
	slog.Info("provisioned frontend channel", "socket", c.endpoint.Socket)
	return c.endpoint, nil
}

func (c *FrontendChannel) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.endpoint.Socket == "" {
		return
	}
	if err := os.Remove(c.endpoint.Socket); err != nil && !os.IsNotExist(err) {
		slog.Warn("failed to unlink frontend socket", "path", c.endpoint.Socket, "error", err)
	}
}

func (e Endpoint) FrontendEnv() map[string]string {
	return map[string]string{
		EnvFrontendSocket: e.Socket,
		EnvFrontendToken:  e.Token,
	}
}
