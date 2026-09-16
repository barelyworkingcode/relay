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

// Manager owns the live set of terminal sessions this host is hosting.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	cfg      Config
	onExit   func(id string, exitCode int)
}

// NewManager constructs a Manager. cfg.ShimBinary must be set before Create
// is ever called.
func NewManager(cfg Config) *Manager {
	return &Manager{sessions: make(map[string]*Session), cfg: cfg}
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
// id is already live or stopped-but-present in this manager's table.
func (m *Manager) Create(spec CreateSpec) (*Session, error) {
	m.mu.Lock()
	if _, exists := m.sessions[spec.SessionID]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrSessionExists, spec.SessionID)
	}
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
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.sessions[spec.SessionID] = s
	m.mu.Unlock()
	return s, nil
}

// LogDir returns the directory per-session log files are written under
// (empty when disk logging is disabled). Used by the terminal log HTTP
// handler to locate a session's head/tail files after it has exited and is
// no longer in this manager's live table's memory-only scrollback.
func (m *Manager) LogDir() string { return m.cfg.LogDir }

// Get returns a terminal session by ID.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

// ListSummary returns one Summary per session, sorted by ID for stable
// iteration.
func (m *Manager) ListSummary() []Summary {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Summary, 0, len(m.sessions))
	for _, s := range m.sessions {
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

// Close kills a terminal and removes it from the manager.
func (m *Manager) Close(id string) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	if ok {
		s.Close()
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

// StopAll closes every running terminal. Called during host shutdown.
func (m *Manager) StopAll() {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.sessions = make(map[string]*Session)
	m.mu.Unlock()

	for _, s := range sessions {
		s.Close()
	}
}
