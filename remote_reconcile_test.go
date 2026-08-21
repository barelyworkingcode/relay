package main

// The remote listener follows settings.json instead of freezing whatever was
// true at startup — both halves of it:
//
//   - The AUTHORIZATION half (issue #21): an enrolment written by another
//     process is honoured by an already-running listener, and one deleted by
//     another process stops being honoured, without waiting for a settings
//     poll or a restart.
//   - The LIFECYCLE half: `remote.enabled` and `remote.listen` bind, move and
//     close the listener as they change, and the refusals ADR-010 builds on
//     (no listener unless explicitly enabled, no listener without auditing)
//     hold at reconcile time and not merely at launch.
//
// Everything here is hermetic: a sandboxed config dir per test, 127.0.0.1:0
// with the port read back off the listener, and never a byte written outside
// the sandbox.

import (
	"context"
	"crypto/tls"
	"net"
	"strings"
	"testing"
	"time"

	"relaygo/bridge"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// supervise builds a supervisor over the fixture's store, router and recorder.
// Nothing is bound until Reconcile runs — which is the point: the supervisor
// only ever opens what the configuration describes.
//
// Cleanup clears the revocation hook unconditionally as well as closing the
// listener, because the hook is a process-global and a test that deliberately
// leaves a listener replaced must not leak its owner into the next test.
func (f *remoteFixture) supervise() *RemoteSupervisor {
	f.t.Helper()
	sup := NewRemoteSupervisor(context.Background(), f.store, f.router, f.audit, nil)
	f.t.Cleanup(func() {
		sup.Close()
		SetEnrolmentRevocationHook(nil)
	})
	return sup
}

// cliWriter returns a SECOND, independent store over the same config dir. That
// is precisely what `relay enrol create` is — another process, with its own
// cache, writing the same settings.json — and it is the condition issue #21
// occurs under. Nothing in these tests may reach the running listener's store,
// or the test proves only that a process can read its own writes.
func (f *remoteFixture) cliWriter() SettingsStore {
	return NewSettingsStoreAt(f.dir)
}

// dialAddr connects to an explicit address with an explicit bundle, so a test
// can dial a listener the fixture does not own (the supervisor's) or use a
// certificate the fixture did not create (one a CLI process enrolled).
func (f *remoteFixture) dialAddr(addr string, b *enrolmentBundle) *remoteTestClient {
	f.t.Helper()
	cert, err := tls.LoadX509KeyPair(b.CertPath, b.KeyPath)
	assertNoErr(f.t, err, "load client keypair from bundle")
	conn, err := tls.Dial("tcp", addr, &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      f.caPool(),
	})
	if err != nil {
		return nil
	}
	f.t.Cleanup(func() { _ = conn.Close() })
	return &remoteTestClient{t: f.t, conn: conn, scanner: bridge.NewScanner(conn)}
}

// freeLoopbackAddr reserves an ephemeral loopback port and immediately releases
// it, so a test can name a concrete address that is free. Never a hardcoded
// port: a fixed one collides with the developer's own running relay.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	assertNoErr(t, err, "reserve a loopback port")
	addr := ln.Addr().String()
	assertNoErr(t, ln.Close(), "release the reserved port")
	return addr
}

// occupiedLoopbackAddr holds a loopback port for the test's lifetime, so a bind
// of that address genuinely fails.
func occupiedLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	assertNoErr(t, err, "occupy a loopback port")
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

// assertNothingListensAt fails if a TCP connection to addr succeeds. This is
// what "the listener stopped" has to mean — the port answering with a refusal
// would still be a listener.
func assertNothingListensAt(t *testing.T, addr, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err != nil {
			return
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatalf("%s: something is still accepting connections on %s", what, addr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// assertConnectionStopsAnswering fails unless the connection stops serving
// requests within a short window. Polled rather than checked once: the socket
// is closed by a watchdog goroutine when its context is cancelled, so "closed"
// is prompt but not synchronous with the call that caused it.
func assertConnectionStopsAnswering(t *testing.T, c *remoteTestClient, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := c.sendRaw(`{"type":"ListTools"}`); err != nil {
			return
		}
		if _, err := c.readFrame(); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: the connection was still being served", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Issue #21: a record written by another process is honoured immediately
// ---------------------------------------------------------------------------

// The headline. `relay enrol create` runs in a CLI process; the tray owns the
// listener. Before the fix the listener resolved certificates against its own
// cached settings, so a freshly created enrolment was refused as "certificate
// is not enrolled" until the tray's next settings poll — a refusal
// indistinguishable from a genuine misconfiguration, whose diagnostic sends the
// operator to `relay enrol list`, which shows the record present.
func TestRemoteServer_AcceptsAnEnrolmentCreatedByAnotherProcess(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})

	// Enrol through a second store. The running listener's store never sees
	// this call — only the file does.
	bundle, err := createEnrolment(f.cliWriter(), enrolmentRequest{
		ClientID:   "hermes-cli",
		ProjectIDs: []string{f.project.ID},
	})
	assertNoErr(t, err, "createEnrolment through a separate settings writer")

	// Precondition, not decoration: this test is only meaningful while the
	// listener's own cached view still lags the file. If this ever fails, the
	// cache stopped lagging and the test no longer exercises issue #21 — read
	// the fix before deleting the assertion.
	if f.store.Get().FindEnrolmentByFingerprint(bundle.Enrolment.Fingerprint) != nil {
		t.Fatal("the listener's cached settings already hold the new enrolment; " +
			"this test no longer reproduces the cross-process condition it was written for")
	}

	c := f.dialAddr(f.server.Addr(), bundle)
	if c == nil {
		t.Fatal("a client enrolled by another process could not complete the handshake")
	}
	if resp := c.roundTrip(`{"type":"ListTools"}`); resp.Type != bridge.RespTools {
		t.Fatalf("an enrolment created by another process was refused: %s %s", resp.Type, resp.Message)
	}
	if resp := c.roundTrip(`{"type":"CallTool","name":"mail_search"}`); resp.Type != bridge.RespResult {
		t.Fatalf("a tool call on a freshly created enrolment was refused: %s %s", resp.Type, resp.Message)
	}
}

// The asymmetry the fix had to preserve, from the other side: a record DELETED
// by another process must stop being honoured on the very next request, on a
// connection that is already open. The revocation hook cannot help here — it is
// a process-global, and the process doing the deleting is not the one holding
// the socket — so this is exactly the guarantee that rests on re-resolving the
// enrolment from the file on every request.
func TestRemoteServer_RevocationByAnotherProcessTakesEffectMidSession(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})
	c := f.dial()

	if resp := c.roundTrip(`{"type":"ListTools"}`); resp.Type != bridge.RespTools {
		t.Fatalf("baseline ListTools failed: %s", resp.Message)
	}

	// Delete the record through a second store and do NOT fire the hook: this
	// is the cross-process shape, where the listener is never told.
	assertNoErr(t, f.cliWriter().With(func(s *Settings) {
		if _, ok := s.RemoveEnrolment("hermes-mail"); !ok {
			t.Error("the enrolment was not there to remove")
		}
	}), "revoke through a separate settings writer")

	resp := c.roundTrip(`{"type":"CallTool","name":"mail_search"}`)
	if resp.Type != bridge.RespError {
		t.Fatalf("a revoked certificate was still served on its open connection: %s", resp.Type)
	}
	if !strings.Contains(resp.Message, "no longer enrolled") {
		t.Errorf("refusal message = %q, want it to name the missing enrolment", resp.Message)
	}
	if f.mcpCalls.Load() != 0 {
		t.Error("a revoked certificate reached an MCP")
	}
}

// And in-process revocation still severs the live SOCKET rather than merely
// refusing the next request — the stronger guarantee, unchanged. Kept beside
// the cross-process test so the difference between the two is visible: one
// closes the connection, the other refuses on it.
func TestRemoteServer_InProcessRevocationStillClosesTheConnection(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})
	c := f.dial()

	if resp := c.roundTrip(`{"type":"ListTools"}`); resp.Type != bridge.RespTools {
		t.Fatalf("baseline ListTools failed: %s", resp.Message)
	}
	if _, err := revokeEnrolment(f.store, "hermes-mail"); err != nil {
		t.Fatalf("revokeEnrolment: %v", err)
	}
	assertConnectionStopsAnswering(t, c, "a revoked enrolment's live connection")
}

// ---------------------------------------------------------------------------
// Lifecycle: enable, disable
// ---------------------------------------------------------------------------

// Turning the block on binds a listener and turning it off closes it, with no
// restart in between — and "off" means the port stops answering, not that calls
// start being refused.
func TestRemoteSupervisor_EnablingStartsAListenerAndDisablingStopsIt(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{noRemoteBlock: true, skipServe: true})
	sup := f.supervise()

	assertNoErr(t, sup.Reconcile(), "reconcile with no remote block")
	if addr := sup.Addr(); addr != "" {
		t.Fatalf("a listener was opened with no remote block, bound to %s", addr)
	}

	// Enable it from another process.
	assertNoErr(t, f.cliWriter().With(func(s *Settings) {
		s.Remote = &RemoteConfig{Enabled: ptr(true), Listen: "127.0.0.1:0"}
	}), "enable the remote block")

	assertNoErr(t, sup.Reconcile(), "reconcile after enabling")
	addr := sup.Addr()
	if addr == "" {
		t.Fatal("enabling the remote block did not open a listener")
	}
	c := f.dialAddr(addr, f.bundle)
	if c == nil {
		t.Fatalf("the newly bound listener at %s refused a handshake", addr)
	}
	if resp := c.roundTrip(`{"type":"ListTools"}`); resp.Type != bridge.RespTools {
		t.Fatalf("the newly bound listener refused an enrolled client: %s %s", resp.Type, resp.Message)
	}

	// Disable it again.
	assertNoErr(t, f.cliWriter().With(func(s *Settings) {
		s.Remote = &RemoteConfig{Enabled: ptr(false), Listen: "127.0.0.1:0"}
	}), "disable the remote block")

	assertNoErr(t, sup.Reconcile(), "reconcile after disabling")
	if got := sup.Addr(); got != "" {
		t.Fatalf("disabling left a listener bound to %s", got)
	}
	assertNothingListensAt(t, addr, "after disabling the remote block")
	assertConnectionStopsAnswering(t, c, "a connection open when the listener was disabled")
}

// The safe default has to survive reconciliation, which is the one place it
// could quietly be lost: an absent block and a block that names an address but
// omits `enabled` must BOTH stay closed, however many times convergence runs.
// This is deliberately the opposite of AuditConfig's default — a missing audit
// block keeps recording, a missing remote enable opens nothing — because
// starting a network listener nobody asked for is the failure ADR-010 exists to
// avoid.
func TestRemoteSupervisor_NeverOpensAListenerConfigDoesNotAskFor(t *testing.T) {
	for name, opts := range map[string]remoteFixtureOpts{
		"absent block":    {noRemoteBlock: true, skipServe: true},
		"enabled omitted": {omitEnabled: true, skipServe: true},
		"explicit false":  {enabled: ptr(false), skipServe: true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRemoteFixture(t, opts)
			sup := f.supervise()
			for i := 0; i < 3; i++ {
				assertNoErr(t, sup.Reconcile(), "reconcile %d", i)
				if addr := sup.Addr(); addr != "" {
					t.Fatalf("reconcile %d opened a listener at %s for %q", i, addr, name)
				}
			}
			if owner := enrolmentRevocationHookOwner(); owner != nil {
				t.Errorf("a revocation hook was installed with no listener running: %v", owner)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Lifecycle: rebinding
// ---------------------------------------------------------------------------

// Changing `remote.listen` moves the listener: the new address serves and the
// old one stops answering. Live connections on the old address are CUT, and
// that is a decision rather than an accident — `listen` is the reachability
// control, so a narrowed bind that left established sessions running on the old
// address would not have narrowed anything for the client that matters most,
// and this protocol holds persistent connections in a scanner loop, so "they
// drop eventually" means never. The client reconnects at the new address with
// its identity and grants untouched.
func TestRemoteSupervisor_ChangingTheListenAddressMovesTheListener(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{skipServe: true})
	sup := f.supervise()
	assertNoErr(t, sup.Reconcile(), "initial reconcile")

	first := sup.Addr()
	if first == "" {
		t.Fatal("no listener was bound")
	}
	c := f.dialAddr(first, f.bundle)
	if c == nil {
		t.Fatal("could not connect to the first listener")
	}
	if resp := c.roundTrip(`{"type":"ListTools"}`); resp.Type != bridge.RespTools {
		t.Fatalf("baseline ListTools on the first address failed: %s", resp.Message)
	}

	moved := freeLoopbackAddr(t)
	assertNoErr(t, f.cliWriter().With(func(s *Settings) {
		s.Remote = &RemoteConfig{Enabled: ptr(true), Listen: moved}
	}), "move the listen address")

	assertNoErr(t, sup.Reconcile(), "reconcile after moving the listen address")
	if got := sup.Addr(); got != moved {
		t.Fatalf("listener is bound to %q, want the newly configured %q", got, moved)
	}
	assertNothingListensAt(t, first, "after the listen address moved")
	assertConnectionStopsAnswering(t, c, "a connection open on the old address")

	// The move is only real if the new address actually serves.
	c2 := f.dialAddr(moved, f.bundle)
	if c2 == nil {
		t.Fatalf("the moved listener at %s refused a handshake", moved)
	}
	if resp := c2.roundTrip(`{"type":"CallTool","name":"mail_search"}`); resp.Type != bridge.RespResult {
		t.Fatalf("the moved listener refused an enrolled client: %s %s", resp.Type, resp.Message)
	}
}

// A reconcile that changes nothing must change nothing: rebinding an unchanged
// address every poll would cut every live connection every two seconds.
func TestRemoteSupervisor_RepeatedReconcileLeavesTheListenerAndItsConnectionsAlone(t *testing.T) {
	addr := freeLoopbackAddr(t)
	f := newRemoteFixture(t, remoteFixtureOpts{listen: addr, skipServe: true})
	sup := f.supervise()
	assertNoErr(t, sup.Reconcile(), "initial reconcile")

	server := sup.Server()
	c := f.dialAddr(addr, f.bundle)
	if c == nil {
		t.Fatal("could not connect to the listener")
	}

	for i := 0; i < 3; i++ {
		assertNoErr(t, sup.Reconcile(), "no-op reconcile %d", i)
		if sup.Server() != server {
			t.Fatalf("no-op reconcile %d replaced the listener", i)
		}
		if got := sup.Addr(); got != addr {
			t.Fatalf("no-op reconcile %d moved the listener to %s", i, got)
		}
		if resp := c.roundTrip(`{"type":"ListTools"}`); resp.Type != bridge.RespTools {
			t.Fatalf("no-op reconcile %d disturbed a live connection: %s %s", i, resp.Type, resp.Message)
		}
	}
}

// A rebind that cannot bind must be LOUD and must leave the old listener
// serving. The failure this guards against is the quiet one: no listener, no
// error, and an operator who believes the address change took.
func TestRemoteSupervisor_FailedRebindKeepsTheOldListenerAndReportsTheError(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{skipServe: true})
	sup := f.supervise()
	assertNoErr(t, sup.Reconcile(), "initial reconcile")

	first := sup.Addr()
	if first == "" {
		t.Fatal("no listener was bound")
	}

	taken := occupiedLoopbackAddr(t)
	assertNoErr(t, f.cliWriter().With(func(s *Settings) {
		s.Remote = &RemoteConfig{Enabled: ptr(true), Listen: taken}
	}), "point listen at an occupied port")

	err := sup.Reconcile()
	if err == nil {
		t.Fatal("binding an occupied port reported no error")
	}
	if !strings.Contains(err.Error(), taken) {
		t.Errorf("the error does not name the address that could not be bound: %v", err)
	}
	if got := sup.Addr(); got != first {
		t.Fatalf("a failed rebind left the listener at %q, want the old %q still serving", got, first)
	}

	// Still serving, not merely still bound.
	c := f.dialAddr(first, f.bundle)
	if c == nil {
		t.Fatal("the old listener stopped accepting after a failed rebind")
	}
	if resp := c.roundTrip(`{"type":"ListTools"}`); resp.Type != bridge.RespTools {
		t.Fatalf("the old listener stopped serving after a failed rebind: %s %s", resp.Type, resp.Message)
	}

	// And the hook still points at the listener that is actually live.
	if owner := enrolmentRevocationHookOwner(); owner != sup.Server() {
		t.Errorf("after a failed rebind the revocation hook owner is %v, want the live listener %v", owner, sup.Server())
	}
}

// The same failure from cold: nothing bound, and the operator is told. There is
// no listener to fall back on, so the error is the whole of the report.
func TestRemoteSupervisor_FailedFirstBindOpensNothingAndReportsTheError(t *testing.T) {
	taken := occupiedLoopbackAddr(t)
	f := newRemoteFixture(t, remoteFixtureOpts{listen: taken, skipServe: true})
	sup := f.supervise()

	err := sup.Reconcile()
	if err == nil {
		t.Fatal("binding an occupied port from cold reported no error")
	}
	if got := sup.Addr(); got != "" {
		t.Fatalf("a failed first bind left a listener at %s", got)
	}

	// It retries: freeing the address is enough, with no operator action.
	free := freeLoopbackAddr(t)
	assertNoErr(t, f.cliWriter().With(func(s *Settings) {
		s.Remote = &RemoteConfig{Enabled: ptr(true), Listen: free}
	}), "point listen at a free port")
	assertNoErr(t, sup.Reconcile(), "reconcile after the address became bindable")
	if got := sup.Addr(); got != free {
		t.Fatalf("listener is bound to %q, want %q", got, free)
	}
}

// ---------------------------------------------------------------------------
// Auditing is a hard dependency, at runtime and not only at launch
// ---------------------------------------------------------------------------

// The case for letting a VM reach host mail rests entirely on the calls being
// recorded. Turning auditing off therefore has to CLOSE the listener — leaving
// it serving until the next restart would make the refusal decorative — and it
// has to cut the connections already established, because a live socket is
// exactly where an unrecorded call would come from.
func TestRemoteSupervisor_DisablingAuditingAtRuntimeStopsTheListener(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{skipServe: true})
	sup := f.supervise()
	assertNoErr(t, sup.Reconcile(), "initial reconcile")

	addr := sup.Addr()
	if addr == "" {
		t.Fatal("no listener was bound")
	}
	c := f.dialAddr(addr, f.bundle)
	if c == nil {
		t.Fatal("could not connect to the listener")
	}
	if resp := c.roundTrip(`{"type":"ListTools"}`); resp.Type != bridge.RespTools {
		t.Fatalf("baseline ListTools failed: %s", resp.Message)
	}

	assertNoErr(t, f.cliWriter().With(func(s *Settings) {
		s.Audit = &AuditConfig{Enabled: ptr(false)}
	}), "disable auditing")

	err := sup.Reconcile()
	if err == nil {
		t.Fatal("disabling auditing left the listener running and reported nothing")
	}
	if !strings.Contains(err.Error(), "audit") {
		t.Errorf("the refusal does not explain itself to an operator: %v", err)
	}
	if got := sup.Addr(); got != "" {
		t.Fatalf("disabling auditing left a listener bound to %s", got)
	}
	assertNothingListensAt(t, addr, "after auditing was disabled")
	assertConnectionStopsAnswering(t, c, "a connection open when auditing was disabled")
	if f.mcpCalls.Load() != 0 {
		t.Error("a tool call ran after auditing was disabled")
	}
}

// Between the settings write and the next reconcile there is a window of up to
// one poll interval, and a request arriving in it must still be refused: the
// per-request path re-reads the audit setting for the same reason it re-reads
// the enrolment. Nothing here reconciles — that is the point.
func TestRemoteServer_RefusesCallsAsSoonAsAuditingIsDisabled(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})
	c := f.dial()

	if resp := c.roundTrip(`{"type":"ListTools"}`); resp.Type != bridge.RespTools {
		t.Fatalf("baseline ListTools failed: %s", resp.Message)
	}

	assertNoErr(t, f.cliWriter().With(func(s *Settings) {
		s.Audit = &AuditConfig{Enabled: ptr(false)}
	}), "disable auditing")

	resp := c.roundTrip(`{"type":"CallTool","name":"mail_search"}`)
	if resp.Type != bridge.RespError {
		t.Fatalf("a call ran with auditing disabled: %s", resp.Type)
	}
	if !strings.Contains(resp.Message, "audit") {
		t.Errorf("refusal message = %q, want it to name auditing", resp.Message)
	}
	if f.mcpCalls.Load() != 0 {
		t.Error("a call with auditing disabled reached an MCP")
	}
}

// ---------------------------------------------------------------------------
// The revocation hook survives a reconcile cycle
// ---------------------------------------------------------------------------

// A rebind tears down one listener and builds another, and the revocation hook
// is a process-global that both of them touch. Get the ordering wrong — clear
// on teardown after the replacement installed its own — and revocation silently
// stops severing live connections while every test that only looks at the
// deleted record still passes. So: exactly one hook, owned by the listener that
// is actually live, and it still works.
func TestRemoteSupervisor_RevocationHookIsInstalledOnceAndFollowsTheLiveListener(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{skipServe: true})
	sup := f.supervise()
	assertNoErr(t, sup.Reconcile(), "initial reconcile")

	first := sup.Server()
	if first == nil {
		t.Fatal("no listener was bound")
	}
	if owner := enrolmentRevocationHookOwner(); owner != first {
		t.Fatalf("revocation hook owner = %v, want the listener just started %v", owner, first)
	}

	moved := freeLoopbackAddr(t)
	assertNoErr(t, f.cliWriter().With(func(s *Settings) {
		s.Remote = &RemoteConfig{Enabled: ptr(true), Listen: moved}
	}), "move the listen address")
	assertNoErr(t, sup.Reconcile(), "reconcile after moving the listen address")

	second := sup.Server()
	if second == nil || second == first {
		t.Fatalf("the rebind did not produce a new listener (got %v, was %v)", second, first)
	}
	owner := enrolmentRevocationHookOwner()
	if owner == nil {
		t.Fatal("the rebind left NO revocation hook installed: revocation would no longer cut live connections")
	}
	if owner != second {
		t.Fatalf("revocation hook owner = %v, want the live listener %v", owner, second)
	}

	// Installed is not the same as working. Revoke for real and watch the live
	// connection on the CURRENT listener go.
	c := f.dialAddr(moved, f.bundle)
	if c == nil {
		t.Fatal("could not connect to the moved listener")
	}
	if resp := c.roundTrip(`{"type":"ListTools"}`); resp.Type != bridge.RespTools {
		t.Fatalf("baseline ListTools on the moved listener failed: %s", resp.Message)
	}
	if _, err := revokeEnrolment(f.store, "hermes-mail"); err != nil {
		t.Fatalf("revokeEnrolment: %v", err)
	}
	assertConnectionStopsAnswering(t, c, "a revoked enrolment's connection on the rebound listener")

	// And stopping altogether leaves no hook behind pointing at a dead server.
	assertNoErr(t, f.cliWriter().With(func(s *Settings) {
		s.Remote = &RemoteConfig{Enabled: ptr(false)}
	}), "disable the remote block")
	assertNoErr(t, sup.Reconcile(), "reconcile after disabling")
	if owner := enrolmentRevocationHookOwner(); owner != nil {
		t.Errorf("a stopped listener left its revocation hook installed: %v", owner)
	}
}
