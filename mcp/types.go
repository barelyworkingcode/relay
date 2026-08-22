package mcp

import "encoding/json"

// ProtocolVersion is the MCP protocol version supported by this implementation.
const ProtocolVersion = "2024-11-05"

// MCP JSON-RPC method names.
const (
	MethodInitialize  = "initialize"
	MethodInitialized = "notifications/initialized"
	MethodToolsList   = "tools/list"
	MethodToolsCall   = "tools/call"
	// MethodProgress is the standard MCP server→client progress notification.
	// A client opts in by including _meta.progressToken on a request; the
	// server then emits these referencing that token.
	MethodProgress = "notifications/progress"

	// MethodContextEnumerate asks a server to list the valid values of one
	// contextSchema field it declared `enumerable: true` (ADR-011 decision 6),
	// so an operator picks a resource rather than typing its name.
	//
	// Params: {"field": "<name>", "values": {"<dependency>": <chosen>}}
	// Result: {"field": "<name>", "values": [{"value": …, "label": "…"}]}
	//
	// A server that does not implement it answers -32601, which relay reads as
	// "free-text entry for this MCP" and stops asking. It is NOT a tool call:
	// it carries no _meta, spends no budget, and never reaches the audited
	// tool chokepoint.
	MethodContextEnumerate = "context/enumerate"
)

// Tool represents an MCP tool definition.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema interface{}     `json:"inputSchema"`
	Annotations json.RawMessage `json:"annotations,omitempty"`
	Category    string          `json:"category,omitempty"`
}

// CallToolResult is the result of calling a tool.
type CallToolResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

// Content represents a single content item in a tool result.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}
