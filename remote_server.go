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

// RemoteServer is the listener a client on another machine reaches relay
// through (ADR-010 decisions 1, 2, 4 and 9). It sits BESIDE BridgeServer and
// never wraps or replaces it: the Unix socket keeps its ten request types and
// its local trust model untouched, and this listener has two request types and
// a completely different one.
//
// The three properties that make it a different listener rather than a flag on
// the old one:
//
//   - Its dispatch table holds exactly ListTools and CallTool. The other eight
//     bridge request types are not refused by a check — there is no code path
//     from here to ResolvePtyEnv or RegisterManifest at all. A new admin op is
//     unreachable from a VM until someone deliberately adds it to a two-entry
//     list that is visibly a security boundary (decision 1).
//   - Identity comes from a client certificate resolved to an enrolment BEFORE
//     any request is read, so an unenrolled caller cannot probe (decision 2).
//   - The wire has no field to authenticate with, and unknown fields are an
//     error rather than a shrug (decision 4).

// defaultRemoteListen binds loopback deliberately. Misconfiguration must not
// expose the control plane to a LAN, and same-machine development must need no
// configuration at all — a remote project is reachable from 127.0.0.1 exactly
// as it is from another machine, because remoteness is a property of the
// grant's shape and not of the caller's location. Reaching relay from a VM is
// then a deliberate act (widening this address, or running a tunnel) rather
// than something that happens by leaving a field unset (decision 9).
const defaultRemoteListen = "127.0.0.1:9910"

var (
	// remoteHandshakeTimeout bounds the time between a TCP accept and a
	// completed TLS handshake. This is the actual slowloris bound: a peer that
	// connects and never speaks holds a goroutine and an fd until it expires.
	// BridgeServer needs no equivalent because its peer is same-user behind a
	// 0600 socket.
	remoteHandshakeTimeout = 15 * time.Second

	// remoteIdleTimeout bounds SILENCE on an established connection, never work
	// in progress: FrameConn pushes the deadline out on every frame read and
	// every frame written, so a tool call that legitimately runs for an hour
	// while streaming progress is untouched, and one that runs for an hour in
	// silence still gets its response written (the write refreshes the deadline
	// before it happens). What this does cut off is a connection nobody is
	// using — a compromised agent parking sockets, or a peer that vanished
	// without a FIN.
	//
	// Vars rather than consts so tests can shorten them.
	remoteIdleTimeout = 5 * time.Minute
)

// ---------------------------------------------------------------------------
// Configuration (decision 9)
// ---------------------------------------------------------------------------

// RemoteConfig is the "remote" block in settings.json:
//
//	"remote": {
//	  "enabled": true,
//	  "listen": "127.0.0.1:9910"
//	}
//
// Absent block means NO LISTENER AT ALL — not a listener bound to nothing, not
// a listener that refuses every call. Nothing is opened, so an install that
// never enrolled a client has no network surface to reason about.
//
// Enabled is a *bool for the same reason AuditConfig's is: absent, false and
// true have to stay distinguishable. They differ in what absent MEANS, and the
// difference is deliberate. Auditing defaults on because a missing block on an
// old install should keep recording; a network listener defaults OFF, so a
// block that names a listen address but forgets `enabled` opens nothing and
// says so in the log. Starting a listener nobody asked for is the failure mode
// this whole ADR exists to avoid, and "the operator clearly meant it" is not a
// good enough reason to guess.
type RemoteConfig struct {
	Enabled *bool  `json:"enabled,omitempty"`
	Listen  string `json:"listen,omitempty"`
}

// resolvedRemoteConfig is RemoteConfig with defaults applied.
type resolvedRemoteConfig struct {
	Enabled bool
	Listen  string
}

// resolve applies decision 9's defaults. Nil-safe: an absent block resolves to
// disabled, which is the whole of "disabled unless configured".
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

// ---------------------------------------------------------------------------
// The narrow router surface
// ---------------------------------------------------------------------------

// RemoteToolRouter is the ENTIRE router surface a remote connection can reach.
// It is a separate, two-method interface rather than bridge.ToolRouter so that
// the narrowing is a compile-time property: this file cannot call
// ResolvePtyEnv even by accident, because it does not hold anything that has
// the method. *appRouter satisfies it.
type RemoteToolRouter interface {
	ListTools(ctx context.Context, token string) (json.RawMessage, error)
	CallTool(ctx context.Context, name string, args json.RawMessage, token string) (json.RawMessage, error)
}

var _ RemoteToolRouter = (*appRouter)(nil)

// remoteHandler handles one remote request. token is the internal project
// token RemoteServer resolved from the enrolment's grant — see resolveGrant
// for why it is a server-side implementation detail rather than a credential
// the client holds.
type remoteHandler func(ctx context.Context, req *bridge.RemoteRequest, router RemoteToolRouter, token string) bridge.BridgeResponse

// remoteHandlers is the dispatch table decision 1 is about. It has two
// entries. Adding a third is a deliberate act with its own review, not a
// discovery that something already worked: nothing here falls through to
// bridgeHandlers, and no request type outside this map reaches any router
// method at all.
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
	result, err := router.CallTool(ctx, req.Name, req.Arguments, token)
	if err != nil {
		return bridge.ErrorResponse(bridge.ErrorCode(err), err.Error())
	}
	return bridge.BridgeResponse{Type: bridge.RespResult, Result: result}
}

// ---------------------------------------------------------------------------
// The listener
// ---------------------------------------------------------------------------

// RemoteServer is the mTLS listener. A nil *RemoteServer is a valid "no
// listener configured" value and every method tolerates it, so the tray wires
// it in unconditionally and the disabled case needs no branch at the call
// sites.
type RemoteServer struct {
	router RemoteToolRouter
	store  SettingsStore
	// cfg is the resolved configuration this listener was BUILT from, kept so
	// RemoteSupervisor can answer "does the live listener still match what
	// settings ask for?" without re-deriving it. Written once at construction
	// and never mutated: a listener does not change its address, it is replaced
	// by one that has the new address.
	cfg resolvedRemoteConfig
	// audit is held only to re-assert that recording is live. RemoteServer
	// never writes an event itself — that happens at appRouter.CallTool, the
	// chokepoint every transport funnels through.
	audit    *AuditRecorder
	listener net.Listener
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	// conns is the connection table revocation acts on, keyed by certificate
	// fingerprint — the enrolment's identity, and the value the revocation hook
	// hands us. Keyed by fingerprint rather than by client id because the
	// fingerprint is what the connection itself proved; a client id is a label
	// on a record that has, by the time the hook fires, already been deleted.
	mu    sync.Mutex
	conns map[string]map[*remoteConn]struct{}
}

// currentSettings is the ONLY way this listener may read settings. Every
// decision here — is this certificate enrolled, does the enrolment still hold
// this grant, is auditing still on — is an authorization decision, and relay is
// not one process: `relay enrol create`, `relay enrol revoke` and a hand-edited
// settings.json all happen outside the tray. See freshSettings for the cost.
func (s *RemoteServer) currentSettings() *Settings {
	return freshSettings(s.store)
}

// remoteAuditingLive reports whether a remote call may run at all: the recorder
// must be recording AND settings must still ask for it.
//
// Both halves are load-bearing and neither implies the other. The recorder can
// die under a listener that started cleanly (a closed writer, a rotation
// failure), and settings can be edited to `audit.enabled: false` in another
// process while a recorder from before the edit is still happily writing. The
// first is the fail-closed case ADR-010 decision 5 is about; the second is the
// operator saying remote access is no longer justified, and honouring it only
// at the next process start would make the setting look decorative.
func remoteAuditingLive(s *Settings, audit *AuditRecorder) bool {
	return audit.Enabled() && s.Audit.resolve().Enabled
}

// remoteConn is one live, authenticated connection.
type remoteConn struct {
	conn        net.Conn
	clientID    string
	fingerprint string
}

// NewRemoteServer binds the remote listener, or returns (nil, nil) when no
// listener is configured.
//
// Three refusals live here rather than deeper in, because a listener that is
// open but cannot serve safely is worse than one that never opened: it answers
// a TCP connect, it accepts a handshake, and it teaches an operator that the
// port is fine.
//
//   - Not configured → no listener at all (decision 9).
//   - Auditing disabled → REFUSED, loudly. The case for letting a VM reach host
//     mail rests entirely on detection: ADR-009's threat model is exfiltration,
//     and ADR-010 decision 5 pays availability for evidence. Remove the
//     evidence and the justification for the whole feature goes with it. Local
//     tooling is unaffected — this refusal is scoped to this listener and
//     nothing else consults it.
//   - No CA, or no server certificate → refused, since there is no meaningful
//     degraded mode for mutual TLS.
func NewRemoteServer(ctx context.Context, store SettingsStore, router RemoteToolRouter, audit *AuditRecorder) (*RemoteServer, error) {
	// freshSettings, not Get(): the operator who just edited settings.json in a
	// CLI process is the same operator watching for the listener to come up, and
	// deciding whether to open a network socket off a snapshot that predates
	// their edit is the same class of bug as issue #21, one level out.
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
	// Issued per process start rather than persisted: the client verifies it
	// against the CA certificate in its bundle, so rotation costs the client
	// nothing and the host keeps one fewer private key on disk.
	cert, err := ca.IssueServerCert(remoteCertHosts(cfg.Listen)...)
	if err != nil {
		return nil, fmt.Errorf("remote listener: %w", err)
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		// The certificate IS the identity, so a handshake that did not prove
		// one is not a connection worth having. RequireAndVerifyClientCert
		// against relay's own pool means nothing signed by any other authority
		// — public CA included — can complete a handshake here.
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  ca.Pool(),
		// Both ends are ours and the CA is ours, so there is no legacy peer to
		// accommodate and no reason to offer anything older.
		MinVersion: tls.VersionTLS13,
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

	// Revocation must sever live connections, not merely take effect on the
	// next one: this protocol holds persistent connections in a scanner loop,
	// so a compromised agent that never reconnects would otherwise keep working
	// indefinitely (decision 8). The enrolment code deletes the record and
	// fires this hook; closing the socket is ours because the connection table
	// is ours.
	//
	// Installed WITH AN OWNER because this listener can be replaced without the
	// process restarting (RemoteSupervisor, remote_reconcile.go): a rebind binds
	// the new address before closing the old listener, so the old one's teardown
	// has to be able to tell that the hook is no longer its own and leave the
	// live server's hook alone.
	SetEnrolmentRevocationHookFor(s, s.closeEnrolment)

	slog.Info("remote listener started", "addr", ln.Addr().String())
	return s, nil
}

// remoteCertHosts returns the SANs the server certificate must carry for a
// client to verify a connection to the configured listen address.
//
// Loopback is always included: it is the default bind and the address
// same-machine development uses. A wildcard bind cannot be enumerated — relay
// has no way to know which of the host's addresses a client will name — so an
// operator who binds one deliberately names the reachable address in `listen`
// and gets a SAN for it, while `0.0.0.0` alone gets loopback only and a client
// dialling a LAN address sees a verification failure rather than an
// unauthenticated tunnel.
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

// Addr returns the address the listener actually bound, which is what a test
// binding :0 needs and what an operator reading the log wants. "" when there
// is no listener.
func (s *RemoteServer) Addr() string {
	if s == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// Serve accepts connections until the listener is closed. Mirrors
// BridgeServer.Serve.
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

// StopAccepting closes the listener and cancels in-flight request contexts,
// the first phase of the same two-phase shutdown BridgeServer uses.
func (s *RemoteServer) StopAccepting() {
	if s == nil {
		return
	}
	s.cancel()
	_ = s.listener.Close()
}

// Close completes shutdown: stops accepting, uninstalls the revocation hook,
// and drains the connection handlers. There is no socket file to unlink.
func (s *RemoteServer) Close() {
	if s == nil {
		return
	}
	s.StopAccepting()
	// Drop the hook before draining: a revocation arriving mid-shutdown has
	// nothing left to close, and leaving a closure over a dead server installed
	// in a process-global would keep it reachable after teardown. Compare-and-
	// clear, never an unconditional clear: on a rebind the REPLACEMENT listener
	// already owns the hook by the time this runs, and uninstalling it here
	// would leave revocation unable to sever a live connection — a security
	// regression no test that checks only the deleted record would catch.
	ClearEnrolmentRevocationHookFor(s)
	s.wg.Wait()
}

// ---------------------------------------------------------------------------
// Connection handling
// ---------------------------------------------------------------------------

// handleConn resolves the certificate to an enrolment and only then reads
// anything. The ordering is decision 2's requirement and not an optimisation:
// a connection whose certificate does not resolve is closed WITHOUT PROCESSING
// A REQUEST, so an unenrolled caller — even one holding a certificate relay
// itself signed for an enrolment since revoked — cannot probe for valid grants,
// valid tool names, or the shape of an error.
func (s *RemoteServer) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("remote handler panic (recovered)", "panic", r)
		}
	}()

	// connCtx is the connection's lifetime; the request context derived from it
	// further down carries the caller identity. Two names rather than one
	// reassigned variable, because the watchdog goroutine below reads it
	// concurrently and rebinding a captured variable under it is a race.
	connCtx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	// Close the connection when this handler or the server is cancelled, so a
	// handler parked in Scan() unblocks at shutdown. Same reasoning as
	// BridgeServer's; here it is also what makes revocation's Close() take
	// effect promptly.
	go func() {
		<-connCtx.Done()
		_ = conn.Close()
	}()

	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		// Unreachable: the listener is tls.Listen. Refuse rather than fall
		// through to a plaintext path that must never exist.
		slog.Error("remote: non-TLS connection on the remote listener; closing")
		return
	}

	// Handshake explicitly, under its own deadline, so the identity below is
	// resolved before any read and a peer that never speaks cannot camp.
	_ = conn.SetDeadline(time.Now().Add(remoteHandshakeTimeout))
	if err := tlsConn.HandshakeContext(connCtx); err != nil {
		slog.Warn("remote: TLS handshake failed", "remote_addr", conn.RemoteAddr().String(), "error", err)
		return
	}

	state := tlsConn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		// Unreachable under RequireAndVerifyClientCert; kept because the
		// consequence of it ever becoming reachable is an unidentified caller.
		slog.Warn("remote: handshake completed with no peer certificate; closing", "remote_addr", conn.RemoteAddr().String())
		return
	}

	fingerprint := FingerprintCert(state.PeerCertificates[0])
	// currentSettings, not Get(): `relay enrol create` runs in a CLI PROCESS
	// and writes settings.json, so a cached view here refuses a brand-new
	// enrolment as "not enrolled" until the tray's next settings poll — a
	// refusal indistinguishable from a genuine misconfiguration, and one whose
	// diagnostic sends the operator to `relay enrol list`, which shows the
	// record present (issue #21). Reading the file is what makes creation as
	// immediate as revocation already was.
	settings := s.currentSettings()
	enrolment := settings.FindEnrolmentByFingerprint(fingerprint)
	if enrolment == nil {
		// A CA-signed certificate that no enrolment claims. This is the state a
		// revoked client is in, and it gets no request read and no error frame
		// — the disconnect tells it nothing it did not already know.
		slog.Warn("remote: closing connection, certificate is not enrolled",
			"fingerprint", fingerprint, "remote_addr", conn.RemoteAddr().String())
		return
	}

	// Belt and braces against a window the start-up refusal cannot cover: the
	// recorder could have been closed under us, or auditing could have been
	// turned off in settings since this listener bound. RemoteSupervisor stops
	// the listener when that happens, but "on the next reconcile" is not soon
	// enough for a connection arriving in between — there must be no path on
	// which a remote call runs unrecorded.
	if !remoteAuditingLive(settings, s.audit) {
		slog.Error("remote: closing connection, auditing is not active",
			"client_id", enrolment.ClientID, "remote_addr", conn.RemoteAddr().String())
		return
	}

	// This is the value that puts every call on this connection onto the
	// fail-closed audit path, and every field in it is derived from the
	// connection rather than asserted by the caller (decision 6). Called once
	// per connection because none of it can change while the socket is open.
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

// handleRequest decodes one frame and dispatches it. Every refusal here
// happens before the router is touched.
func (s *RemoteServer) handleRequest(ctx context.Context, fingerprint, line string) bridge.BridgeResponse {
	req, err := bridge.DecodeRemoteRequest([]byte(line))
	if err != nil {
		// Strict decoding means a client sending `cwd` (or `token`) lands
		// here — loudly — rather than having the field ignored and believing
		// it authenticated by something it did not (decision 4).
		return bridge.ErrorResponse(jsonrpc.CodeInvalidParams, "remote request: "+err.Error())
	}

	h, ok := remoteHandlers[req.Type]
	if !ok {
		// Not a refusal by policy check: the type simply has no handler on this
		// listener. ResolvePtyEnv and friends are unreachable here in the same
		// way "Frobnicate" is.
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

// resolveGrant turns "this certificate, that project id" into the internal
// project token appRouter.CallTool expects, refusing anything that does not
// resolve cleanly. This is the chain the whole design rests on, so each link
// is spelled out:
//
//  0. Re-check that auditing is live, because the answer can change under an
//     open connection and a remote call that cannot be recorded is refused
//     rather than served (decision 5). Per REQUEST, not per connection, for the
//     same reason step 1 is.
//  1. Re-resolve the ENROLMENT from CURRENT ON-DISK settings on every request,
//     not once per connection and not from a cached snapshot. A revocation that
//     raced the connection table, a grant edited while the socket stayed open,
//     or a record written by a CLI process in another PID must take effect on
//     the next call rather than at the next reconnect or the next settings
//     poll. This is the step that already made revocation immediate; reading
//     through to the file is what extends the same immediacy to creation.
//  2. Select the project id. An enrolment holding exactly one grant may leave
//     it off — sending it would be ceremony. An enrolment holding several must
//     say which, because guessing would make the choice relay's rather than
//     the caller's.
//  3. Honour the id only if the enrolment HOLDS that grant. The id is not a
//     secret and is not treated as one; it selects among grants, it never
//     confers one.
//  4. Re-check IsRemote() immediately before dispatch (decision 3, point 3).
//     Enrolment-time and conversion-time validation already cover the paths
//     relay anticipated; this covers the ones it did not — a hand-edited
//     settings.json, a mutator called directly, a future code path nobody has
//     written yet. IsRemote(), never a Kind comparison: the zero value is
//     local and must stay unreadable as remote.
//  5. Resolve the token SERVER-SIDE. It is never on the wire, never in a
//     response, and never leaves the host; it is how this listener calls the
//     existing router, not a credential the client holds. That is exactly why
//     decision 2 could delete the token from the remote path without changing
//     anything below the transport.
func (s *RemoteServer) resolveGrant(fingerprint, projectID string) (string, error) {
	settings := s.currentSettings()

	if !remoteAuditingLive(settings, s.audit) {
		return "", jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized,
			fmt.Errorf("remote calls are refused while the tool-call audit log is disabled"))
	}

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
	if !proj.IsRemote() {
		return "", jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized,
			fmt.Errorf("project %q is not a remote project: a grant that went stale is refused at call time rather than honoured", projectID))
	}
	if proj.Token == "" {
		return "", jsonrpc.NewCodedError(jsonrpc.CodeInternalError,
			fmt.Errorf("project %q has no token", projectID))
	}
	return proj.Token, nil
}

// ---------------------------------------------------------------------------
// Connection table + revocation (decision 8)
// ---------------------------------------------------------------------------

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

// closeEnrolment is the revocation hook: it severs every live connection
// holding the revoked certificate.
//
// Deleting the record is only half of revocation. Connections here are
// persistent and sit in a scanner loop, so a client that never reconnects
// would keep calling tools with a credential the operator believes they
// destroyed — for as long as the process lives. Closing the socket is what
// makes "revoked" mean revoked now.
//
// Matching is on the fingerprint, which is what each connection proved. The
// client id comes along for the log line, because the record it named is
// already gone by the time this runs.
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
