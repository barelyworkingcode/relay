package hostapi

import (
	"os"
	"sync"

	"github.com/barelyworkingcode/relay/internal/membership"
)

// sessionState is deliberately just enough to answer /launch, /terminate and
// /permission — not the real session lifecycle (dormant/resume/ledger) C5
// describes, which is R-S4a/R-S7c's job.
type sessionState int

const (
	stateLaunching sessionState = iota
	stateLive
	stateEnded
)

// sessionEntry is one launched session's bookkeeping. RootPID/RootStart pin
// the exact target process instance the way C2's Bind pins a launch
// identity's root — a bare pid is reusable and must never be trusted alone.
type sessionEntry struct {
	id          string
	state       sessionState
	shimPID     int
	rootPID     int
	rootStart   membership.ProcInfo
	cancelWatch func()
}

// sessionTable is the host's in-memory session table. It is also the
// membership.Roots this package's /permission handler resolves against
// (rootsAdapter, below): the plan's own C3 section names "the host's own
// session table" as exactly what the hook socket checks membership against,
// so there is no separate stub type here — the real table doubles as the
// Roots implementation.
type sessionTable struct {
	mu   sync.Mutex
	byID map[string]*sessionEntry
}

func newSessionTable() *sessionTable {
	return &sessionTable{byID: make(map[string]*sessionEntry)}
}

func (t *sessionTable) get(id string) (*sessionEntry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.byID[id]
	return e, ok
}

func (t *sessionTable) exists(id string) bool {
	_, ok := t.get(id)
	return ok
}

func (t *sessionTable) put(e *sessionEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.byID[e.id] = e
}

func (t *sessionTable) markEnded(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.byID[id]; ok {
		e.state = stateEnded
	}
}

// rootByPID implements membership.Roots.RootByPID: only a session in state
// stateLive is a valid membership root — a launching session has no root
// pid yet, and an ended one's pid may already have been recycled by the
// kernel for something else.
func (t *sessionTable) rootByPID(pid int) (membership.Root, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range t.byID {
		if e.state == stateLive && e.rootPID == pid {
			return membership.Root{
				SessionID: e.id,
				PID:       e.rootPID,
				StartSec:  e.rootStart.StartSec,
				StartUsec: e.rootStart.StartUsec,
			}, true
		}
	}
	return membership.Root{}, false
}

// rootsAdapter satisfies membership.Roots by delegating to a sessionTable
// plus this host process's own pid, the walk's other stop condition.
type rootsAdapter struct{ table *sessionTable }

func (r rootsAdapter) RootByPID(pid int) (membership.Root, bool) { return r.table.rootByPID(pid) }
func (r rootsAdapter) HostPID() int                              { return os.Getpid() }
