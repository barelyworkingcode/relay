package session_test

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/barelyworkingcode/relay/internal/sessions/session"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const raceSessionID = "55555555-5555-5555-5555-555555555555"

var (
	raceLaunchSpec = session.CreateSpec{SessionID: raceSessionID, Kind: session.KindPi, ModelKey: "acme-key-launch", SandboxProfile: "/sandbox/acme-launch.sb"}
	raceResumeSpec = session.CreateSpec{SessionID: raceSessionID, Kind: session.KindPi, ModelKey: "acme-key-resume", SandboxProfile: "/sandbox/acme-resume.sb", Resume: true}
)

func closeOnce(ch chan struct{}) func() {
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

// raceRig holds a live ad-hoc session whose provider has died. Build #1 is
// the launched provider (parks its first Alive call), build #2 is the
// resume's provider (parks in Start), anything later is a plain fake.
type raceRig struct {
	mgr          *session.Manager
	launched     *aliveGateProvider
	resumed      *fakeProvider
	releaseAlive func()
	releaseStart func()

	mu    sync.Mutex
	specs []session.CreateSpec
}

func newRaceRig(t *testing.T, resumeStartErr error) *raceRig {
	t.Helper()
	r := &raceRig{
		launched: &aliveGateProvider{aliveGate: make(chan struct{}), entered: make(chan struct{})},
		resumed:  &fakeProvider{startGate: make(chan struct{}), startErr: resumeStartErr},
	}
	r.releaseAlive = closeOnce(r.launched.aliveGate)
	r.releaseStart = closeOnce(r.resumed.startGate)
	r.mgr = session.NewManager(session.Config{}, session.NewStore(t.TempDir()), nil)
	r.mgr.SetProviderFactory(func(_ *sessionstypes.Session, spec session.CreateSpec, _ sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.specs = append(r.specs, spec)
		switch len(r.specs) {
		case 1:
			return r.launched, nil
		case 2:
			return r.resumed, nil
		default:
			return &fakeProvider{}, nil
		}
	})
	sess, err := r.mgr.Create(raceLaunchSpec)
	if err != nil {
		t.Fatalf("setup: Create: %v", err)
	}
	sess.Provider().Kill()
	return r
}

func (r *raceRig) builtSpecs() []session.CreateSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.specs)
}

func (r *raceRig) sendInBackground(t *testing.T) chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- r.mgr.SendMessage(raceSessionID, "hello", nil) }()
	synctest.Wait()
	select {
	case <-r.launched.entered:
	default:
		t.Errorf("setup: SendMessage did not park in the dead provider's Alive")
	}
	return done
}

func (r *raceRig) resumeInBackground(t *testing.T) chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := r.mgr.Create(raceResumeSpec)
		done <- err
	}()
	synctest.Wait()
	if n := len(r.builtSpecs()); n != 2 {
		t.Errorf("setup: builds = %d once the resume started, want 2 (resume parked in Start)", n)
	}
	return done
}

func assertNoEmptyProfile(t *testing.T, specs []session.CreateSpec) {
	t.Helper()
	for i, spec := range specs {
		if spec.SandboxProfile == "" {
			t.Errorf("build #%d used an empty SandboxProfile: %+v", i+1, spec)
		}
	}
}

func TestManager_RespawnDuringResume_SendMessage(t *testing.T) {
	cases := []struct {
		name           string
		resumeStartErr error
		wantSendErr    error
		wantDelivered  []string
	}{
		{name: "resume succeeds", wantDelivered: []string{"hello"}},
		{name: "resume fails", resumeStartErr: errors.New("acme: start failed"), wantSendErr: session.ErrResumeRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := newRaceRig(t, tc.resumeStartErr)
				defer r.releaseStart()
				defer r.releaseAlive()

				sendDone := r.sendInBackground(t)
				createDone := r.resumeInBackground(t)
				r.releaseAlive()
				synctest.Wait()
				r.releaseStart()
				sendErr, createErr := <-sendDone, <-createDone

				if tc.wantSendErr == nil && sendErr != nil {
					t.Errorf("SendMessage = %v, want nil", sendErr)
				}
				if tc.wantSendErr != nil && !errors.Is(sendErr, tc.wantSendErr) {
					t.Errorf("SendMessage = %v, want %v", sendErr, tc.wantSendErr)
				}
				if tc.resumeStartErr == nil && createErr != nil {
					t.Errorf("resume Create = %v, want nil", createErr)
				}
				specs := r.builtSpecs()
				if len(specs) != 2 {
					t.Errorf("builds = %d, want 2 (the restart must build nothing itself)", len(specs))
				}
				assertNoEmptyProfile(t, specs)
				if got := r.resumed.Sent(); !slices.Equal(got, tc.wantDelivered) {
					t.Errorf("resumed provider received %q, want %q", got, tc.wantDelivered)
				}
			})
		})
	}
}

type gatedSink struct {
	gate    chan struct{}
	entered chan struct{}
	taken   atomic.Bool
}

func (s *gatedSink) SendToSession(string, map[string]any) {
	if s.taken.CompareAndSwap(false, true) {
		close(s.entered)
		<-s.gate
	}
}

func TestManager_RespawnDuringResume_ClearSessionUsesResumeSpec(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRaceRig(t, nil)
		defer r.releaseStart()
		r.releaseAlive()
		sink := &gatedSink{gate: make(chan struct{}), entered: make(chan struct{})}
		releaseSink := closeOnce(sink.gate)
		defer releaseSink()
		r.mgr.SetEventSink(sink)

		clearDone := make(chan error, 1)
		go func() { clearDone <- r.mgr.ClearSession(raceSessionID) }()
		synctest.Wait()
		select {
		case <-sink.entered:
		default:
			t.Errorf("setup: ClearSession did not park in the event sink")
		}
		createDone := r.resumeInBackground(t)
		releaseSink()
		synctest.Wait()
		r.releaseStart()
		clearErr := <-clearDone
		<-createDone

		if clearErr != nil {
			t.Errorf("ClearSession = %v, want nil", clearErr)
		}
		specs := r.builtSpecs()
		assertNoEmptyProfile(t, specs)
		if len(specs) != 3 {
			t.Fatalf("builds = %d, want 3 (launch, resume, clear restart)", len(specs))
		}
		if got := specs[2]; got.ModelKey != raceResumeSpec.ModelKey || got.SandboxProfile != raceResumeSpec.SandboxProfile {
			t.Errorf("clear restart spec = %+v, want the resume's ModelKey and SandboxProfile", got)
		}
	})
}

func TestManager_Respawn_SlotRemovedBeforeRestart_Refused(t *testing.T) {
	cases := []struct {
		name string
		stop func(*session.Manager)
	}{
		{"EndSession", func(m *session.Manager) { m.EndSession(raceSessionID) }},
		{"StopAll", func(m *session.Manager) { m.StopAll() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := newRaceRig(t, nil)
				defer r.releaseStart()
				defer r.releaseAlive()

				sendDone := r.sendInBackground(t)
				tc.stop(r.mgr)
				r.releaseAlive()
				r.releaseStart()
				sendErr := <-sendDone

				if !errors.Is(sendErr, session.ErrResumeRequired) {
					t.Errorf("SendMessage = %v, want ErrResumeRequired", sendErr)
				}
				if n := len(r.builtSpecs()); n != 1 {
					t.Errorf("builds = %d, want 1 (the restart must build nothing)", n)
				}
			})
		})
	}
}

func assertResumeGuidance(t *testing.T, op string, err error) {
	t.Helper()
	if !errors.Is(err, session.ErrResumeRequired) {
		t.Errorf("%s = %v, want ErrResumeRequired", op, err)
		return
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "project") || !strings.Contains(msg, "new session") {
		t.Errorf("%s message %q does not tell the user to start a new session in a project", op, err.Error())
	}
}

func TestManager_Respawn_LazyLoadedAdHocSession_Refused(t *testing.T) {
	store := session.NewStore(t.TempDir())
	persisted := &sessionstypes.Session{
		ID:           raceSessionID,
		ProviderType: session.KindClaude,
		Messages:     []sessionstypes.Message{{Timestamp: "t0", Role: "user", Content: []byte(`"hi"`)}},
	}
	if err := store.Save(persisted); err != nil {
		t.Fatalf("Save: %v", err)
	}
	mgr := session.NewManager(session.Config{}, store, nil)
	var builds atomic.Int32
	mgr.SetProviderFactory(func(*sessionstypes.Session, session.CreateSpec, sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		builds.Add(1)
		return &fakeProvider{}, nil
	})

	assertResumeGuidance(t, "first SendMessage", mgr.SendMessage(raceSessionID, "hello", nil))
	if err := mgr.SendMessage(raceSessionID, "again", nil); !errors.Is(err, session.ErrResumeRequired) {
		t.Errorf("second SendMessage = %v, want ErrResumeRequired (not ErrAlreadyProcessing)", err)
	}
	assertResumeGuidance(t, "ClearSession", mgr.ClearSession(raceSessionID))

	if n := builds.Load(); n != 0 {
		t.Errorf("builds = %d, want 0 (a session this manager never launched is not restarted)", n)
	}
	sess, ok := mgr.Get(raceSessionID)
	if !ok {
		t.Fatal("Get: session gone after a refused restart")
	}
	if len(sess.Messages) != 0 {
		t.Errorf("history after ClearSession = %d messages, want 0", len(sess.Messages))
	}
}
