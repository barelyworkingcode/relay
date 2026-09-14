package service

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
)

// Environment variable names for services (from bridge package).
const (
	EnvBridgeSocket = bridge.EnvBridgeSocket
	EnvServiceID    = bridge.EnvServiceID
	EnvMcpCommand   = bridge.EnvMcpCommand
	EnvLaunchFD     = bridge.EnvLaunchFD
)

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
	// Runtime reports pid + start time for every running service, keyed by
	// id. The Services tab's "pid 21093 · up 2h 14m" line reads this instead
	// of pairing PIDsByServiceID with a separate start-time map that could
	// drift from it under a concurrent Stop/Start.
	Runtime() map[string]ServiceRuntime
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
	launch    *Launch
	startedAt time.Time
}

// ServiceRuntime is the process-identity half of a running service's status
// that outlives any single poll: a pid and a start time, neither of which
// PollStatuses' manifest round-trip carries.
type ServiceRuntime struct {
	PID       int
	StartedAt time.Time
}

// Registry manages background service processes and their lifecycle.
type Registry struct {
	mu        sync.Mutex
	processes map[string]*serviceProcess

	// Launches, FrontendEnv, OpenLog, Enhanced, and OnProcessExit are all
	// set once during initialization, before any services are started, so
	// concurrent reads from reaper goroutines need no lock of their own.
	//
	// Launches is where every start records its launch secret and where Hello
	// binds it (docs/launch-identity.md). Nil launches services with no launch
	// fd, so they can never obtain an identity.
	Launches *Launches
	// FrontendEnv provisions the frontend socket and returns the environment
	// variables a frontend-consuming service needs. Nil means no frontend
	// channel is wired up. None of it is a credential: a consumer reaches the
	// socket by its launch identity. A
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

	// Relay's own environment is what the child inherits, so a relay started
	// from a shell that carries any of these must not pass them on.
	ScrubEnv(cmd, append([]string{EnvLaunchFD, bridge.EnvFrontendSocket}, bridge.RemovedCredentialEnv...)...)

	relayBin, _ := os.Executable()
	relayBin, _ = filepath.EvalSymlinks(relayBin)
	MergeEnv(cmd, map[string]string{
		EnvBridgeSocket: bridge.SocketPath(),
		EnvServiceID:    cfg.ID,
		EnvMcpCommand:   relayBin,
	})

	// The frontend socket path goes only to frontend consumers (e.g. eve);
	// a backend has no frontend identity, so the path would be of no use to it.
	if r.FrontendEnv != nil && frontendCredsEnabled(cfg) {
		env, err := r.FrontendEnv()
		if err != nil {
			return fmt.Errorf("provision frontend channel for %s: %w", cfg.ID, err)
		}
		MergeEnv(cmd, env)
	}

	var launch *Launch
	var launchRead *os.File
	if r.Launches != nil {
		launch, launchRead, err = r.beginLaunch(cfg)
		if err != nil {
			return err
		}
		cmd.ExtraFiles = []*os.File{launchRead}
		MergeEnv(cmd, map[string]string{EnvLaunchFD: strconv.Itoa(bridge.LaunchFD)})
	}

	committed := false
	defer func() {
		// The child holds its own copy of the read end from cmd.Start on;
		// relay's copy is closed on every path so the pipe's only reader is
		// the child.
		if launchRead != nil {
			_ = launchRead.Close()
		}
		if !committed {
			launch.End()
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
		_ = logFile.Close()
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
		launch:    launch,
		startedAt: time.Now(),
	}

	// Defers run LIFO: launch.End -> logFile.Close -> close(done) ->
	// OnProcessExit, so the identity is gone before Stop, which waits on
	// done, returns, and done is closed before the exit callback reads
	// process state.
	serviceID := cfg.ID
	go func() {
		defer func() {
			if r.OnProcessExit != nil {
				r.OnProcessExit()
			}
		}()
		defer close(proc.done)
		defer func() { _ = logFile.Close() }()
		defer removePidFile(serviceID)
		defer proc.launch.End()
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

// beginLaunch records a launch for cfg and returns the read end of a pipe
// already holding its secret and closed for writing, so the child reads the
// secret to EOF and nothing else can ever be written behind it.
func (r *Registry) beginLaunch(cfg *config.ServiceConfig) (*Launch, *os.File, error) {
	secret, launch, err := r.Launches.Begin(Identity{
		Kind:             IdentityKindService,
		Name:             cfg.ID,
		FrontendConsumer: frontendCredsEnabled(cfg),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("begin launch for %q: %w", cfg.ID, err)
	}
	read, write, err := os.Pipe()
	if err != nil {
		launch.End()
		return nil, nil, fmt.Errorf("launch pipe for %q: %w", cfg.ID, err)
	}
	// This is deliberate: 64 bytes is below PIPE_BUF, so the write completes
	// without a reader and cannot block Start while holding r.mu.
	_, writeErr := write.WriteString(secret)
	closeErr := write.Close()
	if writeErr != nil || closeErr != nil {
		_ = read.Close()
		launch.End()
		return nil, nil, fmt.Errorf("write launch secret for %q: %w", cfg.ID, errors.Join(writeErr, closeErr))
	}
	return launch, read, nil
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

func (r *Registry) Runtime() map[string]ServiceRuntime {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]ServiceRuntime, len(r.processes))
	for id, proc := range r.processes {
		if !r.isRunningLocked(id) {
			continue
		}
		if proc.cmd == nil || proc.cmd.Process == nil {
			continue
		}
		out[id] = ServiceRuntime{PID: proc.cmd.Process.Pid, StartedAt: proc.startedAt}
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
