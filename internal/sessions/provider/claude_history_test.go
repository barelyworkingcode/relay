package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseClaudeHistoryJSONL_UserAndAssistantTurns(t *testing.T) {
	const sid = "sess-abc"
	jsonl := strings.Join([]string{
		`{"type":"user","sessionId":"sess-abc","timestamp":"t1","message":{"role":"user","content":"hello"}}`,
		`{"type":"assistant","sessionId":"sess-abc","timestamp":"t2","message":{"id":"m1","content":[{"type":"text","text":"hi there"}]}}`,
	}, "\n")

	msgs, err := parseClaudeHistoryJSONL(strings.NewReader(jsonl), sid, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" {
		t.Errorf("msgs[0].Role = %q", msgs[0].Role)
	}
	if msgs[1].Role != "assistant" {
		t.Errorf("msgs[1].Role = %q", msgs[1].Role)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(msgs[1].Content, &blocks); err != nil {
		t.Fatalf("unmarshal assistant content: %v", err)
	}
	if len(blocks) != 1 || blocks[0]["text"] != "hi there" {
		t.Errorf("unexpected assistant blocks: %+v", blocks)
	}
}

func TestParseClaudeHistoryJSONL_SkipsOtherSessions(t *testing.T) {
	jsonl := `{"type":"user","sessionId":"other-session","timestamp":"t1","message":{"role":"user","content":"hello"}}`
	msgs, err := parseClaudeHistoryJSONL(strings.NewReader(jsonl), "sess-abc", nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected no messages for a foreign session, got %+v", msgs)
	}
}

func TestReadClaudeHistory_EmptySessionIDErrors(t *testing.T) {
	if _, err := ReadClaudeHistory("/tmp", nil, ""); err == nil {
		t.Fatal("expected error for empty claude session id")
	}
}

func TestEncodeClaudeProjectDir(t *testing.T) {
	got := encodeClaudeProjectDir("/Users/me/source/project")
	want := "-Users-me-source-project"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestRunSSHCommand_ChildEnvStripsRelaySecrets is the fix for the ssh exec
// bypassing childBaseEnv entirely: this is the SSH path, so the leak would
// otherwise reach a locally-spawned ssh process's environment.
func TestRunSSHCommand_ChildEnvStripsRelaySecrets(t *testing.T) {
	dir := t.TempDir()
	envOut := filepath.Join(dir, "env.out")
	script := filepath.Join(dir, "fake-ssh.sh")
	content := "#!/bin/sh\nenv > " + envOut + "\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("RELAY_PROJECT_TOKEN", "leaked-project-token")

	if _, err := runSSHCommand([]string{script}, 5*time.Second); err != nil {
		t.Fatalf("runSSHCommand: %v", err)
	}

	data, err := os.ReadFile(envOut)
	if err != nil {
		t.Fatalf("read env dump: %v", err)
	}
	if strings.Contains(string(data), "RELAY_PROJECT_TOKEN") {
		t.Fatalf("ambient RELAY_PROJECT_TOKEN reached the locally-spawned ssh child:\n%s", data)
	}
}
