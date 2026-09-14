package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"

	"github.com/barelyworkingcode/relay/internal/jsonrpc"
	"github.com/barelyworkingcode/relay/internal/peertoken"
	"github.com/barelyworkingcode/relay/internal/presence"
)

type BridgeServer struct {
	router   ToolRouter
	listener net.Listener
	sockPath string
	wg       sync.WaitGroup
	ctx      context.Context
	cancel   context.CancelFunc

	// closed gates wg.Add against a concurrent wg.Wait. Adding to a WaitGroup
	// whose counter has reached zero while Wait is running is a race, and
	// closing the listener does not prevent it: Accept can return a connection
	// just before StopAccepting runs.
	mu     sync.Mutex
	closed bool

	// callerSession resolves a connection's presence.CallerSession (§6.6).
	// nil in every struct literal that does not set it explicitly — a
	// handful of this package's own tests build a BridgeServer by hand
	// rather than through NewBridgeServer — so handleConn falls back to
	// PeerCallerSession itself rather than requiring every call site to
	// know about this field. Production code never overrides it.
	callerSession func(net.Conn) presence.CallerSession
}

// SetCallerSessionResolverForTest overrides how THIS server resolves a
// connection's presence.CallerSession. It exists because a real peer's
// kernel audit session is whatever the process running the test happens to
// have — there is no way to force AU_SESSION_FLAG_HAS_GRAPHIC_ACCESS off
// from inside the test binary itself — and a test proving §6.6's refusal
// end to end needs that fact pinned, not ambient. This does not touch
// package presence or weaken Gate.Request in any way: it only supplies the
// one input Gate.Request already reads off the context before deciding
// whether to prompt at all. Production never calls this; NewBridgeServer's
// default (PeerCallerSession) is the only resolver a shipped relay uses.
func (s *BridgeServer) SetCallerSessionResolverForTest(fn func(net.Conn) presence.CallerSession) {
	s.callerSession = fn
}

func NewBridgeServer(ctx context.Context, router ToolRouter) (*BridgeServer, error) {
	sockPath := SocketPath()

	_ = os.Remove(sockPath)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, err
	}
	// Enforce owner-only access regardless of umask.
	if err := os.Chmod(sockPath, 0o600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)

	return &BridgeServer{
		router:        router,
		listener:      listener,
		sockPath:      sockPath,
		ctx:           ctx,
		cancel:        cancel,
		callerSession: PeerCallerSession,
	}, nil
}

func (s *BridgeServer) Serve() error {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return err
		}
		if !s.trackConn() {
			_ = conn.Close()
			return net.ErrClosed
		}
		go s.handleConn(conn)
	}
}

func (s *BridgeServer) trackConn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.wg.Add(1)
	return true
}

// StopAccepting is the first phase of a two-phase shutdown: it stops taking
// new connections and cancels in-flight request contexts, but existing
// handlers keep running until they return or notice cancellation. Call Close
// after backends are torn down to drain the rest.
func (s *BridgeServer) StopAccepting() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()

	s.cancel()
	_ = s.listener.Close()
}

// Close waits for all connection handlers to finish and removes the socket
// file. Safe to call without a prior StopAccepting.
func (s *BridgeServer) Close() {
	s.StopAccepting()
	s.wg.Wait()
	_ = os.Remove(s.sockPath)
}

func bridgeError(code int, msg string) BridgeResponse {
	return ErrorResponse(code, msg)
}

func (s *BridgeServer) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer func() { _ = conn.Close() }()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("bridge handler panic (recovered)", "panic", r)
		}
	}()

	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()

	// Resolved once per connection, not per request: it can't change for the
	// life of the socket, and the getsockopt is pure overhead on every
	// subsequent frame. Audit attribution only.
	ctx = WithCallerPID(ctx, PeerPID(conn))
	if tok, err := peertoken.FromConn(conn); err == nil {
		ctx = WithCallerPeer(ctx, tok)
	}

	// Resolved once per connection too, beside PeerPID, and for the same
	// reason: it can't change for the socket's lifetime. Unlike PeerPID this
	// is a presence-gate INPUT, not audit-only — a caller whose session
	// cannot show a prompt must refuse before the gate ever asks
	// LocalAuthentication, which does not itself refuse a remote caller and
	// would otherwise raise the login-password prompt on the physical
	// console for whoever is sitting there (§6.6).
	resolveSession := s.callerSession
	if resolveSession == nil {
		resolveSession = PeerCallerSession
	}
	ctx = presence.WithCallerSession(ctx, resolveSession(conn))

	// Close the connection when this context is cancelled so a handler
	// blocked in scanner.Scan() unblocks promptly at shutdown. Without this,
	// StopAccepting() only cancels the context and closes the *listener* — an
	// in-flight Scan() keeps waiting for the peer to disconnect, and Close()'s
	// wg.Wait() can deadlock against a managed service holding a persistent
	// bridge connection that isn't killed until later in teardown.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	// No deadlines: this is a 0600 Unix socket, so the peer is same-user and a
	// stalled client is a bug rather than an attack. RemoteServer, whose peer
	// is across a network, passes a non-zero idle timeout instead.
	NewFrameConn(conn, "bridge", 0).Serve(ctx, s.handleRequest)
}

type bridgeHandler struct {
	requireAdmin bool
	handle       func(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse
}

var bridgeHandlers = map[string]bridgeHandler{
	ReqListTools:              {handle: handleListTools},
	ReqCallTool:               {handle: handleCallTool},
	ReqReconcileExternalMcps:  {requireAdmin: true, handle: handleReconcile},
	ReqReloadExternalMcp:      {requireAdmin: true, handle: handleReloadMcp},
	ReqReloadService:          {requireAdmin: true, handle: handleReloadService},
	ReqListProjects:           {handle: handleListProjects},
	ReqGetProject:             {handle: handleGetProject},
	ReqResolvePtyEnv:          {handle: handleResolvePtyEnv},
	ReqResolveProjectTemplate: {handle: handleResolveProjectTemplate},
	ReqDescribeProject:        {handle: handleDescribeProject},
	ReqRegisterManifest:       {handle: handleRegisterManifest},
	ReqHello:                  {handle: handleHello},

	// This is deliberate: unlike every requireAdmin entry above, admin_op
	// carries no bearer. ADR-015 and ADR-016 both refuse to spend the 0600
	// socket's ambient trust twice, and admin_secret is a sealed value the
	// caller can no longer read to present here anyway. The gate that
	// matters lives inside the operation core this dispatches to, not on
	// this transport.
	ReqAdminOp: {handle: handleAdminOp},
}

func (s *BridgeServer) handleRequest(ctx context.Context, line string) BridgeResponse {
	var req BridgeRequest
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		return bridgeError(jsonrpc.CodeParseError, "parse error: "+err.Error())
	}

	h, ok := bridgeHandlers[req.Type]
	if !ok {
		slog.Warn("bridge: unknown request type", "type", req.Type)
		return bridgeError(jsonrpc.CodeMethodNotFound, "unknown request type: "+req.Type)
	}

	if h.requireAdmin {
		if err := s.router.ValidateAdmin(req.Token); err != nil {
			return bridgeError(jsonrpc.CodeUnauthorized, "admin auth: "+err.Error())
		}
	}

	// Directory auth is a fallback for a tokenless caller; every handler that
	// doesn't authenticate a project ignores it. Hello's token field is the
	// launch secret, never absent in a valid Hello, and never a project token.
	if req.Token == "" && req.Type != ReqHello {
		ctx = WithCallerCwd(ctx, req.Cwd)
	}

	return h.handle(ctx, &req, s.router)
}

func handleListTools(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	tools, err := router.ListTools(ctx, req.Token)
	if err != nil {
		return bridgeError(classifyErrorCode(err), err.Error())
	}
	return BridgeResponse{Type: RespTools, Tools: tools}
}

func handleCallTool(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	result, err := router.CallTool(ctx, req.Name, req.Arguments, req.Token)
	if err != nil {
		return bridgeError(classifyErrorCode(err), err.Error())
	}
	return BridgeResponse{Type: RespResult, Result: result}
}

func classifyErrorCode(err error) int {
	return ErrorCode(err)
}

func handleReconcile(ctx context.Context, _ *BridgeRequest, router ToolRouter) BridgeResponse {
	router.ReconcileExternalMcps(ctx)
	return BridgeResponse{Type: RespOK}
}

func handleReloadMcp(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	if err := router.ReloadExternalMcp(ctx, req.Name); err != nil {
		return bridgeError(classifyErrorCode(err), err.Error())
	}
	return BridgeResponse{Type: RespOK}
}

func handleReloadService(_ context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	if err := router.ReloadService(req.Name); err != nil {
		return bridgeError(classifyErrorCode(err), err.Error())
	}
	return BridgeResponse{Type: RespOK}
}

// handleHello answers every refusal with one fixed message: the router's
// error names the reason for relay's log, and a caller must learn neither
// that reason nor, ever, the secret it sent.
func handleHello(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	result, err := router.Hello(ctx, req.Name, req.Token)
	if err != nil {
		slog.Warn("bridge: hello refused", "name", req.Name, "peer_pid", CallerPIDFromContext(ctx), "reason", err)
		return bridgeError(jsonrpc.CodeUnauthorized, "hello refused")
	}
	data, err := json.Marshal(result)
	if err != nil {
		return bridgeError(jsonrpc.CodeInternalError, "hello: encode result")
	}
	return BridgeResponse{Type: RespOK, Data: data}
}

func handleListProjects(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	data, err := router.ListProjects(ctx, req.Token)
	if err != nil {
		return bridgeError(classifyErrorCode(err), err.Error())
	}
	return BridgeResponse{Type: RespProjects, Data: data}
}

func handleGetProject(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	data, err := router.GetProject(ctx, req.ProjectID, req.Token)
	if err != nil {
		return bridgeError(classifyErrorCode(err), err.Error())
	}
	return BridgeResponse{Type: RespProject, Data: data}
}

func handleResolvePtyEnv(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	if len(req.Arguments) == 0 {
		return bridgeError(jsonrpc.CodeInvalidParams, "resolve_pty_env: missing arguments")
	}
	var p PtyEnvRequest
	if err := json.Unmarshal(req.Arguments, &p); err != nil {
		return bridgeError(jsonrpc.CodeParseError, "resolve_pty_env: "+err.Error())
	}
	resp, err := router.ResolvePtyEnv(ctx, p, req.Token)
	if err != nil {
		return bridgeError(classifyErrorCode(err), err.Error())
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return bridgeError(jsonrpc.CodeInternalError, err.Error())
	}
	return BridgeResponse{Type: RespPtyEnv, Data: data}
}

func handleDescribeProject(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	desc, err := router.DescribeProject(ctx, req.Token)
	if err != nil {
		return bridgeError(classifyErrorCode(err), err.Error())
	}
	data, err := json.Marshal(desc)
	if err != nil {
		return bridgeError(jsonrpc.CodeInternalError, err.Error())
	}
	return BridgeResponse{Type: RespProjectDescription, Data: data}
}

func handleResolveProjectTemplate(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	if len(req.Arguments) == 0 {
		return bridgeError(jsonrpc.CodeInvalidParams, "resolve_project_template: missing arguments")
	}
	var p ShellTemplateRequest
	if err := json.Unmarshal(req.Arguments, &p); err != nil {
		return bridgeError(jsonrpc.CodeParseError, "resolve_project_template: "+err.Error())
	}
	resp, err := router.ResolveProjectTemplate(ctx, p, req.Token)
	if err != nil {
		return bridgeError(classifyErrorCode(err), err.Error())
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return bridgeError(jsonrpc.CodeInternalError, err.Error())
	}
	return BridgeResponse{Type: RespProjectTemplate, Data: data}
}

func handleAdminOp(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	result, err := router.AdminOp(ctx, req.Name, req.Arguments)
	if err != nil {
		return bridgeError(classifyErrorCode(err), err.Error())
	}
	return BridgeResponse{Type: RespResult, Result: result}
}

func handleRegisterManifest(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	if len(req.Arguments) == 0 {
		return bridgeError(jsonrpc.CodeInvalidParams, "register_manifest: missing arguments")
	}
	var r RegisterManifestRequest
	if err := json.Unmarshal(req.Arguments, &r); err != nil {
		return bridgeError(jsonrpc.CodeParseError, "register_manifest: "+err.Error())
	}
	if err := r.Validate(); err != nil {
		return bridgeError(jsonrpc.CodeInvalidParams, err.Error())
	}
	if err := router.RegisterManifest(ctx, r, req.Token); err != nil {
		return bridgeError(classifyErrorCode(err), err.Error())
	}
	return BridgeResponse{Type: RespOK}
}
