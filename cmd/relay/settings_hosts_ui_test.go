package main

import (
	"strings"
	"testing"
)

// TestHostsTab_RendersRows is the required smoke test for the whole tab:
// name, monospace target, a status word and the probe summary line all show
// up for each row, and the empty state offers an explicit way in.
func TestHostsTab_RendersRows(t *testing.T) {
	vm := newAppVM(t)
	script := `(function(){
		window.state.hosts = [
			{id:'h1', name:'devbox', target:'admin@devbox.local', port:0, status:'connected',
			 probe:{ok:true, os:'Darwin', arch:'arm64', node_path:'/opt/homebrew/bin/node', node_version:'v24.7.0',
			         claude_path:'/opt/homebrew/bin/claude', claude_version:'2.1.258'}},
			{id:'h2', name:'ci-box', target:'ci@ci.example', port:2222, status:'unreachable',
			 probe:{ok:false, error:'Connection refused'}}
		];
		var html = window.renderHosts();
		return JSON.stringify({
			showsName1: html.indexOf('devbox') >= 0,
			showsTarget1: html.indexOf('admin@devbox.local') >= 0,
			showsStatus1: html.indexOf('connected') >= 0,
			showsSummary1: html.indexOf('Darwin') >= 0 && html.indexOf('node v24.7.0') >= 0 && html.indexOf('claude 2.1.258') >= 0,
			showsName2: html.indexOf('ci-box') >= 0,
			showsPort2: html.indexOf('ci@ci.example:2222') >= 0,
			showsStatus2: html.indexOf('unreachable') >= 0,
			showsError2: html.indexOf('Connection refused') >= 0,
			showsDisconnectFor1: /disconnectHost\('h1'\)/.test(html),
			noDisconnectFor2: !/disconnectHost\('h2'\)/.test(html)
		});
	})()`
	got := evalString(t, vm, script)
	for _, want := range []string{
		`"showsName1":true`, `"showsTarget1":true`, `"showsStatus1":true`, `"showsSummary1":true`,
		`"showsName2":true`, `"showsPort2":true`, `"showsStatus2":true`, `"showsError2":true`,
		`"showsDisconnectFor1":true`, `"noDisconnectFor2":true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hosts tab rows: missing %s in %s", want, got)
		}
	}
}

func TestHostsTab_EmptyState(t *testing.T) {
	vm := newAppVM(t)
	script := `(function(){
		window.state.hosts = [];
		var html = window.renderHosts();
		return JSON.stringify({
			sentence: html.indexOf('No hosts yet. A host is a machine you reach over ssh; projects can live on one.') >= 0,
			hasAddButton: /newHost\(\)/.test(html)
		});
	})()`
	got := evalString(t, vm, script)
	for _, want := range []string{`"sentence":true`, `"hasAddButton":true`} {
		if !strings.Contains(got, want) {
			t.Errorf("hosts empty state: missing %s in %s", want, got)
		}
	}
}

// TestHostForm_AddEditHarvest covers the add/edit form: fields render with
// their placeholders, an existing host seeds the form, and the harvested
// payload carries name/target/port/identity_file (port coerced to a number,
// omitted when blank).
func TestHostForm_AddEditHarvest(t *testing.T) {
	vm := newAppVM(t)
	script := `(function(){
		window.newHost();
		var blankHtml = window.renderHostForm();

		document.getElementById('hostName').value = 'devbox';
		document.getElementById('hostTarget').value = 'admin@devbox.local';
		document.getElementById('hostPort').value = '2222';
		document.getElementById('hostIdentityFile').value = '/Users/admin/.ssh/id_ed25519';
		var payload = window.harvestHostForm();

		window.state.hosts = [{id:'h1', name:'devbox', target:'admin@devbox.local', port:2222,
			identity_file:'/Users/admin/.ssh/id_ed25519', status:'idle', probe:{ok:true, node_path:'', claude_path:''}}];
		window.editHost('h1');
		var seeded = window.state.hostForm;

		return JSON.stringify({
			placeholderTarget: blankHtml.indexOf('user@host or ssh-config alias') >= 0,
			placeholderPort: blankHtml.indexOf('placeholder="22"') >= 0,
			placeholderIdentity: blankHtml.indexOf('optional, absolute path') >= 0,
			payloadName: payload.name,
			payloadTarget: payload.target,
			payloadPort: payload.port,
			payloadIdentity: payload.identity_file,
			seededName: seeded.name,
			seededTarget: seeded.target,
			seededPort: seeded.port
		});
	})()`
	got := evalString(t, vm, script)
	for _, want := range []string{
		`"placeholderTarget":true`, `"placeholderPort":true`, `"placeholderIdentity":true`,
		`"payloadName":"devbox"`, `"payloadTarget":"admin@devbox.local"`, `"payloadPort":2222`,
		`"payloadIdentity":"/Users/admin/.ssh/id_ed25519"`,
		`"seededName":"devbox"`, `"seededTarget":"admin@devbox.local"`, `"seededPort":"2222"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("host form add/edit/harvest: missing %s in %s", want, got)
		}
	}
}

// TestHostForm_ProbeResultCard covers the three-line ✓/✗ card, including the
// claude-missing remedy sentence from docs/ssh-hosts.md.
func TestHostForm_ProbeResultCard(t *testing.T) {
	vm := newAppVM(t)
	script := `(function(){
		window.state.hosts = [{id:'h1', name:'devbox', target:'admin@devbox.local',
			probe:{ok:true, node_path:'/opt/homebrew/bin/node', node_version:'v24.7.0', claude_path:''}}];
		window.editHost('h1');
		var html = window.renderHostForm();

		window.state.hosts = [{id:'h2', name:'broken', target:'x@y', probe:{ok:false, error:'Connection refused'}}];
		window.editHost('h2');
		var failHtml = window.renderHostForm();

		return JSON.stringify({
			reachable: html.indexOf('Reachable as admin@devbox.local') >= 0,
			nodeOk: html.indexOf('node v24.7.0 at /opt/homebrew/bin/node') >= 0,
			claudeMissing: html.indexOf('claude not found — install Claude Code on this host and run Probe again') >= 0,
			failShowsError: failHtml.indexOf('Connection refused') >= 0
		});
	})()`
	got := evalString(t, vm, script)
	for _, want := range []string{`"reachable":true`, `"nodeOk":true`, `"claudeMissing":true`, `"failShowsError":true`} {
		if !strings.Contains(got, want) {
			t.Errorf("host probe card: missing %s in %s", want, got)
		}
	}
}

// TestProjectForm_WhereControl is the required round-trip test: choosing a
// host relabels the path field, hides the MCP picker with the one-line
// reason, and the harvested payload carries host_id. Choosing "This Mac"
// again clears it.
func TestProjectForm_WhereControl(t *testing.T) {
	vm := newAppVM(t)
	script := `(function(){
		window.state.hosts = [{id:'h1', name:'devbox', target:'admin@devbox.local'}];
		window.state.externalMcps = [{id:'fsmcp', display_name:'fsMCP'}];
		window.newProject();
		var consoleHtml = window.renderProjectForm();

		window.setProjWhere('h1');
		var hostedHtml = window.renderProjectForm();
		document.getElementById('projName').value = 'relayfs';
		document.getElementById('projPath').value = '/home/admin/src/relayfs';
		var hostedPayload = window.harvestProjectForm();

		window.setProjWhere('');
		var backToConsolePayload = window.harvestProjectForm();

		return JSON.stringify({
			consoleShowsThisMacActive: /perm-btn active[^>]*>This Mac/.test(consoleHtml),
			consoleShowsHostSegment: consoleHtml.indexOf('devbox') >= 0,
			consolePathLabel: consoleHtml.indexOf('<label>Project path</label>') >= 0,
			hostedPathLabel: hostedHtml.indexOf('<label>Path on devbox</label>') >= 0,
			hostedHidesMcpPicker: hostedHtml.indexOf('proj-mcp-row') < 0,
			hostedShowsNote: hostedHtml.indexOf("Relay tools aren't available on a host yet") >= 0,
			hostedHidesSkillSection: hostedHtml.indexOf('Auto-generate SKILL.md') < 0,
			hostedHidesDirAuthSection: hostedHtml.indexOf('Directory Auth') < 0,
			payloadHostId: hostedPayload.host_id,
			payloadNoMcps: Array.isArray(hostedPayload.allowed_mcp_ids) && hostedPayload.allowed_mcp_ids.length === 0,
			payloadSkillOff: hostedPayload.generate_skill === false,
			backToConsoleClearsHostId: backToConsolePayload.host_id === ''
		});
	})()`
	got := evalString(t, vm, script)
	for _, want := range []string{
		`"consoleShowsThisMacActive":true`, `"consoleShowsHostSegment":true`, `"consolePathLabel":true`,
		`"hostedPathLabel":true`, `"hostedHidesMcpPicker":true`, `"hostedShowsNote":true`,
		`"hostedHidesSkillSection":true`, `"hostedHidesDirAuthSection":true`,
		`"payloadHostId":"h1"`, `"payloadNoMcps":true`, `"payloadSkillOff":true`,
		`"backToConsoleClearsHostId":true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("project form Where control: missing %s in %s", want, got)
		}
	}
}

// TestProjectForm_WhereControl_ExistingHostedProjectRoundTrips confirms
// editProject seeds host_id from a stored project (projectFormFromExisting)
// and an unmodified save harvests back to the same id.
func TestProjectForm_WhereControl_ExistingHostedProjectRoundTrips(t *testing.T) {
	vm := newAppVM(t)
	script := `(function(){
		window.state.hosts = [{id:'h1', name:'devbox', target:'admin@devbox.local'}];
		window.state.projects = [{id:'p1', name:'relayfs', path:'/home/admin/src/relayfs', host_id:'h1',
			allowed_mcp_ids:[], allowed_models:[], disabled_tools:{}}];
		window.editProject('p1');
		var html = window.renderProjectForm();
		var payload = window.harvestProjectForm();
		return JSON.stringify({
			formSeeded: window.state.projectForm.host_id === 'h1',
			pathLabel: html.indexOf('<label>Path on devbox</label>') >= 0,
			roundTrips: payload.host_id === 'h1' && payload.path === '/home/admin/src/relayfs'
		});
	})()`
	got := evalString(t, vm, script)
	for _, want := range []string{`"formSeeded":true`, `"pathLabel":true`, `"roundTrips":true`} {
		if !strings.Contains(got, want) {
			t.Errorf("hosted project round trip: missing %s in %s", want, got)
		}
	}
}

// TestProjectForm_HostIdAndKindRemoteMutuallyExclusive covers the client-side
// half of docs/ssh-hosts.md's mutual-exclusion rule: switching an in-progress
// form to kind:remote clears any host_id it was carrying, so a stray value
// from flipping Where before Kind can't reach the server.
func TestProjectForm_HostIdAndKindRemoteMutuallyExclusive(t *testing.T) {
	vm := newAppVM(t)
	script := `(function(){
		window.state.hosts = [{id:'h1', name:'devbox', target:'admin@devbox.local'}];
		window.newProject();
		window.setProjWhere('h1');
		window.setProjKind('remote');
		var payload = window.harvestProjectForm();
		return JSON.stringify({ clearedOnRemote: payload.host_id === '', kindIsRemote: payload.kind === 'remote' });
	})()`
	got := evalString(t, vm, script)
	for _, want := range []string{`"clearedOnRemote":true`, `"kindIsRemote":true`} {
		if !strings.Contains(got, want) {
			t.Errorf("host_id/kind mutual exclusion: missing %s in %s", want, got)
		}
	}
}

// TestProjectList_HostChip covers the project list card's host tag — present
// only for a hosted project, absent (not just empty) for a console one.
func TestProjectList_HostChip(t *testing.T) {
	vm := newAppVM(t)
	script := `(function(){
		window.state.hosts = [{id:'h1', name:'devbox', target:'admin@devbox.local'}];
		window.state.projects = [
			{id:'p1', name:'OnHost', path:'/home/admin/x', host_id:'h1', allowed_mcp_ids:[], allowed_models:[], disabled_tools:{}},
			{id:'p2', name:'OnConsole', path:'/Users/admin/y', allowed_mcp_ids:['*'], allowed_models:['*'], disabled_tools:{}}
		];
		var html = window.renderProjects();
		var hostSection = html.slice(html.indexOf('OnHost'), html.indexOf('OnConsole'));
		var consoleSection = html.slice(html.indexOf('OnConsole'));
		return JSON.stringify({
			hostShowsChip: hostSection.indexOf('proj-host-chip') >= 0 && hostSection.indexOf('devbox') >= 0,
			consoleHasNoChip: consoleSection.indexOf('proj-host-chip') < 0
		});
	})()`
	got := evalString(t, vm, script)
	for _, want := range []string{`"hostShowsChip":true`, `"consoleHasNoChip":true`} {
		if !strings.Contains(got, want) {
			t.Errorf("project list host chip: missing %s in %s", want, got)
		}
	}
}
