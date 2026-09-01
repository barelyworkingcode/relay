package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/project"
	"github.com/dop251/goja"
)

const scopeFieldsFixture = `{
	macmcp: [
		{name:'mail_accounts', type:'array', item_type:'string', description:'Mail accounts this client may read from or send as', source:'operator', applies_to:['mail_*'], enumerable:true},
		{name:'mail_mailboxes', type:'array', item_type:'string', description:'Mailbox paths within those accounts this client may reach', source:'operator', applies_to:['mail_*'], enumerable:true, depends_on:['mail_accounts']},
		{name:'file_dirs', type:'array', item_type:'string', description:'Directories this client may write files into', source:'project_path', applies_to:['mail_save_attachment','mail_get_source']}
	],
	quietmcp: []
}`

const scopeMcpsFixture = `[{id:'macmcp', display_name:'macMCP'}, {id:'quietmcp', display_name:'Quiet MCP'}]`

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

const scopeProjectsFixture = `[
	{id:'p_bob', name:'Hermes — Bob INBOX', kind:'remote', path:'', allowed_mcp_ids:['macmcp'], allowed_models:[],
	 allowed_tools:{macmcp:['mail_*']}, access:{macmcp:'read'},
	 context:{macmcp:{mail_accounts:['Bob'], mail_mailboxes:['INBOX']}}, disabled_tools:{}},
	{id:'p_bare', name:'Hermes Mail', kind:'remote', path:'', allowed_mcp_ids:['macmcp'], allowed_models:[], disabled_tools:{}},
	{id:'p_local', name:'Workspace', path:'/Users/x/work', allowed_mcp_ids:['macmcp'], allowed_models:['*'],
	 context:{macmcp:{file_dirs:['/Users/x/work'], mail_accounts:['Alice']}}, disabled_tools:{}}
]`

func TestProjectList_ShowsEffectiveAuthority(t *testing.T) {
	vm := seedScopeVM(t, scopeProjectsFixture, "")
	html := evalString(t, vm, `window.renderProjects()`)

	for _, want := range []string{
		`>read<`,
		`mail_*`,
		"mail_accounts: Bob",
		"mail_mailboxes: INBOX",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("list row does not show %q\n%s", want, html)
		}
	}
	if !strings.Contains(html, "no tools") {
		t.Error("a profile with no allowed_tools does not say it holds none")
	}
	if !strings.Contains(html, `>write<`) {
		t.Error("the local project's default write mode is not shown")
	}
}

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
	if !strings.Contains(html, "needs a scope value for") {
		t.Error("the incomplete row itself carries no warning")
	}
	gaps := evalString(t, vm, `JSON.stringify(window.projScopeGaps(window.state.projects[0]))`)
	if gaps != "[]" {
		t.Errorf("a fully-scoped profile was reported as incomplete: %s", gaps)
	}
	local := evalString(t, vm, `JSON.stringify(window.projScopeGaps(window.state.projects[2]))`)
	if !strings.Contains(local, "mail_mailboxes") {
		t.Errorf("a local project missing a scope value was not reported: %s", local)
	}
}

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
		"mail_accounts",
		"Mail accounts this client may read from or send as",
		"list of strings, one per line",
		`toggleScopeFieldPicker('macmcp', 'mail_accounts')`,
		"Values here are read within mail_accounts",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("permission panel is missing %q\n%s", want, html)
		}
	}
	if !strings.Contains(html, `<div class="proj-scope-summary">Bob</div>`) ||
		!strings.Contains(html, `<div class="proj-scope-summary">INBOX</div>`) {
		t.Errorf("stored scope values are not shown on the closed panel\n%s", html)
	}
	sent := evalString(t, vm, `JSON.stringify(window.__sent)`)
	if strings.Contains(sent, "enumerate_scope_field") {
		t.Errorf("rendering the editor fired a live enumeration: %s", sent)
	}
}

func TestProjectForm_ProjectPathFieldIsReadOnly(t *testing.T) {
	vm := seedScopeVM(t, scopeProjectsFixture, "p_local")
	html := evalString(t, vm, `window.renderProjectForm()`)
	if !strings.Contains(html, `readonly value="/Users/x/work"`) {
		t.Errorf("the derived file_dirs value is not shown read-only\n%s", html)
	}
	if strings.Contains(html, `setProjScopeText('macmcp', 'file_dirs'`) {
		t.Error("a derived field was rendered as an editable input")
	}

	vm = seedScopeVM(t, scopeProjectsFixture, "p_bob")
	html = evalString(t, vm, `window.renderProjectForm()`)
	for _, want := range []string{"no host directory", "mail_save_attachment", "is refused"} {
		if !strings.Contains(html, want) {
			t.Errorf("the profile's derived field does not explain itself: missing %q\n%s", want, html)
		}
	}
}

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
		// allow_external is a pointer on the update DTO: an omitted map means
		// "no change", so it must be sent even when it grants nothing.
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

// context is a pointer on the update DTO: an omitted map means "no change",
// so a cleared value must still be sent rather than dropped from the payload.
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
		"Hermes — Bob INBOX",
		`>read<`,
		"mail_accounts: Bob",
		"no tools",
		"needs a scope value for",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("enrolment card does not answer 'what can this client do': missing %q\n%s", want, html)
		}
	}
}

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

	var create project.CreateFields
	if err := json.Unmarshal([]byte(raw), &create); err != nil {
		t.Fatalf("editor payload did not decode as a create: %v\n%s", err, raw)
	}
	if create.Access["macmcp"] != config.AccessRead {
		t.Errorf(`"access" did not reach project.CreateFields.Access — check the json tag: %#v`, create.Access)
	}
	if len(create.AllowedTools["macmcp"]) != 1 {
		t.Errorf(`"allowed_tools" did not reach the DTO: %#v`, create.AllowedTools)
	}
	if !strings.Contains(string(create.Context["macmcp"]), "INBOX") {
		t.Errorf(`"context" did not reach the DTO: %s`, create.Context["macmcp"])
	}

	var update project.UpdateFields
	if err := json.Unmarshal([]byte(raw), &update); err != nil {
		t.Fatalf("editor payload did not decode as an update: %v", err)
	}
	if update.Access == nil || update.Context == nil || update.AllowedTools == nil {
		t.Fatalf("a pointer field stayed nil, which the update path reads as 'no change': %#v", update)
	}

	s := &config.Settings{Version: 1}
	created, err := project.ApplyCreate(s, create, v2Surfaces())
	if err != nil {
		t.Fatalf("the editor's own payload was refused by project.ApplyCreate: %v", err)
	}
	if created.Access["macmcp"] != config.AccessRead || !strings.Contains(string(created.Context["macmcp"]), "Bob") {
		t.Errorf("the permission set did not survive the create: %#v", created)
	}
}

func TestProjectForm_OutboundGrantIsItsOwnControlAndSaysWhatItDoes(t *testing.T) {
	vm := seedScopeVM(t, scopeProjectsFixture, "p_bob")
	html := evalString(t, vm, `window.renderProjectForm()`)

	for _, want := range []string{
		"Outside this Mac",
		`setProjAllowExternal('macmcp', false)`,
		`setProjAllowExternal('macmcp', true)`,
		"mail_send",
		"web_fetch",
		"Drafting still works",
		"mail_create_draft",
		"nothing is delivered",
		"does <em>not</em> stay on this Mac",
		"separate</em> question from Read/Write",
		"openWorldHint",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the outbound control is missing %q\n%s", want, html)
		}
	}
	if !strings.Contains(html, "Unset defaults to <strong>refused</strong> for an access profile") {
		t.Errorf("the panel does not name the profile default\n%s", html)
	}
	if !strings.Contains(html, "no way off this Mac except through relay") {
		t.Error("the panel asserts the profile default without the reason for it")
	}
	vm = seedScopeVM(t, scopeProjectsFixture, "p_local")
	local := evalString(t, vm, `window.renderProjectForm()`)
	if !strings.Contains(local, "Unset defaults to <strong>allowed</strong> for a local project") {
		t.Errorf("the panel does not name the local default\n%s", local)
	}
	if !strings.Contains(local, "already has this Mac") {
		t.Error("the local default is asserted without the reason for it")
	}
	if !strings.Contains(local, `setProjAllowExternal('macmcp', false)`) {
		t.Error("a local project cannot refuse its own outbound channel from the editor")
	}
}

// Agreeing with the kind's default deletes the key rather than writing it, so
// a local project's Allow cannot survive a later conversion into an access
// profile and hand the converted profile a channel nobody granted it.
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

func TestProjectList_ShowsTheOutboundGrantOnTheRow(t *testing.T) {
	vm := seedScopeVM(t, scopeProjectsFixture, "")
	html := evalString(t, vm, `window.renderProjects()`)
	if n := strings.Count(html, `<span class="proj-auth-external off">local only</span>`); n != 2 {
		t.Errorf("want both profiles marked local only, got %d\n%s", n, html)
	}
	if n := strings.Count(html, `<span class="proj-auth-external on">may reach outside</span>`); n != 1 {
		t.Errorf("want the local project marked as reaching outside, got %d", n)
	}
	vm = seedScopeVM(t, `[{id:'p_out', name:'Outbound', kind:'remote', path:'', allowed_mcp_ids:['macmcp'],
		allowed_models:[], allowed_tools:{macmcp:['mail_*']}, access:{macmcp:'write'},
		allow_external:{macmcp:true},
		context:{macmcp:{mail_accounts:['Bob'], mail_mailboxes:['INBOX']}}, disabled_tools:{}}]`, "")
	html = evalString(t, vm, `window.renderProjects()`)
	if !strings.Contains(html, `<span class="proj-auth-external on">may reach outside</span>`) {
		t.Errorf("a row does not show an outbound grant that is in force\n%s", html)
	}
}

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

	// Ungranting the MCP drops its outbound grant too: a grant for an MCP the
	// record no longer reaches would read as an authority it does not have.
	dropped := evalString(t, vm, `(function(){
		window.setProjAllowExternal('macmcp', true);
		window.setProjMcpGranted('macmcp', false);
		return JSON.stringify(window.state.projectForm.allow_external);
	})()`)
	if dropped != "{}" {
		t.Errorf("the outbound grant outlived the MCP grant in the form: %s", dropped)
	}
}
