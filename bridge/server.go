package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"

	"relaygo/jsonrpc"
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
		listener.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)

	return &BridgeServer{
		router:   router,
		listener: listener,
		sockPath: sockPath,
		ctx:      ctx,
		cancel:   cancel,
	}, nil
}

func (s *BridgeServer) Serve() error {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return err
		}
		if !s.trackConn() {
			conn.Close()
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
	defer conn.Close()
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
	ReqRegisterManifest:       {handle: handleRegisterManifest},

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
	// doesn't authenticate a project ignores it.
	if req.Token == "" {
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

func handleListProjects(_ context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	data, err := router.ListProjects(req.Token)
	if err != nil {
		return bridgeError(classifyErrorCode(err), err.Error())
	}
	return BridgeResponse{Type: RespProjects, Data: data}
}

func handleGetProject(_ context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	data, err := router.GetProject(req.ProjectID, req.Token)
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
