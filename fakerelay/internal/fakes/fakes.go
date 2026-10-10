// Package fakes is the seam between the session and API doors and whatever
// stands behind them: an agent, an MCP and a model host. The implementations
// here run in process; an adapter that runs a real fake binary implements the
// same interfaces.
package fakes

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

// Frame is one wire frame an agent emits during a turn. The hub adds the
// session id and routes it by its "type": llm_event or permission_request.
type Frame map[string]any

// SessionInfo is what an agent is told about its session.
type SessionInfo struct {
	ID, ProjectID, Name, Directory, Model, Kind string
}

// Agent serves one session.
type Agent interface {
	// Turn emits the frames of one turn and returns when the turn ends. A
	// *TurnError return is an API failure; ctx ending is a stop.
	Turn(ctx context.Context, text string, emit func(Frame)) error
	Answer(permissionID string, approved bool)
}

type AgentFactory func(model world.Model, session SessionInfo) (Agent, error)

type MCPServer interface {
	Tools(ctx context.Context) ([]json.RawMessage, error)
}

type ModelHost interface {
	Models(ctx context.Context) ([]json.RawMessage, error)
}

// TurnError is a failed turn: message_complete carries isError and Status.
type TurnError struct{ Status int }

func (e *TurnError) Error() string { return fmt.Sprintf("api error %d", e.Status) }
