package service

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
)

// Restart policy for a service relay is responsible for running (docs/service-manifest.md,
// CLAUDE.md's Security section). Vars, not consts, so a test can shorten them
// to exercise a full crash loop without waiting out real backoff; nothing in
// production writes them.
var (
	// The pause before the first restart attempt: a service that dies on
	// spawn would otherwise be relaunched in a tight loop for as long as the
	// attempt budget lasted.
	ServiceRestartBaseDelay = 1 * time.Second

	ServiceRestartMaxDelay = 60 * time.Second

	// Consecutive failures before relay gives up and marks the service
	// failed. Not a lifetime budget: a run that stays up
	// ServiceRestartStableWindow resets it (below), so this bounds restart
	// INTENSITY, not how many times relay will ever restart a service that
	// is basically healthy.
	ServiceRestartMaxAttempts = 5

	// Comfortably longer than a launch, Hello and a service's own startup
	// work, so a service that dies during startup can never read as stable.
	ServiceRestartStableWindow = 60 * time.Second
)

// ExitCodeLaunchIdentity is EX_CONFIG (sysexits.h). A service exits with this
// code when it could not establish its launch identity (docs/launch-identity.md)
// -- most often because its own launch secret was already spent trying to
// restart itself, the exact failure mode relay's supervision replaces.
// Counted as a failure like any other exit; logged distinctly so a launch-
// identity problem doesn't read as an ordinary crash in the service's log.
const ExitCodeLaunchIdentity = 78

// SupervisionPhase is what relay believes about a supervised service's
// restart campaign right now.
type SupervisionPhase string

const (
	SupervisionRunning    SupervisionPhase = "running"
	SupervisionRestarting SupervisionPhase = "restarting"
	SupervisionFailed     SupervisionPhase = "failed"
)

// SupervisionStatus is the read-only view the CLI and the Settings status
// surface render. A service id with no entry in Registry.SupervisionStatuses
// is not being supervised at all -- never started this session, or the
// operator stopped it -- and its running/stopped state comes from
// IsRunning/RunningIDs exactly as it always has.
type SupervisionStatus struct {
	Phase SupervisionPhase `json:"phase"`
	// Attempt is the restart attempt this campaign is on (or last reached
	// before giving up); 0 while Running and never restarted.
	Attempt int `json:"attempt"`
	// NextAttempt is when a scheduled restart is due; zero unless Phase ==
	// SupervisionRestarting.
	NextAttempt  time.Time `json:"next_attempt,omitempty"`
	LastExitCode int       `json:"last_exit_code,omitempty"`
	HasExitCode  bool      `json:"has_exit_code,omitempty"`
}

// serviceSupervisor is the record of one id's restart campaign. At most one
// is ever "of record" for an id (Registry.supervisors) -- mirroring
// mcpbroker's mcpSupervisor (internal/mcpbroker/external_mcp.go): cancelling
// rather than merely replacing a predecessor matters, because a predecessor
// mid-backoff would otherwise relaunch a service an operator, or a newer
// Start, already took charge of.
type serviceSupervisor struct {
	id string

	// ctx/cancel gate whether a pending or in-flight restart may proceed.
	// Cancelled by Stop, Reload's Stop half, and StopAll -- never by the
	// service simply exiting.
	ctx    context.Context
	cancel context.CancelFunc

	mu sync.Mutex
	// cfg is relaunched verbatim on every attempt. An edited config only
	// reaches a running campaign via Reload, which installs a fresh
	// supervisor rather than mutating this one.
	cfg          config.ServiceConfig
	phase        SupervisionPhase
	attempt      int
	nextAttempt  time.Time
	lastExitCode int
	hasExitCode  bool
}

func (s *serviceSupervisor) status() SupervisionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SupervisionStatus{
		Phase:        s.phase,
		Attempt:      s.attempt,
		NextAttempt:  s.nextAttempt,
		LastExitCode: s.lastExitCode,
		HasExitCode:  s.hasExitCode,
	}
}

func (r *Registry) clock() Clock {
	if r.Clock != nil {
		return r.Clock
	}
	return realClock{}
}

// installSupervisor makes sup the supervisor of record for cfg.ID,
// cancelling whatever supervisor -- mid-backoff or not -- it replaces.
// Called by every successful Start, so an explicit Start (operator-driven or
// autostart) always begins a clean restart campaign at attempt 0.
func (r *Registry) installSupervisor(cfg *config.ServiceConfig) *serviceSupervisor {
	ctx, cancel := context.WithCancel(context.Background())
	sup := &serviceSupervisor{id: cfg.ID, cfg: *cfg, ctx: ctx, cancel: cancel, phase: SupervisionRunning}

	r.svMu.Lock()
	old := r.supervisors[cfg.ID]
	r.supervisors[cfg.ID] = sup
	r.svMu.Unlock()

	if old != nil {
		old.cancel()
	}
	return sup
}

// retireSupervisor removes sup if it is still the supervisor of record -- a
// newer one (a fresh explicit Start) must not be uninstalled by an older
// campaign's own cleanup -- and cancels it either way.
func (r *Registry) retireSupervisor(sup *serviceSupervisor) {
	r.svMu.Lock()
	if r.supervisors[sup.id] == sup {
		delete(r.supervisors, sup.id)
	}
	r.svMu.Unlock()
	sup.cancel()
}

// stopSupervision is Stop's half of "don't restart a service the operator
// stopped": it removes and cancels id's supervisor of record, if any, before
// Stop kills the process. Whichever of a concurrent crash and this call
// reaches supervisorOfRecord's check first, the operator's Stop wins: either
// this removes the supervisor before superviseExit looks for it, or it
// cancels a supervisor superviseExit already found, waking a pending
// restart's sleep with no relaunch.
func (r *Registry) stopSupervision(id string) {
	r.svMu.Lock()
	sup, ok := r.supervisors[id]
	if ok {
		delete(r.supervisors, id)
	}
	r.svMu.Unlock()
	if ok {
		sup.cancel()
	}
}

// stopAllSupervision cancels every pending or in-flight restart campaign at
// once -- tray shutdown's use, so no supervised service relaunches after
// StopAll has started tearing everything down.
func (r *Registry) stopAllSupervision() {
	r.svMu.Lock()
	sups := r.supervisors
	r.supervisors = make(map[string]*serviceSupervisor)
	r.svMu.Unlock()
	for _, sup := range sups {
		sup.cancel()
	}
}

// supervisorOfRecord reports whether sup is still installed and not
// cancelled. Every restart decision -- the initial one in superviseExit and
// the one right before spawning in restartAfterBackoff -- makes this exact
// check under the same lock stopSupervision/stopAllSupervision/
// installSupervisor take, which is what makes the races above safe rather
// than merely unlikely.
func (r *Registry) supervisorOfRecord(sup *serviceSupervisor) bool {
	r.svMu.Lock()
	current, ok := r.supervisors[sup.id]
	r.svMu.Unlock()
	return ok && current == sup && sup.ctx.Err() == nil
}

// SupervisionStatuses reports every id relay is actively supervising the
// restart campaign of: currently running under one, mid-backoff, or given
// up. An id absent from the result is not under supervision; its
// running/stopped state comes from IsRunning/RunningIDs.
func (r *Registry) SupervisionStatuses() map[string]SupervisionStatus {
	r.svMu.Lock()
	sups := make([]*serviceSupervisor, 0, len(r.supervisors))
	for _, sup := range r.supervisors {
		sups = append(sups, sup)
	}
	r.svMu.Unlock()

	out := make(map[string]SupervisionStatus, len(sups))
	for _, sup := range sups {
		out[sup.id] = sup.status()
	}
	return out
}

// superviseExit runs once, from the tail of a spawned process's own exit
// goroutine, after every defer that clears its state has already run --
// launch identity included, so any restart it triggers always begins a
// launch whose Hello relay has not yet seen. It decides nothing on its own:
// it checks whether sup is still the id's supervisor of record and, only
// then, hands off to scheduleRestart.
func (r *Registry) superviseExit(sup *serviceSupervisor, exitCode int, ranFor time.Duration) {
	if sup == nil || !r.supervisorOfRecord(sup) {
		return
	}
	r.scheduleRestart(sup, exitCode, ranFor)
}

// serviceRestartDelay mirrors mcpRestartDelay (internal/mcpbroker/external_mcp.go):
// base delay doubled per attempt, capped.
func serviceRestartDelay(attempt int) time.Duration {
	d := ServiceRestartBaseDelay
	for i := 1; i < attempt; i++ {
		if d >= ServiceRestartMaxDelay {
			break
		}
		d *= 2
	}
	if d > ServiceRestartMaxDelay {
		d = ServiceRestartMaxDelay
	}
	return d
}

// scheduleRestart applies the backoff/give-up policy for one exit and, if
// the budget allows it, spawns a goroutine that sleeps off the delay and
// relaunches. ranFor >= ServiceRestartStableWindow resets the attempt
// counter first: a service that ran fine for a while and then died is a
// fresh incident, not the continuation of a crash loop.
func (r *Registry) scheduleRestart(sup *serviceSupervisor, exitCode int, ranFor time.Duration) {
	sup.mu.Lock()
	if ranFor >= ServiceRestartStableWindow {
		sup.attempt = 0
	}
	sup.attempt++
	attempt := sup.attempt
	sup.lastExitCode = exitCode
	sup.hasExitCode = true
	cfg := sup.cfg
	sup.mu.Unlock()

	if exitCode == ExitCodeLaunchIdentity {
		slog.Error("service exited: launch identity could not be established (EX_CONFIG)",
			"id", sup.id, "attempt", attempt, "exit_code", exitCode)
	}

	if attempt > ServiceRestartMaxAttempts {
		sup.mu.Lock()
		sup.phase = SupervisionFailed
		sup.nextAttempt = time.Time{}
		sup.mu.Unlock()
		slog.Error("service restart budget exhausted; staying down until an explicit start or restart",
			"id", sup.id, "attempts", attempt-1, "last_exit_code", exitCode)
		return
	}

	delay := serviceRestartDelay(attempt)
	sup.mu.Lock()
	sup.phase = SupervisionRestarting
	sup.nextAttempt = r.clock().Now().Add(delay)
	sup.mu.Unlock()

	slog.Warn("service exited unexpectedly; restart scheduled",
		"id", sup.id, "attempt", attempt, "exit_code", exitCode, "delay", delay)

	go r.restartAfterBackoff(sup, cfg, delay)
}

// restartAfterBackoff sleeps off delay on the registry's clock, cancellable
// by sup.ctx, then relaunches -- re-checking that sup is still of record
// right before it spawns, so a Stop/Reload/StopAll landing anywhere in this
// window, including after the sleep already completed, wins.
func (r *Registry) restartAfterBackoff(sup *serviceSupervisor, cfg config.ServiceConfig, delay time.Duration) {
	if !sleepClock(sup.ctx, r.clock(), delay) {
		return
	}

	r.mu.Lock()
	if r.isRunningLocked(sup.id) || !r.supervisorOfRecord(sup) {
		r.mu.Unlock()
		return
	}
	proc, err := r.startLocked(&cfg, sup)
	r.mu.Unlock()
	if err != nil {
		slog.Error("service restart failed to spawn", "id", sup.id, "error", err)
		// ranFor 0: this attempt never got a process up, so it is not a
		// stable run and must not reset the counter that is trying to give
		// up on it.
		r.scheduleRestart(sup, -1, 0)
		return
	}

	sup.mu.Lock()
	sup.phase = SupervisionRunning
	sup.nextAttempt = time.Time{}
	sup.mu.Unlock()
	slog.Info("service restarted", "id", sup.id, "pid", proc.cmd.Process.Pid)
}

func sleepClock(ctx context.Context, clk Clock, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	select {
	case <-ctx.Done():
		return false
	case <-clk.After(d):
		return true
	}
}
