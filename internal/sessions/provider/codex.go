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
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/barelyworkingcode/relay/internal/sessions/codexdaemon"
	"github.com/barelyworkingcode/relay/internal/sessions/events"
	sessionsmcp "github.com/barelyworkingcode/relay/internal/sessions/mcp"
	"github.com/barelyworkingcode/relay/internal/sessions/runner"
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

	// Launcher runs in place of the codex binary, with app-server after its
	// args. nil runs Binary.
	Launcher *sessionstypes.Launcher
	// DefaultModel starts a thread with no model, so codex uses its own.
	DefaultModel bool

	// Resume marks a start that continues an existing conversation. A runner
	// session looks for a running agent first and attaches to it, so a resume
	// never starts a second agent.
	Resume bool
	// AttachOnly makes Start attach to a running agent and fail with
	// ErrNoDetachedRun rather than start one.
	AttachOnly bool

	// RunnerBase is the directory local sessions' run directories live under.
	// Every local codex session runs under the runner there; Start refuses a
	// local session when it is empty.
	RunnerBase string
}

// codexRunnerPatterns are the strings the runner matches in codex's stdout: a
// turn/completed notification ends a turn. Codex has no question path, so
// Asking stays empty and never matches.
var codexRunnerPatterns = runner.Patterns{TurnEnded: `"method":"turn/completed"`}

// codexProbe is what codex's attach probe counts: turn endings only, since
// relay answers every server request at once and holds none for a person.
var codexProbe = probeSpec{TurnEnded: codexRunnerPatterns.TurnEnded}

// codexNotifications is codex-cli 0.160.0's ServerNotification method list.
// A method outside it is unrecognised and warned about.
var codexNotifications = setOf(
	"error", "thread/started", "thread/status/changed", "thread/archived", "thread/deleted",
	"thread/unarchived", "thread/closed", "thread/reverted", "skills/changed", "thread/name/updated",
	"thread/attachment/updated", "thread/goal/updated", "thread/goal/cleared", "thread/queue/changed",
	"project/changed", "thread/project/updated", "thread/environment/connected",
	"thread/environment/disconnected", "thread/settings/updated", "thread/tokenUsage/updated",
	"thread/readState/changed", "thread/prediction/updated",
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

	// cmd and stdin are the pipes of a host session that runs under ssh -T;
	// every other session talks to its agent through det.
	cmd   *exec.Cmd
	stdin io.WriteCloser
	mu    sync.Mutex // guards stdin writes
	alive atomic.Bool

	// det follows the agent a runner session keeps under the runner; nil for
	// a piped session.
	det atomic.Pointer[runFollower]
	// released is set by Release, which leaves the agent running.
	released atomic.Bool

	// daemonAttach is set by RestoreState: the thread is held by the codex
	// daemon, and this provider is one more client of it. daemon is that
	// connection while it lives. A daemon-attached provider never starts a codex
	// of its own.
	daemonAttach atomic.Bool
	daemon       atomic.Pointer[codexdaemon.Conn]

	directory string
	slug      string

	// stateMu guards threadID, turnID, pending, reqPrefix, restoredRun and
	// attachedTurnOpen.
	stateMu  sync.Mutex
	threadID string
	turnID   string
	// relayTurn is the turn this provider's own turn/start opened on a
	// daemon-attached thread, until that turn ends. Only a request for this
	// turn is answered.
	relayTurn string
	pending   map[string]func(codexReply)
	nextID    atomic.Int64
	// reqPrefix starts every client request id of one spawn. Deliberate: a reply
	// that a run directory still holds from before an attach never matches a
	// request made after it.
	reqPrefix string
	// restoredRun is the run and cursor the saved state named.
	restoredRun *detachedCursor
	// attachedTurnOpen is what an attach found and left for TakeAttach.
	attachedTurnOpen bool

	// Turn state, touched only by the stdout reader goroutine.
	items      map[string]*codexItem
	nextBlock  int
	allBlocks  []map[string]any
	turnActive bool

	warnedMu sync.Mutex
	warned   map[string]struct{}

	waitDone     chan struct{}
	exit         *spawnExit
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
		pending:   make(map[string]func(codexReply)),
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
func buildCodexHostExec(spec *sessionstypes.HostSpec, program []string, dir, sessionID string) (name string, argv []string, err error) {
	if len(spec.SSHArgv) == 0 {
		return "", nil, fmt.Errorf("host %q has no ssh_argv", spec.Name)
	}
	if len(program) == 0 || program[0] == "" {
		return "", nil, fmt.Errorf("host %q has no codex path: set its codex template's command", spec.Name)
	}
	env := map[string]string{"RELAY_SESSION_ID": sessionID}
	remote := sshhost.RemoteCommandForOS(spec.OS, dir, append(slices.Clone(program), "app-server"), env)
	name = spec.SSHArgv[0]
	argv = append(append([]string{}, spec.SSHArgv[1:]...), "-T", "--", remote)
	return name, argv, nil
}

func (p *CodexProvider) Start() (err error) {
	// An attach starts no thread, so it needs no model.
	if p.slug == "" && !p.cfg.DefaultModel && !p.cfg.AttachOnly && !p.daemonAttach.Load() {
		return errors.New("codex session has no model")
	}

	p.stateMu.Lock()
	p.pending = make(map[string]func(codexReply))
	p.turnID = ""
	p.relayTurn = ""
	p.reqPrefix = "relay-" + uuid.NewString()[:8] + "-"
	p.stateMu.Unlock()
	p.nextID.Store(0)
	p.warnedMu.Lock()
	p.warned = nil
	p.warnedMu.Unlock()
	p.resetTurnState()
	p.det.Store(nil)

	if p.daemonAttach.Load() {
		return p.startDaemon()
	}

	host := p.session.GetHost()
	if host == nil {
		if useRelayTools(p.session.Settings) {
			warnRelayToolsUnavailable(p.session.ID, "codex", "unsupported_kind")
		}
		return p.startLocal()
	}
	if useRelayTools(p.session.Settings) {
		warnRelayToolsUnavailable(p.session.ID, "codex", "host_session")
	}
	if p.cfg.SandboxProfile != "" || p.cfg.Identity != nil {
		slog.Warn("codex: sandbox/identity requested for a host session; ignoring", "session", p.session.ID)
	}
	if DetachedHost(host) {
		return p.startHost(host)
	}
	return p.startPiped(host)
}

// startPiped runs codex on a host under ssh -T, with its pipes as the only
// link: the agent ends with the connection. A host with detach_sessions false
// and a Windows host run this way.
func (p *CodexProvider) startPiped(host *sessionstypes.HostSpec) error {
	name, argv, herr := buildCodexHostExec(host, p.cfg.Launcher.Program(host.CodexPath), p.directory, p.session.ID)
	if herr != nil {
		return herr
	}
	cmd := exec.Command(name, argv...)
	cmd.Env = childBaseEnv()
	fds := &spawnFDs{}
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

	p.cmd = cmd
	p.stdin = stdin
	p.exitMu.Lock()
	p.spawnGen++
	gen := p.spawnGen
	p.exitMu.Unlock()
	p.alive.Store(true)
	p.waitDone = make(chan struct{})
	p.exit = &spawnExit{done: make(chan struct{})}
	p.drainTimeout = providerDrainTimeout
	p.killed = &atomic.Bool{}

	out := newSpawnOutput(stdoutR, stderrR)
	go p.readStdout(stdoutR, sawStdout, out.stdoutDone)
	go func() { out.stderrTail <- logProviderStderr(stderrR, p.session.ID, "codex", sawStdout) }()
	go p.waitForExit(cmd, p.waitDone, out, p.drainTimeout, p.killed, p.exit, gen)

	slog.Info("codex process started", "session", p.session.ID, "model", p.slug, "pid", cmd.Process.Pid)
	return p.startThread()
}

// startThread runs the handshake. A start that fails here must not surface as
// a live session's exit: the generation advances so the spawn's own exit stays
// silent, and the agent ends.
func (p *CodexProvider) startThread() error {
	if err := p.handshake(); err != nil {
		p.exitMu.Lock()
		p.spawnGen++
		p.exitMu.Unlock()
		p.Kill()
		return fmt.Errorf("codex handshake: %w", err)
	}
	return nil
}

// startLocal runs the agent on this machine under the runner, so it outlives
// relay-sessions. A resume or attach finds a runner that still lives first and
// follows it: a second agent must never share the conversation.
func (p *CodexProvider) startLocal() error {
	base := p.cfg.RunnerBase
	if base == "" {
		return errors.New("codex: no runner directory is configured for local sessions")
	}
	if p.cfg.Resume || p.cfg.AttachOnly {
		st, err := runner.LocalStatus(base, p.session.ID, "")
		if err != nil {
			return fmt.Errorf("read the local runner state: %w", err)
		}
		if st.Phase == runner.PhaseAlive {
			return p.attachRun(nil, localConnFor(base, p.session.ID), st.Generation)
		}
		if p.cfg.AttachOnly {
			return fmt.Errorf("%w on this machine", ErrNoDetachedRun)
		}
	}

	program := p.cfg.Launcher.Program(resolveCodexPath(p.cfg.Binary))
	add := map[string]string{"RELAY_SESSION_ID": p.session.ID}
	if p.cfg.BridgeSocket != "" {
		add["RELAY_BRIDGE_SOCKET"] = p.cfg.BridgeSocket
	}
	identitySecret := ""
	if p.cfg.Identity != nil {
		identitySecret = p.cfg.Identity.Secret
	}
	run, st, err := startLocalRun("codex", base, p.directory, shimSpec{
		Binary:         p.cfg.ShimBinary,
		SessionID:      p.session.ID,
		BridgeSocket:   p.cfg.BridgeSocket,
		SandboxProfile: p.cfg.SandboxProfile,
		Identity:       p.cfg.Identity,
	}, program, []string{"app-server"}, mergeEnv(ensurePath(childBaseEnv()), add), codexRunnerPatterns, identitySecret)
	if err != nil {
		return err
	}
	det, _ := p.beginRun(nil, localConnFor(base, p.session.ID), run.Generation, 0)
	det.Run()
	slog.Info("codex process started", "session", p.session.ID, "model", p.slug, "runner", st.PID, "generation", run.Generation)
	if err := p.startThread(); err != nil {
		// The generation never held a conversation, so its directory goes.
		_ = os.RemoveAll(run.Dir())
		return err
	}
	return nil
}

// startHost runs the agent on a Unix host under the runner, so it outlives the
// ssh link and relay-sessions. It is never run in a tmux pane: the drop-in is
// Claude's.
func (p *CodexProvider) startHost(host *sessionstypes.HostSpec) error {
	program := p.cfg.Launcher.Program(host.CodexPath)
	if len(program) == 0 || program[0] == "" {
		return fmt.Errorf("host %q has no codex path: set its codex template's command", host.Name)
	}
	if p.cfg.Resume || p.cfg.AttachOnly {
		ctx, cancel := context.WithTimeout(context.Background(), detachedStartTimeout)
		st, err := DetachedRunState(ctx, host, p.session.ID)
		cancel()
		if err != nil {
			return fmt.Errorf("ask host %q whether the agent runs: %w", host.Name, err)
		}
		if st.Phase == sshhost.RunAlive {
			return p.attachRun(host, hostConnFor(host, p.session.ID), st.Generation)
		}
		if p.cfg.AttachOnly {
			return fmt.Errorf("%w on host %q", ErrNoDetachedRun, host.Name)
		}
	}
	argv := append(slices.Clone(program), "app-server")
	env := map[string]string{"RELAY_SESSION_ID": p.session.ID}
	remote := sshhost.RunnerStartIn(host.OS, p.directory, sshhost.RunDir{Session: p.session.ID}, argv, env, codexRunnerPatterns)

	ctx, cancel := context.WithTimeout(context.Background(), detachedStartTimeout)
	defer cancel()
	stdout, msg, err := runHost(ctx, host, remote, nil)
	if err != nil {
		return fmt.Errorf("start detached codex on host %q: %s (hosts[].detach_sessions: false runs this host's sessions under ssh -T)", host.Name, msg)
	}
	gen, err := sshhost.ParseRunStart(stdout)
	if err != nil {
		return fmt.Errorf("start detached codex on host %q: %w (hosts[].detach_sessions: false runs this host's sessions under ssh -T)", host.Name, err)
	}
	det, _ := p.beginRun(host, hostConnFor(host, p.session.ID), gen, 0)
	det.Run()
	slog.Info("codex detached agent started", "session", p.session.ID, "host", host.ID, "generation", gen)
	return p.startThread()
}

// codexSpawnState is what one runner spawn's Line hook counts.
type codexSpawnState struct {
	// countEndsFrom, when positive, makes the read count the turn endings it
	// delivers from that byte offset on; an attach sets it.
	countEndsFrom int64
	endsSeen      atomic.Int64
}

// beginRun installs the state of a spawn that follows a run from offset and
// marks the provider alive. It starts no goroutine: Run does, so an attach can
// read what the agent wrote while relay was away first.
func (p *CodexProvider) beginRun(host *sessionstypes.HostSpec, connFor func(gen string) runConn, gen string, offset int64) (*runFollower, *codexSpawnState) {
	spawn := &codexSpawnState{}
	p.cmd, p.stdin = nil, nil
	p.exitMu.Lock()
	p.spawnGen++
	spawnGen := p.spawnGen
	p.exitMu.Unlock()
	p.alive.Store(true)
	waitDone := make(chan struct{})
	p.waitDone = waitDone
	p.drainTimeout = providerDrainTimeout
	killed := &atomic.Bool{}
	p.killed = killed
	exit := &spawnExit{done: make(chan struct{})}
	p.exit = exit

	det := newRunFollower(host, connFor, gen, offset, runHooks{
		Kind:    "codex",
		Session: p.session.ID,
		Line: func(body json.RawMessage, start int64) {
			if spawn.countEndsFrom > 0 && start >= spawn.countEndsFrom && bytes.Contains(body, []byte(codexRunnerPatterns.TurnEnded)) {
				spawn.endsSeen.Add(1)
			}
			p.processLine(body)
		},
		Exited: func(code int, tail []string, _ bool) {
			p.runExited(spawnGen, killed, exit, waitDone, code, tail)
		},
		Redact: p.redactTail,
	})
	p.det.Store(det)
	return det, spawn
}

// runExited delivers process_exited the way waitForExit does.
func (p *CodexProvider) runExited(gen uint64, killed *atomic.Bool, exit *spawnExit, waitDone chan struct{}, code int, tail []string) {
	defer close(exit.done)

	p.alive.Store(false)
	exit.code = code
	close(waitDone)

	p.exitMu.Lock()
	defer p.exitMu.Unlock()
	if p.spawnGen != gen {
		slog.Debug("provider exit from a superseded spawn dropped", "session", p.session.ID, "kind", "codex", "exitCode", code)
		return
	}
	slog.Info("codex process exited", "session", p.session.ID, "exitCode", code)
	wasKilled := killed.Load()
	if !wasKilled {
		warnProviderExit(p.session.ID, "codex", code, tail)
	}
	p.handler("process_exited", providerExitData(code, tail, wasKilled))
}

// redactTail masks the launch's secrets in each tail line. The secret is empty
// after a relay restart; the shape-based masks still apply then.
func (p *CodexProvider) redactTail(lines []string) []string {
	secrets := []string{}
	if p.cfg.Identity != nil {
		secrets = append(secrets, p.cfg.Identity.Secret)
	}
	for i, l := range lines {
		lines[i] = redactStderrLine(l, secrets)
	}
	return lines
}

// attachRun follows the agent already running in generation gen. It reads,
// before it returns, what the agent wrote since the saved cursor, so a message
// sent next meets only its own reply. It sends no initialize, initialized,
// thread/start or thread/resume: the agent's connection is already
// initialized, and a second initialize is an error to it. A cursor that is
// absent, from another generation or past the output starts at the end of the
// output.
func (p *CodexProvider) attachRun(host *sessionstypes.HostSpec, connFor func(gen string) runConn, gen string) error {
	ctx, cancel := context.WithTimeout(context.Background(), detachedStartTimeout)
	defer cancel()
	probe, err := connFor(gen).Probe(ctx, codexProbe)
	if err != nil {
		if host == nil {
			return fmt.Errorf("attach to the local agent: %w", err)
		}
		return fmt.Errorf("attach to the agent on host %q: %w", host.Name, err)
	}

	offset := probe.size
	p.stateMu.Lock()
	saved := p.restoredRun
	p.stateMu.Unlock()
	if saved != nil && saved.Generation == gen && saved.Offset >= 0 && saved.Offset <= probe.size {
		offset = saved.Offset
	}

	det, spawn := p.beginRun(host, connFor, gen, offset)
	// A request the run directory already holds an answer for is not answered
	// again when the replay meets it.
	for _, raw := range probe.controls {
		if id := codexAnswerID(raw); id != "" {
			det.Claim(id)
		}
	}
	spawn.countEndsFrom = probe.size
	det.CatchUp(ctx)
	// Endings the read delivered from past the probe end close a turn the probe
	// counted as open; older ones it replayed from the cursor do not count.
	turnOpen := probe.users > probe.results+int(spawn.endsSeen.Load())
	p.session.SetProcessing(turnOpen)
	p.stateMu.Lock()
	p.attachedTurnOpen = turnOpen
	p.stateMu.Unlock()
	det.Run()

	slog.Info("codex detached agent attached", "session", p.session.ID, "where", det.where(), "generation", gen, "cursor", offset)
	return nil
}

// codexIDKey is a JSON-RPC id compacted, so the same id compares equal whatever
// its spacing.
func codexIDKey(id json.RawMessage) string {
	var buf bytes.Buffer
	if len(id) == 0 || string(id) == "null" || json.Compact(&buf, id) != nil {
		return ""
	}
	return buf.String()
}

// codexAnswerID is the id a client line answers: the id of a line that has an
// id and no method. A request or notification relay sent answers nothing.
func codexAnswerID(raw json.RawMessage) string {
	var line struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if json.Unmarshal(raw, &line) != nil || line.Method != "" {
		return ""
	}
	return codexIDKey(line.ID)
}

// startLocalRun starts an agent under the runner on this machine, behind the
// shim when the launch needs one, and returns once runner.json exists. A
// refused or failed start leaves no generation behind. kind names the agent in
// errors and logs; the secrets are masked in what the agent wrote to stderr.
func startLocalRun(kind, base, dir string, shim shimSpec, program, args, env []string, patterns runner.Patterns, secrets ...string) (runner.LocalRun, runner.State, error) {
	run, err := runner.AllocLocal(base, shim.SessionID)
	if err != nil {
		return runner.LocalRun{}, runner.State{}, err
	}
	started := false
	defer func() {
		if !started {
			_ = os.RemoveAll(run.Dir())
		}
	}()

	fds := &spawnFDs{}
	defer fds.closeUnlessStarted()
	var argv []string
	switch {
	case shim.wanted() && shim.Binary == "":
		return runner.LocalRun{}, runner.State{}, ErrShimRequired
	case shim.Identity != nil && shim.BridgeSocket == "":
		return runner.LocalRun{}, runner.State{}, ErrNoBridgeSocket
	case shim.wanted():
		cmd, statusR, extraFiles, berr := buildShimCmd(shim, program[0], append(slices.Clone(program[1:]), args...))
		if berr != nil {
			return runner.LocalRun{}, runner.State{}, berr
		}
		argv, fds.statusR, fds.extraFiles = cmd.Args, statusR, extraFiles
	default:
		if _, err := exec.LookPath(program[0]); err != nil {
			return runner.LocalRun{}, runner.State{}, fmt.Errorf("failed to start %s: %w", kind, err)
		}
		argv = append(slices.Clone(program), args...)
	}

	st, err := run.Start(runner.LocalSpec{Argv: argv, Env: env, Dir: dir, ExtraFiles: fds.extraFiles, Patterns: patterns})
	if err != nil {
		return runner.LocalRun{}, runner.State{}, fmt.Errorf("failed to start %s: %w", kind, err)
	}
	fds.started = true
	// Load-bearing: without closing the parent's own copies, the status pipe
	// (and, when present, the identity secret pipe) never reaches EOF.
	for _, f := range fds.extraFiles {
		_ = f.Close()
	}
	if fds.statusR != nil {
		outcome, _ := readShimStatus(fds.statusR)
		_ = fds.statusR.Close()
		var failure error
		switch {
		case outcome.identityRefused:
			failure = fmt.Errorf("%w: session %s", ErrIdentityRefused, shim.SessionID)
		case !outcome.started || outcome.spawnFailed:
			failure = fmt.Errorf("%w: errno=%d", ErrSpawnFailed, outcome.spawnErrno)
		}
		if failure != nil {
			logRefusedRunStart(kind, shim.SessionID, run, secrets...)
			stopRun(kind, shim.SessionID, "this machine", localConn{run: run})
			return runner.LocalRun{}, runner.State{}, failure
		}
	}
	started = true
	return run, st, nil
}

// logRefusedRunStart logs what the agent wrote to stderr before the shim
// refused it, with the launch's secrets masked.
func logRefusedRunStart(kind, session string, run runner.LocalRun, secrets ...string) {
	tail, err := localConn{run: run}.StderrTail(context.Background())
	if err != nil || tail == "" {
		return
	}
	var sawStdout atomic.Bool
	logProviderStderr(io.NopCloser(strings.NewReader(tail)), session, kind, &sawStdout, secrets...)
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
	if err := p.writeLine(context.Background(), map[string]any{"method": "initialized"}, runner.MessageControl, 0); err != nil {
		return err
	}

	params := map[string]any{
		"cwd":            p.directory,
		"approvalPolicy": "never",
		"sandbox":        "danger-full-access",
	}
	if p.slug != "" {
		params["model"] = p.slug
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

// call sends one control request and waits for its reply, the deadline, or the
// agent's exit.
func (p *CodexProvider) call(method string, params any, deadline <-chan time.Time) (json.RawMessage, error) {
	ch := make(chan codexReply, 1)
	id, err := p.request(context.Background(), method, params, runner.MessageControl, 0, func(r codexReply) { ch <- r })
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
// reader goroutine. The id is a string: a prefix that is this spawn's own, then
// a counter.
func (p *CodexProvider) request(ctx context.Context, method string, params any, kind runner.MessageKind, wait time.Duration, onReply func(codexReply)) (string, error) {
	p.stateMu.Lock()
	id := p.reqPrefix + strconv.FormatInt(p.nextID.Add(1), 10)
	p.pending[id] = onReply
	p.stateMu.Unlock()
	if err := p.writeLine(ctx, map[string]any{"id": id, "method": method, "params": params}, kind, wait); err != nil {
		p.dropPending(id)
		return "", err
	}
	return id, nil
}

func (p *CodexProvider) dropPending(id string) {
	p.stateMu.Lock()
	delete(p.pending, id)
	p.stateMu.Unlock()
}

// writeLine sends one line to the agent. A runner session queues it as kind in
// the runner's inbox and returns once the runner has written it to the agent,
// waiting up to wait for a lost host link to return; a piped session writes it
// to stdin.
func (p *CodexProvider) writeLine(ctx context.Context, v any, kind runner.MessageKind, wait time.Duration) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if conn := p.daemon.Load(); conn != nil {
		if !p.alive.Load() {
			return errors.New("codex process not running")
		}
		return conn.Send(ctx, data)
	}
	if det := p.det.Load(); det != nil {
		if !p.alive.Load() {
			return errors.New("codex process not running")
		}
		return det.Send(ctx, kind, data, wait)
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
func (p *CodexProvider) waitForExit(cmd *exec.Cmd, waitDone chan struct{}, out *spawnOutput, drainTimeout time.Duration, killed *atomic.Bool, exit *spawnExit, gen uint64) {
	defer close(exit.done)
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
	wasKilled := killed.Load()
	if !wasKilled {
		warnProviderExit(p.session.ID, "codex", exitCode, tail)
	}
	data := providerExitData(exitCode, tail, wasKilled)
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
		if p.daemonAttach.Load() && !p.forOwnThread(env.Params) {
			return
		}
		p.translate(env.Method, env.Params, raw)
	default:
		p.warnUnrecognisedOnce("(no method)", "")
		p.handler("raw_output", raw)
	}
}

func (p *CodexProvider) deliverReply(env codexEnvelope) {
	var id string
	if err := json.Unmarshal(env.ID, &id); err != nil {
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

// answerServerRequest replies to every request of a turn relay runs: a request
// left waiting would hang the turn. Approvals are declined because
// approvalPolicy "never" means one only arrives when something outside relay
// forced it. On an attached thread it answers only for the turn relay started;
// a request on a terminal's turn is left for the terminal user.
func (p *CodexProvider) answerServerRequest(env codexEnvelope) {
	if p.daemonAttach.Load() && !p.ownTurnRequest(env) {
		return
	}
	// A request the run directory already shows an answer for, or one this
	// provider answered already, is not answered twice.
	if det := p.det.Load(); det != nil && !det.Claim(codexIDKey(env.ID)) {
		return
	}
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
	if err := p.writeLine(context.Background(), reply, runner.MessageControl, 0); err != nil {
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
		// A daemon-attached thread may be shown as working by its status first.
		alreadyOpen := p.daemonAttach.Load() && p.turnActive
		if !alreadyOpen {
			p.resetTurnState()
			p.turnActive = true
		}
		var ev struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if json.Unmarshal(params, &ev) == nil {
			p.stateMu.Lock()
			if ev.Turn.ID != "" {
				p.turnID = ev.Turn.ID
			}
			// An attach whose saved state lost the thread learns it from the
			// agent's own lines.
			if p.threadID == "" {
				p.threadID = ev.ThreadID
			}
			p.stateMu.Unlock()
		}
		if alreadyOpen {
			return
		}
		p.emitter.SystemInit(p.slug, p.directory, nil, nil)
		p.emitter.MessageStart("")
		if p.daemonAttach.Load() {
			p.handler(handlerTurnStarted, nil)
		}

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
		if p.daemonAttach.Load() && !p.turnActive {
			// The thread's status already ended the turn.
			return
		}
		p.translateTurnCompleted(params)

	case "thread/status/changed", "thread/closed":
		if p.daemonAttach.Load() {
			p.translateDaemonThread(method, params)
		}

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
	p.relayTurn = ""
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
	return p.SendMessageWithin(context.Background(), text, files, DefaultLinkWait)
}

// SendMessageWithin is SendMessage with the longest a runner session on a host
// waits for a lost link to return, ended early when ctx ends. A runner session
// returns once the runner has written the message to the agent.
func (p *CodexProvider) SendMessageWithin(ctx context.Context, text string, files []sessionstypes.FileAttachment, wait time.Duration) error {
	if !p.alive.Load() || (p.stdin == nil && p.det.Load() == nil && p.daemon.Load() == nil) {
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
	_, err := p.request(ctx, "turn/start", map[string]any{
		"threadId": thread,
		"input":    []map[string]string{{"type": "text", "text": text}},
	}, runner.MessageTurn, wait, p.onTurnStartReply)
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
		if p.daemonAttach.Load() {
			p.relayTurn = res.Turn.ID
		}
		p.stateMu.Unlock()
	}
}

func (p *CodexProvider) StopGeneration() {
	det := p.det.Load()
	daemon := p.daemon.Load()
	if !p.alive.Load() || (p.stdin == nil && det == nil && daemon == nil) {
		return
	}
	p.stateMu.Lock()
	thread, turn := p.threadID, p.turnID
	p.stateMu.Unlock()
	if thread == "" || turn == "" {
		return
	}
	_, err := p.request(context.Background(), "turn/interrupt", map[string]any{"threadId": thread, "turnId": turn}, runner.MessageControl, 0, func(r codexReply) {
		if r.err != "" {
			slog.Warn("codex turn/interrupt failed", "session", p.session.ID, "error", r.err)
		}
	})
	if err == nil {
		return
	}
	if det != nil || daemon != nil {
		// Deliberate: an interrupt that did not go does not end the agent. The
		// run keeps its conversation, and the person can stop again.
		slog.Warn("codex interrupt not sent", "session", p.session.ID, "error", err)
		return
	}
	slog.Warn("codex interrupt write failed, killing", "session", p.session.ID, "error", err)
	p.Kill()
}

func (p *CodexProvider) Kill() {
	if conn := p.daemon.Load(); conn != nil {
		p.detachDaemon(conn, false)
		return
	}
	if det := p.det.Load(); det != nil {
		p.killDetached(det, true)
		return
	}
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}
	exit := p.exit
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
		_ = p.cmd.Process.Kill()
		<-p.waitDone
	}
	// waitDone means reaped; the exit is delivered or dropped only once the
	// drain and the handler finish. The handler must not call Kill.
	if exit != nil {
		<-exit.done
	}
	slog.Info("codex process killed", "session", p.session.ID)
}

// killDetached ends the follow loop. stopAgent false leaves the agent running.
func (p *CodexProvider) killDetached(det *runFollower, stopAgent bool) {
	if p.killed != nil {
		p.killed.Store(true)
	}
	p.alive.Store(false)
	det.Stop(stopAgent)
	if p.exit != nil {
		<-p.exit.done
	}
	slog.Info("codex process killed", "session", p.session.ID)
}

// Release detaches this provider from its agent and leaves the agent running:
// the reader stops and process_exited follows, nothing is sent to the agent.
// A relay-sessions shutdown uses it, because a codex agent holds no relay
// credential and outlives relay. Kill, by contrast, ends the agent.
func (p *CodexProvider) Release() {
	if conn := p.daemon.Load(); conn != nil {
		p.detachDaemon(conn, true)
		return
	}
	if det := p.det.Load(); det != nil {
		p.released.Store(true)
		p.killDetached(det, false)
		return
	}
	p.Kill()
}

// Released reports whether Release detached this provider from a running
// agent.
func (p *CodexProvider) Released() bool { return p.released.Load() }

func (p *CodexProvider) Alive() bool { return p.alive.Load() }

// Events returns up to max complete events the agent wrote after the cursor
// and the cursor to pass next. The cursor is "<generation>:<byte offset of a
// line start>"; "" starts at the current generation's beginning.
func (p *CodexProvider) Events(ctx context.Context, after string, max int) ([]json.RawMessage, string, error) {
	det := p.det.Load()
	if det == nil {
		return nil, "", ErrNotDetached
	}
	return det.Events(ctx, after, max)
}

// TakeAttach returns, once, whether the attach found a turn in progress. The
// manager calls it after it publishes the session. Codex holds no request for
// a person, so the func it returns raises nothing.
func (p *CodexProvider) TakeAttach() (turnOpen bool, raise func()) {
	p.stateMu.Lock()
	turnOpen = p.attachedTurnOpen
	p.attachedTurnOpen = false
	p.stateMu.Unlock()
	return turnOpen, func() {}
}

// DeleteSession ends the agent and deletes its run directories, which hold the
// agent's output. Codex's own transcripts stay where Codex keeps them.
func (p *CodexProvider) DeleteSession() error {
	if p.daemonAttach.Load() {
		// The thread and its terminal belong to codex: only this connection ends,
		// and relay holds no run folder for it.
		p.Kill()
		return nil
	}
	if host := p.session.GetHost(); host != nil {
		p.Kill()
		DeleteDetachedAgent("codex", p.session.ID, host)
	} else if p.cfg.RunnerBase != "" {
		p.Kill()
		DeleteLocalAgent("codex", p.cfg.RunnerBase, p.session.ID)
	}
	return nil
}

func (p *CodexProvider) GetState() json.RawMessage {
	p.stateMu.Lock()
	tid := p.threadID
	p.stateMu.Unlock()
	state := map[string]any{"threadId": tid}
	if p.daemonAttach.Load() {
		state["daemonAttach"] = true
	}
	if det := p.det.Load(); det != nil {
		state["detached"] = det.Cursor()
	}
	data, _ := json.Marshal(state)
	return data
}

func (p *CodexProvider) RestoreState(state json.RawMessage) {
	if state == nil {
		return
	}
	var s struct {
		ThreadID     string          `json:"threadId"`
		Detached     *detachedCursor `json:"detached"`
		DaemonAttach bool            `json:"daemonAttach"`
	}
	if err := json.Unmarshal(state, &s); err == nil {
		p.daemonAttach.Store(s.DaemonAttach)
		p.stateMu.Lock()
		p.threadID = s.ThreadID
		p.restoredRun = s.Detached
		p.stateMu.Unlock()
	}
}
