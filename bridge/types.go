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
	// RespProgress is an intermediate, non-terminal frame emitted zero or more
	// times during an in-flight CallTool before the terminal Result/Error.
	// Clients that don't understand it skip it and keep reading.
	RespProgress = "Progress"
)

// Skill-regen mode values used internally by relay's EmitSkills (RegenMode).
// Skill generation is owned by relay; these are no longer a PtyEnvRequest wire
// field. Reference the constants instead of hand-typing strings.
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
	EnvMcpCommand     = "RELAY_MCP_COMMAND"

	// EnvServiceToken carries the ephemeral, full-access service token relay
	// injects into every spawned service so it can authenticate its own bridge
	// calls (ResolvePtyEnv, RegisterManifest). It is NOT a project token and
	// must never be injected into a user shell — doing so would hand a child
	// god-mode bridge access. Project-scoped work uses EnvProjectToken.
	EnvServiceToken = "RELAY_SERVICE_TOKEN"

	// EnvProjectToken carries a project-scoped token into a spawned child (an
	// LLM CLI, the `relay mcp` subprocess, or a project-scoped terminal). Its
	// privileges are limited to one project's allowed MCPs/tools — distinct
	// from the full-access EnvServiceToken.
	EnvProjectToken = "RELAY_PROJECT_TOKEN"

	// Legacy env-var names, retained one release for cross-repo migration:
	// relay sets EnvServiceTokenLegacy alongside EnvServiceToken, and readers
	// (relay's `relay mcp`/`mcp call`, relayLLM's bridge client) fall back to
	// the *Legacy names. Remove once relay + relayLLM have both shipped the
	// rename. EnvServiceTokenLegacy was previously named EnvMcpToken.
	EnvServiceTokenLegacy = "RELAY_MCP_TOKEN"
	EnvProjectTokenLegacy = "RELAY_TOKEN"
)

// PtyEnvRequest is the payload for ReqResolvePtyEnv requests, carried in
// BridgeRequest.Arguments as JSON. Service-token caller required. The call
// resolves a project-scoped token + working dir; skill generation is owned by
// relay and is not driven from here.
//
// Project resolution precedence: ProjectID (authoritative) → Project (as ID
// then name) → Directory matched against Project.Path (legacy). When ProjectID
// is set and Directory is non-empty, relay validates the directory is within
// the project's path and rejects a mismatch — this prevents a service-token
// holder from binding an arbitrary cwd to another project's token. The legacy
// Project/Directory match remains so older callers keep working during migration.
type PtyEnvRequest struct {
	ProjectID string `json:"project_id,omitempty"` // authoritative: resolve by project ID; Directory (if set) is validated within Project.Path
	Project   string `json:"project,omitempty"`    // legacy: project ID or name
	Directory string `json:"directory,omitempty"`  // legacy fallback: match against Project.Path
}

// PtyEnvResponse is returned as BridgeResponse.Data on a successful
// ReqResolvePtyEnv. RelayToken is the project's plaintext token — handle with
// care (env-var only, never in argv or files).
type PtyEnvResponse struct {
	RelayToken string `json:"relay_token"`
	WorkingDir string `json:"working_dir"`
}

// BridgeRequest is the wire format for requests sent over the Unix socket.
type BridgeRequest struct {
	Type      string          `json:"type"`                 // request type
	Name      string          `json:"name,omitempty"`       // tool name for CallTool, MCP ID for ReloadExternalMcp
	Arguments json.RawMessage `json:"arguments,omitempty"`  // tool arguments for CallTool
	Token     string          `json:"token,omitempty"`      // auth token
	ProjectID string          `json:"project_id,omitempty"` // for GetProject
}

// BridgeResponse is the wire format for responses sent over the Unix socket.
type BridgeResponse struct {
	Type     string          `json:"type"`               // response type
	Tools    json.RawMessage `json:"tools,omitempty"`    // JSON-encoded []mcp.Tool
	Result   json.RawMessage `json:"result,omitempty"`   // JSON-encoded mcp.CallToolResult
	Data     json.RawMessage `json:"data,omitempty"`     // generic JSON payload (projects, etc.)
	Progress *ProgressUpdate `json:"progress,omitempty"` // set only on RespProgress frames
	Code     int             `json:"code,omitempty"`     // error code
	Message  string          `json:"message,omitempty"`  // error message
}

// ProgressUpdate is a tool-progress notification forwarded up the call chain.
// Mirrors MCP's notifications/progress payload minus the hop-specific
// progressToken (each transport layer attaches its own token).
type ProgressUpdate struct {
	Message string `json:"message,omitempty"`
	// Progress has no omitempty: a legitimate 0.0 (e.g. the initial "queuing"
	// update) must survive the bridge hop. Total stays optional (omit when 0/
	// unknown), matching the MCP spec where total is optional.
	Progress float64 `json:"progress"`
	Total    float64 `json:"total,omitempty"`
}

// ProgressFunc receives tool-progress updates during an in-flight CallTool.
type ProgressFunc func(ProgressUpdate)

type progressCtxKey struct{}

// WithProgress returns a context carrying a progress sink for the duration of
// a CallTool. The external-MCP client invokes it when a downstream server
// emits notifications/progress; the bridge server forwards each update to its
// caller as a RespProgress frame. A nil fn returns ctx unchanged.
func WithProgress(ctx context.Context, fn ProgressFunc) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, progressCtxKey{}, fn)
}

// ProgressFromContext returns the progress sink set by WithProgress, or nil.
func ProgressFromContext(ctx context.Context) ProgressFunc {
	fn, _ := ctx.Value(progressCtxKey{}).(ProgressFunc)
	return fn
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
