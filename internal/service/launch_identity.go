package service

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/membership"
	"github.com/barelyworkingcode/relay/internal/peertoken"
)

// IdentityKind names what a bound launch identity is, and so what it may do.
// The protocol (docs/launch-identity.md) is the same for every kind; only the
// record relay binds, and the capability lookup that reads it, differ.
type IdentityKind string

const (
	// IdentityKindService is a process relay's service registry launched.
	// Its capabilities are the service record's capabilities.
	IdentityKindService IdentityKind = "service"
	// IdentityKindProjectSession is a project-bound session's root process —
	// a `relay-sessions exec` shim, per plan-broker-and-sessions.md C2 and
	// C6. Its authority is the named project's own live grant, not a fixed
	// capability set. ParentLaunch always names the relaysessions launch
	// that started it; RootStartSec/RootStartUsec pin the exact process
	// instance Bind observed, for C3's membership walk (R-S2a) to match
	// descendants against.
	IdentityKindProjectSession IdentityKind = "project_session"
)

// ProjectSessionLaunchTTL is how long an unbound project_session launch may
// sit waiting for its shim's Hello before Begin's caller must treat it as
// gone (plan-broker-and-sessions.md §2 C2). Enforced by BeginWithTTL, not
// automatically by Begin — a caller that wants the old "never expires while
// unbound" behavior for a service launch keeps calling Begin.
const ProjectSessionLaunchTTL = 30 * time.Second

// Identity is what relay knows about one launch: who it launched, and once
// Hello succeeds, which process presented the launch secret.
type Identity struct {
	Kind IdentityKind
	// Name is the launch's name on the wire: Hello's "name". For a service it
	// is the service id; for a project_session it is the session id (the
	// same value SessionID carries — Hello's OK data reuses it as
	// "service_id" so an existing Go client checking ServiceID == name keeps
	// working).
	Name string
	// Capabilities is fixed when the launch begins; editing the service record
	// changes the next launch, not this one. Empty and unused for a
	// project_session identity — its authority is ProjectID's live grant.
	Capabilities []config.ServiceCapability

	// ProjectID names the project a project_session identity is bound to.
	// Empty for a service identity.
	ProjectID string
	// SessionID is the session id a project_session identity was launched
	// for. Always equal to Name for that kind; kept as its own field because
	// callers that reason about sessions (audit, C3's Roots.RootByPID)
	// should not have to know that a launch's wire name happens to double as
	// one.
	SessionID string
	// ParentLaunch is always "relaysessions" for a project_session identity:
	// the launch name of the relay-sessions host that started it, so
	// EndByParent can find every session a given host launch owns.
	ParentLaunch string
	// RootStartSec/RootStartUsec are the root process's kernel start time, as
	// read at Bind. Zero until bound. C3's membership walk (R-S2a) matches a
	// candidate root by pid AND this exact start time — a pid number alone is
	// reused too often to trust.
	RootStartSec  int64
	RootStartUsec int32

	// Process is the zero value until Hello binds the launch.
	Process peertoken.Process
}

// Allows reports whether this identity may perform op.
func (i Identity) Allows(op Operation) bool {
	return Allowed(i.Kind, i.Capabilities, op)
}

// LaunchSecretHexLen is the length of the launch secret on the wire: 32
// random bytes, lowercase hex.
const LaunchSecretHexLen = 64

// ErrHelloRefused is the one error every refused Hello returns. The reason is
// wrapped for relay's own log; the bridge answers every refusal identically so
// a caller cannot learn which launches exist or whether its guess was close.
var ErrHelloRefused = errors.New("hello refused")

// Launches is the table of live launches and the identities bound to them.
// The service registry begins and ends launches, the bridge binds them at
// Hello, and both the bridge router and the frontend server look callers up
// by peer audit token.
type Launches struct {
	mu     sync.Mutex
	byName map[string]*Launch
	bound  map[peertoken.Process]*Launch

	// clock, rootSource and watchRoot are test seams; their zero-argument
	// production values are wired in NewLaunches. rootSource and watchRoot
	// back a project_session Bind's ancestry pinning (C2): internal/membership
	// already exists in this tree (R-M0 landed before this unit started), so
	// this wires the real thing rather than stubbing it — see the SetXxxForTest
	// setters' doc comments for exactly what is real and what a test may
	// override.
	clock      func() time.Time
	rootSource membership.Source
	watchRoot  func(pid int, want membership.ProcInfo, onExit func()) (cancel func(), err error)
}

// Launch is one launch's record. Its handle is what ends it.
type Launch struct {
	table *Launches
	id    Identity
	// secretHash rather than the secret: relay needs to recognise the secret,
	// never to produce it again after writing it into the child's pipe.
	secretHash [sha256.Size]byte
	spent      bool
	ended      bool
	// deadline is when an unbound launch is treated as never having existed.
	// The zero Time means no deadline — Begin's own, unchanged behavior.
	deadline time.Time
	// rootWatchCancel cancels the C2 ancestry watch registered at Bind for a
	// project_session launch. nil for every other kind, and nil until Bind
	// succeeds. Cancelling on every End path (endLocked) means no watch
	// outlives the launch it was registered for, whatever ends it first.
	rootWatchCancel func()
}

func NewLaunches() *Launches {
	return &Launches{
		byName:     make(map[string]*Launch),
		bound:      make(map[peertoken.Process]*Launch),
		clock:      time.Now,
		rootSource: membership.NewSource(),
		watchRoot:  membership.WatchExit,
	}
}

// SetClockForTest overrides the clock BeginWithTTL and the reap-on-access
// check read, so a TTL expiry test never sleeps in real time.
func (t *Launches) SetClockForTest(now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.clock = now
}

// SetRootSourceForTest overrides the ancestry Source a project_session Bind
// reads the root's start time from. Production wires membership.NewSource()
// (a real proc_pidinfo(PROC_PIDTBSDINFO) call); a test that cannot fabricate
// a real process instead controls what Bind believes the kernel reported.
func (t *Launches) SetRootSourceForTest(src membership.Source) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rootSource = src
}

// SetRootWatcherForTest overrides the ancestry-exit watch a project_session
// Bind registers. Production wires membership.WatchExit (a real kqueue
// EVFILT_PROC watch); a test controls when — or whether — the root is
// reported to have exited, and can capture onExit to fire it deterministically.
func (t *Launches) SetRootWatcherForTest(watch func(pid int, want membership.ProcInfo, onExit func()) (cancel func(), err error)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.watchRoot = watch
}

// Begin records a new launch named id.Name and returns its single-use secret.
// A launch already recorded under that name is ended first: a name has at
// most one live launch, so a restart can never leave the previous process's
// identity standing beside the new one's.
//
// For a project_session identity this applies ProjectSessionLaunchTTL
// automatically (C2's 30s default) — a caller has no deadline to remember.
// Every other kind gets BeginWithTTL's no-deadline behavior: an unbound
// launch waits forever, what every existing service launch depends on. A
// caller that genuinely needs a different project_session deadline (a test,
// or a future policy) calls BeginWithTTL directly instead.
func (t *Launches) Begin(id Identity) (string, *Launch, error) {
	var ttl time.Duration
	if id.Kind == IdentityKindProjectSession {
		ttl = ProjectSessionLaunchTTL
	}
	return t.BeginWithTTL(id, ttl)
}

// BeginWithTTL is Begin plus an explicit expiry: once ttl has passed with
// the launch still unbound, it is treated as never having existed — Bind,
// Lookup and Bound all refuse it, the same as after End. ttl <= 0 means no
// deadline. A launch that DID bind before its deadline never expires: the
// deadline only ever governs the window before Hello.
func (t *Launches) BeginWithTTL(id Identity, ttl time.Duration) (string, *Launch, error) {
	if id.Name == "" {
		return "", nil, errors.New("launch identity: empty name")
	}
	if id.Kind == "" {
		return "", nil, errors.New("launch identity: empty kind")
	}
	// This is subtle: SessionID always equals Name for a project_session
	// identity (Hello's OK data reuses Name as "service_id" — see Identity's
	// doc comment), but RootByPID reads SessionID specifically, not Name. A
	// caller that sets Name and forgets SessionID would otherwise leave
	// RootByPID returning a live root under an empty session id — fail-OPEN
	// into a nonsense identifier, not fail-closed, once C3's membership walk
	// (R-S2a) starts trusting that value. Defaulting it here, once, removes
	// the chance to forget it at any Begin call site.
	if id.Kind == IdentityKindProjectSession && id.SessionID == "" {
		id.SessionID = id.Name
	}
	secret, err := GenerateRandomHex(LaunchSecretHexLen / 2)
	if err != nil {
		return "", nil, fmt.Errorf("launch identity: %w", err)
	}
	id.Process = peertoken.Process{}
	id.Capabilities = slices.Clone(id.Capabilities)
	l := &Launch{table: t, id: id, secretHash: sha256.Sum256([]byte(secret))}

	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.clock()
	if ttl > 0 {
		l.deadline = now.Add(ttl)
	}
	t.reapExpiredLocked(now)
	if prev := t.byName[id.Name]; prev != nil {
		t.endLocked(prev)
	}
	t.byName[id.Name] = l
	return secret, l, nil
}

// End clears this launch and any identity bound to it. Safe to call more
// than once, and a no-op for a launch a later Begin already replaced.
func (l *Launch) End() {
	if l == nil {
		return
	}
	l.table.mu.Lock()
	defer l.table.mu.Unlock()
	l.table.endLocked(l)
}

func (t *Launches) endLocked(l *Launch) {
	if l.ended {
		return
	}
	l.ended = true
	if l.rootWatchCancel != nil {
		// Safe to call from any goroutine, including from inside the onExit
		// this very cancel might race (membership.WatchExit's documented
		// contract) — endLocked runs both when End is called directly and
		// from inside a root-exit onExit callback that calls End itself.
		l.rootWatchCancel()
	}
	if t.byName[l.id.Name] == l {
		delete(t.byName, l.id.Name)
	}
	if l.spent && t.bound[l.id.Process] == l {
		delete(t.bound, l.id.Process)
	}
}

// reapExpiredLocked ends every unbound launch whose deadline has passed.
// Called at the top of every method that reads or writes byName/bound, under
// t.mu — there is no background sweeper (see reapExpiredAPICredentials in
// internal/config for the same discipline and the same reason: a timer is a
// writer nothing asked for).
func (t *Launches) reapExpiredLocked(now time.Time) {
	for _, l := range t.byName {
		if !l.spent && !l.deadline.IsZero() && !now.Before(l.deadline) {
			t.endLocked(l)
		}
	}
}

// Bind is Hello: it binds the launch named name to peer if secret is that
// launch's secret. On success the secret is spent and no later Bind for the
// same launch succeeds, whoever presents it. Equivalent to BindKind with no
// kind assertion.
//
// A wrong secret does not spend the launch. This is deliberate: spending on a
// miss would let any same-user process that can name a service turn that
// service's start into a failure by guessing once, while a guess against 256
// bits buys nothing.
func (t *Launches) Bind(name, secret string, peer peertoken.Token) (Identity, error) {
	return t.BindKind(name, secret, peer, "")
}

// BindKind is Bind plus an optional wire-level kind assertion
// (plan-broker-and-sessions.md §2 C2): Hello may name the kind it expects to
// bind. An empty wantKind skips the check (every service launch that
// predates this field, and every launch a caller doesn't care to assert
// about). A non-empty wantKind that does not match the kind recorded at
// Begin refuses before the secret is spent — same reasoning as a wrong
// secret: naming the wrong kind is a caller's mistake, not a credential
// presentation, and must not burn the one legitimate Hello.
//
// For a project_session launch, a successful bind also pins the root
// process's exact start time (RootStartSec/RootStartUsec) and registers an
// ancestry-exit watch that ends this launch when the root process dies —
// both read from t.rootSource/t.watchRoot, which is membership.NewSource()/
// membership.WatchExit in production. A failure to read the root's start
// time, or to register the watch (including the process having already
// exited: membership.ErrExited), refuses the Hello and leaves the launch
// unbound and unspent, exactly like a wrong secret.
func (t *Launches) BindKind(name, secret string, peer peertoken.Token, wantKind IdentityKind) (Identity, error) {
	if !peer.Valid() {
		return Identity{}, fmt.Errorf("%w: peer audit token unavailable", ErrHelloRefused)
	}
	if !isLaunchSecretShape(secret) {
		return Identity{}, fmt.Errorf("%w: malformed secret", ErrHelloRefused)
	}
	presented := sha256.Sum256([]byte(secret))
	proc := peer.Process()

	t.mu.Lock()
	defer t.mu.Unlock()
	t.reapExpiredLocked(t.clock())

	l := t.byName[name]
	if l == nil {
		return Identity{}, fmt.Errorf("%w: no live launch named %q", ErrHelloRefused, name)
	}
	if wantKind != "" && l.id.Kind != wantKind {
		return Identity{}, fmt.Errorf("%w: launch %q is kind %q, not %q", ErrHelloRefused, name, l.id.Kind, wantKind)
	}
	if subtle.ConstantTimeCompare(presented[:], l.secretHash[:]) != 1 {
		return Identity{}, fmt.Errorf("%w: secret does not match launch %q", ErrHelloRefused, name)
	}
	if l.spent {
		return Identity{}, fmt.Errorf("%w: launch %q is already bound", ErrHelloRefused, name)
	}
	if other := t.bound[proc]; other != nil {
		return Identity{}, fmt.Errorf("%w: process %d already holds identity %q", ErrHelloRefused, proc.PID, other.id.Name)
	}

	var cancel func()
	if l.id.Kind == IdentityKindProjectSession {
		info, ok := t.rootSource.Info(int(proc.PID))
		if !ok {
			return Identity{}, fmt.Errorf("%w: could not read the root process's start time", ErrHelloRefused)
		}
		c, err := t.watchRoot(int(proc.PID), info, func() { l.End() })
		if err != nil {
			if errors.Is(err, membership.ErrExited) {
				// This is deliberate: ErrExited is not "the watch failed", it
				// is "the process this launch is for is already gone" — the
				// pid died mid-Hello, or was recycled between the start-time
				// read above and the registration. Refusing the Hello alone
				// would leave the launch live and unbound for the rest of its
				// TTL, with a secret that is still spendable by whoever else
				// holds it. There is no session left to protect, so the
				// launch ends here rather than waiting to expire.
				t.endLocked(l)
			}
			return Identity{}, fmt.Errorf("%w: root process ancestry watch: %v", ErrHelloRefused, err)
		}
		cancel = c
		l.id.RootStartSec = info.StartSec
		l.id.RootStartUsec = info.StartUsec
	}

	l.spent = true
	l.id.Process = proc
	l.rootWatchCancel = cancel
	t.bound[proc] = l
	return l.id, nil
}

// Lookup returns the identity bound to the process peer names, if any.
func (t *Launches) Lookup(peer peertoken.Token) (Identity, bool) {
	if t == nil || !peer.Valid() {
		return Identity{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reapExpiredLocked(t.clock())
	l := t.bound[peer.Process()]
	if l == nil {
		return Identity{}, false
	}
	return l.id, true
}

// Bound returns the identity bound to the live launch named name, if Hello
// has bound one.
func (t *Launches) Bound(name string) (Identity, bool) {
	if t == nil {
		return Identity{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reapExpiredLocked(t.clock())
	l := t.byName[name]
	if l == nil || !l.spent {
		return Identity{}, false
	}
	return l.id, true
}

// Len reports how many launches are live, bound or not.
func (t *Launches) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reapExpiredLocked(t.clock())
	return len(t.byName)
}

// EndByParent ends every live project_session launch whose ParentLaunch is
// name, and reports how many it ended. Meant to run from the relaysessions
// launch's own End (R-S9 wires this): when the host process that launched
// every session under it goes away, those sessions' identities go with it.
// R-S9 also SIGKILLs each root's process group — this function only clears
// the identity table's view of them, since Launches has no process-group
// bookkeeping of its own.
func (t *Launches) EndByParent(name string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.endWhereLocked(func(l *Launch) bool {
		return l.id.Kind == IdentityKindProjectSession && l.id.ParentLaunch == name
	})
}

// EndByProject ends every live project_session launch bound to projectID,
// and reports how many it ended. Meant to run on project delete.
func (t *Launches) EndByProject(projectID string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.endWhereLocked(func(l *Launch) bool {
		return l.id.Kind == IdentityKindProjectSession && l.id.ProjectID == projectID
	})
}

// endWhereLocked ends every launch match selects and reports how many.
// Collected into a slice first: endLocked mutates byName, and ranging over a
// map while deleting from it inside the loop body is undefined in general —
// safe in Go's actual implementation for the key being visited, but not for
// this loop, which may end OTHER entries as a side effect of ending one
// (a future unit's cleanup hooks) and must not depend on that being safe.
func (t *Launches) endWhereLocked(match func(*Launch) bool) int {
	var matched []*Launch
	for _, l := range t.byName {
		if !l.ended && match(l) {
			matched = append(matched, l)
		}
	}
	for _, l := range matched {
		t.endLocked(l)
	}
	return len(matched)
}

// RootByPID returns the live, bound project_session identity whose root
// process is pid, shaped as a membership.Root for C3's Roots interface
// (RootByPID(pid) (Root, bool); HostPID() int — R-S2a supplies HostPID by
// wrapping this table, since a launch has no notion of relay's own pid).
func (t *Launches) RootByPID(pid int) (membership.Root, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for proc, l := range t.bound {
		if int(proc.PID) != pid || l.id.Kind != IdentityKindProjectSession {
			continue
		}
		return membership.Root{
			SessionID: l.id.SessionID,
			PID:       int(proc.PID),
			StartSec:  l.id.RootStartSec,
			StartUsec: l.id.RootStartUsec,
		}, true
	}
	return membership.Root{}, false
}

func isLaunchSecretShape(s string) bool {
	if len(s) != LaunchSecretHexLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
