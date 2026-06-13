package main

// Coverage for resolveToolArgs — the --args / --args-file / stdin resolution
// that `relay mcp call` uses. The file/stdin path exists specifically to dodge
// shell-quoting bugs (the "Van Gogh's apostrophe" case) for every skill-driven
// tool call, and was previously untested.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveToolArgs_InlineArgs(t *testing.T) {
	got, err := resolveToolArgs("", `{"a":1}`, nil)
	if err != nil {
		t.Fatalf("resolveToolArgs: %v", err)
	}
	if string(got) != `{"a":1}` {
		t.Errorf("got %q, want {\"a\":1}", got)
	}
}

func TestResolveToolArgs_FromFile(t *testing.T) {
	// The load-bearing case: a prompt with quotes/apostrophes/parens that would
	// be mangled by shell quoting if passed inline.
	body := `{"prompt":"Van Gogh's \"Starry Night\" (1889)"}`
	dir := t.TempDir()
	path := filepath.Join(dir, "args.json")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveToolArgs(path, "", nil)
	if err != nil {
		t.Fatalf("resolveToolArgs: %v", err)
	}
	if string(got) != body {
		t.Errorf("got %q, want %q", got, body)
	}
}

func TestResolveToolArgs_FromStdin(t *testing.T) {
	body := `{"prompt":"line one\nline two"}`
	got, err := resolveToolArgs("-", "", strings.NewReader(body))
	if err != nil {
		t.Fatalf("resolveToolArgs: %v", err)
	}
	if string(got) != body {
		t.Errorf("got %q, want %q", got, body)
	}
}

func TestResolveToolArgs_BothSourcesRejected(t *testing.T) {
	_, err := resolveToolArgs("some-file", `{"a":1}`, nil)
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("want conflicting-flags error, got %v", err)
	}
}

func TestResolveToolArgs_InvalidJSONRejected(t *testing.T) {
	_, err := resolveToolArgs("", `{not json`, nil)
	if err == nil || !strings.Contains(err.Error(), "invalid args JSON") {
		t.Fatalf("want invalid-JSON error, got %v", err)
	}
}

func TestResolveToolArgs_EmptyIsNil(t *testing.T) {
	got, err := resolveToolArgs("", "   ", nil)
	if err != nil {
		t.Fatalf("resolveToolArgs: %v", err)
	}
	if got != nil {
		t.Errorf("empty args should yield nil, got %q", got)
	}
}

func TestResolveToolArgs_MissingFileIsError(t *testing.T) {
	_, err := resolveToolArgs(filepath.Join(t.TempDir(), "nope.json"), "", nil)
	if err == nil || !strings.Contains(err.Error(), "args-file") {
		t.Fatalf("want unreadable-file error, got %v", err)
	}
}
