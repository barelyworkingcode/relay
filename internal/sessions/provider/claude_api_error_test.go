package provider

import (
	"encoding/json"
	"testing"

	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const apiErrorAssistantLine = `{"type":"assistant","message":{"id":"00000000-0000-0000-0000-000000000001","model":"<synthetic>","role":"assistant","stop_reason":"stop_sequence","type":"message","content":[{"type":"text","text":"Failed to authenticate. API Error: 401 API key is invalid."}]},"parent_tool_use_id":null,"session_id":"00000000-0000-0000-0000-000000000002","uuid":"00000000-0000-0000-0000-000000000003","error":"authentication_failed","is_api_error_message":true}`

const apiErrorResultLine = `{"type":"result","subtype":"success","is_error":true,"api_error_status":401,"result":"Failed to authenticate. API Error: 401 API key is invalid.","num_turns":1,"total_cost_usd":0,"usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`

func messageStartEvents(t *testing.T, lines ...string) []map[string]any {
	t.Helper()
	var starts []map[string]any
	p := NewClaudeProvider(&sessionstypes.Session{Model: "sonnet"}, func(ev string, data json.RawMessage) {
		if ev != "llm_event" {
			return
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("llm_event not JSON: %v", err)
		}
		if msg, ok := m["message"].(map[string]any); ok && m["type"] == "assistant" {
			if c, _ := msg["content"].([]any); msg["role"] == "assistant" && len(c) == 0 {
				starts = append(starts, m)
			}
		}
	}, ClaudeConfig{}, nil)
	for _, line := range lines {
		p.processLine(json.RawMessage(line), nil)
	}
	return starts
}

func TestClaudeAssistant_APIErrorMarkerReachesMessageStart(t *testing.T) {
	starts := messageStartEvents(t, apiErrorAssistantLine)
	if len(starts) != 1 {
		t.Fatalf("message_start count = %d, want 1", len(starts))
	}
	if got := starts[0]["error"]; got != "authentication_failed" {
		t.Fatalf("message_start error = %v, want %q", got, "authentication_failed")
	}
	if _, ok := starts[0]["apiErrorStatus"]; ok {
		t.Fatalf("assistant line carries no status; apiErrorStatus = %v, want absent", starts[0]["apiErrorStatus"])
	}
}

func TestClaudeAssistant_APIErrorStatusOnAssistantLineIsForwarded(t *testing.T) {
	line := `{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"x"}]},"error":"rate_limit","api_error_status":429}`
	starts := messageStartEvents(t, line)
	if len(starts) != 1 {
		t.Fatalf("message_start count = %d, want 1", len(starts))
	}
	if got := starts[0]["error"]; got != "rate_limit" {
		t.Fatalf("error = %v, want rate_limit", got)
	}
	if got := starts[0]["apiErrorStatus"]; got != float64(429) {
		t.Fatalf("apiErrorStatus = %v, want 429", got)
	}
}

func TestClaudeAssistant_NormalTurnCarriesNoErrorFields(t *testing.T) {
	line := `{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"hello"}]}}`
	starts := messageStartEvents(t, line)
	if len(starts) != 1 {
		t.Fatalf("message_start count = %d, want 1", len(starts))
	}
	for _, k := range []string{"error", "apiErrorStatus"} {
		if _, ok := starts[0][k]; ok {
			t.Fatalf("normal message_start has key %q = %v, want absent", k, starts[0][k])
		}
	}
}

func TestClaudeResult_APIErrorStatusReachesMessageComplete(t *testing.T) {
	var got []json.RawMessage
	p := NewClaudeProvider(&sessionstypes.Session{Model: "sonnet"}, func(ev string, data json.RawMessage) {
		if ev == "message_complete" {
			got = append(got, data)
		}
	}, ClaudeConfig{}, nil)
	p.processLine(json.RawMessage(apiErrorResultLine), nil)
	if len(got) != 1 {
		t.Fatalf("message_complete count = %d, want 1", len(got))
	}
	var d map[string]any
	if err := json.Unmarshal(got[0], &d); err != nil {
		t.Fatalf("message_complete data %q: %v", got[0], err)
	}
	if d["isError"] != true {
		t.Fatalf("isError = %v, want true", d["isError"])
	}
	if d["apiErrorStatus"] != float64(401) {
		t.Fatalf("apiErrorStatus = %v, want 401", d["apiErrorStatus"])
	}
}

func TestClaudeAssistant_APIErrorReusingMessageIDStillEmitsMessageStart(t *testing.T) {
	normal := `{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"hello"}]}}`
	errLine := `{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"Failed to authenticate."}]},"error":"authentication_failed"}`
	starts := messageStartEvents(t, normal, errLine)
	if len(starts) != 2 {
		t.Fatalf("message_start count = %d, want 2", len(starts))
	}
	if got := starts[1]["error"]; got != "authentication_failed" {
		t.Fatalf("second message_start error = %v, want %q", got, "authentication_failed")
	}
}
