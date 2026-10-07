package main

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/dop251/goja"

	"github.com/barelyworkingcode/relay/internal/config"
)

// Every row of the suitability table and the order the checks run in. The
// last four rows fail several checks at once.
func cosParityProjects() []config.Project {
	base := func(name string) config.Project {
		return config.Project{ID: "id-" + name, Name: name, AllowedModels: []string{"*"}, AllowedTemplates: []string{"claude-code"}}
	}
	with := func(name string, edit func(p *config.Project)) config.Project {
		p := base(name)
		edit(&p)
		return p
	}
	policy := &config.PermissionPolicy{DefaultMode: "plan"}
	return []config.Project{
		base("suitable"),
		with("listed model", func(p *config.Project) { p.AllowedModels = []string{"haiku", "opus"} }),
		with("empty model list", func(p *config.Project) { p.AllowedModels = nil }),
		with("wildcard templates", func(p *config.Project) { p.AllowedTemplates = []string{"*"} }),
		with("access profile", func(p *config.Project) { p.Kind = config.ProjectKindRemote }),
		with("ssh host", func(p *config.Project) { p.HostID = "h1" }),
		with("policy", func(p *config.Project) { p.PermissionPolicy = policy }),
		with("model", func(p *config.Project) { p.AllowedModels = []string{"opus"} }),
		with("template", func(p *config.Project) { p.AllowedTemplates = []string{"pi"} }),
		with("no templates", func(p *config.Project) { p.AllowedTemplates = nil }),
		with("all fail", func(p *config.Project) {
			p.Kind, p.HostID, p.PermissionPolicy, p.AllowedModels, p.AllowedTemplates = config.ProjectKindRemote, "h1", policy, []string{"opus"}, nil
		}),
		with("host policy model template", func(p *config.Project) {
			p.HostID, p.PermissionPolicy, p.AllowedModels, p.AllowedTemplates = "h1", policy, []string{"opus"}, nil
		}),
		with("policy model template", func(p *config.Project) {
			p.PermissionPolicy, p.AllowedModels, p.AllowedTemplates = policy, []string{"opus"}, nil
		}),
		with("model template", func(p *config.Project) { p.AllowedModels, p.AllowedTemplates = []string{"opus"}, nil }),
	}
}

func TestChiefOfStaff_JSAndGoAgreeOnEveryReason(t *testing.T) {
	vm := goja.New()
	if _, err := vm.RunString(bundleForTest(t, "web/src/lib/chief_of_staff.js", "COS")); err != nil {
		t.Fatalf("loading chief_of_staff bundle: %v", err)
	}
	projects := cosParityProjects()
	seen := map[string]bool{}
	for _, model := range config.ChiefOfStaffModels {
		for i := range projects {
			p := &projects[i]
			// Marshalled by hand: a Project's sealed token refuses to serialise.
			raw, err := json.Marshal(map[string]any{
				"id": p.ID, "name": p.Name, "kind": p.Kind, "host_id": p.HostID, "permission_policy": p.PermissionPolicy,
				"allowed_models": p.AllowedModels, "allowed_templates": p.AllowedTemplates,
			})
			if err != nil {
				t.Fatal(err)
			}
			want := config.ChiefOfStaffUnsuitable(p, model)
			seen[want] = true
			got := evalString(t, vm, `COS.chiefOfStaffUnsuitable(`+string(raw)+`, `+`'`+model+`')`)
			if got != want {
				t.Errorf("%s on %s: JS %q, Go %q", p.Name, model, got, want)
			}
		}
	}
	for _, r := range []string{"", "It's an access profile.", "It runs on an SSH host.", "It has a permission policy.",
		"It doesn't allow the sonnet model.", "It doesn't allow the claude-code template."} {
		if !seen[r] {
			t.Errorf("table never produced %q; the parity check skips that row", r)
		}
	}
}

// ---- app tier ----------------------------------------------------------

const cosAppFixture = `[
	{id:'p1', name:'Zulu', kind:'', path:'/tmp/z', allowed_mcp_ids:[], allowed_models:['*'], allowed_templates:['claude-code'], disabled_tools:{}},
	{id:'p2', name:'Alpha', path:'/tmp/a', allowed_mcp_ids:[], allowed_models:[], allowed_templates:['*'], disabled_tools:{}},
	{id:'p3', name:'Bravo', path:'/tmp/b', allowed_mcp_ids:[], allowed_models:['*'], allowed_templates:['claude-code'], permission_policy:{default_mode:'plan'}, disabled_tools:{}},
	{id:'p4', name:'Opus only', path:'/tmp/o', allowed_mcp_ids:[], allowed_models:['opus'], allowed_templates:['claude-code'], disabled_tools:{}},
	{id:'p5', name:'No template', path:'/tmp/n', allowed_mcp_ids:[], allowed_models:['*'], allowed_templates:[], disabled_tools:{}},
	{id:'p6', name:'Remote profile', kind:'remote', path:'', allowed_mcp_ids:[], allowed_models:[], allowed_templates:[], disabled_tools:{}}
]`

func seedCoSVM(t *testing.T, view string) *goja.Runtime {
	t.Helper()
	vm := newAppVM(t)
	evalString(t, vm, `(function(){
		window.__sent = [];
		window.webkit = { messageHandlers: { ipc: { postMessage: function(m){ window.__sent.push(String(m)); } } } };
		window.state.page = 'projects';
		window.state.projects = `+cosAppFixture+`;
		window.onChiefOfStaffUpdated(`+view+`);
		return true;
	})()`)
	return vm
}

type cosOption struct {
	value, label string
	selected     bool
	disabled     bool
}

func cosOptions(html, id string) []cosOption {
	sel := regexp.MustCompile(`(?s)<select[^>]*id="` + id + `"[^>]*>(.*?)</select>`).FindStringSubmatch(html)
	if sel == nil {
		return nil
	}
	var out []cosOption
	for _, m := range regexp.MustCompile(`<option([^>]*)>([^<]*)</option>`).FindAllStringSubmatch(sel[1], -1) {
		out = append(out, cosOption{
			value:    regexp.MustCompile(`value="([^"]*)"`).FindStringSubmatch(m[1])[1],
			label:    m[2],
			selected: regexp.MustCompile(`\sselected`).MatchString(m[1]),
			disabled: regexp.MustCompile(`\sdisabled`).MatchString(m[1]),
		})
	}
	return out
}

func cosLabels(opts []cosOption) string {
	var l []string
	for _, o := range opts {
		l = append(l, o.label)
	}
	return strings.Join(l, "|")
}

func cosReasonLines(html string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`<li>([^<]*)</li>`).FindAllStringSubmatch(regexp.MustCompile(`(?s)<ul[^>]*id="cosUnsuitable".*?</ul>`).FindString(html), -1) {
		out = append(out, strings.ReplaceAll(m[1], "&#39;", "'"))
	}
	return out
}

func cosRender(t *testing.T, vm *goja.Runtime) string {
	t.Helper()
	return evalString(t, vm, `window.renderProjects()`)
}

func TestChiefOfStaffPanel_PickerListsOnlySuitableLocalProjectsAndReasonsFollowModel(t *testing.T) {
	vm := seedCoSVM(t, `{configured:false}`)
	html := cosRender(t, vm)
	for _, id := range []string{`id="cosProject"`, `aria-label="Chief of Staff project"`, `id="cosModel"`, `aria-label="Chief of Staff model"`, `id="cosDailyCalls"`, `aria-label="Chief of Staff daily model calls"`} {
		if !strings.Contains(html, id) {
			t.Errorf("panel lacks %s", id)
		}
	}
	if got := cosLabels(cosOptions(html, "cosProject")); got != "Not set|Alpha|Zulu" {
		t.Errorf("sonnet picker = %q, want Not set|Alpha|Zulu", got)
	}
	if got := cosOptions(html, "cosProject")[0]; got.value != "" || !got.selected {
		t.Errorf("first option = %+v, want Not set with value \"\" selected", got)
	}
	if got := cosLabels(cosOptions(html, "cosModel")); got != "Haiku|Sonnet|Opus" {
		t.Errorf("model options = %q", got)
	}
	want := []string{
		"Bravo: It has a permission policy.",
		"No template: It doesn't allow the claude-code template.",
		"Opus only: It doesn't allow the sonnet model.",
	}
	if got := cosReasonLines(html); !reflect.DeepEqual(got, want) {
		t.Errorf("reason lines = %q, want %q", got, want)
	}
	if strings.Contains(html, "Remote profile") && strings.Contains(html[strings.Index(html, `id="cosPanel"`):], "Remote profile") {
		t.Error("an access profile appears in the Chief of Staff panel")
	}

	evalString(t, vm, `(window.setChiefOfStaffModel('opus'), true)`)
	html = cosRender(t, vm)
	if got := cosLabels(cosOptions(html, "cosProject")); got != "Not set|Alpha|Opus only|Zulu" {
		t.Errorf("opus picker = %q, want Not set|Alpha|Opus only|Zulu", got)
	}
	if got := cosReasonLines(html); !reflect.DeepEqual(got, want[:2]) {
		t.Errorf("opus reason lines = %q, want %q", got, want[:2])
	}
}

func TestChiefOfStaffPanel_NoReasonListWhenEveryProjectSuits(t *testing.T) {
	vm := seedCoSVM(t, `{configured:false}`)
	evalString(t, vm, `(window.state.projects = window.state.projects.filter(function(p){ return p.id === 'p1' || p.id === 'p6'; }), true)`)
	if html := cosRender(t, vm); strings.Contains(html, `id="cosUnsuitable"`) {
		t.Errorf("reason list shown with nothing to explain:\n%s", html)
	}
}

func TestChiefOfStaffPanel_StoredValuesAndStaleProject(t *testing.T) {
	cases := []struct {
		name, view string
		wantLabels string
		sel        cosOption
	}{
		{"stored suitable project", `{configured:true, projectId:'p1', model:'haiku', dailyModelCalls:40}`, "Not set|Alpha|Zulu", cosOption{value: "p1", label: "Zulu", selected: true}},
		{"stored project no longer suits", `{configured:true, projectId:'p3', model:'haiku', dailyModelCalls:40}`, "Not set|Alpha|Zulu|Bravo", cosOption{value: "p3", label: "Bravo", selected: true, disabled: true}},
		{"stored project gone", `{configured:true, projectId:'p-gone', model:'haiku', dailyModelCalls:40}`, "Not set|Alpha|Zulu|Removed project", cosOption{value: "p-gone", label: "Removed project", selected: true, disabled: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			html := cosRender(t, seedCoSVM(t, c.view))
			opts := cosOptions(html, "cosProject")
			if got := cosLabels(opts); got != c.wantLabels {
				t.Errorf("options = %q, want %q", got, c.wantLabels)
			}
			var selected []cosOption
			for _, o := range opts {
				if o.selected {
					selected = append(selected, o)
				}
			}
			if len(selected) != 1 || selected[0] != c.sel {
				t.Errorf("selected = %+v, want exactly %+v", selected, c.sel)
			}
			if m := cosOptions(html, "cosModel"); !m[0].selected {
				t.Errorf("model options = %+v, want Haiku selected", m)
			}
			if !strings.Contains(html, `id="cosDailyCalls"`) || !regexp.MustCompile(`id="cosDailyCalls"[^>]*value="40"`).MatchString(html) {
				t.Error("daily calls does not show the stored 40")
			}
		})
	}
}

func TestChiefOfStaffPanel_DefaultsWhenNotSet(t *testing.T) {
	html := cosRender(t, seedCoSVM(t, `{configured:false}`))
	var model string
	for _, o := range cosOptions(html, "cosModel") {
		if o.selected {
			model = o.value
		}
	}
	if model != "sonnet" || !regexp.MustCompile(`id="cosDailyCalls"[^>]*value="100"`).MatchString(html) {
		t.Errorf("not-set defaults: model %q, want sonnet and 100 calls", model)
	}
}

func cosSent(t *testing.T, vm *goja.Runtime) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(sentMessages(t, vm), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func TestChiefOfStaffPanel_ChangesSendTheExactMessage(t *testing.T) {
	vm := seedCoSVM(t, `{configured:true, projectId:'p1', model:'haiku', dailyModelCalls:40}`)

	steps := []struct {
		name, js string
		want     map[string]any
	}{
		{"pick a project", `window.setChiefOfStaffProject('p2')`, map[string]any{"type": "set_chief_of_staff", "project_id": "p2", "model": "haiku", "daily_model_calls": float64(40)}},
		{"change the model", `window.setChiefOfStaffModel('opus')`, map[string]any{"type": "set_chief_of_staff", "project_id": "p1", "model": "opus", "daily_model_calls": float64(40)}},
		{"change the limit", `window.setChiefOfStaffDailyCalls('55')`, map[string]any{"type": "set_chief_of_staff", "project_id": "p1", "model": "opus", "daily_model_calls": float64(55)}},
		{"pick Not set", `window.setChiefOfStaffProject('')`, map[string]any{"type": "set_chief_of_staff", "project_id": "", "model": "opus", "daily_model_calls": float64(55)}},
	}
	for _, s := range steps {
		evalString(t, vm, `(window.__sent = [], `+s.js+`, true)`)
		if got := cosSent(t, vm); len(got) != 1 || !reflect.DeepEqual(got[0], s.want) {
			t.Fatalf("%s sent %v, want exactly %v", s.name, got, s.want)
		}
	}
}

func TestChiefOfStaffPanel_WhileNotSetModelAndLimitSendNothingUntilAProjectIsPicked(t *testing.T) {
	vm := seedCoSVM(t, `{configured:false}`)
	evalString(t, vm, `(window.setChiefOfStaffModel('haiku'), window.setChiefOfStaffDailyCalls('25'), true)`)
	if sent := sentMessages(t, vm); sent != "" {
		t.Fatalf("a change while Not set sent %s", sent)
	}
	evalString(t, vm, `(window.setChiefOfStaffProject('p1'), true)`)
	want := map[string]any{"type": "set_chief_of_staff", "project_id": "p1", "model": "haiku", "daily_model_calls": float64(25)}
	if got := cosSent(t, vm); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Errorf("picking a project sent %v, want the held model and limit in %v", got, want)
	}
}

func TestChiefOfStaffPanel_SettingsReloadUpdatesThePanel(t *testing.T) {
	vm := seedCoSVM(t, `{configured:false}`)
	evalString(t, vm, `(window.onSettingsReloaded({external_mcps:[], services:[], running_ids:[], chief_of_staff:{configured:true, projectId:'p2', model:'opus', dailyModelCalls:7}}), true)`)
	html := cosRender(t, vm)
	var proj, model string
	for _, o := range cosOptions(html, "cosProject") {
		if o.selected {
			proj = o.value
		}
	}
	for _, o := range cosOptions(html, "cosModel") {
		if o.selected {
			model = o.value
		}
	}
	if proj != "p2" || model != "opus" || !regexp.MustCompile(`id="cosDailyCalls"[^>]*value="7"`).MatchString(html) {
		t.Errorf("after reload: project %q model %q, want p2 opus 7", proj, model)
	}

	evalString(t, vm, `(window.onSettingsReloaded({external_mcps:[], services:[], running_ids:[], chief_of_staff:{configured:false}}), true)`)
	if opts := cosOptions(cosRender(t, vm), "cosProject"); !opts[0].selected {
		t.Errorf("after an unset reload: %+v, want Not set selected", opts)
	}
}
