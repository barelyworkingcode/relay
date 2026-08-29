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

	"relaygo/bridge"
	"relaygo/jsonrpc"
)

// defaultRemoteListen binds loopback: reaching relay from a VM must be a
// deliberate act, never the default outcome of leaving a field unset.
const defaultRemoteListen = "127.0.0.1:9910"

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

// Enabled is a *bool so absent, false and true stay distinguishable: unlike
// AuditConfig, a network listener defaults OFF, so a block that names a
// listen address but omits `enabled` opens nothing.
type RemoteConfig struct {
	Enabled *bool  `json:"enabled,omitempty"`
	Listen  string `json:"listen,omitempty"`
}

type resolvedRemoteConfig struct {
	Enabled bool
	Listen  string
}

func (c *RemoteConfig) resolve() resolvedRemoteConfig {
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

// A nil *RemoteServer is a valid "no listener configured" value and every
// method tolerates it, so callers need no branch for the disabled case.
type RemoteServer struct {
	router RemoteToolRouter
	store  SettingsStore
	cfg    resolvedRemoteConfig
	audit  *AuditRecorder

	listener net.Listener
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	// conns is keyed by certificate fingerprint, not client id: by the time
	// the revocation hook fires, the record naming a client id is gone.
	mu    sync.Mutex
	conns map[string]map[*remoteConn]struct{}
}

func (s *RemoteServer) currentSettings() *Settings {
	return freshSettings(s.store)
}

// remoteAuditingLive requires both halves; neither implies the other. The
// recorder can die under a listener that started cleanly, independently of
// settings flipping audit.enabled off in another process.
func remoteAuditingLive(s *Settings, audit *AuditRecorder) bool {
	return audit.Enabled() && s.Audit.resolve().Enabled
}

type remoteConn struct {
	conn        net.Conn
	clientID    string
	fingerprint string
}

func NewRemoteServer(ctx context.Context, store SettingsStore, router RemoteToolRouter, audit *AuditRecorder) (*RemoteServer, error) {
	// freshSettings, not Get(): the operator who just edited settings.json is
	// the same operator watching the listener come up.
	settings := freshSettings(store)
	cfg := settings.Remote.resolve()
	if !cfg.Enabled {
		slog.Debug("remote listener not enabled; no socket opened")
		return nil, nil
	}

	if !remoteAuditingLive(settings, audit) {
		return nil, fmt.Errorf("remote listener refuses to start while the tool-call audit log is disabled: " +
			"a remote grant is justified by the calls it records, so serving remote traffic unrecorded is not a degraded mode — " +
			"set audit.enabled to true, or remove the remote block from settings.json")
	}

	ca, err := LoadOrCreateCA()
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
	}

	ln, err := tls.Listen("tcp", cfg.Listen, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("remote listener: listen on %s: %w", cfg.Listen, err)
	}

	sctx, cancel := context.WithCancel(ctx)
	s := &RemoteServer{
		router:   router,
		store:    store,
		cfg:      cfg,
		audit:    audit,
		listener: ln,
		ctx:      sctx,
		cancel:   cancel,
		conns:    map[string]map[*remoteConn]struct{}{},
	}

	// Installed with an owner because a rebind (RemoteSupervisor) binds the
	// new listener before closing the old one, so the old one's teardown
	// must be able to tell the hook is no longer its own.
	SetEnrolmentRevocationHookFor(s, s.closeEnrolment)

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
	ClearEnrolmentRevocationHookFor(s)
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

	fingerprint := FingerprintCert(state.PeerCertificates[0])
	// currentSettings, not Get(): a cached view here would refuse a
	// brand-new enrolment until the tray's next settings poll.
	settings := s.currentSettings()
	enrolment := settings.FindEnrolmentByFingerprint(fingerprint)
	if enrolment == nil {
		slog.Warn("remote: closing connection, certificate is not enrolled",
			"fingerprint", fingerprint, "remote_addr", conn.RemoteAddr().String())
		return
	}

	// Re-checked here, not just at startup: auditing could have been
	// turned off since this listener bound.
	if !remoteAuditingLive(settings, s.audit) {
		slog.Error("remote: closing connection, auditing is not active",
			"client_id", enrolment.ClientID, "remote_addr", conn.RemoteAddr().String())
		return
	}

	ctx := bridge.WithRemoteCaller(connCtx, bridge.RemoteCaller{
		ClientID:    enrolment.ClientID,
		Fingerprint: fingerprint,
		RemoteAddr:  conn.RemoteAddr().String(),
	})

	rc := &remoteConn{conn: conn, clientID: enrolment.ClientID, fingerprint: fingerprint}
	s.track(rc)
	defer s.untrack(rc)

	slog.Info("remote client connected", "client_id", enrolment.ClientID, "remote_addr", conn.RemoteAddr().String())

	bridge.NewFrameConn(conn, "remote", remoteIdleTimeout).
		Serve(ctx, func(ctx context.Context, line string) bridge.BridgeResponse {
			return s.handleRequest(ctx, fingerprint, line)
		})
}

func (s *RemoteServer) handleRequest(ctx context.Context, fingerprint, line string) bridge.BridgeResponse {
	req, err := bridge.DecodeRemoteRequest([]byte(line))
	if err != nil {
		// Strict decoding: a client sending `cwd` or `token` lands here
		// loudly rather than the field being silently ignored.
		return bridge.ErrorResponse(jsonrpc.CodeInvalidParams, "remote request: "+err.Error())
	}

	h, ok := remoteHandlers[req.Type]
	if !ok {
		slog.Warn("remote: request type is not available on the remote listener", "type", req.Type)
		return bridge.ErrorResponse(jsonrpc.CodeMethodNotFound,
			"request type is not available to remote clients: "+req.Type)
	}

	token, err := s.resolveGrant(fingerprint, req.ProjectID)
	if err != nil {
		return bridge.ErrorResponse(bridge.ErrorCode(err), err.Error())
	}

	return h(ctx, req, s.router, token)
}

func (s *RemoteServer) resolveGrant(fingerprint, projectID string) (string, error) {
	settings := s.currentSettings()

	if !remoteAuditingLive(settings, s.audit) {
		return "", jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized,
			fmt.Errorf("remote calls are refused while the tool-call audit log is disabled"))
	}

	// Re-resolved from current on-disk settings on every request, never
	// cached: a revocation must take effect on the next call, not the
	// next reconnect.
	enrolment := settings.FindEnrolmentByFingerprint(fingerprint)
	if enrolment == nil {
		return "", jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized,
			fmt.Errorf("this certificate is no longer enrolled"))
	}

	if projectID == "" {
		switch len(enrolment.ProjectIDs) {
		case 0:
			return "", jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized,
				fmt.Errorf("enrolment %q holds no grants", enrolment.ClientID))
		case 1:
			projectID = enrolment.ProjectIDs[0]
		default:
			return "", jsonrpc.NewCodedError(jsonrpc.CodeInvalidParams,
				fmt.Errorf("project_id is required: enrolment %q holds %d grants", enrolment.ClientID, len(enrolment.ProjectIDs)))
		}
	}

	if !enrolment.GrantsProject(projectID) {
		return "", jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized,
			fmt.Errorf("enrolment %q does not grant project %q", enrolment.ClientID, projectID))
	}

	proj, _ := settings.findProjectByID(projectID)
	if proj == nil {
		return "", jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized,
			fmt.Errorf("project %q no longer exists", projectID))
	}
	// IsRemote(), never a Kind comparison: the zero value is local and must
	// stay unreadable as remote.
	if !proj.IsRemote() {
		return "", jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized,
			fmt.Errorf("project %q is not a remote project: a grant that went stale is refused at call time rather than honoured", projectID))
	}
	if proj.Token == "" {
		return "", jsonrpc.NewCodedError(jsonrpc.CodeInternalError,
			fmt.Errorf("project %q has no token", projectID))
	}
	// Resolved server-side, never put on the wire: not a credential the
	// client holds.
	return proj.Token, nil
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
