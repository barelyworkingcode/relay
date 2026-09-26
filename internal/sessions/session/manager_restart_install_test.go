package session_test

import (
	"errors"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/barelyworkingcode/relay/internal/sessions/session"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const installSessionID = "77777777-7777-7777-7777-777777777777"

var (
	installLaunchSpec = session.CreateSpec{SessionID: installSessionID, Kind: session.KindPi, ModelKey: "acme-key-launch", SandboxProfile: "/sandbox/acme.sb"}
	installResumeSpec = session.CreateSpec{SessionID: installSessionID, Kind: session.KindPi, ModelKey: "acme-key-resume", SandboxProfile: "/sandbox/acme.sb", Resume: true}
)

// spawnedGateProvider is alive as soon as Start is entered, then parks
// before returning: the window after the process spawns and before Start
// returns, which a stock fakeProvider cannot model.
type spawnedGateProvider struct {
	fakeProvider
	gate    chan struct{}
	entered chan struct{}
}

func (p *spawnedGateProvider) Start() error {
	p.fakeProvider.mu.Lock()
	p.fakeProvider.alive = true
	p.fakeProvider.startCount++
	p.fakeProvider.mu.Unlock()
	close(p.entered)
	<-p.gate
	return nil
}

// preSpawnGateProvider parks in Start before its process exists. Like the
// real providers, Kill is a no-op until the process has spawned; after that
// it fires process_exited on its own goroutine, as waitForExit does.
type preSpawnGateProvider struct {
	fakeProvider
	handler sessionstypes.EventHandler
	gate    chan struct{}
	entered chan struct{}
	spawned bool
}

func (p *preSpawnGateProvider) Start() error {
	close(p.entered)
	<-p.gate
	p.fakeProvider.mu.Lock()
	defer p.fakeProvider.mu.Unlock()
	p.spawned = true
	p.fakeProvider.alive = true
	p.fakeProvider.startCount++
	return nil
}

func (p *preSpawnGateProvider) Kill() {
	p.fakeProvider.mu.Lock()
	spawned := p.spawned
	p.fakeProvider.mu.Unlock()
	if !spawned {
		return
	}
	p.fakeProvider.Kill()
	if p.handler != nil {
		go p.handler("process_exited", []byte(`{"exitCode":0}`))
	}
}

func newPreSpawnGateProvider() *preSpawnGateProvider {
	return &preSpawnGateProvider{gate: make(chan struct{}), entered: make(chan struct{})}
}

// parkRestartInStart lets the parked restart's build return and waits until
// its provider is parked inside Start.
func (r *restartInstallRig) parkRestartInStart(t *testing.T, restarted *preSpawnGateProvider) {
	t.Helper()
	close(r.restartGate)
	synctest.Wait()
	select {
	case <-restarted.entered:
	default:
		t.Fatal("setup: restart's provider did not park in Start")
	}
}

// liveObserver records what reaches a session's viewers and exit handler
// once a resume has made it live again.
type liveObserver struct {
	sink         recordingSink
	mu           sync.Mutex
	exitCalls    int
	framesBefore int
}

func observeLiveSession(mgr *session.Manager) *liveObserver {
	o := &liveObserver{}
	mgr.SetEventSink(&o.sink)
	mgr.SetExitHandler(func(string, int) {
		o.mu.Lock()
		o.exitCalls++
		o.mu.Unlock()
	})
	return o
}

func (o *liveObserver) markResumed() {
	o.framesBefore = o.sink.count("process_exited")
}

func (o *liveObserver) assertLiveUntouched(t *testing.T, store *session.Store, live *fakeProvider, wantName string) {
	t.Helper()
	if got := o.sink.count("process_exited") - o.framesBefore; got != 0 {
		t.Errorf("%d process_exited frame(s) sent to the live session's viewers (live provider alive=%v)", got, live.Alive())
	}
	o.mu.Lock()
	exits := o.exitCalls
	o.mu.Unlock()
	if exits != 0 {
		t.Errorf("exit handler called %d time(s) for the live session (live provider alive=%v)", exits, live.Alive())
	}
	onDisk, err := store.Load(installSessionID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if onDisk.Name != wantName {
		t.Errorf("persisted name = %q, want %q", onDisk.Name, wantName)
	}
}

type recordingSink struct {
	mu   sync.Mutex
	msgs []map[string]any
}

func (s *recordingSink) SendToSession(_ string, msg map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, msg)
}

func (s *recordingSink) count(typ string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, m := range s.msgs {
		if m["type"] == typ {
			n++
		}
	}
	return n
}

// restartInstallRig builds the launched provider first, parks the second
// build (the ad-hoc restart's) in the factory until restartGate closes, and
// hands every later build the resume's provider.
type restartInstallRig struct {
	mgr            *session.Manager
	store          *session.Store
	launched       *fakeProvider
	restartGate    chan struct{}
	restartEntered chan struct{}
}

func newRestartInstallRig(t *testing.T, restarted, resumed sessionstypes.Provider) *restartInstallRig {
	t.Helper()
	r := &restartInstallRig{
		store:          session.NewStore(t.TempDir()),
		launched:       &fakeProvider{},
		restartGate:    make(chan struct{}),
		restartEntered: make(chan struct{}),
	}
	r.mgr = session.NewManager(session.Config{}, r.store, nil)
	var mu sync.Mutex
	builds := 0
	r.mgr.SetProviderFactory(func(_ *sessionstypes.Session, _ session.CreateSpec, h sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		mu.Lock()
		builds++
		n := builds
		mu.Unlock()
		switch n {
		case 1:
			return r.launched, nil
		case 2:
			close(r.restartEntered)
			<-r.restartGate
			switch p := restarted.(type) {
			case *delayedExitProvider:
				p.handler = h
			case *preSpawnGateProvider:
				p.handler = h
			}
			return restarted, nil
		default:
			return resumed, nil
		}
	})
	return r
}

// launchAndParkRestart launches the session, kills its provider, and starts
// a SendMessage whose restart is left parked in the factory.
func (r *restartInstallRig) launchAndParkRestart(t *testing.T) (*sessionstypes.Session, <-chan error) {
	t.Helper()
	sess, err := r.mgr.Create(installLaunchSpec)
	if err != nil {
		t.Fatalf("setup: launch Create: %v", err)
	}
	r.launched.Kill()
	sendDone := make(chan error, 1)
	go func() { sendDone <- r.mgr.SendMessage(installSessionID, "hello", nil) }()
	synctest.Wait()
	select {
	case <-r.restartEntered:
	default:
		t.Fatal("setup: restart did not park in the factory")
	}
	return sess, sendDone
}

func TestManager_RestartLosingToInFlightResume_LeavesResumeLive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		restarted := &fakeProvider{}
		resumed := &spawnedGateProvider{gate: make(chan struct{}), entered: make(chan struct{})}
		r := newRestartInstallRig(t, restarted, resumed)
		_, sendDone := r.launchAndParkRestart(t)

		createDone := make(chan error, 1)
		var created *sessionstypes.Session
		go func() {
			s, err := r.mgr.Create(installResumeSpec)
			created = s
			createDone <- err
		}()
		synctest.Wait()
		select {
		case <-resumed.entered:
		default:
			t.Fatal("setup: resume did not park in its provider's Start")
		}

		close(r.restartGate)
		sendErr := <-sendDone
		close(resumed.gate)
		createErr := <-createDone

		if !errors.Is(sendErr, session.ErrResumeRequired) {
			t.Errorf("SendMessage = %v, want ErrResumeRequired", sendErr)
		}
		if createErr != nil {
			t.Fatalf("resume Create = %v, want success", createErr)
		}
		if p := created.Provider(); p == nil || !p.Alive() {
			t.Errorf("resume Create succeeded but its session's provider is dead (is restart's: %v, restart kills=%d, resume kills=%d)",
				p == sessionstypes.Provider(restarted), restarted.Kills(), resumed.Kills())
		}
		if _, ok := r.mgr.LiveSession(installSessionID); !ok {
			t.Errorf("LiveSession(%s) = not live right after a successful resume Create", installSessionID)
		}
	})
}

func TestManager_RestartLosingToEndAndResume_DoesNotReachLiveSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		restarted := &delayedExitProvider{}
		live := &fakeProvider{}
		r := newRestartInstallRig(t, restarted, live)
		obs := observeLiveSession(r.mgr)
		stale, sendDone := r.launchAndParkRestart(t)

		r.mgr.EndSession(installSessionID)
		fresh, err := r.mgr.Create(installResumeSpec)
		if err != nil {
			t.Fatalf("setup: resume Create: %v", err)
		}
		if fresh == stale {
			t.Fatal("setup: resume reused the ended session object")
		}
		if err := r.mgr.RenameSession(installSessionID, "Acme review"); err != nil {
			t.Fatalf("setup: RenameSession: %v", err)
		}
		obs.markResumed()

		close(r.restartGate)
		sendErr := <-sendDone
		// Lets any process_exited the restart's provider fires land first.
		synctest.Wait()

		if !errors.Is(sendErr, session.ErrResumeRequired) {
			t.Errorf("SendMessage = %v, want ErrResumeRequired", sendErr)
		}
		obs.assertLiveUntouched(t, r.store, live, "Acme review")
	})
}

func TestManager_RestartBuiltAfterResumePublished_KeepsResumeProvider(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		restarted := &fakeProvider{}
		resumed := &fakeProvider{}
		r := newRestartInstallRig(t, restarted, resumed)
		sess, sendDone := r.launchAndParkRestart(t)

		created, err := r.mgr.Create(installResumeSpec)
		if err != nil {
			t.Fatalf("setup: resume Create: %v", err)
		}
		if created != sess {
			t.Fatal("setup: resume did not reuse the live session object")
		}
		if created.Provider() != sessionstypes.Provider(resumed) || !resumed.Alive() {
			t.Fatal("setup: resume did not publish a live provider")
		}

		close(r.restartGate)
		sendErr := <-sendDone
		synctest.Wait()

		if !errors.Is(sendErr, session.ErrResumeRequired) {
			t.Errorf("SendMessage = %v, want ErrResumeRequired", sendErr)
		}
		if p := created.Provider(); p != sessionstypes.Provider(resumed) {
			t.Errorf("installed provider is not the resume's (is restart's: %v)", p == sessionstypes.Provider(restarted))
		}
		if !resumed.Alive() {
			t.Errorf("resume's provider is dead (kills=%d)", resumed.Kills())
		}
	})
}

func TestManager_RestartStoppedDuringStart_KillsItsProviderAndReportsExit(t *testing.T) {
	stops := map[string]func(*testing.T, *session.Manager){
		"EndSession": func(_ *testing.T, m *session.Manager) { m.EndSession(installSessionID) },
		"StopAll":    func(_ *testing.T, m *session.Manager) { m.StopAll() },
		"EndSessionThenLazyLoad": func(t *testing.T, m *session.Manager) {
			m.EndSession(installSessionID)
			if _, ok := m.Get(installSessionID); !ok {
				t.Fatal("setup: Get did not load the ended session from disk")
			}
			if _, live := m.LiveSession(installSessionID); live {
				t.Fatal("setup: LiveSession reports the lazy-loaded session live")
			}
		},
	}
	for name, stop := range stops {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				restarted := newPreSpawnGateProvider()
				r := newRestartInstallRig(t, restarted, &fakeProvider{})
				var exitMu sync.Mutex
				var exitIDs []string
				r.mgr.SetExitHandler(func(id string, _ int) {
					exitMu.Lock()
					exitIDs = append(exitIDs, id)
					exitMu.Unlock()
				})
				_, sendDone := r.launchAndParkRestart(t)
				r.parkRestartInStart(t, restarted)

				stop(t, r.mgr)
				close(restarted.gate)
				sendErr := <-sendDone
				synctest.Wait()

				if !errors.Is(sendErr, session.ErrResumeRequired) {
					t.Errorf("SendMessage = %v, want ErrResumeRequired", sendErr)
				}
				if restarted.Alive() {
					t.Errorf("restart's provider left running after %s (kills=%d)", name, restarted.Kills())
				}
				exitMu.Lock()
				gotIDs := append([]string(nil), exitIDs...)
				exitMu.Unlock()
				if len(gotIDs) != 1 || gotIDs[0] != installSessionID {
					t.Errorf("exit handler calls = %q, want exactly one for %s", gotIDs, installSessionID)
				}
			})
		})
	}
}

func TestManager_RestartLosingToEndAndResumeDuringStart_DoesNotReachLiveSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		restarted := newPreSpawnGateProvider()
		live := &fakeProvider{}
		r := newRestartInstallRig(t, restarted, live)
		obs := observeLiveSession(r.mgr)
		stale, sendDone := r.launchAndParkRestart(t)
		r.parkRestartInStart(t, restarted)

		r.mgr.EndSession(installSessionID)
		fresh, err := r.mgr.Create(installResumeSpec)
		if err != nil {
			t.Fatalf("setup: resume Create: %v", err)
		}
		if fresh == stale {
			t.Fatal("setup: resume reused the ended session object")
		}
		if err := r.mgr.RenameSession(installSessionID, "Acme review"); err != nil {
			t.Fatalf("setup: RenameSession: %v", err)
		}
		obs.markResumed()

		close(restarted.gate)
		sendErr := <-sendDone
		// Lets any process_exited the restart's provider fires land first.
		synctest.Wait()

		if !errors.Is(sendErr, session.ErrResumeRequired) {
			t.Errorf("SendMessage = %v, want ErrResumeRequired", sendErr)
		}
		obs.assertLiveUntouched(t, r.store, live, "Acme review")
	})
}
