package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// oauthMetadata holds discovered OAuth 2.1 authorization server metadata.
type oauthMetadata struct {
	AuthorizationEndpoint         string   `json:"authorization_endpoint"`
	TokenEndpoint                 string   `json:"token_endpoint"`
	RegistrationEndpoint          string   `json:"registration_endpoint,omitempty"`
	ResponseTypesSupported        []string `json:"response_types_supported,omitempty"`
	GrantTypesSupported           []string `json:"grant_types_supported,omitempty"`
	CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported,omitempty"`
}

// protectedResourceMetadata holds the PRM document (RFC 9728).
type protectedResourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers,omitempty"`
	ScopesSupported      []string `json:"scopes_supported,omitempty"`
}

// oauthDiscoveryResult holds the results of the full OAuth discovery chain.
type oauthDiscoveryResult struct {
	Metadata *oauthMetadata
	Scope    string // space-separated scopes from PRM
}

// oauthTokenResponse is the response from the token endpoint.
type oauthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

// oauthRegistrationResponse is the response from dynamic client registration.
type oauthRegistrationResponse struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret,omitempty"`
}

// pkceParams holds PKCE code verifier and challenge.
type pkceParams struct {
	Verifier  string
	Challenge string
}

var oauthHTTPClient = &http.Client{Timeout: 15 * time.Second}

// probeForResourceMetadata sends a request to the MCP URL and extracts the
// resource_metadata URL from the 401 WWW-Authenticate header.
func probeForResourceMetadata(mcpURL string) string {
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	req, err := http.NewRequest("POST", mcpURL, strings.NewReader(body))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		return ""
	}

	return parseResourceMetadataURL(resp.Header.Get("WWW-Authenticate"))
}

// parseResourceMetadataURL extracts resource_metadata="<url>" from a WWW-Authenticate header.
func parseResourceMetadataURL(wwwAuth string) string {
	if wwwAuth == "" {
		return ""
	}
	const prefix = `resource_metadata="`
	idx := strings.Index(wwwAuth, prefix)
	if idx < 0 {
		return ""
	}
	start := idx + len(prefix)
	rest := wwwAuth[start:]
	end := strings.Index(rest, `"`)
	if end <= 0 {
		return ""
	}
	return rest[:end]
}

// fetchProtectedResourceMetadata fetches a PRM document (RFC 9728).
func fetchProtectedResourceMetadata(prmURL string) (*protectedResourceMetadata, error) {
	resp, err := oauthHTTPClient.Get(prmURL)
	if err != nil {
		return nil, fmt.Errorf("fetch PRM: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("PRM fetch returned HTTP %d", resp.StatusCode)
	}

	var prm protectedResourceMetadata
	if err := json.NewDecoder(resp.Body).Decode(&prm); err != nil {
		return nil, fmt.Errorf("parse PRM: %w", err)
	}
	return &prm, nil
}

// tryFetchOAuthMetadata tries to GET and parse an OAuth AS metadata document.
func tryFetchOAuthMetadata(metadataURL string) *oauthMetadata {
	resp, err := oauthHTTPClient.Get(metadataURL)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var meta oauthMetadata
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil
	}
	if meta.AuthorizationEndpoint == "" || meta.TokenEndpoint == "" {
		return nil
	}
	return &meta
}

// discoverOAuth discovers the OAuth authorization server metadata
// following the MCP spec (2025-03-26) discovery chain:
//  1. Probe MCP URL for 401 -> WWW-Authenticate resource_metadata
//  2. Fetch PRM -> get authorization_servers + scopes_supported
//  3. Fetch AS metadata (path-aware, then non-path-aware)
//  4. Fallback to guessing from MCP base URL
func discoverOAuth(mcpURL string) (*oauthDiscoveryResult, error) {
	parsed, err := url.Parse(mcpURL)
	if err != nil {
		return nil, fmt.Errorf("invalid MCP URL: %w", err)
	}

	mcpBase := fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host)

	// Step 1: Probe for Protected Resource Metadata URL.
	resourceMetaURL := probeForResourceMetadata(mcpURL)

	// Step 2: Follow PRM -> authorization server chain.
	var authServerBase string
	var scope string
	if resourceMetaURL != "" {
		slog.Info("oauth: found resource_metadata", "url", resourceMetaURL)
		prm, err := fetchProtectedResourceMetadata(resourceMetaURL)
		if err == nil {
			if len(prm.AuthorizationServers) > 0 {
				authServerBase = prm.AuthorizationServers[0]
				slog.Info("oauth: authorization server", "url", authServerBase)
			}
			if len(prm.ScopesSupported) > 0 {
				scope = strings.Join(prm.ScopesSupported, " ")
				slog.Info("oauth: scopes", "scopes", scope)
			}
		}
	}

	// Step 3: Try to fetch AS metadata.
	// Try from the authorization server if we found one, otherwise from the MCP host.
	searchBases := []string{}
	if authServerBase != "" {
		searchBases = append(searchBases, authServerBase)
	}
	searchBases = append(searchBases, mcpBase)

	for _, base := range searchBases {
		// Path-aware: /.well-known/oauth-authorization-server<mcpPath>
		if parsed.Path != "" && parsed.Path != "/" {
			pathAware := base + "/.well-known/oauth-authorization-server" + parsed.Path
			if meta := tryFetchOAuthMetadata(pathAware); meta != nil {
				slog.Info("oauth: discovered metadata", "url", pathAware)
				return &oauthDiscoveryResult{Metadata: meta, Scope: scope}, nil
			}
		}

		// Non-path-aware: /.well-known/oauth-authorization-server
		nonPathAware := base + "/.well-known/oauth-authorization-server"
		if meta := tryFetchOAuthMetadata(nonPathAware); meta != nil {
			slog.Info("oauth: discovered metadata", "url", nonPathAware)
			return &oauthDiscoveryResult{Metadata: meta, Scope: scope}, nil
		}
	}

	// Step 4: Fallback -- construct default endpoints.
	fallback := mcpBase
	if authServerBase != "" {
		fallback = authServerBase
	}
	slog.Info("oauth: using fallback endpoints", "base", fallback)
	authEndpoint, _ := url.JoinPath(fallback, "authorize")
	tokenEndpoint, _ := url.JoinPath(fallback, "token")
	regEndpoint, _ := url.JoinPath(fallback, "register")
	return &oauthDiscoveryResult{
		Metadata: &oauthMetadata{
			AuthorizationEndpoint: authEndpoint,
			TokenEndpoint:         tokenEndpoint,
			RegistrationEndpoint:  regEndpoint,
		},
		Scope: scope,
	}, nil
}

// dynamicClientRegister attempts RFC 7591 dynamic client registration.
func dynamicClientRegister(meta *oauthMetadata, redirectURI, scope string) (*oauthRegistrationResponse, error) {
	if meta.RegistrationEndpoint == "" {
		return nil, fmt.Errorf("server has no registration endpoint; manual client registration required")
	}

	regBody := map[string]interface{}{
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "client_secret_post",
		"client_name":                "Relay MCP Client",
	}
	if scope != "" {
		regBody["scope"] = scope
	}
	body, _ := json.Marshal(regBody)

	slog.Info("oauth: registering client", "endpoint", meta.RegistrationEndpoint)
	resp, err := oauthHTTPClient.Post(meta.RegistrationEndpoint, "application/json", strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("registration request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("dynamic client registration failed (HTTP %d): %s\nCheck if the server requires manual client registration at: %s",
			resp.StatusCode, string(respBody), meta.RegistrationEndpoint)
	}

	var regResp oauthRegistrationResponse
	if err := json.NewDecoder(resp.Body).Decode(&regResp); err != nil {
		return nil, fmt.Errorf("parse registration response: %w", err)
	}
	if regResp.ClientID == "" {
		return nil, fmt.Errorf("registration response missing client_id")
	}
	slog.Info("oauth: client registered", "client_id", regResp.ClientID)
	return &regResp, nil
}

// generatePKCE creates a PKCE code verifier and S256 challenge.
func generatePKCE() (*pkceParams, error) {
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		return nil, fmt.Errorf("generate PKCE verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)

	h := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(h[:])

	return &pkceParams{Verifier: verifier, Challenge: challenge}, nil
}

// generateState creates a random state parameter.
func generateState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// startOAuthFlow orchestrates the full OAuth 2.1 flow:
//  1. Discover metadata (PRM chain + path-aware well-known)
//  2. Start local callback server
//  3. Dynamic client registration
//  4. Generate PKCE + state
//  5. Open browser to authorization URL
//  6. Wait for callback
//  7. Exchange code for tokens
func startOAuthFlow(mcpURL string, openBrowser func(string)) (*OAuthState, error) {
	discovery, err := discoverOAuth(mcpURL)
	if err != nil {
		return nil, fmt.Errorf("OAuth discovery: %w", err)
	}
	meta := discovery.Metadata

	// Start local callback server on a random port.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start callback server: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/oauth/callback", port)

	// Dynamic client registration.
	reg, err := dynamicClientRegister(meta, redirectURI, discovery.Scope)
	if err != nil {
		listener.Close()
		return nil, err
	}

	// PKCE.
	pkce, err := generatePKCE()
	if err != nil {
		listener.Close()
		return nil, err
	}

	state, err := generateState()
	if err != nil {
		listener.Close()
		return nil, err
	}

	// Channel to receive the authorization code.
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			errCh <- fmt.Errorf("OAuth state mismatch")
			http.Error(w, "State mismatch", http.StatusBadRequest)
			return
		}
		if errMsg := r.URL.Query().Get("error"); errMsg != "" {
			desc := r.URL.Query().Get("error_description")
			errCh <- fmt.Errorf("OAuth error: %s: %s", errMsg, desc)
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, "<html><body><h2>Authorization Failed</h2><p>%s</p><p>You can close this window.</p></body></html>", desc)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			errCh <- fmt.Errorf("no authorization code in callback")
			http.Error(w, "Missing code", http.StatusBadRequest)
			return
		}
		codeCh <- code
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body><h2>Authorization Successful</h2><p>You can close this window and return to Relay.</p></body></html>")
	})

	server := &http.Server{Handler: mux}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("callback server: %w", err)
		}
	}()

	// Build authorization URL.
	authURL, err := url.Parse(meta.AuthorizationEndpoint)
	if err != nil {
		server.Close()
		return nil, fmt.Errorf("invalid authorization endpoint: %w", err)
	}
	q := authURL.Query()
	q.Set("response_type", "code")
	q.Set("client_id", reg.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("code_challenge", pkce.Challenge)
	q.Set("code_challenge_method", "S256")
	if discovery.Scope != "" {
		q.Set("scope", discovery.Scope)
	}
	authURL.RawQuery = q.Encode()

	slog.Info("oauth: opening browser for authorization")

	// Open browser.
	openBrowser(authURL.String())

	// Wait for callback or timeout.
	var code string
	select {
	case code = <-codeCh:
		slog.Info("oauth: received authorization code")
	case err := <-errCh:
		server.Close()
		return nil, err
	case <-time.After(5 * time.Minute):
		server.Close()
		return nil, fmt.Errorf("OAuth flow timed out waiting for authorization (5 minutes)")
	}

	server.Close()

	// Exchange code for tokens.
	tokenResp, err := exchangeCode(meta, code, pkce.Verifier, redirectURI, reg.ClientID, reg.ClientSecret)
	if err != nil {
		return nil, err
	}

	oauthState := &OAuthState{
		ClientID:     reg.ClientID,
		ClientSecret: reg.ClientSecret,
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
	}
	if tokenResp.ExpiresIn > 0 {
		oauthState.TokenExpiry = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	}

	return oauthState, nil
}

// exchangeCode exchanges an authorization code for tokens.
func exchangeCode(meta *oauthMetadata, code, verifier, redirectURI, clientID, clientSecret string) (*oauthTokenResponse, error) {
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	}
	if clientSecret != "" {
		data.Set("client_secret", clientSecret)
	}

	resp, err := oauthHTTPClient.PostForm(meta.TokenEndpoint, data)
	if err != nil {
		return nil, fmt.Errorf("token exchange request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("token exchange failed (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var tokenResp oauthTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("parse token response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return nil, fmt.Errorf("token response missing access_token")
	}
	return &tokenResp, nil
}

// refreshAccessToken uses a refresh token to obtain a new access token.
func refreshAccessToken(meta *oauthMetadata, refreshToken, clientID, clientSecret string) (*oauthTokenResponse, error) {
	data := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
	}
	if clientSecret != "" {
		data.Set("client_secret", clientSecret)
	}

	resp, err := oauthHTTPClient.PostForm(meta.TokenEndpoint, data)
	if err != nil {
		return nil, fmt.Errorf("token refresh request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("token refresh failed (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var tokenResp oauthTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("parse refresh response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return nil, fmt.Errorf("refresh response missing access_token")
	}
	return &tokenResp, nil
}
