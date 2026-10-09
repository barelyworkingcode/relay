package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/projectfs"
	"github.com/barelyworkingcode/relay/internal/service"
)

// serverOptions is what a front end hands the server core: the tray passes the
// Cocoa platform, `relay serve` the headless one.
type serverOptions struct {
	Platform Platform
}

// readyFileName sits directly in the config dir; its presence means a server
// owns the dir and every listener in it is bound.
const readyFileName = "ready.json"

// unixSocketPathMax is the longest Unix socket path macOS accepts, less the
// terminating NUL.
const unixSocketPathMax = 103

// ListenAddrs is the TCP addresses a server actually bound, so a port-0
// request reports its real port. A listener that is not bound has no entry.
type ListenAddrs struct {
	API       string `json:"api,omitempty"`
	Model     string `json:"model,omitempty"`
	Remote    string `json:"remote,omitempty"`
	Enrolment string `json:"enrolment,omitempty"`
}

// ListenAddrs reads the live sockets, never settings.
func (a *App) ListenAddrs() ListenAddrs {
	if a == nil {
		return ListenAddrs{}
	}
	a.addrMu.RLock()
	defer a.addrMu.RUnlock()
	return ListenAddrs{
		API:       a.frontendServer.LoopbackAddr(),
		Model:     a.modelEndpoint.TCPAddr(),
		Remote:    a.remote.Addr(),
		Enrolment: a.remote.EnrolAddr(),
	}
}

// currentListenAddrs is ListenAddrs for the running server; empty in a process
// that runs none.
func currentListenAddrs() ListenAddrs {
	return appInstance.ListenAddrs()
}

type readySockets struct {
	Bridge   string `json:"bridge"`
	Frontend string `json:"frontend"`
	Model    string `json:"model"`
}

// readyFile is the machine-readable record of a live server. It carries no
// credential: a reader still needs a token or a peer identity to do anything.
type readyFile struct {
	Schema    int          `json:"schema"`
	PID       int          `json:"pid"`
	Version   string       `json:"version"`
	ConfigDir string       `json:"config_dir"`
	Sockets   readySockets `json:"sockets"`
	Listeners ListenAddrs  `json:"listeners"`
}

func readyFilePath(configDir string) string {
	return filepath.Join(configDir, readyFileName)
}

// checkSocketPathLengths refuses a config dir whose sockets would not bind.
// Checked before anything starts so the failure names the directory instead of
// surfacing as a bind error from deep inside startup.
func checkSocketPathLengths(configDir string) error {
	sockets := []string{
		filepath.Join(configDir, "relay.sock"),
		filepath.Join(configDir, "model.sock"),
		filepath.Join(configDir, fmt.Sprintf("relay-frontend-%d.sock", os.Getpid())),
		service.RelaySessionsInternalSocketPath(configDir),
		service.RelaySessionsHookSocketPath(configDir),
	}
	for _, s := range sockets {
		if len(s) > unixSocketPathMax {
			return fmt.Errorf("config dir %s is too long: socket %s is %d bytes and macOS allows %d; choose a shorter directory", configDir, s, len(s), unixSocketPathMax)
		}
	}
	return nil
}

// exportConfigDirToChildren makes every spawned service address this
// instance. The default dir clears the variable instead, so a child of the
// default instance never inherits a stale name from the launching shell.
func exportConfigDirToChildren(configDir string) error {
	if filepath.Clean(configDir) == filepath.Clean(bridge.DefaultConfigDir()) {
		if err := os.Unsetenv(bridge.EnvConfigDir); err != nil {
			return fmt.Errorf("clear %s for children: %w", bridge.EnvConfigDir, err)
		}
		return nil
	}
	if err := os.Setenv(bridge.EnvConfigDir, configDir); err != nil {
		return fmt.Errorf("set %s=%s for children: %w", bridge.EnvConfigDir, configDir, err)
	}
	return nil
}

// buildReadyFile snapshots the bound addresses.
func (a *App) buildReadyFile() ([]byte, error) {
	rf := readyFile{
		Schema:    1,
		PID:       os.Getpid(),
		Version:   buildVersion,
		ConfigDir: a.configDir,
		Sockets: readySockets{
			Bridge:   bridge.SocketPath(),
			Frontend: a.frontendSocketPath,
			Model:    bridge.ModelSocketPath(),
		},
		Listeners: a.ListenAddrs(),
	}
	return json.MarshalIndent(rf, "", "  ")
}

// writeReadyFile writes ready.json once every listener is bound and arms
// refreshReadyFile.
func (a *App) writeReadyFile() error {
	a.readyMu.Lock()
	defer a.readyMu.Unlock()
	data, err := a.buildReadyFile()
	if err != nil {
		return fmt.Errorf("encode ready file: %w", err)
	}
	if err := config.AtomicWriteFile(readyFilePath(a.configDir), data, 0600); err != nil {
		return fmt.Errorf("write ready file: %w", err)
	}
	a.readyLast = data
	a.readyArmed = true
	return nil
}

// refreshReadyFile rewrites ready.json after a listener reconcile moved or
// bound a listener. Silent when nothing changed, and inert before the first
// write and after shutdown began, so it can never publish a half-started
// server or resurrect the file cleanup removed.
func (a *App) refreshReadyFile() {
	a.readyMu.Lock()
	defer a.readyMu.Unlock()
	if !a.readyArmed || a.ctx.Err() != nil {
		return
	}
	data, err := a.buildReadyFile()
	if err != nil {
		slog.Warn("ready file not refreshed", "error", err)
		return
	}
	if bytes.Equal(data, a.readyLast) {
		return
	}
	if err := config.AtomicWriteFile(readyFilePath(a.configDir), data, 0600); err != nil {
		slog.Warn("ready file not refreshed", "path", readyFilePath(a.configDir), "error", err)
		return
	}
	a.readyLast = data
}

// removeReadyFile runs first in cleanup: a reader that sees no ready.json
// knows the server is going away.
func (a *App) removeReadyFile() {
	a.readyMu.Lock()
	defer a.readyMu.Unlock()
	a.readyArmed = false
	if err := os.Remove(readyFilePath(a.configDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("ready file not removed", "path", readyFilePath(a.configDir), "error", err)
	}
}

// startServerCore starts every listener, service and loop for
// bridge.ConfigDir() and returns once all are serving and ready.json is
// written. It opens no window and no tray item; the caller owns the run loop.
// A SIGTERM or SIGINT cleans up and exits 0.
func startServerCore(opts serverOptions) (*App, error) {
	platform := opts.Platform

	// The keyring is constructed here and NOWHERE else in the program
	// (§5.3.3, AC-29), through newKeyring (keystore.go): this is the tray's
	// own path, and the ACL binds to whatever code identity
	// SecTrustedApplicationCreateFromPath reads from it. A CLI process
	// carries relay's own code identity too, so a second call site here would
	// satisfy the very ACL this design depends on the CLI never asking —
	// TestSeal_NoCLIPathReachesTheKeychain is what keeps that true.
	configDir := bridge.ConfigDir()
	releaseOwnership, err := config.AcquireTrayOwnership(configDir)
	if err != nil {
		return nil, err
	}
	// Past the lock this process is the only owner of configDir, so a
	// ready.json found here is a dead process's and never describes a live
	// server.
	if err := os.Remove(readyFilePath(configDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		releaseOwnership()
		return nil, fmt.Errorf("remove stale ready file: %w", err)
	}
	if err := checkSocketPathLengths(configDir); err != nil {
		releaseOwnership()
		return nil, err
	}
	if err := exportConfigDirToChildren(configDir); err != nil {
		releaseOwnership()
		return nil, err
	}
	keyring, err := newKeyring(configDir)
	if err != nil {
		releaseOwnership()
		return nil, err
	}
	// Built once and handed to every consumer; there is no package-level clock.
	clock := newServerClock(configDir)
	store, err := config.ResolveSealedStore(configDir, keyring)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve the sealed store: %w", err)
	}
	store.OwnExclusively()

	// Ensure admin secret is generated and persisted on first launch, run
	// migration on first encounter with a plaintext settings.json (§4.7),
	// or — on a degraded store — do nothing beyond loading what the clear
	// fields already say (§5.6 clause 1: relay starts, it does not exit).
	if err := store.EnsureInitialized(); err != nil {
		return nil, fmt.Errorf("failed to initialize settings: %w", err)
	}
	// SH §2.1: persist a bare, autostart-only relaysessions record the first
	// time settings.json has never held one. List, SetAutostart and Start
	// (cmd/relay/service_ops.go) all read the store directly, never through
	// EnsureBuiltinRelaySessionsService's in-memory synthesis below, so
	// without this write there is no record for any of them to act on, and
	// no supported way to turn autostart back off. Command and Args are
	// never written here; sanitizeIfBuiltin strips them from whatever is
	// stored on every load regardless. Best-effort: a degraded store simply
	// cannot write yet, same as every other write on one.
	errRelaySessionsRecordAlreadyPersisted := errors.New("relaysessions record already persisted")
	err = config.WithDeclinable(store, func(s *config.Settings) error {
		if svc, _ := config.FindServiceByID(s, config.RelaySessionsServiceID); svc != nil {
			return errRelaySessionsRecordAlreadyPersisted
		}
		service.EnsureBuiltinRelaySessionsRecord(s)
		return nil
	})
	if err != nil && !errors.Is(err, errRelaySessionsRecordAlreadyPersisted) {
		slog.Warn("could not persist the default relaysessions record", "error", err)
	}
	// Terminal templates live only in settings.json, so an empty list means
	// no terminal can launch. Persist the default shell template when there is
	// none, where the operator can see and change it; an install that already
	// has templates is not rewritten. Best-effort, like the record above.
	errTemplatesAlreadyPresent := errors.New("terminal templates already present")
	err = config.WithDeclinable(store, func(s *config.Settings) error {
		if !config.EnsureDefaultTerminalTemplates(s) {
			return errTemplatesAlreadyPresent
		}
		return nil
	})
	if err != nil && !errors.Is(err, errTemplatesAlreadyPresent) {
		slog.Warn("could not persist the default terminal template", "error", err)
	}
	var sealStatus string
	if reason := store.SealStatus(); reason != nil {
		// §5.6: the read half works in full from here on — relay grant,
		// relay audit, every list, the Settings window (rendering sealed
		// values as sealUnavailablePlaceholder) and the tray menu below.
		// Every sealed operation refuses naming this same reason; nothing
		// here creates or adopts a replacement key (§5.5.1).
		slog.Warn("sealed store is degraded — sealed operations will refuse until this is resolved", "reason", reason)
		sealStatus = reason.Error()
	}
	settings := store.Get()
	slog.Info("settings loaded")

	// The real LocalAuthentication provider is constructed in exactly one
	// place: here. The hermetic suite never calls runTrayApp, so it never
	// reaches this line — presence.LocalAuthProviderConstructions() staying
	// at zero across `go test ./...` is what AC-20 checks instead of hoping.
	presenceProvider := newPresenceProvider(configDir)
	presenceGate, err := presence.NewGate(presenceProvider)
	if err != nil {
		return nil, fmt.Errorf("failed to construct the presence gate: %w", err)
	}

	// External MCP manager with injected callback for OAuth token refresh persistence.
	// mcpOps is bound below, before extMgr.StartAll can connect anything and so
	// before any refresh can fire.
	var mcpOps *McpOps
	extMgr := mcpbroker.NewManager(
		func(mcpID string, oauth *config.OAuthState) {
			if err := mcpOps.PersistOAuthState(mcpID, oauth); err != nil {
				if errors.Is(err, errMcpNotFound) {
					slog.Warn("refreshed OAuth token dropped: the MCP was removed", "mcp", mcpID)
					return
				}
				slog.Error("failed to persist refreshed OAuth token", "mcp", mcpID, "error", err)
			}
		},
	)

	extMgr.SetStderrLog(openMcpStderrLog)
	extMgr.SetClock(clock)

	ctx, cancel := context.WithCancel(context.Background())

	registry := service.NewRegistry()
	registry.Clock = clock
	serviceQueue, err := config.NewCommandQueue(32)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to start service command queue: %w", err)
	}

	app := &App{
		ctx:              ctx,
		cancel:           cancel,
		releaseOwnership: releaseOwnership,
		importFile:       store.ImportFile,
		store:            store,
		platform:         platform,
		extMgr:           extMgr,
		registry:         registry,
		serviceQueue:     serviceQueue,
		presenceGate:     presenceGate,
		sealedKeyring:    keyring,
		clock:            clock,
		configDir:        configDir,
		sealStatus:       sealStatus,
		lastHealth:       map[string]mcpbroker.HealthEvent{},
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
		Enhanced: enhancedRegistry,
		Queue:    serviceQueue,
		Gate:     presenceGate,
		SessionHost: func(autostart bool) config.ServiceConfig {
			return service.BuiltinRelaySessionsService(resolveRelayBin(), configDir, autostart)
		},
		OnChange: func() {
			app.platform.DispatchToMain(func() {
				app.updateMenu()
				app.pushServiceStatus()
			})
		},
	}
	app.serviceOps = serviceOps

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
		ConfigDir:              configDir,
		LogsDir:                serviceLogDir,
	}

	// Tool-call audit log. A failure here is logged and auditing stays off
	// rather than taking the tray down with it: the recorder is observability,
	// not an authorization control, and relay is more useful running blind than
	// not running at all. The Tool Calls tab surfaces the disabled state.
	// Named rec rather than audit in this scope: a local audit would shadow
	// the internal/audit package import that the rest of this function needs.
	rec := startAuditRecorder(store.Get())
	app.audit = rec
	// serviceOps is constructed above, before the audit recorder exists, so
	// its Issuance field is wired here rather than in the literal. Gate was
	// already set there — the real provider has no such ordering constraint.
	serviceOps.Issuance = issuanceAuditorOrNil(rec)

	// A dead external MCP used to be invisible: every client got
	// `read response: EOF` and nothing in relay said the server behind them was
	// gone (issue #39). The supervisor now reports every death, restart, and
	// abandonment, and this is where those reports become rows in the log an
	// operator is told to treat as ground truth. Installed here rather than at
	// construction because the manager is built before the recorder exists.
	extMgr.SetHealthObserver(func(ev mcpbroker.HealthEvent) {
		recordMcpSupervision(rec, ev)
		app.recordHealthEvent(ev)
	})

	// Create and start bridge server.
	router := &appRouter{
		store:    store,
		tools:    extMgr,
		services: app.registry,
		enhanced: enhancedRegistry,
		onChange: app.onExternalChange,
		audit:    rec,
	}
	// router implements SkillLister (ListTools); set it on the IPC context
	// now that it exists so the Projects-tab "Regen Now" button can run.
	app.ipcCtx.SkillLister = router
	app.ipcCtx.Audit = rec
	router.serviceOps = serviceOps

	// credentialOps is admin_op's only door onto CredentialOps (ADR-017
	// implementation spec S6): `relay credential mint|revoke` is host-only
	// and has no Settings tab of its own, so it is wired straight onto the
	// router rather than threaded through IPCContext the way the other five
	// cores are.
	credentialOps := &CredentialOps{Store: store, Queue: serviceQueue, Gate: presenceGate, Issuance: issuanceAuditorOrNil(rec), Clock: clock}
	router.credentialOps = credentialOps

	// auditOps is the one core behind both the Tool Calls tab (via
	// app.ipcCtx.AuditOps) and RegisterAuditRoutes on the frontend server
	// (ADR-014). Read-only, so unlike serviceOps/enrolmentOps it carries no
	// OnChange — a query changes nothing another view needs to learn about.
	auditOps := &audit.AuditOps{Audit: rec}
	app.ipcCtx.AuditOps = auditOps

	// enrolmentOps is the one core behind both the Remote Clients tab (via
	// app.ipcCtx.EnrolmentOps) and RegisterEnrolmentRoutes on the frontend
	// server (ADR-014). pushFullSettings already carries enrolments and the
	// remote block, so reusing it here is what keeps an open Settings window
	// in sync with an enrolment created or revoked from curl.
	enrolmentOps := &EnrolmentOps{
		Store: store,
		Queue: serviceQueue,
		Audit: rec,
		Gate:  presenceGate,
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
		Queue: serviceQueue,
		Audit: rec,
		Gate:  presenceGate,
		Clock: clock,
	}
	app.loginOps = loginOps
	app.ipcCtx.LoginOps = loginOps
	router.loginOps = loginOps

	// eveEnrolmentOps is the second-browser counterpart to loginOps
	// (docs/eve-passkey-enrolment.md): the tray's "Allow Eve Passkey
	// Enrolment…" item and `relay eve enrol` both call Open on this exact
	// instance, and RegisterEveEnrolmentRoutes (wired into NewFrontendServer
	// below) shares it for Status/Consume, so the menu's countdown line and
	// eve's own poll can never disagree about whether a window is open.
	eveEnrolmentOps := &EveEnrolmentOps{
		Store:  store,
		Queue:  serviceQueue,
		Audit:  rec,
		Gate:   presenceGate,
		Notify: platform.Notify,
		Clock:  clock,
	}
	app.eveEnrolmentOps = eveEnrolmentOps
	router.eveEnrolmentOps = eveEnrolmentOps

	// evePasskeyOps is the mirror-and-revoke counterpart to loginOps for
	// eve's own credentials (docs/eve-passkey-enrolment.md decisions 8-13):
	// the Passkeys tab's eve section, `relay eve list|revoke`, and eve's own
	// PUT/GET routes (RegisterEvePasskeyRoutes, wired into NewFrontendServer
	// below) all share this exact instance. A report or revoke reaches an
	// open Settings window through the post-commit event, like loginOps'.
	evePasskeyOps := &EvePasskeyOps{
		Store:  store,
		Queue:  serviceQueue,
		Audit:  rec,
		Gate:   presenceGate,
		Notify: platform.Notify,
	}
	app.evePasskeyOps = evePasskeyOps
	app.ipcCtx.EvePasskeyOps = evePasskeyOps
	router.evePasskeyOps = evePasskeyOps

	// mcpOps is the one core behind both the MCP Servers tab (via
	// app.ipcCtx.McpOps) and RegisterMcpRoutes on the frontend server
	// (ADR-014) -- an MCP added from curl and one added from the tray share
	// the same SSRF guard, discovery, and reconcile. Only add/remove get an
	// HTTP door; authenticate and reset-permissions stay IPC-only (McpOps
	// doc comment explains why) and keep calling this same instance.
	mcpOps = &McpOps{
		Store:           store,
		Ctx:             ctx,
		Queue:           serviceQueue,
		Gate:            presenceGate,
		Issuance:        issuanceAuditorOrNil(rec),
		NotifyReconcile: bridge.SendReconcile,
		NotifyReloadMcp: bridge.SendReloadMcp,
		Clock:           clock,
	}
	app.ipcCtx.McpOps = mcpOps
	router.mcpOps = mcpOps

	// Live-tail the Tool Calls tab. Fires on the audit writer goroutine, so
	// hop to main before touching the WebView.
	rec.SetSink(func(ev audit.AuditEvent) {
		if !app.settingsOpen.Load() {
			return
		}
		app.platform.DispatchToMain(func() { app.emitSettingsEvent("onAuditEvent", ev) })
	})
	// One launch table: the registry begins and ends launches, the bridge
	// router binds them at Hello and authenticates by them, and the frontend
	// server admits a service holding the frontend capability by them. It lives only in memory, so a
	// crashed relay leaves no identity behind.
	launches := service.NewLaunches()
	launches.SetClock(clock.Now)
	registry.Launches = launches
	router.launches = launches

	// SP3/R-S9: relay-sessions is the one service whose code identity is
	// pinned, both before it is spawned and again at its own Hello. Only a
	// build carrying an embedded helper cdhash (build.sh's Helpers step) can
	// construct the real verifier; a plain `go build` leaves both nil and
	// simply does not gate this service any more strictly than any other --
	// exactly this repo's own hermetic test suite and a developer checkout.
	if HelperCDHash != "" {
		if v, err := service.NewDarwinHelperVerifier(HelperTeam, HelperCDHash); err != nil {
			slog.Error("relay-sessions helper verifier could not be constructed; the built-in session host will not start", "error", err)
		} else {
			registry.HelperVerifier = v
			launches.SetHelperVerifier(v)
		}
	}

	// The model endpoint's own tables: at most one live upstream, and the
	// model keys minted for it (docs/model-endpoint.md). Wired onto the
	// router so RegisterModelHost can reach modelHosts under the same launch
	// identity check every other service operation uses.
	modelHosts := NewModelHostRegistry(launches)
	router.modelHosts = modelHosts
	modelKeys := NewModelKeyTable()
	app.modelEndpoint = NewModelEndpointServer(store, launches, modelKeys, modelHosts)
	app.modelEndpoint.catalog.SetClock(clock.Now)
	app.modelEndpoint.AuditHook = func(ev ModelCallAudit) {
		recordModelCall(rec, ev)
	}
	app.ipcCtx.ModelCatalog = &ModelCatalogOps{
		HostModels: (&sessionHostClient{enhanced: enhancedRegistry, launches: launches}).ListModels,
		BrokerRows: app.modelEndpoint.catalog.Snapshot,
	}

	sessLedger := openSessionLedger(configDir)

	// sessionAccounts is the launch-identity/model-key bookkeeping SessionExited
	// (router_sessions.go) and project-delete cleanup (project_ops.go) both
	// need, since neither the ledger nor *service.Launches carries it (see
	// sessionAccounting's own doc comment, session_routes.go).
	sessionAccounts := newSessionAccounting()
	router.sessions = sessLedger
	router.modelKeys = modelKeys
	router.sessionAccounts = sessionAccounts

	// sessionDeps is the one instance RegisterSessionRoutes, ProjectOps'
	// delete cleanup and appRouter.SessionExited all share, so a create, a
	// resume, an advisory exit report and a project delete can never observe
	// a different ledger, launch table or accounting map than each other.
	sessionDeps := sessionRouteDeps{
		store:       store,
		launches:    launches,
		sessions:    sessLedger,
		modelKeys:   modelKeys,
		enhanced:    enhancedRegistry,
		auditor:     rec,
		accounting:  sessionAccounts,
		resumeGuard: newResumeGuard(),
		systemModel: app.modelEndpoint.IsSystemModel,
	}

	router.sessionDeps = sessionDeps

	frontendChannel := NewFrontendChannel()
	app.frontendChannel = frontendChannel
	registry.FrontendEnv = func() (map[string]string, error) {
		ep, err := frontendChannel.Ensure()
		if err != nil {
			return nil, err
		}
		return ep.FrontendEnv(), nil
	}
	registry.OpenLog = func(id string) (io.WriteCloser, error) {
		dir, err := serviceLogDir()
		if err != nil {
			return nil, err
		}
		return openRotatingLog(filepath.Join(dir, id+".log"))
	}
	bs, err := bridge.NewBridgeServer(ctx, router)
	if err != nil {
		return nil, fmt.Errorf("failed to start bridge server: %w", err)
	}
	app.bridgeServer = bs

	// Materialize the channel up front so the frontend HTTP server can bind
	// before any client (Eve, scheduler) tries to dial it. Spawned consumers
	// learn the same socket path via registry.FrontendEnv.
	frontendEndpoint, err := frontendChannel.Ensure()
	if err != nil {
		return nil, fmt.Errorf("failed to provision frontend channel: %w", err)
	}
	app.frontendSocketPath = frontendEndpoint.Socket
	retireLegacyFrontendCredentialOnStart(store)
	// onProjectsChanged is the frontend server's legacy project-refresh hook;
	// committed project changes refresh the UI through onConfigCommitted.
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
		Store:          store,
		Queue:          serviceQueue,
		Gate:           presenceGate,
		Issuance:       issuanceAuditorOrNil(rec),
		SessionCleanup: sessionDeps,
	}
	app.ipcCtx.ProjectOps = projectOps
	router.projectOps = projectOps
	// hostOps is the one core behind both the Hosts tab (via
	// app.ipcCtx.HostOps) and RegisterHostRoutes on the frontend server
	// (docs/ssh-hosts.md) — a host created from curl and one created from
	// the tray share the same probe and the same audit record.
	hostPool := projectfs.NewHostPool(projectfs.HostPoolOptions{})
	hostOps := &HostOps{
		Store:   store,
		Queue:   serviceQueue,
		Auditor: rec,
		Agents:  hostPool,
	}
	app.ipcCtx.HostOps = hostOps
	router.hostOps = hostOps
	router.modelCatalog = app.ipcCtx.ModelCatalog
	router.mcpSurfaces = extMgr.AllMcpSurfaces
	router.openURL = platform.OpenURL
	router.overview = func() overviewSeed { return app.buildOverviewSeed(config.DisplaySettings(store)) }
	// The doors catalogue collects the routes both muxes register, so it is
	// built before the frontend server and handed its recorder.
	_, headless := platform.(*headlessPlatform)
	doors := newDoorCatalog(headless)
	router.headless = headless
	router.resetSealed = app.resetSealed
	router.doors = doors
	templateOps := &TemplateOps{Store: store, Queue: serviceQueue}
	app.ipcCtx.TemplateOps = templateOps
	app.ipcCtx.HostTemplateOps = &HostTemplateOps{Store: store, Queue: serviceQueue}
	frontend, err := NewFrontendServer(store, extMgr, extMgr, extMgr, frontendEndpoint, enhancedRegistry, router, onProjectsChanged, serviceOps, enrolmentOps, auditOps, mcpOps, projectOps, hostOps, templateOps, eveEnrolmentOps, evePasskeyOps, NewCredentialAuthorizer(store, clock), audit.ControlAuditorOrNil(rec), launches, sessionDeps, doors.recordRoute, clock)
	if err != nil {
		return nil, fmt.Errorf("failed to start frontend server: %w", err)
	}
	frontend.routeDeps.loginOps = loginOps
	frontend.FileOps().Hosts = hostPool
	// The router reaches the instances the frontend server holds, so a CLI
	// door and an HTTP door share one watch hub and one tmux lister.
	router.fileOps = frontend.FileOps()
	router.persistentSessions = frontend.routeDeps.persistentSessionOps
	app.addrMu.Lock()
	app.frontendServer = frontend
	app.addrMu.Unlock()
	app.goFunc(func() {
		if err := frontend.Serve(); err != nil {
			slog.Error("frontend server exited with error", "error", err)
		}
	})
	apiAddr, apiSource, err := resolveAPIListen(settings, os.Getenv)
	if err != nil {
		return nil, fmt.Errorf("refused API listener: %w", err)
	}
	if apiAddr != "" {
		app.addrMu.Lock()
		err := frontend.ListenLoopback(apiAddr)
		app.addrMu.Unlock()
		if err != nil {
			// Refused rather than downgraded to the socket alone: someone who
			// asked for a TCP door and silently did not get one would debug
			// the wrong thing.
			return nil, fmt.Errorf("failed to bind API listener (%s): %w", apiSource, err)
		}
		doors.recordLoginRoutes(frontend.LoginPatterns())
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
	// The built-in relay-sessions record (SH §2.1): synthesized here, every
	// start, never read from settings.json beyond the autostart bit --
	// internal/config's sanitizeIfBuiltin already stripped any stored
	// Command, this is what puts the real one in. settings is store.Get()'s
	// own clone, so mutating it here never touches the file.
	settings.Services = service.EnsureBuiltinRelaySessionsService(settings.Services, resolveRelayBin(), configDir)

	// Reclaim orphans from a previous tray session that was killed before
	// the reaper could SIGTERM its children. Without this, autostart of any
	// port-binding service (scheduler, kokoro, whisper, comfy) fails with
	// EADDRINUSE on every restart. Must run before StartAllAutostart.
	app.registry.ReclaimOrphans(settings.Services)
	app.registry.StartAllAutostart(settings.Services)

	app.goFunc(func() {
		if err := bs.Serve(); err != nil && !errors.Is(err, net.ErrClosed) {
			slog.Error("bridge server stopped serving", "error", err)
		}
	})
	slog.Info("bridge server started")

	// model.sock is always served, beside relay.sock, whether or not a model
	// host has ever registered (docs/model-endpoint.md): "no host" is a 503
	// on each call, not an absent listener. A bind failure here is fatal to
	// relay in the same way a bridge-server bind failure is — the socket is
	// as core to the process as the bridge itself, unlike the optional TCP
	// listener below.
	if err := app.modelEndpoint.ListenSocket(); err != nil {
		return nil, fmt.Errorf("failed to bind model endpoint socket: %w", err)
	}
	app.goFunc(func() {
		if err := app.modelEndpoint.ServeSocket(); err != nil {
			slog.Error("model endpoint socket server stopped serving", "error", err)
		}
	})
	// The TCP listener stays off unless settings.json's model_endpoint block
	// names an address; Reconcile is a no-op either way when nothing
	// changed. onConfigCommitted re-converges it after every committed change.
	app.modelEndpoint.Reconcile()
	slog.Info("model endpoint socket started")

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
	// launch. onConfigCommitted drives the convergence from here on.
	remote := NewRemoteSupervisor(ctx, store, router, rec, projectOps, extMgr.AllMcpSurfaces, app.goFunc, clock)
	app.addrMu.Lock()
	app.remote = remote
	app.addrMu.Unlock()
	app.remote.Reconcile() //nolint:errcheck // logs its own failure; a listener is never fatal to the tray
	serviceQueue.SetCommitObserver(store.Commits, app.onConfigCommitted)
	if stop, err := config.WatchSettingsFile(configDir, app.importSettingsEdit); err != nil {
		slog.Error("settings.json edits will not be picked up while relay runs", "error", err)
	} else {
		app.stopSettingsWatch = stop
	}

	// Wires the Remote Clients tab's Pending requests panel and the tray's
	// passive count line onto the SAME pending-enrolment-request table the
	// listener above just started serving — the table is built once, inside
	// the supervisor, and outlives every rebind of the listener around it
	// (RemoteSupervisor.EnrolTable's own doc comment), so this assignment
	// needs no further synchronization: nothing reads enrolmentOps.Requests
	// before this line runs.
	enrolmentOps.Requests = app.remote.EnrolTable()

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

	if err := app.writeReadyFile(); err != nil {
		return nil, err
	}
	logging.BeginEvent(ctx, "server.ready").Set("config_dir", configDir).Set("ready_file", readyFilePath(configDir)).
		Set("pid", os.Getpid()).End(logging.OutcomeOK, "", nil)

	// Health poll: service status and memory; settings changes arrive as
	// post-commit events, not through this loop.
	app.goFunc(app.statusPoller)

	return app, nil
}
