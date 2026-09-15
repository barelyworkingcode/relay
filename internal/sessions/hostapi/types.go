// Package hostapi is relay-sessions' internal API server: contract C5's
// "relay ↔ relay-sessions internal API". It serves two Unix sockets:
//
//   - the internal socket, dialed only by relay itself, carrying POST
//     /launch and POST /terminate;
//   - the hook socket, dialed by `relay-sessions hook` processes (running
//     as a Claude Code PreToolUse hook), carrying POST /permission.
//
// This package is a skeleton in the sense the plan's R-S5 row describes: it
// holds enough of a session table to make /launch and /terminate meaningful
// — spawning a real internal/sessions/shim child, tracking its root pid and
// start time, answering /permission's membership question against that
// table — but no real terminal, Claude, or pi hosting sits behind it yet.
// That lands in R-S6/R-S7b/R-S7c on top of this package's Launcher.
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
	Host           json.RawMessage   `json:"host"`
	Sandbox        *SandboxSpec      `json:"sandbox"`
	Identity       *IdentitySpec     `json:"identity"`
	ModelKey       string            `json:"model_key,omitempty"`
	SessionRequest json.RawMessage   `json:"session_request,omitempty"`
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
