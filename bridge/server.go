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

// BridgeServer listens on a Unix socket and routes requests via a ToolRouter.
type BridgeServer struct {
	router   ToolRouter
	listener net.Listener
	sockPath string
	wg       sync.WaitGroup
	ctx      context.Context
	cancel   context.CancelFunc
}

// NewBridgeServer creates a BridgeServer bound to the default socket path.
// The provided context is used as the parent for all per-connection contexts,
// enabling graceful cancellation of in-flight requests during shutdown.
func NewBridgeServer(ctx context.Context, router ToolRouter) (*BridgeServer, error) {
	sockPath := SocketPath()

	// Remove stale socket file.
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

// Serve accepts connections and handles them. Blocks until the listener is closed.
func (s *BridgeServer) Serve() error {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			// Listener was closed.
			return err
		}
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

// StopAccepting stops the server from accepting new connections and cancels
// all in-flight request contexts. Existing handlers continue running until
// they return or notice their context is cancelled. This is the first phase
// of a two-phase shutdown: call StopAccepting early to prevent new work,
// then call Close after backends are torn down to drain remaining handlers.
func (s *BridgeServer) StopAccepting() {
	s.cancel()
	_ = s.listener.Close()
}

// Close completes server shutdown: waits for all connection handlers to finish
// and removes the socket file. If StopAccepting was already called, this only
// drains and cleans up. Safe to call without a prior StopAccepting — it will
// stop accepting as part of the close.
func (s *BridgeServer) Close() {
	s.StopAccepting()
	s.wg.Wait()
	_ = os.Remove(s.sockPath)
}

// bridgeError creates an error BridgeResponse with the given code and message.
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

	// Per-connection context — cancelled when the connection or server closes.
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()

	// Resolve the peer pid once per connection rather than per request: it can't
	// change for the life of the socket, and the getsockopt is pure overhead on
	// every subsequent frame. Audit attribution only.
	ctx = WithCallerPID(ctx, PeerPID(conn))

	// Close the connection when the server (or this handler) is cancelled so a
	// handler blocked in scanner.Scan() unblocks promptly at shutdown. Without
	// this, StopAccepting() only cancels the context and closes the *listener* —
	// an in-flight Scan() keeps waiting for the peer to disconnect, so Close()'s
	// wg.Wait() can deadlock against a managed service that holds a persistent
	// bridge connection and isn't killed until later in the teardown. The
	// goroutine always exits because `defer cancel()` fires on return.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	// No deadlines: this is a 0600 Unix socket, so the peer is same-user and a
	// stalled client is a bug rather than an attack. RemoteServer, whose peer
	// is across a network, passes a non-zero idle timeout instead.
	NewFrameConn(conn, "bridge", 0).Serve(ctx, s.handleRequest)
}

// bridgeHandler defines a handler for a bridge request type.
type bridgeHandler struct {
	requireAdmin bool
	handle       func(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse
}

// bridgeHandlers maps request types to their handlers.
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

	// Carry the caller-asserted cwd so the router can fall back to directory
	// auth when no token was supplied. Ignored by every handler that doesn't
	// authenticate a project.
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

// classifyErrorCode extracts a JSON-RPC error code from the error chain.
// Router methods wrap auth/permission errors with jsonrpc.CodedError so the
// bridge can classify them without fragile string matching.
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
