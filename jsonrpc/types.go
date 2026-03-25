package jsonrpc

import "encoding/json"

// Standard JSON-RPC 2.0 error codes.
const (
	CodeParseError     = -32700
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Application-defined error codes (reserved range -32000 to -32099).
const (
	CodeUnauthorized = -32001
)

// Version is the JSON-RPC protocol version string.
const Version = "2.0"

// Request is an outgoing JSON-RPC 2.0 message.
type Request struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id,omitempty"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// NewRequest creates a JSON-RPC 2.0 request with the version field pre-set.
func NewRequest(id interface{}, method string, params interface{}) Request {
	return Request{JSONRPC: Version, ID: id, Method: method, Params: params}
}

// NewNotification creates a JSON-RPC 2.0 notification (no ID, no response expected).
func NewNotification(method string) Request {
	return Request{JSONRPC: Version, Method: method}
}

// ServerRequest is an incoming JSON-RPC 2.0 message where Params is preserved
// as raw JSON to avoid lossy round-trips through interface{}.
type ServerRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is an incoming JSON-RPC 2.0 message.
// ID intentionally omits `omitempty` so that null IDs are serialized as
// "id": null (required by JSON-RPC 2.0 spec for parse error responses).
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC 2.0 error object.
type Error struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// RespIDEquals checks if a JSON-RPC response ID matches an expected int64 value.
func RespIDEquals(id interface{}, expected int64) bool {
	v, ok := RespIDToInt64(id)
	return ok && v == expected
}

// RespIDToInt64 extracts an int64 from a JSON-RPC response ID.
func RespIDToInt64(id interface{}) (int64, bool) {
	switch v := id.(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	}
	return 0, false
}
