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
	// SupervisionStatuses reports the restart-campaign state of every
	// service relay is currently supervising (running/restarting/failed).
	// An id absent from the result is not supervised: never started this
	// session, or the operator stopped it.
	SupervisionStatuses() map[string]SupervisionStatus
}

var _ Manager = (*Registry)(nil)

type serviceProcess struct {
	cmd       *exec.Cmd
	logFile   io.WriteCloser
	done      chan struct{}
	launch    *Launch
	startedAt time.Time
	// sup is the supervisor of record at the moment this process was
	// spawned. The exit goroutine hands it to superviseExit rather than
	// looking id up again, so a newer supervisor installed after this
	// process started (a fresh explicit Start) is never mistaken for this
	// one's owner.
	sup *serviceSupervisor
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

	// Clock is the source of "now" and of the cancellable delay restart
	// backoff sleeps on. Nil means the real wall clock; a supervision test
	// injects a FakeClock so a crash loop can be driven to its give-up limit
	// without waiting out real backoff delays.
	Clock Clock

	// HelperVerifier gates config.RelaySessionsServiceID's own on-disk
	// binary before it is ever spawned (SP3, R-S9): startLocked refuses to
	// start that one id if VerifyStatic fails, and starts every other
	// service exactly as before. nil skips the check -- main wires the real
	// darwin implementation; a hermetic test wires a stub.
	HelperVerifier HelperVerifier

	svMu        sync.Mutex
	supervisors map[string]*serviceSupervisor
}

// NewRegistry creates a new service registry.
func NewRegistry() *Registry {
	return &Registry{
		processes:   make(map[string]*serviceProcess),
		supervisors: make(map[string]*serviceSupervisor),
	}
}

// Start spawns the service through the platform shell so the user's profile
// (PATH, env) is loaded, and installs it as the id's supervisor of record:
// from here on, an exit relay did not request (Stop/Reload/StopAll) is
// restarted through this same path (docs/service-manifest.md's lifecycle
// section).
func (r *Registry) Start(cfg *config.ServiceConfig) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid service config: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.isRunningLocked(cfg.ID) {
		return nil
	}

	sup := r.installSupervisor(cfg)
	if _, err := r.startLocked(cfg, sup); err != nil {
		// Nothing was ever spawned, so there will never be an exit event to
		// drive a restart -- an idle "running" supervisor left behind here
		// would be a state IsRunning already contradicts.
		r.retireSupervisor(sup)
		return err
	}
	return nil
}

// startLocked does the actual spawn; the caller holds r.mu and has already
// decided sup is the process's supervisor. Both Start (an operator's first
// launch) and restartAfterBackoff (a supervised relaunch) call this, so a
// crash and an explicit Start begin an identical launch -- BuildCommand,
// launch identity, log file, pidfile, reaper -- and both mint a brand new
// launch secret (Launches.Begin ends any prior launch under the same name).
func (r *Registry) startLocked(cfg *config.ServiceConfig, sup *serviceSupervisor) (*serviceProcess, error) {
	cmd, err := BuildCommand(cfg)
	if err != nil {
		return nil, fmt.Errorf("build command for %q: %w", cfg.ID, err)
	}

	// SP3/R-S9: the one service whose on-disk binary must be proven to be
	// exactly the one relay shipped before it is ever spawned -- it is the
	// one process that receives project-session secrets. Every other
	// service starts exactly as before HelperVerifier existed.
	if cfg.ID == config.RelaySessionsServiceID && r.HelperVerifier != nil {
		if err := r.HelperVerifier.VerifyStatic(cfg.Command); err != nil {
			slog.Error("relay-sessions helper failed its static code-identity check; refusing to start it",
				"id", cfg.ID, "command", cfg.Command, "error", err)
			return nil, fmt.Errorf("helper code identity: %w", err)
		}
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

	// The frontend socket path goes only to a service holding the frontend
	// capability; to any other it would be a door its identity cannot open.
	if r.FrontendEnv != nil && cfg.HasCapability(config.ServiceCapabilityFrontend) {
		env, err := r.FrontendEnv()
		if err != nil {
			return nil, fmt.Errorf("provision frontend channel for %s: %w", cfg.ID, err)
		}
		MergeEnv(cmd, env)
	}

	var launch *Launch
	var launchRead *os.File
	if r.Launches != nil {
		launch, launchRead, err = r.beginLaunch(cfg)
		if err != nil {
			return nil, err
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
		return nil, fmt.Errorf("start %q: no log destination configured", cfg.ID)
	}
	// Assigning an io.Writer (not *os.File) makes Go pump the child's merged
	// stdout+stderr through one copy goroutine, which cmd.Wait awaits before
	// the reaper closes the writer below.
	logFile, err := r.OpenLog(cfg.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to create log file: %w", err)
	}

	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("failed to start '%s': %w", cfg.DisplayName, err)
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
		startedAt: r.clock().Now(),
		sup:       sup,
	}

	// Defers run LIFO. Textual order below is (last to run first):
	// superviseExit -> OnProcessExit -> close(done) -> logFile.Close ->
	// removePidFile -> endSessionsUnderParent -> launch.End -> Enhanced.Forget.
	// superviseExit is registered FIRST so it runs LAST: any restart it
	// schedules begins after the launch identity is cleared, the pidfile
	// removed and the log closed, i.e. after a completely fresh launch has
	// become possible. endSessionsUnderParent is registered BEFORE launch.End
	// so it runs AFTER it: relaysessions' own launch identity is gone from
	// Launches before its children are swept, narrowing the window in which
	// a new project_session could register with this now-dead launch as its
	// parent and never get cleaned up.
	serviceID := cfg.ID
	go func() {
		var exitCode int
		startedAt := proc.startedAt
		defer func() {
			r.superviseExit(sup, exitCode, r.clock().Now().Sub(startedAt))
		}()
		defer func() {
			if r.OnProcessExit != nil {
				r.OnProcessExit()
			}
		}()
		defer close(proc.done)
		defer func() { _ = logFile.Close() }()
		defer removePidFile(serviceID)
		if serviceID == config.RelaySessionsServiceID {
			// SH §4.4, §6: every project_session identity relaysessions
			// parented dies with it, on ANY exit of this process -- a
			// crash-restart, an operator Stop, Reload's Stop half, or
			// StopAll -- not only the supervised-restart case, since a
			// clean stop leaves exactly the same orphan risk if nothing
			// signals the process groups those sessions' shims led.
			defer r.endSessionsUnderParent(serviceID)
		}
		defer proc.launch.End()
		if r.Enhanced != nil {
			defer r.Enhanced.Forget(serviceID)
		}
		waitErr := cmd.Wait()
		if waitErr != nil {
			slog.Warn("service exited with error", "id", serviceID, "error", waitErr)
		}
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		} else {
			exitCode = -1
		}
	}()

	r.processes[cfg.ID] = proc
	return proc, nil
}

// beginLaunch records a launch for cfg and returns the read end of a pipe
// already holding its secret and closed for writing, so the child reads the
// secret to EOF and nothing else can ever be written behind it.
func (r *Registry) beginLaunch(cfg *config.ServiceConfig) (*Launch, *os.File, error) {
	secret, launch, err := r.Launches.Begin(Identity{
		Kind:         IdentityKindService,
		Name:         cfg.ID,
		Capabilities: cfg.Capabilities,
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

// endSessionsUnderParent is R-S9's cleanup for a relaysessions launch that
// just ended, whatever caused the exit: every project_session identity it
// parented is gone the instant its own launch ends (EndByParent), but the
// shim and target processes those launches named have no way to learn
// that on their own -- SIGKILL to each root's process group is what stops
// them from running on, orphaned, believed-dead by relay (spec-session-
// host.md §4.4, §6). EndByParent's own doc comment is explicit that
// Launches keeps no process-group bookkeeping, so this registry is where
// that pairing happens, and where each root is re-validated against the
// live process table immediately before it is signalled -- the launch's
// exit watch is gone the moment its identity ends, so nothing else stops
// its pid from being recycled onto an unrelated process group in the
// meantime.
func (r *Registry) endSessionsUnderParent(name string) {
	if r.Launches == nil {
		return
	}
	ended, roots := r.Launches.EndByParent(name)
	if ended > 0 {
		slog.Info("relaysessions launch ended; ending its project sessions and killing their process groups",
			"parent", name, "sessions", ended, "roots", len(roots))
	}
	for _, root := range roots {
		if !r.Launches.RootStillAlive(root.PID, root.StartSec, root.StartUsec) {
			slog.Warn("relaysessions root pid no longer matches its bound start time; skipping kill to avoid signalling a recycled pid",
				"parent", name, "pid", root.PID)
			continue
		}
		KillProcessGroupPID(root.PID)
	}
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
//
// stopSupervision runs before anything else: whichever of this and a
// concurrent crash's exit reaches the supervisor-of-record check first, this
// wins (its own doc comment has the full argument), so a service the
// operator stops is never restarted underneath them.
func (r *Registry) Stop(id string) {
	r.stopSupervision(id)

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

// Reload is Stop then Start in place: Stop already retires id's supervisor
// and cancels any pending restart before this returns, so the crash path (if
// the old process happened to be mid-backoff) never fires a second, redundant
// relaunch alongside this one.
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
// doesn't block the others. stopAllSupervision runs first and unconditionally,
// so tray shutdown cancels every pending or in-flight restart before a
// single process is killed -- nothing relaunches partway through teardown.
func (r *Registry) StopAll() {
	r.stopAllSupervision()

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
