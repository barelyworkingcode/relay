package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"relaye2e/fakes/calllog"
)

// oauthServer is the authorization server and the protected-resource metadata
// for one fakemcp listener, following the MCP authorization spec (2025-06-18):
// dynamic client registration, authorization code with PKCE S256, refresh.
// State is in memory, so a restart of the fake forgets every token.
type oauthServer struct {
	base string
	ttl  time.Duration
	log  *calllog.Writer

	mu      sync.Mutex
	clients map[string][]string // client_id to registered redirect URIs
	codes   map[string]authCode
	access  map[string]time.Time
	refresh map[string]string // refresh token to client_id
}

type authCode struct {
	clientID    string
	redirectURI string
	challenge   string
}

func newOAuthServer(base string, ttl time.Duration, log *calllog.Writer) *oauthServer {
	return &oauthServer{
		base: base, ttl: ttl, log: log,
		clients: map[string][]string{},
		codes:   map[string]authCode{},
		access:  map[string]time.Time{},
		refresh: map[string]string{},
	}
}

func (o *oauthServer) mount(mux *http.ServeMux) {
	// The metadata documents are also served under a path suffix, which is how
	// a client that derives them from a resource path looks them up.
	mux.HandleFunc("/.well-known/oauth-protected-resource", o.protectedResource)
	mux.HandleFunc("/.well-known/oauth-protected-resource/", o.protectedResource)
	mux.HandleFunc("/.well-known/oauth-authorization-server", o.serverMetadata)
	mux.HandleFunc("/.well-known/oauth-authorization-server/", o.serverMetadata)
	mux.HandleFunc("/register", o.register)
	mux.HandleFunc("/authorize", o.authorize)
	mux.HandleFunc("/token", o.token)
}

func (o *oauthServer) validBearer(header string) bool {
	const scheme = "bearer "
	if len(header) <= len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return false
	}
	tok := strings.TrimSpace(header[len(scheme):])
	o.mu.Lock()
	defer o.mu.Unlock()
	exp, ok := o.access[tok]
	return ok && time.Now().Before(exp)
}

func (o *oauthServer) record(endpoint string, r *http.Request, params any, raw []byte) {
	var pj []byte
	if params != nil {
		pj, _ = json.Marshal(params)
	}
	_ = o.log.Append(calllog.NewCall("http", "oauth/"+endpoint, nil, pj, raw, calllog.AuthLabel(r.Header.Get("Authorization"))))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, code, desc string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": code, "error_description": desc})
}

func (o *oauthServer) protectedResource(w http.ResponseWriter, r *http.Request) {
	o.record("protected-resource", r, nil, nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 o.base + "/mcp",
		"authorization_servers":    []string{o.base},
		"bearer_methods_supported": []string{"header"},
	})
}

func (o *oauthServer) serverMetadata(w http.ResponseWriter, r *http.Request) {
	o.record("authorization-server", r, nil, nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                o.base,
		"authorization_endpoint":                o.base + "/authorize",
		"token_endpoint":                        o.base + "/token",
		"registration_endpoint":                 o.base + "/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
	})
}

func (o *oauthServer) register(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxBody))
	o.record("register", r, json.RawMessage(body), body)
	var req struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if json.Unmarshal(body, &req) != nil || len(req.RedirectURIs) == 0 {
		oauthError(w, "invalid_client_metadata", "redirect_uris is required")
		return
	}
	id := randomToken(16)
	o.mu.Lock()
	o.clients[id] = req.RedirectURIs
	o.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  id,
		"client_name":                req.ClientName,
		"redirect_uris":              req.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

// secretFields are request parameters whose value is a credential; the log
// keeps a hash of each instead.
var secretFields = map[string]bool{"code": true, "code_verifier": true, "refresh_token": true, "client_secret": true}

func redactedForm(v url.Values) map[string]string {
	out := make(map[string]string, len(v))
	for k := range v {
		if secretFields[k] {
			out[k+"_sha256"] = calllog.SHA256Hex([]byte(v.Get(k)))
			continue
		}
		out[k] = v.Get(k)
	}
	return out
}

func isLoopbackRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return false
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

func (o *oauthServer) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	o.record("authorize", r, redactedForm(q), []byte(r.URL.RawQuery))

	redirect := q.Get("redirect_uri")
	if !isLoopbackRedirect(redirect) {
		oauthError(w, "invalid_request", "redirect_uri must be a loopback http URL")
		return
	}
	clientID := q.Get("client_id")
	o.mu.Lock()
	registered, known := o.clients[clientID]
	o.mu.Unlock()
	if known && !containsString(registered, redirect) {
		oauthError(w, "invalid_request", "redirect_uri is not registered")
		return
	}
	// From here the redirect URI is trusted, so errors go back to it.
	fail := func(code, desc string) {
		u, _ := url.Parse(redirect)
		v := u.Query()
		v.Set("error", code)
		v.Set("error_description", desc)
		if st := q.Get("state"); st != "" {
			v.Set("state", st)
		}
		u.RawQuery = v.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	}
	if q.Get("response_type") != "code" {
		fail("unsupported_response_type", "response_type must be code")
		return
	}
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		fail("invalid_request", "PKCE with S256 is required")
		return
	}
	code := randomToken(16)
	o.mu.Lock()
	o.codes[code] = authCode{clientID: clientID, redirectURI: redirect, challenge: q.Get("code_challenge")}
	o.mu.Unlock()

	u, _ := url.Parse(redirect)
	v := u.Query()
	v.Set("code", code)
	if st := q.Get("state"); st != "" {
		v.Set("state", st)
	}
	u.RawQuery = v.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (o *oauthServer) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxBody))
	form, err := url.ParseQuery(string(body))
	if err != nil {
		oauthError(w, "invalid_request", "form body expected")
		return
	}
	o.record("token", r, redactedForm(form), body)

	switch form.Get("grant_type") {
	case "authorization_code":
		o.exchangeCode(w, form)
	case "refresh_token":
		o.exchangeRefresh(w, form)
	default:
		oauthError(w, "unsupported_grant_type", "authorization_code or refresh_token")
	}
}

func (o *oauthServer) exchangeCode(w http.ResponseWriter, form url.Values) {
	o.mu.Lock()
	ac, ok := o.codes[form.Get("code")]
	delete(o.codes, form.Get("code")) // single use, even when the rest fails
	o.mu.Unlock()
	if !ok {
		oauthError(w, "invalid_grant", "unknown or used code")
		return
	}
	if ru := form.Get("redirect_uri"); ru != "" && ru != ac.redirectURI {
		oauthError(w, "invalid_grant", "redirect_uri mismatch")
		return
	}
	sum := sha256.Sum256([]byte(form.Get("code_verifier")))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if form.Get("code_verifier") == "" || subtle.ConstantTimeCompare([]byte(want), []byte(ac.challenge)) != 1 {
		oauthError(w, "invalid_grant", "code_verifier does not match")
		return
	}
	o.issue(w, ac.clientID)
}

func (o *oauthServer) exchangeRefresh(w http.ResponseWriter, form url.Values) {
	o.mu.Lock()
	clientID, ok := o.refresh[form.Get("refresh_token")]
	delete(o.refresh, form.Get("refresh_token")) // rotated on use
	o.mu.Unlock()
	if !ok {
		oauthError(w, "invalid_grant", "unknown refresh token")
		return
	}
	o.issue(w, clientID)
}

func (o *oauthServer) issue(w http.ResponseWriter, clientID string) {
	at, rt := randomToken(32), randomToken(32)
	o.mu.Lock()
	o.access[at] = time.Now().Add(o.ttl)
	o.refresh[rt] = clientID
	o.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  at,
		"token_type":    "Bearer",
		"expires_in":    int(o.ttl.Seconds()),
		"refresh_token": rt,
	})
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
