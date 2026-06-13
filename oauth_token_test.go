package main

// Coverage for the OAuth token-endpoint POST (exchangeCode / refreshAccessToken
// / postTokenEndpoint) and the local callback server. The SSRF guard and PKCE
// helpers were already tested; these close the gap on actually getting and
// refreshing credentials, and on the CSRF-critical callback state check.
//
// The token endpoint runs on httptest's 127.0.0.1 listener, which the OAuth
// SSRF validator permits (loopback HTTP is allowed), so these exercise the real
// oauthHTTPClient.PostForm path end to end.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// tokenEndpoint starts an httptest server that records the last posted form and
// replies with the supplied status + body.
func tokenEndpoint(t *testing.T, status int, body string) (*oauthMetadata, *url.Values) {
	t.Helper()
	var lastForm url.Values
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		lastForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return &oauthMetadata{TokenEndpoint: ts.URL + "/token"}, &lastForm
}

func TestExchangeCode_HappyPath(t *testing.T) {
	meta, form := tokenEndpoint(t, 200, `{"access_token":"at-123","token_type":"Bearer","expires_in":3600,"refresh_token":"rt-456"}`)

	resp, err := exchangeCode(meta, "the-code", "the-verifier", "http://127.0.0.1/cb", "client-1", "secret-1")
	if err != nil {
		t.Fatalf("exchangeCode: %v", err)
	}
	if resp.AccessToken != "at-123" || resp.RefreshToken != "rt-456" || resp.ExpiresIn != 3600 {
		t.Fatalf("unexpected token response: %+v", resp)
	}
	// The authorization_code grant must carry the PKCE verifier and code.
	if form.Get("grant_type") != "authorization_code" {
		t.Errorf("grant_type = %q, want authorization_code", form.Get("grant_type"))
	}
	for k, want := range map[string]string{
		"code":          "the-code",
		"code_verifier": "the-verifier",
		"redirect_uri":  "http://127.0.0.1/cb",
		"client_id":     "client-1",
		"client_secret": "secret-1",
	} {
		if got := form.Get(k); got != want {
			t.Errorf("form[%s] = %q, want %q", k, got, want)
		}
	}
}

func TestRefreshAccessToken_HappyPath(t *testing.T) {
	meta, form := tokenEndpoint(t, 200, `{"access_token":"new-at","token_type":"Bearer","expires_in":1800}`)

	resp, err := refreshAccessToken(meta, "old-rt", "client-1", "")
	if err != nil {
		t.Fatalf("refreshAccessToken: %v", err)
	}
	if resp.AccessToken != "new-at" {
		t.Fatalf("access token = %q, want new-at", resp.AccessToken)
	}
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "old-rt" {
		t.Errorf("refresh form wrong: %v", form.Encode())
	}
	// No client secret supplied → must be omitted, not sent empty.
	if form.Has("client_secret") {
		t.Errorf("client_secret should be absent when empty; got %q", form.Get("client_secret"))
	}
}

func TestPostTokenEndpoint_Non200IsError(t *testing.T) {
	meta, _ := tokenEndpoint(t, 400, `{"error":"invalid_grant"}`)
	_, err := exchangeCode(meta, "c", "v", "http://127.0.0.1/cb", "id", "")
	if err == nil {
		t.Fatal("expected error on non-200 token response")
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("error should surface status + body: %v", err)
	}
}

func TestPostTokenEndpoint_MissingAccessTokenIsError(t *testing.T) {
	meta, _ := tokenEndpoint(t, 200, `{"token_type":"Bearer"}`) // no access_token
	_, err := refreshAccessToken(meta, "rt", "id", "")
	if err == nil || !strings.Contains(err.Error(), "missing access_token") {
		t.Fatalf("want missing-access_token error, got %v", err)
	}
}

func TestPostTokenEndpoint_RejectsNonLoopbackHTTP(t *testing.T) {
	// The SSRF validator runs before any request: a public http:// token
	// endpoint must be rejected outright.
	meta := &oauthMetadata{TokenEndpoint: "http://token.example.com/token"}
	if _, err := refreshAccessToken(meta, "rt", "id", ""); err == nil {
		t.Fatal("expected rejection of non-loopback HTTP token endpoint")
	}
}

// ---------------------------------------------------------------------------
// oauthCallbackServer
// ---------------------------------------------------------------------------

func TestOAuthCallback_DeliversCode(t *testing.T) {
	srv, redirectURI, err := newOAuthCallbackServer("state-xyz")
	if err != nil {
		t.Fatalf("newOAuthCallbackServer: %v", err)
	}
	defer srv.Close()

	resp, err := http.Get(redirectURI + "?state=state-xyz&code=auth-code-1")
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	resp.Body.Close()

	code, err := srv.WaitForCode(2 * time.Second)
	if err != nil {
		t.Fatalf("WaitForCode: %v", err)
	}
	if code != "auth-code-1" {
		t.Errorf("code = %q, want auth-code-1", code)
	}
}

// CSRF defense: a callback whose state doesn't match must be rejected (400) and
// must not deliver a code.
func TestOAuthCallback_StateMismatchRejected(t *testing.T) {
	srv, redirectURI, err := newOAuthCallbackServer("expected-state")
	if err != nil {
		t.Fatalf("newOAuthCallbackServer: %v", err)
	}
	defer srv.Close()

	resp, err := http.Get(redirectURI + "?state=attacker-state&code=evil")
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("browser status = %d, want 400 on state mismatch", resp.StatusCode)
	}
	resp.Body.Close()

	if _, err := srv.WaitForCode(2 * time.Second); err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("want state-mismatch error, got %v", err)
	}
}

func TestOAuthCallback_ErrorParamSurfaces(t *testing.T) {
	srv, redirectURI, err := newOAuthCallbackServer("s")
	if err != nil {
		t.Fatalf("newOAuthCallbackServer: %v", err)
	}
	defer srv.Close()

	resp, err := http.Get(redirectURI + "?state=s&error=access_denied&error_description=user+said+no")
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	resp.Body.Close()

	if _, err := srv.WaitForCode(2 * time.Second); err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("want access_denied error, got %v", err)
	}
}

func TestOAuthCallback_MissingCodeIsError(t *testing.T) {
	srv, redirectURI, err := newOAuthCallbackServer("s")
	if err != nil {
		t.Fatalf("newOAuthCallbackServer: %v", err)
	}
	defer srv.Close()

	resp, err := http.Get(redirectURI + "?state=s") // valid state, no code, no error
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	resp.Body.Close()

	if _, err := srv.WaitForCode(2 * time.Second); err == nil || !strings.Contains(err.Error(), "no authorization code") {
		t.Fatalf("want missing-code error, got %v", err)
	}
}

func TestOAuthCallback_WaitForCodeTimeout(t *testing.T) {
	srv, _, err := newOAuthCallbackServer("s")
	if err != nil {
		t.Fatalf("newOAuthCallbackServer: %v", err)
	}
	defer srv.Close()

	if _, err := srv.WaitForCode(100 * time.Millisecond); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want timeout error, got %v", err)
	}
}
