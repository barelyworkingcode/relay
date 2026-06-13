package main

import (
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
// All fields must be read/written under httpMcpConn.mu.
type httpOAuth struct {
	url          string // MCP endpoint URL for OAuth discovery
	meta         *oauthMetadata
	accessToken  string
	refreshToken string
	clientID     string
	clientSecret string
	tokenExpiry  time.Time
}

// toOAuthState converts runtime OAuth state to the persistable OAuthState.
// Caller must hold httpMcpConn.mu (or ensure no concurrent access).
func (o *httpOAuth) toOAuthState() *OAuthState {
	return &OAuthState{
		ClientID:     o.clientID,
		ClientSecret: o.clientSecret,
		AccessToken:  o.accessToken,
		RefreshToken: o.refreshToken,
		TokenExpiry:  o.tokenExpiry.UTC().Format(time.RFC3339),
	}
}

// mcpSessionIDHeader is the header name for MCP session identification.
const mcpSessionIDHeader = "Mcp-Session-Id"

// maxSessionIDLen caps the Mcp-Session-Id value relay will store and echo back.
// The header comes from an untrusted server; 1 KB is far beyond any legitimate
// session token while bounding memory held per connection.
const maxSessionIDLen = 1024

// httpMcpConn implements McpConnection for Streamable HTTP transport.
type httpMcpConn struct {
	baseMcpConn
	url        string
	sessionID  string
	httpClient *http.Client
	mu         sync.Mutex // protects sessionID and all oauth fields
	tokenMu    sync.Mutex // serializes refresh operations (separate so non-refresh requests don't block on I/O)
	closeOnce  sync.Once  // ensures Close is idempotent

	oauth httpOAuth

	// Callback to persist refreshed tokens. Injected by ExternalMcpManager.
	onTokenRefresh func(oauth *OAuthState)
}

// sessionSnapshot holds pre-snapshotted OAuth and session state,
// captured under lock to avoid holding it during HTTP I/O.
type sessionSnapshot struct {
	accessToken string
	sessionID   string
}

// snapshot captures OAuth/session state under lock for use in HTTP requests.
func (c *httpMcpConn) snapshot() sessionSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return sessionSnapshot{
		accessToken: c.oauth.accessToken,
		sessionID:   c.sessionID,
	}
}

func newHTTPMcpConn(cfg ExternalMcp) *httpMcpConn {
	conn := &httpMcpConn{
		url:        cfg.URL,
		httpClient: &http.Client{
			// No client-level Timeout: it covers the entire transaction including
			// body reads, which would kill long-running SSE streams. Per-request
			// deadlines are set via context instead.
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

// tokenRefreshSnap holds values snapshotted under mu for a token refresh.
type tokenRefreshSnap struct {
	meta         *oauthMetadata
	refreshToken string
	clientID     string
	clientSecret string
	oauthURL     string
	tokenExpiry  time.Time
}

// tokenRefreshSnapshot reads OAuth state under mu and returns whether a refresh
// is needed. All lock/unlock is handled via defer.
func (c *httpMcpConn) tokenRefreshSnapshot() (tokenRefreshSnap, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	needsRefresh := c.oauth.refreshToken != "" &&
		!c.oauth.tokenExpiry.IsZero() &&
		time.Now().After(c.oauth.tokenExpiry.Add(-OAuthTokenRefreshWindow))
	if !needsRefresh {
		return tokenRefreshSnap{}, false
	}
	return tokenRefreshSnap{
		meta:         c.oauth.meta,
		refreshToken: c.oauth.refreshToken,
		clientID:     c.oauth.clientID,
		clientSecret: c.oauth.clientSecret,
		oauthURL:     c.oauth.url,
		tokenExpiry:  c.oauth.tokenExpiry,
	}, true
}

// applyRefreshedToken writes refreshed OAuth state under mu and notifies the
// persistence callback. All lock/unlock is handled via defer.
func (c *httpMcpConn) applyRefreshedToken(meta *oauthMetadata, tokenResp *oauthTokenResponse) {
	c.mu.Lock()
	c.oauth.meta = meta
	c.oauth.accessToken = tokenResp.AccessToken
	if tokenResp.RefreshToken != "" {
		c.oauth.refreshToken = tokenResp.RefreshToken
	}
	if tokenResp.ExpiresIn > 0 {
		c.oauth.tokenExpiry = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	} else {
		// No expires_in in the refresh response: clear the (now-stale) expiry so
		// tokenRefreshSnapshot's IsZero() guard disables proactive refresh.
		// Leaving the old past timestamp would make every subsequent request see
		// now.After(expiry-window) and refresh on each call — a refresh storm.
		c.oauth.tokenExpiry = time.Time{}
	}
	oauthState := c.oauth.toOAuthState()
	c.mu.Unlock()

	if c.onTokenRefresh != nil {
		c.onTokenRefresh(oauthState)
	}
}

// refreshTokenIfNeeded checks token expiry and refreshes if within the refresh
// window. Uses tokenMu to serialize refresh operations and mu to synchronize
// token field access with concurrent SendRequest calls. Network I/O happens
// without holding mu.
func (c *httpMcpConn) refreshTokenIfNeeded() error {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()

	snap, needsRefresh := c.tokenRefreshSnapshot()
	if !needsRefresh {
		return nil
	}

	// stillValid reports whether the current access token has not yet expired.
	// Refresh fires OAuthTokenRefreshWindow *before* expiry, so a transient
	// refresh failure inside that window shouldn't fail the call — the existing
	// token still works. Only hard-fail once the token is actually expired.
	stillValid := func(err error) bool {
		if !snap.tokenExpiry.IsZero() && time.Now().Before(snap.tokenExpiry) {
			slog.Warn("OAuth proactive refresh failed; proceeding with still-valid token", "url", snap.oauthURL, "error", err)
			return true
		}
		return false
	}

	// Discover metadata if needed (network I/O, no locks held).
	meta := snap.meta
	if meta == nil {
		discovery, err := discoverOAuth(snap.oauthURL)
		if err != nil {
			if stillValid(err) {
				return nil
			}
			return fmt.Errorf("discover OAuth metadata for refresh: %w", err)
		}
		meta = discovery.Metadata
	}

	// Refresh token (network I/O, no locks held).
	tokenResp, err := refreshAccessToken(meta, snap.refreshToken, snap.clientID, snap.clientSecret)
	if err != nil {
		if stillValid(err) {
			return nil
		}
		return fmt.Errorf("token refresh: %w", err)
	}

	c.applyRefreshedToken(meta, tokenResp)
	return nil
}

// setHeaders applies common headers using pre-snapshotted session state,
// avoiding the need to hold a lock during HTTP I/O.
func (c *httpMcpConn) setHeaders(req *http.Request, snap sessionSnapshot) {
	req.Header.Set("Content-Type", "application/json")
	if snap.accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+snap.accessToken)
	}
	if snap.sessionID != "" {
		req.Header.Set(mcpSessionIDHeader, snap.sessionID)
	}
}

func (c *httpMcpConn) SendRequest(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	// Bound the whole transaction so a hung or malicious HTTP MCP can't block a
	// tool call (and its bridge handler goroutine) indefinitely. At runtime the
	// caller's ctx carries no deadline; only startup wraps a tighter one, and
	// context.WithTimeout keeps the earlier deadline, so this never loosens an
	// existing bound. Mirrors stdio's MCPRequestTimeout hard cap. net/http
	// cancels in-flight body reads on ctx expiry, so this bounds SSE bodies too.
	ctx, cancel := context.WithTimeout(ctx, MCPRequestTimeout)
	defer cancel()

	// Refresh token outside the request lock to avoid blocking other requests.
	if err := c.refreshTokenIfNeeded(); err != nil {
		return nil, err
	}

	id := c.allocID()
	body, err := json.Marshal(jsonrpc.NewRequest(id, method, params))
	if err != nil {
		return nil, err
	}

	snap := c.snapshot()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create HTTP request: %w", err)
	}
	c.setHeaders(httpReq, snap)
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
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("HTTP MCP %s: HTTP %d: %s", method, resp.StatusCode, string(respBody))
	}

	// Update session ID from response under lock. The value is echoed verbatim
	// on every subsequent request, so cap its length: the remote server is
	// untrusted and net/http only rejects CR/LF, not an unbounded header.
	if sid := resp.Header.Get(mcpSessionIDHeader); sid != "" {
		if len(sid) > maxSessionIDLen {
			return nil, fmt.Errorf("HTTP MCP %s: server returned an oversized %s header (%d bytes)", method, mcpSessionIDHeader, len(sid))
		}
		c.mu.Lock()
		c.sessionID = sid
		c.mu.Unlock()
	}

	contentType := resp.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "text/event-stream") {
		return c.parseSSEResponse(resp.Body, id)
	}

	// Direct JSON response. Cap the body at the bridge's MaxMessageSize: the
	// remote MCP server is untrusted, and an unbounded Decode would let a
	// multi-GB body OOM the tray (the error and SSE paths are already capped).
	var rpcResp jsonrpc.Response
	if err := json.NewDecoder(io.LimitReader(resp.Body, bridge.MaxMessageSize)).Decode(&rpcResp); err != nil {
		return nil, fmt.Errorf("parse JSON-RPC response: %w", err)
	}
	if rpcResp.Error != nil {
		return nil, formatJSONRPCError(rpcResp.Error)
	}
	return rpcResp.Result, nil
}

// parseSSEResponse reads SSE events and extracts the JSON-RPC response matching our ID.
// Per the SSE spec, an event's data can span multiple consecutive "data:" lines
// (concatenated with newlines) and is terminated by a blank line.
func (c *httpMcpConn) parseSSEResponse(reader io.Reader, expectedID int64) (json.RawMessage, error) {
	const maxDataSize = 1 << 20  // 1 MB cap on buffered SSE event data
	const maxSSEEvents = 1 << 20 // defense-in-depth event cap; the request timeout (SendRequest) is the primary bound
	scanner := bridge.NewScanner(reader)
	var dataBuf strings.Builder
	events := 0

	// flush parses the buffered data as a JSON-RPC response. Returns
	// (result, err, matched) where matched indicates the response ID matched.
	flush := func() (json.RawMessage, error, bool) {
		if dataBuf.Len() == 0 {
			return nil, nil, false
		}
		data := dataBuf.String()
		dataBuf.Reset()

		var rpcResp jsonrpc.Response
		if err := json.Unmarshal([]byte(data), &rpcResp); err != nil {
			slog.Warn("HTTP MCP: skipping malformed SSE event", "error", err)
			return nil, nil, false
		}
		if rpcResp.ID == nil {
			return nil, nil, false // notification
		}
		if jsonrpc.RespIDEquals(rpcResp.ID, expectedID) {
			if rpcResp.Error != nil {
				return nil, formatJSONRPCError(rpcResp.Error), true
			}
			return rpcResp.Result, nil, true
		}
		return nil, nil, false
	}

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "data:") {
			// Per SSE spec, strip only a single leading space after the colon.
			payload := strings.TrimPrefix(line, "data:")
			payload = strings.TrimPrefix(payload, " ")
			if payload == "" {
				continue
			}
			if dataBuf.Len()+len(payload)+1 > maxDataSize {
				return nil, fmt.Errorf("SSE event data exceeds %d bytes", maxDataSize)
			}
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(payload)
			continue
		}

		// Blank line = event boundary; other lines (event:, id:, retry:) are ignored.
		if line == "" {
			if result, err, matched := flush(); matched {
				return result, err
			}
			events++
			if events > maxSSEEvents {
				return nil, fmt.Errorf("SSE stream exceeded %d events without a matching response for ID %d", maxSSEEvents, expectedID)
			}
		}
	}

	// Flush any buffered data at end-of-stream (some servers omit trailing blank line).
	if result, err, matched := flush(); matched {
		return result, err
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("SSE read error: %w", err)
	}
	return nil, fmt.Errorf("SSE stream ended without matching response for ID %d", expectedID)
}

func (c *httpMcpConn) SendNotification(method string) {
	body, err := json.Marshal(jsonrpc.NewNotification(method))
	if err != nil {
		slog.Debug("HTTP MCP: failed to marshal notification", "method", method, "error", err)
		return
	}

	snap := c.snapshot()

	// Short timeout for notifications — they are best-effort and should not
	// block the handshake sequence if the server is unresponsive.
	ctx, cancel := context.WithTimeout(context.Background(), MCPNotificationTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.url, bytes.NewReader(body))
	if err != nil {
		slog.Debug("HTTP MCP: failed to create notification request", "method", method, "error", err)
		return
	}
	c.setHeaders(httpReq, snap)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		slog.Debug("HTTP MCP: notification failed", "method", method, "error", err)
		return
	}
	resp.Body.Close()
}

func (c *httpMcpConn) Close() {
	c.closeOnce.Do(c.doClose)
}

func (c *httpMcpConn) doClose() {
	defer c.httpClient.CloseIdleConnections()

	snap := c.snapshot()
	if snap.sessionID == "" {
		return
	}
	// Send DELETE to end session with a short timeout. This is best-effort
	// cleanup; we don't want an unresponsive server to block app shutdown.
	ctx, cancel := context.WithTimeout(context.Background(), HTTPSessionCloseTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "DELETE", c.url, nil)
	if err != nil {
		slog.Debug("HTTP MCP: failed to create session close request", "url", c.url, "error", err)
		return
	}
	c.setHeaders(req, snap)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		slog.Debug("HTTP MCP: session close failed", "url", c.url, "error", err)
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
			m.setConnection(mcpCfg.ID, conn)
			return ErrAuthRequired
		}
		conn.Close()
		return err
	}

	m.finalizeConnection(mcpCfg.ID, conn, result)
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
	defer conn.Close() // Safe for all paths: Close is a no-op if no session was established.

	// Bound discovery so a hung HTTP server can't block the UI indefinitely.
	// Matches DiscoverExternalMcp's use of MCPDiscoveryTimeout for stdio.
	discoverCtx, cancel := context.WithTimeout(ctx, MCPDiscoveryTimeout)
	defer cancel()

	result, err := discoverMcp(discoverCtx, conn, cfg)
	if err != nil {
		if errors.Is(err, ErrAuthRequired) {
			cfg.DiscoveredTools = []ToolInfo{}
			return &cfg, ErrAuthRequired
		}
		return nil, err
	}

	return result, nil
}
