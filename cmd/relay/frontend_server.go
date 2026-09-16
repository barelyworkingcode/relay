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

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/login"
	"github.com/barelyworkingcode/relay/internal/peertoken"
	"github.com/barelyworkingcode/relay/internal/project"
	"github.com/barelyworkingcode/relay/internal/service"
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
		Handler:           frontendPublicDoor(publicMux, frontendCredentialAuth(s.routeDeps.store, nil, frontendRecover(warnOnUnmatchedTCPRoute(tcpMux)))),
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
	verifier, err := login.NewWebAuthnVerifier(origin, webauthnRPID)
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
	store             config.SettingsStore
	mcps              McpSurfaceProvider
	tools             MCPToolsProvider
	enum              project.ContextEnumerator
	skillLister       SkillLister
	onProjectsChanged ProjectsChangedFn
	ops               *ServiceOps
	enrolmentOps      *EnrolmentOps
	auditOps          *audit.AuditOps
	mcpOps            *McpOps
	projectOps        *ProjectOps
	hostOps           *HostOps
	// eveEnrolmentOps backs eve's own status/consume door
	// (docs/eve-passkey-enrolment.md). Unlike every other field here it has
	// no IPC-tab counterpart: Open is reachable only from the tray menu and
	// `relay eve enrol`, never from this server, so registerFrontendRoutes
	// wires only Status and Consume onto it.
	eveEnrolmentOps *EveEnrolmentOps
	// evePasskeyOps backs eve's mirror doors (docs/eve-passkey-enrolment.md):
	// PUT to report its list, GET to poll pending revocations. Like
	// eveEnrolmentOps it has no IPC-tab counterpart of its own on this
	// server -- the Passkeys tab's eve section reaches the same instance
	// through the tray's IPC door instead.
	evePasskeyOps *EvePasskeyOps
	enhanced      *EnhancedServiceRegistry
	// sessionHost bundles what RegisterSessionRoutes (session_routes.go) needs
	// to authorize a launch, reach relay-sessions and keep relay's ledger and
	// model-key tables in sync (plan-broker-and-sessions.md §2 C5). Its zero
	// value is a legitimate "session routes not wired" -- sessionRouteDeps.ready()
	// is what registerFrontendRoutes checks before registering anything.
	sessionHost sessionRouteDeps
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
	RegisterTemplateRoutes(rr, deps.store)
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
	if deps.hostOps != nil {
		RegisterHostRoutes(rr, deps.hostOps)
	}
	if deps.eveEnrolmentOps != nil {
		RegisterEveEnrolmentRoutes(rr, deps.eveEnrolmentOps)
	}
	if deps.evePasskeyOps != nil {
		RegisterEvePasskeyRoutes(rr, deps.evePasskeyOps)
	}
	if deps.sessionHost.ready() {
		RegisterSessionRoutes(rr, deps.sessionHost)
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
// identical redaction to one run from the tray. Its embedded *audit.AuditRecorder
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
// eveEnrolmentOps backs eve's own status/consume door
// (docs/eve-passkey-enrolment.md) -- the same instance the tray menu item
// and `relay eve enrol` call Open on. A nil eveEnrolmentOps is filled in with
// a bare *EveEnrolmentOps{Store: store}, the same discipline projectOps and
// hostOps follow: Consume is ungated, so this is never "everyone can open a
// window," only "everyone can ask whether one is open and try to consume
// it."
//
// evePasskeyOps backs eve's PUT/GET mirror doors alongside eveEnrolmentOps
// -- the same instance the Passkeys tab's eve section and `relay eve
// list|revoke` use. A nil evePasskeyOps is filled in the same way: Report
// and the revocations GET are both ungated, so this is "eve may tell relay
// its list and ask what's pending," never "everyone may revoke."
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
//
// launches resolves a socket peer's launch identity (docs/launch-identity.md);
// nil admits no caller by identity, only by bearer.
//
// sessionHost is variadic rather than a plain trailing parameter so every
// call site built before R-S4b (this repo's own tests included) keeps
// compiling unchanged: omitted, it is sessionRouteDeps{}, whose ready()
// reads false and registers no session routes at all, the same "wired
// nothing, refuses everything that needs it" default every other optional
// dependency in this constructor already has (see projectOps/hostOps/
// eveEnrolmentOps/evePasskeyOps's own nil-fallback comments above). A second
// or later value is ignored -- there is exactly one session-host deps set
// per server.
func NewFrontendServer(store config.SettingsStore, mcps McpSurfaceProvider, tools MCPToolsProvider, enum project.ContextEnumerator, frontend Endpoint, enhanced *EnhancedServiceRegistry, skillLister SkillLister, onProjectsChanged ProjectsChangedFn, ops *ServiceOps, enrolmentOps *EnrolmentOps, auditOps *audit.AuditOps, mcpOps *McpOps, projectOps *ProjectOps, hostOps *HostOps, eveEnrolmentOps *EveEnrolmentOps, evePasskeyOps *EvePasskeyOps, authz control.Authorizer, auditor control.ControlAuditor, launches *service.Launches, sessionHost ...sessionRouteDeps) (*FrontendServer, error) {
	if frontend.Socket == "" {
		return nil, errors.New("frontend socket path is empty")
	}
	if enhanced == nil {
		return nil, errors.New("enhanced-services registry is nil")
	}
	if projectOps == nil {
		projectOps = &ProjectOps{Store: store}
	}
	if hostOps == nil {
		hostOps = &HostOps{Store: store}
	}
	if eveEnrolmentOps == nil {
		eveEnrolmentOps = &EveEnrolmentOps{Store: store}
	}
	if evePasskeyOps == nil {
		evePasskeyOps = &EvePasskeyOps{Store: store}
	}

	var sessionDeps sessionRouteDeps
	if len(sessionHost) > 0 {
		sessionDeps = sessionHost[0]
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
		hostOps:           hostOps,
		eveEnrolmentOps:   eveEnrolmentOps,
		evePasskeyOps:     evePasskeyOps,
		enhanced:          enhanced,
		sessionHost:       sessionDeps,
		issuance:          issuanceAuditorOrNil(auditOps.Recorder()),
	}

	socketMux := http.NewServeMux()
	registerFrontendRoutes(&control.RouteRegistrar{Mux: socketMux, Transport: control.TransportSocket, Authz: authz, Auditor: auditor, Reserve: deps.enhanced, CredentialID: APICredentialIDFromContext}, deps)

	// The socket door is composed through the same function the loopback one
	// is, with an empty public set: a browser cannot reach a Unix socket and
	// a socket has no origin, so there is no ceremony to serve here and the
	// public mux is nil rather than populated. Composing it anyway is what
	// keeps the two doors' shape identical.
	handler := frontendPublicDoor(nil, frontendCredentialAuth(store, launches, frontendRecover(withRelayRouteReadDeadline(socketMux))))

	srv := &http.Server{
		Handler:     handler,
		ConnContext: withFrontendPeerFromConn,
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

	slog.Info("frontend server bound", "socket", frontend.Socket)
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

type frontendPeerCtxKey struct{}

// withFrontendPeerFromConn records the socket peer's audit token once per
// connection. A peer that cannot be read records nothing, and matches no
// identity.
func withFrontendPeerFromConn(ctx context.Context, c net.Conn) context.Context {
	tok, err := peertoken.FromConn(c)
	if err != nil {
		return ctx
	}
	return context.WithValue(ctx, frontendPeerCtxKey{}, tok)
}

func frontendPeerFromContext(ctx context.Context) peertoken.Token {
	tok, _ := ctx.Value(frontendPeerCtxKey{}).(peertoken.Token)
	return tok
}

// frontendCredentialAuth answers "is this anyone?" in one of two ways and
// leaves "may they do this?" to control.RouteRegistrar's per-route class
// check.
//
// A request with no Authorization header whose connection's peer is bound to
// a launch identity holding the frontend capability is that identity, holding
// exactly frontendConsumerClasses. It is resolved per request, not per connection,
// so a connection outliving its launch stops being admitted.
//
// Every other request must present a bearer that resolves to a credential in
// Settings.APICredentials (ADR-015 decision 3). This is the ONLY bearer check
// in front of either mux, deliberately. A second gate here admitting one
// fixed value would make every other credential unreachable and the class
// check behind it dead code.
//
// Resolution runs before any handler, so an unauthenticated WS upgrade never
// allocates a session, and goes through findAPICredentialByHash,
// which compares in constant time over every credential on the host.
//
// On the bearer path an empty credential set fails CLOSED: serving open would
// silently expose every proxied service.
//
// Absent, malformed and unknown bearers, and a headerless caller with no
// frontend identity, all get the same 401 with the same body, deliberately —
// a message that told them apart would be an oracle.
func frontendCredentialAuth(store config.SettingsStore, launches *service.Launches, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, hasHeader := r.Header["Authorization"]; !hasHeader {
			id, ok := launches.Lookup(frontendPeerFromContext(r.Context()))
			if !ok || !id.Allows(service.OpFrontendSocket) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(withFrontendIdentity(r.Context(), id)))
			return
		}
		var s *config.Settings
		if store != nil {
			s = config.FreshSettings(store)
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
		if authenticateAPICredential(s, token) == nil {
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
// It also carries the TCP mux's read-deadline duty (withRelayRouteReadDeadline's
// doc comment explains why): both need mux.Handler(r)'s matched pattern
// before ServeHTTP runs, and the TCP mux never registers the "/" catch-all
// (ClassProxy is socket-only), so every match here is one of relay's own
// routes and gets the deadline unconditionally.
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
		} else {
			r = setFrontendRouteReadDeadline(w, r)
		}
		mux.ServeHTTP(w, r)
	})
}

// unmatchedRouteWarning is a constant so a test can match the line without
// restating it.
const unmatchedRouteWarning = "frontend: no route registered for this request"

// frontendRouteReadDeadline bounds how long relay waits to receive the body
// of a request against one of ITS OWN routes (projects, services, audit,
// MCPs, hosts, enrolments). It must never reach the "/" catch-all: sessions
// and terminals proxied through it run for many minutes, and a WS upgrade
// on that path is a long-lived connection, not a slow request.
//
// A var, not a const, so a test can shorten it rather than trickle a body
// for the real 10s (the same accommodation MCPRequestTimeout makes).
var frontendRouteReadDeadline = 10 * time.Second

// readDeadlineExtenderKey is unexported so the only way to reach the value
// it names is readDeadlineExtenderFromContext below — the same
// context-carries-a-capability shape presence.CallerSessionFromContext
// already uses.
type readDeadlineExtenderKey struct{}

// readDeadlineExtender re-arms the read deadline on the connection a
// request arrived on. d == 0 clears it outright (no artificial bound); d >
// 0 sets it to time.Now().Add(d). It exists so a presence-gated handler can
// suspend frontendRouteReadDeadline for the human-timescale wait a prompt
// takes, then restore it once that wait is over — see requireGate.
type readDeadlineExtender func(d time.Duration)

func withReadDeadlineExtender(ctx context.Context, extend readDeadlineExtender) context.Context {
	return context.WithValue(ctx, readDeadlineExtenderKey{}, extend)
}

// readDeadlineExtenderFromContext is presence_gate.go's hook back into the
// connection a gated request arrived on. It is absent for every caller of
// requireGate that isn't a frontend HTTP route (IPC, CLI) — requireGate's
// ok check treats that as "nothing to extend around", not an error, since
// neither of those doors sets this deadline in the first place.
func readDeadlineExtenderFromContext(ctx context.Context) (readDeadlineExtender, bool) {
	extend, ok := ctx.Value(readDeadlineExtenderKey{}).(readDeadlineExtender)
	return extend, ok
}

// setFrontendRouteReadDeadline arms frontendRouteReadDeadline on r's
// connection and returns r carrying a readDeadlineExtender bound to that
// same connection, for requireGate to reach for later. Every caller must
// serve the returned request, not the one it was given — the extender is
// only reachable through the context on the new value.
func setFrontendRouteReadDeadline(w http.ResponseWriter, r *http.Request) *http.Request {
	rc := http.NewResponseController(w)
	extend := func(d time.Duration) {
		var deadline time.Time
		if d > 0 {
			deadline = time.Now().Add(d)
		}
		// Best-effort: a client that already dropped the connection makes
		// this fail in some ordinary, non-actionable way (not
		// http.ErrNotSupported), and there is nothing a handler already
		// past the presence gate can do about that but proceed and let its
		// own write fail where it fails.
		_ = rc.SetReadDeadline(deadline)
	}
	if err := rc.SetReadDeadline(time.Now().Add(frontendRouteReadDeadline)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		slog.Warn("frontend: could not set read deadline", "error", err, "path", r.URL.Path)
	}
	return r.WithContext(withReadDeadlineExtender(r.Context(), extend))
}

// withRelayRouteReadDeadline sets frontendRouteReadDeadline before serving
// any request that resolves to one of relay's own registered patterns on
// mux, and leaves the "/" catch-all (the proxied dispatcher, including WS
// upgrades) untouched — that mount is registered by registerFrontendRoutes
// on the socket mux only (control.ClassProxy is socket-only), so this is the
// socket door's counterpart to warnOnUnmatchedTCPRoute's deadline duty on
// the TCP door.
//
// mux.Handler(r) only looks up the match; it neither invokes nor consumes
// the request, so calling it ahead of ServeHTTP is safe — the same
// technique warnOnUnmatchedTCPRoute uses to inspect the match before
// dispatch.
func withRelayRouteReadDeadline(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" && pattern != "/" {
			r = setFrontendRouteReadDeadline(w, r)
		}
		mux.ServeHTTP(w, r)
	})
}

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
