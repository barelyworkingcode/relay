package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"relaygo/jsonrpc"
	"relaygo/mcp"
)

// mcpConnection abstracts a connection to an external MCP server (stdio or HTTP).
type mcpConnection interface {
	sendRequest(method string, params interface{}) (json.RawMessage, error)
	sendNotification(method string)
	close()
	getTools() []mcp.Tool
	getConfig() ExternalMcp
}

// ExternalMcpManager manages connections to external MCP servers.
type ExternalMcpManager struct {
	mu    sync.RWMutex
	conns map[string]mcpConnection
}

type externalMcpConn struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	tools  []mcp.Tool
	config ExternalMcp
	mu     sync.Mutex // serializes JSON-RPC requests
	nextID int64
}

// handshakeResult holds the results of an MCP initialize + tools/list sequence.
type handshakeResult struct {
	Tools         []mcp.Tool
	ToolInfos     []ToolInfo
	ContextSchema json.RawMessage
}

// mcpHandshake performs the MCP initialize -> notifications/initialized -> tools/list
// sequence on any mcpConnection. Transport-agnostic.
func mcpHandshake(conn mcpConnection) (*handshakeResult, error) {
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
		return nil, fmt.Errorf("MCP handshake failed: %w", err)
	}

	contextSchema := extractContextSchema(initResp)
	conn.sendNotification("notifications/initialized")

	resp, err := conn.sendRequest("tools/list", nil)
	if err != nil {
		return nil, fmt.Errorf("tools/list failed: %w", err)
	}

	var toolsResult struct {
		Tools []mcp.Tool `json:"tools"`
	}
	if err := json.Unmarshal(resp, &toolsResult); err != nil {
		return nil, fmt.Errorf("parse tools: %w", err)
	}

	toolInfos := make([]ToolInfo, 0, len(toolsResult.Tools))
	for _, t := range toolsResult.Tools {
		toolInfos = append(toolInfos, ToolInfo{
			Name:        t.Name,
			Description: t.Description,
			Category:    toolCategory(t),
		})
	}

	return &handshakeResult{
		Tools:         toolsResult.Tools,
		ToolInfos:     toolInfos,
		ContextSchema: contextSchema,
	}, nil
}

// extractContextSchema pulls the contextSchema from an initialize response.
func extractContextSchema(initResp json.RawMessage) json.RawMessage {
	if initResp == nil {
		return nil
	}
	var result struct {
		ServerInfo struct {
			ContextSchema json.RawMessage `json:"contextSchema,omitempty"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(initResp, &result); err == nil && len(result.ServerInfo.ContextSchema) > 0 {
		return result.ServerInfo.ContextSchema
	}
	return nil
}

// NewExternalMcpManager creates an empty manager.
func NewExternalMcpManager() *ExternalMcpManager {
	return &ExternalMcpManager{
		conns: make(map[string]mcpConnection),
	}
}

// StartAll launches all configured external MCP servers.
func (m *ExternalMcpManager) StartAll(mcps []ExternalMcp) {
	for i := range mcps {
		if err := m.startOne(&mcps[i]); err != nil {
			slog.Error("failed to start external MCP", "id", mcps[i].ID, "error", err)
		}
	}
}

func (m *ExternalMcpManager) startOne(mcpCfg *ExternalMcp) error {
	if mcpCfg.IsHTTP() {
		return m.startHTTP(mcpCfg)
	}
	return m.startStdio(mcpCfg)
}

func (m *ExternalMcpManager) startStdio(mcpCfg *ExternalMcp) error {
	cmd := exec.Command(mcpCfg.Command, mcpCfg.Args...)
	setProcessGroup(cmd)
	if len(mcpCfg.Env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range mcpCfg.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
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
		nextID: 1,
	}

	result, err := mcpHandshake(conn)
	if err != nil {
		conn.close()
		return err
	}
	conn.tools = result.Tools

	WithSettings(func(s *Settings) {
		s.UpdateDiscoveredTools(mcpCfg.ID, result.ToolInfos)
		s.UpdateContextSchema(mcpCfg.ID, result.ContextSchema)
	})

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
			slog.Error("failed to start external MCP", "id", cfg.ID, "error", err)
		}
	}
}

// Reload stops a running MCP and starts it fresh from the given config.
func (m *ExternalMcpManager) Reload(id string, cfg *ExternalMcp) error {
	m.Stop(id)
	return m.startOne(cfg)
}

// Tools returns the tool list for a given external MCP.
func (m *ExternalMcpManager) Tools(id string) []mcp.Tool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if conn, ok := m.conns[id]; ok {
		return conn.getTools()
	}
	return nil
}

// FindToolOwner returns the ID and config of the external MCP that owns the named tool.
func (m *ExternalMcpManager) FindToolOwner(toolName string) (string, *ExternalMcp) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for id, conn := range m.conns {
		for _, t := range conn.getTools() {
			if t.Name == toolName {
				cfg := conn.getConfig()
				return id, &cfg
			}
		}
	}
	return "", nil
}

// CallTool invokes a tool on the specified external MCP via JSON-RPC.
// If meta is non-nil, it is injected as _meta in the tool call params,
// enabling per-token context like allowed_dirs.
func (m *ExternalMcpManager) CallTool(id, name string, args json.RawMessage, meta json.RawMessage) (json.RawMessage, error) {
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

	return resp, nil
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
		conn.close()
	}
}

// StopAll kills all external MCP connections.
func (m *ExternalMcpManager) StopAll() {
	m.mu.Lock()
	conns := m.conns
	m.conns = make(map[string]mcpConnection)
	m.mu.Unlock()

	for _, conn := range conns {
		conn.close()
	}
}

// DiscoverExternalMcp performs a one-shot spawn, handshake, tool listing, then kills.
func DiscoverExternalMcp(displayName, id, command string, args []string, env map[string]string) (*ExternalMcp, error) {
	cmd := exec.Command(command, args...)
	setProcessGroup(cmd)
	if len(env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
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
		nextID: 1,
	}

	defer conn.close()

	// Run handshake with overall timeout (stdio processes can hang).
	type hsOut struct {
		result *handshakeResult
		err    error
	}
	ch := make(chan hsOut, 1)
	go func() {
		r, err := mcpHandshake(conn)
		ch <- hsOut{r, err}
	}()

	select {
	case out := <-ch:
		if out.err != nil {
			return nil, out.err
		}
		return &ExternalMcp{
			ID:              id,
			DisplayName:     displayName,
			Command:         command,
			Args:            args,
			Env:             env,
			DiscoveredTools: out.result.ToolInfos,
			ContextSchema:   out.result.ContextSchema,
		}, nil
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("MCP discovery timed out after 30s")
	}
}

// sendRequest sends a JSON-RPC request and reads the response.
func (c *externalMcpConn) sendRequest(method string, params interface{}) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.nextID
	c.nextID++
	req := jsonrpc.Request{
		JSONRPC: "2.0",
		ID:      id,
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

		var resp jsonrpc.Response
		if err := json.Unmarshal(line, &resp); err != nil {
			// Skip malformed lines (e.g. notifications from server).
			continue
		}

		// Skip notifications (no ID).
		if resp.ID == nil {
			continue
		}

		if jsonrpc.RespIDEquals(resp.ID, id) {
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

	req := jsonrpc.Request{
		JSONRPC: "2.0",
		Method:  method,
	}
	data, _ := json.Marshal(req)
	data = append(data, '\n')
	_, _ = c.stdin.Write(data)
}

func (c *externalMcpConn) close() {
	if c.stdin != nil {
		c.stdin.Close()
	}
	if c.cmd != nil {
		killProcessGroup(c.cmd)
		_ = c.cmd.Wait()
	}
}

func (c *externalMcpConn) getTools() []mcp.Tool   { return c.tools }
func (c *externalMcpConn) getConfig() ExternalMcp { return c.config }

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
