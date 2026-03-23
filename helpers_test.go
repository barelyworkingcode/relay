package main

import (
	"os"
	"os/exec"
	"testing"
)

// ---------------------------------------------------------------------------
// mergeEnv
// ---------------------------------------------------------------------------

func TestMergeEnv_EmptyMapIsNoop(t *testing.T) {
	cmd := exec.Command("true")
	mergeEnv(cmd, nil)
	if cmd.Env != nil {
		t.Errorf("expected cmd.Env to remain nil, got %v", cmd.Env)
	}

	cmd2 := exec.Command("true")
	mergeEnv(cmd2, map[string]string{})
	if cmd2.Env != nil {
		t.Errorf("expected cmd.Env to remain nil for empty map, got %v", cmd2.Env)
	}
}

func TestMergeEnv_MergesWithOsEnviron(t *testing.T) {
	cmd := exec.Command("true")
	mergeEnv(cmd, map[string]string{"TEST_KEY_RELAY": "test_value"})
	if cmd.Env == nil {
		t.Fatal("expected cmd.Env to be set")
	}

	// cmd.Env should contain the existing environment plus our new var.
	osEnvLen := len(os.Environ())
	if len(cmd.Env) < osEnvLen+1 {
		t.Errorf("expected at least %d env vars, got %d", osEnvLen+1, len(cmd.Env))
	}

	found := false
	for _, entry := range cmd.Env {
		if entry == "TEST_KEY_RELAY=test_value" {
			found = true
			break
		}
	}
	if !found {
		t.Error("merged env does not contain TEST_KEY_RELAY=test_value")
	}
}

// ---------------------------------------------------------------------------
// slugify
// ---------------------------------------------------------------------------

func TestSlugify(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"My App", "my-app"},
		{"hello", "hello"},
		{"FOO BAR", "foo-bar"},
		{"", ""},
		{"---multiple---dashes---", "multiple-dashes"},
		{"special!@#chars$%^here", "special-chars-here"},
		{"  leading trailing  ", "leading-trailing"},
		{"UPPER123lower", "upper123lower"},
		{"a", "a"},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := slugify(tc.input)
			if got != tc.want {
				t.Errorf("slugify(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// validateMcpURL
// ---------------------------------------------------------------------------

func TestValidateMcpURL_ValidHTTP(t *testing.T) {
	if err := validateMcpURL("http://example.com/mcp"); err != nil {
		t.Errorf("expected no error for valid http URL, got: %v", err)
	}
}

func TestValidateMcpURL_ValidHTTPS(t *testing.T) {
	if err := validateMcpURL("https://example.com/mcp"); err != nil {
		t.Errorf("expected no error for valid https URL, got: %v", err)
	}
}

func TestValidateMcpURL_FTPSchemeRejected(t *testing.T) {
	err := validateMcpURL("ftp://example.com/file")
	if err == nil {
		t.Fatal("expected error for ftp scheme, got nil")
	}
	if want := "unsupported URL scheme"; !contains(err.Error(), want) {
		t.Errorf("error %q should mention %q", err.Error(), want)
	}
}

func TestValidateMcpURL_MissingHostRejected(t *testing.T) {
	err := validateMcpURL("http:///path")
	if err == nil {
		t.Fatal("expected error for missing host, got nil")
	}
	if want := "missing a host"; !contains(err.Error(), want) {
		t.Errorf("error %q should mention %q", err.Error(), want)
	}
}

func TestValidateMcpURL_EmptyString(t *testing.T) {
	if err := validateMcpURL(""); err == nil {
		t.Fatal("expected error for empty string, got nil")
	}
}

func TestValidateMcpURL_MalformedURL(t *testing.T) {
	if err := validateMcpURL("://bad"); err == nil {
		t.Fatal("expected error for malformed URL, got nil")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
