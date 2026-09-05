package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/enrolment"
	"github.com/barelyworkingcode/relay/internal/jsonrpc"
	"github.com/barelyworkingcode/relay/internal/project"
)

// boolOr is a generic "absent means def" reader for a *bool config field. It
// is not audit-domain logic — it happens to be needed here and by
// ipc_enrolments.go, both reading unrelated *bool settings fields, so it
// stays a small shared helper in main rather than exported from
// internal/audit (which has its own private copy for the same reason).
func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// defaultRemoteListen binds loopback: reaching relay from a VM must be a
// deliberate act, never the default outcome of leaving a field unset.
const defaultRemoteListen = "127.0.0.1:9910"

// defaultEnrolmentListen is the enrolment-request listener's own default —
// a distinct port from defaultRemoteListen, loopback for the same reason
// (ADR-010 decision 9).
const defaultEnrolmentListen = "127.0.0.1:9911"

// mountALPN selects the 9P mount plane at TLS handshake time — the only
// way the two planes can be told apart, since ReqMountAttach is
// deliberately absent from every request-type dispatch table.
const mountALPN = "relay-9p/1"

var (
	// remoteHandshakeTimeout is the slowloris bound: a peer that connects
	// and never speaks holds a goroutine and an fd until it expires.
	remoteHandshakeTimeout = 15 * time.Second

	// remoteIdleTimeout bounds silence, never work in progress: FrameConn
	// pushes the deadline out on every frame read and written.
	//
	// Vars rather than consts so tests can shorten them.
	remoteIdleTimeout = 5 * time.Minute
)

type resolvedRemoteConfig struct {
	Enabled bool
	Listen  string
}

func resolveRemoteConfig(c *config.RemoteConfig) resolvedRemoteConfig {
	if c == nil {
		return resolvedRemoteConfig{Enabled: false, Listen: defaultRemoteListen}
	}
	out := resolvedRemoteConfig{
		Enabled: boolOr(c.Enabled, false),
		Listen:  c.Listen,
	}
	if out.Listen == "" {
		out.Listen = defaultRemoteListen
	}
	return out
}

// resolveRemoteEnrolment derives the enrolment-request listener's config from
// the same block resolveRemoteConfig reads for the tool-plane listener.
// Unlike resolveRemoteConfig, this can fail: enrolment_requests:true with
// enabled:false names a configuration mistake rather than something to
// silently take at face value — the enrolment channel is a companion to the
// tool-plane listener, not a substitute for enabling it, and letting it
// start on its own would mean "no remote access" in settings.json no longer
// implied "no network door open."
func resolveRemoteEnrolment(c *config.RemoteConfig) (resolvedRemoteConfig, error) {
	wantEnrolment := c != nil && boolOr(c.EnrolmentRequests, false)
	if !wantEnrolment {
		return resolvedRemoteConfig{Enabled: false, Listen: defaultEnrolmentListen}, nil
	}
	if !resolveRemoteConfig(c).Enabled {
		return resolvedRemoteConfig{}, fmt.Errorf(
			"remote.enrolment_requests is true but remote.enabled is false: the enrolment-request channel " +
				"is a companion to the remote tool-plane listener, not a replacement for it — set remote.enabled " +
				"to true as well, or turn enrolment_requests off")
	}
	listen := c.EnrolmentListen
	if listen == "" {
		listen = defaultEnrolmentListen
	}
	return resolvedRemoteConfig{Enabled: true, Listen: listen}, nil
}

// RemoteToolRouter is a separate, two-method interface so the narrowing is
// a compile-time property: this file cannot call ResolvePtyEnv even by
// accident, because it holds nothing with that method.
type RemoteToolRouter interface {
	ListTools(ctx context.Context, token string) (json.RawMessage, error)
	CallTool(ctx context.Context, name string, args json.RawMessage, token string) (json.RawMessage, error)
}

var _ RemoteToolRouter = (*appRouter)(nil)

type remoteHandler func(ctx context.Context, req *bridge.RemoteRequest, router RemoteToolRouter, token string) bridge.BridgeResponse

// remoteHandlers has exactly two entries; no request type outside this map
// reaches any router method at all.
var remoteHandlers = map[string]remoteHandler{
	bridge.ReqListTools: handleRemoteListTools,
	bridge.ReqCallTool:  handleRemoteCallTool,
}

func handleRemoteListTools(ctx context.Context, _ *bridge.RemoteRequest, router RemoteToolRouter, token string) bridge.BridgeResponse {
	tools, err := router.ListTools(ctx, token)
	if err != nil {
		return bridge.ErrorResponse(bridge.ErrorCode(err), err.Error())
	}
	return bridge.BridgeResponse{Type: bridge.RespTools, Tools: tools}
}

func handleRemoteCallTool(ctx context.Context, req *bridge.RemoteRequest, router RemoteToolRouter, token string) bridge.BridgeResponse {
	ctx = bridge.WithArgsSHA256(ctx, req.ArgsSHA256)
	result, err := router.CallTool(ctx, req.Name, req.Arguments, token)
	if err != nil {
		return bridge.ErrorResponse(bridge.ErrorCode(err), err.Error())
	}
	return bridge.BridgeResponse{Type: bridge.RespResult, Result: result}
}

// RemoteConfigurer is the two-method surface the configuration plane holds,
// narrow for the same compile-time reason RemoteToolRouter is: this file
// cannot call ProjectOps.Update even by accident, because it holds nothing
// with that method.
type RemoteConfigurer interface {
	DescribeGrant(s *config.Settings, proj *config.Project) grantView
	NarrowForEnrolment(ctx context.Context, projectID string, f project.NarrowFields, caller bridge.RemoteCaller, surfaces func() project.McpSurfaces) (config.Project, []string, error)
}

var _ RemoteConfigurer = (*ProjectOps)(nil)

// remoteConfigHandler mirrors remoteHandler's shape: everything a config
// handler needs, resolved once by handleRequest and handed down rather than
// re-resolved.
type remoteConfigHandler func(ctx context.Context, req *bridge.RemoteRequest, configurer RemoteConfigurer, surfaces func() project.McpSurfaces, settings *config.Settings, proj *config.Project, caller bridge.RemoteCaller) bridge.BridgeResponse

// remoteConfigEntry pairs a handler with the capability class its operation
// carries. buildRemoteConfigHandlers refuses to install an entry whose
// class is not reachable on TCP, so an execute- or proxy-class operation
// added here later is absent from the table rather than refused inside it.
type remoteConfigEntry struct {
	class   control.CapabilityClass
	handler remoteConfigHandler
}

// buildRemoteConfigHandlers drops any entry whose class control.ClassReachableOn
// refuses for control.TransportTCP — the exact structural shape control.RouteRegistrar.Handle
// uses, applied to this dispatch table for the first time
// (ADR-010 decision 4's "route" meant a map entry before control.RouteRegistrar
// existed; this is the same idea, catching up).
func buildRemoteConfigHandlers(entries map[string]remoteConfigEntry) map[string]remoteConfigEntry {
	out := make(map[string]remoteConfigEntry, len(entries))
	for reqType, entry := range entries {
		if !control.ClassReachableOn(entry.class, control.TransportTCP) {
			continue
		}
		out[reqType] = entry
	}
	return out
}

// remoteConfigHandlers is consulted ONLY for an enrolment whose record
// carries cli_admin. It is a separate table from remoteHandlers, not extra
// entries in it: the tool plane and the configuration plane are different
// grants, and a table that mixed them would make "what can a certificate
// without cli-admin reach" a question about a field rather than about a map.
var remoteConfigHandlers = buildRemoteConfigHandlers(map[string]remoteConfigEntry{
	bridge.ReqDescribeGrant: {control.ClassRead, handleRemoteDescribeGrant},
	bridge.ReqNarrowGrant:   {control.ClassConfigure, handleRemoteNarrowGrant},
})

func handleRemoteDescribeGrant(_ context.Context, _ *bridge.RemoteRequest, configurer RemoteConfigurer, _ func() project.McpSurfaces, settings *config.Settings, proj *config.Project, _ bridge.RemoteCaller) bridge.BridgeResponse {
	data, err := json.Marshal(configurer.DescribeGrant(settings, proj))
	if err != nil {
		return bridge.ErrorResponse(jsonrpc.CodeInternalError, "describe grant: "+err.Error())
	}
	return bridge.BridgeResponse{Type: bridge.RespResult, Result: data}
}

// remoteNarrowGrantResult carries what changed and the resulting posture —
// the same grantView DescribeGrant and `relay grant` show — so a caller
// narrowing its own grant sees the result without a second round trip.
type remoteNarrowGrantResult struct {
	Changed []string  `json:"changed"`
	Grant   grantView `json:"grant"`
}

func handleRemoteNarrowGrant(ctx context.Context, req *bridge.RemoteRequest, configurer RemoteConfigurer, surfaces func() project.McpSurfaces, settings *config.Settings, proj *config.Project, caller bridge.RemoteCaller) bridge.BridgeResponse {
	f, err := project.DecodeNarrowFields(req.Arguments)
	if err != nil {
		// Strict decoding: a client sending allow_cwd_auth or any other
		// field this struct doesn't declare lands here loudly.
		return bridge.ErrorResponse(jsonrpc.CodeInvalidParams, "narrow grant: "+err.Error())
	}
	updated, changed, err := configurer.NarrowForEnrolment(ctx, proj.ID, f, caller, surfaces)
	if err != nil {
		return bridge.ErrorResponse(bridge.ErrorCode(err), err.Error())
	}
	data, err := json.Marshal(remoteNarrowGrantResult{Changed: changed, Grant: configurer.DescribeGrant(settings, &updated)})
	if err != nil {
		return bridge.ErrorResponse(jsonrpc.CodeInternalError, "narrow grant: "+err.Error())
	}
	return bridge.BridgeResponse{Type: bridge.RespResult, Result: data}
}

// cliAdminRequiredMessage names the permission (§4.5 of the spec this
// implements): it discloses nothing a client binary and its docs don't
// already say, and the caller is a legitimate party that needs to know what
// to ask the human for.
func cliAdminRequiredMessage(clientID string) string {
	return fmt.Sprintf("this request needs the cli-admin permission on enrolment %q, which is not set; "+
		"a human must turn it on with 'relay enrol update --client-id %s --cli-admin' on the host", clientID, clientID)
}

// A nil *RemoteServer is a valid "no listener configured" value and every
// method tolerates it, so callers need no branch for the disabled case.
type RemoteServer struct {
	router RemoteToolRouter
	store  config.SettingsStore
	cfg    resolvedRemoteConfig
	audit  *audit.AuditRecorder
	// configurer is nil in every deployment that never wires one, and a nil
	// value means the configuration table is absent — handleRequest treats
	// every config request type as unknown, fail-closed. Deliberately not a
	// *ProjectOps: see RemoteConfigurer's own doc comment.
	configurer RemoteConfigurer
	// surfaces resolves live MCP schemas for NarrowForEnrolment's validation.
	// A read-only capability, unlike configurer: this cannot register, remove
	// or reconfigure an MCP, only see what one currently declares.
	surfaces func() project.McpSurfaces

	listener net.Listener
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	// conns is keyed by certificate fingerprint, not client id: by the time
	// the revocation hook fires, the record naming a client id is gone.
	mu    sync.Mutex
	conns map[string]map[*remoteConn]struct{}

	// mountBudgets is the mount plane's shared budget ledger — one for the
	// whole server, not one per session: Budgets is keyed by fingerprint and
	// is shared across every connection from that enrolment, which is the
	// point of putting the bound on the enrolment rather than the session.
	mountBudgets *enrolment.Budgets

	// mountConns is the live mount sessions, a flat set (not
	// fingerprint-keyed like conns): revocation already closes the
	// underlying conn via the conns table — a mount connection is tracked
	// there too — this table exists only so revalidateMounts can find live
	// sessions to freshness-check, a different question from revocation.
	mountConns map[*mountConn]struct{}
}

func (s *RemoteServer) currentSettings() *config.Settings {
	return config.FreshSettings(s.store)
}

// remoteAuditingLive requires both halves; neither implies the other. The
// recorder can die under a listener that started cleanly, independently of
// settings flipping audit.enabled off in another process.
func remoteAuditingLive(s *config.Settings, rec *audit.AuditRecorder) bool {
	return rec.Enabled() && audit.ResolveAuditConfig(s.Audit).Enabled
}

type remoteConn struct {
	conn        net.Conn
	clientID    string
	fingerprint string
}

func NewRemoteServer(ctx context.Context, store config.SettingsStore, router RemoteToolRouter, audit *audit.AuditRecorder, configurer RemoteConfigurer, surfaces func() project.McpSurfaces) (*RemoteServer, error) {
	// freshSettings, not Get(): the operator who just edited settings.json is
	// the same operator watching the listener come up.
	settings := config.FreshSettings(store)
	cfg := resolveRemoteConfig(settings.Remote)
	if !cfg.Enabled {
		slog.Debug("remote listener not enabled; no socket opened")
		return nil, nil
	}

	if !remoteAuditingLive(settings, audit) {
		return nil, fmt.Errorf("remote listener refuses to start while the tool-call audit log is disabled: " +
			"a remote grant is justified by the calls it records, so serving remote traffic unrecorded is not a degraded mode — " +
			"set audit.enabled to true, or remove the remote block from settings.json")
	}

	ca, err := enrolment.LoadOrCreateCA(store.Sealer())
	if err != nil {
		return nil, fmt.Errorf("remote listener: %w", err)
	}
	// Issued per process start rather than persisted: the client verifies
	// against the CA certificate in its bundle.
	cert, err := ca.IssueServerCert(remoteCertHosts(cfg.Listen)...)
	if err != nil {
		return nil, fmt.Errorf("remote listener: %w", err)
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    ca.Pool(),
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{mountALPN},
	}

	ln, err := tls.Listen("tcp", cfg.Listen, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("remote listener: listen on %s: %w", cfg.Listen, err)
	}

	sctx, cancel := context.WithCancel(ctx)
	s := &RemoteServer{
		router:       router,
		store:        store,
		cfg:          cfg,
		audit:        audit,
		configurer:   configurer,
		surfaces:     surfaces,
		listener:     ln,
		ctx:          sctx,
		cancel:       cancel,
		conns:        map[string]map[*remoteConn]struct{}{},
		mountBudgets: &enrolment.Budgets{},
		mountConns:   map[*mountConn]struct{}{},
	}

	// Installed with an owner because a rebind (RemoteSupervisor) binds the
	// new listener before closing the old one, so the old one's teardown
	// must be able to tell the hook is no longer its own.
	enrolment.SetRevocationHookFor(s, s.closeEnrolment)

	slog.Info("remote listener started", "addr", ln.Addr().String())
	return s, nil
}

// A wildcard bind cannot be enumerated, so `0.0.0.0` alone gets loopback
// only; an operator who binds a specific address gets a SAN for it too.
func remoteCertHosts(listen string) []string {
	hosts := []string{"127.0.0.1", "::1", "localhost"}
	host, _, err := net.SplitHostPort(listen)
	if err != nil || host == "" {
		return hosts
	}
	if host == "0.0.0.0" || host == "::" || slices.Contains(hosts, host) {
		return hosts
	}
	return append(hosts, host)
}

func (s *RemoteServer) Addr() string {
	if s == nil {
		return ""
	}
	return s.listener.Addr().String()
}

func (s *RemoteServer) Serve() error {
	if s == nil {
		return nil
	}
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return err
		}
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

func (s *RemoteServer) StopAccepting() {
	if s == nil {
		return
	}
	s.cancel()
	_ = s.listener.Close()
}

func (s *RemoteServer) Close() {
	if s == nil {
		return
	}
	s.StopAccepting()
	// Compare-and-clear, never unconditional: on a rebind the replacement
	// already owns the hook by the time this runs.
	enrolment.ClearRevocationHookFor(s)
	s.wg.Wait()
}

// handleConn resolves the certificate to an enrolment and only then reads a
// request, so an unenrolled caller cannot probe for valid grants or names.
func (s *RemoteServer) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("remote handler panic (recovered)", "panic", r)
		}
	}()

	// Two names rather than one reassigned variable: the goroutine below
	// reads connCtx concurrently, and rebinding it would be a race.
	connCtx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	go func() {
		<-connCtx.Done()
		_ = conn.Close()
	}()

	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		// Unreachable: the listener is tls.Listen.
		slog.Error("remote: non-TLS connection on the remote listener; closing")
		return
	}

	_ = conn.SetDeadline(time.Now().Add(remoteHandshakeTimeout))
	if err := tlsConn.HandshakeContext(connCtx); err != nil {
		slog.Warn("remote: TLS handshake failed", "remote_addr", conn.RemoteAddr().String(), "error", err)
		return
	}

	state := tlsConn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		// Unreachable under RequireAndVerifyClientCert.
		slog.Warn("remote: handshake completed with no peer certificate; closing", "remote_addr", conn.RemoteAddr().String())
		return
	}

	fingerprint := enrolment.FingerprintCert(state.PeerCertificates[0])
	// currentSettings, not Get(): a cached view here would refuse a
	// brand-new enrolment until the tray's next settings poll.
	settings := s.currentSettings()
	enr := enrolment.FindByFingerprint(settings, fingerprint)
	if enr == nil {
		slog.Warn("remote: closing connection, certificate is not enrolled",
			"fingerprint", fingerprint, "remote_addr", conn.RemoteAddr().String())
		return
	}

	// Re-checked here, not just at startup: auditing could have been
	// turned off since this listener bound.
	if !remoteAuditingLive(settings, s.audit) {
		slog.Error("remote: closing connection, auditing is not active",
			"client_id", enr.ClientID, "remote_addr", conn.RemoteAddr().String())
		return
	}

	ctx := bridge.WithRemoteCaller(connCtx, bridge.RemoteCaller{
		ClientID:    enr.ClientID,
		Fingerprint: fingerprint,
		RemoteAddr:  conn.RemoteAddr().String(),
	})

	rc := &remoteConn{conn: conn, clientID: enr.ClientID, fingerprint: fingerprint}
	s.track(rc)
	defer s.untrack(rc)

	if state.NegotiatedProtocol == mountALPN {
		remoteCaller := bridge.RemoteCaller{
			ClientID:    enr.ClientID,
			Fingerprint: fingerprint,
			RemoteAddr:  conn.RemoteAddr().String(),
		}
		s.serveMount(conn, remoteCaller, fingerprint)
		return
	}

	slog.Info("remote client connected", "client_id", enr.ClientID, "remote_addr", conn.RemoteAddr().String())

	bridge.NewFrameConn(conn, "remote", remoteIdleTimeout).
		Serve(ctx, func(ctx context.Context, line string) bridge.BridgeResponse {
			return s.handleRequest(ctx, fingerprint, line)
		})
}

// handleRequest resolves the caller BEFORE choosing a dispatch table
// (decode -> resolveCaller -> pick table -> dispatch): which table applies
// depends on the enrolment's CLIAdmin bit, read fresh on every request, never cached
// from handleConn's one-time resolution — see resolveCaller's own doc
// comment for why that placement matters. The cost, stated plainly: an
// unknown request type now pays one freshSettings stat before being
// refused, and a revoked certificate sending one gets "this certificate is
// no longer enrolled" rather than "not available to remote clients" — both
// are refusals, and neither leaks anything the other didn't.
func (s *RemoteServer) handleRequest(ctx context.Context, fingerprint, line string) bridge.BridgeResponse {
	req, err := bridge.DecodeRemoteRequest([]byte(line))
	if err != nil {
		// Strict decoding: a client sending `cwd` or `token` lands here
		// loudly rather than the field being silently ignored.
		return bridge.ErrorResponse(jsonrpc.CodeInvalidParams, "remote request: "+err.Error())
	}

	settings, enrolment, proj, err := s.resolveCaller(fingerprint, req.ProjectID)
	if err != nil {
		return bridge.ErrorResponse(bridge.ErrorCode(err), err.Error())
	}

	if h, ok := remoteHandlers[req.Type]; ok {
		token, err := revealProjectToken(proj)
		if err != nil {
			return bridge.ErrorResponse(bridge.ErrorCode(err), err.Error())
		}
		return h(ctx, req, s.router, token)
	}

	if entry, ok := remoteConfigHandlers[req.Type]; ok {
		caller, _ := bridge.RemoteCallerFromContext(ctx)
		if !enrolment.CLIAdmin {
			s.recordConfigRefusal(entry.class, req.Type, caller)
			return bridge.ErrorResponse(jsonrpc.CodeMethodNotFound, cliAdminRequiredMessage(enrolment.ClientID))
		}
		// A nil configurer means the configuration table is absent (fail
		// closed) — no different, from the wire, than a type nobody
		// registered. Not a control.ControlDecision: there is no operation this
		// listener actually carries to measure the refusal against, the
		// same reasoning ADR-015 gives for an unmatched TCP route.
		if s.configurer == nil {
			slog.Warn("remote: configuration request type has no configurer wired", "type", req.Type)
			return bridge.ErrorResponse(jsonrpc.CodeMethodNotFound,
				"request type is not available to remote clients: "+req.Type)
		}
		return entry.handler(ctx, req, s.configurer, s.surfaces, settings, proj, caller)
	}

	slog.Warn("remote: request type is not available on the remote listener", "type", req.Type)
	return bridge.ErrorResponse(jsonrpc.CodeMethodNotFound,
		"request type is not available to remote clients: "+req.Type)
}

// recordConfigRefusal is the control_decision half of refusing a config
// request for want of cli-admin: the request named a real operation on a
// real table, so — unlike an unregistered request type — this IS an
// authorization decision, and ADR-015's record is what answers "did this
// identity try" after the fact.
func (s *RemoteServer) recordConfigRefusal(class control.CapabilityClass, reqType string, caller bridge.RemoteCaller) {
	s.audit.RecordDecision(control.ControlDecision{
		Method:      reqType,
		Class:       class,
		Transport:   control.TransportTCP,
		Allowed:     false,
		Reason:      "cli-admin is not set on this enrolment",
		ClientID:    caller.ClientID,
		Fingerprint: caller.Fingerprint,
	})
}

// resolveCaller is the shared middle of every remote request: the audit-live check, the
// fingerprint -> enrolment resolution, the project_id defaulting,
// GrantsProject, and the IsRemote() re-check. The enrolment and project it
// returns are the ONLY objects a remote request can act on — the
// configuration plane's self-scoping (§4.2) rests on this being the single
// place identity and target are resolved, for both the tool plane and the
// configuration plane alike.
//
// Re-resolved from current on-disk settings on every call, never cached: a
// revocation, or a cli_admin toggle, must take effect on the NEXT request,
// not the next reconnect (handleConn resolves the enrolment once too, at
// the TLS handshake, but only to decide whether to accept the connection at
// all — never consult that copy for an authorization decision made later in
// the connection's life).
func (s *RemoteServer) resolveCaller(fingerprint, projectID string) (*config.Settings, *config.Enrolment, *config.Project, error) {
	settings := s.currentSettings()

	if !remoteAuditingLive(settings, s.audit) {
		return nil, nil, nil, jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized,
			fmt.Errorf("remote calls are refused while the tool-call audit log is disabled"))
	}

	enr := enrolment.FindByFingerprint(settings, fingerprint)
	if enr == nil {
		return nil, nil, nil, jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized,
			fmt.Errorf("this certificate is no longer enrolled"))
	}

	if projectID == "" {
		switch len(enr.ProjectIDs) {
		case 0:
			return nil, nil, nil, jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized,
				fmt.Errorf("enrolment %q holds no grants", enr.ClientID))
		case 1:
			projectID = enr.ProjectIDs[0]
		default:
			return nil, nil, nil, jsonrpc.NewCodedError(jsonrpc.CodeInvalidParams,
				fmt.Errorf("project_id is required: enrolment %q holds %d grants", enr.ClientID, len(enr.ProjectIDs)))
		}
	}

	if !enr.GrantsProject(projectID) {
		return nil, nil, nil, jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized,
			fmt.Errorf("enrolment %q does not grant project %q", enr.ClientID, projectID))
	}

	proj, _ := config.FindProjectByID(settings, projectID)
	if proj == nil {
		return nil, nil, nil, jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized,
			fmt.Errorf("project %q no longer exists", projectID))
	}
	// IsRemote(), never a Kind comparison: the zero value is local and must
	// stay unreadable as remote.
	if !proj.IsRemote() {
		return nil, nil, nil, jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized,
			fmt.Errorf("project %q is not a remote project: a grant that went stale is refused at call time rather than honoured", projectID))
	}
	return settings, enr, proj, nil
}

// revealProjectToken is handleRequest's final
// step for the tool plane: turning an already-resolved, already-authorized
// project into the plaintext token CallTool/ListTools need. Never put on
// the wire — resolved server-side only.
func revealProjectToken(proj *config.Project) (string, error) {
	token, ok := proj.Token.Reveal()
	if !ok || token == "" {
		return "", jsonrpc.NewCodedError(jsonrpc.CodeInternalError,
			fmt.Errorf("project %q has no token: the sealed store may be unavailable", proj.ID))
	}
	return token, nil
}

func (s *RemoteServer) track(rc *remoteConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	byFP := s.conns[rc.fingerprint]
	if byFP == nil {
		byFP = map[*remoteConn]struct{}{}
		s.conns[rc.fingerprint] = byFP
	}
	byFP[rc] = struct{}{}
}

func (s *RemoteServer) untrack(rc *remoteConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	byFP := s.conns[rc.fingerprint]
	delete(byFP, rc)
	if len(byFP) == 0 {
		delete(s.conns, rc.fingerprint)
	}
}

// mountConn is one live mount (9P) session. Deliberately a flat set, not
// fingerprint-keyed like conns: revocation already closes the underlying
// net.Conn via the EXISTING conns table (a mount connection is tracked
// there too — handleConn tracks it on both planes), so revocation needs no
// new code at all; this table exists purely so revalidateMounts can find
// live mount sessions to freshness-check, which is a different question
// from revocation.
type mountConn struct {
	conn        net.Conn
	fingerprint string
	projectID   string
	mountID     string
}

func (s *RemoteServer) trackMountSession(fingerprint, projectID, mountID string, conn net.Conn) *mountConn {
	mc := &mountConn{conn: conn, fingerprint: fingerprint, projectID: projectID, mountID: mountID}
	s.mu.Lock()
	s.mountConns[mc] = struct{}{}
	s.mu.Unlock()
	return mc
}

func (s *RemoteServer) untrackMountSession(mc *mountConn) {
	if mc == nil {
		return
	}
	s.mu.Lock()
	delete(s.mountConns, mc)
	s.mu.Unlock()
}

// revalidateMounts closes any live mount session whose grant no longer
// resolves at all — called from RemoteSupervisor's settings tick
// (remote_reconcile.go), the same cadence that already reconciles the
// listener itself. A session merely narrowed from write to read is NOT
// closed here: reads still work, and the next write attempt is refused by
// BeginMutation's own freshness check (EROFS) — this function exists for
// the case a write check would never fire: an idle read-only session
// whose mount (or project, or grant) was removed outright.
//
// A nil *RemoteServer is a valid "no listener configured" value and is
// tolerated, matching every other method on this type: the settings tick
// reaches this from the supervisor, which may hold no live server.
func (s *RemoteServer) revalidateMounts() {
	if s == nil {
		return
	}
	s.mu.Lock()
	victims := make([]*mountConn, 0)
	for mc := range s.mountConns {
		if _, err := s.resolveMount(mc.fingerprint, mc.projectID, mc.mountID); err != nil {
			victims = append(victims, mc)
		}
	}
	s.mu.Unlock()

	for _, mc := range victims {
		_ = mc.conn.Close()
	}
}

// closeEnrolment is the revocation hook: deleting the settings record is
// only half of revocation, since a connection here is persistent and a
// client that never reconnects would otherwise keep working.
func (s *RemoteServer) closeEnrolment(clientID, fingerprint string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	byFP := s.conns[fingerprint]
	delete(s.conns, fingerprint)
	victims := make([]*remoteConn, 0, len(byFP))
	for rc := range byFP {
		victims = append(victims, rc)
	}
	s.mu.Unlock()

	for _, rc := range victims {
		_ = rc.conn.Close()
	}
	if len(victims) > 0 {
		slog.Warn("remote: closed live connections for revoked enrolment",
			"client_id", clientID, "fingerprint", fingerprint, "connections", len(victims))
	}
}
