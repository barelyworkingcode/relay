package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"relaygo/mcp"
)

// ExternalMcpManager manages connections to external stdio-based MCP servers.
type ExternalMcpManager struct {
	mu    sync.RWMutex
	conns map[string]*externalMcpConn
}

type externalMcpConn struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	tools  []mcp.Tool
	config ExternalMcp
	mu     sync.Mutex // serializes JSON-RPC requests
	nextID atomic.Int64
}

// jsonRPCRequest is an outgoing JSON-RPC 2.0 message.
type jsonRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      *int64      `json:"id,omitempty"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// jsonRPCResponse is an incoming JSON-RPC 2.0 message.
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// NewExternalMcpManager creates an empty manager.
func NewExternalMcpManager() *ExternalMcpManager {
	return &ExternalMcpManager{
		conns: make(map[string]*externalMcpConn),
	}
}

// StartAll launches all configured external MCP servers.
func (m *ExternalMcpManager) StartAll(mcps []ExternalMcp) {
	for i := range mcps {
		if err := m.startOne(&mcps[i]); err != nil {
			fmt.Fprintf(os.Stderr, "failed to start external MCP '%s': %v\n", mcps[i].ID, err)
		}
	}
}

func (m *ExternalMcpManager) startOne(mcpCfg *ExternalMcp) error {
	cmd := exec.Command(mcpCfg.Command, mcpCfg.Args...)
	for k, v := range mcpCfg.Env {
		cmd.Env = append(cmd.Environ(), k+"="+v)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn failed: %w", err)
	}

	conn := &externalMcpConn{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
		config: *mcpCfg,
	}
	conn.nextID.Store(1)

	// MCP handshake: initialize
	initParams := map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]interface{}{},
		"clientInfo": map[string]interface{}{
			"name":    "relay",
			"version": "1.0.0",
		},
	}
	initResp, err := conn.sendRequest("initialize", initParams)
	if err != nil {
		conn.kill()
		return fmt.Errorf("MCP handshake failed: %w", err)
	}

	// Extract contextSchema from initialize response.
	var contextSchema json.RawMessage
	if initResp != nil {
		var initResult struct {
			ServerInfo struct {
				ContextSchema json.RawMessage `json:"contextSchema,omitempty"`
			} `json:"serverInfo"`
		}
		if err := json.Unmarshal(initResp, &initResult); err == nil {
			if len(initResult.ServerInfo.ContextSchema) > 0 {
				contextSchema = initResult.ServerInfo.ContextSchema
			}
		}
	}

	// Send initialized notification (no id, no response expected).
	conn.sendNotification("notifications/initialized")

	// List tools.
	resp, err := conn.sendRequest("tools/list", nil)
	if err != nil {
		conn.kill()
		return fmt.Errorf("list_tools failed: %w", err)
	}

	var toolsResult struct {
		Tools []mcp.Tool `json:"tools"`
	}
	if err := json.Unmarshal(resp, &toolsResult); err != nil {
		conn.kill()
		return fmt.Errorf("parse tools response: %w", err)
	}
	conn.tools = toolsResult.Tools

	// Persist discovered tools and context schema so settings UI can display them.
	discoveredTools := make([]ToolInfo, 0, len(toolsResult.Tools))
	for _, t := range toolsResult.Tools {
		discoveredTools = append(discoveredTools, ToolInfo{
			Name:        t.Name,
			Description: t.Description,
			Category:    toolCategory(t),
		})
	}
	s := LoadSettings()
	s.UpdateDiscoveredTools(mcpCfg.ID, discoveredTools)
	s.UpdateContextSchema(mcpCfg.ID, contextSchema)

	m.mu.Lock()
	m.conns[mcpCfg.ID] = conn
	m.mu.Unlock()

	return nil
}

// Reconcile stops removed MCPs and starts missing ones.
func (m *ExternalMcpManager) Reconcile(mcps []ExternalMcp) {
	desired := make(map[string]*ExternalMcp, len(mcps))
	for i := range mcps {
		desired[mcps[i].ID] = &mcps[i]
	}

	// Stop removed.
	m.mu.RLock()
	var toStop []string
	for id := range m.conns {
		if _, ok := desired[id]; !ok {
			toStop = append(toStop, id)
		}
	}
	m.mu.RUnlock()

	for _, id := range toStop {
		m.Stop(id)
	}

	// Start missing.
	m.mu.RLock()
	var toStart []*ExternalMcp
	for _, mcpCfg := range mcps {
		if _, ok := m.conns[mcpCfg.ID]; !ok {
			cfg := mcpCfg
			toStart = append(toStart, &cfg)
		}
	}
	m.mu.RUnlock()

	for _, cfg := range toStart {
		if err := m.startOne(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "failed to start external MCP '%s': %v\n", cfg.ID, err)
		}
	}
}

// Tools returns the tool list for a given external MCP.
func (m *ExternalMcpManager) Tools(id string) []mcp.Tool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if conn, ok := m.conns[id]; ok {
		return conn.tools
	}
	return nil
}

// FindToolOwner returns the ID and config of the external MCP that owns the named tool.
func (m *ExternalMcpManager) FindToolOwner(toolName string) (string, *ExternalMcp) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for id, conn := range m.conns {
		for _, t := range conn.tools {
			if t.Name == toolName {
				cfg := conn.config
				return id, &cfg
			}
		}
	}
	return "", nil
}

// CallTool invokes a tool on the specified external MCP via JSON-RPC.
// If meta is non-nil, it is injected as _meta in the tool call params,
// enabling per-token context like allowed_dirs.
func (m *ExternalMcpManager) CallTool(id, name string, args json.RawMessage, meta json.RawMessage) (*mcp.CallToolResult, error) {
	m.mu.RLock()
	conn, ok := m.conns[id]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("external MCP '%s' not connected", id)
	}

	params := map[string]interface{}{
		"name": name,
	}
	if args != nil {
		var arguments interface{}
		if err := json.Unmarshal(args, &arguments); err == nil {
			params["arguments"] = arguments
		}
	}
	if meta != nil && len(meta) > 0 && string(meta) != "null" {
		var metaObj interface{}
		if err := json.Unmarshal(meta, &metaObj); err == nil {
			params["_meta"] = metaObj
		}
	}

	resp, err := conn.sendRequest("tools/call", params)
	if err != nil {
		return nil, fmt.Errorf("external MCP call failed: %w", err)
	}

	var result mcp.CallToolResult
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("parse call result: %w", err)
	}
	return &result, nil
}

// Stop kills and removes a specific external MCP connection.
func (m *ExternalMcpManager) Stop(id string) {
	m.mu.Lock()
	conn, ok := m.conns[id]
	if ok {
		delete(m.conns, id)
	}
	m.mu.Unlock()

	if ok {
		conn.kill()
	}
}

// StopAll kills all external MCP connections.
func (m *ExternalMcpManager) StopAll() {
	m.mu.Lock()
	conns := m.conns
	m.conns = make(map[string]*externalMcpConn)
	m.mu.Unlock()

	for _, conn := range conns {
		conn.kill()
	}
}

// DiscoverExternalMcp performs a one-shot spawn, handshake, tool listing, then kills.
func DiscoverExternalMcp(displayName, id, command string, args []string, env map[string]string) (*ExternalMcp, error) {
	cmd := exec.Command(command, args...)
	for k, v := range env {
		cmd.Env = append(cmd.Environ(), k+"="+v)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn failed: %w", err)
	}

	conn := &externalMcpConn{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
	}
	conn.nextID.Store(1)

	defer conn.kill()

	// Handshake with 30s timeout.
	type initResult struct {
		resp json.RawMessage
		err  error
	}
	initCh := make(chan initResult, 1)
	go func() {
		resp, err := conn.sendRequest("initialize", map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]interface{}{},
			"clientInfo": map[string]interface{}{
				"name":    "relay",
				"version": "1.0.0",
			},
		})
		initCh <- initResult{resp, err}
	}()

	var contextSchema json.RawMessage
	select {
	case r := <-initCh:
		if r.err != nil {
			return nil, fmt.Errorf("MCP handshake failed: %w", r.err)
		}
		// Extract contextSchema from initialize response.
		if r.resp != nil {
			var initParsed struct {
				ServerInfo struct {
					ContextSchema json.RawMessage `json:"contextSchema,omitempty"`
				} `json:"serverInfo"`
			}
			if err := json.Unmarshal(r.resp, &initParsed); err == nil {
				if len(initParsed.ServerInfo.ContextSchema) > 0 {
					contextSchema = initParsed.ServerInfo.ContextSchema
				}
			}
		}
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("MCP handshake timed out after 30s")
	}

	conn.sendNotification("notifications/initialized")

	// List tools with 15s timeout.
	type toolsResult struct {
		resp json.RawMessage
		err  error
	}
	toolsCh := make(chan toolsResult, 1)
	go func() {
		resp, err := conn.sendRequest("tools/list", nil)
		toolsCh <- toolsResult{resp, err}
	}()

	var toolsResp json.RawMessage
	select {
	case r := <-toolsCh:
		if r.err != nil {
			return nil, fmt.Errorf("list_tools failed: %w", r.err)
		}
		toolsResp = r.resp
	case <-time.After(15 * time.Second):
		return nil, fmt.Errorf("list_tools timed out after 15s")
	}

	var toolsList struct {
		Tools []mcp.Tool `json:"tools"`
	}
	if err := json.Unmarshal(toolsResp, &toolsList); err != nil {
		return nil, fmt.Errorf("parse tools: %w", err)
	}

	discoveredTools := make([]ToolInfo, 0, len(toolsList.Tools))
	for _, tool := range toolsList.Tools {
		discoveredTools = append(discoveredTools, ToolInfo{
			Name:        tool.Name,
			Description: tool.Description,
			Category:    toolCategory(tool),
		})
	}

	return &ExternalMcp{
		ID:              id,
		DisplayName:     displayName,
		Command:         command,
		Args:            args,
		Env:             env,
		DiscoveredTools: discoveredTools,
		ContextSchema:   contextSchema,
	}, nil
}

// sendRequest sends a JSON-RPC request and reads the response.
func (c *externalMcpConn) sendRequest(method string, params interface{}) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.nextID.Add(1) - 1
	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  method,
		Params:  params,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')

	if _, err := c.stdin.Write(data); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}

	// Read lines until we get a response matching our ID.
	for {
		line, err := c.stdout.ReadBytes('\n')
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}

		var resp jsonRPCResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			// Skip malformed lines (e.g. notifications from server).
			continue
		}

		// Skip notifications (no ID).
		if resp.ID == nil {
			continue
		}

		if *resp.ID == id {
			if resp.Error != nil {
				return nil, fmt.Errorf("JSON-RPC error %d: %s", resp.Error.Code, resp.Error.Message)
			}
			return resp.Result, nil
		}
	}
}

// sendNotification sends a JSON-RPC notification (no ID, no response expected).
func (c *externalMcpConn) sendNotification(method string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	req := jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  method,
	}
	data, _ := json.Marshal(req)
	data = append(data, '\n')
	_, _ = c.stdin.Write(data)
}

func (c *externalMcpConn) kill() {
	if c.stdin != nil {
		c.stdin.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
	}
}

// toolCategory returns the category for a tool, using the server-supplied
// value if present, otherwise deriving it from the tool name prefix (the
// part before the first underscore, title-cased).
func toolCategory(t mcp.Tool) string {
	if t.Category != "" {
		return t.Category
	}
	if idx := strings.IndexByte(t.Name, '_'); idx > 0 {
		prefix := t.Name[:idx]
		return strings.ToUpper(prefix[:1]) + strings.ToLower(prefix[1:])
	}
	return ""
}
