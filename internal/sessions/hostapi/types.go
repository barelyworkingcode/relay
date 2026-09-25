// Package hostapi is relay-sessions' internal API server: contract C5's
// "relay ↔ relay-sessions internal API". It serves two Unix sockets:
//
//   - the internal socket, dialed only by relay itself, carrying POST
//     /launch and POST /terminate;
//   - the hook socket, dialed by `relay-sessions hook` processes (running
//     as a Claude Code PreToolUse hook), carrying POST /permission.
//
// handleLaunch/handleTerminate are a thin dispatcher, not a spawner: a "pty"
// launch routes to internal/sessions/terminal.Manager and a "claude"/"pi"/
// "chat" launch routes to internal/sessions/session.Manager, each of which
// owns its own shim-spawn (or direct-spawn) mechanics end to end, including
// the identity Hello wait. This package's own sessionTable holds pty
// sessions only: the shim is that launch's process root, and the table
// answers /permission's C3 membership walk for it and recalls the root pid
// at exit time for the SessionExited report. A provider-hosted
// (claude/pi/chat) session's root is read live from session.Manager on each
// /permission instead (rootsAdapter), so its PreToolUse hook authenticates
// by process ancestry too.
//
// The eve-facing manifest HTTP/WS surface (internal/sessions/api's
// Hub/handlers) is mounted on the internal socket's own mux (see
// ListenInternal), guarded by the same checkInternalPeer mutual check
// /launch and /terminate use — see the docs/session-host.md note on that
// surface's trust model. /permission's decision flow is docs/session-host.md's
// "Tool permissions".
//
// A "claude"/"pi" launch dispatched through this package now carries the
// sandbox profile and launch identity relay minted for it through to the
// target: provider.ClaudeConfig/PiConfig carry Sandbox/Identity fields (like
// ChatConfig always did), and session.Manager.buildProvider threads
// spec.SandboxProfile/spec.Identity into both. Both providers spawn through
// the shim (internal/sessions/provider/shimspawn.go, pipe mode — no --pty)
// whenever either is set, and refuse to spawn unconfined when a sandbox
// profile is requested but no shim binary is configured
// (provider.ErrShimRequired). root_pid stays 0 for every provider-hosted
// launch: a provider's root is looked up live per /permission, not pinned
// at /launch.
package hostapi

import "encoding/json"

// LaunchRequest is C5's POST /launch body (v1). Only the fields this
// skeleton acts on are typed strictly; everything else round-trips as
// opaque JSON so a later unit can add real handling without this package
// needing to change its wire contract.
type LaunchRequest struct {
	V              int               `json:"v"`
	SessionID      string            `json:"session_id"`
	Kind           string            `json:"kind"`
	Resume         bool              `json:"resume"`
	Project        json.RawMessage   `json:"project"`
	Directory      string            `json:"directory"`
	Name           string            `json:"name"`
	TemplateID     string            `json:"template_id"`
	Argv           []string          `json:"argv,omitempty"`
	ExtraArgs      []string          `json:"extra_args,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	PTY            *PTYSpec          `json:"pty"`
	IdleTimeoutSec int               `json:"idle_timeout_sec,omitempty"`
	// omitempty and decodeHostSpec's explicit "null"-string guard
	// (dispatch.go) are independent, both load-bearing: omitempty keeps a
	// nil *HostSpec (every non-hosted launch, e.g. relay's own
	// AuthorizeLaunch marshaling one) from putting a literal "host":null on
	// the wire in the first place, but it only ever omits a zero-length
	// json.RawMessage — it cannot stop some other caller's differently
	// shaped encoding from producing the 4 literal bytes "null" some other
	// way. decodeHostSpec checks for that string explicitly rather than
	// trusting the sender, so the guard holds even if this tag is ever
	// removed.
	Host           json.RawMessage `json:"host,omitempty"`
	Sandbox        *SandboxSpec    `json:"sandbox"`
	Identity       *IdentitySpec   `json:"identity"`
	ModelKey       string          `json:"model_key,omitempty"`
	SessionRequest json.RawMessage `json:"session_request,omitempty"`
}

// PTYSpec is C5's non-null "pty" object.
type PTYSpec struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// IdentitySpec is C5's non-null "identity" object: the 64-hex secret the
// shim must present over the bridge socket per C6 step 3.
type IdentitySpec struct {
	Secret string `json:"secret"`
}

// SandboxSpec carries only what this skeleton needs: the absolute profile
// path the host would have written (C7's profiles/<session_id>.sb). Real
// profile generation is R-S8's job; this unit just plumbs the path through
// to the shim's --sandbox-profile flag when a caller supplies one.
type SandboxSpec struct {
	ProfilePath string `json:"profile_path,omitempty"`
}

// LaunchResponse is C5's 201 body.
type LaunchResponse struct {
	SessionID string          `json:"session_id"`
	RootPID   int             `json:"root_pid"`
	Body      json.RawMessage `json:"body"`
}

// ErrorResponse is C5's non-201 body shape.
type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// Error codes C5 names explicitly.
const (
	ErrInvalidSpec     = "invalid_spec"     // 400
	ErrSessionExists   = "session_exists"   // 409
	ErrIdentityRefused = "identity_refused" // 502
	ErrSpawnFailed     = "spawn_failed"     // 500
)

// TerminateRequest is C5's POST /terminate body.
type TerminateRequest struct {
	SessionID string `json:"session_id"`
	Reason    string `json:"reason"`
}

// projectRef mirrors LaunchRequest's non-null "project" object shape
// (cmd/relay/session_launch.go's own inline anonymous struct) — duplicated
// rather than imported for the same reason permissionRequestBody is: cmd
// depends on this package, never the reverse.
type projectRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

// sessionRequestBody mirrors cmd/relay/session_launch.go's
// eveSessionRequestBody — C5's "session_request: claude/pi/chat: eve's POST
// /api/sessions body after relay's policy merge" — duplicated rather than
// imported for the same reason permissionRequestBody is.
type sessionRequestBody struct {
	ProjectID      string          `json:"projectId"`
	Directory      string          `json:"directory"`
	Name           string          `json:"name"`
	Model          string          `json:"model"`
	Settings       json.RawMessage `json:"settings,omitempty"`
	SystemPrompt   string          `json:"systemPrompt,omitempty"`
	AppendClaudeMd bool            `json:"appendClaudeMd,omitempty"`
}

// permissionRequestBody mirrors internal/sessions/hook's wire shape for
// /permission — duplicated rather than imported so hostapi and the hook
// client can each change their own JSON tag details without coupling two
// otherwise-independent small packages together over one shared struct.
type permissionRequestBody struct {
	SessionID string `json:"sessionId"`
	ToolName  string `json:"toolName"`
	ToolInput string `json:"toolInput"`
	ToolUseID string `json:"toolUseId"`
}

type permissionResponseBody struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}
