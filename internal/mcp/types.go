package mcp

import "encoding/json"

const ProtocolVersion = "2024-11-05"

const (
	MethodInitialize  = "initialize"
	MethodInitialized = "notifications/initialized"
	MethodToolsList   = "tools/list"
	MethodToolsCall   = "tools/call"
	MethodProgress    = "notifications/progress"

	// MethodContextEnumerate is NOT a tool call: it carries no _meta, spends no
	// budget, and never reaches the audited tool chokepoint (ADR-011 decision 6).
	// A server without a handler for it answers -32601, which relay reads as
	// "free-text entry for this MCP" and stops asking.
	MethodContextEnumerate = "context/enumerate"
)

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema interface{}     `json:"inputSchema"`
	Annotations json.RawMessage `json:"annotations,omitempty"`
	Category    string          `json:"category,omitempty"`
}

type CallToolResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}
