package main

import (
	"context"
	"encoding/json"
	"testing"
)

// TestAdminOps_TableIsEmpty pins the S3 state: nothing has registered into
// adminOps yet, so admin_op resolves no operation at all. A later step that
// wires an op in is expected to make this fail — that is the point of
// pinning the count rather than just checking it is non-negative.
func TestAdminOps_TableIsEmpty(t *testing.T) {
	if len(adminOps) != 0 {
		t.Fatalf("adminOps has %d entries, want 0 at this build step: %v", len(adminOps), adminOps)
	}
}

// TestAppRouter_AdminOpRefusesUnknownOperation exercises the table through
// appRouter.AdminOp exactly as bridge/server.go's handleAdminOp calls it: an
// operation name the table doesn't hold must come back as an error, never a
// panic from an unchecked map lookup or a nil handler invocation.
func TestAppRouter_AdminOpRefusesUnknownOperation(t *testing.T) {
	r := newTestRouter(t, makeSettings(nil, nil, nil), NewExternalMcpManager(nil))

	result, err := r.AdminOp(context.Background(), "credential.mint", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("AdminOp accepted an operation not in adminOps")
	}
	if result != nil {
		t.Errorf("AdminOp returned a non-nil result alongside an error: %s", result)
	}
}
