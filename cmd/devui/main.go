// Command devui serves the Relay settings UI in an ordinary browser for visual
// QA and behavioral testing (theme/light-dark, layout, the status-poll focus
// regression). It is a DEVELOPER TOOL ONLY:
//
//   - It is a separate binary, never linked into Relay.app — there is zero
//     chance of it adding production attack surface.
//   - It binds 127.0.0.1 only, serves the static HTML with canned fixture data,
//     and answers IPC messages from a hard-coded mock. No Unix socket, no bearer
//     token, no real Settings mutators, nothing reads or writes user config.
//
// The page is the exact file the WKWebView loads, so layout/markup/CSS render
// faithfully. Note one fidelity caveat: the `-apple-system-*` CSS color
// keywords resolve only in WebKit (Safari/WKWebView), so in Chrome the standard
// CSS system-color fallbacks (Canvas/CanvasText/AccentColor) are what you see —
// good enough for layout + light/dark behavior, but final native color fidelity
// must be checked in the real app.
//
// Usage:
//
//	go run ./cmd/devui                       # serves web/dist/settings.html on :8765
//	go run ./cmd/devui -html web/dist/settings.html
//	go run ./cmd/devui -addr 127.0.0.1:9000
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
)

func main() {
	htmlPath := flag.String("html", "web/dist/settings.html", "path to the settings HTML file to serve")
	addr := flag.String("addr", "127.0.0.1:8765", "loopback address to listen on")
	flag.Parse()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		raw, err := os.ReadFile(*htmlPath)
		if err != nil {
			http.Error(w, "read "+*htmlPath+": "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// Read fresh on every request so editing the HTML + refreshing the tab
		// is the whole dev loop — no rebuild, no restart.
		if _, err := w.Write([]byte(buildPage(string(raw)))); err != nil {
			log.Printf("write response: %v", err)
		}
	})

	fmt.Printf("devui serving %s at http://%s/  (Ctrl-C to stop)\n", *htmlPath, *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

// buildPage substitutes the five init-data tokens with fixtures and injects the
// mock-IPC bridge (before the page script) plus the status-poll simulator
// (after it).
func buildPage(html string) string {
	html = strings.NewReplacer(
		"__EXTERNAL_MCPS_JSON__", fixtureExternalMcps,
		"__SERVICES_JSON__", fixtureServices,
		"__RUNNING_IDS_JSON__", fixtureRunningIDs,
		"__PROJECTS_JSON__", fixtureProjects,
		"__MCP_TOOL_CACHE_JSON__", fixtureMcpToolCache,
	).Replace(html)

	// The mock must define window.webkit BEFORE the page's ipc() runs, so it
	// goes right after <body> (the page script lives further down the body).
	html = strings.Replace(html, "<body>", "<body>\n"+mockBridgeScript, 1)
	// The poll simulator must run AFTER the page defines its window.onX handlers
	// and its bootstrap render(), so it goes just before </body>.
	html = strings.Replace(html, "</body>", pollSimScript+"\n</body>", 1)
	return html
}

// --- Fixtures -------------------------------------------------------------
//
// Representative, PII-free sample data that exercises every render path: stdio
// + HTTP MCPs, several services, a project, and (via the status batch below) a
// rich service manifest covering object/array/map/keyValue/leaf config nodes.

const fixtureExternalMcps = `[
  {"id":"fsmcp","display_name":"fsMCP","command":"/usr/local/bin/fsmcp","args":["--root","/Users/you"],"env":{},"transport":"stdio","tcc_services":[]},
  {"id":"macmcp","display_name":"macMCP","command":"/usr/local/bin/macmcp","args":[],"env":{},"transport":"stdio","tcc_services":["calendar","contacts"]},
  {"id":"krisp","display_name":"Krisp","args":[],"env":{},"transport":"http","url":"https://mcp.krisp.ai/mcp","oauth_state":{"access_token":"authenticated"}}
]`

const fixtureServices = `[
  {"id":"relay-llm","display_name":"Relay LLM","command":"/Users/you/source/relayLLM/relayllm","args":["--router-port","8180"],"env":{},"autostart":true},
  {"id":"kokoro-daemon","display_name":"kokoro-daemon","command":"/Users/you/source/kokoro/daemon/daemon_wrapper.sh","args":[],"env":{},"autostart":true},
  {"id":"stt-daemon","display_name":"STT Daemon","command":"/Users/you/source/whisper/daemon/daemon_wrapper.sh","args":[],"env":{},"autostart":true},
  {"id":"relaycomfy","display_name":"relaycomfy","command":"/Users/you/source/relayComfy/daemon/daemon_wrapper.sh","args":[],"env":{},"autostart":false,"url":"http://localhost:8188"}
]`

const fixtureRunningIDs = `["relay-llm","kokoro-daemon","stt-daemon"]`

const fixtureProjects = `[
  {"id":"proj-acme","name":"Acme Website","path":"/Users/you/projects/acme","allowed_mcp_ids":["*"],"allowed_models":["*"],"chat_templates":[{"id":"tpl-1","name":"Default","model":"claude-sonnet","system_prompt":"You are a helpful assistant.","append_claude_md":true,"use_relay_tools":true}],"permission_policy":{"default_mode":"acceptEdits","allowed_tools":["Read","Grep"],"denied_tools":["Bash(rm *)"]},"generate_skill":true,"token":"relay_proj_8f2a1c9d4e6b0a7f3c5d","disabled_tools":{}},
  {"id":"proj-internal","name":"Internal Tools","path":"/Users/you/projects/internal","allowed_mcp_ids":["fsmcp"],"allowed_models":["claude-opus","claude-sonnet"],"chat_templates":[],"permission_policy":{"default_mode":""},"generate_skill":false,"token":"relay_proj_1a2b3c4d5e6f7a8b9c0d","disabled_tools":{"fsmcp":["write_file"]}}
]`

const fixtureMcpToolCache = `{
  "fsmcp":[
    {"name":"read_file","description":"Read the contents of a file at the given path."},
    {"name":"write_file","description":"Write content to a file, creating it if needed."},
    {"name":"list_dir","description":"List entries in a directory."}
  ]
}`

// mockBridgeScript stands in for the WKWebView message bridge. ipc() in the page
// takes the window.webkit branch, so every IPC posts here; we answer a few op
// types with canned data and log the rest.
var mockBridgeScript = `<script>
(function () {
  var FIXTURE_CONFIG_TEXT = ` + jsString(fixtureConfigText) + `;
  var FIXTURE_TOOLS = ` + inlineJSON(fixtureMcpToolCacheTools) + `;
  window.webkit = { messageHandlers: { ipc: { postMessage: function (raw) {
    var msg; try { msg = JSON.parse(raw); } catch (e) { console.warn('[devui] bad ipc', raw); return; }
    console.log('[devui ipc →]', msg);
    setTimeout(function () { handle(msg); }, 140); // simulate round-trip latency
  } } } };
  function handle(msg) {
    switch (msg.type) {
      case 'service_config':
        if (msg.op === 'get') window.onServiceConfigResult({ serviceId: msg.serviceId, op: 'get', ok: true, text: FIXTURE_CONFIG_TEXT });
        else if (msg.op === 'save') { window.onServiceConfigResult({ serviceId: msg.serviceId, op: 'save', ok: true }); window.onServiceConfigApplied({ serviceId: msg.serviceId, mode: 'restarting' }); }
        break;
      case 'list_mcp_tools': window.onMcpToolsListed(msg.mcp_id, FIXTURE_TOOLS[msg.mcp_id] || []); break;
      case 'service_action': window.onServiceActionResult({ serviceId: msg.serviceId, actionId: msg.actionId, row: msg.row, ok: true }); break;
      // add/update/remove/start/stop etc. — no-op in the harness, just logged above
    }
  }
})();
</script>`

// pollSimScript reproduces the tray's 2-second status poll so the Service
// Inspector populates and the focus-clobber regression is reproducible: focus an
// input on the Inspector tab and watch whether a tick wipes it.
var pollSimScript = `<script>
(function () {
  var BATCH = ` + inlineJSON(fixtureStatusBatch) + `;
  function fire() {
    var b = JSON.parse(JSON.stringify(BATCH));
    for (var i = 0; i < b.length; i++) b[i].fetchedAt = Date.now();
    if (window.onServiceStatusBatch) window.onServiceStatusBatch(b);
  }
  setTimeout(fire, 200);
  setInterval(fire, 2000); // mirrors StatusPollInterval
})();
</script>`

// fixtureMcpToolCacheTools is the tool list keyed by MCP id, served to
// list_mcp_tools requests (the project tri-state picker).
const fixtureMcpToolCacheTools = fixtureMcpToolCache

// fixtureConfigText is the raw config file the mock returns for a config 'get'.
// JSONC-style comment included to exercise the comment stripper.
const fixtureConfigText = `{
  // relayLLM configuration (sample)
  "openai": { "baseUrl": "http://localhost:1234/v1", "apiKey": "sk-sample-key" },
  "llama": { "binaryPath": "/usr/local/bin/llama-server", "modelDir": "~/models/", "basePort": 8000 },
  "models": [
    { "alias": "Dolphin Mistral 24B RP", "ctx-size": 8192, "n-gpu-layers": 99, "flash-attn": true }
  ],
  "aliases": { "fast": "qwen-7b", "smart": "dolphin-24b" },
  "verbose": false,
  "logLevel": "info"
}`

// fixtureStatusBatch is one ServiceStatusSnapshot whose manifest exercises every
// config node type (object, array of objects with a rest:true keyValue, map,
// bool, select, number, text, secret) plus a forEach row action.
const fixtureStatusBatch = `[
  {
    "serviceId": "relay-llm",
    "ok": true,
    "fetchedAt": 0,
    "status": {
      "sessions": 95,
      "uptimeSeconds": 3559,
      "instances": [
        { "alias": "Dolphin Mistral 24B RP", "port": 8004, "pid": 21093, "startedAt": "2026-06-09T15:10:00Z", "healthy": true, "exited": false }
      ],
      "terminals": []
    },
    "manifest": {
      "routes": ["/api/sessions", "/ws"],
      "status": { "path": "/api/status" },
      "actions": [
        { "id": "stop-instance", "label": "Stop", "method": "DELETE", "pathTemplate": "/api/llama/{port}", "forEach": "instances" }
      ],
      "config": {
        "label": "settings.json",
        "help": "relayLLM configuration. Saving restarts relayLLM to apply.",
        "applyMode": "restart",
        "schema": [
          { "id": "openai", "type": "object", "label": "OpenAI-compatible endpoints", "help": "OpenAI-compatible API providers (LM Studio, Ollama, OpenAI, …).", "fields": [
            { "id": "baseUrl", "type": "text", "label": "Base URL" },
            { "id": "apiKey", "type": "secret", "label": "API key" }
          ] },
          { "id": "llama", "type": "object", "label": "llama.cpp server", "fields": [
            { "id": "binaryPath", "type": "text", "label": "Binary path" },
            { "id": "modelDir", "type": "text", "label": "Model directory", "help": "Base directory for relative model paths." },
            { "id": "basePort", "type": "number", "label": "Base port" }
          ] },
          { "id": "models", "type": "array", "label": "Models", "item": { "type": "object", "label": "model", "fields": [
            { "id": "alias", "type": "text", "label": "Alias" },
            { "id": "flags", "type": "keyValue", "rest": true, "keyLabel": "flag" }
          ] } },
          { "id": "aliases", "type": "map", "label": "Aliases", "keyLabel": "name", "item": { "type": "text" } },
          { "id": "verbose", "type": "bool", "label": "Verbose logging" },
          { "id": "logLevel", "type": "select", "label": "Log level", "options": ["debug", "info", "warn", "error"] }
        ]
      }
    }
  }
]`

// inlineJSON neutralizes any literal </ in JSON before it is spliced into an
// inline <script> as a JS value, so a string value containing </script> can't
// terminate the <script> element early. (<\/ is a valid escape inside a JS/JSON
// string; structural JSON contains no </, so this is a no-op there.)
func inlineJSON(s string) string {
	return strings.ReplaceAll(s, "</", "<\\/")
}

// jsString renders s as a safely-quoted JavaScript string literal (used to embed
// the raw config text as a JS value inside the mock script).
func jsString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '<':
			b.WriteString("\\u003c") // never let a literal </script> break out of the inline script
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
