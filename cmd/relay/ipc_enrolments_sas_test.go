package main

// §4.8: the pending-request view widened with the comparison state and the
// host's client-id suggestion. TestPendingEnrolmentRequestView_CarriesNoCSRBytes
// (ipc_enrolment_requests_test.go) is the other half of this and passes
// unmodified against the widened type — AC-49 — so what is added here is the
// positive coverage: the new fields carry what the panel needs, and the
// suggestion is advisory rather than authoritative.

import (
	"encoding/json"
	"github.com/barelyworkingcode/relay/internal/config"
	"strings"
	"testing"
)

// The three comparison states survive the projection intact. A view that
// collapsed "no code yet" into "no code ever" would render a register row as
// a carried-pin one and re-enable the Approve button this whole slice exists
// to disable.
func TestPendingEnrolmentRequestView_CarriesTheComparisonState(t *testing.T) {
	mkEmptySandboxRelayHome(t)
	table := newEnrolmentRequestTable()
	seedCAInto(t, table)

	legacy, err := table.Lodge(genClientCSRPEM(t, "carried"), "vm-carried", "", "", "10.0.0.5:1")
	assertNoErr(t, err, "legacy Lodge")

	waiting := newSASClient(t, "waiting")
	wl, err := table.Lodge(waiting.csrPEM, "vm-waiting", "p_mail", waiting.commit, "10.0.0.6:1")
	assertNoErr(t, err, "waiting Lodge")

	ready := newSASClient(t, "ready")
	rl, err := table.Lodge(ready.csrPEM, "vm-ready", "", ready.commit, "10.0.0.7:1")
	assertNoErr(t, err, "ready Lodge")
	if _, err := table.Poll(rl.RequestID, ready.open()); err != nil {
		t.Fatalf("Poll with a good open: %v", err)
	}

	failed := newSASClient(t, "failed")
	fl, err := table.Lodge(failed.csrPEM, "vm-failed", "", failed.commit, "10.0.0.8:1")
	assertNoErr(t, err, "failed Lodge")
	// A mismatched open is refused at the poll and marks the row failed —
	// the error IS the outcome under test here.
	if _, err := table.Poll(fl.RequestID, otherNonce(t)); err == nil {
		t.Fatal("Poll accepted an opening that does not match the commitment")
	}

	byID := map[string]pendingEnrolmentRequestView{}
	for _, v := range pendingEnrolmentRequestViewsOf(table.List(), &config.Settings{}) {
		byID[v.RequestID] = v
	}

	if v := byID[legacy.RequestID]; !v.IsLegacyRequest || v.SASReady || v.SASFailed || v.SAS != "" {
		t.Errorf("a carried-pin row projected as %+v, want legacy with no code", v)
	}
	if v := byID[wl.RequestID]; v.IsLegacyRequest || v.SASReady || v.SASFailed || v.SAS != "" {
		t.Errorf("an unopened row projected as %+v, want not-legacy, not-ready, not-failed", v)
	}
	if v := byID[wl.RequestID]; v.RequestedProfile != "p_mail" {
		t.Errorf("the requested profile did not reach the view: %+v", v)
	}
	if v := byID[rl.RequestID]; !v.SASReady || v.SAS == "" || v.SASFailed || v.IsLegacyRequest {
		t.Errorf("an opened row projected as %+v, want ready with a code", v)
	}
	if v := byID[fl.RequestID]; !v.SASFailed || v.SASReady || v.SAS != "" {
		t.Errorf("a mismatched open projected as %+v, want failed with no code", v)
	}
}

// The suggestion is the host's, and it resolves collisions the operator
// cannot see — suggestClientID's own contract, checked here at the boundary
// the sheet actually reads it from.
func TestPendingEnrolmentRequestView_SuggestsAFreeClientID(t *testing.T) {
	mkEmptySandboxRelayHome(t)
	table := newEnrolmentRequestTable()
	if _, err := table.Lodge(genClientCSRPEM(t, "a"), "hermes-mail", "", "", "10.0.0.5:1"); err != nil {
		t.Fatalf("Lodge: %v", err)
	}

	free := pendingEnrolmentRequestViewsOf(table.List(), &config.Settings{})
	if free[0].SuggestedClientID != "hermes-mail" {
		t.Errorf("suggestion for a free label = %q, want hermes-mail", free[0].SuggestedClientID)
	}

	taken := &config.Settings{Enrolments: []config.Enrolment{{ClientID: "hermes-mail"}}}
	collided := pendingEnrolmentRequestViewsOf(table.List(), taken)
	if collided[0].SuggestedClientID != "hermes-mail-2" {
		t.Errorf("suggestion for a taken label = %q, want hermes-mail-2", collided[0].SuggestedClientID)
	}
}

// The new fields ride out under the names the panel reads, and the widened
// view still says nothing about the CSR it was derived from.
func TestPendingEnrolmentRequestView_MarshalsTheNewFields(t *testing.T) {
	mkEmptySandboxRelayHome(t)
	table := newEnrolmentRequestTable()
	seedCAInto(t, table)

	c := newSASClient(t, "wire")
	l, err := table.Lodge(c.csrPEM, "vm-mail-a", "p_mail", c.commit, "10.0.0.5:1")
	assertNoErr(t, err, "Lodge")
	if _, err := table.Poll(l.RequestID, c.open()); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	data, err := json.Marshal(pendingEnrolmentRequestViewsOf(table.List(), &config.Settings{})[0])
	assertNoErr(t, err, "marshal view")
	for _, want := range []string{`"sas":`, `"sas_ready":true`, `"sas_failed":false`, `"is_legacy_request":false`, `"requested_profile":"p_mail"`, `"suggested_client_id":"vm-mail-a"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("marshalled view is missing %s: %s", want, data)
		}
	}
	if strings.Contains(strings.ToLower(string(data)), "csr") || strings.Contains(strings.ToLower(string(data)), "pem") {
		t.Fatalf("the widened view mentions CSR/PEM material: %s", data)
	}
}
