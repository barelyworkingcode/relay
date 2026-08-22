package main

// Remote Clients tab under goja, using the same bundle + DOM shim as
// settings_bundle_test.go. ADR-010 decision 8 says enrolments are listed
// "beside the grants they reach — a credential you cannot see is one you will
// not revoke", so these tests pin the parts of that sentence a reviewer
// actually depends on: that a grant reads as a project NAME, that the
// certificate fingerprint reaches the screen whole, that no key material has
// any path into the page, and that the three states of the `remote` block are
// told apart rather than collapsed into "not working".

import (
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// Two remote projects and one local one. The local project exists so the
// "only remote projects are offered" test has something to fail against.
const enrolProjectsFixture = `[
	{id:'p_mail', name:'Mail', kind:'remote', allowed_mcp_ids:['macmcp'], allowed_models:[], disabled_tools:{}},
	{id:'p_cal',  name:'Calendar', kind:'remote', allowed_mcp_ids:['macmcp'], allowed_models:[], disabled_tools:{}},
	{id:'p_work', name:'Workspace', path:'/Users/x/work', allowed_mcp_ids:['*'], allowed_models:['*'], disabled_tools:{}}
]`

// A full sha256 fingerprint — 64 hex characters after the prefix, which is the
// length the UI must render without shortening.
const enrolFingerprint = "sha256:9f2a4c1d6b8e0f37a5c9d2e4b6081f3a7c5e9d1b3f5a7c9e1d3b5f7a9c1e3d5b"

const enrolmentsFixture = `[{
	client_id: 'hermes-mail',
	fingerprint: '` + enrolFingerprint + `',
	project_ids: ['p_mail', 'p_cal'],
	budget: { window_seconds: 60, max_calls: 60, max_result_bytes: 8388608 },
	created_at: '2026-08-20T09:14:00Z'
}]`

const remoteEnabled = `{configured:true, enabled:true, listen:'127.0.0.1:9910', effective:'127.0.0.1:9910', audit_enabled:true}`
const remoteDisabled = `{configured:true, enabled:false, listen:'127.0.0.1:9910', effective:'127.0.0.1:9910', audit_enabled:true}`
const remoteAbsent = `{configured:false, enabled:false, listen:'', effective:'127.0.0.1:9910', audit_enabled:true}`
const remoteAuditOff = `{configured:true, enabled:true, listen:'127.0.0.1:9910', effective:'127.0.0.1:9910', audit_enabled:false}`

// seedRemoteVM loads the app bundle, switches to the Remote Clients tab, and
// installs the two browser affordances the shim lacks: confirm() (captured, so
// a test can read the destructive prompt) and the webkit IPC channel (captured,
// so a test can read what the page actually sent).
func seedRemoteVM(t *testing.T, projectsJSON, enrolmentsJSON, remoteJSON string) *goja.Runtime {
	t.Helper()
	vm := newAppVM(t)
	script := `(function(){
		window.__confirmed = [];
		window.__confirmAnswer = true;
		window.confirm = function(msg){ window.__confirmed.push(String(msg)); return window.__confirmAnswer; };
		window.__sent = [];
		window.webkit = { messageHandlers: { ipc: { postMessage: function(m){ window.__sent.push(String(m)); } } } };
		window.state.page = 'remote';
		window.state.projects = ` + projectsJSON + `;
		window.state.enrolments = ` + enrolmentsJSON + `;
		window.state.remote = ` + remoteJSON + `;
		window.state.remoteDraft = null;
		window.state.enrolForm = null;
		window.state.enrolBundle = null;
		window.state.enrolRevoked = null;
		window.state.enrolmentBudgetDefaults = {window_seconds:60, max_calls:60, max_result_bytes:8388608};
		return true;
	})()`
	if _, err := vm.RunString(script); err != nil {
		t.Fatalf("seeding remote state: %v", err)
	}
	return vm
}

// ---------------------------------------------------------------------------
// The list
// ---------------------------------------------------------------------------

// Grants render as project NAMES. An operator about to revoke has to know what
// they are cutting; "p_mail, p_cal" is not that. The id survives only as the
// chip's tooltip, so nothing is lost — it is just not what you read first.
func TestRemoteTab_GrantsRenderAsProjectNames(t *testing.T) {
	vm := seedRemoteVM(t, enrolProjectsFixture, enrolmentsFixture, remoteEnabled)
	html := evalString(t, vm, `window.renderEnrolments()`)

	for _, want := range []string{
		"hermes-mail",                // the enrolment
		`title="p_mail">Mail</span>`, // name is the text, id is the tooltip
		`title="p_cal">Calendar</span>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("enrolment card is missing %q\n%s", want, html)
		}
	}
}

// A grant naming an access profile that no longer exists is SHOWN, marked, not
// silently dropped: an enrolment holding something relay cannot resolve is
// exactly the row an operator needs to see before deciding to revoke.
func TestRemoteTab_DanglingGrantIsShownNotHidden(t *testing.T) {
	vm := seedRemoteVM(t, enrolProjectsFixture, `[{
		client_id:'ghost', fingerprint:'`+enrolFingerprint+`',
		project_ids:['p_deleted'], budget:{window_seconds:60,max_calls:60,max_result_bytes:1024},
		created_at:'2026-08-20T09:14:00Z'
	}]`, remoteEnabled)
	html := evalString(t, vm, `window.renderEnrolments()`)

	if !strings.Contains(html, "p_deleted") || !strings.Contains(html, "unknown access profile") {
		t.Errorf("a grant naming a missing access profile was not surfaced\n%s", html)
	}
	if !strings.Contains(html, "enrol-grant dangling") {
		t.Error("dangling grant did not get its own class, so it reads like a normal grant")
	}
}

// The fingerprint prints in full. After the enrolment is deleted it is the only
// thing that identifies this client's calls in the audit log (which is why the
// Tool Calls tab prints it whole too), so any truncation here would be the
// obvious place for someone to copy a short form from.
func TestRemoteTab_FingerprintRendersInFull(t *testing.T) {
	vm := seedRemoteVM(t, enrolProjectsFixture, enrolmentsFixture, remoteEnabled)
	html := evalString(t, vm, `window.renderEnrolments()`)

	if !strings.Contains(html, enrolFingerprint) {
		t.Fatalf("the full fingerprint is not on screen\n%s", html)
	}
	// And it is not ALSO rendered shortened somewhere — an ellipsis adjacent
	// to a fingerprint prefix is the shape a truncation regression takes.
	for _, bad := range []string{enrolFingerprint[:20] + "…", enrolFingerprint[:20] + "..."} {
		if strings.Contains(html, bad) {
			t.Errorf("a truncated fingerprint appears in the page: %q", bad)
		}
	}
}

// The budget is on the card, because a budget nobody can see is a budget
// nobody will tune — and `throttled` in the audit log is meaningless without
// the number it was measured against.
func TestRemoteTab_BudgetIsVisibleOnTheCard(t *testing.T) {
	vm := seedRemoteVM(t, enrolProjectsFixture, enrolmentsFixture, remoteEnabled)
	html := evalString(t, vm, `window.renderEnrolments()`)
	for _, want := range []string{"60 calls", "8 MiB", "per 60s"} {
		if !strings.Contains(html, want) {
			t.Errorf("budget summary is missing %q", want)
		}
	}
}

// ---------------------------------------------------------------------------
// Key material
// ---------------------------------------------------------------------------

// The page holds a bundle DIRECTORY and nothing else. This drives the create
// event with a hostile payload — one carrying key material the Go side never
// sends — and proves the page would still not render or retain it: the only
// field read off the bundle is `dir`.
func TestRemoteTab_NeverRendersOrRetainsKeyMaterial(t *testing.T) {
	vm := seedRemoteVM(t, enrolProjectsFixture, `[]`, remoteEnabled)
	got := evalString(t, vm, `(function(){
		window.onEnrolmentCreated(
			{client_id:'hermes-mail', fingerprint:'`+enrolFingerprint+`', project_ids:['p_mail'],
			 budget:{window_seconds:60,max_calls:60,max_result_bytes:8388608}, created_at:'2026-08-20T09:14:00Z'},
			{dir:'/tmp/relay/enrolments/hermes-mail',
			 key_pem:'-----BEGIN PRIVATE KEY-----MIIEvgIBADANBgkq-----END PRIVATE KEY-----',
			 key_path:'/tmp/relay/enrolments/hermes-mail/client.key'});
		var html = window.renderEnrolments();
		return JSON.stringify({
			bundleKeys: Object.keys(window.state.enrolBundle).sort(),
			showsDir: html.indexOf('/tmp/relay/enrolments/hermes-mail') >= 0,
			leaksPem: html.indexOf('BEGIN PRIVATE KEY') >= 0 || html.indexOf('MIIEvgIBADANBgkq') >= 0,
			stateLeaksPem: JSON.stringify(window.state).indexOf('MIIEvgIBADANBgkq') >= 0
		});
	})()`)

	for _, want := range []string{
		`"bundleKeys":["client_id","dir"]`, // nothing else is kept
		`"showsDir":true`,
		`"leaksPem":false`,
		`"stateLeaksPem":false`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("bundle handling: missing %s in %s", want, got)
		}
	}
}

// The banner tells the operator to MOVE the directory, which is the only
// mitigation available for a key that travels — the same thing
// `relay enrol create` says, in the same words.
func TestRemoteTab_BundleBannerSaysMoveNotCopy(t *testing.T) {
	vm := seedRemoteVM(t, enrolProjectsFixture, `[]`, remoteEnabled)
	html := evalString(t, vm, `(function(){
		window.state.enrolBundle = {client_id:'hermes-mail', dir:'/tmp/relay/enrolments/hermes-mail'};
		return window.renderEnrolments();
	})()`)
	for _, want := range []string{"Move (don't copy)", "client.key", "ca.crt", "/tmp/relay/enrolments/hermes-mail"} {
		if !strings.Contains(html, want) {
			t.Errorf("bundle banner is missing %q", want)
		}
	}
}

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

// ValidateEnrolmentGrants refuses a grant naming a local project, so the form
// must not offer one: a UI that presents a choice the server is about to
// reject teaches the operator that refusals are arbitrary.
func TestRemoteTab_OnlyRemoteProjectsAreOffered(t *testing.T) {
	vm := seedRemoteVM(t, enrolProjectsFixture, `[]`, remoteEnabled)
	got := evalString(t, vm, `(function(){
		window.newEnrolment();
		var html = window.renderEnrolmentForm();
		return JSON.stringify({
			offered: window.remoteGrantableProjects().map(function(p){ return p.id; }),
			showsMail: html.indexOf('p_mail') >= 0,
			showsCalendar: html.indexOf('p_cal') >= 0,
			showsLocalProject: html.indexOf('p_work') >= 0 || html.indexOf('Workspace') >= 0
		});
	})()`)

	for _, want := range []string{
		`"offered":["p_mail","p_cal"]`,
		`"showsMail":true`,
		`"showsCalendar":true`,
		`"showsLocalProject":false`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("grant picker: missing %s in %s", want, got)
		}
	}
}

// With no access profile to grant, the form says where to make one rather than
// showing an empty list that looks broken.
func TestRemoteTab_NoRemoteProjectsExplainsWhere(t *testing.T) {
	vm := seedRemoteVM(t, `[{id:'p_work', name:'Workspace', path:'/w', allowed_mcp_ids:['*'], allowed_models:['*'], disabled_tools:{}}]`, `[]`, remoteEnabled)
	html := evalString(t, vm, `(function(){ window.newEnrolment(); return window.renderEnrolmentForm(); })()`)
	if !strings.Contains(html, "No access profiles exist yet") || !strings.Contains(html, "Projects") {
		t.Errorf("empty grant picker does not say where to create an access profile\n%s", html)
	}
}

// The create payload carries the chosen grants and sends 0 for a blank budget
// field — which normalizeEnrolmentBudget reads as "unset, use the conservative
// default". Zero must never travel as, or be read as, "unlimited".
func TestRemoteTab_CreatePayloadShape(t *testing.T) {
	vm := seedRemoteVM(t, enrolProjectsFixture, `[]`, remoteEnabled)
	got := evalString(t, vm, `(function(){
		window.newEnrolment();
		window.toggleEnrolGrant('p_mail', true);
		document.getElementById('enrolClientId').value = '  hermes-mail  ';
		document.getElementById('enrolWindow').value = '';
		document.getElementById('enrolMaxCalls').value = '30';
		document.getElementById('enrolMaxBytes').value = '';
		window.saveEnrolment();
		return window.__sent[window.__sent.length - 1];
	})()`)

	for _, want := range []string{
		`"type":"create_enrolment"`,
		`"client_id":"hermes-mail"`, // trimmed
		`"project_ids":["p_mail"]`,
		`"window_seconds":0`, // blank -> unset -> server default
		`"max_calls":30`,
		`"max_result_bytes":0`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("create payload missing %s in %s", want, got)
		}
	}
}

// Enrolling with no grant is legal and is the expected "enrol now, widen
// deliberately later" resting state — but it is said out loud, exactly as
// `relay enrol create` says it, rather than silently issuing a certificate
// that reaches nothing.
func TestRemoteTab_ZeroGrantsIsAllowedAndAnnounced(t *testing.T) {
	vm := seedRemoteVM(t, enrolProjectsFixture, `[]`, remoteEnabled)
	html := evalString(t, vm, `(function(){ window.newEnrolment(); return window.renderEnrolmentForm(); })()`)
	if !strings.Contains(html, "can reach no access profile until one is added") {
		t.Errorf("form does not say what a grantless enrolment means\n%s", html)
	}
}

func TestRemoteTab_CreateRequiresClientID(t *testing.T) {
	vm := seedRemoteVM(t, enrolProjectsFixture, `[]`, remoteEnabled)
	got := evalString(t, vm, `(function(){
		window.newEnrolment();
		document.getElementById('enrolClientId').value = '   ';
		window.saveEnrolment();
		return JSON.stringify({ sent: window.__sent.length, err: window.state.enrolmentError });
	})()`)
	if !strings.Contains(got, `"sent":0`) {
		t.Error("a create with no client id was sent to the backend")
	}
	if !strings.Contains(got, "client id is required") {
		t.Errorf("no error shown for a missing client id: %s", got)
	}
}

// ---------------------------------------------------------------------------
// Revoke
// ---------------------------------------------------------------------------

// The confirmation NAMES what is being cut. "Are you sure?" over an opaque
// client id is not a decision anyone can make, and the wording matches
// `relay enrol revoke`: the certificate is unchanged, no project is touched,
// and live connections close immediately.
func TestRemoteTab_RevokeConfirmationNamesTheGrants(t *testing.T) {
	vm := seedRemoteVM(t, enrolProjectsFixture, enrolmentsFixture, remoteEnabled)
	got := evalString(t, vm, `(function(){
		window.__confirmAnswer = false;   // decline: nothing may be sent
		window.revokeEnrolment('hermes-mail');
		return JSON.stringify({ prompt: window.__confirmed[0] || '', sent: window.__sent.length });
	})()`)

	for _, want := range []string{"hermes-mail", "Mail", "Calendar", "Live connections", "no project is touched"} {
		if !strings.Contains(got, want) {
			t.Errorf("revoke confirmation is missing %q: %s", want, got)
		}
	}
	if !strings.Contains(got, `"sent":0`) {
		t.Error("declining the confirmation still sent a revoke")
	}
}

// Accepting sends the revoke; the event handler then removes the row and keeps
// the fingerprint on screen, because that is now the only thing naming this
// client's calls in the Tool Calls log.
func TestRemoteTab_RevokeRemovesTheRowAndKeepsTheFingerprint(t *testing.T) {
	vm := seedRemoteVM(t, enrolProjectsFixture, enrolmentsFixture, remoteEnabled)
	got := evalString(t, vm, `(function(){
		window.revokeEnrolment('hermes-mail');
		var sent = window.__sent[window.__sent.length - 1];
		window.onEnrolmentRevoked('hermes-mail', '`+enrolFingerprint+`');
		var html = window.renderEnrolments();
		return JSON.stringify({
			sent: sent,
			remaining: window.state.enrolments.length,
			keepsFingerprint: html.indexOf('`+enrolFingerprint+`') >= 0,
			showsEmptyState: html.indexOf('No enrolled clients') >= 0
		});
	})()`)

	for _, want := range []string{
		`\"type\":\"revoke_enrolment\"`,
		`\"client_id\":\"hermes-mail\"`,
		`"remaining":0`,
		`"keepsFingerprint":true`,
		`"showsEmptyState":true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("revoke flow: missing %s in %s", want, got)
		}
	}
}

// ---------------------------------------------------------------------------
// The listener block
// ---------------------------------------------------------------------------

// Absent, present-but-disabled, and enabled are three different facts about an
// install, and two of them are surprising. Each gets its own badge and its own
// sentence — an absent block is NOT a switched-off listener.
func TestRemoteTab_ListenerStatesRenderDistinctly(t *testing.T) {
	cases := []struct {
		name       string
		remoteJSON string
		wantBadge  string
		wantCopy   string
		notWant    string
	}{
		{
			name:       "absent",
			remoteJSON: remoteAbsent,
			wantBadge:  `remote-state absent`,
			wantCopy:   "no listener is opened at all",
			notWant:    `remote-state on`,
		},
		{
			name:       "present but disabled",
			remoteJSON: remoteDisabled,
			wantBadge:  `remote-state off`,
			wantCopy:   "omits <code>enabled</code> resolves to disabled",
			notWant:    "no listener is opened at all",
		},
		{
			name:       "enabled",
			remoteJSON: remoteEnabled,
			wantBadge:  `remote-state on`,
			wantCopy:   "127.0.0.1:9910",
			notWant:    "no listener is opened at all",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vm := seedRemoteVM(t, enrolProjectsFixture, `[]`, c.remoteJSON)
			html := evalString(t, vm, `window.renderRemoteListener()`)
			if !strings.Contains(html, c.wantBadge) {
				t.Errorf("missing badge %q\n%s", c.wantBadge, html)
			}
			if !strings.Contains(html, c.wantCopy) {
				t.Errorf("missing copy %q\n%s", c.wantCopy, html)
			}
			if strings.Contains(html, c.notWant) {
				t.Errorf("state %q also rendered %q", c.name, c.notWant)
			}
		})
	}
}

// Auditing is a hard dependency of remote access: the listener refuses to
// start while it is off. A block that looks configured in that state is dead,
// and the tab has to say so as a CONSEQUENCE rather than let it read as on.
func TestRemoteTab_DisabledAuditingReadsAsRemoteOff(t *testing.T) {
	vm := seedRemoteVM(t, enrolProjectsFixture, enrolmentsFixture, remoteAuditOff)
	html := evalString(t, vm, `window.renderEnrolments()`)

	for _, want := range []string{
		"Remote access is off because the tool-call audit log is disabled",
		"refuses to start",
		"Off &#8212; auditing disabled",
	} {
		if !strings.Contains(html, want) && !strings.Contains(html, strings.ReplaceAll(want, "&#8212;", "—")) {
			t.Errorf("audit-off consequence is missing %q\n%s", want, html)
		}
	}
	if strings.Contains(html, `remote-state on`) {
		t.Error("the listener still badges as On while auditing is disabled")
	}
}

// The restart caveat is honest today and lives in exactly one place. This test
// exists so that removing it (when the listener picks address changes up live)
// is a deliberate act with a failing test attached, not a silent drift into
// telling the operator something untrue.
func TestRemoteTab_StatesWhatIsLiveAndWhatIsNot(t *testing.T) {
	vm := seedRemoteVM(t, enrolProjectsFixture, `[]`, remoteEnabled)
	html := evalString(t, vm, `window.renderRemoteListener()`)
	// The listener reconciles, so a blanket "restart required" would be false
	// for everything a user is likely to change. The one genuine exception is
	// re-enabling auditing, and that must still be stated.
	if !strings.Contains(html, "no restart needed") {
		t.Errorf("the listener section does not say reconfiguration is live\n%s", html)
	}
	if !strings.Contains(html, "Re-enabling auditing") {
		t.Errorf("the listener section does not state the one case that still needs a relaunch\n%s", html)
	}
}

// The default binds loopback so misconfiguration cannot expose the control
// plane to a LAN. A wider bind is allowed — it is how you reach relay from a
// VM — but it is said out loud rather than accepted in silence.
func TestRemoteTab_NonLoopbackBindIsWarned(t *testing.T) {
	vm := seedRemoteVM(t, enrolProjectsFixture, `[]`, remoteEnabled)
	got := evalString(t, vm, `(function(){
		var quiet = window.renderRemoteListener().indexOf('binds beyond loopback') >= 0;
		window.remoteDraftSet('listen', '0.0.0.0:9910');
		var loud = window.renderRemoteListener().indexOf('binds beyond loopback') >= 0;
		return JSON.stringify({
			loopbackQuiet: quiet, wideLoud: loud,
			isLoopback: window.remoteListenIsLoopback('127.0.0.1:9910'),
			v6Loopback: window.remoteListenIsLoopback('[::1]:9910'),
			wide: window.remoteListenIsLoopback('0.0.0.0:9910')
		});
	})()`)

	for _, want := range []string{
		`"loopbackQuiet":false`, `"wideLoud":true`,
		`"isLoopback":true`, `"v6Loopback":true`, `"wide":false`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("loopback warning: missing %s in %s", want, got)
		}
	}
}

// Saving sends the toggle and the address together; removing sends `remove`
// after a confirmation that says what an absent block means.
func TestRemoteTab_SaveAndRemovePayloads(t *testing.T) {
	vm := seedRemoteVM(t, enrolProjectsFixture, `[]`, remoteDisabled)
	got := evalString(t, vm, `(function(){
		window.remoteDraftSet('enabled', true);
		window.remoteDraftSet('listen', '  127.0.0.1:9911  ');
		window.saveRemoteConfig();
		var save = window.__sent[window.__sent.length - 1];
		window.removeRemoteConfig();
		return JSON.stringify({ save: save, remove: window.__sent[window.__sent.length - 1], prompt: window.__confirmed[0] || '' });
	})()`)

	for _, want := range []string{
		`\"type\":\"update_remote_config\"`,
		`\"enabled\":true`,
		`\"listen\":\"127.0.0.1:9911\"`, // trimmed
		`\"remove\":true`,
		"No listener will be opened at all",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("listener payloads: missing %s in %s", want, got)
		}
	}
}

// A push-sourced reload must not eat an in-flight edit — the same rule the
// Projects tab follows for its form.
func TestRemoteTab_PushDoesNotClobberAnEdit(t *testing.T) {
	vm := seedRemoteVM(t, enrolProjectsFixture, `[]`, remoteDisabled)
	got := evalString(t, vm, `(function(){
		window.remoteDraftSet('listen', '10.0.0.4:9910');
		window.onSettingsReloaded({
			external_mcps: [], services: [], running_ids: [],
			projects: `+enrolProjectsFixture+`,
			remote: `+remoteEnabled+`
		});
		return JSON.stringify({ draftListen: window.state.remoteDraft.listen, serverListen: window.state.remote.listen });
	})()`)

	if !strings.Contains(got, `"draftListen":"10.0.0.4:9910"`) {
		t.Errorf("a settings push overwrote an uncommitted address: %s", got)
	}
	if !strings.Contains(got, `"serverListen":"127.0.0.1:9910"`) {
		t.Errorf("the pushed server state was not stored: %s", got)
	}
}

// An external change (a CLI `relay enrol` while the window is open) reaches
// the list through the same full-settings push every other tab uses.
func TestRemoteTab_SettingsPushUpdatesEnrolments(t *testing.T) {
	vm := seedRemoteVM(t, enrolProjectsFixture, `[]`, remoteEnabled)
	got := evalString(t, vm, `(function(){
		window.onSettingsReloaded({
			external_mcps: [], services: [], running_ids: [],
			projects: `+enrolProjectsFixture+`,
			enrolments: `+enrolmentsFixture+`,
			remote: `+remoteEnabled+`
		});
		var html = window.renderEnrolments();
		return JSON.stringify({ count: window.state.enrolments.length, showsName: html.indexOf('hermes-mail') >= 0 });
	})()`)
	if !strings.Contains(got, `"count":1`) || !strings.Contains(got, `"showsName":true`) {
		t.Errorf("a full-settings push did not reach the Remote Clients tab: %s", got)
	}
}

// The tab is reachable from the sidebar, and switching to it renders without
// throwing — the same smoke property settings_bundle_test.go pins for the
// other tabs, which is what catches an un-globalized inline handler.
func TestRemoteTab_ShowPageRendersWithoutThrowing(t *testing.T) {
	vm := seedRemoteVM(t, enrolProjectsFixture, enrolmentsFixture, remoteEnabled)
	if got := evalString(t, vm, `(function(){
		window.showPage('remote');
		return window.state.page + ':' + (document.getElementById('content').innerHTML.indexOf('Remote Clients') >= 0);
	})()`); got != "remote:true" {
		t.Errorf("showPage('remote') = %q, want remote:true", got)
	}
}
