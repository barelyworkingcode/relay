package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"relaygo/bridge"
)

// appInstance is the singleton tray app, set by runTrayApp and read by Cocoa
// callbacks (exported Go functions called from cgo). This global is required
// because cgo //export functions cannot capture closures or accept user data.
// It is safe because the tray app is inherently single-instance.
var appInstance *App

// App is the main tray application state.
type App struct {
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	store          SettingsStore
	platform       Platform
	extMgr         *ExternalMcpManager
	registry       ServiceManager
	bridgeServer   *bridge.BridgeServer
	frontendServer *FrontendServer
	ipcCtx         *IPCContext // pre-built once, reused on every IPC call
	// settingsOpen gates UI emits on whether the Settings window is up. Written
	// on the main thread (open/close) but read from background goroutines (the
	// status poller, HTTP-driven project refresh), so it must be atomic.
	settingsOpen   atomic.Bool
	cleanupOnce    sync.Once
	svcMenuMap     map[int]string // menu item ID -> service ID

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

	// lastStatusBatchDigest fingerprints the most recently emitted service-
	// status batch (FetchedAt zeroed). Identical batches across ticks are
	// suppressed so the inspector's WebView doesn't re-render every 2s for
	// no reason. Pointer so atomic.CompareAndSwap on a content-derived value
	// is straightforward.
	lastStatusBatchDigest atomic.Pointer[[32]byte]
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
	menuIDSettings = 2
	menuIDExit     = 3
	menuIDSvcBase  = 100 // service items start here
)


func runTrayApp() {
	slog.Info("starting tray app")

	platform := NewPlatform()

	// Initialize platform UI first so app delegate exists before tray setup.
	platform.Init()
	slog.Info("platform initialized")

	store := NewSettingsStore()

	// Ensure admin secret is generated and persisted on first launch.
	if err := store.EnsureInitialized(); err != nil {
		slog.Error("failed to initialize settings", "error", err)
		os.Exit(1)
	}
	settings := store.Get()
	slog.Info("settings loaded")

	// External MCP manager with injected callback for OAuth token refresh persistence.
	extMgr := NewExternalMcpManager(
		func(mcpID string, oauth *OAuthState) {
			store.With(func(s *Settings) { s.UpdateOAuthState(mcpID, oauth) })
		},
	)

	ctx, cancel := context.WithCancel(context.Background())

	registry := NewServiceRegistry()

	app := &App{
		ctx:      ctx,
		cancel:   cancel,
		store:    store,
		platform: platform,
		extMgr:   extMgr,
		registry: registry,
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
	}

	// Create and start bridge server.
	router := &appRouter{
		store:    store,
		tools:    extMgr,
		services: app.registry,
		enhanced: enhancedRegistry,
		onChange: app.onExternalChange,
	}
	// router implements SkillLister (ListTools); set it on the IPC context
	// now that it exists so the Projects-tab "Regen Now" button can run.
	app.ipcCtx.SkillLister = router
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
	frontend, err := NewFrontendServer(store, extMgr, extMgr, frontendEndpoint, enhancedRegistry, router, onProjectsChanged)
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

	// Start external MCPs and autostart services before the bridge accepts
	// connections, so tool lists and service status are populated when the
	// first client connects.
	extMgr.StartAll(ctx, settings.ExternalMcps)
	// Reclaim orphans from a previous tray session that was killed before
	// the reaper could SIGTERM its children. Without this, autostart of any
	// port-binding service (scheduler, kokoro, whisper, comfy) fails with
	// EADDRINUSE on every restart. Must run before StartAllAutostart.
	app.registry.ReclaimOrphans(settings.Services)
	app.registry.StartAllAutostart(settings.Services)

	app.goFunc(func() { bs.Serve() })
	slog.Info("bridge server started")


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

		a.platform.DispatchToMain(func() {
			// store.Get() deep-copies, so prefer the already-loaded snapshot
			// from ReloadIfChanged when present and pay the copy only on miss.
			cur := s
			if cur == nil {
				cur = a.store.Get()
			}
			a.updateMenuWithSettings(cur)
			a.pushServiceStatus()
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

	items = append(items,
		menuItem{Title: "Settings...", ID: menuIDSettings, Enabled: true},
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

// onMenuClick is called from the platform menu action on the main thread.
func (a *App) onMenuClick(itemID int) {
	switch {
	case itemID == menuIDSettings:
		a.openSettingsWindow()

	case itemID == menuIDExit:
		a.cleanup()
		os.Exit(0)

	case itemID >= menuIDSvcBase:
		a.toggleService(itemID)
	}
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
		a.extMgr.StopAll()
		if a.bridgeServer != nil {
			a.bridgeServer.Close()
		}
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
	})
}
