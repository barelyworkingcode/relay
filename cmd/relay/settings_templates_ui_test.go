package main

import (
	"strings"
	"testing"
)

// The edit round trip: an existing template seeds the form, the form's
// inputs come back out as the update message, and the fresh list the server
// answers with closes the form.
func TestTemplatesTab_EditSavesAndClosesOnList(t *testing.T) {
	vm := newAppVM(t)
	got := evalString(t, vm, `(function(){
		var sent = [];
		window.webkit = { messageHandlers: { ipc: { postMessage: function(msg){ sent.push(JSON.parse(msg)); } } } };
		window.state.page = 'templates';
		window.state.templates = [{id:'shell', name:'Shell', sandbox:true, read_write:['~'], env:{A:'1'}}];
		window.editTemplate('shell');
		// The goja DOM does not parse innerHTML, so every input is set the way
		// a browser would already hold it.
		var set = function(id, v){ document.getElementById('tpl_' + id).value = v; };
		set('id', 'shell'); set('name', 'Shell'); set('read_write', '~'); set('env', 'A=1');
		var box = document.getElementById('tpl_sandbox'); box.type = 'checkbox'; box.checked = true;
		set('deny', '~/.ssh\n~/Library/Application Support/relay\n');
		window.saveTemplateForm();
		var msg = sent[sent.length - 1];
		var stillOpen = window.state.editingTemplateId;
		window.onTemplatesListed(window.state.templates);
		return JSON.stringify({
			type: msg.type, id: msg.id, deny: msg.deny, readWrite: msg.read_write, env: msg.env, sandbox: msg.sandbox,
			stillOpen: stillOpen, closed: window.state.editingTemplateId === null
		});
	})()`)
	for _, want := range []string{
		`"type":"update_template"`, `"id":"shell"`, `"deny":["~/.ssh","~/Library/Application Support/relay"]`,
		`"readWrite":["~"]`, `"env":{"A":"1"}`, `"sandbox":true`, `"stillOpen":"shell"`, `"closed":true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("template edit: missing %s in %s", want, got)
		}
	}
}

func TestTemplatesTab_ErrorKeepsTheFormOpen(t *testing.T) {
	vm := newAppVM(t)
	got := evalString(t, vm, `(function(){
		window.webkit = { messageHandlers: { ipc: { postMessage: function(){} } } };
		window.state.page = 'templates';
		window.newTemplate();
		document.getElementById('tpl_id').value = 'x';
		document.getElementById('tpl_name').value = 'X';
		window.saveTemplateForm();
		window.onTemplateError('template "x" already exists');
		return JSON.stringify({
			open: window.state.editingTemplateId === 'new',
			shown: window.renderTemplateForm().indexOf('already exists') >= 0,
			saveEnabled: window.renderTemplateForm().indexOf('disabled') < 0
		});
	})()`)
	for _, want := range []string{`"open":true`, `"shown":true`, `"saveEnabled":true`} {
		if !strings.Contains(got, want) {
			t.Errorf("template error: missing %s in %s", want, got)
		}
	}
}

// A project's template list is opt-in: a new project holds none, the wildcard
// switch writes a lone "*", and a remote profile always sends an empty list.
func TestProjectForm_AllowedTemplates(t *testing.T) {
	vm := newAppVM(t)
	got := evalString(t, vm, `(function(){
		window.state.templates = [{id:'shell', name:'Shell'}, {id:'pi', name:'pi'}];
		window.newProject();
		var f = window.state.projectForm;
		var blank = JSON.stringify(f.allowed_templates);
		window.toggleProjTemplate('pi');
		var picked = JSON.stringify(f.allowed_templates);
		window.setProjTemplatesWildcard(true);
		var wild = JSON.stringify(window.harvestProjectForm().allowed_templates);
		window.setProjKind('remote');
		var remote = JSON.stringify(window.harvestProjectForm().allowed_templates);
		return JSON.stringify({blank: blank, picked: picked, wild: wild, remote: remote});
	})()`)
	for _, want := range []string{`"blank":"[]"`, `"picked":"[\"pi\"]"`, `"wild":"[\"*\"]"`, `"remote":"[]"`} {
		if !strings.Contains(got, want) {
			t.Errorf("project templates: missing %s in %s", want, got)
		}
	}
}

// A template is sandboxed unless it says sandbox: false, so the list's pill
// and the edit toggle both read an absent key as sandboxed.
func TestTemplatesTab_SandboxPillAndToggle(t *testing.T) {
	for _, c := range []struct {
		name, tmpl string
		want       string
	}{
		{"absent", `{id:'t', name:'T'}`, `{"pill":true,"toggle":true}`},
		{"true", `{id:'t', name:'T', sandbox:true}`, `{"pill":true,"toggle":true}`},
		{"false", `{id:'t', name:'T', sandbox:false}`, `{"pill":false,"toggle":false}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			vm := newAppVM(t)
			got := evalString(t, vm, `(function(){
				window.webkit = { messageHandlers: { ipc: { postMessage: function(){} } } };
				window.state.page = 'templates';
				window.state.editingTemplateId = null;
				window.state.templates = [`+c.tmpl+`];
				window.render();
				var pill = document.getElementById('content').innerHTML.indexOf('>sandboxed<') >= 0;
				// render() re-reads form inputs from the DOM, which the goja shim
				// cannot hold, so the form is read straight from its builder.
				var toggle = window.templateFormFromExisting(window.state.templates[0]).sandbox;
				return JSON.stringify({pill: pill, toggle: toggle});
			})()`)
			if got != c.want {
				t.Errorf("sandbox %s: got %s, want %s", c.name, got, c.want)
			}
		})
	}
}

func TestTemplatesTab_NewTemplateStartsSandboxed(t *testing.T) {
	vm := newAppVM(t)
	got := evalString(t, vm, `String(window.blankTemplateForm().sandbox)`)
	if got != "true" {
		t.Errorf("a new template's sandbox toggle = %s, want true", got)
	}
}
