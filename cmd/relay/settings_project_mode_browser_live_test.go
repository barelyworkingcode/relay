//go:build live

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
)

// projectModeBridge stands in for the WKWebView bridge: it records every
// message and answers list_models, update_project and set_default_project
// asynchronously, as relay does.
const projectModeBridge = `<script>
(function () {
  window.__sent = [];
  window.webkit = { messageHandlers: { ipc: { postMessage: function (raw) {
    var msg = JSON.parse(raw);
    window.__sent.push(msg);
    setTimeout(function () {
      if (msg.type === 'list_models') window.onModelsListed(` + pickerCatalogOK + `);
      if (msg.type === 'update_project') {
        var stored = window.state.projects.find(function (p) { return p.id === msg.id; });
        window.onProjectUpdated(Object.assign({}, stored, msg));
      }
      if (msg.type === 'set_default_project') {
        var next = Object.assign({}, window.state.defaultProject || {});
        next[msg.mode] = msg.project_id;
        window.onDefaultProjectUpdated(next);
      }
    }, 0);
  } } } };
})();
</script>`

// The seeded block names a deleted project for home, so the seed itself is
// what raises the Needs-attention row.
func serveProjectModeSettings(t *testing.T) string {
	t.Helper()
	mkSandboxRelayHome(t)
	settings := &config.Settings{
		Projects: []config.Project{
			{ID: "p1", Name: "Acme", Path: "/tmp/acme", AllowedMcpIDs: []string{}, AllowedModels: []string{"*"}},
			{ID: "p2", Name: "Acme work", Mode: config.ProjectModeWork, Path: "/tmp/acme-work", AllowedMcpIDs: []string{}, AllowedModels: []string{"*"}},
		},
		DefaultProject: &config.DefaultProjects{Home: "p-gone", Work: "p2"},
	}
	html := strings.Replace(renderSettingsHTML(settings, nil, nil, nil), "<body>", "<body>\n"+projectModeBridge, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, html)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestProjectModeBrowser(t *testing.T) {
	if fi, err := os.Stat(chromeBinary); err != nil || fi.IsDir() {
		t.Skipf("%s not installed; install Chrome and re-run with -tags=live", chromeBinary)
	}
	base := serveProjectModeSettings(t)
	page := startChromePage(t)

	cases := []struct {
		name string
		run  func(t *testing.T, p *chromePage)
	}{
		{"Work button saves mode work", func(t *testing.T, p *chromePage) {
			p.eval(t, `(showPage('projects'), editProject('p1'), true)`, nil)
			waitForJS(t, p, `window.state.projectForm && !window.state.modelCatalogPending && document.querySelector('[data-proj-mode="work"]')`)
			p.eval(t, `(window.__sent = [], document.querySelector('[data-proj-mode="work"]').click(), true)`, nil)
			p.eval(t, `(Array.from(document.querySelectorAll('#content button')).find(function (b) { return b.textContent.trim() === 'Save'; }).click(), true)`, nil)
			waitForJS(t, p, `!window.state.projectForm`)
			if got := evalJSON(t, p, `window.__sent.filter(function (m) { return m.type === 'update_project'; }).pop().mode`); got != `"work"` {
				t.Errorf("update_project mode = %s, want \"work\"", got)
			}
		}},
		{"seeded panel, then a select change sets the default", func(t *testing.T, p *chromePage) {
			p.eval(t, `(showPage('projects'), true)`, nil)
			waitForJS(t, p, `document.getElementById('projDefaultHome') && document.getElementById('projDefaultWork')`)
			if got := evalJSON(t, p, `[document.getElementById('projDefaultHome').value, document.getElementById('projDefaultWork').value]`); got != `["","p2"]` {
				t.Errorf("seeded [home, work] selects = %s, want [\"\",\"p2\"]", got)
			}
			p.eval(t, `(function (s) { s.value = 'p1'; s.dispatchEvent(new Event('change', {bubbles: true})); return true; })(document.getElementById('projDefaultHome'))`, nil)
			waitForJS(t, p, `window.__sent.some(function (m) { return m.type === 'set_default_project' && m.mode === 'home' && m.project_id === 'p1'; })`)
			waitForJS(t, p, `Array.from(document.querySelectorAll('.proj-card')).some(function (c) { return c.querySelector('.proj-card-name').textContent === 'Acme' && c.querySelector('.proj-default-chip'); })`)
		}},
		{"a deleted default shows in Needs attention", func(t *testing.T, p *chromePage) {
			p.eval(t, `(showPage('overview'), true)`, nil)
			waitForJS(t, p, `document.getElementById('content').textContent.indexOf('The default project for Home no longer exists.') >= 0`)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			openSettings(t, page, base+"/")
			c.run(t, page)
		})
	}
}
