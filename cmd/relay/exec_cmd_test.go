package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// relay mcp / relay mcp call must read RELAY_PROJECT_TOKEN but never the
// retired legacy alias RELAY_TOKEN (docs/tokens.md; C12 skill text).
func TestResolveMcpToken_ReadsProjectTokenNotLegacyAlias(t *testing.T) {
	t.Setenv("RELAY_PROJECT_TOKEN", "")
	t.Setenv("RELAY_TOKEN", "")

	if got := resolveMcpToken("flag-token"); got != "flag-token" {
		t.Errorf("explicit --token should win over any env var, got %q", got)
	}

	t.Setenv("RELAY_PROJECT_TOKEN", "proj-token-value")
	if got := resolveMcpToken(""); got != "proj-token-value" {
		t.Errorf("expected RELAY_PROJECT_TOKEN to be read, got %q", got)
	}

	t.Setenv("RELAY_PROJECT_TOKEN", "")
	t.Setenv("RELAY_TOKEN", "legacy-token-value")
	if got := resolveMcpToken(""); got != "" {
		t.Errorf("RELAY_TOKEN (legacy alias) must not be read; got %q", got)
	}
}

func TestResolveToolArgs(t *testing.T) {
	// The file row is the load-bearing case: a prompt with quotes, apostrophes
	// and parens that shell quoting would mangle if passed inline.
	fileBody := `{"prompt":"Van Gogh's \"Starry Night\" (1889)"}`
	argsPath := filepath.Join(t.TempDir(), "args.json")
	if err := os.WriteFile(argsPath, []byte(fileBody), 0600); err != nil {
		t.Fatal(err)
	}
	stdinBody := `{"prompt":"line one\nline two"}`

	tests := []struct {
		name     string
		argsFile string
		toolArgs string
		stdin    io.Reader
		want     string // compared only when wantErr is empty; "" means nil
		wantErr  string
	}{
		{name: "inline", toolArgs: `{"a":1}`, want: `{"a":1}`},
		{name: "file", argsFile: argsPath, want: fileBody},
		{name: "stdin", argsFile: "-", stdin: strings.NewReader(stdinBody), want: stdinBody},
		{name: "both", argsFile: "some-file", toolArgs: `{"a":1}`, wantErr: "not both"},
		{name: "invalid JSON", toolArgs: `{not json`, wantErr: "invalid args JSON"},
		{name: "blank", toolArgs: "   "},
		{name: "missing file", argsFile: filepath.Join(t.TempDir(), "nope.json"), wantErr: "args-file"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveToolArgs(tc.argsFile, tc.toolArgs, tc.stdin)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want an error mentioning %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveToolArgs: %v", err)
			}
			if tc.want == "" {
				if got != nil {
					t.Errorf("want nil args, got %q", got)
				}
				return
			}
			if string(got) != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
