package main

// The gap this file closes: every existing hermetic test for the enrolment
// channel built remoteConfigFields (or ipcRemoteConfigMsg) directly, so a UI
// or wire path that never carried enrolment_requests/enrolment_listen at all
// still passed every test in the suite — the channel worked end to end, but
// nothing on the operator's own surfaces could turn it on. See ADR-018
// decision 8: opening a network door is a thing the operator says, not a
// thing relay infers, and that has to be provable from the doors an operator
// actually has.
//
// These tests drive the two real doors instead of the struct behind them:
//   - ipcHandlers[MsgUpdateRemoteConfig], the exact map onSettingsIpc
//     dispatches a WebView message through.
//   - PUT /api/remote through a real control.RouteRegistrar, the exact route
//     enrolment_routes.go registers.
//
// Both assert on the SUPERVISOR actually binding or closing a socket, not on
// the settings field the handler wrote — a struct field can be right while
// nothing downstream ever reads it, which is exactly how this gap shipped.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/barelyworkingcode/relay/internal/control"
)

// newOperatorSurfaceFixture wires a real RemoteSupervisor and a real
// EnrolmentOps over the SAME store, exactly as trayapp.go wires
// a.ipcCtx.EnrolmentOps and the supervisor from one *Settings — never two
// independent copies that could drift from each other.
func newOperatorSurfaceFixture(t *testing.T) (*remoteFixture, *RemoteSupervisor, *EnrolmentOps) {
	t.Helper()
	f := newRemoteFixture(t, remoteFixtureOpts{skipServe: true})
	sup := f.supervise()
	// Gate: allowGate(t) -- both doors below toggle enabled/enrolment_requests
	// on, which is exactly what remote.configure gates (enrolment_ops.go's
	// remoteConfigChangedFields); a nil Gate would refuse before either ever
	// reaches the supervisor this file is actually testing.
	ops := &EnrolmentOps{Store: f.store, Audit: f.audit, Gate: allowGate(t)}
	return f, sup, ops
}

// TestIPCUpdateRemoteConfig_TogglingEnrolmentRequestsOpensAndClosesTheListener
// drives update_remote_config through the real ipcHandlers map — the
// dispatch table onSettingsIpc consults for every message a settings WebView
// click ever sends — and proves the enrolment-request listener actually
// binds a socket in response, and actually stops answering when the
// operator switches it back off.
func TestIPCUpdateRemoteConfig_TogglingEnrolmentRequestsOpensAndClosesTheListener(t *testing.T) {
	_, sup, ops := newOperatorSurfaceFixture(t)
	ui := &recordingUI{}
	// GoFunc runs synchronously: ipcUpdateRemoteConfig now hops off the main
	// thread before calling the (possibly gated) core, the same deadlock
	// precaution ipcCreateEnrolment already takes.
	ctx := &IPCContext{Ctx: context.Background(), EnrolmentOps: ops, UI: ui, Platform: stubPlatform{}, GoFunc: func(fn func()) { fn() }}

	handler, ok := ipcHandlers[MsgUpdateRemoteConfig]
	if !ok {
		t.Fatal("ipcHandlers has no entry for update_remote_config")
	}

	// Ephemeral addresses for both listeners: never the package defaults, so
	// this test cannot collide with a developer's own running relay or with
	// another test in the same run.
	toolListen := freeLoopbackAddr(t)
	enrolListen := freeLoopbackAddr(t)
	handler(ctx, json.RawMessage(`{
		"enabled": true,
		"listen": "`+toolListen+`",
		"enrolment_requests": true,
		"enrolment_listen": "`+enrolListen+`"
	}`))
	if ui.hasEvent("onRemoteConfigError") {
		t.Fatalf("update_remote_config with enrolment_requests:true errored: %+v", ui.events)
	}

	assertNoErr(t, sup.Reconcile(), "reconcile after enabling enrolment_requests over IPC")

	addr := sup.EnrolAddr()
	if addr == "" {
		t.Fatal("the IPC path set enrolment_requests but no enrolment listener opened")
	}
	if addr != enrolListen {
		t.Fatalf("enrolment listener bound %q, want the address the IPC message carried %q", addr, enrolListen)
	}
	conn, err := net.Dial("tcp", addr)
	assertNoErr(t, err, "dial the enrolment listener the IPC path opened")
	_ = conn.Close()

	// Untoggle: the same message type, enrolment_requests now false. This is
	// the control the settings checkbox drives — removing it (or defaulting
	// it back on) is exactly the regression this test exists to catch.
	handler(ctx, json.RawMessage(`{
		"enabled": true,
		"listen": "`+toolListen+`",
		"enrolment_requests": false
	}`))
	if ui.hasEvent("onRemoteConfigError") {
		t.Fatalf("update_remote_config with enrolment_requests:false errored: %+v", ui.events)
	}
	assertNoErr(t, sup.Reconcile(), "reconcile after disabling enrolment_requests over IPC")

	if got := sup.EnrolAddr(); got != "" {
		t.Fatalf("enrolment listener still reports address %q after the IPC path turned enrolment_requests off", got)
	}
	assertNothingListensAt(t, addr, "enrolment listener after the IPC path turned enrolment_requests off")
}

// TestHTTPPutRemote_TogglingEnrolmentRequestsOpensAndClosesTheListener is the
// same proof over PUT /api/remote — the HTTP door RegisterEnrolmentRoutes
// registers, socket-only (control.ClassExecute), which is the door `relay` itself
// and any control-plane credential holder both go through.
func TestHTTPPutRemote_TogglingEnrolmentRequestsOpensAndClosesTheListener(t *testing.T) {
	_, sup, ops := newOperatorSurfaceFixture(t)

	mux := http.NewServeMux()
	RegisterEnrolmentRoutes(&control.RouteRegistrar{CredentialID: APICredentialIDFromContext, Mux: mux, Transport: control.TransportSocket}, ops)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	toolListen := freeLoopbackAddr(t)
	enrolListen := freeLoopbackAddr(t)
	resp, body := doJSON(t, "PUT", srv.URL+"/api/remote", map[string]interface{}{
		"enabled":            true,
		"listen":             toolListen,
		"enrolment_requests": true,
		"enrolment_listen":   enrolListen,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /api/remote with enrolment_requests:true: %d %s", resp.StatusCode, body)
	}

	assertNoErr(t, sup.Reconcile(), "reconcile after enabling enrolment_requests over HTTP")

	addr := sup.EnrolAddr()
	if addr == "" {
		t.Fatal("PUT /api/remote set enrolment_requests but no enrolment listener opened")
	}
	if addr != enrolListen {
		t.Fatalf("enrolment listener bound %q, want the address the HTTP body carried %q", addr, enrolListen)
	}
	conn, err := net.Dial("tcp", addr)
	assertNoErr(t, err, "dial the enrolment listener the HTTP path opened")
	_ = conn.Close()

	resp, body = doJSON(t, "PUT", srv.URL+"/api/remote", map[string]interface{}{
		"enabled":            true,
		"listen":             toolListen,
		"enrolment_requests": false,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /api/remote with enrolment_requests:false: %d %s", resp.StatusCode, body)
	}
	assertNoErr(t, sup.Reconcile(), "reconcile after disabling enrolment_requests over HTTP")

	if got := sup.EnrolAddr(); got != "" {
		t.Fatalf("enrolment listener still reports address %q after the HTTP path turned enrolment_requests off", got)
	}
	assertNothingListensAt(t, addr, "enrolment listener after the HTTP path turned enrolment_requests off")
}
