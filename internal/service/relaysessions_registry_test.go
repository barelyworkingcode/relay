//go:build !windows

package service

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/peertoken"
)

// stubHelperVerifier is the hermetic stand-in for darwinHelperVerifier
// (SP3): a test drives its return values directly instead of needing a
// real signed binary on disk.
type stubHelperVerifier struct {
	staticErr error
	guestErr  error
}

func (v *stubHelperVerifier) VerifyStatic(string) error         { return v.staticErr }
func (v *stubHelperVerifier) VerifyGuest(peertoken.Token) error { return v.guestErr }

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// TestRegistry_HelperVerifierRefusesAMismatchedHelper is the plan's own
// required case: "verifier stub refuses a mismatched helper -> not
// started, logged". Registry.Start must never spawn
// config.RelaySessionsServiceID's binary if HelperVerifier.VerifyStatic
// refuses it -- every other service is unaffected.
func TestRegistry_HelperVerifierRefusesAMismatchedHelper(t *testing.T) {
	r := NewRegistry()
	r.HelperVerifier = &stubHelperVerifier{staticErr: errors.New("cdhash mismatch: not the binary relay shipped")}

	cfg := &config.ServiceConfig{
		ID:          config.RelaySessionsServiceID,
		DisplayName: "Session Host",
		Command:     "/bin/sleep",
		Args:        []string{"300"},
	}
	if err := r.Start(cfg); err == nil {
		t.Fatal("Start succeeded despite a refused static code-identity check")
	}
	if r.IsRunning(cfg.ID) {
		t.Fatal("the mismatched helper was started")
	}
	if pid := r.PIDsByServiceID()[cfg.ID]; pid != 0 {
		t.Fatalf("a pid was recorded for a helper that should never have spawned: %d", pid)
	}

	// A service with any other id is completely unaffected by a refusing
	// HelperVerifier -- the check is scoped to config.RelaySessionsServiceID
	// alone.
	other := &config.ServiceConfig{ID: "unrelated-service", DisplayName: "x", Command: "/bin/sleep", Args: []string{"300"}}
	r.OpenLog = func(string) (io.WriteCloser, error) { return nopWriteCloser{Writer: io.Discard}, nil }
	if err := r.Start(other); err != nil {
		t.Fatalf("an unrelated service was refused by relay-sessions' own verifier: %v", err)
	}
	t.Cleanup(func() { r.Stop(other.ID) })
	if !r.IsRunning(other.ID) {
		t.Fatal("an unrelated service did not start")
	}
}

// TestRegistry_RelaySessionsRestartEndsChildIdentitiesAndKillsRootPgids
// pins the plan's other required case: "supervision restart ends child
// identities and kills root pgids (fake clock)". A crash of the
// relaysessions launch -- driven through the real supervision path, with a
// FakeClock standing in for the restart backoff -- must end every
// project_session identity it parented and SIGKILL each one's real,
// still-running process group (spec-session-host.md §4.4, §6), not merely
// forget about them.
func TestRegistry_RelaySessionsRestartEndsChildIdentitiesAndKillsRootPgids(t *testing.T) {
	r := NewRegistry()
	r.Launches = NewLaunches()
	fc := NewFakeClock(time.Now())
	r.Clock = fc
	r.OpenLog = func(string) (io.WriteCloser, error) { return nopWriteCloser{Writer: io.Discard}, nil }

	// Stand-ins for two sessions' real root processes (shims), each its own
	// process group leader -- exactly the shape a real relay-sessions shim
	// has (SetProcessGroup). Real processes, real membership.NewSource() /
	// membership.WatchExit underneath (Launches' production defaults):
	// nothing here is faked except which pid presents Hello.
	root1 := spawnGroupLeader(t, "sleep", "300")
	root2 := spawnGroupLeader(t, "sleep", "300")
	pid1, pid2 := int32(root1.Process.Pid), int32(root2.Process.Pid)

	for _, root := range []struct {
		name string
		pid  int32
	}{{"sess-1", pid1}, {"sess-2", pid2}} {
		secret, _, err := r.Launches.Begin(Identity{
			Kind: IdentityKindProjectSession, Name: root.name, SessionID: root.name,
			ProjectID: "proj-a", ParentLaunch: config.RelaySessionsServiceID,
		})
		if err != nil {
			t.Fatalf("Begin %s: %v", root.name, err)
		}
		if _, err := r.Launches.BindKind(root.name, secret, peertoken.ForProcessForTest(root.pid, 1), IdentityKindProjectSession); err != nil {
			t.Fatalf("bind %s: %v", root.name, err)
		}
	}
	if _, ok := r.Launches.Bound("sess-1"); !ok {
		t.Fatal("sess-1 did not bind")
	}

	// The relaysessions launch itself: a real process that exits almost
	// immediately with a nonzero code, so supervision schedules a restart.
	cfg := &config.ServiceConfig{
		ID:          config.RelaySessionsServiceID,
		DisplayName: "Session Host",
		Command:     "/bin/sh",
		Args:        []string{"-c", "exit 1"},
	}
	if err := r.Start(cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { r.Stop(cfg.ID) })

	// "(fake clock)": confirm this really is going through supervision's
	// FakeClock-governed restart path, not merely that the process died.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := r.SupervisionStatuses()[cfg.ID]; ok && st.Phase == SupervisionRestarting {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st := r.SupervisionStatuses()[cfg.ID]; st.Phase != SupervisionRestarting {
		t.Fatalf("relaysessions did not reach SupervisionRestarting via the fake clock: %+v", st)
	}

	// The cleanup itself does not wait on the backoff timer -- it runs from
	// the exit goroutine's own defer chain, independent of when (or
	// whether) fc.Advance ever fires the next attempt.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, ok1 := r.Launches.Bound("sess-1")
		_, ok2 := r.Launches.Bound("sess-2")
		if !ok1 && !ok2 && !processGroupAlive(int(pid1)) && !processGroupAlive(int(pid2)) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, ok1 := r.Launches.Bound("sess-1")
	_, ok2 := r.Launches.Bound("sess-2")
	t.Fatalf("cleanup incomplete: sess-1 bound=%v sess-2 bound=%v root1 alive=%v root2 alive=%v",
		ok1, ok2, processGroupAlive(int(pid1)), processGroupAlive(int(pid2)))
}
