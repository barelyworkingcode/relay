package main

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// parseResourceMetadataURL
// ---------------------------------------------------------------------------

func TestParseResourceMetadataURL_ValidHeader(t *testing.T) {
	header := `Bearer resource_metadata="https://example.com/.well-known/resource"`
	got := parseResourceMetadataURL(header)
	want := "https://example.com/.well-known/resource"
	if got != want {
		t.Errorf("parseResourceMetadataURL(%q) = %q, want %q", header, got, want)
	}
}

func TestParseResourceMetadataURL_EmptyHeader(t *testing.T) {
	got := parseResourceMetadataURL("")
	if got != "" {
		t.Errorf("expected empty string for empty header, got %q", got)
	}
}

func TestParseResourceMetadataURL_NoResourceMetadata(t *testing.T) {
	got := parseResourceMetadataURL("Bearer realm=\"example\"")
	if got != "" {
		t.Errorf("expected empty string when resource_metadata absent, got %q", got)
	}
}

func TestParseResourceMetadataURL_MissingClosingQuote(t *testing.T) {
	header := `Bearer resource_metadata="https://example.com`
	got := parseResourceMetadataURL(header)
	if got != "" {
		t.Errorf("expected empty string for missing closing quote, got %q", got)
	}
}

func TestParseResourceMetadataURL_MultipleParams(t *testing.T) {
	header := `Bearer realm="example", resource_metadata="https://auth.example.com/prm", error="invalid_token"`
	got := parseResourceMetadataURL(header)
	want := "https://auth.example.com/prm"
	if got != want {
		t.Errorf("parseResourceMetadataURL(%q) = %q, want %q", header, got, want)
	}
}

// ---------------------------------------------------------------------------
// generatePKCE
// ---------------------------------------------------------------------------

func TestGeneratePKCE_NonEmpty(t *testing.T) {
	pkce, err := generatePKCE()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pkce.Verifier == "" {
		t.Error("verifier should not be empty")
	}
	if pkce.Challenge == "" {
		t.Error("challenge should not be empty")
	}
}

func TestGeneratePKCE_VerifierAndChallengeDiffer(t *testing.T) {
	pkce, err := generatePKCE()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pkce.Verifier == pkce.Challenge {
		t.Error("verifier and challenge should be different")
	}
}

func TestGeneratePKCE_ChallengeIsBase64URL(t *testing.T) {
	pkce, err := generatePKCE()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(pkce.Challenge, "+") {
		t.Error("challenge contains '+' which is not valid base64url")
	}
	if strings.Contains(pkce.Challenge, "/") {
		t.Error("challenge contains '/' which is not valid base64url")
	}
	if strings.Contains(pkce.Challenge, "=") {
		t.Error("challenge contains '=' padding which should be absent in raw base64url")
	}
}

func TestGeneratePKCE_TwoCallsProduceDifferentValues(t *testing.T) {
	pkce1, err := generatePKCE()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pkce2, err := generatePKCE()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pkce1.Verifier == pkce2.Verifier {
		t.Error("two calls produced the same verifier")
	}
	if pkce1.Challenge == pkce2.Challenge {
		t.Error("two calls produced the same challenge")
	}
}

// ---------------------------------------------------------------------------
// generateState
// ---------------------------------------------------------------------------

func TestGenerateState_NonEmpty(t *testing.T) {
	state, err := generateState()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state == "" {
		t.Error("state should not be empty")
	}
}

func TestGenerateState_TwoCallsProduceDifferentValues(t *testing.T) {
	state1, err := generateState()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	state2, err := generateState()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state1 == state2 {
		t.Error("two calls produced the same state")
	}
}

func TestGenerateState_IsBase64URL(t *testing.T) {
	state, err := generateState()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(state, "+") {
		t.Error("state contains '+' which is not valid base64url")
	}
	if strings.Contains(state, "/") {
		t.Error("state contains '/' which is not valid base64url")
	}
	if strings.Contains(state, "=") {
		t.Error("state contains '=' padding which should be absent in raw base64url")
	}
}
