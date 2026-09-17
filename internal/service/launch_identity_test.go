package service

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/membership"
	"github.com/barelyworkingcode/relay/internal/peertoken"
)

// fakeWatchCall records one watchRoot invocation a test can later drive:
// call onExit to simulate the root process dying, or read cancelled to see
// whether Bind's caller (End) tore the watch down.
type fakeWatchCall struct {
	pid       int
	want      membership.ProcInfo
	onExit    func()
	cancelled bool
}

// fakeWatcher is the hermetic stand-in for membership.WatchExit: it never
// touches a real kqueue, and its Info reader and the outcome of a
// registration attempt are both plain fields the test drives directly.
type fakeWatcher struct {
	registerErr error
	calls       []*fakeWatchCall
}

func (f *fakeWatcher) watch(pid int, want membership.ProcInfo, onExit func()) (func(), error) {
	if f.registerErr != nil {
		return nil, f.registerErr
	}
	call := &fakeWatchCall{pid: pid, want: want, onExit: onExit}
	f.calls = append(f.calls, call)
	return func() { call.cancelled = true }, nil
}

// fakeRootSource is the hermetic stand-in for membership.NewSource(): a
// fixed pid -> ProcInfo table. A pid absent from it reports the same
// unreadable result a real proc_pidinfo failure would.
type fakeRootSource map[int]membership.ProcInfo

func (f fakeRootSource) Info(pid int) (membership.ProcInfo, bool) {
	info, ok := f[pid]
	return info, ok
}

// newProjectSessionTable wires a Launches table for hermetic project_session
// tests: a fixed clock, a fake root source and a fake watcher whose calls the
// test can inspect and drive, none of which ever touch a real process or
// kqueue.
func newProjectSessionTable(t *testing.T, src fakeRootSource, watcher *fakeWatcher) *Launches {
	t.Helper()
	table := NewLaunches()
	table.SetRootSourceForTest(src)
	table.SetRootWatcherForTest(watcher.watch)
	return table
}

func beginProjectSession(t *testing.T, table *Launches, name, projectID string) (string, *Launch) {
	t.Helper()
	secret, l, err := table.Begin(Identity{
		Kind: IdentityKindProjectSession, Name: name, SessionID: name,
		ProjectID: projectID, ParentLaunch: "relaysessions",
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return secret, l
}

func beginService(t *testing.T, table *Launches, name string, frontend bool) (string, *Launch) {
	t.Helper()
	secret, l, err := table.Begin(Identity{Kind: IdentityKindService, Name: name, Capabilities: capsFor(frontend)})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return secret, l
}

func TestLaunches_BeginIssuesA64HexSecret(t *testing.T) {
	secret, _ := beginService(t, NewLaunches(), "svc", false)
	if !isLaunchSecretShape(secret) {
		t.Fatalf("secret %q is not 64 lowercase hex characters", secret)
	}
}

func TestLaunches_BindRecordsTheProcessAndLookupFindsIt(t *testing.T) {
	table := NewLaunches()
	secret, _ := beginService(t, table, "svc", false)
	peer := peertoken.ForProcessForTest(500, 9)

	id, err := table.Bind("svc", secret, peer)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if id.Kind != IdentityKindService || id.Name != "svc" || !id.Allows(OpRegisterManifest) || id.Allows(OpFrontendSocket) {
		t.Fatalf("bound identity = %+v", id)
	}
	got, ok := table.Lookup(peer)
	if !ok || got.Process != peer.Process() {
		t.Fatalf("Lookup = %+v, %v", got, ok)
	}
}

func TestLaunches_AReusedPidWithANewPidversionIsNotTheService(t *testing.T) {
	table := NewLaunches()
	secret, _ := beginService(t, table, "svc", false)
	if _, err := table.Bind("svc", secret, peertoken.ForProcessForTest(500, 9)); err != nil {
		t.Fatal(err)
	}
	if _, ok := table.Lookup(peertoken.ForProcessForTest(500, 10)); ok {
		t.Fatal("a different pidversion under the same pid inherited the identity")
	}
}

func TestLaunches_WrongSecretIsRefusedAndDoesNotSpendTheLaunch(t *testing.T) {
	table := NewLaunches()
	secret, _ := beginService(t, table, "svc", false)
	forged := strings.Repeat("a", LaunchSecretHexLen)

	_, err := table.Bind("svc", forged, peertoken.ForProcessForTest(600, 1))
	if !errors.Is(err, ErrHelloRefused) {
		t.Fatalf("forged Bind: err = %v", err)
	}
	if strings.Contains(err.Error(), forged) || strings.Contains(err.Error(), secret) {
		t.Fatalf("refusal echoed a secret: %v", err)
	}
	if _, ok := table.Lookup(peertoken.ForProcessForTest(600, 1)); ok {
		t.Fatal("a forged Hello bound an identity")
	}
	if _, err := table.Bind("svc", secret, peertoken.ForProcessForTest(700, 1)); err != nil {
		t.Fatalf("the real secret was refused after a forgery: %v", err)
	}
}

func TestLaunches_SecondBindWithTheRightSecretIsRefused(t *testing.T) {
	table := NewLaunches()
	secret, _ := beginService(t, table, "svc", false)
	first := peertoken.ForProcessForTest(800, 1)
	if _, err := table.Bind("svc", secret, first); err != nil {
		t.Fatal(err)
	}
	second := peertoken.ForProcessForTest(801, 1)
	if _, err := table.Bind("svc", secret, second); !errors.Is(err, ErrHelloRefused) {
		t.Fatalf("second Bind: err = %v, want refused", err)
	}
	if _, ok := table.Lookup(second); ok {
		t.Fatal("the second presenter gained the identity")
	}
	if _, ok := table.Lookup(first); !ok {
		t.Fatal("the second attempt disturbed the first binding")
	}
}

func TestLaunches_RefusesMalformedSecretsUnknownNamesAndInvalidPeers(t *testing.T) {
	table := NewLaunches()
	secret, _ := beginService(t, table, "svc", false)
	peer := peertoken.ForProcessForTest(900, 1)
	for name, tc := range map[string]struct {
		name, secret string
		peer         peertoken.Token
	}{
		"uppercase":    {"svc", strings.ToUpper(secret), peer},
		"short":        {"svc", secret[:63], peer},
		"unknown name": {"other", secret, peer},
		"no peer":      {"svc", secret, peertoken.Token{}},
	} {
		if _, err := table.Bind(tc.name, tc.secret, tc.peer); !errors.Is(err, ErrHelloRefused) {
			t.Errorf("%s: err = %v, want refused", name, err)
		}
	}
}

func TestLaunches_OneProcessHoldsAtMostOneIdentity(t *testing.T) {
	table := NewLaunches()
	s1, _ := beginService(t, table, "one", false)
	s2, _ := beginService(t, table, "two", true)
	peer := peertoken.ForProcessForTest(1000, 1)
	if _, err := table.Bind("one", s1, peer); err != nil {
		t.Fatal(err)
	}
	if _, err := table.Bind("two", s2, peer); !errors.Is(err, ErrHelloRefused) {
		t.Fatalf("a second identity for the same process: err = %v", err)
	}
}

func TestLaunches_EndClearsTheIdentity(t *testing.T) {
	table := NewLaunches()
	secret, l := beginService(t, table, "svc", false)
	peer := peertoken.ForProcessForTest(1100, 1)
	if _, err := table.Bind("svc", secret, peer); err != nil {
		t.Fatal(err)
	}
	l.End()
	if _, ok := table.Lookup(peer); ok {
		t.Fatal("identity outlived its launch")
	}
	if _, ok := table.Bound("svc"); ok {
		t.Fatal("Bound still reports an ended launch")
	}
	if _, err := table.Bind("svc", secret, peertoken.ForProcessForTest(1101, 1)); !errors.Is(err, ErrHelloRefused) {
		t.Fatalf("an ended launch's secret still binds: %v", err)
	}
	l.End()
}

func TestLaunches_ARestartReplacesThePreviousLaunch(t *testing.T) {
	table := NewLaunches()
	oldSecret, oldLaunch := beginService(t, table, "svc", false)
	oldPeer := peertoken.ForProcessForTest(1200, 1)
	if _, err := table.Bind("svc", oldSecret, oldPeer); err != nil {
		t.Fatal(err)
	}
	newSecret, _ := beginService(t, table, "svc", false)
	if _, ok := table.Lookup(oldPeer); ok {
		t.Fatal("the previous launch's identity survived a new Begin")
	}
	if _, err := table.Bind("svc", oldSecret, oldPeer); err == nil {
		t.Fatal("the previous launch's secret binds the new launch")
	}
	newPeer := peertoken.ForProcessForTest(1201, 1)
	if _, err := table.Bind("svc", newSecret, newPeer); err != nil {
		t.Fatal(err)
	}
	oldLaunch.End()
	if _, ok := table.Lookup(newPeer); !ok {
		t.Fatal("ending the replaced launch cleared the new launch's identity")
	}
}

// --- C2: BindKind, TTL, EndByParent/EndByProject, RootByPID -----------------

func TestBindKind_AbsentKindStillBindsAnyKind(t *testing.T) {
	table := NewLaunches()
	secret, _ := beginService(t, table, "svc", false)
	id, err := table.BindKind("svc", secret, peertoken.ForProcessForTest(2000, 1), "")
	if err != nil {
		t.Fatalf("BindKind with no wantKind: %v", err)
	}
	if id.Kind != IdentityKindService {
		t.Fatalf("kind = %q", id.Kind)
	}
}

func TestBindKind_WrongKindRefusedAndDoesNotSpend(t *testing.T) {
	table := NewLaunches()
	secret, _ := beginService(t, table, "svc", false)
	peer := peertoken.ForProcessForTest(2001, 1)

	_, err := table.BindKind("svc", secret, peer, IdentityKindProjectSession)
	if !errors.Is(err, ErrHelloRefused) {
		t.Fatalf("BindKind with the wrong kind: err = %v, want refused", err)
	}
	if !strings.Contains(err.Error(), "svc") {
		t.Errorf("refusal should name the launch: %v", err)
	}
	if _, ok := table.Lookup(peer); ok {
		t.Fatal("a kind-mismatched Hello bound an identity")
	}
	// The secret must still work for the RIGHT kind: a wrong kind assertion
	// is a caller mistake, not a spend, same discipline as a wrong secret.
	if _, err := table.BindKind("svc", secret, peer, IdentityKindService); err != nil {
		t.Fatalf("the real kind was refused after a mismatched attempt: %v", err)
	}
}

func TestBindKind_MatchingKindBinds(t *testing.T) {
	table := NewLaunches()
	secret, _ := beginService(t, table, "svc", false)
	if _, err := table.BindKind("svc", secret, peertoken.ForProcessForTest(2002, 1), IdentityKindService); err != nil {
		t.Fatalf("BindKind with the matching kind: %v", err)
	}
}

func TestBeginWithTTL_UnboundLaunchExpiresAndIsGoneEverywhere(t *testing.T) {
	table := NewLaunches()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	table.SetClockForTest(func() time.Time { return now })

	secret, _, err := table.BeginWithTTL(Identity{Kind: IdentityKindService, Name: "svc"}, 30*time.Second)
	if err != nil {
		t.Fatalf("BeginWithTTL: %v", err)
	}
	if _, ok := table.Bound("svc"); ok {
		t.Fatal("an unbound launch reports as Bound")
	}

	now = start.Add(29 * time.Second)
	if _, err := table.Bind("svc", secret, peertoken.ForProcessForTest(2100, 1)); err != nil {
		t.Fatalf("bind just before the deadline: %v", err)
	}

	// A second launch, bound AFTER its deadline: the fake clock proves this
	// without a real 30-second sleep.
	now = start
	secret2, _, err := table.BeginWithTTL(Identity{Kind: IdentityKindService, Name: "svc2"}, 30*time.Second)
	if err != nil {
		t.Fatalf("BeginWithTTL svc2: %v", err)
	}
	now = start.Add(30 * time.Second) // not before the deadline: expired
	if _, err := table.Bind("svc2", secret2, peertoken.ForProcessForTest(2101, 1)); !errors.Is(err, ErrHelloRefused) {
		t.Fatalf("Bind after the TTL expired: err = %v, want refused", err)
	}
	if _, ok := table.Bound("svc2"); ok {
		t.Fatal("an expired launch reports as Bound")
	}
	if table.Len() != 1 { // svc (bound, never expires) survives; svc2 was reaped
		t.Fatalf("Len = %d, want 1 (only the bound launch survives)", table.Len())
	}
}

func TestBeginWithTTL_ABoundLaunchNeverExpires(t *testing.T) {
	table := NewLaunches()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	table.SetClockForTest(func() time.Time { return now })

	secret, _, err := table.BeginWithTTL(Identity{Kind: IdentityKindService, Name: "svc"}, 30*time.Second)
	if err != nil {
		t.Fatalf("BeginWithTTL: %v", err)
	}
	peer := peertoken.ForProcessForTest(2110, 1)
	if _, err := table.Bind("svc", secret, peer); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	now = start.Add(24 * time.Hour) // long past the original TTL
	if _, ok := table.Lookup(peer); !ok {
		t.Fatal("a bound launch expired long after its TTL had passed")
	}
	if _, ok := table.Bound("svc"); !ok {
		t.Fatal("Bound reports an expired-looking bound launch as gone")
	}
}

func TestBeginWithTTL_ZeroMeansNoDeadline(t *testing.T) {
	table := NewLaunches()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	table.SetClockForTest(func() time.Time { return now })

	secret, _, err := table.BeginWithTTL(Identity{Kind: IdentityKindService, Name: "svc"}, 0)
	if err != nil {
		t.Fatalf("BeginWithTTL: %v", err)
	}
	now = start.Add(365 * 24 * time.Hour)
	if _, err := table.Bind("svc", secret, peertoken.ForProcessForTest(2120, 1)); err != nil {
		t.Fatalf("a zero-TTL launch expired: %v", err)
	}
}

func TestEndByParent_EndsOnlyMatchingLiveProjectSessions(t *testing.T) {
	table := newProjectSessionTable(t, fakeRootSource{
		3000: {PID: 3000, StartSec: 1},
		3001: {PID: 3001, StartSec: 1},
		3002: {PID: 3002, StartSec: 1},
	}, &fakeWatcher{})

	s1, _ := beginProjectSession(t, table, "sess-1", "proj-a")
	s2, _ := beginProjectSession(t, table, "sess-2", "proj-b")
	s3secret, l3 := beginService(t, table, "unrelated-service", false)
	_ = s3secret

	if _, err := table.BindKind("sess-1", s1, peertoken.ForProcessForTest(3000, 1), IdentityKindProjectSession); err != nil {
		t.Fatalf("bind sess-1: %v", err)
	}
	if _, err := table.BindKind("sess-2", s2, peertoken.ForProcessForTest(3001, 1), IdentityKindProjectSession); err != nil {
		t.Fatalf("bind sess-2: %v", err)
	}

	ended, roots := table.EndByParent("relaysessions")
	if ended != 2 {
		t.Fatalf("EndByParent ended %d launches, want 2", ended)
	}
	if _, ok := table.Bound("sess-1"); ok {
		t.Fatal("sess-1 survived EndByParent")
	}
	if _, ok := table.Bound("sess-2"); ok {
		t.Fatal("sess-2 survived EndByParent")
	}
	if l3.ended {
		t.Fatal("EndByParent ended a non-project_session launch that happens to share no ParentLaunch")
	}

	wantRoots := map[int32]bool{3000: true, 3001: true}
	if len(roots) != len(wantRoots) {
		t.Fatalf("EndByParent returned %d roots, want %d: %+v", len(roots), len(wantRoots), roots)
	}
	for _, root := range roots {
		if !wantRoots[root.PID] {
			t.Fatalf("EndByParent returned an unexpected root pid %d: %+v", root.PID, roots)
		}
		if root.StartSec != 1 {
			t.Fatalf("root %d carries the wrong start time: %+v", root.PID, root)
		}
	}

	// Idempotent: nothing left to end.
	if ended, roots := table.EndByParent("relaysessions"); ended != 0 || len(roots) != 0 {
		t.Fatalf("a second EndByParent ended %d launches with %d roots, want 0 and 0", ended, len(roots))
	}
}

// TestEndByParent_UnboundSessionCountsButHasNoRoot pins the split between
// EndByParent's two return values: a project_session launch that began but
// never bound (no shim ever presented Hello) is still ended -- it must not
// linger as a live launch once its parent is gone -- but it contributes no
// entry to roots, since there is no real process to signal.
func TestEndByParent_UnboundSessionCountsButHasNoRoot(t *testing.T) {
	table := newProjectSessionTable(t, fakeRootSource{}, &fakeWatcher{})
	beginProjectSession(t, table, "sess-unbound", "proj-a")

	ended, roots := table.EndByParent("relaysessions")
	if ended != 1 {
		t.Fatalf("EndByParent ended %d launches, want 1", ended)
	}
	if len(roots) != 0 {
		t.Fatalf("an unbound session produced a root to signal: %+v", roots)
	}
}

// TestEndByParent_ReturnsMatchedRootOnSingleSession pins EndByParent's
// return shape for one bound project_session: the count and the returned
// root's pid and start time both match what was bound. EndByParent's own
// doc comment is where the race this exists to close is actually closed --
// by construction, one lock acquisition covering both the read and the end
// -- a property no single-threaded test can exercise directly; this test
// only pins the shape a caller sees, not the concurrent case.
func TestEndByParent_ReturnsMatchedRootOnSingleSession(t *testing.T) {
	table := newProjectSessionTable(t, fakeRootSource{
		4000: {PID: 4000, StartSec: 1},
	}, &fakeWatcher{})
	secret, _ := beginProjectSession(t, table, "sess-race", "proj-a")
	if _, err := table.BindKind("sess-race", secret, peertoken.ForProcessForTest(4000, 1), IdentityKindProjectSession); err != nil {
		t.Fatalf("bind sess-race: %v", err)
	}

	ended, roots := table.EndByParent("relaysessions")
	if ended != 1 || len(roots) != 1 || roots[0].PID != 4000 {
		t.Fatalf("EndByParent lost the bound session under its own lock: ended=%d roots=%+v", ended, roots)
	}
	if _, ok := table.Bound("sess-race"); ok {
		t.Fatal("sess-race survived EndByParent")
	}
}

func TestRootStillAlive_MatchesPIDAndStartTime(t *testing.T) {
	table := newProjectSessionTable(t, fakeRootSource{
		5000: {PID: 5000, StartSec: 100, StartUsec: 7},
	}, &fakeWatcher{})

	if !table.RootStillAlive(5000, 100, 7) {
		t.Fatal("a live pid with a matching start time was reported dead")
	}
	if table.RootStillAlive(5000, 100, 8) {
		t.Fatal("a mismatched StartUsec (a recycled pid) was reported alive")
	}
	if table.RootStillAlive(5000, 99, 7) {
		t.Fatal("a mismatched StartSec (a recycled pid) was reported alive")
	}
	if table.RootStillAlive(5001, 100, 7) {
		t.Fatal("a pid the kernel does not report was reported alive")
	}
}

func TestEndByProject_EndsOnlyThatProjectsSessions(t *testing.T) {
	table := newProjectSessionTable(t, fakeRootSource{
		3010: {PID: 3010, StartSec: 1},
		3011: {PID: 3011, StartSec: 1},
	}, &fakeWatcher{})

	sA, _ := beginProjectSession(t, table, "sess-a", "proj-a")
	sB, _ := beginProjectSession(t, table, "sess-b", "proj-b")
	if _, err := table.BindKind("sess-a", sA, peertoken.ForProcessForTest(3010, 1), IdentityKindProjectSession); err != nil {
		t.Fatalf("bind sess-a: %v", err)
	}
	if _, err := table.BindKind("sess-b", sB, peertoken.ForProcessForTest(3011, 1), IdentityKindProjectSession); err != nil {
		t.Fatalf("bind sess-b: %v", err)
	}

	if n := table.EndByProject("proj-a"); n != 1 {
		t.Fatalf("EndByProject(proj-a) ended %d, want 1", n)
	}
	if _, ok := table.Bound("sess-a"); ok {
		t.Fatal("sess-a survived EndByProject(proj-a)")
	}
	if _, ok := table.Bound("sess-b"); !ok {
		t.Fatal("EndByProject(proj-a) ended an unrelated project's session")
	}
}

func TestRootByPID_FindsOnlyABoundProjectSessionRoot(t *testing.T) {
	table := newProjectSessionTable(t, fakeRootSource{
		3020: {PID: 3020, StartSec: 42, StartUsec: 7},
	}, &fakeWatcher{})

	secret, _ := beginProjectSession(t, table, "sess-x", "proj-x")
	if _, ok := table.RootByPID(3020); ok {
		t.Fatal("RootByPID found a root before Bind ran")
	}
	if _, err := table.BindKind("sess-x", secret, peertoken.ForProcessForTest(3020, 1), IdentityKindProjectSession); err != nil {
		t.Fatalf("bind: %v", err)
	}

	root, ok := table.RootByPID(3020)
	if !ok {
		t.Fatal("RootByPID did not find the bound root")
	}
	if root.SessionID != "sess-x" || root.PID != 3020 || root.StartSec != 42 || root.StartUsec != 7 {
		t.Fatalf("root = %+v", root)
	}
	if _, ok := table.RootByPID(9999); ok {
		t.Fatal("RootByPID found a root for an unbound pid")
	}

	// A bound SERVICE identity, even sharing a pid space, is never a root.
	beginService(t, table, "svc-not-a-root", false)
	if _, err := table.Bind("svc-not-a-root", "", peertoken.Token{}); err == nil {
		t.Fatal("sanity: an empty secret must not bind")
	}
}

func TestBindKind_ProjectSession_UnreadableRootStartTimeRefuses(t *testing.T) {
	table := newProjectSessionTable(t, fakeRootSource{}, &fakeWatcher{}) // empty: every pid unreadable
	secret, _ := beginProjectSession(t, table, "sess-y", "proj-y")

	_, err := table.BindKind("sess-y", secret, peertoken.ForProcessForTest(3030, 1), IdentityKindProjectSession)
	if !errors.Is(err, ErrHelloRefused) {
		t.Fatalf("err = %v, want refused", err)
	}
	if _, ok := table.Bound("sess-y"); ok {
		t.Fatal("a launch with an unreadable root start time still bound")
	}
}

func TestBindKind_ProjectSession_WatchRegistrationFailureRefuses(t *testing.T) {
	watcher := &fakeWatcher{registerErr: membership.ErrExited}
	table := newProjectSessionTable(t, fakeRootSource{3040: {PID: 3040, StartSec: 1}}, watcher)
	secret, _ := beginProjectSession(t, table, "sess-z", "proj-z")

	_, err := table.BindKind("sess-z", secret, peertoken.ForProcessForTest(3040, 1), IdentityKindProjectSession)
	if !errors.Is(err, ErrHelloRefused) {
		t.Fatalf("err = %v, want refused", err)
	}
	if _, ok := table.Bound("sess-z"); ok {
		t.Fatal("a launch whose ancestry watch failed to register still bound")
	}
}

func TestBindKind_ProjectSession_RootExitEndsTheLaunch(t *testing.T) {
	watcher := &fakeWatcher{}
	table := newProjectSessionTable(t, fakeRootSource{3050: {PID: 3050, StartSec: 1}}, watcher)
	secret, _ := beginProjectSession(t, table, "sess-exit", "proj-exit")
	peer := peertoken.ForProcessForTest(3050, 1)

	if _, err := table.BindKind("sess-exit", secret, peer, IdentityKindProjectSession); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if len(watcher.calls) != 1 {
		t.Fatalf("watch calls = %d, want 1", len(watcher.calls))
	}

	watcher.calls[0].onExit() // simulate the root process dying

	if _, ok := table.Lookup(peer); ok {
		t.Fatal("the identity outlived its root's simulated exit")
	}
	if _, ok := table.Bound("sess-exit"); ok {
		t.Fatal("Bound still reports a launch whose root exited")
	}
}

func TestBindKind_ProjectSession_EndCancelsTheWatch(t *testing.T) {
	watcher := &fakeWatcher{}
	table := newProjectSessionTable(t, fakeRootSource{3060: {PID: 3060, StartSec: 1}}, watcher)
	secret, l := beginProjectSession(t, table, "sess-cancel", "proj-cancel")
	peer := peertoken.ForProcessForTest(3060, 1)

	if _, err := table.BindKind("sess-cancel", secret, peer, IdentityKindProjectSession); err != nil {
		t.Fatalf("bind: %v", err)
	}
	l.End()
	if len(watcher.calls) != 1 || !watcher.calls[0].cancelled {
		t.Fatalf("ending the launch did not cancel its ancestry watch: %+v", watcher.calls)
	}
}

func TestBindKind_ProjectSession_SecondHelloWithTheRightSecretIsRefused(t *testing.T) {
	table := newProjectSessionTable(t, fakeRootSource{
		3080: {PID: 3080, StartSec: 1},
	}, &fakeWatcher{})
	secret, _ := beginProjectSession(t, table, "sess-twice", "proj-twice")

	first := peertoken.ForProcessForTest(3080, 1)
	if _, err := table.BindKind("sess-twice", secret, first, IdentityKindProjectSession); err != nil {
		t.Fatalf("first Hello: %v", err)
	}
	second := peertoken.ForProcessForTest(3081, 1)
	_, err := table.BindKind("sess-twice", secret, second, IdentityKindProjectSession)
	if !errors.Is(err, ErrHelloRefused) {
		t.Fatalf("second Hello: err = %v, want refused", err)
	}
	if _, ok := table.Lookup(second); ok {
		t.Fatal("the second presenter gained the identity")
	}
	if _, ok := table.Lookup(first); !ok {
		t.Fatal("the second attempt disturbed the first binding")
	}
}

func TestBindKind_ProjectSession_WrongSecretDoesNotSpend(t *testing.T) {
	table := newProjectSessionTable(t, fakeRootSource{
		3090: {PID: 3090, StartSec: 1},
		3091: {PID: 3091, StartSec: 1},
	}, &fakeWatcher{})
	secret, _ := beginProjectSession(t, table, "sess-forged", "proj-forged")
	forged := strings.Repeat("f", LaunchSecretHexLen)

	_, err := table.BindKind("sess-forged", forged, peertoken.ForProcessForTest(3090, 1), IdentityKindProjectSession)
	if !errors.Is(err, ErrHelloRefused) {
		t.Fatalf("forged Bind: err = %v", err)
	}
	// The real secret must still work — a forged attempt must not have spent it.
	if _, err := table.BindKind("sess-forged", secret, peertoken.ForProcessForTest(3091, 1), IdentityKindProjectSession); err != nil {
		t.Fatalf("the real secret was refused after a forgery: %v", err)
	}
}

// TestProjectSessionLaunchTTL_Is30Seconds pins the literal value C2 names,
// so a change to it is a deliberate edit to this constant, not a silent
// drift the TTL-expiry test above wouldn't catch (that test uses its own
// literal 30*time.Second so it stays meaningful even if this constant were
// ever wrong).
// TestBegin_ProjectSessionGetsTheTTLAutomatically pins the fix for a real
// footgun: a caller reaching for plain Begin (not BeginWithTTL) for a
// project_session identity must still get ProjectSessionLaunchTTL, not a
// launch that waits unbound forever.
func TestBegin_ProjectSessionGetsTheTTLAutomatically(t *testing.T) {
	table := newProjectSessionTable(t, fakeRootSource{}, &fakeWatcher{})
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	table.SetClockForTest(func() time.Time { return now })

	secret, _, err := table.Begin(Identity{Kind: IdentityKindProjectSession, Name: "sess-auto-ttl", ProjectID: "proj-x", ParentLaunch: "relaysessions"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	now = start.Add(ProjectSessionLaunchTTL) // not before the deadline: expired
	if _, err := table.Bind("sess-auto-ttl", secret, peertoken.ForProcessForTest(3100, 1)); !errors.Is(err, ErrHelloRefused) {
		t.Fatalf("Bind after the automatic TTL elapsed: err = %v, want refused", err)
	}
}

// TestBegin_ProjectSessionGetsNoAutomaticTTLForAServiceIdentity is the
// companion check: the automatic TTL is project_session-specific, not a
// change to Begin's behavior for the service kind every existing launch
// depends on.
func TestBegin_ProjectSessionGetsNoAutomaticTTLForAServiceIdentity(t *testing.T) {
	table := NewLaunches()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	table.SetClockForTest(func() time.Time { return now })

	secret, _, err := table.Begin(Identity{Kind: IdentityKindService, Name: "svc-no-ttl"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	now = start.Add(ProjectSessionLaunchTTL + time.Hour)
	if _, err := table.Bind("svc-no-ttl", secret, peertoken.ForProcessForTest(3101, 1)); err != nil {
		t.Fatalf("a service launch expired: %v", err)
	}
}

// TestBegin_ProjectSessionDefaultsSessionIDToName pins the fix for a
// fail-open trap: RootByPID reads Identity.SessionID, not Name, and a
// caller that sets Name but forgets SessionID must not leave RootByPID
// (and, downstream, C3's membership walk) resolving a member into an empty
// session id.
func TestBegin_ProjectSessionDefaultsSessionIDToName(t *testing.T) {
	table := newProjectSessionTable(t, fakeRootSource{3110: {PID: 3110, StartSec: 1}}, &fakeWatcher{})
	secret, l, err := table.Begin(Identity{Kind: IdentityKindProjectSession, Name: "sess-no-explicit-id", ProjectID: "proj-y", ParentLaunch: "relaysessions"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if l.id.SessionID != "sess-no-explicit-id" {
		t.Fatalf("SessionID = %q, want it defaulted to Name", l.id.SessionID)
	}
	if _, err := table.BindKind("sess-no-explicit-id", secret, peertoken.ForProcessForTest(3110, 1), IdentityKindProjectSession); err != nil {
		t.Fatalf("bind: %v", err)
	}
	root, ok := table.RootByPID(3110)
	if !ok || root.SessionID != "sess-no-explicit-id" {
		t.Fatalf("RootByPID = %+v, ok=%v, want SessionID defaulted to the launch name, never empty", root, ok)
	}
}

func TestProjectSessionLaunchTTL_Is30Seconds(t *testing.T) {
	if ProjectSessionLaunchTTL != 30*time.Second {
		t.Fatalf("ProjectSessionLaunchTTL = %v, want 30s", ProjectSessionLaunchTTL)
	}
}

func TestBindKind_ProjectSession_ServiceIdentityNeverConsultsAncestry(t *testing.T) {
	watcher := &fakeWatcher{}
	table := newProjectSessionTable(t, fakeRootSource{}, watcher) // no pid readable
	secret, _ := beginService(t, table, "svc-plain", false)

	if _, err := table.Bind("svc-plain", secret, peertoken.ForProcessForTest(3070, 1)); err != nil {
		t.Fatalf("a plain service Bind consulted ancestry and failed: %v", err)
	}
	if len(watcher.calls) != 0 {
		t.Fatal("a service identity's Bind registered an ancestry watch")
	}
}
