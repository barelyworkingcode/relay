package modelbroker

import (
	"bytes"
	"encoding/json"
)

// Usage is the token-count pair spec §7 puts in a model_call audit record.
// It is the only thing this package ever extracts from a response body —
// never the content that produced it.
type Usage struct {
	PromptTokens     int64
	CompletionTokens int64
}

// ParseOpenAIUsage reads {"usage":{"prompt_tokens":N,"completion_tokens":N}}
// from the top level of a non-streamed OpenAI-shaped response body (chat
// completions or responses).
func ParseOpenAIUsage(body []byte) (Usage, bool) {
	var env struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Usage == nil {
		return Usage{}, false
	}
	return Usage{PromptTokens: env.Usage.PromptTokens, CompletionTokens: env.Usage.CompletionTokens}, true
}

// ParseAnthropicUsage reads the top-level "usage":{"input_tokens":N,
// "output_tokens":N} object from a non-streamed Anthropic Messages response.
func ParseAnthropicUsage(body []byte) (Usage, bool) {
	var env struct {
		Usage *struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Usage == nil {
		return Usage{}, false
	}
	return Usage{PromptTokens: env.Usage.InputTokens, CompletionTokens: env.Usage.OutputTokens}, true
}

// maxSSELineBytes bounds one buffered SSE line inside SSEUsageScanner. A
// real usage event is a few hundred bytes; a content-delta line can be
// larger but is never the line this scanner needs to read in full — it is
// deliberately truncated rather than grown without limit, since scanning it
// for "usage" would fail the same way either way once it isn't valid JSON.
const maxSSELineBytes = 1 << 20

// SSEUsageScanner extracts the final token-usage figures from an OpenAI- or
// Anthropic-shaped SSE stream as it passes through, retaining nothing but
// those numbers and the current partial line. It implements io.Writer so a
// caller can wire it in with io.MultiWriter/io.TeeReader alongside the
// unmodified copy that goes to the client — this package never sees, and
// never needs to see, the whole stream at once.
type SSEUsageScanner struct {
	shape Shape
	line  []byte
	usage Usage
	have  bool
}

// NewSSEUsageScanner returns a scanner for one response of the given shape.
func NewSSEUsageScanner(shape Shape) *SSEUsageScanner {
	return &SSEUsageScanner{shape: shape}
}

// Write implements io.Writer. It never returns an error itself — a
// malformed or truncated stream simply leaves Usage unset, which is the
// same "no usage available" outcome as a well-formed stream that never
// reported one.
func (s *SSEUsageScanner) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.appendLine(p)
			break
		}
		s.appendLine(p[:i])
		s.consumeLine()
		s.line = s.line[:0]
		p = p[i+1:]
	}
	return total, nil
}

func (s *SSEUsageScanner) appendLine(b []byte) {
	if len(s.line) >= maxSSELineBytes {
		return
	}
	if room := maxSSELineBytes - len(s.line); len(b) > room {
		b = b[:room]
	}
	s.line = append(s.line, b...)
}

func (s *SSEUsageScanner) consumeLine() {
	line := bytes.TrimRight(s.line, "\r")
	data, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "[DONE]" {
		return
	}
	switch s.shape {
	case ShapeOpenAI:
		if u, ok := ParseOpenAIUsage(data); ok {
			s.usage = u
			s.have = true
		}
	case ShapeAnthropic:
		s.consumeAnthropicEvent(data)
	}
}

// consumeAnthropicEvent reads one SSE data payload's "type" field to find
// message_start (which carries the real input_tokens, and an initial
// output_tokens of 0) and message_delta (which carries the running
// output_tokens total — the last one seen before message_stop is the
// final count). Anthropic never repeats input_tokens on message_delta, so a
// delta event only ever updates the completion side.
func (s *SSEUsageScanner) consumeAnthropicEvent(data []byte) {
	var env struct {
		Type    string `json:"type"`
		Message *struct {
			Usage *struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Usage *struct {
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return
	}
	switch env.Type {
	case "message_start":
		if env.Message != nil && env.Message.Usage != nil {
			s.usage.PromptTokens = env.Message.Usage.InputTokens
			s.usage.CompletionTokens = env.Message.Usage.OutputTokens
			s.have = true
		}
	case "message_delta":
		if env.Usage != nil {
			s.usage.CompletionTokens = env.Usage.OutputTokens
			s.have = true
		}
	}
}

// Usage returns the last usage figures observed, and whether any were.
func (s *SSEUsageScanner) Usage() (Usage, bool) {
	return s.usage, s.have
}
