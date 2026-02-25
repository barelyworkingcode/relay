package bridge

import (
	"bufio"
	"encoding/json"
	"log"
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

func (s *BridgeServer) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	// Allow up to 10MB lines.
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		resp := s.handleRequest(line)

		data, err := json.Marshal(resp)
		if err != nil {
			data, _ = json.Marshal(BridgeResponse{
				Type:    "Error",
				Code:    -1,
				Message: err.Error(),
			})
		}
		data = append(data, '\n')

		if _, err := conn.Write(data); err != nil {
			return
		}
	}
}

func (s *BridgeServer) handleRequest(line string) BridgeResponse {
	var req BridgeRequest
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		return BridgeResponse{
			Type:    "Error",
			Code:    -32700,
			Message: "parse error: " + err.Error(),
		}
	}

	switch req.Type {
	case "ListTools":
		tools, err := s.router.ListTools(req.Token)
		if err != nil {
			return BridgeResponse{
				Type:    "Error",
				Code:    -32603,
				Message: err.Error(),
			}
		}
		return BridgeResponse{
			Type:  "Tools",
			Tools: tools,
		}

	case "CallTool":
		result, err := s.router.CallTool(req.Name, req.Arguments, req.Token)
		if err != nil {
			return BridgeResponse{
				Type:    "Error",
				Code:    -32603,
				Message: err.Error(),
			}
		}
		return BridgeResponse{
			Type:   "Result",
			Result: result,
		}

	case "ReconcileExternalMcps":
		s.router.ReconcileExternalMcps()
		return BridgeResponse{
			Type: "OK",
		}

	case "ReloadExternalMcp":
		s.router.ReloadExternalMcp(req.Name)
		return BridgeResponse{
			Type: "OK",
		}

	default:
		log.Printf("bridge: unknown request type: %s", req.Type)
		return BridgeResponse{
			Type:    "Error",
			Code:    -32601,
			Message: "unknown request type: " + req.Type,
		}
	}
}
