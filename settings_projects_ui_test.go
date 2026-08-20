package main

// Characterization + regression tests for the Projects tab's tri-state MCP
// picker and (later in this file) the remote-project form behavior added on
// top of it. Part 1 (characterization) pins CURRENT behavior of projMcpState,
// setProjMcpState, toggleProjTool, pruneStaleDisabledTool, and
// harvestProjectForm's payload shape BEFORE the remote-kind changes land, so a
// regression in the surrounding refactor shows up here. Part 2 covers the new
// remote-project behavior itself. Uses the same goja harness as
// settings_bundle_test.go (newAppVM, evalString, domShim).

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Part 1 — characterization tests (pin behavior that predates this change)
// ---------------------------------------------------------------------------

// TestProjMcpStateThreeStates pins how projMcpState derives each of its three
// states. The subtle one: 'selected' is signalled by KEY PRESENCE in
// disabled_tools (even an empty array), not by array length — setProjMcpState
// deliberately seeds disabled_tools[mcpID] = [] when the user picks "Selected"
// before unchecking anything, and projMcpState must still read that back as
// 'selected' rather than falling through to 'all'.
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

// TestSetProjMcpStateSelectedSeedsEmptyArray pins the sentinel behavior
// explicitly: clicking "Selected" writes an EMPTY array into disabled_tools,
// not no key at all — because key presence (not length) is what projMcpState
// reads. If this ever regressed to skip seeding, the UI would render as "All
// tools" immediately after the user clicked "Selected".
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

// TestSetProjMcpStateExpandsWildcard pins that the FIRST per-MCP edit off of
// the wildcard state expands allowed_mcp_ids into the concrete registered MCP
// ID set (and resets disabled_tools) before applying the requested state.
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

// TestSetProjMcpStateTransitions pins the 'all' / 'selected' / 'none' button
// transitions on an already-expanded (non-wildcard) form.
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

// TestToggleProjToolDenylistPreservesStale pins two things about
// toggleProjTool: (1) it stores a DENYLIST — unchecking a tool adds it to
// disabled_tools[mcpID], checking it removes it; (2) it preserves any
// previously-disabled tool names that are no longer present in the live tool
// list (e.g. the MCP was renamed/upgraded), regardless of what's being
// toggled in the same call.
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

// TestPruneStaleDisabledTool pins pruneStaleDisabledTool: kept=true is a
// no-op, kept=false removes exactly that name from the mcp's denylist.
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

// TestHarvestProjectFormPayloadShape pins the current (pre-remote-kind)
// harvestProjectForm payload: it carries the form's identity/grant/policy
// fields, and it deliberately OMITS chat_templates (Eve owns editing them;
// update_project treats an absent field as "leave unchanged"). This must
// still hold after the remote-kind form work — the only additive change
// expected there is a new `kind` field.
func TestHarvestProjectFormPayloadShape(t *testing.T) {
	vm := newAppVM(t)
	script := `(function(){
		window.state.projects = [{
			id: 'p1', name: 'Proj', path: '/tmp/proj',
			allowed_mcp_ids: ['a', 'b'], allowed_models: ['*'],
			disabled_tools: { a: ['x'] },
			generate_skill: true, allow_cwd_auth: true,
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
			allowCwdAuth: payload.allow_cwd_auth,
			disabledTools: payload.disabled_tools,
			omitsChatTemplates: !('chat_templates' in payload)
		});
	})()`
	got := evalString(t, vm, script)
	for _, want := range []string{
		`"name":"Proj"`, `"path":"/tmp/proj"`, `"allowedMcpIds":["a","b"]`,
		`"allowedModels":["*"]`, `"generateSkill":true`, `"allowCwdAuth":true`,
		`"disabledTools":{"a":["x"]}`, `"omitsChatTemplates":true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("harvestProjectForm payload shape: missing %s in %s", want, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Part 3 — new remote-project form behavior
// ---------------------------------------------------------------------------

// TestRemoteProjectFormHidesHostControls covers both the edit form for an
// existing remote project and the new-project form after switching to Remote:
// the path input, Directory Auth section, Generate Skill section, and the "*"
// wildcard toggles (MCPs and models) must all be ABSENT from the rendered
// HTML — not merely disabled — and a short note must explain why.
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

// TestRemoteProjectZeroMcpHarvest covers the valid resting state described in
// project_apply.go: a remote project with zero MCP grants harvests to a
// payload the server will accept — kind "remote", no path key at all, an
// empty allowed_models, and an empty (not missing) allowed_mcp_ids. Also
// checks that saveProjectForm's client-side validation does not block on the
// (now absent) path.
func TestRemoteProjectZeroMcpHarvest(t *testing.T) {
	vm := newAppVM(t)
	script := `(function(){
		window.state.projects = [{
			id: 'p2', name: 'Remote Zero', kind: 'remote', path: '',
			allowed_mcp_ids: [], allowed_models: [], disabled_tools: {},
			generate_skill: false, allow_cwd_auth: false
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
			allowCwdAuth: payload.allow_cwd_auth,
			formErrorAfterSave: window.state.projectFormError
		});
	})()`
	got := evalString(t, vm, script)
	for _, want := range []string{
		`"kind":"remote"`, `"hasPathKey":false`, `"allowedModels":[]`, `"allowedMcpIds":[]`,
		`"generateSkill":false`, `"allowCwdAuth":false`, `"formErrorAfterSave":null`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("remote zero-MCP harvest: missing %s in %s", want, got)
		}
	}
}

// TestSwitchLocalToRemoteClearsWildcard covers the new-project default: the
// blank form starts with the "*" MCP wildcard set (matching today's local
// default), and switching to Remote before first save must clear it to an
// empty explicit list rather than letting Save send a wildcard the server
// will reject (validateProjectShape refuses "*" on a remote project).
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

// TestLocalProjectFormUnchanged is the regression net for the daily-driver
// path: with kind local (the default, both for a brand-new form and an
// existing project), every control that existed before the remote-kind work
// must still render — path input, MCP wildcard toggle, models section +
// wildcard toggle, chat templates, permission policy, skill section,
// directory auth section, and (edit-only) the bearer token section. None of
// the remote-only explanatory text should appear.
func TestLocalProjectFormUnchanged(t *testing.T) {
	vm := newAppVM(t)
	script := `(function(){
		window.state.externalMcps = [{id:'a', display_name:'Alpha'}];
		window.state.projects = [{
			id: 'p1', name: 'Local One', path: '/tmp/local', kind: 'local',
			allowed_mcp_ids: ['*'], allowed_models: ['*'], disabled_tools: {},
			chat_templates: [], permission_policy: { default_mode: '', allowed_tools: [], denied_tools: [] },
			generate_skill: false, allow_cwd_auth: false
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
			editHasDirAuthSection: editHtml.indexOf('Directory Auth') >= 0,
			editHasTokenSection: editHtml.indexOf('Bearer Token') >= 0,
			editHasKindLabel: editHtml.indexOf('Local') >= 0,
			editNoRemoteNote: editHtml.indexOf('no host directory') < 0,
			newFormDefaultsLocal: newFormDefaultsLocal,
			newHasPathInput: newHtml.indexOf('id="projPath"') >= 0,
			newHasMcpWildcard: newHtml.indexOf('Allow all registered MCPs') >= 0,
			newHasModelsWildcard: newHtml.indexOf('Allow all models') >= 0,
			newHasSkillSection: newHtml.indexOf('Skill (CLAUDE.md') >= 0,
			newHasDirAuthSection: newHtml.indexOf('Directory Auth') >= 0,
			newHasKindSelector: newHtml.indexOf("setProjKind('local')") >= 0 && newHtml.indexOf("setProjKind('remote')") >= 0
		});
	})()`
	got := evalString(t, vm, script)
	for _, want := range []string{
		`"editHasPathInput":true`, `"editHasMcpWildcard":true`, `"editHasModelsSection":true`,
		`"editHasModelsWildcard":true`, `"editHasChatTemplates":true`, `"editHasPermissionPolicy":true`,
		`"editHasSkillSection":true`, `"editHasDirAuthSection":true`, `"editHasTokenSection":true`,
		`"editHasKindLabel":true`, `"editNoRemoteNote":true`,
		`"newFormDefaultsLocal":true`, `"newHasPathInput":true`, `"newHasMcpWildcard":true`,
		`"newHasModelsWildcard":true`, `"newHasSkillSection":true`, `"newHasDirAuthSection":true`,
		`"newHasKindSelector":true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("local project form regression: missing %s in %s", want, got)
		}
	}
}

// TestProjectListBadgesRemote covers renderProjects: a remote project gets a
// visible badge and local projects don't. Also pins that a LOCAL project with
// zero explicit MCP grants keeps showing the plain "0" count (unchanged),
// while a REMOTE project with zero grants shows the friendlier "no tools
// granted" instead of looking broken/empty-by-accident.
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
			remoteShowsNoTools: html.indexOf('no tools granted') >= 0,
			localZeroStillShowsPlainZero: /MCPs: <strong>0<\/strong>/.test(html)
		});
	})()`
	got := evalString(t, vm, script)
	for _, want := range []string{
		`"badgeCount":1`, `"remoteShowsNoTools":true`, `"localZeroStillShowsPlainZero":true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("project list remote badge: missing %s in %s", want, got)
		}
	}
}
