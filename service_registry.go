package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// serviceProcess bundles a running process with its log file for cleanup.
type serviceProcess struct {
	cmd     *exec.Cmd
	logFile *os.File
	done    chan struct{} // closed when cmd.Wait() returns
}

// ServiceRegistry manages background service child processes.
type ServiceRegistry struct {
	mu        sync.Mutex
	processes map[string]*serviceProcess
}

// NewServiceRegistry creates an empty registry.
func NewServiceRegistry() *ServiceRegistry {
	return &ServiceRegistry{
		processes: make(map[string]*serviceProcess),
	}
}

func serviceLogDir() string {
	dir := filepath.Join(settingsDir(), "logs")
	_ = os.MkdirAll(dir, 0700)
	return dir
}

// Start spawns a service through the platform shell so the user's profile is available.
// Stdout and stderr go to a log file.
func (r *ServiceRegistry) Start(config *ServiceConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.isRunningLocked(config.ID) {
		return nil
	}

	cmd := buildCommand(config)

	logPath := filepath.Join(serviceLogDir(), config.ID+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("failed to create log file: %w", err)
	}

	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("failed to start '%s': %w", config.DisplayName, err)
	}

	proc := &serviceProcess{
		cmd:     cmd,
		logFile: logFile,
		done:    make(chan struct{}),
	}

	// Reap the process in the background so ProcessState is populated
	// and we can detect exit via the done channel.
	go func() {
		_ = cmd.Wait()
		logFile.Close()
		close(proc.done)
	}()

	r.processes[config.ID] = proc
	return nil
}

// Stop kills a service process and waits for it to exit.
func (r *ServiceRegistry) Stop(id string) {
	r.mu.Lock()
	proc, ok := r.processes[id]
	if ok {
		delete(r.processes, id)
	}
	r.mu.Unlock()

	if ok {
		killProcessGroup(proc.cmd)
		<-proc.done
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

// StopAll kills all running service processes.
func (r *ServiceRegistry) StopAll() {
	r.mu.Lock()
	ids := make([]string, 0, len(r.processes))
	for id := range r.processes {
		ids = append(ids, id)
	}
	r.mu.Unlock()

	for _, id := range ids {
		r.Stop(id)
	}
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
	for id := range r.processes {
		if !r.isRunningLocked(id) {
			slog.Info("cleaned up dead service", "id", id)
		}
	}
}
