package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/attention"
	"github.com/barelyworkingcode/relay/internal/sessions/clock"
	"github.com/barelyworkingcode/relay/internal/sessions/events"
	sessionsmcp "github.com/barelyworkingcode/relay/internal/sessions/mcp"
	"github.com/barelyworkingcode/relay/internal/sessions/permission"
	"github.com/barelyworkingcode/relay/internal/sessions/provider"
	"github.com/barelyworkingcode/relay/internal/sessions/runner"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
	"github.com/barelyworkingcode/relay/internal/sshhost"
)

// Session kinds this manager can actually spawn a provider for. Mirrors
// cmd/relay/session_launch.go's KindClaude/KindPi/KindCodex/KindChat wire values
// (that package cannot be imported from here — cmd depends on internal,
// never the other way — so the strings are the shared contract, not a Go
// symbol). A kind outside this set is refused explicitly by Create rather
// than silently mis-routed, the same choice hostapi's own handleLaunch
// already makes for pty (launch.go's own doc comment).
const (
	KindClaude = "claude"
	KindPi     = "pi"
	KindCodex  = "codex"
	KindChat   = "chat"
)

var (
	// ErrSessionExists is returned by Create for a duplicate, still-live
	// session id.
	ErrSessionExists = errors.New("session: session already exists")
	// ErrSessionNotFound is returned when no in-memory or persisted session
	// answers to the given id.
	ErrSessionNotFound = errors.New("session: not found")
	// ErrAlreadyProcessing is returned by SendMessage when a generation is
	// already in flight for the session.
	ErrAlreadyProcessing = errors.New("session: already processing a message")
	// ErrResumeRequired is SH-6's replacement for relayLLM's silent
	// host-driven respawn: a project-bound session whose provider process
	// is not running answers send_message/join_session with this instead
	// of restarting anything. Only a caller-driven resume (Create with
	// CreateSpec.Resume) brings it back.
	ErrResumeRequired = errors.New("session: provider not running; resume required")
	// ErrNotTracked is returned by Wait for a session that reports no agent
	// state.
	ErrNotTracked = errors.New("session: no agent state is tracked for this session")
	// ErrReplayUnavailable is returned by Events for a session whose agent
	// does not keep a replayable event log: only a detached host session does.
	ErrReplayUnavailable = errors.New("session: events cannot be replayed for this session")
)

// Config is what Manager needs to spawn claude/pi providers: the base
// per-kind provider config (socket paths, binaries — everything static
// across every session this host spawns) plus the clock tests substitute.
// Claude.Binary/Pi.Binary etc. are resolved by the caller (cmd/relaysessions,
// a later wiring unit), never read back out of this process's own
// environment — see internal/sessions/provider's own doc comment.
type Config struct {
	Claude provider.ClaudeConfig
	Pi     provider.PiConfig
	Codex  provider.CodexConfig
	Chat   provider.ChatConfig
	Clock  clock.Clock
}

func (c Config) clockOrDefault() clock.Clock {
	if c.Clock == nil {
		return clock.DefaultClock
	}
	return c.Clock
}

// sessionSlot mirrors internal/sessions/terminal/manager.go's own type of
// the same name and purpose: the existence check and the reservation of a
// session id happen under the same lock acquisition (Create), so two
// concurrent Creates for the same id can never both pass the check and both
// spawn a live, untracked provider process. A slot with sess == nil and
// launching == true is a live reservation, not an empty entry — any other
// path that touches m.slots[id] (Get's lazy load included) must wait for it
// or fail closed, never overwrite it with an unrelated *sessionSlot.
type sessionSlot struct {
	launching bool
	stopping  bool
	done      chan struct{}
	sess      *sessionstypes.Session // nil until Create's spawn resolves

	// spec is the CreateSpec that produced sess — the authorizing identity
	// (ModelKey included) this slot's provider was actually launched with.
	// A manager-internal restart (ClearSession, SendMessage's ad-hoc
	// respawn) reuses it via respawnSpec rather than building a spec from
	// nothing, so a respawned process never loses the key it was launched
	// with. Zero value for a slot Get filled from a lazy disk load — this
	// process never authorized that session's launch, so it never restarts.
	spec CreateSpec

	// held is true while a terminal owns the session's conversation. Guarded
	// by Manager.mu.
	held bool
}

// Manager owns the set of sessions this host is hosting: live ones with a
// running provider process, and ones this process merely knows about
// because Get lazy-loaded them from disk (relayLLM's own GetSession
// behavior, ported unchanged — a session surviving a host restart must
// still answer join_session with its history, even with no live provider).
type Manager struct {
	mu    sync.Mutex
	slots map[string]*sessionSlot

	store *Store
	perms *permission.PermissionManager
	cfg   Config

	sink sessionstypes.EventSink

	attn     *attention.Board
	attnSink *sinkAdapter
	watch    *turnWatch

	// onExit mirrors internal/sessions/terminal.Manager's own field of the
	// same name and purpose: a later unit's SessionExited bridge hook, fired
	// from handleProviderEvent's "process_exited" case, usually on the
	// provider's own waitForExit goroutine (see SetExitHandler).
	onInterrupt func(context.Context, string) error
	onExit      func(id string, exitCode int)

	tools toolBoard

	collMu     sync.Mutex
	collectors map[string]*ResponseCollector

	// reattaching holds one mutex per session id, so the startup reattach and
	// a message that arrives meanwhile attach the same agent once.
	reattachMu  sync.Mutex
	reattaching map[string]*sync.Mutex

	// onRebind is called once after each attach of a local runner session whose
	// provider reports a process root (SetRebindHandler).
	onRebind func(sessionID string, runner sessionstypes.ProcessRoot)

	// askedMu guards asked, the sessions whose pending permissions are above
	// zero, so only the rise from zero tells the runner.
	askedMu sync.Mutex
	asked   map[string]bool

	// providerFactory, when non-nil, replaces the built-in claude/pi switch
	// in startProvider. Test-only seam, mirroring relayLLM's own
	// SessionManager.SetProviderFactory — that package's doc comment names
	// it as "the hermetic tier's only test seam" for provider construction,
	// a deliberate, narrow substitute for spawning a real claude/pi binary
	// in every test, not a broader DI rewrite. Production never sets this.
	providerFactory func(sess *sessionstypes.Session, spec CreateSpec, handler sessionstypes.EventHandler) (sessionstypes.Provider, error)
}

// SetProviderFactory installs f as described on the providerFactory field.
func (m *Manager) SetProviderFactory(f func(sess *sessionstypes.Session, spec CreateSpec, handler sessionstypes.EventHandler) (sessionstypes.Provider, error)) {
	m.mu.Lock()
	m.providerFactory = f
	m.mu.Unlock()
}

func (m *Manager) getProviderFactory() func(*sessionstypes.Session, CreateSpec, sessionstypes.EventHandler) (sessionstypes.Provider, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.providerFactory
}

// NewManager constructs a Manager. perms may be nil for a manager that never
// spawns a claude session with host-control_request permissions (tests, a
// pi-only deployment).
func NewManager(cfg Config, store *Store, perms *permission.PermissionManager) *Manager {
	adapter := &sinkAdapter{}
	watch := newTurnWatch(adapter)
	m := &Manager{
		slots:      make(map[string]*sessionSlot),
		store:      store,
		perms:      perms,
		cfg:        cfg,
		collectors: make(map[string]*ResponseCollector),
		attn:       attention.NewBoard(cfg.clockOrDefault(), watch),
		attnSink:   adapter,
		watch:      watch,
	}
	if perms != nil {
		perms.SetPendingObserver(m.permissionPending)
	}
	return m
}

// SetEventSink installs the sink every provider event and lifecycle
// notification is forwarded to (typically the WS hub's SessionHandlers).
func (m *Manager) SetEventSink(sink sessionstypes.EventSink) {
	m.mu.Lock()
	m.sink = sink
	m.mu.Unlock()
}

func (m *Manager) eventSink() sessionstypes.EventSink {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sink
}

// SetExitHandler installs fn to be called whenever a provider this manager
// owns reports its process has exited — mirrors
// internal/sessions/terminal.Manager.SetExitHandler's signature. For
// claude/pi, handleProviderEvent's "process_exited" case runs on the
// provider's own waitForExit goroutine, except when a claude permission-mode
// restart fails: that restart reports the killed spawn's exit on the
// caller's goroutine. ChatProvider has no OS process to wait on, so its
// Kill invokes fn synchronously on the caller's own goroutine too — fn
// must tolerate either. Kill returns only after fn does, so fn must not call
// Kill on that provider or wait on anything a Kill caller may hold.
func (m *Manager) SetExitHandler(fn func(id string, exitCode int)) {
	m.mu.Lock()
	m.onExit = fn
	m.mu.Unlock()
}

var ErrInterruptAuditUnavailable = errors.New("interrupt audit unavailable")

// SetInterruptHandler records permission to deliver an adopted Pi or attached
// codex interrupt.
func (m *Manager) SetInterruptHandler(fn func(context.Context, string) error) {
	m.mu.Lock()
	m.onInterrupt = fn
	m.mu.Unlock()
}

// SetRebindHandler installs fn. The manager calls fn once after each successful
// attach of a local runner session whose ProcessRoot() is set.
func (m *Manager) SetRebindHandler(fn func(sessionID string, runner sessionstypes.ProcessRoot)) {
	m.mu.Lock()
	m.onRebind = fn
	m.mu.Unlock()
}

func (m *Manager) rebindHandler() func(sessionID string, runner sessionstypes.ProcessRoot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.onRebind
}

// claimRebind hands a freshly attached local session's runner to the rebind
// handler. It runs with the reattach lock held, so a boot attach and a lazy
// attach of the same session claim once each, not at once.
func (m *Manager) claimRebind(id string) {
	fn := m.rebindHandler()
	if fn == nil {
		return
	}
	sess, ok := m.Get(id)
	if !ok {
		return
	}
	p, ok := liveProvider(sess)
	if !ok {
		return
	}
	reporter, ok := p.(sessionstypes.RootReporter)
	if !ok {
		return
	}
	if root, ok := reporter.ProcessRoot(); ok {
		fn(id, root)
	}
}

func (m *Manager) exitHandler() func(id string, exitCode int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.onExit
}

// CreateSpec is what a caller supplies to start or resume one session. It is
// the claude/pi-shaped subset of hostapi.LaunchRequest plus the
// eveSessionRequestBody fields cmd/relay/session_launch.go's SessionRequest
// carries — already resolved and authorized by the caller (relay), exactly
// as internal/sessions/terminal.CreateSpec's own doc comment describes for
// pty templates.
type CreateSpec struct {
	SessionID string
	ProjectID string
	Kind      string
	Directory string
	Name      string
	Model     string
	// Origin is who asked for the session, empty for the person.
	Origin string
	// Unsandboxed marks a claude session with no project and no sandbox. The
	// host saves it with the session and lists it; hostapi has already checked
	// it fits no other shape.
	Unsandboxed bool

	Settings       json.RawMessage
	SystemPrompt   string
	AppendClaudeMd bool

	Host *sessionstypes.HostSpec

	// ModelKey is C8's per-launch-or-resume model key (pi and chat only;
	// empty disables pi's relay-model overlay / chat's own model broker
	// auth). Never cached across calls: every Create spawns a brand new
	// PiProvider/ChatProvider with exactly the ModelKey this call supplied,
	// so a resumed session's process is never handed a stale key left over
	// from before it died.
	ModelKey string

	// Identity is this launch's own project_session secret (chat only
	// today: its MCP tool child is the only provider-spawned process this
	// package hands off to the shim). nil for a launch with no identity to
	// mint (ad-hoc, SSH-hosted). Never cached across calls, same rule as
	// ModelKey.
	Identity *sessionsmcp.IdentitySpec
	// ClaudeToken is this launch's Claude long-term OAuth token (claude only;
	// sandboxed local launches). Never cached across calls, same rule as
	// Identity; never logged.
	ClaudeToken string
	// ClaudeEnv is this launch's claude_env_passthrough values (claude only).
	ClaudeEnv map[string]string
	// SandboxProfile is this launch's own absolute SBPL profile path (C7),
	// threaded to chat's MCP tool child. "" runs it unsandboxed.
	SandboxProfile string

	// Launcher replaces the program a claude, codex or pi session runs; nil
	// runs the kind's own binary. Relay sends it with every launch of an agent
	// entry that names a command, resumes included.
	Launcher *sessionstypes.Launcher
	// Entry names the project agent entry the session was started from. A
	// claude or codex entry with no model runs on the binary's own default.
	Entry string

	// Resume is true for relay's POST /launch resume:true (SH §3.4 driven
	// by relay's own POST /api/sessions/{id}/resume, never by this host):
	// reattach the persisted session (history, provider state) under a
	// freshly spawned provider process, rather than starting empty.
	Resume bool
	// AttachOnly makes a resume of a detached host session attach to its
	// running agent and fail when none runs, where a plain resume would start
	// one.
	AttachOnly bool

	// ResumeConversation seeds a new claude session's provider state with an
	// existing Claude conversation id, so the first start resumes it.
	ResumeConversation string
	ResumeTranscript   string
	// AttachCodexThread seeds a new codex session's provider state with a thread
	// the codex daemon holds, so the first start attaches to it instead of
	// running a codex of its own.
	AttachCodexThread string
	// AdoptHost makes the start of a detached host claude session the end of
	// an adopt: the host script checks and stops the outside process, then
	// starts the agent.
	AdoptHost *sshhost.AdoptPins
}

func (m *Manager) providerKindSupported(kind string) bool {
	return kind == KindClaude || kind == KindPi || kind == KindCodex || kind == KindChat
}

// Create starts (or, with CreateSpec.Resume, reattaches) one session.
//
// The existence check, the resume-already-live short-circuit, and the slot
// reservation all happen under one lock acquisition, then the actual spawn
// (provider.Start, which execs a child process) runs unlocked — mirroring
// internal/sessions/terminal.Manager.Create exactly, for the same two
// reasons: two concurrent Creates for the same id can never both spawn a
// live untracked process, and nothing here blocks on a locked mutex while a
// provider's own Start/Kill (each doing real, potentially slow process I/O)
// is in flight.
func (m *Manager) Create(spec CreateSpec) (*sessionstypes.Session, error) {
	if spec.SessionID == "" {
		return nil, errors.New("session: session id is required")
	}
	if m.getProviderFactory() == nil && !m.providerKindSupported(spec.Kind) {
		return nil, fmt.Errorf("session: kind %q has no provider wired (claude/pi/codex/chat only)", spec.Kind)
	}
	if err := m.checkAttachResume(spec); err != nil {
		return nil, err
	}

	m.mu.Lock()
	existing, exists := m.slots[spec.SessionID]
	var relaunch sessionstypes.Provider
	if exists {
		if !spec.Resume {
			m.mu.Unlock()
			return nil, fmt.Errorf("%w: %s", ErrSessionExists, spec.SessionID)
		}
		if existing.held {
			m.mu.Unlock()
			return nil, ErrDroppedIn
		}
		if existing.launching {
			m.mu.Unlock()
			return nil, fmt.Errorf("session: %s: launch already in flight", spec.SessionID)
		}
		if existing.sess != nil {
			if p := existing.sess.Provider(); p != nil && p.Alive() {
				if existing.spec.ModelKey == spec.ModelKey {
					live := existing.sess
					m.mu.Unlock()
					return live, nil // already live, same key: resume is a no-op
				}
				// C8: this resume carries a different key than the one the
				// running provider was actually launched with — continuing
				// silently would leave the process on a key relay may
				// already be revoking. Kill it and fall through to the
				// ordinary reservation+spawn path below, exactly like
				// resuming a dead provider.
				relaunch = p
			}
		}
	}

	var reused *sessionstypes.Session
	if exists {
		reused = existing.sess
	}
	slot := &sessionSlot{launching: true, done: make(chan struct{})}
	m.slots[spec.SessionID] = slot
	m.mu.Unlock()

	if relaunch != nil {
		// Cleared before Kill, not left dangling: Kill delivers the old
		// provider's process_exited itself, and handleProviderEvent drops
		// that exit only once the old provider is no longer installed
		// (reporting it would revoke the key just minted for this resume).
		// startProvider also kills whatever provider it displaces, and
		// reused is the same *Session this relaunch was read from.
		reused.SetProvider(nil)
		relaunch.Kill()
	}

	sess, err := m.resolveSessionForCreate(spec, reused)
	if err == nil {
		// Deliberate: an attach starts nothing, so it neither shows the
		// session as starting nor, when it fails, as errored. A failed attach
		// leaves the session as it was, for a later call to try again.
		if !spec.AttachOnly {
			m.signal(sess, attention.Launching)
		}
		_, err = m.startProvider(sess, spec)
	}

	m.mu.Lock()
	stopping := slot.stopping
	slot.launching = false
	if err != nil || stopping {
		delete(m.slots, spec.SessionID)
	} else {
		slot.sess = sess
		slot.spec = spec
		// Deliberate: a restart of this session reuses the stored spec, and the
		// adopt script must never run again for a session it already started.
		slot.spec.AdoptHost = nil
	}
	m.mu.Unlock()

	if err != nil {
		if sess != nil && !spec.AttachOnly {
			m.signal(sess, attention.LaunchFailed)
		}
		// Deliberate: a host adopt whose result never arrived, or whose start
		// failed after the outside process ended, keeps its session saved so it
		// lists as dormant and a resume finds it. A refusal leaves nothing.
		if sess != nil && spec.AdoptHost != nil && (errors.Is(err, provider.ErrAdoptLinkLost) || errors.Is(err, provider.ErrAdoptLaunchFailed)) {
			m.persist(sess)
		}
		close(slot.done)
		return nil, err
	}
	if stopping {
		m.signal(sess, attention.SessionEnded)
		// A Close (EndSession/DeleteSession) for this id arrived while the
		// spawn was still in flight: it found only the reservation, not a
		// provider to kill, so this goroutine — the one that actually
		// knows the spawn succeeded — must finish the close itself instead
		// of ever publishing the session as live. Mirrors terminal's
		// Manager.Create stopping branch exactly.
		if p := sess.Provider(); p != nil {
			p.Kill()
		}
		close(slot.done)
		return nil, fmt.Errorf("session: %s: closed while starting", spec.SessionID)
	}
	if spec.AttachOnly {
		// The board learns of an attach only once it has succeeded.
		m.signal(sess, attention.Launching)
	}
	m.signal(sess, attention.Launched)
	close(slot.done)
	m.announceAttach(sess)
	return sess, nil
}

// announceAttach tells the attention board and the viewers what an attach to
// a detached agent found: a turn in progress and permission prompts nobody
// answered. It runs once the session is published, because a prompt raised
// earlier reaches no board and no viewer.
func (m *Manager) announceAttach(sess *sessionstypes.Session) {
	a, ok := sess.Provider().(interface {
		TakeAttach() (bool, func())
	})
	if !ok {
		return
	}
	turnOpen, raise := a.TakeAttach()
	if turnOpen {
		m.signal(sess, attention.TurnStarted)
	}
	raise()
}

func (m *Manager) resolveSessionForCreate(spec CreateSpec, reused *sessionstypes.Session) (*sessionstypes.Session, error) {
	if reused != nil {
		return reused, nil
	}
	if spec.Resume {
		sess, err := m.store.Load(spec.SessionID)
		if errors.Is(err, fs.ErrNotExist) && spec.AdoptHost != nil {
			// Deliberate: a host adopt saves its session only after the start
			// succeeds, so a resume after a dropped link finds none. It builds
			// the session the adopt would have, seeded with the conversation.
			return buildNewSession(spec, m.cfg.clockOrDefault().Now()), nil
		}
		if err != nil {
			return nil, fmt.Errorf("session: resume %s: not found on disk: %w", spec.SessionID, err)
		}
		// Deliberate: the launch decides the mark, so a session never keeps
		// "unsandboxed" from a saved file when relay resumes it in a project.
		sess.Unsandboxed = spec.Unsandboxed
		return sess, nil
	}
	return buildNewSession(spec, m.cfg.clockOrDefault().Now()), nil
}

func buildNewSession(spec CreateSpec, now time.Time) *sessionstypes.Session {
	name := spec.Name
	if name == "" {
		name = "New Session"
	}

	systemPrompt := spec.SystemPrompt
	// Mirrors relayLLM's CreateSession: CLAUDE.md is prepended only for a
	// non-claude provider with a local (non-host) directory — Claude Code
	// already reads CLAUDE.md itself.
	if spec.AppendClaudeMd && spec.Kind != KindClaude && spec.Directory != "" && spec.Host == nil {
		if content, err := os.ReadFile(filepath.Join(spec.Directory, "CLAUDE.md")); err == nil {
			if systemPrompt != "" {
				systemPrompt = string(content) + "\n---\n" + systemPrompt
			} else {
				systemPrompt = string(content)
			}
		}
	}

	var parsed struct {
		Headless         bool                            `json:"headless"`
		Agent            bool                            `json:"agent"`
		PermissionMode   string                          `json:"permissionMode"`
		PermissionPolicy *sessionstypes.PermissionPolicy `json:"permissionPolicy"`
		ThinkingLevel    string                          `json:"thinkingLevel"`
	}
	if len(spec.Settings) > 0 {
		_ = json.Unmarshal(spec.Settings, &parsed)
	}
	mode := parsed.PermissionMode
	// Deliberate: an unsandboxed session has no sandbox to bound bypass, so
	// being headless does not set it.
	if parsed.Headless && !spec.Unsandboxed {
		mode = "bypassPermissions"
	}
	if parsed.PermissionPolicy != nil && mode == "" {
		mode = parsed.PermissionPolicy.DefaultMode
	}

	var providerState json.RawMessage
	if spec.AttachCodexThread != "" {
		providerState, _ = json.Marshal(map[string]any{"threadId": spec.AttachCodexThread, "daemonAttach": true})
	} else if spec.ResumeConversation != "" {
		if spec.Kind == KindPi {
			providerState, _ = json.Marshal(map[string]string{"piSessionId": spec.ResumeConversation, "piSessionFile": spec.ResumeTranscript})
		} else {
			providerState, _ = json.Marshal(map[string]string{"claudeSessionId": spec.ResumeConversation})
		}
	}

	return &sessionstypes.Session{
		ID:             spec.SessionID,
		ProjectID:      spec.ProjectID,
		ProviderState:  providerState,
		Name:           name,
		Directory:      spec.Directory,
		Model:          spec.Model,
		ProviderType:   spec.Kind,
		Settings:       spec.Settings,
		SystemPrompt:   systemPrompt,
		ThinkingLevel:  parsed.ThinkingLevel,
		CreatedAt:      now.UTC().Format(time.RFC3339),
		Messages:       []sessionstypes.Message{},
		Stats:          sessionstypes.SessionStats{},
		Headless:       parsed.Headless,
		Agent:          parsed.Agent,
		Origin:         spec.Origin,
		Unsandboxed:    spec.Unsandboxed,
		PermissionMode: mode,
		Policy:         parsed.PermissionPolicy,
		Host:           spec.Host,
	}
}

// startProvider constructs a brand new provider instance for sess and
// starts it — never a reused/cached one, including across a resume, so a
// resumed process always gets exactly the identity (model key, hook token)
// this call was handed, never one left over from before the previous
// process died (this package's own security framing: identity continuity
// is "launched exactly like a fresh one", not "specially reused").
func (m *Manager) startProvider(sess *sessionstypes.Session, spec CreateSpec) (sessionstypes.Provider, error) {
	p, err := m.newProvider(sess, spec)
	if err != nil {
		return nil, err
	}

	// SwapProvider, not SetProvider: Create's startProvider can find a
	// provider restartProvider installed in the same session that is still
	// inside its own Start() and therefore reports Alive() == false — that provider is not
	// "empty", it is a reservation of its own, and overwriting sess's
	// provider field out from under it would leave it running with nothing
	// in the table pointing at it. Kill whatever was there before this call
	// ever gets to claim the field.
	if old := sess.SwapProvider(p); old != nil {
		old.Kill()
	}
	if err := p.Start(); err != nil {
		return p, err
	}
	if m.runnerSession(sess) {
		// The saved state names the run, its cursor and its root; a restart before
		// the first result would otherwise find none of them. A pi session, which
		// ends with relay-sessions, needs it too: its adopted transcript is the
		// only way back to the conversation when the restart comes mid-turn.
		m.persist(sess)
	}
	return p, nil
}

// newProvider constructs, but does not install or start, a provider for
// sess wired to report its events as its own.
func (m *Manager) newProvider(sess *sessionstypes.Session, spec CreateSpec) (sessionstypes.Provider, error) {
	// self is read by handler only from a goroutine the provider itself
	// spawns inside Start() (claude.go/pi.go's waitForExit, ChatProvider's
	// runToolLoop), never before — so the write below, sequenced before any
	// such goroutine is created, happens-before every read of it. This is
	// what lets handleProviderEvent tell "this provider's own exit" apart
	// from the event a just-displaced provider delivers during its own
	// Kill() (Kill returns only after process_exited reached the handler,
	// on the exiting provider's own goroutine; a resume clears the
	// session's provider first, so that exit is not the live one's).
	var self sessionstypes.Provider
	handler := func(eventType string, data json.RawMessage) {
		m.handleProviderEvent(sess, self, eventType, data)
	}

	var p sessionstypes.Provider
	var err error
	if factory := m.getProviderFactory(); factory != nil {
		p, err = factory(sess, spec, handler)
	} else {
		p, err = m.buildProvider(sess, spec, handler)
	}
	if err != nil {
		return nil, err
	}
	self = p

	// Applied uniformly, factory or built-in: a resumed session's fresh
	// provider instance picks up the persisted ProviderState (e.g. Claude's
	// own --resume session id) before Start, regardless of which path
	// constructed it.
	if sess.ProviderState != nil {
		p.RestoreState(sess.ProviderState)
	}
	return p, nil
}

// errNotLaunchedHere is the refusal a user sees when a project-less session
// cannot be restarted from a launch of this process's own.
var errNotLaunchedHere = fmt.Errorf("%w: this session cannot be restarted automatically; start a new session in a project to continue", ErrResumeRequired)

// respawnSpec returns the CreateSpec this process launched sess with, so a
// manager-internal restart (ClearSession, SendMessage's project-less
// restart) carries forward the same identity, key and sandbox profile. A
// launch in progress for sess's id is waited out first, then the slot is
// read again. Kind always comes from the persisted session: sess.ProviderType
// is the authoritative provider kind once a session exists.
//
// It also returns the slot it judged owned. Every launch publishes a new
// *sessionSlot and a published slot is never mutated, so that pointer is the
// ownership token restartProvider checks: the restart may act only while
// m.slots[sess.ID] is still that exact slot.
func (m *Manager) respawnSpec(sess *sessionstypes.Session) (CreateSpec, *sessionSlot, error) {
	m.mu.Lock()
	if slot, ok := m.slots[sess.ID]; ok && slot.launching {
		done := slot.done
		m.mu.Unlock()
		<-done
		m.mu.Lock()
	}
	slot, ok := m.slots[sess.ID]
	// Deliberate: only a slot this process launched, holding exactly this
	// session, with its stored spec, may be restarted. A lazy-loaded slot, a
	// failed or stopped launch, or a replaced session has no spec of its own,
	// and a spec holding only Kind starts claude or pi with no sandbox, no
	// identity and no key.
	owned := ok && !slot.launching && slot.sess == sess && slot.spec.SessionID != ""
	var spec CreateSpec
	if owned {
		spec = slot.spec
	}
	m.mu.Unlock()
	if !owned {
		return CreateSpec{}, nil, errNotLaunchedHere
	}
	spec.Kind = sess.ProviderType
	return spec, slot, nil
}

// restartProvider starts a project-less session's provider from the spec
// and slot respawnSpec returned. The restart acts only while m.slots[sess.ID]
// is still that exact slot: the new provider is installed under m.mu only if
// it is, and checked again after Start. If the slot was ended, stopped or
// replaced in either window, the provider this call built is killed and the
// restart is refused; after Start it is also taken out of sess first, but
// only when a launch of this process's own now owns the id.
func (m *Manager) restartProvider(sess *sessionstypes.Session, slot *sessionSlot, spec CreateSpec) error {
	p, err := m.newProvider(sess, spec)
	if err != nil {
		return fmt.Errorf("session: restart provider: %w", err)
	}

	m.mu.Lock()
	if m.slots[sess.ID] != slot {
		m.mu.Unlock()
		p.Kill()
		return errNotLaunchedHere
	}
	old := sess.SwapProvider(p)
	m.mu.Unlock()
	// Deliberate: killed outside m.mu. Kill does process I/O, and nothing
	// may block on m.mu while a provider's Start or Kill is in flight.
	if old != nil {
		old.Kill()
	}

	m.signal(sess, attention.Launching)
	if err := p.Start(); err != nil {
		m.signal(sess, attention.LaunchFailed)
		return fmt.Errorf("session: restart provider: %w", err)
	}
	m.mu.Lock()
	cur := m.slots[sess.ID]
	launched := cur != nil && (cur.launching || cur.spec.SessionID != "")
	m.mu.Unlock()
	if cur == slot {
		m.signal(sess, attention.Launched)
		return nil
	}
	if cur == nil {
		m.signal(sess, attention.SessionEnded)
	}
	// Deliberate: kill p itself, never sess.Provider(), and take p out of
	// sess first only when another launch owns the id. Then p's exit event
	// fails handleProviderEvent's displaced-provider guard and never reaches
	// the live launch; the swap applies only if sess still holds p. A slot
	// Get filled from a disk load is not a launch (its spec is zero) and owns
	// nothing. With no owner, p's exit is the only report relay gets for the
	// id (its SessionExited handling drives the cleanup), so p stays
	// installed. The window between reading an unowned slot and that exit's
	// delivery is the same one an ordinary EndSession followed by a fast
	// resume has, and is not closed here.
	if launched {
		sess.CompareAndSwapProvider(p, nil)
	}
	p.Kill()
	return errNotLaunchedHere
}

func (m *Manager) buildProvider(sess *sessionstypes.Session, spec CreateSpec, handler sessionstypes.EventHandler) (sessionstypes.Provider, error) {
	switch spec.Kind {
	case KindClaude:
		ccfg := m.cfg.Claude
		ccfg.Identity = spec.Identity
		ccfg.ClaudeToken = spec.ClaudeToken
		ccfg.PassthroughEnv = spec.ClaudeEnv
		ccfg.SandboxProfile = spec.SandboxProfile
		ccfg.Launcher = spec.Launcher
		// Deliberate: a host session with no entry and no model is an adopt that
		// found none (a raw host start must carry one), and the binary runs its
		// own default, on every restart of it as well.
		ccfg.DefaultModel = spec.Model == "" && (spec.Entry != "" || spec.Host != nil)
		ccfg.Resume = spec.Resume
		ccfg.AttachOnly = spec.AttachOnly
		ccfg.AdoptHost = spec.AdoptHost
		return provider.NewClaudeProvider(sess, handler, ccfg, m.perms), nil
	case KindPi:
		picfg := m.cfg.Pi
		picfg.ModelKey = spec.ModelKey
		picfg.Identity = spec.Identity
		picfg.SandboxProfile = spec.SandboxProfile
		picfg.Launcher = spec.Launcher
		return provider.NewPiProvider(sess, handler, picfg), nil
	case KindCodex:
		cxcfg := m.cfg.Codex
		cxcfg.Identity = spec.Identity
		cxcfg.SandboxProfile = spec.SandboxProfile
		cxcfg.Launcher = spec.Launcher
		cxcfg.DefaultModel = spec.Entry != "" && spec.Model == ""
		cxcfg.Resume = spec.Resume
		cxcfg.AttachOnly = spec.AttachOnly
		return provider.NewCodexProvider(sess, handler, cxcfg), nil
	case KindChat:
		chatcfg := m.cfg.Chat
		chatcfg.ModelKey = spec.ModelKey
		chatcfg.Identity = spec.Identity
		chatcfg.SandboxProfile = spec.SandboxProfile
		return provider.NewChatProvider(sess, handler, chatcfg), nil
	default:
		return nil, fmt.Errorf("session: kind %q has no provider wired (claude/pi/chat only)", spec.Kind)
	}
}

// Get returns a session by id: first from this process's live table, then
// (relayLLM's own GetSession behavior) lazy-loaded from disk. A session
// found only on disk is returned with a nil Provider — Alive() callers must
// treat that as "not live", not "not found".
//
// A slot already reserved by an in-flight Create (sess == nil, launching)
// is not a miss: Get waits for that Create to resolve rather than racing a
// disk load against it, so a join_session arriving mid-resume can never
// observe a stale/nil-provider session while the real one is spawning, and
// never overwrites the reservation Create still owns.
//
// The same reservation can appear *after* Get has already committed to a
// disk load — Create racing in during the load, not before it. Get must
// still never write its own disk-loaded object into a slot it did not
// reserve itself: that object and Create's spawned one are different
// *sessionstypes.Session values, and a caller (SendMessage's ad-hoc
// respawn included) holding the one Get handed out would end up acting on
// an object no slot in the table actually references once Create finishes
// and overwrites slot.sess with its own. So every path that meets a
// reservation it does not own — found immediately, or found only after the
// load completes — discards what it has and waits for that reservation,
// then retries the whole lookup from the top.
func (m *Manager) Get(id string) (*sessionstypes.Session, bool) {
	for {
		m.mu.Lock()
		slot, ok := m.slots[id]
		if ok && slot.sess != nil {
			sess := slot.sess
			m.mu.Unlock()
			return sess, true
		}
		if ok {
			reservation := slot.done
			m.mu.Unlock()
			<-reservation
			continue
		}
		m.mu.Unlock()

		sess, err := m.store.Load(id)
		if err != nil {
			return nil, false
		}

		m.mu.Lock()
		if slot, ok := m.slots[id]; ok {
			if slot.sess != nil {
				live := slot.sess
				m.mu.Unlock()
				return live, true
			}
			reservation := slot.done
			m.mu.Unlock()
			<-reservation
			continue
		}
		m.slots[id] = &sessionSlot{sess: sess}
		m.mu.Unlock()
		return sess, true
	}
}

// Exists reports whether id has a live in-memory slot — live, still
// launching, or ended-but-not-yet-stopped — exactly the existence test
// Create's own duplicate check uses. Unlike Get, this never lazy-loads from
// disk: a caller outside this package (hostapi's cross-manager launch
// dispatcher) needs to know whether *this process* already considers id
// claimed, not whether a same-named session was ever persisted by an
// earlier, possibly unrelated run.
func (m *Manager) Exists(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.slots[id]
	return ok
}

// liveSessions snapshots the sessions of slots whose Create has finished.
// Callers query providers only after m.mu is released.
func (m *Manager) liveSessions() []*sessionstypes.Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*sessionstypes.Session, 0, len(m.slots))
	for _, slot := range m.slots {
		if slot.sess != nil && !slot.launching {
			out = append(out, slot.sess)
		}
	}
	return out
}

func liveProvider(sess *sessionstypes.Session) (sessionstypes.Provider, bool) {
	p := sess.Provider()
	if p == nil || !p.Alive() {
		return nil, false
	}
	return p, true
}

// LiveSession returns id's session only when its slot is launched and its
// provider is alive. Unlike Get, it never lazy-loads from disk and never
// waits on an in-flight Create.
func (m *Manager) LiveSession(id string) (*sessionstypes.Session, bool) {
	m.mu.Lock()
	slot, ok := m.slots[id]
	if !ok || slot.sess == nil || slot.launching {
		m.mu.Unlock()
		return nil, false
	}
	sess := slot.sess
	m.mu.Unlock()

	if _, ok := liveProvider(sess); !ok {
		return nil, false
	}
	return sess, true
}

// RootByPID finds the live session whose provider reports pid as its
// process root.
func (m *Manager) RootByPID(pid int) (sessionID string, root sessionstypes.ProcessRoot, ok bool) {
	for _, sess := range m.liveSessions() {
		p, ok := liveProvider(sess)
		if !ok {
			continue
		}
		reporter, ok := p.(sessionstypes.RootReporter)
		if !ok {
			continue
		}
		r, ok := reporter.ProcessRoot()
		if ok && r.PID == pid {
			return sess.ID, r, true
		}
	}
	return "", sessionstypes.ProcessRoot{}, false
}

// stopSlot removes id from the live table and returns the session to tear
// down, waiting out an in-flight launch first if necessary (mirrors
// internal/sessions/terminal.Manager.Close). Returns nil if id names nothing
// or if a concurrent stop already claimed it.
func (m *Manager) stopSlot(id string) *sessionstypes.Session {
	m.mu.Lock()
	slot, ok := m.slots[id]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	if slot.launching {
		slot.stopping = true
		done := slot.done
		m.mu.Unlock()
		<-done // Create's own goroutine kills the provider in this case.
		return nil
	}
	sess := slot.sess
	delete(m.slots, id)
	m.mu.Unlock()
	return sess
}

// EndSession kills the provider (if any) and persists the session's final
// state, but keeps its on-disk record — the same "kill, don't delete"
// distinction relayLLM's EndSession vs DeleteSession made.
func (m *Manager) EndSession(id string) {
	sess := m.stopSlot(id)
	if sess == nil {
		return
	}
	m.signal(sess, attention.SessionEnded)
	p := sess.Provider()
	// Deliberate: a detached session with no live provider may still have an
	// agent on its host, which relay or this host left running; ending the
	// session ends it.
	orphan := m.runnerSession(sess) && (p == nil || !p.Alive())
	if p != nil {
		p.Kill()
	}
	if orphan {
		if sess.GetHost() == nil {
			provider.StopLocalAgent(sess.ProviderType, m.runnerBase(sess.ProviderType), sess.ID)
		} else {
			provider.StopDetachedAgent(sess.ProviderType, sess.ID, sess.GetHost())
		}
	}
	m.persist(sess)
}

// DeleteSession kills the provider, deletes provider-specific data (e.g.
// Claude's JSONL history) and removes the persisted session file.
func (m *Manager) DeleteSession(id string) {
	sess := m.stopSlot(id)
	if sess != nil {
		sess.MarkDeleted()
		m.signal(sess, attention.SessionEnded)
	}
	if p := providerOf(sess); p != nil {
		if err := p.DeleteSession(); err != nil {
			slog.Warn("session: delete provider data failed", "id", id, "error", err)
		}
		p.Kill()
	} else {
		// A dormant session has no provider, but a detached host agent may
		// still run; it must end with the session.
		stored := sess
		if stored == nil {
			stored, _ = m.store.Load(id)
		}
		if stored != nil && m.runnerSession(stored) && stored.GetHost() == nil {
			provider.DeleteLocalAgent(stored.ProviderType, m.runnerBase(stored.ProviderType), id)
		} else if stored != nil {
			provider.DeleteDetachedAgent(stored.ProviderType, id, stored.GetHost())
		}
	}
	if m.perms != nil {
		m.perms.RevokeHookToken(id)
	}
	if err := m.store.Delete(id); err != nil {
		slog.Warn("session: delete persisted file failed", "id", id, "error", err)
	}
}

// detachedMaxParallelProbes bounds the ssh questions a startup reattach asks
// at once.
const detachedMaxParallelProbes = 4

// ReattachDetached attaches every persisted detached host session and local
// runner session whose agent still runs. The agent is not this process's
// child, so a restart of this host or of relay finds it alive; a session whose
// agent exited stays dormant and resumes with its history. A pi runner that
// still runs is stopped instead: its model key was bound to the launch that
// ended, and the model proxy it used ran inside the relay-sessions that is gone.
// It returns when every attach has finished.
func (m *Manager) ReattachDetached() {
	sessions, err := m.store.LoadAll()
	if err != nil {
		slog.Warn("session: reattach: could not list persisted sessions", "error", err)
		return
	}
	sem := make(chan struct{}, detachedMaxParallelProbes)
	var wg sync.WaitGroup
	for _, sess := range sessions {
		if sess.ProviderType == KindPi && m.runnerSession(sess) {
			m.stopStalePi(sess)
			continue
		}
		if !m.attachable(sess) {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			_ = m.reattachDetached(id)
		}(sess.ID)
	}
	wg.Wait()
}

// runnerBase is the directory kind's local run directories live under.
func (m *Manager) runnerBase(kind string) string {
	switch kind {
	case KindClaude:
		return m.cfg.Claude.RunnerBase
	case KindCodex:
		return m.cfg.Codex.RunnerBase
	case KindPi:
		return m.cfg.Pi.RunnerBase
	}
	return ""
}

// runnerSession reports whether sess is a session whose agent runs under the
// runner: a Claude or codex session on a detached host or on this machine, or a
// pi session on this machine. Pi has no host form.
func (m *Manager) runnerSession(sess *sessionstypes.Session) bool {
	switch sess.ProviderType {
	case KindClaude, KindCodex:
		if sess.GetHost() == nil {
			return m.runnerBase(sess.ProviderType) != ""
		}
		return provider.DetachedHost(sess.GetHost())
	case KindPi:
		return sess.GetHost() == nil && m.cfg.Pi.RunnerBase != ""
	}
	return false
}

// attachable reports whether sess is a runner session that outlives
// relay-sessions, so a start finds its agent and follows it again. A pi agent
// ends with relay-sessions and is never attached.
func (m *Manager) attachable(sess *sessionstypes.Session) bool {
	return sess.ProviderType != KindPi && m.runnerSession(sess)
}

// stopStalePi ends the pi runner a crashed relay-sessions left behind and
// leaves the session dormant. A session this process already holds is a launch
// of its own and stays.
func (m *Manager) stopStalePi(sess *sessionstypes.Session) {
	m.mu.Lock()
	_, held := m.slots[sess.ID]
	m.mu.Unlock()
	if held {
		return
	}
	provider.StopLocalAgent(KindPi, m.cfg.Pi.RunnerBase, sess.ID)
}

var (
	// ErrHostUnreachable means the host did not say whether a detached
	// session's agent runs.
	ErrHostUnreachable = errors.New("session: the host did not say whether the agent runs")
	// ErrAttachFailed means a detached session's agent runs and could not be
	// attached.
	ErrAttachFailed = errors.New("session: the running agent could not be attached")
)

// reattachIfDetached attaches id's agent when id is a detached host session
// with no live provider. Every caller that would otherwise find the session
// dormant calls it first. The error is non-nil only when the host could not
// say whether the agent runs, or the agent runs and could not be attached.
func (m *Manager) reattachIfDetached(id string) error {
	sess, ok := m.Get(id)
	if !ok || !m.attachable(sess) {
		return nil
	}
	if p := sess.Provider(); p != nil && p.Alive() {
		return nil
	}
	return m.reattachDetached(id)
}

// reattachDetached asks the host whether id's agent runs and, when it does,
// attaches a provider to it. It never starts an agent: AttachOnly makes the
// provider fail when the run is gone, and the status question first keeps a
// dormant session from reading as launching. An agent that does not run is
// not an error.
func (m *Manager) reattachDetached(id string) error {
	m.reattachMu.Lock()
	if m.reattaching == nil {
		m.reattaching = map[string]*sync.Mutex{}
	}
	lock := m.reattaching[id]
	if lock == nil {
		lock = &sync.Mutex{}
		m.reattaching[id] = lock
	}
	m.reattachMu.Unlock()
	lock.Lock()
	defer lock.Unlock()

	sess, ok := m.Get(id)
	if !ok || !m.attachable(sess) || m.Held(id) {
		return nil
	}
	if p := sess.Provider(); p != nil && p.Alive() {
		return nil
	}
	host := sess.GetHost()
	var err error
	if host == nil {
		st, err := runner.LocalStatus(m.runnerBase(sess.ProviderType), id, "")
		if err != nil || st.Phase != runner.PhaseAlive {
			return nil
		}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		st, err := provider.DetachedRunState(ctx, host, id)
		cancel()
		if err != nil {
			slog.Warn("session: reattach: the host did not say whether the agent runs", "session", id, "host", host.ID, "error", err)
			return fmt.Errorf("%w: host %q: %v", ErrHostUnreachable, host.Name, err)
		}
		if st.Phase != sshhost.RunAlive {
			return nil
		}
	}
	// Deliberate: a delete that ran during the status question ended the agent
	// and dropped the slot, and attaching now would find nothing to attach.
	m.mu.Lock()
	slot := m.slots[id]
	gone := slot == nil || slot.sess != sess
	m.mu.Unlock()
	if gone {
		return nil
	}
	spec := CreateSpec{
		SessionID: id, ProjectID: sess.ProjectID, Kind: sess.ProviderType, Directory: sess.Directory,
		Name: sess.Name, Model: sess.Model, Origin: sess.Origin, Settings: sess.Settings,
		SystemPrompt: sess.SystemPrompt, Host: host, Resume: true, AttachOnly: true,
	}
	// One more try: the ssh question inside the attach can be cut short by
	// a link that is still settling.
	if _, err = m.Create(spec); err != nil && !errors.Is(err, provider.ErrNoDetachedRun) {
		_, err = m.Create(spec)
	}
	// Deliberate: an agent that exited between the status question and the
	// attach is not a failure; the session stays dormant and resumes as any
	// other whose agent is gone.
	if errors.Is(err, provider.ErrNoDetachedRun) {
		return nil
	}
	if err != nil {
		if host == nil {
			slog.Warn("session: reattach failed", "session", id, "error", err)
			return fmt.Errorf("%w: %v", ErrAttachFailed, err)
		}
		slog.Warn("session: reattach failed", "session", id, "host", host.ID, "error", err)
		return fmt.Errorf("%w: host %q: %v", ErrAttachFailed, host.Name, err)
	}
	if host == nil {
		m.claimRebind(id)
	}
	return nil
}

// askingWait bounds how long the Asked signal waits for the runner's receipt
// of the asking event.
const askingWait = 2 * time.Second

// permissionPending is the permission manager's pending observer. When a
// session's pending count rises above zero it has the runner write an asking
// event first, and signals Asked after the receipt, so a reader that saw the
// session ask finds the event. Deliberate: past askingWait the signal follows
// anyway, because a stuck runner must not hide a prompt from the person.
func (m *Manager) permissionPending(sessionID string, pending int) {
	m.askedMu.Lock()
	if m.asked == nil {
		m.asked = map[string]bool{}
	}
	rose := pending > 0 && !m.asked[sessionID]
	if pending > 0 {
		m.asked[sessionID] = true
	} else {
		delete(m.asked, sessionID)
	}
	m.askedMu.Unlock()
	if rose {
		m.announceAsking(sessionID)
	}
	m.onPermissionPending(sessionID, pending)
}

func (m *Manager) announceAsking(sessionID string) {
	m.mu.Lock()
	slot := m.slots[sessionID]
	var sess *sessionstypes.Session
	if slot != nil {
		sess = slot.sess
	}
	m.mu.Unlock()
	if sess == nil {
		return
	}
	src, ok := sess.Provider().(interface {
		SendEvent(ctx context.Context, typ string) error
	})
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), askingWait)
	defer cancel()
	if err := src.SendEvent(ctx, "asking"); err != nil {
		slog.Warn("session: the runner did not confirm the asking event", "session", sessionID, "error", err)
	}
}

// StopAll tears down every live and in-flight session. Every provider Kill
// call — the one that can block on a subprocess's own shutdown grace period
// — completes before StopAll returns, mirroring
// internal/sessions/terminal.Manager.StopAll's own discipline: shutdown
// must never return while a process it is responsible for might still be
// alive or still exiting. The one deliberate exception: a detached host agent
// is released and left running on its host.
func (m *Manager) StopAll() {
	m.mu.Lock()
	var live []*sessionstypes.Session
	var pending []chan struct{}
	for id, slot := range m.slots {
		if slot.launching {
			slot.stopping = true
			pending = append(pending, slot.done)
			continue
		}
		if slot.sess != nil {
			live = append(live, slot.sess)
		}
		delete(m.slots, id)
	}
	m.mu.Unlock()

	for _, sess := range live {
		m.signal(sess, attention.SessionEnded)
		if p := sess.Provider(); p != nil {
			// Deliberate: a detached host agent outlives relay, so shutdown
			// releases it instead of ending it.
			if r, ok := p.(interface{ Release() }); ok {
				r.Release()
			} else {
				p.Kill()
			}
		}
		m.persist(sess)
	}
	for _, done := range pending {
		<-done
	}
}

// SendMessage sends a user message to sess's provider. SH-6: a project-bound
// session (ProjectID != "") whose provider is not running never respawns —
// callers must check errors.Is(err, ErrResumeRequired) and drive a real
// resume instead. A project-less session (ProjectID == "") restarts only
// if this process launched it, with the spec it was launched with (see
// respawnSpec); anything else also answers ErrResumeRequired.
func (m *Manager) SendMessage(id, text string, files []sessionstypes.FileAttachment) error {
	return m.SendMessageAs(id, text, files, "")
}

// SendMessageAs is SendMessage with the message's origin recorded on the
// transcript entry and the live user_message frame. An empty origin is a
// person's message and leaves both unmarked.
func (m *Manager) SendMessageAs(id, text string, files []sessionstypes.FileAttachment, origin string) error {
	return m.SendMessageWith(context.Background(), id, text, files, origin, provider.DefaultLinkWait)
}

// SendMessageWith is SendMessageAs for a caller that can leave: a detached
// session waits up to linkWait for a lost host link, and a ctx that ends
// first abandons the send with nothing written.
func (m *Manager) SendMessageWith(ctx context.Context, id, text string, files []sessionstypes.FileAttachment, origin string, linkWait time.Duration) error {
	_ = m.reattachIfDetached(id)
	sess, ok := m.Get(id)
	if !ok {
		return ErrSessionNotFound
	}
	if m.Held(id) {
		return ErrDroppedIn
	}
	if !sess.TryStartProcessing() {
		return ErrAlreadyProcessing
	}

	p := sess.Provider()
	if p == nil || !p.Alive() {
		if sess.ProjectID != "" {
			sess.SetProcessing(false)
			return ErrResumeRequired
		}
		spec, slot, err := m.respawnSpec(sess)
		if err != nil {
			sess.SetProcessing(false)
			return err
		}
		if p = sess.Provider(); p == nil || !p.Alive() {
			if err := m.restartProvider(sess, slot, spec); err != nil {
				sess.SetProcessing(false)
				return err
			}
			p = sess.Provider()
		}
	}

	contentJSON, _ := json.Marshal(text)
	sess.Lock()
	sess.Messages = append(sess.Messages, sessionstypes.Message{
		Timestamp: m.cfg.clockOrDefault().Now().UTC().Format(time.RFC3339),
		Role:      "user",
		Content:   contentJSON,
		Files:     files,
		Origin:    origin,
	})
	sess.Unlock()

	if sink := m.eventSink(); sink != nil {
		frame := map[string]any{
			"type":      events.WSMsgUserMessage,
			"sessionId": id,
			"text":      text,
		}
		if origin != "" {
			frame["origin"] = origin
		}
		sink.SendToSession(id, frame)
	}

	before, _ := m.attn.Get(id)
	m.signal(sess, attention.TurnStarted)
	if err := deliver(ctx, p, text, files, linkWait); err != nil {
		linkDown := errors.Is(err, provider.ErrHostLinkDown)
		switch {
		case linkDown:
			// A dropped link is not a failure of the agent: the message
			// never left, so the session returns to what it was.
			if tracked(sess) {
				m.attn.AbortSend(id, before)
			}
		case errors.Is(err, provider.ErrSendUnknown):
			// The line may have landed; the agent's own events settle the
			// state.
		default:
			m.signal(sess, attention.TurnFailed)
		}
		sess.SetProcessing(false)
		if len(files) > 0 || linkDown {
			sess.Lock()
			if n := len(sess.Messages); n > 0 && sess.Messages[n-1].Role == "user" {
				sess.Messages = sess.Messages[:n-1]
			}
			sess.Unlock()
		}
		return err
	}
	return nil
}

// deliver hands the message to the provider; a provider that can wait for a
// lost host link gets the caller's ctx and wait.
func deliver(ctx context.Context, p sessionstypes.Provider, text string, files []sessionstypes.FileAttachment, wait time.Duration) error {
	if w, ok := p.(interface {
		SendMessageWithin(context.Context, string, []sessionstypes.FileAttachment, time.Duration) error
	}); ok {
		return w.SendMessageWithin(ctx, text, files, wait)
	}
	return p.SendMessage(text, files)
}

// sendMessageSyncTimeout bounds SendMessageSync's wait. Test-only seam.
var sendMessageSyncTimeout = 5 * time.Minute

// SendMessageSync sends a message and waits for the complete response —
// relayLLM's own synchronous HTTP surface for services that aren't a WS
// client (relayScheduler, relayTelegram).
func (m *Manager) SendMessageSync(id, text string, files []sessionstypes.FileAttachment) (string, sessionstypes.SessionStats, error) {
	return m.SendMessageSyncWith(context.Background(), id, text, files, provider.DefaultLinkWait)
}

// SendMessageSyncWith is SendMessageSync with SendMessageWith's ctx and
// link wait.
func (m *Manager) SendMessageSyncWith(ctx context.Context, id, text string, files []sessionstypes.FileAttachment, linkWait time.Duration) (string, sessionstypes.SessionStats, error) {
	// Deliberate: before the collector exists, so the output an attach reads
	// from the time relay was away cannot complete this send's reply.
	_ = m.reattachIfDetached(id)
	if _, ok := m.Get(id); !ok {
		return "", sessionstypes.SessionStats{}, ErrSessionNotFound
	}

	collector := NewResponseCollector()
	m.collMu.Lock()
	// A collector already waiting belongs to the in-flight call; replacing it
	// would starve that call of its turn_done.
	if _, waiting := m.collectors[id]; waiting {
		m.collMu.Unlock()
		return "", sessionstypes.SessionStats{}, ErrAlreadyProcessing
	}
	m.collectors[id] = collector
	m.collMu.Unlock()
	defer func() {
		m.collMu.Lock()
		if m.collectors[id] == collector {
			delete(m.collectors, id)
		}
		m.collMu.Unlock()
	}()

	if err := m.SendMessageWith(ctx, id, text, files, "", linkWait); err != nil {
		return "", sessionstypes.SessionStats{}, err
	}

	result, stats, err := collector.Wait(sendMessageSyncTimeout)
	if errors.Is(err, ErrResponseTimeout) {
		_ = m.StopGeneration(id)
	}
	return result, stats, err
}

// StopGeneration aborts the in-flight response, if any.
func (m *Manager) StopGeneration(id string) error {
	sess, ok := m.Get(id)
	if !ok {
		return ErrSessionNotFound
	}
	sess.Lock()
	kind := sess.ProviderType
	raw := append(json.RawMessage(nil), sess.ProviderState...)
	sess.Unlock()
	if kind == KindPi || kind == KindCodex {
		if p := sess.Provider(); p != nil {
			raw = p.GetState()
		}
		var state struct {
			File         string `json:"piSessionFile"`
			DaemonAttach bool   `json:"daemonAttach"`
		}
		// An ordinary Pi or codex session has no adopted state and may carry none
		// at all; only non-empty, unreadable state is an error.
		if len(raw) > 0 && json.Unmarshal(raw, &state) != nil {
			return errors.New("interrupt: invalid session state")
		}
		if (kind == KindPi && state.File != "") || (kind == KindCodex && state.DaemonAttach) {
			m.mu.Lock()
			fn := m.onInterrupt
			m.mu.Unlock()
			if fn == nil {
				return ErrInterruptAuditUnavailable
			}
			if err := fn(context.Background(), id); err != nil {
				slog.Warn("session interrupt: audit refused", "session", id, "error", err)
				return ErrInterruptAuditUnavailable
			}
		}
	}
	m.signal(sess, attention.TurnStopped)
	m.tools.clear(id)
	if p := sess.Provider(); p != nil {
		p.StopGeneration()
	}
	// message_complete tells clients the turn is definitively over even
	// though the provider's own (now-discarded) goroutine may still emit
	// its own — matches relayLLM's StopGeneration. Not a provider-sourced
	// event, so there is no provider identity to compare against — the
	// case this manufactures is never "process_exited", the only case that
	// check applies to.
	m.handleProviderEvent(sess, nil, events.HandlerMessageComplete, nil)
	return nil
}

// ErrClearAttached refuses clear_session on a codex thread relay attached to:
// the thread belongs to the codex daemon, and relay never starts its own codex
// for it.
var ErrClearAttached = errors.New("this session is attached to a codex thread and its history cannot be cleared from relay")

// attachedCodexThread returns the daemon thread sess is attached to. State that
// cannot be read counts as not attached only when empty.
func (m *Manager) attachedCodexThread(sess *sessionstypes.Session) (string, bool) {
	sess.Lock()
	kind := sess.ProviderType
	raw := append(json.RawMessage(nil), sess.ProviderState...)
	sess.Unlock()
	if kind != KindCodex {
		return "", false
	}
	if p := sess.Provider(); p != nil {
		raw = p.GetState()
	}
	var state struct {
		ThreadID     string `json:"threadId"`
		DaemonAttach bool   `json:"daemonAttach"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &state) != nil || !state.DaemonAttach {
		return "", false
	}
	return state.ThreadID, true
}

// attachedCodex reports whether sess is an attach to a daemon-held codex
// thread.
func (m *Manager) attachedCodex(sess *sessionstypes.Session) bool {
	_, attached := m.attachedCodexThread(sess)
	return attached
}

// ErrAttachRequired refuses a resume that would start a provider for an
// attached codex thread outside relay's attach checks.
var ErrAttachRequired = errors.New("session: an attached codex session resumes only through relay's attach checks")

// checkAttachResume holds the attach checks' door shut: a stored attach resumes
// only for a request that names its own thread, and only a stored attach of that
// thread takes such a request.
func (m *Manager) checkAttachResume(spec CreateSpec) error {
	if !spec.Resume {
		return nil
	}
	var thread string
	var attached bool
	if sess, ok := m.Get(spec.SessionID); ok {
		thread, attached = m.attachedCodexThread(sess)
		sess.Lock()
		headless := sess.Headless
		sess.Unlock()
		attached = attached && (headless || spec.AttachCodexThread == "")
	}
	if attached && spec.AttachCodexThread != thread {
		return ErrAttachRequired
	}
	if spec.AttachCodexThread != "" && !attached {
		return ErrAttachRequired
	}
	return nil
}

// ClearSession kills the provider and clears history/stats. A project-bound
// session (ProjectID != "") is left dead afterward and returns
// ErrResumeRequired instead of restarting — a manager-internal restart has
// no authorizing CreateSpec of its own to launch with, and doc.go's
// guarantee that this package never respawns a project-bound session's dead
// provider on its own must hold for every internal path, clear_session
// included, not only SendMessage. A project-less session (ProjectID == "")
// this process launched restarts fresh, same session id and directory, empty
// history, with the spec it was launched with; any other project-less
// session keeps its cleared history and answers ErrResumeRequired.
func (m *Manager) ClearSession(id string) error {
	sess, ok := m.Get(id)
	if !ok {
		return ErrSessionNotFound
	}
	// Clearing drops ProviderState, which holds the thread id and the
	// daemonAttach mark. Without them a later resume would start relay's own
	// codex for an attached thread, so the clear is refused before it changes
	// anything.
	if m.attachedCodex(sess) {
		return ErrClearAttached
	}

	m.signal(sess, attention.TurnStopped)
	m.tools.clear(id)
	old := sess.SwapProvider(nil)
	sess.Lock()
	sess.Messages = []sessionstypes.Message{}
	sess.Stats = sessionstypes.SessionStats{}
	sess.ProviderState = nil
	sess.Unlock()
	sess.SetProcessing(false)

	if old != nil {
		old.Kill()
	}
	m.persist(sess)

	if sink := m.eventSink(); sink != nil {
		sink.SendToSession(id, map[string]any{"type": events.WSMsgClearMessages, "sessionId": id})
		sink.SendToSession(id, map[string]any{"type": events.HandlerStatsUpdate, "sessionId": id, "stats": sessionstypes.SessionStats{}})
		sink.SendToSession(id, map[string]any{"type": events.WSMsgSystemMessage, "sessionId": id, "message": "Conversation history cleared"})
	}

	if sess.ProjectID != "" {
		m.signal(sess, attention.SessionEnded)
		return ErrResumeRequired
	}
	spec, slot, err := m.respawnSpec(sess)
	if err != nil {
		return err
	}
	return m.restartProvider(sess, slot, spec)
}

// RenameSession updates the session's display name and persists.
func (m *Manager) RenameSession(id, name string) error {
	sess, ok := m.Get(id)
	if !ok {
		return ErrSessionNotFound
	}
	sess.Lock()
	sess.Name = name
	sess.Unlock()
	m.persist(sess)

	if sink := m.eventSink(); sink != nil {
		sink.SendToSession(id, map[string]any{"type": events.WSMsgSessionRenamed, "sessionId": id, "name": name})
	}
	return nil
}

// SetSessionFolder updates the session's UI grouping label and persists. An
// empty folder means ungrouped.
func (m *Manager) SetSessionFolder(id, folder string) error {
	sess, ok := m.Get(id)
	if !ok {
		return ErrSessionNotFound
	}
	sess.Lock()
	sess.Folder = folder
	sess.Unlock()
	m.persist(sess)

	if sink := m.eventSink(); sink != nil {
		sink.SendToSession(id, map[string]any{"type": events.WSMsgSessionFolderChanged, "sessionId": id, "folder": folder})
	}
	return nil
}

func (m *Manager) persist(sess *sessionstypes.Session) {
	// A killed provider's exit event can arrive after DeleteSession has
	// already removed the file; skip the write instead of resurrecting it.
	if sess.Deleted() {
		return
	}
	if p := sess.Provider(); p != nil {
		state := p.GetState()
		sess.Lock()
		sess.ProviderState = state
		sess.Unlock()
	}
	if err := m.store.Save(sess); err != nil {
		slog.Error("session: save failed", "id", sess.ID, "error", err)
	}
}

// handleProviderEvent processes one event from source, the provider
// instance that actually emitted it (nil for the synthetic message_complete
// StopGeneration manufactures itself). source is only consulted in the
// "process_exited" case: a provider Create already displaced via
// CreateSpec.Resume's relaunch path delivers its own exit event while being
// killed (newProvider's own comment on self) — reporting
// that as sess's exit would tear down the replacement provider's own,
// already-live credentials, not the dead one's.
func (m *Manager) handleProviderEvent(sess *sessionstypes.Session, source sessionstypes.Provider, eventType string, data json.RawMessage) {
	var msg map[string]any

	switch eventType {
	case events.HandlerLLMEvent:
		msg = map[string]any{"type": events.HandlerLLMEvent, "sessionId": sess.ID, "event": data}
		m.trackTools(sess, data)
		if tracked(sess) {
			sig, text := classifyLLMEvent(data)
			m.attn.Signal(sess.ID, sig)
			if text != "" {
				m.attn.Reply(sess.ID, text)
			}
		}

	case events.HandlerStatsUpdate:
		var stats sessionstypes.SessionStats
		if err := json.Unmarshal(data, &stats); err != nil {
			return
		}
		sess.Lock()
		sess.Stats = stats
		current := sess.Stats
		sess.Unlock()
		m.signal(sess, attention.Activity)
		msg = map[string]any{"type": events.HandlerStatsUpdate, "sessionId": sess.ID, "stats": current}

	case events.HandlerMessageComplete:
		sess.SetProcessing(false)
		m.tools.clear(sess.ID)
		m.signal(sess, completionSignal(data))
		msg = map[string]any{"type": events.HandlerMessageComplete, "sessionId": sess.ID}
		var failed struct {
			IsError        bool `json:"isError"`
			APIErrorStatus int  `json:"apiErrorStatus"`
		}
		if len(data) > 0 && json.Unmarshal(data, &failed) == nil && failed.IsError {
			msg["isError"] = true
			if failed.APIErrorStatus > 0 {
				msg["apiErrorStatus"] = failed.APIErrorStatus
			}
		}
		m.persist(sess)

	case "process_exited":
		if sess.Provider() != source {
			return
		}
		sess.SetProcessing(false)
		m.tools.clear(sess.ID)
		var payload struct {
			ExitCode   int      `json:"exitCode"`
			Failed     bool     `json:"failed"`
			StderrTail []string `json:"stderrTail"`
		}
		_ = json.Unmarshal(data, &payload)
		msg = map[string]any{"type": events.WSMsgProcessExited, "sessionId": sess.ID}
		if r, ok := source.(interface{ Released() bool }); ok && r.Released() {
			// Deliberate: a released detached provider left its agent running,
			// so the session did not end; its board state stays what it was.
		} else if payload.Failed {
			x := &attention.Exit{Code: payload.ExitCode, Stderr: payload.StderrTail}
			m.signalExit(sess, x)
			msg["exit"] = x
		} else {
			m.signal(sess, attention.ProcessExited)
		}
		m.persist(sess)
		if fn := m.exitHandler(); fn != nil {
			fn(sess.ID, payload.ExitCode)
		}

	case "turn_started":
		// A turn that no send of this manager started: a daemon-attached codex
		// thread's terminal began it. A turn this manager's own send began has
		// the session running already.
		sess.SetProcessing(true)
		if cur, ok := m.attn.Get(sess.ID); !ok || cur.State != attention.Running {
			m.signal(sess, attention.TurnStarted)
		}
		return

	case "raw_output":
		m.signal(sess, attention.Activity)
		msg = map[string]any{"type": events.WSMsgRawOutput, "sessionId": sess.ID, "text": string(data)}

	case "error":
		sess.SetProcessing(false)
		m.tools.clear(sess.ID)
		m.signal(sess, attention.TurnFailed)
		msg = map[string]any{"type": events.WSMsgError, "sessionId": sess.ID, "message": string(data)}

	default:
		return
	}

	m.collMu.Lock()
	collector := m.collectors[sess.ID]
	m.collMu.Unlock()
	if collector != nil {
		collector.HandleEvent(msg)
	}

	if sink := m.eventSink(); sink != nil {
		sink.SendToSession(sess.ID, msg)
	}
}

// Summary is the slim per-row shape a list surface (GET /api/sessions)
// renders.
type Summary struct {
	ID            string            `json:"id"`
	ProjectID     string            `json:"projectId"`
	Name          string            `json:"name"`
	Folder        string            `json:"folder,omitempty"`
	Directory     string            `json:"directory"`
	Model         string            `json:"model"`
	Live          bool              `json:"live"`
	CreatedAt     string            `json:"createdAt"`
	MessageCount  int               `json:"messageCount"`
	LastMessageAt string            `json:"lastMessageAt,omitempty"`
	Host          map[string]string `json:"host,omitempty"`
	// Headless is the same Session.Headless flag the drop-in handoff checks.
	Headless bool `json:"headless,omitempty"`
	// Origin is set when someone other than the person started the session.
	Origin string `json:"origin,omitempty"`
	// Unsandboxed is set only on a session relay runs with no project and no sandbox.
	Unsandboxed bool `json:"unsandboxed,omitempty"`

	Attention *attention.Attention `json:"attention,omitempty"`
}

func summarize(sess *sessionstypes.Session) Summary {
	p := sess.Provider()
	sess.Lock()
	defer sess.Unlock()
	return Summary{
		ID:            sess.ID,
		ProjectID:     sess.ProjectID,
		Name:          sess.Name,
		Folder:        sess.Folder,
		Directory:     sess.Directory,
		Model:         sess.Model,
		Live:          p != nil && p.Alive(),
		CreatedAt:     sess.CreatedAt,
		MessageCount:  len(sess.Messages),
		LastMessageAt: lastMessageAt(sess.Messages),
		Host:          sess.Host.Chip(),
		Headless:      sess.Headless,
		Origin:        sess.Origin,
		Unsandboxed:   sess.Unsandboxed,
	}
}

func lastMessageAt(msgs []sessionstypes.Message) string {
	if len(msgs) == 0 {
		return ""
	}
	return msgs[len(msgs)-1].Timestamp
}

// isListed is the one predicate for "a person can see this session": a
// headless run is hidden unless it is marked as an agent.
func isListed(sess *sessionstypes.Session) bool {
	return !sess.Headless || sess.Agent
}

// IsListed reports whether id is a known session that List would show.
// Unlisted sessions answer false exactly like unknown ones, so a caller
// cannot tell a hidden routine run from a missing id.
func (m *Manager) IsListed(id string) bool {
	sess, ok := m.Get(id)
	return ok && isListed(sess)
}

// List returns every session this manager knows about except headless runs
// not marked as agents — live, in-memory-but-idle, and persisted-only (merged in from disk, relayLLM's
// own ListSessions behavior) — sorted by id.
func (m *Manager) List() []Summary {
	m.mu.Lock()
	seen := make(map[string]bool, len(m.slots))
	out := make([]Summary, 0, len(m.slots))
	for id, slot := range m.slots {
		if slot.sess == nil {
			continue
		}
		seen[id] = true
		if !isListed(slot.sess) {
			continue
		}
		row := summarize(slot.sess)
		if !slot.launching {
			if a, ok := m.attn.Get(id); ok {
				row.Attention = &a
			}
		}
		out = append(out, row)
	}
	m.mu.Unlock()

	if persisted, err := m.store.LoadAll(); err == nil {
		for _, sess := range persisted {
			if seen[sess.ID] || !isListed(sess) {
				continue
			}
			out = append(out, summarize(sess))
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ClaudeSessionIDs returns the Claude conversation id of every session this
// host owns, live or persisted. Relay uses it to keep managed sessions out of
// the outside-session list.
func (m *Manager) ClaudeSessionIDs() []string {
	seen := map[string]bool{}
	ids := []string{}
	add := func(sess *sessionstypes.Session) {
		if id := claudeConversationID(sess); id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, sess := range m.liveSessions() {
		add(sess)
	}
	if persisted, err := m.store.LoadAll(); err == nil {
		for _, sess := range persisted {
			add(sess)
		}
	}
	return ids
}

func (m *Manager) PiSessionIDs() []string {
	seen := map[string]bool{}
	ids := []string{}
	add := func(sess *sessionstypes.Session) {
		sess.Lock()
		kind := sess.ProviderType
		raw := append(json.RawMessage(nil), sess.ProviderState...)
		sess.Unlock()
		if kind != KindPi {
			return
		}
		if p := sess.Provider(); p != nil {
			raw = p.GetState()
		}
		var state struct {
			ID string `json:"piSessionId"`
		}
		if json.Unmarshal(raw, &state) == nil && state.ID != "" && !seen[state.ID] {
			seen[state.ID] = true
			ids = append(ids, state.ID)
		}
	}
	for _, sess := range m.liveSessions() {
		add(sess)
	}
	if persisted, err := m.store.LoadAll(); err == nil {
		for _, sess := range persisted {
			add(sess)
		}
	}
	return ids
}

// CodexThreadIDs returns the thread id of every codex session this host owns,
// live or persisted. Relay uses it to keep managed threads out of the
// outside-session list.
func (m *Manager) CodexThreadIDs() []string {
	seen := map[string]bool{}
	ids := []string{}
	add := func(sess *sessionstypes.Session) {
		sess.Lock()
		kind := sess.ProviderType
		raw := append(json.RawMessage(nil), sess.ProviderState...)
		sess.Unlock()
		if kind != KindCodex {
			return
		}
		if p := sess.Provider(); p != nil {
			raw = p.GetState()
		}
		var state struct {
			ThreadID string `json:"threadId"`
		}
		if json.Unmarshal(raw, &state) == nil && state.ThreadID != "" && !seen[state.ThreadID] {
			seen[state.ThreadID] = true
			ids = append(ids, state.ThreadID)
		}
	}
	for _, sess := range m.liveSessions() {
		add(sess)
	}
	if persisted, err := m.store.LoadAll(); err == nil {
		for _, sess := range persisted {
			add(sess)
		}
	}
	return ids
}

func providerOf(sess *sessionstypes.Session) sessionstypes.Provider {
	if sess == nil {
		return nil
	}
	return sess.Provider()
}

// WaitTimeout is the state Wait reports when its cap passes first. The turn
// keeps running.
const WaitTimeout attention.State = "timeout"

// WaitResult is how a Wait ended. Excerpt is the end of the last turn's reply.
// Exit is set when the agent died.
type WaitResult struct {
	State   attention.State
	Excerpt string
	Exit    *attention.Exit
}

// Wait returns when the session's current turn needs the caller: idle (the
// turn ended, or none was running), asking, errored or ended. A cap above zero
// ends the wait with WaitTimeout, measured on the manager's clock.
func (m *Manager) Wait(ctx context.Context, id string, cap time.Duration) (WaitResult, error) {
	// A detached agent that is running is attached first, so the wait reads
	// its real state rather than a dormant session's.
	_ = m.reattachIfDetached(id)
	sess, ok := m.Get(id)
	if !ok {
		return WaitResult{}, ErrSessionNotFound
	}
	if !tracked(sess) {
		return WaitResult{}, ErrNotTracked
	}
	var deadline <-chan time.Time
	if cap > 0 {
		deadline = m.cfg.clockOrDefault().After(cap)
	}
	changed, unsubscribe := m.watch.subscribe(id)
	defer unsubscribe()
	for {
		a, live := m.attn.Get(id)
		if !live {
			return WaitResult{State: attention.Ended}, nil
		}
		switch a.State {
		case attention.Idle, attention.Asking, attention.Errored, attention.Ended:
			return WaitResult{State: a.State, Excerpt: m.watch.excerpt(id), Exit: a.Exit}, nil
		}
		select {
		case <-changed:
		case <-deadline:
			return WaitResult{State: WaitTimeout}, nil
		case <-ctx.Done():
			return WaitResult{}, ctx.Err()
		}
	}
}

// turnWatch sits between the attention board and its sink. It keeps the last
// turn's excerpt and wakes the waiters of a session when its state changes.
type turnWatch struct {
	next attention.Sink

	mu       sync.Mutex
	excerpts map[string]string
	subs     map[string]map[chan struct{}]struct{}
}

func newTurnWatch(next attention.Sink) *turnWatch {
	return &turnWatch{next: next, excerpts: map[string]string{}, subs: map[string]map[chan struct{}]struct{}{}}
}

func (w *turnWatch) subscribe(id string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	w.mu.Lock()
	if w.subs[id] == nil {
		w.subs[id] = map[chan struct{}]struct{}{}
	}
	w.subs[id][ch] = struct{}{}
	w.mu.Unlock()
	return ch, func() {
		w.mu.Lock()
		delete(w.subs[id], ch)
		if len(w.subs[id]) == 0 {
			delete(w.subs, id)
		}
		w.mu.Unlock()
	}
}

func (w *turnWatch) excerpt(id string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.excerpts[id]
}

func (w *turnWatch) StateChanged(c attention.Change) {
	w.mu.Lock()
	if c.State == attention.Running || c.State == attention.Ended {
		delete(w.excerpts, c.SessionID)
	}
	for ch := range w.subs[c.SessionID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	w.mu.Unlock()
	w.next.StateChanged(c)
}

// TurnDone arrives before the state change it belongs to, so a waiter woken
// by that change reads this turn's excerpt.
func (w *turnWatch) TurnDone(d attention.TurnDone) {
	w.mu.Lock()
	w.excerpts[d.SessionID] = d.Excerpt
	w.mu.Unlock()
	w.next.TurnDone(d)
}

type eventSource interface {
	Events(ctx context.Context, after string, max int) ([]json.RawMessage, string, error)
}

// Events returns up to max events a detached session's agent wrote after the
// cursor, and the cursor to pass next. Any other session answers
// ErrReplayUnavailable.
func (m *Manager) Events(ctx context.Context, id, after string, max int) ([]json.RawMessage, string, error) {
	_ = m.reattachIfDetached(id)
	sess, ok := m.Get(id)
	if !ok {
		return nil, "", ErrSessionNotFound
	}
	src, ok := sess.Provider().(eventSource)
	if !ok {
		return nil, "", ErrReplayUnavailable
	}
	lines, next, err := src.Events(ctx, after, max)
	if errors.Is(err, provider.ErrNotDetached) {
		return nil, "", ErrReplayUnavailable
	}
	return lines, next, err
}
