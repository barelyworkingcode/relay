package main

import (
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
)

// appInstance is the singleton tray app, set by runTrayApp and read by Cocoa callbacks.
var appInstance *App

// appRunning tracks whether the app is still alive (for cleanup).
var appRunning atomic.Bool

// App is the main tray application state.
type App struct {
	settings     *Settings
	platform     Platform
	extMgr       *ExternalMcpManager
	registry     *ServiceRegistry
	bridgeServer *bridge.BridgeServer
	settingsOpen bool
	cleanupOnce  sync.Once
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

	// Ensure admin secret is generated and persisted on first launch.
	WithSettings(func(s *Settings) {})
	settings := LoadSettings()
	slog.Info("settings loaded")

	// External MCP manager.
	extMgr := NewExternalMcpManager()

	app := &App{
		settings: settings,
		platform: platform,
		extMgr:   extMgr,
		registry: NewServiceRegistry(),
	}
	appInstance = app

	// Create and start bridge server.
	router := &appRouter{app: app}
	bs, err := bridge.NewBridgeServer(router)
	if err != nil {
		slog.Error("failed to start bridge server", "error", err)
		os.Exit(1)
	}
	app.bridgeServer = bs
	go bs.Serve()
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
	go func() {
		sig := <-sigCh
		slog.Info("received signal, cleaning up", "signal", sig)
		app.cleanup()
		os.Exit(0)
	}()

	// Poll service status every 2s.
	go app.statusPoller()

	// Block on the platform run loop (must be on main thread).
	slog.Info("entering run loop")
	appRunning.Store(true)
	platform.Run()
}

// statusPoller periodically checks service status and updates the menu.
func (a *App) statusPoller() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		if !appRunning.Load() {
			return
		}
		a.registry.CleanupDead()
		s := LoadSettings()
		a.platform.DispatchToMain(func() {
			a.updateMenuWithSettings(s)
			a.pushServiceStatus()
		})
	}
}

// updateMenu rebuilds the tray menu JSON and pushes it to the platform.
func (a *App) updateMenu() {
	a.updateMenuWithSettings(LoadSettings())
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
		running := a.registry.IsRunning(svc.ID)
		dot := "\U0001F534" // red
		if running {
			dot = "\U0001F7E2" // green
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
	s := LoadSettings()
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
		go func() {
			a.registry.Stop(id)
			a.platform.DispatchToMain(func() {
				a.pushServiceStatus()
				a.updateMenu()
			})
		}()
	} else {
		if err := a.registry.Start(config); err != nil {
			slog.Error("service toggle failed", "error", err)
		}
		a.updateMenu()
	}
}

func (a *App) cleanup() {
	a.cleanupOnce.Do(func() {
		appRunning.Store(false)
		a.registry.StopAll()
		a.extMgr.StopAll()
		if a.bridgeServer != nil {
			a.bridgeServer.Close()
		}
	})
}
