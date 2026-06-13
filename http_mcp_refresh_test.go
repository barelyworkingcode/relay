package main

// Integration coverage for the HTTP-MCP OAuth auto-refresh path. The snapshot
// and applyRefreshedToken units were tested in isolation; this wires the whole
// chain together — a within-window SendRequest must hit the token endpoint,
// put the NEW access token on the wire to the MCP, and fire the onTokenRefresh
// persistence callback — plus the stillValid graceful-degradation branch.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// mcpResultServer captures each request's Authorization header (into authCh,
// non-blocking) and replies with a fixed JSON-RPC result.
func mcpResultServer(t *testing.T, authCh chan<- string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case authCh <- r.Header.Get("Authorization"):
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func refreshTokenServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// newRefreshableConn builds an httpMcpConn whose token expires at `expiry`, with
// its OAuth metadata pre-pointed at tokenURL (so no network discovery runs).
func newRefreshableConn(mcpURL, tokenURL string, expiry time.Time, accessToken string) *httpMcpConn {
	conn := newHTTPMcpConn(ExternalMcp{
		ID: "http-mcp", Transport: "http", URL: mcpURL,
		OAuthState: &OAuthState{
			AccessToken:  accessToken,
			RefreshToken: "old-rt",
			ClientID:     "cid",
			TokenExpiry:  expiry.UTC().Format(time.RFC3339),
		},
	})
	conn.oauth.meta = &oauthMetadata{TokenEndpoint: tokenURL + "/token"}
	return conn
}

func TestHTTPMcp_AutoRefresh_UsesNewTokenAndPersists(t *testing.T) {
	tokenSrv := refreshTokenServer(t, 200,
		`{"access_token":"new-at","token_type":"Bearer","expires_in":3600,"refresh_token":"new-rt"}`)
	authCh := make(chan string, 1)
	mcpSrv := mcpResultServer(t, authCh)

	// Expiry 10s out → inside the 30s refresh window → proactive refresh fires.
	conn := newRefreshableConn(mcpSrv.URL, tokenSrv.URL, time.Now().Add(10*time.Second), "old-at")
	var persisted *OAuthState
	conn.onTokenRefresh = func(o *OAuthState) { persisted = o }

	res, err := conn.SendRequest(context.Background(), "tools/list", nil)
	if err != nil {
		t.Fatalf("SendRequest: %v", err)
	}
	if string(res) != `{"ok":true}` {
		t.Errorf("result = %s, want {\"ok\":true}", res)
	}
	if gotAuth := <-authCh; gotAuth != "Bearer new-at" {
		t.Errorf("MCP saw %q, want Bearer new-at (refreshed token on the wire)", gotAuth)
	}
	if persisted == nil || persisted.AccessToken != "new-at" || persisted.RefreshToken != "new-rt" {
		t.Errorf("onTokenRefresh persisted %+v, want new-at/new-rt", persisted)
	}
}

func TestHTTPMcp_AutoRefresh_TransientFailureProceedsWithValidToken(t *testing.T) {
	tokenSrv := refreshTokenServer(t, 500, `{"error":"temporarily_unavailable"}`)
	authCh := make(chan string, 1)
	mcpSrv := mcpResultServer(t, authCh)

	// Within the refresh window but NOT yet expired → a failed refresh must not
	// fail the call; the still-valid token is used.
	conn := newRefreshableConn(mcpSrv.URL, tokenSrv.URL, time.Now().Add(10*time.Second), "old-at")
	refreshed := false
	conn.onTokenRefresh = func(*OAuthState) { refreshed = true }

	if _, err := conn.SendRequest(context.Background(), "tools/list", nil); err != nil {
		t.Fatalf("should proceed with still-valid token despite refresh failure: %v", err)
	}
	if gotAuth := <-authCh; gotAuth != "Bearer old-at" {
		t.Errorf("MCP saw %q, want Bearer old-at (still-valid token reused)", gotAuth)
	}
	if refreshed {
		t.Error("onTokenRefresh must not fire on a failed refresh")
	}
}

func TestHTTPMcp_AutoRefresh_HardFailsWhenTokenExpired(t *testing.T) {
	tokenSrv := refreshTokenServer(t, 500, `{"error":"invalid_grant"}`)
	authCh := make(chan string, 1)
	mcpSrv := mcpResultServer(t, authCh)

	// Already expired AND refresh fails → the call must hard-fail rather than
	// proceed with a dead token.
	conn := newRefreshableConn(mcpSrv.URL, tokenSrv.URL, time.Now().Add(-1*time.Second), "old-at")
	if _, err := conn.SendRequest(context.Background(), "tools/list", nil); err == nil {
		t.Fatal("expected hard failure when token is expired and refresh fails")
	}
}
