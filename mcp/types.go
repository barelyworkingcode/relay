package mcp

import "encoding/json"

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
