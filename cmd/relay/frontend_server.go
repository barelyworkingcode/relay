package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/barelyworkingcode/relay/internal/control"
)

// FrontendServer hosts the HTTP API that Eve and relayScheduler consume. It
// owns the relay-frontend Unix socket, registers project endpoints directly
// (projects are a relay-internal concern), and dispatches everything else
// to whichever enhanced service registered for the route prefix.
//
// The caller's bearer is resolved to a control-plane credential on every
// request and WS upgrade. The dispatcher then injects each service's own
// internal token before dialing it — the two trust boundaries stay distinct.
type FrontendServer struct {
	socketPath string
	server     *http.Server
	listener   net.Listener
	tcpLn      net.Listener
	tcpServer  *http.Server

	// loginOrigin is the origin ListenLoopback derived from the address it
	// actually bound, and the one every WebAuthn ceremony is checked
	// against. Empty means no TCP listener and therefore no login routes.
	loginOrigin string

	// routeDeps, authz and auditor let ListenLoopback build the TCP mux at
	// call time, from the same ingredients NewFrontendServer used for the
	// socket mux, without threading a second copy of NewFrontendServer's
	// parameter list through it.
	routeDeps frontendRouteDeps
	authz     control.Authorizer
	auditor   control.ControlAuditor
}

// EnvAPIListen opts the API into a loopback TCP listener beside the 0600 Unix
// socket, so a browser can reach it. Absent means no TCP listener at all
// (ADR-014): the socket stays the only door unless someone asks otherwise.
const EnvAPIListen = "RELAY_API_LISTEN"

// ErrNonLoopbackAPIListen refuses any bind that is not loopback. Classing
// and scoping (ADR-015) narrow what a stolen or over-broad credential can
// reach; neither is a transport authentication scheme, so reachability is
// still the only boundary against a network attacker, and a typo must not
// be the thing that removes it.
var ErrNonLoopbackAPIListen = errors.New("api listen address must be loopback")

func loopbackOnly(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("parse %s: %w", EnvAPIListen, err)
	}
	// An empty host means "all interfaces", which is the failure this guards.
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("%w: %q", ErrNonLoopbackAPIListen, addr)
	}
	return nil
}

// ListenLoopback adds a second listener carrying its OWN route set, built
// fresh from routeDeps for control.TransportTCP (ADR-015 decision 2): registerFrontendRoutes
// runs control.ClassReachableOn against control.TransportTCP this time, so an execute-class
// route — one where the caller supplies what runs — is never handed to this
// mux at all, on any credential. The socket and TCP handlers still share
// frontendCredentialAuth and frontendRecover, so authentication and panic
// handling cannot drift between the two doors even though their route sets
// now deliberately do.
func (s *FrontendServer) ListenLoopback(addr string) error {
	if s == nil || addr == "" {
		return nil
	}
	if err := loopbackOnly(addr); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}

	tcpMux := http.NewServeMux()
	registerFrontendRoutes(&control.RouteRegistrar{Mux: tcpMux, Transport: control.TransportTCP, Authz: s.authz, Auditor: s.auditor, Reserve: s.routeDeps.enhanced, CredentialID: APICredentialIDFromContext}, s.routeDeps)

	// The origin comes from the address the kernel actually gave this
	// listener, never from a constant or a request header — an ephemeral
	// bind resolves its port only here. The host is localhost rather than
	// the bound IP because an RP ID must be a domain: http://127.0.0.1:PORT
	// cannot carry a WebAuthn ceremony at all (ADR-016 decision 1), so the
	// bind address stays what it is and the URL the owner types is part of
	// the design.
	port := ln.Addr().(*net.TCPAddr).Port
	s.loginOrigin = fmt.Sprintf("http://%s:%d", webauthnRPID, port)
	publicMux, err := s.newLoginMuxFor(s.loginOrigin)
	if err != nil {
		_ = ln.Close()
		s.loginOrigin = ""
		return err
	}

	s.tcpLn = ln
	s.tcpServer = &http.Server{
		Handler:           frontendPublicDoor(publicMux, frontendCredentialAuth(s.routeDeps.store, frontendRecover(warnOnUnmatchedTCPRoute(tcpMux)))),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       5 * time.Minute,
	}
	slog.Warn("frontend API bound to loopback TCP in addition to its socket",
		"addr", addr)
	return nil
}

// newLoginMuxFor builds the one mux that is served with no credential at
// all. It exists only here, and only for a bound TCP listener, because a
// WebAuthn ceremony is verified against an origin and a Unix socket has
// none: with no listener bound there is nothing the login routes could check
// an assertion against, so they are not registered anywhere rather than
// registered against a placeholder (the same rule control.RouteRegistrar.Handle
// follows for an unreachable class).
func (s *FrontendServer) newLoginMuxFor(origin string) (*http.ServeMux, error) {
	verifier, err := NewWebAuthnVerifier(origin, webauthnRPID)
	if err != nil {
		return nil, fmt.Errorf("login routes: %w", err)
	}
	lr := newLoginRoutes(s.routeDeps.store, verifier, s.auditor)
	lr.issuance = s.routeDeps.issuance
	return newLoginMux(lr), nil
}

// LoginOrigin reports the origin the login ceremony is bound to, or ""
// when no TCP listener is bound and there are therefore no login routes.
func (s *FrontendServer) LoginOrigin() string {
	if s == nil {
		return ""
	}
	return s.loginOrigin
}

// ServeLoopback blocks until Shutdown. No-op when ListenLoopback was not called.
func (s *FrontendServer) ServeLoopback() error {
	if s == nil || s.tcpLn == nil {
		return nil
	}
	if err := s.tcpServer.Serve(s.tcpLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// frontendRouteDeps bundles what registerFrontendRoutes needs to build one
// transport's route set. NewFrontendServer captures it once so ListenLoopback
// can build the TCP mux later from the same ingredients, without a second
// copy of NewFrontendServer's parameter list.
type frontendRouteDeps struct {
	store             SettingsStore
	mcps              McpSurfaceProvider
	tools             MCPToolsProvider
	enum              ContextEnumerator
	skillLister       SkillLister
	onProjectsChanged ProjectsChangedFn
	ops               *ServiceOps
	enrolmentOps      *EnrolmentOps
	auditOps          *AuditOps
	mcpOps            *McpOps
	projectOps        *ProjectOps
	enhanced          *EnhancedServiceRegistry
	// issuance is derived once here from auditOps' recorder so the socket mux,
	// the TCP mux and the login routes cannot disagree about whether an
	// issuance is recorded.
	issuance IssuanceAuditor
}

// registerFrontendRoutes builds the full relay-internal route set onto
// rr.Mux for rr.Transport, then mounts the manifest-driven dispatcher as the
// catch-all. Called once per transport (socket, and TCP when ListenLoopback
// runs) from the two call sites in this file, so the two muxes are always
// built by the same code and cannot drift apart.
//
// Every registrar takes rr itself and registers each route through
// rr.Handle(class, ...): a route whose class is not reachable on rr.Transport
// is never registered at all (ADR-015 decision 2), so calling all five
// registrars unconditionally on every transport is safe — the per-route
// class, not a call site here, decides what lands on TCP.
func registerFrontendRoutes(rr *control.RouteRegistrar, deps frontendRouteDeps) {
	RegisterProjectRoutes(rr, deps.store, deps.projectOps, deps.mcps, deps.tools, deps.enum, deps.skillLister, deps.onProjectsChanged)
	if deps.auditOps != nil {
		RegisterAuditRoutes(rr, deps.auditOps)
	}
	if deps.ops != nil {
		RegisterServiceRoutes(rr, deps.ops)
	}
	if deps.enrolmentOps != nil {
		RegisterEnrolmentRoutes(rr, deps.enrolmentOps)
	}
	if deps.mcpOps != nil {
		RegisterMcpRoutes(rr, deps.mcpOps)
	}

	// Catch-all dispatcher: any path not matched by a more specific handler
	// (project routes above) is resolved against the manifest registry and
	// reverse-proxied to the matching enhanced service. WS upgrades are
	// handled by the same dispatcher (it detects them from the request).
	dispatcher := NewFrontendDispatcher(deps.enhanced)

	// Session creation is the one proxied route relay must inspect: the
	// per-project model allowlist lives only in relay's settings, so it can
	// only be enforced in front of the proxy. The guard wraps the catch-all
	// dispatcher (rather than a single exact pattern) so trailing-slash and
	// sibling create paths can't route around it; it self-classifies the
	// request and forwards everything that isn't a session-create POST.
	//
	// control.ClassProxy, not control.ClassConfigure: what this mount reaches is whatever a
	// manifest declares — relayLLM's sessions, terminals and /ws included —
	// which relay cannot see and therefore cannot class as configuration.
	// The class is socket-only, so this registration is also the reason the
	// TCP mux has no catch-all: a near-miss like POST /api/services there is
	// a 405 from http.ServeMux rather than a proxied request (ADR-016
	// decision 4).
	rr.Handle(control.ClassProxy, "/", newSessionModelGuard(deps.store, dispatcher))
}

// NewFrontendServer wires the mux and binds the frontend Unix socket at 0600.
// skillLister supplies the live tool list used for out-of-band SKILL.md
// regeneration on project mutations; pass nil to disable.
//
// tools enumerates the live MCP tool list for the project-picker UI; nil
// makes the GET /api/mcps/{id}/tools endpoint return 503.
//
// enum asks a connected MCP for a scope field's real values (ADR-011
// decision 6). It rides on this server rather than on any other because the
// answer is disclosure — every mail account on the host — and this is the
// admin-authenticated surface.
//
// onProjectsChanged fires after every successful project mutation so the
// tray Settings webview can rebuild its state; nil suppresses fan-out.
//
// ops is the shared ServiceOps core (ADR-014) — the same instance the
// Services tab IPC handlers use, so a service started from curl and one
// started from the tray go through identical validation and the same
// Registry.
//
// enrolmentOps is ops's counterpart for the Remote Clients tab and the
// `remote` block (ADR-014): the same instance ipc_enrolments.go's handlers
// use, so an enrolment created from curl and one created from the tray share
// the CA and the revocation hook.
//
// auditOps is ops's counterpart for the Tool Calls tab (ADR-014): the same
// instance ipc_audit.go's handlers use, so a query run from curl sees
// identical redaction to one run from the tray. Its embedded *AuditRecorder
// is nil-safe and degrades to an empty result when auditing is off — nothing
// here needs to special-case that.
//
// mcpOps is ops's counterpart for the MCP Servers tab (ADR-014), but only
// for add and remove: ipc_mcps.go's authenticate and ipc_mcp_permissions.go's
// reset-permissions have no route here and never will (McpOps explains why),
// so this server only ever calls McpOps.Add and McpOps.Remove.
//
// The dispatcher is the single handler for every route not claimed by
// relay-internal endpoints (project and service routes). It reads from the
// enhanced-services registry to pick a target service per request — no
// hardcoded per-service handlers live here.
//
// authz decides which credential may exercise which class (ADR-015); nil
// allows everything, which is what the hermetic route tests want. auditor
// records every authorization decision; nil is safe and simply records
// nothing.
// projectOps is ops's counterpart for the Projects tab (ADR-014): the same
// instance ipc_projects.go's handlers use, so a project created from curl
// and one created from the tray share the presence gate and the audit
// record (ADR-017 decisions 2 and 3). A nil projectOps is filled in with a
// bare *ProjectOps{Store: store} so every existing caller that does not yet
// wire one keeps working — ungated, since a nil Gate inside it refuses
// every gated act rather than allowing one (§6.7's fail-closed rule).
func NewFrontendServer(store SettingsStore, mcps McpSurfaceProvider, tools MCPToolsProvider, enum ContextEnumerator, frontend Endpoint, enhanced *EnhancedServiceRegistry, skillLister SkillLister, onProjectsChanged ProjectsChangedFn, ops *ServiceOps, enrolmentOps *EnrolmentOps, auditOps *AuditOps, mcpOps *McpOps, projectOps *ProjectOps, authz control.Authorizer, auditor control.ControlAuditor) (*FrontendServer, error) {
	if frontend.Socket == "" {
		return nil, errors.New("frontend socket path is empty")
	}
	if enhanced == nil {
		return nil, errors.New("enhanced-services registry is nil")
	}
	if projectOps == nil {
		projectOps = &ProjectOps{Store: store}
	}

	deps := frontendRouteDeps{
		store:             store,
		mcps:              mcps,
		tools:             tools,
		enum:              enum,
		skillLister:       skillLister,
		onProjectsChanged: onProjectsChanged,
		ops:               ops,
		enrolmentOps:      enrolmentOps,
		auditOps:          auditOps,
		mcpOps:            mcpOps,
		projectOps:        projectOps,
		enhanced:          enhanced,
		issuance:          issuanceAuditorOrNil(auditOps.recorder()),
	}

	ensureFrontendTokenIsCredential(store, frontend.Token)

	socketMux := http.NewServeMux()
	registerFrontendRoutes(&control.RouteRegistrar{Mux: socketMux, Transport: control.TransportSocket, Authz: authz, Auditor: auditor, Reserve: deps.enhanced, CredentialID: APICredentialIDFromContext}, deps)

	// The socket door is composed through the same function the loopback one
	// is, with an empty public set: a browser cannot reach a Unix socket and
	// a socket has no origin, so there is no ceremony to serve here and the
	// public mux is nil rather than populated. Composing it anyway is what
	// keeps the two doors' shape identical.
	handler := frontendPublicDoor(nil, frontendCredentialAuth(store, frontendRecover(socketMux)))

	srv := &http.Server{
		Handler: handler,
		// Streaming sessions run for many minutes; only header/idle timeouts
		// apply, never write timeout (it would kill in-progress generations).
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       5 * time.Minute,
	}

	if err := os.MkdirAll(filepath.Dir(frontend.Socket), 0o700); err != nil {
		return nil, fmt.Errorf("create frontend socket dir: %w", err)
	}
	_ = os.Remove(frontend.Socket)
	ln, err := net.Listen("unix", frontend.Socket)
	if err != nil {
		return nil, fmt.Errorf("listen on frontend socket: %w", err)
	}
	if err := os.Chmod(frontend.Socket, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("chmod frontend socket: %w", err)
	}

	slog.Info("frontend server bound",
		"socket", frontend.Socket, "auth", frontend.Token != "")
	return &FrontendServer{
		socketPath: frontend.Socket,
		server:     srv,
		listener:   ln,
		routeDeps:  deps,
		authz:      authz,
		auditor:    auditor,
	}, nil
}

// Serve blocks accepting connections until Shutdown is called.
func (s *FrontendServer) Serve() error {
	if err := s.server.Serve(s.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown drains in-flight requests and unlinks the socket file.
func (s *FrontendServer) Shutdown(ctx context.Context) {
	if s == nil || s.server == nil {
		return
	}
	if err := s.server.Shutdown(ctx); err != nil {
		slog.Warn("frontend server did not drain cleanly", "error", err)
	}
	// The socket and TCP listeners are now served by two independent
	// *http.Server values (they carry different route sets), so draining
	// the socket's server no longer drains the loopback one for free.
	if s.tcpServer != nil {
		if err := s.tcpServer.Shutdown(ctx); err != nil {
			slog.Warn("frontend loopback server did not drain cleanly", "error", err)
		}
	}
	if s.listener != nil {
		_ = s.listener.Close()
	}
	if s.tcpLn != nil {
		_ = s.tcpLn.Close()
	}
	if s.socketPath != "" {
		_ = os.Remove(s.socketPath)
	}
}

var errFrontendTokenAlreadyCredential = errors.New("frontend token is already recorded as a credential")

// ensureFrontendTokenIsCredential makes RELAY_FRONTEND_TOKEN reach the API
// the only way anything reaches it: as a credential.
//
// This is deliberate, and it looks redundant because trayapp.go runs the same
// migration before it calls NewFrontendServer. The invariant belongs to the
// server, not to one caller's ordering: a server that admits a token no
// credential covers 401s every consumer it just handed that token to, and
// nothing in the constructor's signature would say so. The pre-check keeps
// the tray's start from writing settings.json a second time.
//
// Not fatal, matching the tray's stance on the same migration: a relay that
// fails this still starts, and frontendCredentialAuth then refuses the legacy
// token rather than admitting it unclassed.
func ensureFrontendTokenIsCredential(store SettingsStore, token string) {
	if store == nil || token == "" {
		return
	}
	if freshSettings(store).AuthenticateAPICredential(token) != nil {
		return
	}
	// This is subtle: the migration's own bool is the decline. The pre-check
	// above resolves through freshSettings and the reload inside the write
	// resolves again, so another process migrating in the gap is the case that
	// reaches here with nothing left to do — and a save there would rewrite
	// settings.json for no change on every relay start that lost that race.
	err := withDeclinable(store, func(s *Settings) error {
		if !migrateFrontendTokenToCredential(s, token) {
			return errFrontendTokenAlreadyCredential
		}
		return nil
	})
	if err != nil && !errors.Is(err, errFrontendTokenAlreadyCredential) {
		slog.Error("frontend: could not migrate the frontend token to a credential", "error", err)
	}
}

// frontendCredentialAuth admits any bearer that resolves to a credential in
// Settings.APICredentials (ADR-015 decision 3) and leaves what that credential
// may then DO to control.RouteRegistrar's per-route class check.
//
// This is the ONLY bearer check in front of either mux, deliberately. A second
// gate here admitting one fixed value would make every other credential
// unreachable and the class check behind it dead code — the two layers answer
// "is this anyone?" and "may they do this?", and only the second may narrow.
//
// Resolution runs before any handler, so an unauthenticated WS upgrade never
// allocates a session, and goes through Settings.findAPICredentialByHash,
// which compares in constant time over every credential on the host.
//
// An empty credential set fails CLOSED. The frontend channel always mints a
// token and ensureFrontendTokenIsCredential always records it, so empty means
// misconfiguration, and serving open would silently expose every proxied
// service.
//
// Absent, malformed and unknown bearers all get the same 401 with the same
// body, deliberately — a message that told them apart would be an oracle.
func frontendCredentialAuth(store SettingsStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var s *Settings
		if store != nil {
			s = freshSettings(store)
		}
		if s == nil || len(s.APICredentials) == 0 {
			slog.Error("frontend: no API credentials configured — rejecting all requests (fail closed)")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		token, ok := bearerToken(r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if s.AuthenticateAPICredential(token) == nil {
			slog.Warn("frontend: bad bearer token",
				"method", r.Method, "path", r.URL.Path)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// warnOnUnmatchedTCPRoute logs a request no pattern on the TCP mux claims —
// http.ServeMux answering with its own 404 or its own 405, no relay handler
// run. It sits inside frontendCredentialAuth, so only a caller who already
// authenticated can produce a line.
//
// This is deliberate: it logs and records nothing. An unregistered route is
// not an authorization decision, and a control.ControlDecision here would put an
// attacker-drivable write on the listener ADR-015 decision 2 deliberately
// leaves empty. The socket keeps no counterpart at all: its catch-all
// absorbs every unmatched path, so a miss there is the dispatcher's, not
// the mux's.
//
// This is subtle: the handler mux.Handler returns is discarded rather than
// served. Only ServeMux.ServeHTTP stores the wildcard values a handler reads
// back through r.PathValue, so serving it directly would empty every {id} in
// the route set.
func warnOnUnmatchedTCPRoute(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern == "" {
			slog.Warn(unmatchedRouteWarning,
				"method", r.Method, "path", r.URL.Path, "transport", string(control.TransportTCP))
		}
		mux.ServeHTTP(w, r)
	})
}

// unmatchedRouteWarning is a constant so a test can match the line without
// restating it.
const unmatchedRouteWarning = "frontend: no route registered for this request"

func frontendRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				slog.Error("frontend: panic in handler",
					"error", err, "method", r.Method, "path", r.URL.Path)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
