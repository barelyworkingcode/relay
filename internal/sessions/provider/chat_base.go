package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/events"
	sessionsmcp "github.com/barelyworkingcode/relay/internal/sessions/mcp"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// ChatConfig carries everything Start needs for one chat session's HTTP
// client, resolved by the caller (the relay-sessions host process) exactly
// like ClaudeConfig/PiConfig -- see this package's doc comment on C5's env
// table. A chat session has no external CLI child: the "provider process"
// is this client talking to relay's own model broker.
type ChatConfig struct {
	ModelSocket string // C5 env table: RELAY_MODEL_SOCKET

	// ModelKey is this session's launch-or-resume model key (C8, "rmk_" +
	// 64 hex), presented to the model broker as a bearer. Never cached
	// across calls -- session.CreateSpec.ModelKey's own doc comment.
	ModelKey string

	// RelayMCPCommand is the absolute path to the command chat's own tool
	// loop spawns for relay's tools: unlike Claude/pi, a chat session has no
	// other process to serve as this launch's project_session root, so this
	// command is spawned through the shim (ShimBinary) rather than directly
	// -- see buildChatMCPManager.
	RelayMCPCommand string

	// ShimBinary is the absolute path to relay-sessions' own binary, run in
	// "exec" mode to wrap the tool child (buildChatMCPManager). Static
	// across every session this host spawns, like ModelSocket.
	ShimBinary string
	// BridgeSocket is C5's RELAY_BRIDGE_SOCKET: the shim's own Hello dials
	// it, not the tool child itself.
	BridgeSocket string

	// SandboxProfile is this launch's own absolute SBPL profile path (C7);
	// "" runs the tool child unsandboxed. Never cached across calls, same
	// rule as ModelKey and Identity below.
	SandboxProfile string
	// Identity is this launch's own project_session secret, presented by
	// the shim to relay's bridge socket. nil for a launch with no identity
	// to mint (ad-hoc, SSH-hosted) -- mirrors hostapi.LaunchRequest.Identity's
	// own nullability rule.
	Identity *sessionsmcp.IdentitySpec

	// dial overrides how the transport reaches ModelSocket. Test-only seam;
	// nil dials ModelSocket over a real Unix socket (see openai.go).
	dial dialFunc
}

// chatTransport abstracts the wire format for a streaming chat API.
// Lifecycle, the tool-call loop, and event emission live in ChatProvider; a
// transport only handles format-specific concerns (request shape, response
// parsing, auth, and how to append assistant/tool messages to the running
// conversation). Exactly one implementation exists today (chatHTTPTransport,
// openai.go) -- the interface exists so ChatProvider's own tests can
// substitute a scripted transport, not to anticipate a second wire format.
type chatTransport interface {
	Ping(ctx context.Context) error
	BuildMessages(systemPrompt string, msgs []sessionstypes.Message) []map[string]any
	PostChat(ctx context.Context, messages []map[string]any, tools []map[string]any) (*http.Response, error)
	StreamChunks(resp *http.Response, startTime time.Time, emit func(delta ChatDelta)) NormalizedStreamResult
	AppendAssistantWithToolCalls(messages []map[string]any, text string, toolCalls []NormalizedToolCall) []map[string]any
	AppendToolResult(messages []map[string]any, tc NormalizedToolCall, result string) []map[string]any
}

// NormalizedToolCall is the transport-agnostic representation of a single
// tool call emitted by the model. The ID is always populated -- synthesized
// by ChatProvider's stream state machine when the transport doesn't track
// ids natively.
type NormalizedToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// NormalizedStreamResult is what a transport returns after streaming one
// response. Tool calls are NOT in here -- ChatProvider builds them from the
// streamed deltas (single source of truth). FullText is the concatenated
// text content (excluding thinking and tool args).
type NormalizedStreamResult struct {
	FullText string
	Stats    sessionstypes.SessionStats
	Err      error
}

// ChatDelta is a single streamed piece of output from a transport. Exactly
// one field should be populated per call.
type ChatDelta struct {
	Text      string
	Thinking  string
	ToolStart *ToolStartEvent
	ToolArgs  *ToolArgsEvent
}

// ToolStartEvent signals that a new tool call has begun streaming. Index is
// the transport's stream-local accumulator key (matching subsequent
// ToolArgs events). ID may be empty; ChatProvider will synthesize one.
type ToolStartEvent struct {
	Index int
	ID    string
	Name  string
}

// ToolArgsEvent carries one fragment of a tool call's JSON-encoded
// arguments. Multiple ToolArgs events for the same Index are concatenated.
type ToolArgsEvent struct {
	Index   int
	Partial string
}

// BaseChatSettings holds the knobs a chat session's own Settings JSON may
// carry. A caller can never specify an MCP server to spawn here -- the only
// MCP server a chat session ever runs is the fixed "relay" entry
// buildChatMCPManager builds itself, gated by useRelayTools alone.
type BaseChatSettings struct {
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	MaxTokens   *int     `json:"max_tokens,omitempty"`
}

func parseBaseSettings(raw json.RawMessage) BaseChatSettings {
	var s BaseChatSettings
	if len(raw) == 0 {
		return s
	}
	_ = json.Unmarshal(raw, &s)
	return s
}

// buildChatMCPManager returns nil unless the session opted in via
// useRelayTools -- no tool child is ever spawned speculatively. The single
// "relay" entry it builds is always this process's own fixed,
// relay-controlled command (cfg.RelayMCPCommand "mcp"); nothing decoded
// from session.Settings ever reaches it.
//
// A chat session has no target process of its own -- ChatProvider is
// purely an in-process HTTP client -- so unlike ClaudeProvider's
// relayMCPConfig (a plain child of an already-rooted Claude process), this
// tool child is spawned through the shim (ShimSpec) to become its own
// project_session root: that is what makes relay's own ancestry-based tool
// auth resolve for it at all, and what gets it C7's default sandbox.
func buildChatMCPManager(cfg ChatConfig, session *sessionstypes.Session) sessionsmcp.MCPClient {
	if !useRelayTools(session.Settings) || cfg.RelayMCPCommand == "" {
		return nil
	}
	servers := map[string]sessionsmcp.MCPServerConfig{
		"relay": {
			Command: cfg.RelayMCPCommand,
			Args:    []string{"mcp"},
			Shim: &sessionsmcp.ShimSpec{
				Binary:         cfg.ShimBinary,
				SessionID:      session.ID,
				BridgeSocket:   cfg.BridgeSocket,
				Identity:       cfg.Identity,
				SandboxProfile: cfg.SandboxProfile,
			},
		},
	}
	return sessionsmcp.NewMCPManager(servers)
}

// ChatProvider implements sessionstypes.Provider by delegating format-
// specific work to a chatTransport. It owns the provider lifecycle, the
// tool-calling loop, and event emission -- ported from relayLLM's
// BaseChatProvider, trimmed to relay's own single upstream shape (no
// built-in tools, no managed-backend acquisition: both existed for
// providers this port does not carry forward, see SP10).
type ChatProvider struct {
	session    *sessionstypes.Session
	handler    sessionstypes.EventHandler
	transport  chatTransport
	mcpManager sessionsmcp.MCPClient

	mu         sync.Mutex
	started    atomic.Bool
	cancelFn   context.CancelFunc
	activeBody io.Closer // resp.Body of the in-flight stream; closed on stop
	generation atomic.Uint64
}

// NewChatProvider constructs a provider for session, talking to relay's
// model broker per cfg.
func NewChatProvider(session *sessionstypes.Session, handler sessionstypes.EventHandler, cfg ChatConfig) *ChatProvider {
	return &ChatProvider{
		session:    session,
		handler:    handler,
		transport:  newChatHTTPTransport(cfg, session.Model, session.Settings),
		mcpManager: buildChatMCPManager(cfg, session),
	}
}

// SetMCPClient replaces the MCP client built from session settings.
// Test-only seam -- production never calls this.
func (p *ChatProvider) SetMCPClient(c sessionsmcp.MCPClient) {
	p.mcpManager = c
}

func (p *ChatProvider) Start() error {
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pingCancel()
	if err := p.transport.Ping(pingCtx); err != nil {
		return fmt.Errorf("chat: ping: %w", err)
	}

	p.started.Store(true)

	if p.mcpManager != nil {
		mcpCtx, mcpCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer mcpCancel()
		if err := p.mcpManager.Start(mcpCtx); err != nil {
			slog.Warn("chat: MCP servers failed to start (tool calling disabled)",
				"session", p.session.ID, "error", err)
			p.mcpManager = nil
		}
	}

	slog.Info("chat provider started", "session", p.session.ID, "model", p.session.Model)
	return nil
}

func (p *ChatProvider) SendMessage(_ string, _ []sessionstypes.FileAttachment) error {
	if !p.started.Load() {
		return fmt.Errorf("chat: provider not started")
	}

	p.mu.Lock()
	messages := p.transport.BuildMessages(p.session.SystemPrompt, p.copyHistory())
	ctx, cancel := context.WithCancel(context.Background())
	p.cancelFn = cancel
	gen := p.generation.Add(1)
	p.mu.Unlock()

	tools := p.toolDefs()
	slog.Debug("chat: sending message", "session", p.session.ID, "tools", len(tools))

	resp, err := p.transport.PostChat(ctx, messages, tools)
	if err != nil {
		cancel()
		return fmt.Errorf("chat: %w", err)
	}

	if p.generation.Load() != gen {
		cancel()
		resp.Body.Close()
		return nil
	}

	go p.runToolLoop(ctx, cancel, resp, messages, time.Now(), gen)
	return nil
}

// copyHistory snapshots the session's message history under the session
// lock. The transport must not reach into session.Messages directly.
func (p *ChatProvider) copyHistory() []sessionstypes.Message {
	p.session.Lock()
	defer p.session.Unlock()
	msgs := make([]sessionstypes.Message, len(p.session.Messages))
	copy(msgs, p.session.Messages)
	return msgs
}

func (p *ChatProvider) toolDefs() []map[string]any {
	if p.mcpManager != nil && p.mcpManager.HasTools() {
		return p.mcpManager.ChatToolDefs()
	}
	return nil
}

// runToolLoop drives the conversation: stream the first response, and if
// the model emitted tool calls, execute them via MCP and loop with the
// updated message list until the model stops calling tools (or the
// iteration cap is hit). Runs in a goroutine -- the session layer only
// observes streaming events and eventually message_complete.
func (p *ChatProvider) runToolLoop(ctx context.Context, cancel context.CancelFunc, resp *http.Response, messages []map[string]any, startTime time.Time, gen uint64) {
	defer cancel()

	// Guarded emit: silently discards events if a newer generation has
	// started (i.e. StopGeneration or a new SendMessage was called).
	stale := func() bool { return p.generation.Load() != gen }
	guardedHandler := func(eventType string, data json.RawMessage) {
		if stale() {
			return
		}
		p.handler(eventType, data)
	}
	guardedEmitter := events.NewEventEmitter(func(eventType string, data json.RawMessage) {
		if stale() {
			return
		}
		p.handler(eventType, data)
	})

	const maxIterations = 10
	const maxToolResultLen = 8192

	var toolMessages []sessionstypes.Message

	// All tool-loop iterations are one assistant turn from the client's POV.
	if !stale() {
		guardedEmitter.MessageStart("")
	}
	if !stale() {
		var toolNames, mcpNames []string
		if p.mcpManager != nil && p.mcpManager.HasTools() {
			toolNames = p.mcpManager.ToolNames()
			mcpNames = p.mcpManager.ServerNames()
		}
		guardedEmitter.SystemInit(p.session.Model, p.session.Directory, toolNames, mcpNames)
	}

	for iteration := 0; iteration <= maxIterations; iteration++ {
		p.mu.Lock()
		p.activeBody = resp.Body
		p.mu.Unlock()

		state := newTurnStreamState(guardedEmitter)
		result := p.transport.StreamChunks(resp, startTime, func(d ChatDelta) {
			if stale() {
				return
			}
			switch {
			case d.ToolStart != nil:
				state.onToolStart(d.ToolStart)
			case d.ToolArgs != nil:
				state.onToolArgs(d.ToolArgs)
			case d.Thinking != "":
				state.onThinking(d.Thinking)
			case d.Text != "":
				state.onText(d.Text)
			}
		})
		toolCalls := state.finalize()

		p.mu.Lock()
		p.activeBody = nil
		p.mu.Unlock()

		if ctx.Err() != nil {
			// Stop requested: the session layer emits its own
			// message_complete.
			return
		}

		streamText := state.fullText.String()
		if streamText == "" {
			streamText = result.FullText
		}

		if result.Err != nil {
			slog.Error("chat: stream error", "session", p.session.ID, "error", result.Err)
			guardedHandler("error", mustJSON(map[string]string{"error": result.Err.Error()}))
			return
		}

		// Terminal condition: no more tool calls, no tool handler, or cap hit.
		if len(toolCalls) == 0 || p.mcpManager == nil || iteration == maxIterations {
			statsData, _ := json.Marshal(result.Stats)
			guardedHandler(events.HandlerStatsUpdate, statsData)

			if len(state.blocks) > 0 {
				toolMessages = append(toolMessages, sessionstypes.Message{
					Timestamp: timeNow(),
					Role:      "assistant",
					Content:   mustJSON(state.blocks),
				})
			}
			if len(toolMessages) > 0 {
				p.session.Lock()
				p.session.Messages = append(p.session.Messages, toolMessages...)
				p.session.Unlock()
			}

			guardedHandler(events.HandlerMessageComplete, nil)
			return
		}

		toolMessages = append(toolMessages, sessionstypes.Message{
			Timestamp: timeNow(),
			Role:      "assistant",
			Content:   mustJSON(state.blocks),
		})
		messages = p.transport.AppendAssistantWithToolCalls(messages, streamText, toolCalls)

		for _, tc := range toolCalls {
			if ctx.Err() != nil {
				return
			}

			toolResult, toolErr := p.mcpManager.CallTool(ctx, tc.Name, tc.Arguments, func(msg string) {
				guardedEmitter.ToolProgress(tc.ID, tc.Name, msg)
			})
			isError := toolErr != nil
			if toolErr != nil {
				if ctx.Err() != nil {
					return
				}
				toolResult = fmt.Sprintf("Error: %s", toolErr.Error())
				slog.Warn("chat: tool call failed", "tool", tc.Name, "error", toolErr)
			}
			if len(toolResult) > maxToolResultLen {
				toolResult = toolResult[:maxToolResultLen] + "\n...(truncated)"
			}

			guardedEmitter.ToolResult(tc.ID, tc.Name, toolResult, isError)

			resultContent, _ := json.Marshal(toolResult)
			toolMessages = append(toolMessages, sessionstypes.Message{
				Timestamp: timeNow(),
				Role:      "tool",
				Content:   resultContent,
				ToolName:  tc.Name,
				ToolUseID: tc.ID,
			})
			messages = p.transport.AppendToolResult(messages, tc, toolResult)
		}

		var err error
		resp, err = p.transport.PostChat(ctx, messages, p.toolDefs())
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			guardedHandler("error", mustJSON(map[string]string{"error": err.Error()}))
			return
		}
	}
}

func (p *ChatProvider) StopGeneration() {
	p.mu.Lock()
	cancel := p.cancelFn
	p.cancelFn = nil
	body := p.activeBody
	p.activeBody = nil
	// Increment generation before releasing p.mu -- see BaseChatProvider's
	// (relayLLM) original comment on this ordering, ported unchanged: any
	// events the old goroutine emits after this point are discarded by the
	// guarded handler, and no SendMessage between the unlock and this call
	// can be wrongly marked stale by it.
	p.generation.Add(1)
	p.mu.Unlock()

	if body != nil {
		body.Close()
	}
	if cancel != nil {
		cancel()
	}
	slog.Info("chat generation stopped", "session", p.session.ID)
}

func (p *ChatProvider) Kill() {
	p.StopGeneration()
	if p.mcpManager != nil {
		p.mcpManager.Close()
	}
	p.started.Store(false)
	slog.Info("chat provider killed", "session", p.session.ID)
}

func (p *ChatProvider) DeleteSession() error           { return nil }
func (p *ChatProvider) Alive() bool                    { return p.started.Load() }
func (p *ChatProvider) GetState() json.RawMessage      { return json.RawMessage(`{}`) }
func (p *ChatProvider) RestoreState(_ json.RawMessage) {}

func timeNow() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func mustJSON(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

// ToolCallsFromContent extracts tool calls from a persisted assistant
// sessionstypes.Message.Content. Tool calls live inside canonical content
// blocks as tool_use entries -- single source of truth on history replay.
func ToolCallsFromContent(content json.RawMessage) []NormalizedToolCall {
	if len(content) == 0 || !bytes.Contains(content, toolUseMarker) {
		return nil
	}
	var blocks []struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil
	}
	var out []NormalizedToolCall
	for _, b := range blocks {
		if b.Type != events.BlockToolUse || b.Name == "" {
			continue
		}
		args := b.Input
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		out = append(out, NormalizedToolCall{
			ID:        b.ID,
			Name:      b.Name,
			Arguments: args,
		})
	}
	return out
}

var toolUseMarker = []byte(`"tool_use"`)

// turnStreamState drives canonical event emission for a single streamed
// response from the transport. It tracks the open content block (so
// transitions emit content_block_stop + content_block_start), assigns
// canonical block indices, and accumulates tool-call state so that at
// stream end the caller can read out the resolved tool calls.
type turnStreamState struct {
	emitter *events.EventEmitter

	nextBlockIdx int

	openKind    blockKind
	openIndex   int
	openToolIdx int

	tools     map[int]*toolBlockState
	toolOrder []int

	openBlockText strings.Builder
	fullText      strings.Builder

	blocks []json.RawMessage
}

type blockKind int

const (
	blockNone blockKind = iota
	blockOpenText
	blockOpenThinking
	blockOpenToolUse
)

type toolBlockState struct {
	blockIdx int
	id       string
	name     string
	args     strings.Builder
}

func newTurnStreamState(emitter *events.EventEmitter) *turnStreamState {
	return &turnStreamState{
		emitter: emitter,
		tools:   make(map[int]*toolBlockState),
	}
}

func (s *turnStreamState) onText(text string) {
	if s.openKind != blockOpenText {
		s.closeOpen()
		s.openKind = blockOpenText
		s.openIndex = s.nextBlockIdx
		s.nextBlockIdx++
		s.openBlockText.Reset()
		s.emitter.TextBlockStart(s.openIndex)
	}
	s.openBlockText.WriteString(text)
	s.fullText.WriteString(text)
	s.emitter.TextDelta(s.openIndex, text)
}

func (s *turnStreamState) onThinking(text string) {
	if s.openKind != blockOpenThinking {
		s.closeOpen()
		s.openKind = blockOpenThinking
		s.openIndex = s.nextBlockIdx
		s.nextBlockIdx++
		s.openBlockText.Reset()
		s.emitter.ThinkingBlockStart(s.openIndex)
	}
	s.openBlockText.WriteString(text)
	s.emitter.ThinkingDelta(s.openIndex, text)
}

func (s *turnStreamState) onToolStart(ev *ToolStartEvent) {
	if _, exists := s.tools[ev.Index]; exists {
		return
	}
	s.closeOpen()
	id := ev.ID
	if id == "" {
		id = events.SynthesizeToolUseID(ev.Index, ev.Name)
	}
	tb := &toolBlockState{
		blockIdx: s.nextBlockIdx,
		id:       id,
		name:     ev.Name,
	}
	s.tools[ev.Index] = tb
	s.toolOrder = append(s.toolOrder, ev.Index)
	s.openKind = blockOpenToolUse
	s.openIndex = s.nextBlockIdx
	s.openToolIdx = ev.Index
	s.nextBlockIdx++
	s.emitter.ToolUseBlockStart(tb.blockIdx, tb.id, tb.name)
}

func (s *turnStreamState) onToolArgs(ev *ToolArgsEvent) {
	tb, ok := s.tools[ev.Index]
	if !ok {
		return
	}
	tb.args.WriteString(ev.Partial)
	s.emitter.InputJsonDelta(tb.blockIdx, ev.Partial)
}

// closeOpen emits content_block_stop for whatever block is currently open
// and appends the resolved block to s.blocks. Empty text/thinking blocks
// aren't persisted (no user-visible content).
func (s *turnStreamState) closeOpen() {
	switch s.openKind {
	case blockOpenText, blockOpenThinking:
		blockType, contentKey := events.BlockText, "text"
		if s.openKind == blockOpenThinking {
			blockType, contentKey = events.BlockThinking, "thinking"
		}
		if text := s.openBlockText.String(); text != "" {
			block, _ := json.Marshal(map[string]any{"type": blockType, contentKey: text})
			s.blocks = append(s.blocks, block)
		}
		s.emitter.BlockStop(s.openIndex)
	case blockOpenToolUse:
		if tb := s.tools[s.openToolIdx]; tb != nil {
			args := strings.TrimSpace(tb.args.String())
			if args == "" {
				args = "{}"
			}
			block, _ := json.Marshal(map[string]any{
				"type":  events.BlockToolUse,
				"id":    tb.id,
				"name":  tb.name,
				"input": json.RawMessage(args),
			})
			s.blocks = append(s.blocks, block)
			s.emitter.ToolUseBlockStop(tb.blockIdx, tb.id, tb.name, json.RawMessage(args))
		}
	}
	s.openKind = blockNone
}

// finalize closes any still-open block at end of stream. Returns the
// resolved tool calls in the order they were started.
func (s *turnStreamState) finalize() []NormalizedToolCall {
	s.closeOpen()
	out := make([]NormalizedToolCall, 0, len(s.toolOrder))
	for _, idx := range s.toolOrder {
		tb := s.tools[idx]
		args := strings.TrimSpace(tb.args.String())
		if args == "" {
			args = "{}"
		}
		out = append(out, NormalizedToolCall{
			ID:        tb.id,
			Name:      tb.name,
			Arguments: json.RawMessage(args),
		})
	}
	return out
}
