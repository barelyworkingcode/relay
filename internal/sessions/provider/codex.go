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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/events"
	sessionsmcp "github.com/barelyworkingcode/relay/internal/sessions/mcp"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
	"github.com/barelyworkingcode/relay/internal/sshhost"
)

const codexHandshakeTimeout = 30 * time.Second

// CodexConfig carries what Start needs to build one Codex app-server child,
// resolved by the caller. Codex brings its own login, so there is no model
// key here.
type CodexConfig struct {
	Binary         string // "" → resolveCodexPath
	ShimBinary     string
	BridgeSocket   string
	SandboxProfile string                    // per launch
	Identity       *sessionsmcp.IdentitySpec // per launch
}

// codexNotifications is codex-cli 0.160.0's ServerNotification method list.
// A method outside it is unrecognised and warned about.
var codexNotifications = setOf(
	"error", "thread/started", "thread/status/changed", "thread/archived", "thread/deleted",
	"thread/unarchived", "thread/closed", "thread/reverted", "skills/changed", "thread/name/updated",
	"thread/attachment/updated", "thread/goal/updated", "thread/goal/cleared", "thread/queue/changed",
	"project/changed", "thread/project/updated", "thread/environment/connected",
	"thread/environment/disconnected", "thread/settings/updated", "thread/tokenUsage/updated",
	"turn/started", "hook/started", "turn/completed", "hook/completed", "turn/diff/updated",
	"turn/plan/updated", "item/started", "item/autoApprovalReview/started",
	"item/autoApprovalReview/completed", "autoApprovalReview/strictReviewRequired", "item/completed",
	"item/agentMessage/delta", "item/plan/delta", "command/exec/outputDelta", "process/outputDelta",
	"process/exited", "item/commandExecution/outputDelta", "item/commandExecution/terminalInteraction",
	"item/fileChange/outputDelta", "item/fileChange/patchUpdated", "serverRequest/resolved",
	"item/mcpToolCall/progress", "mcpServer/oauthLogin/completed", "mcpServer/startupStatus/updated",
	"mcpServer/event/stream/notification", "account/updated", "account/gatewayOAuth/changed",
	"account/rateLimits/updated", "app/list/updated", "remoteControl/status/changed",
	"externalAgentConfig/import/progress", "externalAgentConfig/import/completed", "fs/changed",
	"item/reasoning/summaryTextDelta", "item/reasoning/summaryPartAdded", "item/reasoning/textDelta",
	"thread/compacted", "model/rerouted", "model/verification", "modelProvider/authRecoveryStarted",
	"modelProvider/authRecoveryCompleted", "turn/moderationMetadata", "model/safetyBuffering/updated",
	"warning", "guardianWarning", "deprecationNotice", "configWarning", "fuzzyFileSearch/sessionUpdated",
	"fuzzyFileSearch/sessionCompleted", "thread/realtime/started", "thread/realtime/itemAdded",
	"thread/realtime/item/started", "thread/realtime/item/transcript/delta",
	"thread/realtime/item/completed", "thread/realtime/transcript/delta", "thread/realtime/transcript/done",
	"thread/realtime/outputAudio/delta", "thread/realtime/sdp", "thread/realtime/error",
	"thread/realtime/closed", "windows/worldWritableWarning", "windowsSandbox/setupCompleted",
	"account/login/completed",
)

// codexItemTypes is codex-cli 0.160.0's ThreadItem type list.
var codexItemTypes = setOf(
	"userMessage", "hookPrompt", "agentMessage", "functionCallOutput", "plan", "reasoning",
	"commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall", "collabAgentToolCall",
	"subAgentActivity", "webSearch", "imageView", "sleep", "imageGeneration", "enteredReviewMode",
	"exitedReviewMode", "contextCompaction",
)

func setOf(items ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(items))
	for _, s := range items {
		m[s] = struct{}{}
	}
	return m
}

// codexRPCError is a JSON-RPC error object.
type codexRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// codexReply is the outcome of one client request. err is non-empty for a
// JSON-RPC error reply.
type codexReply struct {
	result json.RawMessage
	err    string
}

// codexItem is one open agent-message or tool item of the current turn.
type codexItem struct {
	idx     int
	text    strings.Builder
	started bool
}

// CodexProvider drives one `codex app-server` child over newline-delimited
// JSON-RPC and translates its notifications into the standard event stream.
type CodexProvider struct {
	session *sessionstypes.Session
	handler sessionstypes.EventHandler
	cfg     CodexConfig
	emitter *events.EventEmitter

	cmd   *exec.Cmd
	stdin io.WriteCloser
	mu    sync.Mutex // guards stdin writes
	alive atomic.Bool

	// targetPID is the shim's child when the launch went through the shim.
	targetPID int

	directory string
	slug      string

	// stateMu guards threadID, turnID and pending.
	stateMu  sync.Mutex
	threadID string
	turnID   string
	pending  map[int64]func(codexReply)
	nextID   atomic.Int64

	// Turn state, touched only by the stdout reader goroutine.
	items      map[string]*codexItem
	nextBlock  int
	allBlocks  []map[string]any
	turnActive bool

	warnedMu sync.Mutex
	warned   map[string]struct{}

	waitDone     chan struct{}
	drainTimeout time.Duration
	// killed is per spawn so an old spawn's late exit cannot read the new
	// spawn's flag.
	killed *atomic.Bool
	// exitMu guards spawnGen; a spawn's waitForExit holds it from the
	// generation check through the process_exited handler call. Deliberate:
	// the handler runs under the lock, so it must never call Start here.
	exitMu   sync.Mutex
	spawnGen uint64
}

func NewCodexProvider(session *sessionstypes.Session, handler sessionstypes.EventHandler, cfg CodexConfig) *CodexProvider {
	return &CodexProvider{
		session:   session,
		handler:   handler,
		cfg:       cfg,
		emitter:   events.NewEventEmitter(handler),
		directory: session.Directory,
		slug:      strings.TrimPrefix(session.Model, codexModelPrefix),
		pending:   make(map[int64]func(codexReply)),
		items:     make(map[string]*codexItem),
	}
}

// resolveCodexPath finds the codex binary: configured path first, then
// well-known install locations, then PATH, then the literal "codex".
func resolveCodexPath(configured string) string {
	home, _ := os.UserHomeDir()
	if configured != "" {
		if rest, ok := strings.CutPrefix(configured, "~/"); ok && home != "" {
			return filepath.Join(home, rest)
		}
		return configured
	}
	candidates := []string{"/opt/homebrew/bin/codex", "/usr/local/bin/codex"}
	if home != "" {
		candidates = append(candidates, filepath.Join(home, ".local", "bin", "codex"))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	if p, err := exec.LookPath("codex"); err == nil {
		return p
	}
	return "codex"
}

// buildCodexHostExec assembles argv to run `codex app-server` on a host:
// relay's ssh_argv prefix, `-T` (no tty), `--`, and a RemoteCommand running
// the host's codex path under dir. env carries only RELAY_SESSION_ID.
func buildCodexHostExec(spec *sessionstypes.HostSpec, dir, sessionID string) (name string, argv []string, err error) {
	if len(spec.SSHArgv) == 0 {
		return "", nil, fmt.Errorf("host %q has no ssh_argv", spec.Name)
	}
	if spec.CodexPath == "" {
		return "", nil, fmt.Errorf("host %q has no codex path: set its codex template's command", spec.Name)
	}
	env := map[string]string{"RELAY_SESSION_ID": sessionID}
	remote := sshhost.RemoteCommandForOS(spec.OS, dir, []string{spec.CodexPath, "app-server"}, env)
	name = spec.SSHArgv[0]
	argv = append(append([]string{}, spec.SSHArgv[1:]...), "-T", "--", remote)
	return name, argv, nil
}

func (p *CodexProvider) Start() (err error) {
	if p.slug == "" {
		return errors.New("codex session has no model")
	}

	p.stateMu.Lock()
	p.pending = make(map[int64]func(codexReply))
	p.turnID = ""
	p.stateMu.Unlock()
	p.nextID.Store(0)
	p.warnedMu.Lock()
	p.warned = nil
	p.warnedMu.Unlock()
	p.resetTurnState()

	var cmd *exec.Cmd
	var statusR *os.File
	var extraFiles []*os.File

	if host := p.session.GetHost(); host != nil {
		if useRelayTools(p.session.Settings) {
			warnRelayToolsUnavailable(p.session.ID, "codex", "host_session")
		}
		if p.cfg.SandboxProfile != "" || p.cfg.Identity != nil {
			slog.Warn("codex: sandbox/identity requested for a host session; ignoring", "session", p.session.ID)
		}
		name, argv, herr := buildCodexHostExec(host, p.directory, p.session.ID)
		if herr != nil {
			return herr
		}
		cmd = exec.Command(name, argv...)
		cmd.Env = childBaseEnv()
	} else {
		if useRelayTools(p.session.Settings) {
			warnRelayToolsUnavailable(p.session.ID, "codex", "unsupported_kind")
		}
		codexPath := resolveCodexPath(p.cfg.Binary)
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
			cmd, statusR, extraFiles, berr = buildShimCmd(spec, codexPath, []string{"app-server"})
			if berr != nil {
				return berr
			}
		default:
			cmd = exec.Command(codexPath, "app-server")
		}
		cmd.Dir = p.directory
		add := map[string]string{"RELAY_SESSION_ID": p.session.ID}
		if p.cfg.BridgeSocket != "" {
			add["RELAY_BRIDGE_SOCKET"] = p.cfg.BridgeSocket
		}
		cmd.Env = mergeEnv(ensurePath(childBaseEnv()), add)
	}
	fds := &spawnFDs{statusR: statusR, extraFiles: extraFiles}
	defer fds.closeUnlessStarted()

	stdoutR, stdoutW, err := newStdoutPipe(cmd)
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	fds.pipes = append(fds.pipes, stdoutR, stdoutW)

	stderrR, stderrW, err := newStderrPipe(cmd)
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	fds.pipes = append(fds.pipes, stderrR, stderrW)

	// Deliberate: StdinPipe is last so no return sits between it and Start.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start codex: %w", err)
	}
	fds.started = true
	_ = stdoutW.Close()
	_ = stderrW.Close()
	sawStdout := &atomic.Bool{}
	identitySecret := ""
	if p.cfg.Identity != nil {
		identitySecret = p.cfg.Identity.Secret
	}
	logStderr := func() []string {
		return logProviderStderr(stderrR, p.session.ID, "codex", sawStdout, identitySecret)
	}
	// The deadline is deliberate: a target forked with Setpgid can outlive
	// the Kill and keep the write end open, and Start must not wait on it.
	drainStderr := func() {
		_ = stdoutR.Close()
		_ = stderrR.SetReadDeadline(time.Now().Add(time.Second))
		logStderr()
	}
	// Load-bearing: without closing the parent's own copies, the status pipe
	// (and the identity secret pipe) never reaches EOF.
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
			drainStderr()
			return fmt.Errorf("%w: session %s", ErrIdentityRefused, p.session.ID)
		case !outcome.started || outcome.spawnFailed:
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			drainStderr()
			return fmt.Errorf("%w: errno=%d", ErrSpawnFailed, outcome.spawnErrno)
		default:
			p.targetPID = outcome.targetPID
		}
	}

	p.cmd = cmd
	p.stdin = stdin
	p.exitMu.Lock()
	p.spawnGen++
	gen := p.spawnGen
	p.exitMu.Unlock()
	p.alive.Store(true)
	p.waitDone = make(chan struct{})
	p.drainTimeout = providerDrainTimeout
	p.killed = &atomic.Bool{}

	out := newSpawnOutput(stdoutR, stderrR)
	go p.readStdout(stdoutR, sawStdout, out.stdoutDone)
	go func() { out.stderrTail <- logStderr() }()
	go p.waitForExit(cmd, p.waitDone, out, p.drainTimeout, p.killed, gen)

	slog.Info("codex process started", "session", p.session.ID, "model", p.slug, "pid", cmd.Process.Pid, "targetPid", p.targetPID)

	if err := p.handshake(); err != nil {
		// A failed Start must not surface as a live session's exit: advance
		// the generation so this spawn's waitForExit stays silent.
		p.exitMu.Lock()
		p.spawnGen++
		p.exitMu.Unlock()
		p.Kill()
		return fmt.Errorf("codex handshake: %w", err)
	}
	return nil
}

// handshake runs initialize, initialized and thread/start (or thread/resume)
// under one deadline.
func (p *CodexProvider) handshake() error {
	deadline := time.After(codexHandshakeTimeout)

	if _, err := p.call("initialize", map[string]any{
		"clientInfo": map[string]string{"name": "relay", "version": "1"},
	}, deadline); err != nil {
		return err
	}
	if err := p.writeLine(map[string]any{"method": "initialized"}); err != nil {
		return err
	}

	params := map[string]any{
		"cwd":            p.directory,
		"model":          p.slug,
		"approvalPolicy": "never",
		"sandbox":        "danger-full-access",
	}
	if p.session.SystemPrompt != "" {
		params["developerInstructions"] = p.session.SystemPrompt
	}
	method := "thread/start"
	p.stateMu.Lock()
	resume := p.threadID
	p.stateMu.Unlock()
	if resume != "" {
		method = "thread/resume"
		params["threadId"] = resume
	}
	result, err := p.call(method, params, deadline)
	if err != nil {
		return err
	}
	var r struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(result, &r); err != nil || r.Thread.ID == "" {
		return fmt.Errorf("%s: reply carries no thread id", method)
	}
	p.stateMu.Lock()
	p.threadID = r.Thread.ID
	p.stateMu.Unlock()
	return nil
}

// call sends one request and waits for its reply, the deadline, or the
// child's exit.
func (p *CodexProvider) call(method string, params any, deadline <-chan time.Time) (json.RawMessage, error) {
	ch := make(chan codexReply, 1)
	id, err := p.request(method, params, func(r codexReply) { ch <- r })
	if err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		if r.err != "" {
			return nil, fmt.Errorf("%s: %s", method, r.err)
		}
		return r.result, nil
	case <-deadline:
		p.dropPending(id)
		return nil, fmt.Errorf("%s: timed out", method)
	case <-p.waitDone:
		p.dropPending(id)
		return nil, fmt.Errorf("%s: codex exited", method)
	}
}

// request writes a request and registers onReply, which runs on the stdout
// reader goroutine.
func (p *CodexProvider) request(method string, params any, onReply func(codexReply)) (int64, error) {
	id := p.nextID.Add(1)
	p.stateMu.Lock()
	p.pending[id] = onReply
	p.stateMu.Unlock()
	if err := p.writeLine(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		p.dropPending(id)
		return 0, err
	}
	return id, nil
}

func (p *CodexProvider) dropPending(id int64) {
	p.stateMu.Lock()
	delete(p.pending, id)
	p.stateMu.Unlock()
}

func (p *CodexProvider) writeLine(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	data = append(data, '\n')
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.alive.Load() || p.stdin == nil {
		return errors.New("codex process not running")
	}
	if _, err := p.stdin.Write(data); err != nil {
		return fmt.Errorf("write to stdin: %w", err)
	}
	return nil
}

func (p *CodexProvider) readStdout(r io.ReadCloser, sawStdout *atomic.Bool, done chan<- struct{}) {
	defer close(done)
	defer func() { _ = r.Close() }()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		sawStdout.Store(true)
		p.processLine(json.RawMessage(append([]byte(nil), line...)))
	}
	if err := scanner.Err(); err != nil {
		logStdoutReadError(p.session.ID, "codex", err)
	}
}

// waitForExit takes its spawn's own cmd and channels as arguments: after Kill
// then Start, a goroutine re-reading the fields would race the next Start.
func (p *CodexProvider) waitForExit(cmd *exec.Cmd, waitDone chan struct{}, out *spawnOutput, drainTimeout time.Duration, killed *atomic.Bool, gen uint64) {
	err := cmd.Wait()
	p.alive.Store(false)
	close(waitDone)

	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
	}

	tail := out.drain(drainTimeout)
	p.exitMu.Lock()
	defer p.exitMu.Unlock()
	if p.spawnGen != gen {
		slog.Debug("provider exit from a superseded spawn dropped", "session", p.session.ID, "kind", "codex", "exitCode", exitCode)
		return
	}
	slog.Info("codex process exited", "session", p.session.ID, "exitCode", exitCode)
	if !killed.Load() {
		warnProviderExit(p.session.ID, "codex", exitCode, tail)
	}
	data, _ := json.Marshal(map[string]interface{}{"exitCode": exitCode})
	p.handler("process_exited", data)
}

// codexEnvelope is the union of the three JSON-RPC line shapes: a reply
// (id, no method), a server request (id and method) and a notification
// (method, no id).
type codexEnvelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *codexRPCError  `json:"error"`
}

func (p *CodexProvider) processLine(raw json.RawMessage) {
	var env codexEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		p.warnUnrecognisedOnce("(not json)", "")
		p.handler("raw_output", raw)
		return
	}
	hasID := len(env.ID) > 0 && string(env.ID) != "null"
	switch {
	case hasID && env.Method != "":
		p.answerServerRequest(env)
	case hasID:
		p.deliverReply(env)
	case env.Method != "":
		p.translate(env.Method, env.Params, raw)
	default:
		p.warnUnrecognisedOnce("(no method)", "")
		p.handler("raw_output", raw)
	}
}

func (p *CodexProvider) deliverReply(env codexEnvelope) {
	id, err := strconv.ParseInt(string(env.ID), 10, 64)
	if err != nil {
		return
	}
	p.stateMu.Lock()
	cb := p.pending[id]
	delete(p.pending, id)
	p.stateMu.Unlock()
	if cb == nil {
		return
	}
	reply := codexReply{result: env.Result}
	if env.Error != nil {
		reply.err = env.Error.Message
		if reply.err == "" {
			reply.err = "request failed"
		}
	}
	cb(reply)
}

// answerServerRequest always replies: a request left waiting would hang the
// turn. Approvals are declined because approvalPolicy "never" means one only
// arrives when something outside relay forced it.
func (p *CodexProvider) answerServerRequest(env codexEnvelope) {
	var result any
	switch env.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		result = map[string]string{"decision": "decline"}
	case "applyPatchApproval", "execCommandApproval":
		result = map[string]string{"decision": "denied"}
	case "mcpServer/elicitation/request":
		result = map[string]string{"action": "decline"}
	}
	reply := map[string]any{"id": env.ID}
	if result != nil {
		reply["result"] = result
		slog.Warn("codex: server request declined", "session", p.session.ID, "method", env.Method)
	} else {
		reply["error"] = codexRPCError{Code: -32601, Message: "unsupported by relay"}
		slog.Warn("codex: server request unsupported", "session", p.session.ID, "method", env.Method)
	}
	if err := p.writeLine(reply); err != nil {
		slog.Warn("codex: reply to server request failed", "session", p.session.ID, "method", env.Method, "error", err)
	}
}

// warnUnrecognisedOnce logs one Warn per method (and item type) per spawn.
func (p *CodexProvider) warnUnrecognisedOnce(method, itemType string) {
	key := method + "\x00" + itemType
	p.warnedMu.Lock()
	_, seen := p.warned[key]
	if !seen {
		if p.warned == nil {
			p.warned = make(map[string]struct{})
		}
		p.warned[key] = struct{}{}
	}
	p.warnedMu.Unlock()
	if seen {
		return
	}
	if itemType != "" {
		slog.Warn("codex: unrecognised event", "session", p.session.ID, "method", method, "itemType", itemType)
		return
	}
	slog.Warn("codex: unrecognised event", "session", p.session.ID, "method", method)
}

func (p *CodexProvider) resetTurnState() {
	p.items = make(map[string]*codexItem)
	p.nextBlock = 0
	p.allBlocks = nil
}

type codexThreadItem struct {
	Type             string `json:"type"`
	ID               string `json:"id"`
	Text             string `json:"text"`
	Command          string `json:"command"`
	Status           string `json:"status"`
	AggregatedOutput string `json:"aggregatedOutput"`
	ExitCode         *int   `json:"exitCode"`
}

func (p *CodexProvider) translate(method string, params, raw json.RawMessage) {
	switch method {
	case "turn/started":
		p.resetTurnState()
		p.turnActive = true
		var ev struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if json.Unmarshal(params, &ev) == nil && ev.Turn.ID != "" {
			p.stateMu.Lock()
			p.turnID = ev.Turn.ID
			p.stateMu.Unlock()
		}
		p.emitter.SystemInit(p.slug, p.directory, nil, nil)
		p.emitter.MessageStart("")

	case "item/agentMessage/delta":
		var ev struct {
			ItemID string `json:"itemId"`
			Delta  string `json:"delta"`
		}
		if json.Unmarshal(params, &ev) != nil || ev.ItemID == "" {
			return
		}
		it := p.item(ev.ItemID)
		if !it.started {
			it.started = true
			p.emitter.TextBlockStart(it.idx)
		}
		it.text.WriteString(ev.Delta)
		p.emitter.TextDelta(it.idx, ev.Delta)

	case "item/started", "item/completed":
		var ev struct {
			Item codexThreadItem `json:"item"`
		}
		if json.Unmarshal(params, &ev) != nil {
			p.warnUnrecognisedOnce(method, "(malformed)")
			p.handler("raw_output", raw)
			return
		}
		p.translateItem(method == "item/completed", ev.Item, raw)

	case "error":
		var ev struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			WillRetry bool `json:"willRetry"`
		}
		if json.Unmarshal(params, &ev) != nil {
			return
		}
		p.emitter.SystemAPIError(nestedCodexError(ev.Error.Message), ev.WillRetry)

	case "turn/completed":
		p.translateTurnCompleted(params)

	default:
		if _, known := codexNotifications[method]; known {
			return
		}
		p.warnUnrecognisedOnce(method, "")
		p.handler("raw_output", raw)
	}
}

func (p *CodexProvider) item(id string) *codexItem {
	it := p.items[id]
	if it == nil {
		it = &codexItem{idx: p.nextBlock}
		p.nextBlock++
		p.items[id] = it
	}
	return it
}

func (p *CodexProvider) translateItem(completed bool, item codexThreadItem, raw json.RawMessage) {
	if _, known := codexItemTypes[item.Type]; !known {
		p.warnUnrecognisedOnce("item/started|completed", item.Type)
		p.handler("raw_output", raw)
		return
	}
	switch item.Type {
	case "agentMessage":
		if !completed {
			return
		}
		it := p.item(item.ID)
		if !it.started {
			it.started = true
			p.emitter.TextBlockStart(it.idx)
			if item.Text != "" {
				it.text.WriteString(item.Text)
				p.emitter.TextDelta(it.idx, item.Text)
			}
		}
		p.emitter.BlockStop(it.idx)
		p.allBlocks = append(p.allBlocks, map[string]any{"type": events.BlockText, "text": it.text.String()})
		delete(p.items, item.ID)

	case "commandExecution":
		p.openShellItem(item)
		if !completed {
			return
		}
		isErr := item.Status == "failed" || (item.ExitCode != nil && *item.ExitCode != 0)
		p.emitter.ToolResult(item.ID, "shell", item.AggregatedOutput, isErr, false)
		delete(p.items, item.ID)
	}
}

// openShellItem emits the tool_use block once per command item, whether the
// first event seen is its start or its completion.
func (p *CodexProvider) openShellItem(item codexThreadItem) {
	it := p.item(item.ID)
	if it.started {
		return
	}
	it.started = true
	input, _ := json.Marshal(map[string]string{"command": item.Command})
	p.emitter.ToolUseBlockStart(it.idx, item.ID, "shell")
	p.emitter.ToolUseBlockStop(it.idx, item.ID, "shell", input)
	p.allBlocks = append(p.allBlocks, map[string]any{
		"type":  events.BlockToolUse,
		"id":    item.ID,
		"name":  "shell",
		"input": json.RawMessage(input),
	})
}

func (p *CodexProvider) translateTurnCompleted(params json.RawMessage) {
	var ev struct {
		Turn struct {
			Status string `json:"status"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(params, &ev)

	// Close any text block a lost item/completed left open.
	for id, it := range p.items {
		if it.started && it.text.Len() > 0 {
			p.emitter.BlockStop(it.idx)
			p.allBlocks = append(p.allBlocks, map[string]any{"type": events.BlockText, "text": it.text.String()})
		}
		delete(p.items, id)
	}

	blocks := p.allBlocks
	p.allBlocks = nil
	p.turnActive = false
	p.stateMu.Lock()
	p.turnID = ""
	p.stateMu.Unlock()

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

	if ev.Turn.Status == "failed" {
		msg := "turn failed"
		if ev.Turn.Error != nil && ev.Turn.Error.Message != "" {
			msg = shortenError(nestedCodexError(ev.Turn.Error.Message))
		}
		slog.Warn("codex turn failed", "session", p.session.ID, "model", p.slug, "error", msg)
		data, _ := json.Marshal(map[string]string{"error": msg})
		p.handler("error", data)
	}
	p.handler(events.HandlerMessageComplete, nil)
}

// nestedCodexError unwraps the model API's JSON error body Codex passes
// through as a string, returning its error.message, or s unchanged when it
// is not that shape.
func nestedCodexError(s string) string {
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if strings.HasPrefix(strings.TrimSpace(s), "{") && json.Unmarshal([]byte(s), &body) == nil && body.Error.Message != "" {
		return body.Error.Message
	}
	return s
}

func (p *CodexProvider) SendMessage(text string, files []sessionstypes.FileAttachment) error {
	if !p.alive.Load() || p.stdin == nil {
		return errors.New("codex process not running")
	}
	p.stateMu.Lock()
	thread := p.threadID
	p.stateMu.Unlock()
	if thread == "" {
		return errors.New("codex thread not started")
	}
	if len(files) > 0 {
		slog.Warn("codex: attachments are not supported; sending text only", "session", p.session.ID, "files", len(files))
	}
	_, err := p.request("turn/start", map[string]any{
		"threadId": thread,
		"input":    []map[string]string{{"type": "text", "text": text}},
	}, p.onTurnStartReply)
	return err
}

func (p *CodexProvider) onTurnStartReply(r codexReply) {
	if r.err != "" {
		slog.Warn("codex turn/start failed", "session", p.session.ID, "error", r.err)
		data, _ := json.Marshal(map[string]string{"error": r.err})
		p.handler("error", data)
		p.handler(events.HandlerMessageComplete, nil)
		return
	}
	var res struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(r.result, &res) == nil && res.Turn.ID != "" {
		p.stateMu.Lock()
		p.turnID = res.Turn.ID
		p.stateMu.Unlock()
	}
}

func (p *CodexProvider) StopGeneration() {
	if !p.alive.Load() || p.stdin == nil {
		return
	}
	p.stateMu.Lock()
	thread, turn := p.threadID, p.turnID
	p.stateMu.Unlock()
	if thread == "" || turn == "" {
		return
	}
	_, err := p.request("turn/interrupt", map[string]any{"threadId": thread, "turnId": turn}, func(r codexReply) {
		if r.err != "" {
			slog.Warn("codex turn/interrupt failed", "session", p.session.ID, "error", r.err)
		}
	})
	if err != nil {
		slog.Warn("codex interrupt write failed, killing", "session", p.session.ID, "error", err)
		p.Kill()
	}
}

func (p *CodexProvider) Kill() {
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}
	if p.killed != nil {
		p.killed.Store(true)
	}
	p.alive.Store(false)

	if p.stdin != nil {
		_ = p.stdin.Close()
	}
	_ = p.cmd.Process.Signal(os.Interrupt)

	select {
	case <-p.waitDone:
	case <-time.After(3 * time.Second):
		// Target group first, then the shim, so the shim's own Wait can
		// return. targetPID stays 0 on a direct spawn.
		if p.targetPID > 0 {
			_ = syscall.Kill(-p.targetPID, syscall.SIGKILL)
		}
		_ = p.cmd.Process.Kill()
		<-p.waitDone
	}
	slog.Info("codex process killed", "session", p.session.ID)
}

func (p *CodexProvider) Alive() bool { return p.alive.Load() }

// DeleteSession is a no-op: Codex's own transcripts stay where Codex keeps
// them.
func (p *CodexProvider) DeleteSession() error { return nil }

func (p *CodexProvider) GetState() json.RawMessage {
	p.stateMu.Lock()
	tid := p.threadID
	p.stateMu.Unlock()
	data, _ := json.Marshal(map[string]string{"threadId": tid})
	return data
}

func (p *CodexProvider) RestoreState(state json.RawMessage) {
	if state == nil {
		return
	}
	var s struct {
		ThreadID string `json:"threadId"`
	}
	if err := json.Unmarshal(state, &s); err == nil {
		p.stateMu.Lock()
		p.threadID = s.ThreadID
		p.stateMu.Unlock()
	}
}
