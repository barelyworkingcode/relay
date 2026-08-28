package jsonrpc

import "encoding/json"

const (
	CodeParseError     = -32700
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

const (
	CodeUnauthorized = -32001
)

const Version = "2.0"

type Request struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id,omitempty"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

func NewRequest(id interface{}, method string, params interface{}) Request {
	return Request{JSONRPC: Version, ID: id, Method: method, Params: params}
}

func NewNotification(method string) Request {
	return Request{JSONRPC: Version, Method: method}
}

// Params is raw JSON, not interface{}, to avoid a lossy round-trip.
type ServerRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// ID intentionally omits `omitempty`: null IDs must serialize as "id": null
// per the JSON-RPC 2.0 spec for parse error responses.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

type Error struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

type CodedError struct {
	RPCCode int
	Err     error
}

func (e *CodedError) Error() string { return e.Err.Error() }
func (e *CodedError) Unwrap() error { return e.Err }

func NewCodedError(code int, err error) *CodedError {
	return &CodedError{RPCCode: code, Err: err}
}

func RespIDEquals(id interface{}, expected int64) bool {
	v, ok := RespIDToInt64(id)
	return ok && v == expected
}

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
