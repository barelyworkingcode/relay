package bridge

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"
)

type Client struct {
	sockPath string
	token    string
	cwd      string // sent only when token is empty; see BridgeRequest.Cwd
}

// NewClient falls back to directory auth when token is empty: it sends its
// working directory, which relay resolves against projects that opted in via
// AllowCwdAuth. The cwd is captured once here (the process doesn't chdir
// between calls) and is never sent alongside a token, so an authenticated
// call can't be re-scoped by the directory it happens to run from.
func NewClient(token string) *Client {
	c := &Client{
		sockPath: SocketPath(),
		token:    token,
	}
	if token == "" {
		c.cwd, _ = os.Getwd()
	}
	return c
}

func checkError(resp *BridgeResponse) error {
	if resp.Type == RespError {
		return fmt.Errorf("bridge error (code %d): %s", resp.Code, resp.Message)
	}
	return nil
}

func (c *Client) ListTools() (json.RawMessage, error) {
	resp, err := c.send(BridgeRequest{
		Type:  ReqListTools,
		Token: c.token,
		Cwd:   c.cwd,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list tools: %w", err)
	}
	if err := checkError(resp); err != nil {
		return nil, err
	}
	return resp.Tools, nil
}

func (c *Client) CallTool(name string, args json.RawMessage) (json.RawMessage, error) {
	return c.CallToolStreaming(name, args, nil)
}

// CallToolStreaming invokes onProgress for each RespProgress frame received
// before the terminal response; a nil onProgress behaves like CallTool.
func (c *Client) CallToolStreaming(name string, args json.RawMessage, onProgress func(ProgressUpdate)) (json.RawMessage, error) {
	resp, err := c.sendStreaming(BridgeRequest{
		Type:      ReqCallTool,
		Name:      name,
		Arguments: args,
		Token:     c.token,
		Cwd:       c.cwd,
	}, onProgress)
	if err != nil {
		return nil, fmt.Errorf("failed to call tool %q: %w", name, err)
	}
	if err := checkError(resp); err != nil {
		return nil, err
	}
	return resp.Result, nil
}

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

// DescribeProject requires a project token and answers for that token's own
// project only.
func (c *Client) DescribeProject() (ProjectDescription, error) {
	resp, err := c.send(BridgeRequest{
		Type:  ReqDescribeProject,
		Token: c.token,
	})
	if err != nil {
		return ProjectDescription{}, fmt.Errorf("describe project: %w", err)
	}
	if err := checkError(resp); err != nil {
		return ProjectDescription{}, err
	}
	var out ProjectDescription
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		return ProjectDescription{}, fmt.Errorf("parse response: %w", err)
	}
	return out, nil
}

// ResolvePtyEnv requires a launch identity holding the projects capability,
// so the client carries no token. Skill generation is
// owned by relay and is not driven by this call.
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

// ResolveProjectTemplate requires a launch identity holding the projects
// capability, so the client carries no token. The response
// carries only the template definition — never a token.
func (c *Client) ResolveProjectTemplate(req ShellTemplateRequest) (ShellTemplateResponse, error) {
	args, err := json.Marshal(req)
	if err != nil {
		return ShellTemplateResponse{}, fmt.Errorf("marshal request: %w", err)
	}
	resp, err := c.send(BridgeRequest{
		Type:      ReqResolveProjectTemplate,
		Arguments: args,
		Token:     c.token,
	})
	if err != nil {
		return ShellTemplateResponse{}, fmt.Errorf("resolve project template: %w", err)
	}
	if err := checkError(resp); err != nil {
		return ShellTemplateResponse{}, err
	}
	var out ShellTemplateResponse
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		return ShellTemplateResponse{}, fmt.Errorf("parse response: %w", err)
	}
	return out, nil
}

// RegisterManifest is called on startup after the service has picked + bound
// its own internal socket. It requires a launch identity holding the manifest
// capability, so the client carries no token.
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

// AdminOp sends one brokered admin operation by name. It carries no token
// and no cwd: brokered mutation is gated at the operation core the tray
// dispatches into, not by anything a caller can present at this 0600 socket
// (ADR-017 decision 2), so there is nothing here for either field to widen.
func (c *Client) AdminOp(op string, args json.RawMessage) (json.RawMessage, error) {
	resp, err := c.send(BridgeRequest{
		Type:      ReqAdminOp,
		Name:      op,
		Arguments: args,
	})
	if err != nil {
		return nil, fmt.Errorf("admin op %q failed: %w", op, err)
	}
	if err := checkError(resp); err != nil {
		return nil, err
	}
	return resp.Result, nil
}

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

func SendReconcile(token string) error {
	return sendAdmin(ReqReconcileExternalMcps, "", token)
}

func SendReloadMcp(id, token string) error {
	return sendAdmin(ReqReloadExternalMcp, id, token)
}

// bridgeTimeout bounds inactivity, not total call time: it is reset on every
// frame received during a streaming call (see sendStreaming), so a tool that
// legitimately streams progress for minutes stays alive. A var (not const) so
// tests can shorten it to exercise the idle-reset behavior deterministically.
var bridgeTimeout = 10 * time.Minute

func (c *Client) send(req BridgeRequest) (*BridgeResponse, error) {
	return c.sendStreaming(req, nil)
}

func (c *Client) sendStreaming(req BridgeRequest, onProgress func(ProgressUpdate)) (*BridgeResponse, error) {
	conn, err := net.Dial("unix", c.sockPath)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to Relay bridge at %s: %w (is the Relay tray app running?)", c.sockPath, err)
	}
	defer func() { _ = conn.Close() }()

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
		// Reset on every frame so bridgeTimeout is an inactivity timeout, not a
		// hard cap: a hung peer is still cut off, but a legitimately long call
		// stays alive as long as it keeps producing frames.
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
