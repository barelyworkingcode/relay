package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// One catalog shared by the module and app tiers: a Claude alias, a broker
// alias with its target, the target itself, and one non-chat model.
const pickerCatalogOK = `{status:'ok', models:[
	{id:'haiku', label:'haiku', group:'Claude', provider:'claude', kind:'chat'},
	{id:'Chat', label:'Chat → acme-llm/Chat', group:'Model broker · aliases', provider:'chat', kind:'chat', target:'acme-llm/Chat'},
	{id:'acme-llm/Chat', label:'acme-llm/Chat', group:'Model broker · acme-llm', provider:'chat', kind:'chat'},
	{id:'acme-llm/kokoro-tts', label:'acme-llm/kokoro-tts', group:'Model broker · acme-llm', provider:'chat', kind:'other'}
]}`

const pickerCatalogUnavailable = `{status:'unavailable', error:'session host unavailable', models:[]}`

func newModelPickerVM(t *testing.T) *goja.Runtime {
	t.Helper()
	vm := goja.New()
	if _, err := vm.RunString(bundleForTest(t, "web/src/lib/model_picker.js", "PICKER")); err != nil {
		t.Fatalf("loading model_picker bundle: %v", err)
	}
	if _, err := vm.RunString(`var OK = ` + pickerCatalogOK + `; var DOWN = ` + pickerCatalogUnavailable + `;`); err != nil {
		t.Fatalf("seeding catalogs: %v", err)
	}
	return vm
}

func TestModelPicker_Module(t *testing.T) {
	vm := newModelPickerVM(t)
	cases := []struct{ name, expr, want string }{
		// P1
		{"unavailable: missing saved ids once, in saved order, no wildcard",
			`JSON.stringify(PICKER.unavailableSavedModels(['zeta','*','haiku','alpha','zeta'], OK))`, `["zeta","alpha"]`},
		{"unavailable: nothing is missing against a view that is not ok",
			`JSON.stringify(PICKER.unavailableSavedModels(['zeta'], DOWN))`, `[]`},
		// P2
		{"group ok: unavailable first, chat groups first-seen, one Other",
			`JSON.stringify(PICKER.groupModelCatalog(OK, ['gone-model','haiku']).map(function(g){ return [g.label, g.kind, g.rows.map(function(r){ return r.id; })]; }))`,
			`[["Not currently available","unavailable",["gone-model"]],["Claude","chat",["haiku"]],["Model broker · aliases","chat",["Chat"]],["Model broker · acme-llm","chat",["acme-llm/Chat"]],["Other","other",["acme-llm/kokoro-tts"]]]`},
		{"group ok: alias row keeps its target",
			`PICKER.groupModelCatalog(OK, [])[1].rows[0].target`, `acme-llm/Chat`},
		{"group not ok: a single Saved group of saved ids, no wildcard, not marked",
			`JSON.stringify(PICKER.groupModelCatalog(DOWN, ['*','haiku','gone-model']))`,
			`[{"label":"Saved","kind":"saved","rows":[{"id":"haiku","label":"haiku","target":"","unavailable":false},{"id":"gone-model","label":"gone-model","target":"","unavailable":false}]}]`},
		// P4
		{"filter: case-insensitive match on the target, empty groups dropped",
			`JSON.stringify(PICKER.filterModelGroups(PICKER.groupModelCatalog(OK, []), 'ACME-LLM/CHAT').map(function(g){ return g.rows.map(function(r){ return r.id; }); }))`,
			`[["Chat"],["acme-llm/Chat"]]`},
		{"filter: a matching group label keeps the whole group",
			`JSON.stringify(PICKER.filterModelGroups(PICKER.groupModelCatalog(OK, []), 'claude').map(function(g){ return g.label; }))`,
			`["Claude"]`},
		{"filter: match on id only",
			`JSON.stringify(PICKER.filterModelGroups(PICKER.groupModelCatalog(OK, []), 'tts').map(function(g){ return g.rows[0].id; }))`,
			`["acme-llm/kokoro-tts"]`},
		// P5: banner
		{"banner: ok with no warnings is empty", `PICKER.renderModelPickerBanner(OK, false)`, ``},
		{"banner: pending shows loading", `/id="projModelsBanner"[^>]*>Loading models…/.test(PICKER.renderModelPickerBanner(null, true))`, `true`},
		{"banner: unavailable shows the error and Retry",
			`(function(h){ return h.indexOf('id="projModelsBanner"') >= 0 && h.indexOf('session host unavailable') >= 0 && h.indexOf('onclick="requestModelCatalog()"') >= 0; })(PICKER.renderModelPickerBanner(DOWN, false))`, `true`},
		{"banner: a warning shows with Retry",
			`(function(h){ return h.indexOf('model broker unavailable') >= 0 && h.indexOf('onclick="requestModelCatalog()"') >= 0; })(PICKER.renderModelPickerBanner({status:'ok', warnings:['model broker unavailable'], models:[]}, false))`, `true`},
		// P5: empty list
		{"list: empty and not searching", `PICKER.renderModelPickerList([], {searching:false}).indexOf('No models listed.') >= 0`, `true`},
		{"list: empty while searching", `PICKER.renderModelPickerList([], {searching:true}).indexOf('No models match.') >= 0`, `true`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := evalString(t, vm, c.expr); got != c.want {
				t.Errorf("got  %s\nwant %s", got, c.want)
			}
		})
	}
}

func TestModelPicker_ModuleRenderList(t *testing.T) {
	vm := newModelPickerVM(t)
	render := func(otherOpen, searching bool) string {
		return evalString(t, vm, `(function(){
			var groups = PICKER.groupModelCatalog(OK, ['gone-model','haiku']);
			groups[1].rows.push({id:'a"b', label:'a"b', target:'', unavailable:false});
			__bound = [];
			return PICKER.renderModelPickerList(groups, {selected:['gone-model','haiku'],
				bindToggle:function(id){ __bound.push(id); return 'data-act="7"'; },
				otherOpen:`+boolJS(otherOpen)+`, searching:`+boolJS(searching)+`});
		})()`)
	}
	closed := render(false, false)
	for _, want := range []string{
		`data-model-group="Claude" data-model-group-kind="chat"`,
		`data-model-group-kind="unavailable"`,
		`data-model-group-kind="other"`,
		`id="projModelsOtherToggle"`,
		`class="model-unavailable">not currently available`,
		`class="model-alias-tag">alias`,
		`data-model-id="a&quot;b"`,
	} {
		if !strings.Contains(closed, want) {
			t.Errorf("render missing %s\n%s", want, closed)
		}
	}
	if strings.Contains(closed, `a"b`) {
		t.Errorf("an id with a quote rendered unescaped\n%s", closed)
	}
	if strings.Contains(closed, `data-model-id="acme-llm/kokoro-tts"`) {
		t.Errorf("Other rows rendered while collapsed\n%s", closed)
	}
	var checked []string
	for _, in := range regexp.MustCompile(`<input[^>]*>`).FindAllString(closed, -1) {
		if !strings.Contains(in, `data-act="7"`) {
			t.Errorf("checkbox not bound through bindToggle: %s", in)
		}
		if regexp.MustCompile(`\schecked[\s/>]`).MatchString(in) {
			checked = append(checked, regexp.MustCompile(`data-model-id="([^"]*)"`).FindStringSubmatch(in)[1])
		}
	}
	if strings.Join(checked, ",") != "gone-model,haiku" {
		t.Errorf("checked = %v, want gone-model,haiku\n%s", checked, closed)
	}
	if got := evalString(t, vm, `__bound.indexOf('a"b') >= 0`); got != "true" {
		t.Errorf("bindToggle was not given the raw id")
	}
	for _, m := range regexp.MustCompile(`\son\w+="[^"]*"`).FindAllString(closed, -1) {
		if strings.TrimSpace(m) != `onclick="toggleProjModelsOther()"` {
			t.Errorf("an on* attribute other than the constant Other toggle: %s", m)
		}
	}

	for name, html := range map[string]string{"open": render(true, false), "searching": render(false, true)} {
		if !strings.Contains(html, `data-model-id="acme-llm/kokoro-tts"`) {
			t.Errorf("%s: Other rows hidden\n%s", name, html)
		}
	}
}

func boolJS(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// ---- app tier ----------------------------------------------------------

const modelPickerProjectsFixture = `[
	{id:'p1', name:'Acme', path:'/tmp/acme', allowed_mcp_ids:[], allowed_models:['gone-model','haiku'], disabled_tools:{}},
	{id:'p2', name:'Acme remote', kind:'remote', path:'', allowed_mcp_ids:[], allowed_models:[], disabled_tools:{}},
	{id:'p3', name:'Acme wild', path:'/tmp/acme-wild', allowed_mcp_ids:[], allowed_models:['*'], disabled_tools:{}}
]`

func seedModelPickerVM(t *testing.T) *goja.Runtime {
	t.Helper()
	vm := newAppVM(t)
	evalString(t, vm, `(function(){
		window.__sent = [];
		window.webkit = { messageHandlers: { ipc: { postMessage: function(m){ window.__sent.push(String(m)); } } } };
		window.state.page = 'projects';
		window.state.projects = `+modelPickerProjectsFixture+`;
		return true;
	})()`)
	return vm
}

// openWithCatalog opens a project form and answers its list_models request.
func openWithCatalog(t *testing.T, vm *goja.Runtime, id, catalog string) string {
	t.Helper()
	return evalString(t, vm, `(function(){
		window.editProject('`+id+`');
		window.onModelsListed(`+catalog+`);
		return window.renderProjectForm();
	})()`)
}

func harvestedModels(t *testing.T, vm *goja.Runtime) string {
	t.Helper()
	return evalString(t, vm, `JSON.stringify(window.harvestProjectForm().allowed_models)`)
}

// clickModel fires the binding the rendered checkbox for id carries.
func clickModel(t *testing.T, vm *goja.Runtime, id string) {
	t.Helper()
	html := evalString(t, vm, `window.renderProjectForm()`)
	m := regexp.MustCompile(`<input[^>]*data-model-id="` + regexp.QuoteMeta(id) + `"[^>]*data-act="(\d+)"`).FindStringSubmatch(html)
	if m == nil {
		t.Fatalf("no bound checkbox for %s\n%s", id, html)
	}
	evalString(t, vm, `(function(){ var e = window.state._actBind[`+m[1]+`]; e[0].apply(null, e[1]); return true; })()`)
}

func TestModelPicker_RequestsCatalogOnlyForLocalForms(t *testing.T) {
	vm := seedModelPickerVM(t)
	evalString(t, vm, `(window.editProject('p2'), true)`)
	if sent := sentMessages(t, vm); strings.Contains(sent, "list_models") {
		t.Fatalf("a remote form asked for the catalog: %s", sent)
	}
	evalString(t, vm, `(window.editProject('p1'), true)`)
	if n := strings.Count(sentMessages(t, vm), `"type":"list_models"`); n != 1 {
		t.Fatalf("opening a local form sent %d list_models, want 1: %s", n, sentMessages(t, vm))
	}
}

func TestModelPicker_RendersGroupsAliasAndCollapsedOther(t *testing.T) {
	vm := seedModelPickerVM(t)
	html := openWithCatalog(t, vm, "p1", pickerCatalogOK)
	for _, want := range []string{
		`data-model-group="Claude"`, `data-model-group="Model broker · aliases"`, `data-model-group="Model broker · acme-llm"`,
		`Chat → acme-llm/Chat`, `model-alias-tag`, `id="projModelsOtherToggle"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("form missing %s\n%s", want, html)
		}
	}
	if strings.Contains(html, `data-model-id="acme-llm/kokoro-tts"`) {
		t.Errorf("Other rows shown before the group is opened\n%s", html)
	}
}

func TestModelPicker_UnavailableSavedIdKeptAndMarked(t *testing.T) {
	vm := seedModelPickerVM(t)
	html := openWithCatalog(t, vm, "p1", pickerCatalogOK)
	row := regexp.MustCompile(`<label[^>]*>\s*<input[^>]*data-model-id="gone-model"[^>]*>.*?</label>`).FindString(html)
	if !strings.Contains(row, " checked") || !strings.Contains(row, "not currently available") {
		t.Errorf("gone-model row not checked and marked: %q\n%s", row, html)
	}
	if got := harvestedModels(t, vm); got != `["gone-model","haiku"]` {
		t.Errorf("untouched harvest = %s, want the stored list exactly", got)
	}
}

func TestModelPicker_TogglingAppendsAndRemoves(t *testing.T) {
	vm := seedModelPickerVM(t)
	openWithCatalog(t, vm, "p1", pickerCatalogOK)
	clickModel(t, vm, "Chat")
	if got := harvestedModels(t, vm); got != `["gone-model","haiku","Chat"]` {
		t.Errorf("after checking Chat: %s", got)
	}
	clickModel(t, vm, "gone-model")
	if got := harvestedModels(t, vm); got != `["haiku","Chat"]` {
		t.Errorf("after unchecking gone-model: %s", got)
	}
}

func TestModelPicker_CatalogUnavailableKeepsSavedIds(t *testing.T) {
	vm := seedModelPickerVM(t)
	html := openWithCatalog(t, vm, "p1", pickerCatalogUnavailable)
	for _, want := range []string{`id="projModelsBanner"`, `data-model-group-kind="saved"`, `data-model-id="gone-model"`, `data-model-id="haiku"`} {
		if !strings.Contains(html, want) {
			t.Errorf("form missing %s\n%s", want, html)
		}
	}
	if strings.Contains(html, "not currently available") {
		t.Errorf("saved ids marked unavailable when the whole list is unavailable\n%s", html)
	}
	if got := harvestedModels(t, vm); got != `["gone-model","haiku"]` {
		t.Errorf("harvest = %s, want the stored list exactly", got)
	}
}

func TestModelPicker_WildcardHidesList(t *testing.T) {
	vm := seedModelPickerVM(t)
	on := openWithCatalog(t, vm, "p3", pickerCatalogOK)
	if strings.Contains(on, `id="projModelsSearch"`) || strings.Contains(on, `id="projModelsList"`) {
		t.Errorf("wildcard on still renders the picker\n%s", on)
	}
	if got := harvestedModels(t, vm); got != `["*"]` {
		t.Errorf("wildcard harvest = %s", got)
	}
	off := evalString(t, vm, `(window.setProjModelsWildcard(false), window.renderProjectForm())`)
	for _, want := range []string{`id="projModelsSearch"`, `id="projModelsList"`, `id="projModelsEmptyNote"`} {
		if !strings.Contains(off, want) {
			t.Errorf("wildcard off missing %s\n%s", want, off)
		}
	}
}

func TestModelPicker_SearchRepaintsOnlyTheList(t *testing.T) {
	vm := seedModelPickerVM(t)
	openWithCatalog(t, vm, "p1", pickerCatalogOK)
	got := evalString(t, vm, `(function(){
		window.render();
		var before = window.state._actBind.slice();
		window.setProjModelSearch('tts');
		var after = window.state._actBind;
		var kept = before.length > 0 && after.length > before.length;
		for (var i = 0; i < before.length; i++) if (after[i] !== before[i]) kept = false;
		return JSON.stringify({kept: kept, list: document.getElementById('projModelsList').innerHTML});
	})()`)
	if !strings.Contains(got, `"kept":true`) {
		t.Errorf("search cleared or rewrote the form's other bindings: %s", got)
	}
	ids := regexp.MustCompile(`data-model-id=\\"([^"\\]*)\\"`).FindAllStringSubmatch(got, -1)
	if len(ids) != 1 || ids[0][1] != "acme-llm/kokoro-tts" {
		t.Errorf("search 'tts' listed %v, want only acme-llm/kokoro-tts: %s", ids, got)
	}
}

func TestModelPicker_OtherToggleShowsRows(t *testing.T) {
	vm := seedModelPickerVM(t)
	openWithCatalog(t, vm, "p1", pickerCatalogOK)
	html := evalString(t, vm, `(window.toggleProjModelsOther(), window.renderProjectForm())`)
	if !strings.Contains(html, `data-model-id="acme-llm/kokoro-tts"`) {
		t.Errorf("Other rows still hidden after toggling\n%s", html)
	}
}
