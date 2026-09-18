package provider

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/barelyworkingcode/relay/internal/sessions/events"
	sessionsmcp "github.com/barelyworkingcode/relay/internal/sessions/mcp"
	"github.com/barelyworkingcode/relay/internal/sessions/pioverlay"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const piIdleTimeout = 15 * time.Minute
const piRPCTimeout = 10 * time.Second

// PiConfig carries everything Start needs to build one pi CLI child's argv
// and env, resolved by the caller rather than read back out of this
// process's own environment.
type PiConfig struct {
	Binary    string // resolved pi binary path; "" falls back to well-known locations then PATH
	DataDir   string // relay-sessions data dir; pi session JSONLs live under {DataDir}/pi-sessions
	ExtraArgs []string

	BridgeSocket string // C5 env table: RELAY_BRIDGE_SOCKET
	ModelSocket  string // C5 env table: RELAY_MODEL_SOCKET — also the source startModelProxy bridges for pi's overlay baseUrl

	// ModelKey is the session's own LaunchSpec model key (C8, "rmk_" + 64
	// hex), already minted by relay at launch time. Empty disables the pi
	// overlay entirely — pi falls back to its own global ~/.pi/agent/
	// config, unauthenticated against relay's model broker.
	ModelKey string

	// ShimBinary is the absolute path to relay-sessions' own binary, run in
	// "exec" mode to wrap the pi child. Empty means a direct spawn, which is
	// refused outright when SandboxProfile or Identity is set (Start).
	ShimBinary string

	// SandboxProfile is this launch's own absolute SBPL profile path (C7);
	// "" runs the child unsandboxed. Never cached across calls, same rule as
	// Identity below.
	SandboxProfile string

	// Identity is this launch's own project_session secret, presented by the
	// shim to relay's bridge socket — never by the pi child itself, and never
	// in argv. nil for a launch with no identity to mint.
	Identity *sessionsmcp.IdentitySpec
}

// PiProvider manages a persistent pi CLI process in `--mode rpc`. The wire
// format is JSONL: commands written to stdin one per line, responses +
// events read from stdout one per line. Translates pi's event vocabulary
// into the same canonical event stream ClaudeProvider produces.
type PiProvider struct {
	session *sessionstypes.Session
	handler sessionstypes.EventHandler
	cfg     PiConfig

	cmd   *exec.Cmd
	stdin io.WriteCloser
	mu    sync.Mutex
	alive atomic.Bool

	// targetPID is the shim's own child (the real pi process) when this
	// launch went through the shim — 0 on a direct spawn. Used by Kill's
	// hard-kill fallback to reach the target's whole process group, not just
	// the shim.
	targetPID int

	piSessionID   string
	modelID       string
	thinkingLevel string
	directory     string

	modelProxy *modelProxy
	spawnFiles []string
	// overlayDir is the pi project overlay directory (models.json holds the
	// live LaunchSpec model key at rest — SP9's finding) materialized for
	// the current spawn, if any. Removed on Kill so the key does not sit on
	// disk for longer than the session that owns it — SP9's "residual-key
	// window" callout, closed at this package's own layer rather than left
	// to depend solely on relay's key table expiring the key server-side.
	overlayDir string

	rpcMu      sync.Mutex
	rpcPending map[string]chan json.RawMessage

	emitter *events.EventEmitter

	streamMu        sync.Mutex
	currentBlockIdx int
	piIdxToRelay    map[int]int
	openRelayIdx    int
	openKind        string
	openText        strings.Builder
	openToolID      string
	openToolName    string
	openToolArgs    strings.Builder
	allBlocks       []map[string]any
	toolNamesByID   map[string]string

	lastActivity atomic.Int64
	stopIdle     chan struct{}
	stopIdleOnce sync.Once
	waitDone     chan struct{}

	msgStartNano   atomic.Int64
	firstTokenNano atomic.Int64
}

func NewPiProvider(session *sessionstypes.Session, handler sessionstypes.EventHandler, cfg PiConfig) *PiProvider {
	p := &PiProvider{
		session:       session,
		handler:       handler,
		cfg:           cfg,
		modelID:       session.Model,
		thinkingLevel: session.ThinkingLevel,
		directory:     session.Directory,
		rpcPending:    make(map[string]chan json.RawMessage),
		piIdxToRelay:  make(map[int]int),
		toolNamesByID: make(map[string]string),
	}
	p.emitter = events.NewEventEmitter(handler)
	return p
}

func (p *PiProvider) touchActivity() {
	p.lastActivity.Store(time.Now().Unix())
}

func (p *PiProvider) idleWatcher() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopIdle:
			return
		case <-ticker.C:
			idle := time.Now().Unix() - p.lastActivity.Load()
			if idle > int64(piIdleTimeout.Seconds()) {
				slog.Info("pi process idle, killing", "session", p.session.ID, "idleSecs", idle)
				p.Kill()
				return
			}
		}
	}
}

func (p *PiProvider) sessionDir() string {
	return filepath.Join(p.cfg.DataDir, "pi-sessions")
}

// resolveSkillDir returns the project skills directory to auto-mount via
// --skill, or "" to skip. Skipped when the user already wired --skill
// through extraArgs, or when the convention dir (<project>/.claude/skills)
// is absent. Pure filesystem convention, no credential involved.
func (p *PiProvider) resolveSkillDir() string {
	if hasArg(p.cfg.ExtraArgs, "--skill") {
		return ""
	}
	if p.directory == "" {
		return ""
	}
	candidate := filepath.Join(p.directory, ".claude", "skills")
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		return candidate
	}
	return ""
}

// buildPiArgs assembles the `pi --mode rpc` argv. sessionDir is the resolved
// --session-dir; skillDir/sysPromptPath are resolved file paths ("" to
// omit). extraArgs is appended verbatim — no ${...} placeholder expansion:
// relayLLM's expansion supported ${RELAY_TOKEN}, exactly the mechanism
// internal/config/templates.go already refuses for terminal templates: a
// caller-supplied extraArgs list must not be able to *name* a credential
// into existence here either.
func (p *PiProvider) buildPiArgs(sessionDir, skillDir, sysPromptPath string) []string {
	args := []string{"--mode", "rpc"}

	if p.modelID != "" {
		args = append(args, "--provider", pioverlay.RelayProvider, "--model", p.modelID)
	}
	if p.thinkingLevel != "" {
		args = append(args, "--thinking", p.thinkingLevel)
	}
	if p.piSessionID != "" {
		args = append(args, "--session", p.piSessionID)
	}

	args = append(args, "--session-dir", sessionDir)

	if sysPromptPath != "" {
		args = append(args, "--append-system-prompt", sysPromptPath)
	}

	if skillDir != "" {
		args = append(args, "--skill", skillDir)
	}

	args = append(args, p.cfg.ExtraArgs...)

	return args
}

func (p *PiProvider) Start() (err error) {
	p.cleanupSpawnFiles()
	p.closeModelProxyAndOverlay()

	defer func() {
		if err != nil {
			p.closeModelProxyAndOverlay()
		}
	}()

	sessionDir := p.sessionDir()
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return fmt.Errorf("create pi session dir: %w", err)
	}

	var sysPromptPath string
	if p.session.SystemPrompt != "" {
		path, err := writeSpawnFile(os.TempDir(), "pi-sysprompt-*.txt", []byte(p.session.SystemPrompt))
		if err != nil {
			return fmt.Errorf("write system prompt: %w", err)
		}
		sysPromptPath = path
		p.spawnFiles = append(p.spawnFiles, path)
	}

	args := p.buildPiArgs(sessionDir, p.resolveSkillDir(), sysPromptPath)

	piPath := resolvePiPath(p.cfg.Binary)

	spec := shimSpec{
		Binary:         p.cfg.ShimBinary,
		SessionID:      p.session.ID,
		BridgeSocket:   p.cfg.BridgeSocket,
		SandboxProfile: p.cfg.SandboxProfile,
		Identity:       p.cfg.Identity,
	}
	var cmd *exec.Cmd
	var statusR *os.File
	var extraFiles []*os.File
	switch {
	case spec.wanted() && spec.Binary == "":
		return ErrShimRequired
	case spec.Identity != nil && spec.BridgeSocket == "":
		return ErrNoBridgeSocket
	case spec.wanted():
		var berr error
		cmd, statusR, extraFiles, berr = buildShimCmd(spec, piPath, args)
		if berr != nil {
			return berr
		}
	default:
		cmd = exec.Command(piPath, args...) // unchanged direct spawn
	}
	cmd.Dir = p.directory
	env := ensurePath(childBaseEnv())
	env = append(env, "PI_OFFLINE=1", "PI_SKIP_VERSION_CHECK=1")

	add := map[string]string{"RELAY_SESSION_ID": p.session.ID}
	if p.cfg.BridgeSocket != "" {
		add["RELAY_BRIDGE_SOCKET"] = p.cfg.BridgeSocket
	}
	if p.cfg.ModelSocket != "" {
		add["RELAY_MODEL_SOCKET"] = p.cfg.ModelSocket
	}
	env = mergeEnv(env, add)

	if p.cfg.ModelKey != "" {
		baseURL, proxy, err := startModelProxy(p.cfg.ModelSocket)
		if err != nil {
			return fmt.Errorf("pi: start model proxy: %w", err)
		}
		p.modelProxy = proxy
		if baseURL != "" {
			overlayDir, err := pioverlay.MaterializePiOverlay(p.directory, pioverlay.PiOverlayInputs{
				ModelID:        p.modelID,
				ModelKey:       p.cfg.ModelKey,
				BaseURL:        baseURL + "/v1",
				SupportsImages: true,
			})
			if err != nil {
				return fmt.Errorf("pi: %w", err)
			}
			if overlayDir != "" {
				p.overlayDir = overlayDir
				env = mergeEnv(env, map[string]string{"PI_CODING_AGENT_DIR": overlayDir})
			}
		}
	}
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start pi: %w", err)
	}
	// Load-bearing: without closing the parent's own copies, the status pipe
	// (and, when present, the identity secret pipe) never reaches EOF.
	for _, f := range extraFiles {
		_ = f.Close()
	}

	if statusR != nil {
		outcome, _ := readShimStatus(statusR)
		_ = statusR.Close()
		switch {
		case outcome.identityRefused:
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return fmt.Errorf("%w: session %s", ErrIdentityRefused, p.session.ID)
		case !outcome.started || outcome.spawnFailed:
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return fmt.Errorf("%w: errno=%d", ErrSpawnFailed, outcome.spawnErrno)
		default:
			p.targetPID = outcome.targetPID
		}
	}

	p.cmd = cmd
	p.stdin = stdin
	p.alive.Store(true)
	p.stopIdle = make(chan struct{})
	p.stopIdleOnce = sync.Once{}
	p.waitDone = make(chan struct{})
	p.touchActivity()

	go p.readStdout(stdout)
	go p.readStderr(stderr)
	go p.waitForExit()
	go p.idleWatcher()

	slog.Info("pi process started", "session", p.session.ID, "model", p.modelID, "pid", cmd.Process.Pid, "targetPid", p.targetPID)

	go p.fetchInitialState()

	return nil
}

func (p *PiProvider) cleanupSpawnFiles() {
	for _, path := range p.spawnFiles {
		_ = os.Remove(path)
	}
	p.spawnFiles = nil
}

// closeModelProxyAndOverlay tears down the model proxy listener and the
// overlay directory (with it, the live model key at rest in models.json)
// from any prior Start call. Called both at the top of Start — so a
// re-Start without an intervening Kill can't orphan the previous listener —
// and from Start's own error path, so a failed Start leaves neither a bound
// listener nor a key-bearing overlay behind.
func (p *PiProvider) closeModelProxyAndOverlay() {
	if p.modelProxy != nil {
		p.modelProxy.Close()
		p.modelProxy = nil
	}
	if p.overlayDir != "" {
		_ = os.RemoveAll(p.overlayDir)
		p.overlayDir = ""
	}
}

func (p *PiProvider) fetchInitialState() {
	resp, err := p.sendRPC(map[string]interface{}{"type": "get_state"})
	if err != nil {
		slog.Warn("pi get_state failed", "session", p.session.ID, "error", err)
		return
	}
	var r struct {
		Success bool `json:"success"`
		Data    struct {
			SessionID string `json:"sessionId"`
		} `json:"data"`
	}
	if json.Unmarshal(resp, &r) == nil && r.Success && r.Data.SessionID != "" {
		p.mu.Lock()
		p.piSessionID = r.Data.SessionID
		p.mu.Unlock()
	}
}

func (p *PiProvider) readStdout(r io.ReadCloser) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		p.processLine(json.RawMessage(append([]byte(nil), line...)))
	}

	if err := scanner.Err(); err != nil {
		slog.Error("pi stdout read error", "session", p.session.ID, "error", err)
	}
}

func (p *PiProvider) readStderr(r io.ReadCloser) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		text := scanner.Text()
		if text != "" {
			slog.Debug("pi stderr", "session", p.session.ID, "text", text)
		}
	}
}

func (p *PiProvider) waitForExit() {
	err := p.cmd.Wait()
	p.alive.Store(false)
	close(p.waitDone)

	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
	}

	slog.Info("pi process exited", "session", p.session.ID, "exitCode", exitCode)

	data, _ := json.Marshal(map[string]interface{}{"exitCode": exitCode})
	p.handler("process_exited", data)
}

func (p *PiProvider) processLine(raw json.RawMessage) {
	p.touchActivity()

	var envelope struct {
		Type string `json:"type"`
		ID   string `json:"id,omitempty"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		p.handler("raw_output", raw)
		return
	}

	if envelope.Type == "response" {
		p.deliverResponse(envelope.ID, raw)
		return
	}

	p.translate(envelope.Type, raw)
}

func (p *PiProvider) deliverResponse(id string, raw json.RawMessage) {
	if id == "" {
		return
	}
	p.rpcMu.Lock()
	ch, ok := p.rpcPending[id]
	if ok {
		delete(p.rpcPending, id)
	}
	p.rpcMu.Unlock()
	if ok {
		select {
		case ch <- raw:
		default:
		}
	}
}

func (p *PiProvider) allocBlockIndex(piIdx int) int {
	p.streamMu.Lock()
	defer p.streamMu.Unlock()
	if idx, ok := p.piIdxToRelay[piIdx]; ok {
		return idx
	}
	idx := p.currentBlockIdx
	p.currentBlockIdx++
	p.piIdxToRelay[piIdx] = idx
	return idx
}

func (p *PiProvider) finalizeOpenBlockLocked() {
	switch p.openKind {
	case events.BlockText:
		p.emitter.BlockStop(p.openRelayIdx)
		p.allBlocks = append(p.allBlocks, map[string]any{
			"type": events.BlockText,
			"text": p.openText.String(),
		})
	case events.BlockThinking:
		p.emitter.BlockStop(p.openRelayIdx)
		p.allBlocks = append(p.allBlocks, map[string]any{
			"type":     events.BlockThinking,
			"thinking": p.openText.String(),
		})
	case events.BlockToolUse:
		var input json.RawMessage
		if s := p.openToolArgs.String(); s != "" {
			input = json.RawMessage(s)
		}
		p.emitter.ToolUseBlockStop(p.openRelayIdx, p.openToolID, p.openToolName, input)
		if len(input) == 0 {
			input = json.RawMessage(`{}`)
		}
		p.allBlocks = append(p.allBlocks, map[string]any{
			"type":  events.BlockToolUse,
			"id":    p.openToolID,
			"name":  p.openToolName,
			"input": input,
		})
	}
	p.openKind = ""
	p.openText.Reset()
	p.openToolID = ""
	p.openToolName = ""
	p.openToolArgs.Reset()
}

func (p *PiProvider) startBlock(idx int, kind string) {
	p.streamMu.Lock()
	defer p.streamMu.Unlock()
	if p.openKind != "" && p.openRelayIdx != idx {
		p.finalizeOpenBlockLocked()
	}
	p.openRelayIdx = idx
	p.openKind = kind
}

// endBlock closes idx if it's the currently open block. A no-op if idx is
// already closed (pi sometimes emits out-of-order _end after auto-close).
func (p *PiProvider) endBlock(idx int) {
	p.streamMu.Lock()
	defer p.streamMu.Unlock()
	if p.openKind == "" || p.openRelayIdx != idx {
		return
	}
	p.finalizeOpenBlockLocked()
}

func (p *PiProvider) appendText(s string) {
	p.streamMu.Lock()
	p.openText.WriteString(s)
	p.streamMu.Unlock()
}
func (p *PiProvider) appendToolArgs(s string) {
	p.streamMu.Lock()
	p.openToolArgs.WriteString(s)
	p.streamMu.Unlock()
}
func (p *PiProvider) setOpenTool(id, name string) {
	p.streamMu.Lock()
	p.openToolID = id
	p.openToolName = name
	p.streamMu.Unlock()
}

func (p *PiProvider) resetTurnState() {
	p.streamMu.Lock()
	p.currentBlockIdx = 0
	p.piIdxToRelay = make(map[int]int)
	p.openKind = ""
	p.openRelayIdx = 0
	p.openText.Reset()
	p.openToolID = ""
	p.openToolName = ""
	p.openToolArgs.Reset()
	p.allBlocks = nil
	p.toolNamesByID = make(map[string]string)
	p.streamMu.Unlock()
}

func (p *PiProvider) rememberToolName(id, name string) {
	if id == "" || name == "" {
		return
	}
	p.streamMu.Lock()
	p.toolNamesByID[id] = name
	p.streamMu.Unlock()
}

func (p *PiProvider) lookupToolName(id string) string {
	if id == "" {
		return ""
	}
	p.streamMu.Lock()
	name := p.toolNamesByID[id]
	p.streamMu.Unlock()
	return name
}

func (p *PiProvider) flushOpenBlock() {
	p.streamMu.Lock()
	defer p.streamMu.Unlock()
	if p.openKind != "" {
		p.finalizeOpenBlockLocked()
	}
}

func (p *PiProvider) translate(eventType string, raw json.RawMessage) {
	switch eventType {
	case "agent_start":
		p.resetTurnState()
		p.firstTokenNano.Store(0)
		p.mu.Lock()
		modelID := p.modelID
		p.mu.Unlock()
		p.emitter.SystemInit(modelID, p.directory, nil, nil)
		p.emitter.MessageStart("")

	case "message_update":
		p.translateMessageUpdate(raw)

	case "tool_execution_end":
		p.translateToolResult(raw)

	case "agent_end":
		p.translateAgentEnd(raw)

	case "tool_execution_start", "tool_execution_update":
		var ev struct {
			ToolCallID string `json:"toolCallId"`
			ToolName   string `json:"toolName"`
		}
		if json.Unmarshal(raw, &ev) == nil {
			p.rememberToolName(ev.ToolCallID, ev.ToolName)
		}

	case "message_start", "message_end", "turn_start", "turn_end":
		// Bookkeeping — already covered by the finer message_update translations.

	case "auto_retry_start", "auto_retry_end":
		p.emitRetryNotice(eventType, raw)

	default:
		p.handler("raw_output", raw)
	}
}

func (p *PiProvider) emitRetryNotice(eventType string, raw json.RawMessage) {
	var text string
	switch eventType {
	case "auto_retry_start":
		var ev struct {
			Attempt      int     `json:"attempt"`
			MaxAttempts  int     `json:"maxAttempts"`
			DelayMs      float64 `json:"delayMs"`
			ErrorMessage string  `json:"errorMessage"`
		}
		_ = json.Unmarshal(raw, &ev)
		text = fmt.Sprintf("Retry %d/%d in %.0fs — %s",
			ev.Attempt, ev.MaxAttempts, ev.DelayMs/1000, shortenError(ev.ErrorMessage))
	case "auto_retry_end":
		var ev struct {
			Success    bool   `json:"success"`
			Attempt    int    `json:"attempt"`
			FinalError string `json:"finalError"`
		}
		_ = json.Unmarshal(raw, &ev)
		if ev.Success {
			text = fmt.Sprintf("Retry succeeded on attempt %d", ev.Attempt)
		} else {
			text = fmt.Sprintf("Retry failed after %d attempts — %s", ev.Attempt, shortenError(ev.FinalError))
		}
	}
	p.handler("raw_output", json.RawMessage([]byte(text)))
}

func shortenError(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unknown error"
	}
	if i := strings.Index(s, "{"); i > 0 {
		s = strings.TrimSpace(s[:i])
	}
	const max = 120
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

type piContentBlock struct {
	Type         string          `json:"type"`
	ContentIndex int             `json:"contentIndex"`
	Delta        string          `json:"delta,omitempty"`
	Content      string          `json:"content,omitempty"`
	Reason       string          `json:"reason,omitempty"`
	ToolCall     *piToolCall     `json:"toolCall,omitempty"`
	ToolCallID   string          `json:"toolCallId,omitempty"`
	ToolName     string          `json:"toolName,omitempty"`
	Partial      json.RawMessage `json:"partial,omitempty"`
}

type piToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

func (p *PiProvider) resolveToolIdentity(ev piContentBlock) (id, name string) {
	if ev.ToolCall != nil {
		id, name = ev.ToolCall.ID, ev.ToolCall.Name
	}
	if id == "" {
		id = ev.ToolCallID
	}
	if name == "" {
		name = ev.ToolName
	}
	if name == "" {
		name = p.lookupToolName(id)
	}
	if name == "" {
		slog.Warn("pi: toolcall_start missing tool name", "session", p.session.ID, "toolCallId", id)
		name = piMissingToolNamePlaceholder
	}
	return id, name
}

const piMissingToolNamePlaceholder = "tool"

func (p *PiProvider) translateMessageUpdate(raw json.RawMessage) {
	var msg struct {
		AssistantMessageEvent piContentBlock `json:"assistantMessageEvent"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return
	}
	ev := msg.AssistantMessageEvent
	switch ev.Type {
	case "start":
		// Bookkeeping — agent_start already opened the turn.

	case "text_start":
		p.firstTokenNano.CompareAndSwap(0, time.Now().UnixNano())
		idx := p.allocBlockIndex(ev.ContentIndex)
		p.startBlock(idx, events.BlockText)
		p.emitter.TextBlockStart(idx)

	case "text_delta":
		p.firstTokenNano.CompareAndSwap(0, time.Now().UnixNano())
		idx := p.allocBlockIndex(ev.ContentIndex)
		p.emitter.TextDelta(idx, ev.Delta)
		p.appendText(ev.Delta)

	case "text_end":
		idx := p.allocBlockIndex(ev.ContentIndex)
		p.endBlock(idx)

	case "thinking_start":
		p.firstTokenNano.CompareAndSwap(0, time.Now().UnixNano())
		idx := p.allocBlockIndex(ev.ContentIndex)
		p.startBlock(idx, events.BlockThinking)
		p.emitter.ThinkingBlockStart(idx)

	case "thinking_delta":
		p.firstTokenNano.CompareAndSwap(0, time.Now().UnixNano())
		idx := p.allocBlockIndex(ev.ContentIndex)
		p.emitter.ThinkingDelta(idx, ev.Delta)
		p.appendText(ev.Delta)

	case "thinking_end":
		idx := p.allocBlockIndex(ev.ContentIndex)
		p.endBlock(idx)

	case "toolcall_start":
		idx := p.allocBlockIndex(ev.ContentIndex)
		p.startBlock(idx, events.BlockToolUse)
		id, name := p.resolveToolIdentity(ev)
		p.setOpenTool(id, name)
		p.emitter.ToolUseBlockStart(idx, id, name)

	case "toolcall_delta":
		idx := p.allocBlockIndex(ev.ContentIndex)
		p.emitter.InputJsonDelta(idx, ev.Delta)
		p.appendToolArgs(ev.Delta)

	case "toolcall_end":
		idx := p.allocBlockIndex(ev.ContentIndex)
		p.endBlock(idx)

	case "done":
		// Carries the model's stop reason; translateAgentEnd signals turn end.

	case "error":
		p.emitter.ResultError(ev.Reason)
	}
}

func (p *PiProvider) translateToolResult(raw json.RawMessage) {
	var ev struct {
		ToolCallID string `json:"toolCallId"`
		ToolName   string `json:"toolName"`
		Result     struct {
			Content json.RawMessage `json:"content"`
		} `json:"result"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return
	}
	p.emitter.ToolResult(ev.ToolCallID, ev.ToolName, sessionstypes.FlattenTextBlocks(ev.Result.Content), ev.IsError)
}

func (p *PiProvider) translateAgentEnd(raw json.RawMessage) {
	p.flushOpenBlock()

	stats := extractUsageFromAgentEnd(raw)

	startNano := p.msgStartNano.Load()
	firstNano := p.firstTokenNano.Load()
	nowNano := time.Now().UnixNano()
	if startNano > 0 && firstNano > 0 {
		stats.TimeToFirstToken = float64(firstNano-startNano) / 1e9
		genSecs := float64(nowNano-firstNano) / 1e9
		if genSecs > 0 && stats.OutputTokens > 0 {
			stats.TokensPerSecond = float64(stats.OutputTokens) / genSecs
		}
	}

	statsData, _ := json.Marshal(stats)
	p.handler(events.HandlerStatsUpdate, statsData)

	p.streamMu.Lock()
	blocks := p.allBlocks
	p.allBlocks = nil
	p.streamMu.Unlock()
	if len(blocks) > 0 {
		contentJSON, _ := json.Marshal(blocks)
		p.session.Lock()
		p.session.Messages = append(p.session.Messages, sessionstypes.Message{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Role:      "assistant",
			Content:   contentJSON,
		})
		p.session.Unlock()
	}

	p.handler(events.HandlerMessageComplete, nil)
}

func extractUsageFromAgentEnd(raw json.RawMessage) sessionstypes.SessionStats {
	var ev struct {
		Messages []struct {
			Role  string `json:"role"`
			Usage struct {
				Input      int `json:"input"`
				Output     int `json:"output"`
				CacheRead  int `json:"cacheRead"`
				CacheWrite int `json:"cacheWrite"`
				Cost       struct {
					Total float64 `json:"total"`
				} `json:"cost"`
			} `json:"usage"`
		} `json:"messages"`
	}
	var s sessionstypes.SessionStats
	if err := json.Unmarshal(raw, &ev); err != nil {
		return s
	}
	for _, m := range ev.Messages {
		if m.Role != "assistant" {
			continue
		}
		s.InputTokens += m.Usage.Input
		s.OutputTokens += m.Usage.Output
		s.CacheReadTokens += m.Usage.CacheRead
		s.CacheCreationTokens += m.Usage.CacheWrite
		s.CostUsd += m.Usage.Cost.Total
	}
	return s
}

func (p *PiProvider) sendRPC(cmd map[string]interface{}) (json.RawMessage, error) {
	id := uuid.New().String()
	cmd["id"] = id

	ch := make(chan json.RawMessage, 1)
	p.rpcMu.Lock()
	p.rpcPending[id] = ch
	p.rpcMu.Unlock()

	cleanup := func() {
		p.rpcMu.Lock()
		delete(p.rpcPending, id)
		p.rpcMu.Unlock()
	}

	data, err := json.Marshal(cmd)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("marshal rpc: %w", err)
	}
	data = append(data, '\n')

	p.mu.Lock()
	if !p.alive.Load() || p.stdin == nil {
		p.mu.Unlock()
		cleanup()
		return nil, fmt.Errorf("pi process not running")
	}
	_, err = p.stdin.Write(data)
	p.mu.Unlock()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("write rpc: %w", err)
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("rpc aborted: %s (process killed)", cmd["type"])
		}
		return resp, nil
	case <-time.After(piRPCTimeout):
		cleanup()
		return nil, fmt.Errorf("rpc timeout: %s", cmd["type"])
	}
}

func (p *PiProvider) SendMessage(text string, files []sessionstypes.FileAttachment) error {
	if !p.alive.Load() || p.stdin == nil {
		return fmt.Errorf("pi process not running")
	}

	p.touchActivity()

	cmd := map[string]interface{}{
		"id":      uuid.New().String(),
		"type":    "prompt",
		"message": text,
	}
	if len(files) > 0 {
		images := make([]map[string]interface{}, 0, len(files))
		for _, f := range files {
			images = append(images, map[string]interface{}{
				"type":     "image",
				"data":     f.Data,
				"mimeType": f.MimeType,
			})
		}
		cmd["images"] = images
	}

	data, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal prompt: %w", err)
	}
	data = append(data, '\n')

	p.msgStartNano.Store(time.Now().UnixNano())
	p.firstTokenNano.Store(0)

	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := p.stdin.Write(data); err != nil {
		return fmt.Errorf("write to stdin: %w", err)
	}
	return nil
}

func (p *PiProvider) StopGeneration() {
	if !p.alive.Load() || p.stdin == nil {
		return
	}
	cmd := map[string]interface{}{"type": "abort"}
	data, _ := json.Marshal(cmd)
	data = append(data, '\n')

	p.mu.Lock()
	_, err := p.stdin.Write(data)
	p.mu.Unlock()
	if err != nil {
		slog.Warn("pi abort write failed, killing", "session", p.session.ID, "error", err)
		p.Kill()
	}
}

func (p *PiProvider) Kill() {
	p.closeModelProxyAndOverlay()
	p.cleanupSpawnFiles()

	if p.cmd == nil || p.cmd.Process == nil {
		return
	}

	p.alive.Store(false)

	if p.stopIdle != nil {
		p.stopIdleOnce.Do(func() { close(p.stopIdle) })
	}

	p.rpcMu.Lock()
	for id, ch := range p.rpcPending {
		close(ch)
		delete(p.rpcPending, id)
	}
	p.rpcMu.Unlock()

	if p.stdin != nil {
		_ = p.stdin.Close()
	}

	_ = p.cmd.Process.Signal(os.Interrupt)

	select {
	case <-p.waitDone:
	case <-time.After(3 * time.Second):
		// Target group first, then the shim, so the shim's own Wait can
		// return and emit its final exit status. p.targetPID stays 0 on a
		// direct spawn, so that behavior here is unchanged.
		if p.targetPID > 0 {
			_ = syscall.Kill(-p.targetPID, syscall.SIGKILL)
		}
		_ = p.cmd.Process.Kill()
		<-p.waitDone
	}

	slog.Info("pi process killed", "session", p.session.ID)
}

func (p *PiProvider) Alive() bool {
	return p.alive.Load()
}

// SetThinkingLevel changes pi's thinking/reasoning level via the
// set_thinking_level RPC. Valid levels: off, minimal, low, medium, high,
// xhigh (xhigh is OpenAI-codex-max only).
func (p *PiProvider) SetThinkingLevel(level string) error {
	switch level {
	case "off", "minimal", "low", "medium", "high", "xhigh":
	default:
		return fmt.Errorf("invalid thinking level: %q", level)
	}

	resp, err := p.sendRPC(map[string]interface{}{
		"type":  "set_thinking_level",
		"level": level,
	})
	if err != nil {
		return err
	}
	var r struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(resp, &r); err != nil {
		return fmt.Errorf("parse set_thinking_level response: %w", err)
	}
	if !r.Success {
		return fmt.Errorf("set_thinking_level rejected: %s", r.Error)
	}

	p.mu.Lock()
	p.thinkingLevel = level
	p.mu.Unlock()

	p.session.Lock()
	p.session.ThinkingLevel = level
	p.session.Unlock()
	return nil
}

func (p *PiProvider) DeleteSession() error {
	p.mu.Lock()
	sid := p.piSessionID
	p.mu.Unlock()
	if sid == "" {
		return nil
	}

	patterns := []string{
		filepath.Join(p.sessionDir(), "*_"+sid+".jsonl"),
		filepath.Join(p.sessionDir(), "*", "*_"+sid+".jsonl"),
	}
	var matches []string
	for _, pat := range patterns {
		m, err := filepath.Glob(pat)
		if err != nil {
			return fmt.Errorf("glob pi session: %w", err)
		}
		matches = append(matches, m...)
	}
	if len(matches) == 0 {
		slog.Warn("pi DeleteSession: no matching file found", "piSessionId", sid, "dir", p.sessionDir())
		return nil
	}
	for _, path := range matches {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove pi session file: %w", err)
		}
		slog.Info("deleted pi session file", "path", path)
	}
	return nil
}

func (p *PiProvider) GetState() json.RawMessage {
	p.mu.Lock()
	sid := p.piSessionID
	p.mu.Unlock()
	data, _ := json.Marshal(map[string]interface{}{"piSessionId": sid})
	return data
}

func (p *PiProvider) RestoreState(state json.RawMessage) {
	if state == nil {
		return
	}
	var s struct {
		PiSessionID string `json:"piSessionId"`
	}
	if err := json.Unmarshal(state, &s); err == nil {
		p.mu.Lock()
		p.piSessionID = s.PiSessionID
		p.mu.Unlock()
	}
}
