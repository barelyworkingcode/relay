//go:build live

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
)

// modelPickerBridge stands in for the WKWebView bridge. It records every
// message and answers list_models from ?catalog=ok|unavailable. Answers are
// async, as relay's are: requestModelCatalog runs before the form's render.
const modelPickerBridge = `<script>
(function () {
  var catalog = new URLSearchParams(location.search).get('catalog') === 'unavailable'
    ? ` + pickerCatalogUnavailable + ` : ` + pickerCatalogOK + `;
  window.__sent = [];
  window.webkit = { messageHandlers: { ipc: { postMessage: function (raw) {
    var msg = JSON.parse(raw);
    window.__sent.push(msg);
    setTimeout(function () {
      if (msg.type === 'list_models') window.onModelsListed(catalog);
      if (msg.type === 'update_project') {
        var stored = window.state.projects.find(function (p) { return p.id === msg.id; });
        window.onProjectUpdated(Object.assign({}, stored, msg));
      }
    }, 0);
  } } } };
})();
</script>`

func serveModelPickerSettings(t *testing.T) string {
	t.Helper()
	mkSandboxRelayHome(t)
	settings := &config.Settings{Projects: []config.Project{
		{ID: "p1", Name: "Acme", Path: "/tmp/acme", AllowedMcpIDs: []string{}, AllowedModels: []string{"gone-model", "haiku"}},
	}}
	html := strings.Replace(renderSettingsHTML(settings, nil, nil, nil), "<body>", "<body>\n"+modelPickerBridge, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, html)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func openSettings(t *testing.T, p *chromePage, url string) {
	t.Helper()
	var nav struct {
		ErrorText string `json:"errorText"`
	}
	p.call(t, "Page.navigate", map[string]any{"url": url}, &nav)
	if nav.ErrorText != "" {
		t.Fatalf("navigate to %s: %s", url, nav.ErrorText)
	}
	waitForJS(t, p, `document.readyState === "complete" && typeof window.showPage === "function" && !!document.getElementById("content")`)
}

func waitForJS(t *testing.T, p *chromePage, cond string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var ok bool
		p.eval(t, `!!(`+cond+`)`, &ok)
		if ok {
			return
		}
		time.Sleep(cdpPollStep)
	}
	t.Fatalf("timed out waiting for %s", cond)
}

func evalJSON(t *testing.T, p *chromePage, expr string) string {
	t.Helper()
	var out string
	p.eval(t, `JSON.stringify(`+expr+`)`, &out)
	return out
}

func editProjectP1(t *testing.T, p *chromePage) {
	t.Helper()
	p.eval(t, `(showPage('projects'), editProject('p1'), true)`, nil)
	waitForJS(t, p, `window.state.projectForm && !window.state.modelCatalogPending && document.getElementById('projModelsList')`)
}

func clickModelBox(t *testing.T, p *chromePage, id string) {
	t.Helper()
	p.eval(t, `(document.querySelector('input[data-model-id="' + CSS.escape(`+jsQuote(id)+`) + '"]').click(), true)`, nil)
}

func saveAndReadModels(t *testing.T, p *chromePage) string {
	t.Helper()
	p.eval(t, `(Array.from(document.querySelectorAll('#content button')).find(function (b) { return b.textContent.trim() === 'Save'; }).click(), true)`, nil)
	waitForJS(t, p, `!window.state.projectForm`)
	return evalJSON(t, p, `window.__sent.filter(function (m) { return m.type === 'update_project'; }).pop().allowed_models`)
}

const checkedModelsJS = `Array.from(document.querySelectorAll('#projModelsList input[data-model-id]')).filter(function (e) { return e.checked; }).map(function (e) { return e.dataset.modelId; }).sort()`
const listedModelsJS = `Array.from(document.querySelectorAll('#projModelsList input[data-model-id]')).map(function (e) { return e.dataset.modelId; })`

func TestModelPickerBrowser(t *testing.T) {
	if fi, err := os.Stat(chromeBinary); err != nil || fi.IsDir() {
		t.Skipf("%s not installed; install Chrome and re-run with -tags=live", chromeBinary)
	}
	base := serveModelPickerSettings(t)
	page := startChromePage(t)
	page.call(t, "Emulation.setFocusEmulationEnabled", map[string]any{"enabled": true}, nil)

	cases := []struct {
		name    string
		catalog string
		run     func(t *testing.T, p *chromePage)
	}{
		{"save round-trip", "ok", func(t *testing.T, p *chromePage) {
			clickModelBox(t, p, "Chat")
			if got := saveAndReadModels(t, p); got != `["gone-model","haiku","Chat"]` {
				t.Errorf("saved %s", got)
			}
			editProjectP1(t, p)
			if got := evalJSON(t, p, checkedModelsJS); got != `["Chat","gone-model","haiku"]` {
				t.Errorf("reopened with %s checked", got)
			}
		}},
		{"unavailable id preserved on save", "ok", func(t *testing.T, p *chromePage) {
			if got := evalJSON(t, p, `document.querySelector('input[data-model-id="gone-model"]').closest('label').textContent`); !strings.Contains(got, "not currently available") {
				t.Errorf("gone-model row not marked: %s", got)
			}
			if got := saveAndReadModels(t, p); got != `["gone-model","haiku"]` {
				t.Errorf("untouched save sent %s", got)
			}
		}},
		{"wildcard hides the list", "ok", func(t *testing.T, p *chromePage) {
			p.eval(t, `(document.querySelector('input[aria-label^="Allow all models"]').click(), true)`, nil)
			if got := evalJSON(t, p, `[!!document.getElementById('projModelsList'), !!document.getElementById('projModelsSearch')]`); got != `[false,false]` {
				t.Errorf("wildcard on: list/search present = %s", got)
			}
			if got := saveAndReadModels(t, p); got != `["*"]` {
				t.Errorf("wildcard save sent %s", got)
			}
		}},
		{"search filters and keeps focus", "ok", func(t *testing.T, p *chromePage) {
			p.eval(t, `(document.getElementById('projModelsSearch').focus(), true)`, nil)
			for _, ch := range "tts" {
				p.call(t, "Input.insertText", map[string]any{"text": string(ch)}, nil)
				if got := evalJSON(t, p, `document.activeElement && document.activeElement.id`); got != `"projModelsSearch"` {
					t.Fatalf("after typing %q focus is on %s", ch, got)
				}
			}
			if got := evalJSON(t, p, listedModelsJS); got != `["acme-llm/kokoro-tts"]` {
				t.Errorf("search 'tts' lists %s", got)
			}
		}},
		{"Other collapsed then expands", "ok", func(t *testing.T, p *chromePage) {
			const other = `!!document.querySelector('input[data-model-id="acme-llm/kokoro-tts"]')`
			if got := evalJSON(t, p, other); got != "false" {
				t.Fatal("Other rows visible before expanding")
			}
			p.eval(t, `(document.getElementById('projModelsOtherToggle').click(), true)`, nil)
			if got := evalJSON(t, p, other); got != "true" {
				t.Error("Other rows hidden after expanding")
			}
		}},
		{"alias label", "ok", func(t *testing.T, p *chromePage) {
			got := evalJSON(t, p, `(function (l) { return [l.textContent, !!l.querySelector('.model-alias-tag')]; })(document.querySelector('input[data-model-id="Chat"]').closest('label'))`)
			if !strings.Contains(got, "Chat → acme-llm/Chat") || !strings.HasSuffix(got, "true]") {
				t.Errorf("alias row = %s", got)
			}
		}},
		{"catalog unavailable keeps saved ids", "unavailable", func(t *testing.T, p *chromePage) {
			if got := evalJSON(t, p, `(document.getElementById('projModelsBanner') || {}).textContent`); !strings.Contains(got, "session host unavailable") {
				t.Errorf("banner = %s", got)
			}
			if got := evalJSON(t, p, checkedModelsJS); got != `["gone-model","haiku"]` {
				t.Errorf("checked = %s", got)
			}
			if got := evalJSON(t, p, `document.querySelectorAll('.model-unavailable').length`); got != "0" {
				t.Errorf("%s saved ids marked unavailable with no list to compare against", got)
			}
			if got := saveAndReadModels(t, p); got != `["gone-model","haiku"]` {
				t.Errorf("save sent %s", got)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			openSettings(t, page, base+"/?catalog="+c.catalog)
			editProjectP1(t, page)
			c.run(t, page)
		})
	}
}
