package mcp

import (
	"bufio"
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

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	encoder := json.NewEncoder(os.Stdout)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req jsonrpc.ServerRequest
		if err := json.Unmarshal(line, &req); err != nil {
			slog.Error("failed to parse request", "error", err)
			resp := jsonrpc.Response{
				JSONRPC: "2.0",
				Error: &jsonrpc.Error{
					Code:    -32700,
					Message: "parse error: " + err.Error(),
				},
			}
			_ = encoder.Encode(resp)
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
func marshalResult(v interface{}) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		slog.Error("marshalResult failed", "error", err)
		return json.RawMessage("null")
	}
	return json.RawMessage(data)
}

func handleMethod(client *bridge.Client, req *jsonrpc.ServerRequest) *jsonrpc.Response {
	switch req.Method {
	case "initialize":
		return &jsonrpc.Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: marshalResult(map[string]interface{}{
				"protocolVersion": "2024-11-05",
				"capabilities": map[string]interface{}{
					"tools": map[string]interface{}{},
				},
				"serverInfo": map[string]interface{}{
					"name":    "relay",
					"version": "1.0.0",
				},
			}),
		}

	case "notifications/initialized":
		// Notification, no response.
		return nil

	case "tools/list":
		tools, err := client.ListTools()
		if err != nil {
			slog.Error("ListTools failed", "error", err)
			return &jsonrpc.Response{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error: &jsonrpc.Error{
					Code:    -32603,
					Message: err.Error(),
				},
			}
		}
		return &jsonrpc.Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: marshalResult(map[string]interface{}{
				"tools": json.RawMessage(tools),
			}),
		}

	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if req.Params != nil {
			if err := json.Unmarshal(req.Params, &params); err != nil {
				return &jsonrpc.Response{
					JSONRPC: "2.0",
					ID:      req.ID,
					Error: &jsonrpc.Error{
						Code:    -32602,
						Message: "invalid params: " + err.Error(),
					},
				}
			}
		}
		if params.Arguments == nil {
			params.Arguments = json.RawMessage("{}")
		}

		result, err := client.CallTool(params.Name, params.Arguments)
		if err != nil {
			slog.Error("CallTool failed", "tool", params.Name, "error", err)
			return &jsonrpc.Response{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error: &jsonrpc.Error{
					Code:    -32603,
					Message: err.Error(),
				},
			}
		}
		return &jsonrpc.Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  json.RawMessage(result),
		}

	default:
		return &jsonrpc.Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &jsonrpc.Error{
				Code:    -32601,
				Message: "method not found: " + req.Method,
			},
		}
	}
}
