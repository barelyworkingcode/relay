package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/presence/presencetest"
)

// bpdSignallingRouter reports when an admin op has finished, so the test
// learns completion without closing the server (which would cancel every
// request ctx and hide the defect under test).
type bpdSignallingRouter struct {
	*appRouter
	done chan error
}

func (r *bpdSignallingRouter) AdminOp(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	out, err := r.appRouter.AdminOp(ctx, name, args)
	r.done <- err
	return out, err
}

func TestBridge_DisconnectedCallerDoesNotCompleteGatedOp(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")

	prompt := presencetest.NewBlocking()
	gate, err := presence.NewGate(prompt)
	assertNoErr(t, err, "NewGate")
	rec := enabledIssuanceRecorder(t)

	router := &bpdSignallingRouter{
		appRouter: &appRouter{
			store: store,
			credentialOps: &CredentialOps{
				Store:    store,
				Gate:     gate,
				Issuance: issuanceAuditorOrNil(rec),
			},
		},
		done: make(chan error, 1),
	}

	ctx, cancel := context.WithCancel(context.Background())
	bs, err := bridge.NewBridgeServer(ctx, router)
	assertNoErr(t, err, "NewBridgeServer")
	bs.SetCallerSessionResolverForTest(func(net.Conn) presence.CallerSession {
		return presence.CallerSession{GraphicAccess: true}
	})
	go bs.Serve()
	t.Cleanup(func() {
		prompt.Release(presence.ErrRefused)
		bs.Close()
		cancel()
	})

	var suffix [4]byte
	_, err = rand.Read(suffix[:])
	assertNoErr(t, err, "rand")
	name := "stale-probe-" + hex.EncodeToString(suffix[:])

	args, err := json.Marshal(credentialMintRequest{Name: name, Classes: []string{"read"}})
	assertNoErr(t, err, "marshal args")
	frame, err := json.Marshal(bridge.BridgeRequest{Type: bridge.ReqAdminOp, Name: "credential.mint", Arguments: args})
	assertNoErr(t, err, "marshal frame")

	conn, err := net.Dial("unix", bridge.SocketPath())
	assertNoErr(t, err, "dial bridge")
	_, err = conn.Write(append(frame, '\n'))
	assertNoErr(t, err, "write frame")

	select {
	case <-prompt.Entered():
	case <-time.After(5 * time.Second):
		_ = conn.Close()
		t.Fatal("credential.mint never reached the presence prompt")
	}

	assertNoErr(t, conn.Close(), "close client conn")

	// This is subtle: the owner's answer must come after relay has seen the
	// disconnect. An approval that lands before relay could know the caller
	// left is a legitimate approve, so releasing early would test nothing.
	// The wait is bounded and not fatal on its own: when relay never notices,
	// the assertions below are what report it.
	select {
	case <-prompt.CtxDone():
	case <-time.After(3 * time.Second):
	}
	prompt.Release(nil)

	var opErr error
	select {
	case opErr = <-router.done:
	case <-time.After(5 * time.Second):
		t.Fatal("credential.mint did not finish after the prompt was answered")
	}

	for _, c := range store.Get().APICredentials {
		if c.Name == name {
			t.Errorf("credential %q (id %s) was minted for a caller that had disconnected before the prompt was answered", name, c.ID)
		}
	}
	rec.Flush()
	for _, ev := range audit.ReadAuditTail(rec.Path(), audit.AuditTailBudget) {
		if ev.Event == audit.AuditEventCredentialIssued {
			t.Errorf("%s row recorded (subject %q) for a caller that had disconnected", ev.Event, ev.Subject)
		}
	}
	if opErr == nil {
		t.Error("credential.mint returned no error for a caller that had disconnected before the prompt was answered")
	}
}
