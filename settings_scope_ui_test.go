package main

// The operator half of ADR-011, under goja: the per-MCP permission panel, the
// authority summary on a list row, and the "needs a scope value" warning.
//
// These are UI tests because the ADR's second constraint is a UI constraint —
// "an operator must be able to express [a confinement] without it being easy
// to make a mistake", and a configuration UI that is hard to get right defeats
// the security constraint rather than trading against it. The server refuses a
// bad value (project_permissions_test.go); this file is about whether the
// screen tells an operator the truth about what a grant permits.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// macMCP's worked example as the UI receives it: what Go's ScopeFieldView
// projects, with `source` already normalised.
const scopeFieldsFixture = `{
	macmcp: [
		{name:'mail_accounts', type:'array', item_type:'string', description:'Mail accounts this client may read from or send as', source:'operator', applies_to:['mail_*'], enumerable:true},
		{name:'mail_mailboxes', type:'array', item_type:'string', description:'Mailbox paths within those accounts this client may reach', source:'operator', applies_to:['mail_*'], enumerable:true, depends_on:['mail_accounts']},
		{name:'file_dirs', type:'array', item_type:'string', description:'Directories this client may write files into', source:'project_path', applies_to:['mail_save_attachment','mail_get_source']}
	],
	quietmcp: []
}`

const scopeMcpsFixture = `[{id:'macmcp', display_name:'macMCP'}, {id:'quietmcp', display_name:'Quiet MCP'}]`

// seedScopeVM loads the bundle with a live v2 schema and one record open in
// the editor.
func seedScopeVM(t *testing.T, projectsJSON, editingID string) *goja.Runtime {
	t.Helper()
	vm := newAppVM(t)
	script := `(function(){
		window.__sent = [];
		window.webkit = { messageHandlers: { ipc: { postMessage: function(m){ window.__sent.push(String(m)); } } } };
		window.state.page = 'projects';
		window.state.externalMcps = ` + scopeMcpsFixture + `;
		window.state.mcpScopeFields = ` + scopeFieldsFixture + `;
		window.state.projects = ` + projectsJSON + `;
		window.state.editingProjectId = null;
		window.state.projectForm = null;
		` + editingScript(editingID) + `
		return true;
	})()`
	if _, err := vm.RunString(script); err != nil {
		t.Fatalf("seeding scope state: %v", err)
	}
	return vm
}

func editingScript(id string) string {
	if id == "" {
		return ""
	}
	return `window.editProject('` + id + `');`
}

// A profile with a full permission set, and one with none of it — the second
// is what every existing remote grant looks like the moment this ships.
const scopeProjectsFixture = `[
	{id:'p_bob', name:'Hermes — Bob INBOX', kind:'remote', path:'', allowed_mcp_ids:['macmcp'], allowed_models:[],
	 allowed_tools:{macmcp:['mail_*']}, access:{macmcp:'read'},
	 context:{macmcp:{mail_accounts:['Bob'], mail_mailboxes:['INBOX']}}, disabled_tools:{}},
	{id:'p_bare', name:'Hermes Mail', kind:'remote', path:'', allowed_mcp_ids:['macmcp'], allowed_models:[], disabled_tools:{}},
	{id:'p_local', name:'Workspace', path:'/Users/x/work', allowed_mcp_ids:['macmcp'], allowed_models:['*'],
	 context:{macmcp:{file_dirs:['/Users/x/work'], mail_accounts:['Alice']}}, disabled_tools:{}}
]`

// ---------------------------------------------------------------------------
// The list row
// ---------------------------------------------------------------------------

// A row says what the client can DO — mode, tools, scope — not how many MCPs
// it names. "MCPs: 1" beside a client that can read every mailbox on the
// machine is the failure this ADR exists to fix, and it was rendered here.
func TestProjectList_ShowsEffectiveAuthority(t *testing.T) {
	vm := seedScopeVM(t, scopeProjectsFixture, "")
	html := evalString(t, vm, `window.renderProjects()`)

	for _, want := range []string{
		`>read<`,             // the mode, in force
		`mail_*`,             // the tools it may call
		"mail_accounts: Bob", // the resources it may reach
		"mail_mailboxes: INBOX",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("list row does not show %q\n%s", want, html)
		}
	}
	// The bare profile: no allowed_tools means NO tools, and the row says so
	// rather than leaving an empty space that reads as "unrestricted".
	if !strings.Contains(html, "no tools") {
		t.Error("a profile with no allowed_tools does not say it holds none")
	}
	// A local project defaults to write; a profile defaults to read. The
	// asymmetry is the whole of ADR-011 decision 2 and it has to be visible.
	if !strings.Contains(html, `>write<`) {
		t.Error("the local project's default write mode is not shown")
	}
}

// "N profiles need a scope value for macmcp" — the operator-facing half of
// loud-and-closed. The client-facing half is a `denied` at call time, which is
// silent from this side.
func TestProjectList_NamesAMissingScopeValue(t *testing.T) {
	vm := seedScopeVM(t, scopeProjectsFixture, "")
	html := evalString(t, vm, `window.renderProjects()`)

	if !strings.Contains(html, "proj-scope-gap") {
		t.Fatalf("no missing-scope banner at all\n%s", html)
	}
	for _, want := range []string{"Hermes Mail", "macmcp", "mail_accounts", "mail_mailboxes"} {
		if !strings.Contains(html, want) {
			t.Errorf("the banner does not name %q", want)
		}
	}
	// The complete profile is not in the banner, and the incomplete one is
	// marked on its own row too.
	if !strings.Contains(html, "needs a scope value for") {
		t.Error("the incomplete row itself carries no warning")
	}
	gaps := evalString(t, vm, `JSON.stringify(window.projScopeGaps(window.state.projects[0]))`)
	if gaps != "[]" {
		t.Errorf("a fully-scoped profile was reported as incomplete: %s", gaps)
	}
	// A local project missing a value is named too: decision 4 is not
	// remote-only, and the live wildcard "Relay" project is the case.
	local := evalString(t, vm, `JSON.stringify(window.projScopeGaps(window.state.projects[2]))`)
	if !strings.Contains(local, "mail_mailboxes") {
		t.Errorf("a local project missing a scope value was not reported: %s", local)
	}
}

// Regen Skill cannot do anything for a profile — validateProjectShape refuses
// generate_skill on a remote record and the handler refuses a record with no
// path — so the button is absent rather than present and inert. ADR-009
// decision 2's argument applies to the control as much as to the flag.
func TestProjectList_NoRegenSkillButtonForAProfile(t *testing.T) {
	vm := seedScopeVM(t, scopeProjectsFixture, "")
	html := evalString(t, vm, `window.renderProjects()`)
	if n := strings.Count(html, "regenProjectSkill("); n != 1 {
		t.Errorf("want exactly one Regen Skill button (the local project), got %d\n%s", n, html)
	}
	if strings.Contains(html, `regenProjectSkill('p_bob')`) || strings.Contains(html, `regenProjectSkill('p_bare')`) {
		t.Error("a profile was offered a control that cannot do anything")
	}
}

// A remote-kind record is an ACCESS PROFILE everywhere an operator reads it.
func TestProjectList_CallsAProfileAProfile(t *testing.T) {
	vm := seedScopeVM(t, scopeProjectsFixture, "")
	html := evalString(t, vm, `window.renderProjects()`)
	if !strings.Contains(html, "Access profile") {
		t.Error("the badge does not name a remote record an access profile")
	}
	if !strings.Contains(html, "no host directory") {
		t.Error("a profile's row still reads as a project with a missing path")
	}
}

// ---------------------------------------------------------------------------
// The editor
// ---------------------------------------------------------------------------

// The panel is the whole authority in one place: which operations, which
// tools, which resources.
func TestProjectForm_PermissionPanel(t *testing.T) {
	vm := seedScopeVM(t, scopeProjectsFixture, "p_bob")
	html := evalString(t, vm, `window.renderProjectForm()`)

	for _, want := range []string{
		"Operations",
		`setProjAccess('macmcp', 'read')`,
		`setProjAccess('macmcp', 'write')`,
		"Tools",
		`setProjAllowedToolsText('macmcp', this.value)`,
		"Resource scope",
		// Each field, with the MCP's OWN description as help text and its
		// declared type named — an operator typing into a box has to be told
		// what shape belongs in it.
		"mail_accounts",
		"Mail accounts this client may read from or send as",
		"list of strings, one per line",
		// Both mail fields declare enumerable, so both get the picker rather
		// than a box to type a mailbox name into (ADR-011 decision 6).
		`toggleScopeFieldPicker('macmcp', 'mail_accounts')`,
		// The dependency the MCP declared, so an operator knows one field is
		// read within the other.
		"Values here are read within mail_accounts",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("permission panel is missing %q\n%s", want, html)
		}
	}
	// The stored values are on screen WITHOUT anything being opened: what is
	// stored is what confines the client, so it is never behind a disclosure.
	if !strings.Contains(html, `<div class="proj-scope-summary">Bob</div>`) ||
		!strings.Contains(html, `<div class="proj-scope-summary">INBOX</div>`) {
		t.Errorf("stored scope values are not shown on the closed panel\n%s", html)
	}
	// And nothing was enumerated by merely rendering the form: a live call
	// into another process is not something a paint does.
	sent := evalString(t, vm, `JSON.stringify(window.__sent)`)
	if strings.Contains(sent, "enumerate_scope_field") {
		t.Errorf("rendering the editor fired a live enumeration: %s", sent)
	}
}

// A source: "project_path" field is READ-ONLY and shows the derived value: an
// operator has to see that the bound exists without being able to type in it.
func TestProjectForm_ProjectPathFieldIsReadOnly(t *testing.T) {
	// Local: the bound exists and its value is on screen.
	vm := seedScopeVM(t, scopeProjectsFixture, "p_local")
	html := evalString(t, vm, `window.renderProjectForm()`)
	if !strings.Contains(html, `readonly value="/Users/x/work"`) {
		t.Errorf("the derived file_dirs value is not shown read-only\n%s", html)
	}
	if strings.Contains(html, `setProjScopeText('macmcp', 'file_dirs'`) {
		t.Error("a derived field was rendered as an editable input")
	}

	// Profile: there is nothing to derive, and the panel says the tools that
	// field governs are refused — the intended outcome, not a gap to fill in.
	vm = seedScopeVM(t, scopeProjectsFixture, "p_bob")
	html = evalString(t, vm, `window.renderProjectForm()`)
	for _, want := range []string{"no host directory", "mail_save_attachment", "is refused"} {
		if !strings.Contains(html, want) {
			t.Errorf("the profile's derived field does not explain itself: missing %q\n%s", want, html)
		}
	}
}

// A profile gets two grant states, not three: the third is a denylist, and
// validateProjectShape refuses disabled_tools on a remote record outright.
func TestProjectForm_ProfileHasNoDenylistTriState(t *testing.T) {
	vm := seedScopeVM(t, scopeProjectsFixture, "p_bob")
	html := evalString(t, vm, `window.renderProjectForm()`)
	if strings.Contains(html, "Selected</button>") {
		t.Error("a profile was offered the disabled_tools tri-state, which the server refuses")
	}
	for _, want := range []string{"Granted</button>", "Not granted</button>"} {
		if !strings.Contains(html, want) {
			t.Errorf("profile grant control is missing %q", want)
		}
	}
}

// An MCP that declares nothing narrowable says so, and one relay has never
// connected to says something DIFFERENT — "scopes nothing" and "cannot say"
// must not read the same, because only the first means an editor may safely
// offer no fields.
func TestProjectForm_TellsSilenceFromIgnorance(t *testing.T) {
	vm := seedScopeVM(t, `[{id:'p1', name:'P', kind:'remote', path:'', allowed_mcp_ids:['quietmcp','ghostmcp'], allowed_models:[], disabled_tools:{}}]`, "p1")
	vm.RunString(`window.state.externalMcps = [{id:'quietmcp', display_name:'Quiet MCP'}, {id:'ghostmcp', display_name:'Ghost MCP'}];`)
	html := evalString(t, vm, `window.renderProjectForm()`)
	if !strings.Contains(html, "declares nothing narrowable") {
		t.Error("an MCP with no restrict fields does not say so")
	}
	if !strings.Contains(html, "has not connected to this MCP") {
		t.Error("an MCP relay has never seen is not distinguished from one that scopes nothing")
	}
}

// What the editor SENDS. Typed text becomes the three maps relay stores, and a
// derived field is never sent — relay owns it and refuses an operator copy.
func TestProjectForm_HarvestsThePermissionSet(t *testing.T) {
	vm := seedScopeVM(t, scopeProjectsFixture, "p_bob")
	got := evalString(t, vm, `(function(){
		window.setProjAccess('macmcp', 'write');
		window.setProjAllowedToolsText('macmcp', 'mail_*\n mail_send \n\n');
		window.setProjScopeText('macmcp', 'mail_accounts', 'Alice\nBob\n');
		window.setProjScopeText('macmcp', 'mail_mailboxes', '  INBOX  ');
		document.getElementById('projName').value = 'Hermes — Bob INBOX';
		return JSON.stringify(window.harvestProjectForm());
	})()`)

	for _, want := range []string{
		`"access":{"macmcp":"write"}`,
		// Sent even when nothing is granted, because it is a pointer on the
		// update DTO: an omitted map means "no change", so an editor that only
		// sent the grant could never revoke one.
		`"allow_external":{}`,
		`"allowed_tools":{"macmcp":["mail_*","mail_send"]}`,
		`"mail_accounts":["Alice","Bob"]`,
		`"mail_mailboxes":["INBOX"]`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("harvested payload missing %s in %s", want, got)
		}
	}
	if strings.Contains(got, "file_dirs") {
		t.Errorf("the editor sent a field relay derives and refuses: %s", got)
	}
}

// Clearing a scope value SENDS the clearance rather than omitting the field.
// context is a pointer on the update DTO — an omitted map means "no change",
// so an editor that dropped an emptied field could never clear one.
func TestProjectForm_ClearingAScopeValueIsSent(t *testing.T) {
	vm := seedScopeVM(t, scopeProjectsFixture, "p_bob")
	got := evalString(t, vm, `(function(){
		window.setProjScopeText('macmcp', 'mail_accounts', '');
		document.getElementById('projName').value = 'X';
		return JSON.stringify(window.harvestProjectForm());
	})()`)
	if strings.Contains(got, "mail_accounts") {
		t.Errorf("a cleared value was still sent: %s", got)
	}
	if !strings.Contains(got, `"context":{`) {
		t.Errorf("context was omitted entirely, which reads as 'no change': %s", got)
	}
}

// ---------------------------------------------------------------------------
// The Remote Clients tab
// ---------------------------------------------------------------------------

// ADR-011 decision 1: the enrolment listing gains the profile's EFFECTIVE
// authority inline, so "what can this client do" is answered in one place
// without mentally joining two records. That is the real cost of keeping two
// records and it is paid here, in the UI.
func TestRemoteTab_ShowsEffectiveAuthorityInline(t *testing.T) {
	vm := seedRemoteVM(t, scopeProjectsFixture, `[{
		client_id:'hermes-bob', fingerprint:'`+enrolFingerprint+`',
		project_ids:['p_bob','p_bare'], budget:{window_seconds:60,max_calls:60,max_result_bytes:1024},
		created_at:'2026-08-20T09:14:00Z'
	}]`, remoteEnabled)
	if _, err := vm.RunString(`window.state.externalMcps = ` + scopeMcpsFixture + `; window.state.mcpScopeFields = ` + scopeFieldsFixture + `;`); err != nil {
		t.Fatalf("seed schema: %v", err)
	}
	html := evalString(t, vm, `window.renderEnrolments()`)

	for _, want := range []string{
		"Hermes — Bob INBOX", // the profile's name, as before
		`>read<`,             // and now what it permits
		"mail_accounts: Bob",
		"no tools", // the bare profile, which grants nothing
		"needs a scope value for",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("enrolment card does not answer 'what can this client do': missing %q\n%s", want, html)
		}
	}
}

// The editor's payload has to decode into the DTOs the routes and the IPC
// handlers share. Every field on projectUpdateFields is optional and an
// unrecognised key is silently ignored, so a tag mismatch here would look
// exactly like an operator's edit not sticking — which is the failure mode
// this whole change exists to remove. This is TestRemoteProjectPayloadFromSettingsUI
// with the real harvest output rather than a hand-written copy of it.
func TestProjectForm_PayloadDecodesIntoTheSharedDTOs(t *testing.T) {
	vm := seedScopeVM(t, scopeProjectsFixture, "p_bob")
	raw := evalString(t, vm, `(function(){
		window.setProjAccess('macmcp', 'read');
		window.setProjAllowedToolsText('macmcp', 'mail_*');
		window.setProjScopeText('macmcp', 'mail_accounts', 'Bob');
		window.setProjScopeText('macmcp', 'mail_mailboxes', 'INBOX');
		document.getElementById('projName').value = 'Hermes — Bob INBOX';
		return JSON.stringify(window.harvestProjectForm());
	})()`)

	var create projectCreateFields
	if err := json.Unmarshal([]byte(raw), &create); err != nil {
		t.Fatalf("editor payload did not decode as a create: %v\n%s", err, raw)
	}
	if create.Access["macmcp"] != AccessRead {
		t.Errorf(`"access" did not reach projectCreateFields.Access — check the json tag: %#v`, create.Access)
	}
	if len(create.AllowedTools["macmcp"]) != 1 {
		t.Errorf(`"allowed_tools" did not reach the DTO: %#v`, create.AllowedTools)
	}
	if !strings.Contains(string(create.Context["macmcp"]), "INBOX") {
		t.Errorf(`"context" did not reach the DTO: %s`, create.Context["macmcp"])
	}

	var update projectUpdateFields
	if err := json.Unmarshal([]byte(raw), &update); err != nil {
		t.Fatalf("editor payload did not decode as an update: %v", err)
	}
	if update.Access == nil || update.Context == nil || update.AllowedTools == nil {
		t.Fatalf("a pointer field stayed nil, which the update path reads as 'no change': %#v", update)
	}

	// And the round trip: what the editor sends is accepted by the mutator
	// layer both surfaces call.
	s := &Settings{Version: 1}
	created, err := applyProjectCreate(s, create, v2Surfaces())
	if err != nil {
		t.Fatalf("the editor's own payload was refused by applyProjectCreate: %v", err)
	}
	if created.Access["macmcp"] != AccessRead || !strings.Contains(string(created.Context["macmcp"]), "Bob") {
		t.Errorf("the permission set did not survive the create: %#v", created)
	}
}

// ---------------------------------------------------------------------------
// The outbound grant (ADR-011 decision 2c)
// ---------------------------------------------------------------------------

// The control is its own block rather than a third button beside Read/Write,
// because the two axes cross and a control that read as a third mode would
// teach an operator that they are one. The copy has to say what it does in an
// operator's terms — which tools go away, and which keep working.
func TestProjectForm_OutboundGrantIsItsOwnControlAndSaysWhatItDoes(t *testing.T) {
	vm := seedScopeVM(t, scopeProjectsFixture, "p_bob")
	html := evalString(t, vm, `window.renderProjectForm()`)

	for _, want := range []string{
		"Outside this Mac",
		`setProjAllowExternal('macmcp', false)`,
		`setProjAllowExternal('macmcp', true)`,
		// Named examples, because "external access" means nothing on its own.
		"mail_send",
		"web_fetch",
		// The request this feature exists for, in the operator's words —
		// including what drafting does NOT buy, which the copy claimed
		// wrongly until it was measured: Mail IMAP-APPENDs a draft to the
		// account's own server, so it does not stay on this Mac.
		"Drafting still works",
		"mail_create_draft",
		"nothing is delivered",
		"does <em>not</em> stay on this Mac",
		// The orthogonality, said rather than implied.
		"separate</em> question from Read/Write",
		// And the trap: an unannotated tool counts as outbound.
		"openWorldHint",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the outbound control is missing %q\n%s", want, html)
		}
	}
	// A profile's default is refused, and the panel says WHY in the operator's
	// terms rather than asserting it: relay is the only way off this Mac for a
	// client on another machine.
	if !strings.Contains(html, "Unset defaults to <strong>refused</strong> for an access profile") {
		t.Errorf("the panel does not name the profile default\n%s", html)
	}
	if !strings.Contains(html, "no way off this Mac except through relay") {
		t.Error("the panel asserts the profile default without the reason for it")
	}
	// A local project's default is the opposite, and the panel says why that
	// too — the same asymmetry Operations has, from the same threat model.
	vm = seedScopeVM(t, scopeProjectsFixture, "p_local")
	local := evalString(t, vm, `window.renderProjectForm()`)
	if !strings.Contains(local, "Unset defaults to <strong>allowed</strong> for a local project") {
		t.Errorf("the panel does not name the local default\n%s", local)
	}
	if !strings.Contains(local, "already has this Mac") {
		t.Error("the local default is asserted without the reason for it")
	}
	// And it is still refusable there, which is the case the default is wrong
	// for: a confined local agent with no other way out.
	if !strings.Contains(local, `setProjAllowExternal('macmcp', false)`) {
		t.Error("a local project cannot refuse its own outbound channel from the editor")
	}
}

// Only DISSENT from the kind's default is stored. A click that agrees with the
// default deletes the key rather than writing it, so a local project's Allow
// cannot survive a later conversion into an access profile and hand the
// converted profile a channel nobody granted it.
func TestProjectForm_OutboundGrantStoresOnlyDissent(t *testing.T) {
	vm := seedScopeVM(t, scopeProjectsFixture, "p_local")
	got := evalString(t, vm, `(function(){
		window.setProjAllowExternal('macmcp', true);   // the local default
		return JSON.stringify(window.state.projectForm.allow_external);
	})()`)
	if got != "{}" {
		t.Errorf("agreeing with the local default wrote an explicit value: %s", got)
	}
	got = evalString(t, vm, `(function(){
		window.setProjAllowExternal('macmcp', false);  // dissent
		return JSON.stringify(window.state.projectForm.allow_external);
	})()`)
	if got != `{"macmcp":false}` {
		t.Errorf("a local project's refusal was not stored: %s", got)
	}

	// The mirror on a profile: Refuse is the default and writes nothing, Allow
	// is the dissent and is stored.
	vm = seedScopeVM(t, scopeProjectsFixture, "p_bob")
	got = evalString(t, vm, `(function(){
		window.setProjAllowExternal('macmcp', false);
		return JSON.stringify(window.state.projectForm.allow_external);
	})()`)
	if got != "{}" {
		t.Errorf("agreeing with the profile default wrote an explicit value: %s", got)
	}
	got = evalString(t, vm, `(function(){
		window.setProjAllowExternal('macmcp', true);
		return JSON.stringify(window.state.projectForm.allow_external);
	})()`)
	if got != `{"macmcp":true}` {
		t.Errorf("a profile's outbound grant was not stored: %s", got)
	}
}

// A list row answers "what can this client do", and a row that showed only the
// mode would answer it with the axis that does not mention the network.
func TestProjectList_ShowsTheOutboundGrantOnTheRow(t *testing.T) {
	vm := seedScopeVM(t, scopeProjectsFixture, "")
	html := evalString(t, vm, `window.renderProjects()`)
	// The two profiles say "local only" by default; the local project says the
	// opposite by default. Both defaults are visible on the list, which is the
	// point — a row that showed only the mode would answer "what can this
	// client do" with the axis that does not mention the network.
	if n := strings.Count(html, `<span class="proj-auth-external off">local only</span>`); n != 2 {
		t.Errorf("want both profiles marked local only, got %d\n%s", n, html)
	}
	if n := strings.Count(html, `<span class="proj-auth-external on">may reach outside</span>`); n != 1 {
		t.Errorf("want the local project marked as reaching outside, got %d", n)
	}
	// And a PROFILE that has been granted one says so, in the louder style.
	vm = seedScopeVM(t, `[{id:'p_out', name:'Outbound', kind:'remote', path:'', allowed_mcp_ids:['macmcp'],
		allowed_models:[], allowed_tools:{macmcp:['mail_*']}, access:{macmcp:'write'},
		allow_external:{macmcp:true},
		context:{macmcp:{mail_accounts:['Bob'], mail_mailboxes:['INBOX']}}, disabled_tools:{}}]`, "")
	html = evalString(t, vm, `window.renderProjects()`)
	if !strings.Contains(html, `<span class="proj-auth-external on">may reach outside</span>`) {
		t.Errorf("a row does not show an outbound grant that is in force\n%s", html)
	}
}

// What the editor sends when the grant is turned on, and — the direction that
// matters — when it is turned back off.
func TestProjectForm_HarvestsTheOutboundGrantBothWays(t *testing.T) {
	vm := seedScopeVM(t, scopeProjectsFixture, "p_bob")
	got := evalString(t, vm, `(function(){
		window.setProjAllowExternal('macmcp', true);
		document.getElementById('projName').value = 'X';
		return JSON.stringify(window.harvestProjectForm());
	})()`)
	if !strings.Contains(got, `"allow_external":{"macmcp":true}`) {
		t.Errorf("the granted channel was not sent: %s", got)
	}

	got = evalString(t, vm, `(function(){
		window.setProjAllowExternal('macmcp', false);
		document.getElementById('projName').value = 'X';
		return JSON.stringify(window.harvestProjectForm());
	})()`)
	if !strings.Contains(got, `"allow_external":{}`) {
		t.Errorf("revoking the channel did not reach the wire: %s", got)
	}
	if strings.Contains(got, `"macmcp":false`) {
		t.Errorf("a false was sent where an absent key is the same state: %s", got)
	}

	// Ungranting the MCP drops it with everything else: a grant for an MCP the
	// record no longer reaches reads as an authority it does not have.
	dropped := evalString(t, vm, `(function(){
		window.setProjAllowExternal('macmcp', true);
		window.setProjMcpGranted('macmcp', false);
		return JSON.stringify(window.state.projectForm.allow_external);
	})()`)
	if dropped != "{}" {
		t.Errorf("the outbound grant outlived the MCP grant in the form: %s", dropped)
	}
}
