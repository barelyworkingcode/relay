package modelbroker

import "testing"

func TestParseOpenAIUsage_NonStream(t *testing.T) {
	body := []byte(`{"id":"x","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":34,"total_tokens":46}}`)
	u, ok := ParseOpenAIUsage(body)
	if !ok {
		t.Fatalf("expected usage found")
	}
	if u != (Usage{PromptTokens: 12, CompletionTokens: 34}) {
		t.Fatalf("got %+v", u)
	}
}

func TestParseOpenAIUsage_Absent(t *testing.T) {
	if _, ok := ParseOpenAIUsage([]byte(`{"id":"x","choices":[]}`)); ok {
		t.Fatalf("expected no usage")
	}
}

func TestParseAnthropicUsage_NonStream(t *testing.T) {
	body := []byte(`{"id":"msg_1","type":"message","usage":{"input_tokens":7,"output_tokens":9}}`)
	u, ok := ParseAnthropicUsage(body)
	if !ok {
		t.Fatalf("expected usage found")
	}
	if u != (Usage{PromptTokens: 7, CompletionTokens: 9}) {
		t.Fatalf("got %+v", u)
	}
}

// TestSSEUsageScanner_OpenAI_FinalChunk feeds a realistic chat.completions
// SSE stream — several content-delta chunks with no usage, then the final
// chunk carrying it (stream_options.include_usage) — split across multiple
// Write calls at arbitrary byte boundaries, including mid-line, to prove
// the scanner buffers correctly regardless of how the network chunks it.
func TestSSEUsageScanner_OpenAI_FinalChunk(t *testing.T) {
	stream := "" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\" there\"}}],\"usage\":null}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"

	s := NewSSEUsageScanner(ShapeOpenAI)
	feedInChunks(t, s, stream, 7) // 7 bytes at a time: guarantees a mid-line split
	u, ok := s.Usage()
	if !ok {
		t.Fatalf("expected usage found")
	}
	if u != (Usage{PromptTokens: 5, CompletionTokens: 2}) {
		t.Fatalf("got %+v", u)
	}
}

// TestSSEUsageScanner_Anthropic_MessageEvents mirrors a real Anthropic
// stream: message_start carries input_tokens and an initial output_tokens
// of 0, then message_delta events carry the running output_tokens total.
// The last message_delta before message_stop is the true final count.
func TestSSEUsageScanner_Anthropic_MessageEvents(t *testing.T) {
	stream := "" +
		"event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":21,\"output_tokens\":0}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":null},\"usage\":{\"output_tokens\":3}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":9}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	s := NewSSEUsageScanner(ShapeAnthropic)
	feedInChunks(t, s, stream, 13)
	u, ok := s.Usage()
	if !ok {
		t.Fatalf("expected usage found")
	}
	if u != (Usage{PromptTokens: 21, CompletionTokens: 9}) {
		t.Fatalf("got %+v", u)
	}
}

func TestSSEUsageScanner_NoUsageEver(t *testing.T) {
	s := NewSSEUsageScanner(ShapeOpenAI)
	s.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
	s.Write([]byte("data: [DONE]\n\n"))
	if _, ok := s.Usage(); ok {
		t.Fatalf("expected no usage")
	}
}

// feedInChunks writes s in fixed-size pieces (the last one short), never as
// one Write, to exercise the scanner's line-buffering rather than relying
// on each SSE line arriving whole.
func feedInChunks(t *testing.T, w interface{ Write([]byte) (int, error) }, s string, chunk int) {
	t.Helper()
	b := []byte(s)
	for len(b) > 0 {
		n := chunk
		if n > len(b) {
			n = len(b)
		}
		if _, err := w.Write(b[:n]); err != nil {
			t.Fatalf("Write: %v", err)
		}
		b = b[n:]
	}
}
