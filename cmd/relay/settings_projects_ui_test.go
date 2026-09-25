package main

import (
	"strings"
	"testing"
)

// Subtle: 'selected' is signaled by key presence in disabled_tools, not array
// length — setProjMcpState seeds an empty array on purpose, and projMcpState
// must not mistake that for 'all'.
func TestProjMcpStateThreeStates(t *testing.T) {
	vm := newAppVM(t)
	script := `(function(){
		var wildForm = { allowed_mcp_ids: ['*'], disabled_tools: {} };
		var noneForm = { allowed_mcp_ids: ['b'], disabled_tools: {} };
		var allForm = { allowed_mcp_ids: ['a'], disabled_tools: {} };
		var selectedEmptyForm = { allowed_mcp_ids: ['a'], disabled_tools: { a: [] } };
		var selectedNonEmptyForm = { allowed_mcp_ids: ['a'], disabled_tools: { a: ['tool1'] } };
		return JSON.stringify({
			wild: window.projMcpState(wildForm, 'anything'),
			none: window.projMcpState(noneForm, 'a'),
			allNoKey: window.projMcpState(allForm, 'a'),
			selectedEmptyArray: window.projMcpState(selectedEmptyForm, 'a'),
			selectedNonEmptyArray: window.projMcpState(selectedNonEmptyForm, 'a')
		});
	})()`
	got := evalString(t, vm, script)
	for _, want := range []string{
		`"wild":"all"`,
		`"none":"none"`,
		`"allNoKey":"all"`,
		`"selectedEmptyArray":"selected"`,
		`"selectedNonEmptyArray":"selected"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("projMcpState: missing %s in %s", want, got)
		}
	}
}

func TestSetProjMcpStateSelectedSeedsEmptyArray(t *testing.T) {
	vm := newAppVM(t)
	script := `(function(){
		window.state.externalMcps = [{id:'a'}, {id:'b'}];
		window.newProject();
		window.state.projectForm.allowed_mcp_ids = [];
		window.state.projectForm.disabled_tools = {};
		window.setProjMcpState('a', 'selected');
		var f = window.state.projectForm;
		return JSON.stringify({
			hasKey: Object.prototype.hasOwnProperty.call(f.disabled_tools, 'a'),
			isEmptyArray: Array.isArray(f.disabled_tools.a) && f.disabled_tools.a.length === 0,
			stateReadsSelected: window.projMcpState(f, 'a') === 'selected'
		});
	})()`
	got := evalString(t, vm, script)
	for _, want := range []string{`"hasKey":true`, `"isEmptyArray":true`, `"stateReadsSelected":true`} {
		if !strings.Contains(got, want) {
			t.Errorf("setProjMcpState('selected') sentinel: missing %s in %s", want, got)
		}
	}
}

func TestSetProjMcpStateExpandsWildcard(t *testing.T) {
	vm := newAppVM(t)
	script := `(function(){
		window.state.externalMcps = [{id:'a'}, {id:'b'}, {id:'c'}];
		window.newProject(); // blank form starts wildcard
		var wildBefore = window.isProjMcpWildcard(window.state.projectForm);
		window.setProjMcpState('b', 'none');
		var f = window.state.projectForm;
		return JSON.stringify({
			wildBefore: wildBefore,
			wildAfter: window.isProjMcpWildcard(f),
			ids: f.allowed_mcp_ids.slice().sort(),
			bState: window.projMcpState(f, 'b')
		});
	})()`
	got := evalString(t, vm, script)
	for _, want := range []string{
		`"wildBefore":true`, `"wildAfter":false`, `"ids":["a","c"]`, `"bState":"none"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("wildcard expansion: missing %s in %s", want, got)
		}
	}
}

func TestSetProjMcpStateTransitions(t *testing.T) {
	vm := newAppVM(t)
	script := `(function(){
		window.state.externalMcps = [{id:'a'}];
		window.newProject();
		var f = window.state.projectForm;
		f.allowed_mcp_ids = [];
		f.disabled_tools = {};

		window.setProjMcpState('a', 'all');
		var afterAll = { ids: f.allowed_mcp_ids.slice(), state: window.projMcpState(f, 'a') };

		window.setProjMcpState('a', 'selected');
		var afterSelected = { ids: f.allowed_mcp_ids.slice(), state: window.projMcpState(f, 'a'), hasKey: Object.prototype.hasOwnProperty.call(f.disabled_tools, 'a') };

		window.setProjMcpState('a', 'none');
		var afterNone = { ids: f.allowed_mcp_ids.slice(), state: window.projMcpState(f, 'a'), hasKey: Object.prototype.hasOwnProperty.call(f.disabled_tools, 'a') };

		return JSON.stringify({ afterAll: afterAll, afterSelected: afterSelected, afterNone: afterNone });
	})()`
	got := evalString(t, vm, script)
	for _, want := range []string{
		`"afterAll":{"ids":["a"],"state":"all"}`,
		`"afterSelected":{"ids":["a"],"state":"selected","hasKey":true}`,
		`"afterNone":{"ids":[],"state":"none","hasKey":false}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("setProjMcpState transitions: missing %s in %s", want, got)
		}
	}
}

// toggleProjTool stores a denylist (unchecking adds, checking removes) and
// preserves stale disabled names no longer present in the live tool list.
func TestToggleProjToolDenylistPreservesStale(t *testing.T) {
	vm := newAppVM(t)
	script := `(function(){
		window.state.externalMcps = [{id:'m'}];
		window.newProject();
		var f = window.state.projectForm;
		f.allowed_mcp_ids = ['m'];
		f.disabled_tools = { m: ['stale-tool'] };
		window.state.mcpToolCache = { m: [{name:'a'}, {name:'b'}] };

		// Uncheck live tool 'a' -> denylist gains 'a', keeps preserving 'stale-tool'.
		window.toggleProjTool('m', 'a', false);
		var afterUncheck = f.disabled_tools.m.slice().sort();

		// Re-check 'a' -> denylist loses 'a' but 'stale-tool' survives untouched.
		window.toggleProjTool('m', 'a', true);
		var afterRecheck = f.disabled_tools.m.slice().sort();

		return JSON.stringify({ afterUncheck: afterUncheck, afterRecheck: afterRecheck });
	})()`
	got := evalString(t, vm, script)
	for _, want := range []string{
		`"afterUncheck":["a","stale-tool"]`,
		`"afterRecheck":["stale-tool"]`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("toggleProjTool denylist/stale preservation: missing %s in %s", want, got)
		}
	}
}

func TestPruneStaleDisabledTool(t *testing.T) {
	vm := newAppVM(t)
	script := `(function(){
		window.state.externalMcps = [{id:'m'}];
		window.newProject();
		var f = window.state.projectForm;
		f.allowed_mcp_ids = ['m'];
		f.disabled_tools = { m: ['stale-tool', 'other-stale'] };

		window.pruneStaleDisabledTool('m', 'stale-tool', true); // kept=true -> no-op
		var afterKeep = window.state.projectForm.disabled_tools.m.slice().sort();

		window.pruneStaleDisabledTool('m', 'stale-tool', false); // kept=false -> removed
		var afterPrune = window.state.projectForm.disabled_tools.m.slice().sort();

		return JSON.stringify({ afterKeep: afterKeep, afterPrune: afterPrune });
	})()`
	got := evalString(t, vm, script)
	for _, want := range []string{
		`"afterKeep":["other-stale","stale-tool"]`,
		`"afterPrune":["other-stale"]`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("pruneStaleDisabledTool: missing %s in %s", want, got)
		}
	}
}

// Deliberate: harvestProjectForm omits chat_templates — Eve owns editing them,
// and update_project treats an absent field as "leave unchanged".
func TestHarvestProjectFormPayloadShape(t *testing.T) {
	vm := newAppVM(t)
	script := `(function(){
		window.state.projects = [{
			id: 'p1', name: 'Proj', path: '/tmp/proj',
			allowed_mcp_ids: ['a', 'b'], allowed_models: ['*'],
			disabled_tools: { a: ['x'] },
			generate_skill: true,
			chat_templates: [{id:'t1', name:'T', model:'claude-sonnet'}]
		}];
		window.editProject('p1');
		var payload = window.harvestProjectForm();
		return JSON.stringify({
			name: payload.name,
			path: payload.path,
			allowedMcpIds: payload.allowed_mcp_ids,
			allowedModels: payload.allowed_models,
			generateSkill: payload.generate_skill,
			disabledTools: payload.disabled_tools,
			omitsChatTemplates: !('chat_templates' in payload)
		});
	})()`
	got := evalString(t, vm, script)
	for _, want := range []string{
		`"name":"Proj"`, `"path":"/tmp/proj"`, `"allowedMcpIds":["a","b"]`,
		`"allowedModels":["*"]`, `"generateSkill":true`,
		`"disabledTools":{"a":["x"]}`, `"omitsChatTemplates":true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("harvestProjectForm payload shape: missing %s in %s", want, got)
		}
	}
}

// Controls must be absent from the rendered HTML, not merely disabled.
func TestRemoteProjectFormHidesHostControls(t *testing.T) {
	vm := newAppVM(t)
	script := `(function(){
		window.state.externalMcps = [{id:'a', display_name:'Alpha'}];
		window.state.projects = [{
			id: 'p1', name: 'Remote One', kind: 'remote', path: '',
			allowed_mcp_ids: [], allowed_models: [], disabled_tools: {}
		}];
		window.editProject('p1');
		var editHtml = window.renderProjectForm();

		window.newProject();
		window.setProjKind('remote');
		var newHtml = window.renderProjectForm();

		return JSON.stringify({
			editNoPathInput: editHtml.indexOf('id="projPath"') < 0,
			editNoDirAuth: editHtml.indexOf('Directory Auth') < 0,
			editNoSkillSection: editHtml.indexOf('Auto-generate SKILL.md') < 0,
			editNoMcpWildcard: editHtml.indexOf('Allow all registered MCPs') < 0,
			editNoModelsWildcard: editHtml.indexOf('Allow all models') < 0,
			editHasNote: editHtml.indexOf('no host directory') >= 0,
			editShowsKindLabel: editHtml.indexOf('Remote') >= 0,
			newNoPathInput: newHtml.indexOf('id="projPath"') < 0,
			newNoDirAuth: newHtml.indexOf('Directory Auth') < 0,
			newNoSkillSection: newHtml.indexOf('Auto-generate SKILL.md') < 0,
			newNoMcpWildcard: newHtml.indexOf('Allow all registered MCPs') < 0,
			newNoModelsWildcard: newHtml.indexOf('Allow all models') < 0,
			newHasKindSelector: newHtml.indexOf("setProjKind('local')") >= 0 && newHtml.indexOf("setProjKind('remote')") >= 0
		});
	})()`
	got := evalString(t, vm, script)
	for _, want := range []string{
		`"editNoPathInput":true`, `"editNoDirAuth":true`, `"editNoSkillSection":true`,
		`"editNoMcpWildcard":true`, `"editNoModelsWildcard":true`, `"editHasNote":true`,
		`"editShowsKindLabel":true`,
		`"newNoPathInput":true`, `"newNoDirAuth":true`, `"newNoSkillSection":true`,
		`"newNoMcpWildcard":true`, `"newNoModelsWildcard":true`, `"newHasKindSelector":true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("remote form hides host controls: missing %s in %s", want, got)
		}
	}
}

// Remote + zero MCP grants must harvest with no path key at all (an absent
// key, not an empty string) but an empty (not missing) allowed_mcp_ids.
func TestRemoteProjectZeroMcpHarvest(t *testing.T) {
	vm := newAppVM(t)
	script := `(function(){
		window.state.projects = [{
			id: 'p2', name: 'Remote Zero', kind: 'remote', path: '',
			allowed_mcp_ids: [], allowed_models: [], disabled_tools: {},
			generate_skill: false
		}];
		window.editProject('p2');
		var payload = window.harvestProjectForm();

		window.editProject('p2'); // saveProjectForm mutates state; re-select
		window.saveProjectForm();

		return JSON.stringify({
			kind: payload.kind,
			hasPathKey: ('path' in payload),
			allowedModels: payload.allowed_models,
			allowedMcpIds: payload.allowed_mcp_ids,
			generateSkill: payload.generate_skill,
			formErrorAfterSave: window.state.projectFormError
		});
	})()`
	got := evalString(t, vm, script)
	for _, want := range []string{
		`"kind":"remote"`, `"hasPathKey":false`, `"allowedModels":[]`, `"allowedMcpIds":[]`,
		`"generateSkill":false`, `"formErrorAfterSave":null`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("remote zero-MCP harvest: missing %s in %s", want, got)
		}
	}
}

// Switching to remote must clear the "*" wildcard — project.ValidateShape
// refuses "*" on a remote project.
func TestSwitchLocalToRemoteClearsWildcard(t *testing.T) {
	vm := newAppVM(t)
	script := `(function(){
		window.newProject();
		var wildBefore = window.isProjMcpWildcard(window.state.projectForm);
		window.setProjKind('remote');
		var f = window.state.projectForm;
		return JSON.stringify({
			wildBefore: wildBefore,
			kindAfter: f.kind,
			idsAfter: f.allowed_mcp_ids,
			modelsAfter: f.allowed_models
		});
	})()`
	got := evalString(t, vm, script)
	for _, want := range []string{
		`"wildBefore":true`, `"kindAfter":"remote"`, `"idsAfter":[]`, `"modelsAfter":[]`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("switch local->remote wildcard clear: missing %s in %s", want, got)
		}
	}
}

func TestLocalProjectFormUnchanged(t *testing.T) {
	vm := newAppVM(t)
	script := `(function(){
		window.state.externalMcps = [{id:'a', display_name:'Alpha'}];
		window.state.projects = [{
			id: 'p1', name: 'Local One', path: '/tmp/local', kind: 'local',
			allowed_mcp_ids: ['*'], allowed_models: ['*'], disabled_tools: {},
			chat_templates: [], permission_policy: { default_mode: '', allowed_tools: [], denied_tools: [] },
			generate_skill: false
		}];
		window.editProject('p1');
		var editHtml = window.renderProjectForm();

		window.newProject();
		var newHtml = window.renderProjectForm();
		var newFormDefaultsLocal = window.state.projectForm.kind === 'local';

		return JSON.stringify({
			editHasPathInput: editHtml.indexOf('id="projPath"') >= 0,
			editHasMcpWildcard: editHtml.indexOf('Allow all registered MCPs') >= 0,
			editHasModelsSection: editHtml.indexOf('Allowed Models') >= 0,
			editHasModelsWildcard: editHtml.indexOf('Allow all models') >= 0,
			editHasChatTemplates: editHtml.indexOf('Chat Templates') >= 0,
			editHasPermissionPolicy: editHtml.indexOf('Permission Policy') >= 0,
			editHasSkillSection: editHtml.indexOf('Skill (CLAUDE.md') >= 0,
			editHasTokenSection: editHtml.indexOf('Bearer Token') >= 0,
			editHasKindLabel: editHtml.indexOf('Local') >= 0,
			editNoRemoteNote: editHtml.indexOf('no host directory') < 0,
			newFormDefaultsLocal: newFormDefaultsLocal,
			newHasPathInput: newHtml.indexOf('id="projPath"') >= 0,
			newHasMcpWildcard: newHtml.indexOf('Allow all registered MCPs') >= 0,
			newHasModelsWildcard: newHtml.indexOf('Allow all models') >= 0,
			newHasSkillSection: newHtml.indexOf('Skill (CLAUDE.md') >= 0,
			newHasKindSelector: newHtml.indexOf("setProjKind('local')") >= 0 && newHtml.indexOf("setProjKind('remote')") >= 0
		});
	})()`
	got := evalString(t, vm, script)
	for _, want := range []string{
		`"editHasPathInput":true`, `"editHasMcpWildcard":true`, `"editHasModelsSection":true`,
		`"editHasModelsWildcard":true`, `"editHasChatTemplates":true`, `"editHasPermissionPolicy":true`,
		`"editHasSkillSection":true`, `"editHasTokenSection":true`,
		`"editHasKindLabel":true`, `"editNoRemoteNote":true`,
		`"newFormDefaultsLocal":true`, `"newHasPathInput":true`, `"newHasMcpWildcard":true`,
		`"newHasModelsWildcard":true`, `"newHasSkillSection":true`,
		`"newHasKindSelector":true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("local project form regression: missing %s in %s", want, got)
		}
	}
}

// No bare MCP count: "MCPs: 1" beside a client that can read every mailbox is
// exactly the disclosure ADR-011 forbids.
func TestProjectListBadgesRemote(t *testing.T) {
	vm := newAppVM(t)
	script := `(function(){
		window.state.projects = [
			{id:'r1', name:'Remote Proj', kind:'remote', path:'', allowed_mcp_ids:[], allowed_models:[]},
			{id:'l1', name:'Local Proj', kind:'local', path:'/tmp/l', allowed_mcp_ids:['*'], allowed_models:['*']},
			{id:'l2', name:'Local Zero', kind:'local', path:'/tmp/z', allowed_mcp_ids:[], allowed_models:[]}
		];
		window.state.editingProjectId = null;
		var html = window.renderProjects();
		var badgeCount = (html.match(/proj-badge-remote/g) || []).length;
		return JSON.stringify({
			badgeCount: badgeCount,
			remoteSaysProfileReachesNothing: html.indexOf('this access profile reaches nothing') >= 0,
			localSaysProjectReachesNothing: html.indexOf('this project reaches nothing') >= 0,
			noBareMcpCount: !/MCPs: <strong>/.test(html)
		});
	})()`
	got := evalString(t, vm, script)
	for _, want := range []string{
		`"badgeCount":1`, `"remoteSaysProfileReachesNothing":true`,
		`"localSaysProjectReachesNothing":true`, `"noBareMcpCount":true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("project list remote badge: missing %s in %s", want, got)
		}
	}
}

func TestGrantingAnMcpDoesNotEatTheTypedName(t *testing.T) {
	vm := newAppVM(t)
	script := `(function(){
		window.state.page = 'projects';
		window.state.externalMcps = [{id:'macmcp', display_name:'macMCP'}];
		window.newProject();
		window.setProjKind('remote');
		document.getElementById('projName').value = 'Hermes — Bob INBOX';
		window.setProjMcpGranted('macmcp', true);
		return JSON.stringify({
			stateName: window.state.projectForm.name,
			htmlHasValue: window.renderProjectForm().indexOf('value="Hermes — Bob INBOX"') >= 0,
			granted: window.state.projectForm.allowed_mcp_ids.indexOf('macmcp') >= 0
		});
	})()`
	got := evalString(t, vm, script)
	for _, want := range []string{
		`"stateName":"Hermes — Bob INBOX"`,
		`"htmlHasValue":true`,
		`"granted":true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("granting an MCP lost the typed name: missing %s in %s", want, got)
		}
	}
}

func TestSaveProjectForm_RefusedCreateNamesTheFieldAndFocusesIt(t *testing.T) {
	vm := newAppVM(t)
	script := `(function(){
		window.__sent = [];
		window.webkit = { messageHandlers: { ipc: { postMessage: function(m){ window.__sent.push(String(m)); } } } };
		window.state.page = 'projects';
		window.newProject();
		// Opening the form requests the model catalog; only the save must send nothing.
		window.__sent = [];
		document.getElementById('projName').value = '';
		document.getElementById('projPath').value = '/tmp/whatever';
		var focused = null;
		document.getElementById('projName').focus = function(){ focused = 'projName'; };
		window.saveProjectForm();
		var html = window.renderProjectForm();
		return JSON.stringify({
			errorField: window.state.projectFormErrorField,
			errorText: window.state.projectFormError,
			htmlHasFieldError: html.indexOf('proj-field-error') >= 0,
			htmlHasInvalidClass: html.indexOf('proj-field-invalid') >= 0,
			focused: focused,
			sentAnything: window.__sent.length > 0
		});
	})()`
	got := evalString(t, vm, script)
	for _, want := range []string{
		`"errorField":"projName"`,
		`"errorText":"Project name is required"`,
		`"htmlHasFieldError":true`,
		`"htmlHasInvalidClass":true`,
		`"focused":"projName"`,
		`"sentAnything":false`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("refused create did not name+focus the field: missing %s in %s", want, got)
		}
	}
}

func TestOnProjectError_SurfacesWhileTheFormIsStillOpen(t *testing.T) {
	vm := newAppVM(t)
	script := `(function(){
		window.state.page = 'projects';
		window.newProject();
		var before = document.getElementById('content').innerHTML;
		window.onProjectError('Access for macmcp must be read or write, not wrIte');
		var after = document.getElementById('content').innerHTML;
		return JSON.stringify({
			stillEditing: window.state.editingProjectId !== null,
			repainted: before !== after,
			bannerAppeared: after.indexOf('Access for macmcp must be read or write, not wrIte') >= 0
		});
	})()`
	got := evalString(t, vm, script)
	for _, want := range []string{
		`"stillEditing":true`,
		`"repainted":true`,
		`"bannerAppeared":true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("server-side refusal did not surface while the form was open: missing %s in %s", want, got)
		}
	}
}
