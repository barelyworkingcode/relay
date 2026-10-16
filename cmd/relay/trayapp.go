package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/sealed"
	"github.com/barelyworkingcode/relay/internal/service"
	"github.com/barelyworkingcode/relay/internal/sessions/ledger"
	"github.com/barelyworkingcode/relay/internal/sshhost"
)

// appInstance is the singleton tray app, set by runTrayApp and read by Cocoa
// callbacks (exported Go functions called from cgo). This global is required
// because cgo //export functions cannot capture closures or accept user data.
// It is safe because the tray app is inherently single-instance.
var appInstance *App

// App is the main tray application state.
type App struct {
	// clock is the one clock startServerCore built; the tray countdown and
	// the settings document read it. Nil reads as wall time.
	clock        serverClock
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	store        config.SettingsStore
	platform     Platform
	extMgr       *mcpbroker.Manager
	registry     service.Manager
	serviceQueue *config.CommandQueue
	serviceOps   *ServiceOps
	bridgeServer *bridge.BridgeServer
	// frontendChannel provisions and, on shutdown, closes the frontend
	// socket/token. Owned here rather than by the registry: the registry
	// only ever needs the env it produces, wired through Registry.FrontendEnv.
	frontendChannel *FrontendChannel
	// remote supervises the mTLS listener remote clients reach relay through:
	// it binds, rebinds and stops as `remote.enabled` / `remote.listen` change,
	// so neither needs a restart to take effect. Holds no listener at all
	// whenever no remote block is configured, which is the overwhelmingly
	// common case; every method on it is nil-safe so nothing branches here.
	remote         *RemoteSupervisor
	frontendServer *FrontendServer
	// modelEndpoint serves model.sock always and a loopback TCP listener
	// when settings.json's model_endpoint block configures one
	// (docs/model-endpoint.md). Reconciled on the same poll tick as remote.
	modelEndpoint *ModelEndpointServer
	ipcCtx        *IPCContext // pre-built once, reused on every IPC call
	// audit is the tool-call recorder. Nil when auditing is disabled or failed
	// to start; every method on it is nil-safe.
	audit *audit.AuditRecorder
	// settingsOpen gates UI emits on whether the Settings window is up. Written
	// on the main thread (open/close) but read from background goroutines (the
	// status poller, HTTP-driven project refresh), so it must be atomic.
	settingsOpen atomic.Bool
	cleanupOnce  sync.Once
	svcMenuMap   map[int]string // menu item ID -> service ID

	// rssByID is the most recent per-service subtree memory sample, refreshed
	// by statusPoller. Treated as immutable once published — the writer always
	// stores a fresh map rather than mutating in place, so readers can use the
	// loaded map without copying.
	rssByID atomic.Pointer[map[string]uint64]

	// lastMenuJSON caches the most recently dispatched menu JSON so the poller
	// can skip platform.UpdateMenu calls when nothing has changed. macOS
	// rebuilds NSMenu via removeAllItems; suppressing no-op updates avoids
	// redraw churn while the menu is open.
	lastMenuJSON string

	// loginOps backs the tray's login-code item and the Passkeys tab; it is
	// the same core `relay login` runs through.
	loginOps *LoginOps

	// eveEnrolmentOps owns the Home|Work window the Allow Enrolment sheet
	// opens and Cancel Enrolment closes; it is the same core `relay eve enrol`
	// runs through (docs/eve-passkey-enrolment.md).
	eveEnrolmentOps *EveEnrolmentOps

	// shownEnrolment remembers which of the Home|Work window and relay sign-in
	// code the tray last listed, so a poll that finds one gone knows to ask
	// whether it expired. Main-thread only, like lastMenuJSON.
	shownEnrolment map[string]bool

	// evePasskeyOps backs the Passkeys tab's eve section and
	// pushFullSettings' eve_passkeys payload; it is the same core `relay eve
	// list|revoke` and eve's own PUT/GET mirror routes use.
	evePasskeyOps *EvePasskeyOps

	// pendingSettingsPage is the page a tray action wants the next Settings
	// window to open on, waiting for the first paint: a window that is not up
	// has no document to receive an emit. Main-thread only. Empty is the
	// ordinary case: the window opens where it always did.
	pendingSettingsPage string

	// lastStatusBatchDigest fingerprints the most recently emitted service-
	// status batch (FetchedAt zeroed). Identical batches across ticks are
	// suppressed so the inspector's WebView doesn't re-render every 2s for
	// no reason. Pointer so atomic.CompareAndSwap on a content-derived value
	// is straightforward.
	lastStatusBatchDigest atomic.Pointer[[32]byte]

	// presenceGate is the real LocalAuthentication-backed gate every gated
	// core's own Gate field points at (ADR-017 decisions 3 and 4). The tray
	// uses it directly for exactly one act of its own: resetSealedStore.
	presenceGate *presence.Gate

	// sealedKeyring is the ONE production keychain keyring (§5.3.3: the
	// keychain is asked only from the tray, never from a CLI process). Held
	// here only for the break-glass reset, which is the one act that must
	// delete the keychain item the running store's key lives in — every
	// ordinary seal and unseal goes through the store's own sealer instead.
	sealedKeyring sealed.Keyring

	// enrolNotifier raises the coalesced, rate-limited banner for newly
	// lodged enrolment requests. Built lazily on the main thread by
	// updateMenuWithSettings, the only place that drives it, so a test that
	// constructs an App literal can install its own clock and sink first.
	enrolNotifier *pendingEnrolmentNotifier

	// configDir is where settings.json, ca.key.sealed and ca.crt live.
	// resetSealedStore is the one place outside FileSettingsStore itself
	// that deletes files in this directory by name.
	configDir string

	// addrMu orders the writes of frontendServer, its TCP listener and remote
	// against ListenAddrs, which request handlers may call while startup is
	// still binding.
	addrMu sync.RWMutex

	// frontendSocketPath is the frontend socket ready.json reports.
	frontendSocketPath string

	// readyMu guards the ready.json state below. readyArmed is false before
	// the first write and again once cleanup removed the file, which is what
	// keeps a late listener reconcile from publishing or resurrecting it.
	readyMu    sync.Mutex
	readyLast  []byte
	readyArmed bool

	// releaseOwnership drops the per-config-dir tray lock; cleanup calls it
	// last, so the directory stays owned until nothing of this tray runs.
	releaseOwnership func()

	// stopSettingsWatch ends the settings.json file watch; nil if it never
	// started.
	stopSettingsWatch func()
	// importFile is the store's ImportFile, the only path by which a hand edit
	// of settings.json reaches the tray's state.
	importFile func() (bool, error)

	// sealStatus is store.SealStatus()'s reason, captured once at boot: the
	// sealed store degrades (or not) before EnsureInitialized returns, and
	// nothing after that point changes it in the lifetime of this process.
	// Empty means healthy. Surfaced on the Overview tab and in the tray menu.
	sealStatus string

	// mcpHealthMu guards lastHealth, written from the health observer (an
	// mcpSupervisor's own goroutine, per SetHealthObserver's doc comment)
	// and read whenever the Overview/MCP Servers tab payload is built.
	mcpHealthMu sync.Mutex
	// lastHealth is the most recent HealthEvent per external MCP id. No
	// entry means no death or restart has ever been reported for that MCP —
	// which combined with extMgr.IsConnected is what lets the UI tell "never
	// had a problem" apart from "just recovered from one".
	lastHealth map[string]mcpbroker.HealthEvent
}

// goFunc launches a tracked goroutine. All goroutines launched this way are
// waited on during cleanup, ensuring clean shutdown.
func (a *App) goFunc(fn func()) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		fn()
	}()
}

// cleanupWaitGroupTimeout bounds cleanup()'s wait for tracked goroutines.
// cleanup runs ON the Cocoa main thread (the Exit menu item and
// applicationWillTerminate both call it there directly), and a tracked
// goroutine can itself need the main thread — ResetMcpPermissions
// (ipc_mcp_permissions.go) dispatch_syncs to it. An unbounded Wait() there
// would deadlock permanently: the goroutine can never reach a main thread
// that is busy waiting for it. The process is exiting either way once
// cleanup returns, so anything still running past this bound is abandoned,
// not resumed.
const cleanupWaitGroupTimeout = 5 * time.Second

// openSessionLedger opens the session ledger (plan-broker-and-sessions.md §2
// C5): relay's only on-disk record of a claude/pi/chat session, never a
// terminal (never persisted) and never a secret or key. A first start with no
// ledger file yet imports relayLLM's own dormant sessions once, so an upgrade
// does not silently drop every resumable session a user already had. It
// returns nil when the ledger cannot be read; resume is then unavailable this
// run.
func openSessionLedger(configDir string, store config.SettingsStore) *ledger.Ledger {
	isFirstRun := !ledger.Exists(configDir)
	l, err := ledger.Open(configDir)
	if err != nil {
		slog.Error("failed to open session ledger; session resume will be unavailable this run", "error", err)
		return nil
	}
	// Deliberate: relayLLM's sessions belong to the default install, so any
	// other config dir starts with an empty ledger.
	if isFirstRun && filepath.Clean(configDir) == filepath.Clean(bridge.DefaultConfigDir()) {
		importSessionLedgerFromRelayLLM(l)
	}
	// Deliberate: relay-sessions and every provider it hosts are children of
	// this relay process, so no record from an earlier run can still be live.
	// A live record left over would make resume skip the launch. The one
	// exception is a detached host session whose agent still runs: the agent
	// is not relay's child, and relay-sessions reattaches it as it starts.
	running := detachedRunnersAnswering(l, store)
	n, err := l.MarkLiveDormant(func(r ledger.Record) bool { return running[r.SessionID] })
	if err != nil {
		slog.Warn("session ledger: could not age stale live sessions to dormant", "count", n, "error", err)
	} else if n > 0 {
		slog.Info("session ledger: aged stale live sessions to dormant", "count", n)
	}
	return l
}

// detachedRunnerProbeTimeout bounds each startup question to a host. A host
// that does not answer in time counts as not running: the session reads
// dormant, and a resume asks again.
const detachedRunnerProbeTimeout = 15 * time.Second

// detachedRunnerAlive asks the host of a detached claude record whether its
// agent's runner is alive and reports whether it is. A host that
// cannot answer, or an unknown or Windows host, reads as not alive.
func detachedRunnerAlive(settings *config.Settings, rec ledger.Record) bool {
	if settings == nil || rec.Detached == nil || rec.Kind != KindClaude {
		return false
	}
	var host *config.Host
	for i := range settings.Hosts {
		if settings.Hosts[i].ID == rec.Detached.HostID {
			host = &settings.Hosts[i]
			break
		}
	}
	if host == nil || host.Probe == nil || sshhost.IsWindowsOS(host.Probe.OS) {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), detachedRunnerProbeTimeout)
	defer cancel()
	st, err := sshhost.AgentRunStatus(ctx, *host, host.Probe.OS, rec.SessionID)
	if err != nil {
		slog.Warn("session ledger: could not ask a host whether a detached agent runs", "session", rec.SessionID, "host", host.ID, "error", err)
		return false
	}
	return st.Phase == sshhost.RunAlive
}

// detachedRunnersAnswering asks the host of every live detached claude record
// whether its agent's runner is alive, one ssh question per record and all at
// once, and returns the ids that answered "alive". The runner check compares
// the process start time with run.json, so a recycled pid reads as gone.
func detachedRunnersAnswering(l *ledger.Ledger, store config.SettingsStore) map[string]bool {
	var settings *config.Settings
	if store != nil {
		settings = store.Get()
	}
	var mu sync.Mutex
	running := map[string]bool{}
	var wg sync.WaitGroup
	for _, rec := range l.All() {
		if rec.State != ledger.StateLive || rec.Detached == nil || rec.Kind != KindClaude || settings == nil {
			continue
		}
		wg.Add(1)
		go func(rec ledger.Record) {
			defer wg.Done()
			if !detachedRunnerAlive(settings, rec) {
				return
			}
			mu.Lock()
			running[rec.SessionID] = true
			mu.Unlock()
		}(rec)
	}
	wg.Wait()
	return running
}

// importSessionLedgerFromRelayLLM runs once, on a feature build's first
// start with no ledger file yet (plan-broker-and-sessions.md §2 C5): every
// dormant session ImportFromRelayLLM finds in relayLLM's own on-disk
// session store is written into l. Best-effort throughout -- a user with no
// relayLLM install, or whose home directory can't be resolved, simply gets
// an empty ledger, not a failed relay start.
func importSessionLedgerFromRelayLLM(l *ledger.Ledger) {
	home, err := os.UserHomeDir()
	if err != nil {
		slog.Warn("session ledger: could not resolve home directory for relayLLM import", "error", err)
		return
	}
	dir := filepath.Join(home, "Library", "Application Support", "relayLLM", "sessions")
	records, err := ledger.ImportFromRelayLLM(dir)
	if err != nil {
		slog.Warn("session ledger: relayLLM import failed", "dir", dir, "error", err)
		return
	}
	for _, rec := range records {
		if err := l.Put(rec); err != nil {
			slog.Warn("session ledger: import write failed", "session", rec.SessionID, "error", err)
		}
	}
	if len(records) > 0 {
		slog.Info("session ledger: imported dormant sessions from relayLLM", "count", len(records))
	}
}

// waitWithTimeout reports whether wg finished within d.
func waitWithTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// Menu item IDs.
const (
	menuIDSettings          = 2
	menuIDExit              = 3
	menuIDResetSealedStore  = 5
	menuIDPendingEnrolments = 6
	menuIDAllowEnrolment    = 8
	menuIDCancelEnrolment   = 9
	menuIDSvcBase           = 100 // service items start here
)

func runTrayApp() {
	slog.Info("starting tray app")

	platform := NewPlatform()

	// Initialize platform UI first so app delegate exists before tray setup.
	platform.Init()
	slog.Info("platform initialized")

	app, err := startServerCore(serverOptions{Platform: platform})
	if err != nil {
		slog.Error("relay tray cannot start", "config_dir", bridge.ConfigDir(), "error", err)
		os.Exit(1)
	}

	// Set up tray icon.
	slog.Info("setting up tray icon")
	rgba, w, h := CreateIconRGBA()
	platform.SetupTray(rgba, w, h)
	slog.Info("tray icon set up")

	// Build and set initial menu.
	app.updateMenu()
	slog.Info("menu built")

	// Block on the platform run loop (must be on main thread).
	slog.Info("entering run loop")
	platform.Run()
}

// onConfigCommitted is the one subscriber to the config queue's post-commit
// event: every committed change refreshes the UI and converges the listeners
// from tray-owned state. It runs on the queue worker, so it only hands work
// off.
func (a *App) onConfigCommitted() {
	if a.ctx.Err() != nil {
		return
	}
	a.goFunc(a.reconcileListeners)
	a.platform.DispatchToMain(func() {
		a.pushFullSettings()
		a.pushFullProjects()
		a.updateMenu()
		a.pushServiceStatus()
	})
}

// importSettingsEdit is the watcher's callback: a hand edit of settings.json
// enters through the config queue like any other mutation, so it is ordered
// with them. An edit that does not validate leaves the current settings alone.
func (a *App) importSettingsEdit() {
	err := a.serviceQueue.Do(a.ctx, func(context.Context) error {
		_, err := a.importFile()
		return err
	})
	if err != nil && a.ctx.Err() == nil {
		slog.Warn("settings.json edit not applied; keeping the current settings", "error", err)
	}
}

// reconcileListeners converges the remote and model-endpoint listeners on the
// tray's current settings. Each Reconcile is silent when nothing changed and
// logs its own failure; a listener is never fatal to the tray.
func (a *App) reconcileListeners() {
	_ = a.remote.Reconcile()
	a.modelEndpoint.Reconcile()
	a.refreshReadyFile()
}

// onExternalChange dispatches UI updates to the main thread after external
// changes (bridge reconcile, reload). Centralizes the "push settings + update
// menu" pattern so the router doesn't reach into platform dispatch directly.
func (a *App) onExternalChange() {
	// A bridge-driven reconcile means "settings changed underneath you", which
	// is true of the remote block as much as of the MCP list. Waiting for the
	// next poll would work, but this is the one path where relay has already
	// been TOLD, so acting on it costs nothing. In a tracked goroutine because
	// a bind must not run on the main thread, and because the caller is a
	// bridge handler that should not wait on a socket.
	a.goFunc(func() {
		_ = a.remote.Reconcile() // logs its own failure; a listener is never fatal to the tray
		a.refreshReadyFile()
	})
	a.platform.DispatchToMain(func() {
		a.pushFullSettings()
		a.updateMenu()
	})
}

// statusPoller is the tray's health poll: it reaps dead children, samples
// per-service memory usage and pushes service status to the settings WebView.
// It never reads settings.json and is not how a configuration change is
// noticed; that is onConfigCommitted. A slower recovery tick re-converges the
// listeners from tray-owned state in case an event was missed or a listener
// died.
//
// Process-exit menu updates are still event-driven via
// service.Registry.OnProcessExit (see runTrayApp) so a stopped service's
// toggle flips immediately, not on the next 2s tick.
func (a *App) statusPoller() {
	ticker := time.NewTicker(StatusPollInterval)
	defer ticker.Stop()
	recovery := time.NewTicker(RecoveryPollInterval)
	defer recovery.Stop()

	for {
		select {
		case <-a.ctx.Done():
			return
		case <-recovery.C:
			a.reconcileListeners()
			continue
		case <-ticker.C:
		}

		a.registry.CleanupDead()

		// Sample memory off the main thread; ps takes ~5ms and we don't want
		// to block the UI dispatch behind it.
		pidByID := a.registry.PIDsByServiceID()
		roots := make([]int, 0, len(pidByID))
		for _, pid := range pidByID {
			roots = append(roots, pid)
		}
		rssByPID := SampleRSSByRoot(roots)
		rssByID := make(map[string]uint64, len(pidByID))
		for id, pid := range pidByID {
			rssByID[id] = rssByPID[pid]
		}
		a.rssByID.Store(&rssByID)

		a.platform.DispatchToMain(func() {
			a.updateMenu()
			a.pushServiceStatus()
			a.pushEnrolmentRequests()
		})
		// Service status polling makes HTTP calls per service — must stay
		// off-main. pushServiceStatusBatch hops to main itself for the emit.
		a.pushServiceStatusBatch()
	}
}

// updateMenu rebuilds the tray menu JSON and pushes it to the platform.
func (a *App) updateMenu() {
	a.updateMenuWithSettings(config.DisplaySettings(a.store))
}

// countUnapprovedEnrolmentRequests is the tray's "Pending enrolment
// requests: N" — rows still waiting for a human decision, which is what
// "pending" means to the operator reading the menu bar. An already-approved
// row (spec §2: "approved, not yet collected") is not counted here: it
// needs nothing further from this menu, and folding it in would make the
// number go up on an act the operator already took.
func countUnapprovedEnrolmentRequests(views []enrolmentRequestView) int {
	n := 0
	for _, v := range views {
		if !v.Approved {
			n++
		}
	}
	return n
}

// pushEnrolmentRequests refreshes the Remote Clients tab's Pending requests
// panel on every poll tick, the same way pushServiceStatus refreshes
// service state: a network peer's Lodge never calls into the tray (P1 —
// the pending table has no callback hook by design, see
// enrolmentRequestTable's own doc comment), so this tick is the only path
// by which an open Settings window learns a new request arrived without
// the operator switching tabs away and back. Gated on settingsOpen like
// every other tick-driven emit, and on a nil EnrolmentOps for the same
// reason updateMenuWithSettings guards it.
func (a *App) pushEnrolmentRequests() {
	if !a.settingsOpen.Load() || a.ipcCtx == nil || a.ipcCtx.EnrolmentOps == nil {
		return
	}
	a.emitSettingsEvent("onEnrolmentRequestsChanged",
		marshalForUI(pendingEnrolmentRequestViewsOf(a.ipcCtx.EnrolmentOps.PendingRequests(), config.DisplaySettings(a.store))))
}

func (a *App) updateMenuWithSettings(s *config.Settings) {
	type menuItem struct {
		Title   string `json:"title"`
		ID      int    `json:"id"`
		Enabled bool   `json:"enabled"`
		Toggle  bool   `json:"toggle,omitempty"`
		On      bool   `json:"on,omitempty"`
		URL     string `json:"url,omitempty"`
		Aux     string `json:"aux,omitempty"`
		// Key is a Cocoa key equivalent ("," for Settings…, "q" for Quit
		// Relay), always under the default Command modifier — cocoa_darwin.m
		// never sets keyEquivalentModifierMask, so plain Command is what
		// every NSMenuItem gets. Empty means no shortcut, the ordinary case.
		Key string `json:"key,omitempty"`
	}

	// One registry-lock acquisition rather than IsRunning() per service.
	pidByID := a.registry.PIDsByServiceID()
	var rss map[string]uint64
	if p := a.rssByID.Load(); p != nil {
		rss = *p
	}

	var items []menuItem

	// Build menu-item-ID -> service-ID mapping so click handlers resolve by ID,
	// not positional index (which can go stale if services change between builds).
	svcMap := make(map[int]string, len(s.Services))
	emitted := 0
	for i, svc := range s.Services {
		if svc.HideFromMenu {
			continue
		}
		emitted++
		menuID := menuIDSvcBase + i
		_, running := pidByID[svc.ID]

		if svc.ID == config.RelaySessionsServiceID {
			// This is deliberate: the built-in session host is started only
			// by StartAllAutostart's own fully-populated synthesis, never by
			// a hand click (ServiceOps.Start refuses it for the identical
			// reason). The stored record is bare, so a toggle wired to it
			// would turn "on" into a Registry.Start call that fails
			// Validate() -- a one-way-off switch. Showing status text with
			// no toggle avoids offering a control with no working "on" path,
			// rather than building one that silently fails.
			status := "stopped"
			if running {
				status = "running"
			}
			items = append(items, menuItem{Title: fmt.Sprintf("%s (%s)", svc.DisplayName, status), ID: 0})
			continue
		}

		svcMap[menuID] = svc.ID
		var aux string
		if running {
			aux = formatBytes(rss[svc.ID])
		}
		items = append(items, menuItem{
			Title:   svc.DisplayName,
			ID:      menuID,
			Enabled: true,
			Toggle:  true,
			On:      running,
			URL:     svc.URL,
			Aux:     aux,
		})
	}
	a.svcMenuMap = svcMap

	if emitted > 0 {
		items = append(items, menuItem{Title: "-", ID: 0})
	}

	// §5.6's degraded-state surface: a disabled, non-clickable line naming
	// exactly why sealed operations are refusing, so the operator sees this
	// in the menu bar without first opening Settings. ID 0 is already used
	// above for separators, which the click handler ignores the same way.
	if ss, ok := a.store.(*config.FileSettingsStore); ok {
		if reason := ss.SealStatus(); reason != nil {
			items = append(items, menuItem{Title: "⚠ Sealed store: " + reason.Error(), ID: 0})
		}
	}

	// The tray's ENTIRE surface for the enrolment-request channel (spec §2's
	// headline): a count, and nothing else. It is enabled (clickable) so
	// that clicking it opens Settings — the same act "Settings..." below
	// already performs — but that click carries no request id and calls no
	// gated core method; the click handler for this ID is a bare
	// openSettingsWindow, same as menuIDSettings'. What makes this the
	// "no prompt is reachable from the network" property is not that the
	// line is inert, but that NOTHING behind it can mint or approve: a
	// network peer can grow this number to its cap and no further, and
	// every code path from here ends at a window, never a presence prompt.
	// Shown only when there is something to act on, matching the sealed-
	// store warning above: a permanent "Pending enrolment requests: 0" line
	// would be noise on every install that never enables the channel.
	if a.ipcCtx != nil && a.ipcCtx.EnrolmentOps != nil {
		views := a.ipcCtx.EnrolmentOps.PendingRequests()
		n := countUnapprovedEnrolmentRequests(views)
		// The notification is derived from THIS read, on the timer that
		// already runs, so the banner and the line below can never disagree.
		// It is a pull: nothing on the lodge path calls into the tray, and
		// LodgeGeneration is the only fact a network peer can move.
		if a.enrolNotifier == nil {
			a.enrolNotifier = newPendingEnrolmentNotifier(func() time.Time { return clockNow(a.clock) }, a.platform.Notify)
		}
		a.enrolNotifier.tick(a.ipcCtx.EnrolmentOps.LodgeGeneration(), n, len(views), maxPendingEnrolmentRequests, a.settingsOpen.Load())
		if n > 0 {
			items = append(items, menuItem{
				Title:   fmt.Sprintf("Pending enrolment requests: %d", n),
				ID:      menuIDPendingEnrolments,
				Enabled: true,
			})
		}
	}

	items = append(items,
		menuItem{Title: "-", ID: 0},
		menuItem{Title: "Settings...", ID: menuIDSettings, Enabled: true, Key: ","},
		menuItem{Title: "Allow Enrolment…", ID: menuIDAllowEnrolment, Enabled: true},
	)

	// One disabled line per open window, then the one item that closes them
	// all. The lines are derived from the same read the expiry notice uses,
	// on the timer that already runs, so the menu and the notice never
	// disagree about what is open.
	open := a.enrolmentWindowOps().statusOf(s, clockNow(a.clock))
	a.noteEnrolmentExpiry(open, s)
	now := clockNow(a.clock)
	for _, v := range open {
		items = append(items, menuItem{Title: enrolmentMenuLine(v, now), ID: 0})
	}
	if len(open) > 0 {
		items = append(items, menuItem{Title: "Cancel Enrolment", ID: menuIDCancelEnrolment, Enabled: true})
	}

	items = append(items,
		menuItem{Title: "-", ID: 0},
		menuItem{Title: "Reset Sealed Store...", ID: menuIDResetSealedStore, Enabled: true},
		menuItem{Title: "-", ID: 0},
		// "Exit" is not a macOS word; every system app calls this Quit.
		menuItem{Title: "Quit Relay", ID: menuIDExit, Enabled: true, Key: "q"},
	)

	data, err := json.Marshal(items)
	if err != nil {
		slog.Error("failed to marshal menu items", "error", err)
		return
	}
	jsonStr := string(data)
	// Skip the cgo hop when nothing has changed — avoids redraw churn while
	// the menu is open and the 2s poll keeps firing.
	if jsonStr == a.lastMenuJSON {
		return
	}
	a.lastMenuJSON = jsonStr
	a.platform.UpdateMenu(jsonStr)
}

// settingsPageRemoteClients is web/src/app.js's showPage id for the Remote
// Clients tab. The sidebar's own onclick attributes in web/shell.html use the
// same string; changing one without the other opens the window on nothing.
const settingsPageRemoteClients = "remote"

// openRemoteClientsPage is the whole of what the pending-enrolments menu line
// and the notification banner do when clicked: put the window that can show
// the request on screen, on the page that shows it. It carries no request id
// and calls no gated core method — the approval still happens from the panel,
// behind enrolment.sign's own prompt.
//
// Two arms, for the reason openAllowEnrolmentSheet's doc comment gives in full: a
// window that is not up yet has no document to receive an emit, because
// cocoa_settings_eval_js drops a script when no WebView exists and
// OpenSettings loads its document asynchronously — so a cold open can only be
// told through its first paint. A window that IS up is never reloaded by
// OpenSettings, so for that one the emit is the only channel. Getting this
// wrong is not cosmetic here: the banner exists to put the operator in front
// of the request, and landing them on Services is landing them nowhere.
func (a *App) openRemoteClientsPage() {
	if a.settingsOpen.Load() {
		a.emitSettingsEvent("showPage", settingsPageRemoteClients)
	} else {
		a.pendingSettingsPage = settingsPageRemoteClients
	}
	a.openSettingsWindow()
}

// onNotificationClick is the banner's click handler, reached from Cocoa's
// UNUserNotificationCenter delegate. Same act as the tray line, deliberately:
// two surfaces, one destination, nothing gated behind either.
func (a *App) onNotificationClick() {
	a.openRemoteClientsPage()
}

// onMenuClick is called from the platform menu action on the main thread.
func (a *App) onMenuClick(itemID int) {
	switch {
	case itemID == menuIDSettings:
		a.openSettingsWindow()

	case itemID == menuIDAllowEnrolment:
		a.openAllowEnrolmentSheet()

	case itemID == menuIDCancelEnrolment:
		a.cancelEnrolment()

	case itemID == menuIDResetSealedStore:
		a.confirmAndResetSealedStore()

	case itemID == menuIDPendingEnrolments:
		// Opens Settings and nothing else — see the menu item's own
		// comment. Approving or refusing a request happens from the
		// Remote Clients tab this opens, never from the tray itself.
		a.openRemoteClientsPage()

	case itemID == menuIDExit:
		a.cleanup()
		os.Exit(0)

	case itemID >= menuIDSvcBase:
		a.toggleService(itemID)
	}
}

// settingsPageAllowEnrolment is the initial-page id that opens Settings with
// the Allow Enrolment sheet up. web/src/app.js reads it from INITIAL_PAGE; it
// is not a sidebar page.
const settingsPageAllowEnrolment = "allow-enrolment"

// openAllowEnrolmentSheet puts the Allow Enrolment sheet in front of the
// operator. It opens the sheet and nothing else: the presence prompt for a
// target is asked only when the operator chooses one and presses Open, from
// the sheet's IPC door. relay is LSUIElement, so Settings is the only surface
// this app has for showing anything.
//
// This is subtle: cocoa_settings_eval_js drops a script when no WebView
// exists yet, and OpenSettings loads its document asynchronously, so a window
// that is not up can only be told through its first paint. One that IS up is
// never reloaded by OpenSettings, so for that one the emit is the only
// channel. Hence the two arms rather than a single call.
//
// This is deliberate: onMenuClick runs on the Cocoa main thread, and every
// gated core reaches a real LocalAuthentication provider whose Evaluate blocks
// the calling goroutine until an async completion handler fires, which needs
// that same thread's run loop pumped. A gated call made here would deadlock
// against itself (§6.5), so gated calls run in tracked goroutines and every UI
// touch after them hops back to main.
func (a *App) openAllowEnrolmentSheet() {
	if a.settingsOpen.Load() {
		a.emitSettingsEvent("openAllowEnrolmentSheet")
	} else {
		a.pendingSettingsPage = settingsPageAllowEnrolment
	}
	a.openSettingsWindow()
}

// cancelEnrolment closes every open enrolment window. The close is ungated,
// but it writes the store, which must not happen on the main thread the menu
// click arrives on.
func (a *App) cancelEnrolment() {
	a.goFunc(func() {
		window := a.enrolmentWindowOps()
		if window == nil {
			return
		}
		closed, err := window.Close(a.ctx, auditViaTray)
		if err != nil {
			slog.Error("failed to close the enrolment windows from the tray", "error", err)
		}
		if closed == nil {
			closed = []string{}
		}
		a.platform.DispatchToMain(func() {
			a.emitSettingsEvent("onEnrolmentWindowsClosed", enrolmentWindowCloseResult{Closed: closed})
			a.updateMenu()
		})
	})
}

func (a *App) enrolmentWindowOps() *EnrolmentWindowOps {
	if a.ipcCtx == nil || a.ipcCtx.EnrolmentOps == nil {
		return nil
	}
	return a.ipcCtx.EnrolmentOps.Window
}

// enrolmentMenuLine is the disabled line for one open window,
// "Enrolment open: <target> · m:ss left".
func enrolmentMenuLine(v enrolmentWindowView, now time.Time) string {
	name := v.ProfileName
	if name == "" {
		name = v.ProfileID
	}
	return fmt.Sprintf("Enrolment open: %s · %s left", enrolmentTargetLabel(v.Target, name), enrolmentCountdown(v.Expires, now))
}

// enrolmentCountdown formats the time left as m:ss, never negative. An expiry
// that does not parse reads as none left, matching how every window treats it.
func enrolmentCountdown(expires string, now time.Time) string {
	var d time.Duration
	if at, err := time.Parse(time.RFC3339, expires); err == nil {
		d = at.Sub(now).Round(time.Second)
	}
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%d:%02d", int(d/time.Minute), int(d/time.Second)%60)
}

// noteEnrolmentExpiry raises "Enrolment closed; nothing enrolled." when a
// Home|Work window or relay sign-in code the menu showed has gone because its
// time ran out unused.
//
// This is subtle: the decision reads the stores' own state, not poll timing. A
// consume or a cancel clears the record; an expiry leaves it in place, past its
// time, until the next open replaces it. So a target that left the open list
// while its record is still stored expired unused, and one whose record is gone
// was consumed or cancelled. The access-profile window raises its own notice
// from the supervisor. Main thread only.
func (a *App) noteEnrolmentExpiry(open []enrolmentWindowView, s *config.Settings) {
	current := map[string]bool{}
	for _, v := range open {
		current[v.Target] = true
	}
	expired := false
	for target := range a.shownEnrolment {
		if current[target] {
			continue
		}
		switch {
		case target == enrolmentWindowTargetHomeWork && s.EveEnrolment != nil,
			target == enrolmentWindowTargetRelayLogin && s.LoginBootstrap != nil:
			expired = true
		}
	}
	a.shownEnrolment = map[string]bool{}
	for _, t := range []string{enrolmentWindowTargetHomeWork, enrolmentWindowTargetRelayLogin} {
		if current[t] {
			a.shownEnrolment[t] = true
		}
	}
	if expired {
		a.platform.Notify("Relay", "Enrolment closed; nothing enrolled.")
	}
}

// confirmAndResetSealedStore is the tray's break-glass menu item (§5.6
// clause 5): "Reset Sealed Store…". There is no separate confirmation panel
// in front of the presence prompt — the prompt itself, via
// sealedResetReason, is the one surface guaranteed to work regardless of
// what a degraded store lets the Settings WebView render, and answering it
// (with the login password) IS the confirmation. A cancelled or refused
// prompt leaves every file untouched; resetSealedStore only starts deleting
// after Require succeeds.
//
// Runs in a tracked goroutine for the reason openAllowEnrolmentSheet gives:
// resetSealedStore reaches the real presence gate, which must never be
// invoked from the Cocoa main thread onMenuClick runs on (§6.5).
func (a *App) confirmAndResetSealedStore() {
	a.goFunc(func() {
		if err := a.resetSealed(a.ctx, auditViaTray); err != nil {
			slog.Error("sealed store reset failed", "error", err)
		}
	})
}

func (a *App) toggleService(menuItemID int) {
	svcID, ok := a.svcMenuMap[menuItemID]
	if !ok {
		return
	}
	if svcID == config.RelaySessionsServiceID {
		// Not reachable through a click today -- updateMenuWithSettings
		// never puts this id in svcMenuMap -- but ServiceOps guards the
		// identical Registry.Start hazard at every path that reaches it
		// (Create, Update, Start), not just the ones currently wired up.
		// Same discipline here.
		slog.Error("service toggle refused: relaysessions is relay's built-in session host and cannot be toggled from the tray")
		return
	}
	if a.registry.IsRunning(svcID) {
		a.goFunc(func() {
			if a.serviceOps != nil {
				if err := a.serviceOps.Stop(logging.ContextWithTrace(context.Background(), logging.NewTraceID()), svcID); err != nil {
					slog.Error("service toggle failed", "id", svcID, "error", err)
				}
			} else {
				a.registry.Stop(svcID)
			}
			a.platform.DispatchToMain(func() {
				a.pushServiceStatus()
				a.updateMenu()
			})
		})
	} else {
		// Off-main, same as Stop above: Start spawns a process and does its
		// own file I/O (pidfile, log dir), neither of which belongs on the
		// menu-click thread.
		a.goFunc(func() {
			var err error
			if a.serviceOps != nil {
				err = a.serviceOps.Start(logging.ContextWithTrace(context.Background(), logging.NewTraceID()), svcID)
			} else {
				svc, _ := config.FindServiceByID(config.FreshSettings(a.store), svcID)
				if svc == nil {
					err = fmt.Errorf("service %q not found", svcID)
				} else {
					err = a.registry.Start(svc)
				}
			}
			if err != nil {
				slog.Error("service toggle failed", "error", err)
			}
			a.platform.DispatchToMain(func() {
				a.pushServiceStatus()
				a.updateMenu()
			})
		})
	}
}

// cleanup performs graceful shutdown using a drain-then-kill ordering:
//
//  1. Cancel the app context — signals all goroutines to stop.
//  2. Stop accepting bridge connections — no new requests can arrive.
//  3. Kill external MCPs — in-flight CallTool requests fail fast,
//     unblocking any bridge handlers waiting on MCP responses.
//  4. Drain bridge handlers — wait for in-flight handlers to finish
//     and remove the socket file.
//  5. Kill service processes — runs last so it catches any service
//     spawned by a ReloadService handler that raced with shutdown.
//  6. Wait for tracked goroutines (statusPoller, Serve loop), bounded by
//     cleanupWaitGroupTimeout rather than a plain Wait() — see its own doc
//     comment for why an unbounded wait here can deadlock permanently.
//
// This ordering prevents orphan service processes: if StopAll ran before
// bridge handlers drained, a concurrent Reload handler could Start a new
// service after StopAll's snapshot, leaving it unmanaged.
func (a *App) cleanup() {
	a.cleanupOnce.Do(func() {
		a.removeReadyFile()
		a.cancel()
		if a.bridgeServer != nil {
			a.bridgeServer.StopAccepting()
		}
		a.remote.StopAccepting()
		if a.modelEndpoint != nil {
			a.modelEndpoint.Close()
		}
		a.extMgr.StopAll()
		if a.bridgeServer != nil {
			a.bridgeServer.Close()
		}
		if a.stopSettingsWatch != nil {
			a.stopSettingsWatch()
		}
		if a.serviceQueue != nil {
			a.serviceQueue.Close()
			ctx, cancel := context.WithTimeout(context.Background(), cleanupWaitGroupTimeout)
			if err := a.serviceQueue.Shutdown(ctx); err != nil {
				slog.Warn("service command queue did not shut down cleanly", "error", err)
			}
			cancel()
		}
		// Drained after the MCPs die, like the bridge: an in-flight remote
		// CallTool fails fast rather than holding the drain open, and its
		// completion record is still written because the audit log is closed
		// last of all.
		a.remote.Close()
		// Stop the frontend server before the children that proxy to relayLLM
		// — once relayLLM dies, in-flight proxied requests fail with 502
		// rather than hanging.
		if a.frontendServer != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			a.frontendServer.Shutdown(ctx)
			cancel()
		}
		a.registry.StopAll()
		// Unlink the LLM channel sockets after the children that depend on
		// them have stopped. Tokens persist in-memory until the process
		// exits.
		if a.frontendChannel != nil {
			a.frontendChannel.Close()
		}
		if !waitWithTimeout(&a.wg, cleanupWaitGroupTimeout) {
			slog.Warn("cleanup: tracked goroutines did not finish within the shutdown grace period; exiting anyway",
				"timeout", cleanupWaitGroupTimeout)
		}
		// Last: nothing can produce a tool call any more, so drain the audit
		// queue and close the log. Closing earlier would drop the shutdown-time
		// events that a post-incident review is most likely to want.
		a.audit.Close()
		if a.releaseOwnership != nil {
			a.releaseOwnership()
		}
	})
}
