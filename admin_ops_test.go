package main

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
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
		"login.bootstrap.mint",
		"login.passkey.revoke",
		"mcp.register",
		"mcp.unregister",
		"service.register",
		"service.unregister",
		"service.restart",
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
	r := newTestRouter(t, makeSettings(nil, nil, nil), NewExternalMcpManager(nil))

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
	r := newTestRouter(t, makeSettings(nil, nil, nil), NewExternalMcpManager(nil))

	_, err := r.AdminOp(context.Background(), "credential.mint", json.RawMessage(`{"name":"x","classes":["read"]}`))
	if err == nil {
		t.Fatal("AdminOp succeeded with no CredentialOps wired")
	}
}
