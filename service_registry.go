package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// ServiceRegistry manages background service child processes.
type ServiceRegistry struct {
	mu        sync.Mutex
	processes map[string]*exec.Cmd
}

// NewServiceRegistry creates an empty registry.
func NewServiceRegistry() *ServiceRegistry {
	return &ServiceRegistry{
		processes: make(map[string]*exec.Cmd),
	}
}

func serviceLogDir() string {
	dir := filepath.Join(settingsDir(), "logs")
	_ = os.MkdirAll(dir, 0755)
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

	r.processes[config.ID] = cmd
	return nil
}

// Stop kills a service process and waits for it to exit.
func (r *ServiceRegistry) Stop(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if cmd, ok := r.processes[id]; ok {
		delete(r.processes, id)
		killProcessGroup(cmd)
		_ = cmd.Wait()
	}
}

// Reload restarts a running service with new config. If the service is not
// running, this is a no-op (does not auto-start).
func (r *ServiceRegistry) Reload(id string, cfg *ServiceConfig) error {
	r.mu.Lock()
	wasRunning := r.isRunningLocked(id)
	r.mu.Unlock()

	if !wasRunning {
		return nil
	}

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
	cmd, ok := r.processes[id]
	if !ok {
		return false
	}
	// ProcessState is nil if Wait hasn't been called and process hasn't exited.
	if cmd.ProcessState != nil {
		delete(r.processes, id)
		return false
	}
	if cmd.Process != nil {
		if !processAlive(cmd.Process.Pid) {
			delete(r.processes, id)
			return false
		}
	}
	return true
}

// StartAllAutostart starts all services with autostart enabled.
func (r *ServiceRegistry) StartAllAutostart(configs []ServiceConfig) {
	for i := range configs {
		if configs[i].Autostart {
			if err := r.Start(&configs[i]); err != nil {
				fmt.Fprintf(os.Stderr, "service autostart: %v\n", err)
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
		r.isRunningLocked(id)
	}
}
