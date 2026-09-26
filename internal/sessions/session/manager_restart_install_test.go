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
// build (the ad-hoc restart's) in the factory until releaseRestart, and
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
			if d, ok := restarted.(*delayedExitProvider); ok {
				d.handler = h
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
		sink := &recordingSink{}
		r.mgr.SetEventSink(sink)
		var exitMu sync.Mutex
		exitCalls := 0
		r.mgr.SetExitHandler(func(string, int) {
			exitMu.Lock()
			exitCalls++
			exitMu.Unlock()
		})
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
		exitsBefore := sink.count("process_exited")

		close(r.restartGate)
		sendErr := <-sendDone
		// Lets any process_exited the restart's provider fires land first.
		synctest.Wait()

		if !errors.Is(sendErr, session.ErrResumeRequired) {
			t.Errorf("SendMessage = %v, want ErrResumeRequired", sendErr)
		}
		if got := sink.count("process_exited") - exitsBefore; got != 0 {
			t.Errorf("%d process_exited frame(s) sent to the live session's viewers (live provider alive=%v)", got, live.Alive())
		}
		exitMu.Lock()
		gotExits := exitCalls
		exitMu.Unlock()
		if gotExits != 0 {
			t.Errorf("exit handler called %d time(s) for the live session (live provider alive=%v)", gotExits, live.Alive())
		}
		onDisk, err := r.store.Load(installSessionID)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if onDisk.Name != "Acme review" {
			t.Errorf("persisted name = %q, want %q", onDisk.Name, "Acme review")
		}
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
