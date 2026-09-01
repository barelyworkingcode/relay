package main

// TestControlAudit_RefusalFloodCannotRotateRetentionWindowClean is the one
// audit_control_cap_test.go test that did not move to internal/audit with
// the rest of the file: it pins the interaction between RecordDecision's
// field caps (internal/audit) and real log rotation (log_rotate.go, which
// internal/audit deliberately does not own — see audit_start.go). The
// package-audit test suite's own newTestAudit uses a plain append-only file
// precisely because it has no rotation policy to test; this one needs the
// real rotatingWriter, which only exists here.

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
)

// acc2ReadGenerations reads the current audit file plus every rotated
// generation up to and including n, concatenating their bytes. A missing
// generation (rotation never reached it) is skipped rather than failing the
// read.
func acc2ReadGenerations(t *testing.T, rec *audit.AuditRecorder, n int) string {
	t.Helper()
	rec.Flush()
	var out strings.Builder
	paths := []string{rec.Path()}
	for i := 1; i <= n; i++ {
		paths = append(paths, rec.Path()+"."+strconv.Itoa(i))
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("read %s: %v", p, err)
		}
		out.Write(data)
	}
	return out.String()
}

// TestControlAudit_RefusalFloodCannotRotateRetentionWindowClean is the test
// that pins the fix end to end: it reproduces the reviewer's probe (a flood
// of refused requests with maximal paths, driven at a small retention
// window) and asserts an earlier record survives. Before the cap, a handful
// of maximal-path refusals were enough to rotate a window this size clean —
// each request wrote hundreds of KB. After the cap, the same flood writes
// only a few hundred bytes each, which this window comfortably outlasts.
//
// The window is small enough to run in milliseconds and the flood count is
// fixed, so this is deterministic rather than timing-dependent.
func TestControlAudit_RefusalFloodCannotRotateRetentionWindowClean(t *testing.T) {
	const (
		maxFileBytes = 16384
		generations  = 4
		floodSize    = 40
		attackerPath = 400_000 // matches the size demonstrated against relay
	)

	rec := newTestAudit(t, &config.AuditConfig{
		MaxFileBytes: maxFileBytes,
		Generations:  generations,
	})

	const canary = "acc2-incident-canary-c0ffee"
	rec.Record(audit.AuditEvent{
		ID:      audit.NewAuditID(),
		Event:   audit.AuditEventCallTool,
		Outcome: audit.AuditOutcomeOK,
		Tool:    canary,
		Actor:   audit.AuditActor{Kind: audit.AuditActorProject},
	})
	rec.Flush()

	if !strings.Contains(acc2ReadGenerations(t, rec, generations), canary) {
		t.Fatal("canary record was not written before the flood started")
	}

	hugePath := "/" + strings.Repeat("Q", attackerPath)
	for i := 0; i < floodSize; i++ {
		rec.RecordDecision(control.ControlDecision{
			Method:    "POST",
			Path:      hugePath,
			Class:     control.ClassExecute,
			Transport: control.TransportSocket,
			CredID:    "cred-attacker",
			Allowed:   false,
			Reason:    "class not granted",
		})
	}
	rec.Flush()

	// Confirm the flood actually exercised rotation, so a passing test means
	// the window survived pressure rather than never having been under any.
	if _, err := os.Stat(rec.Path() + ".1"); err != nil {
		t.Fatalf("expected the flood to rotate at least one generation, stat .1: %v", err)
	}

	if !strings.Contains(acc2ReadGenerations(t, rec, generations), canary) {
		t.Fatal("the pre-flood record was rotated out of every generation by refusals alone " +
			"(the amplification bug this test pins)")
	}
}
