package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/enrolment"
	"log/slog"
	"sync"
)

// Reconcile is a convergence step, not a command: there is deliberately no
// Start/Stop/Restart trio — a caller cannot ask for a listener the
// configuration does not describe.
//
// A nil *RemoteSupervisor is a valid "this process has no listener to
// manage" value and every method tolerates it, exactly as RemoteServer does.
//
// This supervisor owns TWO listeners — the tool-plane RemoteServer and the
// enrolment-request EnrolmentRequestServer (spec §1) — and converges both on
// one tick. They are reconciled independently (reconcileToolListenerLocked /
// reconcileEnrolmentListenerLocked), each against its own "did anything
// change" comparison: a single combined check would repeat §11.8's bug,
// where a change to only the enrolment address matched an unrelated
// comparison and was silently ignored.
type RemoteSupervisor struct {
	ctx    context.Context
	store  config.SettingsStore
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

	// enrolTable is the pending enrolment-request table. Built once, here,
	// and reused across every EnrolmentRequestServer this supervisor binds
	// — including a rebind onto a new enrolment_listen address — because
	// the pending requests it holds are real state that must survive a
	// listener move exactly as an enrolment survives a tool-listener move.
	enrolTable *enrolmentRequestTable

	mu     sync.Mutex
	server *RemoteServer
	// enrolServer mirrors server for the enrolment-request listener.
	enrolServer *EnrolmentRequestServer
	// closed latches at shutdown so a reconcile racing cleanup cannot bind
	// a fresh socket behind it.
	closed bool
	// lastReport / lastEnrolReport debounce logging per listener: a steady
	// failure on one is loud once, then repeats at debug, independently of
	// the other listener's own report state.
	lastReport      string
	lastEnrolReport string
}

func NewRemoteSupervisor(ctx context.Context, store config.SettingsStore, router RemoteToolRouter, audit *AuditRecorder, configurer RemoteConfigurer, surfaces func() McpSurfaces, goFunc func(func())) *RemoteSupervisor {
	return &RemoteSupervisor{
		ctx: ctx, store: store, router: router, audit: audit,
		configurer: configurer, surfaces: surfaces, goFunc: goFunc,
		enrolTable: newEnrolmentRequestTable(),
	}
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

// EnrolTable exposes the pending enrolment-request table as the sink
// EnrolmentOps.Approve/Refuse/PendingRequests need (spec §3). The table is
// built once, in NewRemoteSupervisor, and outlives every rebind of the
// listener around it (see enrolTable's own doc comment), so returning it
// here is a one-time read at wiring time, not something Reconcile can race:
// the pointer this returns never changes for the supervisor's lifetime.
func (sup *RemoteSupervisor) EnrolTable() EnrolmentRequestApprovalSink {
	if sup == nil {
		return nil
	}
	return sup.enrolTable
}

// EnrolAddr and EnrolServer mirror Addr and Server for the enrolment-request
// listener.
func (sup *RemoteSupervisor) EnrolAddr() string {
	if sup == nil {
		return ""
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	return sup.enrolServer.Addr()
}

func (sup *RemoteSupervisor) EnrolServer() *EnrolmentRequestServer {
	if sup == nil {
		return nil
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	return sup.enrolServer
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

	settings := config.FreshSettings(sup.store)

	toolErr := sup.reconcileToolListenerLocked(settings)
	enrolErr := sup.reconcileEnrolmentListenerLocked(settings)
	return errors.Join(toolErr, enrolErr)
}

// reconcileToolListenerLocked is the tool-plane RemoteServer's own
// convergence step, unchanged in behaviour from before this supervisor grew
// a second listener — only its "nothing changed" comparison (sup.server's
// own Listen) is now scoped to this listener alone, never the enrolment
// one's.
func (sup *RemoteSupervisor) reconcileToolListenerLocked(settings *config.Settings) error {
	desired := resolveRemoteConfig(settings.Remote)

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
		enrolment.ClearRevocationHookFor(old)
		sup.run(old.Close)
		slog.Warn("remote listener moved; connections on the old address were closed",
			"from", old.cfg.Listen, "to", ns.Addr())
	}

	sup.reportLocked(desired.Listen, nil)
	return nil
}

// reconcileEnrolmentListenerLocked is reconcileToolListenerLocked's twin for
// the enrolment-request listener. It has no revocation hook to move (this
// listener holds no per-connection identity to revoke) and its own desired
// config can itself be an error (resolveEnrolment refuses
// enrolment_requests:true with enabled:false), which is reported and
// returned exactly like a failed bind.
func (sup *RemoteSupervisor) reconcileEnrolmentListenerLocked(settings *config.Settings) error {
	desired, cfgErr := resolveRemoteEnrolment(settings.Remote)
	if cfgErr != nil {
		sup.stopEnrolLocked("enrolment-request configuration is invalid")
		sup.reportEnrolLocked("", cfgErr)
		return cfgErr
	}

	if !desired.Enabled {
		sup.stopEnrolLocked("settings no longer enable the enrolment-request listener")
		sup.reportEnrolLocked("", nil)
		return nil
	}

	if !remoteAuditingLive(settings, sup.audit) {
		err := errors.New("enrolment-request listener not serving: the tool-call audit log is not recording — " +
			"set audit.enabled to true and relaunch relay so the recorder starts (it is built once, at launch), " +
			"or turn off remote.enrolment_requests")
		sup.stopEnrolLocked("auditing is no longer active")
		sup.reportEnrolLocked(desired.Listen, err)
		return err
	}

	// On every tick, not only at bind, so a break-glass CA regeneration
	// reaches the table that derives the comparison code — and so a CA
	// that vanished makes a commitment-bearing lodge refuse rather than
	// answer with a code over a certificate that is no longer on disk.
	// Bytes, never the *enrolment.RelayCA: see setCACert.
	//
	// Ordering is safe. resolveEnrolment already refuses
	// enrolment_requests:true with enabled:false, so the tool-plane
	// listener converges first in this same tick and NewRemoteServer's
	// enrolment.LoadOrCreateCA has already written ca.crt.
	if certPEM, spki, err := enrolment.CAMaterialFromDisk(); err == nil {
		sup.enrolTable.setCACert(certPEM, spki)
	} else {
		sup.enrolTable.setCACert(nil, nil)
	}

	if sup.enrolServer != nil && sup.enrolServer.cfg.Listen == desired.Listen {
		sup.reportEnrolLocked(desired.Listen, nil)
		return nil
	}

	ns, err := NewEnrolmentRequestServer(sup.ctx, sup.enrolTable, sup.audit, desired)
	if err != nil {
		err = fmt.Errorf("enrolment-request listener could not bind %s: %w", desired.Listen, err)
		sup.reportEnrolLocked(desired.Listen, err)
		return err
	}
	if ns == nil {
		sup.stopEnrolLocked("settings disabled the enrolment-request listener mid-reconcile")
		sup.reportEnrolLocked("", nil)
		return nil
	}

	old := sup.enrolServer
	sup.enrolServer = ns
	sup.run(func() { _ = ns.Serve() })

	if old != nil {
		old.StopAccepting()
		sup.run(old.Close)
		slog.Warn("enrolment-request listener moved; connections on the old address were closed",
			"from", old.cfg.Listen, "to", ns.Addr())
	}

	sup.reportEnrolLocked(desired.Listen, nil)
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
	sup.enrolServer.StopAccepting()
}

func (sup *RemoteSupervisor) Close() {
	if sup == nil {
		return
	}
	sup.mu.Lock()
	sup.closed = true
	s := sup.server
	sup.server = nil
	es := sup.enrolServer
	sup.enrolServer = nil
	sup.mu.Unlock()
	s.Close()
	es.Close()
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
	enrolment.ClearRevocationHookFor(old)
	sup.run(old.Close)
	slog.Warn("remote listener stopped", "addr", old.cfg.Listen, "reason", why)
}

// stopEnrolLocked mirrors stopLocked for the enrolment-request listener.
// No revocation hook to clear: this listener resolves no identity, so there
// is nothing for a revocation to close early.
func (sup *RemoteSupervisor) stopEnrolLocked(why string) {
	if sup.enrolServer == nil {
		return
	}
	old := sup.enrolServer
	sup.enrolServer = nil
	old.StopAccepting()
	sup.run(old.Close)
	slog.Warn("enrolment-request listener stopped", "addr", old.cfg.Listen, "reason", why)
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

// reportEnrolLocked mirrors reportLocked with its own debounce state
// (lastEnrolReport), so a steady failure on one listener does not silence —
// or get silenced by — a steady failure on the other.
func (sup *RemoteSupervisor) reportEnrolLocked(addr string, err error) {
	key := addr
	if err != nil {
		key += "\x00" + err.Error()
	}
	repeat := key == sup.lastEnrolReport
	sup.lastEnrolReport = key
	if err == nil {
		return
	}
	if repeat {
		slog.Debug("enrolment-request listener still not available", "listen", addr, "error", err)
		return
	}
	slog.Error("enrolment-request listener not available", "listen", addr, "error", err)
}

func (sup *RemoteSupervisor) run(fn func()) {
	if sup.goFunc != nil {
		sup.goFunc(fn)
		return
	}
	go fn()
}
