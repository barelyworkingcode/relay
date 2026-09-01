package service

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
)

// Environment variable names for services (from bridge package).
const (
	EnvBridgeSocket       = bridge.EnvBridgeSocket
	EnvServiceID          = bridge.EnvServiceID
	EnvServiceToken       = bridge.EnvServiceToken
	EnvServiceTokenLegacy = bridge.EnvServiceTokenLegacy
	EnvMcpCommand         = bridge.EnvMcpCommand
)

// TokenStore registers and tracks service authentication tokens.
type TokenStore interface {
	Register(hash string)
	Remove(hash string)
}

// EnhancedRegistry tracks registered services for dispatch.
type EnhancedRegistry interface {
	Forget(serviceID string)
}

// Manager manages the lifecycle of background services.
type Manager interface {
	Start(cfg *config.ServiceConfig) error
	Stop(id string)
	Reload(id string, cfg *config.ServiceConfig) error
	IsRunning(id string) bool
	RunningIDs() []string
	PIDsByServiceID() map[string]int
	CleanupDead()
	ReclaimOrphans(configs []config.ServiceConfig)
	StartAllAutostart(configs []config.ServiceConfig)
	StopAll()
}

var _ Manager = (*Registry)(nil)

type serviceProcess struct {
	cmd       *exec.Cmd
	logFile   io.WriteCloser
	done      chan struct{}
	tokenHash string
}

// Registry manages background service processes and their lifecycle.
type Registry struct {
	mu        sync.Mutex
	processes map[string]*serviceProcess

	// TokenStore, FrontendEnv, OpenLog, Enhanced, and OnProcessExit are all
	// set once during initialization, before any services are started, so
	// concurrent reads from reaper goroutines need no lock of their own.
	TokenStore TokenStore
	// FrontendEnv provisions the frontend socket/token and returns the
	// environment variables a frontend-consuming service needs. Nil means no
	// frontend channel is wired up (no consumer will get credentials). A
	// callback rather than a manager type: the registry has no business
	// depending on the frontend channel's concrete type or owning its
	// lifecycle -- main provisions it, main closes it, the registry only
	// needs the env it produces.
	FrontendEnv func() (map[string]string, error)
	// OpenLog opens the log destination for a spawned service's merged
	// stdout+stderr, keyed by service id. A callback rather than a concrete
	// writer type: log rotation is general-purpose infrastructure shared
	// with relay's own log and the audit log, so it is main's to own.
	OpenLog       func(id string) (io.WriteCloser, error)
	Enhanced      EnhancedRegistry
	OnProcessExit func()
}

// NewRegistry creates a new service registry.
func NewRegistry() *Registry {
	return &Registry{
		processes: make(map[string]*serviceProcess),
	}
}

// Start spawns the service through the platform shell so the user's profile
// (PATH, env) is loaded.
func (r *Registry) Start(cfg *config.ServiceConfig) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid service config: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.isRunningLocked(cfg.ID) {
		return nil
	}

	cmd, err := BuildCommand(cfg)
	if err != nil {
		return fmt.Errorf("build command for %q: %w", cfg.ID, err)
	}

	var tokenHash string
	if r.TokenStore != nil {
		rawToken, err := GenerateRandomHex(32)
		if err != nil {
			return fmt.Errorf("generate service token for %q: %w", cfg.ID, err)
		}
		tokenHash = config.HashToken(rawToken)
		r.TokenStore.Register(tokenHash)

		relayBin, _ := os.Executable()
		relayBin, _ = filepath.EvalSymlinks(relayBin)

		MergeEnv(cmd, map[string]string{
			EnvServiceToken: rawToken,
			// Transition: also set the legacy name so an un-migrated service
			// (older relayLLM) still authenticates. Drop after relayLLM ships
			// the rename.
			EnvServiceTokenLegacy: rawToken,
			EnvMcpCommand:         relayBin,
		})
	}

	// Frontend creds (RELAY_FRONTEND_SOCKET/TOKEN) go only to frontend
	// consumers (e.g. eve); backends never dial the front door, and handing
	// them the bearer would leak it into any process they spawn.
	if r.FrontendEnv != nil && frontendCredsEnabled(cfg) {
		env, err := r.FrontendEnv()
		if err != nil {
			return fmt.Errorf("provision frontend channel for %s: %w", cfg.ID, err)
		}
		MergeEnv(cmd, env)
	}
	MergeEnv(cmd, map[string]string{
		EnvBridgeSocket: bridge.SocketPath(),
		EnvServiceID:    cfg.ID,
	})

	committed := false
	defer func() {
		if !committed && tokenHash != "" {
			r.TokenStore.Remove(tokenHash)
		}
	}()

	if r.OpenLog == nil {
		return fmt.Errorf("start %q: no log destination configured", cfg.ID)
	}
	// Assigning an io.Writer (not *os.File) makes Go pump the child's merged
	// stdout+stderr through one copy goroutine, which cmd.Wait awaits before
	// the reaper closes the writer below.
	logFile, err := r.OpenLog(cfg.ID)
	if err != nil {
		return fmt.Errorf("failed to create log file: %w", err)
	}

	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("failed to start '%s': %w", cfg.DisplayName, err)
	}
	committed = true

	// Best-effort: pidfile failure must not abort a successful spawn.
	if err := writePidFile(cfg.ID, cmd.Process.Pid); err != nil {
		slog.Warn("write pidfile failed", "id", cfg.ID, "error", err)
	}

	proc := &serviceProcess{
		cmd:       cmd,
		logFile:   logFile,
		done:      make(chan struct{}),
		tokenHash: tokenHash,
	}

	// Defers run LIFO: logFile.Close -> close(done) -> OnProcessExit,
	// ensuring done is closed before the exit callback reads process state.
	serviceID := cfg.ID
	go func() {
		defer func() {
			if r.OnProcessExit != nil {
				r.OnProcessExit()
			}
		}()
		defer close(proc.done)
		defer logFile.Close()
		defer removePidFile(serviceID)
		if proc.tokenHash != "" && r.TokenStore != nil {
			defer r.TokenStore.Remove(proc.tokenHash)
		}
		if r.Enhanced != nil {
			defer r.Enhanced.Forget(serviceID)
		}
		if err := cmd.Wait(); err != nil {
			slog.Warn("service exited with error", "id", serviceID, "error", err)
		}
	}()

	r.processes[cfg.ID] = proc
	return nil
}

func frontendCredsEnabled(cfg *config.ServiceConfig) bool {
	return cfg.FrontendConsumer == nil || *cfg.FrontendConsumer
}

// GenerateRandomHex returns a random hex string, or an error rather than a
// zero/partial token if the CSPRNG read fails -- a token derived from a
// failed read would be predictable.
func GenerateRandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand failed: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Stop kills a service and waits for it to exit. The process stays in the
// map while stopping so IsRunning returns true, preventing a concurrent
// Start from spawning a duplicate.
func (r *Registry) Stop(id string) {
	r.mu.Lock()
	proc, ok := r.processes[id]
	r.mu.Unlock()

	if ok {
		KillProcessGroup(proc.cmd)
		<-proc.done

		r.mu.Lock()
		// Only delete if this is still the same process (not replaced by a new Start).
		if r.processes[id] == proc {
			delete(r.processes, id)
		}
		r.mu.Unlock()
	}
}

func (r *Registry) Reload(id string, cfg *config.ServiceConfig) error {
	r.Stop(id)
	return r.Start(cfg)
}

func (r *Registry) IsRunning(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.isRunningLocked(id)
}

// isRunningLocked also reaps: an exited process is removed from the map as
// a side effect. Caller must hold r.mu.
func (r *Registry) isRunningLocked(id string) bool {
	proc, ok := r.processes[id]
	if !ok {
		return false
	}
	select {
	case <-proc.done:
		delete(r.processes, id)
		return false
	default:
		return true
	}
}

// ReclaimOrphans terminates leftover service processes from a previous tray
// session: if the tray was SIGKILLed or force-quit, Start's reaper goroutine
// never ran, so children were reparented to launchd (PPID 1) and are still
// holding their listen ports, which fails the next autostart with "address
// already in use". Reads each service's pidfile, confirms the pid still
// belongs to that service (ps lookup, to defeat pid recycling), then SIGTERMs
// the process group. Stale pidfiles are silently removed.
func (r *Registry) ReclaimOrphans(configs []config.ServiceConfig) {
	for i := range configs {
		cfg := &configs[i]
		pid, err := readPidFile(cfg.ID)
		if err != nil {
			slog.Warn("read pidfile failed", "id", cfg.ID, "error", err)
			continue
		}
		if pid == 0 {
			continue
		}
		if ReclaimOrphan(pid, cfg.Command) {
			slog.Warn("reclaimed orphan service from previous session",
				"id", cfg.ID, "pid", pid)
		}
		removePidFile(cfg.ID)
	}
}

func (r *Registry) StartAllAutostart(configs []config.ServiceConfig) {
	for i := range configs {
		if configs[i].Autostart {
			if err := r.Start(&configs[i]); err != nil {
				slog.Error("service autostart failed", "error", err)
			}
		}
	}
}

// StopAll stops every running service concurrently so one slow shutdown
// doesn't block the others.
func (r *Registry) StopAll() {
	r.mu.Lock()
	procs := make(map[string]*serviceProcess, len(r.processes))
	for id, proc := range r.processes {
		procs[id] = proc
	}
	r.mu.Unlock()

	var wg sync.WaitGroup
	for _, proc := range procs {
		wg.Add(1)
		go func(p *serviceProcess) {
			defer wg.Done()
			KillProcessGroup(p.cmd)
			<-p.done
		}(proc)
	}
	wg.Wait()

	r.mu.Lock()
	for id, proc := range procs {
		if r.processes[id] == proc {
			delete(r.processes, id)
		}
	}
	r.mu.Unlock()
}

func (r *Registry) RunningIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.processes))
	for id := range r.processes {
		if r.isRunningLocked(id) {
			ids = append(ids, id)
		}
	}
	return ids
}

func (r *Registry) PIDsByServiceID() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int, len(r.processes))
	for id, proc := range r.processes {
		if !r.isRunningLocked(id) {
			continue
		}
		if proc.cmd == nil || proc.cmd.Process == nil {
			continue
		}
		out[id] = proc.cmd.Process.Pid
	}
	return out
}

func (r *Registry) CleanupDead() {
	r.mu.Lock()
	defer r.mu.Unlock()

	var dead []string
	for id, proc := range r.processes {
		select {
		case <-proc.done:
			dead = append(dead, id)
		default:
		}
	}
	for _, id := range dead {
		delete(r.processes, id)
		slog.Info("cleaned up dead service", "id", id)
	}
}
