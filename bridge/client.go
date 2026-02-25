package bridge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
)

// Client connects to the bridge Unix socket to list and call tools.
type Client struct {
	sockPath string
	token    string
}

// NewClient creates a Client that will authenticate with the given token.
func NewClient(token string) *Client {
	return &Client{
		sockPath: SocketPath(),
		token:    token,
	}
}

// ListTools sends a ListTools request and returns the raw JSON tool array.
func (c *Client) ListTools() (json.RawMessage, error) {
	resp, err := c.send(BridgeRequest{
		Type:  "ListTools",
		Token: c.token,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list tools: %w", err)
	}
	if resp.Type == "Error" {
		return nil, fmt.Errorf("bridge error: %s", resp.Message)
	}
	return resp.Tools, nil
}

// CallTool sends a CallTool request and returns the raw JSON result.
// Opens a fresh connection per call, matching the Rust implementation.
func (c *Client) CallTool(name string, args json.RawMessage) (json.RawMessage, error) {
	resp, err := c.send(BridgeRequest{
		Type:      "CallTool",
		Name:      name,
		Arguments: args,
		Token:     c.token,
	})
	if err != nil {
		return nil, fmt.Errorf("bridge request failed: %w", err)
	}
	if resp.Type == "Error" {
		return nil, fmt.Errorf("bridge error (code %d): %s", resp.Code, resp.Message)
	}
	return resp.Result, nil
}

// SendReconcile sends a ReconcileExternalMcps request. No token needed.
func SendReconcile() error {
	c := &Client{sockPath: SocketPath()}
	resp, err := c.send(BridgeRequest{
		Type: "ReconcileExternalMcps",
	})
	if err != nil {
		return fmt.Errorf("reconcile request failed: %w", err)
	}
	if resp.Type == "Error" {
		return fmt.Errorf("bridge error: %s", resp.Message)
	}
	return nil
}

// SendReloadMcp sends a ReloadExternalMcp request for the given MCP ID. No token needed.
func SendReloadMcp(id string) error {
	c := &Client{sockPath: SocketPath()}
	resp, err := c.send(BridgeRequest{
		Type: "ReloadExternalMcp",
		Name: id,
	})
	if err != nil {
		return fmt.Errorf("reload request failed: %w", err)
	}
	if resp.Type == "Error" {
		return fmt.Errorf("bridge error: %s", resp.Message)
	}
	return nil
}

// send opens a connection, writes the request, reads one response, and closes.
func (c *Client) send(req BridgeRequest) (*BridgeResponse, error) {
	conn, err := net.Dial("unix", c.sockPath)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to Relay bridge at %s: %w (is the Relay tray app running?)", c.sockPath, err)
	}
	defer conn.Close()

	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')

	if _, err := conn.Write(data); err != nil {
		return nil, fmt.Errorf("write failed: %w", err)
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("read failed: %w", err)
		}
		return nil, fmt.Errorf("bridge closed connection")
	}

	var resp BridgeResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("parse response failed: %w", err)
	}
	return &resp, nil
}
