package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePiListModels_FixedWidthTable(t *testing.T) {
	table := "provider   model               context  max-out  thinking  images\n" +
		"anthropic  claude-sonnet-4     200K     16K      yes       yes\n" +
		"llama-cpp  Qwen3.6 27B Q4      128K     16.4K    no        no\n"

	models := parsePiListModels([]byte(table))
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2: %+v", len(models), models)
	}
	if models[0].Value != "pi/anthropic/claude-sonnet-4" {
		t.Errorf("models[0].Value = %q", models[0].Value)
	}
	if models[1].Value != "pi/llama-cpp/Qwen3.6 27B Q4" {
		t.Errorf("models[1].Value = %q", models[1].Value)
	}
}

func TestParsePiListModels_MissingHeaderReturnsNil(t *testing.T) {
	if got := parsePiListModels([]byte("not a table\n")); got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

func TestFetchPiModels_MissingBinaryReturnsNil(t *testing.T) {
	got := FetchPiModels(context.Background(), "/nonexistent/pi/binary")
	if got != nil {
		t.Fatalf("expected nil for a missing binary, got %+v", got)
	}
}

// TestFetchPiModels_ChildEnvStripsRelaySecrets is the fix for the
// `--list-models` exec bypassing childBaseEnv entirely: the child must never
// see relay-sessions' own ambient RELAY_* credentials.
func TestFetchPiModels_ChildEnvStripsRelaySecrets(t *testing.T) {
	dir := t.TempDir()
	envOut := filepath.Join(dir, "env.out")
	script := filepath.Join(dir, "pi")
	content := "#!/bin/sh\nenv > " + envOut + "\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("RELAY_PROJECT_TOKEN", "leaked-project-token")

	FetchPiModels(context.Background(), script)

	data, err := os.ReadFile(envOut)
	if err != nil {
		t.Fatalf("read env dump: %v", err)
	}
	if strings.Contains(string(data), "RELAY_PROJECT_TOKEN") {
		t.Fatalf("ambient RELAY_PROJECT_TOKEN reached the pi --list-models child:\n%s", data)
	}
}
