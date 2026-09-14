// Package membership answers one question: is a process at the far end of a
// tokenless bridge connection a descendant of a known session root? It walks
// process ancestry using kernel-reported pid/ppid/start-time triples, never
// anything a peer asserts about itself.
//
// See spec-session-host.md §4.3 and plan-broker-and-sessions.md contract C3
// for the algorithm this package implements, and spikes/SP5.md for the
// real-process verification behind its ordering rules.
package membership

import (
	"log/slog"
	"time"
)

// ProcInfo is one process's ancestry-relevant state, read from the kernel.
type ProcInfo struct {
	PID       int
	PPID      int
	StartSec  int64
	StartUsec int32
}

// Source reads a single process's ProcInfo. The darwin implementation calls
// proc_pidinfo(PROC_PIDTBSDINFO); every other platform's Source reports
// every pid as unreadable, so Resolve always answers not-a-member there.
type Source interface {
	Info(pid int) (ProcInfo, bool)
}

// Root is a live session root: the process whose descendants are members of
// SessionID. PID plus the exact start time is what a walk must match — a pid
// number alone is reused too often to trust.
type Root struct {
	SessionID string
	PID       int
	StartSec  int64
	StartUsec int32
}

// Roots is the caller's live root table: which pids are session roots right
// now, and which pid is relay's own host process (the walk's other stop
// condition, alongside pid 1).
type Roots interface {
	RootByPID(pid int) (Root, bool)
	HostPID() int
}

// maxDepth bounds the ancestry walk. A real process tree never needs
// anywhere near this many hops; the cap exists so a Source bug or a
// deliberately falsified ancestry chain can't spin Resolve forever.
const maxDepth = 64

// procTime is a (sec, usec) pair, compared as a single ordered value.
type procTime struct {
	sec  int64
	usec int32
}

func startOf(info ProcInfo) procTime { return procTime{info.StartSec, info.StartUsec} }

func timeOf(t time.Time) procTime {
	return procTime{t.Unix(), int32(t.Nanosecond() / 1000)}
}

func (a procTime) lessOrEqual(b procTime) bool {
	if a.sec != b.sec {
		return a.sec < b.sec
	}
	return a.usec <= b.usec
}

func (a procTime) less(b procTime) bool {
	if a.sec != b.sec {
		return a.sec < b.sec
	}
	return a.usec < b.usec
}

func (a procTime) equal(b procTime) bool { return a == b }

// Resolve walks the ancestry of peerPID, starting from the process itself,
// and reports the session it is a member of, if any. acceptedAt is the time
// the bridge accepted the connection peerPID was read from.
//
// Any failure — an unreadable pid, the depth cap, an ordering violation, or
// a root match that doesn't survive the post-match recheck — resolves to
// ("", false). The caller answers the standard unauthorized error in every
// case; there is no partial or fallback membership.
func Resolve(src Source, roots Roots, peerPID int, acceptedAt time.Time) (sessionID string, ok bool) {
	acceptedTime := timeOf(acceptedAt)

	p := peerPID
	var bound *procTime // ancestor.start <= child.start; nil means unbounded (peer itself)
	var peerStart procTime

	for depth := 1; ; depth++ {
		if depth > maxDepth {
			reject("depth-exceeded", peerPID, p, "")
			return "", false
		}

		// This is subtle: the pid<=1/HostPID stop is checked on the pid
		// value alone, before Info(p) is ever called for it. proc_pidinfo
		// refuses PID 1 (launchd) to a non-root caller, so reading its start
		// time to decide whether to stop would itself fail — and a failed
		// Info() is indistinguishable from "not a member" for the wrong
		// reason. SP5 verified this ordering against a real launchd.
		if p <= 1 || p == roots.HostPID() {
			reject("reached-host-or-init", peerPID, p, "")
			return "", false
		}

		info, found := src.Info(p)
		if !found {
			reject("info-failed", peerPID, p, "")
			return "", false
		}
		cur := startOf(info)

		if bound != nil && !cur.lessOrEqual(*bound) {
			// This is deliberate: a genuine ancestor never starts after its
			// child. A pid that appears to is a different, later process
			// that reused the number after the "child" already existed —
			// treat that as a broken chain, not a coincidence.
			reject("recycled-pid", peerPID, p, "")
			return "", false
		}

		if p == peerPID {
			if !cur.less(acceptedTime) {
				reject("peer-too-new", peerPID, p, "")
				return "", false
			}
			peerStart = cur
		}

		if root, isRoot := roots.RootByPID(p); isRoot && cur.equal(procTime{root.StartSec, root.StartUsec}) {
			// This is subtle: re-read the peer's own start time before
			// trusting the match. The walk above already spent time doing
			// syscalls; if the peer pid exited and was recycled in that
			// window, the earlier reads describe a process that no longer
			// exists at that pid.
			recheck, stillThere := src.Info(peerPID)
			if !stillThere || !startOf(recheck).equal(peerStart) {
				reject("peer-start-changed", peerPID, p, root.SessionID)
				return "", false
			}
			return root.SessionID, true
		}

		bound = &cur
		p = info.PPID
	}
}

func reject(reason string, peerPID, stoppedAt int, sessionID string) {
	args := []any{"reason", reason, "peer_pid", peerPID, "stopped_at_pid", stoppedAt}
	if sessionID != "" {
		args = append(args, "session_id", sessionID)
	}
	// Never logged above debug: a connection failing membership is routine
	// (most tokenless callers on a shared bridge socket are not members of
	// anything), not an operational problem.
	slog.Debug("membership: not a member", args...)
}
