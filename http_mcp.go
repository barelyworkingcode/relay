package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"relaygo/jsonrpc"
	"relaygo/mcp"
)

// ErrAuthRequired indicates the HTTP MCP server returned 401.
var ErrAuthRequired = errors.New("authentication required (HTTP 401)")

// httpMcpConn implements mcpConnection for Streamable HTTP transport.
type httpMcpConn struct {
	url          string
	sessionID    string
	accessToken  string
	httpClient   *http.Client
	mu           sync.Mutex
	nextID       int64
	tools        []mcp.Tool
	config       ExternalMcp

	// OAuth refresh fields.
	oauthMeta    *oauthMetadata
	refreshToken string
	clientID     string
	clientSecret string
	tokenExpiry  time.Time

	// Callback to persist refreshed tokens.
	onTokenRefresh func(oauth *OAuthState)
}

func newHTTPMcpConn(cfg ExternalMcp) *httpMcpConn {
	conn := &httpMcpConn{
		url:    cfg.URL,
		config: cfg,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
	conn.nextID = 1

	if cfg.OAuthState != nil {
		conn.accessToken = cfg.OAuthState.AccessToken
		conn.refreshToken = cfg.OAuthState.RefreshToken
		conn.clientID = cfg.OAuthState.ClientID
		conn.clientSecret = cfg.OAuthState.ClientSecret
		if cfg.OAuthState.TokenExpiry != "" {
			if t, err := time.Parse(time.RFC3339, cfg.OAuthState.TokenExpiry); err == nil {
				conn.tokenExpiry = t
			}
		}
	}

	return conn
}

// refreshIfNeeded checks token expiry and refreshes if necessary.
func (c *httpMcpConn) refreshIfNeeded() error {
	if c.refreshToken == "" || c.tokenExpiry.IsZero() {
		return nil
	}
	// Refresh if within 30 seconds of expiry.
	if time.Now().Before(c.tokenExpiry.Add(-30 * time.Second)) {
		return nil
	}

	meta := c.oauthMeta
	if meta == nil {
		var err error
		discovery, err := discoverOAuth(c.url)
		if err != nil {
			return fmt.Errorf("discover OAuth metadata for refresh: %w", err)
		}
		meta = discovery.Metadata
		c.oauthMeta = meta
	}

	tokenResp, err := refreshAccessToken(meta, c.refreshToken, c.clientID, c.clientSecret)
	if err != nil {
		return fmt.Errorf("token refresh: %w", err)
	}

	c.accessToken = tokenResp.AccessToken
	if tokenResp.RefreshToken != "" {
		c.refreshToken = tokenResp.RefreshToken
	}
	if tokenResp.ExpiresIn > 0 {
		c.tokenExpiry = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	}

	if c.onTokenRefresh != nil {
		c.onTokenRefresh(&OAuthState{
			ClientID:     c.clientID,
			ClientSecret: c.clientSecret,
			AccessToken:  c.accessToken,
			RefreshToken: c.refreshToken,
			TokenExpiry:  c.tokenExpiry.UTC().Format(time.RFC3339),
		})
	}

	return nil
}

func (c *httpMcpConn) sendRequest(method string, params interface{}) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.refreshIfNeeded(); err != nil {
		return nil, err
	}

	id := c.nextID
	c.nextID++
	req := jsonrpc.Request{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequest("POST", c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create HTTP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if c.accessToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.accessToken)
	}
	if c.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", c.sessionID)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrAuthRequired
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	// Capture session ID from response.
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.sessionID = sid
	}

	contentType := resp.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "text/event-stream") {
		return c.parseSSEResponse(resp.Body, id)
	}

	// Direct JSON response.
	var rpcResp jsonrpc.Response
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return nil, fmt.Errorf("parse JSON-RPC response: %w", err)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("JSON-RPC error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	return rpcResp.Result, nil
}

// parseSSEResponse reads SSE data lines and extracts the JSON-RPC response matching our ID.
func (c *httpMcpConn) parseSSEResponse(reader io.Reader, expectedID int64) (json.RawMessage, error) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}

		var rpcResp jsonrpc.Response
		if err := json.Unmarshal([]byte(data), &rpcResp); err != nil {
			// Skip malformed SSE data lines.
			continue
		}

		// Skip notifications (no ID).
		if rpcResp.ID == nil {
			continue
		}

		if jsonrpc.RespIDEquals(rpcResp.ID, expectedID) {
			if rpcResp.Error != nil {
				return nil, fmt.Errorf("JSON-RPC error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
			}
			return rpcResp.Result, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("SSE read error: %w", err)
	}
	return nil, fmt.Errorf("SSE stream ended without matching response")
}

func (c *httpMcpConn) sendNotification(method string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	req := jsonrpc.Request{
		JSONRPC: "2.0",
		Method:  method,
	}
	body, _ := json.Marshal(req)

	httpReq, err := http.NewRequest("POST", c.url, bytes.NewReader(body))
	if err != nil {
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.accessToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.accessToken)
	}
	if c.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", c.sessionID)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return
	}
	resp.Body.Close()
}

func (c *httpMcpConn) close() {
	if c.sessionID == "" {
		return
	}
	// Send DELETE to end session.
	req, err := http.NewRequest("DELETE", c.url, nil)
	if err != nil {
		return
	}
	if c.accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.accessToken)
	}
	req.Header.Set("Mcp-Session-Id", c.sessionID)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

func (c *httpMcpConn) getTools() []mcp.Tool   { return c.tools }
func (c *httpMcpConn) getConfig() ExternalMcp { return c.config }

// startHTTP connects to an HTTP MCP server and performs the initialize handshake.
func (m *ExternalMcpManager) startHTTP(mcpCfg *ExternalMcp) error {
	conn := newHTTPMcpConn(*mcpCfg)

	conn.onTokenRefresh = func(oauth *OAuthState) {
		WithSettings(func(s *Settings) { s.UpdateOAuthState(mcpCfg.ID, oauth) })
	}

	result, err := mcpHandshake(conn)
	if err != nil {
		if errors.Is(err, ErrAuthRequired) {
			// Store conn without tools -- UI will show "Authenticate" button.
			m.mu.Lock()
			m.conns[mcpCfg.ID] = conn
			m.mu.Unlock()
			return ErrAuthRequired
		}
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

	slog.Info("HTTP MCP connected", "id", mcpCfg.ID, "tools", len(result.Tools))
	return nil
}

// DiscoverHTTPMcp performs a one-shot HTTP handshake and tool listing.
func DiscoverHTTPMcp(displayName, id, mcpURL string, oauth *OAuthState) (*ExternalMcp, error) {
	cfg := ExternalMcp{
		ID:          id,
		DisplayName: displayName,
		Transport:   "http",
		URL:         mcpURL,
		OAuthState:  oauth,
	}

	conn := newHTTPMcpConn(cfg)

	result, err := mcpHandshake(conn)
	if err != nil {
		if errors.Is(err, ErrAuthRequired) {
			// No session was established, so no close/DELETE needed.
			cfg.DiscoveredTools = []ToolInfo{}
			return &cfg, ErrAuthRequired
		}
		return nil, err
	}
	conn.close()

	cfg.DiscoveredTools = result.ToolInfos
	cfg.ContextSchema = result.ContextSchema
	return &cfg, nil
}
