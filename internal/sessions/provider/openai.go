package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// errChatModelKeyRequired is returned instead of ever sending a chat
// request with no Authorization header: relay-sessions' own peer identity
// on the model broker socket carries enough capability to make an
// unauthenticated request succeed for the wrong reason (Ping's /v1/models
// call, scoped by the *service* rather than the calling project), not
// because the session is actually authorized.
var errChatModelKeyRequired = errors.New("chat: model key is required")

// dialFunc is the chat transport's single seam onto relay's model broker.
// Every request the transport makes goes through exactly this function --
// there is no endpoint field, no configurable host, and no alternate
// transport anywhere in this file, so a chat session can never be pointed
// at anything but ModelSocket in production, and a hermetic test can
// substitute a fake broker only by replacing this seam, never by handing
// the transport a URL.
type dialFunc func(ctx context.Context) (net.Conn, error)

// unixDialer builds the production dialFunc: every call dials socketPath
// over a Unix socket, ignoring whatever host/port net/http computed from
// the request URL (see chatBrokerBaseURL below and modelproxy.go for the
// identical pattern pi's overlay already uses).
func unixDialer(socketPath string) dialFunc {
	d := net.Dialer{}
	return func(ctx context.Context) (net.Conn, error) {
		return d.DialContext(ctx, "unix", socketPath)
	}
}

// chatBrokerBaseURL is a fixed placeholder host: the transport's
// http.Transport.DialContext ignores the network/address net/http derives
// from it and always calls the seam instead, so this string is never
// resolved or dialed as a real hostname.
const chatBrokerBaseURL = "http://model.sock"

// chatHTTPTransport implements chatTransport against relay's model broker's
// OpenAI-compatible /v1/chat/completions route (model_endpoint.go).
type chatHTTPTransport struct {
	model    string
	modelKey string
	client   *http.Client
	settings BaseChatSettings

	// iterCounter scopes synthesized tool_call IDs to a specific invocation
	// of AppendAssistantWithToolCalls so two successive tool-using turns
	// within one conversation don't reuse the same ID.
	iterCounter atomic.Uint64
}

// newChatHTTPTransport constructs a transport for cfg. dial defaults to
// unixDialer(cfg.ModelSocket) when cfg.dial is nil (production); a test
// supplies cfg.dial directly.
func newChatHTTPTransport(cfg ChatConfig, model string, settings json.RawMessage) *chatHTTPTransport {
	dial := cfg.dial
	if dial == nil {
		dial = unixDialer(cfg.ModelSocket)
	}
	return &chatHTTPTransport{
		model:    stripManagedAliasPrefix(model),
		modelKey: cfg.ModelKey,
		client: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dial(ctx)
			},
		}},
		settings: parseBaseSettings(settings),
	}
}

// stripManagedAliasPrefix drops a "llama/" or "mlx/" prefix so a session
// carrying relayLLM's old managed-server model spelling (session.go's
// deriveProviderType, dropped under relay per SP10/R-S7d) still addresses
// the broker's catalog by the alias's bare id. Relay's own model broker
// resolves either spelling (internal/modelbroker/normalise.go's
// canonicalize), but sending the bare id here means this session's choice
// of spelling never depends on that resolution succeeding.
func stripManagedAliasPrefix(model string) string {
	for _, prefix := range []string{"llama/", "mlx/"} {
		if rest, ok := strings.CutPrefix(model, prefix); ok {
			return rest
		}
	}
	return model
}

// addAuth attaches the session's model key as a Bearer credential -- the
// only auth this transport ever sends, and the only header any request from
// it carries a credential in.
func (t *chatHTTPTransport) addAuth(req *http.Request) {
	if t.modelKey != "" {
		req.Header.Set("Authorization", "Bearer "+t.modelKey)
	}
}

func (t *chatHTTPTransport) Ping(ctx context.Context) error {
	if t.modelKey == "" {
		return errChatModelKeyRequired
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, chatBrokerBaseURL+"/v1/models", nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	t.addAuth(req)

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("model broker not reachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("model broker /v1/models returned %d", resp.StatusCode)
	}
	return nil
}

// BuildMessages converts session history into OpenAI chat format. Images
// are embedded as content blocks in the user message, and tool result
// messages carry a tool_call_id pairing them back to the assistant entry
// that produced them.
func (t *chatHTTPTransport) BuildMessages(systemPrompt string, msgs []sessionstypes.Message) []map[string]any {
	result := make([]map[string]any, 0, len(msgs)+1)

	if systemPrompt != "" {
		result = append(result, map[string]any{
			"role":    "system",
			"content": systemPrompt,
		})
	}

	var lastAssistantCalls []NormalizedToolCall
	var toolResultIdx int

	for idx, msg := range msgs {
		switch msg.Role {
		case "tool":
			entry := map[string]any{
				"role":    "tool",
				"content": sessionstypes.ExtractTextContent(msg),
			}
			if toolResultIdx < len(lastAssistantCalls) {
				if id := lastAssistantCalls[toolResultIdx].ID; id != "" {
					entry["tool_call_id"] = id
				} else {
					entry["tool_call_id"] = synthesizeToolCallID(
						scopeForHistoryMsg(idx-toolResultIdx-1), msg.ToolName, toolResultIdx)
				}
				toolResultIdx++
			}
			result = append(result, entry)

		case "assistant":
			entry := map[string]any{
				"role":    "assistant",
				"content": sessionstypes.ExtractTextContent(msg),
			}
			if norm := ToolCallsFromContent(msg.Content); len(norm) > 0 {
				scope := scopeForHistoryMsg(idx)
				for i := range norm {
					if norm[i].ID == "" {
						norm[i].ID = synthesizeToolCallID(scope, norm[i].Name, i)
					}
				}
				entry["tool_calls"] = buildOpenAIToolCallEntries(scope, norm)
				lastAssistantCalls = norm
			} else {
				lastAssistantCalls = nil
			}
			toolResultIdx = 0
			result = append(result, entry)

		default: // "user" and any other role
			lastAssistantCalls = nil
			toolResultIdx = 0

			text := sessionstypes.ExtractTextContent(msg)
			if len(msg.Files) == 0 {
				result = append(result, map[string]any{
					"role":    msg.Role,
					"content": text,
				})
				continue
			}
			parts := make([]map[string]any, 0, 1+len(msg.Files))
			if text != "" {
				parts = append(parts, map[string]any{
					"type": "text",
					"text": text,
				})
			}
			for _, f := range msg.Files {
				url := f.Data
				if !strings.HasPrefix(url, "data:") {
					mime := f.MimeType
					if mime == "" {
						mime = "image/png"
					}
					url = fmt.Sprintf("data:%s;base64,%s", mime, url)
				}
				parts = append(parts, map[string]any{
					"type": "image_url",
					"image_url": map[string]any{
						"url": url,
					},
				})
			}
			result = append(result, map[string]any{
				"role":    msg.Role,
				"content": parts,
			})
		}
	}
	return result
}

// PostChat sends a streaming /v1/chat/completions request.
func (t *chatHTTPTransport) PostChat(ctx context.Context, messages []map[string]any, tools []map[string]any) (*http.Response, error) {
	if t.modelKey == "" {
		return nil, errChatModelKeyRequired
	}
	body := t.buildChatBody(messages, tools)
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatBrokerBaseURL+"/v1/chat/completions", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	t.addAuth(req)

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, decodeChatError(resp.StatusCode, bodyBytes)
	}
	return resp, nil
}

// decodeChatError turns a non-2xx response body into a user-facing error.
// The model broker returns {"error":{"message":...}} on its own request
// errors (model_endpoint.go's errorBodyFor) and forwards the upstream's
// body unchanged on an upstream error, which is usually the same shape.
func decodeChatError(status int, body []byte) error {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &parsed) == nil && parsed.Error.Message != "" {
		msg = parsed.Error.Message
	}
	return fmt.Errorf("HTTP %d: %s", status, msg)
}

func (t *chatHTTPTransport) buildChatBody(messages []map[string]any, tools []map[string]any) map[string]any {
	body := map[string]any{
		"model":    t.model,
		"messages": messages,
		"stream":   true,
		// Required for relay's own audit trail: model_endpoint.go scans this
		// same SSE stream for usage to build the model_call record's token
		// counts, and an upstream only emits that chunk when asked.
		"stream_options": map[string]any{"include_usage": true},
	}
	if t.settings.Temperature != nil {
		body["temperature"] = *t.settings.Temperature
	}
	if t.settings.TopP != nil {
		body["top_p"] = *t.settings.TopP
	}
	if t.settings.MaxTokens != nil {
		body["max_tokens"] = *t.settings.MaxTokens
	}
	if len(tools) > 0 {
		body["tools"] = tools
		body["tool_choice"] = "auto"
	}
	return body
}

// openAIStreamChunk mirrors the OpenAI /chat/completions streaming chunk
// shape. Fields not directly used are kept for forward-compat.
type openAIStreamChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string                `json:"role"`
			Content          string                `json:"content"`
			ReasoningContent string                `json:"reasoning_content"`
			Reasoning        string                `json:"reasoning"`
			ToolCalls        []openAIToolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openAIUsage `json:"usage"`
}

type openAIToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// StreamChunks reads the SSE-formatted response body, emits text/thinking/
// tool deltas through emit, and returns the accumulated result.
func (t *chatHTTPTransport) StreamChunks(resp *http.Response, startTime time.Time, emit func(ChatDelta)) NormalizedStreamResult {
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)

	var fullText strings.Builder
	var usage *openAIUsage
	var firstTokenAt time.Time

	startedTools := make(map[int]bool)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			break
		}

		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			slog.Warn("chat: invalid SSE chunk", "error", err, "lineBytes", len(line))
			continue
		}

		if chunk.Usage != nil {
			usage = chunk.Usage
		}

		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta

		reasoning := delta.ReasoningContent
		if reasoning == "" {
			reasoning = delta.Reasoning
		}
		if reasoning != "" {
			if firstTokenAt.IsZero() {
				firstTokenAt = time.Now()
			}
			emit(ChatDelta{Thinking: reasoning})
		}

		if delta.Content != "" {
			if firstTokenAt.IsZero() {
				firstTokenAt = time.Now()
			}
			fullText.WriteString(delta.Content)
			emit(ChatDelta{Text: delta.Content})
		}

		for _, tcd := range delta.ToolCalls {
			if !startedTools[tcd.Index] && tcd.Function.Name != "" {
				emit(ChatDelta{ToolStart: &ToolStartEvent{
					Index: tcd.Index,
					ID:    tcd.ID,
					Name:  tcd.Function.Name,
				}})
				startedTools[tcd.Index] = true
			}
			if tcd.Function.Arguments != "" {
				emit(ChatDelta{ToolArgs: &ToolArgsEvent{
					Index:   tcd.Index,
					Partial: tcd.Function.Arguments,
				}})
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return NormalizedStreamResult{FullText: fullText.String(), Err: err}
	}

	stats := sessionstypes.SessionStats{}
	if usage != nil {
		stats.InputTokens = usage.PromptTokens
		stats.OutputTokens = usage.CompletionTokens
	}
	if !firstTokenAt.IsZero() {
		stats.TimeToFirstToken = firstTokenAt.Sub(startTime).Seconds()
		if stats.OutputTokens > 0 {
			elapsed := time.Since(firstTokenAt).Seconds()
			if elapsed > 0 {
				stats.TokensPerSecond = float64(stats.OutputTokens) / elapsed
			}
		}
	}

	return NormalizedStreamResult{
		FullText: fullText.String(),
		Stats:    stats,
	}
}

// AppendAssistantWithToolCalls adds an assistant-with-tool-calls entry in
// OpenAI's wire shape. The iterCounter scope ensures synthesized IDs from
// one live turn don't collide with synthesized IDs from a later turn.
func (t *chatHTTPTransport) AppendAssistantWithToolCalls(messages []map[string]any, text string, toolCalls []NormalizedToolCall) []map[string]any {
	scope := scopeForIter(t.iterCounter.Add(1))
	return append(messages, map[string]any{
		"role":       "assistant",
		"content":    text,
		"tool_calls": buildOpenAIToolCallEntries(scope, toolCalls),
	})
}

// AppendToolResult adds a tool result entry with the required tool_call_id.
func (t *chatHTTPTransport) AppendToolResult(messages []map[string]any, tc NormalizedToolCall, result string) []map[string]any {
	id := tc.ID
	if id == "" {
		id = synthesizeToolCallID(scopeForIter(t.iterCounter.Load()), tc.Name, 0)
	}
	return append(messages, map[string]any{
		"role":         "tool",
		"tool_call_id": id,
		"content":      result,
	})
}

// synthesizeToolCallID produces a deterministic fallback id for tool calls
// that didn't carry one. The scope makes the id unique across iterations
// within a single conversation.
func synthesizeToolCallID(scope, name string, index int) string {
	if scope == "" {
		return fmt.Sprintf("call_%s_%d", name, index)
	}
	return fmt.Sprintf("call_%s_%s_%d", scope, name, index)
}

func scopeForHistoryMsg(idx int) string { return fmt.Sprintf("m%d", idx) }
func scopeForIter(n uint64) string      { return fmt.Sprintf("t%d", n) }

// buildOpenAIToolCallEntries converts normalized tool calls into the
// OpenAI wire shape.
func buildOpenAIToolCallEntries(scope string, toolCalls []NormalizedToolCall) []map[string]any {
	out := make([]map[string]any, len(toolCalls))
	for i, n := range toolCalls {
		id := n.ID
		if id == "" {
			id = synthesizeToolCallID(scope, n.Name, i)
		}
		args := string(n.Arguments)
		if args == "" {
			args = "{}"
		}
		out[i] = map[string]any{
			"id":   id,
			"type": "function",
			"function": map[string]any{
				"name":      n.Name,
				"arguments": args,
			},
		}
	}
	return out
}
