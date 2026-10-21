package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/barelyworkingcode/relay/internal/membership"
	"github.com/barelyworkingcode/relay/internal/sessions/runner"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
	"github.com/barelyworkingcode/relay/internal/sshhost"
)

// A detached Claude session keeps its agent under the runner (docs/ssh-hosts.md,
// "Detached sessions"). runFollower reads it and carries input to it; this file
// holds what is Claude's own: its patterns, its starts, its permission prompts.

// claudeProbe is what Claude's attach probe counts.
var claudeProbe = probeSpec{TurnEnded: claudeRunnerPatterns.TurnEnded, Request: `"control_request"`}

// claudeRunnerPatterns are the strings the runner matches in Claude's stdout:
// the result line ends a turn, and a can_use_tool request is a permission
// prompt.
var claudeRunnerPatterns = runner.Patterns{
	TurnEnded: `"type":"result"`,
	Asking:    `"subtype":"can_use_tool"`,
}

func (p *ClaudeProvider) startDetached(host *sessionstypes.HostSpec, args []string) error {
	if sshhost.IsWindowsOS(host.OS) {
		return fmt.Errorf("host %q is a Windows host: detach_sessions is not available there", host.Name)
	}
	if len(host.SSHArgv) == 0 {
		return fmt.Errorf("host %q has no ssh_argv", host.Name)
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
		if st.Phase == sshhost.RunLegacy {
			// A run from before the message runner has no inbox to send to: it
			// is stopped once and the session resumes on its conversation.
			stopRun("claude", p.session.ID, fmt.Sprintf("host %q", host.Name), hostConnFor(host, p.session.ID)(st.Generation))
		}
		if p.cfg.AttachOnly {
			return fmt.Errorf("%w on host %q", ErrNoDetachedRun, host.Name)
		}
	}
	program := p.cfg.Launcher.Program(host.ClaudePath)
	env := map[string]string{"RELAY_SESSION_ID": p.session.ID}
	argv := append(slices.Clone(program), args...)
	run := sshhost.RunDir{Session: p.session.ID}
	remote := sshhost.RunnerStartIn(host.OS, p.directory, run, argv, env, claudeRunnerPatterns)
	if host.TmuxPath != "" {
		remote = sshhost.RunnerStartInPane(host.OS, p.directory, run, argv, env, claudeRunnerPatterns, host.TmuxPath, sshhost.AgentPaneName(p.session.ID))
	}

	ctx, cancel := context.WithTimeout(context.Background(), detachedStartTimeout)
	defer cancel()
	stdout, msg, err := runHost(ctx, host, remote, nil)
	if err != nil {
		return fmt.Errorf("start detached claude on host %q: %s (hosts[].detach_sessions: false runs this host's sessions under ssh -T)", host.Name, msg)
	}
	gen, err := sshhost.ParseRunStart(stdout)
	if err != nil {
		return fmt.Errorf("start detached claude on host %q: %w (hosts[].detach_sessions: false runs this host's sessions under ssh -T)", host.Name, err)
	}

	p.mu.Lock()
	p.restoredRun = nil
	p.mu.Unlock()
	det, _ := p.beginDetached(host, hostConnFor(host, p.session.ID), gen, 0)
	p.runDetached(det)
	slog.Info("claude detached agent started", "session", p.session.ID, "host", host.ID, "generation", gen)
	return nil
}

// beginDetached installs the state of a spawn that follows a run from offset and
// marks the provider alive. It starts no goroutine: runDetached does, so an
// attach can read what the agent wrote while relay was away first.
func (p *ClaudeProvider) beginDetached(host *sessionstypes.HostSpec, connFor func(gen string) runConn, gen string, offset int64) (*runFollower, *claudeSpawnState) {
	spawn := &claudeSpawnState{}
	p.cmd, p.stdin, p.targetPID = nil, nil, 0
	p.holdMu.Lock()
	p.holding, p.held = false, nil
	p.holdMu.Unlock()
	p.exitMu.Lock()
	p.spawnGen++
	spawnGen := p.spawnGen
	p.exitMu.Unlock()
	p.alive.Store(true)
	p.stopIdle = make(chan struct{})
	p.stopIdleOnce = sync.Once{}
	p.waitDone = make(chan struct{})
	p.drainTimeout = providerDrainTimeout
	killed := &atomic.Bool{}
	p.killed = killed
	exit := &spawnExit{done: make(chan struct{})}
	p.exit = exit
	p.touchActivity()

	det := newRunFollower(host, connFor, gen, offset, runHooks{
		Kind:    "claude",
		Session: p.session.ID,
		Line: func(body json.RawMessage, start int64) {
			if spawn.countResultsFrom > 0 && start >= spawn.countResultsFrom && bytes.Contains(body, []byte(`"type":"result"`)) {
				spawn.resultsSeen.Add(1)
			}
			spawn.sawStdout.Store(true)
			p.processLine(body, spawn)
		},
		Exited: func(code int, tail []string, stopped bool) {
			p.claudeExited(spawnGen, killed, exit, code, tail)
		},
		Redact: p.redactTail,
	})
	p.det = det
	return det, spawn
}

func (p *ClaudeProvider) runDetached(det *runFollower) {
	det.Run()
	go p.idleWatcher(p.stopIdle)
}

// claudeExited delivers process_exited the way waitForExit does.
func (p *ClaudeProvider) claudeExited(gen uint64, killed *atomic.Bool, exit *spawnExit, code int, tail []string) {
	defer close(exit.done)

	p.alive.Store(false)
	exit.code = code

	p.exitMu.Lock()
	defer p.exitMu.Unlock()
	if p.spawnGen != gen {
		slog.Debug("provider exit from a superseded spawn dropped", "session", p.session.ID, "kind", "claude", "exitCode", code)
		return
	}
	slog.Info("claude process exited", "session", p.session.ID, "exitCode", code)
	wasKilled := killed.Load()
	if !wasKilled {
		warnProviderExit(p.session.ID, "claude", code, tail)
	}
	p.handler("process_exited", providerExitData(code, tail, wasKilled))
}

// redactTail masks the launch's secrets in each tail line. The secrets are
// empty after a relay restart; the shape-based masks still apply then.
func (p *ClaudeProvider) redactTail(lines []string) []string {
	secrets := []string{p.identitySecret(), p.cfg.ClaudeToken}
	for i, l := range lines {
		lines[i] = redactStderrLine(l, secrets)
	}
	return lines
}

// claimRequest reports whether the control request in raw has not been
// answered or raised before, and marks it as handled.
func claimRequest(det *runFollower, raw json.RawMessage) bool {
	return det.Claim(controlRequestID(raw))
}

// deferRaise holds raw for a later raise and reports whether it did.
func (p *ClaudeProvider) deferRaise(raw json.RawMessage) bool {
	p.holdMu.Lock()
	defer p.holdMu.Unlock()
	if !p.holding {
		return false
	}
	p.held = append(p.held, raw)
	return true
}

// endDeferring stops holding requests and returns the ones held.
func (p *ClaudeProvider) endDeferring() []json.RawMessage {
	p.holdMu.Lock()
	defer p.holdMu.Unlock()
	p.holding = false
	held := p.held
	p.held = nil
	return held
}

// controlRequestID is the request id of a control_request or control_response
// line, compacted so the same id compares equal whatever its spacing.
func controlRequestID(raw json.RawMessage) string {
	var line struct {
		RequestID json.RawMessage `json:"request_id"`
		Response  struct {
			RequestID json.RawMessage `json:"request_id"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &line) != nil {
		return ""
	}
	id := line.RequestID
	if len(id) == 0 {
		id = line.Response.RequestID
	}
	var buf bytes.Buffer
	if len(id) == 0 || json.Compact(&buf, id) != nil {
		return ""
	}
	return buf.String()
}

// sendDetached queues one line (data ends in a newline) in the runner's inbox
// and returns once the runner has written it to the agent.
func (p *ClaudeProvider) sendDetached(callerCtx context.Context, det *runFollower, data []byte, wait time.Duration) error {
	line := bytes.TrimSuffix(data, []byte("\n"))
	// A control_response answers a request and starts no turn.
	kind := runner.MessageTurn
	if lineType(string(line)) == "control_response" {
		kind = runner.MessageControl
	}
	return det.Send(callerCtx, kind, line, wait)
}

// Release detaches this provider from its agent and leaves the agent running
// on the host: the local reader stops and process_exited follows, nothing is
// sent to the host. A relay or relay-sessions shutdown uses it, because a
// detached agent outlives relay. Kill, by contrast, ends the agent.
func (p *ClaudeProvider) Release() {
	p.root.Store(nil)
	if det := p.det; det != nil {
		p.released.Store(true)
		// Deliberate: no denial of waiting permission prompts. The agent is
		// still blocked on each, and the next attach raises it again.
		p.killDetached(det, false)
		return
	}
	p.Kill()
}

// Released reports whether Release detached this provider from a running
// agent.
func (p *ClaudeProvider) Released() bool { return p.released.Load() }

func (p *ClaudeProvider) killDetached(det *runFollower, stopAgent bool) {
	if p.killed != nil {
		p.killed.Store(true)
	}
	p.alive.Store(false)
	if p.stopIdle != nil {
		p.stopIdleOnce.Do(func() { close(p.stopIdle) })
	}
	det.Stop(stopAgent)
	if p.exit != nil {
		<-p.exit.done
	}
	slog.Info("claude process killed", "session", p.session.ID)
}

// removeDetachedRun ends the agent and deletes its run directories, which
// hold tool output.
func (p *ClaudeProvider) removeDetachedRun(host *sessionstypes.HostSpec) {
	p.Kill()
	DeleteDetachedRun(p.session.ID, host)
}

// Events returns up to max complete events the agent wrote after the cursor
// and the cursor to pass next. The cursor is "<generation>:<byte offset of a
// line start>"; "" starts at the current generation's beginning.
func (p *ClaudeProvider) Events(ctx context.Context, after string, max int) (lines []json.RawMessage, next string, err error) {
	det := p.det
	if det == nil {
		return nil, "", ErrNotDetached
	}
	return det.Events(ctx, after, max)
}

// TakeAttach returns, once, whether the attach found a turn in progress and
// a func that raises the permission requests nobody answered. The manager
// calls it after it publishes the session: the attention board and the
// viewers can show a prompt only then. It returns nil for a provider that did
// not attach.
func (p *ClaudeProvider) TakeAttach() (turnOpen bool, raise func()) {
	p.mu.Lock()
	turnOpen = p.attachedTurnOpen
	p.attachedTurnOpen = false
	det := p.det
	p.mu.Unlock()
	// Deliberate: the hold ends here, not when the attach returns. A request
	// the follow loop delivers before the manager has published the session
	// joins the held ones and is raised with the session in place.
	var reqs []json.RawMessage
	if det != nil {
		reqs = p.endDeferring()
	}
	return turnOpen, func() {
		for _, raw := range reqs {
			p.handleControlRequest(raw)
		}
	}
}

func lineType(line string) string {
	var env struct {
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(line), &env) != nil {
		return ""
	}
	return env.Type
}

// attachRun follows the agent already running in generation gen. It
// reads, before it returns, what the agent wrote since the saved cursor, so a
// message sent next meets only its own reply, and it raises the permission
// requests nobody answered: the agent is blocked on each. A cursor that is
// absent, from another generation or past the output starts at the end of the
// output.
func (p *ClaudeProvider) attachRun(host *sessionstypes.HostSpec, connFor func(gen string) runConn, gen string) error {
	ctx, cancel := context.WithTimeout(context.Background(), detachedStartTimeout)
	defer cancel()
	probe, err := connFor(gen).Probe(ctx, claudeProbe)
	if err != nil {
		if host == nil {
			return fmt.Errorf("attach to the local agent: %w", err)
		}
		return fmt.Errorf("attach to the agent on host %q: %w", host.Name, err)
	}

	offset := probe.size
	p.mu.Lock()
	saved := p.restoredRun
	p.mu.Unlock()
	if saved != nil && saved.Generation == gen && saved.Offset >= 0 && saved.Offset <= probe.size {
		offset = saved.Offset
	}

	det, spawn := p.beginDetached(host, connFor, gen, offset)
	for _, raw := range probe.controls {
		if lineType(string(raw)) == "control_response" {
			if id := controlRequestID(raw); id != "" {
				det.Claim(id)
			}
		}
	}
	p.holdMu.Lock()
	p.holding = true
	p.holdMu.Unlock()
	spawn.countResultsFrom = probe.size
	for _, raw := range probe.requests {
		if lineType(string(raw)) == "control_request" && claimRequest(det, raw) {
			p.holdMu.Lock()
			p.held = append(p.held, raw)
			p.holdMu.Unlock()
		}
	}
	det.CatchUp(ctx)
	// Results the read delivered from past the probe end a turn the probe
	// counted as open; older ones it replayed from the cursor do not count.
	turnOpen := probe.users > probe.results+int(spawn.resultsSeen.Load())
	p.session.SetProcessing(turnOpen)
	p.mu.Lock()
	p.attachedTurnOpen = turnOpen
	p.mu.Unlock()
	p.runDetached(det)

	slog.Info("claude detached agent attached", "session", p.session.ID, "where", det.where(), "generation", gen, "cursor", offset)
	return nil
}

// startLocal runs the agent on this machine under the runner, so it outlives
// relay-sessions. A resume or attach finds a runner that still lives first and
// follows it: a second agent must never share the conversation.
func (p *ClaudeProvider) startLocal(spawn *claudeSpawnState) error {
	base := p.cfg.RunnerBase
	if base == "" {
		return errors.New("claude: no runner directory is configured for local sessions")
	}
	if p.cfg.Resume || p.cfg.AttachOnly {
		st, err := runner.LocalStatus(base, p.session.ID, "")
		if err != nil {
			return fmt.Errorf("read the local runner state: %w", err)
		}
		if st.Phase == runner.PhaseAlive {
			return p.attachLocal(st)
		}
		if p.cfg.AttachOnly {
			return fmt.Errorf("%w on this machine", ErrNoDetachedRun)
		}
	}
	if err := p.ensureHookConfig(); err != nil {
		slog.Warn("claude: hook config write failed", "session", p.session.ID, "directory", p.directory, "error", err)
	}

	run, err := runner.AllocLocal(base, p.session.ID)
	if err != nil {
		return err
	}
	started := false
	defer func() {
		if !started {
			_ = os.RemoveAll(run.Dir())
		}
	}()

	// Deliberate: a sandboxed agent reads these files from inside its sandbox,
	// and no profile grants the runner directory, so its files stay in the temp
	// directory the profile does grant. An unsandboxed agent keeps them in its
	// run directory, which removes them with the session.
	fileDir, tracked := run.Dir(), false
	if p.cfg.SandboxProfile != "" {
		fileDir, tracked = os.TempDir(), true
	}
	writeFile := func(pattern string, data []byte) (string, error) {
		path, err := writeSpawnFile(fileDir, pattern, data)
		if err == nil && tracked {
			p.addSpawnFile(path)
		}
		return path, err
	}
	var mcpConfigPath, sysPromptPath string
	if cfg := p.relayMCPConfig(); cfg != nil {
		data, err := json.Marshal(cfg)
		if err != nil {
			return fmt.Errorf("marshal mcp config: %w", err)
		}
		if mcpConfigPath, err = writeFile("claude-mcp-*.json", data); err != nil {
			return fmt.Errorf("write mcp config: %w", err)
		}
		spawn.relayMCPWritten = true
	} else if useRelayTools(p.session.Settings) {
		warnRelayToolsUnavailable(p.session.ID, "claude", "relay_mcp_command_unset")
	}
	if p.session.SystemPrompt != "" {
		var err error
		if sysPromptPath, err = writeFile("claude-sysprompt-*.txt", []byte(p.session.SystemPrompt)); err != nil {
			return fmt.Errorf("write system prompt: %w", err)
		}
	}

	args := p.buildClaudeArgs(mcpConfigPath, sysPromptPath)
	program := p.cfg.Launcher.Program(resolveClaudePath(p.cfg.Binary))
	shimmed := shimSpec{
		Binary:         p.cfg.ShimBinary,
		SessionID:      p.session.ID,
		BridgeSocket:   p.cfg.BridgeSocket,
		SandboxProfile: p.cfg.SandboxProfile,
		Identity:       p.cfg.Identity,
		ClaudeToken:    p.cfg.ClaudeToken,
	}
	fds := &spawnFDs{}
	defer fds.closeUnlessStarted()
	var argv []string
	switch {
	case shimmed.ClaudeToken != "" && !shimmed.wanted():
		return errors.New("claude token requires the shim")
	case shimmed.wanted() && shimmed.Binary == "":
		return ErrShimRequired
	case shimmed.Identity != nil && shimmed.BridgeSocket == "":
		return ErrNoBridgeSocket
	case shimmed.wanted():
		cmd, statusR, extraFiles, berr := buildShimCmd(shimmed, program[0], append(slices.Clone(program[1:]), args...))
		if berr != nil {
			return berr
		}
		argv, fds.statusR, fds.extraFiles = cmd.Args, statusR, extraFiles
	default:
		if _, err := exec.LookPath(program[0]); err != nil {
			return fmt.Errorf("failed to start claude: %w", err)
		}
		argv = append(slices.Clone(program), args...)
	}
	env, err := p.buildClaudeEnv(ensurePath(childBaseEnv()))
	if err != nil {
		return err
	}

	st, err := run.Start(runner.LocalSpec{Argv: argv, Env: env, Dir: p.directory, ExtraFiles: fds.extraFiles, Patterns: claudeRunnerPatterns})
	if err != nil {
		return fmt.Errorf("failed to start claude: %w", err)
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
			failure = fmt.Errorf("%w: session %s", ErrIdentityRefused, p.session.ID)
		case !outcome.started || outcome.spawnFailed:
			failure = fmt.Errorf("%w: errno=%d", ErrSpawnFailed, outcome.spawnErrno)
		}
		if failure != nil {
			p.logRefusedStart(run)
			stopRun("claude", p.session.ID, "this machine", localConn{run: run})
			return failure
		}
	}
	started = true

	root := p.pinRoot(st.PID) // st.PID is the pid cmd.Start returned
	p.mu.Lock()
	p.restoredRun, p.runnerRoot = nil, root
	p.mu.Unlock()
	p.root.Store(root)
	det, state := p.beginDetached(nil, localConnFor(base, p.session.ID), run.Generation, 0)
	state.relayMCPWritten = spawn.relayMCPWritten
	p.runDetached(det)
	slog.Info("claude process started", "session", p.session.ID, "model", p.model, "runner", st.PID, "generation", run.Generation)
	return nil
}

// logRefusedStart logs what the agent wrote to stderr before the shim refused
// it, with the launch's secrets masked.
func (p *ClaudeProvider) logRefusedStart(run runner.LocalRun) {
	tail, err := localConn{run: run}.StderrTail(context.Background())
	if err != nil || tail == "" {
		return
	}
	var sawStdout atomic.Bool
	logProviderStderr(io.NopCloser(strings.NewReader(tail)), p.session.ID, "claude", &sawStdout, p.identitySecret(), p.cfg.ClaudeToken)
}

// pinRoot pins pid's start time as the permission-hook root: the runner is the
// ancestor of the shim and every process under it. It returns nil when the
// start time is unreadable, and the hook then denies.
func (p *ClaudeProvider) pinRoot(pid int) *sessionstypes.ProcessRoot {
	info, ok := membership.NewSource().Info(pid)
	if !ok {
		slog.Warn("claude: process start time unreadable; session has no process root", "session", p.session.ID, "pid", pid)
		return nil
	}
	return &sessionstypes.ProcessRoot{PID: pid, StartSec: info.StartSec, StartUsec: info.StartUsec}
}

// attachLocal follows the runner that still lives. It records the process root
// only when runner.json's pid and the kernel's start time of that pid both
// match the root saved when the runner started: a recycled pid, or a runner
// nobody saved, leaves the session without a root and the hook denies.
func (p *ClaudeProvider) attachLocal(st runner.Status) error {
	p.mu.Lock()
	saved := p.restoredRoot
	p.mu.Unlock()
	var root *sessionstypes.ProcessRoot
	if saved != nil && saved.PID == st.State.PID {
		if pinned := p.pinRoot(st.State.PID); pinned != nil && *pinned == *saved {
			root = pinned
		}
	}
	if root == nil {
		slog.Warn("claude: local runner does not match the saved process root; session has no process root", "session", p.session.ID, "runner", st.State.PID)
	}
	p.mu.Lock()
	p.runnerRoot = root
	p.mu.Unlock()
	p.root.Store(root)
	if err := p.attachRun(nil, localConnFor(p.cfg.RunnerBase, p.session.ID), st.Generation); err != nil {
		p.root.Store(nil)
		return err
	}
	return nil
}

// SendEvent tells the runner something happened that the agent's output does
// not show. Only "asking" is defined, and a host session answers nil: its
// asks arrive on stdout.
func (p *ClaudeProvider) SendEvent(ctx context.Context, typ string) error {
	det := p.det
	if det == nil {
		return nil
	}
	return det.conn.SendEvent(ctx, typ)
}
