package bridge

import (
	"encoding/json"
	"fmt"
	"net"
	"time"
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

// checkError returns an error if the bridge response is an error response.
func checkError(resp *BridgeResponse) error {
	if resp.Type == RespError {
		return fmt.Errorf("bridge error (code %d): %s", resp.Code, resp.Message)
	}
	return nil
}

// ListTools sends a ListTools request and returns the raw JSON tool array.
func (c *Client) ListTools() (json.RawMessage, error) {
	resp, err := c.send(BridgeRequest{
		Type:  ReqListTools,
		Token: c.token,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list tools: %w", err)
	}
	if err := checkError(resp); err != nil {
		return nil, err
	}
	return resp.Tools, nil
}

// CallTool sends a CallTool request and returns the raw JSON result.
// Opens a fresh connection per call, matching the Rust implementation.
func (c *Client) CallTool(name string, args json.RawMessage) (json.RawMessage, error) {
	resp, err := c.send(BridgeRequest{
		Type:      ReqCallTool,
		Name:      name,
		Arguments: args,
		Token:     c.token,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to call tool %q: %w", name, err)
	}
	if err := checkError(resp); err != nil {
		return nil, err
	}
	return resp.Result, nil
}

// ListProjects sends a ListProjects request and returns the raw JSON project array.
func (c *Client) ListProjects() (json.RawMessage, error) {
	resp, err := c.send(BridgeRequest{
		Type:  ReqListProjects,
		Token: c.token,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list projects: %w", err)
	}
	if err := checkError(resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// GetProject sends a GetProject request and returns the raw JSON project.
func (c *Client) GetProject(id string) (json.RawMessage, error) {
	resp, err := c.send(BridgeRequest{
		Type:      ReqGetProject,
		ProjectID: id,
		Token:     c.token,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get project %q: %w", id, err)
	}
	if err := checkError(resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// sendAdmin sends an admin request to the bridge and returns any error.
func sendAdmin(reqType, name, token string) error {
	c := NewClient(token)
	resp, err := c.send(BridgeRequest{
		Type:  reqType,
		Name:  name,
		Token: token,
	})
	if err != nil {
		return fmt.Errorf("%s request failed: %w", reqType, err)
	}
	return checkError(resp)
}

// SendReconcile sends a ReconcileExternalMcps request with admin authentication.
func SendReconcile(token string) error {
	return sendAdmin(ReqReconcileExternalMcps, "", token)
}

// SendReloadMcp sends a ReloadExternalMcp request for the given MCP ID.
func SendReloadMcp(id, token string) error {
	return sendAdmin(ReqReloadExternalMcp, id, token)
}

// SendReloadService sends a ReloadService request for the given service ID.
func SendReloadService(id, token string) error {
	return sendAdmin(ReqReloadService, id, token)
}

// bridgeTimeout is the maximum time for a complete bridge round-trip (connect + write + read).
// Tool calls can take minutes (LLM inference, long-running tools), so this is generous.
const bridgeTimeout = 10 * time.Minute

// send opens a connection, writes the request, reads one response, and closes.
// Sets a deadline to prevent hanging indefinitely if the tray app is unresponsive.
func (c *Client) send(req BridgeRequest) (*BridgeResponse, error) {
	conn, err := net.Dial("unix", c.sockPath)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to Relay bridge at %s: %w (is the Relay tray app running?)", c.sockPath, err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(bridgeTimeout)); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')

	if _, err := conn.Write(data); err != nil {
		return nil, fmt.Errorf("write failed: %w", err)
	}

	scanner := NewScanner(conn)
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
