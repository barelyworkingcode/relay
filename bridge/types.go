package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
)

// MaxMessageSize is the maximum line/message size for bridge and MCP wire protocols.
const MaxMessageSize = 10 * 1024 * 1024

// Request type constants for the bridge wire protocol.
const (
	ReqListTools             = "ListTools"
	ReqCallTool              = "CallTool"
	ReqReconcileExternalMcps = "ReconcileExternalMcps"
	ReqReloadExternalMcp     = "ReloadExternalMcp"
	ReqReloadService         = "ReloadService"
	ReqListProjects          = "ListProjects"
	ReqGetProject            = "GetProject"
)

// Response type constants for the bridge wire protocol.
const (
	RespTools    = "Tools"
	RespResult   = "Result"
	RespError    = "Error"
	RespOK       = "OK"
	RespProjects = "Projects"
	RespProject  = "Project"
)

// BridgeRequest is the wire format for requests sent over the Unix socket.
type BridgeRequest struct {
	Type      string          `json:"type"`                  // request type
	Name      string          `json:"name,omitempty"`        // tool name for CallTool, MCP ID for ReloadExternalMcp
	Arguments json.RawMessage `json:"arguments,omitempty"`   // tool arguments for CallTool
	Token     string          `json:"token,omitempty"`       // auth token
	ProjectID string          `json:"project_id,omitempty"`  // for GetProject
}

// BridgeResponse is the wire format for responses sent over the Unix socket.
type BridgeResponse struct {
	Type    string          `json:"type"`              // response type
	Tools   json.RawMessage `json:"tools,omitempty"`   // JSON-encoded []mcp.Tool
	Result  json.RawMessage `json:"result,omitempty"`  // JSON-encoded mcp.CallToolResult
	Data    json.RawMessage `json:"data,omitempty"`    // generic JSON payload (projects, etc.)
	Code    int             `json:"code,omitempty"`    // error code
	Message string          `json:"message,omitempty"` // error message
}

// ToolRouter handles bridge requests. Implemented by the main app.
type ToolRouter interface {
	ListTools(ctx context.Context, token string) (json.RawMessage, error)
	CallTool(ctx context.Context, name string, args json.RawMessage, token string) (json.RawMessage, error)
	ValidateAdmin(token string) error
	ReconcileExternalMcps(ctx context.Context)
	ReloadExternalMcp(ctx context.Context, id string)
	ReloadService(id string)
	ListProjects(token string) (json.RawMessage, error)
	GetProject(id string, token string) (json.RawMessage, error)
}

// NewScanner creates a bufio.Scanner configured with the standard bridge buffer
// size. Used by both server and client to avoid duplicating buffer setup.
func NewScanner(r io.Reader) *bufio.Scanner {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), MaxMessageSize)
	return s
}

// ConfigDir returns the platform config directory for relay.
func ConfigDir() string {
	configDir, err := os.UserConfigDir()
	if err != nil {
		configDir, _ = os.UserHomeDir()
	}
	return filepath.Join(configDir, "relay")
}

// SocketPath returns the path to the bridge Unix socket.
// Creates the parent directory if it does not exist.
func SocketPath() string {
	dir := ConfigDir()
	_ = os.MkdirAll(dir, 0o700)
	return filepath.Join(dir, "relay.sock")
}
