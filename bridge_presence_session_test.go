package main

// End-to-end coverage for the gap the S6 developer correctly left for S7:
// bridge/server.go's handleConn now resolves the peer's kernel audit
// session (bridge.PeerCallerSession) and attaches it via
// presence.WithCallerSession, once per connection, beside PeerPID. Every
// test elsewhere in this suite that exercises AC-19's refusal exercises
// package presence directly (presence_test.go, digest_test.go) or
// presence's own session_darwin_test.go — never the actual bridge socket a
// real `relay credential mint` dials. That is exactly why the wiring gap
// survived S5/S6: nothing forced the production code path to run.
//
// bridge.BridgeServer.SetCallerSessionResolverForTest exists because a real
// peer's AU_SESSION_FLAG_HAS_GRAPHIC_ACCESS bit is whatever the process
// running `go test` happens to have — there is no way to force it off from
// inside the test binary — and this test needs that fact pinned rather than
// ambient. It does not touch package presence and cannot weaken Gate.Request
// in any way: it only substitutes the one input Gate.Request already reads
// off the context.

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"relaygo/bridge"
	"relaygo/presence"
	"relaygo/presence/presencetest"
)

// bpsStartBridge wires a real appRouter (one gated core, CredentialOps) to a
// real bridge.BridgeServer over a real Unix socket in the sandboxed config
// dir, so a test can dial ReqAdminOp exactly the way `relay credential
// mint` does. Returns the client and the Recording provider so a test can
// assert both the outcome and whether the provider was ever reached.
func bpsStartBridge(t *testing.T, graphicAccess bool, evalResult error) (*bridge.Client, *presencetest.Recording) {
	t.Helper()
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")

	recording := presencetest.NewRecording(evalResult)
	gate, err := presence.NewGate(recording)
	assertNoErr(t, err, "NewGate")

	router := &appRouter{
		store: store,
		credentialOps: &CredentialOps{
			Store:    store,
			Gate:     gate,
			Issuance: pgwWithIssuance(t),
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	bs, err := bridge.NewBridgeServer(ctx, router)
	assertNoErr(t, err, "NewBridgeServer")
	bs.SetCallerSessionResolverForTest(func(net.Conn) presence.CallerSession {
		return presence.CallerSession{GraphicAccess: graphicAccess}
	})
	go bs.Serve()
	t.Cleanup(func() {
		bs.Close()
		cancel()
	})

	return bridge.NewClient(""), recording
}

// TestBridge_NoGraphicAccessRefusesWithoutPromptingTheProvider is §6.6's
// load-bearing row, proven through the real transport: a peer whose kernel
// audit session reports no graphic access must have a gated admin_op refuse
// with presence.ErrNoSession, and the presence provider — the thing that
// would otherwise raise a LocalAuthentication prompt on the physical
// console for an SSH caller — must never be called at all.
func TestBridge_NoGraphicAccessRefusesWithoutPromptingTheProvider(t *testing.T) {
	client, recording := bpsStartBridge(t, false, nil)

	args, err := json.Marshal(credentialMintRequest{Name: "ssh-attempt", Classes: []string{"read"}})
	assertNoErr(t, err, "marshal args")

	_, opErr := client.AdminOp("credential.mint", args)
	if opErr == nil {
		t.Fatal("credential.mint succeeded from a peer with no graphic access")
	}
	if !strings.Contains(opErr.Error(), presence.ErrNoSession.Error()) {
		t.Fatalf("error = %q, want it to contain %q", opErr.Error(), presence.ErrNoSession.Error())
	}
	if n := recording.Calls(); n != 0 {
		t.Fatalf("presence provider called %d time(s); a caller that cannot show a prompt must never reach it", n)
	}
}

// TestBridge_GraphicAccessPromptsAndSucceeds is the positive control: a
// peer WITH graphic access must still reach the provider and succeed on an
// allowing one — proving the fix does not collapse row 1 (prompt) into
// row 2 (refuse) by mistake.
func TestBridge_GraphicAccessPromptsAndSucceeds(t *testing.T) {
	client, recording := bpsStartBridge(t, true, nil)

	args, err := json.Marshal(credentialMintRequest{Name: "console-mint", Classes: []string{"read"}})
	assertNoErr(t, err, "marshal args")

	if _, opErr := client.AdminOp("credential.mint", args); opErr != nil {
		t.Fatalf("credential.mint from a graphic-access peer with an allowing provider: %v", opErr)
	}
	if n := recording.Calls(); n != 1 {
		t.Fatalf("presence provider called %d time(s), want exactly 1", n)
	}
}

// TestBridge_UndeterminedSessionRefusesLikeNoGraphicAccess is the fourth
// row presence.WithCallerSession's own doc comment insists on: a peer
// whose session could not be determined at all must refuse exactly like a
// confirmed non-graphic one, never fall through to "no session on the
// context" (which would prompt). bridge.PeerCallerSession resolves this
// itself for a non-UnixConn or a failed probe; this test pins the
// resolver's OWN contract directly, since forcing the real kernel probe to
// fail from inside a test is not reliably possible.
func TestPeerCallerSession_NonUnixConnRefusesRatherThanOmittingSession(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	sess := bridge.PeerCallerSession(server)
	if sess.GraphicAccess {
		t.Fatal("a non-UnixConn peer resolved to GraphicAccess: true — an undetermined peer must refuse, not prompt")
	}
}
