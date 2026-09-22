package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/barelyworkingcode/relay/internal/jsonrpc"
	"github.com/barelyworkingcode/relay/internal/peertoken"
)

// Stream frame types, in both directions, after a RespAttached ack.
const (
	// StreamInput and StreamResize travel client to relay.
	StreamInput  = "input"
	StreamResize = "resize"

	// StreamOutput and StreamExit travel relay to client. StreamExit is the
	// last frame relay sends and carries the tool's exit code.
	StreamOutput = "output"
	StreamExit   = "exit"
)

// Refusal reasons a SandboxRefusal carries in BridgeResponse.Data.
const (
	SandboxReasonInsideSession    = "inside_session"
	SandboxReasonInvalidRequest   = "invalid_request"
	SandboxReasonNoProject        = "project_not_found"
	SandboxReasonProjectAmbiguous = "project_ambiguous"
	SandboxReasonProjectMismatch  = "project_mismatch"
	SandboxReasonUnknownTemplate  = "template_unknown"
	SandboxReasonTemplateDenied   = "template_not_allowed"
	SandboxReasonLaunchRefused    = "launch_refused"
	SandboxReasonUnavailable      = "unavailable"
	// SandboxReasonPeerConfined is distinct from SandboxReasonInsideSession:
	// it fires when the ancestry walk finds no session root at all (a
	// reparented process is invisible to it) but the peer's OWN, current
	// Seatbelt confinement — a property no fork can shed — says otherwise.
	// The client message can read the same; the constant exists so relay's
	// own logs can tell which guard caught the caller.
	SandboxReasonPeerConfined = "peer_confined"
)

// SandboxAttachRequest is the Arguments payload of a ReqSandboxAttach. Cwd
// travels here and not in BridgeRequest.Cwd, which the server deliberately
// never reads: it is a claim the request is about, not a credential.
type SandboxAttachRequest struct {
	Template string `json:"template"`
	// Cwd is an absolute path; relay resolves it to a project.
	Cwd string `json:"cwd"`
	// Project optionally names (by id or name) which of the projects holding
	// Cwd to use. It never widens: it must be one of them.
	Project string `json:"project,omitempty"`
	Cols    int    `json:"cols,omitempty"`
	Rows    int    `json:"rows,omitempty"`
}

func (r *SandboxAttachRequest) Validate() error {
	if r.Template == "" {
		return fmt.Errorf("sandbox: template is empty")
	}
	if r.Cwd == "" || r.Cwd[0] != '/' {
		return fmt.Errorf("sandbox: cwd must be an absolute path")
	}
	return nil
}

// SandboxAttachResult is the ack's Data.
type SandboxAttachResult struct {
	SessionID   string `json:"session_id"`
	ProjectName string `json:"project_name"`
	TemplateID  string `json:"template_id"`
	Cols        int    `json:"cols"`
	Rows        int    `json:"rows"`
}

// StreamFrame is one newline-delimited JSON frame of the attached byte stream.
// Data is base64 on the wire (encoding/json's []byte form). Code has no
// omitempty because a zero exit code is the common, meaningful value.
type StreamFrame struct {
	Type string `json:"type"`
	Data []byte `json:"data,omitempty"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
	Code int    `json:"code"`
}

// SandboxRefusal is a ReqSandboxAttach refusal a client can act on. It is the
// Data of the Error response; Message alone is enough to show a human.
type SandboxRefusal struct {
	Reason string `json:"reason"`
	// Projects lists the candidate project names when the reason is
	// project_ambiguous or project_mismatch.
	Projects []string `json:"projects,omitempty"`
	Message  string   `json:"message"`
}

func (r *SandboxRefusal) Error() string { return r.Message }

// SandboxAttacher is implemented by a router that can host sandbox sessions.
// It is an optional interface, like MembershipResolver, so a router that does
// not implement it refuses the request type outright.
type SandboxAttacher interface {
	// SandboxAttach authorizes and launches the session and joins it, or
	// returns an error and leaves nothing running. A *SandboxRefusal error is
	// relayed to the client as is.
	SandboxAttach(ctx context.Context, req SandboxAttachRequest) (SandboxAttachment, error)
}

// SandboxAttachment is a launched, joined session not yet wired to a client.
// Exactly one of Serve and Abort is called.
type SandboxAttachment interface {
	Result() SandboxAttachResult
	// Serve pumps bytes between fc and the session until either side ends,
	// terminates the session unless it already exited, and returns.
	Serve(ctx context.Context, fc *FrameConn)
	// Abort ends the session without a client ever attaching.
	Abort()
}

func handleSandboxAttach(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	// This is the real guard for "not from inside a relay session": the
	// membership answer is the kernel's, where the client's RELAY_SESSION_ID
	// check is only a courtesy. It is not the whole guard, though: the walk
	// stops at a reparented process (ppid 1 before it ever reaches a live
	// root), so a double-forked descendant of a session is invisible to it.
	if _, member := ConnMembershipFromContext(ctx).Session(); member {
		return sandboxRefusalResponse(&SandboxRefusal{
			Reason:  SandboxReasonInsideSession,
			Message: "relay sandbox cannot be run from inside a relay session; run it from your own terminal",
		})
	}
	// This is subtle: ancestry can be erased by reparenting, but a process's
	// own current Seatbelt confinement cannot — a profile is inherited by
	// every descendant a plain fork produces. Checking it independently
	// catches exactly the peer the walk above just lost track of. !ok means
	// the check itself could not run (no readable peer token, or the
	// platform could not resolve its kernel primitive); that is treated
	// identically to confined, never as a pass.
	if confined, ok := connConfinedFromContext(ctx); !ok || confined {
		return sandboxRefusalResponse(&SandboxRefusal{
			Reason:  SandboxReasonPeerConfined,
			Message: "relay sandbox cannot be run from inside a relay session; run it from your own terminal",
		})
	}
	attacher, ok := router.(SandboxAttacher)
	if !ok {
		return bridgeError(jsonrpc.CodeMethodNotFound, "unknown request type: "+req.Type)
	}
	if len(req.Arguments) == 0 {
		return bridgeError(jsonrpc.CodeInvalidParams, "sandbox_attach: missing arguments")
	}
	var r SandboxAttachRequest
	if err := json.Unmarshal(req.Arguments, &r); err != nil {
		return bridgeError(jsonrpc.CodeParseError, "sandbox_attach: "+err.Error())
	}
	if err := r.Validate(); err != nil {
		return bridgeError(jsonrpc.CodeInvalidParams, err.Error())
	}

	att, err := attacher.SandboxAttach(ctx, r)
	if err != nil {
		var refusal *SandboxRefusal
		if errors.As(err, &refusal) {
			return sandboxRefusalResponse(refusal)
		}
		slog.Warn("bridge: sandbox attach failed", "error", err)
		return bridgeError(jsonrpc.CodeInternalError, "sandbox: could not start the session")
	}
	data, err := json.Marshal(att.Result())
	if err != nil {
		att.Abort()
		return bridgeError(jsonrpc.CodeInternalError, "sandbox_attach: encode result")
	}
	if !SetTakeover(ctx, att.Serve) {
		att.Abort()
		return bridgeError(jsonrpc.CodeInternalError, "sandbox_attach: connection cannot carry a stream")
	}
	return BridgeResponse{Type: RespAttached, Data: data}
}

func sandboxRefusalResponse(r *SandboxRefusal) BridgeResponse {
	resp := bridgeError(jsonrpc.CodeInvalidParams, r.Message)
	if r.Reason == SandboxReasonInsideSession || r.Reason == SandboxReasonPeerConfined {
		resp.Code = jsonrpc.CodeUnauthorized
	}
	resp.Data, _ = json.Marshal(r)
	return resp
}

// PeerConfinedFunc reports whether tok's process is, right now, confined by
// any sandbox profile. Unlike MembershipResolver's ancestry walk this is a
// property of the process itself, not its lineage, so reparenting cannot
// hide it. ok is false when the check could not run to completion at all —
// an unreadable peer token, or the platform implementation could not resolve
// its kernel primitive — and a caller must treat !ok exactly like
// confined == true; there is no safe reading of "could not verify".
//
// A darwin build's real implementation lives in peer_confined_darwin.go; the
// signature is declared here because handleSandboxAttach, this type's only
// caller, is platform-independent code that must still build everywhere.
type PeerConfinedFunc func(tok peertoken.Token) (confined bool, ok bool)

type connConfinedCtxKey struct{}

// connConfined is one connection's confinement answer, computed once at
// accept time in BridgeServer.handleConn and never recomputed: like the peer
// token itself, a connection's peer cannot become a different process for
// the life of the socket.
type connConfined struct {
	confined bool
	ok       bool
}

// withConnConfined carries one connection's PeerConfinedFunc answer for
// handleSandboxAttach to read back. It is scoped to this one gate — no other
// handler consults it — the same way SandboxAttacher is scoped to this file.
func withConnConfined(ctx context.Context, confined, ok bool) context.Context {
	return context.WithValue(ctx, connConfinedCtxKey{}, connConfined{confined: confined, ok: ok})
}

// connConfinedFromContext answers not-confined for a context that never
// carried a value. This is deliberate, not a widening default: every
// connection BridgeServer.Serve accepts always calls withConnConfined, so
// the missing case is reached only by a test that calls handleSandboxAttach
// directly with a bare context, bypassing per-connection setup entirely —
// the same convention ConnMembershipFromContext's nil answer already follows
// for membership. It is not a path any real bridge connection takes.
func connConfinedFromContext(ctx context.Context) (confined bool, ok bool) {
	c, present := ctx.Value(connConfinedCtxKey{}).(connConfined)
	if !present {
		return false, true
	}
	return c.confined, c.ok
}
