package main

// EnrolmentOps.Approve: the approval act (spec §3). Sign and Approve share
// one private body (completeSigning in enrolment_ops.go) over the SAME
// "enrolment.sign" gate and digest -- these tests are the concrete form of
// that constraint, not a parallel review of it.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"relaygo/presence"
	"relaygo/presence/presencetest"
)

// aoLodge lodges a fresh CSR into a fresh table and returns the request id
// alongside the table, for a test that needs nothing else set up.
func aoLodge(t *testing.T, cn, remoteAddr string) (*enrolmentRequestTable, lodged) {
	t.Helper()
	table := newEnrolmentRequestTable()
	l, err := table.Lodge(genClientCSRPEM(t, cn), "", remoteAddr)
	assertNoErr(t, err, "Lodge")
	return table, l
}

// ---------------------------------------------------------------------------
// AC-17: no second door into issuance
// ---------------------------------------------------------------------------

func TestPresenceGatedOps_NoEnrolmentApproveEntry(t *testing.T) {
	for _, op := range presence.GatedOps {
		if op == "enrolment.approve" {
			t.Fatal(`presence.GatedOps has an "enrolment.approve" entry -- exactly the second door into issuance ADR-018 forbids`)
		}
	}
}

// ---------------------------------------------------------------------------
// AC-18: Approve signs over the STORED CSR, and approveFields cannot carry one
// ---------------------------------------------------------------------------

func TestEnrolmentOpsApprove_SignsOverTheStoredCSRsPublicKey(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	profile := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
	table, l := aoLodge(t, "hermes-mail", "10.0.0.5:41233")

	rec, ok := table.Get(l.RequestID)
	if !ok {
		t.Fatal("lodged record vanished before Approve ran")
	}
	csr := parseCSRForTest(t, rec.CSRPEM)
	wantSPKI := SPKISHA256Hex(csr.RawSubjectPublicKeyInfo)

	ops := &EnrolmentOps{Store: store, Gate: allowGate(t), Issuance: pgwWithIssuance(t), Requests: table}
	created, err := ops.Approve(context.Background(), approveFields{
		RequestID:  l.RequestID,
		ClientID:   "hermes-mail",
		ProjectIDs: []string{profile.ID},
	}, auditViaCLI, "")
	assertNoErr(t, err, "Approve")

	if created.Enrolment.SPKISHA256 != wantSPKI {
		t.Fatalf("SPKISHA256 = %q, want %q (the lodged CSR's own key)", created.Enrolment.SPKISHA256, wantSPKI)
	}
	if created.Enrolment.ClientID != "hermes-mail" {
		t.Fatalf("ClientID = %q, want hermes-mail", created.Enrolment.ClientID)
	}
	if !created.Enrolment.GrantsProject(profile.ID) {
		t.Fatalf("approved enrolment does not grant %s: %+v", profile.ID, created.Enrolment)
	}

	// The poll response must reflect the approval (spec §5).
	poll, perr := table.Poll(l.RequestID)
	assertNoErr(t, perr, "Poll")
	if poll.Status != "approved" {
		t.Fatalf("poll status = %q, want approved", poll.Status)
	}
	if poll.ClientID != "hermes-mail" || poll.CertPEM == "" || poll.CAPEM == "" {
		t.Fatalf("poll result incomplete: %+v", poll)
	}
	if len(poll.ProjectIDs) != 1 || poll.ProjectIDs[0] != profile.ID {
		t.Fatalf("poll ProjectIDs = %v, want [%s]", poll.ProjectIDs, profile.ID)
	}
}

// approveFields must have no field that could carry a CSR: the digest's
// SPKI binding is decorative unless the signed bytes are provably the
// STORED ones (§11.3).
func TestApproveFields_HasNoFieldThatCouldCarryACSR(t *testing.T) {
	typ := reflect.TypeOf(approveFields{})
	for i := 0; i < typ.NumField(); i++ {
		name := strings.ToLower(typ.Field(i).Name)
		if strings.Contains(name, "csr") {
			t.Fatalf("approveFields has field %q, which could carry a CSR and make the digest's SPKI binding decorative (spec §11.3)", typ.Field(i).Name)
		}
	}
}

// ---------------------------------------------------------------------------
// AC-19: a grant cannot be redeemed for a different key; no exported mutator
// ---------------------------------------------------------------------------

// The pending record is immutable after lodging: no exported method on the
// table accepts a []byte that could replace a record's stored CSR. Lodge is
// the one write path CSR bytes are ever accepted on, by design.
func TestEnrolmentRequestTable_NoExportedMethodCanReplaceStoredCSRBytes(t *testing.T) {
	typ := reflect.TypeOf(&enrolmentRequestTable{})
	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i)
		if m.Name == "Lodge" {
			continue
		}
		for p := 1; p < m.Type.NumIn(); p++ { // p=0 is the receiver
			pt := m.Type.In(p)
			if pt.Kind() == reflect.Slice && pt.Elem().Kind() == reflect.Uint8 {
				t.Fatalf("%s accepts a []byte parameter -- a second path a pending record's CSR bytes could be replaced through, which would make Approve's presence digest binding decorative (spec §11.3)", m.Name)
			}
		}
	}
}

// The concrete form of the binding: a grant minted over one lodged record's
// key fails Redeem against a digest built from a DIFFERENT lodged record's
// key, using the exact bytes Approve itself would read via Get.
func TestEnrolmentRequestApproval_GrantCannotBeRedeemedForADifferentKey(t *testing.T) {
	table := newEnrolmentRequestTable()
	lodgedA, err := table.Lodge(genClientCSRPEM(t, "hermes-a"), "", "10.0.0.5:1")
	assertNoErr(t, err, "lodge A")
	lodgedB, err := table.Lodge(genClientCSRPEM(t, "hermes-b"), "", "10.0.0.6:1")
	assertNoErr(t, err, "lodge B")

	recA, ok := table.Get(lodgedA.RequestID)
	if !ok {
		t.Fatal("record A missing")
	}
	recB, ok := table.Get(lodgedB.RequestID)
	if !ok {
		t.Fatal("record B missing")
	}
	csrA := parseCSRForTest(t, recA.CSRPEM)
	csrB := parseCSRForTest(t, recB.CSRPEM)

	f := enrolmentSignFields{ClientID: "hermes", ProjectIDs: []string{"proj-a"}}
	digestA := f.presenceDigest(csrA)
	digestB := f.presenceDigest(csrB)
	if digestA == digestB {
		t.Fatal("two distinct lodged CSRs produced the same digest")
	}

	g, err := presence.NewGate(presencetest.Allow())
	assertNoErr(t, err, "NewGate")
	grant, err := g.Request(context.Background(), "enrolment.sign", digestA, "approve request A")
	assertNoErr(t, err, "Request")
	if err := g.Redeem(grant, "enrolment.sign", digestB); !errors.Is(err, presence.ErrGrantInvalid) {
		t.Fatalf("a grant minted for record A's key redeemed against record B's digest: err = %v, want ErrGrantInvalid", err)
	}

	grant, err = g.Request(context.Background(), "enrolment.sign", digestA, "approve request A")
	assertNoErr(t, err, "Request")
	if err := g.Redeem(grant, "enrolment.sign", digestA); err != nil {
		t.Fatalf("redeeming against the SAME digest failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// AC-20, AC-21: refusal and the SSH path
// ---------------------------------------------------------------------------

func TestEnrolmentOpsApprove_DeniedLeavesPendingRecordAndWritesNoEnrolment(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	profile := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
	table, l := aoLodge(t, "hermes-mail", "10.0.0.5:1")

	g, err := presence.NewGate(presencetest.Deny())
	assertNoErr(t, err, "NewGate")
	ops := &EnrolmentOps{Store: store, Gate: g, Issuance: pgwWithIssuance(t), Requests: table}
	_, err = ops.Approve(context.Background(), approveFields{
		RequestID: l.RequestID, ClientID: "hermes-mail", ProjectIDs: []string{profile.ID},
	}, auditViaCLI, "")
	if !errors.Is(err, presence.ErrRefused) {
		t.Fatalf("err = %v, want presence.ErrRefused", err)
	}
	if len(store.Get().Enrolments) != 0 {
		t.Fatal("a denied approval must not create an enrolment")
	}
	if _, ok := table.Get(l.RequestID); !ok {
		t.Fatal("a denied approval must leave the pending record present")
	}
}

// AC-21: over a session that cannot show a prompt (the SSH shape), Approve
// refuses without ever reaching signEnrolment.
func TestEnrolmentOpsApprove_NoSessionRefusesWithoutSigning(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	profile := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
	table, l := aoLodge(t, "hermes-mail", "10.0.0.5:1")

	g, err := presence.NewGate(presencetest.NoSession())
	assertNoErr(t, err, "NewGate")
	ops := &EnrolmentOps{Store: store, Gate: g, Issuance: pgwWithIssuance(t), Requests: table}
	_, err = ops.Approve(context.Background(), approveFields{
		RequestID: l.RequestID, ClientID: "hermes-mail", ProjectIDs: []string{profile.ID},
	}, auditViaCLI, "")
	if !errors.Is(err, presence.ErrUnavailable) {
		t.Fatalf("err = %v, want presence.ErrUnavailable -- the SSH path", err)
	}
	if len(store.Get().Enrolments) != 0 {
		t.Fatal("a no-session refusal must not create an enrolment")
	}
	if _, ok := table.Get(l.RequestID); !ok {
		t.Fatal("a no-session refusal must leave the pending record present")
	}
}

// ---------------------------------------------------------------------------
// AC-22: ParseClientCSR runs before the gate
// ---------------------------------------------------------------------------

func TestEnrolmentOpsApprove_MalformedStoredCSRRefusesWithZeroProviderCalls(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	profile := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
	table := newEnrolmentRequestTable()
	// Lodge itself would never store this (it runs ParseClientCSR before
	// admitting a row) -- direct map access, legal white-box test code in
	// this same package, is the only way to construct the case §11.14 asks
	// for: a record whose CSR ParseClientCSR refuses.
	table.pending["req_bad"] = &enrolmentRequestRecord{
		id: "req_bad", csrPEM: []byte("not a csr at all"), spkiSHA256: "x",
		remoteAddr: "10.0.0.5:1", arrivedAt: time.Now(), expiresAt: time.Now().Add(time.Hour),
	}

	recording := presencetest.NewRecording(nil)
	g, err := presence.NewGate(recording)
	assertNoErr(t, err, "NewGate")
	ops := &EnrolmentOps{Store: store, Gate: g, Issuance: pgwWithIssuance(t), Requests: table}
	_, err = ops.Approve(context.Background(), approveFields{
		RequestID: "req_bad", ClientID: "hermes-mail", ProjectIDs: []string{profile.ID},
	}, auditViaCLI, "")
	if err == nil || !errors.Is(err, errEnrolmentInvalid) {
		t.Fatalf("err = %v, want errEnrolmentInvalid", err)
	}
	if n := recording.Calls(); n != 0 {
		t.Errorf("presence provider called %d time(s) for a malformed stored CSR; must refuse before the gate", n)
	}
	if len(store.Get().Enrolments) != 0 {
		t.Fatal("a malformed stored CSR must not create an enrolment")
	}
}

// ---------------------------------------------------------------------------
// AC-23: the duplicate-SPKI check is authoritative from inside store.With
// ---------------------------------------------------------------------------

func TestEnrolmentOpsApprove_DuplicateSPKIRefusedNamingTheExistingClient(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	profile := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	assertNoErr(t, err, "generate shared key")
	csrFirst := genCSRPEMFromKey(t, key, 0, "hermes-first")
	_, err = signEnrolment(store, enrolmentRequest{ClientID: "hermes-first", ProjectIDs: []string{profile.ID}}, parseCSRForTest(t, csrFirst))
	assertNoErr(t, err, "seed an existing enrolment over the shared key")

	// A second CSR over the SAME key, lodged and approved under a different
	// client id: Lodge itself has no visibility into settings.json enrolments
	// (only into other PENDING rows), so it succeeds -- the refusal must come
	// from ValidateEnrolment inside Approve's own commitEnrolment.
	table := newEnrolmentRequestTable()
	l, err := table.Lodge(genCSRPEMFromKey(t, key, 0, "hermes-second"), "", "10.0.0.5:1")
	assertNoErr(t, err, "lodge a second CSR over the same key")

	ops := &EnrolmentOps{Store: store, Gate: allowGate(t), Issuance: pgwWithIssuance(t), Requests: table}
	_, err = ops.Approve(context.Background(), approveFields{
		RequestID: l.RequestID, ClientID: "hermes-second", ProjectIDs: []string{profile.ID},
	}, auditViaCLI, "")
	if err == nil || !strings.Contains(err.Error(), "hermes-first") {
		t.Fatalf("err = %v, want a refusal naming the existing client hermes-first", err)
	}
	if len(store.Get().Enrolments) != 1 {
		t.Fatalf("a refused approval must not add a second enrolment: %+v", store.Get().Enrolments)
	}
	if _, ok := table.Get(l.RequestID); !ok {
		t.Fatal("the pending record must survive a duplicate-SPKI refusal, for the operator to refuse it deliberately")
	}
}

// ---------------------------------------------------------------------------
// AC-24: an unrecorded issuance revokes, and the poll never says "approved"
// ---------------------------------------------------------------------------

func TestEnrolmentOpsApprove_UnrecordedIssuanceRevokesAndPollNeverApproves(t *testing.T) {
	dir, store := newEnrolmentSandbox(t)
	profile := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
	table, l := aoLodge(t, "hermes-mail", "10.0.0.5:1")

	broken := &aiBrokenAuditor{}
	ops := &EnrolmentOps{Store: store, Gate: allowGate(t), Issuance: broken, Requests: table}
	_, err := ops.Approve(context.Background(), approveFields{
		RequestID: l.RequestID, ClientID: "hermes-mail", ProjectIDs: []string{profile.ID},
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
	if _, statErr := os.Stat(filepath.Join(dir, "enrolments", "hermes-mail")); statErr == nil {
		t.Fatal("the emitted certificate directory was not removed")
	}

	poll, perr := table.Poll(l.RequestID)
	assertNoErr(t, perr, "Poll")
	if poll.Status != "pending" {
		t.Fatalf("poll status = %q, want pending -- MarkApproved must never run when completeSigning's own undo fired", poll.Status)
	}
	if _, ok := table.Get(l.RequestID); !ok {
		t.Fatal("the pending record must survive an unrecorded-issuance refusal")
	}
}

// ---------------------------------------------------------------------------
// AC-34 / §11.7: a stale client.key still refuses the bundle write, but the
// approval still delivers the certificate and the record still lands.
// ---------------------------------------------------------------------------

func TestEnrolmentOpsApprove_BundleWriteFailureStillDeliversTheCertificate(t *testing.T) {
	dir, store := newEnrolmentSandbox(t)
	profile := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
	table, l := aoLodge(t, "hermes-mail", "10.0.0.5:1")

	// A legacy `enrol create` bundle already left a client.key at the
	// target directory this approval will write to.
	bundleDir := filepath.Join(dir, enrolmentBundleDir, "hermes-mail")
	assertNoErr(t, os.MkdirAll(bundleDir, 0700), "mkdir bundle dir")
	assertNoErr(t, os.WriteFile(filepath.Join(bundleDir, "client.key"), []byte("stale key"), 0600), "seed stale client.key")

	ops := &EnrolmentOps{Store: store, Gate: allowGate(t), Issuance: pgwWithIssuance(t), Requests: table}
	created, err := ops.Approve(context.Background(), approveFields{
		RequestID: l.RequestID, ClientID: "hermes-mail", ProjectIDs: []string{profile.ID},
	}, auditViaCLI, "")
	if !errors.Is(err, errEnrolmentBundle) {
		t.Fatalf("err = %v, want errEnrolmentBundle", err)
	}
	if created.CertPEM == "" || created.CAPEM == "" {
		t.Fatal("the certificate must still be delivered when only the on-host bundle write failed (spec §11.7)")
	}
	if store.Get().FindEnrolment("hermes-mail") == nil {
		t.Fatal("the enrolment record must have landed even though the bundle write failed")
	}

	poll, perr := table.Poll(l.RequestID)
	assertNoErr(t, perr, "Poll")
	if poll.Status != "approved" {
		t.Fatalf("poll status = %q, want approved -- the poll response must still deliver the certificate (spec §11.7)", poll.Status)
	}
	if poll.CertPEM == "" || poll.CAPEM == "" {
		t.Fatal("poll must carry cert_pem/ca_pem even though the on-host bundle write failed")
	}
}

// ---------------------------------------------------------------------------
// Refuse and PendingRequests: lighter coverage for the two siblings
// ---------------------------------------------------------------------------

func TestEnrolmentOpsRefuse_RemovesTheRecordAndAuditsOneControlDecision(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	table, l := aoLodge(t, "hermes-mail", "10.0.0.5:1")
	rec := startAuditRecorder(store.Get())
	if rec == nil {
		t.Fatal("startAuditRecorder returned nil -- auditing is off in this store's settings")
	}
	t.Cleanup(rec.Close)

	ops := &EnrolmentOps{Store: store, Audit: rec, Requests: table}
	assertNoErr(t, ops.Refuse(l.RequestID), "Refuse")

	if _, ok := table.Get(l.RequestID); ok {
		t.Fatal("Refuse must remove the pending record")
	}
	// RecordDecision goes through the async Record path (audit.go), unlike
	// recordEnrolmentIssued's durable write, so a flush is needed before the
	// file is read back.
	rec.Flush()
	events := aiParse(t, aiLogText(t))
	found := false
	for _, e := range events {
		if e.Event == AuditEventControlDecision && e.Method == "enrolment.request.refuse" {
			found = true
			if e.Outcome != AuditOutcomeDenied {
				t.Errorf("a refusal's outcome = %q, want %q", e.Outcome, AuditOutcomeDenied)
			}
		}
	}
	if !found {
		t.Fatal("no control_decision recorded for enrolment.request.refuse")
	}

	if err := ops.Refuse(l.RequestID); !errors.Is(err, errEnrolmentRequestNotFound) {
		t.Fatalf("refusing an already-refused id: err = %v, want errEnrolmentRequestNotFound", err)
	}
}

func TestEnrolmentOpsPendingRequests_ListsLiveRowsAndReadsSafelyWhenUnwired(t *testing.T) {
	ops := &EnrolmentOps{}
	if got := ops.PendingRequests(); len(got) != 0 {
		t.Fatalf("PendingRequests() with no table wired = %v, want empty", got)
	}

	table, l := aoLodge(t, "hermes-mail", "10.0.0.5:1")
	ops.Requests = table
	got := ops.PendingRequests()
	if len(got) != 1 || got[0].RequestID != l.RequestID {
		t.Fatalf("PendingRequests() = %+v, want exactly the one lodged request", got)
	}
}

// Approve and Refuse must refuse, not panic, when no table is wired -- the
// state every door that predates this slice leaves EnrolmentOps in.
func TestEnrolmentOpsApproveAndRefuse_RefuseWhenNoTableIsWired(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	ops := &EnrolmentOps{Store: store, Gate: allowGate(t), Issuance: pgwWithIssuance(t)}

	_, err := ops.Approve(context.Background(), approveFields{RequestID: "req_x", ClientID: "hermes"}, auditViaCLI, "")
	if !errors.Is(err, errEnrolmentRequestsNotWired) {
		t.Fatalf("Approve with no table wired: err = %v, want errEnrolmentRequestsNotWired", err)
	}
	if err := ops.Refuse("req_x"); !errors.Is(err, errEnrolmentRequestsNotWired) {
		t.Fatalf("Refuse with no table wired: err = %v, want errEnrolmentRequestsNotWired", err)
	}
}
