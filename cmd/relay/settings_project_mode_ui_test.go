package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// p1 both, p2 work-only, p3 home-only with a name that needs escaping, p4 an
// access profile.
const projectModeFixture = `[
	{id:'p1', name:'Alpha', path:'/tmp/alpha', allowed_mcp_ids:[], allowed_models:['*'], disabled_tools:{}},
	{id:'p2', name:'Bravo', mode:'work', path:'/tmp/bravo', allowed_mcp_ids:[], allowed_models:['*'], disabled_tools:{}},
	{id:'p3', name:'Charlie <home>', mode:'home', path:'/tmp/charlie', allowed_mcp_ids:[], allowed_models:['*'], disabled_tools:{}},
	{id:'p4', name:'Delta', kind:'remote', path:'', allowed_mcp_ids:[], allowed_models:[], disabled_tools:{}}
]`

func TestProjectMode_Module(t *testing.T) {
	vm := goja.New()
	if _, err := vm.RunString(bundleForTest(t, "web/src/lib/project_mode.js", "PMODE") + `; var P = ` + projectModeFixture + `;`); err != nil {
		t.Fatalf("loading project_mode bundle: %v", err)
	}
	rows := func(defaults string) string {
		return `JSON.stringify(PMODE.defaultProjectAttentionRows(P, ` + defaults + `).map(function(r){ return r.text; }))`
	}
	cases := []struct{ name, expr, want string }{
		{"modes in order", `JSON.stringify(PMODE.DEFAULT_MODES)`, `["home","work"]`},
		{"missing mode is both", `PMODE.projMode(P[0])`, `both`},
		{"unknown mode is both", `PMODE.projMode({mode:'office'})`, `both`},
		{"stored mode", `PMODE.projMode(P[1])`, `work`},
		{"both includes home", `PMODE.modeIncludes('both', 'home')`, `true`},
		{"work excludes home", `PMODE.modeIncludes('work', 'home')`, `false`},
		{"labels", `[PMODE.modeLabel('home'), PMODE.modeLabel('work'), PMODE.modeLabel('both')].join()`, `Home,Work,Both`},
		{"access profile never eligible", `PMODE.isDefaultEligible(P[3], 'home')`, `false`},
		{"eligible for home", `JSON.stringify(PMODE.eligibleDefaultProjects(P, 'home').map(function(p){ return p.id; }))`, `["p1","p3"]`},
		{"eligible for work", `JSON.stringify(PMODE.eligibleDefaultProjects(P, 'work').map(function(p){ return p.id; }))`, `["p1","p2"]`},
		{"effective default drops an ineligible id", `PMODE.effectiveDefaultId(P, {home:'p2'}, 'home')`, ``},
		{"effective default keeps a valid id", `PMODE.effectiveDefaultId(P, {home:'p1'}, 'home')`, `p1`},
		{"default modes in order", `JSON.stringify(PMODE.defaultModesFor({work:'p1', home:'p1'}, 'p1'))`, `["home","work"]`},
		{"never configured: no gaps", `JSON.stringify(PMODE.defaultProjectGaps(P, null))`, `[]`},
		{"unset", rows(`{}`), `["No default project for Home.","No default project for Work."]`},
		{"missing", rows(`{home:'p-gone', work:'p1'}`), `["The default project for Home no longer exists."]`},
		{"ineligible, name escaped", rows(`{home:'p1', work:'p3'}`), `["Charlie &lt;home&gt; can't be the default project for Work."]`},
		{"all valid", rows(`{home:'p3', work:'p2'}`), `[]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := evalString(t, vm, c.expr); got != c.want {
				t.Errorf("got  %s\nwant %s", got, c.want)
			}
		})
	}
}

// ---- app tier ----------------------------------------------------------

func seedProjectModeVM(t *testing.T, defaults string) *goja.Runtime {
	t.Helper()
	vm := newAppVM(t)
	evalString(t, vm, `(function(){
		window.__sent = [];
		window.webkit = { messageHandlers: { ipc: { postMessage: function(m){ window.__sent.push(String(m)); } } } };
		window.state.page = 'projects';
		window.state.projects = `+projectModeFixture+`;
		window.state.defaultProject = `+defaults+`;
		return true;
	})()`)
	return vm
}

func pmLastSent(t *testing.T, vm *goja.Runtime, typ string) map[string]any {
	t.Helper()
	var last map[string]any
	for _, line := range strings.Split(sentMessages(t, vm), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["type"] == typ {
			last = m
		}
	}
	return last
}

func TestProjectMode_FormSectionOrderAndSave(t *testing.T) {
	cases := []struct{ name, id, pick string }{
		{"picking Work saves work", "p1", "work"},
		{"an untouched save keeps the stored mode", "p2", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vm := seedProjectModeVM(t, `null`)
			html := evalString(t, vm, `(window.editProject('`+c.id+`'), window.renderProjectForm())`)
			identity, mode, mcps := strings.Index(html, ">Identity<"), strings.Index(html, ">Mode<"), strings.Index(html, ">MCPs")
			if identity < 0 || mode < identity || mcps < mode {
				t.Errorf("section order Identity=%d Mode=%d MCPs=%d, want Identity < Mode < MCPs", identity, mode, mcps)
			}
			for _, m := range []string{"home", "work", "both"} {
				if !regexp.MustCompile(`<button[^>]*data-proj-mode="` + m + `"`).MatchString(html) {
					t.Errorf("no Mode button for %s", m)
				}
			}
			if c.pick != "" {
				evalString(t, vm, `(window.setProjMode('`+c.pick+`'), true)`)
			}
			evalString(t, vm, `(function(){
				var p = window.state.projects.find(function(x){ return x.id === '`+c.id+`'; });
				document.getElementById('projName').value = p.name;
				document.getElementById('projPath').value = p.path;
				window.__sent = [];
				window.saveProjectForm();
				return true;
			})()`)
			if msg := pmLastSent(t, vm, "update_project"); msg == nil || msg["mode"] != "work" {
				t.Errorf("update_project = %v, want mode work", msg)
			}
		})
	}
}

func pmSelectOptions(html, id string) (values []string, selected string) {
	sel := regexp.MustCompile(`(?s)<select[^>]*id="` + id + `"[^>]*>(.*?)</select>`).FindStringSubmatch(html)
	if sel == nil {
		return nil, ""
	}
	for _, o := range regexp.MustCompile(`<option[^>]*>`).FindAllString(sel[1], -1) {
		v := regexp.MustCompile(`value="([^"]*)"`).FindStringSubmatch(o)[1]
		values = append(values, v)
		if regexp.MustCompile(`\sselected[\s>]`).MatchString(o) {
			selected = v
		}
	}
	return values, selected
}

func TestDefaultProject_PanelOffersEligibleProjectsAndNone(t *testing.T) {
	vm := seedProjectModeVM(t, `{home:'p2', work:'p1'}`)
	html := evalString(t, vm, `window.renderProjects()`)
	if !strings.Contains(html, `id="projDefaults"`) {
		t.Fatalf("no defaults panel with local projects present\n%s", html)
	}
	for _, c := range []struct {
		id, want, selected string
	}{
		{"projDefaultHome", `,p1,p3`, ""},
		{"projDefaultWork", `,p1,p2`, "p1"},
	} {
		values, selected := pmSelectOptions(html, c.id)
		if strings.Join(values, ",") != c.want || selected != c.selected {
			t.Errorf("%s options %v selected %q, want %s selected %q", c.id, values, selected, c.want, c.selected)
		}
	}

	only := evalString(t, vm, `(window.state.projects = window.state.projects.filter(function(p){ return p.kind === 'remote'; }), window.renderProjects())`)
	if strings.Contains(only, `id="projDefaults"`) {
		t.Error("defaults panel shown with no local project")
	}
}

func TestDefaultProject_SelectSendsSetDefaultProject(t *testing.T) {
	vm := seedProjectModeVM(t, `null`)
	evalString(t, vm, `(window.setDefaultProject('home', 'p1'), true)`)
	if msg := pmLastSent(t, vm, "set_default_project"); msg == nil || msg["mode"] != "home" || msg["project_id"] != "p1" {
		t.Errorf("set_default_project = %v, want mode home project_id p1", msg)
	}
	evalString(t, vm, `(window.__sent = [], window.setDefaultProject('both', 'p1'), true)`)
	if sent := sentMessages(t, vm); sent != "" {
		t.Errorf("a mode with no default sent %s", sent)
	}
}

func pmCard(html, name string) string {
	for _, seg := range strings.Split(html, `class="proj-card"`)[1:] {
		if strings.Contains(seg, `class="proj-card-name">`+name+`<`) {
			return seg
		}
	}
	return ""
}

func TestDefaultProject_CardChipsFollowUpdates(t *testing.T) {
	vm := seedProjectModeVM(t, `null`)
	html := evalString(t, vm, `(window.onDefaultProjectUpdated({home:'p1', work:'p2'}), window.renderProjects())`)
	for _, c := range []struct {
		name         string
		badge, chips int
	}{
		{"Alpha", 0, 1}, {"Bravo", 1, 1}, {"Charlie &lt;home&gt;", 1, 0}, {"Delta", 0, 0},
	} {
		card := pmCard(html, c.name)
		if card == "" {
			t.Fatalf("no card for %s\n%s", c.name, html)
		}
		if n := strings.Count(card, "proj-mode-badge"); n != c.badge {
			t.Errorf("%s: %d mode badges, want %d", c.name, n, c.badge)
		}
		if n := strings.Count(card, "proj-default-chip"); n != c.chips {
			t.Errorf("%s: %d default chips, want %d", c.name, n, c.chips)
		}
	}
}

func pmAttentionTexts(t *testing.T, vm *goja.Runtime) string {
	t.Helper()
	return evalString(t, vm, `JSON.stringify(window.overviewAttentionRows().map(function(r){ return r.text; }))`)
}

func TestDefaultProject_NeedsAttentionFollowsRemoveAndReload(t *testing.T) {
	vm := seedProjectModeVM(t, `{home:'p1', work:'p2'}`)
	if got := pmAttentionTexts(t, vm); strings.Contains(got, "default project") {
		t.Fatalf("valid defaults raised %s", got)
	}
	evalString(t, vm, `(window.onProjectRemoved('p1'), true)`)
	if got := pmAttentionTexts(t, vm); !strings.Contains(got, "No default project for Home.") {
		t.Errorf("after deleting the home default: %s", got)
	}
	if n := strings.Count(evalString(t, vm, `window.renderProjects()`), "proj-default-chip"); n != 1 {
		t.Errorf("%d default chips after deleting the home default, want only Bravo's", n)
	}
	evalString(t, vm, `(window.onSettingsReloaded({external_mcps:[], services:[], running_ids:[], default_project:null}), true)`)
	if got := pmAttentionTexts(t, vm); strings.Contains(got, "default project") {
		t.Errorf("a never-configured block raised %s", got)
	}
}
