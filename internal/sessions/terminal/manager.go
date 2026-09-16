package terminal

import (
	"fmt"
	"sort"
	"sync"

	"github.com/barelyworkingcode/relay/internal/sessions/clock"
)

// Config configures a Manager: where the shim binary lives, where
// per-session logs are written (empty disables disk logging), and this
// host's own bridge/model socket paths, threaded into every spawned
// session's env (buildShimEnv's doc comment).
type Config struct {
	ShimBinary   string
	LogDir       string
	BridgeSocket string
	ModelSocket  string
	Clock        clock.Clock
}

func (c Config) clockOrDefault() clock.Clock {
	if c.Clock == nil {
		return clock.DefaultClock
	}
	return c.Clock
}

// Summary is the slim per-row shape a list surface (Service Inspector,
// GET /api/terminals) renders.
type Summary struct {
	ID         string            `json:"id"`
	TemplateID string            `json:"templateId"`
	Name       string            `json:"name"`
	Directory  string            `json:"directory"`
	State      string            `json:"state"`
	ExitCode   int               `json:"exitCode,omitempty"`
	Host       map[string]string `json:"host,omitempty"`
}

// sessionSlot is one entry in Manager's table, including the window between
// a Create reserving an id and startSession (up to helloWait) returning.
// Mirrors internal/sessions/hostapi/session_table.go's stateLaunching
// reservation: the existence check and the reservation must be the same
// atomic step under Manager.mu, or two concurrent Creates for the same id
// can both pass the check and both spawn a live, untracked process.
type sessionSlot struct {
	launching bool
	stopping  bool // a Close/StopAll arrived while still launching; the spawning Create must close its own result instead of publishing it
	// done is closed only once this slot's fate is fully settled, including
	// any teardown Create had to perform itself (the stopping branch): a
	// waiter (StopAll) must be able to trust that a closed done means the
	// process, if one was ever spawned, is no longer live — not merely that
	// the table entry has been resolved.
	done    chan struct{}
	session *Session // nil until the launch succeeds
}

// Manager owns the live set of terminal sessions this host is hosting.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*sessionSlot
	cfg      Config
	onExit   func(id string, exitCode int)
}

// NewManager constructs a Manager. cfg.ShimBinary must be set before Create
// is ever called.
func NewManager(cfg Config) *Manager {
	return &Manager{sessions: make(map[string]*sessionSlot), cfg: cfg}
}

// SetExitHandler installs fn to be called, on its own goroutine, whenever
// any session this manager owns exits — the hook a later unit wires to C5's
// SessionExited bridge notification. A session already stays in this
// manager's table after exit (Close removes it, not a natural exit) so its
// final state and log stay reachable until then; this callback is purely
// advisory on top of that.
func (m *Manager) SetExitHandler(fn func(id string, exitCode int)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onExit = fn
}

// Create starts a new terminal session. Returns ErrSessionExists if spec's
// id is already live, launching, or stopped-but-present in this manager's
// table — the existence check and the reservation of spec.SessionID happen
// under the same lock acquisition, so two concurrent Creates for the same
// id can never both pass the check.
func (m *Manager) Create(spec CreateSpec) (*Session, error) {
	m.mu.Lock()
	if _, exists := m.sessions[spec.SessionID]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrSessionExists, spec.SessionID)
	}
	slot := &sessionSlot{launching: true, done: make(chan struct{})}
	m.sessions[spec.SessionID] = slot
	m.mu.Unlock()

	onIdle := func(id string) { m.Close(id) }
	onExit := func(id string, exitCode int) {
		m.mu.Lock()
		fn := m.onExit
		m.mu.Unlock()
		if fn != nil {
			fn(id, exitCode)
		}
	}
	s, err := startSession(spec, m.cfg, onExit, onIdle)

	m.mu.Lock()
	stopping := slot.stopping
	slot.launching = false
	if err != nil || stopping {
		delete(m.sessions, spec.SessionID)
	} else {
		slot.session = s
	}
	m.mu.Unlock()

	// done closes only once this branch's own outcome is fully final — for
	// the stopping branch that means after s.Close() below returns, not
	// here — so a StopAll blocked on <-slot.done never observes done closed
	// while a process it's responsible for is still alive. m.mu is not held
	// across any of this: StopAll's own wait on done runs lock-free, so
	// nothing this goroutine does while resolving its outcome can deadlock
	// against it.
	if err != nil {
		close(slot.done)
		return nil, err
	}
	if stopping {
		// A Close/StopAll for this id arrived while the spawn was still in
		// flight (the Hello-wait window): it found only the reservation, not
		// a process to signal, so this goroutine — the one that actually
		// knows the spawn succeeded — must finish the close itself instead
		// of ever publishing the session as live.
		s.Close()
		close(slot.done)
		return nil, fmt.Errorf("terminal: %s: closed while starting", spec.SessionID)
	}
	close(slot.done)
	return s, nil
}

// LogDir returns the directory per-session log files are written under
// (empty when disk logging is disabled). Used by the terminal log HTTP
// handler to locate a session's head/tail files after it has exited and is
// no longer in this manager's live table's memory-only scrollback.
func (m *Manager) LogDir() string { return m.cfg.LogDir }

// Get returns a terminal session by ID. A session still in the launching
// window (reserved, not yet spawned) is reported not-found — there is no
// *Session for it to return yet.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	slot, ok := m.sessions[id]
	if !ok || slot.session == nil {
		return nil, false
	}
	return slot.session, true
}

// ListSummary returns one Summary per fully-launched session, sorted by ID
// for stable iteration. A session still launching has no Summary yet.
func (m *Manager) ListSummary() []Summary {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Summary, 0, len(m.sessions))
	for _, slot := range m.sessions {
		if slot.session == nil {
			continue
		}
		s := slot.session
		state, exitCode := s.Snapshot()
		out = append(out, Summary{
			ID:         s.ID,
			TemplateID: s.TemplateID,
			Name:       s.Name,
			Directory:  s.Directory,
			State:      state,
			ExitCode:   exitCode,
			Host:       s.Host.Chip(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Write sends input to a terminal.
func (m *Manager) Write(id string, data []byte) error {
	s, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("terminal not found: %s", id)
	}
	return s.Write(data)
}

// Resize changes a terminal's PTY dimensions.
func (m *Manager) Resize(id string, cols, rows uint16) error {
	s, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("terminal not found: %s", id)
	}
	return s.Resize(cols, rows)
}

// Close kills a terminal and removes it from the manager. A terminal still
// in its launching window is marked stopping rather than deleted or
// signaled directly — there is no process yet to signal, and deleting the
// reservation here would let a concurrent Create race back in and spawn a
// second one under the same id. The Create goroutine that placed the
// reservation is the one that finishes the close once the spawn resolves.
func (m *Manager) Close(id string) {
	m.mu.Lock()
	slot, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	if slot.launching {
		slot.stopping = true
		m.mu.Unlock()
		return
	}
	delete(m.sessions, id)
	m.mu.Unlock()
	if slot.session != nil {
		slot.session.Close()
	}
}

// NotifyViewerChange is called when a terminal's viewer count changes: zero
// starts the idle timer, non-zero cancels it. Unknown ids are a no-op — a
// viewer racing a terminal's own close must not panic or resurrect an entry.
func (m *Manager) NotifyViewerChange(id string, viewers int) {
	s, ok := m.Get(id)
	if !ok {
		return
	}
	if viewers == 0 {
		s.StartIdleTimer()
	} else {
		s.CancelIdleTimer()
	}
}

// StopAll closes every running terminal, including one still spawning:
// a launch still in its Hello-wait window has no process yet for StopAll to
// signal directly, so it is marked stopping (mirroring Close) and StopAll
// waits on its done channel — bounded by startSession's own helloWait plus
// that Create's own teardown of whatever it finds — so shutdown does not
// return while a process it never accounted for is still alive or still
// being killed. Called during host shutdown.
func (m *Manager) StopAll() {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	pending := make([]chan struct{}, 0)
	for id, slot := range m.sessions {
		if slot.launching {
			slot.stopping = true
			pending = append(pending, slot.done)
			continue
		}
		sessions = append(sessions, slot.session)
		delete(m.sessions, id)
	}
	m.mu.Unlock()

	for _, s := range sessions {
		s.Close()
	}
	for _, done := range pending {
		<-done
	}
}
