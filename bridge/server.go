package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"sync"
)

// BridgeServer listens on a Unix socket and routes requests via a ToolRouter.
type BridgeServer struct {
	router   ToolRouter
	listener net.Listener
	sockPath string
	wg       sync.WaitGroup
}

// NewBridgeServer creates a BridgeServer bound to the default socket path.
func NewBridgeServer(router ToolRouter) (*BridgeServer, error) {
	sockPath := SocketPath()

	// Remove stale socket file.
	_ = os.Remove(sockPath)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, err
	}

	return &BridgeServer{
		router:   router,
		listener: listener,
		sockPath: sockPath,
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

// Close shuts down the server: closes the listener, waits for connections, removes the socket.
func (s *BridgeServer) Close() {
	_ = s.listener.Close()
	s.wg.Wait()
	_ = os.Remove(s.sockPath)
}

// bridgeError creates an error BridgeResponse with the given code and message.
func bridgeError(code int, msg string) BridgeResponse {
	return BridgeResponse{Type: "Error", Code: code, Message: msg}
}

func (s *BridgeServer) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	// Per-connection context — cancelled when the connection closes.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), MaxMessageSize)

	for scanner.Scan() {
		line := scanner.Text()
		resp := s.handleRequest(ctx, line)

		data, err := json.Marshal(resp)
		if err != nil {
			data, _ = json.Marshal(bridgeError(-1, err.Error()))
		}
		data = append(data, '\n')

		if _, err := conn.Write(data); err != nil {
			return
		}
	}
}

// bridgeHandler defines a handler for a bridge request type.
type bridgeHandler struct {
	requireAdmin bool
	handle       func(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse
}

// bridgeHandlers maps request types to their handlers.
var bridgeHandlers = map[string]bridgeHandler{
	"ListTools": {handle: handleListTools},
	"CallTool":  {handle: handleCallTool},
	"ReconcileExternalMcps": {requireAdmin: true, handle: handleReconcile},
	"ReloadExternalMcp":     {requireAdmin: true, handle: handleReloadMcp},
	"ReloadService":         {requireAdmin: true, handle: handleReloadService},
}

func (s *BridgeServer) handleRequest(ctx context.Context, line string) BridgeResponse {
	var req BridgeRequest
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		return bridgeError(-32700, "parse error: "+err.Error())
	}

	h, ok := bridgeHandlers[req.Type]
	if !ok {
		slog.Warn("bridge: unknown request type", "type", req.Type)
		return bridgeError(-32601, "unknown request type: "+req.Type)
	}

	if h.requireAdmin {
		if err := s.router.ValidateAdmin(req.Token); err != nil {
			return bridgeError(-32603, "admin auth: "+err.Error())
		}
	}

	return h.handle(ctx, &req, s.router)
}

func handleListTools(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	tools, err := router.ListTools(ctx, req.Token)
	if err != nil {
		return bridgeError(-32603, err.Error())
	}
	return BridgeResponse{Type: "Tools", Tools: tools}
}

func handleCallTool(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	result, err := router.CallTool(ctx, req.Name, req.Arguments, req.Token)
	if err != nil {
		return bridgeError(-32603, err.Error())
	}
	return BridgeResponse{Type: "Result", Result: result}
}

func handleReconcile(_ context.Context, _ *BridgeRequest, router ToolRouter) BridgeResponse {
	router.ReconcileExternalMcps()
	return BridgeResponse{Type: "OK"}
}

func handleReloadMcp(_ context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	router.ReloadExternalMcp(req.Name)
	return BridgeResponse{Type: "OK"}
}

func handleReloadService(_ context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	router.ReloadService(req.Name)
	return BridgeResponse{Type: "OK"}
}
