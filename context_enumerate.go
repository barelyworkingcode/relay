package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"relaygo/jsonrpc"
	"relaygo/mcp"
)

// context/enumerate (ADR-011 decision 6) asks a connected MCP for the real
// values of a scope field instead of relying on free-text entry, which fails
// closed on a typo (decision 4) -- silently and baffling.
//
// It is a SEPARATE JSON-RPC method, not a tool call: routing it through
// appRouter.CallTool would land it in the audit log as a tool call nobody
// made, consume ADR-010 budget, and run with relay's own unscoped authority
// through the chokepoint that exists to constrain agents. It is deliberately
// not reachable from the remote listener, whose dispatch table holds exactly
// ListTools and CallTool.

// ContextEnumStatus values. There are six because a caller must act
// differently on each, and collapsing any two of them produces a UI that
// lies: the whole hazard here is an empty list rendered as "there are none"
// when the call actually failed.
const (
	EnumStatusOK = "ok"

	// EnumStatusUnsupported: the MCP does not implement enumeration at all.
	// The caller degrades to text entry; relay latches this for the life of
	// the connection so a picker cannot re-ask on every open.
	EnumStatusUnsupported = "unsupported"

	// EnumStatusInvalidField means relay asked for a field the MCP will not
	// enumerate, which is a RELAY bug (relay is meant to ask only for fields
	// declaring enumerable: true). It is surfaced, not degraded like the
	// other failure statuses -- degrading would hide the bug behind a
	// working-looking box.
	EnumStatusInvalidField = "invalid_field"

	EnumStatusUnavailable   = "unavailable"
	EnumStatusUnknownMcp    = "unknown_mcp"
	EnumStatusNotEnumerable = "not_enumerable"
)

// ContextEnumValue is one offered value for a scope field.
//
// Value stays json.RawMessage rather than being decoded to a string because
// the contract is that it goes into _meta VERBATIM: relay does not know what
// a mailbox is (decision 3) and does not need to know what shape one is
// either.
type ContextEnumValue struct {
	Value json.RawMessage `json:"value"`
	Label string          `json:"label,omitempty"`
}

// ContextEnumResult is one enumeration answer, in the one shape both
// operator surfaces return.
//
// Values has NO omitempty, and that is load-bearing: `"values": []` means
// the MCP answered and there are none, while `"values": null` means nobody
// could look. A caller that renders the second as the first is asserting a
// fact about the host it does not have.
type ContextEnumResult struct {
	McpID  string             `json:"mcp_id"`
	Field  string             `json:"field"`
	Status string             `json:"status"`
	Values []ContextEnumValue `json:"values"`
	Error  string             `json:"error,omitempty"`
}

func (r ContextEnumResult) OK() bool { return r.Status == EnumStatusOK }

// ContextEnumerator is implemented by *ExternalMcpManager; taken as an
// interface so a test can supply an MCP that answers -32601, one that
// answers -32602, and one whose transport is dead, without spawning three
// processes.
type ContextEnumerator interface {
	EnumerateContextField(ctx context.Context, mcpID, field string, values map[string]json.RawMessage) ContextEnumResult
}

// enumerateScopeField is THE entry point for both operator surfaces. Every
// check relay makes on its own happens here, exactly once, so the HTTP
// route and the IPC handler cannot drift into asking different questions.
func enumerateScopeField(ctx context.Context, surfaces McpSurfaces, enum ContextEnumerator, mcpID, field string, chosen map[string]json.RawMessage) ContextEnumResult {
	res := ContextEnumResult{McpID: mcpID, Field: field}

	if mcpID == "" || field == "" {
		res.Status = EnumStatusNotEnumerable
		res.Error = "both mcp_id and field are required"
		return res
	}
	if _, known := surfaces[mcpID]; !known {
		res.Status = EnumStatusUnknownMcp
		res.Error = fmt.Sprintf("MCP %q is not registered or not connected, so relay cannot say what it scopes", mcpID)
		return res
	}

	schema := surfaces.Schema(mcpID)
	declared, ok := schema.Field(field)
	if !ok || !declared.Restricts() {
		res.Status = EnumStatusNotEnumerable
		res.Error = fmt.Sprintf("%s does not declare %q as a scope: %q field", mcpID, field, ContextScopeRestrict)
		return res
	}
	if declared.FromProjectPath() {
		// Nothing to pick: relay derives the value from the project's path
		// and refuses an operator-supplied one outright.
		res.Status = EnumStatusNotEnumerable
		res.Error = fmt.Sprintf("%s.%s is derived by relay from the project path, not chosen by an operator", mcpID, field)
		return res
	}
	if !declared.Enumerable {
		res.Status = EnumStatusNotEnumerable
		res.Error = fmt.Sprintf("%s does not declare %s as enumerable", mcpID, field)
		return res
	}

	send := dependencyValues(declared, chosen)

	if enum == nil {
		res.Status = EnumStatusUnavailable
		res.Error = "enumeration is not available in this mode"
		return res
	}
	return enum.EnumerateContextField(ctx, mcpID, field, send)
}

// dependencyValues picks out exactly the fields this one declares in
// depends_on, dropping any that are absent or empty -- for which the
// contract is "across everything", spelled by leaving the key out rather
// than sending an empty list. hasScopeValue is the same emptiness rule the
// rest of ADR-011 uses.
//
// The dropping is the important half, not a tidy-up: the picker's normal
// initial state is a dependency nobody has chosen yet, and a request
// carrying {"mail_accounts": []} invites the server to read it as "match
// nothing" and answer with an empty list -- indistinguishable from a host
// with no mailboxes at all.
func dependencyValues(f ContextField, chosen map[string]json.RawMessage) map[string]json.RawMessage {
	if len(f.DependsOn) == 0 || len(chosen) == 0 {
		return nil
	}
	out := make(map[string]json.RawMessage, len(f.DependsOn))
	for _, name := range f.DependsOn {
		if hasScopeValue(chosen, name) {
			out[name] = chosen[name]
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// EnumerateContextField sends one context/enumerate request to a connected
// MCP and classifies the answer. It never returns an error: every outcome
// is one of the six statuses, because the caller is a picker whose job is
// to degrade correctly rather than to propagate.
func (m *ExternalMcpManager) EnumerateContextField(ctx context.Context, mcpID, field string, values map[string]json.RawMessage) ContextEnumResult {
	res := ContextEnumResult{McpID: mcpID, Field: field}

	m.mu.RLock()
	conn, connected := m.conns[mcpID]
	latched := m.enumUnsupported[mcpID]
	m.mu.RUnlock()

	if latched {
		res.Status = EnumStatusUnsupported
		res.Error = fmt.Sprintf("%s does not implement context/enumerate", mcpID)
		return res
	}
	if !connected {
		res.Status = EnumStatusUnavailable
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
		Field  string             `json:"field"`
		Values []ContextEnumValue `json:"values"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		res.Status = EnumStatusUnavailable
		res.Error = fmt.Sprintf("%s answered context/enumerate with something that is not an enumeration: %v", mcpID, err)
		return res
	}
	// An answer about a different field is not this field's answer: rendering
	// it would put one field's values in another field's picker.
	if payload.Field != "" && payload.Field != field {
		res.Status = EnumStatusUnavailable
		res.Error = fmt.Sprintf("%s was asked about %q and answered about %q", mcpID, field, payload.Field)
		return res
	}
	for i, v := range payload.Values {
		if !hasEnumValue(v.Value) {
			res.Status = EnumStatusUnavailable
			res.Error = fmt.Sprintf("%s offered an entry with no value at index %d", mcpID, i)
			return res
		}
	}

	// Non-nil even when empty: "there are none" must not share a rendering
	// with "nobody could look".
	if payload.Values == nil {
		payload.Values = []ContextEnumValue{}
	}
	res.Status = EnumStatusOK
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
func (m *ExternalMcpManager) classifyEnumError(res ContextEnumResult, mcpID string, err error) ContextEnumResult {
	var rpcErr *mcpRPCError
	if errors.As(err, &rpcErr) {
		switch rpcErr.Code {
		case jsonrpc.CodeMethodNotFound:
			m.latchEnumUnsupported(mcpID)
			res.Status = EnumStatusUnsupported
			res.Error = fmt.Sprintf("%s does not implement context/enumerate", mcpID)
			return res
		case jsonrpc.CodeInvalidParams:
			res.Status = EnumStatusInvalidField
			res.Error = fmt.Sprintf("%s refused the enumeration request for %q: %s", mcpID, res.Field, rpcErr.Message)
			return res
		}
	}
	res.Status = EnumStatusUnavailable
	res.Error = err.Error()
	return res
}

// latchEnumUnsupported records that an MCP answered -32601, so no further
// request is sent to it. Cleared on Stop/StopAll and on a fresh handshake --
// a reconnect is a new process and may be a new build, so the fact is
// scoped to the connection that asserted it, never to the settings entry.
func (m *ExternalMcpManager) latchEnumUnsupported(mcpID string) {
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

// hasEnumValue reports whether an offered value is something that could be
// stored: same emptiness rule as hasScopeValue, one level down.
func hasEnumValue(raw json.RawMessage) bool {
	return hasScopeValue(map[string]json.RawMessage{"v": raw}, "v")
}
