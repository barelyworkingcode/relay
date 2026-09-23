package main

// JS-module-logic coverage for the Services list's ordering and "Show in
// menu" controls, run against the real bundle in goja like every other
// settings_*_ui_test.go file.

import (
	"strings"
	"testing"
)

// serviceOrderHarness installs an IPC capture and helpers that read the
// rendered Services list the way an operator would: by the controls'
// aria-labels, in document order.
const serviceOrderHarness = `
window.__sent = [];
window.webkit = { messageHandlers: { ipc: { postMessage: function(m){ window.__sent.push(m); } } } };
window.__msgs = function(){
	return window.__sent.map(function(m){ return typeof m === 'string' ? JSON.parse(m) : m; });
};
window.__svc = function(id, name, extra){
	var s = {id:id, display_name:name, command:'/bin/' + id, args:[], env:{}, capabilities:[]};
	for (var k in (extra || {})) s[k] = extra[k];
	return s;
};
window.__render = function(){
	window.state._actBind = [];
	return window.renderServices();
};
window.__tags = function(html, tag, label){
	var re = new RegExp('<' + tag + '\\b[^>]*aria-label="' + label + '"[^>]*>', 'g');
	return html.match(re) || [];
};
window.__disabled = function(tagText){ return /\sdisabled(\s|=|>|\/)/.test(tagText); };
window.__checked = function(tagText){ return /\schecked(\s|=|>|\/)/.test(tagText); };
window.__click = function(tagText){
	var m = /data-act="(\d+)"/.exec(tagText);
	if (!m) throw new Error('control has no data-act binding: ' + tagText);
	var b = window.state._actBind[Number(m[1])];
	return b[0].apply(null, b[1]);
};
window.__order = function(html, names){
	return names.slice().sort(function(a, b){ return html.indexOf(a) - html.indexOf(b); });
};
`

func newServiceOrderVM(t *testing.T) func(string) string {
	t.Helper()
	vm := newAppVM(t)
	evalString(t, vm, serviceOrderHarness+`; 'ok'`)
	return func(script string) string { return evalString(t, vm, script) }
}

func wantAll(t *testing.T, what, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("%s: missing %s in %s", what, w, got)
		}
	}
}

func TestServiceOrder_HandlersExist(t *testing.T) {
	eval := newServiceOrderVM(t)
	for _, fn := range []string{"moveService", "updateServiceMenuHidden"} {
		if got := eval(`typeof window.` + fn); got != "function" {
			t.Errorf("window.%s: typeof = %q, want function", fn, got)
		}
	}
}

func TestServiceOrder_SingleServiceBothMovesDisabled(t *testing.T) {
	eval := newServiceOrderVM(t)
	got := eval(`(function(){
		window.state.services = [window.__svc('solo', 'Solo Svc')];
		var html = window.__render();
		var up = window.__tags(html, 'button', 'Move up');
		var down = window.__tags(html, 'button', 'Move down');
		return JSON.stringify({
			upCount: up.length, downCount: down.length,
			upDisabled: up.length === 1 && window.__disabled(up[0]),
			downDisabled: down.length === 1 && window.__disabled(down[0])
		});
	})()`)
	wantAll(t, "single service", got,
		`"upCount":1`, `"downCount":1`, `"upDisabled":true`, `"downDisabled":true`)
}

func TestServiceOrder_ThreeServicesEdgeButtonsDisabled(t *testing.T) {
	eval := newServiceOrderVM(t)
	got := eval(`(function(){
		window.state.services = [
			window.__svc('alpha', 'Alpha Svc'),
			window.__svc('bravo', 'Bravo Svc'),
			window.__svc('charlie', 'Charlie Svc')
		];
		var html = window.__render();
		var up = window.__tags(html, 'button', 'Move up');
		var down = window.__tags(html, 'button', 'Move down');
		return JSON.stringify({
			upCount: up.length, downCount: down.length,
			up: up.map(window.__disabled),
			down: down.map(window.__disabled)
		});
	})()`)
	wantAll(t, "three services", got,
		`"upCount":3`, `"downCount":3`,
		`"up":[true,false,false]`, `"down":[false,false,true]`)
}

func TestServiceOrder_MoveDownReordersAndSendsOneMessage(t *testing.T) {
	eval := newServiceOrderVM(t)
	got := eval(`(function(){
		window.state.services = [
			window.__svc('alpha', 'Alpha Svc'),
			window.__svc('bravo', 'Bravo Svc'),
			window.__svc('charlie', 'Charlie Svc')
		];
		var names = ['Alpha Svc', 'Bravo Svc', 'Charlie Svc'];
		var html = window.__render();
		var before = window.__order(html, names);
		window.__click(window.__tags(html, 'button', 'Move down')[0]);
		var after = window.__order(window.__render(), names);
		var msgs = window.__msgs();
		return JSON.stringify({
			before: before.join(','), after: after.join(','),
			count: msgs.length,
			type: msgs[0] && msgs[0].type, id: msgs[0] && msgs[0].id, index: msgs[0] && msgs[0].index
		});
	})()`)
	wantAll(t, "move down", got,
		`"before":"Alpha Svc,Bravo Svc,Charlie Svc"`,
		`"after":"Bravo Svc,Alpha Svc,Charlie Svc"`,
		`"count":1`, `"type":"move_service"`, `"id":"alpha"`, `"index":1`)
}

func TestServiceOrder_MoveUpReordersAndSendsOneMessage(t *testing.T) {
	eval := newServiceOrderVM(t)
	got := eval(`(function(){
		window.state.services = [
			window.__svc('alpha', 'Alpha Svc'),
			window.__svc('bravo', 'Bravo Svc'),
			window.__svc('charlie', 'Charlie Svc')
		];
		var names = ['Alpha Svc', 'Bravo Svc', 'Charlie Svc'];
		var html = window.__render();
		window.__click(window.__tags(html, 'button', 'Move up')[2]);
		var after = window.__order(window.__render(), names);
		var msgs = window.__msgs();
		return JSON.stringify({
			after: after.join(','),
			count: msgs.length,
			type: msgs[0] && msgs[0].type, id: msgs[0] && msgs[0].id, index: msgs[0] && msgs[0].index
		});
	})()`)
	wantAll(t, "move up", got,
		`"after":"Alpha Svc,Charlie Svc,Bravo Svc"`,
		`"count":1`, `"type":"move_service"`, `"id":"charlie"`, `"index":1`)
}

func TestServiceOrder_MoveServiceDirectCall(t *testing.T) {
	eval := newServiceOrderVM(t)
	got := eval(`(function(){
		window.state.services = [
			window.__svc('alpha', 'Alpha Svc'),
			window.__svc('bravo', 'Bravo Svc'),
			window.__svc('charlie', 'Charlie Svc')
		];
		var names = ['Alpha Svc', 'Bravo Svc', 'Charlie Svc'];
		window.moveService('bravo', 1);
		var afterDown = window.__order(window.__render(), names);
		var downMsgs = window.__msgs();
		window.__sent = [];
		window.moveService('bravo', -1);
		var afterUp = window.__order(window.__render(), names);
		var upMsgs = window.__msgs();
		return JSON.stringify({
			afterDown: afterDown.join(','), downCount: downMsgs.length, down: downMsgs[0],
			afterUp: afterUp.join(','), upCount: upMsgs.length, up: upMsgs[0]
		});
	})()`)
	wantAll(t, "moveService direct", got,
		`"afterDown":"Alpha Svc,Charlie Svc,Bravo Svc"`, `"downCount":1`,
		`"afterUp":"Alpha Svc,Bravo Svc,Charlie Svc"`, `"upCount":1`)
	// Field-level checks so key order in the message does not matter.
	fields := eval(`(function(){
		window.state.services = [window.__svc('a','A1'), window.__svc('b','B1'), window.__svc('c','C1')];
		window.__sent = [];
		window.moveService('b', 1);
		window.moveService('c', -1);
		var m = window.__msgs();
		return JSON.stringify(m.map(function(x){ return [x.type, x.id, x.index]; }));
	})()`)
	if want := `[["move_service","b",2],["move_service","c",0]]`; fields != want {
		t.Errorf("moveService messages = %s, want %s", fields, want)
	}
}

func TestServiceOrder_ShowInMenuToggleReflectsHideFromMenu(t *testing.T) {
	eval := newServiceOrderVM(t)
	got := eval(`(function(){
		window.state.services = [
			window.__svc('hidden', 'Hidden Svc', {hide_from_menu: true}),
			window.__svc('shown', 'Shown Svc', {hide_from_menu: false}),
			window.__svc('absent', 'Absent Svc')
		];
		var html = window.__render();
		var toggles = window.__tags(html, 'input', 'Show in menu');
		return JSON.stringify({
			count: toggles.length,
			checkboxes: toggles.every(function(t){ return /type="checkbox"/.test(t); }),
			checked: toggles.map(window.__checked)
		});
	})()`)
	wantAll(t, "show in menu rendering", got,
		`"count":3`, `"checkboxes":true`, `"checked":[false,true,true]`)
}

func TestServiceOrder_ShowInMenuToggleSendsHidden(t *testing.T) {
	eval := newServiceOrderVM(t)
	got := eval(`(function(){
		window.state.services = [
			window.__svc('alpha', 'Alpha Svc'),
			window.__svc('bravo', 'Bravo Svc', {hide_from_menu: true})
		];
		window.__render();
		window.updateServiceMenuHidden('alpha', false);
		window.updateServiceMenuHidden('bravo', true);
		var m = window.__msgs();
		return JSON.stringify(m.map(function(x){ return [x.type, x.id, x.hidden]; }));
	})()`)
	want := `[["update_service_menu_hidden","alpha",true],["update_service_menu_hidden","bravo",false]]`
	if got != want {
		t.Errorf("show in menu messages = %s, want %s", got, want)
	}
}
