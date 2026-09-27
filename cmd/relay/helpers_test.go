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

func TestValidateMcpURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr string // empty: valid; "*": any error
	}{
		{name: "http", url: "http://example.com/mcp"},
		{name: "https", url: "https://example.com/mcp"},
		{name: "ftp", url: "ftp://example.com/file", wantErr: "unsupported URL scheme"},
		{name: "no host", url: "http:///path", wantErr: "missing a host"},
		{name: "empty", url: "", wantErr: "*"},
		{name: "malformed", url: "://bad", wantErr: "*"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMcpURL(tc.url)
			switch {
			case tc.wantErr == "":
				if err != nil {
					t.Errorf("validateMcpURL(%q) = %v, want nil", tc.url, err)
				}
			case err == nil:
				t.Fatalf("validateMcpURL(%q) = nil, want an error", tc.url)
			case tc.wantErr != "*" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("validateMcpURL(%q) error %q should mention %q", tc.url, err.Error(), tc.wantErr)
			}
		})
	}
}
