package main

// Hermetic unit + smoke tests for the settings UI JavaScript. The previously
// untested ~2,400-line settings layer is exercised here under goja (a pure-Go
// ES VM) with the same esbuild bundler the production build uses — no Node, no
// browser. Two layers:
//
//   1. Pure logic (web/src/lib/pure.js) — JSONC parsing, config-tree path ops,
//      value coercion, required-field scanning, formatting — tested in isolation.
//   2. A smoke test that bundles the WHOLE app (web/src/entry.js) under a minimal
//      DOM shim and asserts it loads + renders without a ReferenceError. This is
//      the regression net for the module split: a missing import or an un-
//      globalized inline handler surfaces as a thrown error here.
//
// The bundle is rebuilt from source on every run, so the tests always reflect
// the current web/src tree (not the committed web/dist artifact).

import (
	"strings"
	"testing"

	"github.com/dop251/goja"
	"github.com/evanw/esbuild/pkg/api"
)

// bundleForTest bundles an entry point to an ES2015 IIFE goja can run. globalName
// (when non-empty) exposes the entry's exports as that global var.
func bundleForTest(t *testing.T, entry, globalName string) string {
	t.Helper()
	opts := api.BuildOptions{
		EntryPoints: []string{entry},
		Bundle:      true,
		Format:      api.FormatIIFE,
		Target:      api.ES2015, // goja is ES5.1 + much of ES6; ES2015 output is safe
		Write:       false,
	}
	if globalName != "" {
		opts.GlobalName = globalName
	}
	r := api.Build(opts)
	if len(r.Errors) > 0 {
		var b strings.Builder
		for _, e := range r.Errors {
			b.WriteString(e.Text)
			b.WriteByte('\n')
		}
		t.Fatalf("esbuild bundling %s failed:\n%s", entry, b.String())
	}
	if len(r.OutputFiles) == 0 {
		t.Fatalf("esbuild produced no output for %s", entry)
	}
	return string(r.OutputFiles[0].Contents)
}

// newPureVM returns a goja runtime with web/src/lib/pure.js loaded as `PURE`.
func newPureVM(t *testing.T) *goja.Runtime {
	t.Helper()
	vm := goja.New()
	if _, err := vm.RunString(bundleForTest(t, "web/src/lib/pure.js", "PURE")); err != nil {
		t.Fatalf("loading pure bundle: %v", err)
	}
	return vm
}

// evalString runs expr and returns its result as a string.
func evalString(t *testing.T, vm *goja.Runtime, expr string) string {
	t.Helper()
	v, err := vm.RunString(expr)
	if err != nil {
		t.Fatalf("eval %q: %v", expr, err)
	}
	return v.String()
}

func TestPureJsonc(t *testing.T) {
	vm := newPureVM(t)
	cases := []struct{ name, expr, want string }{
		{"line comment stripped", `PURE.cfgStripJsonComments('{"a":1} // tail')`, `{"a":1} `},
		{"block comment stripped", `PURE.cfgStripJsonComments('{ /* c */ "a":1 }')`, `{  "a":1 }`},
		{"slashes inside string survive", `PURE.cfgStripJsonComments('{"u":"http://x"}')`, `{"u":"http://x"}`},
		{"parse jsonc to value", `JSON.stringify(PURE.cfgParseConfigText('{ // c\n "a": [1,2] }'))`, `{"a":[1,2]}`},
		{"parse invalid -> undefined", `String(PURE.cfgParseConfigText('{not json'))`, `undefined`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := evalString(t, vm, c.expr); got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestPureCfgTree(t *testing.T) {
	vm := newPureVM(t)
	cases := []struct{ name, expr, want string }{
		{"getAt nested + index", `String(PURE.cfgGetAt({a:{b:[10,20]}}, ['a','b',1]))`, `20`},
		{"getAt missing -> undefined", `String(PURE.cfgGetAt({a:1}, ['a','b','c']))`, `undefined`},
		{"setAt lazily creates chain", `(function(){var o={};PURE.cfgSetAt(o,['x','y'],7);return JSON.stringify(o);})()`, `{"x":{"y":7}}`},
		{"setAt numeric step makes array", `(function(){var o={};PURE.cfgSetAt(o,['a',0],'v');return JSON.stringify(o);})()`, `{"a":["v"]}`},
		{"defaultFor array", `JSON.stringify(PURE.cfgDefaultFor({type:'array'}))`, `[]`},
		{"defaultFor bool", `String(PURE.cfgDefaultFor({type:'bool'}))`, `false`},
		{"defaultFor number", `String(PURE.cfgDefaultFor({type:'number'}))`, `null`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := evalString(t, vm, c.expr); got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestPureCoerce(t *testing.T) {
	vm := newPureVM(t)
	cases := []struct{ name, expr, want string }{
		{"number from text", `String(PURE.cfgCoerce('number', {value:'42'}))`, `42`},
		{"number empty -> null", `String(PURE.cfgCoerce('number', {value:'  '}))`, `null`},
		{"bool from checked", `String(PURE.cfgCoerce('bool', {checked:true}))`, `true`},
		{"string[] splits + trims", `JSON.stringify(PURE.cfgCoerce('string[]', {value:' a \n\n b '}))`, `["a","b"]`},
		{"stringMap parses KEY=VALUE", `JSON.stringify(PURE.cfgCoerce('stringMap', {value:'A=1\nB=two'}))`, `{"A":"1","B":"two"}`},
		{"kvCoerce true -> bool", `String(PURE.cfgKvCoerce('true'))`, `true`},
		{"kvCoerce numeric -> number", `String(PURE.cfgKvCoerce('3.14'))`, `3.14`},
		{"kvCoerce text stays string", `PURE.cfgKvCoerce('dolphin')`, `dolphin`},
		{"kvCoerce empty -> empty string", `'['+PURE.cfgKvCoerce('  ')+']'`, `[]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := evalString(t, vm, c.expr); got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestPureScanRequired(t *testing.T) {
	vm := newPureVM(t)
	// One required leaf, nested inside an object — empty draft should report it.
	schema := `[{id:'svc',type:'object',fields:[{id:'name',type:'text',required:true,label:'Name'}]}]`
	if got := evalString(t, vm, `String(PURE.cfgScanRequired(`+schema+`, {svc:{}}))`); got != "Name" {
		t.Errorf("missing required: got %q want %q", got, "Name")
	}
	if got := evalString(t, vm, `String(PURE.cfgScanRequired(`+schema+`, {svc:{name:'x'}}))`); got != "null" {
		t.Errorf("satisfied required: got %q want null", got)
	}
}

func TestPureFormatScalar(t *testing.T) {
	vm := newPureVM(t)
	cases := []struct{ name, expr, want string }{
		{"bool true", `PURE.formatScalar(true)`, `true`},
		{"null -> empty", `'['+PURE.formatScalar(null)+']'`, `[]`},
		{"number", `PURE.formatScalar(42)`, `42`},
		{"non-iso string passes through", `PURE.formatScalar('relay-llm')`, `relay-llm`},
		{"port-like number not treated as date", `PURE.formatScalar(8090)`, `8090`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := evalString(t, vm, c.expr); got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
	// An old ISO timestamp renders as a relative "... ago" string.
	if got := evalString(t, vm, `PURE.formatScalar('2020-01-01T00:00:00Z')`); !strings.HasSuffix(got, "ago") {
		t.Errorf("iso relative: got %q, want a '... ago' string", got)
	}
}

// domShim is a minimal document/window/navigator surface so the full app bundle
// can load and run its bootstrap render() without a real browser.
const domShim = `
var window = globalThis;
window.__RELAY_INIT__ = { externalMcps: [], services: [], runningIds: [], projects: [], mcpToolCache: {} };
var navigator = { clipboard: { writeText: function(){} } };
var console = { log:function(){}, warn:function(){}, error:function(){} };
function setTimeout(){ return 0; }
var __els = {};
function __mkEl(id){
  return {
    _id:id, _html:'', value:'', checked:false, disabled:false, textContent:'', placeholder:'',
    dataset:{}, style:{},
    classList:{ toggle:function(){}, add:function(){}, remove:function(){} },
    querySelector:function(){ return null; }, closest:function(){ return null; },
    appendChild:function(){}, remove:function(){},
    get innerHTML(){ return this._html; }, set innerHTML(v){ this._html = String(v); }
  };
}
var document = {
  getElementById:function(id){ if(!__els[id]) __els[id]=__mkEl(id); return __els[id]; },
  querySelector:function(){ return null; },
  querySelectorAll:function(){ return []; },
  addEventListener:function(){},
  createElement:function(){ return __mkEl('_created'); },
  body:{ appendChild:function(){} }
};
`

// newAppVM loads the full app bundle under the DOM shim.
func newAppVM(t *testing.T) *goja.Runtime {
	t.Helper()
	vm := goja.New()
	if _, err := vm.RunString(domShim); err != nil {
		t.Fatalf("dom shim: %v", err)
	}
	if _, err := vm.RunString(bundleForTest(t, "web/src/entry.js", "")); err != nil {
		t.Fatalf("app bundle threw on load (likely a missing import or un-globalized handler): %v", err)
	}
	return vm
}

// TestSettingsBundleSmoke asserts the whole app loads + renders without throwing.
// This is the safety net for the module split: a missing import or an
// un-globalized inline handler shows up here as a thrown error.
func TestSettingsBundleSmoke(t *testing.T) {
	vm := newAppVM(t)

	// Globalized handlers are present on window.
	for _, fn := range []string{"render", "showPage", "renderServices", "renderServiceInspector", "cfgEdit", "saveProjectForm"} {
		if got := evalString(t, vm, `typeof window.`+fn); got != "function" {
			t.Errorf("window.%s: typeof = %q, want function", fn, got)
		}
	}
	// The services renderer produces its heading.
	if got := evalString(t, vm, `window.renderServices().indexOf('Services') >= 0`); got != "true" {
		t.Errorf("renderServices() did not contain 'Services'")
	}
	// Switching to the inspector tab (empty status) renders the empty state and
	// does not throw — exercises the surgical-update code path's siblings.
	if got := evalString(t, vm, `(function(){ window.showPage('inspector'); return window.renderServiceInspector().indexOf('Service Inspector') >= 0; })()`); got != "true" {
		t.Errorf("inspector render missing heading")
	}
}

// TestProjectFormTemplatesReadOnly pins the ownership split for chat templates:
// relay stores them on the project record, Eve edits them. The settings form
// must render templates as a read-only summary (no add/remove/edit controls)
// and must omit chat_templates from the save payload entirely — update_project
// treats an absent field as "leave unchanged", so any value sent here would
// overwrite edits made in Eve.
func TestProjectFormTemplatesReadOnly(t *testing.T) {
	vm := newAppVM(t)

	script := `(function(){
		window.state.projects = [{id:'p1', name:'Proj', path:'/tmp/p',
			allowed_mcp_ids:['*'], allowed_models:['*'], disabled_tools:{},
			chat_templates:[{id:'t1', name:'Default', model:'claude-sonnet', mode:'voice'}]}];
		window.editProject('p1');
		var html = window.renderProjectForm();
		var payload = window.harvestProjectForm();
		return JSON.stringify({
			showsName: html.indexOf('Default') >= 0,
			showsModel: html.indexOf('claude-sonnet') >= 0,
			showsVoice: html.indexOf('voice') >= 0,
			noEditor: html.indexOf('addProjTemplate') < 0 && html.indexOf('removeProjTemplate') < 0,
			payloadOmits: !('chat_templates' in payload)
		});
	})()`

	got := evalString(t, vm, script)
	for _, want := range []string{
		`"showsName":true`, `"showsModel":true`, `"showsVoice":true`,
		`"noEditor":true`, `"payloadOmits":true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("project form templates: missing %s in %s", want, got)
		}
	}
}

// TestStatusPollPreservesConfigRegion is the regression test for the focus-clobber
// bug: a steady-state status poll (same set of services) must update ONLY a
// service's #svc-status-<id> region and leave #svc-config-<id> — where an open
// config editor and its focused input live — completely untouched.
func TestStatusPollPreservesConfigRegion(t *testing.T) {
	vm := newAppVM(t)

	// A snapshot for a service that declares a config (so a #svc-config region
	// exists), parameterized by a status value we can detect in the status region.
	snap := func(sessions int) string {
		return `{serviceId:'relay-llm', ok:true, fetchedAt:1, status:{sessions:` +
			itoaTest(sessions) + `}, manifest:{routes:['/x'], status:{path:'/s'}, ` +
			`config:{label:'cfg', schema:[{id:'a', type:'text', label:'A'}]}}}`
	}

	script := `(function(){
		window.showPage('inspector');
		window.onServiceStatusBatch([` + snap(95) + `]);   // empty -> {relay-llm}: full render
		// Stand in for an open editor with a focused field in the config region.
		document.getElementById('svc-config-relay-llm').innerHTML = 'EDITOR_SENTINEL';
		document.getElementById('svc-status-relay-llm').innerHTML = 'OLD_STATUS';
		window.onServiceStatusBatch([` + snap(96) + `]);   // same id set: surgical status-only update
		return JSON.stringify({
			config: document.getElementById('svc-config-relay-llm').innerHTML,
			statusHas96: document.getElementById('svc-status-relay-llm').innerHTML.indexOf('96') >= 0,
			statusChanged: document.getElementById('svc-status-relay-llm').innerHTML !== 'OLD_STATUS'
		});
	})()`

	got := evalString(t, vm, script)
	// config region untouched; status region refreshed with the new value.
	if !strings.Contains(got, `"config":"EDITOR_SENTINEL"`) {
		t.Errorf("config region was clobbered by the poll: %s", got)
	}
	if !strings.Contains(got, `"statusHas96":true`) {
		t.Errorf("status region did not pick up the new status value: %s", got)
	}
	if !strings.Contains(got, `"statusChanged":true`) {
		t.Errorf("status region was not updated: %s", got)
	}
}

// TestActionPendingKeyCanonical is the regression test for the stuck-button bug:
// the pending key is stored from the row in one key order (the service's status
// JSON) and cleared from the row echoed back by Go (which marshals map keys
// alphabetically). canonRowKey must make both keys match so the result clears
// the pending entry and the action button re-enables.
func TestActionPendingKeyCanonical(t *testing.T) {
	vm := newAppVM(t)
	script := `(function(){
		window.dispatchServiceAction('relay-llm','stop-instance',{name:'x',port:8004});
		var afterDispatch = Object.keys(state.serviceActionPending).length;
		// Go echoes the row with sorted keys (port before name -> name? sorted: name,port).
		window.onServiceActionResult({serviceId:'relay-llm',actionId:'stop-instance',row:{port:8004,name:'x'},ok:true});
		return JSON.stringify({afterDispatch:afterDispatch, afterResult:Object.keys(state.serviceActionPending).length});
	})()`
	got := evalString(t, vm, script)
	if !strings.Contains(got, `"afterDispatch":1`) {
		t.Errorf("dispatch did not record a pending entry: %s", got)
	}
	if !strings.Contains(got, `"afterResult":0`) {
		t.Errorf("reordered-key result failed to clear the pending entry (button would stay stuck): %s", got)
	}
}

// itoaTest is a tiny int->string for building JS literals in tests.
func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
