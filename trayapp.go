package main

import (
	"context"
	"encoding/json"
	"fmt"
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
	settings     *Settings
	platform     Platform
	extMgr       *ExternalMcpManager
	registry     *ServiceRegistry
	bridgeServer *bridge.BridgeServer
	settingsOpen bool
	cleanupOnce  sync.Once
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

// Status dot emoji for tray menu service items.
const (
	statusDotRunning = "\U0001F7E2" // green circle
	statusDotStopped = "\U0001F534" // red circle
)

func runTrayApp() {
	slog.Info("starting tray app")

	platform := NewPlatform()

	// Initialize platform UI first so app delegate exists before tray setup.
	platform.Init()
	slog.Info("platform initialized")

	// Ensure admin secret is generated and persisted on first launch.
	WithSettings(func(s *Settings) {})
	settings := GetSettings()
	slog.Info("settings loaded")

	// External MCP manager with injected callbacks for settings persistence.
	extMgr := NewExternalMcpManager(
		// onDiscover: persist discovered tools and context schema.
		func(id string, tools []ToolInfo, schema json.RawMessage) {
			WithSettings(func(s *Settings) {
				s.UpdateDiscoveredTools(id, tools)
				s.UpdateContextSchema(id, schema)
			})
		},
		// onTokenRefresh: persist refreshed OAuth tokens.
		func(mcpID string, oauth *OAuthState) {
			WithSettings(func(s *Settings) { s.UpdateOAuthState(mcpID, oauth) })
		},
	)

	ctx, cancel := context.WithCancel(context.Background())

	app := &App{
		ctx:      ctx,
		cancel:   cancel,
		settings: settings,
		platform: platform,
		extMgr:   extMgr,
		registry: NewServiceRegistry(),
	}
	appInstance = app

	// Create and start bridge server.
	router := &appRouter{
		tools:    extMgr,
		services: app.registry,
		onChange: app.onExternalChange,
	}
	bs, err := bridge.NewBridgeServer(router)
	if err != nil {
		slog.Error("failed to start bridge server", "error", err)
		os.Exit(1)
	}
	app.bridgeServer = bs
	app.goFunc(func() { bs.Serve() })
	slog.Info("bridge server started")

	// Start external MCPs.
	extMgr.StartAll(settings.ExternalMcps)

	// Start autostart services.
	app.registry.StartAllAutostart(settings.Services)

	// Set up tray icon.
	slog.Info("setting up tray icon")
	rgba, w, h := CreateIconRGBA()
	platform.SetupTray(rgba, w, h)
	slog.Info("tray icon set up")

	// Build and set initial menu.
	app.updateMenu()
	slog.Info("menu built")

	// Catch termination signals so child processes get cleaned up.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	app.goFunc(func() {
		select {
		case sig := <-sigCh:
			slog.Info("received signal, cleaning up", "signal", sig)
			app.cleanup()
			os.Exit(0)
		case <-ctx.Done():
			return
		}
	})

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

// statusPoller periodically checks service status and updates the menu.
// Only re-reads settings from disk when the file's modtime changes,
// avoiding unnecessary I/O and JSON parsing on every tick.
func (a *App) statusPoller() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
		}

		a.registry.CleanupDead()

		s := ReloadIfChanged()

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
	a.updateMenuWithSettings(GetSettings())
}

func (a *App) updateMenuWithSettings(s *Settings) {
	type menuItem struct {
		Title   string `json:"title"`
		ID      int    `json:"id"`
		Enabled bool   `json:"enabled"`
	}

	var items []menuItem

	// Service items.
	for i, svc := range s.Services {
		dot := statusDotStopped
		if a.registry.IsRunning(svc.ID) {
			dot = statusDotRunning
		}
		items = append(items, menuItem{
			Title:   fmt.Sprintf("%s %s", dot, svc.DisplayName),
			ID:      menuIDSvcBase + i,
			Enabled: true,
		})
	}

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
		a.toggleService(itemID - menuIDSvcBase)
	}
}

func (a *App) toggleService(index int) {
	s := GetSettings()
	if index < 0 || index >= len(s.Services) {
		return
	}
	config := &s.Services[index]

	// If the service has a URL, lazy-start and open the URL.
	if config.URL != "" {
		if !a.registry.IsRunning(config.ID) {
			if err := a.registry.Start(config); err != nil {
				slog.Error("service start failed", "error", err)
			}
		}
		a.platform.OpenURL(config.URL)
		a.updateMenu()
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

func (a *App) cleanup() {
	a.cleanupOnce.Do(func() {
		a.cancel() // signals all tracked goroutines via context
		a.registry.StopAll()
		a.extMgr.StopAll()
		if a.bridgeServer != nil {
			a.bridgeServer.Close()
		}
		a.wg.Wait() // wait for tracked goroutines to finish
	})
}
