package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/clock"
	"github.com/barelyworkingcode/relay/internal/sessions/events"
	sessionsmcp "github.com/barelyworkingcode/relay/internal/sessions/mcp"
	"github.com/barelyworkingcode/relay/internal/sessions/permission"
	"github.com/barelyworkingcode/relay/internal/sessions/provider"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// Session kinds this manager can actually spawn a provider for. Mirrors
// cmd/relay/session_launch.go's KindClaude/KindPi/KindChat wire values
// (that package cannot be imported from here — cmd depends on internal,
// never the other way — so the strings are the shared contract, not a Go
// symbol). A kind outside this set is refused explicitly by Create rather
// than silently mis-routed, the same choice hostapi's own handleLaunch
// already makes for pty (launch.go's own doc comment).
const (
	KindClaude = "claude"
	KindPi     = "pi"
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

	// onExit mirrors internal/sessions/terminal.Manager's own field of the
	// same name and purpose: a later unit's SessionExited bridge hook, fired
	// from handleProviderEvent's "process_exited" case on the provider's own
	// waitForExit goroutine — never synchronously inside a caller's request.
	onExit func(id string, exitCode int)

	collMu     sync.Mutex
	collectors map[string]*ResponseCollector

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
	return &Manager{
		slots:      make(map[string]*sessionSlot),
		store:      store,
		perms:      perms,
		cfg:        cfg,
		collectors: make(map[string]*ResponseCollector),
	}
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
// provider's own waitForExit goroutine, never inline with a caller's
// SendMessage/Create; ChatProvider has no OS process to wait on, so its
// Kill invokes fn synchronously on the caller's own goroutine instead — fn
// must tolerate either.
func (m *Manager) SetExitHandler(fn func(id string, exitCode int)) {
	m.mu.Lock()
	m.onExit = fn
	m.mu.Unlock()
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
	// SandboxProfile is this launch's own absolute SBPL profile path (C7),
	// threaded to chat's MCP tool child. "" runs it unsandboxed.
	SandboxProfile string

	// Resume is true for relay's POST /launch resume:true (SH §3.4 driven
	// by relay's own POST /api/sessions/{id}/resume, never by this host):
	// reattach the persisted session (history, provider state) under a
	// freshly spawned provider process, rather than starting empty.
	Resume bool
}

func (m *Manager) providerKindSupported(kind string) bool {
	return kind == KindClaude || kind == KindPi || kind == KindChat
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
		return nil, fmt.Errorf("session: kind %q has no provider wired (claude/pi/chat only)", spec.Kind)
	}

	m.mu.Lock()
	existing, exists := m.slots[spec.SessionID]
	var relaunch sessionstypes.Provider
	if exists {
		if !spec.Resume {
			m.mu.Unlock()
			return nil, fmt.Errorf("%w: %s", ErrSessionExists, spec.SessionID)
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
		relaunch.Kill()
		// Cleared, not left dangling: startProvider below also kills
		// whatever provider it displaces (the same guard that protects a
		// concurrent ad-hoc respawn), and reused is the same *Session this
		// relaunch was read from — leaving the field set would hand
		// startProvider a reference this call already tore down itself.
		reused.SetProvider(nil)
	}

	sess, err := m.resolveSessionForCreate(spec, reused)
	if err == nil {
		err = m.startProvider(sess, spec)
	}

	m.mu.Lock()
	stopping := slot.stopping
	slot.launching = false
	if err != nil || stopping {
		delete(m.slots, spec.SessionID)
	} else {
		slot.sess = sess
		slot.spec = spec
	}
	m.mu.Unlock()

	if err != nil {
		close(slot.done)
		return nil, err
	}
	if stopping {
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
	close(slot.done)
	return sess, nil
}

func (m *Manager) resolveSessionForCreate(spec CreateSpec, reused *sessionstypes.Session) (*sessionstypes.Session, error) {
	if reused != nil {
		return reused, nil
	}
	if spec.Resume {
		sess, err := m.store.Load(spec.SessionID)
		if err != nil {
			return nil, fmt.Errorf("session: resume %s: not found on disk: %w", spec.SessionID, err)
		}
		return sess, nil
	}
	return buildNewSession(spec, m.cfg.clockOrDefault().Now()), nil
}

func buildNewSession(spec CreateSpec, now time.Time) *sessionstypes.Session {
	model := spec.Model
	if model == "" {
		model = "sonnet"
	}
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
		PermissionMode   string                          `json:"permissionMode"`
		PermissionPolicy *sessionstypes.PermissionPolicy `json:"permissionPolicy"`
		ThinkingLevel    string                          `json:"thinkingLevel"`
	}
	if len(spec.Settings) > 0 {
		_ = json.Unmarshal(spec.Settings, &parsed)
	}
	mode := parsed.PermissionMode
	if parsed.Headless {
		mode = "bypassPermissions"
	}
	if parsed.PermissionPolicy != nil && mode == "" {
		mode = parsed.PermissionPolicy.DefaultMode
	}

	return &sessionstypes.Session{
		ID:             spec.SessionID,
		ProjectID:      spec.ProjectID,
		Name:           name,
		Directory:      spec.Directory,
		Model:          model,
		ProviderType:   spec.Kind,
		Settings:       spec.Settings,
		SystemPrompt:   systemPrompt,
		ThinkingLevel:  parsed.ThinkingLevel,
		CreatedAt:      now.UTC().Format(time.RFC3339),
		Messages:       []sessionstypes.Message{},
		Stats:          sessionstypes.SessionStats{},
		Headless:       parsed.Headless,
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
func (m *Manager) startProvider(sess *sessionstypes.Session, spec CreateSpec) error {
	// self is read by handler only from a goroutine the provider itself
	// spawns inside Start() (claude.go/pi.go's waitForExit, ChatProvider's
	// runToolLoop), never before — so the write below, sequenced before any
	// such goroutine is created, happens-before every read of it. This is
	// what lets handleProviderEvent tell "this provider's own exit" apart
	// from a stale event a just-displaced provider fires after Kill()
	// already returned (Kill() unblocks on the process dying; the event
	// arrives after, on the exiting provider's own goroutine — a resumed
	// session's brand new provider can already be live by then).
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
		return err
	}
	self = p

	// Applied uniformly, factory or built-in: a resumed session's fresh
	// provider instance picks up the persisted ProviderState (e.g. Claude's
	// own --resume session id) before Start, regardless of which path
	// constructed it.
	if sess.ProviderState != nil {
		p.RestoreState(sess.ProviderState)
	}

	// SwapProvider, not SetProvider: a second startProvider for the same
	// session (SendMessage's ad-hoc respawn racing a Create{Resume:true})
	// can reach here while the first provider is still inside its own
	// Start() and therefore reports Alive() == false — that provider is not
	// "empty", it is a reservation of its own, and overwriting sess's
	// provider field out from under it would leave it running with nothing
	// in the table pointing at it. Kill whatever was there before this call
	// ever gets to claim the field.
	if old := sess.SwapProvider(p); old != nil {
		old.Kill()
	}
	return p.Start()
}

// errNotLaunchedHere is the refusal a user sees when a project-less session
// has no launch of this process's own to restart from.
var errNotLaunchedHere = fmt.Errorf("%w: this session was not started here; start a new session in a project to continue", ErrResumeRequired)

// respawnSpec returns the CreateSpec this process launched sess with, so a
// manager-internal restart (ClearSession, SendMessage's project-less
// restart) carries forward the same identity, key and sandbox profile. A
// launch in progress for sess's id is waited out first, then the slot is
// read again. Kind always comes from the persisted session: sess.ProviderType
// is the authoritative provider kind once a session exists.
func (m *Manager) respawnSpec(sess *sessionstypes.Session) (CreateSpec, error) {
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
		return CreateSpec{}, errNotLaunchedHere
	}
	spec.Kind = sess.ProviderType
	return spec, nil
}

func (m *Manager) buildProvider(sess *sessionstypes.Session, spec CreateSpec, handler sessionstypes.EventHandler) (sessionstypes.Provider, error) {
	switch spec.Kind {
	case KindClaude:
		ccfg := m.cfg.Claude
		ccfg.Identity = spec.Identity
		ccfg.SandboxProfile = spec.SandboxProfile
		return provider.NewClaudeProvider(sess, handler, ccfg, m.perms), nil
	case KindPi:
		picfg := m.cfg.Pi
		picfg.ModelKey = spec.ModelKey
		picfg.Identity = spec.Identity
		picfg.SandboxProfile = spec.SandboxProfile
		return provider.NewPiProvider(sess, handler, picfg), nil
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
	if p := sess.Provider(); p != nil {
		p.Kill()
	}
	m.persist(sess)
}

// DeleteSession kills the provider, deletes provider-specific data (e.g.
// Claude's JSONL history) and removes the persisted session file.
func (m *Manager) DeleteSession(id string) {
	sess := m.stopSlot(id)
	if sess != nil {
		sess.MarkDeleted()
		if p := sess.Provider(); p != nil {
			if err := p.DeleteSession(); err != nil {
				slog.Warn("session: delete provider data failed", "id", id, "error", err)
			}
			p.Kill()
		}
	}
	if m.perms != nil {
		m.perms.RevokeHookToken(id)
	}
	if err := m.store.Delete(id); err != nil {
		slog.Warn("session: delete persisted file failed", "id", id, "error", err)
	}
}

// StopAll tears down every live and in-flight session. Every provider Kill
// call — the one that can block on a subprocess's own shutdown grace period
// — completes before StopAll returns, mirroring
// internal/sessions/terminal.Manager.StopAll's own discipline: shutdown
// must never return while a process it is responsible for might still be
// alive or still exiting.
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
		if p := sess.Provider(); p != nil {
			p.Kill()
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
	sess, ok := m.Get(id)
	if !ok {
		return ErrSessionNotFound
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
		spec, err := m.respawnSpec(sess)
		if err != nil {
			sess.SetProcessing(false)
			return err
		}
		if p = sess.Provider(); p == nil || !p.Alive() {
			if err := m.startProvider(sess, spec); err != nil {
				sess.SetProcessing(false)
				return fmt.Errorf("session: respawn failed: %w", err)
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
	})
	sess.Unlock()

	if sink := m.eventSink(); sink != nil {
		sink.SendToSession(id, map[string]any{
			"type":      events.WSMsgUserMessage,
			"sessionId": id,
			"text":      text,
		})
	}

	if err := p.SendMessage(text, files); err != nil {
		sess.SetProcessing(false)
		if len(files) > 0 {
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

// sendMessageSyncTimeout bounds SendMessageSync's wait. Test-only seam.
var sendMessageSyncTimeout = 5 * time.Minute

// SendMessageSync sends a message and waits for the complete response —
// relayLLM's own synchronous HTTP surface for services that aren't a WS
// client (relayScheduler, relayTelegram).
func (m *Manager) SendMessageSync(id, text string, files []sessionstypes.FileAttachment) (string, sessionstypes.SessionStats, error) {
	if _, ok := m.Get(id); !ok {
		return "", sessionstypes.SessionStats{}, ErrSessionNotFound
	}

	collector := NewResponseCollector()
	m.collMu.Lock()
	m.collectors[id] = collector
	m.collMu.Unlock()
	defer func() {
		m.collMu.Lock()
		delete(m.collectors, id)
		m.collMu.Unlock()
	}()

	if err := m.SendMessage(id, text, files); err != nil {
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
		return ErrResumeRequired
	}
	spec, err := m.respawnSpec(sess)
	if err != nil {
		return err
	}
	if err := m.startProvider(sess, spec); err != nil {
		return fmt.Errorf("session: restart provider: %w", err)
	}
	return nil
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
// CreateSpec.Resume's relaunch path can still fire its own delayed exit
// event afterward (startProvider's own doc comment on self) — reporting
// that as sess's exit would tear down the replacement provider's own,
// already-live credentials, not the dead one's.
func (m *Manager) handleProviderEvent(sess *sessionstypes.Session, source sessionstypes.Provider, eventType string, data json.RawMessage) {
	var msg map[string]any

	switch eventType {
	case events.HandlerLLMEvent:
		msg = map[string]any{"type": events.HandlerLLMEvent, "sessionId": sess.ID, "event": data}

	case events.HandlerStatsUpdate:
		var stats sessionstypes.SessionStats
		if err := json.Unmarshal(data, &stats); err != nil {
			return
		}
		sess.Lock()
		sess.Stats = stats
		current := sess.Stats
		sess.Unlock()
		msg = map[string]any{"type": events.HandlerStatsUpdate, "sessionId": sess.ID, "stats": current}

	case events.HandlerMessageComplete:
		sess.SetProcessing(false)
		msg = map[string]any{"type": events.HandlerMessageComplete, "sessionId": sess.ID}
		m.persist(sess)

	case "process_exited":
		if sess.Provider() != source {
			return
		}
		sess.SetProcessing(false)
		msg = map[string]any{"type": events.WSMsgProcessExited, "sessionId": sess.ID}
		m.persist(sess)
		if fn := m.exitHandler(); fn != nil {
			var payload struct {
				ExitCode int `json:"exitCode"`
			}
			_ = json.Unmarshal(data, &payload)
			fn(sess.ID, payload.ExitCode)
		}

	case "raw_output":
		msg = map[string]any{"type": events.WSMsgRawOutput, "sessionId": sess.ID, "text": string(data)}

	case "error":
		sess.SetProcessing(false)
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
	}
}

func lastMessageAt(msgs []sessionstypes.Message) string {
	if len(msgs) == 0 {
		return ""
	}
	return msgs[len(msgs)-1].Timestamp
}

// List returns every non-headless session this manager knows about — live,
// in-memory-but-idle, and persisted-only (merged in from disk, relayLLM's
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
		if slot.sess.Headless {
			continue
		}
		out = append(out, summarize(slot.sess))
	}
	m.mu.Unlock()

	if persisted, err := m.store.LoadAll(); err == nil {
		for _, sess := range persisted {
			if seen[sess.ID] || sess.Headless {
				continue
			}
			out = append(out, summarize(sess))
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
