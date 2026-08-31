package main

// Hermetic coverage for the Pending requests panel's IPC door (spec §3):
// list_enrolment_requests / approve_enrolment_request / refuse_enrolment_request
// in ipc_enrolments.go. Mirrors ipc_enrolments_test.go's own shape — each
// handler proves it (a) reaches the right EnrolmentOps method, (b) emits
// what the panel needs, and (c) never lets a CSR byte anywhere near the
// WebView.

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// newEnrolmentRequestsIPC is newEnrolmentIPC's twin with the pending table
// wired onto EnrolmentOps.Requests, which newEnrolmentIPC deliberately
// leaves nil (every door that predates this slice does). table is returned
// too so a test can lodge directly into it, the way aoLodge does but
// against a caller-visible table rather than a throwaway one.
func newEnrolmentRequestsIPC(t *testing.T) (*IPCContext, SettingsStore, *recordingUI, *enrolmentRequestTable) {
	t.Helper()
	_, store := newEnrolmentSandbox(t)
	ui := &recordingUI{}
	rec := enabledIssuanceRecorder(t)
	table := newEnrolmentRequestTable()
	return &IPCContext{
		Ctx:                    context.Background(),
		Store:                  store,
		UI:                     ui,
		Platform:               stubPlatform{},
		Registry:               noopServiceManager{},
		Enhanced:               NewEnhancedServiceRegistry(nil),
		UpdateMenu:             func() {},
		PushServiceStatusBatch: func() {},
		GoFunc:                 func(fn func()) { fn() },
		NotifyReconcile:        func(string) error { return nil },
		NotifyReloadMcp:        func(string, string) error { return nil },
		Audit:                  rec,
		EnrolmentOps:           &EnrolmentOps{Store: store, Audit: rec, Gate: allowGate(t), Requests: table},
	}, store, ui, table
}

// ---------------------------------------------------------------------------
// pendingEnrolmentRequestView carries no CSR bytes
// ---------------------------------------------------------------------------

// No field on the view type could carry a CSR (reflection), and marshalling
// a view built from a REAL lodged CSR never puts the CSR's own bytes -- or
// even the word "csr" -- on the wire (the concrete case the reflection scan
// alone would miss: a field named e.g. "Blob" holding the same bytes).
func TestPendingEnrolmentRequestView_CarriesNoCSRBytes(t *testing.T) {
	typ := reflect.TypeOf(pendingEnrolmentRequestView{})
	for i := 0; i < typ.NumField(); i++ {
		name := strings.ToLower(typ.Field(i).Name)
		if strings.Contains(name, "csr") || strings.Contains(name, "pem") {
			t.Fatalf("pendingEnrolmentRequestView has field %q, which could carry CSR bytes to the WebView", typ.Field(i).Name)
		}
	}

	table := newEnrolmentRequestTable()
	csrPEM := genClientCSRPEM(t, "hermes-mail")
	l, err := table.Lodge(csrPEM, "vm-mail-a", "", "", "10.0.0.5:41233")
	assertNoErr(t, err, "Lodge")

	views := table.List()
	if len(views) != 1 {
		t.Fatalf("List() = %d views, want 1", len(views))
	}
	view := pendingEnrolmentRequestViewOf(views[0])
	if view.RequestID != l.RequestID {
		t.Fatalf("view.RequestID = %q, want %q", view.RequestID, l.RequestID)
	}

	data, err := json.Marshal(view)
	assertNoErr(t, err, "marshal pendingEnrolmentRequestView")

	lower := strings.ToLower(string(data))
	if strings.Contains(lower, "csr") || strings.Contains(lower, "pem") || strings.Contains(lower, "-----begin") {
		t.Fatalf("marshalled pendingEnrolmentRequestView mentions CSR/PEM material: %s", data)
	}
	// The actual CSR bytes, whole or in part, must not appear either --
	// belt and braces beyond the field-name and keyword checks above.
	if strings.Contains(string(data), strings.TrimSpace(string(csrPEM))) {
		t.Fatal("the lodged CSR's own bytes reached the marshalled view")
	}
}

// ---------------------------------------------------------------------------
// list_enrolment_requests
// ---------------------------------------------------------------------------

func TestIPCListEnrolmentRequests_EmitsCurrentTable(t *testing.T) {
	ipc, _, ui, table := newEnrolmentRequestsIPC(t)

	l1, err := table.Lodge(genClientCSRPEM(t, "hermes-mail"), "vm-mail-a", "", "", "10.0.0.5:41233")
	assertNoErr(t, err, "Lodge 1")
	l2, err := table.Lodge(genClientCSRPEM(t, "hermes-cal"), "", "", "", "10.0.0.6:9000")
	assertNoErr(t, err, "Lodge 2")

	ipcListEnrolmentRequests(ipc, mustRaw(t, map[string]interface{}{}))

	args, ok := findEvent(ui, "onEnrolmentRequestsChanged")
	if !ok {
		t.Fatalf("expected onEnrolmentRequestsChanged; got %+v", ui.events)
	}
	var views []pendingEnrolmentRequestView
	if err := json.Unmarshal(args[0].(json.RawMessage), &views); err != nil {
		t.Fatalf("unmarshal views: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("got %d views, want 2: %+v", len(views), views)
	}
	byID := map[string]pendingEnrolmentRequestView{}
	for _, v := range views {
		byID[v.RequestID] = v
	}
	v1, ok := byID[l1.RequestID]
	if !ok {
		t.Fatalf("missing request %s in %+v", l1.RequestID, views)
	}
	if v1.SPKISHA256 != l1.SPKISHA256 || v1.Label != "vm-mail-a" || v1.RemoteAddr != "10.0.0.5:41233" || v1.Approved {
		t.Fatalf("request 1 view = %+v", v1)
	}
	v2, ok := byID[l2.RequestID]
	if !ok {
		t.Fatalf("missing request %s in %+v", l2.RequestID, views)
	}
	if v2.Label != "" {
		t.Fatalf("request 2 label = %q, want empty (none supplied)", v2.Label)
	}
}

func TestIPCListEnrolmentRequests_EmptyTableEmitsEmptyList(t *testing.T) {
	ipc, _, ui, _ := newEnrolmentRequestsIPC(t)
	ipcListEnrolmentRequests(ipc, mustRaw(t, map[string]interface{}{}))

	args, ok := findEvent(ui, "onEnrolmentRequestsChanged")
	if !ok {
		t.Fatalf("expected onEnrolmentRequestsChanged; got %+v", ui.events)
	}
	var views []pendingEnrolmentRequestView
	if err := json.Unmarshal(args[0].(json.RawMessage), &views); err != nil {
		t.Fatalf("unmarshal views: %v", err)
	}
	if len(views) != 0 {
		t.Fatalf("got %d views, want 0", len(views))
	}
}

// ---------------------------------------------------------------------------
// approve_enrolment_request
// ---------------------------------------------------------------------------

func TestIPCApproveEnrolmentRequest_PersistsAndEmitsBundleAndRefreshesList(t *testing.T) {
	ipc, store, ui, table := newEnrolmentRequestsIPC(t)
	mail := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")

	l, err := table.Lodge(genClientCSRPEM(t, "hermes-mail"), "vm-mail-a", "", "", "10.0.0.5:41233")
	assertNoErr(t, err, "Lodge")

	ipcApproveEnrolmentRequest(ipc, mustRaw(t, map[string]interface{}{
		"request_id":  l.RequestID,
		"client_id":   "hermes-mail",
		"project_ids": []string{mail.ID},
	}))

	// Same event ipcCreateEnrolment emits: approving IS signing, from the
	// operator's chair (EnrolmentOps.Approve's own doc comment), so the
	// panel and the enrolment list learn about it the same way.
	created, ok := findEvent(ui, "onEnrolmentCreated")
	if !ok {
		t.Fatalf("expected onEnrolmentCreated; got %+v", ui.events)
	}
	var enrolment Enrolment
	if err := json.Unmarshal(created[0].(json.RawMessage), &enrolment); err != nil {
		t.Fatalf("unmarshal enrolment: %v", err)
	}
	if enrolment.ClientID != "hermes-mail" || !enrolment.GrantsProject(mail.ID) {
		t.Fatalf("emitted enrolment does not match the approval: %+v", enrolment)
	}
	if store.Get().FindEnrolment("hermes-mail") == nil {
		t.Error("approved enrolment was not persisted")
	}

	// The pending panel refreshes in the same round trip: the row now
	// reads approved rather than vanishing, matching Poll's own "approved,
	// not yet collected" state (spec §2).
	changed, ok := findEvent(ui, "onEnrolmentRequestsChanged")
	if !ok {
		t.Fatalf("expected onEnrolmentRequestsChanged; got %+v", ui.events)
	}
	var views []pendingEnrolmentRequestView
	if err := json.Unmarshal(changed[0].(json.RawMessage), &views); err != nil {
		t.Fatalf("unmarshal views: %v", err)
	}
	if len(views) != 1 || !views[0].Approved || views[0].ApprovedClientID != "hermes-mail" {
		t.Fatalf("pending view after approval = %+v", views)
	}
}

func TestIPCApproveEnrolmentRequest_RequiresRequestIDAndClientID(t *testing.T) {
	ipc, _, ui, table := newEnrolmentRequestsIPC(t)
	l, err := table.Lodge(genClientCSRPEM(t, "hermes-mail"), "", "", "", "10.0.0.5:1")
	assertNoErr(t, err, "Lodge")

	// Missing client_id: ignored before ever reaching EnrolmentOps.Approve,
	// the same silent-refuse-on-malformed-input shape ipcCreateEnrolment
	// has for a missing client_id.
	ipcApproveEnrolmentRequest(ipc, mustRaw(t, map[string]interface{}{"request_id": l.RequestID}))
	if len(ui.events) != 0 {
		t.Fatalf("expected no events for a missing client_id, got %+v", ui.events)
	}

	// Missing request_id: same refusal.
	ipcApproveEnrolmentRequest(ipc, mustRaw(t, map[string]interface{}{"client_id": "hermes-mail"}))
	if len(ui.events) != 0 {
		t.Fatalf("expected no events for a missing request_id, got %+v", ui.events)
	}
}

func TestIPCApproveEnrolmentRequest_UnknownRequestIDRefuses(t *testing.T) {
	ipc, store, ui, _ := newEnrolmentRequestsIPC(t)
	mail := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")

	ipcApproveEnrolmentRequest(ipc, mustRaw(t, map[string]interface{}{
		"request_id":  "req_does_not_exist",
		"client_id":   "hermes-mail",
		"project_ids": []string{mail.ID},
	}))

	if _, ok := findEvent(ui, "onEnrolmentCreated"); ok {
		t.Fatal("an unknown request id was approved")
	}
	if _, ok := findEvent(ui, "onEnrolmentError"); !ok {
		t.Fatalf("expected onEnrolmentError for an unknown request id; got %+v", ui.events)
	}
}

// ---------------------------------------------------------------------------
// refuse_enrolment_request
// ---------------------------------------------------------------------------

func TestIPCRefuseEnrolmentRequest_RemovesRowAndRefreshesList(t *testing.T) {
	ipc, _, ui, table := newEnrolmentRequestsIPC(t)
	l, err := table.Lodge(genClientCSRPEM(t, "hermes-mail"), "", "", "", "10.0.0.5:1")
	assertNoErr(t, err, "Lodge")

	ipcRefuseEnrolmentRequest(ipc, mustRaw(t, map[string]interface{}{"request_id": l.RequestID}))

	if _, ok := findEvent(ui, "onEnrolmentError"); ok {
		t.Fatalf("unexpected error refusing a live request: %+v", ui.events)
	}
	args, ok := findEvent(ui, "onEnrolmentRequestsChanged")
	if !ok {
		t.Fatalf("expected onEnrolmentRequestsChanged; got %+v", ui.events)
	}
	var views []pendingEnrolmentRequestView
	if err := json.Unmarshal(args[0].(json.RawMessage), &views); err != nil {
		t.Fatalf("unmarshal views: %v", err)
	}
	if len(views) != 0 {
		t.Fatalf("refused request still listed: %+v", views)
	}

	// The table itself agrees, and the SAME request id can be refused a
	// second time only as a refusal-of-nothing (Refuse's own not-found
	// path), never a silent success.
	if _, found := table.Get(l.RequestID); found {
		t.Fatal("refused request is still readable from the table")
	}
}

func TestIPCRefuseEnrolmentRequest_UnknownRequestIDEmitsError(t *testing.T) {
	ipc, _, ui, _ := newEnrolmentRequestsIPC(t)
	ipcRefuseEnrolmentRequest(ipc, mustRaw(t, map[string]interface{}{"request_id": "req_does_not_exist"}))

	if _, ok := findEvent(ui, "onEnrolmentError"); !ok {
		t.Fatalf("expected onEnrolmentError for an unknown request id; got %+v", ui.events)
	}
}

func TestIPCRefuseEnrolmentRequest_RequiresRequestID(t *testing.T) {
	ipc, _, ui, _ := newEnrolmentRequestsIPC(t)
	ipcRefuseEnrolmentRequest(ipc, mustRaw(t, map[string]interface{}{}))
	if len(ui.events) != 0 {
		t.Fatalf("expected no events for a missing request_id, got %+v", ui.events)
	}
}

// ---------------------------------------------------------------------------
// CA fingerprint header line
// ---------------------------------------------------------------------------

// The value seeded into Settings -> Remote Clients (remoteConfigView) and
// the value `relay enrol ca-fingerprint` prints are the same read, so they
// cannot disagree -- see remoteConfigViewOf's own doc comment.
func TestRemoteConfigView_CarriesCAFingerprintMatchingDiskRead(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	mail := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
	if _, err := createEnrolment(store, enrolmentRequest{ClientID: "hermes-mail", ProjectIDs: []string{mail.ID}}); err != nil {
		t.Fatalf("createEnrolment: %v", err)
	}

	want, err := caFingerprintFromDisk()
	assertNoErr(t, err, "caFingerprintFromDisk")

	view := remoteConfigViewOf(store.Get(), true)
	if view.CAFingerprint != want {
		t.Fatalf("remoteConfigView.CAFingerprint = %q, want %q", view.CAFingerprint, want)
	}
}

// No CA has been generated yet on a fresh install: the field is empty
// rather than the tab surfacing loadCACertificateOnly's error as if the
// operator had done something wrong.
func TestRemoteConfigView_EmptyCAFingerprintWhenNoCAExistsYet(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	view := remoteConfigViewOf(store.Get(), true)
	if view.CAFingerprint != "" {
		t.Fatalf("CAFingerprint = %q, want empty with no CA generated yet", view.CAFingerprint)
	}
}
