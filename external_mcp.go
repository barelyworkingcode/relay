package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"relaygo/jsonrpc"
	"relaygo/mcp"
)

// mcpRequestTimeout is the maximum time to wait for a JSON-RPC response from any MCP.
const mcpRequestTimeout = 5 * time.Minute

// discoveryTimeout is the maximum time for a one-shot MCP discovery handshake.
const discoveryTimeout = 30 * time.Second

// McpConnection abstracts a connection to an external MCP server (stdio or HTTP).
type McpConnection interface {
	SendRequest(ctx context.Context, method string, params interface{}) (json.RawMessage, error)
	SendNotification(method string)
	Close()
	GetTools() []mcp.Tool
	SetTools([]mcp.Tool)
	GetConfig() ExternalMcp
}

// OnDiscoverFunc is called after a successful MCP handshake with discovered tools and schema.
// Injected at ExternalMcpManager construction to decouple the manager from Settings persistence.
type OnDiscoverFunc func(id string, tools []ToolInfo, schema json.RawMessage)

// OnTokenRefreshFunc is called when an HTTP MCP refreshes its OAuth token.
// Injected at ExternalMcpManager construction to decouple from Settings persistence.
type OnTokenRefreshFunc func(mcpID string, oauth *OAuthState)

// ExternalMcpManager manages connections to external MCP servers.
type ExternalMcpManager struct {
	mu             sync.RWMutex
	conns          map[string]McpConnection
	onDiscover     OnDiscoverFunc
	onTokenRefresh OnTokenRefreshFunc
}

// pendingResponse holds a channel for delivering a JSON-RPC response to a waiting caller.
type pendingResponse struct {
	ch chan readerResult
}

// readerResult is the value delivered to a pending request's channel.
type readerResult struct {
	resp jsonrpc.Response
	err  error
}

// baseMcpConn holds fields and methods shared by stdio and HTTP MCP connections.
type baseMcpConn struct {
	nextID  atomic.Int64
	toolsMu sync.RWMutex // protects tools
	tools   []mcp.Tool
	config  ExternalMcp
}

func (b *baseMcpConn) allocID() int64 {
	return b.nextID.Add(1)
}

func (b *baseMcpConn) GetTools() []mcp.Tool {
	b.toolsMu.RLock()
	defer b.toolsMu.RUnlock()
	return b.tools
}

func (b *baseMcpConn) SetTools(tools []mcp.Tool) {
	b.toolsMu.Lock()
	defer b.toolsMu.Unlock()
	b.tools = tools
}

func (b *baseMcpConn) GetConfig() ExternalMcp { return b.config }

type externalMcpConn struct {
	baseMcpConn
	cmd   *exec.Cmd
	stdin io.WriteCloser

	mu      sync.Mutex // protects writes to stdin and pending map
	pending map[int64]*pendingResponse

	readerDone chan struct{} // closed when the reader goroutine exits
	readerErr  error        // set before readerDone is closed
	closeOnce  sync.Once    // ensures Close is idempotent
}

// handshakeResult holds the results of an MCP initialize + tools/list sequence.
type handshakeResult struct {
	Tools         []mcp.Tool
	ToolInfos     []ToolInfo
	ContextSchema json.RawMessage
}

// mcpHandshake performs the MCP initialize -> notifications/initialized -> tools/list
// sequence on any mcpConnection. Transport-agnostic.
func mcpHandshake(ctx context.Context, conn McpConnection) (*handshakeResult, error) {
	initParams := map[string]interface{}{
		"protocolVersion": mcp.ProtocolVersion,
		"capabilities":    map[string]interface{}{},
		"clientInfo": map[string]interface{}{
			"name":    "relay",
			"version": "1.0.0",
		},
	}
	initResp, err := conn.SendRequest(ctx, "initialize", initParams)
	if err != nil {
		return nil, fmt.Errorf("MCP handshake failed: %w", err)
	}

	contextSchema := extractContextSchema(initResp)
	conn.SendNotification("notifications/initialized")

	resp, err := conn.SendRequest(ctx, "tools/list", nil)
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

// NewExternalMcpManager creates a manager with injected callbacks for discovery
// and token refresh persistence. This keeps the manager decoupled from Settings.
func NewExternalMcpManager(onDiscover OnDiscoverFunc, onTokenRefresh OnTokenRefreshFunc) *ExternalMcpManager {
	return &ExternalMcpManager{
		conns:          make(map[string]McpConnection),
		onDiscover:     onDiscover,
		onTokenRefresh: onTokenRefresh,
	}
}

// setConnection stores a connection, closing any existing connection for the same ID
// to prevent resource leaks during concurrent operations or reconnections.
func (m *ExternalMcpManager) setConnection(id string, conn McpConnection) {
	m.mu.Lock()
	old := m.conns[id]
	m.conns[id] = conn
	m.mu.Unlock()
	if old != nil {
		old.Close()
	}
}

// finalizeConnection completes MCP startup after a successful handshake:
// sets discovered tools on the connection, persists discovery results, and
// stores the connection in the manager.
func (m *ExternalMcpManager) finalizeConnection(id string, conn McpConnection, result *handshakeResult) {
	conn.SetTools(result.Tools)
	if m.onDiscover != nil {
		m.onDiscover(id, result.ToolInfos, result.ContextSchema)
	}
	m.setConnection(id, conn)
	slog.Info("MCP connected", "id", id, "tools", len(result.Tools))
}

// StartAll launches all configured external MCP servers concurrently.
// Each MCP handshake involves network I/O, so parallel startup avoids
// linear growth in startup time as MCPs are added.
func (m *ExternalMcpManager) StartAll(ctx context.Context, mcps []ExternalMcp) {
	var wg sync.WaitGroup
	for i := range mcps {
		wg.Add(1)
		go func(cfg *ExternalMcp) {
			defer wg.Done()
			if err := m.startOne(ctx, cfg); err != nil {
				logMcpStartError(cfg.ID, err)
			}
		}(&mcps[i])
	}
	wg.Wait()
}

// logMcpStartError logs an MCP startup error at the appropriate level.
// ErrAuthRequired is expected for HTTP MCPs that need OAuth — log at Info.
// All other errors are genuine failures — log at Error.
func logMcpStartError(id string, err error) {
	if errors.Is(err, ErrAuthRequired) {
		slog.Info("external MCP requires authentication", "id", id)
	} else {
		slog.Error("failed to start external MCP", "id", id, "error", err)
	}
}

func (m *ExternalMcpManager) startOne(ctx context.Context, mcpCfg *ExternalMcp) error {
	// Don't spawn processes if the context is already cancelled (e.g., during shutdown).
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := mcpCfg.Validate(); err != nil {
		return fmt.Errorf("invalid MCP config: %w", err)
	}
	if mcpCfg.IsHTTP() {
		return m.startHTTP(ctx, mcpCfg)
	}
	return m.startStdio(ctx, mcpCfg)
}

// spawnStdioConn creates and starts a stdio MCP connection.
// The caller is responsible for calling Close() on error or when done.
func spawnStdioConn(command string, args []string, env map[string]string, config *ExternalMcp) (*externalMcpConn, error) {
	cmd := exec.Command(command, args...)
	setProcessGroup(cmd)
	mergeEnv(cmd, env)

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
		cmd:        cmd,
		stdin:      stdin,
		pending:    make(map[int64]*pendingResponse),
		readerDone: make(chan struct{}),
	}
	if config != nil {
		conn.config = *config
	}

	go conn.readLoop(bufio.NewReader(stdout))
	return conn, nil
}

func (m *ExternalMcpManager) startStdio(ctx context.Context, mcpCfg *ExternalMcp) error {
	conn, err := spawnStdioConn(mcpCfg.Command, mcpCfg.Args, mcpCfg.Env, mcpCfg)
	if err != nil {
		return err
	}

	result, err := mcpHandshake(ctx, conn)
	if err != nil {
		conn.Close()
		return err
	}

	m.finalizeConnection(mcpCfg.ID, conn, result)
	return nil
}

// Reconcile stops removed MCPs and starts missing ones.
func (m *ExternalMcpManager) Reconcile(ctx context.Context, mcps []ExternalMcp) {
	desired := make(map[string]*ExternalMcp, len(mcps))
	for i := range mcps {
		desired[mcps[i].ID] = &mcps[i]
	}

	// Compute both toStop and toStart in a single critical section to avoid
	// TOCTOU issues between separate lock acquisitions.
	m.mu.RLock()
	var toStop []string
	for id := range m.conns {
		if _, ok := desired[id]; !ok {
			toStop = append(toStop, id)
		}
	}
	var toStart []*ExternalMcp
	for _, mcpCfg := range mcps {
		if _, ok := m.conns[mcpCfg.ID]; !ok {
			cfg := mcpCfg
			toStart = append(toStart, &cfg)
		}
	}
	m.mu.RUnlock()

	for _, id := range toStop {
		m.Stop(id)
	}

	// Start new MCPs concurrently, matching StartAll behavior.
	var wg sync.WaitGroup
	for _, cfg := range toStart {
		wg.Add(1)
		go func(c *ExternalMcp) {
			defer wg.Done()
			if err := m.startOne(ctx, c); err != nil {
				logMcpStartError(c.ID, err)
			}
		}(cfg)
	}
	wg.Wait()
}

// Reload stops a running MCP and starts it fresh from the given config.
func (m *ExternalMcpManager) Reload(ctx context.Context, id string, cfg *ExternalMcp) error {
	m.Stop(id)
	return m.startOne(ctx, cfg)
}

// Tools returns the tool list for a given external MCP.
func (m *ExternalMcpManager) Tools(id string) []mcp.Tool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if conn, ok := m.conns[id]; ok {
		return conn.GetTools()
	}
	return nil
}

// FindToolOwner returns the ID and config of the external MCP that owns the named tool.
func (m *ExternalMcpManager) FindToolOwner(toolName string) (string, *ExternalMcp) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for id, conn := range m.conns {
		for _, t := range conn.GetTools() {
			if t.Name == toolName {
				cfg := conn.GetConfig()
				return id, &cfg
			}
		}
	}
	return "", nil
}

// CallTool invokes a tool on the specified external MCP via JSON-RPC.
// If meta is non-nil, it is injected as _meta in the tool call params,
// enabling per-token context like allowed_dirs.
func (m *ExternalMcpManager) CallTool(ctx context.Context, id, name string, args json.RawMessage, meta json.RawMessage) (json.RawMessage, error) {
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
		if err := json.Unmarshal(args, &arguments); err != nil {
			return nil, fmt.Errorf("invalid tool arguments: %w", err)
		}
		params["arguments"] = arguments
	}
	if len(meta) > 0 && string(meta) != "null" {
		var metaObj interface{}
		if err := json.Unmarshal(meta, &metaObj); err != nil {
			return nil, fmt.Errorf("invalid tool context metadata: %w", err)
		}
		params["_meta"] = metaObj
	}

	resp, err := conn.SendRequest(ctx, "tools/call", params)
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
		conn.Close()
	}
}

// StopAll kills all external MCP connections concurrently to avoid one
// slow connection (e.g., HTTP session DELETE) blocking the shutdown of others.
func (m *ExternalMcpManager) StopAll() {
	m.mu.Lock()
	conns := m.conns
	m.conns = make(map[string]McpConnection)
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, conn := range conns {
		wg.Add(1)
		go func(c McpConnection) {
			defer wg.Done()
			c.Close()
		}(conn)
	}
	wg.Wait()
}

// discoverMcp performs a handshake on an already-connected McpConnection and
// populates the given ExternalMcp config with discovered tools and context schema.
// Shared by both stdio and HTTP discovery paths.
func discoverMcp(ctx context.Context, conn McpConnection, base ExternalMcp) (*ExternalMcp, error) {
	result, err := mcpHandshake(ctx, conn)
	if err != nil {
		return nil, err
	}
	base.DiscoveredTools = result.ToolInfos
	base.ContextSchema = result.ContextSchema
	return &base, nil
}

// DiscoverExternalMcp performs a one-shot spawn, handshake, tool listing, then kills.
func DiscoverExternalMcp(ctx context.Context, displayName, id, command string, args []string, env map[string]string) (*ExternalMcp, error) {
	conn, err := spawnStdioConn(command, args, env, nil)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// Run handshake with overall timeout (stdio processes can hang).
	discoverCtx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	return discoverMcp(discoverCtx, conn, ExternalMcp{
		ID:          id,
		DisplayName: displayName,
		Command:     command,
		Args:        args,
		Env:         env,
	})
}

// ---------------------------------------------------------------------------
// stdio connection implementation
// ---------------------------------------------------------------------------

// readLoop reads JSON-RPC responses from stdout and dispatches them to waiting callers.
// Runs in its own goroutine for the lifetime of the connection.
func (c *externalMcpConn) readLoop(reader *bufio.Reader) {
	defer close(c.readerDone)

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			c.mu.Lock()
			c.readerErr = fmt.Errorf("read response: %w", err)
			// Signal all pending requests that the reader is dead.
			for id, p := range c.pending {
				p.ch <- readerResult{err: c.readerErr}
				delete(c.pending, id)
			}
			c.mu.Unlock()
			return
		}

		var resp jsonrpc.Response
		if err := json.Unmarshal(line, &resp); err != nil {
			slog.Debug("stdio MCP: skipping malformed response line", "error", err)
			continue
		}
		if resp.ID == nil {
			continue // skip notifications
		}

		respID, ok := jsonrpc.RespIDToInt64(resp.ID)
		if !ok {
			slog.Debug("stdio MCP: skipping response with non-numeric ID", "id", resp.ID)
			continue
		}

		c.mu.Lock()
		p, exists := c.pending[respID]
		if exists {
			delete(c.pending, respID)
		}
		c.mu.Unlock()

		if exists {
			p.ch <- readerResult{resp: resp}
		}
	}
}

// SendRequest sends a JSON-RPC request and waits for the response with a timeout.
func (c *externalMcpConn) SendRequest(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	c.mu.Lock()

	// Check if reader is already dead.
	select {
	case <-c.readerDone:
		c.mu.Unlock()
		if c.readerErr != nil {
			return nil, c.readerErr
		}
		return nil, fmt.Errorf("connection closed")
	default:
	}

	id := c.allocID()
	data, err := json.Marshal(jsonrpc.NewRequest(id, method, params))
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	data = append(data, '\n')

	p := &pendingResponse{ch: make(chan readerResult, 1)}
	c.pending[id] = p

	if _, err := c.stdin.Write(data); err != nil {
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("write request: %w", err)
	}
	c.mu.Unlock()

	// Wait for response, reader death, context cancellation, or timeout.
	timer := time.NewTimer(mcpRequestTimeout)
	defer timer.Stop()

	select {
	case result := <-p.ch:
		if result.err != nil {
			return nil, result.err
		}
		if result.resp.Error != nil {
			return nil, formatJSONRPCError(result.resp.Error)
		}
		return result.resp.Result, nil
	case <-c.readerDone:
		if c.readerErr != nil {
			return nil, c.readerErr
		}
		return nil, fmt.Errorf("connection closed")
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case <-timer.C:
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("request timed out after %s", mcpRequestTimeout)
	}
}

// SendNotification sends a JSON-RPC notification (no ID, no response expected).
func (c *externalMcpConn) SendNotification(method string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	data, err := json.Marshal(jsonrpc.NewNotification(method))
	if err != nil {
		slog.Debug("stdio MCP: failed to marshal notification", "method", method, "error", err)
		return
	}
	data = append(data, '\n')
	if _, err := c.stdin.Write(data); err != nil {
		slog.Debug("stdio MCP: failed to write notification", "method", method, "error", err)
	}
}

func (c *externalMcpConn) Close() {
	c.closeOnce.Do(func() {
		if c.stdin != nil {
			c.stdin.Close()
		}
		if c.cmd != nil {
			killProcessGroup(c.cmd)
			_ = c.cmd.Wait()
		}
		// Wait for readLoop to finish so no goroutine is leaked and all pending
		// requests are drained before the connection is considered closed.
		<-c.readerDone
	})
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
