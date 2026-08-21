package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

// RemoteSupervisor keeps the remote listener in step with settings.json without
// a restart, the way ExternalMcpManager.Reconcile keeps MCP processes in step
// and ReloadService keeps a service process in step.
//
// The listener used to be built exactly once, in runTrayApp. That made
// `remote.enabled` and `remote.listen` the only settings in relay whose only
// application mechanism was quitting the tray — and it made the safety
// properties around them stale rather than enforced: turning auditing off left
// a listener serving, and moving the bind address left it on the old one.
//
// Reconcile is a CONVERGENCE step, not a command: it compares what settings ask
// for against what is actually bound and does the minimum to close the gap.
// That is what makes it safe to call on every settings poll, and it is why
// there is no Start/Stop/Restart trio — a caller cannot ask for a listener the
// configuration does not describe, which is the property ADR-010 decision 9
// exists to protect.
//
// A nil *RemoteSupervisor is a valid "this process has no listener to manage"
// value and every method tolerates it, exactly as RemoteServer does.
type RemoteSupervisor struct {
	ctx    context.Context
	store  SettingsStore
	router RemoteToolRouter
	audit  *AuditRecorder
	// goFunc runs the accept loop, and the drain of a listener that has been
	// replaced, under the owner's waitgroup so shutdown waits for both. nil
	// falls back to a bare `go`, which is what a test wants.
	goFunc func(func())

	mu     sync.Mutex
	server *RemoteServer
	// closed latches at shutdown so a reconcile racing cleanup cannot bind a
	// fresh socket behind it.
	closed bool
	// lastReport is the last (address, failure) pair reported, so a reconcile
	// that runs every two seconds does not turn one misconfigured port into a
	// log flood. State CHANGES are loud; a steady failure is loud once and then
	// repeats at debug.
	lastReport string
}

// NewRemoteSupervisor wires a supervisor. It binds nothing — call Reconcile.
func NewRemoteSupervisor(ctx context.Context, store SettingsStore, router RemoteToolRouter, audit *AuditRecorder, goFunc func(func())) *RemoteSupervisor {
	return &RemoteSupervisor{ctx: ctx, store: store, router: router, audit: audit, goFunc: goFunc}
}

// Addr returns the address the live listener is bound to, or "" when there is
// none. Reads the actual socket, not the configured string, so a test binding
// :0 gets the assigned port and an operator gets the truth.
func (sup *RemoteSupervisor) Addr() string {
	if sup == nil {
		return ""
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	return sup.server.Addr()
}

// Server returns the live listener, or nil. For tests that need to assert on
// the object identity the revocation hook points at.
func (sup *RemoteSupervisor) Server() *RemoteServer {
	if sup == nil {
		return nil
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	return sup.server
}

// Reconcile converges the live listener onto what settings.json now says.
//
// The returned error is the same one that was logged; it exists so a caller can
// surface a bind failure and a test can assert on it. Nothing about the
// supervisor's state depends on the caller handling it — a failure always
// leaves a coherent state behind, never a half-bound one.
//
// Five outcomes, in the order they are decided:
//
//  1. Not enabled → stop whatever is running and open nothing. An ABSENT block
//     and a block that omits `enabled` both land here, because RemoteConfig
//     resolves both to disabled (deliberately the opposite of AuditConfig).
//     Reconciliation must never open a listener the configuration does not
//     explicitly ask for, so this branch is checked before anything else.
//  2. Enabled but auditing is not live → stop, and say why. Auditing is a hard
//     dependency of remote access, not a preference: the case for letting a VM
//     reach host mail rests entirely on the calls being recorded. Turning
//     auditing off at runtime therefore has to CLOSE the listener, not leave it
//     serving until the next restart.
//  3. Enabled, auditing live, and the bound address already matches → do
//     nothing at all. Live connections are untouched. This is the overwhelmingly
//     common outcome, since this runs on every settings poll.
//  4. Enabled and the address differs (or nothing is bound) → bind the NEW
//     address first, and only then tear the old listener down. A rebind that
//     fails must not leave relay with no listener and no error, so the old one
//     keeps serving its old address and the failure is loud.
//  5. Bind failed → old state stands, error returned and logged. The next poll
//     retries, so a port freed by whatever was holding it is picked up without
//     an operator doing anything.
func (sup *RemoteSupervisor) Reconcile() error {
	if sup == nil {
		return nil
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	if sup.closed || sup.ctx.Err() != nil {
		return nil
	}

	settings := freshSettings(sup.store)
	desired := settings.Remote.resolve()

	if !desired.Enabled {
		sup.stopLocked("settings no longer enable the remote listener")
		sup.reportLocked("", nil)
		return nil
	}

	if !remoteAuditingLive(settings, sup.audit) {
		err := errors.New("remote listener not serving: the tool-call audit log is not recording, and a remote grant is " +
			"justified by the calls it records — set audit.enabled to true and relaunch relay so the recorder starts " +
			"(it is built once, at launch), or remove the remote block from settings.json")
		sup.stopLocked("auditing is no longer active")
		sup.reportLocked(desired.Listen, err)
		return err
	}

	if sup.server != nil && sup.server.cfg.Listen == desired.Listen {
		// Already converged. Deliberately NOT a rebind-anyway: rebinding on an
		// unchanged address would cut every live connection every two seconds.
		sup.reportLocked(desired.Listen, nil)
		return nil
	}

	// Bind before tearing down. NewRemoteServer installs the revocation hook as
	// part of coming up, so from this line on the hook points at the NEW
	// listener; the old one's teardown below compare-and-clears and therefore
	// leaves it alone.
	ns, err := NewRemoteServer(sup.ctx, sup.store, sup.router, sup.audit)
	if err != nil {
		// Reported and returned as the SAME error, so what a test asserts on and
		// what an operator reads in the log cannot drift apart.
		err = fmt.Errorf("remote listener could not bind %s: %w", desired.Listen, err)
		sup.reportLocked(desired.Listen, err)
		return err
	}
	if ns == nil {
		// Only reachable if settings changed to disabled between our read and
		// NewRemoteServer's. Treat it as outcome 1 rather than as success:
		// whatever the file says now is what wins.
		sup.stopLocked("settings disabled the remote listener mid-reconcile")
		sup.reportLocked("", nil)
		return nil
	}

	old := sup.server
	sup.server = ns
	sup.run(func() { _ = ns.Serve() })

	if old != nil {
		// Live connections on the OLD address are cut, deliberately.
		//
		// `listen` is the reachability control. If sessions established on the
		// old address survived the move, then narrowing the bind (0.0.0.0 →
		// 127.0.0.1, say) would not actually narrow anything for the client
		// that most matters — and this protocol holds persistent connections in
		// a scanner loop, so "they will drop eventually" means "never". That is
		// the same reasoning ADR-010 decision 8 applies to revocation, and the
		// answer is the same here.
		//
		// The cost is bounded and visible: an in-flight call on the old socket
		// fails, and the client reconnects at the new address with its identity
		// and grants untouched. Nothing about the client's authorization
		// changed — only where relay listens.
		//
		// StopAccepting is synchronous so the old address stops answering
		// before this returns; the drain is not, because a handler blocked in a
		// tool call would otherwise hold the settings poll for as long as the
		// MCP takes.
		old.StopAccepting()
		ClearEnrolmentRevocationHookFor(old)
		sup.run(old.Close)
		slog.Warn("remote listener moved; connections on the old address were closed",
			"from", old.cfg.Listen, "to", ns.Addr())
	}

	// No "reconciled" line here: NewRemoteServer already logged the address it
	// bound, and a second line saying the same thing on every change is how a
	// log stops being read.
	sup.reportLocked(desired.Listen, nil)
	return nil
}

// StopAccepting closes the listener and latches the supervisor shut, so a
// reconcile racing shutdown cannot bind a fresh socket behind it. Phase one of
// the tray's drain-then-kill cleanup.
func (sup *RemoteSupervisor) StopAccepting() {
	if sup == nil {
		return
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	sup.closed = true
	sup.server.StopAccepting()
}

// Close completes shutdown, draining the live listener's handlers. Synchronous:
// at shutdown the caller genuinely does want to wait.
func (sup *RemoteSupervisor) Close() {
	if sup == nil {
		return
	}
	sup.mu.Lock()
	sup.closed = true
	s := sup.server
	sup.server = nil
	sup.mu.Unlock()
	s.Close()
}

// stopLocked tears the live listener down and leaves nothing in its place.
// Silent when there was nothing running, so the disabled steady state — by far
// the most common configuration — says nothing on every poll.
func (sup *RemoteSupervisor) stopLocked(why string) {
	if sup.server == nil {
		return
	}
	old := sup.server
	sup.server = nil
	old.StopAccepting()
	// Clear here rather than leaving it to the background drain: after this
	// call there is no listener, and a revocation must not be handed to a
	// closure over a torn-down server.
	ClearEnrolmentRevocationHookFor(old)
	sup.run(old.Close)
	slog.Warn("remote listener stopped", "addr", old.cfg.Listen, "reason", why)
}

// reportLocked logs a state change once. addr+err identical to last time means
// the situation has not changed, so the repeat goes to debug: this runs on a
// two-second poll, and a misconfigured port must not bury the log.
func (sup *RemoteSupervisor) reportLocked(addr string, err error) {
	key := addr
	if err != nil {
		key += "\x00" + err.Error()
	}
	repeat := key == sup.lastReport
	sup.lastReport = key
	if err == nil {
		return
	}
	if repeat {
		slog.Debug("remote listener still not available", "listen", addr, "error", err)
		return
	}
	slog.Error("remote listener not available", "listen", addr, "error", err)
}

// run launches fn on the owner's waitgroup when there is one. Tests pass nil
// and get a bare goroutine.
func (sup *RemoteSupervisor) run(fn func()) {
	if sup.goFunc != nil {
		sup.goFunc(fn)
		return
	}
	go fn()
}
