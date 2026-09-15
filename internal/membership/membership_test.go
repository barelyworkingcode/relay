package membership

import (
	"testing"
	"time"
)

// fakeSource is a table of canned ProcInfo answers, keyed by pid. Each
// lookup pops the first queued answer for that pid and keeps returning it
// once the queue is down to one entry — most tests want a stable answer,
// and the ones that don't (the recheck-changed case) queue two.
type fakeSource struct {
	queues map[int][]ProcInfo
	vanish map[int]int // pid -> call count after which Info reports !ok
	calls  []int
}

func newFakeSource() *fakeSource { return &fakeSource{queues: map[int][]ProcInfo{}} }

func (f *fakeSource) set(pid int, infos ...ProcInfo) *fakeSource {
	f.queues[pid] = infos
	return f
}

// vanishAfter makes Info report !ok for pid once it has been queried more
// than n times — for a "there, then gone" pid, as opposed to set's
// "there, then differently" queue.
func (f *fakeSource) vanishAfter(pid, n int) *fakeSource {
	if f.vanish == nil {
		f.vanish = map[int]int{}
	}
	f.vanish[pid] = n
	return f
}

func (f *fakeSource) Info(pid int) (ProcInfo, bool) {
	f.calls = append(f.calls, pid)
	if n, limited := f.vanish[pid]; limited && f.callCount(pid) > n {
		return ProcInfo{}, false
	}
	q, ok := f.queues[pid]
	if !ok || len(q) == 0 {
		return ProcInfo{}, false
	}
	info := q[0]
	if len(q) > 1 {
		f.queues[pid] = q[1:]
	}
	return info, true
}

func (f *fakeSource) callCount(pid int) int {
	n := 0
	for _, p := range f.calls {
		if p == pid {
			n++
		}
	}
	return n
}

func (f *fakeSource) sawPID(pid int) bool {
	for _, p := range f.calls {
		if p == pid {
			return true
		}
	}
	return false
}

// fakeRoots is a static live-root table plus a fixed host pid.
type fakeRoots struct {
	roots   map[int]Root
	hostPID int
}

func newFakeRoots(hostPID int) *fakeRoots {
	return &fakeRoots{roots: map[int]Root{}, hostPID: hostPID}
}

func (r *fakeRoots) add(root Root) *fakeRoots {
	r.roots[root.PID] = root
	return r
}

func (r *fakeRoots) RootByPID(pid int) (Root, bool) {
	root, ok := r.roots[pid]
	return root, ok
}

func (r *fakeRoots) HostPID() int { return r.hostPID }

var acceptedAt = time.Unix(1_000_000, 0)

func atInfo(pid int, t time.Time) ProcInfo {
	return ProcInfo{PID: pid, StartSec: t.Unix(), StartUsec: int32(t.Nanosecond() / 1000)}
}

func TestResolve_DirectChild(t *testing.T) {
	const child, root = 100, 200
	src := newFakeSource().
		set(child, withPPID(atInfo(child, acceptedAt.Add(-time.Minute)), root)).
		set(root, atInfo(root, acceptedAt.Add(-time.Hour)))
	roots := newFakeRoots(999).add(Root{SessionID: "sessA", PID: root, StartSec: acceptedAt.Add(-time.Hour).Unix()})

	id, ok := Resolve(src, roots, child, acceptedAt)
	if !ok || id != "sessA" {
		t.Fatalf("Resolve() = %q, %v; want sessA, true", id, ok)
	}
}

func TestResolve_Grandchild(t *testing.T) {
	const grandchild, mid, root = 100, 150, 200
	src := newFakeSource().
		set(grandchild, withPPID(atInfo(grandchild, acceptedAt.Add(-time.Minute)), mid)).
		set(mid, withPPID(atInfo(mid, acceptedAt.Add(-2*time.Minute)), root)).
		set(root, atInfo(root, acceptedAt.Add(-time.Hour)))
	roots := newFakeRoots(999).add(Root{SessionID: "sessA", PID: root, StartSec: acceptedAt.Add(-time.Hour).Unix()})

	id, ok := Resolve(src, roots, grandchild, acceptedAt)
	if !ok || id != "sessA" {
		t.Fatalf("Resolve() = %q, %v; want sessA, true", id, ok)
	}
}

func withPPID(info ProcInfo, ppid int) ProcInfo {
	info.PPID = ppid
	return info
}

func TestResolve_ReparentedToPid1(t *testing.T) {
	const peer = 100
	src := newFakeSource().set(peer, withPPID(atInfo(peer, acceptedAt.Add(-time.Minute)), 1))
	roots := newFakeRoots(999)

	id, ok := Resolve(src, roots, peer, acceptedAt)
	if ok {
		t.Fatalf("Resolve() = %q, true; want not-a-member", id)
	}
	if src.sawPID(1) {
		t.Fatalf("Info(1) was called; the pid<=1 stop must be checked before Info")
	}
}

func TestResolve_HostPIDStop(t *testing.T) {
	const peer, host = 100, 555
	src := newFakeSource().set(peer, withPPID(atInfo(peer, acceptedAt.Add(-time.Minute)), host))
	roots := newFakeRoots(host)

	id, ok := Resolve(src, roots, peer, acceptedAt)
	if ok {
		t.Fatalf("Resolve() = %q, true; want not-a-member", id)
	}
	if src.sawPID(host) {
		t.Fatalf("Info(hostPID) was called; the HostPID stop must be checked before Info")
	}
}

func TestResolve_RecycledPid_AncestorStartsLater(t *testing.T) {
	const peer, ancestor = 100, 200
	// The alleged ancestor's start time is AFTER the child's: a different
	// process now sits at that pid.
	src := newFakeSource().
		set(peer, withPPID(atInfo(peer, acceptedAt.Add(-time.Hour)), ancestor)).
		set(ancestor, atInfo(ancestor, acceptedAt.Add(-time.Minute)))
	roots := newFakeRoots(999).add(Root{SessionID: "sessA", PID: ancestor, StartSec: acceptedAt.Add(-time.Minute).Unix()})

	id, ok := Resolve(src, roots, peer, acceptedAt)
	if ok {
		t.Fatalf("Resolve() = %q, true; want not-a-member (recycled pid)", id)
	}
}

func TestResolve_StartTimeTieAccepted(t *testing.T) {
	const peer, root = 100, 200
	tie := acceptedAt.Add(-time.Hour)
	src := newFakeSource().
		set(peer, withPPID(atInfo(peer, tie), root)).
		set(root, atInfo(root, tie))
	roots := newFakeRoots(999).add(Root{SessionID: "sessA", PID: root, StartSec: tie.Unix(), StartUsec: int32(tie.Nanosecond() / 1000)})

	id, ok := Resolve(src, roots, peer, acceptedAt)
	if !ok || id != "sessA" {
		t.Fatalf("Resolve() = %q, %v; want sessA, true (equal start times must be accepted)", id, ok)
	}
}

func TestResolve_PeerStartedAfterAccept(t *testing.T) {
	// The peer has a real, reachable root as its parent: if the acceptedAt
	// check were skipped or broken, Resolve would wrongly accept. Refusal
	// here can only come from the "peer existed before accept" check.
	const peer, root = 100, 200
	rootStart := acceptedAt.Add(-time.Hour)
	roots := newFakeRoots(999).add(Root{
		SessionID: "sessA", PID: root,
		StartSec: rootStart.Unix(), StartUsec: int32(rootStart.Nanosecond() / 1000),
	})

	cases := []struct {
		name  string
		start time.Time
	}{
		{"after accept", acceptedAt.Add(time.Second)},
		{"exactly at accept", acceptedAt}, // boundary: strict less-than, not less-or-equal
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := newFakeSource().
				set(peer, withPPID(atInfo(peer, c.start), root)).
				set(root, atInfo(root, rootStart))

			id, ok := Resolve(src, roots, peer, acceptedAt)
			if ok {
				t.Fatalf("Resolve() = %q, true; want not-a-member (peer start %s)", id, c.name)
			}
		})
	}
}

// buildChain builds a fake ancestry of n pids, each older than the last,
// pids[0] the peer and pids[n-1] the oldest (top) ancestor. It sets no root
// and leaves pids[n-1]'s ppid at zero; callers add whatever root they need.
func buildChain(n int, newest time.Time) (src *fakeSource, pids []int) {
	src = newFakeSource()
	pids = make([]int, n)
	for i := range pids {
		pids[i] = 5000 + i
	}
	for i, pid := range pids {
		start := newest.Add(-time.Duration(i) * time.Second)
		info := atInfo(pid, start)
		if i < n-1 {
			info = withPPID(info, pids[i+1])
		}
		src.set(pid, info)
	}
	return src, pids
}

func TestResolve_DepthCap(t *testing.T) {
	newest := acceptedAt.Add(-time.Hour)

	t.Run("root at depth 64 accepted", func(t *testing.T) {
		const n = 64
		src, pids := buildChain(n, newest)
		rootStart := newest.Add(-time.Duration(n-1) * time.Second)
		roots := newFakeRoots(999999).add(Root{
			SessionID: "sessDeep", PID: pids[n-1],
			StartSec: rootStart.Unix(), StartUsec: int32(rootStart.Nanosecond() / 1000),
		})

		id, ok := Resolve(src, roots, pids[0], acceptedAt)
		if !ok || id != "sessDeep" {
			t.Fatalf("Resolve() = %q, %v; want sessDeep, true (a root at exactly depth 64 must be reachable)", id, ok)
		}
	})

	t.Run("root at depth 65 refused", func(t *testing.T) {
		const n = 65
		src, pids := buildChain(n, newest)
		rootStart := newest.Add(-time.Duration(n-1) * time.Second)
		roots := newFakeRoots(999999).add(Root{
			SessionID: "sessTooDeep", PID: pids[n-1],
			StartSec: rootStart.Unix(), StartUsec: int32(rootStart.Nanosecond() / 1000),
		})

		id, ok := Resolve(src, roots, pids[0], acceptedAt)
		if ok {
			t.Fatalf("Resolve() = %q, true; want not-a-member (root sits one hop past the depth cap)", id)
		}
	})
}

func TestResolve_InfoFailureMidWalk(t *testing.T) {
	const peer, mid = 100, 150
	src := newFakeSource().
		set(peer, withPPID(atInfo(peer, acceptedAt.Add(-time.Minute)), mid))
	// mid is deliberately absent from the table: src.Info(mid) reports !ok.
	roots := newFakeRoots(999)

	id, ok := Resolve(src, roots, peer, acceptedAt)
	if ok {
		t.Fatalf("Resolve() = %q, true; want not-a-member (Info failure)", id)
	}
}

func TestResolve_PeerStartChangedOnRecheck(t *testing.T) {
	const peer = 100
	original := atInfo(peer, acceptedAt.Add(-time.Minute))
	changed := atInfo(peer, acceptedAt.Add(-30*time.Second))
	// peer matches a root directly (depth 1): the match triggers an
	// immediate second Info(peer) call, which this queue answers
	// differently from the first.
	src := newFakeSource().set(peer, original, changed)
	roots := newFakeRoots(999).add(Root{SessionID: "sessA", PID: peer, StartSec: original.StartSec, StartUsec: original.StartUsec})

	id, ok := Resolve(src, roots, peer, acceptedAt)
	if ok {
		t.Fatalf("Resolve() = %q, true; want not-a-member (peer start changed on recheck)", id)
	}
}

func TestResolve_NonMatchingRootPIDContinuesWalk(t *testing.T) {
	// midStale sits at a pid that a (now-dead) root once used, but its
	// actual, current start time doesn't match that stale root record. A
	// pid-only match must not short-circuit the walk here — it has to keep
	// climbing and find the real, currently-live root above it.
	const peer, midStale, realRoot = 100, 150, 200
	realRootStart := acceptedAt.Add(-time.Hour)
	staleRootStart := acceptedAt.Add(-3 * time.Hour)
	src := newFakeSource().
		set(peer, withPPID(atInfo(peer, acceptedAt.Add(-time.Minute)), midStale)).
		set(midStale, withPPID(atInfo(midStale, acceptedAt.Add(-2*time.Minute)), realRoot)).
		set(realRoot, atInfo(realRoot, realRootStart))
	roots := newFakeRoots(999).
		add(Root{SessionID: "stale", PID: midStale, StartSec: staleRootStart.Unix()}).
		add(Root{SessionID: "sessReal", PID: realRoot, StartSec: realRootStart.Unix(), StartUsec: int32(realRootStart.Nanosecond() / 1000)})

	id, ok := Resolve(src, roots, peer, acceptedAt)
	if !ok || id != "sessReal" {
		t.Fatalf("Resolve() = %q, %v; want sessReal, true (a pid match with the wrong start must not stop the walk)", id, ok)
	}
}

func TestResolve_RecheckPeerGone(t *testing.T) {
	const peer, root = 100, 200
	rootStart := acceptedAt.Add(-time.Hour)
	src := newFakeSource().
		set(peer, withPPID(atInfo(peer, acceptedAt.Add(-time.Minute)), root)).
		set(root, atInfo(root, rootStart)).
		// The main walk's read of peer succeeds; the post-match recheck
		// read (the 2nd) finds it gone entirely, not merely different.
		vanishAfter(peer, 1)
	roots := newFakeRoots(999).add(Root{SessionID: "sessA", PID: root, StartSec: rootStart.Unix()})

	id, ok := Resolve(src, roots, peer, acceptedAt)
	if ok {
		t.Fatalf("Resolve() = %q, true; want not-a-member (peer vanished before the recheck)", id)
	}
}

func TestResolve_PeerStartChangedOnRecheck_DepthGreaterThanOne(t *testing.T) {
	// The existing depth-1 version of this case has the peer match a root
	// directly, so its single Info(peer) read at depth 1 and its recheck
	// read are the only two ever made for that pid. This variant matches at
	// depth 2, proving the recheck still fires when the peer isn't the pid
	// that matched.
	const peer, root = 100, 200
	original := atInfo(peer, acceptedAt.Add(-time.Minute))
	changed := atInfo(peer, acceptedAt.Add(-30*time.Second))
	rootStart := acceptedAt.Add(-time.Hour)
	src := newFakeSource().
		set(peer, withPPID(original, root), withPPID(changed, root)).
		set(root, atInfo(root, rootStart))
	roots := newFakeRoots(999).add(Root{SessionID: "sessA", PID: root, StartSec: rootStart.Unix()})

	id, ok := Resolve(src, roots, peer, acceptedAt)
	if ok {
		t.Fatalf("Resolve() = %q, true; want not-a-member (peer start changed on recheck, depth 2 match)", id)
	}
}

func TestResolve_NeverQueriesInitPID(t *testing.T) {
	const peer = 100
	src := newFakeSource().set(peer, withPPID(atInfo(peer, acceptedAt.Add(-time.Minute)), 1))
	roots := newFakeRoots(999)

	Resolve(src, roots, peer, acceptedAt)

	if src.sawPID(1) {
		t.Fatalf("Info(1) must never be called; pid 1 refuses proc_pidinfo to a non-root caller")
	}
}
