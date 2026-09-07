package main

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
)

// TestAdminOps_TableHasExactlyTheS6Operations pins §7.2's operation table:
// every mutating CLI command S6 brokers has an entry, and nothing else does.
// `project.rotate_token` and `project.grant` are presence-gated
// (presence.GatedOps) but have no admin_op entry — they have no CLI
// surface (§7.2: "— (IPC + HTTP today)") — and `mcp.oauth.start` is
// IPC-only by design (ADR-014 section 4), so neither belongs here. Adding
// an op to this table without adding it here, or the reverse, fails the
// test by name.
func TestAdminOps_TableHasExactlyTheS6Operations(t *testing.T) {
	want := []string{
		"credential.mint",
		"credential.revoke",
		"enrolment.create",
		"enrolment.sign",
		"enrolment.update",
		"enrolment.revoke",
		"enrolment.request.list",
		"enrolment.request.approve",
		"enrolment.request.refuse",
		"login.bootstrap.mint",
		"login.passkey.revoke",
		"mcp.register",
		"mcp.unregister",
		"service.register",
		"service.unregister",
		"service.restart",
		"eve.enrolment.open",
		"eve.passkey.revoke",
	}
	sort.Strings(want)

	var got []string
	for op := range adminOps {
		got = append(got, op)
	}
	sort.Strings(got)

	if len(got) != len(want) {
		t.Fatalf("adminOps has %d entries, want %d: got=%v want=%v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("adminOps key set = %v, want %v", got, want)
		}
	}
}

// TestAppRouter_AdminOpRefusesUnknownOperation exercises the table through
// appRouter.AdminOp exactly as bridge/server.go's handleAdminOp calls it: an
// operation name the table doesn't hold must come back as an error, never a
// panic from an unchecked map lookup or a nil handler invocation.
func TestAppRouter_AdminOpRefusesUnknownOperation(t *testing.T) {
	r := newTestRouter(t, makeSettings(nil, nil, nil), mcpbroker.NewManager(nil))

	result, err := r.AdminOp(context.Background(), "no.such.operation", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("AdminOp accepted an operation not in adminOps")
	}
	if result != nil {
		t.Errorf("AdminOp returned a non-nil result alongside an error: %s", result)
	}
}

// TestAppRouter_AdminOpRefusesWhenCoreNotWired proves requireXxxOps's own
// refusal path: a router that never had its ops cores wired (as a test
// appRouter that forgot to, or a shipped build with a wiring bug) refuses
// by name rather than panicking three calls deep inside a nil core.
func TestAppRouter_AdminOpRefusesWhenCoreNotWired(t *testing.T) {
	r := newTestRouter(t, makeSettings(nil, nil, nil), mcpbroker.NewManager(nil))

	_, err := r.AdminOp(context.Background(), "credential.mint", json.RawMessage(`{"name":"x","classes":["read"]}`))
	if err == nil {
		t.Fatal("AdminOp succeeded with no CredentialOps wired")
	}
}

// TestAdminEveEnrolmentOpen_DispatchesIntoEveEnrolmentOps proves
// "eve.enrolment.open" reaches the SAME core `relay eve enrol` and the
// tray's own menu item call, exactly as adminLoginBootstrapMint's own
// dispatch does for "login.bootstrap.mint" — newBrokerRouter wires
// EveEnrolmentOps with an allowing gate and a live issuance auditor, the
// shape that proves a brokered call genuinely dispatches into its core
// rather than merely reaching the transport.
func TestAdminEveEnrolmentOpen_DispatchesIntoEveEnrolmentOps(t *testing.T) {
	store := newCLISandboxStore(t)
	r := newBrokerRouter(t, store, nil)

	raw, err := r.AdminOp(context.Background(), "eve.enrolment.open", json.RawMessage(`{}`))
	assertNoErr(t, err, "AdminOp eve.enrolment.open")

	var view eveEnrolmentStatusView
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if !view.Open || view.Expires == "" {
		t.Fatalf("result = %+v, want an open window", view)
	}
	if store.Get().EveEnrolment == nil {
		t.Fatal("no eve_enrolment record was written")
	}
}

// TestAdminEveEnrolmentOpen_RefusesWhenCoreNotWired is
// TestAppRouter_AdminOpRefusesWhenCoreNotWired's counterpart for
// requireEveEnrolmentOps.
func TestAdminEveEnrolmentOpen_RefusesWhenCoreNotWired(t *testing.T) {
	r := newTestRouter(t, makeSettings(nil, nil, nil), mcpbroker.NewManager(nil))

	_, err := r.AdminOp(context.Background(), "eve.enrolment.open", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("AdminOp succeeded with no EveEnrolmentOps wired")
	}
}

// TestAdminEvePasskeyRevoke_DispatchesIntoEvePasskeyOps proves
// "eve.passkey.revoke" reaches the SAME core `relay eve revoke` and the
// Passkeys tab's eve section call, exactly as adminLoginPasskeyRevoke's own
// dispatch does for "login.passkey.revoke".
func TestAdminEvePasskeyRevoke_DispatchesIntoEvePasskeyOps(t *testing.T) {
	store := newCLISandboxStore(t)
	assertNoErr(t, store.With(func(s *config.Settings) {
		s.EvePasskeys = []config.EvePasskey{{ID: "p1"}, {ID: "p2"}}
	}), "seed two eve passkeys")
	r := newBrokerRouter(t, store, nil)

	body, err := json.Marshal(evePasskeyRevokeRequest{ID: "p1"})
	assertNoErr(t, err, "marshal request")
	raw, err := r.AdminOp(context.Background(), "eve.passkey.revoke", body)
	assertNoErr(t, err, "AdminOp eve.passkey.revoke")

	var rec config.EvePasskeyRevocation
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if rec.ID != "p1" {
		t.Fatalf("result = %+v, want ID p1", rec)
	}
	if revs := store.Get().EvePasskeyRevocations; len(revs) != 1 || revs[0].ID != "p1" {
		t.Fatalf("no pending revocation was written: %+v", revs)
	}
}

// TestAdminEvePasskeyRevoke_RefusesWhenCoreNotWired is
// TestAppRouter_AdminOpRefusesWhenCoreNotWired's counterpart for
// requireEvePasskeyOps.
func TestAdminEvePasskeyRevoke_RefusesWhenCoreNotWired(t *testing.T) {
	r := newTestRouter(t, makeSettings(nil, nil, nil), mcpbroker.NewManager(nil))

	_, err := r.AdminOp(context.Background(), "eve.passkey.revoke", json.RawMessage(`{"id":"p1"}`))
	if err == nil {
		t.Fatal("AdminOp succeeded with no EvePasskeyOps wired")
	}
}
