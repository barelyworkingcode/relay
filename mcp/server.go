package mcp

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"relaygo/bridge"
	"relaygo/jsonrpc"
)

// RunMCPServer runs the MCP stdio server, bridging JSON-RPC to the bridge client.
//
// tools/call requests are handled on their own goroutine so a long-running
// call (e.g. image generation, minutes) doesn't block other tool calls or the
// progress notifications it streams. All stdout writes go through a single
// mutex-guarded emit so concurrent responses/notifications never interleave.
// The handshake methods (initialize / tools/list / notifications) stay inline
// to preserve their natural ordering.
func RunMCPServer(token string) error {
	client := bridge.NewClient(token)

	scanner := bridge.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	var writeMu sync.Mutex
	emit := func(v interface{}) {
		writeMu.Lock()
		defer writeMu.Unlock()
		if err := encoder.Encode(v); err != nil {
			slog.Error("failed to write response", "error", err)
		}
	}

	var wg sync.WaitGroup
	for scanner.Scan() {
		// Copy: scanner reuses its buffer, and tools/call handling runs async.
		line := append([]byte(nil), scanner.Bytes()...)
		if len(line) == 0 {
			continue
		}

		var req jsonrpc.ServerRequest
		if err := json.Unmarshal(line, &req); err != nil {
			slog.Error("failed to parse request", "error", err)
			emit(rpcError(nil, jsonrpc.CodeParseError, "parse error: "+err.Error()))
			continue
		}

		if req.Method == MethodToolsCall && req.ID != nil {
			wg.Add(1)
			reqCopy := req
			go func() {
				defer wg.Done()
				handleToolsCall(client, &reqCopy, emit)
			}()
			continue
		}

		if resp := handleMethod(client, &req); resp != nil {
			emit(resp)
		}
	}

	wg.Wait()
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("stdin read error: %w", err)
	}
	return nil
}

// marshalResult converts an arbitrary value into json.RawMessage for a Response.
func marshalResult(v interface{}) (json.RawMessage, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal result: %w", err)
	}
	return json.RawMessage(data), nil
}

// jsonrpcVersion is the protocol version string for all responses.
const jsonrpcVersion = jsonrpc.Version

// rpcResult builds a success Response with the given result for the request ID.
// If marshaling fails, returns an internal error response instead.
func rpcResult(id interface{}, result json.RawMessage, err error) *jsonrpc.Response {
	if err != nil {
		return rpcError(id, jsonrpc.CodeInternalError, err.Error())
	}
	return &jsonrpc.Response{JSONRPC: jsonrpcVersion, ID: id, Result: result}
}

// rpcError builds an error Response with the given code and message for the request ID.
func rpcError(id interface{}, code int, msg string) *jsonrpc.Response {
	return &jsonrpc.Response{JSONRPC: jsonrpcVersion, ID: id, Error: &jsonrpc.Error{Code: code, Message: msg}}
}

func handleMethod(client *bridge.Client, req *jsonrpc.ServerRequest) *jsonrpc.Response {
	switch req.Method {
	case MethodInitialize:
		if req.ID == nil {
			return nil // notification — no response
		}
		return handleInitialize(req)
	case MethodInitialized:
		return nil
	case MethodToolsList:
		if req.ID == nil {
			return nil
		}
		return handleToolsList(client, req)
	case MethodToolsCall:
		// tools/call is routed to its own goroutine in RunMCPServer (so it can
		// stream progress). Reaching here means a malformed notification-form
		// tools/call (no ID) — nothing to reply to.
		return nil
	default:
		// JSON-RPC 2.0: servers MUST NOT reply to notifications (no ID).
		if req.ID == nil {
			return nil
		}
		return rpcError(req.ID, jsonrpc.CodeMethodNotFound, "method not found: "+req.Method)
	}
}

func handleInitialize(req *jsonrpc.ServerRequest) *jsonrpc.Response {
	data, err := marshalResult(map[string]interface{}{
		"protocolVersion": ProtocolVersion,
		"capabilities": map[string]interface{}{
			"tools": map[string]interface{}{},
		},
		"serverInfo": map[string]interface{}{
			"name":    "relay",
			"version": "1.0.0",
		},
	})
	return rpcResult(req.ID, data, err)
}

func handleToolsList(client *bridge.Client, req *jsonrpc.ServerRequest) *jsonrpc.Response {
	tools, err := client.ListTools()
	if err != nil {
		slog.Error("ListTools failed", "error", err)
		return rpcError(req.ID, jsonrpc.CodeInternalError, err.Error())
	}
	data, err := marshalResult(map[string]interface{}{
		"tools": json.RawMessage(tools),
	})
	return rpcResult(req.ID, data, err)
}

// handleToolsCall proxies a tools/call to the bridge and writes the result via
// emit. If the caller included _meta.progressToken, downstream progress is
// streamed back as notifications/progress referencing that same token.
func handleToolsCall(client *bridge.Client, req *jsonrpc.ServerRequest, emit func(interface{})) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      struct {
			ProgressToken interface{} `json:"progressToken"`
		} `json:"_meta"`
	}
	if req.Params != nil {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			emit(rpcError(req.ID, jsonrpc.CodeInvalidParams, "invalid params: "+err.Error()))
			return
		}
	}
	if params.Name == "" {
		emit(rpcError(req.ID, jsonrpc.CodeInvalidParams, "missing required parameter: name"))
		return
	}
	if params.Arguments == nil {
		params.Arguments = json.RawMessage("{}")
	}

	var onProgress func(bridge.ProgressUpdate)
	if token := params.Meta.ProgressToken; token != nil {
		onProgress = func(u bridge.ProgressUpdate) {
			emit(progressNotification(token, u))
		}
	}

	result, err := client.CallToolStreaming(params.Name, params.Arguments, onProgress)
	if err != nil {
		slog.Error("CallTool failed", "tool", params.Name, "error", err)
		emit(rpcError(req.ID, jsonrpc.CodeInternalError, err.Error()))
		return
	}
	emit(rpcResult(req.ID, json.RawMessage(result), nil))
}

// progressNotification builds an MCP notifications/progress message (no ID)
// referencing the caller's progressToken.
func progressNotification(token interface{}, u bridge.ProgressUpdate) jsonrpc.Request {
	return jsonrpc.Request{
		JSONRPC: jsonrpcVersion,
		Method:  MethodProgress,
		Params: map[string]interface{}{
			"progressToken": token,
			"progress":      u.Progress,
			"total":         u.Total,
			"message":       u.Message,
		},
	}
}
