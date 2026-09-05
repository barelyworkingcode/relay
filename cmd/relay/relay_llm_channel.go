package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"syscall"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/service"
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
	// A crashed or force-quit relay never runs its own Close(), so its
	// relay-frontend-<pid>.sock lingers here across restarts (observed:
	// three stale ones beside the live one). Swept once per Ensure, not
	// just at Close, so a long-uptime tray with several prior crashes
	// still cleans them up on its next start rather than only after its
	// own graceful exit.
	pruneStaleFrontendSockets(dir)

	token, err := service.GenerateRandomHex(32)
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

// frontendSocketNameRe matches this process's own naming scheme
// (relay-frontend-<pid>.sock) so pruneStaleFrontendSockets never touches an
// unrelated file that happens to live in the same directory.
var frontendSocketNameRe = regexp.MustCompile(`^relay-frontend-(\d+)\.sock$`)

// pruneStaleFrontendSockets removes relay-frontend-<pid>.sock files left
// behind by a relay instance that never reached its own Close() (a crash or
// a force-quit). A file is removed ONLY when its embedded pid names no
// running process right now (processAlive false) -- never one that is
// alive, even if that process turns out to be something else entirely
// (pid reuse): "no process has this pid" is the one fact this is allowed to
// act on, which is also why it never needs to special-case this process's
// own socket name -- processAlive(os.Getpid()) is always true.
func pruneStaleFrontendSockets(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		m := frontendSocketNameRe.FindStringSubmatch(entry.Name())
		if m == nil {
			continue
		}
		pid, err := strconv.Atoi(m[1])
		if err != nil || pid <= 0 || processAlive(pid) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			slog.Warn("failed to unlink stale frontend socket", "path", path, "error", err)
			continue
		}
		slog.Info("removed stale frontend socket left by a process that is no longer running", "path", path, "pid", pid)
	}
}

// processAlive reports whether pid names a live process via the standard
// "send signal 0" probe: no signal is actually delivered, but the kernel
// still performs the existence/permission check. EPERM means the pid
// exists but is owned by another user -- still alive, just not something
// this process could signal for real.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
