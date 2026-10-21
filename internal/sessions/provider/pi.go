package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/adopt"
	"github.com/google/uuid"

	"github.com/barelyworkingcode/relay/internal/sessions/events"
	sessionsmcp "github.com/barelyworkingcode/relay/internal/sessions/mcp"
	"github.com/barelyworkingcode/relay/internal/sessions/pioverlay"
	"github.com/barelyworkingcode/relay/internal/sessions/runner"
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

	// Launcher runs in place of the pi binary, with the kind's own arguments
	// after its args. nil runs Binary.
	Launcher *sessionstypes.Launcher

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

	// RunnerBase is the directory local sessions' run directories live under.
	// Every pi session runs under the runner there; Start refuses a session when
	// it is empty. A pi runner ends when relay-sessions shuts down.
	RunnerBase string
}

// piRunnerPatterns are the strings the runner matches in pi's stdout. The
// agent_settled line ends a turn: pi writes it after agent_end, and an agent_end
// with willRetry set is followed by a retry turn. Pi has no question path, so
// Asking stays empty and never matches.
var piRunnerPatterns = runner.Patterns{TurnEnded: `"type":"agent_settled"`}

// PiProvider manages a persistent pi CLI process in `--mode rpc`. The wire
// format is JSONL: commands written to stdin one per line, responses +
// events read from stdout one per line. Translates pi's event vocabulary
// into the same canonical event stream ClaudeProvider produces.
type PiProvider struct {
	session *sessionstypes.Session
	handler sessionstypes.EventHandler
	cfg     PiConfig

	mu    sync.Mutex
	alive atomic.Bool

	// det follows the agent under the runner; nil before Start.
	det atomic.Pointer[runFollower]

	piSessionID   string
	piSessionFile string
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
	pendingError    string

	warnedMu    sync.Mutex
	warnedTypes map[string]struct{}

	lastActivity atomic.Int64
	stopIdle     chan struct{}
	stopIdleOnce sync.Once
	waitDone     chan struct{}
	exit         *spawnExit
	drainTimeout time.Duration
	// killed is per spawn: a restarted spawn gets a fresh one, so an old
	// spawn's late exit can't read the new spawn's flag.
	killed *atomic.Bool
	// exitMu guards spawnGen, which counts Starts. A spawn's waitForExit
	// holds it from the generation check through the process_exited
	// handler call, so a killed spawn whose drain outlasts a restart stays
	// silent, and Start cannot go live between the check and the emit.
	// Deliberate: the handler runs under the lock, so it must never call
	// Start on this provider.
	exitMu   sync.Mutex
	spawnGen uint64

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

func (p *PiProvider) idleWatcher(stopIdle <-chan struct{}) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-stopIdle:
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
	if p.piSessionFile != "" {
		return filepath.Dir(p.piSessionFile)
	}
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
		args = append(args, "--provider", pioverlay.RelayProvider, "--model", piBrokerModelID(p.modelID))
	}
	if p.thinkingLevel != "" {
		args = append(args, "--thinking", p.thinkingLevel)
	}
	if p.piSessionID != "" {
		file := p.piSessionID
		if p.piSessionFile != "" {
			file = p.piSessionFile
		}
		args = append(args, "--session", file)
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

// piBrokerModelID maps a session model to the id relay's model broker
// dispatches on. Session creation spells a pi model "pi/<provider>/<id>" so
// the session kind can be derived from the string; the provider segment names
// an entry in pi's own global config, which the relay overlay replaces, so
// only <id> means anything to the broker. Any other spelling passes through.
func piBrokerModelID(model string) string {
	rest, ok := strings.CutPrefix(model, "pi/")
	if !ok {
		return model
	}
	provider, id, ok := strings.Cut(rest, "/")
	if !ok || provider == "" || id == "" {
		return model
	}
	return id
}

func (p *PiProvider) Start() (err error) {
	p.cleanupSpawnFiles()
	p.closeModelProxyAndOverlay()
	p.det.Store(nil)

	defer func() {
		if err != nil {
			p.closeModelProxyAndOverlay()
		}
	}()

	base := p.cfg.RunnerBase
	if base == "" {
		return errors.New("pi: no runner directory is configured for local sessions")
	}
	if p.piSessionFile != "" {
		home, e := os.UserHomeDir()
		if e != nil {
			return e
		}
		if e := adopt.ValidatePiTranscript(home, p.directory, p.piSessionID, p.piSessionFile); e != nil {
			return fmt.Errorf("pi: invalid adopted transcript: %w", e)
		}
	}
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

	program := p.cfg.Launcher.Program(resolvePiPath(p.cfg.Binary))

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
				ModelID:        piBrokerModelID(p.modelID),
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

	identitySecret := ""
	if p.cfg.Identity != nil {
		identitySecret = p.cfg.Identity.Secret
	}
	run, st, err := startLocalRun("pi", base, p.directory, shimSpec{
		Binary:         p.cfg.ShimBinary,
		SessionID:      p.session.ID,
		BridgeSocket:   p.cfg.BridgeSocket,
		SandboxProfile: p.cfg.SandboxProfile,
		Identity:       p.cfg.Identity,
	}, program, args, env, piRunnerPatterns, p.cfg.ModelKey, identitySecret)
	if err != nil {
		return err
	}

	det := p.beginRun(localConnFor(base, p.session.ID), run.Generation)
	det.Run()
	go p.idleWatcher(p.stopIdle)

	slog.Info("pi process started", "session", p.session.ID, "model", p.modelID, "runner", st.PID, "generation", run.Generation)

	if p.piSessionFile != "" {
		if err := p.fetchInitialState(); err != nil {
			p.Kill()
			return err
		}
	} else {
		go func() {
			if err := p.fetchInitialState(); err != nil {
				slog.Warn("pi get_state failed", "session", p.session.ID, "error", err)
			}
		}()
	}

	return nil
}

// beginRun installs the state of a spawn that follows a run from its start and
// marks the provider alive. Pi never attaches: a pi agent ends with the
// relay-sessions that started it, so every spawn starts a run and reads it from
// byte zero.
func (p *PiProvider) beginRun(connFor func(gen string) runConn, gen string) *runFollower {
	p.exitMu.Lock()
	p.spawnGen++
	spawnGen := p.spawnGen
	p.exitMu.Unlock()
	p.alive.Store(true)
	p.stopIdle = make(chan struct{})
	p.stopIdleOnce = sync.Once{}
	waitDone := make(chan struct{})
	p.waitDone = waitDone
	p.drainTimeout = providerDrainTimeout
	killed := &atomic.Bool{}
	p.killed = killed
	exit := &spawnExit{done: make(chan struct{})}
	p.exit = exit
	p.touchActivity()

	det := newRunFollower(nil, connFor, gen, 0, runHooks{
		Kind:    "pi",
		Session: p.session.ID,
		Line:    func(body json.RawMessage, _ int64) { p.processLine(body) },
		Exited: func(code int, tail []string, _ bool) {
			p.runExited(spawnGen, killed, exit, waitDone, code, tail)
		},
		Redact: p.redactTail,
	})
	p.det.Store(det)
	return det
}

// redactTail masks the launch's secrets in each tail line.
func (p *PiProvider) redactTail(lines []string) []string {
	secrets := []string{p.cfg.ModelKey}
	if p.cfg.Identity != nil {
		secrets = append(secrets, p.cfg.Identity.Secret)
	}
	for i, l := range lines {
		lines[i] = redactStderrLine(l, secrets)
	}
	return lines
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

func (p *PiProvider) fetchInitialState() error {
	resp, err := p.sendRPC(map[string]interface{}{"type": "get_state"})
	if err != nil {
		return fmt.Errorf("pi get_state: %w", err)
	}
	var r struct {
		Success bool `json:"success"`
		Data    struct {
			SessionID   string `json:"sessionId"`
			SessionFile string `json:"sessionFile"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp, &r); err != nil || !r.Success || r.Data.SessionID == "" {
		return errors.New("pi: invalid initial state")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.piSessionFile != "" && (r.Data.SessionID != p.piSessionID || r.Data.SessionFile != p.piSessionFile) {
		return errors.New("pi: resumed conversation differs from the adopted transcript")
	}
	p.piSessionID = r.Data.SessionID
	return nil
}

// runExited delivers process_exited once the runner is gone or Stop ran. Its
// spawn's own channels arrive as arguments: after Kill then Start, a goroutine
// re-reading the fields would race the next Start.
func (p *PiProvider) runExited(gen uint64, killed *atomic.Bool, exit *spawnExit, waitDone chan struct{}, code int, tail []string) {
	defer close(exit.done)
	p.alive.Store(false)
	exit.code = code
	close(waitDone)

	p.exitMu.Lock()
	defer p.exitMu.Unlock()
	if p.spawnGen != gen {
		slog.Debug("provider exit from a superseded spawn dropped", "session", p.session.ID, "kind", "pi", "exitCode", code)
		return
	}
	slog.Info("pi process exited", "session", p.session.ID, "exitCode", code)
	wasKilled := killed.Load()
	if !wasKilled {
		warnProviderExit(p.session.ID, "pi", code, tail)
	}

	data := providerExitData(code, tail, wasKilled)
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
	p.pendingError = ""
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

	case "message_end":
		p.holdFailedAssistantMessage(raw)

	case "message_start", "turn_start", "turn_end", "agent_settled":
		// Bookkeeping — already covered by the message_update and agent_end translations.

	case "auto_retry_start", "auto_retry_end":
		p.emitRetryNotice(eventType, raw)

	default:
		p.warnUnrecognisedOnce(eventType)
		p.handler("raw_output", raw)
	}
}

func (p *PiProvider) warnUnrecognisedOnce(eventType string) {
	p.warnedMu.Lock()
	_, seen := p.warnedTypes[eventType]
	if !seen {
		if p.warnedTypes == nil {
			p.warnedTypes = make(map[string]struct{})
		}
		p.warnedTypes[eventType] = struct{}{}
	}
	p.warnedMu.Unlock()
	if !seen {
		slog.Warn("pi: unrecognised event type", "session", p.session.ID, "type", eventType)
	}
}

// holdFailedAssistantMessage records a failed model request. pi reports one
// (an HTTP error from the model endpoint, an unknown model) only as an
// assistant message with stopReason "error" and no content; without this the
// turn ends as an empty reply. The failure is held until agent_end rather
// than emitted here, because pi fails the message the same way before an
// automatic retry that may still succeed.
func (p *PiProvider) holdFailedAssistantMessage(raw json.RawMessage) {
	var ev struct {
		Message struct {
			Role         string `json:"role"`
			StopReason   string `json:"stopReason"`
			ErrorMessage string `json:"errorMessage"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &ev) != nil || ev.Message.Role != "assistant" || ev.Message.StopReason != "error" {
		return
	}
	msg := ev.Message.ErrorMessage
	if msg == "" {
		msg = "model request failed"
	}
	p.streamMu.Lock()
	p.pendingError = msg
	p.streamMu.Unlock()
}

// reportHeldFailure emits the failure held by holdFailedAssistantMessage,
// unless pi is about to retry the run.
func (p *PiProvider) reportHeldFailure(willRetry bool) {
	p.streamMu.Lock()
	msg := p.pendingError
	p.pendingError = ""
	p.streamMu.Unlock()
	if msg == "" {
		return
	}
	p.mu.Lock()
	modelID := p.modelID
	p.mu.Unlock()
	if willRetry {
		slog.Info("pi model request failed, retrying", "session", p.session.ID, "model", modelID, "error", msg)
		return
	}
	slog.Warn("pi model request failed", "session", p.session.ID, "model", modelID, "error", msg)
	data, _ := json.Marshal(map[string]string{"error": msg})
	p.handler("error", data)
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
		// message_end carries the same failure with pi's full errorMessage;
		// agent_end reports it, once, unless pi retries.
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
	p.emitter.ToolResult(ev.ToolCallID, ev.ToolName, sessionstypes.FlattenTextBlocks(ev.Result.Content), ev.IsError, false)
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

	var end struct {
		WillRetry bool `json:"willRetry"`
	}
	_ = json.Unmarshal(raw, &end)
	p.reportHeldFailure(end.WillRetry)

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

	det := p.det.Load()
	if !p.alive.Load() || det == nil {
		cleanup()
		return nil, fmt.Errorf("pi process not running")
	}
	// Every RPC relay makes besides a prompt is a control message: get_state and
	// set_thinking_level start no turn.
	if err := det.Send(context.Background(), runner.MessageControl, data, 0); err != nil {
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
	det := p.det.Load()
	if !p.alive.Load() || det == nil {
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

	p.msgStartNano.Store(time.Now().UnixNano())
	p.firstTokenNano.Store(0)

	// Returns once the runner has written the prompt to pi.
	if err := det.Send(context.Background(), runner.MessageTurn, data, 0); err != nil {
		return fmt.Errorf("send to pi: %w", err)
	}
	return nil
}

func (p *PiProvider) StopGeneration() {
	det := p.det.Load()
	if !p.alive.Load() || det == nil {
		return
	}
	data, _ := json.Marshal(map[string]interface{}{"type": "abort"})
	if err := det.Send(context.Background(), runner.MessageControl, data, 0); err != nil {
		slog.Warn("pi abort not sent, killing", "session", p.session.ID, "error", err)
		p.Kill()
	}
}

// Kill ends the agent and its runner. Pi has no Release: a pi agent's model
// key is bound to the launch that ends with relay-sessions, so a shutdown ends
// the agent too.
func (p *PiProvider) Kill() {
	p.closeModelProxyAndOverlay()
	p.cleanupSpawnFiles()

	det := p.det.Load()
	if det == nil {
		return
	}

	exit := p.exit
	if p.killed != nil {
		p.killed.Store(true)
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

	det.Stop(true)
	// The exit is delivered or dropped only once the handler finishes. The
	// handler must not call Kill.
	if exit != nil {
		<-exit.done
	}

	slog.Info("pi process killed", "session", p.session.ID)
}

// Events returns up to max complete events the agent wrote after the cursor
// and the cursor to pass next. The cursor is "<generation>:<byte offset of a
// line start>"; "" starts at the current generation's beginning.
func (p *PiProvider) Events(ctx context.Context, after string, max int) ([]json.RawMessage, string, error) {
	det := p.det.Load()
	if det == nil {
		return nil, "", ErrNotDetached
	}
	return det.Events(ctx, after, max)
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
	if p.cfg.RunnerBase != "" {
		p.Kill()
		DeleteLocalAgent("pi", p.cfg.RunnerBase, p.session.ID)
	}
	p.mu.Lock()
	sid := p.piSessionID
	file := p.piSessionFile
	p.mu.Unlock()
	if sid == "" {
		return nil
	}

	if file != "" {
		home, e := os.UserHomeDir()
		if e != nil {
			return e
		}
		if e := adopt.ValidatePiTranscript(home, p.directory, sid, file); e != nil {
			if os.IsNotExist(e) {
				return nil
			}
			return e
		}
		return os.Remove(file)
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
	file := p.piSessionFile
	p.mu.Unlock()
	data, _ := json.Marshal(map[string]interface{}{"piSessionId": sid, "piSessionFile": file})
	return data
}

func (p *PiProvider) RestoreState(state json.RawMessage) {
	if state == nil {
		return
	}
	var s struct {
		PiSessionID   string `json:"piSessionId"`
		PiSessionFile string `json:"piSessionFile"`
	}
	if err := json.Unmarshal(state, &s); err == nil {
		p.mu.Lock()
		p.piSessionID = s.PiSessionID
		p.piSessionFile = s.PiSessionFile
		p.mu.Unlock()
	}
}

// ResolvePiNodeBinary pins the interpreter of the operator-selected Pi launch
// chain. Candidate argv and candidate package metadata never choose this pin.
func ResolvePiNodeBinary(configured string) (string, error) {
	path := resolvePiPath(configured)
	if !filepath.IsAbs(path) {
		return "", errors.New("pi: launcher is not absolute")
	}
	read := func(path string) ([]byte, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return nil, errors.New("pi: launcher metadata is not regular")
		}
		data, err := io.ReadAll(io.LimitReader(f, 64<<10+1))
		if err != nil {
			return nil, err
		}
		if len(data) > 64<<10 {
			return nil, errors.New("pi: launcher metadata exceeds 64 KiB")
		}
		return data, nil
	}
	packageBin := func(entry string) bool {
		dir := filepath.Dir(entry)
		for depth := 0; depth < 5; depth++ {
			data, err := read(filepath.Join(dir, "package.json"))
			if err == nil {
				var pkg struct {
					Name string            `json:"name"`
					Bin  map[string]string `json:"bin"`
				}
				if json.Unmarshal(data, &pkg) == nil && (pkg.Name == "@earendil-works/pi-coding-agent" || pkg.Name == "@mariozechner/pi-coding-agent") {
					bin := pkg.Bin["pi"]
					if bin == "" || filepath.IsAbs(bin) {
						return false
					}
					resolved, e := filepath.EvalSymlinks(filepath.Join(dir, bin))
					return e == nil && resolved == entry
				}
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
		return false
	}
	wrapper := regexp.MustCompile(`(?s)^#![ \t]*/bin/(?:bash|sh)[ \t]*\r?\n[ \t\r\n]*PI_SKIP_VERSION_CHECK="1"[ \t]+exec[ \t]+"(/[^"$\x60\\\r\n]+)"[ \t]+"\$@"[ \t\r\n]*$`)
	seen := map[string]bool{}
	for stage := 0; stage < 4; stage++ {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", err
		}
		if seen[resolved] {
			return "", errors.New("pi: launcher loop")
		}
		seen[resolved] = true
		data, err := read(resolved)
		if err != nil {
			return "", err
		}
		if !packageBin(resolved) {
			match := wrapper.FindSubmatch(data)
			if match == nil {
				return "", errors.New("pi: unsupported launcher")
			}
			path = string(match[1])
			continue
		}
		line := strings.SplitN(string(data), "\n", 2)[0]
		line = strings.TrimSuffix(line, "\r")
		if !strings.HasPrefix(line, "#!") {
			return "", errors.New("pi: package bin has no interpreter")
		}
		interpreter := strings.Fields(strings.TrimPrefix(line, "#!"))
		node := ""
		if len(interpreter) == 2 && interpreter[0] == "/usr/bin/env" && interpreter[1] == "node" {
			node, err = exec.LookPath("node")
			if err != nil {
				return "", err
			}
		} else if len(interpreter) == 1 && filepath.IsAbs(interpreter[0]) && filepath.Base(interpreter[0]) == "node" {
			node = interpreter[0]
		} else {
			return "", errors.New("pi: unsupported Node interpreter")
		}
		if !filepath.IsAbs(node) {
			return "", errors.New("pi: Node interpreter is not absolute")
		}
		node, err = filepath.EvalSymlinks(node)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(node)
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return "", errors.New("pi: Node interpreter is not executable")
		}
		return node, nil
	}
	return "", errors.New("pi: launcher chain exceeds four stages")
}
