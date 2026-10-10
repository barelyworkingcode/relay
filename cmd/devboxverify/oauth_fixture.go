package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// oauthFixture is a loopback OAuth-protected MCP server: discovery,
// dynamic registration, authorization code with PKCE, and a one-tool MCP that
// answers only the token it issued. Every value it mints is a throwaway.
type oauthFixture struct {
	srv    *http.Server
	origin string

	mu           sync.Mutex
	events       []string
	access       string
	refresh      string
	clientID     string
	clientSecret string
	redirectURI  string
	codes        map[string]oauthCode
}

type oauthCode struct {
	challenge string
	used      bool
}

type oauthHits struct {
	Register, Authorize, CodeExchange, Refresh, MCPAuthorized, MCPUnauthorized int
	PKCEOK                                                                     bool
}

const (
	hitRegister     = "register"
	hitAuthorize    = "authorize"
	hitExchange     = "exchange"
	hitExchangeBad  = "exchange-bad"
	hitRefresh      = "refresh"
	hitMCPAuthed    = "mcp-authorized"
	hitMCPUnauthed  = "mcp-unauthorized"
	fixtureToolName = "oauthprobe_ping"
)

func randomHex() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func startOAuthFixture() (*oauthFixture, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("oauth fixture: %w", err)
	}
	f := &oauthFixture{
		origin:  "http://" + ln.Addr().String(),
		access:  randomHex(),
		refresh: randomHex(),
		codes:   map[string]oauthCode{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", f.handleMCP)
	mux.HandleFunc("/.well-known/oauth-protected-resource", f.handleResourceMetadata)
	mux.HandleFunc("/.well-known/oauth-authorization-server", f.handleServerMetadata)
	mux.HandleFunc("/.well-known/oauth-authorization-server/mcp", f.handleServerMetadata)
	mux.HandleFunc("/register", f.handleRegister)
	mux.HandleFunc("/authorize", f.handleAuthorize)
	mux.HandleFunc("/token", f.handleToken)
	f.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = f.srv.Serve(ln) }()
	return f, nil
}

func (f *oauthFixture) MCPURL() string { return f.origin + "/mcp" }

// Origin is scheme://host:port of the fixture.
func (f *oauthFixture) Origin() string { return f.origin }

// Tokens are the issued bearers. They live in memory only and never go into a detail.
func (f *oauthFixture) Tokens() (access, refresh string) { return f.access, f.refresh }

func (f *oauthFixture) record(kind string) {
	f.mu.Lock()
	f.events = append(f.events, kind)
	f.mu.Unlock()
}

// Mark is the position HitsSince counts from.
func (f *oauthFixture) Mark() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}

func (f *oauthFixture) HitsSince(mark int) oauthHits {
	f.mu.Lock()
	defer f.mu.Unlock()
	var h oauthHits
	bad := false
	for _, k := range f.events[mark:] {
		switch k {
		case hitRegister:
			h.Register++
		case hitAuthorize:
			h.Authorize++
		case hitExchange:
			h.CodeExchange++
		case hitExchangeBad:
			h.CodeExchange++
			bad = true
		case hitRefresh:
			h.Refresh++
		case hitMCPAuthed:
			h.MCPAuthorized++
		case hitMCPUnauthed:
			h.MCPUnauthorized++
		}
	}
	h.PKCEOK = h.CodeExchange > 0 && !bad
	return h
}

func (f *oauthFixture) Close() { _ = f.srv.Close() }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *oauthFixture) handleResourceMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":              f.origin + "/mcp",
		"authorization_servers": []string{f.origin},
		"scopes_supported":      []string{"mcp"},
	})
}

func (f *oauthFixture) handleServerMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                           f.origin,
		"authorization_endpoint":           f.origin + "/authorize",
		"token_endpoint":                   f.origin + "/token",
		"registration_endpoint":            f.origin + "/register",
		"response_types_supported":         []string{"code"},
		"grant_types_supported":            []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported": []string{"S256"},
	})
}

func isLoopbackCallback(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "http" && u.Path == "/oauth/callback" && u.Hostname() == "127.0.0.1" && u.Port() != ""
}

func (f *oauthFixture) handleRegister(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RedirectURIs []string `json:"redirect_uris"`
	}
	if r.Method != http.MethodPost || json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body) != nil ||
		len(body.RedirectURIs) != 1 || !isLoopbackCallback(body.RedirectURIs[0]) {
		http.Error(w, "bad registration", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.clientID, f.clientSecret, f.redirectURI = randomHex(), randomHex(), body.RedirectURIs[0]
	id, secret := f.clientID, f.clientSecret
	f.events = append(f.events, hitRegister)
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]string{"client_id": id, "client_secret": secret})
}

func (f *oauthFixture) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f.mu.Lock()
	ok := f.clientID != "" && q.Get("client_id") == f.clientID && q.Get("redirect_uri") == f.redirectURI &&
		q.Get("response_type") == "code" && q.Get("code_challenge_method") == "S256" &&
		q.Get("code_challenge") != "" && q.Get("state") != ""
	var code string
	if ok {
		code = randomHex()
		f.codes[code] = oauthCode{challenge: q.Get("code_challenge")}
		f.events = append(f.events, hitAuthorize)
	}
	redirect := f.redirectURI
	f.mu.Unlock()
	if !ok {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	loc, _ := url.Parse(redirect)
	rq := loc.Query()
	rq.Set("code", code)
	rq.Set("state", q.Get("state"))
	loc.RawQuery = rq.Encode()
	http.Redirect(w, r, loc.String(), http.StatusFound)
}

func (f *oauthFixture) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.ParseForm() != nil {
		http.Error(w, "bad token request", http.StatusBadRequest)
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "refresh_token":
		f.record(hitRefresh)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
	case "authorization_code":
		f.exchangeCode(w, r)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
	}
}

func (f *oauthFixture) exchangeCode(w http.ResponseWriter, r *http.Request) {
	form := r.PostForm
	f.mu.Lock()
	c, found := f.codes[form.Get("code")]
	sum := sha256.Sum256([]byte(form.Get("code_verifier")))
	good := found && !c.used &&
		form.Get("client_id") == f.clientID && form.Get("client_secret") == f.clientSecret &&
		form.Get("redirect_uri") == f.redirectURI &&
		base64.RawURLEncoding.EncodeToString(sum[:]) == c.challenge
	if found {
		c.used = true
		f.codes[form.Get("code")] = c
	}
	if good {
		f.events = append(f.events, hitExchange)
	} else {
		f.events = append(f.events, hitExchangeBad)
	}
	f.mu.Unlock()
	if !good {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": f.access, "token_type": "Bearer", "expires_in": 3600, "refresh_token": f.refresh,
	})
}

func (f *oauthFixture) bearerOK(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(f.access)) == 1
}

func (f *oauthFixture) handleMCP(w http.ResponseWriter, r *http.Request) {
	if !f.bearerOK(r) {
		f.record(hitMCPUnauthed)
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+f.origin+`/.well-known/oauth-protected-resource"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	f.record(hitMCPAuthed)
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"params"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(req.ID) == 0 || strings.HasPrefix(req.Method, "notifications/") {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	reply := func(result any, rpcErr any) {
		out := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rpcErr != nil {
			out["error"] = rpcErr
		} else {
			out["result"] = result
		}
		writeJSON(w, http.StatusOK, out)
	}
	switch req.Method {
	case "initialize":
		version := req.Params.ProtocolVersion
		if version == "" {
			version = "2025-03-26"
		}
		reply(map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "devboxverify-oauth", "version": "1"},
		}, nil)
	case "tools/list":
		reply(map[string]any{"tools": []map[string]any{{
			"name":        fixtureToolName,
			"description": "Answers pong.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		}}}, nil)
	case "tools/call":
		reply(map[string]any{"content": []map[string]string{{"type": "text", "text": "pong"}}}, nil)
	default:
		reply(nil, map[string]any{"code": -32601, "message": "method not found"})
	}
}
