package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// tokenDisplayLen is the number of characters stored as prefix/suffix
// for token display (e.g. "abc123...xyz789").
const tokenDisplayLen = 6

// Sentinel errors for authentication failures.
var (
	ErrNoTokens    = errors.New("no tokens configured")
	ErrNoToken     = errors.New("no token provided")
	ErrInvalidToken = errors.New("invalid token")
)

func hashToken(plaintext string) string {
	h := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(h[:])
}

// GenerateToken creates a new random token. Returns the plaintext (shown once)
// and the StoredToken (persisted with hash only).
func GenerateToken(name string, defaultPermissions map[string]Permission) (string, StoredToken, error) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", StoredToken{}, fmt.Errorf("generate token: %w", err)
	}
	plaintext := hex.EncodeToString(bytes[:])
	hash := hashToken(plaintext)

	token := StoredToken{
		Name:        name,
		Hash:        hash,
		Prefix:      plaintext[:tokenDisplayLen],
		Suffix:      plaintext[len(plaintext)-tokenDisplayLen:],
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		Permissions: defaultPermissions,
	}
	return plaintext, token, nil
}

// Authenticate validates a bearer token against stored hashes.
// Returns the matching StoredToken on success, or a sentinel error.
func (s *Settings) Authenticate(plaintext string) (*StoredToken, error) {
	if len(s.Tokens) == 0 {
		return nil, ErrNoTokens
	}
	if plaintext == "" {
		return nil, ErrNoToken
	}
	hash := hashToken(plaintext)
	if tok, _ := s.findTokenByHash(hash); tok != nil {
		return tok, nil
	}
	return nil, ErrInvalidToken
}

// GetPermission returns the permission level for a token+service pair.
// Defaults to PermOn if the permission is not explicitly set for a known token.
// Returns PermOff if the token hash is not found (defense-in-depth; callers
// should authenticate first). Legacy "read"/"full" values are treated as PermOn.
func (s *Settings) GetPermission(tokenHash, serviceName string) Permission {
	tok, _ := s.findTokenByHash(tokenHash)
	if tok == nil {
		return PermOff
	}
	if p, ok := tok.Permissions[serviceName]; ok && p == PermOff {
		return PermOff
	}
	// Default to PermOn: unknown token+service pairs are allowed, and legacy
	// values like "read"/"full" are treated as enabled.
	return PermOn
}

// DeleteToken removes a token by its hash. Does not save; use within store.With.
func (s *Settings) DeleteToken(hash string) {
	s.Tokens = slices.DeleteFunc(s.Tokens, func(t StoredToken) bool { return t.Hash == hash })
}

// RevokeAll removes all tokens. Does not save; use within store.With.
func (s *Settings) RevokeAll() {
	s.Tokens = []StoredToken{}
}

// UpdatePermission sets a specific permission. Does not save; use within store.With.
func (s *Settings) UpdatePermission(hash, service string, perm Permission) {
	tok, _ := s.findTokenByHash(hash)
	if tok == nil {
		return
	}
	if tok.Permissions == nil {
		tok.Permissions = make(map[string]Permission)
	}
	tok.Permissions[service] = perm
}

// IsToolDisabled returns true if a specific tool is disabled for the given token and MCP.
func (s *Settings) IsToolDisabled(tokenHash, mcpID, toolName string) bool {
	tok, _ := s.findTokenByHash(tokenHash)
	if tok == nil || tok.DisabledTools == nil {
		return false
	}
	return slices.Contains(tok.DisabledTools[mcpID], toolName)
}

// SetToolDisabled enables or disables a specific tool for a token+MCP pair.
// Does not save; use within store.With.
func (s *Settings) SetToolDisabled(hash, mcpID, toolName string, disabled bool) {
	tok, _ := s.findTokenByHash(hash)
	if tok == nil {
		return
	}
	if tok.DisabledTools == nil {
		tok.DisabledTools = make(map[string][]string)
	}
	if disabled {
		if !slices.Contains(tok.DisabledTools[mcpID], toolName) {
			tok.DisabledTools[mcpID] = append(tok.DisabledTools[mcpID], toolName)
		}
	} else {
		filtered := slices.DeleteFunc(tok.DisabledTools[mcpID], func(n string) bool { return n == toolName })
		if len(filtered) == 0 {
			delete(tok.DisabledTools, mcpID)
		} else {
			tok.DisabledTools[mcpID] = filtered
		}
	}
}

// SetAllToolsDisabled sets all tools for a token+MCP pair to disabled or enabled.
// Does not save; use within store.With.
func (s *Settings) SetAllToolsDisabled(hash, mcpID string, toolNames []string, disabled bool) {
	tok, _ := s.findTokenByHash(hash)
	if tok == nil {
		return
	}
	if tok.DisabledTools == nil {
		tok.DisabledTools = make(map[string][]string)
	}
	if disabled {
		names := make([]string, len(toolNames))
		copy(names, toolNames)
		tok.DisabledTools[mcpID] = names
	} else {
		delete(tok.DisabledTools, mcpID)
	}
}

// SetContext sets per-MCP context for a token. Context is passed as _meta to
// the external MCP on tool calls, enabling per-token restrictions like allowed_dirs.
// Does not save; use within store.With.
func (s *Settings) SetContext(hash, mcpID string, ctx json.RawMessage) {
	tok, _ := s.findTokenByHash(hash)
	if tok == nil {
		return
	}
	if tok.Context == nil {
		tok.Context = make(map[string]json.RawMessage)
	}
	if len(ctx) == 0 || string(ctx) == "null" {
		delete(tok.Context, mcpID)
	} else {
		tok.Context[mcpID] = ctx
	}
}
