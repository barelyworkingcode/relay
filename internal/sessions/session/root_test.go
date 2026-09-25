package session_test

import (
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/session"
	"github.com/barelyworkingcode/relay/internal/sessions/testutil"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// rootedProvider reports a fixed process root, and none once killed, per the
// RootReporter contract.
type rootedProvider struct {
	*fakeProvider
	root sessionstypes.ProcessRoot
}

func (p *rootedProvider) ProcessRoot() (sessionstypes.ProcessRoot, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.root, !p.killed
}

func newRooted(pid int) *rootedProvider {
	return &rootedProvider{
		fakeProvider: &fakeProvider{},
		root:         sessionstypes.ProcessRoot{PID: pid, StartSec: 1_700_000_000 + int64(pid), StartUsec: 42},
	}
}

const (
	rootSessA = "aaaaaaaa-0000-0000-0000-000000000001"
	rootSessB = "aaaaaaaa-0000-0000-0000-000000000002"
)

func createWith(t *testing.T, mgr *session.Manager, id string, p sessionstypes.Provider) *sessionstypes.Session {
	t.Helper()
	mgr.SetProviderFactory(factoryReturning(p))
	sess, err := mgr.Create(session.CreateSpec{SessionID: id, ProjectID: "proj-1", Kind: session.KindClaude})
	if err != nil {
		t.Fatalf("Create %s: %v", id, err)
	}
	return sess
}

func TestManager_RootByPID_LiveSessions(t *testing.T) {
	mgr, _ := newTestManager(t)
	pa, pb := newRooted(4101), newRooted(4102)
	sessA := createWith(t, mgr, rootSessA, pa)
	createWith(t, mgr, rootSessB, pb)

	for _, want := range []struct {
		id string
		p  *rootedProvider
	}{{rootSessA, pa}, {rootSessB, pb}} {
		id, root, ok := mgr.RootByPID(want.p.root.PID)
		if !ok || id != want.id || root != want.p.root {
			t.Fatalf("RootByPID(%d) = (%q, %+v, %v), want (%q, %+v, true)", want.p.root.PID, id, root, ok, want.id, want.p.root)
		}
	}
	if _, _, ok := mgr.RootByPID(4999); ok {
		t.Fatal("RootByPID of an unknown pid hit")
	}

	got, ok := mgr.LiveSession(rootSessA)
	if !ok || got != sessA {
		t.Fatalf("LiveSession(%s) = (%p, %v), want the live session %p", rootSessA, got, ok, sessA)
	}
	if _, ok := mgr.LiveSession("aaaaaaaa-0000-0000-0000-00000000ffff"); ok {
		t.Fatal("LiveSession of an unknown id hit")
	}
}

func TestManager_RootByPID_DeadSlotsMiss(t *testing.T) {
	cases := []struct {
		name  string
		abort func(mgr *session.Manager, p *rootedProvider)
	}{
		{"provider died", func(_ *session.Manager, p *rootedProvider) { p.Kill() }},
		{"session ended", func(mgr *session.Manager, _ *rootedProvider) { mgr.EndSession(rootSessA) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr, _ := newTestManager(t)
			p := newRooted(4201)
			createWith(t, mgr, rootSessA, p)
			tc.abort(mgr, p)

			if _, _, ok := mgr.RootByPID(p.root.PID); ok {
				t.Fatal("RootByPID hit a dead slot")
			}
			if _, ok := mgr.LiveSession(rootSessA); ok {
				t.Fatal("LiveSession hit a dead slot")
			}
		})
	}
}

func TestManager_RootByPID_LaunchingSlotMisses(t *testing.T) {
	mgr, _ := newTestManager(t)
	gate := make(chan struct{})
	p := newRooted(4301)
	p.startGate = gate
	mgr.SetProviderFactory(factoryReturning(p))

	createDone := make(chan struct{})
	go func() {
		_, _ = mgr.Create(session.CreateSpec{SessionID: rootSessA, ProjectID: "proj-1", Kind: session.KindClaude})
		close(createDone)
	}()
	t.Cleanup(func() { close(gate); <-createDone })
	testutil.WaitFor(t, 2*time.Second, func() bool { return mgr.Exists(rootSessA) })

	if _, _, ok := mgr.RootByPID(p.root.PID); ok {
		t.Fatal("RootByPID hit a slot whose Create has not finished")
	}

	liveDone := make(chan bool, 1)
	go func() {
		_, ok := mgr.LiveSession(rootSessA)
		liveDone <- ok
	}()
	select {
	case ok := <-liveDone:
		if ok {
			t.Fatal("LiveSession hit a slot whose Create has not finished")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("LiveSession blocked on an in-flight Create")
	}
}

func TestManager_LiveSession_NoLazyLoad(t *testing.T) {
	store := session.NewStore(t.TempDir())
	if err := store.Save(&sessionstypes.Session{ID: rootSessA, ProjectID: "proj-1", ProviderType: session.KindClaude}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	mgr := session.NewManager(session.Config{}, store, nil)

	if _, ok := mgr.LiveSession(rootSessA); ok {
		t.Fatal("LiveSession hit a session that exists only on disk")
	}
	if mgr.Exists(rootSessA) {
		t.Fatal("LiveSession lazy-loaded the session into the live table")
	}

	if _, ok := mgr.Get(rootSessA); !ok {
		t.Fatal("Get: persisted session not found")
	}
	if _, ok := mgr.LiveSession(rootSessA); ok {
		t.Fatal("LiveSession hit a lazy-loaded slot with no provider")
	}
}
