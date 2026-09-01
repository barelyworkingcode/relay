package mcpbroker

import (
	"fmt"

	"github.com/barelyworkingcode/relay/internal/jsonrpc"
)

// mcpRPCError carries the JSON-RPC error CODE alongside the rendered text
// because a caller can be required to act differently on different ones:
// context/enumerate must tell -32601 ("this MCP does not implement
// enumeration" — degrade permanently) from -32602 ("relay asked for a field
// it should not have" — a relay bug, surface it) from everything else
// ("could not answer right now" — offer a retry), and matching on the
// message text is how the three quietly become one.
//
// It is deliberately NOT jsonrpc.CodedError. That type means "this is the
// code relay's own listener should answer its caller with"
// (bridge/frameconn.go reads it that way), and an external MCP's -32001 is
// not relay's -32001 — promoting one to the other would let an MCP dictate
// how relay's own access denials read.
type mcpRPCError struct {
	Code    int
	Message string
	Data    interface{}
}

func (e *mcpRPCError) Error() string {
	if e.Data != nil {
		return fmt.Sprintf("JSON-RPC error %d: %s (data: %v)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message)
}

func formatJSONRPCError(e *jsonrpc.Error) error {
	return &mcpRPCError{Code: e.Code, Message: e.Message, Data: e.Data}
}
