package main

// ADR-015 decisions 2-3 plus the audit-record attribution fix: a class
// refusal through the real credentialAuthorizer must still name the
// credential that attempted it, and relay audit's table must render a
// control_decision row legibly rather than blank, without ever putting the
// bearer or its hash on the rendered surface.

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func acsNewAuditRecorder(t *testing.T) *AuditRecorder {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit", "toolcalls.jsonl")
	rec, err := NewAuditRecorder(&AuditConfig{}, path)
	if err != nil {
		t.Fatalf("NewAuditRecorder: %v", err)
	}
	if rec != nil {
		t.Cleanup(rec.Close)
	}
	return rec
}

func acsDoBearer(t *testing.T, method, url, token string) int {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	assertNoErr(t, err, "acsDoBearer: NewRequest")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	assertNoErr(t, err, "acsDoBearer: Do")
	resp.Body.Close()
	return resp.StatusCode
}

func acsMintCredential(t *testing.T, store SettingsStore, classes ...CapabilityClass) (APICredential, string) {
	t.Helper()
	var cred APICredential
	var plaintext string
	assertNoErr(t, store.With(func(s *Settings) {
		var err error
		cred, plaintext, err = s.Mint("acs-cred", classes)
		assertNoErr(t, err, "Mint")
	}), "store.With mint")
	return cred, plaintext
}

// A class refusal through the real Authorizer + RouteRegistrar.Handle must
// name the credential that attempted it, the same standing a tool-call
// denial already has; an unresolved bearer (absent or matching nothing) has
// no credential to name and must record an empty CredID rather than one
// invented from thin air.
func TestAcsControlDecision_ClassRefusalRecordsCredID_UnknownBearerRecordsEmpty(t *testing.T) {
	store := newCLISandboxStore(t)
	cred, plaintext := acsMintCredential(t, store, ClassRead)

	authz := NewCredentialAuthorizer(store)
	rec := acsNewAuditRecorder(t)

	mux := http.NewServeMux()
	rr := &RouteRegistrar{Mux: mux, Transport: TransportSocket, Authz: authz, Auditor: rec}
	rr.Handle(ClassGrant, "GET /api/acs-refuse", func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler must not run: credential lacks ClassGrant")
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	if status := acsDoBearer(t, "GET", srv.URL+"/api/acs-refuse", plaintext); status != http.StatusForbidden {
		t.Fatalf("class refusal status = %d, want 403", status)
	}
	if status := acsDoBearer(t, "GET", srv.URL+"/api/acs-refuse", "acs-not-a-real-token"); status != http.StatusUnauthorized {
		t.Fatalf("unknown bearer status = %d, want 401", status)
	}
	if status := acsDoBearer(t, "GET", srv.URL+"/api/acs-refuse", ""); status != http.StatusUnauthorized {
		t.Fatalf("absent bearer status = %d, want 401", status)
	}

	events := readLoggedEvents(t, rec)
	var control []AuditEvent
	for _, ev := range events {
		if ev.Event == AuditEventControlDecision {
			control = append(control, ev)
		}
	}
	if len(control) != 3 {
		t.Fatalf("want 3 control_decision records, got %d: %+v", len(control), control)
	}

	classRefusal := control[0]
	if classRefusal.Outcome != AuditOutcomeDenied {
		t.Fatalf("class-refusal outcome = %q, want %q", classRefusal.Outcome, AuditOutcomeDenied)
	}
	if classRefusal.Actor.CredID != cred.ID {
		t.Fatalf("class-refusal CredID = %q, want the resolved credential's id %q", classRefusal.Actor.CredID, cred.ID)
	}

	for _, ev := range control[1:] {
		if ev.Actor.CredID != "" {
			t.Fatalf("unresolved-bearer decision carries CredID %q, want empty: %+v", ev.Actor.CredID, ev)
		}
	}

	for _, ev := range control {
		dump := fmt.Sprintf("%+v", ev)
		ceAssertNoLeak(t, dump, plaintext, cred.Hash)
	}
}

// The rendered table is the surface the bug report calls out as blank for a
// control_decision row today; this proves the fix makes it legible and that
// legibility does not come at the cost of a leaked credential.
func TestAcsControlDecision_TableRow_ShowsMethodPathClassTransportCredID_NoLeak(t *testing.T) {
	store := newCLISandboxStore(t)
	const plaintext = "acs-render-plaintext-do-not-leak-114400"
	var cred APICredential
	assertNoErr(t, store.With(func(s *Settings) {
		cred = APICredential{
			ID:      "acs-render-cred",
			Name:    "acs-render",
			Hash:    hashToken(plaintext),
			Classes: []CapabilityClass{ClassGrant},
			Created: "2026-01-01T00:00:00Z",
		}
		s.AddAPICredential(cred)
	}), "store.With add credential")

	authz := NewCredentialAuthorizer(store)
	rec := acsNewAuditRecorder(t)

	mux := http.NewServeMux()
	rr := &RouteRegistrar{Mux: mux, Transport: TransportTCP, Authz: authz, Auditor: rec}
	rr.Handle(ClassGrant, "POST /api/enrolments", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	if status := acsDoBearer(t, "POST", srv.URL+"/api/enrolments", plaintext); status != http.StatusOK {
		t.Fatalf("allowed request status = %d, want 200", status)
	}

	events := readLoggedEvents(t, rec)
	ev := onlyEvent(t, events)
	if ev.Event != AuditEventControlDecision {
		t.Fatalf("event = %q, want %q", ev.Event, AuditEventControlDecision)
	}

	var buf bytes.Buffer
	writeAuditTable(&buf, []AuditEvent{ev}, false)
	rendered := buf.String()

	for _, want := range []string{"POST", "/api/enrolments", "class=grant", "transport=tcp", cred.ID} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered table row missing %q:\n%s", want, rendered)
		}
	}

	ceAssertNoLeak(t, rendered, plaintext, cred.Hash)
	ceAssertNoLeak(t, fmt.Sprintf("%+v", ev), plaintext, cred.Hash)
}
