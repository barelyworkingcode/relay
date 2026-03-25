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

// Close shuts down the server: cancels in-flight requests, closes the listener,
// waits for connections, removes the socket.
func (s *BridgeServer) Close() {
	s.cancel()
	_ = s.listener.Close()
	s.wg.Wait()
	_ = os.Remove(s.sockPath)
}

// bridgeError creates an error BridgeResponse with the given code and message.
func bridgeError(code int, msg string) BridgeResponse {
	return BridgeResponse{Type: RespError, Code: code, Message: msg}
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

	scanner := NewScanner(conn)

	for scanner.Scan() {
		line := scanner.Text()
		resp := s.handleRequest(ctx, line)

		data, err := json.Marshal(resp)
		if err != nil {
			data, _ = json.Marshal(bridgeError(jsonrpc.CodeInternalError, err.Error()))
		}
		data = append(data, '\n')

		if _, err := conn.Write(data); err != nil {
			return
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Debug("bridge connection read error", "error", err)
	}
}

// bridgeHandler defines a handler for a bridge request type.
type bridgeHandler struct {
	requireAdmin bool
	handle       func(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse
}

// bridgeHandlers maps request types to their handlers.
var bridgeHandlers = map[string]bridgeHandler{
	ReqListTools:             {handle: handleListTools},
	ReqCallTool:              {handle: handleCallTool},
	ReqReconcileExternalMcps: {requireAdmin: true, handle: handleReconcile},
	ReqReloadExternalMcp:     {requireAdmin: true, handle: handleReloadMcp},
	ReqReloadService:         {requireAdmin: true, handle: handleReloadService},
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

	return h.handle(ctx, &req, s.router)
}

func handleListTools(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	tools, err := router.ListTools(ctx, req.Token)
	if err != nil {
		return bridgeError(jsonrpc.CodeInternalError, err.Error())
	}
	return BridgeResponse{Type: RespTools, Tools: tools}
}

func handleCallTool(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	result, err := router.CallTool(ctx, req.Name, req.Arguments, req.Token)
	if err != nil {
		return bridgeError(jsonrpc.CodeInternalError, err.Error())
	}
	return BridgeResponse{Type: RespResult, Result: result}
}

func handleReconcile(ctx context.Context, _ *BridgeRequest, router ToolRouter) BridgeResponse {
	router.ReconcileExternalMcps(ctx)
	return BridgeResponse{Type: RespOK}
}

func handleReloadMcp(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	router.ReloadExternalMcp(ctx, req.Name)
	return BridgeResponse{Type: RespOK}
}

func handleReloadService(_ context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	router.ReloadService(req.Name)
	return BridgeResponse{Type: RespOK}
}
