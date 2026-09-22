package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/barelyworkingcode/relay/internal/peertoken"
)

const MaxMessageSize = 10 * 1024 * 1024

const (
	ReqListTools             = "ListTools"
	ReqCallTool              = "CallTool"
	ReqReconcileExternalMcps = "ReconcileExternalMcps"
	ReqReloadExternalMcp     = "ReloadExternalMcp"
	ReqReloadService         = "ReloadService"
	ReqRegisterManifest      = "RegisterManifest"

	// ReqRegisterModelHost registers a service's router socket as the model
	// endpoint's upstream (docs/model-endpoint.md). Tokenless: authenticated
	// by the caller's launch identity, which must hold model_host.
	ReqRegisterModelHost = "RegisterModelHost"

	// ReqHello binds a launch secret to the calling process's audit token
	// (docs/launch-identity.md). Name is the launch name, Token the secret.
	ReqHello = "Hello"

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

	// ReqSessionExited is relay-sessions' advisory, tokenless report that one
	// of its sessions is gone (plan-broker-and-sessions.md §2 C5). Requires
	// the caller's launch identity to hold the sessions capability, which
	// config restricts to the built-in relay-sessions service.
	ReqSessionExited = "SessionExited"

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

	// ReqSandboxAttach launches a terminal session for the project holding the
	// caller's directory and turns the connection into that session's byte
	// stream (docs/sandbox-command.md). It is in bridgeHandlers only:
	// remoteHandlers and the enrolment table have no entry for it.
	ReqSandboxAttach = "SandboxAttach"
)

const (
	RespTools    = "Tools"
	RespResult   = "Result"
	RespError    = "Error"
	RespOK       = "OK"
	RespProgress = "Progress"

	RespProjectDescription = "ProjectDescription"

	// RespAttached acknowledges a ReqSandboxAttach: Data is a
	// SandboxAttachResult, and every later frame on the connection is a
	// StreamFrame in each direction.
	RespAttached = "Attached"
)

const (
	RegenSkillsAlways       = "always"
	RegenSkillsSkipIfExists = "skipIfExists"
	RegenSkillsNever        = "never"
)

// Env vars relay injects into every spawned service. Lives in bridge so
// cross-repo consumers can import the names without depending on relay's
// main package. None of them is a secret (docs/launch-identity.md).
const (
	EnvFrontendSocket = "RELAY_FRONTEND_SOCKET"
	EnvBridgeSocket   = "RELAY_BRIDGE_SOCKET"
	EnvServiceID      = "RELAY_SERVICE_ID"
	EnvMcpCommand     = "RELAY_MCP_COMMAND"

	// EnvLaunchFD names the inherited descriptor holding the launch secret.
	// Its value is always LaunchFD; the secret itself is never in the
	// environment.
	EnvLaunchFD = "RELAY_LAUNCH_FD"

	// EnvProjectToken is scoped to one project's allowed MCPs/tools.
	EnvProjectToken = "RELAY_PROJECT_TOKEN"

	EnvProjectTokenLegacy = "RELAY_TOKEN"
)

// LaunchFD is the descriptor number the launch secret arrives on:
// exec.Cmd.ExtraFiles[0].
const LaunchFD = 3

// RemovedCredentialEnv lists names that once carried a relay credential in a
// service's environment. Relay scrubs every one of them from any environment
// it passes to a service, so none can reach a service by inheritance.
var RemovedCredentialEnv = []string{"RELAY_SERVICE_TOKEN", "RELAY_MCP_TOKEN", "RELAY_FRONTEND_TOKEN"}

// HelloResult is a successful Hello's Data. It recognises the caller and
// never carries a credential.
type HelloResult struct {
	Kind      string `json:"kind"`
	ServiceID string `json:"service_id"`
	RelayPID  int    `json:"relay_pid"`
	// ProjectID is set only for a project_session identity
	// (plan-broker-and-sessions.md §2 C2); absent for every other kind.
	ProjectID string `json:"project_id,omitempty"`
}

// RegisterModelHostRequest is the Arguments payload for a
// ReqRegisterModelHost call. RouterSocket is the service's own internal Unix
// socket serving relayLLM's router mux (docs/model-endpoint.md); unlike
// RegisterManifestRequest there is no internal token, because the model
// endpoint authenticates the socket itself by the kernel peer audit token of
// whoever answers on RouterSocket, not by a bearer the service declares.
type RegisterModelHostRequest struct {
	ServiceID    string `json:"service_id"`
	RouterSocket string `json:"router_socket"`
}

// Validate covers only the request in isolation. RouterSocket must be an
// absolute path: it is passed straight to net.Dial("unix", ...) on every
// model-endpoint call (model_endpoint.go's dialVerifiedUnix), and a
// relative path would resolve against relay's own working directory rather
// than anything the registering service actually meant.
func (r *RegisterModelHostRequest) Validate() error {
	if r.ServiceID == "" {
		return fmt.Errorf("register_model_host: service_id is empty")
	}
	if r.RouterSocket == "" {
		return fmt.Errorf("register_model_host: router_socket is empty")
	}
	if !filepath.IsAbs(r.RouterSocket) {
		return fmt.Errorf("register_model_host: router_socket must be an absolute path")
	}
	return nil
}

// SessionExitedRequest is the Arguments payload for a ReqSessionExited
// report: C5's exact wire shape (session_id, root_pid, exit_status, reason).
// It carries no project id — the caller (relay itself, cmd/relay's
// router_sessions.go) resolves that from its own launch/ledger bookkeeping,
// never from anything the host asserts.
type SessionExitedRequest struct {
	SessionID  string `json:"session_id"`
	RootPID    int    `json:"root_pid"`
	ExitStatus int    `json:"exit_status"`
	Reason     string `json:"reason"`
}

func (r *SessionExitedRequest) Validate() error {
	if r.SessionID == "" {
		return fmt.Errorf("session_exited: session_id is empty")
	}
	return nil
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

	// Kind is Hello-only and optional: the IdentityKind the caller expects to
	// bind (plan-broker-and-sessions.md §2 C2). Absent means "don't care" —
	// every Hello sender that predates this field keeps working unchanged.
	// Present and wrong refuses the Hello before the secret is spent, the
	// same as a wrong secret.
	Kind string `json:"kind,omitempty"`

	// Cwd is accepted on the wire and ignored entirely. Directory auth is
	// retired (plan-broker-and-sessions.md §2 C3): a working directory is
	// asserted by the caller, not attested by the kernel, and relay now
	// identifies a tokenless caller by its audit token and process ancestry
	// instead. The field remains only so a request from a client built
	// before the retirement still decodes; nothing server-side authenticates
	// on it, whether or not a caller sends one.
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

type callerPeerCtxKey struct{}

// WithCallerPeer carries the connection's peer audit token. Unlike the pid
// above this IS an authorization input: a launch identity is bound to it, and
// only a (pid, pidversion) pair — never a pid alone — is matched.
func WithCallerPeer(ctx context.Context, tok peertoken.Token) context.Context {
	if !tok.Valid() {
		return ctx
	}
	return context.WithValue(ctx, callerPeerCtxKey{}, tok)
}

// CallerPeerFromContext returns the zero Token, which matches no identity,
// when the connection's peer could not be read.
func CallerPeerFromContext(ctx context.Context) peertoken.Token {
	tok, _ := ctx.Value(callerPeerCtxKey{}).(peertoken.Token)
	return tok
}

type ToolRouter interface {
	ListTools(ctx context.Context, token string) (json.RawMessage, error)
	CallTool(ctx context.Context, name string, args json.RawMessage, token string) (json.RawMessage, error)
	ValidateAdmin(token string) error
	ReconcileExternalMcps(ctx context.Context)
	ReloadExternalMcp(ctx context.Context, id string) error
	ReloadService(id string) error
	// Hello binds a launch secret to the caller's peer audit token. kind is
	// BridgeRequest.Kind verbatim — empty when the caller doesn't assert one.
	Hello(ctx context.Context, name, secret, kind string) (HelloResult, error)
	// Project token only; answers for that token's own project.
	DescribeProject(ctx context.Context, token string) (ProjectDescription, error)
	// Re-registration with the same ServiceID replaces the prior record.
	RegisterManifest(ctx context.Context, req RegisterManifestRequest, token string) error
	// RegisterModelHost registers a service's router socket as the model
	// endpoint's upstream. Requires the model_host capability, under the
	// caller's own service id only.
	RegisterModelHost(ctx context.Context, req RegisterModelHostRequest, token string) error
	// AdminOp dispatches one brokered admin operation by name. name and args
	// are opaque to the transport; the implementation resolves name against
	// its own inner table and decides whether it exists at all.
	AdminOp(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error)
	// SessionExited handles relay-sessions' advisory SessionExited report.
	// Requires a launch identity holding the sessions capability; token is
	// always empty on this wire (tokenless, like RegisterModelHost).
	SessionExited(ctx context.Context, req SessionExitedRequest, token string) error
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

// ModelSocketPath is the model endpoint's Unix socket (docs/model-endpoint.md),
// beside relay.sock in the same directory and chmod'd 0600 the same way.
func ModelSocketPath() string {
	dir := ConfigDir()
	_ = os.MkdirAll(dir, 0o700)
	return filepath.Join(dir, "model.sock")
}
