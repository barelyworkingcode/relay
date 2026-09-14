package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
)

const MaxMessageSize = 10 * 1024 * 1024

const (
	ReqListTools              = "ListTools"
	ReqCallTool               = "CallTool"
	ReqReconcileExternalMcps  = "ReconcileExternalMcps"
	ReqReloadExternalMcp      = "ReloadExternalMcp"
	ReqReloadService          = "ReloadService"
	ReqListProjects           = "ListProjects"
	ReqGetProject             = "GetProject"
	ReqResolvePtyEnv          = "ResolvePtyEnv"
	ReqResolveProjectTemplate = "ResolveProjectTemplate"
	ReqRegisterManifest       = "RegisterManifest"

	// ReqDescribeProject is the one project read a project token may make:
	// its own record and resolved grant, never another project's, never a
	// token or hash.
	ReqDescribeProject = "DescribeProject"

	// ReqAdminOp is the one entry point for every brokered mutation (ADR-017
	// decision 2): BridgeRequest.Name carries the operation name and
	// Arguments its JSON payload. It is deliberately absent from
	// remoteHandlers — a VM has no code path to it at all, not a refusal on
	// one.
	ReqAdminOp = "admin_op"

	// ReqDescribeGrant and ReqNarrowGrant are the configuration plane ADR-018
	// decision 4 opens for an enrolment holding cli_admin. Neither has a field
	// naming an enrolment or a project owner: the acting identity is the TLS
	// certificate and the target is RemoteRequest.ProjectID, honoured only when
	// the resolved enrolment already holds that grant.
	ReqDescribeGrant = "DescribeGrant"
	ReqNarrowGrant   = "NarrowGrant"

	// ReqMountAttach is the mount plane's one request type: the single
	// preamble line a relay-9p/1 connection sends before the 9P stream
	// begins. It is deliberately absent from BOTH of remote_server.go's
	// dispatch tables — a client that sends it over the ordinary JSON plane
	// (no ALPN offered) must fall through handleRequest's existing final
	// case and get the existing "not available to remote clients" refusal
	// with zero new code. The two planes can only be crossed by ALPN, which
	// the TLS handshake fixes for the life of the connection; the request
	// type itself is not the discriminator.
	ReqMountAttach = "MountAttach"
)

const (
	RespTools           = "Tools"
	RespResult          = "Result"
	RespError           = "Error"
	RespOK              = "OK"
	RespProjects        = "Projects"
	RespProject         = "Project"
	RespPtyEnv          = "PtyEnv"
	RespProjectTemplate = "ProjectTemplate"
	RespProgress        = "Progress"

	RespProjectDescription = "ProjectDescription"
)

const (
	RegenSkillsAlways       = "always"
	RegenSkillsSkipIfExists = "skipIfExists"
	RegenSkillsNever        = "never"
)

// Env vars relay injects into every spawned service. Lives in bridge so
// cross-repo consumers can import the names without depending on relay's
// main package.
const (
	EnvFrontendSocket = "RELAY_FRONTEND_SOCKET"
	EnvFrontendToken  = "RELAY_FRONTEND_TOKEN"
	EnvBridgeSocket   = "RELAY_BRIDGE_SOCKET"
	EnvServiceID      = "RELAY_SERVICE_ID"
	EnvMcpCommand     = "RELAY_MCP_COMMAND"

	// EnvServiceToken is ephemeral and full-access. Never inject it into a
	// spawned child's shell — only the service process itself gets it.
	EnvServiceToken = "RELAY_SERVICE_TOKEN"

	// EnvProjectToken is scoped to one project's allowed MCPs/tools, unlike
	// the full-access EnvServiceToken.
	EnvProjectToken = "RELAY_PROJECT_TOKEN"

	// Legacy names, kept one release as a fallback for cross-repo migration.
	// Remove once relay + relayLLM have both shipped the rename.
	EnvServiceTokenLegacy = "RELAY_MCP_TOKEN"
	EnvProjectTokenLegacy = "RELAY_TOKEN"
)

// PtyEnvRequest resolves a project-scoped token + working dir. Service-token
// caller required. ProjectID is authoritative; when Directory is also set,
// relay validates it lies within the project's path so a service-token
// holder can't bind an arbitrary cwd to another project's token.
type PtyEnvRequest struct {
	ProjectID string `json:"project_id,omitempty"`
	Project   string `json:"project,omitempty"`
	Directory string `json:"directory,omitempty"`
}

// PtyEnvResponse.RelayToken is plaintext — env-var only, never argv or files.
type PtyEnvResponse struct {
	RelayToken string `json:"relay_token"`
	WorkingDir string `json:"working_dir"`
	// Host is set only for a project whose directory lives on another
	// machine (docs/ssh-hosts.md): RelayToken is then always "" (decision 6
	// — no relay-brokered tools on a host in v1) and WorkingDir names the
	// directory on the HOST, not the console.
	Host *HostSpec `json:"host,omitempty"`
}

// HostSpec is what a caller needs to launch a process on a host: the ssh
// argv prefix (internal/sshhost.SSHArgv) plus the absolute tool paths and
// shell a probe already discovered. It never carries a credential — ssh
// authenticates with the operator's own identity (key/agent), not a bearer
// relay hands out.
type HostSpec struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	SSHArgv    []string `json:"ssh_argv"`
	NodePath   string   `json:"node_path"`
	ClaudePath string   `json:"claude_path"`
	Shell      string   `json:"shell"`
	OS         string   `json:"os"`
}

// ShellTemplateRequest resolves a project-scoped shell launch template by
// (ProjectID, TemplateID), so relayLLM can spawn a private shell whose
// command lives in relay's project record rather than its own global pty map.
type ShellTemplateRequest struct {
	ProjectID  string `json:"project_id"`
	TemplateID string `json:"template_id"`
}

// ShellTemplateResponse carries NO credential — keeping ResolvePtyEnv the
// single plaintext-token egress over the bridge (ADR-007).
type ShellTemplateResponse struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Description string            `json:"description,omitempty"`
	Icon        string            `json:"icon,omitempty"`
}

// ProjectDescription is DescribeProject's answer: the caller's own project
// and its grant as the router resolves it. It carries no credential.
type ProjectDescription struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	HostID string `json:"host_id,omitempty"`
	// AllowedModels is the record's value verbatim; empty or ["*"] means
	// unrestricted, the same rule the frontend session guard applies.
	AllowedModels []string                `json:"allowed_models"`
	Mcps          []ProjectMcpDescription `json:"mcps"`
}

// ProjectMcpDescription is one MCP the grant reaches. Tools is exactly what
// ListTools lists from this MCP for the same token. Root is the --root relay
// launched the MCP with, empty when it was launched without one.
type ProjectMcpDescription struct {
	ID     string   `json:"id"`
	Access string   `json:"access"`
	Root   string   `json:"root,omitempty"`
	Tools  []string `json:"tools"`
}

type BridgeRequest struct {
	Type      string          `json:"type"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Token     string          `json:"token,omitempty"`
	ProjectID string          `json:"project_id,omitempty"`

	// Cwd is sent ONLY when no token is set, and ignored whenever a token is
	// present, so it can never widen an authenticated call's scope. Advisory,
	// not attested — anything able to lie here can already read every token
	// out of the 0600 settings.json.
	Cwd string `json:"cwd,omitempty"`
}

type BridgeResponse struct {
	Type     string          `json:"type"`
	Tools    json.RawMessage `json:"tools,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
	Progress *ProgressUpdate `json:"progress,omitempty"`
	Code     int             `json:"code,omitempty"`
	Message  string          `json:"message,omitempty"`
}

// MountAttachResult is the mount plane's one reply payload, carried in a
// BridgeResponse's Result field as marshaled JSON — the mount-attach
// preamble's second and last JSON line before the connection becomes raw
// 9P. Mount and Access echo what the grant actually resolved to (Access
// always exactly "read" or "write", never the raw stored string), and
// MsgSize is the negotiated 9P msize the client should expect (relayFS
// requests 1 MiB via p9.WithMessageSize; the server accepts up to that —
// this field lets the client confirm rather than assume).
type MountAttachResult struct {
	Mount   string `json:"mount"`
	Access  string `json:"access"`
	MsgSize uint32 `json:"msize"`
}

type ProgressUpdate struct {
	Message string `json:"message,omitempty"`
	// No omitempty: a legitimate 0.0 (the initial "queuing" update) must
	// survive the bridge hop.
	Progress float64 `json:"progress"`
	Total    float64 `json:"total,omitempty"`
}

type ProgressFunc func(ProgressUpdate)

type progressCtxKey struct{}

func WithProgress(ctx context.Context, fn ProgressFunc) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, progressCtxKey{}, fn)
}

func ProgressFromContext(ctx context.Context) ProgressFunc {
	fn, _ := ctx.Value(progressCtxKey{}).(ProgressFunc)
	return fn
}

type callerCwdCtxKey struct{}

// WithCallerCwd carries BridgeRequest.Cwd in context rather than a
// ToolRouter parameter, so the cross-repo interface stays unchanged.
func WithCallerCwd(ctx context.Context, dir string) context.Context {
	if dir == "" {
		return ctx
	}
	return context.WithValue(ctx, callerCwdCtxKey{}, dir)
}

func CallerCwdFromContext(ctx context.Context) string {
	dir, _ := ctx.Value(callerCwdCtxKey{}).(string)
	return dir
}

type callerPIDCtxKey struct{}

// WithCallerPID carries the peer pid (see PeerPID) for the same
// cross-repo-interface reason as the cwd. Audit attribution only — never
// consult this for an authorization decision.
func WithCallerPID(ctx context.Context, pid int) context.Context {
	if pid <= 0 {
		return ctx
	}
	return context.WithValue(ctx, callerPIDCtxKey{}, pid)
}

func CallerPIDFromContext(ctx context.Context) int {
	pid, _ := ctx.Value(callerPIDCtxKey{}).(int)
	return pid
}

type ToolRouter interface {
	ListTools(ctx context.Context, token string) (json.RawMessage, error)
	CallTool(ctx context.Context, name string, args json.RawMessage, token string) (json.RawMessage, error)
	ValidateAdmin(token string) error
	ReconcileExternalMcps(ctx context.Context)
	ReloadExternalMcp(ctx context.Context, id string) error
	ReloadService(id string) error
	ListProjects(token string) (json.RawMessage, error)
	GetProject(id string, token string) (json.RawMessage, error)
	// Project token only; answers for that token's own project.
	DescribeProject(ctx context.Context, token string) (ProjectDescription, error)
	ResolvePtyEnv(ctx context.Context, req PtyEnvRequest, token string) (PtyEnvResponse, error)
	// Never returns the project token.
	ResolveProjectTemplate(ctx context.Context, req ShellTemplateRequest, token string) (ShellTemplateResponse, error)
	// Re-registration with the same ServiceID replaces the prior record.
	RegisterManifest(ctx context.Context, req RegisterManifestRequest, token string) error
	// AdminOp dispatches one brokered admin operation by name. name and args
	// are opaque to the transport; the implementation resolves name against
	// its own inner table and decides whether it exists at all.
	AdminOp(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error)
}

func NewScanner(r io.Reader) *bufio.Scanner {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), MaxMessageSize)
	return s
}

// configDirOverride bypasses os.UserConfigDir when non-empty. Unsynchronized:
// callers must set it during init/test setup, before any concurrent
// ConfigDir() calls.
var configDirOverride string

func SetConfigDirForTest(dir string) { configDirOverride = dir }

// SetConfigDir is identical to SetConfigDirForTest but named for production
// callsites so grep'ing for the test seam stays clean.
func SetConfigDir(dir string) { configDirOverride = dir }

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

func SocketPath() string {
	dir := ConfigDir()
	_ = os.MkdirAll(dir, 0o700)
	return filepath.Join(dir, "relay.sock")
}
