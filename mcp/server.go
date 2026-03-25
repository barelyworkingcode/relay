package mcp

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"relaygo/bridge"
	"relaygo/jsonrpc"
)

// RunMCPServer runs the MCP stdio server, bridging JSON-RPC to the bridge client.
func RunMCPServer(token string) error {
	client := bridge.NewClient(token)

	scanner := bridge.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req jsonrpc.ServerRequest
		if err := json.Unmarshal(line, &req); err != nil {
			slog.Error("failed to parse request", "error", err)
			_ = encoder.Encode(rpcError(nil, jsonrpc.CodeParseError, "parse error: "+err.Error()))
			continue
		}

		resp := handleMethod(client, &req)
		if resp == nil {
			// Notification, no response needed.
			continue
		}
		if err := encoder.Encode(resp); err != nil {
			slog.Error("failed to write response", "error", err)
		}
	}

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
	case "initialize":
		if req.ID == nil {
			return nil // notification — no response
		}
		return handleInitialize(req)
	case "notifications/initialized":
		return nil
	case "tools/list":
		if req.ID == nil {
			return nil
		}
		return handleToolsList(client, req)
	case "tools/call":
		if req.ID == nil {
			return nil
		}
		return handleToolsCall(client, req)
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

func handleToolsCall(client *bridge.Client, req *jsonrpc.ServerRequest) *jsonrpc.Response {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if req.Params != nil {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return rpcError(req.ID, jsonrpc.CodeInvalidParams, "invalid params: "+err.Error())
		}
	}
	if params.Name == "" {
		return rpcError(req.ID, jsonrpc.CodeInvalidParams, "missing required parameter: name")
	}
	if params.Arguments == nil {
		params.Arguments = json.RawMessage("{}")
	}

	result, err := client.CallTool(params.Name, params.Arguments)
	if err != nil {
		slog.Error("CallTool failed", "tool", params.Name, "error", err)
		return rpcError(req.ID, jsonrpc.CodeInternalError, err.Error())
	}
	return rpcResult(req.ID, json.RawMessage(result), nil)
}
