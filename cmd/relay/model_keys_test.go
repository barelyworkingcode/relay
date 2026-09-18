package main

import (
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/membership"
	"github.com/barelyworkingcode/relay/internal/peertoken"
	"github.com/barelyworkingcode/relay/internal/service"
)

func TestModelKeyTable_MintLookupRevoke(t *testing.T) {
	table := NewModelKeyTable()

	plaintext, err := table.Mint("proj-1", "session:abc")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !HasModelKeyPrefix(plaintext) {
		t.Fatalf("minted key %q does not have the rmk_ prefix", plaintext)
	}

	projectID, label, ok := table.Lookup(plaintext)
	if !ok || projectID != "proj-1" || label != "session:abc" {
		t.Fatalf("Lookup = %q, %q, %v; want proj-1, session:abc, true", projectID, label, ok)
	}

	table.Revoke("proj-1", "session:abc")
	if _, _, ok := table.Lookup(plaintext); ok {
		t.Fatal("a revoked key still resolves")
	}
}

// TestModelKeyTable_RevokeIsScopedToProject proves Revoke needs BOTH the
// project id and the label to match: two different projects using the same
// label convention (e.g. "session:abc") must not be able to revoke each
// other's key.
func TestModelKeyTable_RevokeIsScopedToProject(t *testing.T) {
	table := NewModelKeyTable()
	a, err := table.Mint("proj-a", "session:abc")
	if err != nil {
		t.Fatal(err)
	}
	b, err := table.Mint("proj-b", "session:abc")
	if err != nil {
		t.Fatal(err)
	}
	table.Revoke("proj-a", "session:abc")
	if _, _, ok := table.Lookup(a); ok {
		t.Fatal("proj-a's key survived its own revoke")
	}
	if _, _, ok := table.Lookup(b); !ok {
		t.Fatal("revoking proj-a's key also revoked proj-b's same-labelled key")
	}
}

func TestModelKeyTable_UnknownKeyDoesNotResolve(t *testing.T) {
	table := NewModelKeyTable()
	if _, _, ok := table.Lookup("rmk_" + "0000000000000000000000000000000000000000000000000000000000000"); ok {
		t.Fatal("a key that was never minted resolved")
	}
}

func TestModelKeyTable_TwoMintsAreDistinct(t *testing.T) {
	table := NewModelKeyTable()
	a, err := table.Mint("p", "l1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := table.Mint("p", "l2")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two mints produced the same plaintext")
	}
	table.Revoke("p", "l1")
	if _, _, ok := table.Lookup(a); ok {
		t.Fatal("revoking l1 left it resolvable")
	}
	if _, _, ok := table.Lookup(b); !ok {
		t.Fatal("revoking l1 also revoked l2")
	}
}

func TestHasModelKeyPrefix(t *testing.T) {
	cases := map[string]bool{
		"rmk_" + "a": true,
		"rmk_":       false, // exactly the prefix with nothing after is not a key
		"":           false,
		"rmkxabc":    false,
		"xrmk_abc":   false,
	}
	for in, want := range cases {
		if got := HasModelKeyPrefix(in); got != want {
			t.Errorf("HasModelKeyPrefix(%q) = %v, want %v", in, got, want)
		}
	}
}

// bindableLaunch begins and binds a project_session launch whose root-exit
// watcher the test drives by hand, and returns the launch handle with the
// function that simulates its root process exiting.
func bindableLaunch(t *testing.T, launches *service.Launches, name string, rootPID int) (*service.Launch, func()) {
	t.Helper()
	procs := newProcTable()
	procs.set(membership.ProcInfo{PID: rootPID, PPID: 1, StartSec: 1})
	var onExit func()
	launches.SetRootSourceForTest(procs)
	launches.SetRootWatcherForTest(func(_ int, _ membership.ProcInfo, cb func()) (func(), error) {
		onExit = cb
		return func() {}, nil
	})
	secret, launch, err := launches.Begin(service.Identity{
		Kind: service.IdentityKindProjectSession, Name: name, ProjectID: "proj-1",
		ParentLaunch: config.RelaySessionsServiceID,
	})
	assertNoErr(t, err, "Begin")
	_, err = launches.BindKind(name, secret, peertoken.ForProcessForTest(int32(rootPID), 1), service.IdentityKindProjectSession)
	assertNoErr(t, err, "Bind")
	return launch, func() { onExit() }
}

// A key bound to its session's launch dies when the launch ends through the
// root-exit watcher, with nothing ever reporting SessionExited or calling
// Revoke.
func TestModelKeyTable_BoundKeyDiesWhenItsLaunchEnds(t *testing.T) {
	launches := service.NewLaunches()
	table := NewModelKeyTable()
	key, err := table.Mint("proj-1", "session:s1")
	assertNoErr(t, err, "Mint")
	launch, rootExits := bindableLaunch(t, launches, "s1", 71001)
	table.BindLaunch(key, launch)

	if _, _, ok := table.Lookup(key); !ok {
		t.Fatal("a key bound to a live launch does not resolve")
	}

	rootExits()

	if _, _, ok := table.Lookup(key); ok {
		t.Fatal("the key still resolves after its launch's root exited")
	}
	if _, _, ok := table.Lookup(key); ok {
		t.Fatal("the key came back on a second lookup")
	}
}

func TestModelKeyTable_BoundKeyDiesWhenAnotherBeginReplacesItsLaunch(t *testing.T) {
	launches := service.NewLaunches()
	table := NewModelKeyTable()
	key, err := table.Mint("proj-1", "session:s2")
	assertNoErr(t, err, "Mint")
	launch, _ := bindableLaunch(t, launches, "s2", 71002)
	table.BindLaunch(key, launch)

	// A resume of the same session id begins a new launch under the name,
	// which ends the old one.
	_, _, err = launches.Begin(service.Identity{Kind: service.IdentityKindProjectSession, Name: "s2", ProjectID: "proj-1", ParentLaunch: config.RelaySessionsServiceID})
	assertNoErr(t, err, "second Begin")

	if _, _, ok := table.Lookup(key); ok {
		t.Fatal("a key outlived the launch a resume replaced")
	}
}

// A key nobody bound keeps the pre-existing lifetime: explicit revocation or
// a relay restart, and nothing a launch does touches it.
func TestModelKeyTable_UnboundKeyIsUnaffectedByLaunches(t *testing.T) {
	launches := service.NewLaunches()
	table := NewModelKeyTable()
	key, err := table.Mint("proj-1", "session:s3")
	assertNoErr(t, err, "Mint")
	launch, rootExits := bindableLaunch(t, launches, "s3", 71003)
	rootExits()
	launch.End()

	if _, _, ok := table.Lookup(key); !ok {
		t.Fatal("a key that was never bound to a launch stopped resolving when a launch ended")
	}
}

func TestModelKeyTable_BindLaunchIgnoresWhatItCannotBind(t *testing.T) {
	launches := service.NewLaunches()
	table := NewModelKeyTable()
	launch, _ := bindableLaunch(t, launches, "s4", 71004)
	table.BindLaunch("rmk_never_minted", launch) // must not panic or invent a record
	table.BindLaunch("", launch)
	key, err := table.Mint("proj-1", "session:s4")
	assertNoErr(t, err, "Mint")
	table.BindLaunch(key, nil)
	if _, _, ok := table.Lookup(key); !ok {
		t.Fatal("binding a nil launch made a live key stop resolving")
	}
	if _, _, ok := table.Lookup("rmk_never_minted"); ok {
		t.Fatal("BindLaunch created a key for a plaintext that was never minted")
	}
}
