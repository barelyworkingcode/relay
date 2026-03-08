package jsonrpc

import "encoding/json"

// Request is an outgoing JSON-RPC 2.0 message.
type Request struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id,omitempty"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
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
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
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
	switch v := id.(type) {
	case float64:
		return int64(v) == expected
	case int64:
		return v == expected
	case json.Number:
		n, err := v.Int64()
		return err == nil && n == expected
	}
	return false
}
