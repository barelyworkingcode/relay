package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newTestSettings returns a *Settings with the given tokens and MCPs.
func newTestSettings(t *testing.T, tokens []StoredToken, mcps []ExternalMcp) *Settings {
	t.Helper()
	if tokens == nil {
		tokens = []StoredToken{}
	}
	if mcps == nil {
		mcps = []ExternalMcp{}
	}
	return &Settings{
		Version:      1,
		Tokens:       tokens,
		ExternalMcps: mcps,
		Services:     []ServiceConfig{},
	}
}

// generateAndStore creates a token via GenerateToken, appends it to s.Tokens,
// and returns the plaintext and hash.
func generateAndStore(t *testing.T, s *Settings, name string) (plaintext, hash string) {
	t.Helper()
	pt, tok := GenerateToken(name, nil)
	s.Tokens = append(s.Tokens, tok)
	return pt, tok.Hash
}

// ---------------------------------------------------------------------------
// Sentinel errors
// ---------------------------------------------------------------------------

func TestSentinelErrors(t *testing.T) {
	t.Run("ErrNoTokens is distinct", func(t *testing.T) {
		if errors.Is(ErrNoTokens, ErrNoToken) {
			t.Fatal("ErrNoTokens should not match ErrNoToken")
		}
		if errors.Is(ErrNoTokens, ErrInvalidToken) {
			t.Fatal("ErrNoTokens should not match ErrInvalidToken")
		}
	})
	t.Run("ErrNoToken is distinct", func(t *testing.T) {
		if errors.Is(ErrNoToken, ErrInvalidToken) {
			t.Fatal("ErrNoToken should not match ErrInvalidToken")
		}
	})
}

// ---------------------------------------------------------------------------
// Authenticate
// ---------------------------------------------------------------------------

func TestAuthenticate(t *testing.T) {
	t.Run("no tokens configured", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		_, err := s.Authenticate("anything")
		if !errors.Is(err, ErrNoTokens) {
			t.Fatalf("expected ErrNoTokens, got %v", err)
		}
	})

	t.Run("empty token string", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		plaintext, tok := GenerateToken("test", nil)
		_ = plaintext
		s.Tokens = append(s.Tokens, tok)

		_, err := s.Authenticate("")
		if !errors.Is(err, ErrNoToken) {
			t.Fatalf("expected ErrNoToken, got %v", err)
		}
	})

	t.Run("valid token", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		plaintext, tok := GenerateToken("mytoken", nil)
		s.Tokens = append(s.Tokens, tok)

		result, err := s.Authenticate(plaintext)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.Name != "mytoken" {
			t.Fatalf("expected token name 'mytoken', got %q", result.Name)
		}
		if result.Hash != tok.Hash {
			t.Fatal("returned token hash does not match")
		}
	})

	t.Run("invalid token", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		_, tok := GenerateToken("real", nil)
		s.Tokens = append(s.Tokens, tok)

		_, err := s.Authenticate("completely-wrong-token")
		if !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("expected ErrInvalidToken, got %v", err)
		}
	})

	t.Run("multiple tokens finds correct one", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		_, _ = generateAndStore(t, s, "first")
		pt2, _ := generateAndStore(t, s, "second")
		_, _ = generateAndStore(t, s, "third")

		result, err := s.Authenticate(pt2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.Name != "second" {
			t.Fatalf("expected 'second', got %q", result.Name)
		}
	})
}

// ---------------------------------------------------------------------------
// GenerateToken
// ---------------------------------------------------------------------------

func TestGenerateToken(t *testing.T) {
	t.Run("returns 64-char hex plaintext", func(t *testing.T) {
		pt, _ := GenerateToken("t", nil)
		if len(pt) != 64 {
			t.Fatalf("expected 64-char plaintext, got %d chars", len(pt))
		}
	})

	t.Run("stored token fields are populated", func(t *testing.T) {
		pt, tok := GenerateToken("myname", map[string]Permission{"svc": PermOff})
		if tok.Name != "myname" {
			t.Fatalf("expected name 'myname', got %q", tok.Name)
		}
		if tok.Prefix != pt[:6] {
			t.Fatalf("prefix mismatch: got %q, want %q", tok.Prefix, pt[:6])
		}
		if tok.Suffix != pt[len(pt)-6:] {
			t.Fatalf("suffix mismatch: got %q, want %q", tok.Suffix, pt[len(pt)-6:])
		}
		if tok.CreatedAt == "" {
			t.Fatal("CreatedAt should not be empty")
		}
		if tok.Permissions["svc"] != PermOff {
			t.Fatalf("expected PermOff for 'svc', got %q", tok.Permissions["svc"])
		}
	})

	t.Run("hash matches hashToken of plaintext", func(t *testing.T) {
		pt, tok := GenerateToken("check", nil)
		if tok.Hash != hashToken(pt) {
			t.Fatal("hash does not match hashToken(plaintext)")
		}
	})

	t.Run("two calls produce different tokens", func(t *testing.T) {
		pt1, tok1 := GenerateToken("a", nil)
		pt2, tok2 := GenerateToken("b", nil)
		if pt1 == pt2 {
			t.Fatal("two generated plaintexts should differ")
		}
		if tok1.Hash == tok2.Hash {
			t.Fatal("two generated hashes should differ")
		}
	})
}

// ---------------------------------------------------------------------------
// GetPermission
// ---------------------------------------------------------------------------

func TestGetPermission(t *testing.T) {
	t.Run("defaults to PermOn for unknown token", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		if got := s.GetPermission("nonexistent", "svc"); got != PermOn {
			t.Fatalf("expected PermOn, got %q", got)
		}
	})

	t.Run("defaults to PermOn for unset service", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		_, hash := generateAndStore(t, s, "tok")
		if got := s.GetPermission(hash, "unknown-svc"); got != PermOn {
			t.Fatalf("expected PermOn, got %q", got)
		}
	})

	t.Run("returns PermOff when set", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		_, tok := GenerateToken("tok", map[string]Permission{"blocked": PermOff})
		s.Tokens = append(s.Tokens, tok)
		if got := s.GetPermission(tok.Hash, "blocked"); got != PermOff {
			t.Fatalf("expected PermOff, got %q", got)
		}
	})

	t.Run("returns PermOn for explicit PermOn", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		_, tok := GenerateToken("tok", map[string]Permission{"allowed": PermOn})
		s.Tokens = append(s.Tokens, tok)
		if got := s.GetPermission(tok.Hash, "allowed"); got != PermOn {
			t.Fatalf("expected PermOn, got %q", got)
		}
	})

	t.Run("legacy non-off value treated as PermOn", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		_, tok := GenerateToken("tok", map[string]Permission{"svc": "full"})
		s.Tokens = append(s.Tokens, tok)
		if got := s.GetPermission(tok.Hash, "svc"); got != PermOn {
			t.Fatalf("expected PermOn for legacy 'full', got %q", got)
		}
	})
}

// ---------------------------------------------------------------------------
// DeleteToken
// ---------------------------------------------------------------------------

func TestDeleteToken(t *testing.T) {
	t.Run("removes the correct token", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		_, h1 := generateAndStore(t, s, "keep")
		_, h2 := generateAndStore(t, s, "delete")

		s.DeleteToken(h2)
		if len(s.Tokens) != 1 {
			t.Fatalf("expected 1 token, got %d", len(s.Tokens))
		}
		if s.Tokens[0].Hash != h1 {
			t.Fatal("wrong token was removed")
		}
	})

	t.Run("no-op for unknown hash", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		generateAndStore(t, s, "stay")
		s.DeleteToken("bogus")
		if len(s.Tokens) != 1 {
			t.Fatalf("expected 1 token, got %d", len(s.Tokens))
		}
	})
}

// ---------------------------------------------------------------------------
// RevokeAll
// ---------------------------------------------------------------------------

func TestRevokeAll(t *testing.T) {
	s := newTestSettings(t, nil, nil)
	generateAndStore(t, s, "a")
	generateAndStore(t, s, "b")
	generateAndStore(t, s, "c")

	s.RevokeAll()
	if len(s.Tokens) != 0 {
		t.Fatalf("expected 0 tokens after RevokeAll, got %d", len(s.Tokens))
	}
}

// ---------------------------------------------------------------------------
// UpdatePermission
// ---------------------------------------------------------------------------

func TestUpdatePermission(t *testing.T) {
	t.Run("sets permission on existing token", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		_, hash := generateAndStore(t, s, "tok")

		s.UpdatePermission(hash, "mcp1", PermOff)
		if got := s.GetPermission(hash, "mcp1"); got != PermOff {
			t.Fatalf("expected PermOff, got %q", got)
		}

		s.UpdatePermission(hash, "mcp1", PermOn)
		if got := s.GetPermission(hash, "mcp1"); got != PermOn {
			t.Fatalf("expected PermOn, got %q", got)
		}
	})

	t.Run("no-op for unknown hash", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		// Should not panic.
		s.UpdatePermission("nonexistent", "svc", PermOff)
	})

	t.Run("initializes nil permissions map", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		tok := StoredToken{Name: "bare", Hash: "h1"}
		s.Tokens = append(s.Tokens, tok)

		s.UpdatePermission("h1", "svc", PermOff)
		if s.Tokens[0].Permissions == nil {
			t.Fatal("permissions map should have been initialized")
		}
		if s.Tokens[0].Permissions["svc"] != PermOff {
			t.Fatalf("expected PermOff, got %q", s.Tokens[0].Permissions["svc"])
		}
	})
}

// ---------------------------------------------------------------------------
// AddExternalMcp / RemoveExternalMcp
// ---------------------------------------------------------------------------

func TestAddExternalMcp(t *testing.T) {
	t.Run("adds MCP and defaults token permissions to PermOff", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		_, hash := generateAndStore(t, s, "tok")

		mcp := ExternalMcp{ID: "mcp1", DisplayName: "Test MCP"}
		s.AddExternalMcp(mcp)

		if len(s.ExternalMcps) != 1 {
			t.Fatalf("expected 1 MCP, got %d", len(s.ExternalMcps))
		}
		if s.ExternalMcps[0].ID != "mcp1" {
			t.Fatalf("expected ID 'mcp1', got %q", s.ExternalMcps[0].ID)
		}
		if got := s.GetPermission(hash, "mcp1"); got != PermOff {
			t.Fatalf("new MCP should default to PermOff for existing tokens, got %q", got)
		}
	})

	t.Run("does not overwrite existing permission", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		_, tok := GenerateToken("tok", map[string]Permission{"mcp1": PermOn})
		s.Tokens = append(s.Tokens, tok)

		mcp := ExternalMcp{ID: "mcp1", DisplayName: "Test MCP"}
		s.AddExternalMcp(mcp)

		if got := s.GetPermission(tok.Hash, "mcp1"); got != PermOn {
			t.Fatalf("pre-existing PermOn should be preserved, got %q", got)
		}
	})
}

func TestRemoveExternalMcp(t *testing.T) {
	t.Run("removes MCP and cleans up token permissions", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		_, hash := generateAndStore(t, s, "tok")

		mcp := ExternalMcp{ID: "mcp1", DisplayName: "Test MCP"}
		s.AddExternalMcp(mcp)

		// Also set up a disabled tool for this MCP.
		s.SetToolDisabled(hash, "mcp1", "tool1", true)

		s.RemoveExternalMcp("mcp1")

		if len(s.ExternalMcps) != 0 {
			t.Fatalf("expected 0 MCPs, got %d", len(s.ExternalMcps))
		}
		// Permission for removed MCP should be gone.
		tok, _ := s.findTokenByHash(hash)
		if _, exists := tok.Permissions["mcp1"]; exists {
			t.Fatal("permission for removed MCP should be deleted")
		}
		if _, exists := tok.DisabledTools["mcp1"]; exists {
			t.Fatal("disabled tools for removed MCP should be deleted")
		}
	})

	t.Run("no-op for unknown ID", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		s.AddExternalMcp(ExternalMcp{ID: "keep"})
		s.RemoveExternalMcp("nonexistent")
		if len(s.ExternalMcps) != 1 {
			t.Fatalf("expected 1 MCP, got %d", len(s.ExternalMcps))
		}
	})
}

// ---------------------------------------------------------------------------
// UpdateExternalMcp
// ---------------------------------------------------------------------------

func TestUpdateExternalMcp(t *testing.T) {
	t.Run("preserves DiscoveredTools when new has none", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		s.AddExternalMcp(ExternalMcp{
			ID:              "mcp1",
			DisplayName:     "Old",
			Command:         "/old",
			DiscoveredTools: []ToolInfo{{Name: "t1", Description: "desc1"}},
		})

		s.UpdateExternalMcp(ExternalMcp{
			ID:          "mcp1",
			DisplayName: "New",
			Command:     "/new",
			// DiscoveredTools is nil/empty.
		})

		mcp, _ := s.findMcpByID("mcp1")
		if mcp == nil {
			t.Fatal("MCP should still exist after update")
		}
		if mcp.DisplayName != "New" {
			t.Fatalf("expected DisplayName 'New', got %q", mcp.DisplayName)
		}
		if mcp.Command != "/new" {
			t.Fatalf("expected Command '/new', got %q", mcp.Command)
		}
		if len(mcp.DiscoveredTools) != 1 || mcp.DiscoveredTools[0].Name != "t1" {
			t.Fatal("DiscoveredTools should be preserved from old config")
		}
	})

	t.Run("replaces DiscoveredTools when new has them", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		s.AddExternalMcp(ExternalMcp{
			ID:              "mcp1",
			DiscoveredTools: []ToolInfo{{Name: "old_tool"}},
		})

		s.UpdateExternalMcp(ExternalMcp{
			ID:              "mcp1",
			DiscoveredTools: []ToolInfo{{Name: "new_tool"}},
		})

		mcp, _ := s.findMcpByID("mcp1")
		if len(mcp.DiscoveredTools) != 1 || mcp.DiscoveredTools[0].Name != "new_tool" {
			t.Fatal("DiscoveredTools should be replaced with new values")
		}
	})

	t.Run("no-op for unknown ID", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		// Should not panic.
		s.UpdateExternalMcp(ExternalMcp{ID: "nonexistent", DisplayName: "Ghost"})
	})
}

// ---------------------------------------------------------------------------
// IsToolDisabled / SetToolDisabled
// ---------------------------------------------------------------------------

func TestIsToolDisabled(t *testing.T) {
	t.Run("false for unknown token", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		if s.IsToolDisabled("nope", "mcp1", "t1") {
			t.Fatal("expected false for unknown token")
		}
	})

	t.Run("false by default", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		_, hash := generateAndStore(t, s, "tok")
		if s.IsToolDisabled(hash, "mcp1", "t1") {
			t.Fatal("expected false by default")
		}
	})

	t.Run("true after disabling", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		_, hash := generateAndStore(t, s, "tok")
		s.SetToolDisabled(hash, "mcp1", "t1", true)

		if !s.IsToolDisabled(hash, "mcp1", "t1") {
			t.Fatal("expected true after disabling")
		}
	})

	t.Run("false after re-enabling", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		_, hash := generateAndStore(t, s, "tok")
		s.SetToolDisabled(hash, "mcp1", "t1", true)
		s.SetToolDisabled(hash, "mcp1", "t1", false)

		if s.IsToolDisabled(hash, "mcp1", "t1") {
			t.Fatal("expected false after re-enabling")
		}
	})
}

func TestSetToolDisabled(t *testing.T) {
	t.Run("disable is idempotent", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		_, hash := generateAndStore(t, s, "tok")
		s.SetToolDisabled(hash, "mcp1", "t1", true)
		s.SetToolDisabled(hash, "mcp1", "t1", true) // duplicate

		tok, _ := s.findTokenByHash(hash)
		if len(tok.DisabledTools["mcp1"]) != 1 {
			t.Fatalf("expected 1 disabled tool, got %d", len(tok.DisabledTools["mcp1"]))
		}
	})

	t.Run("enable cleans up empty list", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		_, hash := generateAndStore(t, s, "tok")
		s.SetToolDisabled(hash, "mcp1", "t1", true)
		s.SetToolDisabled(hash, "mcp1", "t1", false)

		tok, _ := s.findTokenByHash(hash)
		if _, exists := tok.DisabledTools["mcp1"]; exists {
			t.Fatal("empty disabled tool list should be removed from map")
		}
	})

	t.Run("enable keeps other tools disabled", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		_, hash := generateAndStore(t, s, "tok")
		s.SetToolDisabled(hash, "mcp1", "t1", true)
		s.SetToolDisabled(hash, "mcp1", "t2", true)
		s.SetToolDisabled(hash, "mcp1", "t1", false)

		tok, _ := s.findTokenByHash(hash)
		if len(tok.DisabledTools["mcp1"]) != 1 {
			t.Fatalf("expected 1 remaining disabled tool, got %d", len(tok.DisabledTools["mcp1"]))
		}
		if tok.DisabledTools["mcp1"][0] != "t2" {
			t.Fatalf("expected 't2' to remain, got %q", tok.DisabledTools["mcp1"][0])
		}
	})

	t.Run("no-op for unknown token", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		// Should not panic.
		s.SetToolDisabled("nonexistent", "mcp1", "t1", true)
	})
}

// ---------------------------------------------------------------------------
// SetAllToolsDisabled
// ---------------------------------------------------------------------------

func TestSetAllToolsDisabled(t *testing.T) {
	t.Run("disable all", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		_, hash := generateAndStore(t, s, "tok")

		tools := []string{"t1", "t2", "t3"}
		s.SetAllToolsDisabled(hash, "mcp1", tools, true)

		for _, name := range tools {
			if !s.IsToolDisabled(hash, "mcp1", name) {
				t.Fatalf("expected %q to be disabled", name)
			}
		}
	})

	t.Run("enable all removes key", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		_, hash := generateAndStore(t, s, "tok")

		s.SetAllToolsDisabled(hash, "mcp1", []string{"t1", "t2"}, true)
		s.SetAllToolsDisabled(hash, "mcp1", []string{"t1", "t2"}, false)

		tok, _ := s.findTokenByHash(hash)
		if _, exists := tok.DisabledTools["mcp1"]; exists {
			t.Fatal("enabling all should remove the MCP key from DisabledTools")
		}
	})

	t.Run("disable all makes a copy of input slice", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		_, hash := generateAndStore(t, s, "tok")

		tools := []string{"t1", "t2"}
		s.SetAllToolsDisabled(hash, "mcp1", tools, true)

		// Mutate the original slice.
		tools[0] = "mutated"

		tok, _ := s.findTokenByHash(hash)
		if tok.DisabledTools["mcp1"][0] != "t1" {
			t.Fatal("SetAllToolsDisabled should copy, not alias the input slice")
		}
	})

	t.Run("no-op for unknown token", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		// Should not panic.
		s.SetAllToolsDisabled("nonexistent", "mcp1", []string{"t1"}, true)
	})
}

// ---------------------------------------------------------------------------
// SetContext
// ---------------------------------------------------------------------------

func TestSetContext(t *testing.T) {
	t.Run("set context value", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		_, hash := generateAndStore(t, s, "tok")

		ctx := json.RawMessage(`{"allowed_dirs": ["/tmp"]}`)
		s.SetContext(hash, "mcp1", ctx)

		tok, _ := s.findTokenByHash(hash)
		if tok.Context == nil {
			t.Fatal("Context map should be initialized")
		}
		if string(tok.Context["mcp1"]) != `{"allowed_dirs": ["/tmp"]}` {
			t.Fatalf("unexpected context: %s", string(tok.Context["mcp1"]))
		}
	})

	t.Run("clear context with empty value", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		_, hash := generateAndStore(t, s, "tok")

		s.SetContext(hash, "mcp1", json.RawMessage(`{"key":"val"}`))
		s.SetContext(hash, "mcp1", json.RawMessage{})

		tok, _ := s.findTokenByHash(hash)
		if _, exists := tok.Context["mcp1"]; exists {
			t.Fatal("empty context should delete the key")
		}
	})

	t.Run("clear context with null", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		_, hash := generateAndStore(t, s, "tok")

		s.SetContext(hash, "mcp1", json.RawMessage(`{"key":"val"}`))
		s.SetContext(hash, "mcp1", json.RawMessage(`null`))

		tok, _ := s.findTokenByHash(hash)
		if _, exists := tok.Context["mcp1"]; exists {
			t.Fatal("null context should delete the key")
		}
	})

	t.Run("no-op for unknown token", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		// Should not panic.
		s.SetContext("nonexistent", "mcp1", json.RawMessage(`{}`))
	})
}

// ---------------------------------------------------------------------------
// findTokenByHash / findMcpByID / findServiceByID
// ---------------------------------------------------------------------------

func TestFindTokenByHash(t *testing.T) {
	t.Run("finds existing token", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		_, hash := generateAndStore(t, s, "target")

		tok, idx := s.findTokenByHash(hash)
		if tok == nil {
			t.Fatal("expected to find token")
		}
		if tok.Name != "target" {
			t.Fatalf("expected name 'target', got %q", tok.Name)
		}
		if idx != 0 {
			t.Fatalf("expected index 0, got %d", idx)
		}
	})

	t.Run("returns nil for missing hash", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		tok, idx := s.findTokenByHash("missing")
		if tok != nil {
			t.Fatal("expected nil for missing hash")
		}
		if idx != -1 {
			t.Fatalf("expected index -1, got %d", idx)
		}
	})

	t.Run("returns pointer into slice (mutations apply)", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		_, hash := generateAndStore(t, s, "mutable")

		tok, _ := s.findTokenByHash(hash)
		tok.Name = "changed"

		if s.Tokens[0].Name != "changed" {
			t.Fatal("mutation via returned pointer should affect the slice")
		}
	})
}

func TestFindMcpByID(t *testing.T) {
	t.Run("finds existing MCP", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		s.AddExternalMcp(ExternalMcp{ID: "mcp1", DisplayName: "First"})
		s.AddExternalMcp(ExternalMcp{ID: "mcp2", DisplayName: "Second"})

		mcp, idx := s.findMcpByID("mcp2")
		if mcp == nil {
			t.Fatal("expected to find MCP")
		}
		if mcp.DisplayName != "Second" {
			t.Fatalf("expected 'Second', got %q", mcp.DisplayName)
		}
		if idx != 1 {
			t.Fatalf("expected index 1, got %d", idx)
		}
	})

	t.Run("returns nil for missing ID", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		mcp, idx := s.findMcpByID("nope")
		if mcp != nil {
			t.Fatal("expected nil for missing ID")
		}
		if idx != -1 {
			t.Fatalf("expected index -1, got %d", idx)
		}
	})
}

func TestFindServiceByID(t *testing.T) {
	t.Run("finds existing service", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		s.AddService(ServiceConfig{ID: "svc1", DisplayName: "Alpha"})
		s.AddService(ServiceConfig{ID: "svc2", DisplayName: "Beta"})

		svc, idx := s.findServiceByID("svc1")
		if svc == nil {
			t.Fatal("expected to find service")
		}
		if svc.DisplayName != "Alpha" {
			t.Fatalf("expected 'Alpha', got %q", svc.DisplayName)
		}
		if idx != 0 {
			t.Fatalf("expected index 0, got %d", idx)
		}
	})

	t.Run("returns nil for missing ID", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		svc, idx := s.findServiceByID("nope")
		if svc != nil {
			t.Fatal("expected nil for missing ID")
		}
		if idx != -1 {
			t.Fatalf("expected index -1, got %d", idx)
		}
	})
}

// ---------------------------------------------------------------------------
// Service helpers
// ---------------------------------------------------------------------------

func TestAddRemoveService(t *testing.T) {
	t.Run("add and remove", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		s.AddService(ServiceConfig{ID: "svc1", DisplayName: "One"})
		s.AddService(ServiceConfig{ID: "svc2", DisplayName: "Two"})

		if len(s.Services) != 2 {
			t.Fatalf("expected 2 services, got %d", len(s.Services))
		}

		s.RemoveService("svc1")
		if len(s.Services) != 1 {
			t.Fatalf("expected 1 service, got %d", len(s.Services))
		}
		if s.Services[0].ID != "svc2" {
			t.Fatal("wrong service was removed")
		}
	})
}

func TestUpdateService(t *testing.T) {
	t.Run("updates existing service", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		s.AddService(ServiceConfig{ID: "svc1", DisplayName: "Old", Command: "/old"})
		s.UpdateService(ServiceConfig{ID: "svc1", DisplayName: "New", Command: "/new"})

		svc, _ := s.findServiceByID("svc1")
		if svc.DisplayName != "New" {
			t.Fatalf("expected 'New', got %q", svc.DisplayName)
		}
		if svc.Command != "/new" {
			t.Fatalf("expected '/new', got %q", svc.Command)
		}
	})

	t.Run("no-op for unknown ID", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		// Should not panic.
		s.UpdateService(ServiceConfig{ID: "nonexistent"})
	})
}

// ---------------------------------------------------------------------------
// UpdateOAuthState / UpdateDiscoveredTools / UpdateContextSchema
// ---------------------------------------------------------------------------

func TestUpdateOAuthState(t *testing.T) {
	s := newTestSettings(t, nil, nil)
	s.AddExternalMcp(ExternalMcp{ID: "mcp1", Transport: "http", URL: "https://example.com"})

	oauth := &OAuthState{ClientID: "cid", AccessToken: "at"}
	s.UpdateOAuthState("mcp1", oauth)

	mcp, _ := s.findMcpByID("mcp1")
	if mcp.OAuthState == nil {
		t.Fatal("OAuthState should be set")
	}
	if mcp.OAuthState.ClientID != "cid" {
		t.Fatalf("expected ClientID 'cid', got %q", mcp.OAuthState.ClientID)
	}

	// No-op for unknown.
	s.UpdateOAuthState("nonexistent", oauth)
}

func TestUpdateDiscoveredTools(t *testing.T) {
	s := newTestSettings(t, nil, nil)
	s.AddExternalMcp(ExternalMcp{ID: "mcp1"})

	tools := []ToolInfo{{Name: "tool1", Description: "desc"}}
	s.UpdateDiscoveredTools("mcp1", tools)

	mcp, _ := s.findMcpByID("mcp1")
	if len(mcp.DiscoveredTools) != 1 || mcp.DiscoveredTools[0].Name != "tool1" {
		t.Fatal("DiscoveredTools should be updated")
	}

	// No-op for unknown.
	s.UpdateDiscoveredTools("nonexistent", tools)
}

func TestUpdateContextSchema(t *testing.T) {
	s := newTestSettings(t, nil, nil)
	s.AddExternalMcp(ExternalMcp{ID: "mcp1"})

	schema := json.RawMessage(`{"type":"object"}`)
	s.UpdateContextSchema("mcp1", schema)

	mcp, _ := s.findMcpByID("mcp1")
	if string(mcp.ContextSchema) != `{"type":"object"}` {
		t.Fatalf("unexpected ContextSchema: %s", string(mcp.ContextSchema))
	}

	// No-op for unknown.
	s.UpdateContextSchema("nonexistent", schema)
}

// ---------------------------------------------------------------------------
// AllExternalMcpIDs
// ---------------------------------------------------------------------------

func TestAllExternalMcpIDs(t *testing.T) {
	s := newTestSettings(t, nil, nil)
	s.AddExternalMcp(ExternalMcp{ID: "b"})
	s.AddExternalMcp(ExternalMcp{ID: "a"})
	s.AddExternalMcp(ExternalMcp{ID: "c"})

	ids := s.AllExternalMcpIDs()
	if len(ids) != 3 {
		t.Fatalf("expected 3 IDs, got %d", len(ids))
	}
	// Order should match insertion order.
	expected := []string{"b", "a", "c"}
	for i, want := range expected {
		if ids[i] != want {
			t.Fatalf("ids[%d] = %q, want %q", i, ids[i], want)
		}
	}
}

// ---------------------------------------------------------------------------
// ExternalMcp.IsHTTP
// ---------------------------------------------------------------------------

func TestIsHTTP(t *testing.T) {
	t.Run("http transport", func(t *testing.T) {
		m := &ExternalMcp{Transport: "http"}
		if !m.IsHTTP() {
			t.Fatal("expected true for http transport")
		}
	})
	t.Run("stdio transport", func(t *testing.T) {
		m := &ExternalMcp{Transport: "stdio"}
		if m.IsHTTP() {
			t.Fatal("expected false for stdio transport")
		}
	})
	t.Run("empty transport", func(t *testing.T) {
		m := &ExternalMcp{}
		if m.IsHTTP() {
			t.Fatal("expected false for empty transport")
		}
	})
}

// ---------------------------------------------------------------------------
// hashToken
// ---------------------------------------------------------------------------

func TestHashToken(t *testing.T) {
	t.Run("deterministic", func(t *testing.T) {
		h1 := hashToken("hello")
		h2 := hashToken("hello")
		if h1 != h2 {
			t.Fatal("same input should produce same hash")
		}
	})

	t.Run("different inputs produce different hashes", func(t *testing.T) {
		h1 := hashToken("hello")
		h2 := hashToken("world")
		if h1 == h2 {
			t.Fatal("different inputs should produce different hashes")
		}
	})

	t.Run("output is 64-char hex", func(t *testing.T) {
		h := hashToken("test")
		if len(h) != 64 {
			t.Fatalf("expected 64-char hex, got %d chars", len(h))
		}
	})
}

// ---------------------------------------------------------------------------
// defaultSettings
// ---------------------------------------------------------------------------

func TestDefaultSettings(t *testing.T) {
	s := defaultSettings()
	if s.Version != 1 {
		t.Fatalf("expected version 1, got %d", s.Version)
	}
	if s.Tokens == nil || len(s.Tokens) != 0 {
		t.Fatal("Tokens should be non-nil empty slice")
	}
	if s.ExternalMcps == nil || len(s.ExternalMcps) != 0 {
		t.Fatal("ExternalMcps should be non-nil empty slice")
	}
	if s.Services == nil || len(s.Services) != 0 {
		t.Fatal("Services should be non-nil empty slice")
	}
}

// ---------------------------------------------------------------------------
// Settings cache: Get, Reload, With
// ---------------------------------------------------------------------------

// These tests create fresh SettingsStore instances for each test, ensuring
// isolation without needing to manipulate global state. Since settingsDir()
// and settingsPath() are not injectable, these tests use the actual config
// dir but clean up carefully.

func TestSettingsCache(t *testing.T) {
	store := NewSettingsStore()

	// Ensure the settings directory exists.
	dir := settingsDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("could not create settings dir: %v", err)
	}

	// Back up existing settings.json if present.
	sp := settingsPath()
	origData, origErr := os.ReadFile(sp)
	t.Cleanup(func() {
		if origErr == nil {
			_ = os.WriteFile(sp, origData, 0600)
		} else {
			_ = os.Remove(sp)
		}
	})

	t.Run("Get returns defaults when no file and no cache", func(t *testing.T) {
		store = NewSettingsStore()
		_ = os.Remove(sp)

		s := store.Get()
		if s == nil {
			t.Fatal("Get should never return nil")
		}
		if s.Version != 1 {
			t.Fatalf("expected version 1, got %d", s.Version)
		}
		if len(s.Tokens) != 0 {
			t.Fatal("expected no tokens in default settings")
		}
	})

	t.Run("Get returns distinct snapshots on each call", func(t *testing.T) {
		store = NewSettingsStore()

		s1 := store.Get()
		s2 := store.Get()
		if s1 == s2 {
			t.Fatal("Get should return distinct snapshot pointers")
		}
		if s1.Version != s2.Version || len(s1.Tokens) != len(s2.Tokens) {
			t.Fatal("snapshots should be structurally equal")
		}
	})

	t.Run("With writes to disk and updates cache", func(t *testing.T) {
		store = NewSettingsStore()

		err := store.With(func(s *Settings) {
			s.Tokens = append(s.Tokens, StoredToken{
				Name: "cache-test",
				Hash: "cache-test-hash",
			})
		})
		if err != nil {
			t.Fatalf("With failed: %v", err)
		}

		s := store.Get()
		found := false
		for _, tok := range s.Tokens {
			if tok.Name == "cache-test" {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("cached settings should contain token added by With")
		}

		data, err := os.ReadFile(sp)
		if err != nil {
			t.Fatalf("failed to read settings from disk: %v", err)
		}
		var diskSettings Settings
		if err := json.Unmarshal(data, &diskSettings); err != nil {
			t.Fatalf("failed to parse settings from disk: %v", err)
		}
		found = false
		for _, tok := range diskSettings.Tokens {
			if tok.Name == "cache-test" {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("disk settings should contain token added by With")
		}
	})

	t.Run("Reload refreshes cache from disk", func(t *testing.T) {
		store = NewSettingsStore()

		err := store.With(func(s *Settings) {
			s.Tokens = []StoredToken{{Name: "original", Hash: "orig-hash"}}
		})
		if err != nil {
			t.Fatalf("With failed: %v", err)
		}

		modified := Settings{
			Version: 1,
			Tokens: []StoredToken{
				{Name: "disk-written", Hash: "disk-hash"},
			},
			ExternalMcps: []ExternalMcp{},
			Services:     []ServiceConfig{},
		}
		data, _ := json.Marshal(modified)
		if err := os.WriteFile(sp, data, 0600); err != nil {
			t.Fatalf("failed to write modified settings: %v", err)
		}

		s := store.Get()
		if len(s.Tokens) == 0 || s.Tokens[0].Name != "original" {
			t.Fatal("Get should return cached (stale) value")
		}

		s = store.Reload()
		if len(s.Tokens) == 0 || s.Tokens[0].Name != "disk-written" {
			t.Fatalf("Reload should return fresh disk data, got %+v", s.Tokens)
		}

		s = store.Get()
		if len(s.Tokens) == 0 || s.Tokens[0].Name != "disk-written" {
			t.Fatal("Get after Reload should return refreshed data")
		}
	})

	t.Run("With generates AdminSecret if missing", func(t *testing.T) {
		store = NewSettingsStore()
		_ = os.Remove(sp)

		err := store.With(func(s *Settings) {
			// no-op mutation; ensureAdminSecret runs before fn.
		})
		if err != nil {
			t.Fatalf("With failed: %v", err)
		}

		s := store.Get()
		if s.AdminSecret == "" {
			t.Fatal("AdminSecret should be auto-generated by With")
		}
		if len(s.AdminSecret) != 32 {
			t.Fatalf("AdminSecret should be 32 hex chars, got %d", len(s.AdminSecret))
		}
	})
}

// ---------------------------------------------------------------------------
// deepCopySettings: map isolation
// ---------------------------------------------------------------------------

func TestDeepCopySettings_MapIsolation(t *testing.T) {
	original := &Settings{
		Version: 1,
		Tokens: []StoredToken{{
			Name: "t1",
			Hash: "h1",
			Permissions:   map[string]Permission{"mcp-a": PermOn},
			DisabledTools: map[string][]string{"mcp-a": {"tool1"}},
			Context:       map[string]json.RawMessage{"mcp-a": json.RawMessage(`{"key":"val"}`)},
		}},
		ExternalMcps: []ExternalMcp{{
			ID:              "mcp-a",
			DisplayName:     "A",
			Env:             map[string]string{"FOO": "bar"},
			DiscoveredTools: []ToolInfo{{Name: "tool1"}},
		}},
		Services: []ServiceConfig{{
			ID:  "svc-a",
			Env: map[string]string{"BAZ": "qux"},
		}},
	}

	cp := deepCopySettings(original)

	// Mutate every map and slice in the copy.
	cp.Tokens[0].Permissions["mcp-a"] = PermOff
	cp.Tokens[0].Permissions["mcp-new"] = PermOn
	cp.Tokens[0].DisabledTools["mcp-a"] = append(cp.Tokens[0].DisabledTools["mcp-a"], "tool2")
	cp.Tokens[0].DisabledTools["mcp-new"] = []string{"x"}
	cp.Tokens[0].Context["mcp-a"] = json.RawMessage(`{"changed":true}`)
	cp.Tokens[0].Context["mcp-new"] = json.RawMessage(`{}`)
	cp.ExternalMcps[0].Env["FOO"] = "changed"
	cp.ExternalMcps[0].Env["NEW"] = "added"
	cp.ExternalMcps[0].DiscoveredTools = append(cp.ExternalMcps[0].DiscoveredTools, ToolInfo{Name: "tool2"})
	cp.Services[0].Env["BAZ"] = "changed"

	// Verify original is untouched.
	if original.Tokens[0].Permissions["mcp-a"] != PermOn {
		t.Fatal("original token Permissions was corrupted")
	}
	if _, ok := original.Tokens[0].Permissions["mcp-new"]; ok {
		t.Fatal("original token Permissions has unexpected key")
	}
	if len(original.Tokens[0].DisabledTools["mcp-a"]) != 1 {
		t.Fatalf("original DisabledTools corrupted: got %v", original.Tokens[0].DisabledTools["mcp-a"])
	}
	if _, ok := original.Tokens[0].DisabledTools["mcp-new"]; ok {
		t.Fatal("original DisabledTools has unexpected key")
	}
	if string(original.Tokens[0].Context["mcp-a"]) != `{"key":"val"}` {
		t.Fatalf("original Context corrupted: got %s", original.Tokens[0].Context["mcp-a"])
	}
	if _, ok := original.Tokens[0].Context["mcp-new"]; ok {
		t.Fatal("original Context has unexpected key")
	}
	if original.ExternalMcps[0].Env["FOO"] != "bar" {
		t.Fatal("original ExternalMcp Env was corrupted")
	}
	if _, ok := original.ExternalMcps[0].Env["NEW"]; ok {
		t.Fatal("original ExternalMcp Env has unexpected key")
	}
	if len(original.ExternalMcps[0].DiscoveredTools) != 1 {
		t.Fatal("original DiscoveredTools was corrupted")
	}
	if original.Services[0].Env["BAZ"] != "qux" {
		t.Fatal("original Service Env was corrupted")
	}
}

// TestDeepCopySettings_AllFieldsCovered uses reflection to verify that every
// map and slice field in Settings (and nested types) is properly deep-copied.
// This catches regressions when new fields are added but deepCopySettings is
// not updated.
func TestDeepCopySettings_AllFieldsCovered(t *testing.T) {
	original := &Settings{
		Version: 1,
		Tokens: []StoredToken{{
			Name:          "t1",
			Hash:          "h1",
			Permissions:   map[string]Permission{"x": PermOn},
			DisabledTools: map[string][]string{"x": {"tool1"}},
			Context:       map[string]json.RawMessage{"x": json.RawMessage(`{}`)},
		}},
		ExternalMcps: []ExternalMcp{{
			ID:              "mcp1",
			Env:             map[string]string{"K": "V"},
			DiscoveredTools: []ToolInfo{{Name: "t"}},
		}},
		Services: []ServiceConfig{{
			ID:  "svc1",
			Env: map[string]string{"A": "B"},
		}},
	}

	cp := deepCopySettings(original)

	// Check top-level slices are different pointers.
	checkSliceCopy(t, "Tokens", original.Tokens, cp.Tokens)
	checkSliceCopy(t, "ExternalMcps", original.ExternalMcps, cp.ExternalMcps)
	checkSliceCopy(t, "Services", original.Services, cp.Services)

	// Check nested maps in Tokens.
	checkMapCopy(t, "Tokens[0].Permissions", original.Tokens[0].Permissions, cp.Tokens[0].Permissions)
	checkMapCopy(t, "Tokens[0].DisabledTools", original.Tokens[0].DisabledTools, cp.Tokens[0].DisabledTools)
	checkMapCopy(t, "Tokens[0].Context", original.Tokens[0].Context, cp.Tokens[0].Context)

	// Check nested maps/slices in ExternalMcps.
	checkMapCopy(t, "ExternalMcps[0].Env", original.ExternalMcps[0].Env, cp.ExternalMcps[0].Env)
	checkSliceCopy(t, "ExternalMcps[0].DiscoveredTools", original.ExternalMcps[0].DiscoveredTools, cp.ExternalMcps[0].DiscoveredTools)

	// Check nested maps in Services.
	checkMapCopy(t, "Services[0].Env", original.Services[0].Env, cp.Services[0].Env)
}

func checkSliceCopy[T any](t *testing.T, name string, orig, cp []T) {
	t.Helper()
	if len(orig) == 0 {
		return
	}
	if &orig[0] == &cp[0] {
		t.Errorf("deepCopySettings: %s shares backing array with original", name)
	}
}

func checkMapCopy[K comparable, V any](t *testing.T, name string, orig, cp map[K]V) {
	t.Helper()
	if orig == nil {
		return
	}
	// Mutate the copy and verify the original is unchanged.
	origLen := len(orig)
	var zeroK K
	for k := range cp {
		zeroK = k
		break
	}
	delete(cp, zeroK)
	if len(orig) != origLen {
		t.Errorf("deepCopySettings: %s shares map with original", name)
	}
	// Restore the deleted key (best effort).
	var zeroV V
	cp[zeroK] = zeroV
}

// ---------------------------------------------------------------------------
// loadSettingsInternal: JSON round-trip and normalization
// ---------------------------------------------------------------------------

func TestLoadSettingsInternal(t *testing.T) {
	sp := settingsPath()
	dir := settingsDir()
	_ = os.MkdirAll(dir, 0700)
	origData, origErr := os.ReadFile(sp)
	t.Cleanup(func() {
		if origErr == nil {
			_ = os.WriteFile(sp, origData, 0600)
		} else {
			_ = os.Remove(sp)
		}
	})

	t.Run("sets version to 1 if missing", func(t *testing.T) {
		data := []byte(`{"tokens":[],"external_mcps":[],"services":[]}`)
		if err := os.WriteFile(sp, data, 0600); err != nil {
			t.Fatal(err)
		}
		s := loadSettingsInternal()
		if s.Version != 1 {
			t.Fatalf("expected version 1, got %d", s.Version)
		}
	})

	t.Run("ensures nil slices become non-nil", func(t *testing.T) {
		data := []byte(`{"version":1}`)
		if err := os.WriteFile(sp, data, 0600); err != nil {
			t.Fatal(err)
		}
		s := loadSettingsInternal()
		if s.Tokens == nil {
			t.Fatal("Tokens should not be nil")
		}
		if s.ExternalMcps == nil {
			t.Fatal("ExternalMcps should not be nil")
		}
		if s.Services == nil {
			t.Fatal("Services should not be nil")
		}
	})

	t.Run("ensures nil DiscoveredTools in MCPs become non-nil", func(t *testing.T) {
		data := []byte(`{"version":1,"external_mcps":[{"id":"m1","args":[],"env":{}}]}`)
		if err := os.WriteFile(sp, data, 0600); err != nil {
			t.Fatal(err)
		}
		s := loadSettingsInternal()
		if len(s.ExternalMcps) != 1 {
			t.Fatalf("expected 1 MCP, got %d", len(s.ExternalMcps))
		}
		if s.ExternalMcps[0].DiscoveredTools == nil {
			t.Fatal("DiscoveredTools should not be nil")
		}
	})

	t.Run("returns defaults for missing file", func(t *testing.T) {
		_ = os.Remove(sp)
		s := loadSettingsInternal()
		if s.Version != 1 {
			t.Fatalf("expected version 1, got %d", s.Version)
		}
	})

	t.Run("returns defaults for invalid JSON", func(t *testing.T) {
		if err := os.WriteFile(sp, []byte(`{not json`), 0600); err != nil {
			t.Fatal(err)
		}
		s := loadSettingsInternal()
		if s.Version != 1 {
			t.Fatalf("expected version 1 for invalid JSON, got %d", s.Version)
		}
	})
}

// ---------------------------------------------------------------------------
// saveSettingsInternal: atomic write
// ---------------------------------------------------------------------------

func TestSaveSettingsInternal(t *testing.T) {
	sp := settingsPath()
	dir := settingsDir()
	_ = os.MkdirAll(dir, 0700)
	origData, origErr := os.ReadFile(sp)
	t.Cleanup(func() {
		if origErr == nil {
			_ = os.WriteFile(sp, origData, 0600)
		} else {
			_ = os.Remove(sp)
		}
	})

	t.Run("writes valid JSON", func(t *testing.T) {
		s := &Settings{
			Version:      1,
			Tokens:       []StoredToken{{Name: "save-test", Hash: "st-hash"}},
			ExternalMcps: []ExternalMcp{},
			Services:     []ServiceConfig{},
		}
		err := saveSettingsInternal(s)
		if err != nil {
			t.Fatalf("saveSettingsInternal failed: %v", err)
		}

		data, err := os.ReadFile(sp)
		if err != nil {
			t.Fatalf("failed to read back: %v", err)
		}
		var loaded Settings
		if err := json.Unmarshal(data, &loaded); err != nil {
			t.Fatalf("written file is not valid JSON: %v", err)
		}
		if len(loaded.Tokens) != 1 || loaded.Tokens[0].Name != "save-test" {
			t.Fatal("saved data does not match")
		}
	})

	t.Run("no temp file left behind", func(t *testing.T) {
		s := defaultSettings()
		_ = saveSettingsInternal(s)

		tmp := sp + ".tmp"
		if _, err := os.Stat(tmp); !os.IsNotExist(err) {
			t.Fatal("temp file should not remain after successful save")
		}
	})

	t.Run("creates directory if missing", func(t *testing.T) {
		s := defaultSettings()
		err := saveSettingsInternal(s)
		if err != nil {
			t.Fatalf("saveSettingsInternal failed: %v", err)
		}

		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("settings dir should exist: %v", err)
		}
		if !info.IsDir() {
			t.Fatal("settings dir should be a directory")
		}
	})
}

// ---------------------------------------------------------------------------
// ensureAdminSecret
// ---------------------------------------------------------------------------

func TestEnsureAdminSecret(t *testing.T) {
	t.Run("generates secret when empty", func(t *testing.T) {
		s := defaultSettings()
		ensureAdminSecret(s)
		if s.AdminSecret == "" {
			t.Fatal("AdminSecret should be generated")
		}
		if len(s.AdminSecret) != 32 {
			t.Fatalf("expected 32 hex chars, got %d", len(s.AdminSecret))
		}
	})

	t.Run("does not overwrite existing secret", func(t *testing.T) {
		s := defaultSettings()
		s.AdminSecret = "keep-me"
		ensureAdminSecret(s)
		if s.AdminSecret != "keep-me" {
			t.Fatalf("expected 'keep-me', got %q", s.AdminSecret)
		}
	})
}

// ---------------------------------------------------------------------------
// JSON round-trip: Settings serialization
// ---------------------------------------------------------------------------

func TestSettingsJSONRoundTrip(t *testing.T) {
	original := &Settings{
		Version: 1,
		Tokens: []StoredToken{
			{
				Name:        "tok1",
				Hash:        "h1",
				Prefix:      "aaaaaa",
				Suffix:      "zzzzzz",
				CreatedAt:   "2025-01-01T00:00:00Z",
				Permissions: map[string]Permission{"mcp1": PermOn, "mcp2": PermOff},
				DisabledTools: map[string][]string{
					"mcp1": {"tool_a"},
				},
				Context: map[string]json.RawMessage{
					"mcp1": json.RawMessage(`{"dirs":["/tmp"]}`),
				},
			},
		},
		ExternalMcps: []ExternalMcp{
			{
				ID:          "mcp1",
				DisplayName: "Test MCP",
				Command:     "/usr/bin/test",
				Args:        []string{"--flag"},
				Env:         map[string]string{"KEY": "VAL"},
				DiscoveredTools: []ToolInfo{
					{Name: "tool_a", Description: "A tool"},
				},
				Transport: "stdio",
			},
		},
		Services: []ServiceConfig{
			{
				ID:          "svc1",
				DisplayName: "Test Service",
				Command:     "/usr/bin/svc",
				Args:        []string{},
				Env:         map[string]string{},
				Autostart:   true,
			},
		},
		AdminSecret: "secret123",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var restored Settings
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	// Spot-check key fields.
	if restored.Version != 1 {
		t.Fatalf("version: got %d, want 1", restored.Version)
	}
	if len(restored.Tokens) != 1 {
		t.Fatalf("tokens: got %d, want 1", len(restored.Tokens))
	}
	if restored.Tokens[0].Permissions["mcp2"] != PermOff {
		t.Fatal("mcp2 permission should be PermOff")
	}
	if len(restored.ExternalMcps) != 1 {
		t.Fatalf("mcps: got %d, want 1", len(restored.ExternalMcps))
	}
	if restored.ExternalMcps[0].Env["KEY"] != "VAL" {
		t.Fatal("env KEY should be VAL")
	}
	if len(restored.Services) != 1 || !restored.Services[0].Autostart {
		t.Fatal("service autostart should be true")
	}
	if restored.AdminSecret != "secret123" {
		t.Fatalf("admin secret: got %q, want 'secret123'", restored.AdminSecret)
	}

	// Verify context round-trips.
	ctx := restored.Tokens[0].Context["mcp1"]
	if string(ctx) != `{"dirs":["/tmp"]}` {
		t.Fatalf("context round-trip failed: %s", string(ctx))
	}

	// Verify disabled tools round-trip.
	dt := restored.Tokens[0].DisabledTools["mcp1"]
	if len(dt) != 1 || dt[0] != "tool_a" {
		t.Fatalf("disabled tools round-trip failed: %v", dt)
	}
}

// ---------------------------------------------------------------------------
// Integration: full lifecycle
// ---------------------------------------------------------------------------

func TestIntegrationLifecycle(t *testing.T) {
	// This test exercises a realistic sequence of operations on a single
	// Settings instance without touching the file system.

	s := newTestSettings(t, nil, nil)

	// 1. Generate two tokens.
	pt1, tok1 := GenerateToken("alice", nil)
	pt2, tok2 := GenerateToken("bob", nil)
	s.Tokens = append(s.Tokens, tok1, tok2)

	// 2. Add two MCPs. Both should default to PermOff for existing tokens.
	s.AddExternalMcp(ExternalMcp{ID: "fs", DisplayName: "File System MCP"})
	s.AddExternalMcp(ExternalMcp{ID: "mac", DisplayName: "macOS MCP"})

	if s.GetPermission(tok1.Hash, "fs") != PermOff {
		t.Fatal("new MCP should default to PermOff")
	}
	if s.GetPermission(tok2.Hash, "mac") != PermOff {
		t.Fatal("new MCP should default to PermOff")
	}

	// 3. Grant alice access to fs, bob access to mac.
	s.UpdatePermission(tok1.Hash, "fs", PermOn)
	s.UpdatePermission(tok2.Hash, "mac", PermOn)

	if s.GetPermission(tok1.Hash, "fs") != PermOn {
		t.Fatal("alice should have PermOn for fs")
	}
	if s.GetPermission(tok2.Hash, "mac") != PermOn {
		t.Fatal("bob should have PermOn for mac")
	}
	if s.GetPermission(tok1.Hash, "mac") != PermOff {
		t.Fatal("alice should have PermOff for mac")
	}

	// 4. Disable a specific tool for alice on fs.
	s.SetToolDisabled(tok1.Hash, "fs", "delete_file", true)
	if !s.IsToolDisabled(tok1.Hash, "fs", "delete_file") {
		t.Fatal("delete_file should be disabled for alice")
	}
	if s.IsToolDisabled(tok2.Hash, "fs", "delete_file") {
		t.Fatal("delete_file should not be disabled for bob")
	}

	// 5. Set context for alice on fs.
	s.SetContext(tok1.Hash, "fs", json.RawMessage(`{"allowed_dirs":["/home/alice"]}`))
	aliceTok, _ := s.findTokenByHash(tok1.Hash)
	if string(aliceTok.Context["fs"]) != `{"allowed_dirs":["/home/alice"]}` {
		t.Fatal("alice context for fs should be set")
	}

	// 6. Authenticate both tokens.
	result, err := s.Authenticate(pt1)
	if err != nil || result.Name != "alice" {
		t.Fatalf("alice auth failed: %v", err)
	}
	result, err = s.Authenticate(pt2)
	if err != nil || result.Name != "bob" {
		t.Fatalf("bob auth failed: %v", err)
	}

	// 7. Remove the fs MCP. Should clean up permissions and disabled tools.
	s.RemoveExternalMcp("fs")
	aliceTok, _ = s.findTokenByHash(tok1.Hash)
	if _, exists := aliceTok.Permissions["fs"]; exists {
		t.Fatal("fs permission should be cleaned up after removal")
	}
	if _, exists := aliceTok.DisabledTools["fs"]; exists {
		t.Fatal("fs disabled tools should be cleaned up after removal")
	}

	// 8. Delete bob's token.
	s.DeleteToken(tok2.Hash)
	_, err = s.Authenticate(pt2)
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken after delete, got %v", err)
	}

	// 9. Revoke all.
	s.RevokeAll()
	_, err = s.Authenticate(pt1)
	if !errors.Is(err, ErrNoTokens) {
		t.Fatalf("expected ErrNoTokens after RevokeAll, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Edge cases
// ---------------------------------------------------------------------------

func TestEdgeCases(t *testing.T) {
	t.Run("settingsPath is under settingsDir", func(t *testing.T) {
		dir := settingsDir()
		path := settingsPath()
		if filepath.Dir(path) != dir {
			t.Fatalf("settingsPath %q should be inside settingsDir %q", path, dir)
		}
		if filepath.Base(path) != "settings.json" {
			t.Fatalf("settings file should be named settings.json, got %q", filepath.Base(path))
		}
	})

	t.Run("GenerateToken with nil permissions", func(t *testing.T) {
		_, tok := GenerateToken("nil-perms", nil)
		// Permissions should be nil (not set), which is fine.
		if tok.Permissions != nil {
			t.Fatal("nil default permissions should remain nil")
		}
	})

	t.Run("Authenticate returns pointer into Tokens slice", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		pt, tok := GenerateToken("ptr-test", nil)
		s.Tokens = append(s.Tokens, tok)

		result, _ := s.Authenticate(pt)
		result.Name = "mutated"
		if s.Tokens[0].Name != "mutated" {
			t.Fatal("Authenticate should return a pointer into the Tokens slice")
		}
	})

	t.Run("SetToolDisabled with nil DisabledTools initializes map", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		tok := StoredToken{Name: "bare", Hash: "bare-hash"}
		s.Tokens = append(s.Tokens, tok)

		s.SetToolDisabled("bare-hash", "mcp1", "t1", true)
		if s.Tokens[0].DisabledTools == nil {
			t.Fatal("DisabledTools should be initialized")
		}
	})

	t.Run("SetContext with nil Context initializes map", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		tok := StoredToken{Name: "bare", Hash: "bare-hash"}
		s.Tokens = append(s.Tokens, tok)

		s.SetContext("bare-hash", "mcp1", json.RawMessage(`{"k":"v"}`))
		if s.Tokens[0].Context == nil {
			t.Fatal("Context should be initialized")
		}
	})
}

func TestUpsertExternalMcp(t *testing.T) {
	t.Run("inserts new MCP and returns false", func(t *testing.T) {
		s := newTestSettings(t, []StoredToken{{Hash: "h1", Permissions: map[string]Permission{}}}, nil)
		cfg := ExternalMcp{ID: "new-mcp", DisplayName: "New", DiscoveredTools: []ToolInfo{}}
		updated := s.UpsertExternalMcp(cfg)
		if updated {
			t.Fatal("expected insert (false), got update (true)")
		}
		if len(s.ExternalMcps) != 1 || s.ExternalMcps[0].ID != "new-mcp" {
			t.Fatal("MCP not added")
		}
		// AddExternalMcp should have set PermOff for existing tokens.
		if s.Tokens[0].Permissions["new-mcp"] != PermOff {
			t.Fatal("expected PermOff default for new MCP")
		}
	})

	t.Run("updates existing MCP and returns true", func(t *testing.T) {
		s := newTestSettings(t, nil, []ExternalMcp{
			{ID: "mcp1", DisplayName: "Old", Command: "old-cmd", DiscoveredTools: []ToolInfo{{Name: "tool1"}}},
		})
		cfg := ExternalMcp{ID: "mcp1", DisplayName: "Updated", Command: "new-cmd", DiscoveredTools: []ToolInfo{}}
		updated := s.UpsertExternalMcp(cfg)
		if !updated {
			t.Fatal("expected update (true), got insert (false)")
		}
		if len(s.ExternalMcps) != 1 {
			t.Fatal("should still have 1 MCP")
		}
		if s.ExternalMcps[0].Command != "new-cmd" {
			t.Fatal("command not updated")
		}
		// UpdateExternalMcp preserves tools when new has none.
		if len(s.ExternalMcps[0].DiscoveredTools) != 1 {
			t.Fatal("DiscoveredTools should be preserved")
		}
	})
}

func TestUpsertService(t *testing.T) {
	t.Run("inserts new service and returns false", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		cfg := ServiceConfig{ID: "svc1", DisplayName: "Svc 1", Command: "cmd"}
		updated := s.UpsertService(cfg)
		if updated {
			t.Fatal("expected insert (false), got update (true)")
		}
		if len(s.Services) != 1 || s.Services[0].ID != "svc1" {
			t.Fatal("service not added")
		}
	})

	t.Run("updates existing service and returns true", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		s.Services = []ServiceConfig{{ID: "svc1", DisplayName: "Old", Command: "old-cmd"}}
		cfg := ServiceConfig{ID: "svc1", DisplayName: "New", Command: "new-cmd"}
		updated := s.UpsertService(cfg)
		if !updated {
			t.Fatal("expected update (true), got insert (false)")
		}
		if len(s.Services) != 1 {
			t.Fatal("should still have 1 service")
		}
		if s.Services[0].Command != "new-cmd" {
			t.Fatal("command not updated")
		}
	})
}

func TestMergeServiceDefaults(t *testing.T) {
	t.Run("fills zero-value fields from existing", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		s.Services = []ServiceConfig{{
			ID:         "svc1",
			Command:    "cmd",
			Args:       []string{"--flag"},
			Env:        map[string]string{"K": "V"},
			WorkingDir: "/old/dir",
			URL:        "http://old",
		}}
		cfg := ServiceConfig{ID: "svc1", Command: "new-cmd"}
		s.MergeServiceDefaults(&cfg)
		if cfg.Command != "new-cmd" {
			t.Fatal("should not overwrite non-zero Command")
		}
		if len(cfg.Args) != 1 || cfg.Args[0] != "--flag" {
			t.Fatal("should inherit Args")
		}
		if cfg.Env["K"] != "V" {
			t.Fatal("should inherit Env")
		}
		if cfg.WorkingDir != "/old/dir" {
			t.Fatal("should inherit WorkingDir")
		}
		if cfg.URL != "http://old" {
			t.Fatal("should inherit URL")
		}
	})

	t.Run("does not overwrite non-zero fields", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		s.Services = []ServiceConfig{{
			ID:         "svc1",
			Args:       []string{"--old"},
			Env:        map[string]string{"OLD": "1"},
			WorkingDir: "/old",
			URL:        "http://old",
		}}
		cfg := ServiceConfig{
			ID:         "svc1",
			Args:       []string{"--new"},
			Env:        map[string]string{"NEW": "2"},
			WorkingDir: "/new",
			URL:        "http://new",
		}
		s.MergeServiceDefaults(&cfg)
		if cfg.Args[0] != "--new" {
			t.Fatal("should keep caller's Args")
		}
		if cfg.Env["NEW"] != "2" {
			t.Fatal("should keep caller's Env")
		}
		if cfg.WorkingDir != "/new" {
			t.Fatal("should keep caller's WorkingDir")
		}
		if cfg.URL != "http://new" {
			t.Fatal("should keep caller's URL")
		}
	})

	t.Run("no-op for unknown service", func(t *testing.T) {
		s := newTestSettings(t, nil, nil)
		cfg := ServiceConfig{ID: "missing", Command: "cmd"}
		s.MergeServiceDefaults(&cfg)
		if cfg.Command != "cmd" {
			t.Fatal("should not mutate when service not found")
		}
	})
}

func TestResolveMcpID(t *testing.T) {
	s := newTestSettings(t, nil, []ExternalMcp{
		{ID: "mcp1", DisplayName: "My MCP"},
		{ID: "mcp2", DisplayName: "Other MCP"},
	})

	t.Run("finds by id", func(t *testing.T) {
		if s.ResolveMcpID("mcp1", "") != "mcp1" {
			t.Fatal("should find by id")
		}
	})

	t.Run("finds by name", func(t *testing.T) {
		if s.ResolveMcpID("", "Other MCP") != "mcp2" {
			t.Fatal("should find by display name")
		}
	})

	t.Run("returns empty for unknown id", func(t *testing.T) {
		if s.ResolveMcpID("nope", "") != "" {
			t.Fatal("should return empty for unknown id")
		}
	})

	t.Run("returns empty for unknown name", func(t *testing.T) {
		if s.ResolveMcpID("", "Nope") != "" {
			t.Fatal("should return empty for unknown name")
		}
	})

	t.Run("id takes precedence over name", func(t *testing.T) {
		if s.ResolveMcpID("mcp1", "Other MCP") != "mcp1" {
			t.Fatal("id should take precedence")
		}
	})
}

func TestResolveServiceID(t *testing.T) {
	s := newTestSettings(t, nil, nil)
	s.Services = []ServiceConfig{
		{ID: "svc1", DisplayName: "My Service"},
		{ID: "svc2", DisplayName: "Other Service"},
	}

	t.Run("finds by id", func(t *testing.T) {
		if s.ResolveServiceID("svc1", "") != "svc1" {
			t.Fatal("should find by id")
		}
	})

	t.Run("finds by name", func(t *testing.T) {
		if s.ResolveServiceID("", "Other Service") != "svc2" {
			t.Fatal("should find by display name")
		}
	})

	t.Run("returns empty for unknown id", func(t *testing.T) {
		if s.ResolveServiceID("nope", "") != "" {
			t.Fatal("should return empty for unknown id")
		}
	})

	t.Run("returns empty for unknown name", func(t *testing.T) {
		if s.ResolveServiceID("", "Nope") != "" {
			t.Fatal("should return empty for unknown name")
		}
	})
}
