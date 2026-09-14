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
	calls  []int
}

func newFakeSource() *fakeSource { return &fakeSource{queues: map[int][]ProcInfo{}} }

func (f *fakeSource) set(pid int, infos ...ProcInfo) *fakeSource {
	f.queues[pid] = infos
	return f
}

func (f *fakeSource) Info(pid int) (ProcInfo, bool) {
	f.calls = append(f.calls, pid)
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
	const peer = 100
	src := newFakeSource().set(peer, atInfo(peer, acceptedAt.Add(time.Second)))
	roots := newFakeRoots(999)

	id, ok := Resolve(src, roots, peer, acceptedAt)
	if ok {
		t.Fatalf("Resolve() = %q, true; want not-a-member (peer started after accept)", id)
	}
}

func TestResolve_DepthCap(t *testing.T) {
	src := newFakeSource()
	// Build a chain of 66 pids (peerPID=1000..1065), each older than the
	// last but none matching any root and never reaching pid<=1 or the
	// host, so only the depth cap can stop the walk.
	start := acceptedAt.Add(-time.Hour)
	for i := 0; i < 66; i++ {
		pid := 1000 + i
		next := 1000 + i + 1
		src.set(pid, withPPID(atInfo(pid, start.Add(-time.Duration(i)*time.Second)), next))
	}
	roots := newFakeRoots(999999)

	id, ok := Resolve(src, roots, 1000, acceptedAt)
	if ok {
		t.Fatalf("Resolve() = %q, true; want not-a-member (depth cap)", id)
	}
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

func TestResolve_NeverQueriesInitPID(t *testing.T) {
	const peer = 100
	src := newFakeSource().set(peer, withPPID(atInfo(peer, acceptedAt.Add(-time.Minute)), 1))
	roots := newFakeRoots(999)

	Resolve(src, roots, peer, acceptedAt)

	if src.sawPID(1) {
		t.Fatalf("Info(1) must never be called; pid 1 refuses proc_pidinfo to a non-root caller")
	}
}
