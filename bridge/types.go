package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// BridgeRequest is the wire format for requests sent over the Unix socket.
type BridgeRequest struct {
	Type      string          `json:"type"`                // "ListTools", "CallTool", "ReconcileExternalMcps"
	Name      string          `json:"name,omitempty"`      // tool name for CallTool
	Arguments json.RawMessage `json:"arguments,omitempty"` // tool arguments for CallTool
	Token     string          `json:"token,omitempty"`     // auth token
}

// BridgeResponse is the wire format for responses sent over the Unix socket.
type BridgeResponse struct {
	Type    string          `json:"type"`              // "Tools", "Result", "Error"
	Tools   json.RawMessage `json:"tools,omitempty"`   // JSON-encoded []mcp.Tool
	Result  json.RawMessage `json:"result,omitempty"`  // JSON-encoded mcp.CallToolResult
	Code    int             `json:"code,omitempty"`    // error code
	Message string          `json:"message,omitempty"` // error message
}

// ToolRouter handles bridge requests. Implemented by the main app.
type ToolRouter interface {
	ListTools(token string) (json.RawMessage, error)
	CallTool(name string, args json.RawMessage, token string) (json.RawMessage, error)
	ReconcileExternalMcps()
}

// SocketPath returns the path to the bridge Unix socket.
// Creates the parent directory if it does not exist.
func SocketPath() string {
	configDir, err := os.UserConfigDir()
	if err != nil {
		configDir, _ = os.UserHomeDir()
	}
	dir := filepath.Join(configDir, "relay")
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, "relay.sock")
}
