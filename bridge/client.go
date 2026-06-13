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

// CallTool sends a CallTool request and returns the raw JSON result,
// discarding any progress frames. Opens a fresh connection per call.
func (c *Client) CallTool(name string, args json.RawMessage) (json.RawMessage, error) {
	return c.CallToolStreaming(name, args, nil)
}

// CallToolStreaming is CallTool with progress: onProgress is invoked for each
// RespProgress frame received before the terminal result. A nil onProgress
// behaves exactly like CallTool. Opens a fresh connection per call.
func (c *Client) CallToolStreaming(name string, args json.RawMessage, onProgress func(ProgressUpdate)) (json.RawMessage, error) {
	resp, err := c.sendStreaming(BridgeRequest{
		Type:      ReqCallTool,
		Name:      name,
		Arguments: args,
		Token:     c.token,
	}, onProgress)
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

// ResolvePtyEnv asks relay for the env bundle (project-scoped token + working
// dir) needed to spawn a project-scoped PTY. Service-token authentication
// required. Skill generation is owned by relay and is not driven by this call.
func (c *Client) ResolvePtyEnv(req PtyEnvRequest) (PtyEnvResponse, error) {
	args, err := json.Marshal(req)
	if err != nil {
		return PtyEnvResponse{}, fmt.Errorf("marshal request: %w", err)
	}
	resp, err := c.send(BridgeRequest{
		Type:      ReqResolvePtyEnv,
		Arguments: args,
		Token:     c.token,
	})
	if err != nil {
		return PtyEnvResponse{}, fmt.Errorf("resolve pty env: %w", err)
	}
	if err := checkError(resp); err != nil {
		return PtyEnvResponse{}, err
	}
	var out PtyEnvResponse
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		return PtyEnvResponse{}, fmt.Errorf("parse response: %w", err)
	}
	return out, nil
}

// RegisterManifest tells relay where to reach this service and what it
// exposes. Called on startup after the service has picked + bound its
// own internal socket. Service-token authentication required.
// Re-registration with the same serviceID replaces the prior record.
func (c *Client) RegisterManifest(req RegisterManifestRequest) error {
	args, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	resp, err := c.send(BridgeRequest{
		Type:      ReqRegisterManifest,
		Arguments: args,
		Token:     c.token,
	})
	if err != nil {
		return fmt.Errorf("register manifest: %w", err)
	}
	return checkError(resp)
}

// sendAdmin sends an admin request to the bridge and returns any error.
func sendAdmin(reqType, name, token string) error {
	c := NewClient(token)
	resp, err := c.send(BridgeRequest{
		Type:  reqType,
		Name:  name,
		Token: c.token,
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

// bridgeTimeout bounds inactivity on a bridge round-trip: it caps connect +
// write + time-to-first-frame, and is reset on every frame received during a
// streaming call (see sendStreaming) so it acts as an idle timeout rather than
// a hard cap. Tool calls can take minutes (LLM inference, long-running tools)
// and stream progress throughout, so this is generous. A var (not const) so
// tests can shorten it to exercise the idle-reset behavior deterministically.
var bridgeTimeout = 10 * time.Minute

// send opens a connection, writes the request, reads one terminal response,
// and closes. Equivalent to sendStreaming with no progress handler.
func (c *Client) send(req BridgeRequest) (*BridgeResponse, error) {
	return c.sendStreaming(req, nil)
}

// sendStreaming opens a connection, writes the request, then reads frames
// until a terminal (non-progress) response. Each RespProgress frame is passed
// to onProgress (if non-nil) and reading continues. Sets a deadline so a call
// can't hang indefinitely if the tray app is unresponsive.
func (c *Client) sendStreaming(req BridgeRequest, onProgress func(ProgressUpdate)) (*BridgeResponse, error) {
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
	for scanner.Scan() {
		// Reset the deadline on every received frame so bridgeTimeout acts as an
		// inactivity timeout rather than a hard cap. A tool that legitimately
		// streams progress for longer than bridgeTimeout stays alive as long as
		// it keeps producing frames; a silent or hung peer is still cut off after
		// bridgeTimeout of no output.
		if err := conn.SetDeadline(time.Now().Add(bridgeTimeout)); err != nil {
			return nil, fmt.Errorf("reset deadline: %w", err)
		}
		var resp BridgeResponse
		if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
			return nil, fmt.Errorf("parse response failed: %w", err)
		}
		if resp.Type == RespProgress {
			if onProgress != nil && resp.Progress != nil {
				onProgress(*resp.Progress)
			}
			continue
		}
		return &resp, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read failed: %w", err)
	}
	return nil, fmt.Errorf("bridge closed connection")
}
