package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"relaygo/bridge"
	"relaygo/presence"
	"relaygo/sealed"
)

// appInstance is the singleton tray app, set by runTrayApp and read by Cocoa
// callbacks (exported Go functions called from cgo). This global is required
// because cgo //export functions cannot capture closures or accept user data.
// It is safe because the tray app is inherently single-instance.
var appInstance *App

// App is the main tray application state.
type App struct {
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	store        SettingsStore
	platform     Platform
	extMgr       *ExternalMcpManager
	registry     ServiceManager
	bridgeServer *bridge.BridgeServer
	// remote supervises the mTLS listener remote clients reach relay through:
	// it binds, rebinds and stops as `remote.enabled` / `remote.listen` change,
	// so neither needs a restart to take effect. Holds no listener at all
	// whenever no remote block is configured, which is the overwhelmingly
	// common case; every method on it is nil-safe so nothing branches here.
	remote         *RemoteSupervisor
	frontendServer *FrontendServer
	ipcCtx         *IPCContext // pre-built once, reused on every IPC call
	// audit is the tool-call recorder. Nil when auditing is disabled or failed
	// to start; every method on it is nil-safe.
	audit *AuditRecorder
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

	// pendingLoginCode is a just-minted bootstrap code waiting for the first
	// paint of a Settings window that is not open yet. Main-thread only, like
	// lastMenuJSON and svcMenuMap.
	pendingLoginCode *loginCodeView

	// pendingSettingsPage is the page a tray action wants the next Settings
	// window to open on, waiting for the same first paint and for the same
	// reason pendingLoginCode does. Main-thread only. Empty is the ordinary
	// case: the window opens where it always did.
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

// Menu item IDs.
const (
	menuIDSettings          = 2
	menuIDExit              = 3
	menuIDLoginCode         = 4
	menuIDResetSealedStore  = 5
	menuIDPendingEnrolments = 6
	menuIDSvcBase           = 100 // service items start here
)

func runTrayApp() {
	slog.Info("starting tray app")

	platform := NewPlatform()

	// Initialize platform UI first so app delegate exists before tray setup.
	platform.Init()
	slog.Info("platform initialized")

	// The keychain keyring is constructed here and NOWHERE else in the
	// program (§5.3.3, AC-29): this is the tray's own path, and the ACL
	// binds to whatever code identity SecTrustedApplicationCreateFromPath
	// reads from it. A CLI process carries relay's own code identity too,
	// so a second call site here would satisfy the very ACL this design
	// depends on the CLI never asking — TestSeal_NoCLIPathReachesTheKeychain
	// is what keeps that true.
	configDir := bridge.ConfigDir()
	keyring := sealed.NewKeychainKeyring(resolveRelayBin())
	store, err := ResolveSealedStore(configDir, keyring)
	if err != nil {
		slog.Error("failed to resolve the sealed store", "error", err)
		os.Exit(1)
	}

	// Ensure admin secret is generated and persisted on first launch, run
	// migration on first encounter with a plaintext settings.json (§4.7),
	// or — on a degraded store — do nothing beyond loading what the clear
	// fields already say (§5.6 clause 1: relay starts, it does not exit).
	if err := store.EnsureInitialized(); err != nil {
		slog.Error("failed to initialize settings", "error", err)
		os.Exit(1)
	}
	if reason := store.SealStatus(); reason != nil {
		// §5.6: the read half works in full from here on — relay grant,
		// relay audit, every list, the Settings window (rendering sealed
		// values as sealUnavailablePlaceholder) and the tray menu below.
		// Every sealed operation refuses naming this same reason; nothing
		// here creates or adopts a replacement key (§5.5.1).
		slog.Warn("sealed store is degraded — sealed operations will refuse until this is resolved", "reason", reason)
	}
	settings := store.Get()
	slog.Info("settings loaded")

	// The real LocalAuthentication provider is constructed in exactly one
	// place: here. The hermetic suite never calls runTrayApp, so it never
	// reaches this line — presence.LocalAuthProviderConstructions() staying
	// at zero across `go test ./...` is what AC-20 checks instead of hoping.
	presenceProvider := presence.NewLocalAuthProvider()
	presenceGate, err := presence.NewGate(presenceProvider)
	if err != nil {
		slog.Error("failed to construct the presence gate", "error", err)
		os.Exit(1)
	}

	// External MCP manager with injected callback for OAuth token refresh persistence.
	extMgr := NewExternalMcpManager(
		func(mcpID string, oauth *OAuthState) {
			store.With(func(s *Settings) { s.UpdateOAuthState(mcpID, oauth) })
		},
	)

	ctx, cancel := context.WithCancel(context.Background())

	registry := NewServiceRegistry()

	app := &App{
		ctx:           ctx,
		cancel:        cancel,
		store:         store,
		platform:      platform,
		extMgr:        extMgr,
		registry:      registry,
		presenceGate:  presenceGate,
		sealedKeyring: keyring,
		configDir:     configDir,
	}

	// Event-driven menu updates: rebuild tray status dots immediately when
	// any managed process exits, instead of waiting for the next settings
	// file change or user interaction. Called from the reaper goroutine.
	// Guarded by ctx check to avoid dispatching UI work after shutdown
	// starts — the main thread may be blocked in cleanup or state may be
	// partially torn down.
	registry.OnProcessExit = func() {
		if ctx.Err() != nil {
			return
		}
		app.platform.DispatchToMain(func() {
			app.updateMenu()
			app.pushServiceStatus()
		})
	}
	appInstance = app

	// Enhanced-services registry: bridge handler writes on RegisterManifest;
	// service_registry calls Forget on exit; the front-door dispatcher reads.
	enhancedRegistry := NewEnhancedServiceRegistry(nil)
	registry.Enhanced = enhancedRegistry

	// serviceOps is the one core behind both the Services tab (via
	// app.ipcCtx.Ops below) and RegisterServiceRoutes on the frontend server
	// (ADR-014) — a service started from curl and one started from the tray
	// share the same validation and the same Registry. OnChange may fire from
	// an HTTP-server goroutine or an IPC GoFunc, so it hops to main before
	// touching the WebView or rebuilding the NSMenu.
	serviceOps := &ServiceOps{
		Store:    store,
		Registry: registry,
		Gate:     presenceGate,
		OnChange: func() {
			app.platform.DispatchToMain(func() {
				app.updateMenu()
				app.pushServiceStatus()
			})
		},
	}

	app.ipcCtx = &IPCContext{
		Ctx:                    ctx,
		Store:                  store,
		UI:                     app,
		Platform:               platform,
		Registry:               app.registry,
		Enhanced:               enhancedRegistry,
		UpdateMenu:             app.updateMenu,
		PushServiceStatusBatch: app.pushServiceStatusBatch,
		GoFunc:                 app.goFunc,
		NotifyReconcile:        bridge.SendReconcile,
		NotifyReloadMcp:        bridge.SendReloadMcp,
		Tools:                  extMgr,
		Enumerate:              extMgr,
		Ops:                    serviceOps,
	}

	// Tool-call audit log. A failure here is logged and auditing stays off
	// rather than taking the tray down with it: the recorder is observability,
	// not an authorization control, and relay is more useful running blind than
	// not running at all. The Tool Calls tab surfaces the disabled state.
	audit := startAuditRecorder(store.Get())
	app.audit = audit
	// serviceOps is constructed above, before the audit recorder exists, so
	// its Issuance field is wired here rather than in the literal. Gate was
	// already set there — the real provider has no such ordering constraint.
	serviceOps.Issuance = issuanceAuditorOrNil(audit)

	// A dead external MCP used to be invisible: every client got
	// `read response: EOF` and nothing in relay said the server behind them was
	// gone (issue #39). The supervisor now reports every death, restart, and
	// abandonment, and this is where those reports become rows in the log an
	// operator is told to treat as ground truth. Installed here rather than at
	// construction because the manager is built before the recorder exists.
	extMgr.SetHealthObserver(audit.RecordMcpSupervision)

	// Create and start bridge server.
	router := &appRouter{
		store:    store,
		tools:    extMgr,
		services: app.registry,
		enhanced: enhancedRegistry,
		onChange: app.onExternalChange,
		audit:    audit,
	}
	// router implements SkillLister (ListTools); set it on the IPC context
	// now that it exists so the Projects-tab "Regen Now" button can run.
	app.ipcCtx.SkillLister = router
	app.ipcCtx.Audit = audit
	router.serviceOps = serviceOps

	// credentialOps is admin_op's only door onto CredentialOps (ADR-017
	// implementation spec S6): `relay credential mint|revoke` is host-only
	// and has no Settings tab of its own, so it is wired straight onto the
	// router rather than threaded through IPCContext the way the other five
	// cores are.
	credentialOps := &CredentialOps{Store: store, Gate: presenceGate, Issuance: issuanceAuditorOrNil(audit)}
	router.credentialOps = credentialOps

	// auditOps is the one core behind both the Tool Calls tab (via
	// app.ipcCtx.AuditOps) and RegisterAuditRoutes on the frontend server
	// (ADR-014). Read-only, so unlike serviceOps/enrolmentOps it carries no
	// OnChange — a query changes nothing another view needs to learn about.
	auditOps := &AuditOps{Audit: audit}
	app.ipcCtx.AuditOps = auditOps

	// enrolmentOps is the one core behind both the Remote Clients tab (via
	// app.ipcCtx.EnrolmentOps) and RegisterEnrolmentRoutes on the frontend
	// server (ADR-014). pushFullSettings already carries enrolments and the
	// remote block, so reusing it here is what keeps an open Settings window
	// in sync with an enrolment created or revoked from curl.
	enrolmentOps := &EnrolmentOps{
		Store: store,
		Audit: audit,
		Gate:  presenceGate,
		OnChange: func() {
			app.platform.DispatchToMain(app.pushFullSettings)
		},
	}
	app.ipcCtx.EnrolmentOps = enrolmentOps
	router.enrolmentOps = enrolmentOps

	// loginOps is the one core behind the tray's login-code item, the
	// Passkeys tab and `relay login` (ADR-016). It gets no HTTP door: passkey
	// registration is a host-side act, and a route for it would be the
	// self-service enrolment ADR-010 decision 8 refuses. pushFullSettings
	// carries passkeys and live sessions, so reusing it here is what keeps an
	// open window in sync with a revoke made from a terminal.
	loginOps := &LoginOps{
		Store: store,
		Audit: audit,
		Gate:  presenceGate,
		OnChange: func() {
			app.platform.DispatchToMain(app.pushFullSettings)
		},
	}
	app.loginOps = loginOps
	app.ipcCtx.LoginOps = loginOps
	router.loginOps = loginOps

	// mcpOps is the one core behind both the MCP Servers tab (via
	// app.ipcCtx.McpOps) and RegisterMcpRoutes on the frontend server
	// (ADR-014) -- an MCP added from curl and one added from the tray share
	// the same SSRF guard, discovery, and reconcile. Only add/remove get an
	// HTTP door; authenticate and reset-permissions stay IPC-only (McpOps
	// doc comment explains why) and keep calling this same instance.
	mcpOps := &McpOps{
		Store:           store,
		Ctx:             ctx,
		Gate:            presenceGate,
		Issuance:        issuanceAuditorOrNil(audit),
		NotifyReconcile: bridge.SendReconcile,
		NotifyReloadMcp: bridge.SendReloadMcp,
		OnChange: func() {
			app.platform.DispatchToMain(app.pushFullSettings)
		},
	}
	app.ipcCtx.McpOps = mcpOps
	router.mcpOps = mcpOps

	// Live-tail the Tool Calls tab. Fires on the audit writer goroutine, so
	// hop to main before touching the WebView.
	audit.SetSink(func(ev AuditEvent) {
		if !app.settingsOpen.Load() {
			return
		}
		app.platform.DispatchToMain(func() { app.emitSettingsEvent("onAuditEvent", ev) })
	})
	// Share the in-memory service token store between the router (auth) and
	// the registry (token lifecycle). Tokens live only in memory — no cleanup
	// needed on crash.
	registry.TokenStore = &router.serviceTokens

	registry.FrontendChannel = NewFrontendChannel()
	bs, err := bridge.NewBridgeServer(ctx, router)
	if err != nil {
		slog.Error("failed to start bridge server", "error", err)
		os.Exit(1)
	}
	app.bridgeServer = bs

	// Materialize the channel up front so the frontend HTTP server can bind
	// before any client (Eve, scheduler) tries to dial it. Spawned services
	// inherit the same credentials via service_registry.
	frontendEndpoint, err := registry.FrontendChannel.Ensure()
	if err != nil {
		slog.Error("failed to provision frontend channel", "error", err)
		os.Exit(1)
	}
	// Existing frontend consumers (Eve, relayScheduler) hold RELAY_FRONTEND_TOKEN;
	// this mints or refreshes the read+configure credential that lets them keep
	// authenticating unchanged (ADR-015 decision 3). Not fatal: a relay that
	// fails this still starts, it just leaves those consumers to 401 until the
	// next restart retries the migration.
	if err := store.With(func(s *Settings) {
		migrateFrontendTokenToCredential(s, frontendEndpoint.Token)
	}); err != nil {
		slog.Error("failed to migrate legacy frontend token to a credential", "error", err)
	}
	// onProjectsChanged refreshes the tray Settings webview when projects
	// mutate via the HTTP API (Eve, scheduler, CLI). Local IPC mutations
	// fire their own emit events; this fan-out keeps the in-tray Projects
	// tab in sync with edits made elsewhere.
	onProjectsChanged := func() {
		if app != nil {
			// Fires on an HTTP-server goroutine (Eve/scheduler/CLI). pushFullProjects
			// calls WKWebView's evaluateJavaScript, which is main-thread-only, so hop
			// to main rather than touching the WebView off-thread.
			app.platform.DispatchToMain(app.pushFullProjects)
		}
	}
	// projectOps is the one core behind both the Projects tab (via
	// app.ipcCtx.ProjectOps) and RegisterProjectRoutes on the frontend
	// server (ADR-014) — a project created from curl and one created from
	// the tray share the presence gate and the audit record.
	projectOps := &ProjectOps{
		Store:    store,
		Gate:     presenceGate,
		Issuance: issuanceAuditorOrNil(audit),
		OnChange: func() {
			app.platform.DispatchToMain(app.pushFullProjects)
		},
	}
	app.ipcCtx.ProjectOps = projectOps
	frontend, err := NewFrontendServer(store, extMgr, extMgr, extMgr, frontendEndpoint, enhancedRegistry, router, onProjectsChanged, serviceOps, enrolmentOps, auditOps, mcpOps, projectOps, NewCredentialAuthorizer(store), controlAuditorOrNil(audit))
	if err != nil {
		slog.Error("failed to start frontend server", "error", err)
		os.Exit(1)
	}
	app.frontendServer = frontend
	app.goFunc(func() {
		if err := frontend.Serve(); err != nil {
			slog.Error("frontend server exited with error", "error", err)
		}
	})
	if addr := os.Getenv(EnvAPIListen); addr != "" {
		if err := frontend.ListenLoopback(addr); err != nil {
			// Refused rather than downgraded to the socket alone: someone who
			// asked for a TCP door and silently did not get one would debug
			// the wrong thing.
			slog.Error("failed to bind API listener", "error", err)
			os.Exit(1)
		}
		app.goFunc(func() {
			if err := frontend.ServeLoopback(); err != nil {
				slog.Error("API listener exited with error", "error", err)
			}
		})
	}

	// Start external MCPs and autostart services before the bridge accepts
	// connections, so tool lists and service status are populated when the
	// first client connects.
	extMgr.StartAll(ctx, settings.ExternalMcps)
	// Regenerate SKILL.md on startup for every project with GenerateSkill: true,
	// so generated skills reflect the current tool surface after a relay restart.
	// Runs after StartAll (MCP handshakes have completed) so tool lists are
	// populated; best-effort — errors are logged inside regenProjectSkills.
	router.regenProjectSkills(ctx, settings)
	// Reclaim orphans from a previous tray session that was killed before
	// the reaper could SIGTERM its children. Without this, autostart of any
	// port-binding service (scheduler, kokoro, whisper, comfy) fails with
	// EADDRINUSE on every restart. Must run before StartAllAutostart.
	app.registry.ReclaimOrphans(settings.Services)
	app.registry.StartAllAutostart(settings.Services)

	app.goFunc(func() { bs.Serve() })
	slog.Info("bridge server started")

	// The remote listener sits BESIDE the bridge, never in front of it: the
	// Unix socket keeps its ten request types, and this one has two. A
	// configuration error here is fatal to the listener and to nothing else —
	// relay without a remote listener is relay as it has always been, whereas
	// a remote listener that started anyway despite (say) disabled auditing is
	// exactly the system ADR-010 exists to prevent.
	//
	// Started through the supervisor rather than bound directly, so the same
	// code path that opens it at launch is the one that moves or closes it when
	// settings change — a listener whose configuration only applied at startup
	// made `remote.listen` the one setting in relay that needed a quit, and
	// made `audit.enabled: false` a refusal that only held until the next
	// launch. statusPoller drives the convergence from here on.
	app.remote = NewRemoteSupervisor(ctx, store, router, audit, projectOps, extMgr.AllMcpSurfaces, app.goFunc)
	app.remote.Reconcile() // logs its own failure; a listener is never fatal to the tray

	// Wires the Remote Clients tab's Pending requests panel and the tray's
	// passive count line onto the SAME pending-enrolment-request table the
	// listener above just started serving — the table is built once, inside
	// the supervisor, and outlives every rebind of the listener around it
	// (RemoteSupervisor.EnrolTable's own doc comment), so this assignment
	// needs no further synchronization: nothing reads enrolmentOps.Requests
	// before this line runs.
	enrolmentOps.Requests = app.remote.EnrolTable()

	// Set up tray icon.
	slog.Info("setting up tray icon")
	rgba, w, h := CreateIconRGBA()
	platform.SetupTray(rgba, w, h)
	slog.Info("tray icon set up")

	// Build and set initial menu.
	app.updateMenu()
	slog.Info("menu built")

	// Catch termination signals so child processes get cleaned up.
	// This goroutine is NOT tracked via goFunc because cleanup() calls
	// wg.Wait() — tracking it would deadlock (waiting for itself to finish).
	//
	// When ctx is cancelled (by cleanup from the Exit menu or Cocoa
	// termination), we call signal.Reset to restore the OS default signal
	// disposition. Without this, signal.Notify continues to intercept
	// SIGTERM/SIGINT with no goroutine reading the channel, making the
	// process unkillable by those signals. Restoring the default lets the
	// kernel terminate the process if cleanup itself hangs.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		select {
		case sig := <-sigCh:
			slog.Info("received signal, cleaning up", "signal", sig)
			app.cleanup()
			os.Exit(0)
		case <-ctx.Done():
			signal.Stop(sigCh)
			return
		}
	}()

	// Poll service status every 2s.
	app.goFunc(app.statusPoller)

	// Block on the platform run loop (must be on main thread).
	slog.Info("entering run loop")
	platform.Run()
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
	a.goFunc(func() { a.remote.Reconcile() })
	a.platform.DispatchToMain(func() {
		a.pushFullSettings()
		a.updateMenu()
	})
}

// statusPoller periodically re-reads settings from disk (when the file's
// modtime changes) to pick up CLI-driven changes, samples per-service memory
// usage, and pushes service status to the settings WebView. The tray menu is
// also rebuilt every tick so the memory readout stays fresh; updateMenu
// short-circuits on the platform when nothing changed.
//
// Process-exit menu updates are still event-driven via
// ServiceRegistry.OnProcessExit (see runTrayApp) so a stopped service's
// toggle flips immediately, not on the next 2s tick.
func (a *App) statusPoller() {
	ticker := time.NewTicker(StatusPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-a.ctx.Done():
			return
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

		s := a.store.ReloadIfChanged()

		// Converge the remote listener on the same tick that picks up settings
		// changes, and off the main thread because it may bind a socket. This
		// is the trigger for every out-of-process edit — `relay enrol`, a
		// hand-edited settings.json, the Settings UI — for the same reason the
		// poll exists at all: relay is not one process, and the tray is not the
		// only writer. Cheap and silent when nothing changed; see
		// RemoteSupervisor.Reconcile for what "nothing changed" means.
		a.remote.Reconcile()

		a.platform.DispatchToMain(func() {
			// store.Get() deep-copies, so prefer the already-loaded snapshot
			// from ReloadIfChanged when present and pay the copy only on miss.
			cur := s
			if cur == nil {
				cur = a.store.Get()
			}
			a.updateMenuWithSettings(cur)
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
	a.updateMenuWithSettings(a.store.Get())
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
		marshalForUI(pendingEnrolmentRequestViewsOf(a.ipcCtx.EnrolmentOps.PendingRequests(), a.store.Get())))
}

func (a *App) updateMenuWithSettings(s *Settings) {
	type menuItem struct {
		Title   string `json:"title"`
		ID      int    `json:"id"`
		Enabled bool   `json:"enabled"`
		Toggle  bool   `json:"toggle,omitempty"`
		On      bool   `json:"on,omitempty"`
		URL     string `json:"url,omitempty"`
		Aux     string `json:"aux,omitempty"`
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
	for i, svc := range s.Services {
		menuID := menuIDSvcBase + i
		svcMap[menuID] = svc.ID
		_, running := pidByID[svc.ID]
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

	if len(s.Services) > 0 {
		items = append(items, menuItem{Title: "-", ID: 0})
	}

	// §5.6's degraded-state surface: a disabled, non-clickable line naming
	// exactly why sealed operations are refusing, so the operator sees this
	// in the menu bar without first opening Settings. ID 0 is already used
	// above for separators, which the click handler ignores the same way.
	if ss, ok := a.store.(*FileSettingsStore); ok {
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
			a.enrolNotifier = newPendingEnrolmentNotifier(nil, a.platform.Notify)
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
		menuItem{Title: "Settings...", ID: menuIDSettings, Enabled: true},
		menuItem{Title: "Show Login Code...", ID: menuIDLoginCode, Enabled: true},
		menuItem{Title: "Reset Sealed Store...", ID: menuIDResetSealedStore, Enabled: true},
		menuItem{Title: "-", ID: 0},
		menuItem{Title: "Exit", ID: menuIDExit, Enabled: true},
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
// Two arms, for the reason showLoginCode's doc comment gives in full: a
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

	case itemID == menuIDLoginCode:
		a.showLoginCode()

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

// showLoginCode mints a bootstrap code and puts it in front of the operator
// in the Settings window. That window is the only surface this app has for
// showing anything — relay is LSUIElement, so there is no dock icon and no
// main window, and the one other menu item with something to show opens it
// too.
//
// This is subtle: cocoa_settings_eval_js drops a script when no WebView
// exists yet, and OpenSettings loads its document asynchronously, so a window
// that is not up can only be told through its first paint. One that IS up is
// never reloaded by OpenSettings, so for that one the emit is the only
// channel. Hence the two arms rather than a single call.
//
// This is deliberate: onMenuClick runs on the Cocoa main thread, and
// login.bootstrap.mint is gated (§6.4) — MintBootstrap now reaches a real
// LocalAuthentication provider whose Evaluate blocks the calling goroutine
// on a channel until the async completion handler fires. Running that on
// the same thread that owns the run loop the dialog needs pumped would
// deadlock the two against each other (§6.5), so the mint happens in a
// tracked goroutine and every UI touch after it hops back to main.
func (a *App) showLoginCode() {
	a.goFunc(func() {
		view, err := a.loginOps.MintBootstrap(a.ctx, auditViaTray)
		if err != nil {
			slog.Error("failed to mint a login code from the tray", "error", err)
			view = loginCodeView{Error: err.Error()}
		}
		a.platform.DispatchToMain(func() {
			if a.settingsOpen.Load() {
				a.emitSettingsEvent("onLoginCodeMinted", view)
			} else {
				a.pendingLoginCode = &view
			}
			a.openSettingsWindow()
		})
	})
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
// Runs in a tracked goroutine for the same reason showLoginCode does:
// resetSealedStore reaches the real presence gate, which must never be
// invoked from the Cocoa main thread onMenuClick runs on (§6.5).
func (a *App) confirmAndResetSealedStore() {
	a.goFunc(func() {
		ss, ok := a.store.(*FileSettingsStore)
		if !ok {
			slog.Error("sealed store reset: store is not file-backed")
			return
		}
		if err := resetSealedStore(a.ctx, a.configDir, ss, a.sealedKeyring, a.presenceGate); err != nil {
			slog.Error("sealed store reset failed", "error", err)
			return
		}
		slog.Warn("sealed store reset: settings.json, the CA and the keychain key were deleted; relay re-initialised with a fresh key")
		a.platform.DispatchToMain(func() {
			a.updateMenu()
			if a.settingsOpen.Load() {
				a.pushFullSettings()
			}
		})
	})
}

func (a *App) toggleService(menuItemID int) {
	svcID, ok := a.svcMenuMap[menuItemID]
	if !ok {
		return
	}
	s := a.store.Get()
	config, _ := s.findServiceByID(svcID)
	if config == nil {
		return
	}

	if a.registry.IsRunning(config.ID) {
		id := config.ID
		a.goFunc(func() {
			a.registry.Stop(id)
			a.platform.DispatchToMain(func() {
				a.pushServiceStatus()
				a.updateMenu()
			})
		})
	} else {
		if err := a.registry.Start(config); err != nil {
			slog.Error("service toggle failed", "error", err)
		}
		a.updateMenu()
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
//  6. Wait for tracked goroutines (statusPoller, Serve loop).
//
// This ordering prevents orphan service processes: if StopAll ran before
// bridge handlers drained, a concurrent Reload handler could Start a new
// service after StopAll's snapshot, leaving it unmanaged.
func (a *App) cleanup() {
	a.cleanupOnce.Do(func() {
		a.cancel()
		if a.bridgeServer != nil {
			a.bridgeServer.StopAccepting()
		}
		a.remote.StopAccepting()
		a.extMgr.StopAll()
		if a.bridgeServer != nil {
			a.bridgeServer.Close()
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
		a.registry.CloseFrontendChannel()
		a.wg.Wait()
		// Last: nothing can produce a tool call any more, so drain the audit
		// queue and close the log. Closing earlier would drop the shutdown-time
		// events that a post-incident review is most likely to want.
		a.audit.Close()
	})
}
