package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/signal"
	"sync"
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
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	store        SettingsStore
	platform     Platform
	extMgr       *ExternalMcpManager
	registry     ServiceManager
	bridgeServer *bridge.BridgeServer
	ipcCtx       *IPCContext // pre-built once, reused on every IPC call
	settingsOpen bool
	cleanupOnce  sync.Once
	svcMenuMap   map[int]string // menu item ID -> service ID
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
	app.ipcCtx = &IPCContext{
		Ctx:             ctx,
		Store:           store,
		UI:              app,
		Platform:        platform,
		Registry:        app.registry,
		UpdateMenu:      app.updateMenu,
		GoFunc:          app.goFunc,
		NotifyReconcile: bridge.SendReconcile,
		NotifyReloadMcp: bridge.SendReloadMcp,
	}
	appInstance = app

	// Create and start bridge server.
	router := &appRouter{
		store:    store,
		tools:    extMgr,
		services: app.registry,
		onChange: app.onExternalChange,
	}
	// Share the in-memory service token store between the router (auth) and
	// the registry (token lifecycle). Tokens live only in memory — no cleanup
	// needed on crash.
	registry.TokenStore = &router.serviceTokens

	// Provision the Eve↔relayLLM channel. Lazy: the bearer token + Unix
	// socket path are generated on the first spawn that participates
	// (Eve or relayLLM, whichever comes first), then reused for the other.
	// See relay_llm_channel.go for the rationale and the participant list.
	registry.LLMChannel = NewLLMChannel()
	bs, err := bridge.NewBridgeServer(ctx, router)
	if err != nil {
		slog.Error("failed to start bridge server", "error", err)
		os.Exit(1)
	}
	app.bridgeServer = bs

	// Start external MCPs and autostart services before the bridge accepts
	// connections, so tool lists and service status are populated when the
	// first client connects.
	extMgr.StartAll(ctx, settings.ExternalMcps)
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
// modtime changes) to pick up CLI-driven changes, and pushes service status
// to the settings WebView. Tray menu status dots are updated event-driven
// via ServiceRegistry.OnProcessExit, not by this poller — see runTrayApp
// where the callback is wired.
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

		s := a.store.ReloadIfChanged()

		a.platform.DispatchToMain(func() {
			if s != nil {
				a.updateMenuWithSettings(s)
			}
			a.pushServiceStatus()
		})
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
	}

	var items []menuItem

	// Build menu-item-ID -> service-ID mapping so click handlers resolve by ID,
	// not positional index (which can go stale if services change between builds).
	svcMap := make(map[int]string, len(s.Services))
	for i, svc := range s.Services {
		menuID := menuIDSvcBase + i
		svcMap[menuID] = svc.ID
		items = append(items, menuItem{
			Title:   svc.DisplayName,
			ID:      menuID,
			Enabled: true,
			Toggle:  true,
			On:      a.registry.IsRunning(svc.ID),
			URL:     svc.URL,
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
	a.platform.UpdateMenu(string(data))
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
		a.registry.StopAll()
		// Unlink the LLM channel socket after the children that depend on it
		// have stopped. The token persists in-memory until the process exits.
		a.registry.CloseLLMChannel()
		a.wg.Wait()
	})
}
