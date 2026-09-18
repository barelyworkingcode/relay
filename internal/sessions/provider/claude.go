package provider

import (
	"bufio"
	"bytes"
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

	"github.com/barelyworkingcode/relay/internal/sessions/events"
	"github.com/barelyworkingcode/relay/internal/sessions/hook"
	sessionsmcp "github.com/barelyworkingcode/relay/internal/sessions/mcp"
	"github.com/barelyworkingcode/relay/internal/sessions/permission"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
	"github.com/barelyworkingcode/relay/internal/sshhost"
)

var emptyJSONObject = []byte("{}")

const claudeIdleTimeout = 15 * time.Minute

// ClaudeConfig carries everything Start needs to build one Claude CLI
// child's argv and env, resolved by the caller (the relay-sessions host
// process) rather than read back out of this process's own environment —
// see C5's env table and this package's doc comment.
type ClaudeConfig struct {
	Binary string // resolved claude CLI path; "" falls back to well-known locations then PATH

	// HookSocket is C6's RELAY_SESSIONS_HOOK_SOCKET value for this session.
	// Empty for a host (ssh) session: there is no local hook binary to dial
	// on the far end, so Claude gets --permission-prompt-tool stdio instead
	// (see buildClaudeArgs).
	HookSocket string
	// HookCommandPath is the absolute path to the relay-sessions binary
	// (this process's own os.Executable(), per C6: "the hook command is
	// <abs>/relay-sessions hook"). Required whenever HookSocket is set.
	HookCommandPath string

	BridgeSocket string // C5 env table: RELAY_BRIDGE_SOCKET, unqualified — every kind gets it
	ModelSocket  string // C5 env table: RELAY_MODEL_SOCKET, unqualified

	// RelayMCPCommand is the absolute path to the command Claude's
	// --mcp-config spawns for relay's own tools (email, calendar, ...), run
	// as Claude's own child and therefore a C3 member of this session's
	// root — no bearer travels in its config or env. Empty disables the
	// relay MCP server entirely regardless of the session's own
	// useRelayTools setting; see this package's doc comment on the
	// judgment call this represents.
	RelayMCPCommand string

	// ShimBinary is the absolute path to relay-sessions' own binary, run in
	// "exec" mode to wrap the CLI child. Empty means a direct spawn, which is
	// refused outright when SandboxProfile or Identity is set (Start).
	ShimBinary string

	// SandboxProfile is this launch's own absolute SBPL profile path (C7);
	// "" runs the child unsandboxed. Never cached across calls, same rule as
	// Identity below.
	SandboxProfile string

	// Identity is this launch's own project_session secret, presented by the
	// shim to relay's bridge socket — never by the CLI child itself, and never
	// in argv. nil for a launch with no identity to mint (SSH-hosted).
	Identity *sessionsmcp.IdentitySpec
}

// ClaudeProvider manages a persistent Claude CLI process and translates its
// stream-json output into canonical relay events.
//
// Persistence: Claude CLI owns the JSONL history file under
// ~/.claude/projects/<dir>/<sid>.jsonl, replayed on session join by
// ReadClaudeHistory. This provider does not accumulate into session.Messages.
type ClaudeProvider struct {
	session *sessionstypes.Session
	handler sessionstypes.EventHandler
	emitter *events.EventEmitter
	cfg     ClaudeConfig

	cmd   *exec.Cmd
	stdin io.WriteCloser
	mu    sync.Mutex // serializes writes to stdin
	alive atomic.Bool

	// targetPID is the shim's own child (the real claude process) when this
	// launch went through the shim — 0 on a direct spawn or an SSH-hosted
	// session. Used by Kill's hard-kill fallback to reach the target's whole
	// process group, not just the shim.
	targetPID int

	claudeSessionID string
	model           string
	directory       string

	// spawnFiles holds every 0600 temp file (mcp-config, system prompt)
	// written for the current/most recent spawn, removed on Kill — neither
	// carries a bearer today, but both can carry project-specific text no
	// process outside this session needs to keep reading after it ends.
	spawnFiles []string

	// perms services a host session's control_request permission prompts —
	// a host has no local hook binary to dial, so Claude's own
	// --permission-prompt-tool stdio moves the same question onto its
	// stdout instead (see handleControlRequest). nil-safe: a provider built
	// without one (non-host sessions, most tests) simply can't
	// register/deny requests.
	perms *permission.PermissionManager

	lastActivity atomic.Int64
	stopIdle     chan struct{}
	stopIdleOnce sync.Once
	waitDone     chan struct{}

	msgStartNano   atomic.Int64
	firstTokenNano atomic.Int64

	// Per-turn snapshot state — see translateAssistantSnapshot.
	snapMu        sync.Mutex
	snapMessageID string
	snapNextIdx   int
}

// NewClaudeProvider constructs a provider for session. perms may be nil for
// a session with no host-control_request path (see ClaudeProvider.perms).
func NewClaudeProvider(session *sessionstypes.Session, handler sessionstypes.EventHandler, cfg ClaudeConfig, perms *permission.PermissionManager) *ClaudeProvider {
	return &ClaudeProvider{
		session:   session,
		handler:   handler,
		emitter:   events.NewEventEmitter(handler),
		cfg:       cfg,
		model:     session.Model,
		directory: session.Directory,
		perms:     perms,
	}
}

// useRelayTools reports the session's own useRelayTools setting, parsed from
// session.Settings. Malformed or absent settings default to false.
func useRelayTools(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var s struct {
		UseRelayTools *bool `json:"useRelayTools"`
	}
	if json.Unmarshal(raw, &s) != nil || s.UseRelayTools == nil {
		return false
	}
	return *s.UseRelayTools
}

// relayMCPConfig renders the relay MCP server entry Claude's --mcp-config
// expects. Returns nil when the session hasn't opted in or no relay MCP
// command is configured. The spawned command is Claude's own child, hence a
// C3 member of this session's root — relay's bridge authenticates it by
// ancestry, so this config carries no bearer of any kind, unlike relayLLM's
// RELAY_PROJECT_TOKEN-bearing equivalent.
func (p *ClaudeProvider) relayMCPConfig() map[string]any {
	if !useRelayTools(p.session.Settings) || p.cfg.RelayMCPCommand == "" {
		return nil
	}
	return map[string]any{
		"mcpServers": map[string]any{
			"relay": map[string]any{
				"command": p.cfg.RelayMCPCommand,
				"args":    []string{"mcp"},
			},
		},
	}
}

func (p *ClaudeProvider) touchActivity() {
	p.lastActivity.Store(time.Now().Unix())
}

func (p *ClaudeProvider) idleWatcher() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopIdle:
			return
		case <-ticker.C:
			idle := time.Now().Unix() - p.lastActivity.Load()
			if idle > int64(claudeIdleTimeout.Seconds()) {
				slog.Info("claude process idle, killing", "session", p.session.ID, "idleSecs", idle)
				p.Kill()
				return
			}
		}
	}
}

// effectivePermissionMode resolves the Claude CLI --permission-mode for this
// session. A headless session forces "bypassPermissions".
func (p *ClaudeProvider) effectivePermissionMode() string {
	if p.session.Headless {
		return "bypassPermissions"
	}
	return p.session.PermissionMode
}

// buildClaudeArgs assembles the claude CLI argv. mcpConfigPath and
// sysPromptPath are file paths ("" to omit) rather than inline values: a
// system prompt or MCP config living in argv is readable by any same-uid
// process via KERN_PROCARGS2, the same exposure this whole package exists to
// close for credentials — routing both through a 0600 file removes them from
// argv entirely, not just from the ones that happen to be secret today.
func (p *ClaudeProvider) buildClaudeArgs(mcpConfigPath, sysPromptPath string) []string {
	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--model", p.model,
	}

	if p.claudeSessionID != "" {
		args = append(args, "--resume", p.claudeSessionID)
	}

	if sysPromptPath != "" {
		args = append(args, "--append-system-prompt-file", sysPromptPath)
	}

	mode := p.effectivePermissionMode()
	if mode != "" && mode != "default" {
		args = append(args, "--permission-mode", mode)
	}
	if mode == "bypassPermissions" {
		args = append(args, "--dangerously-skip-permissions")
	}

	if p.session.Policy != nil {
		if len(p.session.Policy.AllowedTools) > 0 {
			args = append(args, "--allowedTools", strings.Join(p.session.Policy.AllowedTools, ","))
		}
		if len(p.session.Policy.DeniedTools) > 0 {
			args = append(args, "--disallowedTools", strings.Join(p.session.Policy.DeniedTools, ","))
		}
	}

	// A host session has no PreToolUse hook binary or bridge socket to dial,
	// so permissions ride the stream instead: --permission-prompt-tool stdio
	// turns each tool call into a control_request on Claude's own stdout.
	// --mcp-config never applies on a host (v1 carries no relay MCPs there).
	if p.session.Host != nil {
		args = append(args, "--permission-prompt-tool", "stdio")
	} else if mcpConfigPath != "" {
		args = append(args, "--mcp-config", mcpConfigPath)
	}

	return args
}

// buildClaudeEnv assembles the child environment for a console (non-host)
// spawn. base is childBaseEnv()+ensurePath — never a stale relay credential.
// No RELAY_*TOKEN of any kind is ever added: a console session's tools reach
// relay by C3 ancestry (the hook, the --mcp-config child), not a bearer this
// process would otherwise have to mint, store, and eventually revoke.
func (p *ClaudeProvider) buildClaudeEnv(base []string) []string {
	add := map[string]string{
		"RELAY_SESSION_ID": p.session.ID,
	}
	if p.cfg.HookSocket != "" {
		add[hook.EnvHookSocket] = p.cfg.HookSocket
	}
	if p.cfg.BridgeSocket != "" {
		add["RELAY_BRIDGE_SOCKET"] = p.cfg.BridgeSocket
	}
	if p.cfg.ModelSocket != "" {
		add["RELAY_MODEL_SOCKET"] = p.cfg.ModelSocket
	}
	return mergeEnv(base, add)
}

// buildHostExec assembles argv to run Claude on a host: relay's ssh_argv
// prefix, `-T` (no local tty — headless chat, not a terminal), `--`, and a
// RemoteCommand wrapping `claude_path <args>` under dir. env carries only
// RELAY_SESSION_ID (decision 6: never the hook socket or any relay token on
// a host — there is nothing on the far end that could use either safely).
func buildHostExec(spec *sessionstypes.HostSpec, dir string, args []string, sessionID string) (name string, argv []string, err error) {
	if len(spec.SSHArgv) == 0 {
		return "", nil, fmt.Errorf("host %q has no ssh_argv", spec.Name)
	}
	env := map[string]string{"RELAY_SESSION_ID": sessionID}
	remote := sshhost.RemoteCommand(dir, append([]string{spec.ClaudePath}, args...), env)
	name = spec.SSHArgv[0]
	argv = append(append([]string{}, spec.SSHArgv[1:]...), "-T", "--", remote)
	return name, argv, nil
}

func (p *ClaudeProvider) Start() error {
	p.cleanupSpawnFiles()

	var cmd *exec.Cmd
	var statusR *os.File
	var extraFiles []*os.File

	if host := p.session.GetHost(); host != nil {
		if p.cfg.SandboxProfile != "" || p.cfg.Identity != nil {
			// relay's own launch authorization never mints a sandbox profile
			// or launch identity for a hosted project (needsIdentity), so
			// this should be unreachable in practice. Warn and ignore rather
			// than refuse: refusing here would break every SSH-hosted
			// session outright if that guarantee ever drifted, which is
			// strictly worse than silently ignoring two fields that ought
			// to already be empty.
			slog.Warn("claude: sandbox/identity requested for a host session; ignoring", "session", p.session.ID)
		}
		if host.ClaudePath == "" {
			return fmt.Errorf("host %q has no claude: run a probe", host.Name)
		}
		args := p.buildClaudeArgs("", "")
		if p.session.SystemPrompt != "" {
			// A host session's system prompt still needs to reach Claude, but
			// there is no local file the remote process could read — fall
			// back to the argv flag for this branch only (host argv is not
			// visible via KERN_PROCARGS2 on this machine at all; it never
			// executes locally).
			args = append(args, "--append-system-prompt", p.session.SystemPrompt)
		}
		name, argv, err := buildHostExec(host, p.directory, args, p.session.ID)
		if err != nil {
			return err
		}
		cmd = exec.Command(name, argv...)
		cmd.Env = childBaseEnv()
	} else {
		if err := p.ensureHookConfig(); err != nil {
			slog.Warn("claude: hook config write failed", "session", p.session.ID, "directory", p.directory, "error", err)
		}

		var mcpConfigPath, sysPromptPath string
		if cfg := p.relayMCPConfig(); cfg != nil {
			data, err := json.Marshal(cfg)
			if err != nil {
				return fmt.Errorf("marshal mcp config: %w", err)
			}
			path, err := writeSpawnFile(os.TempDir(), "claude-mcp-*.json", data)
			if err != nil {
				return fmt.Errorf("write mcp config: %w", err)
			}
			mcpConfigPath = path
			p.spawnFiles = append(p.spawnFiles, path)
		}
		if p.session.SystemPrompt != "" {
			path, err := writeSpawnFile(os.TempDir(), "claude-sysprompt-*.txt", []byte(p.session.SystemPrompt))
			if err != nil {
				return fmt.Errorf("write system prompt: %w", err)
			}
			sysPromptPath = path
			p.spawnFiles = append(p.spawnFiles, path)
		}

		args := p.buildClaudeArgs(mcpConfigPath, sysPromptPath)
		claudePath := resolveClaudePath(p.cfg.Binary)

		spec := shimSpec{
			Binary:         p.cfg.ShimBinary,
			SessionID:      p.session.ID,
			BridgeSocket:   p.cfg.BridgeSocket,
			SandboxProfile: p.cfg.SandboxProfile,
			Identity:       p.cfg.Identity,
		}
		switch {
		case spec.wanted() && spec.Binary == "":
			return ErrShimRequired
		case spec.Identity != nil && spec.BridgeSocket == "":
			return ErrNoBridgeSocket
		case spec.wanted():
			var berr error
			cmd, statusR, extraFiles, berr = buildShimCmd(spec, claudePath, args)
			if berr != nil {
				return berr
			}
		default:
			cmd = exec.Command(claudePath, args...) // unchanged direct spawn
		}
		cmd.Dir = p.directory
		cmd.Env = p.buildClaudeEnv(ensurePath(childBaseEnv()))
	}

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
		return fmt.Errorf("failed to start claude: %w", err)
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

	slog.Info("claude process started", "session", p.session.ID, "model", p.model, "pid", cmd.Process.Pid, "targetPid", p.targetPID)
	return nil
}

func (p *ClaudeProvider) cleanupSpawnFiles() {
	for _, path := range p.spawnFiles {
		_ = os.Remove(path)
	}
	p.spawnFiles = nil
}

func (p *ClaudeProvider) readStdout(r io.ReadCloser) {
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
		slog.Error("claude stdout read error", "session", p.session.ID, "error", err)
	}
}

func (p *ClaudeProvider) readStderr(r io.ReadCloser) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		text := scanner.Text()
		if text != "" {
			slog.Debug("claude stderr", "session", p.session.ID, "text", text)
		}
	}
}

func (p *ClaudeProvider) waitForExit() {
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

	slog.Info("claude process exited", "session", p.session.ID, "exitCode", exitCode)

	data, _ := json.Marshal(map[string]interface{}{"exitCode": exitCode})
	p.handler("process_exited", data)
}

func (p *ClaudeProvider) processLine(raw json.RawMessage) {
	p.touchActivity()

	var envelope struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype,omitempty"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		p.handler("raw_output", raw)
		return
	}

	switch envelope.Type {
	case events.EvtSystem:
		p.translateSystem(envelope.Subtype, raw)
	case events.EvtAssistant:
		p.translateAssistant(raw)
	case "user":
		p.translateUser(raw)
	case events.EvtResult:
		p.translateResult(raw)
	case "control_request":
		p.handleControlRequest(raw)
	default:
		p.emitter.EmitVersionedRaw(raw)
	}
}

func claudeMCPServerNames(entries []json.RawMessage) []string {
	names := make([]string, 0, len(entries))
	for _, raw := range entries {
		if len(raw) == 0 {
			continue
		}
		if raw[0] == '"' {
			var s string
			if json.Unmarshal(raw, &s) == nil && s != "" {
				names = append(names, s)
			}
			continue
		}
		var obj struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &obj) == nil && obj.Name != "" {
			names = append(names, obj.Name)
		}
	}
	return names
}

func (p *ClaudeProvider) translateSystem(subtype string, raw json.RawMessage) {
	switch subtype {
	case events.SystemInitSubtype:
		var init struct {
			SessionID  string            `json:"session_id"`
			Model      string            `json:"model"`
			Cwd        string            `json:"cwd"`
			Tools      []string          `json:"tools"`
			MCPServers []json.RawMessage `json:"mcp_servers"`
		}
		if err := json.Unmarshal(raw, &init); err != nil {
			p.emitter.EmitVersionedRaw(raw)
			return
		}
		if init.SessionID != "" {
			p.mu.Lock()
			p.claudeSessionID = init.SessionID
			p.mu.Unlock()
		}
		model := init.Model
		if model == "" {
			model = p.model
		}
		cwd := init.Cwd
		if cwd == "" {
			cwd = p.directory
		}
		p.emitter.SystemInit(model, cwd, init.Tools, claudeMCPServerNames(init.MCPServers))

	case events.SystemPermissionRequestSubtype:
		var req struct {
			PermissionID string          `json:"permission_id"`
			ToolName     string          `json:"tool_name"`
			ToolUseID    string          `json:"tool_use_id"`
			ToolInput    json.RawMessage `json:"tool_input"`
		}
		if json.Unmarshal(raw, &req) != nil {
			p.emitter.EmitVersionedRaw(raw)
			return
		}
		p.emitter.PermissionRequest(req.PermissionID, req.ToolName, req.ToolUseID, req.ToolInput)

	case events.SystemQuestionSubtype:
		var q struct {
			Prompt   string          `json:"prompt"`
			Metadata json.RawMessage `json:"metadata"`
		}
		if json.Unmarshal(raw, &q) != nil {
			p.emitter.EmitVersionedRaw(raw)
			return
		}
		p.emitter.SystemQuestion(q.Prompt, q.Metadata)

	case events.SystemStatusSubtype:
		var s struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &s) != nil {
			p.emitter.EmitVersionedRaw(raw)
			return
		}
		p.emitter.SystemStatus(s.Message)

	case events.SystemAPIErrorSubtype:
		var s struct {
			Message  string `json:"message"`
			Retrying bool   `json:"retrying"`
		}
		if json.Unmarshal(raw, &s) != nil {
			p.emitter.EmitVersionedRaw(raw)
			return
		}
		p.emitter.SystemAPIError(s.Message, s.Retrying)

	case events.SystemBridgeStatusSubtype:
		var s struct {
			Status string `json:"status"`
			Detail string `json:"detail"`
		}
		if json.Unmarshal(raw, &s) != nil {
			p.emitter.EmitVersionedRaw(raw)
			return
		}
		p.emitter.SystemBridgeStatus(s.Status, s.Detail)

	case events.SystemStopHookSummarySubtype:
		var s struct {
			Summary string `json:"summary"`
			IsError bool   `json:"is_error"`
		}
		if json.Unmarshal(raw, &s) != nil {
			p.emitter.EmitVersionedRaw(raw)
			return
		}
		p.emitter.SystemStopHookSummary(s.Summary, s.IsError)

	default:
		p.emitter.EmitVersionedRaw(raw)
	}
}

func (p *ClaudeProvider) translateAssistant(raw json.RawMessage) {
	var ev struct {
		Index            *int             `json:"index,omitempty"`
		Message          *json.RawMessage `json:"message,omitempty"`
		ContentBlock     *json.RawMessage `json:"content_block,omitempty"`
		Delta            *json.RawMessage `json:"delta,omitempty"`
		ContentBlockStop *bool            `json:"content_block_stop,omitempty"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		p.emitter.EmitVersionedRaw(raw)
		return
	}

	isStop := ev.ContentBlockStop != nil && *ev.ContentBlockStop

	switch {
	case ev.Message != nil:
		p.translateAssistantSnapshot(*ev.Message)

	case ev.Delta != nil && ev.Index != nil:
		p.firstTokenNano.CompareAndSwap(0, time.Now().UnixNano())
		p.translateBlockDelta(*ev.Index, *ev.Delta)

	case isStop && ev.Index != nil:
		p.translateBlockStop(*ev.Index, ev.ContentBlock)

	case !isStop && ev.ContentBlock != nil && ev.Index != nil:
		p.translateBlockStart(*ev.Index, *ev.ContentBlock)

	default:
		p.emitter.EmitVersionedRaw(raw)
	}
}

type claudeSnapContentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	Thinking string          `json:"thinking,omitempty"`
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
}

func (p *ClaudeProvider) translateAssistantSnapshot(messageRaw json.RawMessage) {
	var m struct {
		ID      string                   `json:"id"`
		Content []claudeSnapContentBlock `json:"content"`
	}
	if err := json.Unmarshal(messageRaw, &m); err != nil {
		p.emitter.EmitVersionedRaw(messageRaw)
		return
	}

	p.snapMu.Lock()
	defer p.snapMu.Unlock()

	if m.ID != p.snapMessageID {
		p.snapMessageID = m.ID
		p.snapNextIdx = 0
		p.emitter.MessageStart(m.ID)
	}

	for _, block := range m.Content {
		idx := p.snapNextIdx
		p.snapNextIdx++
		switch block.Type {
		case events.BlockText:
			p.emitter.TextBlockStart(idx)
			if block.Text != "" {
				p.firstTokenNano.CompareAndSwap(0, time.Now().UnixNano())
				p.emitter.TextDelta(idx, block.Text)
			}
			p.emitter.BlockStop(idx)
		case events.BlockThinking:
			p.emitter.ThinkingBlockStart(idx)
			if block.Thinking != "" {
				p.firstTokenNano.CompareAndSwap(0, time.Now().UnixNano())
				p.emitter.ThinkingDelta(idx, block.Thinking)
			}
			p.emitter.BlockStop(idx)
		case events.BlockToolUse:
			p.emitter.ToolUseBlockStart(idx, block.ID, block.Name)
			input := block.Input
			if len(input) > 0 && !bytes.Equal(input, emptyJSONObject) {
				p.emitter.InputJsonDelta(idx, string(input))
			}
			p.emitter.ToolUseBlockStop(idx, block.ID, block.Name, input)
		}
	}
}

func (p *ClaudeProvider) translateBlockStart(index int, blockRaw json.RawMessage) {
	var cb struct {
		Type     string          `json:"type"`
		ID       string          `json:"id,omitempty"`
		Name     string          `json:"name,omitempty"`
		Text     string          `json:"text,omitempty"`
		Thinking string          `json:"thinking,omitempty"`
		Input    json.RawMessage `json:"input,omitempty"`
	}
	if err := json.Unmarshal(blockRaw, &cb); err != nil {
		return
	}

	switch cb.Type {
	case events.BlockText:
		p.emitter.TextBlockStart(index)
		if cb.Text != "" {
			p.emitter.TextDelta(index, cb.Text)
		}
	case events.BlockThinking:
		p.emitter.ThinkingBlockStart(index)
		if cb.Thinking != "" {
			p.emitter.ThinkingDelta(index, cb.Thinking)
		}
	case events.BlockToolUse:
		p.emitter.ToolUseBlockStart(index, cb.ID, cb.Name)
		if len(cb.Input) > 0 && string(cb.Input) != "{}" {
			p.emitter.InputJsonDelta(index, string(cb.Input))
		}
	}
}

func (p *ClaudeProvider) translateBlockDelta(index int, deltaRaw json.RawMessage) {
	var d struct {
		Type        string `json:"type"`
		Text        string `json:"text,omitempty"`
		Thinking    string `json:"thinking,omitempty"`
		PartialJSON string `json:"partial_json,omitempty"`
	}
	if err := json.Unmarshal(deltaRaw, &d); err != nil {
		return
	}
	switch d.Type {
	case events.DeltaText:
		p.emitter.TextDelta(index, d.Text)
	case events.DeltaThinking:
		p.emitter.ThinkingDelta(index, d.Thinking)
	case events.DeltaInputJSON:
		p.emitter.InputJsonDelta(index, d.PartialJSON)
	}
}

func (p *ClaudeProvider) translateBlockStop(index int, blockRaw *json.RawMessage) {
	if blockRaw == nil {
		p.emitter.BlockStop(index)
		return
	}
	var cb struct {
		Type  string          `json:"type"`
		ID    string          `json:"id,omitempty"`
		Name  string          `json:"name,omitempty"`
		Input json.RawMessage `json:"input,omitempty"`
	}
	if err := json.Unmarshal(*blockRaw, &cb); err != nil || cb.Type != events.BlockToolUse {
		p.emitter.BlockStop(index)
		return
	}
	p.emitter.ToolUseBlockStop(index, cb.ID, cb.Name, cb.Input)
}

func (p *ClaudeProvider) translateUser(raw json.RawMessage) {
	var ev struct {
		Message struct {
			Content []struct {
				Type      string          `json:"type"`
				ToolUseID string          `json:"tool_use_id"`
				ToolName  string          `json:"tool_name,omitempty"`
				Content   json.RawMessage `json:"content"`
				IsError   bool            `json:"is_error"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		p.emitter.EmitVersionedRaw(raw)
		return
	}

	for _, block := range ev.Message.Content {
		if block.Type != "tool_result" {
			continue
		}
		p.emitter.ToolResult(block.ToolUseID, block.ToolName, sessionstypes.FlattenTextBlocks(block.Content), block.IsError)
	}
}

func (p *ClaudeProvider) translateResult(raw json.RawMessage) {
	var result struct {
		Usage *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
		TotalCostUsd float64 `json:"total_cost_usd"`
	}
	if err := json.Unmarshal(raw, &result); err == nil && result.Usage != nil {
		stats := sessionstypes.SessionStats{
			InputTokens:         result.Usage.InputTokens,
			OutputTokens:        result.Usage.OutputTokens,
			CacheReadTokens:     result.Usage.CacheReadInputTokens,
			CacheCreationTokens: result.Usage.CacheCreationInputTokens,
			CostUsd:             result.TotalCostUsd,
		}

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
	}
	p.handler(events.HandlerMessageComplete, nil)
}

// ---------------------------------------------------------------------------
// control_request — host session permissions. A host has no PreToolUse hook
// binary or bridge socket to dial, so --permission-prompt-tool stdio moves
// the same question onto Claude's own stdout as a control_request and takes
// the answer on stdin as a control_response. Console sessions never emit
// control_request (they use the hook instead), so this is a no-op branch
// for them.
// ---------------------------------------------------------------------------

type claudeControlRequest struct {
	RequestID json.RawMessage `json:"request_id"`
	Request   struct {
		Subtype   string          `json:"subtype"`
		ToolName  string          `json:"tool_name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
	} `json:"request"`
}

type controlResponseResult struct {
	Behavior     string          `json:"behavior"`
	UpdatedInput json.RawMessage `json:"updatedInput,omitempty"`
	Message      string          `json:"message,omitempty"`
}

type controlResponseBody struct {
	RequestID json.RawMessage        `json:"request_id"`
	Subtype   string                 `json:"subtype"`
	Response  *controlResponseResult `json:"response,omitempty"`
	Error     string                 `json:"error,omitempty"`
}

type controlResponseEnvelope struct {
	Type     string              `json:"type"`
	Response controlResponseBody `json:"response"`
}

func buildControlResponseAllow(requestID, input json.RawMessage) []byte {
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	data, _ := json.Marshal(controlResponseEnvelope{
		Type: "control_response",
		Response: controlResponseBody{
			RequestID: requestID,
			Subtype:   "success",
			Response:  &controlResponseResult{Behavior: "allow", UpdatedInput: input},
		},
	})
	return data
}

func buildControlResponseDeny(requestID json.RawMessage, message string) []byte {
	data, _ := json.Marshal(controlResponseEnvelope{
		Type: "control_response",
		Response: controlResponseBody{
			RequestID: requestID,
			Subtype:   "success",
			Response:  &controlResponseResult{Behavior: "deny", Message: message},
		},
	})
	return data
}

func buildControlResponseUnsupported(requestID json.RawMessage) []byte {
	data, _ := json.Marshal(controlResponseEnvelope{
		Type: "control_response",
		Response: controlResponseBody{
			RequestID: requestID,
			Subtype:   "error",
			Error:     "unsupported",
		},
	})
	return data
}

func (p *ClaudeProvider) handleControlRequest(raw json.RawMessage) {
	var req claudeControlRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		slog.Warn("claude: malformed control_request", "session", p.session.ID, "error", err)
		return
	}

	if req.Request.Subtype != "can_use_tool" {
		p.writeControlResponse(buildControlResponseUnsupported(req.RequestID))
		return
	}

	toolInput := string(req.Request.Input)
	if toolInput == "" {
		toolInput = "{}"
	}

	if policy := p.session.Policy; policy != nil {
		if permission.MatchToolRule(req.Request.ToolName, toolInput, policy.DeniedTools) {
			p.writeControlResponse(buildControlResponseDeny(req.RequestID, "denied by project policy"))
			return
		}
		if permission.MatchToolRule(req.Request.ToolName, toolInput, policy.AllowedTools) {
			p.writeControlResponse(buildControlResponseAllow(req.RequestID, req.Request.Input))
			return
		}
	}

	if p.perms == nil {
		p.writeControlResponse(buildControlResponseDeny(req.RequestID, "no permission manager configured"))
		return
	}

	pending, ch := p.perms.CreateRequest(p.session.ID, req.Request.ToolName, toolInput, req.Request.ToolUseID)
	p.perms.NotifySession(p.session.ID, map[string]interface{}{
		"type":         "permission_request",
		"sessionId":    p.session.ID,
		"permissionId": pending.ID,
		"toolName":     req.Request.ToolName,
		"toolInput":    toolInput,
		"toolUseId":    req.Request.ToolUseID,
	})

	go func() {
		decision := p.perms.WaitForDecision(pending.ID, ch)
		if decision.Decision == "allow" {
			p.writeControlResponse(buildControlResponseAllow(req.RequestID, req.Request.Input))
			return
		}
		reason := decision.Reason
		if reason == "" {
			reason = "Denied by user"
		}
		p.writeControlResponse(buildControlResponseDeny(req.RequestID, reason))
	}()
}

func (p *ClaudeProvider) writeControlResponse(data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stdin == nil {
		return
	}
	if _, err := p.stdin.Write(append(data, '\n')); err != nil {
		slog.Warn("claude: write control_response failed", "session", p.session.ID, "error", err)
	}
}

func (p *ClaudeProvider) SendMessage(text string, files []sessionstypes.FileAttachment) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.alive.Load() || p.stdin == nil {
		return fmt.Errorf("claude process not running")
	}

	p.touchActivity()

	var content []interface{}
	for _, f := range files {
		content = append(content, map[string]interface{}{
			"type": "image",
			"source": map[string]string{
				"type":       "base64",
				"media_type": f.MimeType,
				"data":       f.Data,
			},
		})
	}
	content = append(content, map[string]string{"type": "text", "text": text})

	msg := map[string]interface{}{
		"type": "user",
		"message": map[string]interface{}{
			"role":    "user",
			"content": content,
		},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}
	data = append(data, '\n')

	p.msgStartNano.Store(time.Now().UnixNano())
	p.firstTokenNano.Store(0)

	if _, err := p.stdin.Write(data); err != nil {
		return fmt.Errorf("write to stdin: %w", err)
	}

	return nil
}

func (p *ClaudeProvider) StopGeneration() {
	p.Kill()
}

func (p *ClaudeProvider) Kill() {
	// The process that would read a control_response is going away — deny
	// every request that's still waiting on one. No-op if p.perms is nil or
	// nothing is pending.
	if p.perms != nil {
		p.perms.DenyAllForSession(p.session.ID, "session stopped")
	}

	p.cleanupSpawnFiles()

	if p.cmd == nil || p.cmd.Process == nil {
		return
	}

	p.alive.Store(false)

	if p.stopIdle != nil {
		p.stopIdleOnce.Do(func() { close(p.stopIdle) })
	}

	if p.stdin != nil {
		_ = p.stdin.Close()
	}

	_ = p.cmd.Process.Signal(os.Interrupt)

	select {
	case <-p.waitDone:
	case <-time.After(3 * time.Second):
		// Target group first, then the shim, so the shim's own Wait can
		// return and emit its final exit status. p.targetPID stays 0 on a
		// direct spawn or an SSH-hosted session, so their behavior here is
		// unchanged.
		if p.targetPID > 0 {
			_ = syscall.Kill(-p.targetPID, syscall.SIGKILL)
		}
		_ = p.cmd.Process.Kill()
		<-p.waitDone
	}

	slog.Info("claude process killed", "session", p.session.ID)
}

func (p *ClaudeProvider) Alive() bool {
	return p.alive.Load()
}

func (p *ClaudeProvider) SetPermissionMode(mode string) error {
	if p.cfg.Identity != nil || p.cfg.SandboxProfile != "" {
		// This launch's identity is single-use (a second Hello is refused)
		// and Kill fires process_exited, which tears down the sandbox
		// profile file and ends the launch identity — a Kill-then-Start
		// restart below would hand --sandbox-profile a path that no longer
		// exists. The caller must drive a real resume instead, which mints
		// both fresh.
		return ErrRestartNeedsResume
	}
	switch mode {
	case "", "default", "acceptEdits", "plan", "bypassPermissions":
	default:
		return fmt.Errorf("invalid permission mode: %q", mode)
	}
	if mode == "" {
		mode = "default"
	}

	ok := p.session.WithLockIfNotProcessing(func() {
		p.session.PermissionMode = mode
		p.session.Headless = (mode == "bypassPermissions")
	})
	if !ok {
		return fmt.Errorf("cannot change permission mode while session is generating; stop the response first")
	}

	if p.Alive() {
		p.Kill()
	}
	return p.Start()
}

func (p *ClaudeProvider) DeleteSession() error {
	p.mu.Lock()
	sid := p.claudeSessionID
	p.mu.Unlock()
	if sid == "" {
		return nil
	}

	if host := p.session.GetHost(); host != nil {
		return deleteClaudeHistoryOverSSH(host, p.directory, sid)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("get home dir: %w", err)
	}

	pattern := filepath.Join(home, ".claude", "projects", "*", sid+".jsonl")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("glob claude session: %w", err)
	}

	for _, path := range matches {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove claude session file: %w", err)
		}
		slog.Info("deleted claude session file", "path", path)
	}

	return nil
}

func (p *ClaudeProvider) GetState() json.RawMessage {
	p.mu.Lock()
	sid := p.claudeSessionID
	p.mu.Unlock()
	state := map[string]interface{}{"claudeSessionId": sid}
	data, _ := json.Marshal(state)
	return data
}

func (p *ClaudeProvider) RestoreState(state json.RawMessage) {
	if state == nil {
		return
	}
	var s struct {
		ClaudeSessionID string `json:"claudeSessionId"`
	}
	if err := json.Unmarshal(state, &s); err == nil {
		p.mu.Lock()
		p.claudeSessionID = s.ClaudeSessionID
		p.mu.Unlock()
	}
}
