package main

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/enrolment"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/presence/presencetest"
)

// AC-11: a malformed CSR is refused with ZERO presence-provider calls — the
// CSR is validated before the gate, so an operator is never made to type a
// password for an act that was going to refuse anyway.
func TestEnrolmentOpsSign_MalformedCSRRefusedWithZeroProviderCalls(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	profile := mkStoreProject(t, store, config.ProjectKindRemote, "Mail", "")

	recording := presencetest.NewRecording(nil)
	gate, err := presence.NewGate(recording)
	assertNoErr(t, err, "NewGate")

	ops := &EnrolmentOps{Store: store, Gate: gate, Issuance: pgwWithIssuance(t)}
	_, err = ops.Sign(context.Background(), enrolmentSignFields{
		ClientID:   "hermes-mail",
		ProjectIDs: []string{profile.ID},
		CSRPEM:     "not a csr at all",
	}, auditViaCLI, "")
	if err == nil || !errors.Is(err, enrolment.ErrInvalid) {
		t.Fatalf("a malformed CSR: err = %v, want enrolment.ErrInvalid", err)
	}
	if n := recording.Calls(); n != 0 {
		t.Errorf("presence provider called %d time(s) for a malformed CSR; must refuse before the gate", n)
	}
	if len(store.Get().Enrolments) != 0 {
		t.Fatal("a malformed CSR must not create an enrolment")
	}
}

// AC-12: changing client id, grant set, any budget field, or the CSR's
// public key all move the digest — a presence grant answered for one
// public key must not be redeemable for another.
func TestEnrolmentSignFields_DigestBindsEveryFieldIncludingTheCSRKey(t *testing.T) {
	csrA := parseCSRForTest(t, genClientCSRPEM(t, "hermes"))
	csrB := parseCSRForTest(t, genClientCSRPEM(t, "hermes")) // different key, same CN

	base := enrolmentSignFields{
		ClientID:   "hermes",
		ProjectIDs: []string{"proj-a"},
		Budget:     config.EnrolmentBudget{WindowSeconds: 3600, MaxCalls: 120, MaxResultBytes: 64 << 20},
	}
	baseDigest := base.presenceDigest(csrA)

	type variant struct {
		name string
		f    enrolmentSignFields
	}
	sameCSRVariants := []variant{
		{"client id", enrolmentSignFields{ClientID: "hermes-2", ProjectIDs: base.ProjectIDs, Budget: base.Budget}},
		{"grant set", enrolmentSignFields{ClientID: base.ClientID, ProjectIDs: []string{"proj-b"}, Budget: base.Budget}},
		{"window seconds", enrolmentSignFields{ClientID: base.ClientID, ProjectIDs: base.ProjectIDs, Budget: config.EnrolmentBudget{WindowSeconds: 1800, MaxCalls: base.Budget.MaxCalls, MaxResultBytes: base.Budget.MaxResultBytes}}},
		{"max calls", enrolmentSignFields{ClientID: base.ClientID, ProjectIDs: base.ProjectIDs, Budget: config.EnrolmentBudget{WindowSeconds: base.Budget.WindowSeconds, MaxCalls: 5, MaxResultBytes: base.Budget.MaxResultBytes}}},
		{"max result bytes", enrolmentSignFields{ClientID: base.ClientID, ProjectIDs: base.ProjectIDs, Budget: config.EnrolmentBudget{WindowSeconds: base.Budget.WindowSeconds, MaxCalls: base.Budget.MaxCalls, MaxResultBytes: 1 << 20}}},
	}
	for _, v := range sameCSRVariants {
		if v.f.presenceDigest(csrA) == baseDigest {
			t.Errorf("variant %q produced the same digest as the base request", v.name)
		}
	}
	// The CSR's own public key, holding every other field fixed.
	if base.presenceDigest(csrB) == baseDigest {
		t.Error("variant \"csr public key\" produced the same digest as the base request")
	}
	if base.presenceDigest(csrA) != base.presenceDigest(csrA) { //nolint:staticcheck // deliberate: same input twice checks the digest is deterministic, not a copy-paste
		t.Fatal("presenceDigest is not deterministic over the same request")
	}
}

// The concrete form of AC-12: a grant minted for one CSR's public key must
// not redeem against a different key's digest, even when every other field
// is identical — and the same digest redeemed against itself succeeds,
// proving the refusal above is the key binding and not a broken gate.
func TestEnrolmentSignFields_PresenceGrantBoundToCSRPublicKey(t *testing.T) {
	gate := allowGate(t)
	csrA := parseCSRForTest(t, genClientCSRPEM(t, "hermes"))
	csrB := parseCSRForTest(t, genClientCSRPEM(t, "hermes"))

	f := enrolmentSignFields{ClientID: "hermes", ProjectIDs: []string{"proj-a"}}
	digestA := f.presenceDigest(csrA)
	digestB := f.presenceDigest(csrB)

	grant, err := gate.Request(context.Background(), "enrolment.sign", digestA, "sign a certificate")
	assertNoErr(t, err, "Request")
	if err := gate.Redeem(grant, "enrolment.sign", digestB); !errors.Is(err, presence.ErrGrantInvalid) {
		t.Fatalf("a grant minted for one public key redeemed against a different one: err = %v, want ErrGrantInvalid", err)
	}

	grant, err = gate.Request(context.Background(), "enrolment.sign", digestA, "sign a certificate")
	assertNoErr(t, err, "Request")
	if err := gate.Redeem(grant, "enrolment.sign", digestA); err != nil {
		t.Fatalf("redeeming against the SAME digest failed: %v", err)
	}
}

// Regression: changing requireGate's op argument inside Sign from
// "enrolment.sign" to any other member of presence.GatedOps (e.g.
// "enrolment.create") leaves every behavioural test in this suite green —
// Gate.Require uses the identical (possibly wrong) op string for both
// minting and redeeming its own nonce in the same call, so nothing observed
// from outside Require distinguishes a wrong-but-still-gated op from a
// correct one. Reading the literal back out of the source, via the shared
// scan in gate_ast_scan_test.go, is what makes the op name provable rather
// than reviewed.
func TestEnrolmentOpsSign_AsksTheGateUnderItsOwnOpName(t *testing.T) {
	sites := scanRequireGateCallSites(t, gateASTModuleRoot(t))
	var got string
	var found bool
	for _, s := range sites {
		if s.method == "EnrolmentOps.Sign" {
			got, found = s.op, true
			break
		}
	}
	if !found {
		t.Fatal("no requireGate call site found for EnrolmentOps.Sign")
	}
	if got != "enrolment.sign" {
		t.Fatalf("EnrolmentOps.Sign asks the gate under op %q, want %q", got, "enrolment.sign")
	}
}

// Regression: enrolmentSignReason is the sentence shown on the macOS
// presence prompt before an operator authorises a sign. Swapping it for
// enrolmentCreateReason at the call site must not leave the suite green —
// this pins both branches via presencetest.Recording.Reasons().
func TestEnrolmentOpsSign_PresenceReasonNamesTheGrantsOrTheirAbsence(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	profileA := mkStoreProject(t, store, config.ProjectKindRemote, "Mail", "")
	profileB := mkStoreProject(t, store, config.ProjectKindRemote, "Calendar", "")

	recording := presencetest.NewRecording(nil)
	gate, err := presence.NewGate(recording)
	assertNoErr(t, err, "NewGate")

	ops := &EnrolmentOps{Store: store, Gate: gate, Issuance: pgwWithIssuance(t)}

	_, err = ops.Sign(context.Background(), enrolmentSignFields{
		ClientID:   "hermes-mail",
		ProjectIDs: []string{profileA.ID, profileB.ID},
		CSRPEM:     string(genClientCSRPEM(t, "hermes-mail")),
	}, auditViaCLI, "")
	assertNoErr(t, err, "Sign with grants")

	_, err = ops.Sign(context.Background(), enrolmentSignFields{
		ClientID: "hermes-nogrant",
		CSRPEM:   string(genClientCSRPEM(t, "hermes-nogrant")),
	}, auditViaCLI, "")
	assertNoErr(t, err, "Sign with no grants")

	reasons := recording.Reasons()
	if len(reasons) != 2 {
		t.Fatalf("Reasons() = %v, want 2 entries", reasons)
	}
	wantWithGrants := `sign a certificate for client "hermes-mail" with access to ` + joinWithAnd([]string{profileA.ID, profileB.ID})
	if reasons[0] != wantWithGrants {
		t.Fatalf("reason[0] = %q, want %q", reasons[0], wantWithGrants)
	}
	wantNoGrants := `sign a certificate for client "hermes-nogrant" with no project access`
	if reasons[1] != wantNoGrants {
		t.Fatalf("reason[1] = %q, want %q", reasons[1], wantNoGrants)
	}
}

// AC-25: a successful Sign writes exactly one credential_issued record
// naming the client id, the grant ids, via "cli", and a non-empty
// presence_id.
func TestEnrolmentOpsSign_WritesExactlyOneCredentialIssuedRecord(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	profile := mkStoreProject(t, store, config.ProjectKindRemote, "Mail", "")
	rec := startAuditRecorder(store.Get())
	if rec == nil {
		t.Fatal("startAuditRecorder returned nil — auditing is off in this store's settings")
	}
	t.Cleanup(rec.Close)

	ops := &EnrolmentOps{Store: store, Gate: allowGate(t), Issuance: issuanceAuditorOrNil(rec)}
	csrPEM := genClientCSRPEM(t, "hermes-mail")
	_, err := ops.Sign(context.Background(), enrolmentSignFields{
		ClientID:   "hermes-mail",
		ProjectIDs: []string{profile.ID},
		CSRPEM:     string(csrPEM),
	}, auditViaCLI, "")
	assertNoErr(t, err, "Sign")

	events := aiParse(t, aiLogText(t))
	issued := aiOnly(t, events, audit.AuditEventCredentialIssued, "hermes-mail")
	if issued.Credential != auditCredentialEnrolment {
		t.Errorf("credential = %q, want %q", issued.Credential, auditCredentialEnrolment)
	}
	if got := aiGrants(issued); got != profile.ID {
		t.Errorf("grants = %q, want %q", got, profile.ID)
	}
	if issued.Via != auditViaCLI {
		t.Errorf("via = %q, want %q", issued.Via, auditViaCLI)
	}
	if issued.PresenceID == "" {
		t.Error("presence_id is empty")
	}
}

// AC-26: when the issuance record cannot be written, Sign returns
// errEnrolmentUnrecorded, the enrolment is gone from settings, and the
// emitted certificate directory has been removed — the same fail-closed
// undo Create already has.
func TestEnrolmentOpsSign_UnrecordedIssuanceRevokesAndRemovesTheBundle(t *testing.T) {
	dir, store := newEnrolmentSandbox(t)
	profile := mkStoreProject(t, store, config.ProjectKindRemote, "Mail", "")
	broken := &aiBrokenAuditor{}

	ops := &EnrolmentOps{Store: store, Gate: allowGate(t), Issuance: broken}
	csrPEM := genClientCSRPEM(t, "hermes-mail")
	_, err := ops.Sign(context.Background(), enrolmentSignFields{
		ClientID:   "hermes-mail",
		ProjectIDs: []string{profile.ID},
		CSRPEM:     string(csrPEM),
	}, auditViaCLI, "")
	if !errors.Is(err, errEnrolmentUnrecorded) {
		t.Fatalf("err = %v, want errEnrolmentUnrecorded", err)
	}
	if broken.calls != 1 {
		t.Errorf("issuance auditor called %d times, want 1", broken.calls)
	}
	if enrolment.Find(store.Get(), "hermes-mail") != nil {
		t.Fatal("the enrolment must be revoked when its issuance cannot be recorded")
	}
	if _, statErr := os.Stat(dir + "/enrolments/hermes-mail"); statErr == nil {
		t.Fatal("the emitted certificate directory was not removed")
	}
}
