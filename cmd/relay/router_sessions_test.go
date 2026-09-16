package main

import (
	"testing"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
	"github.com/barelyworkingcode/relay/internal/service"
	"github.com/barelyworkingcode/relay/internal/sessions/ledger"
)

func newSessionExitedTestRouter(t *testing.T) *appRouter {
	t.Helper()
	r := newTestRouter(t, makeSettings(nil, nil, nil), mcpbroker.NewManager(nil))
	// Built before bindTestIdentity, matching newModelHostTestRouter's own
	// discipline: bindTestIdentity reuses this table rather than allocating
	// its own when r.launches is already non-nil.
	r.launches = service.NewLaunches()
	r.modelKeys = NewModelKeyTable()
	r.sessionAccounts = newSessionAccounting()
	l, err := ledger.Open(t.TempDir())
	assertNoErr(t, err, "ledger.Open")
	r.sessions = l
	return r
}

// TestSessionExited_RequiresSessionsCapability is the required "SessionExited
// without sessions refused" test: a launch identity holding manifest but not
// sessions must not be able to report a session as exited.
func TestSessionExited_RequiresSessionsCapability(t *testing.T) {
	r := newSessionExitedTestRouter(t)
	ctx := bindTestIdentity(t, r, config.RelaySessionsServiceID, []config.ServiceCapability{config.ServiceCapabilityManifest})

	err := r.SessionExited(ctx, bridge.SessionExitedRequest{SessionID: "s1", Reason: "exit"}, "")
	if err == nil {
		t.Fatal("SessionExited succeeded for an identity without the sessions capability")
	}
}

// A service identity that is not relaysessions at all (holds no capability
// this table would ever grant sessions to) is refused the same way.
func TestSessionExited_UnrelatedServiceRefused(t *testing.T) {
	r := newSessionExitedTestRouter(t)
	ctx := bindTestIdentity(t, r, "some-other-service", []config.ServiceCapability{config.ServiceCapabilityFrontend})

	err := r.SessionExited(ctx, bridge.SessionExitedRequest{SessionID: "s1", Reason: "exit"}, "")
	if err == nil {
		t.Fatal("SessionExited succeeded for an unrelated service identity")
	}
}

func TestSessionExited_TokenIsRefused(t *testing.T) {
	r := newSessionExitedTestRouter(t)
	ctx := bindTestIdentity(t, r, config.RelaySessionsServiceID, []config.ServiceCapability{config.ServiceCapabilityManifest, config.ServiceCapabilitySessions})

	err := r.SessionExited(ctx, bridge.SessionExitedRequest{SessionID: "s1", Reason: "exit"}, "some-token")
	if err == nil {
		t.Fatal("SessionExited accepted a bearer token on a tokenless report")
	}
}

// TestSessionExited_EndsIdentityRevokesKeyAndMarksDormant proves the
// positive path end to end: a session this process minted an identity and a
// model key for gets both torn down, and its ledger record survives as
// dormant (the ordinary "exit" reason, not "deleted").
func TestSessionExited_EndsIdentityRevokesKeyAndMarksDormant(t *testing.T) {
	r := newSessionExitedTestRouter(t)

	assertNoErr(t, r.sessions.Put(ledger.Record{SessionID: "s1", Kind: KindPi, ProjectID: "p1", State: ledger.StateLive}), "seed ledger")
	key, err := r.modelKeys.Mint("p1", "session:s1")
	assertNoErr(t, err, "Mint")
	secret, launch, err := r.launches.Begin(service.Identity{
		Kind: service.IdentityKindProjectSession, Name: "s1",
		ProjectID: "p1", SessionID: "s1", ParentLaunch: config.RelaySessionsServiceID,
	})
	assertNoErr(t, err, "Begin")
	// A project_session Bind reads the peer's real kernel start time (C2), so
	// the peer must name an actual running process -- this test binary
	// itself, via selfPeerToken, the same technique launch_identity_test.go
	// uses for the same reason.
	_, err = r.launches.Bind("s1", secret, selfPeerToken(t))
	assertNoErr(t, err, "Bind")
	r.sessionAccounts.track("s1", sessionAccount{projectID: "p1", modelKeyLabel: "session:s1", launch: launch})

	ctx := bindTestIdentity(t, r, config.RelaySessionsServiceID, []config.ServiceCapability{config.ServiceCapabilityManifest, config.ServiceCapabilitySessions})
	assertNoErr(t, r.SessionExited(ctx, bridge.SessionExitedRequest{SessionID: "s1", RootPID: 4242, ExitStatus: 0, Reason: "exit"}, ""), "SessionExited")

	if _, ok := r.launches.Bound("s1"); ok {
		t.Fatal("SessionExited did not end the session's launch identity")
	}
	if _, _, ok := r.modelKeys.Lookup(key); ok {
		t.Fatal("SessionExited did not revoke the session's model key")
	}
	rec, ok := r.sessions.Get("s1")
	if !ok {
		t.Fatal("SessionExited removed the ledger record for an ordinary exit")
	}
	if rec.State != ledger.StateDormant {
		t.Fatalf("ledger state = %q, want dormant", rec.State)
	}
}

// A "deleted" report removes the ledger record entirely rather than marking
// it dormant -- there is nothing left to offer for resume.
func TestSessionExited_DeletedReasonRemovesLedgerRecord(t *testing.T) {
	r := newSessionExitedTestRouter(t)
	assertNoErr(t, r.sessions.Put(ledger.Record{SessionID: "s1", Kind: KindChat, ProjectID: "p1", State: ledger.StateLive}), "seed ledger")

	ctx := bindTestIdentity(t, r, config.RelaySessionsServiceID, []config.ServiceCapability{config.ServiceCapabilityManifest, config.ServiceCapabilitySessions})
	assertNoErr(t, r.SessionExited(ctx, bridge.SessionExitedRequest{SessionID: "s1", Reason: "deleted"}, ""), "SessionExited")

	if _, ok := r.sessions.Get("s1"); ok {
		t.Fatal("a deleted session's ledger record survived SessionExited")
	}
}

// A terminal (never in the ledger, C5) must not make SessionExited error --
// "no such ledger record" is the ordinary pty case, not a failure.
func TestSessionExited_UnknownLedgerRecordIsNotAnError(t *testing.T) {
	r := newSessionExitedTestRouter(t)
	ctx := bindTestIdentity(t, r, config.RelaySessionsServiceID, []config.ServiceCapability{config.ServiceCapabilityManifest, config.ServiceCapabilitySessions})

	if err := r.SessionExited(ctx, bridge.SessionExitedRequest{SessionID: "term-1", Reason: "exit"}, ""); err != nil {
		t.Fatalf("SessionExited for an unknown (terminal) session errored: %v", err)
	}
}
