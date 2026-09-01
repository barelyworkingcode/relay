package main

// ADR-019 §6 under goja: the pending-request row's comparison code and its
// three states, and the four changes to the approval sheet. These run the
// real bundle through the same DOM shim settings_enrolments_ui_test.go uses,
// so what is asserted is the HTML the WebView would actually paint.

import (
	"strings"
	"testing"
)

// One row per state, as pendingEnrolmentRequestView projects them. The
// suggestion and the requested profile ride on the ready row, since that is
// the only one whose sheet can be opened.
const sasReadyRequest = `[{
	request_id: 'req_ready', spki_sha256: 'aa'.repeat(32), label: 'vm-mail-a',
	remote_addr: '10.0.0.7:51422', arrived_at: '2026-08-30T09:14:02Z',
	expires_at: '2026-08-30T09:29:02Z', approved: false,
	sas: '7K3P4Q', sas_ready: true, sas_failed: false, is_legacy_request: false,
	requested_profile: 'p_mail', suggested_client_id: 'vm-mail-a-2'
}]`

const sasWaitingRequest = `[{
	request_id: 'req_waiting', spki_sha256: 'bb'.repeat(32), label: 'vm-waiting',
	remote_addr: '10.0.0.7:51423', arrived_at: '2026-08-30T09:14:02Z',
	expires_at: '2026-08-30T09:29:02Z', approved: false,
	sas: '', sas_ready: false, sas_failed: false, is_legacy_request: false
}]`

const sasFailedRequest = `[{
	request_id: 'req_failed', spki_sha256: 'cc'.repeat(32), label: 'vm-failed',
	remote_addr: '10.0.0.7:51424', arrived_at: '2026-08-30T09:14:02Z',
	expires_at: '2026-08-30T09:29:02Z', approved: false,
	sas: '', sas_ready: false, sas_failed: true, is_legacy_request: false
}]`

const sasLegacyRequest = `[{
	request_id: 'req_legacy', spki_sha256: 'dd'.repeat(32), label: 'vm-carried',
	remote_addr: '10.0.0.7:51425', arrived_at: '2026-08-30T09:14:02Z',
	expires_at: '2026-08-30T09:29:02Z', approved: false,
	sas: '', sas_ready: false, sas_failed: false, is_legacy_request: true
}]`

// ---------------------------------------------------------------------------
// §6.2 — the row
// ---------------------------------------------------------------------------

// The code, and the sentence that says what to do with it. A code with no
// instruction is a number an operator clicks past.
func TestRemoteTab_ReadyRequestShowsTheComparisonCodeAndWhatToDoWithIt(t *testing.T) {
	vm := seedRemoteVMWithPending(t, enrolProjectsFixture, `[]`, remoteEnabled, sasReadyRequest)
	html := evalString(t, vm, `window.renderEnrolments()`)

	for _, want := range []string{
		"7K3P4Q",
		"Compare this with the code shown on the machine asking",
		"something is on the network path",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the ready row is missing %q\n%s", want, html)
		}
	}
}

// AC-48. A row with no completed comparison renders no enabled Approve
// control: approving one is approving without the control this whole slice
// exists to add.
func TestRemoteTab_ApproveIsDisabledWithoutACompletedComparison(t *testing.T) {
	cases := []struct {
		name, fixture, requestID string
		wantEnabled              bool
	}{
		{"comparison not yet opened", sasWaitingRequest, "req_waiting", false},
		{"comparison failed", sasFailedRequest, "req_failed", false},
		{"ready", sasReadyRequest, "req_ready", true},
		// A carried-pin request keeps its enabled Approve exactly as today —
		// this is what keeps `relayremote request` working unchanged.
		{"legacy carried-pin row", sasLegacyRequest, "req_legacy", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vm := seedRemoteVMWithPending(t, enrolProjectsFixture, `[]`, remoteEnabled, c.fixture)
			html := evalString(t, vm, `window.renderEnrolments()`)

			live := strings.Contains(html, `onclick="approveEnrolmentRequestForm('`+c.requestID+`')"`)
			if live != c.wantEnabled {
				t.Fatalf("enabled Approve = %v, want %v\n%s", live, c.wantEnabled, html)
			}
			if !c.wantEnabled && !strings.Contains(html, "disabled") {
				t.Fatalf("Approve is neither wired nor rendered disabled — it must be visibly refused, not merely absent\n%s", html)
			}
			// Refuse stays available in every state: a row nobody can
			// approve is exactly the one an operator wants to clear.
			if !strings.Contains(html, `onclick="refuseEnrolmentRequest('`+c.requestID+`')"`) {
				t.Fatalf("Refuse is not offered\n%s", html)
			}
		})
	}
}

// The two non-code states read as different sentences, because they are
// different facts: one is "not yet", the other is "never".
func TestRemoteTab_WaitingAndFailedComparisonsReadDifferently(t *testing.T) {
	waiting := evalString(t, seedRemoteVMWithPending(t, enrolProjectsFixture, `[]`, remoteEnabled, sasWaitingRequest), `window.renderEnrolments()`)
	if !strings.Contains(waiting, "complete the comparison handshake") {
		t.Errorf("an unopened row does not say what it is waiting for\n%s", waiting)
	}
	if strings.Contains(waiting, "failed its comparison") {
		t.Errorf("an unopened row reads as permanently refused\n%s", waiting)
	}

	failed := evalString(t, seedRemoteVMWithPending(t, enrolProjectsFixture, `[]`, remoteEnabled, sasFailedRequest), `window.renderEnrolments()`)
	if !strings.Contains(failed, "failed its comparison handshake") || !strings.Contains(failed, "cannot be approved") {
		t.Errorf("a failed row does not say it is permanently refused\n%s", failed)
	}

	legacy := evalString(t, seedRemoteVMWithPending(t, enrolProjectsFixture, `[]`, remoteEnabled, sasLegacyRequest), `window.renderEnrolments()`)
	if !strings.Contains(legacy, "carried-pin request") || !strings.Contains(legacy, "CA fingerprint") {
		t.Errorf("a carried-pin row does not name the control that applies to it\n%s", legacy)
	}
}

// ---------------------------------------------------------------------------
// AC-47 — the requested profile is displayed and never honoured
// ---------------------------------------------------------------------------

// It renders as a request in both the row and the sheet, its checkbox is
// unticked on first render, and approving without touching it issues nothing.
func TestRemoteTab_RequestedProfileIsShownAsARequestAndNeverPreTicked(t *testing.T) {
	vm := seedRemoteVMWithPending(t, enrolProjectsFixture, `[]`, remoteEnabled, sasReadyRequest)

	row := evalString(t, vm, `window.renderEnrolments()`)
	if !strings.Contains(row, "asked for:") || !strings.Contains(row, "not a grant") {
		t.Errorf("the row does not mark the requested profile as a request\n%s", row)
	}

	sheet := evalString(t, vm, `(function(){
		window.approveEnrolmentRequestForm('req_ready');
		return window.renderEnrolmentForm();
	})()`)
	if !strings.Contains(sheet, "This machine asked for") || !strings.Contains(sheet, "not a grant") {
		t.Errorf("the sheet does not mark the requested profile as a request\n%s", sheet)
	}
	if strings.Contains(sheet, `checked onchange="toggleEnrolGrant('p_mail'`) {
		t.Fatalf("the requested profile was pre-ticked\n%s", sheet)
	}

	selected := evalString(t, vm, `JSON.stringify(window.state.enrolForm.project_ids)`)
	if selected != "[]" {
		t.Fatalf("opening the sheet pre-selected %s, want []", selected)
	}

	// Approving without touching the list sends an empty grant — the
	// operator having said so explicitly, which is AC-46's other half.
	sent := evalString(t, vm, `(function(){
		window.toggleEnrolNoGrant(true);
		document.getElementById('enrolClientId').value = 'vm-mail-a-2';
		window.saveEnrolment();
		return window.__sent[window.__sent.length - 1];
	})()`)
	if !strings.Contains(sent, `"type":"approve_enrolment_request"`) {
		t.Fatalf("approval was not sent: %s", sent)
	}
	if !strings.Contains(sent, `"project_ids":[]`) {
		t.Fatalf("approving without touching the requested profile issued a grant: %s", sent)
	}
}

// ---------------------------------------------------------------------------
// AC-46 (the sheet) — a grant is required
// ---------------------------------------------------------------------------

// Zero profiles is a legal answer and an explicit one. The checkbox is never
// pre-ticked, the save is refused until one or the other is true, and the
// button says so rather than failing on click.
func TestRemoteTab_ApprovalRequiresAGrantOrAnExplicitNoGrant(t *testing.T) {
	vm := seedRemoteVMWithPending(t, enrolProjectsFixture, `[]`, remoteEnabled, sasReadyRequest)

	first := evalString(t, vm, `(function(){
		window.approveEnrolmentRequestForm('req_ready');
		return JSON.stringify({
			noGrant: window.state.enrolForm.no_grant,
			html: window.renderEnrolmentForm()
		});
	})()`)
	if !strings.Contains(first, `"noGrant":false`) {
		t.Fatalf("the no-access checkbox was pre-ticked: %s", first)
	}
	if !strings.Contains(first, "Enrol with no access for now") {
		t.Fatalf("the sheet does not offer the explicit no-access answer: %s", first)
	}
	if !strings.Contains(first, "disabled") {
		t.Fatalf("the issue button is not disabled with nothing chosen: %s", first)
	}

	refused := evalString(t, vm, `(function(){
		document.getElementById('enrolClientId').value = 'vm-mail-a-2';
		window.saveEnrolment();
		return JSON.stringify({ sent: window.__sent.length, err: window.state.enrolmentError });
	})()`)
	if !strings.Contains(refused, `"sent":0`) {
		t.Fatalf("an approval with no grant and no explicit answer was sent: %s", refused)
	}
	if !strings.Contains(refused, "Enrol with no access for now") {
		t.Fatalf("the refusal does not name the way past it: %s", refused)
	}

	allowed := evalString(t, vm, `(function(){
		window.toggleEnrolGrant('p_mail', true);
		document.getElementById('enrolClientId').value = 'vm-mail-a-2';
		window.saveEnrolment();
		return window.__sent[window.__sent.length - 1];
	})()`)
	if !strings.Contains(allowed, `"project_ids":["p_mail"]`) {
		t.Fatalf("choosing a profile did not unblock the approval: %s", allowed)
	}
}

// The same rule on the plain create path: ADR-019 §7's "never the silent
// default" is a property of the form, not of one of its two entry points.
func TestRemoteTab_CreateAlsoRequiresAGrantOrAnExplicitNoGrant(t *testing.T) {
	vm := seedRemoteVM(t, enrolProjectsFixture, `[]`, remoteEnabled)
	got := evalString(t, vm, `(function(){
		window.newEnrolment();
		document.getElementById('enrolClientId').value = 'hermes-mail';
		window.saveEnrolment();
		var refused = window.__sent.length;
		window.toggleEnrolNoGrant(true);
		document.getElementById('enrolClientId').value = 'hermes-mail';
		window.saveEnrolment();
		return JSON.stringify({ refused: refused, sent: window.__sent[window.__sent.length - 1] });
	})()`)
	if !strings.Contains(got, `"refused":0`) {
		t.Fatalf("a create with no grant and no explicit answer was sent: %s", got)
	}
	if !strings.Contains(got, `create_enrolment`) {
		t.Fatalf("ticking the explicit no-access answer did not let the create through: %s", got)
	}
}

// ---------------------------------------------------------------------------
// §6.3 — the rest of the sheet
// ---------------------------------------------------------------------------

// The code repeats at the top of the sheet: this is the panel open at the
// moment the decision is made, and a comparison the operator has to scroll
// back to is a comparison nobody makes.
func TestRemoteTab_ApprovalSheetRepeatsTheComparisonCode(t *testing.T) {
	vm := seedRemoteVMWithPending(t, enrolProjectsFixture, `[]`, remoteEnabled, sasReadyRequest)
	sheet := evalString(t, vm, `(function(){
		window.approveEnrolmentRequestForm('req_ready');
		return window.renderEnrolmentForm();
	})()`)

	code := strings.Index(sheet, "7K3P4Q")
	identity := strings.Index(sheet, "Identity")
	if code < 0 {
		t.Fatalf("the sheet does not repeat the comparison code\n%s", sheet)
	}
	if identity < 0 || code > identity {
		t.Fatalf("the comparison code is not above the identity section\n%s", sheet)
	}
	// The CA fingerprint line is still there beside it (spec §6.3 change 1).
	if !strings.Contains(sheet, "sha256:41c7f0a1b2c3d4e5f60718293a4b5c6d7e8f9001122334455667788990011ff") {
		t.Errorf("the sheet lost the CA fingerprint line\n%s", sheet)
	}
}

// The client id is pre-filled with the HOST's collision-free suggestion, not
// with the request's label — the label is hostile input and the host owns its
// own client_id namespace (§9.5).
func TestRemoteTab_ApprovalSheetPreFillsTheHostsSuggestionNotTheLabel(t *testing.T) {
	vm := seedRemoteVMWithPending(t, enrolProjectsFixture, `[]`, remoteEnabled, sasReadyRequest)
	got := evalString(t, vm, `(function(){
		window.approveEnrolmentRequestForm('req_ready');
		return JSON.stringify({ id: window.state.enrolForm.client_id, html: window.renderEnrolmentForm() });
	})()`)

	if !strings.Contains(got, `"id":"vm-mail-a-2"`) {
		t.Fatalf("the client id was not pre-filled with suggested_client_id: %s", got)
	}
	if !strings.Contains(got, "relay suggests this") && !strings.Contains(got, "Relay suggests this") {
		t.Errorf("the sheet does not say the suggestion is the operator's to change: %s", got)
	}
	if !strings.Contains(got, `value=\"vm-mail-a-2\"`) {
		t.Errorf("the suggestion is not the input's value: %s", got)
	}
}

// A row the host had no suggestion for leaves the field empty rather than
// falling back to the label as a value — the label stays a placeholder.
func TestRemoteTab_NoSuggestionLeavesTheClientIDEmpty(t *testing.T) {
	noSuggestion := strings.Replace(sasReadyRequest, `, suggested_client_id: 'vm-mail-a-2'`, ``, 1)
	vm := seedRemoteVMWithPending(t, enrolProjectsFixture, `[]`, remoteEnabled, noSuggestion)
	got := evalString(t, vm, `(function(){
		window.approveEnrolmentRequestForm('req_ready');
		return JSON.stringify({ id: window.state.enrolForm.client_id, html: window.renderEnrolmentForm() });
	})()`)
	if !strings.Contains(got, `"id":""`) {
		t.Fatalf("a request with no suggestion pre-filled something: %s", got)
	}
	if !strings.Contains(got, `placeholder=\"vm-mail-a\"`) {
		t.Errorf("the label was not offered as the placeholder: %s", got)
	}
}

// Every checkbox on this form re-renders it, so the typed client id has to
// survive the round trip — otherwise the required grant choice erases the
// required identity.
func TestRemoteTab_TogglingAGrantDoesNotEraseTheTypedClientID(t *testing.T) {
	vm := seedRemoteVM(t, enrolProjectsFixture, `[]`, remoteEnabled)
	got := evalString(t, vm, `(function(){
		window.newEnrolment();
		document.getElementById('enrolClientId').value = 'hermes-mail';
		document.getElementById('enrolMaxCalls').value = '30';
		window.toggleEnrolGrant('p_mail', true);
		return JSON.stringify({ id: window.state.enrolForm.client_id, calls: window.state.enrolForm.max_calls });
	})()`)
	if !strings.Contains(got, `"id":"hermes-mail"`) {
		t.Fatalf("ticking a profile erased the typed client id: %s", got)
	}
	if !strings.Contains(got, `"calls":"30"`) {
		t.Fatalf("ticking a profile erased a typed budget field: %s", got)
	}
}

// ---------------------------------------------------------------------------
// AC-48, the submit path — the rule holds at the door, not only on the control
// ---------------------------------------------------------------------------

// The disabled button is what the operator sees; this is what a future edit
// that re-enables it would hit. The failure mode being guarded is a silent
// widening — a sheet that opens for a row nothing on the host will approve —
// so the rule is asserted here as well as on the control and in
// EnrolmentOps.Approve.
func TestRemoteTab_ApproveFormRefusesToOpenWithoutACompletedComparison(t *testing.T) {
	cases := []struct {
		name, fixture, requestID string
		wantOpens                bool
	}{
		{"comparison not yet opened", sasWaitingRequest, "req_waiting", false},
		{"comparison failed", sasFailedRequest, "req_failed", false},
		{"ready", sasReadyRequest, "req_ready", true},
		{"legacy carried-pin row", sasLegacyRequest, "req_legacy", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vm := seedRemoteVMWithPending(t, enrolProjectsFixture, `[]`, remoteEnabled, c.fixture)
			got := evalString(t, vm, `(function(){
				window.approveEnrolmentRequestForm('`+c.requestID+`');
				return JSON.stringify({
					opened: !!window.state.enrolForm,
					err: window.state.enrolmentError,
					sent: window.__sent.length
				});
			})()`)

			if c.wantOpens {
				if !strings.Contains(got, `"opened":true`) {
					t.Fatalf("the approval sheet refused to open for an approvable row: %s", got)
				}
				return
			}
			if !strings.Contains(got, `"opened":false`) {
				t.Fatalf("the approval sheet opened for a row with no completed comparison: %s", got)
			}
			if !strings.Contains(got, "comparison handshake") {
				t.Errorf("the refusal does not say why: %s", got)
			}
			if !strings.Contains(got, `"sent":0`) {
				t.Errorf("a refused approval still sent something: %s", got)
			}
		})
	}
}

// The refusal is on screen, not only in state: a click that appears to do
// nothing is a bug report.
func TestRemoteTab_RefusedApprovalSheetSaysSoInThePanel(t *testing.T) {
	vm := seedRemoteVMWithPending(t, enrolProjectsFixture, `[]`, remoteEnabled, sasFailedRequest)
	html := evalString(t, vm, `(function(){
		window.approveEnrolmentRequestForm('req_failed');
		return window.renderEnrolments();
	})()`)
	if !strings.Contains(html, "comparison handshake") {
		t.Fatalf("the panel does not show why the approval was refused\n%s", html)
	}
}
