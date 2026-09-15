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

// sessionEntry is one launched session's bookkeeping. rootPID/rootStart pin
// the exact process instance the way C2's Bind pins a launch identity's
// root — a bare pid is reusable and must never be trusted alone.
//
// rootPID is the shim's own pid, not the target's (SH §4.2: "the shim is
// the session root" — see internal/sessions/shim's package doc). This is
// what relay's own launch identity binds to as well, since the shim is
// whoever said Hello (C6 step 3); host and relay must agree on one
// convention for C5's SessionExited{root_pid} to reconcile correctly once
// R-S4b wires it up. targetPID is kept only as an extra, best-effort signal
// target for /terminate (F6) — it is never consulted for membership.
type sessionEntry struct {
	id        string
	state     sessionState
	shimPID   int
	targetPID int
	rootStart membership.ProcInfo
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
	// byRootPID mirrors byID's live entries, keyed by shimPID, so
	// rootByPID — called once per hop of every /permission's C3 ancestry
	// walk (up to membership's own depth cap) — is O(1) rather than an O(n)
	// scan under this same lock.
	byRootPID map[int]*sessionEntry
}

func newSessionTable() *sessionTable {
	return &sessionTable{
		byID:      make(map[string]*sessionEntry),
		byRootPID: make(map[int]*sessionEntry),
	}
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
	if e.state == stateLive && e.shimPID != 0 {
		t.byRootPID[e.shimPID] = e
	}
}

// setLive atomically records a session's root once its shim has started:
// state, shimPID, targetPID and rootStart all change together under one
// critical section, so no reader — in particular rootByPID, consulted by
// every in-flight /permission call — can ever observe stateLive with a
// still-zero rootStart (that half-set window was a real gap; a hostile
// racing membership check landing in it would have failed closed today
// only by accident of rootStart's zero value never matching a real
// process's start time).
func (t *sessionTable) setLive(id string, shimPID, targetPID int, rootStart membership.ProcInfo) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.byID[id]
	if !ok {
		return
	}
	e.state = stateLive
	e.shimPID = shimPID
	e.targetPID = targetPID
	e.rootStart = rootStart
	t.byRootPID[shimPID] = e
}

func (t *sessionTable) markEnded(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.byID[id]
	if !ok {
		return
	}
	e.state = stateEnded
	if e.shimPID != 0 {
		delete(t.byRootPID, e.shimPID)
	}
}

// rootByPID implements membership.Roots.RootByPID: only a session in state
// stateLive is a valid membership root — a launching session has no root
// pid yet, and an ended one's pid may already have been recycled by the
// kernel for something else. markEnded's own removal from byRootPID keeps
// this map from ever answering for a pid whose session already ended.
func (t *sessionTable) rootByPID(pid int) (membership.Root, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.byRootPID[pid]
	if !ok || e.state != stateLive {
		return membership.Root{}, false
	}
	return membership.Root{
		SessionID: e.id,
		PID:       e.shimPID,
		StartSec:  e.rootStart.StartSec,
		StartUsec: e.rootStart.StartUsec,
	}, true
}

// rootsAdapter satisfies membership.Roots by delegating to a sessionTable
// plus this host process's own pid, the walk's other stop condition.
type rootsAdapter struct{ table *sessionTable }

func (r rootsAdapter) RootByPID(pid int) (membership.Root, bool) { return r.table.rootByPID(pid) }
func (r rootsAdapter) HostPID() int                              { return os.Getpid() }
