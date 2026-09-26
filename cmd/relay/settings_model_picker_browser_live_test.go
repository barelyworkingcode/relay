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
// With ?hold=1 the list_models answer waits for window.__releaseModels().
const modelPickerBridge = `<script>
(function () {
  var params = new URLSearchParams(location.search);
  var catalog = params.get('catalog') === 'unavailable'
    ? ` + pickerCatalogUnavailable + ` : ` + pickerCatalogOK + `;
  var held = [];
  window.__releaseModels = function () {
    var q = held.splice(0);
    q.forEach(function (answer) { answer(); });
    return q.length;
  };
  window.__sent = [];
  window.webkit = { messageHandlers: { ipc: { postMessage: function (raw) {
    var msg = JSON.parse(raw);
    window.__sent.push(msg);
    if (msg.type === 'list_models' && params.get('hold') === '1') {
      held.push(function () { window.onModelsListed(catalog); });
      return;
    }
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

// openHeldP1 opens p1 with its list_models answer still held by the bridge.
func openHeldP1(t *testing.T, p *chromePage) {
	t.Helper()
	p.eval(t, `(showPage('projects'), editProject('p1'), true)`, nil)
	waitForJS(t, p, `window.state.modelCatalogPending && document.getElementById('projName') && document.getElementById('projModelsSearch')`)
}

// releaseCatalog delivers the held answer and waits for catalogRow, a row only
// the catalog has, so a surviving focus or text is never measured against a
// picker that did not repaint.
func releaseCatalog(t *testing.T, p *chromePage, catalogRow string) {
	t.Helper()
	if got := evalJSON(t, p, `window.__releaseModels()`); got != "1" {
		t.Fatalf("released %s list_models answers, want 1", got)
	}
	waitForJS(t, p, `!window.state.modelCatalogPending && document.querySelector('input[data-model-id="' + CSS.escape(`+jsQuote(catalogRow)+`) + '"]')`)
}

func insertText(t *testing.T, p *chromePage, text string) {
	t.Helper()
	p.call(t, "Input.insertText", map[string]any{"text": text}, nil)
}

// pressSpace activates the focused control the way a keyboard user does.
func pressSpace(t *testing.T, p *chromePage) {
	t.Helper()
	key := map[string]any{"type": "keyDown", "key": " ", "code": "Space", "windowsVirtualKeyCode": 32, "text": " "}
	p.call(t, "Input.dispatchKeyEvent", key, nil)
	key["type"] = "keyUp"
	delete(key, "text")
	p.call(t, "Input.dispatchKeyEvent", key, nil)
}

// focusThenSpace focuses the control, checks it took focus, then presses Space.
func focusThenSpace(t *testing.T, p *chromePage, control, focusedJS, want string) {
	t.Helper()
	p.eval(t, `(`+control+`.focus(), true)`, nil)
	if got := evalJSON(t, p, focusedJS); got != want {
		t.Fatalf("before Space focus is on %s, want %s", got, want)
	}
	pressSpace(t, p)
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
		held    bool
		run     func(t *testing.T, p *chromePage)
	}{
		{"catalog answer keeps focus and typed tools", "ok", true, func(t *testing.T, p *chromePage) {
			p.eval(t, `(document.getElementById('projAllowedTools').focus(), true)`, nil)
			insertText(t, p, "Read")
			p.eval(t, `(document.getElementById('projName').focus(), true)`, nil)
			releaseCatalog(t, p, "Chat")
			if got := evalJSON(t, p, `[document.activeElement && document.activeElement.id, document.getElementById('projAllowedTools').value]`); got != `["projName","Read"]` {
				t.Errorf("after the catalog answer [focus, allowed tools] = %s", got)
			}
		}},
		{"catalog answer keeps search focus and query", "ok", true, func(t *testing.T, p *chromePage) {
			p.eval(t, `(document.getElementById('projModelsSearch').focus(), true)`, nil)
			insertText(t, p, "tts")
			releaseCatalog(t, p, "acme-llm/kokoro-tts")
			if got := evalJSON(t, p, `[document.activeElement && document.activeElement.id, document.getElementById('projModelsSearch').value]`); got != `["projModelsSearch","tts"]` {
				t.Errorf("after the catalog answer [focus, search] = %s", got)
			}
		}},
		{"toggle keeps the list's scroll position", "ok", false, func(t *testing.T, p *chromePage) {
			// A style tag, not an inline style: it outlives a repaint of the list.
			p.eval(t, `(document.head.insertAdjacentHTML('beforeend', '<style>#projModelsList{max-height:40px!important}</style>'), document.getElementById('projModelsList').scrollTop = 30, true)`, nil)
			before := evalJSON(t, p, `document.getElementById('projModelsList').scrollTop`)
			if before == "0" {
				t.Fatal("the list did not scroll; the fixture is too short to measure")
			}
			clickModelBox(t, p, "acme-llm/Chat")
			waitForJS(t, p, `window.state.projectForm.allowed_models.indexOf('acme-llm/Chat') >= 0`)
			if got := evalJSON(t, p, `document.getElementById('projModelsList').scrollTop`); got != before {
				t.Errorf("scrollTop %s after a toggle, want %s", got, before)
			}
		}},
		{"save round-trip", "ok", false, func(t *testing.T, p *chromePage) {
			clickModelBox(t, p, "Chat")
			if got := saveAndReadModels(t, p); got != `["gone-model","haiku","Chat"]` {
				t.Errorf("saved %s", got)
			}
			editProjectP1(t, p)
			if got := evalJSON(t, p, checkedModelsJS); got != `["Chat","gone-model","haiku"]` {
				t.Errorf("reopened with %s checked", got)
			}
		}},
		{"unavailable id preserved on save", "ok", false, func(t *testing.T, p *chromePage) {
			if got := evalJSON(t, p, `document.querySelector('input[data-model-id="gone-model"]').closest('label').textContent`); !strings.Contains(got, "not currently available") {
				t.Errorf("gone-model row not marked: %s", got)
			}
			if got := saveAndReadModels(t, p); got != `["gone-model","haiku"]` {
				t.Errorf("untouched save sent %s", got)
			}
		}},
		{"wildcard hides the list", "ok", false, func(t *testing.T, p *chromePage) {
			p.eval(t, `(document.querySelector('input[aria-label^="Allow all models"]').click(), true)`, nil)
			if got := evalJSON(t, p, `[!!document.getElementById('projModelsList'), !!document.getElementById('projModelsSearch')]`); got != `[false,false]` {
				t.Errorf("wildcard on: list/search present = %s", got)
			}
			if got := saveAndReadModels(t, p); got != `["*"]` {
				t.Errorf("wildcard save sent %s", got)
			}
		}},
		{"search filters and keeps focus", "ok", false, func(t *testing.T, p *chromePage) {
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
		{"Other collapsed then expands", "ok", false, func(t *testing.T, p *chromePage) {
			const other = `!!document.querySelector('input[data-model-id="acme-llm/kokoro-tts"]')`
			if got := evalJSON(t, p, other); got != "false" {
				t.Fatal("Other rows visible before expanding")
			}
			p.eval(t, `(document.getElementById('projModelsOtherToggle').click(), true)`, nil)
			if got := evalJSON(t, p, other); got != "true" {
				t.Error("Other rows hidden after expanding")
			}
		}},
		{"Space on a model keeps focus on it", "ok", false, func(t *testing.T, p *chromePage) {
			const focused = `(document.activeElement && document.activeElement.dataset.modelId) || null`
			focusThenSpace(t, p, `document.querySelector('#projModelsList input[data-model-id="acme-llm/Chat"]')`, focused, `"acme-llm/Chat"`)
			waitForJS(t, p, `window.state.projectForm.allowed_models.indexOf('acme-llm/Chat') >= 0`)
			if got := evalJSON(t, p, focused); got != `"acme-llm/Chat"` {
				t.Errorf("after Space focus is on model %s", got)
			}
		}},
		{"Space on Other keeps focus on it", "ok", false, func(t *testing.T, p *chromePage) {
			const focused = `document.activeElement && document.activeElement.id`
			focusThenSpace(t, p, `document.getElementById('projModelsOtherToggle')`, focused, `"projModelsOtherToggle"`)
			waitForJS(t, p, `document.querySelector('input[data-model-id="acme-llm/kokoro-tts"]')`)
			if got := evalJSON(t, p, focused); got != `"projModelsOtherToggle"` {
				t.Errorf("after Space focus is on %s", got)
			}
		}},
		{"alias label", "ok", false, func(t *testing.T, p *chromePage) {
			got := evalJSON(t, p, `(function (l) { return [l.textContent, !!l.querySelector('.model-alias-tag')]; })(document.querySelector('input[data-model-id="Chat"]').closest('label'))`)
			if !strings.Contains(got, "Chat → acme-llm/Chat") || !strings.HasSuffix(got, "true]") {
				t.Errorf("alias row = %s", got)
			}
		}},
		{"catalog unavailable keeps saved ids", "unavailable", false, func(t *testing.T, p *chromePage) {
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
			if c.held {
				openSettings(t, page, base+"/?hold=1&catalog="+c.catalog)
				openHeldP1(t, page)
			} else {
				openSettings(t, page, base+"/?catalog="+c.catalog)
				editProjectP1(t, page)
			}
			c.run(t, page)
		})
	}
}
