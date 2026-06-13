package main

import (
	"net/http"
	"net/http/httptest"
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

// ---------------------------------------------------------------------------
// CR-2: OAuth discovery SSRF guard
// ---------------------------------------------------------------------------

func TestValidateOAuthDiscoveryURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"https public", "https://auth.example.com/.well-known/x", false},
		{"http loopback ipv4", "http://127.0.0.1:8080/meta", false},
		{"http localhost", "http://localhost:9000/meta", false},
		{"http loopback ipv6", "http://[::1]:9000/meta", false},
		{"http public host rejected", "http://auth.example.com/meta", true},
		{"http private host rejected", "http://10.0.0.5/meta", true},
		{"file scheme rejected", "file:///etc/passwd", true},
		{"gopher scheme rejected", "gopher://internal:70/", true},
		{"missing host rejected", "https:///path", true},
		{"garbage rejected", "://nonsense", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOAuthDiscoveryURL(tc.url)
			if tc.wantErr && err == nil {
				t.Errorf("validateOAuthDiscoveryURL(%q) = nil, want error", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateOAuthDiscoveryURL(%q) = %v, want nil", tc.url, err)
			}
		})
	}
}

// The fetch helpers must reject an SSRF target before issuing any request.
func TestFetchProtectedResourceMetadata_RejectsSSRFTarget(t *testing.T) {
	if _, err := fetchProtectedResourceMetadata("file:///etc/passwd"); err == nil {
		t.Error("expected PRM fetch to reject a file:// URL")
	}
	if _, err := fetchProtectedResourceMetadata("http://169.254.169.254/latest/meta-data"); err == nil {
		t.Error("expected PRM fetch to reject plaintext http to a link-local host")
	}
}

func TestTryFetchOAuthMetadata_RejectsSSRFTarget(t *testing.T) {
	if meta := tryFetchOAuthMetadata("http://10.0.0.1/.well-known/oauth-authorization-server"); meta != nil {
		t.Error("expected metadata fetch to skip plaintext http to a private host")
	}
}

// CheckRedirect must re-validate the redirect target: a discovered endpoint that
// 302s relay to a blocked scheme/host must not be followed.
func TestOAuthClient_RejectsRedirectToBlockedTarget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data", http.StatusFound)
	}))
	defer srv.Close()

	// srv.URL is http on 127.0.0.1 (loopback) so the initial GET is allowed; the
	// redirect to a non-loopback plaintext-http host must be refused.
	_, err := fetchProtectedResourceMetadata(srv.URL + "/.well-known/oauth-protected-resource")
	if err == nil {
		t.Fatal("expected the redirect to a blocked target to fail the fetch")
	}
	if !strings.Contains(err.Error(), "non-loopback") && !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("error should reflect the blocked redirect, got: %v", err)
	}
}
