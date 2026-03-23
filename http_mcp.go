package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"relaygo/bridge"
	"relaygo/jsonrpc"
)

// ErrAuthRequired indicates the HTTP MCP server returned 401.
var ErrAuthRequired = errors.New("authentication required (HTTP 401)")

// httpOAuth holds runtime OAuth token state for an HTTP MCP connection.
type httpOAuth struct {
	url          string // MCP endpoint URL for OAuth discovery
	meta         *oauthMetadata
	accessToken  string
	refreshToken string
	clientID     string
	clientSecret string
	tokenExpiry  time.Time
}

// refreshIfNeeded checks token expiry and refreshes if within 30s of expiry.
// Returns true if tokens were refreshed. Caller must serialize access.
func (o *httpOAuth) refreshIfNeeded() (bool, error) {
	if o.refreshToken == "" || o.tokenExpiry.IsZero() {
		return false, nil
	}
	if time.Now().Before(o.tokenExpiry.Add(-30 * time.Second)) {
		return false, nil
	}

	meta := o.meta
	if meta == nil {
		discovery, err := discoverOAuth(o.url)
		if err != nil {
			return false, fmt.Errorf("discover OAuth metadata for refresh: %w", err)
		}
		meta = discovery.Metadata
		o.meta = meta
	}

	tokenResp, err := refreshAccessToken(meta, o.refreshToken, o.clientID, o.clientSecret)
	if err != nil {
		return false, fmt.Errorf("token refresh: %w", err)
	}

	o.accessToken = tokenResp.AccessToken
	if tokenResp.RefreshToken != "" {
		o.refreshToken = tokenResp.RefreshToken
	}
	if tokenResp.ExpiresIn > 0 {
		o.tokenExpiry = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	}
	return true, nil
}

// toOAuthState converts runtime OAuth state to the persistable OAuthState.
func (o *httpOAuth) toOAuthState() *OAuthState {
	return &OAuthState{
		ClientID:     o.clientID,
		ClientSecret: o.clientSecret,
		AccessToken:  o.accessToken,
		RefreshToken: o.refreshToken,
		TokenExpiry:  o.tokenExpiry.UTC().Format(time.RFC3339),
	}
}

// httpMcpConn implements McpConnection for Streamable HTTP transport.
type httpMcpConn struct {
	baseMcpConn
	url        string
	sessionID  string
	httpClient *http.Client
	mu         sync.Mutex // protects request sending (nextID, sessionID)
	tokenMu    sync.Mutex // protects OAuth token refresh (separate to avoid blocking requests during refresh)

	oauth httpOAuth

	// Callback to persist refreshed tokens. Injected by ExternalMcpManager.
	onTokenRefresh func(oauth *OAuthState)
}

func newHTTPMcpConn(cfg ExternalMcp) *httpMcpConn {
	conn := &httpMcpConn{
		url: cfg.URL,
		httpClient: &http.Client{
			Timeout: mcpRequestTimeout,
		},
	}
	conn.config = cfg

	conn.oauth.url = cfg.URL

	if cfg.OAuthState != nil {
		conn.oauth.accessToken = cfg.OAuthState.AccessToken
		conn.oauth.refreshToken = cfg.OAuthState.RefreshToken
		conn.oauth.clientID = cfg.OAuthState.ClientID
		conn.oauth.clientSecret = cfg.OAuthState.ClientSecret
		if cfg.OAuthState.TokenExpiry != "" {
			if t, err := time.Parse(time.RFC3339, cfg.OAuthState.TokenExpiry); err == nil {
				conn.oauth.tokenExpiry = t
			}
		}
	}

	return conn
}

// refreshTokenIfNeeded acquires the token mutex and delegates to httpOAuth.
// Called outside the request lock to avoid blocking all requests during refresh.
func (c *httpMcpConn) refreshTokenIfNeeded() error {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	refreshed, err := c.oauth.refreshIfNeeded()
	if err != nil {
		return err
	}
	if refreshed && c.onTokenRefresh != nil {
		c.onTokenRefresh(c.oauth.toOAuthState())
	}
	return nil
}

// setHeaders applies common headers (Content-Type, Authorization, Session-Id) to an HTTP request.
func (c *httpMcpConn) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	if c.oauth.accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.oauth.accessToken)
	}
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}
}

func (c *httpMcpConn) SendRequest(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	// Refresh token outside the request lock to avoid blocking other requests.
	if err := c.refreshTokenIfNeeded(); err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.allocID()
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

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create HTTP request: %w", err)
	}
	c.setHeaders(httpReq)
	httpReq.Header.Set("Accept", "application/json, text/event-stream")

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
		return nil, formatJSONRPCError(rpcResp.Error)
	}
	return rpcResp.Result, nil
}

// parseSSEResponse reads SSE data lines and extracts the JSON-RPC response matching our ID.
func (c *httpMcpConn) parseSSEResponse(reader io.Reader, expectedID int64) (json.RawMessage, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), bridge.MaxMessageSize)
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
				return nil, formatJSONRPCError(rpcResp.Error)
			}
			return rpcResp.Result, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("SSE read error: %w", err)
	}
	return nil, fmt.Errorf("SSE stream ended without matching response")
}

func (c *httpMcpConn) SendNotification(method string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	req := jsonrpc.Request{
		JSONRPC: "2.0",
		Method:  method,
	}
	body, err := json.Marshal(req)
	if err != nil {
		slog.Debug("HTTP MCP: failed to marshal notification", "method", method, "error", err)
		return
	}

	httpReq, err := http.NewRequest("POST", c.url, bytes.NewReader(body))
	if err != nil {
		slog.Debug("HTTP MCP: failed to create notification request", "method", method, "error", err)
		return
	}
	c.setHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		slog.Debug("HTTP MCP: notification failed", "method", method, "error", err)
		return
	}
	resp.Body.Close()
}

func (c *httpMcpConn) Close() {
	if c.sessionID == "" {
		return
	}
	// Send DELETE to end session.
	req, err := http.NewRequest("DELETE", c.url, nil)
	if err != nil {
		return
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// startHTTP connects to an HTTP MCP server and performs the initialize handshake.
func (m *ExternalMcpManager) startHTTP(ctx context.Context, mcpCfg *ExternalMcp) error {
	conn := newHTTPMcpConn(*mcpCfg)

	// Wire up token refresh to the manager's injected callback.
	if m.onTokenRefresh != nil {
		id := mcpCfg.ID
		conn.onTokenRefresh = func(oauth *OAuthState) {
			m.onTokenRefresh(id, oauth)
		}
	}

	result, err := mcpHandshake(ctx, conn)
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

	if m.onDiscover != nil {
		m.onDiscover(mcpCfg.ID, result.ToolInfos, result.ContextSchema)
	}

	m.mu.Lock()
	m.conns[mcpCfg.ID] = conn
	m.mu.Unlock()

	slog.Info("HTTP MCP connected", "id", mcpCfg.ID, "tools", len(result.Tools))
	return nil
}

// DiscoverHTTPMcp performs a one-shot HTTP handshake and tool listing.
func DiscoverHTTPMcp(ctx context.Context, displayName, id, mcpURL string, oauth *OAuthState) (*ExternalMcp, error) {
	cfg := ExternalMcp{
		ID:          id,
		DisplayName: displayName,
		Transport:   "http",
		URL:         mcpURL,
		OAuthState:  oauth,
	}

	conn := newHTTPMcpConn(cfg)

	result, err := mcpHandshake(ctx, conn)
	if err != nil {
		if errors.Is(err, ErrAuthRequired) {
			// No session was established, so no close/DELETE needed.
			cfg.DiscoveredTools = []ToolInfo{}
			return &cfg, ErrAuthRequired
		}
		return nil, err
	}
	conn.Close()

	cfg.DiscoveredTools = result.ToolInfos
	cfg.ContextSchema = result.ContextSchema
	return &cfg, nil
}
