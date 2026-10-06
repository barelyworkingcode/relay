package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/barelyworkingcode/relay/internal/jsonrpc"
)

// DropInAttachRequest is the Arguments payload of a ReqDropInAttach.
type DropInAttachRequest struct {
	SessionID string `json:"session_id"`
	Cols      int    `json:"cols,omitempty"`
	Rows      int    `json:"rows,omitempty"`
}

func (r *DropInAttachRequest) Validate() error {
	if r.SessionID == "" {
		return fmt.Errorf("drop_in_attach: session_id is empty")
	}
	return nil
}

// DropInAttachResult is the ack's Data.
type DropInAttachResult struct {
	SessionID       string `json:"session_id"`
	ClaudeSessionID string `json:"claude_session_id"`
	TerminalID      string `json:"terminal_id"`
	// Host is empty for this machine.
	Host string `json:"host,omitempty"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

// DropInAttacher is implemented by a router that can take a headless Claude
// session over in a relay-hosted terminal. Like SandboxAttacher it is
// optional: a router without it refuses the request type outright.
type DropInAttacher interface {
	// DropInAttach hands the session off, launches and joins the terminal, or
	// returns an error and leaves the session handed back. A *SandboxRefusal
	// error is relayed to the client as is.
	DropInAttach(ctx context.Context, req DropInAttachRequest) (DropInAttachment, error)
}

// DropInAttachment is a launched, joined terminal not yet wired to a client.
// Exactly one of Serve and Abort is called.
type DropInAttachment interface {
	Result() DropInAttachResult
	// Serve pumps bytes between fc and the terminal until either side ends and
	// ends the terminal unless it already exited.
	Serve(ctx context.Context, fc *FrameConn)
	// Abort ends the terminal without a client ever attaching.
	Abort()
}

func handleDropInAttach(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	// These are the same two caller checks as handleSandboxAttach, in the same
	// order and for the same reasons (see there): the kernel's membership
	// answer, then the peer's own current confinement, which reparenting
	// cannot hide.
	if _, member := ConnMembershipFromContext(ctx).Session(); member {
		return sandboxRefusalResponse(&SandboxRefusal{
			Reason:  SandboxReasonInsideSession,
			Message: "relay drop-in cannot be run from inside a relay session; run it from your own terminal",
		})
	}
	if confined, ok := connConfinedFromContext(ctx); !ok || confined {
		return sandboxRefusalResponse(&SandboxRefusal{
			Reason:  SandboxReasonPeerConfined,
			Message: "relay drop-in cannot be run from inside a relay session; run it from your own terminal",
		})
	}
	attacher, ok := router.(DropInAttacher)
	if !ok {
		return bridgeError(jsonrpc.CodeMethodNotFound, "unknown request type: "+req.Type)
	}
	if len(req.Arguments) == 0 {
		return bridgeError(jsonrpc.CodeInvalidParams, "drop_in_attach: missing arguments")
	}
	var r DropInAttachRequest
	if err := json.Unmarshal(req.Arguments, &r); err != nil {
		return bridgeError(jsonrpc.CodeParseError, "drop_in_attach: "+err.Error())
	}
	if err := r.Validate(); err != nil {
		return bridgeError(jsonrpc.CodeInvalidParams, err.Error())
	}

	att, err := attacher.DropInAttach(ctx, r)
	if err != nil {
		var refusal *SandboxRefusal
		if errors.As(err, &refusal) {
			return sandboxRefusalResponse(refusal)
		}
		slog.WarnContext(ctx, "bridge: drop-in attach failed", "error", err)
		return bridgeError(jsonrpc.CodeInternalError, "drop-in: could not start the terminal")
	}
	data, err := json.Marshal(att.Result())
	if err != nil {
		att.Abort()
		return bridgeError(jsonrpc.CodeInternalError, "drop_in_attach: encode result")
	}
	if !SetTakeover(ctx, att.Serve) {
		att.Abort()
		return bridgeError(jsonrpc.CodeInternalError, "drop_in_attach: connection cannot carry a stream")
	}
	return BridgeResponse{Type: RespAttached, Data: data}
}
