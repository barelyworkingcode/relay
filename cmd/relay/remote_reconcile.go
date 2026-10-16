package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/enrolment"
	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/project"
	"log/slog"
	"sync"
	"time"
)

// Reconcile is a convergence step, not a command: there is deliberately no
// Start/Stop/Restart trio — a caller cannot ask for a listener the
// configuration does not describe.
//
// A nil *RemoteSupervisor is a valid "this process has no listener to
// manage" value and every method tolerates it, exactly as RemoteServer does.
//
// This supervisor owns TWO listeners — the tool-plane RemoteServer, which
// Reconcile converges on every tick, and the enrolment-request
// EnrolmentRequestServer (spec §1), which exists only for an enrolment window
// (OpenEnrolmentWindow) and which Reconcile never binds. Reconcile only closes
// an open window when the tool-plane listener or auditing turns off.
type RemoteSupervisor struct {
	ctx    context.Context
	store  config.SettingsStore
	router RemoteToolRouter
	audit  *audit.AuditRecorder
	// configurer and surfaces are threaded straight through to every
	// RemoteServer this supervisor binds — see RemoteConfigurer's own doc
	// comment for why they are two narrow things and not a *ProjectOps.
	configurer RemoteConfigurer
	surfaces   func() project.McpSurfaces
	// goFunc runs the accept loop under the owner's waitgroup; nil falls
	// back to a bare `go`, for tests.
	goFunc func(func())
	// clock times the tool and mount budgets of every RemoteServer this
	// supervisor binds. Nil reads as wall time.
	clock serverClock

	// enrolTable is the pending enrolment-request table. Built once, here,
	// and reused across every window: a request lodged in one window outlives
	// that window (it can be approved until it expires), so the table cannot
	// belong to a listener.
	enrolTable *enrolmentRequestTable

	mu     sync.Mutex
	server *RemoteServer
	// enrolWindow is the open enrolment window, or nil. It lives in memory
	// only: a restart closes it, and nothing reopens a port by itself.
	enrolWindow *enrolmentWindow
	windowGen   uint64
	// lodging holds the servers of consumed windows while a lodging
	// connection is still alive. Remote or auditing turning off closes them:
	// a machine mid-poll must not collect a certificate with no recording.
	lodging map[*EnrolmentRequestServer]struct{}
	// onWindowClosed is told why each window closed. The window core uses it
	// to notify the operator of an expiry.
	onWindowClosed func(reason string)
	// closed latches at shutdown so a reconcile racing cleanup cannot bind
	// a fresh socket behind it.
	closed bool
	// lastReport debounces logging: a steady failure is loud once, then
	// repeats at debug.
	lastReport string
}

// enrolmentWindow is one open window: its profile, its socket and the channel
// that stops its expiry timer.
type enrolmentWindow struct {
	gen       uint64
	profileID string
	expires   time.Time
	server    *EnrolmentRequestServer
	done      chan struct{}
}

// Window close reasons, the enrolment.window.close event's reason values.
const (
	windowClosedLodged      = "lodged"
	windowClosedExpired     = "expired"
	windowClosedCancelled   = "cancelled"
	windowClosedReplaced    = "replaced"
	windowClosedListenerOff = "listener_off"
)

func NewRemoteSupervisor(ctx context.Context, store config.SettingsStore, router RemoteToolRouter, audit *audit.AuditRecorder, configurer RemoteConfigurer, surfaces func() project.McpSurfaces, goFunc func(func()), clock serverClock) *RemoteSupervisor {
	table := newEnrolmentRequestTable()
	table.setClock(func() time.Time { return clockNow(clock) })
	return &RemoteSupervisor{
		ctx: ctx, store: store, router: router, audit: audit,
		configurer: configurer, surfaces: surfaces, goFunc: goFunc,
		enrolTable: table, clock: clock,
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

// EnrolAddr is the address of the open window's socket, or "" with no window.
func (sup *RemoteSupervisor) EnrolAddr() string {
	if sup == nil {
		return ""
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	if sup.enrolWindow == nil {
		return ""
	}
	return sup.enrolWindow.server.Addr()
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
	sup.reconcileEnrolmentWindowLocked(settings)
	// Nil-safe: a nil or not-yet-started server is a valid steady state.
	sup.server.revalidateMounts()
	return toolErr
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
	ns, err := NewRemoteServer(sup.ctx, sup.store, sup.router, sup.audit, sup.configurer, sup.surfaces, sup.clock)
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

// reconcileEnrolmentWindowLocked never binds the enrolment socket. It keeps
// the comparison code's CA current and closes an open window the moment the
// tool-plane listener or auditing is no longer live: a door onto enrolment is
// not left open unrecorded.
func (sup *RemoteSupervisor) reconcileEnrolmentWindowLocked(settings *config.Settings) {
	if !resolveRemoteConfig(settings.Remote).Enabled || !remoteAuditingLive(settings, sup.audit) {
		sup.closeWindowLocked(windowClosedListenerOff)
		sup.closeLodgingLocked()
		return
	}
	sup.refreshEnrolCALocked()
}

// refreshEnrolCALocked hands the table the CA's public bytes, so a break-glass
// CA regeneration reaches the code comparison, and a CA that vanished makes a
// lodge refuse rather than answer with a code over a certificate no longer
// on disk. Bytes, never the *enrolment.RelayCA: see setCACert.
func (sup *RemoteSupervisor) refreshEnrolCALocked() {
	if certPEM, spki, err := enrolment.CAMaterialFromDisk(); err == nil {
		sup.enrolTable.setCACert(certPEM, spki)
	} else {
		sup.enrolTable.setCACert(nil, nil)
	}
}

// OnEnrolmentWindowClosed registers the hook told why each window closed.
func (sup *RemoteSupervisor) OnEnrolmentWindowClosed(fn func(reason string)) {
	if sup == nil {
		return
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	sup.onWindowClosed = fn
}

// OpenEnrolmentWindow binds the enrolment socket for one access profile and
// starts the expiry timer, replacing a window already open. The caller has
// already been through the presence gate and recorded the issuance; this only
// binds. It refuses when the tool-plane listener or auditing is off.
func (sup *RemoteSupervisor) OpenEnrolmentWindow(profileID string, expires time.Time) (addr string, err error) {
	if sup == nil {
		return "", errors.New("enrolment windows are not available in this relay process")
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	if sup.closed || sup.ctx.Err() != nil {
		return "", errors.New("relay is shutting down")
	}
	settings := config.FreshSettings(sup.store)
	desired := resolveRemoteEnrolment(settings.Remote)
	if !desired.Enabled {
		return "", errors.New("turn the remote listener on first: Settings > Remote Clients, or relay remote set")
	}
	if !remoteAuditingLive(settings, sup.audit) {
		return "", errors.New("enrolment is not opened while the tool-call audit log is not recording: " +
			"set audit.enabled to true and relaunch relay so the recorder starts")
	}

	// The old socket is closed first: a replacement binds the same address.
	sup.closeWindowLocked(windowClosedReplaced)
	sup.refreshEnrolCALocked()

	ns, err := NewEnrolmentRequestServer(sup.ctx, sup.enrolTable, sup.audit, desired)
	if err != nil {
		return "", fmt.Errorf("enrolment listener could not bind %s: %w", desired.Listen, err)
	}
	if ns == nil {
		return "", errors.New("turn the remote listener on first: Settings > Remote Clients, or relay remote set")
	}

	sup.windowGen++
	w := &enrolmentWindow{gen: sup.windowGen, profileID: profileID, expires: expires, server: ns, done: make(chan struct{})}
	sup.enrolWindow = w
	sup.enrolTable.openWindow(w.gen, profileID)
	ns.windowConsumed = func(gen uint64) { sup.windowConsumed(gen) }
	sup.run(func() { _ = ns.Serve() })

	// Registered here, before this returns, so a test clock that advances
	// right after the open finds the waiter.
	var expiry <-chan time.Time
	if sup.clock != nil {
		expiry = sup.clock.After(expires.Sub(sup.clock.Now()))
	} else {
		expiry = time.After(time.Until(expires))
	}
	sup.run(func() {
		select {
		case <-expiry:
			sup.closeWindowGen(w.gen, windowClosedExpired)
		case <-w.done:
		case <-sup.ctx.Done():
		}
	})
	return ns.Addr(), nil
}

// CloseEnrolmentWindow closes the open window and reports whether one was open.
func (sup *RemoteSupervisor) CloseEnrolmentWindow(reason string) bool {
	if sup == nil {
		return false
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	return sup.closeWindowLocked(reason)
}

// EnrolmentWindow reports the open window.
func (sup *RemoteSupervisor) EnrolmentWindow() (profileID string, expires time.Time, addr string, open bool) {
	if sup == nil {
		return "", time.Time{}, "", false
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	w := sup.enrolWindow
	if w == nil {
		return "", time.Time{}, "", false
	}
	return w.profileID, w.expires, w.server.Addr(), true
}

// windowConsumed runs on the lodging connection, before its reply is written.
func (sup *RemoteSupervisor) windowConsumed(gen uint64) {
	sup.closeWindowGen(gen, windowClosedLodged)
}

func (sup *RemoteSupervisor) closeWindowGen(gen uint64, reason string) {
	sup.mu.Lock()
	defer sup.mu.Unlock()
	if sup.enrolWindow == nil || sup.enrolWindow.gen != gen {
		return
	}
	sup.closeWindowLocked(reason)
}

// closeWindowLocked ends the window. The socket stops accepting before the
// event is written, so a reader of the event can rely on the port being
// closed. The reason rides as close_reason: a line's own reason key is
// reserved for the failures of a status that is not ok. A lodged window keeps the one connection that lodged; every other
// reason ends all of them.
func (sup *RemoteSupervisor) closeWindowLocked(reason string) bool {
	w := sup.enrolWindow
	if w == nil {
		return false
	}
	sup.enrolWindow = nil
	close(w.done)
	// This is subtle: a window the table no longer holds was already
	// consumed by a lodge whose windowConsumed has not run yet. Whatever
	// raced it here, the close is the lodge's, and the lodging connection
	// must survive it.
	if !sup.enrolTable.closeWindow(w.gen) {
		reason = windowClosedLodged
	}
	w.server.StopAccepting()
	if reason == windowClosedLodged {
		if sup.lodging == nil {
			sup.lodging = map[*EnrolmentRequestServer]struct{}{}
		}
		sup.lodging[w.server] = struct{}{}
		sup.run(func() {
			w.server.CloseWhenIdle()
			sup.mu.Lock()
			delete(sup.lodging, w.server)
			sup.mu.Unlock()
		})
	} else {
		sup.run(w.server.Close)
	}
	logging.BeginEvent(logging.ContextWithTrace(context.Background(), logging.NewTraceID()), "enrolment.window.close").
		Set("target", "profile").Set("profile_id", w.profileID).Set("close_reason", reason).End(logging.OutcomeOK, "", nil)
	if sup.onWindowClosed != nil {
		sup.onWindowClosed(reason)
	}
	return true
}

// closeLodgingLocked ends every lodging connection still alive; each request
// is then abandoned as with any dropped connection.
func (sup *RemoteSupervisor) closeLodgingLocked() {
	for srv := range sup.lodging {
		sup.run(srv.Close)
	}
}

func (sup *RemoteSupervisor) StopAccepting() {
	if sup == nil {
		return
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	sup.closed = true
	sup.server.StopAccepting()
	if sup.enrolWindow != nil {
		sup.enrolWindow.server.StopAccepting()
	}
}

func (sup *RemoteSupervisor) Close() {
	if sup == nil {
		return
	}
	sup.mu.Lock()
	sup.closed = true
	s := sup.server
	sup.server = nil
	var es *EnrolmentRequestServer
	if sup.enrolWindow != nil {
		es = sup.enrolWindow.server
		close(sup.enrolWindow.done)
		sup.enrolWindow = nil
	}
	lodging := sup.lodging
	sup.lodging = nil
	sup.mu.Unlock()
	s.Close()
	es.Close()
	for srv := range lodging {
		srv.Close()
	}
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
