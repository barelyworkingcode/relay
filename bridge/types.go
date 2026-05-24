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
	ReqResolvePtyEnv         = "ResolvePtyEnv"
	ReqRegisterManifest      = "RegisterManifest"
)

// Response type constants for the bridge wire protocol.
const (
	RespTools    = "Tools"
	RespResult   = "Result"
	RespError    = "Error"
	RespOK       = "OK"
	RespProjects = "Projects"
	RespProject  = "Project"
	RespPtyEnv   = "PtyEnv"
)

// Wire values for PtyEnvRequest.RegenSkills. Cross-repo callers should
// reference these constants instead of hand-typing strings.
const (
	RegenSkillsAlways       = "always"
	RegenSkillsSkipIfExists = "skipIfExists"
	RegenSkillsNever        = "never"
)

// Env vars relay injects into every spawned service. The full set is the
// service-spawn ABI: anything a service-side process needs to dial relay
// or identify itself. Lives in bridge so test binaries (cmd/testservice)
// and cross-repo consumers can import the names without depending on the
// relay main package.
const (
	EnvFrontendSocket = "RELAY_FRONTEND_SOCKET"
	EnvFrontendToken  = "RELAY_FRONTEND_TOKEN"
	EnvBridgeSocket   = "RELAY_BRIDGE_SOCKET"
	EnvServiceID      = "RELAY_SERVICE_ID"
	EnvMcpToken       = "RELAY_MCP_TOKEN"
	EnvMcpCommand     = "RELAY_MCP_COMMAND"
)

// PtyEnvRequest is the payload for ReqResolvePtyEnv requests, carried in
// BridgeRequest.Arguments as JSON. Service-token caller required.
//
// RelayLLM substitutes ${project.path} into SkillPath before sending so relay
// does not have to know about template variables.
//
// Project resolution: Project is tried first (as ID then name); if empty or
// unmatched, Directory is matched against Project.Path. PTY launches from
// eve typically only carry the directory, so Directory is the common path.
type PtyEnvRequest struct {
	Project     string `json:"project,omitempty"`   // project ID or name
	Directory   string `json:"directory,omitempty"` // fallback: match against Project.Path
	RegenSkills string `json:"regen_skills"`        // RegenSkillsAlways | RegenSkillsSkipIfExists | RegenSkillsNever
	SkillPath   string `json:"skill_path"`          // skill directory (containing SKILL.md)
}

// PtyEnvResponse is returned as BridgeResponse.Data on a successful
// ReqResolvePtyEnv. RelayToken is the project's plaintext token — handle with
// care (env-var only, never in argv or files).
type PtyEnvResponse struct {
	RelayToken string `json:"relay_token"`
	WorkingDir string `json:"working_dir"`
	SkillPath  string `json:"skill_path"`
}

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
	ResolvePtyEnv(ctx context.Context, req PtyEnvRequest, token string) (PtyEnvResponse, error)

	// RegisterManifest stores an enhanced service's registration after the
	// bridge has done schema-level validation. The router authenticates
	// the service token, detects route conflicts, and updates the
	// front-door dispatch table. Re-registration with the same ServiceID
	// replaces the prior record.
	RegisterManifest(ctx context.Context, req RegisterManifestRequest, token string) error
}

// NewScanner creates a bufio.Scanner configured with the standard bridge buffer
// size. Used by both server and client to avoid duplicating buffer setup.
func NewScanner(r io.Reader) *bufio.Scanner {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), MaxMessageSize)
	return s
}

// configDirOverride is set by SetConfigDirForTest or the --config-dir CLI flag
// (see main.go). When non-empty it bypasses os.UserConfigDir, ensuring tests
// and multi-instance runs cannot accidentally touch the user's real config.
//
// Reads are unsynchronized: callers set the override during init/test setup
// before any concurrent ConfigDir() calls.
var configDirOverride string

// SetConfigDirForTest redirects ConfigDir() to dir. Test-only seam — the
// production CLI flag (--config-dir) sets the same variable through
// SetConfigDir below. Call SetConfigDirForTest("") in t.Cleanup to restore.
func SetConfigDirForTest(dir string) { configDirOverride = dir }

// SetConfigDir is the production entrypoint for the --config-dir flag.
// Identical to SetConfigDirForTest but named for non-test callsites so
// grep'ing for the test seam stays clean.
func SetConfigDir(dir string) { configDirOverride = dir }

// ConfigDir returns the platform config directory for relay. Honors any
// override set via SetConfigDir / SetConfigDirForTest.
func ConfigDir() string {
	if configDirOverride != "" {
		return configDirOverride
	}
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
