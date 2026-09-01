package main

import (
	"strings"
	"testing"
)

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
	if want := "unsupported URL scheme"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q should mention %q", err.Error(), want)
	}
}

func TestValidateMcpURL_MissingHostRejected(t *testing.T) {
	err := validateMcpURL("http:///path")
	if err == nil {
		t.Fatal("expected error for missing host, got nil")
	}
	if want := "missing a host"; !strings.Contains(err.Error(), want) {
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
