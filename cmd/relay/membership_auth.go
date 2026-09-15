package main

import (
	"os"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/membership"
	"github.com/barelyworkingcode/relay/internal/peertoken"
	"github.com/barelyworkingcode/relay/internal/service"
)

// launchRoots is C3's Roots view of the launch table: which pids are live
// session roots right now, plus relay's own pid as the walk's stop
// condition. It holds no state of its own — every answer is read from
// service.Launches at the moment it is asked, so a session that ended one
// instruction ago is already gone from it.
type launchRoots struct {
	launches *service.Launches
	hostPID  int
}

var _ membership.Roots = launchRoots{}

func (l launchRoots) RootByPID(pid int) (membership.Root, bool) {
	if l.launches == nil {
		return membership.Root{}, false
	}
	return l.launches.RootByPID(pid)
}

// HostPID is relay's own pid, not the relay-sessions host's. The walk stops
// there because relay is the ancestor of every process it launches: a walk
// that reached it has left the session tree entirely and is climbing relay's
// own, where everything above is shared by every session and belongs to
// none.
func (l launchRoots) HostPID() int { return l.hostPID }

// membershipAuth is relay's implementation of C3 (plan-broker-and-sessions.md
// §2 C3, spec-session-host.md §4.3): the ancestry walk plus the liveness
// re-check the per-connection cache calls on every request.
//
// Deliberately a value type with no cached state. Every field is either a
// table that answers live (launches) or a stateless reader (src, hostPID),
// so there is no second copy of "which sessions exist" that could disagree
// with the launch table.
type membershipAuth struct {
	launches *service.Launches
	// src reads a process's ppid and start time. membership.NewSource() in
	// production; a test injects a table instead of spawning real processes.
	src     membership.Source
	hostPID int
}

var _ bridge.MembershipResolver = membershipAuth{}

func newMembershipAuth(launches *service.Launches) membershipAuth {
	return membershipAuth{launches: launches, src: membership.NewSource(), hostPID: os.Getpid()}
}

// ResolveMembership walks peer's ancestry once. Every failure — an
// unreadable pid, a peer that did not exist when its own connection was
// accepted, a root match that does not survive the walk's own recheck, a
// session that ended between the walk and this lookup — answers false, and
// the caller answers the standard unauthorized error. There is no partial
// membership and nothing to fall back to.
func (m membershipAuth) ResolveMembership(peer peertoken.Token, acceptedAt time.Time) (bridge.MemberSession, bool) {
	if m.launches == nil || m.src == nil || !peer.Valid() {
		return bridge.MemberSession{}, false
	}
	sessionID, ok := membership.Resolve(m.src, launchRoots{launches: m.launches, hostPID: m.hostPID}, int(peer.PID()), acceptedAt)
	if !ok {
		return bridge.MemberSession{}, false
	}
	return m.session(sessionID)
}

// RefreshMembership re-reads the session and requires the SAME root process
// instance still to be live under that id.
//
// This is subtle: comparing the pinned root pid and start time, rather than
// only asking whether a session with this id exists, is what stops an ended
// session's authority from being inherited. Session ids are relay-minted
// UUIDs, so a collision is not the threat — a launch under a reused NAME is
// (service.Launches.Begin replaces a live launch of the same name), and that
// replacement is a different session the peer was never proved to descend
// from.
func (m membershipAuth) RefreshMembership(s bridge.MemberSession) (bridge.MemberSession, bool) {
	live, ok := m.session(s.SessionID)
	if !ok {
		return bridge.MemberSession{}, false
	}
	if live.RootPID != s.RootPID || live.RootStartSec != s.RootStartSec || live.RootStartUsec != s.RootStartUsec {
		return bridge.MemberSession{}, false
	}
	return live, true
}

// session reads the live, bound project_session identity named sessionID.
//
// Looked up by launch name: a project_session launch's name and its session
// id are the same string by construction (service.Launches.BeginWithTTL
// defaults one from the other), and the SessionID check below refuses rather
// than guesses if that ever stops being true — a record found under the
// wrong name is not a record this walk's answer identified.
func (m membershipAuth) session(sessionID string) (bridge.MemberSession, bool) {
	if sessionID == "" {
		return bridge.MemberSession{}, false
	}
	id, ok := m.launches.Bound(sessionID)
	if !ok || id.Kind != service.IdentityKindProjectSession || id.SessionID != sessionID || id.ProjectID == "" {
		return bridge.MemberSession{}, false
	}
	return bridge.MemberSession{
		SessionID:     id.SessionID,
		ProjectID:     id.ProjectID,
		RootPID:       int(id.Process.PID),
		RootStartSec:  id.RootStartSec,
		RootStartUsec: id.RootStartUsec,
	}, true
}

// membershipAuth returns the resolver this router answers C3 with. The
// override exists so a test can inject a Source table in place of real
// process ancestry; production builds one from the live launch table and
// relay's own pid every time, since both are free to read and neither can
// go stale that way.
func (r *appRouter) membershipAuth() membershipAuth {
	if r.membership != nil {
		return *r.membership
	}
	return newMembershipAuth(r.launches)
}

// ResolveMembership and RefreshMembership make *appRouter the
// bridge.MembershipResolver the bridge server picks up (see
// bridge.NewBridgeServer). The compile-time assertion below is what makes
// that pickup safe: drop either method and the build fails here rather than
// leaving a shipped relay that silently refuses every session member.
func (r *appRouter) ResolveMembership(peer peertoken.Token, acceptedAt time.Time) (bridge.MemberSession, bool) {
	return r.membershipAuth().ResolveMembership(peer, acceptedAt)
}

func (r *appRouter) RefreshMembership(s bridge.MemberSession) (bridge.MemberSession, bool) {
	return r.membershipAuth().RefreshMembership(s)
}

var _ bridge.MembershipResolver = (*appRouter)(nil)
