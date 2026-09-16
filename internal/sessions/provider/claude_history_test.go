package provider

import (
	"encoding/json"
	"strings"
	"testing"
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
	got := encodeClaudeProjectDir("/Users/jonathan/source/project")
	want := "-Users-jonathan-source-project"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
