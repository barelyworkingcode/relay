package mcpbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/barelyworkingcode/relay/internal/jsonrpc"
	"github.com/barelyworkingcode/relay/internal/mcp"
	"github.com/barelyworkingcode/relay/internal/project"
)

// EnumerateContextField sends one context/enumerate request to a connected
// MCP and classifies the answer. It never returns an error: every outcome
// is one of the six statuses, because the caller is a picker whose job is
// to degrade correctly rather than to propagate.
func (m *Manager) EnumerateContextField(ctx context.Context, mcpID, field string, values map[string]json.RawMessage) project.ContextEnumResult {
	res := project.ContextEnumResult{McpID: mcpID, Field: field}

	m.mu.RLock()
	conn, connected := m.conns[mcpID]
	latched := m.enumUnsupported[mcpID]
	m.mu.RUnlock()

	if latched {
		res.Status = project.EnumStatusUnsupported
		res.Error = fmt.Sprintf("%s does not implement context/enumerate", mcpID)
		return res
	}
	if !connected {
		res.Status = project.EnumStatusUnavailable
		res.Error = fmt.Sprintf("%s is not connected", mcpID)
		return res
	}

	params := map[string]interface{}{"field": field}
	if len(values) > 0 {
		params["values"] = values
	}

	callCtx, cancel := context.WithTimeout(ctx, MCPEnumerateTimeout)
	defer cancel()

	raw, err := conn.SendRequest(callCtx, mcp.MethodContextEnumerate, params)
	if err != nil {
		return m.classifyEnumError(res, mcpID, err)
	}

	var payload struct {
		Field  string                     `json:"field"`
		Values []project.ContextEnumValue `json:"values"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		res.Status = project.EnumStatusUnavailable
		res.Error = fmt.Sprintf("%s answered context/enumerate with something that is not an enumeration: %v", mcpID, err)
		return res
	}
	// An answer about a different field is not this field's answer: rendering
	// it would put one field's values in another field's picker.
	if payload.Field != "" && payload.Field != field {
		res.Status = project.EnumStatusUnavailable
		res.Error = fmt.Sprintf("%s was asked about %q and answered about %q", mcpID, field, payload.Field)
		return res
	}
	for i, v := range payload.Values {
		if !project.HasEnumValue(v.Value) {
			res.Status = project.EnumStatusUnavailable
			res.Error = fmt.Sprintf("%s offered an entry with no value at index %d", mcpID, i)
			return res
		}
	}

	// Non-nil even when empty: "there are none" must not share a rendering
	// with "nobody could look".
	if payload.Values == nil {
		payload.Values = []project.ContextEnumValue{}
	}
	res.Status = project.EnumStatusOK
	res.Values = payload.Values
	return res
}

// classifyEnumError turns a SendRequest failure into the status a picker
// acts on. Exactly two JSON-RPC codes are recognised; everything else --
// every other code, a malformed answer, a dead pipe, a timeout -- becomes
// "could not answer right now", the fail-safe default: macMCP answers
// -32000 when Mail itself will not answer, a transient condition to retry
// rather than a fact about whether the method exists, and matching a
// specific implementation-defined code here would make the default wrong
// for any server that picks a different one.
func (m *Manager) classifyEnumError(res project.ContextEnumResult, mcpID string, err error) project.ContextEnumResult {
	var rpcErr *mcpRPCError
	if errors.As(err, &rpcErr) {
		switch rpcErr.Code {
		case jsonrpc.CodeMethodNotFound:
			m.latchEnumUnsupported(mcpID)
			res.Status = project.EnumStatusUnsupported
			res.Error = fmt.Sprintf("%s does not implement context/enumerate", mcpID)
			return res
		case jsonrpc.CodeInvalidParams:
			res.Status = project.EnumStatusInvalidField
			res.Error = fmt.Sprintf("%s refused the enumeration request for %q: %s", mcpID, res.Field, rpcErr.Message)
			return res
		}
	}
	res.Status = project.EnumStatusUnavailable
	res.Error = err.Error()
	return res
}

// latchEnumUnsupported records that an MCP answered -32601, so no further
// request is sent to it. Cleared on Stop/StopAll and on a fresh handshake --
// a reconnect is a new process and may be a new build, so the fact is
// scoped to the connection that asserted it, never to the settings entry.
func (m *Manager) latchEnumUnsupported(mcpID string) {
	m.mu.Lock()
	if m.enumUnsupported == nil {
		m.enumUnsupported = make(map[string]bool)
	}
	already := m.enumUnsupported[mcpID]
	m.enumUnsupported[mcpID] = true
	m.mu.Unlock()
	if !already {
		slog.Info("MCP does not implement context/enumerate; scope values stay free text",
			"id", mcpID, "method", mcp.MethodContextEnumerate)
	}
}
