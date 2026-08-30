package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

// Reconcile is a convergence step, not a command: there is deliberately no
// Start/Stop/Restart trio — a caller cannot ask for a listener the
// configuration does not describe.
//
// A nil *RemoteSupervisor is a valid "this process has no listener to
// manage" value and every method tolerates it, exactly as RemoteServer does.
type RemoteSupervisor struct {
	ctx    context.Context
	store  SettingsStore
	router RemoteToolRouter
	audit  *AuditRecorder
	// configurer and surfaces are threaded straight through to every
	// RemoteServer this supervisor binds — see RemoteConfigurer's own doc
	// comment for why they are two narrow things and not a *ProjectOps.
	configurer RemoteConfigurer
	surfaces   func() McpSurfaces
	// goFunc runs the accept loop under the owner's waitgroup; nil falls
	// back to a bare `go`, for tests.
	goFunc func(func())

	mu     sync.Mutex
	server *RemoteServer
	// closed latches at shutdown so a reconcile racing cleanup cannot bind
	// a fresh socket behind it.
	closed bool
	// lastReport debounces logging: a steady failure is loud once, then
	// repeats at debug.
	lastReport string
}

func NewRemoteSupervisor(ctx context.Context, store SettingsStore, router RemoteToolRouter, audit *AuditRecorder, configurer RemoteConfigurer, surfaces func() McpSurfaces, goFunc func(func())) *RemoteSupervisor {
	return &RemoteSupervisor{ctx: ctx, store: store, router: router, audit: audit, configurer: configurer, surfaces: surfaces, goFunc: goFunc}
}

// Addr reads the live socket, so a test binding :0 gets the assigned port.
func (sup *RemoteSupervisor) Addr() string {
	if sup == nil {
		return ""
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	return sup.server.Addr()
}

func (sup *RemoteSupervisor) Server() *RemoteServer {
	if sup == nil {
		return nil
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	return sup.server
}

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
		// Deliberately not a rebind-anyway: rebinding on an unchanged
		// address would cut every live connection every poll.
		sup.reportLocked(desired.Listen, nil)
		return nil
	}

	// Bind before tearing down: the old listener's teardown below
	// compare-and-clears and leaves the new one's hook alone.
	ns, err := NewRemoteServer(sup.ctx, sup.store, sup.router, sup.audit, sup.configurer, sup.surfaces)
	if err != nil {
		err = fmt.Errorf("remote listener could not bind %s: %w", desired.Listen, err)
		sup.reportLocked(desired.Listen, err)
		return err
	}
	if ns == nil {
		// Only reachable if settings changed to disabled between our read
		// and NewRemoteServer's.
		sup.stopLocked("settings disabled the remote listener mid-reconcile")
		sup.reportLocked("", nil)
		return nil
	}

	old := sup.server
	sup.server = ns
	sup.run(func() { _ = ns.Serve() })

	if old != nil {
		// Live connections on the OLD address are cut deliberately: `listen`
		// is the reachability control, so a narrowed bind that left old
		// sessions running would not have narrowed anything. StopAccepting
		// is synchronous so the old address stops answering before this
		// returns; the drain is not, so a blocked tool call cannot hold up
		// the settings poll.
		old.StopAccepting()
		ClearEnrolmentRevocationHookFor(old)
		sup.run(old.Close)
		slog.Warn("remote listener moved; connections on the old address were closed",
			"from", old.cfg.Listen, "to", ns.Addr())
	}

	sup.reportLocked(desired.Listen, nil)
	return nil
}

func (sup *RemoteSupervisor) StopAccepting() {
	if sup == nil {
		return
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	sup.closed = true
	sup.server.StopAccepting()
}

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

func (sup *RemoteSupervisor) stopLocked(why string) {
	if sup.server == nil {
		return
	}
	old := sup.server
	sup.server = nil
	old.StopAccepting()
	// Cleared here, not left to the background drain: a revocation must
	// not be handed to a closure over a torn-down server.
	ClearEnrolmentRevocationHookFor(old)
	sup.run(old.Close)
	slog.Warn("remote listener stopped", "addr", old.cfg.Listen, "reason", why)
}

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

func (sup *RemoteSupervisor) run(fn func()) {
	if sup.goFunc != nil {
		sup.goFunc(fn)
		return
	}
	go fn()
}
