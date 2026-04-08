package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"relaygo/bridge"
)

// ServiceManager abstracts service lifecycle operations for testability.
type ServiceManager interface {
	Start(config *ServiceConfig) error
	Stop(id string)
	Reload(id string, cfg *ServiceConfig) error
	IsRunning(id string) bool
	RunningIDs() []string
	CleanupDead()
	StartAllAutostart(configs []ServiceConfig)
	StopAll()
}

// Compile-time interface assertions.
var _ ServiceManager = (*ServiceRegistry)(nil)

// serviceProcess bundles a running process with its log file for cleanup.
type serviceProcess struct {
	cmd       *exec.Cmd
	logFile   *os.File
	done      chan struct{} // closed when cmd.Wait() returns
	tokenHash string       // in-memory service token hash (empty if none)
}

// ServiceRegistry manages background service child processes.
type ServiceRegistry struct {
	mu        sync.Mutex
	processes map[string]*serviceProcess

	// TokenStore holds ephemeral in-memory tokens for managed services.
	// Set during initialization, before any services are started.
	TokenStore *serviceTokenStore

	// OnProcessExit is called from the reaper goroutine after a managed
	// process exits. Set once during initialization, before any services
	// are started, so concurrent reads from reaper goroutines are safe.
	// Enables event-driven UI updates (e.g., tray menu status dots)
	// without polling for process state changes.
	OnProcessExit func()
}

// NewServiceRegistry creates an empty registry.
func NewServiceRegistry() *ServiceRegistry {
	return &ServiceRegistry{
		processes: make(map[string]*serviceProcess),
	}
}

func serviceLogDir() (string, error) {
	dir := filepath.Join(bridge.ConfigDir(), "logs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create log directory: %w", err)
	}
	return dir, nil
}

// Start spawns a service through the platform shell so the user's profile is available.
// Stdout and stderr go to a log file.
func (r *ServiceRegistry) Start(config *ServiceConfig) error {
	if err := config.Validate(); err != nil {
		return fmt.Errorf("invalid service config: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.isRunningLocked(config.ID) {
		return nil
	}

	cmd := buildCommand(config)

	// Generate an ephemeral service token and inject Relay MCP env vars.
	var tokenHash string
	if r.TokenStore != nil {
		rawToken := generateRandomHex(32)
		tokenHash = hashToken(rawToken)
		r.TokenStore.Register(tokenHash)

		relayBin, _ := os.Executable()
		relayBin, _ = filepath.EvalSymlinks(relayBin)

		mergeEnv(cmd, map[string]string{
			"RELAY_MCP_TOKEN":   rawToken,
			"RELAY_MCP_COMMAND": relayBin,
		})
	}

	// Clean up the service token on any error path before the process starts.
	committed := false
	defer func() {
		if !committed && tokenHash != "" {
			r.TokenStore.Remove(tokenHash)
		}
	}()

	logDir, err := serviceLogDir()
	if err != nil {
		return err
	}
	logPath := filepath.Join(logDir, config.ID+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("failed to create log file: %w", err)
	}

	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("failed to start '%s': %w", config.DisplayName, err)
	}
	committed = true

	proc := &serviceProcess{
		cmd:       cmd,
		logFile:   logFile,
		done:      make(chan struct{}),
		tokenHash: tokenHash,
	}

	// Reap the process in the background so ProcessState is populated
	// and we can detect exit via the done channel. Defers run LIFO:
	// logFile.Close → close(done) → OnProcessExit, ensuring the done
	// channel is closed before the exit callback reads process state.
	go func() {
		defer func() {
			if r.OnProcessExit != nil {
				r.OnProcessExit()
			}
		}()
		defer close(proc.done)
		defer logFile.Close()
		// Clean up ephemeral token on exit.
		if proc.tokenHash != "" && r.TokenStore != nil {
			defer r.TokenStore.Remove(proc.tokenHash)
		}
		if err := cmd.Wait(); err != nil {
			slog.Warn("service exited with error", "id", config.ID, "error", err)
		}
	}()

	r.processes[config.ID] = proc
	return nil
}

// generateRandomHex returns a random hex string of the given byte length.
func generateRandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// Stop kills a service process and waits for it to exit.
// The process remains in the map while stopping so IsRunning returns true,
// preventing duplicate spawns from concurrent Start calls.
func (r *ServiceRegistry) Stop(id string) {
	r.mu.Lock()
	proc, ok := r.processes[id]
	r.mu.Unlock()

	if ok {
		killProcessGroup(proc.cmd)
		<-proc.done

		r.mu.Lock()
		// Only delete if this is still the same process (not replaced by a new Start).
		if r.processes[id] == proc {
			delete(r.processes, id)
		}
		r.mu.Unlock()
	}
}

// Reload restarts a service with new config. Stops the service if running,
// then starts it. Stop is a no-op for non-running services.
func (r *ServiceRegistry) Reload(id string, cfg *ServiceConfig) error {
	r.Stop(id)
	return r.Start(cfg)
}

// IsRunning checks whether a service process is still alive.
func (r *ServiceRegistry) IsRunning(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.isRunningLocked(id)
}

// isRunningLocked checks whether a process is still alive. If the process has
// exited, it is removed from the map as a side effect (reaping).
// Caller must hold r.mu.
func (r *ServiceRegistry) isRunningLocked(id string) bool {
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

// StartAllAutostart starts all services with autostart enabled.
func (r *ServiceRegistry) StartAllAutostart(configs []ServiceConfig) {
	for i := range configs {
		if configs[i].Autostart {
			if err := r.Start(&configs[i]); err != nil {
				slog.Error("service autostart failed", "error", err)
			}
		}
	}
}

// StopAll kills all running service processes concurrently to avoid one
// slow-to-stop service blocking the shutdown of others.
func (r *ServiceRegistry) StopAll() {
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
			killProcessGroup(p.cmd)
			<-p.done
		}(proc)
	}
	wg.Wait()

	// Clean up after all processes are dead.
	r.mu.Lock()
	for id, proc := range procs {
		if r.processes[id] == proc {
			delete(r.processes, id)
		}
	}
	r.mu.Unlock()
}

// RunningIDs returns the IDs of all currently running services.
func (r *ServiceRegistry) RunningIDs() []string {
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

// CleanupDead removes dead processes from the registry.
func (r *ServiceRegistry) CleanupDead() {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Collect dead IDs first to avoid deleting from the map during iteration.
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
