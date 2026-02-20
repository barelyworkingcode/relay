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
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}
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

// CheckAll returns running status for each given service ID.
func (r *ServiceRegistry) CheckAll(ids []string) map[string]bool {
	result := make(map[string]bool, len(ids))
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range ids {
		result[id] = r.isRunningLocked(id)
	}
	return result
}
