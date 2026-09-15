// Command devui is a developer-only tool for visual QA: a separate binary,
// never linked into Relay.app, that binds 127.0.0.1 only and serves the
// settings UI with canned fixture data and no real Settings mutators.
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
	htmlPath := flag.String("html", "internal/webassets/settings.html", "path to the settings HTML file to serve")
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
		// Read fresh every request: editing the HTML and refreshing the tab is
		// the whole dev loop, no rebuild or restart.
		if _, err := w.Write([]byte(buildPage(string(raw)))); err != nil {
			log.Printf("write response: %v", err)
		}
	})

	fmt.Printf("devui serving %s at http://%s/  (Ctrl-C to stop)\n", *htmlPath, *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

// Every token renderSettingsHTML substitutes must appear here too: an
// unreplaced __X_JSON__ leaves the page's init object syntactically invalid
// and takes the whole bundle down on load.
func buildPage(html string) string {
	html = strings.NewReplacer(
		"__EXTERNAL_MCPS_JSON__", fixtureExternalMcps,
		"__SERVICES_JSON__", fixtureServices,
		"__RUNNING_IDS_JSON__", fixtureRunningIDs,
		"__PROJECTS_JSON__", fixtureProjects,
		"__HOSTS_JSON__", fixtureHosts,
		"__MCP_TOOL_CACHE_JSON__", fixtureMcpToolCache,
		"__MCP_SCOPE_FIELDS_JSON__", fixtureMcpScopeFields,
		"__ENROLMENTS_JSON__", fixtureEnrolments,
		"__REMOTE_JSON__", fixtureRemote,
		"__ENROLMENT_BUDGET_DEFAULTS_JSON__", fixtureEnrolmentBudgetDefaults,
		"__PASSKEYS_JSON__", fixturePasskeys,
		"__LOGIN_SESSIONS_JSON__", fixtureLoginSessions,
		"__LOGIN_CODE_JSON__", fixtureLoginCode,
		"__INITIAL_PAGE_JSON__", `""`,
		"__MCP_HEALTH_JSON__", fixtureMcpHealth,
		"__SERVICE_RUNTIME_JSON__", fixtureServiceRuntime,
		"__SEAL_STATUS_JSON__", fixtureSealStatus,
		"__VERSION_JSON__", fixtureVersion,
		"__PATHS_JSON__", fixturePaths,
	).Replace(html)

	// window.webkit must exist before the page's ipc() runs, so the mock goes
	// right after <body>.
	html = strings.Replace(html, "<body>", "<body>\n"+mockBridgeScript, 1)
	// The poll simulator must run after the page installs its window.onX
	// handlers and its bootstrap render(), so it goes just before </body>.
	html = strings.Replace(html, "</body>", pollSimScript+"\n</body>", 1)
	return html
}

const fixtureExternalMcps = `[
  {"id":"fsmcp","display_name":"fsMCP","command":"/usr/local/bin/fsmcp","args":["--root","/Users/you"],"env":{},"transport":"stdio","tcc_services":[]},
  {"id":"macmcp","display_name":"macMCP","command":"/usr/local/bin/macmcp","args":[],"env":{},"transport":"stdio","tcc_services":["calendar","contacts"]},
  {"id":"krisp","display_name":"Krisp","args":[],"env":{},"transport":"http","url":"https://mcp.krisp.ai/mcp","oauth_state":{"access_token":"authenticated"}}
]`

const fixtureServices = `[
  {"id":"relay-llm","display_name":"Relay LLM","command":"/Users/you/source/relayLLM/relayllm","args":["--router-port","8180"],"env":{},"autostart":true,"capabilities":["manifest","model_host"]},
  {"id":"relaytts-daemon","display_name":"relaytts-daemon","command":"/Users/you/source/relayTTS/daemon/daemon_wrapper.sh","args":[],"env":{},"autostart":true,"capabilities":["manifest"]},
  {"id":"stt-daemon","display_name":"STT Daemon","command":"/Users/you/source/whisper/daemon/daemon_wrapper.sh","args":[],"env":{},"autostart":true,"capabilities":["frontend"]},
  {"id":"relaycomfy","display_name":"relaycomfy","command":"/Users/you/source/relayComfy/daemon/daemon_wrapper.sh","args":[],"env":{},"autostart":false,"url":"http://localhost:8188"}
]`

const fixtureRunningIDs = `["relay-llm","relaytts-daemon","stt-daemon"]`

const fixtureProjects = `[
  {"id":"proj-acme","name":"Acme Website","path":"/Users/you/projects/acme","allowed_mcp_ids":["*"],"allowed_models":["*"],"chat_templates":[{"id":"tpl-1","name":"Default","model":"claude-sonnet","system_prompt":"You are a helpful assistant.","append_claude_md":true,"use_relay_tools":true}],"permission_policy":{"default_mode":"acceptEdits","allowed_tools":["Read","Grep"],"denied_tools":["Bash(rm *)"]},"generate_skill":true,"token":"relay_proj_8f2a1c9d4e6b0a7f3c5d","disabled_tools":{}},
  {"id":"proj-internal","name":"Internal Tools","path":"/Users/you/projects/internal","allowed_mcp_ids":["fsmcp"],"allowed_models":["claude-opus","claude-sonnet"],"chat_templates":[],"permission_policy":{"default_mode":""},"generate_skill":false,"allow_cwd_auth":true,"token":"relay_proj_1a2b3c4d5e6f7a8b9c0d","disabled_tools":{"fsmcp":["write_file"]}},
  {"id":"proj-lab","name":"Remote Lab","path":"/home/you/remote-lab","host_id":"h_devbox","allowed_mcp_ids":[],"allowed_models":["*"],"chat_templates":[],"permission_policy":{"default_mode":""},"generate_skill":false,"token":"relay_proj_9e8d7c6b5a4f3e2d1c0b","disabled_tools":{}},
  {"id":"proj-mail","name":"Mail (remote)","kind":"remote","allowed_mcp_ids":["macmcp"],"allowed_models":[],"chat_templates":[],"generate_skill":false,"token":"relay_proj_0f1e2d3c4b5a6978","disabled_tools":{}}
]`

// ssh_argv is present because the real hostView carries it to the tray;
// it never reaches eve (docs/ssh-hosts.md).
const fixtureHosts = `[
  {"id":"h_devbox","name":"devbox","target":"admin@devbox.local","port":22,"created_at":"2026-09-05T07:00:00Z","status":"connected","ssh_argv":["ssh","-o","ControlMaster=auto","admin@devbox.local"],"probe":{"ok":true,"at":"2026-09-05T07:30:00Z","os":"linux","arch":"arm64","home":"/home/you","shell":"/bin/bash","node_version":"v22.4.0"}},
  {"id":"h_build","name":"build-box","target":"ci@10.0.0.7","port":2222,"identity_file":"~/.ssh/id_build","created_at":"2026-08-20T09:00:00Z","status":"unreachable","ssh_argv":["ssh","ci@10.0.0.7"],"probe":{"ok":false,"at":"2026-09-05T06:00:00Z","error":"ssh: connect to host 10.0.0.7 port 2222: Connection timed out"}}
]`

// The fingerprint is never truncated: after an enrolment is deleted it is the
// only thing that names that client's calls in the audit log. No key material
// appears here, matching production — the create response carries a bundle
// directory and nothing else.
const fixtureEnrolments = `[
  {"client_id":"hermes-mail","fingerprint":"sha256:9f2a4c1d6b8e0f37a5c9d2e4b6081f3a7c5e9d1b3f5a7c9e1d3b5f7a9c1e3d5b","project_ids":["proj-mail"],"budget":{"window_seconds":60,"max_calls":60,"max_result_bytes":8388608},"created_at":"2026-08-20T09:14:00Z"}
]`

const fixtureRemote = `{"configured":true,"enabled":true,"listen":"127.0.0.1:9910","effective":"127.0.0.1:9910","audit_enabled":true,"ca_fingerprint":"sha256:1a2b3c4d5e6f70819203a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f","enrolment_requests":false,"enrolment_listen":"","enrolment_effective":"127.0.0.1:9911"}`

const fixtureEnrolmentBudgetDefaults = `{"window_seconds":60,"max_calls":60,"max_result_bytes":8388608}`

// The credential id is abbreviated and there is no public key here, matching
// production: passkeyView has no field that could carry one.
const fixturePasskeys = `[
  {"id":"cred_9f2a4c1d6b8e0f37a5c9d2e4b6081f3a","short":"cred_9f2a4c…","name":"MacBook Touch ID","created":"2026-08-21T11:02:00Z","sign_count":0,"counter_supported":false},
  {"id":"cred_31bd77aa04e6c9f2118d5c30ab7e6641","short":"cred_31bd77…","name":"YubiKey 5C","created":"2026-08-24T16:40:00Z","sign_count":7,"counter_supported":true}
]`

const fixtureLoginSessions = `[
  {"id":"a3f1c8de-5b21-4f70-9e6a-2d4c81b0e957","name":"login cred_9f2a4c… 2026-08-28T08:12:04Z","created":"2026-08-28T08:12:04Z","expires":"2026-08-28T20:12:04Z"}
]`

// Null, which is the state on every ordinary open: a code is present only in
// the paint the tray's "Show Login Code..." item triggered.
const fixtureLoginCode = `null`

const fixtureMcpScopeFields = `{
  "fsmcp":[
    {"name":"allowed_dirs","type":"array","item_type":"string","description":"Directories this client may reach","source":"project_path"}
  ],
  "macmcp":[
    {"name":"mail_accounts","type":"array","item_type":"string","description":"Mail accounts this client may read from or send as","source":"operator","applies_to":["mail_*"],"enumerable":true},
    {"name":"mail_mailboxes","type":"array","item_type":"string","description":"Mailbox paths within those accounts this client may reach","source":"operator","applies_to":["mail_*"],"enumerable":true,"depends_on":["mail_accounts"]},
    {"name":"file_dirs","type":"array","item_type":"string","description":"Directories this client may write files into","source":"project_path","applies_to":["mail_save_attachment","mail_get_source"]}
  ],
  "krisp":[]
}`

// macMCP is mid-restart-loop so the Overview tile and the MCP Servers pill
// both have a warn state to render; fsMCP has never had a problem; Krisp is
// HTTP, so its pill comes from oauth_state instead and this entry is unused.
const fixtureMcpHealth = `{
  "fsmcp":{"id":"fsmcp","display_name":"fsMCP","connected":true},
  "macmcp":{"id":"macmcp","display_name":"macMCP","connected":false,"state":"restart_failed","attempt":2,"downtime_ms":48000,"error":"read response: EOF"}
}`

const fixtureServiceRuntime = `{
  "relay-llm":{"pid":21093,"started_at":"2026-09-05T05:12:00Z"},
  "relaytts-daemon":{"pid":21110,"started_at":"2026-09-05T05:12:03Z"},
  "stt-daemon":{"pid":21114,"started_at":"2026-09-05T05:12:04Z"}
}`

// Empty means healthy, the ordinary case; devui models that rather than the
// break-glass path, which has no UI of its own beyond this string.
const fixtureSealStatus = `""`

const fixtureVersion = `"0.9.0-dev"`

const fixturePaths = `{"config":"/Users/you/Library/Application Support/relay","logs":"/Users/you/Library/Application Support/relay/logs"}`

const fixtureMcpToolCache = `{
  "fsmcp":[
    {"name":"read_file","description":"Read the contents of a file at the given path."},
    {"name":"write_file","description":"Write content to a file, creating it if needed."},
    {"name":"list_dir","description":"List entries in a directory."}
  ]
}`

// Stands in for the WKWebView bridge; the page's ipc() posts through
// window.webkit, so every message lands here.
var mockBridgeScript = `<script>
(function () {
  var FIXTURE_CONFIG_TEXT = ` + jsString(fixtureConfigText) + `;
  var FIXTURE_TOOLS = ` + inlineJSON(fixtureMcpToolCacheTools) + `;
  var FIXTURE_AUDIT = ` + inlineJSON(fixtureAuditEvents) + `;
  var FIXTURE_AUDIT_STATUS = ` + inlineJSON(fixtureAuditStatus) + `;
  var FIXTURE_REMOTE = ` + inlineJSON(fixtureRemote) + `;
  window.webkit = { messageHandlers: { ipc: { postMessage: function (raw) {
    var msg; try { msg = JSON.parse(raw); } catch (e) { console.warn('[devui] bad ipc', raw); return; }
    console.log('[devui ipc →]', msg);
    setTimeout(function () { handle(msg); }, 140);
  } } } };
  function handle(msg) {
    switch (msg.type) {
      case 'service_config':
        if (msg.op === 'get') window.onServiceConfigResult({ serviceId: msg.serviceId, op: 'get', ok: true, text: FIXTURE_CONFIG_TEXT });
        else if (msg.op === 'save') { window.onServiceConfigResult({ serviceId: msg.serviceId, op: 'save', ok: true }); window.onServiceConfigApplied({ serviceId: msg.serviceId, mode: 'restarting' }); }
        break;
      case 'list_mcp_tools': window.onMcpToolsListed(msg.mcp_id, FIXTURE_TOOLS[msg.mcp_id] || []); break;
      case 'enumerate_scope_field': window.onScopeFieldEnumerated(enumerate(msg)); break;
      case 'service_action': window.onServiceActionResult({ serviceId: msg.serviceId, actionId: msg.actionId, row: msg.row, ok: true }); break;
      case 'query_audit': window.onAuditEvents(FIXTURE_AUDIT, FIXTURE_AUDIT_STATUS); break;
      case 'export_audit': window.onAuditExported('/Users/you/Library/Application Support/relay/logs/audit/toolcalls-export-20260819-150000.jsonl'); break;
      case 'reveal_audit_log': console.log('[devui] would reveal the audit log'); break;
      case 'reveal_config_dir': console.log('[devui] would reveal the config dir'); break;
      case 'reveal_logs_dir': console.log('[devui] would reveal the logs dir'); break;
      case 'reveal_service_log': console.log('[devui] would reveal the log for service', msg.id); break;
      case 'create_enrolment':
        // The real handler emits the persisted record plus a bundle DIRECTORY.
        // The mock does the same, key material included nowhere — mirroring it
        // any other way here would model a boundary that does not exist.
        window.onEnrolmentCreated(
          { client_id: msg.client_id, fingerprint: 'sha256:' + '0123456789abcdef'.repeat(4),
            project_ids: msg.project_ids || [], budget: msg.budget, created_at: new Date().toISOString() },
          { dir: '/Users/you/Library/Application Support/relay/enrolments/' + msg.client_id });
        break;
      case 'revoke_enrolment':
        window.onEnrolmentRevoked(msg.client_id, 'sha256:' + '0123456789abcdef'.repeat(4));
        break;
      case 'update_remote_config':
        if (msg.remove) {
          window.onRemoteConfigUpdated({ configured: false, enabled: false, listen: '', effective: '127.0.0.1:9910', audit_enabled: true,
                                         ca_fingerprint: FIXTURE_REMOTE.ca_fingerprint, enrolment_requests: false, enrolment_listen: '', enrolment_effective: '127.0.0.1:9911' });
          break;
        }
        window.onRemoteConfigUpdated({ configured: true, enabled: !!msg.enabled, listen: msg.listen || '',
                                       effective: msg.listen || '127.0.0.1:9910', audit_enabled: true,
                                       ca_fingerprint: FIXTURE_REMOTE.ca_fingerprint,
                                       enrolment_requests: !!msg.enrolment_requests, enrolment_listen: msg.enrolment_listen || '',
                                       enrolment_effective: msg.enrolment_listen || '127.0.0.1:9911' });
        break;
      // add/update/remove/start/stop etc. are no-ops here; already logged above
    }
  }
  function enumerate(msg) {
    var res = { mcp_id: msg.mcp_id, field: msg.field, status: 'ok', values: null };
    if (msg.mcp_id !== 'macmcp') { res.status = 'unsupported'; res.error = msg.mcp_id + ' does not implement context/enumerate'; return res; }
    if (msg.field === 'mail_accounts') {
      res.values = [{ value: 'Alice', label: 'Alice <alice@example.com>' }, { value: 'Bob', label: 'Bob <bob@example.com>' }];
      return res;
    }
    if (msg.field === 'mail_mailboxes') {
      // Read WITHIN the accounts already chosen; unchosen means all of them.
      var accounts = (msg.values && msg.values.mail_accounts) || ['Alice', 'Bob'];
      res.values = [];
      for (var i = 0; i < accounts.length; i++) {
        res.values.push({ value: 'INBOX', label: 'INBOX (' + accounts[i] + ')' });
        res.values.push({ value: 'Projects/Archive', label: 'Projects/Archive (' + accounts[i] + ')' });
      }
      return res;
    }
    res.status = 'invalid_field';
    res.error = 'no enumerable field named ' + msg.field;
    res.values = null;
    return res;
  }
})();
</script>`

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

const fixtureAuditEvents = `[
  {"id":"ev-1","ts":"2026-08-19T10:24:02.117Z","dur_ms":412,"event":"call_tool",
   "actor":{"kind":"project","project_id":"proj-acme","project_name":"Acme Website","auth":"token","pid":41221,"proc":"relay","parent":"claude"},
   "mcp_id":"fsmcp","tool":"read_file","args":{"path":"/Users/you/projects/acme/README.md"},"args_bytes":48,
   "outcome":"ok","result_bytes":20431},

  {"id":"ev-2","ts":"2026-08-19T10:23:58.004Z","dur_ms":1,"event":"call_tool",
   "actor":{"kind":"project","project_id":"proj-internal","project_name":"Internal Tools","auth":"token","pid":41219,"proc":"relay","parent":"node"},
   "mcp_id":"fsmcp","tool":"write_file","args":{"path":"/etc/hosts","content":"…"},"args_bytes":96,
   "outcome":"denied","error":"access denied: tool 'write_file' is disabled for this token"},

  {"id":"ev-3","ts":"2026-08-19T10:23:44.882Z","dur_ms":88,"event":"call_tool",
   "actor":{"kind":"project","project_id":"proj-internal","project_name":"Internal Tools","auth":"cwd","cwd":"/Users/you/projects/internal/pkg","pid":41210,"proc":"relay","parent":"zsh"},
   "mcp_id":"fsmcp","tool":"list_dir","args":{"path":"/Users/you/projects/internal/pkg"},"args_bytes":52,
   "outcome":"ok","result_bytes":812},

  {"id":"ev-4","ts":"2026-08-19T10:22:31.412Z","dur_ms":1503,"event":"call_tool",
   "actor":{"kind":"project","project_id":"proj-acme","project_name":"Acme Website","auth":"token","pid":41180,"proc":"relay","parent":"claude"},
   "mcp_id":"macmcp","tool":"calendar_events","args":{"range":"today","api_key":"[redacted]"},"args_bytes":64,
   "outcome":"ok","result_bytes":4096,"result_is_error":true},

  {"id":"ev-5","ts":"2026-08-19T10:21:09.230Z","dur_ms":0,"event":"call_tool",
   "actor":{"kind":"unknown","auth":"token","pid":40997,"proc":"curl","parent":"zsh"},
   "tool":"read_file","outcome":"unauthorized","error":"invalid token"},

  {"id":"ev-6","ts":"2026-08-19T10:20:55.100Z","dur_ms":233,"event":"call_tool",
   "actor":{"kind":"project","project_id":"proj-acme","project_name":"Acme Website","auth":"token","pid":40940,"proc":"relay","parent":"claude"},
   "mcp_id":"fsmcp","tool":"write_file","args":"{\"path\":\"/Users/you/projects/acme/bundle.js\",\"content\":\"(function(){var a=1;","args_bytes":184320,"args_truncated":true,
   "outcome":"ok","result_bytes":32}
]`

const fixtureAuditStatus = `{"enabled":true,"path":"/Users/you/Library/Application Support/relay/logs/audit/toolcalls.jsonl","dropped":0,"recorded":6,"log_args":true,"log_lists":false}`

const fixtureMcpToolCacheTools = fixtureMcpToolCache

// The JSONC-style comment inside this literal is deliberate: it exercises the
// comment stripper.
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
// terminate the <script> element early. Structural JSON contains no </, so
// this is a no-op there.
func inlineJSON(s string) string {
	return strings.ReplaceAll(s, "</", "<\\/")
}

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
