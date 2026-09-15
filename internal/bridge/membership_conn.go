package bridge

import (
	"context"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/internal/peertoken"
)

// MemberSession is the session a connection's peer was proved to be a
// descendant of (plan-broker-and-sessions.md §2 C3, spec-session-host.md
// §4.3). Root* pins the exact process instance that was the session's root
// when the walk matched: a session id alone is not enough to re-check
// liveness against, because ending a session and starting another under the
// same id would otherwise read as "still live".
type MemberSession struct {
	SessionID string
	// ProjectID is re-read from the live launch record on every request, so
	// this field is only ever as old as the request reading it.
	ProjectID     string
	RootPID       int
	RootStartSec  int64
	RootStartUsec int32
}

// MembershipResolver answers C3 for one peer. The bridge holds only the
// per-connection caching discipline; everything that knows what a session is
// — the launch table, the ancestry walk, relay's own pid — lives behind this
// interface, in the package that owns those things.
type MembershipResolver interface {
	// ResolveMembership performs the ancestry walk once, for a peer that
	// presented no token and holds no launch identity of its own. acceptedAt
	// is when the connection the peer is on was accepted; the walk refuses a
	// peer that did not already exist at that moment.
	ResolveMembership(peer peertoken.Token, acceptedAt time.Time) (MemberSession, bool)
	// RefreshMembership re-reads the session s names and reports whether the
	// SAME session instance is still live, returning its current record. A
	// session that ended, or whose id now names a different root process,
	// answers false.
	RefreshMembership(s MemberSession) (MemberSession, bool)
}

// ConnMembership is one connection's C3 answer: computed at most once, on
// the first request that asks, and re-checked for liveness on every request
// after that (C3's "Cache" rule). A nil *ConnMembership answers "not a
// member", so a connection that never got one — no resolver wired, no
// readable peer — needs no special case at any call site.
type ConnMembership struct {
	resolver   MembershipResolver
	peer       peertoken.Token
	acceptedAt time.Time

	mu       sync.Mutex
	computed bool
	// session is the zero value both before the walk runs and after it
	// answered "not a member" — computed is what tells those apart, and is
	// what guarantees the walk runs at most once per connection whatever it
	// answered.
	session MemberSession
}

// NewConnMembership returns nil — the "not a member, ever" value — when
// there is no resolver to ask or no kernel-attested peer to ask about.
func NewConnMembership(resolver MembershipResolver, peer peertoken.Token, acceptedAt time.Time) *ConnMembership {
	if resolver == nil || !peer.Valid() {
		return nil
	}
	return &ConnMembership{resolver: resolver, peer: peer, acceptedAt: acceptedAt}
}

// Session reports the live session this connection's peer is a member of.
//
// This is deliberate: an ended session is answered from the cache as "not a
// member" and never recomputed (C3's cache rule). Recomputing would be worse
// than useless — the peer's ancestry has not changed, so a walk would either
// reach the same dead root or, if the root's pid were meanwhile reused by an
// unrelated new session, match a session this connection was never part of.
func (c *ConnMembership) Session() (MemberSession, bool) {
	if c == nil {
		return MemberSession{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.computed {
		c.computed = true
		if s, ok := c.resolver.ResolveMembership(c.peer, c.acceptedAt); ok {
			c.session = s
		}
	}
	if c.session.SessionID == "" {
		return MemberSession{}, false
	}
	live, ok := c.resolver.RefreshMembership(c.session)
	if !ok {
		// Forgotten rather than merely reported gone, so a later request on
		// this connection cannot re-ask a question whose answer is already
		// decided. computed stays true, so this is not a recompute.
		c.session = MemberSession{}
		return MemberSession{}, false
	}
	return live, true
}

type connMembershipCtxKey struct{}

// WithConnMembership carries one connection's membership cache. Storing nil
// is meaningful and normal: it is the value a connection with no resolvable
// peer gets, and ConnMembershipFromContext hands it straight back.
func WithConnMembership(ctx context.Context, m *ConnMembership) context.Context {
	return context.WithValue(ctx, connMembershipCtxKey{}, m)
}

// ConnMembershipFromContext returns nil for a context that never carried one
// — an in-process call, a remote caller, a test's bare context — and nil
// answers "not a member" rather than panicking.
func ConnMembershipFromContext(ctx context.Context) *ConnMembership {
	m, _ := ctx.Value(connMembershipCtxKey{}).(*ConnMembership)
	return m
}
