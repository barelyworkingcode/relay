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

// ---------------------------------------------------------------------------
// context/enumerate — asking an MCP what a scope field's values are
// (ADR-011 decision 6)
// ---------------------------------------------------------------------------
//
// Constraint 2 is the reason this exists. Typing "INBOX" by hand is the
// error-prone step, and under decision 4 a typo fails CLOSED — the agent
// silently gets nothing, which is safe and baffling. So for a field the MCP
// says it can enumerate, the editor offers the real values instead of a box.
//
// It is a SEPARATE JSON-RPC method, not a tool call, and that is decided
// rather than incidental. Routing an operator-UI read through
// appRouter.CallTool would land it in the audit log as a tool call nobody
// made, consume ADR-010 budget, and run with relay's own unscoped authority
// through the chokepoint that exists to constrain agents. It would also make
// relay extract values from a free-form tool result — a path expression per
// MCP, which is domain knowledge by the back door.
//
// The request relay sends:
//
//	{"jsonrpc":"2.0","id":7,"method":"context/enumerate",
//	 "params":{"field":"mail_mailboxes","values":{"mail_accounts":["Bob"]}}}
//
// `values` carries the already-chosen values of the fields THIS field declares
// in depends_on, and nothing else. Absent or empty means "across everything".
//
// The success answer:
//
//	{"result":{"field":"mail_mailboxes",
//	           "values":[{"value":"INBOX","label":"INBOX"},
//	                     {"value":"Projects/Archive","label":"Projects/Archive (Bob)"}]}}
//
// `value` is what goes into _meta verbatim; `label` is display only. An empty
// list is a valid answer and means there are none.
//
// Enumeration is itself DISCLOSURE — the list of every mail account on the
// machine — so this is a new read path and it is guarded like every other
// project route: the frontend Unix socket at 0600 behind the frontend bearer
// token, and the tray's own IPC channel. It is deliberately NOT reachable from
// the remote listener, whose dispatch table holds exactly ListTools and
// CallTool (remote_server.go, TestRemoteDispatchTable_HoldsExactlyListToolsAndCallTool).

// ContextEnumStatus values. There are six because a caller must act
// differently on each, and collapsing any two of them produces a UI that lies:
// the whole hazard here is an empty list rendered as "there are none" when the
// call actually failed.
const (
	// EnumStatusOK — the MCP answered. Values is non-nil, and an EMPTY
	// Values is a real answer meaning "there are none".
	EnumStatusOK = "ok"

	// EnumStatusUnsupported — the MCP answered -32601. It does not implement
	// enumeration at all. The caller degrades to text entry, silently and
	// permanently: relay latches it for the life of the connection so a
	// picker cannot re-ask on every open, and a reconnect re-asks once.
	EnumStatusUnsupported = "unsupported"

	// EnumStatusInvalidField — the MCP answered -32602. Relay asked for a
	// field the MCP will not enumerate, which is a RELAY bug (relay is meant
	// to ask only for fields declaring enumerable: true). It is surfaced, not
	// degraded — degrading would hide the bug behind a working-looking box.
	EnumStatusInvalidField = "invalid_field"

	// EnumStatusUnavailable — anything else: another JSON-RPC error, a
	// malformed answer, a dead connection, a timeout. The MCP could not
	// answer RIGHT NOW. The caller says "could not list — try again" and
	// keeps text entry available, so an operator is never blocked.
	EnumStatusUnavailable = "unavailable"

	// EnumStatusUnknownMcp — relay has never connected to this MCP, so it
	// cannot say what the field even is. Relay's own refusal; no call made.
	EnumStatusUnknownMcp = "unknown_mcp"

	// EnumStatusNotEnumerable — the field is not one this MCP declares as
	// enumerable (or is not a restrict field, or is derived by relay). Relay's
	// own refusal, made BEFORE any call: decision 6 honours enumeration for
	// fields declaring enumerable: true and for no others.
	EnumStatusNotEnumerable = "not_enumerable"
)

// ContextEnumValue is one offered value for a scope field.
//
// Value stays json.RawMessage rather than being decoded to a string because
// the contract is that it goes into _meta VERBATIM. Relay does not know what a
// mailbox is (decision 3) and it does not need to know what shape one is
// either; the declared fragment is what the value is validated against on save.
type ContextEnumValue struct {
	Value json.RawMessage `json:"value"`
	Label string          `json:"label,omitempty"`
}

// ContextEnumResult is one enumeration answer, in the one shape both operator
// surfaces return — the HTTP route eve calls and the tray's IPC event.
//
// Values has NO omitempty, and that is the load-bearing detail: `"values": []`
// means the MCP answered and there are none, while `"values": null` means
// nobody could look. A caller that renders the second as the first is asserting
// a fact about the host it does not have.
type ContextEnumResult struct {
	McpID  string             `json:"mcp_id"`
	Field  string             `json:"field"`
	Status string             `json:"status"`
	Values []ContextEnumValue `json:"values"`
	Error  string             `json:"error,omitempty"`
}

// OK reports whether the answer carries a value list that may be rendered.
func (r ContextEnumResult) OK() bool { return r.Status == EnumStatusOK }

// ContextEnumerator asks a live MCP to enumerate one of its scope fields.
// Implemented by *ExternalMcpManager; taken as an interface by the surfaces so
// a test can supply an MCP that answers -32601, one that answers -32602, and
// one whose transport is dead, without spawning three processes.
type ContextEnumerator interface {
	EnumerateContextField(ctx context.Context, mcpID, field string, values map[string]json.RawMessage) ContextEnumResult
}

// enumerateScopeField is THE entry point for both operator surfaces. Every
// check relay makes on its own — is this an MCP relay knows, is this a field
// it declares, is it one the MCP said it can enumerate, which dependency
// values may be sent — happens here, exactly once, so the HTTP route and the
// IPC handler cannot drift into asking different questions.
//
// chosen is what the operator has already picked in the form. It is FILTERED
// to the field's own depends_on before it leaves relay: a surface that sent
// the whole form would be telling the MCP about fields it never said this one
// depended on, and the request shape is pinned.
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

// dependencyValues picks out of the operator's already-chosen values exactly
// the fields this one declares in depends_on, dropping any that are absent or
// empty — for which the contract is "across everything", spelled by leaving
// the key out rather than by sending an empty list. hasScopeValue is the same
// emptiness rule the rest of ADR-011 uses, so an empty choice cannot arrive
// here reading as a narrowing.
//
// That dropping is the important half, not a tidy-up. The picker's NORMAL
// initial state is a dependency nobody has chosen yet — the panel opens on
// mail_mailboxes with mail_accounts still empty — and a request carrying
// {"mail_accounts": []} invites the server to read it as "match nothing" and
// answer with an empty list. An empty picker at exactly the moment an operator
// opens one is indistinguishable from a host with no mailboxes. macMCP hit the
// server-side half of this before shipping and now reads empty as absent; not
// sending it at all is the half that belongs here.
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

// ---------------------------------------------------------------------------
// The client
// ---------------------------------------------------------------------------

// EnumerateContextField sends one context/enumerate request to a connected MCP
// and classifies the answer. It never returns an error: every outcome is one of
// the six statuses, because the caller is a picker whose job is to degrade
// correctly rather than to propagate.
func (m *ExternalMcpManager) EnumerateContextField(ctx context.Context, mcpID, field string, values map[string]json.RawMessage) ContextEnumResult {
	res := ContextEnumResult{McpID: mcpID, Field: field}

	m.mu.RLock()
	conn, connected := m.conns[mcpID]
	latched := m.enumUnsupported[mcpID]
	m.mu.RUnlock()

	if latched {
		// Answered once, remembered for the life of this connection. Asking
		// again on every field, every time a panel opens, would spend a round
		// trip per open to learn a fact that cannot change without a restart.
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

	// An operator is waiting on a control. MCPRequestTimeout is five minutes
	// because a tool call can be an LLM inference; a dropdown that spins for
	// five minutes is worse than one that says "could not list — try again"
	// with the text box still there.
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
	// An answer about a different field is not this field's answer. Rendering
	// it would put one field's values in another field's picker, which is a
	// confinement the operator did not choose.
	if payload.Field != "" && payload.Field != field {
		res.Status = EnumStatusUnavailable
		res.Error = fmt.Sprintf("%s was asked about %q and answered about %q", mcpID, field, payload.Field)
		return res
	}
	for i, v := range payload.Values {
		if !hasEnumValue(v.Value) {
			// An option with nothing to store cannot be offered, and dropping
			// it silently would shorten a list an operator reads as complete.
			res.Status = EnumStatusUnavailable
			res.Error = fmt.Sprintf("%s offered an entry with no value at index %d", mcpID, i)
			return res
		}
	}

	// Non-nil even when empty: "there are none" is an answer, and it must not
	// share a rendering with "nobody could look".
	if payload.Values == nil {
		payload.Values = []ContextEnumValue{}
	}
	res.Status = EnumStatusOK
	res.Values = payload.Values
	return res
}

// classifyEnumError turns a SendRequest failure into the status a picker acts
// on. The three cases are kept apart deliberately — collapsing -32601 into
// "could not list" would put a retry button in front of an MCP that will never
// answer, and collapsing -32602 into "does not implement" would hide a relay
// bug behind a working-looking text box.
//
// EXACTLY TWO codes are recognised, and everything else — every other JSON-RPC
// code, a malformed answer, a dead pipe, a timeout — is "could not answer right
// now". That is the fail-safe direction and it is the one the range JSON-RPC
// reserves for implementation-defined server errors (-32000..-32099) needs:
// macMCP answers -32000 when Mail itself will not answer, which is a transient
// condition an operator retries, not a fact about whether the method exists.
// Matching a specific code there would make the default the wrong branch for
// every server that picks a different number in the same range.
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
// request is sent to it. Cleared on Stop/StopAll and on a fresh handshake — a
// reconnect is a new process and may be a new build, so the fact is scoped to
// the connection that asserted it and never to the settings entry.
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
// stored. Same emptiness rule as hasScopeValue, one level down: an option that
// stores nothing is an option that confines nothing.
func hasEnumValue(raw json.RawMessage) bool {
	return hasScopeValue(map[string]json.RawMessage{"v": raw}, "v")
}
