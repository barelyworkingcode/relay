package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"testing"

	"relaygo/presence"
	"relaygo/presence/presencetest"
)

// AC-11: a malformed CSR is refused with ZERO presence-provider calls — the
// CSR is validated before the gate, so an operator is never made to type a
// password for an act that was going to refuse anyway.
func TestEnrolmentOpsSign_MalformedCSRRefusedWithZeroProviderCalls(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	profile := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")

	recording := presencetest.NewRecording(nil)
	gate, err := presence.NewGate(recording)
	assertNoErr(t, err, "NewGate")

	ops := &EnrolmentOps{Store: store, Gate: gate, Issuance: pgwWithIssuance(t)}
	_, err = ops.Sign(context.Background(), enrolmentSignFields{
		ClientID:   "hermes-mail",
		ProjectIDs: []string{profile.ID},
		CSRPEM:     "not a csr at all",
	}, auditViaCLI, "")
	if err == nil || !errors.Is(err, errEnrolmentInvalid) {
		t.Fatalf("a malformed CSR: err = %v, want errEnrolmentInvalid", err)
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
		Budget:     EnrolmentBudget{WindowSeconds: 3600, MaxCalls: 120, MaxResultBytes: 64 << 20},
	}
	baseDigest := base.presenceDigest(csrA)

	type variant struct {
		name string
		f    enrolmentSignFields
	}
	sameCSRVariants := []variant{
		{"client id", enrolmentSignFields{ClientID: "hermes-2", ProjectIDs: base.ProjectIDs, Budget: base.Budget}},
		{"grant set", enrolmentSignFields{ClientID: base.ClientID, ProjectIDs: []string{"proj-b"}, Budget: base.Budget}},
		{"window seconds", enrolmentSignFields{ClientID: base.ClientID, ProjectIDs: base.ProjectIDs, Budget: EnrolmentBudget{WindowSeconds: 1800, MaxCalls: base.Budget.MaxCalls, MaxResultBytes: base.Budget.MaxResultBytes}}},
		{"max calls", enrolmentSignFields{ClientID: base.ClientID, ProjectIDs: base.ProjectIDs, Budget: EnrolmentBudget{WindowSeconds: base.Budget.WindowSeconds, MaxCalls: 5, MaxResultBytes: base.Budget.MaxResultBytes}}},
		{"max result bytes", enrolmentSignFields{ClientID: base.ClientID, ProjectIDs: base.ProjectIDs, Budget: EnrolmentBudget{WindowSeconds: base.Budget.WindowSeconds, MaxCalls: base.Budget.MaxCalls, MaxResultBytes: 1 << 20}}},
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
	if base.presenceDigest(csrA) != base.presenceDigest(csrA) {
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

// requireGateOpLiteralIn parses file and returns the string literal passed
// as requireGate's op argument (its third parameter) inside the named
// method on recvType. This exists because that literal is invisible to
// every black-box test: Gate.Require uses the identical (possibly wrong)
// op string for both minting and redeeming its own nonce in the same call,
// so a grant requested and redeemed under a wrong-but-still-gated op name
// succeeds exactly as if it had been asked for correctly — nothing observed
// from outside Require distinguishes the two. Reading the literal back out
// of the source is what makes the op name provable rather than reviewed.
func requireGateOpLiteralIn(t *testing.T, file, recvType, funcName string) string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	assertNoErr(t, err, "parse %s", file)

	var target *ast.FuncDecl
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != funcName || fd.Recv == nil || len(fd.Recv.List) != 1 {
			continue
		}
		if funcRecvTypeName(fd.Recv.List[0].Type) == recvType {
			target = fd
			break
		}
	}
	if target == nil {
		t.Fatalf("%s: no method %s.%s found", file, recvType, funcName)
	}

	var op string
	var found bool
	ast.Inspect(target.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		ident, ok := call.Fun.(*ast.Ident)
		if !ok || ident.Name != "requireGate" {
			return true
		}
		if len(call.Args) < 3 {
			t.Fatalf("%s: requireGate call in %s.%s has %d args, want at least 3", file, recvType, funcName, len(call.Args))
		}
		lit, ok := call.Args[2].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			t.Fatalf("%s: requireGate's op argument in %s.%s is not a string literal (%T)", file, recvType, funcName, call.Args[2])
		}
		unquoted, uerr := strconv.Unquote(lit.Value)
		assertNoErr(t, uerr, "unquote op literal %s", lit.Value)
		op = unquoted
		found = true
		return false
	})
	if !found {
		t.Fatalf("%s: no requireGate call found in %s.%s", file, recvType, funcName)
	}
	return op
}

// funcRecvTypeName strips a leading pointer star, if any, so "*EnrolmentOps"
// and "EnrolmentOps" both report as "EnrolmentOps".
func funcRecvTypeName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// Regression: changing requireGate's op argument inside Sign from
// "enrolment.sign" to any other member of presence.GatedOps (e.g.
// "enrolment.create") leaves every behavioural test in this suite green —
// see requireGateOpLiteralIn's doc comment for why. This is the assertion
// that actually pins it.
func TestEnrolmentOpsSign_AsksTheGateUnderItsOwnOpName(t *testing.T) {
	got := requireGateOpLiteralIn(t, "enrolment_ops.go", "EnrolmentOps", "Sign")
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
	profileA := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
	profileB := mkStoreProject(t, store, ProjectKindRemote, "Calendar", "")

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
	profile := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
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
	issued := aiOnly(t, events, AuditEventCredentialIssued, "hermes-mail")
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
	profile := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
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
	if store.Get().FindEnrolment("hermes-mail") != nil {
		t.Fatal("the enrolment must be revoked when its issuance cannot be recorded")
	}
	if _, statErr := os.Stat(dir + "/enrolments/hermes-mail"); statErr == nil {
		t.Fatal("the emitted certificate directory was not removed")
	}
}
