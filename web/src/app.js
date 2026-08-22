// Settings UI application module: state, the render dispatcher, every tab
// renderer, IPC bridge, and all event handlers. Pure helpers live in
// ./lib/pure.js. Bundled (esbuild) and inlined into web/dist/settings.html.
import {
    esc, formatScalar, cfgParseConfigText, cfgGetAt, cfgSetAt, cfgDefaultFor, cfgCoerce, cfgKvCoerce, cfgKvDisplay, cfgScanRequired, cfgSummary, cfgFormatStringMap, cfgFormatJson, oneLineProj
} from './lib/pure.js';

// Initial data injected by relay's renderSettingsHTML via the shell template.
const EXTERNAL_MCPS_INIT = window.__RELAY_INIT__.externalMcps;
const SERVICES_INIT = window.__RELAY_INIT__.services;
const RUNNING_IDS_INIT = window.__RELAY_INIT__.runningIds;
const PROJECTS_INIT = window.__RELAY_INIT__.projects;
const MCP_TOOL_CACHE_INIT = window.__RELAY_INIT__.mcpToolCache;
// What each MCP declares as narrowable: its scope: "restrict" fields, already
// projected by Go (ScopeFieldView) so the rule that an absent `source` means
// "operator" lives in exactly one place. Seeded rather than fetched because
// the LIST needs it — a row has to be able to say "needs a scope value"
// without anyone opening the editor first.
const MCP_SCOPE_FIELDS_INIT = window.__RELAY_INIT__.mcpScopeFields || {};
const ENROLMENTS_INIT = window.__RELAY_INIT__.enrolments || [];
const REMOTE_INIT = window.__RELAY_INIT__.remote || null;
// The conservative per-enrolment budget defaults, shipped from Go so the
// create form's placeholders name the real numbers instead of a second copy
// of them that can rot apart from normalizeEnrolmentBudget.
const ENROLMENT_BUDGET_DEFAULTS_INIT = window.__RELAY_INIT__.enrolmentBudgetDefaults || {};

function ipc(msg) {
    if (window.webkit && window.webkit.messageHandlers && window.webkit.messageHandlers.ipc)
        window.webkit.messageHandlers.ipc.postMessage(msg);
    else if (window.chrome && window.chrome.webview)
        window.chrome.webview.postMessage(msg);
}

let state = {
    page: 'services',
    externalMcps: EXTERNAL_MCPS_INIT,
    discovering: false,
    discoveryError: null,
    mcpAddMode: 'form',
    mcpTransport: 'stdio',
    authenticatingMcp: null,
    editingMcpId: null,                 // null = list, 'new' = add form (no edit support yet)
    services: SERVICES_INIT,
    runningServices: RUNNING_IDS_INIT.reduce(function(m, id) { m[id] = true; return m; }, {}),
    editingServiceId: null,             // null = list, 'new' = add form, '<id>' = edit form
    // Service Inspector state. Each snapshot in serviceStatuses carries
    // its own manifest, so we derive button layouts from the snapshot —
    // no separate manifest map to keep in sync.
    serviceStatuses: {},      // serviceId -> ServiceStatusSnapshot
    serviceActionPending: {}, // key "svc|action|rowKey" -> true while in flight
    serviceActionError: {},   // serviceId -> last error string (cleared on next ok)

    // Per-service config editor (manifest.config). The service advertises a
    // file path + a recursive schema; relay ships the raw file text, we parse
    // it into a tree, render forms from the schema, and serialize back on save.
    serviceConfigTree: {},     // svcId -> parsed config object (server truth)
    serviceConfigDraft: {},    // svcId -> edited clone (form binds to this)
    serviceConfigOpen: {},     // svcId -> bool (panel expanded)
    serviceConfigError: {},    // svcId -> string (load/parse/save error)
    serviceConfigPending: {},  // svcId -> bool (op in flight)
    serviceConfigApplyMsg: {}, // svcId -> string ("Restarting…" etc.)
    serviceConfigLoaded: {},   // svcId -> bool (fetched at least once)
    serviceConfigExpanded: {}, // svcId -> { JSON.stringify(path): bool } collapse state per node
    // Rebuilt every inspector render: integer-indexed bindings from a rendered
    // input back to its (svcId, path) into the draft tree, plus the set of
    // json-leaf bindings currently holding unparseable text. Avoids encoding
    // arbitrary map keys into HTML — handlers carry an index, not a path.
    _cfgBind: [],
    _cfgBadJson: {},

    // Projects tab.
    projects: PROJECTS_INIT,
    mcpToolCache: MCP_TOOL_CACHE_INIT,     // mcpId -> [{name, description, category}]
    mcpScopeFields: MCP_SCOPE_FIELDS_INIT, // mcpId -> [ScopeFieldView]; NO KEY = relay has never seen that MCP
    editingProjectId: null,                 // null = list, 'new' = create form, '<id>' = edit
    projectForm: null,                      // in-flight form values (kept out of state.projects until Save)
    projectFormError: null,
    projectTokenVisible: {},                // id -> bool (eye toggle)
    projectFreshToken: {},                  // id -> plaintext shown once after rotate
    projectSkillRegen: {},                  // id -> { ok, message, t } (last regen result)
    projectError: null,
    rotatingProjectId: null,

    // The enumeration picker (ADR-011 decision 6). Enumeration is a LIVE call
    // into another process, so none of this is populated by a paint: a list is
    // fetched when an operator opens the control and cached for the life of
    // the form, keyed on (mcp, field, dependency values) — a mailbox list read
    // within Bob is not an answer about Alice.
    scopeEnum: {},            // key -> ContextEnumResult (see scopeEnumKey)
    scopeEnumReq: {},         // "mcp\0field" -> the key of the in-flight request
    scopeEnumOpen: {},        // "mcp\0field" -> bool (the operator opened it)
    scopeEnumUnsupported: {}, // mcpId -> true once it answered -32601, permanently
    _scopeBind: [],           // per-render bindings from a checkbox to its value

    // Remote Clients tab. Enrolments and the remote block are seeded by the
    // initial payload like projects are — the list is one row per enrolled
    // certificate, and a credential you cannot see is one you will not revoke,
    // so it should be on screen the moment the tab is.
    enrolments: ENROLMENTS_INIT,
    remote: REMOTE_INIT,                    // remoteConfigView from Go, or null
    enrolmentBudgetDefaults: ENROLMENT_BUDGET_DEFAULTS_INIT,
    enrolForm: null,                        // null = list, object = create form
    enrolmentError: null,
    enrolBundle: null,                      // {client_id, dir} — DIRECTORY only, never key material
    enrolRevoked: null,                     // {client_id, fingerprint} shown after a revoke
    remoteDraft: null,                      // uncommitted edit of the remote block
    remoteDirty: false,
    remoteError: null,

    // Tool Calls tab. Events arrive newest-first from the recorder's ring (or
    // from a deep query over the log file); `auditFilter` mirrors AuditQuery
    // on the Go side so it can be sent verbatim.
    auditEvents: [],
    auditStatus: null,                      // {enabled, path, dropped, recorded, ...}
    auditFilter: { project_id: '', mcp_id: '', outcome: '', event: '', kind: '', text: '', deep: false },
    auditExpanded: {},                      // event id -> bool
    auditFollow: true,                      // append live events as they arrive
    auditLoaded: false,
    auditError: null,
    auditExportPath: null,
};

// How many live events the Tool Calls tab keeps in the DOM. The Go-side ring
// is the real buffer; this only bounds what one open window renders.
const AUDIT_MAX_ROWS = 500;

function showPage(page) {
    state.page = page;
    const pages = ['services', 'mcps', 'projects', 'remote', 'inspector', 'audit'];
    document.querySelectorAll('.sidebar-item').forEach((el, i) => {
        el.classList.toggle('active', pages[i] === page);
    });
    // The Tool Calls tab is the only one not seeded by the initial payload:
    // the log can be large, so it's fetched the first time it's shown.
    if (page === 'audit' && !state.auditLoaded) queryAudit();
    render();
}

const JSON_PLACEHOLDER = JSON.stringify({"my-server": {"command": "npx", "args": ["-y", "@example/server"], "env": {"API_KEY": "..."}}}, null, 2);

// render(source) repaints #content. When source === 'push' (IPC-driven),
// skip the repaint if a form for the current tab is open so we don't wipe
// keystrokes mid-edit. User-initiated renders always proceed — tab switches
// and explicit form mutations must always reflect on screen.
function render(source) {
    const el = document.getElementById('content');
    const fromPush = source === 'push';
    if (state.page === 'services') {
        if (fromPush && state.editingServiceId) return;
        el.innerHTML = renderServices();
    } else if (state.page === 'inspector') {
        // The 2s status poll updates status regions surgically
        // (updateServiceStatusDOM) and never routes here. But other push sources
        // (e.g. onSettingsReloaded after an external service/MCP change) still
        // call render('push'); skip the full inspector rebuild while a config
        // editor is open so it can't wipe in-flight keystrokes there.
        if (fromPush && anyConfigEditorOpen()) return;
        el.innerHTML = renderServiceInspector();
    } else if (state.page === 'projects') {
        if (fromPush && state.editingProjectId) return;
        el.innerHTML = renderProjects();
    } else if (state.page === 'remote') {
        // Skip a push-sourced repaint while the create form is open or the
        // listener block has uncommitted edits, for the same reason the
        // Projects tab does: an external change must not eat keystrokes.
        if (fromPush && (state.enrolForm || state.remoteDirty)) return;
        el.innerHTML = renderEnrolments();
    } else if (state.page === 'audit') {
        el.innerHTML = renderAudit();
        restoreAuditFocus();
    } else {
        if (fromPush && state.editingMcpId) return;
        el.innerHTML = renderMcpServers();
        const ta = document.getElementById('mcpJson');
        if (ta) ta.placeholder = JSON_PLACEHOLDER;
    }
}

function renderMcpServers() {
    if (state.editingMcpId) return renderMcpForm();

    let html = '<div class="page-header">';
    html += '<h2>MCP Servers</h2>';
    html += '<button class="btn btn-primary" onclick="newMcp()">+ New MCP Server</button>';
    html += '</div>';
    html += '<p class="page-intro">Add external MCP servers so clients only need to connect to Relay.</p>';

    if (state.externalMcps.length === 0) {
        html += '<div class="empty-state">No external MCP servers configured. Click <strong>+ New MCP Server</strong> to add one.</div>';
        return html;
    }

    for (const mcp of state.externalMcps) {
        // discovered_tools is runtime-only on the Go side (json:"-"), so the
        // live count comes from mcpToolCache — same data, single source.
        const toolCount = (state.mcpToolCache[mcp.id] || []).length;
        const isHTTP = mcp.transport === 'http';
        const authenticating = state.authenticatingMcp === mcp.id;
        html += '<div class="mcp-card">';
        html += '<div class="mcp-card-header">';
        html += `<span class="mcp-card-name">${esc(mcp.display_name)}</span>`;
        html += '<div style="display:flex;gap:4px;align-items:center">';
        if (isHTTP) {
            if (mcp.oauth_state && mcp.oauth_state.access_token) {
                html += '<span style="font-size:11px;color:#22c55e;border:1px solid #22c55e;border-radius:3px;padding:2px 6px">Authenticated</span>';
            } else {
                html += '<span style="font-size:11px;color:#f59e0b;border:1px solid #f59e0b;border-radius:3px;padding:2px 6px">Not authenticated</span>';
            }
        }
        if (mcp.tcc_services && mcp.tcc_services.length > 0) {
            const busy = state.resettingMcpPermissions === mcp.id;
            const label = busy ? 'Resetting…' : 'Reset Permissions';
            html += `<button class="btn btn-sm" onclick="resetMcpPermissions('${esc(mcp.id)}')" ${busy ? 'disabled' : ''}>${label}</button>`;
        }
        html += `<button class="btn btn-sm btn-danger" onclick="removeExternalMcp('${esc(mcp.id)}')">Remove</button>`;
        html += '</div></div>';
        if (isHTTP) {
            html += `<div class="mcp-card-cmd">${esc(mcp.url || '')}</div>`;
            html += '<div style="display:flex;align-items:center;gap:8px;margin-top:4px">';
            html += `<div class="mcp-card-tools" style="margin:0">${toolCount} tool${toolCount !== 1 ? 's' : ''}</div>`;
            if (authenticating) {
                html += '<button class="btn btn-sm" disabled><span class="spinner"></span>Authenticating...</button>';
            } else {
                html += `<button class="btn btn-sm" onclick="authenticateMcp('${esc(mcp.id)}')">Authenticate</button>`;
            }
            html += '</div>';
        } else {
            const cmd = mcp.command || '';
            const cmdDisplay = cmd.length > 40 ? '...' + cmd.slice(-37) : cmd;
            const argsDisplay = mcp.args && mcp.args.length > 0 ? ' ' + mcp.args.join(' ') : '';
            html += `<div class="mcp-card-cmd">${esc(cmdDisplay + argsDisplay)}</div>`;
            html += `<div class="mcp-card-tools">${toolCount} tool${toolCount !== 1 ? 's' : ''}</div>`;
        }
        html += '</div>';
    }
    return html;
}

// Form view for adding an MCP server. There is no edit flow today — MCPs are
// add-or-remove; editingMcpId is always 'new' while this is rendered.
function renderMcpForm() {
    let html = '<div class="page-header">';
    html += '<h2>New MCP Server</h2>';
    html += '<button class="btn btn-danger btn-sm" onclick="cancelMcpEdit()">Cancel</button>';
    html += '</div>';

    const isStdio = state.mcpTransport === 'stdio';
    html += `<div style="display:flex;gap:4px;margin-bottom:12px">
        <button class="perm-btn ${isStdio ? 'active' : ''}" onclick="setMcpTransport('stdio')">Stdio</button>
        <button class="perm-btn ${!isStdio ? 'active' : ''}" onclick="setMcpTransport('http')">HTTP</button>
    </div>`;

    if (!isStdio) {
        html += '<label>Display name</label>';
        html += '<input type="text" id="mcpDisplayName" placeholder="e.g. Krisp" />';
        html += '<label>URL</label>';
        html += '<input type="text" id="mcpUrl" placeholder="e.g. https://mcp.krisp.ai/mcp" />';
    } else {
        const formActive = state.mcpAddMode === 'form';
        html += `<div style="display:flex;gap:4px;margin-bottom:12px">
            <button class="perm-btn ${formActive ? 'active' : ''}" onclick="setMcpAddMode('form')">Form</button>
            <button class="perm-btn ${!formActive ? 'active' : ''}" onclick="setMcpAddMode('json')">Paste JSON</button>
        </div>`;

        if (formActive) {
            html += '<label>Display name</label>';
            html += '<input type="text" id="mcpDisplayName" placeholder="e.g. Everything Server" />';
            html += '<label>Command</label>';
            html += '<input type="text" id="mcpCommand" placeholder="e.g. npx or /usr/local/bin/my-server" />';
            html += '<label>Arguments (space-separated)</label>';
            html += '<input type="text" id="mcpArgs" placeholder="e.g. @modelcontextprotocol/server-everything" />';
            html += '<label>Environment variables (KEY=VALUE per line)</label>';
            html += '<textarea id="mcpEnv" rows="3" placeholder="API_KEY=abc123&#10;DEBUG=true"></textarea>';
        } else {
            html += '<label>Paste a Claude Desktop-style JSON config snippet</label>';
            html += '<textarea id="mcpJson" rows="8"></textarea>';
            html += '<p style="color:var(--text-3);font-size:11px;margin-top:4px">Accepts <code style="color:var(--text-2)">&lbrace; "name": &lbrace; "command", "args", "env" &rbrace; &rbrace;</code></p>';
        }
    }

    html += '<div style="margin-top:16px;display:flex;gap:8px">';
    if (state.discovering) {
        html += '<button class="btn" disabled><span class="spinner"></span>Discovering...</button>';
    } else {
        if (!isStdio) {
            html += '<button class="btn btn-primary" onclick="addExternalMcpHttp()">Add MCP Server</button>';
        } else {
            const formActive = state.mcpAddMode === 'form';
            html += `<button class="btn btn-primary" onclick="${formActive ? 'addExternalMcp()' : 'addExternalMcpFromJson()'}">Add MCP Server</button>`;
        }
        html += '<button class="btn btn-danger" onclick="cancelMcpEdit()">Cancel</button>';
    }
    html += '</div>';

    if (state.discoveryError) {
        html += `<div class="error-msg">${esc(state.discoveryError)}</div>`;
    }

    return html;
}



function addExternalMcp() {
    const displayName = document.getElementById('mcpDisplayName').value.trim();
    const command = document.getElementById('mcpCommand').value.trim();
    const argsStr = document.getElementById('mcpArgs').value.trim();
    const envStr = document.getElementById('mcpEnv').value.trim();

    if (!displayName || !command) return;

    const args = argsStr ? argsStr.split(/\s+/) : [];
    const env = {};
    if (envStr) {
        for (const line of envStr.split('\n')) {
            const eq = line.indexOf('=');
            if (eq > 0) {
                env[line.slice(0, eq).trim()] = line.slice(eq + 1).trim();
            }
        }
    }

    state.discoveryError = null;
    ipc(JSON.stringify({
        type: 'add_external_mcp',
        display_name: displayName,
        command,
        args,
        env,
    }));
}

function setMcpTransport(transport) {
    state.mcpTransport = transport;
    state.discoveryError = null;
    render();
}

function setMcpAddMode(mode) {
    state.mcpAddMode = mode;
    state.discoveryError = null;
    render();
}

function addExternalMcpFromJson() {
    const raw = document.getElementById('mcpJson').value.trim();
    if (!raw) return;

    let parsed;
    try {
        parsed = JSON.parse(raw);
    } catch (e) {
        state.discoveryError = 'Invalid JSON: ' + e.message;
        render();
        return;
    }

    // Expect { "name": { "command": "...", ... } }
    const keys = Object.keys(parsed);
    if (keys.length === 0) {
        state.discoveryError = 'JSON must contain at least one server entry';
        render();
        return;
    }
    if (keys.length > 1) {
        state.discoveryError = 'Only one server entry is supported per import. ' + (keys.length - 1) + ' extra entries were ignored.';
    }

    const name = keys[0];
    const cfg = parsed[name];
    if (!cfg || typeof cfg !== 'object' || !cfg.command) {
        state.discoveryError = 'Entry must have a "command" field';
        render();
        return;
    }

    state.discoveryError = null;
    ipc(JSON.stringify({
        type: 'add_external_mcp',
        display_name: name,
        command: cfg.command,
        args: cfg.args || [],
        env: cfg.env || {},
    }));
}

function addExternalMcpHttp() {
    const displayName = document.getElementById('mcpDisplayName').value.trim();
    const url = document.getElementById('mcpUrl').value.trim();
    if (!displayName || !url) return;

    state.discoveryError = null;
    ipc(JSON.stringify({
        type: 'add_external_mcp',
        display_name: displayName,
        transport: 'http',
        url: url,
    }));
}

function newMcp() {
    state.editingMcpId = 'new';
    state.discoveryError = null;
    render();
}

function cancelMcpEdit() {
    state.editingMcpId = null;
    state.discoveryError = null;
    state.discovering = false;
    render();
}

function authenticateMcp(id) {
    ipc(JSON.stringify({ type: 'authenticate_mcp', id }));
}

function removeExternalMcp(id) {
    ipc(JSON.stringify({ type: 'remove_external_mcp', id }));
}

window.onOAuthRequired = function(id) {
    // Server needs auth -- badge already shown from the added MCP data.
};

// renderMcpPush is the gate for every MCP-tab push handler. List-affecting
// pushes (`bypassForm: false`) honor the form-protect guard so keystrokes
// survive. Form-affecting pushes (`bypassForm: true` — discovery spinner,
// error banner) bypass the guard when the form is open, since the new
// state belongs *inside* the form and the user needs to see it.
function renderMcpPush(bypassForm) {
    if (state.page !== 'mcps') return;
    render(bypassForm && state.editingMcpId ? undefined : 'push');
}

window.onOAuthStarted = function(id) {
    state.authenticatingMcp = id;
    renderMcpPush(false);
};

window.onOAuthComplete = function(id) {
    state.authenticatingMcp = null;
    const mcp = state.externalMcps.find(m => m.id === id);
    if (mcp) {
        if (!mcp.oauth_state) mcp.oauth_state = {};
        mcp.oauth_state.access_token = 'authenticated'; // UI placeholder only
    }
    renderMcpPush(false);
};

window.onOAuthError = function(id, msg) {
    state.authenticatingMcp = null;
    state.discoveryError = 'OAuth failed: ' + msg;
    renderMcpPush(true);
};

window.onDiscoveryStarted = function() {
    state.discovering = true;
    state.discoveryError = null;
    renderMcpPush(true);
};

window.onExternalMcpAdded = function(mcp) {
    state.discovering = false;
    state.discoveryError = null;
    state.externalMcps.push(mcp);
    state.editingMcpId = null; // close form on successful add
    renderMcpPush(false);
};

window.onExternalMcpError = function(msg) {
    state.discovering = false;
    state.discoveryError = msg;
    renderMcpPush(true);
};

window.onExternalMcpRemoved = function(id) {
    state.externalMcps = state.externalMcps.filter(m => m.id !== id);
    renderMcpPush(false);
};

// Reset TCC permissions for an MCP. The backend clears tccutil entries for
// each declared service and re-spawns the MCP with --request-permissions —
// the spawn uses the same exec.Command shape as normal stdio MCP startup so
// TCC attributes the resulting prompts to the same responsible parent (relay
// tray) that the MCP runs under at runtime. The user should approve any
// system dialogs that appear while this is running.
function resetMcpPermissions(id) {
    const mcp = state.externalMcps.find(m => m.id === id);
    if (!mcp) return;
    const services = (mcp.tcc_services || []).join(', ');
    if (!confirm('Reset TCC permissions for "' + mcp.display_name + '"?\n\n' +
        'This clears existing grants for: ' + services + '\n' +
        'Then launches the MCP with --request-permissions to trigger fresh prompts.\n\n' +
        'Approve any system dialogs that appear after clicking OK. Can take up to 60s.')) return;
    state.resettingMcpPermissions = id;
    renderMcpPush(false);
    ipc(JSON.stringify({ type: 'reset_mcp_permissions', id }));
}

window.onMcpPermissionsReset = function(id, result) {
    state.resettingMcpPermissions = null;
    renderMcpPush(false);
    if (!result || !result.ok) {
        alert('Reset failed: ' + (result && result.error ? result.error : 'unknown error'));
        return;
    }
    let summary = 'Reset permissions for bundle ' + result.bundle_id + '\n\n';
    if (result.reset_services && result.reset_services.length) {
        summary += 'Cleared: ' + result.reset_services.join(', ') + '\n\n';
    }
    if (result.skipped_reasons && result.skipped_reasons.length) {
        summary += 'Skipped:\n  ' + result.skipped_reasons.join('\n  ') + '\n\n';
    }
    if (result.spawn_output) {
        summary += '--- MCP --request-permissions output ---\n' + result.spawn_output;
    }
    alert(summary);
};

function renderServices() {
    if (state.editingServiceId) return renderServiceForm();

    let html = '<div class="page-header">';
    html += '<h2>Services</h2>';
    html += '<button class="btn btn-primary" onclick="newService()">+ New Service</button>';
    html += '</div>';
    html += '<p class="page-intro">Manage background processes. These appear in the tray menu for quick start/stop.</p>';

    if (state.services.length === 0) {
        html += '<div class="empty-state">No services configured. Click <strong>+ New Service</strong> to add one.</div>';
        return html;
    }

    for (const svc of state.services) {
        const cmdDisplay = svc.command.length > 40 ? '...' + svc.command.slice(-37) : svc.command;
        const argsDisplay = svc.args && svc.args.length > 0 ? ' ' + svc.args.join(' ') : '';
        html += `<div class="mcp-card">
            <div class="mcp-card-header">
                <span class="mcp-card-name">${esc(svc.display_name)}</span>
                <div style="display:flex;gap:4px">
                    <button class="btn btn-sm" onclick="editService('${esc(svc.id)}')">Edit</button>
                    <button class="btn btn-sm btn-danger" onclick="removeService('${esc(svc.id)}')">Remove</button>
                </div>
            </div>
            <div class="mcp-card-cmd">${esc(cmdDisplay + argsDisplay)}</div>
            ${svc.working_dir ? `<div class="mcp-card-tools">cwd: ${esc(svc.working_dir)}</div>` : ''}
            ${svc.url ? `<div class="mcp-card-tools">url: ${esc(svc.url)}</div>` : ''}
            <div class="toggle-row" style="margin-bottom:0;padding:6px 0 0">
                <span style="font-size:12px;color:var(--text-2)">Running</span>
                <label class="switch switch-running">
                    <input type="checkbox" data-svc-running="${esc(svc.id)}" ${state.runningServices[svc.id] ? 'checked' : ''} onchange="toggleServiceRunning('${esc(svc.id)}', this.checked)" />
                    <span class="slider"></span>
                </label>
            </div>
            <div class="toggle-row" style="margin-bottom:0;padding:6px 0 0">
                <span style="font-size:12px;color:var(--text-2)">Autostart on launch</span>
                <label class="switch">
                    <input type="checkbox" ${svc.autostart ? 'checked' : ''} onchange="updateServiceAutostart('${esc(svc.id)}', this.checked)" />
                    <span class="slider"></span>
                </label>
            </div>
        </div>`;
    }
    return html;
}

// Form view for adding or editing a service. Mirrors the Projects pattern:
// state.editingServiceId === 'new' for add, '<id>' for edit.
function renderServiceForm() {
    const isNew = state.editingServiceId === 'new';
    const editing = isNew ? null : state.services.find(s => s.id === state.editingServiceId);
    if (!isNew && !editing) {
        // Stale edit target (e.g. service removed externally); fall back to list.
        state.editingServiceId = null;
        return renderServices();
    }
    const title = isNew ? 'New Service' : 'Edit Service';
    const dn = editing ? esc(editing.display_name) : '';
    const cm = editing ? esc(editing.command) : '';
    const ar = editing ? esc((editing.args || []).join(' ')) : '';
    const wd = editing ? esc(editing.working_dir || '') : '';
    const ev = editing ? esc(Object.entries(editing.env || {}).map(([k,v]) => k + '=' + v).join('\n')) : '';
    const as_ = editing ? editing.autostart : false;
    const ur = editing ? esc(editing.url || '') : '';

    let html = '<div class="page-header">';
    html += `<h2>${title}${editing ? ' <span style="color:var(--text-3);font-size:12px;font-weight:400">(id: ' + esc(editing.id) + ')</span>' : ''}</h2>`;
    html += '<button class="btn btn-danger btn-sm" onclick="cancelServiceEdit()">Cancel</button>';
    html += '</div>';

    html += '<label>Display name</label>';
    html += `<input type="text" id="svcDisplayName" value="${dn}" placeholder="e.g. My API Server" />`;
    html += '<label>Command</label>';
    html += `<input type="text" id="svcCommand" value="${cm}" placeholder="e.g. node or /usr/local/bin/my-server" />`;
    html += '<label>Arguments (space-separated)</label>';
    html += `<input type="text" id="svcArgs" value="${ar}" placeholder="e.g. server.js --port 8080" />`;
    html += '<label>Working directory (optional)</label>';
    html += `<input type="text" id="svcWorkingDir" value="${wd}" placeholder="e.g. /Users/you/project" />`;
    html += '<label>URL (optional, opens in browser on tray click)</label>';
    html += `<input type="text" id="svcUrl" value="${ur}" placeholder="e.g. http://localhost:3000" />`;
    html += '<label>Environment variables (KEY=VALUE per line)</label>';
    html += `<textarea id="svcEnv" rows="3" placeholder="API_KEY=abc123&#10;PORT=8080">${ev}</textarea>`;
    html += `<div class="toggle-row" style="margin-top:8px;margin-bottom:4px">
        <span>Autostart on launch</span>
        <label class="switch">
            <input type="checkbox" id="svcAutostart" ${as_ ? 'checked' : ''} />
            <span class="slider"></span>
        </label>
    </div>`;
    if (editing) {
        html += `<div style="margin-top:16px;display:flex;gap:8px">
            <button class="btn btn-primary" onclick="saveServiceEdit()">Save</button>
            <button class="btn btn-danger" onclick="cancelServiceEdit()">Cancel</button>
        </div>`;
    } else {
        html += `<div style="margin-top:16px;display:flex;gap:8px">
            <button class="btn btn-primary" onclick="addService()">Add Service</button>
            <button class="btn btn-danger" onclick="cancelServiceEdit()">Cancel</button>
        </div>`;
    }
    return html;
}

function svcFormValues() {
    const displayName = document.getElementById('svcDisplayName').value.trim();
    const command = document.getElementById('svcCommand').value.trim();
    const argsStr = document.getElementById('svcArgs').value.trim();
    const workingDir = document.getElementById('svcWorkingDir').value.trim();
    const envStr = document.getElementById('svcEnv').value.trim();
    const autostart = document.getElementById('svcAutostart').checked;
    const url = document.getElementById('svcUrl').value.trim();

    const args = argsStr ? argsStr.split(/\s+/) : [];
    const env = {};
    if (envStr) {
        for (const line of envStr.split('\n')) {
            const eq = line.indexOf('=');
            if (eq > 0) {
                env[line.slice(0, eq).trim()] = line.slice(eq + 1).trim();
            }
        }
    }

    return { displayName, command, args, env, workingDir, autostart, url };
}

function newService() {
    state.editingServiceId = 'new';
    render();
}

function addService() {
    const v = svcFormValues();
    if (!v.displayName || !v.command) return;

    ipc(JSON.stringify({
        type: 'add_service',
        display_name: v.displayName,
        command: v.command,
        args: v.args,
        env: v.env,
        working_dir: v.workingDir || null,
        autostart: v.autostart,
        url: v.url || null,
    }));
    // Form stays open until onServiceAdded confirms — that handler clears
    // editingServiceId. If the add fails (onSettingsError), the form stays
    // up so the user can fix and retry.
}

function editService(id) {
    state.editingServiceId = id;
    render();
}

function cancelServiceEdit() {
    state.editingServiceId = null;
    render();
}

function saveServiceEdit() {
    const v = svcFormValues();
    if (!v.displayName || !v.command) return;

    ipc(JSON.stringify({
        type: 'update_service',
        id: state.editingServiceId,
        display_name: v.displayName,
        command: v.command,
        args: v.args,
        env: v.env,
        working_dir: v.workingDir || null,
        autostart: v.autostart,
        url: v.url || null,
    }));

    const svc = state.services.find(s => s.id === state.editingServiceId);
    if (svc) {
        svc.display_name = v.displayName;
        svc.command = v.command;
        svc.args = v.args;
        svc.env = v.env;
        svc.working_dir = v.workingDir || null;
        svc.autostart = v.autostart;
        svc.url = v.url || null;
    }
    state.editingServiceId = null;
    render();
}

function removeService(id) {
    ipc(JSON.stringify({ type: 'remove_service', id }));
}

function updateServiceAutostart(id, checked) {
    const svc = state.services.find(s => s.id === id);
    if (svc) svc.autostart = checked;
    ipc(JSON.stringify({ type: 'update_service_autostart', id, autostart: checked }));
}

window.onServiceAdded = function(config) {
    state.services.push(config);
    // Close the New Service form on successful add so we return to the list.
    if (state.editingServiceId === 'new') state.editingServiceId = null;
    if (state.page === 'services') render('push');
};

window.onServiceRemoved = function(id) {
    state.services = state.services.filter(s => s.id !== id);
    // If the user happened to be editing the removed service, bail out.
    if (state.editingServiceId === id) state.editingServiceId = null;
    if (state.page === 'services') render('push');
};

function toggleServiceRunning(id, checked) {
    state.runningServices[id] = checked;
    ipc(JSON.stringify({ type: checked ? 'start_service' : 'stop_service', id: id }));
    render();
}

window.onServiceStatus = function(runningIds) {
    var m = {};
    for (var i = 0; i < runningIds.length; i++) m[runningIds[i]] = true;
    state.runningServices = m;
    if (state.page !== 'services') return;
    // Surgically update each toggle in place. A full re-render fires every
    // 2s from the status poller and would wipe text the user is typing into
    // the Add/Edit Service form.
    for (var i = 0; i < state.services.length; i++) {
        var svc = state.services[i];
        var cb = document.querySelector('[data-svc-running="' + svc.id + '"]');
        if (cb) cb.checked = !!m[svc.id];
    }
};

window.onSettingsError = function(msg) {
    console.error('Settings save error:', msg);
    var banner = document.createElement('div');
    banner.textContent = 'Failed to save settings: ' + msg;
    banner.style.cssText = 'position:fixed;top:0;left:0;right:0;padding:10px;background:#c0392b;color:#fff;text-align:center;z-index:9999;font-size:13px';
    document.body.appendChild(banner);
    setTimeout(function() { banner.remove(); }, 5000);
};

window.onSettingsReloaded = function(data) {
    state.externalMcps = data.external_mcps;
    state.services = data.services;
    state.runningServices = data.running_ids.reduce(function(m, id) { m[id] = true; return m; }, {});
    if (data.projects) state.projects = data.projects;
    if (data.mcp_tool_cache) state.mcpToolCache = data.mcp_tool_cache;
    if (data.mcp_scope_fields) state.mcpScopeFields = data.mcp_scope_fields;
    if (data.enrolments) state.enrolments = data.enrolments;
    if (data.remote) {
        state.remote = data.remote;
        // Re-seed the listener draft from the server's answer unless the user
        // is mid-edit; clobbering a half-typed address would be the same bug
        // render('push') avoids for the project form.
        if (!state.remoteDirty) state.remoteDraft = null;
    }
    // Push-sourced repaint of the currently visible tab; render() itself
    // skips if a form is mid-edit. Other tabs pick up the fresh state on
    // next switch — no need to repaint them now.
    render('push');
};

window.onProjectsReloaded = function(projects) {
    state.projects = projects || [];
    // External mutation — drop in-flight form edits to avoid showing stale data.
    if (state.editingProjectId && state.editingProjectId !== 'new') {
        const stillExists = state.projects.some(p => p.id === state.editingProjectId);
        if (!stillExists) {
            state.editingProjectId = null;
            state.projectForm = null;
        }
    }
    if (state.page === 'projects') render('push');
};

// ---------------------------------------------------------------------------
// Projects tab — list view + edit form with tri-state tool picker.
//
// State model: a single in-flight `state.projectForm` object holds the user's
// uncommitted edits. Tri-state buttons and tool checkboxes mutate it; Save
// dispatches `update_project` with the entire patch. Push-sourced renders
// (onProjectsReloaded, etc.) pass source='push' so render() can skip the
// repaint while editingProjectId is set, preserving keystrokes mid-edit.
// User-initiated render() calls always proceed (no source arg).
// ---------------------------------------------------------------------------

const PROJ_MCP_WILDCARD = '*';

// ---------------------------------------------------------------------------
// Effective authority (ADR-011 decisions 1 and 2)
//
// Everything below answers one question in one place: given a record and an
// MCP it grants, what can the client actually do? A row that says "MCPs: 1"
// while the client can read every mailbox on the machine is the problem this
// ADR exists to fix, so the same helpers feed the Projects list, the project
// editor, and the Remote Clients tab — a summary that disagreed with the
// editor beside it would be worse than no summary.
//
// The mode rule is StoredToken.AccessMode's, restated: an explicit entry that
// is not exactly "write" reads as read, and an ABSENT entry defaults read for
// an access profile and write for a local project. The asymmetry is
// deliberate (ADR-011 decision 2) and it is why this is a function rather than
// a lookup with a default argument — a caller that forgot which default
// applied would draw the wrong one.
// ---------------------------------------------------------------------------

// A remote-kind record is an ACCESS PROFILE everywhere an operator reads it
// (ADR-011 decision 1). It has no directory, no skills, no shell and no
// models, and calling it a project invites the reader to expect all four. The
// stored kind is unchanged; this is presentation only.
function projNoun(p) { return isRemoteProject(p) ? 'access profile' : 'project'; }

// mcpScopeFieldsFor returns what an MCP declares as narrowable, or null when
// relay has never connected to it. Null is not an empty list: "this MCP scopes
// nothing" and "relay cannot tell you what this MCP scopes" are different
// answers, and only the first one means an editor may safely offer no fields.
function mcpScopeFieldsFor(mcpID) {
    const m = state.mcpScopeFields || {};
    return Object.prototype.hasOwnProperty.call(m, mcpID) ? (m[mcpID] || []) : null;
}

// projGrantedMcpIds expands the wildcard the way SyncProjectToken does — to
// every MCP relay currently knows about — because that is what the grant
// actually reaches. A summary that printed "*" would be hiding the number the
// operator needs.
function projGrantedMcpIds(p) {
    const ids = (p && p.allowed_mcp_ids) || [];
    if (ids.length === 1 && ids[0] === PROJ_MCP_WILDCARD) {
        return (state.externalMcps || []).map(m => m.id);
    }
    return ids.slice();
}

function projAccessMode(p, mcpID) {
    const explicit = (p && p.access) ? p.access[mcpID] : undefined;
    if (explicit !== undefined && explicit !== null && explicit !== '') {
        return explicit === 'write' ? 'write' : 'read';
    }
    return isRemoteProject(p) ? 'read' : 'write';
}

// scopeValueIsSet mirrors hasScopeValue on the Go side: absent, null, empty
// string, empty list and empty object are all ABSENT, because a restrict field
// with no value refuses every call it governs. "No restriction" is not
// expressible as emptiness anywhere in this model.
function scopeValueIsSet(v) {
    if (v === undefined || v === null) return false;
    if (Array.isArray(v)) return v.length > 0;
    if (typeof v === 'string') return v.trim() !== '';
    if (typeof v === 'object') return Object.keys(v).length > 0;
    return true;
}

function projScopeValue(p, mcpID, fieldName) {
    const perMcp = (p && p.context) ? p.context[mcpID] : null;
    return perMcp ? perMcp[fieldName] : undefined;
}

// scopeValueText renders a stored value for a human. Arrays are the shape
// every scope field met so far declares; anything else prints as its JSON,
// which is honest about a shape this UI does not model.
function scopeValueText(v) {
    if (Array.isArray(v)) return v.join(', ');
    if (typeof v === 'string') return v;
    if (v === undefined || v === null) return '';
    return JSON.stringify(v);
}

// projMissingScopeFields names the OPERATOR-set restrict fields this record
// grants an MCP for but supplies no value for. Those are the ones an operator
// can fix; a project_path field on an access profile is reported separately,
// because there is nothing to type — the tools it governs are simply gone.
function projMissingScopeFields(p, mcpID) {
    const fields = mcpScopeFieldsFor(mcpID);
    if (!fields) return [];
    return fields
        .filter(f => f.source !== 'project_path')
        .filter(f => !scopeValueIsSet(projScopeValue(p, mcpID, f.name)))
        .map(f => f.name);
}

// projScopeGaps is the list-level form: every (record, MCP) pair still missing
// a value someone has to type. This is the operator-facing half of ADR-011's
// loud-and-closed behaviour — the client-facing half is a `denied` at call
// time, which is silent from the operator's side and baffling from the
// agent's.
function projScopeGaps(p) {
    const out = [];
    for (const mcpID of projGrantedMcpIds(p)) {
        const missing = projMissingScopeFields(p, mcpID);
        if (missing.length) out.push({ mcp: mcpID, fields: missing });
    }
    return out;
}

function projAllowedToolPatterns(p, mcpID) {
    return ((p && p.allowed_tools) ? p.allowed_tools[mcpID] : null) || [];
}

// projToolAuthorityText says which tools the grant admits. The two kinds are
// genuinely different mechanisms and the text says so rather than smoothing it
// over: an access profile holds only what allowed_tools enumerates (absent
// means NOTHING), a local project holds everything minus its denylist.
function projToolAuthorityText(p, mcpID) {
    const patterns = projAllowedToolPatterns(p, mcpID);
    if (isRemoteProject(p)) {
        return patterns.length ? patterns.join(', ') : 'no tools';
    }
    if (patterns.length) return patterns.join(', ');
    const disabled = ((p.disabled_tools || {})[mcpID] || []).length;
    return disabled ? ('all tools except ' + disabled) : 'all tools';
}

// projAuthorityRows is the one shape every authority summary renders from.
function projAuthorityRows(p) {
    return projGrantedMcpIds(p).map(function(mcpID) {
        const fields = mcpScopeFieldsFor(mcpID);
        const scope = [];
        const derived = [];
        for (const f of (fields || [])) {
            const v = projScopeValue(p, mcpID, f.name);
            if (f.source === 'project_path') {
                if (scopeValueIsSet(v)) derived.push(f.name + ': ' + scopeValueText(v));
                continue;
            }
            if (scopeValueIsSet(v)) scope.push(f.name + ': ' + scopeValueText(v));
        }
        return {
            mcp: mcpID,
            mode: projAccessMode(p, mcpID),
            tools: projToolAuthorityText(p, mcpID),
            scope: scope,
            derived: derived,
            missing: projMissingScopeFields(p, mcpID),
            schemaUnknown: fields === null,
        };
    });
}

function renderAuthorityRows(p) {
    const rows = projAuthorityRows(p);
    if (!rows.length) {
        return '<div class="proj-auth-row"><span class="proj-auth-none">no MCPs granted — this ' + esc(projNoun(p)) + ' reaches nothing</span></div>';
    }
    let html = '';
    for (const r of rows) {
        html += '<div class="proj-auth-row">';
        html += '<span class="proj-auth-mcp">' + esc(r.mcp) + '</span>';
        html += '<span class="proj-auth-mode ' + esc(r.mode) + '">' + esc(r.mode) + '</span>';
        html += '<span class="proj-auth-tools">' + esc(r.tools) + '</span>';
        if (r.scope.length) html += '<span class="proj-auth-scope">' + esc(r.scope.join(' · ')) + '</span>';
        if (r.missing.length) {
            html += '<span class="proj-auth-missing">needs a scope value for ' + esc(r.missing.join(', ')) + '</span>';
        } else if (!r.scope.length && !r.schemaUnknown) {
            html += '<span class="proj-auth-scope none">no resource scope declared by this MCP</span>';
        }
        if (r.schemaUnknown) html += '<span class="proj-auth-scope none">not connected — scope unknown</span>';
        html += '</div>';
    }
    return html;
}

function renderProjects() {
    if (state.editingProjectId) return renderProjectForm();

    let html = '<div class="page-header">';
    html += '<h2>Projects &amp; Access Profiles</h2>';
    html += '<button class="btn btn-primary" onclick="newProject()">+ New</button>';
    html += '</div>';
    html += '<p class="page-intro">Both kinds are the security boundary and both get a scoped bearer token. A <strong>project</strong> is bound to a host directory and has models, shell templates and skills. An <strong>access profile</strong> is a capability grant to a client on another machine: no directory, no skills, no shell, no models — just which MCPs, which tools, which operations and which resources.</p>';

    if (state.projectError) {
        html += '<div class="proj-error">' + esc(state.projectError) + '</div>';
    }
    html += renderScopeGapBanner();

    if (state.projects.length === 0) {
        html += '<div class="empty-state">Nothing here yet. Click <strong>+ New</strong> to create a project or an access profile.</div>';
    } else {
        for (const p of state.projects) {
            const remote = isRemoteProject(p);
            const modelsCount = p.allowed_models && p.allowed_models.length > 0
                ? (p.allowed_models[0] === PROJ_MCP_WILDCARD ? 'all' : String(p.allowed_models.length))
                : '0';
            const skillState = p.generate_skill ? 'auto' : 'off';
            const policy = (p.permission_policy && p.permission_policy.default_mode) || '—';
            const regen = state.projectSkillRegen[p.id];
            html += '<div class="proj-card">';
            html += '<div class="proj-card-header">';
            html += '<div style="display:flex;align-items:center;gap:6px">';
            html += '<span class="proj-card-name">' + esc(p.name) + '</span>';
            if (remote) html += '<span class="proj-badge-remote">Access profile</span>';
            html += '</div>';
            html += '<div style="display:flex;gap:4px">';
            html += '<button class="btn btn-sm" onclick="editProject(\'' + esc(p.id) + '\')">Edit</button>';
            // Regen Skill is absent for a profile rather than disabled:
            // validateProjectShape refuses generate_skill on a remote record,
            // and the regen handler refuses a record with no path, so the
            // button could never do anything. ADR-009 decision 2's argument
            // applies to the control as much as to the flag — refusing at the
            // door is more honest than something that quietly no-ops.
            if (!remote) {
                html += '<button class="btn btn-sm" onclick="regenProjectSkill(\'' + esc(p.id) + '\')" title="Regenerate SKILL.md now">Regen Skill</button>';
            }
            html += '<button class="btn btn-sm btn-danger" onclick="removeProject(\'' + esc(p.id) + '\', \'' + esc(p.name) + '\')">Delete</button>';
            html += '</div></div>';
            html += '<div class="proj-card-path">' + esc(remote ? 'no host directory — an access profile grants capability, not a filesystem' : (p.path || '(no path)')) + '</div>';
            // What this record can actually DO, per MCP: mode, tools, scope.
            // Counts alone were the ADR's own example of the failure — "MCPs: 1"
            // beside a client that can read every mailbox on the machine.
            html += '<div class="proj-authority">' + renderAuthorityRows(p) + '</div>';
            html += '<div class="proj-card-meta">';
            if (!remote) {
                html += '<span>Models: <strong>' + esc(modelsCount) + '</strong></span>';
                html += '<span>Skill: <strong>' + esc(skillState) + '</strong></span>';
            }
            html += '<span>Policy: <strong>' + esc(policy) + '</strong></span>';
            // Only shown when on: directory auth is the exception, and a row of
            // "off" labels would bury the projects where it's actually enabled.
            if (p.allow_cwd_auth) html += '<span>Dir auth: <strong>on</strong></span>';
            html += '</div>';
            if (regen) {
                const cls = regen.ok ? 'proj-ok' : 'proj-error';
                html += '<div class="' + cls + '">' + (regen.ok ? '✓ Regenerated: ' : '✗ Regen failed: ') + esc(regen.message) + '</div>';
            }
            html += '</div>';
        }
    }

    return html;
}

// renderScopeGapBanner is ADR-011's "N profiles need a scope value for
// `macmcp`" — the operator-facing half of loud-and-closed. Without it the only
// signal is a `denied` in the audit log for a call the operator never saw,
// against a grant the UI otherwise renders as complete.
function renderScopeGapBanner() {
    const rows = [];
    for (const p of (state.projects || [])) {
        for (const gap of projScopeGaps(p)) {
            rows.push({ name: p.name, noun: projNoun(p), mcp: gap.mcp, fields: gap.fields });
        }
    }
    if (!rows.length) return '';
    let html = '<div class="proj-scope-gap">';
    html += '<strong>' + rows.length + (rows.length === 1 ? ' grant needs' : ' grants need') + ' a scope value.</strong> ';
    html += 'Until one is set, every tool the field governs is <em>denied</em> at call time — the client gets nothing and nothing else says why.';
    html += '<ul>';
    for (const r of rows) {
        html += '<li>' + esc(r.name) + ' (' + esc(r.noun) + ') → <code>' + esc(r.mcp) + '</code>: ' + esc(r.fields.join(', ')) + '</li>';
    }
    html += '</ul></div>';
    return html;
}

function blankProjectForm() {
    return {
        id: null,
        kind: 'local',                            // 'local' | 'remote' — see setProjKind
        name: '',
        path: '',
        allowed_mcp_ids: [PROJ_MCP_WILDCARD],   // wildcard by default
        allowed_models: [PROJ_MCP_WILDCARD],
        chat_templates: [],
        permission_policy: { default_mode: '', allowed_tools: [], denied_tools: [] },
        generate_skill: false,
        allow_cwd_auth: false,                   // token-less auth by working directory
        disabled_tools: {},                      // mcpID -> [toolName, ...]
        // The ADR-011 permission set. access and allowed_tools are what relay
        // enforces at its own chokepoint; context is what it injects and
        // cannot verify. All three are absent by default, which for an access
        // profile means read, no tools, and no scope — every layer failing
        // closed until someone widens it deliberately.
        access: {},                              // mcpID -> 'read' | 'write'
        allowed_tools: {},                       // mcpID -> [pattern, ...]
        context: {},                             // mcpID -> { field: value }
        // Raw text as typed, so a half-finished value survives a re-render and
        // is parsed exactly once, at harvest. Underscore-prefixed: never sent.
        _scopeText: {},                          // mcpID -> { field: text }
        _toolsText: {},                          // mcpID -> text
    };
}

function projectFormFromExisting(p) {
    // Deep-clone so edits don't mutate state.projects until Save.
    const policy = p.permission_policy || {};
    return {
        id: p.id,
        kind: isRemoteProject(p) ? 'remote' : 'local',
        name: p.name || '',
        path: p.path || '',
        allowed_mcp_ids: (p.allowed_mcp_ids || []).slice(),
        allowed_models: (p.allowed_models || []).slice(),
        chat_templates: JSON.parse(JSON.stringify(p.chat_templates || [])),
        permission_policy: {
            default_mode: policy.default_mode || '',
            allowed_tools: (policy.allowed_tools || []).slice(),
            denied_tools: (policy.denied_tools || []).slice(),
        },
        generate_skill: !!p.generate_skill,
        allow_cwd_auth: !!p.allow_cwd_auth,
        disabled_tools: JSON.parse(JSON.stringify(p.disabled_tools || {})),
        access: JSON.parse(JSON.stringify(p.access || {})),
        allowed_tools: JSON.parse(JSON.stringify(p.allowed_tools || {})),
        context: JSON.parse(JSON.stringify(p.context || {})),
        _scopeText: {},
        _toolsText: {},
        token: p.token || '',
    };
}

function newProject() {
    state.editingProjectId = 'new';
    state.projectForm = blankProjectForm();
    state.projectFormError = null;
    render();
}

function editProject(id) {
    const p = state.projects.find(x => x.id === id);
    if (!p) return;
    state.editingProjectId = id;
    state.projectForm = projectFormFromExisting(p);
    state.projectFormError = null;
    render();
}

function cancelProjectEdit() {
    state.editingProjectId = null;
    state.projectForm = null;
    state.projectFormError = null;
    render();
}

function regenProjectSkill(id) {
    ipc(JSON.stringify({ type: 'regen_project_skill', id }));
}

function removeProject(id, name) {
    const p = (state.projects || []).find(x => x.id === id);
    const remote = isRemoteProject(p);
    const what = remote ? 'access profile' : 'project';
    // A profile has no skills to remove, and saying it does would be the
    // clearest possible signal that the two kinds are being confused.
    const consequence = remote
        ? '\nThis revokes its token immediately. Any enrolment granting it is left holding an id that resolves to nothing.'
        : '\nThis revokes its token immediately and removes its SKILL.md.';
    if (!confirm('Delete ' + what + ' "' + name + '"?' + consequence)) return;
    ipc(JSON.stringify({ type: 'remove_project', id }));
}

function rotateProjectToken(id, name) {
    if (!confirm('Rotate the bearer token for "' + name + '"?\n\nAny active Eve / relayLLM / CLI session using the old token will get auth errors and must re-authenticate.\n\nThe new token will be shown ONCE — copy it before navigating away.')) return;
    state.rotatingProjectId = id;
    ipc(JSON.stringify({ type: 'rotate_project_token', id }));
}

function toggleProjectTokenVisible(id) {
    state.projectTokenVisible[id] = !state.projectTokenVisible[id];
    render();
}

function copyProjectToken(text) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text);
    }
}

// ---- Kind helpers ----
//
// A remote-kind record is an ACCESS PROFILE (ADR-011 decision 1): a capability
// grant to an agent on another machine, not a host directory. It carries no
// path, can't use allow_cwd_auth or generate_skill (both are
// directory-flavored), can't use
// the "*" MCP wildcard (a remote grant must be an explicit enumeration —
// see validateProjectShape in project_apply.go), and always sends an empty
// allowed_models. Kind is chosen at create time only; the edit form shows it
// read-only (see renderProjectForm) because converting an existing project
// has real consequences and isn't something to expose as a casual dropdown.

function isRemoteProject(p) {
    return !!p && p.kind === 'remote';
}

function isRemoteForm(f) {
    return !!f && f.kind === 'remote';
}

function setProjKind(kind) {
    const f = state.projectForm;
    if (!f) return;
    f.kind = kind;
    if (kind === 'remote') {
        // The wildcard means "every MCP relay currently knows about" — on a
        // remote grant that would let a future MCP registration silently
        // widen what the remote client can reach, so it's not offered (see
        // the MCP section below). If the user had it set (the new-project
        // default), drop to an empty explicit list rather than letting Save
        // send something the server will reject.
        if (isProjMcpWildcard(f)) f.allowed_mcp_ids = [];
        // Remote projects always carry an empty model allowlist.
        f.allowed_models = [];
    }
    render();
}

// ---- Tri-state helpers ----

function projMcpState(form, mcpID) {
    if (isProjMcpWildcard(form)) return 'all';
    if (form.allowed_mcp_ids.indexOf(mcpID) < 0) return 'none';
    // Key presence (even with an empty array) is the "selected mode" sentinel —
    // setProjMcpState writes `[]` when the user clicks Selected before unchecking
    // anything. Length-based checks here would flip the UI back to "All tools".
    return Object.prototype.hasOwnProperty.call(form.disabled_tools || {}, mcpID) ? 'selected' : 'all';
}

function isProjMcpWildcard(form) {
    return form.allowed_mcp_ids.length === 1 && form.allowed_mcp_ids[0] === PROJ_MCP_WILDCARD;
}

function setProjMcpState(mcpID, newState) {
    const f = state.projectForm;
    if (!f) return;
    // Expand wildcard into the explicit list before the first per-MCP edit so
    // subsequent toggles work on a real ID set. Wildcard is reachable again
    // via the wildcard toggle button.
    if (isProjMcpWildcard(f)) {
        f.allowed_mcp_ids = state.externalMcps.map(m => m.id);
        f.disabled_tools = {};
    }
    if (newState === 'none') {
        f.allowed_mcp_ids = f.allowed_mcp_ids.filter(id => id !== mcpID);
        delete f.disabled_tools[mcpID];
    } else if (newState === 'all') {
        if (f.allowed_mcp_ids.indexOf(mcpID) < 0) f.allowed_mcp_ids.push(mcpID);
        delete f.disabled_tools[mcpID];
    } else { // 'selected'
        if (f.allowed_mcp_ids.indexOf(mcpID) < 0) f.allowed_mcp_ids.push(mcpID);
        // Mark with empty disabled list so derivation returns 'selected'; the
        // moment the user unchecks a tool, that tool name goes in.
        if (!f.disabled_tools[mcpID]) f.disabled_tools[mcpID] = [];
        // Trigger a tool-list fetch if we don't already have it cached.
        if (!state.mcpToolCache[mcpID]) {
            ipc(JSON.stringify({ type: 'list_mcp_tools', mcp_id: mcpID }));
        }
    }
    render();
}

function setProjMcpWildcard(checked) {
    const f = state.projectForm;
    if (!f) return;
    if (checked) {
        f.allowed_mcp_ids = [PROJ_MCP_WILDCARD];
        f.disabled_tools = {};
    } else {
        // Drop wildcard; start with no MCPs (user picks explicitly).
        f.allowed_mcp_ids = [];
    }
    render();
}

function toggleProjTool(mcpID, toolName, isChecked) {
    const f = state.projectForm;
    if (!f) return;
    const live = state.mcpToolCache[mcpID] || [];
    const liveNames = live.map(t => t.name);
    // "Selected" semantics: disabled_tools[mcpID] = liveNames - checked.
    const currentlyDisabled = new Set(f.disabled_tools[mcpID] || []);
    if (isChecked) {
        currentlyDisabled.delete(toolName);
    } else {
        currentlyDisabled.add(toolName);
    }
    // Preserve any disabled-tool names that no longer exist in the live MCP
    // (renamed/removed). They're invisible permissively, but the user may
    // want to keep the entry until they explicitly prune.
    for (const name of (f.disabled_tools[mcpID] || [])) {
        if (liveNames.indexOf(name) < 0) currentlyDisabled.add(name);
    }
    f.disabled_tools[mcpID] = Array.from(currentlyDisabled);
}

// ---------------------------------------------------------------------------
// The per-MCP permission panel (ADR-011 decisions 2, 2b, 4, 6)
//
// One panel per granted MCP, and it is the whole authority in one place: which
// operations (the mode), which tools (the allowlist, profiles only), and which
// resources (one input per scope: "restrict" field the MCP declares).
//
// Scope values are TEXT ENTRY for now. `context/enumerate` (decision 6) is a
// later change that replaces them with pickers over real values, and the swap
// is meant to be local: renderScopeFieldInput is the only function that decides
// what a field's control looks like, and scopeValueFromText / scopeTextFromValue
// are the only two that convert between what is typed and what is stored.
// ---------------------------------------------------------------------------

// projFormAccessMode is projAccessMode against the in-flight form, which
// carries `kind` as a string rather than as the stored ProjectKind.
function projFormAccessMode(f, mcpID) {
    const explicit = (f.access || {})[mcpID];
    if (explicit === 'read' || explicit === 'write') return explicit;
    return isRemoteForm(f) ? 'read' : 'write';
}

function setProjAccess(mcpID, mode) {
    const f = state.projectForm;
    if (!f) return;
    if (!f.access) f.access = {};
    f.access[mcpID] = mode;
    render();
}

// setProjMcpGranted is the access-profile form's grant control. A profile has
// no tri-state, because the third state is a DENYLIST and a denylist cannot
// bound a client — validateProjectShape refuses disabled_tools on a remote
// record outright (ADR-011 decision 2b). What replaces it is the allowed-tools
// box in the panel below.
function setProjMcpGranted(mcpID, granted) {
    const f = state.projectForm;
    if (!f) return;
    if (granted) {
        if (f.allowed_mcp_ids.indexOf(mcpID) < 0) f.allowed_mcp_ids.push(mcpID);
    } else {
        f.allowed_mcp_ids = f.allowed_mcp_ids.filter(id => id !== mcpID);
        // Drop the permission set with the grant. Relay's mutators prune these
        // on every resync anyway; leaving them here would show a mode and a
        // scope for an MCP the record no longer reaches, which reads as an
        // authority it does not have.
        delete (f.access || {})[mcpID];
        delete (f.allowed_tools || {})[mcpID];
        delete (f.context || {})[mcpID];
        delete (f._scopeText || {})[mcpID];
        delete (f._toolsText || {})[mcpID];
    }
    render();
}

// ---- Scope value <-> text --------------------------------------------------
//
// The one pair of functions that knows how a typed value becomes a stored one.
// A picker replaces the CONTROL, not this conversion.

function scopeTextFromValue(field, v) {
    if (v === undefined || v === null) return '';
    if (Array.isArray(v)) return v.join('\n');
    if (typeof v === 'string') return v;
    return JSON.stringify(v);
}

function scopeValueFromText(field, text) {
    const raw = String(text === undefined || text === null ? '' : text);
    if (field.type === 'array') {
        return raw.split('\n').map(s => s.trim()).filter(Boolean);
    }
    if (field.type === 'string') return raw.trim();
    // A type outside the declared subset. Send what parses as JSON, otherwise
    // the trimmed string — and let the server refuse it, which it will do
    // against the MCP's own declaration rather than against a guess made here.
    const trimmed = raw.trim();
    if (!trimmed) return '';
    try { return JSON.parse(trimmed); } catch (e) { return trimmed; }
}

function projScopeText(f, mcpID, field) {
    const typed = (f._scopeText || {})[mcpID];
    if (typed && Object.prototype.hasOwnProperty.call(typed, field.name)) return typed[field.name];
    return scopeTextFromValue(field, ((f.context || {})[mcpID] || {})[field.name]);
}

function setProjScopeText(mcpID, fieldName, text) {
    const f = state.projectForm;
    if (!f) return;
    if (!f._scopeText) f._scopeText = {};
    if (!f._scopeText[mcpID]) f._scopeText[mcpID] = {};
    f._scopeText[mcpID][fieldName] = text;
    // No render(): this fires on every keystroke and a repaint would eat the
    // caret. The list-level "needs a scope value" banner catches up on save.
}

function projAllowedToolsText(f, mcpID) {
    const typed = (f._toolsText || {})[mcpID];
    if (typed !== undefined) return typed;
    return ((f.allowed_tools || {})[mcpID] || []).join('\n');
}

function setProjAllowedToolsText(mcpID, text) {
    const f = state.projectForm;
    if (!f) return;
    if (!f._toolsText) f._toolsText = {};
    f._toolsText[mcpID] = text;
}

// ---- The panel -------------------------------------------------------------

function renderProjMcpPermissions(mcpID, f) {
    const remote = isRemoteForm(f);
    const mode = projFormAccessMode(f, mcpID);
    let html = '<div class="proj-perm-panel">';

    // Which operations.
    html += '<div class="proj-perm-block">';
    html += '<div class="proj-perm-label">Operations</div>';
    html += '<div class="perm-btns">';
    html += '<button class="perm-btn ' + (mode === 'read' ? 'active' : '') + '" onclick="setProjAccess(\'' + esc(mcpID) + '\', \'read\')">Read</button>';
    html += '<button class="perm-btn ' + (mode === 'write' ? 'active' : '') + '" onclick="setProjAccess(\'' + esc(mcpID) + '\', \'write\')">Write</button>';
    html += '</div>';
    html += '<p class="proj-section-help">' + (mode === 'read'
        ? 'Only tools this MCP annotates <code>readOnlyHint: true</code>. A tool that is unannotated, malformed, or added later is refused — that is what keeps a new mutating tool out of an old grant.'
        : 'Every tool this grant admits, mutating included. Write implies read.')
        + ' Unset defaults to <strong>' + (remote ? 'read' : 'write') + '</strong> for ' + (remote ? 'an access profile' : 'a local project') + '.</p>';
    html += '</div>';

    // Which tools — profiles only. A local project subtracts with the tool
    // picker above; a profile enumerates, because a denylist grants every tool
    // the MCP gains tomorrow.
    if (remote) {
        const patterns = projAllowedToolsText(f, mcpID);
        html += '<div class="proj-perm-block">';
        html += '<div class="proj-perm-label">Tools</div>';
        html += '<p class="proj-section-help">One name or pattern per line, e.g. <code>mail_*</code>. Patterns are anchored — <code>mail_*</code> admits <code>mail_send</code> and not <code>xmail_send</code>. A pattern that matches by <em>shape</em> rather than by name is refused, whatever it is spelled as: <code>*</code>, <code>**</code>, <code>?*</code>, <code>[a-z]*</code>, <code>*_*</code> all match every tool the MCP has, so a tool registered tomorrow would join this grant with nobody reviewing it. There is no way to say &quot;everything&quot; here, deliberately. <strong>Empty means no tools at all.</strong></p>';
        html += '<textarea rows="3" placeholder="mail_*" oninput="setProjAllowedToolsText(\'' + esc(mcpID) + '\', this.value)">' + esc(patterns) + '</textarea>';
        html += '</div>';
    }

    // Which resources.
    const fields = mcpScopeFieldsFor(mcpID);
    html += '<div class="proj-perm-block">';
    html += '<div class="proj-perm-label">Resource scope</div>';
    if (fields === null) {
        html += '<p class="proj-section-help">Relay has not connected to this MCP, so it cannot say what may be narrowed. Anything this MCP scopes will be enforced at call time regardless — a grant with no value for a field it declares is <em>denied</em>, not unrestricted.</p>';
    } else if (!fields.length) {
        html += '<p class="proj-section-help">This MCP declares nothing narrowable. The grant is bounded by the tools and the mode above and by nothing else.</p>';
    } else {
        html += '<p class="proj-section-help">Each field below is declared by the MCP as one that <strong>restricts</strong> access. An empty value is not "no restriction" — it refuses every tool the field governs. There is no wildcard: to allow everything, list everything.</p>';
        for (const field of fields) {
            html += renderScopeFieldInput(mcpID, field, f);
        }
    }
    html += '</div>';

    html += '</div>';
    return html;
}

// renderScopeFieldInput is the one place a scope field's CONTROL is chosen, so
// swapping text entry for a `context/enumerate` picker (ADR-011 decision 6) is
// a change to this function and nothing else.
function renderScopeFieldInput(mcpID, field, f) {
    const remote = isRemoteForm(f);
    const typeLabel = field.type === 'array'
        ? ('list of ' + (field.item_type || 'value') + 's, one per line')
        : (field.type || 'value');
    let html = '<div class="proj-scope-field">';
    html += '<label>' + esc(field.name) + ' <span class="proj-scope-type">' + esc(typeLabel) + '</span></label>';
    if (field.description) html += '<div class="proj-scope-desc">' + esc(field.description) + '</div>';

    if (field.source === 'project_path') {
        // Read-only, and it shows the DERIVED value rather than hiding the
        // field: an operator has to be able to see that the bound exists
        // without being able to type in it. Relay refuses an operator-supplied
        // value for one of these outright — it would be overwritten by the
        // next resync anyway.
        const derived = ((f.context || {})[mcpID] || {})[field.name];
        let shown, note;
        if (remote) {
            shown = '';
            note = 'An access profile has no host directory, so relay derives nothing here and every tool this field governs'
                + (field.applies_to && field.applies_to.length ? ' (' + field.applies_to.join(', ') + ')' : '')
                + ' is refused. That is the intended outcome, not a gap to fill in.';
        } else if (scopeValueIsSet(derived)) {
            shown = scopeTextFromValue(field, derived);
            note = 'Derived by relay from this project\'s path. Change the path to change it.';
        } else {
            shown = f.path || '';
            note = 'Derived by relay from this project\'s path on save.';
        }
        html += '<input type="text" readonly value="' + esc(shown) + '" placeholder="(nothing derived)" />';
        html += '<div class="proj-scope-desc">' + esc(note) + '</div>';
        html += '</div>';
        return html;
    }

    const text = projScopeText(f, mcpID, field);
    // A picker over real values wherever the MCP says it can list them, and the
    // text box everywhere else — including for an MCP that answered "I do not
    // implement that", which is a permanent, silent degrade for that MCP.
    if (field.enumerable && !state.scopeEnumUnsupported[mcpID]) {
        html += renderScopeFieldPicker(mcpID, field, f, text);
    } else {
        html += renderScopeFieldTextInput(mcpID, field, text);
        if (field.enumerable) {
            html += '<div class="proj-scope-desc">' + esc(mcpID) + ' cannot list this field\'s values, so it is typed by hand. Spelling counts: a value that matches nothing refuses every tool the field governs, silently.</div>';
        }
    }
    if (!String(text).trim()) {
        html += '<div class="proj-scope-missing">No value: every tool this field governs is denied at call time.</div>';
    }
    if (field.depends_on && field.depends_on.length) {
        html += '<div class="proj-scope-desc">Values here are read within ' + esc(field.depends_on.join(', ')) + '.</div>';
    }
    html += '</div>';
    return html;
}

// renderScopeFieldTextInput is the free-text control, which is both the
// non-enumerable case and the fallback for every way enumeration can fail. It
// is deliberately the SAME storage as the picker — see setProjScopeText — so
// the two controls are interchangeable and a value typed here survives the
// picker appearing later, and vice versa.
function renderScopeFieldTextInput(mcpID, field, text) {
    if (field.type === 'array') {
        return '<textarea rows="3" oninput="setProjScopeText(\'' + esc(mcpID) + '\', \'' + esc(field.name) + '\', this.value)">' + esc(text) + '</textarea>';
    }
    return '<input type="text" value="' + esc(text) + '" oninput="setProjScopeText(\'' + esc(mcpID) + '\', \'' + esc(field.name) + '\', this.value)" />';
}

// ---------------------------------------------------------------------------
// The enumeration picker (ADR-011 decision 6)
//
// The picker is a CONTROL OVER THE SAME VALUE the text box edits. It reads
// scopeValueFromText and writes back through setProjScopeText, so harvest,
// clearing, validation and the "needs a scope value" banner all keep working
// unchanged, and an operator can move between the two without either one
// losing what the other set.
//
// That is also what makes the most important behaviour here free: a stored
// value the MCP no longer offers is IN the value already, so it is rendered as
// a selected, flagged entry rather than silently dropped. An account renamed
// on the host must not quietly widen or narrow a profile by disappearing from
// a form.
// ---------------------------------------------------------------------------

// scopeEnumValueKey is the identity of one offered value. Strings are the
// declared subset's shape; anything else is compared by its JSON, which is
// honest about a shape this UI does not model.
function scopeEnumValueKey(v) {
    return typeof v === 'string' ? v : JSON.stringify(v);
}

// scopeDependencyValues mirrors dependencyValues on the Go side: the fields
// THIS one declares in depends_on, and only those with a value.
//
// Dropping an empty one is the load-bearing half. The picker's normal opening
// state is mail_mailboxes with mail_accounts still unchosen, and a request
// carrying {"mail_accounts": []} invites a server to read it as "match
// nothing" — an empty picker at exactly the moment an operator opens one, and
// indistinguishable from a host with no mailboxes. Relay drops it again on the
// way out; this side is what keeps the CACHE KEY the same for "unchosen"
// however the emptiness is spelled.
function scopeDependencyValues(f, mcpID, field) {
    const out = {};
    const fields = mcpScopeFieldsFor(mcpID) || [];
    for (const dep of (field.depends_on || [])) {
        const depField = fields.find(x => x.name === dep);
        if (!depField) continue;
        const v = scopeValueFromText(depField, projScopeText(f, mcpID, depField));
        if (scopeValueIsSet(v)) out[dep] = v;
    }
    return out;
}

// scopeEnumKey identifies one answer: an MCP, a field, and the dependency
// values it was read within. The dependencies are in the key because changing
// the account choice must invalidate the mailbox list rather than leave a
// stale one on screen under a new account's name.
function scopeEnumKey(mcpID, fieldName, deps) {
    return mcpID + ' ' + fieldName + ' ' + JSON.stringify(deps || {});
}

function scopeOpenKey(mcpID, fieldName) { return mcpID + ' ' + fieldName; }

function scopeFieldIsOpen(mcpID, fieldName) {
    return !!state.scopeEnumOpen[scopeOpenKey(mcpID, fieldName)];
}

// requestScopeEnum fires at most one live call per (mcp, field, dependency
// values) for the life of the form. It is never called from a paint or a
// keystroke — only from opening the control, from a retry, and from a change
// to a field something else depends on.
function requestScopeEnum(mcpID, field, force) {
    const f = state.projectForm;
    if (!f) return;
    if (state.scopeEnumUnsupported[mcpID]) return;
    const deps = scopeDependencyValues(f, mcpID, field);
    const key = scopeEnumKey(mcpID, field.name, deps);
    const openKey = scopeOpenKey(mcpID, field.name);
    if (!force) {
        if (Object.prototype.hasOwnProperty.call(state.scopeEnum, key)) return; // cached
        if (state.scopeEnumReq[openKey] === key) return;                        // in flight
    }
    delete state.scopeEnum[key];
    state.scopeEnumReq[openKey] = key;
    ipc(JSON.stringify({ type: 'enumerate_scope_field', mcp_id: mcpID, field: field.name, values: deps }));
}

function scopeFieldByName(mcpID, fieldName) {
    return (mcpScopeFieldsFor(mcpID) || []).find(x => x.name === fieldName) || null;
}

function toggleScopeFieldPicker(mcpID, fieldName) {
    const field = scopeFieldByName(mcpID, fieldName);
    if (!field) return;
    captureProjectFormInputs();
    const openKey = scopeOpenKey(mcpID, fieldName);
    state.scopeEnumOpen[openKey] = !state.scopeEnumOpen[openKey];
    if (state.scopeEnumOpen[openKey]) requestScopeEnum(mcpID, field);
    render();
}

function retryScopeEnum(mcpID, fieldName) {
    const field = scopeFieldByName(mcpID, fieldName);
    if (!field) return;
    captureProjectFormInputs();
    requestScopeEnum(mcpID, field, true);
    render();
}

// refreshDependentScopeFields re-asks for every OPEN field that declares the
// changed one in depends_on. This is what makes dependency order real rather
// than decorative: choosing an account changes which mailboxes exist, and a
// list that did not move would be a picker offering another account's values.
function refreshDependentScopeFields(mcpID, changedField) {
    for (const field of (mcpScopeFieldsFor(mcpID) || [])) {
        if (!field.enumerable) continue;
        if (!(field.depends_on || []).includes(changedField)) continue;
        if (!scopeFieldIsOpen(mcpID, field.name)) continue;
        requestScopeEnum(mcpID, field);
    }
}

// scopeSelectedValues is what the field currently holds, as a list, whichever
// control produced it.
function scopeSelectedValues(field, text) {
    const v = scopeValueFromText(field, text);
    if (Array.isArray(v)) return v;
    return scopeValueIsSet(v) ? [v] : [];
}

// unrecognisedScopeValues names the stored values the MCP did not offer. It
// answers only when there IS an answer to compare against — an empty list from
// a failed call would flag every stored value as unrecognised, which is the
// same lie as rendering the failure as "there are none".
function unrecognisedScopeValues(selected, offered) {
    const known = new Set((offered || []).map(o => scopeEnumValueKey(o.value)));
    return selected.filter(v => !known.has(scopeEnumValueKey(v)));
}

function renderScopeFieldPicker(mcpID, field, f, text) {
    const selected = scopeSelectedValues(field, text);
    const open = scopeFieldIsOpen(mcpID, field.name);
    const deps = scopeDependencyValues(f, mcpID, field);
    const key = scopeEnumKey(mcpID, field.name, deps);
    const res = state.scopeEnum[key];
    const pending = !res && state.scopeEnumReq[scopeOpenKey(mcpID, field.name)] === key;
    const args = '\'' + esc(mcpID) + '\', \'' + esc(field.name) + '\'';

    // Always visible, open or closed: what is stored is what confines the
    // client, so it never sits behind a control someone has to open.
    let html = '<div class="proj-scope-summary">' + (selected.length
        ? esc(selected.map(scopeEnumValueKey).join(', '))
        : '<span class="proj-scope-none">nothing selected — every tool this field governs is refused</span>') + '</div>';

    const unknown = (res && res.status === 'ok') ? unrecognisedScopeValues(selected, res.values) : [];
    if (unknown.length) {
        html += '<div class="proj-scope-unrecognised">' + esc(mcpID) + ' does not offer ' + esc(unknown.map(scopeEnumValueKey).join(', '))
            + '. Kept and still in force — it may have been renamed on the host, or this MCP may be reading a different one. Untick it to remove it.</div>';
    }

    html += '<button class="btn btn-sm" onclick="toggleScopeFieldPicker(' + args + ')">'
        + (open ? 'Done' : 'Choose values…') + '</button>';
    if (!open) return html;

    if (pending) return html + '<div class="proj-scope-pending">Listing values from ' + esc(mcpID) + '…</div>';
    if (!res) {
        // Open, with no answer for THESE dependency values: something the list
        // is read within was edited by hand since it was last asked. Offered as
        // a button rather than fetched from here, because a paint must never
        // start a live call — a re-render loop would be one call per frame.
        return html + '<div class="proj-scope-pending">The values this list is read within have changed.</div>'
            + '<button class="btn btn-sm" onclick="retryScopeEnum(' + args + ')">List values</button>';
    }

    if (res.status === 'ok') {
        return html + renderScopeChoices(mcpID, field, selected, res.values, unknown, deps);
    }

    // Every remaining status keeps the text box, so an operator is never
    // blocked by an MCP that will not answer — and none of them renders as an
    // empty list of values, which would read as "there are none".
    if (res.status === 'invalid_field' || res.status === 'not_enumerable') {
        html += '<div class="proj-scope-failed">Relay asked ' + esc(mcpID) + ' for values it will not enumerate, which is a bug in relay rather than in this profile'
            + (res.error ? ': ' + esc(res.error) : '.') + ' Type the values instead — they are stored and enforced exactly the same way.</div>';
    } else if (res.status === 'unsupported') {
        html += '<div class="proj-scope-desc">' + esc(mcpID) + ' cannot list this field\'s values.</div>';
    } else {
        html += '<div class="proj-scope-failed">Could not list values from ' + esc(mcpID) + ' just now'
            + (res.error ? ': ' + esc(res.error) : '.')
            + ' This is not an empty list — nothing was read. Retry, or type the values.</div>';
        html += '<button class="btn btn-sm" onclick="retryScopeEnum(' + args + ')">Try again</button>';
    }
    return html + renderScopeFieldTextInput(mcpID, field, text);
}

// renderScopeChoices draws one row per offered value, plus one per stored
// value the MCP did not offer — checked, flagged, and removable only by an
// explicit untick.
//
// Values reach their handler through an index into a per-render binding array
// rather than through the onclick attribute. Mailbox names carry quotes,
// apostrophes and emoji, and building a JS string literal out of one in an
// HTML attribute is how a mailbox called `a'); doSomething('` becomes a bug.
function renderScopeChoices(mcpID, field, selected, offered, unknown, deps) {
    const multi = field.type === 'array';
    const rows = [];
    const byKey = {};
    const push = (value, label, isUnknown) => {
        const k = scopeEnumValueKey(value);
        // One value is one choice, however many times it was offered. A
        // cross-product scope makes duplicates normal — every account has an
        // INBOX, and the mailbox value is account-independent — and two boxes
        // holding the same value would tick and untick together, which reads
        // as a bug. The labels are joined instead, because each said something
        // true about where the value came from.
        if (byKey[k]) {
            if (label && byKey[k].labels.indexOf(label) < 0) byKey[k].labels.push(label);
            return;
        }
        byKey[k] = { value: value, labels: [label], unknown: isUnknown };
        rows.push(byKey[k]);
    };
    for (const v of unknown) push(v, scopeEnumValueKey(v), true);
    for (const o of (offered || [])) push(o.value, o.label || scopeEnumValueKey(o.value), false);
    for (const row of rows) row.label = row.labels.join(' · ');

    if (!rows.length) {
        return '<div class="proj-scope-desc">' + esc(mcpID) + ' offers no values for this field'
            + (Object.keys(deps).length ? ' within the values chosen above' : '')
            + '. That is its answer, not a failure — there is nothing here to grant.</div>';
    }

    const selectedKeys = new Set(selected.map(scopeEnumValueKey));
    let html = '<div class="proj-scope-choices">';
    for (const row of rows) {
        const idx = state._scopeBind.length;
        state._scopeBind.push({ mcpID: mcpID, field: field.name, value: row.value });
        const checked = selectedKeys.has(scopeEnumValueKey(row.value)) ? ' checked' : '';
        html += '<div class="proj-scope-choice' + (row.unknown ? ' unrecognised' : '') + '">';
        html += '<input type="' + (multi ? 'checkbox' : 'radio') + '" name="scope-' + esc(mcpID) + '-' + esc(field.name) + '"'
            + checked + ' onchange="toggleProjScopeValueAt(' + idx + ', this.checked)" />';
        html += '<label>' + esc(row.label) + '</label>';
        if (row.unknown) html += '<span class="desc">not offered by this MCP</span>';
        html += '</div>';
    }
    html += '</div>';
    return html;
}

// toggleProjScopeValueAt writes the choice back through the SAME text the box
// edits, so nothing else in the editor has to know a picker exists.
function toggleProjScopeValueAt(index, checked) {
    const bind = state._scopeBind[index];
    if (!bind) return;
    const f = state.projectForm;
    if (!f) return;
    const field = scopeFieldByName(bind.mcpID, bind.field);
    if (!field) return;
    captureProjectFormInputs();

    const current = scopeSelectedValues(field, projScopeText(f, bind.mcpID, field));
    const key = scopeEnumValueKey(bind.value);
    let next;
    if (field.type === 'array') {
        next = current.filter(v => scopeEnumValueKey(v) !== key);
        if (checked) next.push(bind.value);
    } else {
        next = checked ? [bind.value] : [];
    }
    setProjScopeText(bind.mcpID, bind.field, scopeTextFromValue(field, field.type === 'array' ? next : (next[0] !== undefined ? next[0] : '')));
    // Anything read WITHIN this field is now reading within something else.
    refreshDependentScopeFields(bind.mcpID, bind.field);
    render();
}

// captureProjectFormInputs writes back the values that live only in the DOM.
//
// It exists because the picker repaints the form from an ASYNCHRONOUS event —
// an enumeration answer arriving — and a repaint rebuilds every input from
// state.projectForm. The name and path are read from the DOM at harvest and
// nowhere else, so without this a list arriving while someone was typing a
// name would erase what they had typed. Empty is treated as "leave it", the
// same guard harvestProjectForm already uses, so a field the browser has not
// rendered cannot blank a stored value.
function captureProjectFormInputs() {
    const f = state.projectForm;
    if (!f) return;
    const val = id => {
        const el = document.getElementById(id);
        return el && typeof el.value === 'string' ? el.value : '';
    };
    f.name = val('projName') || f.name;
    if (!isRemoteForm(f)) f.path = val('projPath') || f.path;
}

function setProjModelsWildcard(checked) {
    const f = state.projectForm;
    if (!f) return;
    if (checked) {
        f.allowed_models = [PROJ_MCP_WILDCARD];
    } else {
        f.allowed_models = [];
    }
    render();
}

function isProjModelsWildcard(f) {
    return f.allowed_models.length === 1 && f.allowed_models[0] === PROJ_MCP_WILDCARD;
}

// ---- Form renderer ----

function renderProjectForm() {
    const f = state.projectForm;
    if (!f) return '<div class="empty-state">No form state.</div>';
    // Rebuilt every render: a scope choice reaches its handler as an index
    // into this, never as a value interpolated into an onclick attribute.
    // Mailbox names carry quotes, apostrophes and emoji.
    state._scopeBind = [];
    const isNew = !f.id;
    const isRemote = isRemoteForm(f);
    const noun = isRemote ? 'Access Profile' : 'Project';
    const title = (isNew ? 'New ' : 'Edit ') + noun;

    let html = '<h2>' + esc(title) + '</h2>';
    if (state.projectFormError) {
        html += '<div class="proj-error">' + esc(state.projectFormError) + '</div>';
    }
    // The same gap the list names, named again here — this is the editor the
    // operator would have to open to fix it, so it is the one place the
    // sentence has to appear.
    const formGaps = projScopeGaps({
        kind: f.kind,
        allowed_mcp_ids: f.allowed_mcp_ids,
        context: harvestProjectPermissions(f).context,
    });
    if (formGaps.length) {
        html += '<div class="proj-scope-gap">';
        html += '<strong>A scope value is missing.</strong> Every tool the field governs is denied at call time until it is set — the client gets nothing, and nothing else says why.<ul>';
        for (const g of formGaps) {
            html += '<li><code>' + esc(g.mcp) + '</code>: ' + esc(g.fields.join(', ')) + '</li>';
        }
        html += '</ul></div>';
    }

    // ---- Kind ----
    // Chosen at create time only. The edit form shows it read-only: converting
    // an existing project is possible server-side but has real consequences
    // (see project_convert_test.go / project_apply.go), so it isn't offered
    // here as a casual dropdown.
    html += '<div class="proj-section">';
    html += '<div class="proj-section-title">Kind</div>';
    if (isNew) {
        html += '<div class="perm-btns">';
        html += '<button class="perm-btn ' + (!isRemote ? 'active' : '') + '" onclick="setProjKind(\'local\')">Local project</button>';
        html += '<button class="perm-btn ' + (isRemote ? 'active' : '') + '" onclick="setProjKind(\'remote\')">Access profile</button>';
        html += '</div>';
    } else {
        html += '<div class="proj-kind-label">' + (isRemote ? 'Access profile — a capability grant to a client on another machine' : 'Local project — bound to a host directory') + '</div>';
        html += '<p class="proj-section-help">Kind can\'t be changed here after creation.</p>';
    }
    html += '</div>';

    // ---- Identity ----
    html += '<div class="proj-section">';
    html += '<div class="proj-section-title">Identity</div>';
    html += '<label>' + (isRemote ? 'Profile name' : 'Project name') + '</label>';
    html += '<input type="text" id="projName" value="' + esc(f.name) + '" placeholder="' + (isRemote ? 'e.g. Hermes — Bob INBOX (read-only)' : 'e.g. Acme Website') + '" />';
    if (!isRemote) {
        html += '<label>Project path</label>';
        html += '<input type="text" id="projPath" value="' + esc(f.path) + '" placeholder="/Users/you/projects/acme" />';
        html += '<p class="proj-section-help">Absolute path. Filesystem MCPs are auto-scoped to this directory.</p>';
    } else {
        html += '<p class="proj-section-help">An access profile is a capability grant to an agent on another machine. It has no host directory, so path, directory auth, skills, shell templates and models do not apply — what it carries is which MCPs, which tools, which operations and which resources.</p>';
    }
    html += '</div>';

    // ---- Allowed MCPs + tri-state picker ----
    const wild = !isRemote && isProjMcpWildcard(f);
    html += '<div class="proj-section">';
    html += '<div class="proj-section-title">MCPs, Tools, Operations &amp; Resources</div>';
    if (!isRemote) {
        html += '<div class="toggle-row" style="padding:4px 0;margin:0">';
        html += '<span>Allow all registered MCPs (wildcard <code>*</code>)</span>';
        html += '<label class="switch"><input type="checkbox" ' + (wild ? 'checked' : '') + ' onchange="setProjMcpWildcard(this.checked)" /><span class="slider"></span></label>';
        html += '</div>';
    } else {
        html += '<p class="proj-section-help">An access profile can\'t use the wildcard — list MCPs explicitly, because registering a new MCP on the host would otherwise silently widen what this client reaches. Zero granted is a valid starting point; widen it deliberately later.</p>';
    }

    if (!wild) {
        const registered = state.externalMcps.slice();
        // Surface dangling refs (MCP IDs that no longer exist in registry).
        const registeredIds = new Set(registered.map(m => m.id));
        const dangling = f.allowed_mcp_ids.filter(id => id !== PROJ_MCP_WILDCARD && !registeredIds.has(id));
        if (registered.length === 0 && dangling.length === 0) {
            html += '<div class="proj-tool-empty">No MCPs registered yet. Add one in the <strong>MCP Servers</strong> tab.</div>';
        }
        for (const mcp of registered) {
            const st = projMcpState(f, mcp.id);
            const granted = f.allowed_mcp_ids.indexOf(mcp.id) >= 0;
            html += '<div class="proj-mcp-row">';
            html += '<span class="proj-mcp-name">' + esc(mcp.display_name || mcp.id) + ' <span style="color:var(--text-3);font-size:11px">(' + esc(mcp.id) + ')</span></span>';
            html += '<div class="perm-btns">';
            if (isRemote) {
                // Two states, not three. The third one is a denylist, and a
                // denylist grants every tool the MCP gains after the grant was
                // written — the fail-open shape a grant to another machine must
                // not have. What bounds a profile is the allowlist in the panel.
                html += '<button class="perm-btn ' + (granted ? 'active' : '') + '" onclick="setProjMcpGranted(\'' + esc(mcp.id) + '\', true)">Granted</button>';
                html += '<button class="perm-btn ' + (!granted ? 'active' : '') + '" onclick="setProjMcpGranted(\'' + esc(mcp.id) + '\', false)">Not granted</button>';
            } else {
                html += '<button class="perm-btn ' + (st === 'all' ? 'active' : '') + '" onclick="setProjMcpState(\'' + esc(mcp.id) + '\', \'all\')">All tools</button>';
                html += '<button class="perm-btn ' + (st === 'selected' ? 'active' : '') + '" onclick="setProjMcpState(\'' + esc(mcp.id) + '\', \'selected\')">Selected</button>';
                html += '<button class="perm-btn ' + (st === 'none' ? 'active' : '') + '" onclick="setProjMcpState(\'' + esc(mcp.id) + '\', \'none\')">No tools</button>';
            }
            html += '</div>';
            html += '</div>';
            if (!isRemote && st === 'selected') {
                html += renderProjToolPicker(mcp.id, f);
            }
            // Mode, tool allowlist and resource scope for every MCP this
            // record actually grants — the three layers relay checks beneath
            // the MCP itself.
            if (granted) {
                html += renderProjMcpPermissions(mcp.id, f);
            }
        }
        for (const id of dangling) {
            html += '<div class="proj-mcp-row">';
            html += '<span class="proj-mcp-name dangling">' + esc(id) + ' (no longer registered)</span>';
            html += '<button class="perm-btn" onclick="setProjMcpState(\'' + esc(id) + '\', \'none\')">Remove</button>';
            html += '</div>';
        }
    } else {
        // A wildcard grant still reaches every registered MCP, and a scope
        // requirement is not waived by how the grant was spelled — ADR-011
        // decision 4 applies to local projects too, which is why the live
        // wildcard "Relay" project loses macMCP's mail tools until someone
        // sets a value. The panel has to be reachable here or the editor
        // would name a problem it offers no way to fix.
        for (const mcp of state.externalMcps) {
            html += '<div class="proj-mcp-row">';
            html += '<span class="proj-mcp-name">' + esc(mcp.display_name || mcp.id) + ' <span style="color:var(--text-3);font-size:11px">(' + esc(mcp.id) + ')</span></span>';
            html += '<span class="proj-mcp-name" style="color:var(--text-3)">granted by the wildcard</span>';
            html += '</div>';
            html += renderProjMcpPermissions(mcp.id, f);
        }
    }
    html += '</div>';

    // ---- Allowed models ----
    html += '<div class="proj-section">';
    html += '<div class="proj-section-title">Allowed Models</div>';
    if (isRemote) {
        html += '<p class="proj-section-help">Not applicable to an access profile — the model allowlist stays empty.</p>';
    } else {
        const modelsWild = isProjModelsWildcard(f);
        html += '<div class="toggle-row" style="padding:4px 0;margin:0">';
        html += '<span>Allow all models (wildcard <code>*</code>)</span>';
        html += '<label class="switch"><input type="checkbox" ' + (modelsWild ? 'checked' : '') + ' onchange="setProjModelsWildcard(this.checked)" /><span class="slider"></span></label>';
        html += '</div>';
        if (!modelsWild) {
            const csv = f.allowed_models.filter(m => m !== PROJ_MCP_WILDCARD).join(', ');
            html += '<label>Model IDs (comma-separated)</label>';
            html += '<input type="text" id="projModels" value="' + esc(csv) + '" placeholder="claude-opus, claude-sonnet, gpt-4" />';
        }
    }
    html += '</div>';

    // ---- Chat templates (read-only; relay stores them, Eve edits them) ----
    // Absent for an access profile, because the model now REFUSES one on a
    // remote record rather than storing an inert copy (validateProjectShape).
    // A record that still carries templates from an earlier life is the one
    // exception: it is shown, and told what saving will do, because hiding
    // stored data an operator is about to lose is worse than an odd heading.
    if (!isRemote || f.chat_templates.length > 0) {
    html += '<div class="proj-section">';
    html += '<div class="proj-section-title">Chat Templates</div>';
    html += '<p class="proj-section-help">' + (isRemote
        ? 'An access profile has no chat sessions, so relay refuses chat templates on one. These are stored from before that rule; <strong>saving this profile removes them.</strong>'
        : 'Project-scoped chat presets stored with the project. Create and edit them in Eve\'s project dialog, which offers live model selection.') + '</p>';
    if (f.chat_templates.length === 0) {
        html += '<div class="proj-tool-empty">No templates yet.</div>';
    }
    for (const t of f.chat_templates) {
        html += '<div class="proj-template-card">';
        html += '<div class="proj-template-header">';
        html += '<span>' + esc(t.name || '(unnamed)') + '</span>';
        html += '<span class="desc">' + esc(t.model || '') + (t.mode === 'voice' ? ' · voice' : '') + '</span>';
        html += '</div>';
        html += '</div>';
    }
    html += '</div>';
    }

    // ---- Permission policy ----
    // Absent for an access profile, for the same reason the skill toggle and
    // directory auth are: the model refuses it now, so a control here would be
    // one whose only outcome is a refusal on Save. A profile that still
    // carries a policy is told, because saving clears it.
    const pol = f.permission_policy;
    if (isRemote) {
        if (!isPolicyEmpty(pol)) {
            html += '<div class="proj-section">';
            html += '<div class="proj-section-title">Permission Policy</div>';
            html += '<p class="proj-section-help">Claude CLI permission gates, stored from before relay refused them on an access profile. A profile launches no Claude session, so they gate nothing; what bounds a remote client is the operations, tools and resources above. <strong>Saving this profile removes them.</strong></p>';
            html += '</div>';
        }
    } else {
    html += '<div class="proj-section">';
    html += '<div class="proj-section-title">Permission Policy</div>';
    html += '<p class="proj-section-help">Claude CLI permission gates. Empty mode inherits Claude\'s default. Patterns follow Claude\'s tool grammar (e.g. <code>Bash(ls *)</code>).</p>';
    html += '<label>Default mode</label>';
    html += '<select id="projPolicyMode" onchange="state.projectForm.permission_policy.default_mode = this.value">';
    for (const m of ['', 'default', 'acceptEdits', 'plan', 'bypassPermissions']) {
        const sel = pol.default_mode === m ? 'selected' : '';
        html += '<option value="' + esc(m) + '" ' + sel + '>' + (m || '(inherit)') + '</option>';
    }
    html += '</select>';
    html += '<label>Allowed tools (one per line)</label>';
    html += '<textarea id="projAllowedTools" rows="3" placeholder="Read&#10;Grep&#10;Bash(ls *)">' + esc(pol.allowed_tools.join('\n')) + '</textarea>';
    html += '<label>Denied tools (one per line)</label>';
    html += '<textarea id="projDeniedTools" rows="3" placeholder="Bash(rm *)&#10;Write">' + esc(pol.denied_tools.join('\n')) + '</textarea>';
    html += '</div>';
    }

    // ---- Skill ----
    // Skills are written under <path>/.claude/skills — no path, no skill, so
    // the whole section is absent (not disabled) for a remote project rather
    // than showing a toggle that would lie about what it does.
    if (!isRemote) {
        html += '<div class="proj-section">';
        html += '<div class="proj-section-title">Skill (CLAUDE.md / SKILL.md)</div>';
        html += '<p class="proj-section-help">When enabled, relay regenerates <code>&lt;path&gt;/.claude/skills/relay/SKILL.md</code> on project save and MCP changes so Claude Code can discover this project\'s tools.</p>';
        html += '<div class="toggle-row" style="padding:4px 0;margin:0">';
        html += '<span>Auto-generate SKILL.md</span>';
        html += '<label class="switch"><input type="checkbox" ' + (f.generate_skill ? 'checked' : '') + ' onchange="state.projectForm.generate_skill = this.checked" /><span class="slider"></span></label>';
        html += '</div>';
        if (!isNew) {
            html += '<div style="margin-top:8px"><button class="btn btn-sm" onclick="regenProjectSkill(\'' + esc(f.id) + '\')">Regenerate now</button></div>';
            const regen = state.projectSkillRegen[f.id];
            if (regen) {
                const cls = regen.ok ? 'proj-ok' : 'proj-error';
                html += '<div class="' + cls + '">' + (regen.ok ? '✓ Regenerated: ' : '✗ Regen failed: ') + esc(regen.message) + '</div>';
            }
        }
        html += '</div>';
    }

    // ---- Directory auth ----
    // Compares a caller's cwd against Path; a remote project has no Path, so
    // the toggle is absent rather than disabled (see validateProjectShape).
    if (!isRemote) {
        html += '<div class="proj-section">';
        html += '<div class="proj-section-title">Directory Auth</div>';
        html += '<p class="proj-section-help">Lets <code>relay mcp</code> / <code>relay mcp call</code> run with no token when the working directory is inside this project\'s path, granting exactly this project\'s tools. <strong>Any process running as you</strong> gets them by being in the directory — including agents you started for something else. Leave off unless you want that trade.</p>';
        html += '<div class="toggle-row" style="padding:4px 0;margin:0">';
        html += '<span>Allow token-less access from this project\'s directory</span>';
        html += '<label class="switch"><input type="checkbox" ' + (f.allow_cwd_auth ? 'checked' : '') + ' onchange="state.projectForm.allow_cwd_auth = this.checked" /><span class="slider"></span></label>';
        html += '</div>';
        html += '</div>';
    }

    // ---- Token (edit only) ----
    if (!isNew) {
        const visible = !!state.projectTokenVisible[f.id];
        const fresh = state.projectFreshToken[f.id];
        const display = visible ? f.token : (f.token ? '•'.repeat(Math.min(40, f.token.length)) : '');
        html += '<div class="proj-section">';
        html += '<div class="proj-section-title">Bearer Token</div>';
        html += '<p class="proj-section-help">Project-scoped token presented by Eve, relayLLM, and <code>relay mcp --token</code>. Tokens are inline; rotating invalidates the prior token immediately.</p>';
        html += '<div class="proj-token-field">';
        html += '<input type="text" readonly value="' + esc(display) + '" />';
        html += '<button class="btn btn-sm" onclick="toggleProjectTokenVisible(\'' + esc(f.id) + '\')">' + (visible ? 'Hide' : 'Show') + '</button>';
        html += '<button class="btn btn-sm" onclick="copyProjectToken(\'' + esc(f.token) + '\')">Copy</button>';
        html += '<button class="btn btn-sm btn-danger" onclick="rotateProjectToken(\'' + esc(f.id) + '\', \'' + esc(f.name) + '\')">Rotate</button>';
        html += '</div>';
        if (fresh) {
            html += '<div class="proj-token-banner">';
            html += 'New token issued — copy now, this is the only banner that will show it: <code>' + esc(fresh) + '</code>';
            html += '</div>';
        }
        html += '</div>';
    }

    // ---- Actions ----
    html += '<div class="proj-form-actions">';
    html += '<button class="btn btn-primary" onclick="saveProjectForm()">' + (isNew ? 'Create' : 'Save') + '</button>';
    html += '<button class="btn btn-danger" onclick="cancelProjectEdit()">Cancel</button>';
    html += '</div>';

    return html;
}

function renderProjToolPicker(mcpID, f) {
    const live = state.mcpToolCache[mcpID];
    let html = '<div class="proj-tool-picker">';
    if (!live) {
        html += '<div class="proj-tool-empty"><span class="spinner"></span>Loading tools…</div>';
        html += '</div>';
        return html;
    }
    if (live.length === 0) {
        html += '<div class="proj-tool-empty">No tools discovered. If this is an HTTP MCP, authenticate it in the <strong>MCP Servers</strong> tab first.</div>';
        html += '</div>';
        return html;
    }
    const disabledSet = new Set(f.disabled_tools[mcpID] || []);
    for (const t of live) {
        const checked = !disabledSet.has(t.name);
        html += '<label class="proj-tool-row">';
        html += '<input type="checkbox" ' + (checked ? 'checked' : '') + ' onchange="toggleProjTool(\'' + esc(mcpID) + '\', \'' + esc(t.name) + '\', this.checked)" />';
        html += '<div><div>' + esc(t.name) + '</div>';
        if (t.description) html += '<div class="desc">' + esc(oneLineProj(t.description)) + '</div>';
        html += '</div></label>';
    }
    // Stale entries — names in disabled_tools that aren't in the live list.
    const liveNames = new Set(live.map(t => t.name));
    for (const name of (f.disabled_tools[mcpID] || [])) {
        if (liveNames.has(name)) continue;
        html += '<label class="proj-tool-row stale" title="No longer present in the MCP\'s tool list">';
        html += '<input type="checkbox" checked onchange="pruneStaleDisabledTool(\'' + esc(mcpID) + '\', \'' + esc(name) + '\', this.checked)" />';
        html += '<div><div>' + esc(name) + '</div><div class="desc">stale — uncheck to remove</div></div>';
        html += '</label>';
    }
    html += '</div>';
    return html;
}


function pruneStaleDisabledTool(mcpID, name, kept) {
    if (kept) return; // user wants to keep it; no-op
    const f = state.projectForm;
    if (!f) return;
    f.disabled_tools[mcpID] = (f.disabled_tools[mcpID] || []).filter(n => n !== name);
    render();
}

// isPolicyEmpty mirrors permissionPolicyIsEmpty on the Go side. Both surfaces
// have to agree that an emptied policy is not a policy, or the editor would
// send something the model refuses on a record it just cleared.
function isPolicyEmpty(pol) {
    if (!pol) return true;
    return !pol.default_mode && (pol.allowed_tools || []).length === 0 && (pol.denied_tools || []).length === 0;
}

function harvestProjectForm() {
    const f = state.projectForm;
    if (!f) return null;
    const isRemote = isRemoteForm(f);
    const name = (document.getElementById('projName') || {}).value || f.name;
    // A remote project has no path control in the form (see renderProjectForm)
    // and must not send one — validateProjectShape rejects any non-empty path
    // on a remote project.
    let allowedModels = f.allowed_models;
    if (isRemote) {
        allowedModels = [];
    } else if (!isProjModelsWildcard(f)) {
        const csv = (document.getElementById('projModels') || {}).value || '';
        allowedModels = csv.split(',').map(s => s.trim()).filter(Boolean);
    }
    // An access profile sends an EMPTY policy rather than the one it may still
    // carry: relay refuses a policy on a remote record, and this is the form
    // that has to be able to clear one. An empty policy is read as "clear it"
    // on both sides (permissionPolicyIsEmpty / isPolicyEmpty).
    let policy = { default_mode: '', allowed_tools: [], denied_tools: [] };
    if (!isRemote) {
        const allowedToolsTA = document.getElementById('projAllowedTools');
        const deniedToolsTA = document.getElementById('projDeniedTools');
        policy = {
            default_mode: f.permission_policy.default_mode,
            allowed_tools: allowedToolsTA ? allowedToolsTA.value.split('\n').map(s => s.trim()).filter(Boolean) : f.permission_policy.allowed_tools,
            denied_tools: deniedToolsTA ? deniedToolsTA.value.split('\n').map(s => s.trim()).filter(Boolean) : f.permission_policy.denied_tools,
        };
    }
    // chat_templates is intentionally absent: the form is read-only for
    // templates (Eve owns editing), and omitting the field makes
    // update_project leave the stored list untouched. The one exception is a
    // profile that still carries some — relay refuses those now, so the field
    // has to be SENT as empty or the record could never be saved again.
    const payload = {
        name: name.trim(),
        kind: isRemote ? 'remote' : 'local',
        allowed_mcp_ids: f.allowed_mcp_ids,
        allowed_models: allowedModels,
        permission_policy: policy,
        // Both are directory-flavored and meaningless without a path;
        // force them off for remote regardless of stale form state.
        generate_skill: isRemote ? false : f.generate_skill,
        allow_cwd_auth: isRemote ? false : f.allow_cwd_auth,
        disabled_tools: f.disabled_tools,
    };
    if (isRemote && f.chat_templates.length > 0) payload.chat_templates = [];
    // The ADR-011 permission set. Sent on every save, including when it is
    // empty: these are pointer fields on the update DTO, so omitting one means
    // "leave it alone" — which would make clearing a scope value from this
    // editor impossible.
    //
    // Unless the three maps could not be BUILT, which is a different thing
    // from being empty. Under a wildcard grant their keys come from relay's
    // MCP list, of which this side holds only a copy; if that copy is missing
    // the harvest produces {} for all three — indistinguishable, on the wire,
    // from an operator clearing every mode, every tool allowlist and every
    // scope value. Omitting them lets the update path's nil-means-no-change
    // convention do the right thing, which is nothing.
    const perms = harvestProjectPermissions(f);
    if (perms.complete) {
        payload.access = perms.access;
        payload.allowed_tools = perms.allowed_tools;
        payload.context = perms.context;
    }
    if (!isRemote) {
        const path = (document.getElementById('projPath') || {}).value || f.path;
        payload.path = path.trim();
    }
    return payload;
}

// harvestProjectPermissions turns the panel's typed text into the three maps
// relay stores. Every value is validated on the server, whichever surface
// produced it — this side is a convenience, never the check.
//
// Two things it deliberately does NOT do. It does not drop a context key it
// cannot render: a field the MCP no longer declares, or one belonging to an
// MCP relay has not connected to, is passed through untouched, because opening
// the editor must not silently delete a value nobody looked at. And it does
// not send a source: "project_path" field — relay derives those, refuses an
// operator-supplied one, and would overwrite it on the next resync anyway.
function harvestProjectPermissions(f) {
    const remote = isRemoteForm(f);
    const wild = f.allowed_mcp_ids.length === 1 && f.allowed_mcp_ids[0] === PROJ_MCP_WILDCARD;
    const registered = (state.externalMcps || []).map(m => m.id);
    // A wildcard grant's MCP set is not in the form. It is whatever relay
    // currently knows, and this side holds a COPY of that — one that is empty
    // before the first payload arrives, and empty again if one arrives
    // malformed. Expanding it then yields no keys at all, and the three maps
    // built from them come out {} whatever the record actually holds.
    //
    // `complete` is what the caller keys on. It is not a validity flag: a
    // non-wildcard grant naming no MCPs is complete and legitimately produces
    // three empty maps, because the operator said so in the form. What is
    // incomplete is a wildcard with nothing to expand it against.
    const complete = !wild || registered.length > 0;
    const granted = wild ? registered : f.allowed_mcp_ids.slice();

    const access = {};
    const allowedTools = {};
    const context = {};

    for (const mcpID of granted) {
        const mode = (f.access || {})[mcpID];
        if (mode === 'read' || mode === 'write') access[mcpID] = mode;

        if (remote) {
            const text = projAllowedToolsText(f, mcpID);
            const patterns = String(text).split('\n').map(t => t.trim()).filter(Boolean);
            if (patterns.length) allowedTools[mcpID] = patterns;
        } else if (((f.allowed_tools || {})[mcpID] || []).length) {
            // A local project's tool narrowing is the picker above
            // (disabled_tools); an allowlist here can only have arrived from
            // eve or the API, so it is carried through rather than erased.
            allowedTools[mcpID] = (f.allowed_tools[mcpID] || []).slice();
        }

        const existing = Object.assign({}, (f.context || {})[mcpID] || {});
        const fields = mcpScopeFieldsFor(mcpID);
        for (const field of (fields || [])) {
            if (field.source === 'project_path') { delete existing[field.name]; continue; }
            const value = scopeValueFromText(field, projScopeText(f, mcpID, field));
            if (scopeValueIsSet(value)) existing[field.name] = value;
            else delete existing[field.name];
        }
        if (Object.keys(existing).length) context[mcpID] = existing;
    }
    return { access: access, allowed_tools: allowedTools, context: context, complete: complete };
}

function saveProjectForm() {
    const f = state.projectForm;
    if (!f) return;
    const payload = harvestProjectForm();
    if (!payload) return;
    if (!payload.name) {
        state.projectFormError = 'Project name is required';
        render();
        return;
    }
    if (payload.kind !== 'remote' && !payload.path) {
        state.projectFormError = 'Project path is required';
        render();
        return;
    }
    state.projectFormError = null;

    if (!f.id) {
        ipc(JSON.stringify(Object.assign({ type: 'create_project' }, payload)));
    } else {
        ipc(JSON.stringify(Object.assign({ type: 'update_project', id: f.id }, payload)));
    }
}

// ---- Project IPC event handlers ----

window.onProjectAdded = function(p) {
    if (!p || !p.id) return;
    // Replace any provisional entry with the real persisted row.
    state.projects = state.projects.filter(x => x.id !== p.id).concat(p);
    state.editingProjectId = null;
    state.projectForm = null;
    state.projectError = null;
    if (state.page === 'projects') render('push');
};

window.onProjectUpdated = function(p) {
    if (!p || !p.id) return;
    state.projects = state.projects.map(x => x.id === p.id ? p : x);
    // Close the edit form on successful save so we return to the list, matching
    // onProjectAdded and the Save flows in Services / Service Inspector.
    if (state.editingProjectId === p.id) {
        state.editingProjectId = null;
        state.projectForm = null;
    }
    state.projectError = null;
    if (state.page === 'projects') render('push');
};

window.onProjectRemoved = function(id) {
    state.projects = state.projects.filter(x => x.id !== id);
    delete state.projectTokenVisible[id];
    delete state.projectFreshToken[id];
    delete state.projectSkillRegen[id];
    if (state.editingProjectId === id) {
        state.editingProjectId = null;
        state.projectForm = null;
    }
    if (state.page === 'projects') render('push');
};

window.onProjectTokenRotated = function(id, plaintext) {
    state.rotatingProjectId = null;
    state.projectFreshToken[id] = plaintext;
    // Update the project's inline token in our local copy so subsequent edits
    // reflect the new value (the backend stores it inline too).
    const p = state.projects.find(x => x.id === id);
    if (p) p.token = plaintext;
    if (state.projectForm && state.projectForm.id === id) {
        state.projectForm.token = plaintext;
        state.projectTokenVisible[id] = true; // reveal so the banner code is meaningful
    }
    if (state.page === 'projects') render('push');
};

window.onProjectSkillRegen = function(id, ok, message) {
    state.projectSkillRegen[id] = { ok: !!ok, message: message || '', t: Date.now() };
    if (state.page === 'projects') render('push');
};

window.onMcpToolsListed = function(mcpID, tools) {
    state.mcpToolCache[mcpID] = tools || [];
    if (state.page === 'projects' && state.editingProjectId) {
        render('push');
    }
};

// onScopeFieldEnumerated receives one ContextEnumResult, verbatim, including
// its status — which is the whole point. "There are none" is `status: "ok"`
// with an empty list; "nobody could look" is any other status with no list at
// all, and the two must never render the same way.
window.onScopeFieldEnumerated = function(res) {
    if (!res || !res.mcp_id || !res.field) return;
    const openKey = scopeOpenKey(res.mcp_id, res.field);
    const key = state.scopeEnumReq[openKey];
    // No request outstanding for this field: a late answer to a question the
    // dependencies have since changed. Dropping it is right — the key it would
    // be filed under is not the key anything is looking up.
    if (!key) return;
    delete state.scopeEnumReq[openKey];
    state.scopeEnum[key] = res;
    // -32601 is final and it is about the MCP, not about this field: it never
    // implements enumeration, so every field of it degrades to text entry and
    // nothing asks again.
    if (res.status === 'unsupported') state.scopeEnumUnsupported[res.mcp_id] = true;
    if (state.page !== 'projects' || !state.editingProjectId) return;
    // A full repaint, not render('push'): this answer is the direct result of
    // the operator opening a control and it must appear. captureProjectFormInputs
    // is what makes that safe — the repaint would otherwise rebuild the name
    // and path inputs from state and erase anything typed while waiting.
    captureProjectFormInputs();
    render();
};

window.onProjectError = function(msg) {
    state.projectError = msg;
    state.projectFormError = msg;
    if (state.page === 'projects') render('push');
};

// ---------------------------------------------------------------------------
// Remote Clients tab — the enrolments, then the listener they arrive on.
//
// ADR-010 decision 8 ends "enrolments are listed in the Settings UI beside the
// grants they reach — a credential you cannot see is one you will not revoke",
// and that sentence is this tab's whole specification. Two consequences shape
// everything below:
//
//   * Grants render as access-profile NAMES, with the profile's EFFECTIVE
//     AUTHORITY beside each one — MCPs, mode, tools, scope (ADR-011 decision
//     1). Two records is the model, and the cost of two records is paid here:
//     "what can this client do" has to be answerable without mentally joining
//     an enrolment to a profile in another tab. A grant naming a profile that
//     no longer exists still renders — as the raw id, marked — because hiding
//     it would hide the fact that the enrolment is holding something relay
//     cannot resolve.
//   * The certificate fingerprint renders IN FULL. After an enrolment is
//     deleted the fingerprint is the only thing that identifies that client's
//     calls in the audit log, which is why the Tool Calls tab prints it
//     untruncated too (see renderAuditDetail). A UI that shortened it would be
//     the obvious place for someone to copy a short form from.
//
// The listener section below the list is the other half: an enrolment reaches
// nothing if no listener is running, and the reasons a listener is not running
// are surprising enough (an absent block, an omitted `enabled`, auditing
// switched off) that each is stated rather than left to be inferred from an
// empty log.
// ---------------------------------------------------------------------------

// The listener now reconciles: RemoteSupervisor converges on the configured
// state from the settings poll, so enabling, disabling and moving the address
// all take effect without a relaunch. One thing still does not — turning
// auditing back ON — because AuditRecorder is built once at startup and nothing
// reconciles it. Auditing OFF does take effect, and stops the listener, so the
// asymmetry only bites in the recovering direction. Say that rather than a
// blanket "restart required", which would be false for everything a user is
// actually likely to change here.
const REMOTE_NOTE = 'Enabling, disabling and moving the listener take effect within a few seconds — no restart needed. Re-enabling auditing after turning it off is the exception: that still needs Relay to relaunch.';

// remoteGrantableProjects is the only set the create form offers: the access
// profiles. ValidateEnrolmentGrants refuses a grant naming a local project
// outright, so presenting one would be offering a choice relay is about to
// reject. This is
// a courtesy, not the enforcement — the server validates regardless, inside
// the same store.With that claims the client id.
function remoteGrantableProjects() {
    return (state.projects || []).filter(isRemoteProject);
}

// enrolGrantNames resolves an enrolment's grant ids to {id, name} pairs. name
// is null when no project carries that id — a dangling grant, which the card
// shows rather than silently drops.
function enrolGrantNames(e) {
    return ((e && e.project_ids) || []).map(function(id) {
        const p = (state.projects || []).find(x => x.id === id);
        return { id: id, name: p ? p.name : null, profile: p || null };
    });
}

// enrolGrantSummary is the plain-text form used in the revoke confirmation.
// The confirmation names what is being cut; "are you sure?" over an opaque id
// is not a decision anyone can make.
function enrolGrantSummary(e) {
    const names = enrolGrantNames(e).map(g => g.name || (g.id + ' (unknown access profile)'));
    if (!names.length) return 'no access profiles — this enrolment grants nothing today';
    return names.join(', ');
}

// enrolBytes renders a byte budget. Only exact multiples get a friendly unit,
// so a number the operator typed always reads back as the number they typed
// rather than as a rounded approximation of it.
function enrolBytes(n) {
    n = Number(n) || 0;
    if (n >= 1048576 && n % 1048576 === 0) return (n / 1048576) + ' MiB';
    if (n >= 1024 && n % 1024 === 0) return (n / 1024) + ' KiB';
    return n + ' bytes';
}

function enrolBudgetText(b) {
    b = b || {};
    return (b.max_calls || 0) + ' calls / ' + enrolBytes(b.max_result_bytes) + ' per ' + (b.window_seconds || 0) + 's';
}

function renderEnrolments() {
    if (state.enrolForm) return renderEnrolmentForm();

    let html = '<div class="page-header"><h2>Remote Clients</h2>';
    html += '<button class="btn btn-primary" onclick="newEnrolment()">+ New Enrolment</button></div>';
    html += '<p class="page-intro">An enrolment binds one client certificate to the access profiles it may use. The certificate <em>is</em> the identity — there is no bearer token on this path, so a copy of <code>settings.json</code> grants no remote access at all. Enrolments are keyed by certificate, not by machine: several agents on one VM each hold their own, granted and revoked independently.</p>';

    if (state.enrolBundle) html += renderEnrolBundleBanner(state.enrolBundle);
    if (state.enrolRevoked) {
        html += '<div class="audit-note">Revoked <strong>' + esc(state.enrolRevoked.client_id) + '</strong>. Its calls remain in the Tool Calls log under fingerprint <code>' + esc(state.enrolRevoked.fingerprint) + '</code> — now the only thing that names them.</div>';
    }
    if (state.enrolmentError) html += '<div class="proj-error">' + esc(state.enrolmentError) + '</div>';

    const list = state.enrolments || [];
    if (!list.length) {
        html += '<div class="empty-state">No enrolled clients. Click <strong>+ New Enrolment</strong>, or run <code>relay enrol create</code>.</div>';
    }
    for (const e of list) {
        html += '<div class="enrol-card">';
        html += '<div class="enrol-card-header">';
        html += '<span class="enrol-card-name">' + esc(e.client_id) + '</span>';
        html += '<button class="btn btn-sm btn-danger" onclick="revokeEnrolment(\'' + esc(e.client_id) + '\')">Revoke</button>';
        html += '</div>';

        // Grants, by name. A card that showed ids would make the revoke
        // decision unanswerable without a second tab open beside it.
        const grants = enrolGrantNames(e);
        if (!grants.length) {
            html += '<div class="enrol-grants"><span class="enrol-grant none">no access profiles granted</span></div>';
        }
        for (const g of grants) {
            if (!g.name) {
                html += '<div class="enrol-grants"><span class="enrol-grant dangling" title="No access profile carries this id">' + esc(g.id) + ' — unknown access profile</span></div>';
                continue;
            }
            // The profile's name, and then what it actually permits. A card
            // that stopped at the name answers "which grant" and leaves "what
            // can this client do" to a second tab.
            html += '<div class="enrol-profile">';
            html += '<div class="enrol-grants"><span class="enrol-grant" title="' + esc(g.id) + '">' + esc(g.name) + '</span></div>';
            html += '<div class="proj-authority">' + renderAuthorityRows(g.profile) + '</div>';
            html += '</div>';
        }

        html += '<div class="enrol-meta">';
        html += '<span>Budget: <strong>' + esc(enrolBudgetText(e.budget)) + '</strong></span>';
        html += '<span>Enrolled: <strong>' + esc(e.created_at || '—') + '</strong></span>';
        html += '</div>';
        // Full fingerprint, never truncated — see the file header.
        html += '<div class="enrol-fp"><span class="enrol-fp-label">Certificate: </span>' + esc(e.fingerprint || '(none)') + '</div>';
        html += '</div>';
    }

    html += renderRemoteListener();
    return html;
}

// renderEnrolBundleBanner names the emitted directory and tells the operator to
// MOVE it. The private key is inside that directory and is never rendered,
// previewed, or fetched over IPC — the settings WebView is a rendering surface,
// and key material that reaches it has been copied somewhere nobody will think
// to wipe. The filenames below are static copy, matching `relay enrol create`.
function renderEnrolBundleBanner(b) {
    let html = '<div class="enrol-bundle">';
    html += 'Enrolled <strong>' + esc(b.client_id) + '</strong>. Bundle written to <code>' + esc(b.dir) + '</code>';
    html += '<ul>';
    html += '<li><code>client.key</code> — client private key (0600)</li>';
    html += '<li><code>client.crt</code> — client certificate</li>';
    html += '<li><code>ca.crt</code> — relay\'s CA certificate, for verifying the server</li>';
    html += '</ul>';
    html += '<strong>Move (don\'t copy) this directory to the client machine.</strong> The private key travels exactly once; every copy left behind is a credential nobody is tracking.';
    html += '<div style="margin-top:8px"><button class="btn btn-sm" onclick="dismissEnrolBundle()">Done</button></div>';
    html += '</div>';
    return html;
}

function renderEnrolmentForm() {
    const f = state.enrolForm;
    const d = state.enrolmentBudgetDefaults || {};
    let html = '<h2>New Enrolment</h2>';
    if (state.enrolmentError) html += '<div class="proj-error">' + esc(state.enrolmentError) + '</div>';

    html += '<div class="proj-section">';
    html += '<div class="proj-section-title">Identity</div>';
    html += '<p class="proj-section-help">The client id is the certificate\'s Common Name and the bundle\'s directory name, so it is limited to letters, digits, <code>.</code>, <code>_</code> and <code>-</code>. It must be unique: to re-issue a certificate, revoke the existing enrolment first.</p>';
    html += '<label>Client id</label>';
    html += '<input type="text" id="enrolClientId" value="' + esc(f.client_id) + '" placeholder="hermes-mail" />';
    html += '</div>';

    // ---- Grants ----
    html += '<div class="proj-section">';
    html += '<div class="proj-section-title">Granted Access Profiles</div>';
    html += '<p class="proj-section-help">Only <strong>access profiles</strong> can be granted. A remote client granted a local project would inherit that project\'s host-directory scope, so the grant is refused outright — this list offers nothing that would be refused.</p>';
    const grantable = remoteGrantableProjects();
    if (!grantable.length) {
        html += '<div class="proj-tool-empty">No access profiles exist yet. Create one in the <strong>Projects</strong> tab (Kind → Access profile) first.</div>';
    }
    for (const p of grantable) {
        const checked = f.project_ids.indexOf(p.id) >= 0;
        html += '<label class="proj-tool-row">';
        html += '<input type="checkbox" ' + (checked ? 'checked' : '') + ' onchange="toggleEnrolGrant(\'' + esc(p.id) + '\', this.checked)" />';
        html += '<div><div>' + esc(p.name) + '</div><div class="desc">' + esc(p.id) + '</div></div>';
        html += '</label>';
    }
    if (grantable.length && f.project_ids.length === 0) {
        // Same note `relay enrol create` prints: enrolling with nothing is
        // legal and is the expected "enrol now, widen deliberately later"
        // resting state. Say so rather than emitting a certificate that
        // silently reaches nothing.
        html += '<p class="proj-section-help">No grant selected: this client will be enrolled but can reach no access profile until one is added.</p>';
    }
    html += '</div>';

    // ---- Budget ----
    html += '<div class="proj-section">';
    html += '<div class="proj-section-title">Budget</div>';
    html += '<p class="proj-section-help">The enrolment is the unit of compromise, so it is the unit that carries the cap. Rate and volume are capped together because they fail differently — a call limit alone does not stop a slow drain. Leave a field blank for the conservative default; <strong>zero is never unlimited</strong>, there is no way to switch a budget off.</p>';
    html += '<label>Window (seconds)</label>';
    html += '<input type="number" id="enrolWindow" value="' + esc(f.window_seconds) + '" placeholder="' + esc(d.window_seconds || '') + '" />';
    html += '<label>Max tool calls per window</label>';
    html += '<input type="number" id="enrolMaxCalls" value="' + esc(f.max_calls) + '" placeholder="' + esc(d.max_calls || '') + '" />';
    html += '<label>Max cumulative result bytes per window</label>';
    html += '<input type="number" id="enrolMaxBytes" value="' + esc(f.max_result_bytes) + '" placeholder="' + esc(d.max_result_bytes || '') + '" />';
    html += '</div>';

    html += '<div class="proj-form-actions">';
    html += '<button class="btn btn-primary" onclick="saveEnrolment()">Create &amp; issue certificate</button>';
    html += '<button class="btn btn-danger" onclick="cancelEnrolment()">Cancel</button>';
    html += '</div>';
    return html;
}

function newEnrolment() {
    state.enrolForm = { client_id: '', project_ids: [], window_seconds: '', max_calls: '', max_result_bytes: '' };
    state.enrolmentError = null;
    state.enrolBundle = null;
    state.enrolRevoked = null;
    render();
}

function cancelEnrolment() {
    state.enrolForm = null;
    state.enrolmentError = null;
    render();
}

function toggleEnrolGrant(projectID, checked) {
    const f = state.enrolForm;
    if (!f) return;
    const i = f.project_ids.indexOf(projectID);
    if (checked && i < 0) f.project_ids.push(projectID);
    if (!checked && i >= 0) f.project_ids.splice(i, 1);
    render();
}

function saveEnrolment() {
    const f = state.enrolForm;
    if (!f) return;
    const clientID = (((document.getElementById('enrolClientId') || {}).value) || '').trim();
    if (!clientID) {
        state.enrolmentError = 'client id is required';
        render();
        return;
    }
    f.client_id = clientID;
    // A blank or unparseable field sends 0, which normalizeEnrolmentBudget
    // reads as "unset" and fills with the conservative default. Zero never
    // means unlimited anywhere on this path.
    const num = function(id) {
        const raw = (((document.getElementById(id) || {}).value) || '').trim();
        const n = parseInt(raw, 10);
        return (raw === '' || isNaN(n) || n < 0) ? 0 : n;
    };
    state.enrolmentError = null;
    ipc(JSON.stringify({
        type: 'create_enrolment',
        client_id: clientID,
        project_ids: f.project_ids,
        budget: {
            window_seconds: num('enrolWindow'),
            max_calls: num('enrolMaxCalls'),
            max_result_bytes: num('enrolMaxBytes'),
        },
    }));
}

// revokeEnrolment names what is being cut before it cuts it. The wording
// matches `relay enrol revoke`: the certificate is unchanged and no project is
// touched — the record is what granted access, and deleting it also severs the
// client's live connections rather than waiting for it to reconnect.
function revokeEnrolment(clientID) {
    const e = (state.enrolments || []).find(x => x.client_id === clientID);
    if (!e) return;
    const msg = 'Revoke enrolment "' + clientID + '"?\n\n'
        + 'This cuts its access to: ' + enrolGrantSummary(e) + '\n\n'
        + 'Live connections holding its certificate are closed immediately. The certificate itself is unchanged and no project is touched — the record is what granted it access.';
    if (!confirm(msg)) return;
    ipc(JSON.stringify({ type: 'revoke_enrolment', client_id: clientID }));
}

function dismissEnrolBundle() {
    state.enrolBundle = null;
    render();
}

// ---- The listener ----

// remoteDraft is the uncommitted edit of the `remote` block, lazily seeded
// from the server's view. Kept out of state.remote so a push-sourced reload
// can't half-apply someone's typing.
function remoteDraft() {
    if (!state.remoteDraft) {
        const r = state.remote || {};
        state.remoteDraft = { enabled: !!r.enabled, listen: r.listen || '' };
    }
    return state.remoteDraft;
}

function remoteDraftSet(key, value) {
    const d = remoteDraft();
    d[key] = value;
    state.remoteDirty = true;
    // The address field re-renders nothing (a repaint on every keystroke would
    // fight the caret); the toggle does, because the consequence text below it
    // changes with it.
    if (key === 'enabled') render();
}

// remoteListenIsLoopback reports whether an address binds only this machine.
// The default binds loopback so that misconfiguration cannot expose the
// control plane to a LAN — widening it is a legitimate act, and this is what
// lets the UI say so out loud instead of refusing it.
function remoteListenIsLoopback(addr) {
    addr = String(addr || '');
    const i = addr.lastIndexOf(':');
    if (i < 0) return false;
    const host = addr.slice(0, i).replace(/^\[/, '').replace(/\]$/, '');
    return host === '127.0.0.1' || host === 'localhost' || host === '::1';
}

function renderRemoteListener() {
    const r = state.remote || { configured: false, enabled: false, listen: '', effective: '', audit_enabled: true };
    const d = remoteDraft();
    // Auditing is a hard dependency, not a preference: NewRemoteServer refuses
    // to start while it is off, so an enabled block in that state is
    // configured and dead. The badge reports the truth, not the setting.
    const live = r.enabled && r.audit_enabled;

    let html = '<div class="proj-section" style="margin-top:24px">';
    html += '<div class="proj-section-title">Remote Listener';
    if (!r.configured) {
        html += ' <span class="remote-state absent">No block</span>';
    } else if (live) {
        html += ' <span class="remote-state on">On</span>';
    } else if (r.enabled) {
        html += ' <span class="remote-state off">Off — auditing disabled</span>';
    } else {
        html += ' <span class="remote-state off">Off</span>';
    }
    html += '</div>';

    if (state.remoteError) html += '<div class="proj-error">' + esc(state.remoteError) + '</div>';

    // The three states are spelled out because two of them are surprising.
    if (!r.configured) {
        html += '<p class="proj-section-help">There is no <code>remote</code> block in <code>settings.json</code>, which means <strong>no listener is opened at all</strong> — not a listener bound to nothing, not one that refuses every call. This is the default, and it is not the same state as a listener that is switched off.</p>';
    } else if (!r.enabled) {
        html += '<p class="proj-section-help">The <code>remote</code> block exists but the listener is <strong>off</strong>. A block that omits <code>enabled</code> resolves to disabled — the opposite default to auditing, deliberately, so a network listener is never opened by omission.</p>';
    }

    if (r.enabled && !r.audit_enabled) {
        html += '<div class="remote-note danger"><strong>Remote access is off because the tool-call audit log is disabled.</strong> The listener refuses to start while auditing is off: a remote grant is justified by the calls it records, so serving remote traffic unrecorded is not a degraded mode. Re-enable <code>audit.enabled</code> to restore remote access — this block is configured but dead until then.</div>';
    }

    html += '<div class="toggle-row" style="padding:4px 0;margin:0">';
    html += '<span>Accept remote clients on an mTLS listener</span>';
    html += '<label class="switch"><input type="checkbox" ' + (d.enabled ? 'checked' : '') + ' onchange="remoteDraftSet(\'enabled\', this.checked)" /><span class="slider"></span></label>';
    html += '</div>';

    html += '<label>Listen address</label>';
    html += '<input type="text" id="remoteListen" value="' + esc(d.listen) + '" placeholder="' + esc(r.effective || '') + '" oninput="remoteDraftSet(\'listen\', this.value)" />';
    html += '<p class="proj-section-help">Leave blank for the default, <code>' + esc(r.effective || '') + '</code>. ' + esc(REMOTE_NOTE) + '</p>';

    const effective = (d.listen || '').trim() || r.effective || '';
    if (effective && !remoteListenIsLoopback(effective)) {
        html += '<div class="remote-note warn">' + esc(effective) + ' binds beyond loopback: every machine that can reach that address can attempt a TLS handshake. Only a certificate relay signed gets past it, and an unenrolled one is closed before a single request is read — but the default binds loopback precisely so that reaching relay from another machine is a deliberate act. A tunnel is a network path, never an identity: never forward the bridge socket in its place.</div>';
    }

    html += '<div class="proj-form-actions">';
    html += '<button class="btn btn-primary" onclick="saveRemoteConfig()">Save</button>';
    if (r.configured) {
        html += '<button class="btn btn-danger" onclick="removeRemoteConfig()">Remove block</button>';
    }
    html += '</div>';
    html += '</div>';
    return html;
}

function saveRemoteConfig() {
    const d = remoteDraft();
    state.remoteError = null;
    ipc(JSON.stringify({
        type: 'update_remote_config',
        enabled: !!d.enabled,
        listen: String(d.listen || '').trim(),
    }));
}

// removeRemoteConfig returns the install to "no block at all" — the one state
// an operator could otherwise never get back to once they had touched this
// form, and the one that means no socket is opened.
function removeRemoteConfig() {
    const msg = 'Remove the remote block from settings.json?\n\n'
        + 'No listener will be opened at all. Enrolments are not touched — they stay listed here and can still be revoked, but nothing can connect until a listener is configured again.';
    if (!confirm(msg)) return;
    state.remoteError = null;
    ipc(JSON.stringify({ type: 'update_remote_config', remove: true }));
}

// ---- Remote Clients IPC event handlers ----

window.onEnrolmentCreated = function(e, bundle) {
    if (!e || !e.client_id) return;
    state.enrolments = (state.enrolments || []).filter(x => x.client_id !== e.client_id).concat(e);
    state.enrolForm = null;
    state.enrolmentError = null;
    state.enrolRevoked = null;
    // Only the bundle DIRECTORY is ever held here. The private key inside it
    // does not cross the IPC boundary and has no representation in this state.
    state.enrolBundle = { client_id: e.client_id, dir: (bundle && bundle.dir) || '' };
    if (state.page === 'remote') render('push');
};

window.onEnrolmentRevoked = function(clientID, fingerprint) {
    state.enrolments = (state.enrolments || []).filter(x => x.client_id !== clientID);
    state.enrolmentError = null;
    if (state.enrolBundle && state.enrolBundle.client_id === clientID) state.enrolBundle = null;
    // The fingerprint outlives the record on purpose: it is what identifies
    // this client's past calls in the Tool Calls tab now that nothing else
    // names it.
    state.enrolRevoked = { client_id: clientID, fingerprint: fingerprint || '' };
    if (state.page === 'remote') render('push');
};

window.onEnrolmentError = function(msg) {
    state.enrolmentError = msg || 'enrolment failed';
    if (state.page === 'remote') render('push');
};

window.onRemoteConfigUpdated = function(view) {
    state.remote = view || state.remote;
    state.remoteDraft = null;
    state.remoteDirty = false;
    state.remoteError = null;
    if (state.page === 'remote') render('push');
};

window.onRemoteConfigError = function(msg) {
    state.remoteError = msg || 'could not save the remote block';
    if (state.page === 'remote') render('push');
};

// Service Inspector — generic renderer driven by each service's manifest
// (carried inside its status snapshot) plus the snapshot itself.

function renderServiceInspector() {
    // Bindings are recreated fresh each inspector render; config-editor inputs
    // reference indices into _cfgBind, so it must be cleared before the panels
    // append to it.
    state._cfgBind = [];
    state._cfgBadJson = {};
    let html = '<h2>Service Inspector</h2>';
    html += '<p style="color:var(--text-2);font-size:12px;margin-bottom:16px">Live status and actions for every relay-enhanced service. Panels are rendered generically from each service\'s declared manifest.</p>';

    const ids = Object.keys(state.serviceStatuses).sort();
    if (ids.length === 0) {
        html += '<div class="empty-state">No relay-enhanced services are currently registered. Start one (e.g. relayLLM) to see its status here.</div>';
        return html;
    }
    for (const id of ids) {
        html += renderServicePanel(id);
    }
    return html;
}

function serviceBadgeHTML(snap, manifest) {
    if (snap && snap.ok)  return '<span class="svc-badge ok">ok</span>';
    if (snap && !snap.ok) return '<span class="svc-badge err">error</span>';
    if (!manifest.status) return '<span class="svc-badge offline">no status declared</span>';
    return '<span class="svc-badge offline">offline</span>';
}

// A service panel is two sibling regions inside one card:
//   #svc-status-<id> — read-only live status; replaced wholesale on every 2s
//                      status poll via updateServiceStatusDOM.
//   #svc-config-<id> — the schema config editor; owns its own render lifecycle
//                      (expand / save / revert / structural edits) and is NEVER
//                      touched by a status push, so focus and in-flight
//                      keystrokes in it survive the poll. This split is the fix
//                      for the 2s-poll focus-clobber bug.
function renderServicePanel(serviceId) {
    const snap = state.serviceStatuses[serviceId];
    const manifest = (snap && snap.manifest) || {};
    let html = '<div class="svc-card">';
    html += `<div id="svc-status-${esc(serviceId)}">${renderServiceStatus(serviceId, snap, manifest)}</div>`;
    const configHTML = manifest.config ? renderConfigSection(serviceId, manifest.config) : '';
    html += `<div id="svc-config-${esc(serviceId)}">${configHTML}</div>`;
    html += '</div>';
    return html;
}

// renderServiceStatus builds the read-only status portion of a panel: header +
// badge, the status payload (scalars + tables), global action buttons, and the
// last action error. It deliberately touches NOTHING in state._cfgBind — only
// the config editor uses those bindings — so it can be re-rendered on its own
// without disturbing an open editor.
function renderServiceStatus(serviceId, snap, manifest) {
    const actions = manifest.actions || [];
    let html = `<div class="svc-card-header"><div><span class="svc-card-title">${esc(serviceId)}</span>${serviceBadgeHTML(snap, manifest)}</div><div></div></div>`;

    if (snap && !snap.ok) {
        html += `<div class="svc-err">${esc(snap.error || 'fetch failed')}</div>`;
    }

    const status = snap && snap.ok ? snap.status : null;
    if (status && typeof status === 'object') {
        html += renderStatusPayload(serviceId, status, actions);
    } else if (manifest.status) {
        html += '<div class="svc-empty">Waiting for first status snapshot…</div>';
    }

    const globalActions = actions.filter(a => !a.forEach);
    if (globalActions.length > 0) {
        html += '<div class="svc-actions" style="margin-top:10px">';
        for (const action of globalActions) {
            html += renderActionButton(serviceId, action, null);
        }
        html += '</div>';
    }

    const err = state.serviceActionError[serviceId];
    if (err) {
        html += `<div class="svc-err">${esc(err)}</div>`;
    }
    return html;
}

// updateServiceStatusDOM replaces only a service's status region in place. This
// is the surgical path used by the 2s poll (and action dispatch/result), so a
// tick never rebuilds — and never wipes — an open config editor below it. No-op
// when the panel isn't currently in the DOM (e.g. a different tab is showing).
function updateServiceStatusDOM(serviceId, snap) {
    const el = document.getElementById('svc-status-' + serviceId);
    if (!el) return;
    const manifest = (snap && snap.manifest) || {};
    el.innerHTML = renderServiceStatus(serviceId, snap, manifest);
}

// ---------------------------------------------------------------------------
// Service config editor (manifest.config)
//
// The service advertises a config file path plus a recursive schema. Relay
// ships the raw file text; we parse it into a tree, render nested forms from
// the schema (object/array/map/leaf), and serialize the edited draft back to
// JSON on save. Each input binds to a (svcId, path) entry in state._cfgBind so
// arbitrary map keys never have to be encoded into HTML — handlers carry an
// integer index, not a path. Scalar edits mutate the draft in place WITHOUT a
// re-render (preserving the caret); structural edits re-render.
// ---------------------------------------------------------------------------

function cfgGetDraft(svcId) { return state.serviceConfigDraft[svcId]; }

// anyConfigEditorOpen reports whether any service's config panel is expanded, so
// a push-driven full inspector re-render (onSettingsReloaded, etc.) can skip the
// rebuild and not disturb an open editor's focus / in-flight text.
function anyConfigEditorOpen() {
    for (const id of Object.keys(state.serviceConfigOpen)) {
        if (state.serviceConfigOpen[id]) return true;
    }
    return false;
}

// ---- Collapse state (keyed by node path) ----
function cfgExpandKey(path) { return JSON.stringify(path); }
function cfgIsExpanded(svcId, path) {
    const m = state.serviceConfigExpanded[svcId];
    return !!(m && m[cfgExpandKey(path)]);
}
function cfgSetExpanded(svcId, path, val) {
    if (!state.serviceConfigExpanded[svcId]) state.serviceConfigExpanded[svcId] = {};
    state.serviceConfigExpanded[svcId][cfgExpandKey(path)] = val;
}
function cfgToggleExpand(bindIdx) {
    const b = state._cfgBind[bindIdx];
    if (!b) return;
    cfgSetExpanded(b.svcId, b.path, !cfgIsExpanded(b.svcId, b.path));
    if (state.page === 'inspector') render();
}

function cfgChevron(expanded) {
    return `<span class="cfg-chevron${expanded ? ' open' : ''}">▸</span>`;
}


function cfgDirty(svcId) {
    const t = state.serviceConfigTree[svcId];
    const d = state.serviceConfigDraft[svcId];
    if (t === undefined || d === undefined) return false;
    return JSON.stringify(t) !== JSON.stringify(d);
}

function cfgHasBadJson(svcId) {
    for (const k of Object.keys(state._cfgBadJson)) {
        if (state._cfgBadJson[k] === svcId) return true;
    }
    return false;
}




// cfgBind records a binding and returns its index. The index is what handlers
// carry, so arbitrary map keys never reach an HTML attribute. extra carries
// optional per-binding data (e.g. a keyValue's `exclude` set).
function cfgBind(svcId, path, type, extra) {
    const b = { svcId: svcId, path: path.slice(), type: type };
    if (extra) Object.assign(b, extra);
    return state._cfgBind.push(b) - 1;
}

function renderConfigSection(serviceId, config) {
    const open = !!state.serviceConfigOpen[serviceId];
    const loaded = !!state.serviceConfigLoaded[serviceId];

    // Fetch-on-expand: pull the file the first time the panel is opened. Not in
    // the 2s poll — config is read on demand, not continuously.
    if (open && !loaded && !state.serviceConfigPending[serviceId]) {
        dispatchConfigOp(serviceId, 'get', null);
    }

    let html = '<div class="svc-resource">';
    html += `<div class="svc-resource-header ${open ? 'open' : 'closed'}" onclick="toggleConfigSection('${esc(serviceId)}')">`;
    html += `<span class="svc-resource-title"><span class="chevron">▼</span>${esc(config.label || 'Configuration')}</span>`;
    html += '</div>';

    if (open) {
        html += '<div class="svc-resource-body">';
        if (config.help) html += `<div class="svc-resource-help">${esc(config.help)}</div>`;

        const err = state.serviceConfigError[serviceId];
        if (err) html += `<div class="svc-resource-error">${esc(err)}</div>`;
        const applyMsg = state.serviceConfigApplyMsg[serviceId];
        if (applyMsg) html += `<div class="cfg-apply-note">${esc(applyMsg)}</div>`;

        if (!loaded) {
            html += '<div class="svc-resource-empty">Loading…</div>';
        } else if (state.serviceConfigDraft[serviceId] === undefined) {
            html += '<div class="svc-resource-empty">Config unavailable.</div>';
        } else {
            html += '<div class="svc-resource-form">';
            const schema = config.schema || [];
            const draft = state.serviceConfigDraft[serviceId];
            for (const field of schema) {
                html += renderConfigNode(serviceId, [field.id], field, cfgGetAt(draft, [field.id]));
            }
            html += '</div>';

            const dirty = cfgDirty(serviceId);
            const note = (config.applyMode === 'live')
                ? 'Saved changes apply live.'
                : 'Saving restarts the service to apply.';
            html += `<div class="cfg-apply-note" id="cfg-note-${esc(serviceId)}">${esc(note)}</div>`;
            html += '<div class="cfg-actions">';
            html += `<button class="btn btn-primary" id="cfg-save-${esc(serviceId)}" ${dirty ? '' : 'disabled'} onclick="saveConfig('${esc(serviceId)}')">Save</button>`;
            html += `<button class="btn btn-danger" id="cfg-revert-${esc(serviceId)}" ${dirty ? '' : 'disabled'} onclick="revertConfig('${esc(serviceId)}')">Revert</button>`;
            html += '</div>';
        }
        html += '</div>';
    }

    html += '</div>';
    return html;
}

// renderConfigNode renders one schema node bound to its current draft value.
// path is the list of keys from the config root to this node.
function renderConfigNode(svcId, path, field, value) {
    switch (field.type) {
        case 'object':   return renderConfigObject(svcId, path, field, value);
        case 'array':    return renderConfigArray(svcId, path, field, value);
        case 'map':      return renderConfigMap(svcId, path, field, value);
        case 'keyValue': return renderConfigKeyValue(svcId, path, field, (value && typeof value === 'object') ? value : {}, []);
        default:         return renderConfigLeaf(svcId, path, field, value);
    }
}

// renderObjectFields renders an object's declared child fields. A "keyValue"
// child with rest:true is bound to the parent object itself (its rows are every
// parent key except the other declared fields) — this is how a record with a
// few named fields plus an open-ended bag of extras (llama model: alias + flags)
// is edited. Shared by renderConfigObject and renderConfigItem.
function renderObjectFields(svcId, objPath, fields, obj) {
    const o = (obj && typeof obj === 'object') ? obj : {};
    const realKeys = fields.filter(f => !(f.type === 'keyValue' && f.rest)).map(f => f.id);
    let html = '';
    for (const child of fields) {
        if (child.type === 'keyValue' && child.rest) {
            html += renderConfigKeyValue(svcId, objPath, child, o, realKeys);
        } else {
            html += renderConfigNode(svcId, objPath.concat(child.id), child, o[child.id]);
        }
    }
    return html;
}

function cfgNodeLabel(field, fallback) {
    return esc(field.label || field.id || fallback || '');
}

// renderConfigObject is a collapsible group of named child fields. Collapsed by
// default so a deep config presents as a short, navigable list of sections.
function renderConfigObject(svcId, path, field, value) {
    const bindIdx = cfgBind(svcId, path, 'object');
    const expanded = cfgIsExpanded(svcId, path);
    const obj = (value && typeof value === 'object') ? value : {};
    let html = '<div class="cfg-node">';
    html += `<div class="cfg-node-head" onclick="cfgToggleExpand(${bindIdx})">`;
    html += cfgChevron(expanded);
    html += `<span class="cfg-node-title">${cfgNodeLabel(field)}</span>`;
    if (!expanded && field.help) html += `<span class="cfg-node-sub">${esc(field.help)}</span>`;
    html += '</div>';
    if (expanded) {
        html += '<div class="cfg-node-body">';
        if (field.help) html += `<div class="svc-resource-help">${esc(field.help)}</div>`;
        html += renderObjectFields(svcId, path, field.fields || [], obj);
        html += '</div>';
    }
    html += '</div>';
    return html;
}

// renderConfigArray is a collapsible group with a count badge; each element is
// itself a collapsible item row (renderConfigItem) showing a one-line summary.
function renderConfigArray(svcId, path, field, value) {
    const arr = Array.isArray(value) ? value : [];
    const bindIdx = cfgBind(svcId, path, 'array');
    const expanded = cfgIsExpanded(svcId, path);
    let html = '<div class="cfg-node">';
    html += `<div class="cfg-node-head" onclick="cfgToggleExpand(${bindIdx})">`;
    html += cfgChevron(expanded);
    html += `<span class="cfg-node-title">${cfgNodeLabel(field)}</span>`;
    html += `<span class="cfg-badge">${arr.length}</span>`;
    html += '</div>';
    if (expanded) {
        html += '<div class="cfg-node-body">';
        const itemLabel = (field.item && field.item.label) || 'item';
        for (let i = 0; i < arr.length; i++) {
            const title = cfgSummary(field.item, arr[i]) || (itemLabel + ' ' + (i + 1));
            html += renderConfigItem(svcId, path.concat(i), field.item, arr[i], title, bindIdx, i, false, '');
        }
        html += `<button class="btn btn-sm cfg-add" onclick="cfgArrayAdd(${bindIdx})">+ Add ${esc(itemLabel)}</button>`;
        html += '</div>';
    }
    html += '</div>';
    return html;
}

// renderConfigMap is a collapsible group of user-keyed entries. Each entry is a
// collapsible item row titled "<key> — <summary>"; the key is editable inside.
function renderConfigMap(svcId, path, field, value) {
    const obj = (value && typeof value === 'object') ? value : {};
    const bindIdx = cfgBind(svcId, path, 'map');
    const expanded = cfgIsExpanded(svcId, path);
    const keys = Object.keys(obj);
    const keyLabel = field.keyLabel || 'key';
    let html = '<div class="cfg-node">';
    html += `<div class="cfg-node-head" onclick="cfgToggleExpand(${bindIdx})">`;
    html += cfgChevron(expanded);
    html += `<span class="cfg-node-title">${cfgNodeLabel(field)}</span>`;
    html += `<span class="cfg-badge">${keys.length}</span>`;
    html += '</div>';
    if (expanded) {
        html += '<div class="cfg-node-body">';
        for (let ki = 0; ki < keys.length; ki++) {
            const k = keys[ki];
            const sub = cfgSummary(field.item, obj[k]);
            const title = k + (sub ? ' — ' + sub : '');
            html += renderConfigItem(svcId, path.concat(k), field.item, obj[k], title, bindIdx, ki, true, keyLabel);
        }
        html += `<button class="btn btn-sm cfg-add" onclick="cfgMapAdd(${bindIdx})">+ Add</button>`;
        html += '</div>';
    }
    html += '</div>';
    return html;
}

// renderConfigItem renders one collection element as a collapsible card: a
// header (chevron + summary title + Remove) and, when expanded, its fields. For
// map entries the editable key input is rendered first.
function renderConfigItem(svcId, path, itemField, value, title, containerBindIdx, indexOrKey, isMap, keyLabel) {
    const bindIdx = cfgBind(svcId, path, 'item');
    const expanded = cfgIsExpanded(svcId, path);
    const removeCall = isMap
        ? `cfgMapRemove(${containerBindIdx}, ${indexOrKey})`
        : `cfgArrayRemove(${containerBindIdx}, ${indexOrKey})`;
    let html = '<div class="cfg-item">';
    html += '<div class="cfg-item-head">';
    html += `<span class="cfg-item-toggle" onclick="cfgToggleExpand(${bindIdx})">${cfgChevron(expanded)}<span class="cfg-item-title">${esc(title || 'item')}</span></span>`;
    html += `<button class="btn btn-sm btn-danger cfg-item-remove" onclick="${removeCall}">Remove</button>`;
    html += '</div>';
    if (expanded) {
        html += '<div class="cfg-item-body">';
        if (isMap) {
            const curKey = path[path.length - 1];
            html += '<div class="cfg-leaf">';
            html += `<label>${esc(keyLabel)}</label>`;
            html += `<input type="text" value="${esc(String(curKey))}" autocorrect="off" autocapitalize="off" spellcheck="false" onchange="cfgMapRename(${containerBindIdx}, ${indexOrKey}, this)"/>`;
            html += '</div>';
        }
        if (itemField && itemField.type === 'object') {
            html += renderObjectFields(svcId, path, itemField.fields || [], (value && typeof value === 'object') ? value : {});
        } else {
            html += renderConfigNode(svcId, path, itemField, value);
        }
        html += '</div>';
    }
    html += '</div>';
    return html;
}

// renderConfigKeyValue renders an editable bag of key/value rows. containerObj
// is the object the rows live in; excludeKeys are keys owned by sibling fields
// (hidden here). Values are typed on input (true/false → bool, numeric → number,
// else string) so they round-trip to the right JSON type.
function renderConfigKeyValue(svcId, containerPath, field, containerObj, excludeKeys) {
    const exclude = excludeKeys || [];
    const bindIdx = cfgBind(svcId, containerPath, 'keyValue', { exclude: exclude });
    const keys = Object.keys(containerObj || {}).filter(k => exclude.indexOf(k) < 0);
    const keyLabel = field.keyLabel || 'key';
    let html = '<div class="cfg-kv">';
    if (field.id || field.label) html += `<div class="cfg-kv-label">${cfgNodeLabel(field)}</div>`;
    if (field.help) html += `<div class="svc-resource-help">${esc(field.help)}</div>`;
    if (keys.length === 0) html += '<div class="cfg-kv-empty">No entries yet.</div>';
    for (let i = 0; i < keys.length; i++) {
        const k = keys[i];
        html += '<div class="cfg-kv-row">';
        html += `<input class="cfg-kv-key" type="text" value="${esc(k)}" placeholder="${esc(keyLabel)}" autocorrect="off" autocapitalize="off" spellcheck="false" onchange="cfgKvRename(${bindIdx}, ${i}, this)"/>`;
        html += `<input class="cfg-kv-val" type="text" value="${esc(cfgKvDisplay(containerObj[k]))}" placeholder="value" autocorrect="off" autocapitalize="off" spellcheck="false" oninput="cfgKvSetVal(${bindIdx}, ${i}, this)"/>`;
        html += `<button class="btn btn-sm btn-danger cfg-kv-del" onclick="cfgKvRemove(${bindIdx}, ${i})">×</button>`;
        html += '</div>';
    }
    html += `<button class="btn btn-sm cfg-add" onclick="cfgKvAdd(${bindIdx})">+ Add ${esc(keyLabel)}</button>`;
    html += '</div>';
    return html;
}



// cfgKvState resolves the live container object + its visible (non-excluded)
// keys for a keyValue binding.
function cfgKvState(b) {
    const obj = cfgGetAt(cfgGetDraft(b.svcId), b.path) || {};
    const exclude = b.exclude || [];
    return { obj: obj, keys: Object.keys(obj).filter(k => exclude.indexOf(k) < 0) };
}

function cfgKvSetVal(bindIdx, i, el) {
    const b = state._cfgBind[bindIdx];
    if (!b) return;
    const k = cfgKvState(b).keys[i];
    if (k === undefined) return;
    cfgSetAt(cfgGetDraft(b.svcId), b.path.concat(k), cfgKvCoerce(el.value));
    cfgRefreshChrome(b.svcId);
}

function cfgKvRename(bindIdx, i, el) {
    const b = state._cfgBind[bindIdx];
    if (!b) return;
    const st = cfgKvState(b);
    const oldKey = st.keys[i];
    const newKey = el.value.trim();
    if (oldKey === undefined || newKey === oldKey) return;
    if (newKey === '' || newKey in st.obj) { el.value = oldKey; return; }
    // Rebuild over ALL keys (including excluded ones) to preserve order.
    const rebuilt = {};
    for (const kk of Object.keys(st.obj)) rebuilt[kk === oldKey ? newKey : kk] = st.obj[kk];
    cfgSetAt(cfgGetDraft(b.svcId), b.path, rebuilt);
    cfgRerender();
}

function cfgKvRemove(bindIdx, i) {
    const b = state._cfgBind[bindIdx];
    if (!b) return;
    const st = cfgKvState(b);
    const k = st.keys[i];
    if (k !== undefined) delete st.obj[k];
    cfgRerender();
}

function cfgKvAdd(bindIdx) {
    const b = state._cfgBind[bindIdx];
    if (!b) return;
    const draft = cfgGetDraft(b.svcId);
    let obj = cfgGetAt(draft, b.path);
    if (!obj || typeof obj !== 'object') { obj = {}; cfgSetAt(draft, b.path, obj); }
    let key = 'key', n = 2;
    while (key in obj) key = 'key-' + (n++);
    obj[key] = '';
    cfgRerender();
}

function renderConfigLeaf(svcId, path, field, value) {
    const bindIdx = cfgBind(svcId, path, field.type);
    const inputId = 'cfg-in-' + bindIdx;
    const noFix = ' autocorrect="off" autocapitalize="off" spellcheck="false"';
    // Placeholder hints the expected/default value for empty optional fields
    // (the editor shows what's on disk, so an unset field renders blank).
    const ph = field.placeholder ? ` placeholder="${esc(field.placeholder)}"` : '';
    let html = '<div class="cfg-leaf">';
    html += `<label>${cfgNodeLabel(field)}${field.required ? ' *' : ''}</label>`;
    switch (field.type) {
        case 'bool':
            html += `<div class="toggle-row" style="margin-top:4px"><span style="font-size:12px;color:var(--text-2)">${esc(field.help || '')}</span><label class="switch"><input type="checkbox" id="${inputId}" ${value ? 'checked' : ''} onchange="cfgEdit(${bindIdx}, this)"/><span class="slider"></span></label></div>`;
            html += '</div>';
            return html;
        case 'number':
            html += `<input type="number" id="${inputId}" value="${esc(value === undefined || value === null ? '' : String(value))}"${ph} oninput="cfgEdit(${bindIdx}, this)"/>`;
            break;
        case 'select': {
            html += `<select id="${inputId}" onchange="cfgEdit(${bindIdx}, this)">`;
            const opts = field.options || [];
            const cur = (value === undefined || value === null) ? '' : String(value);
            if (cur === '' || opts.indexOf(cur) < 0) html += `<option value="" ${cur === '' ? 'selected' : ''}></option>`;
            for (const o of opts) html += `<option value="${esc(o)}" ${o === cur ? 'selected' : ''}>${esc(o)}</option>`;
            html += '</select>';
            break;
        }
        case 'secret':
            html += `<input type="password" id="${inputId}" value="${esc(value === undefined || value === null ? '' : String(value))}"${ph}${noFix} oninput="cfgEdit(${bindIdx}, this)"/>`;
            break;
        case 'textarea':
            html += `<textarea id="${inputId}" rows="3"${ph}${noFix} oninput="cfgEdit(${bindIdx}, this)">${esc(value || '')}</textarea>`;
            break;
        case 'string[]':
            html += `<textarea id="${inputId}" rows="3" placeholder="one per line"${noFix} oninput="cfgEdit(${bindIdx}, this)">${esc(Array.isArray(value) ? value.join('\n') : (value || ''))}</textarea>`;
            break;
        case 'stringMap':
            html += `<textarea id="${inputId}" rows="3" placeholder="KEY=VALUE per line"${noFix} oninput="cfgEdit(${bindIdx}, this)">${esc(cfgFormatStringMap(value))}</textarea>`;
            break;
        case 'json':
            html += `<textarea id="${inputId}" rows="4" placeholder="raw JSON"${noFix} oninput="cfgEditJson(${bindIdx}, this)">${esc(cfgFormatJson(value))}</textarea>`;
            break;
        default: // text
            html += `<input type="text" id="${inputId}" value="${esc(value === undefined || value === null ? '' : String(value))}"${ph}${noFix} oninput="cfgEdit(${bindIdx}, this)"/>`;
    }
    if (field.help && field.type !== 'bool') {
        html += `<div style="color:var(--text-3);font-size:11px;margin-top:2px">${esc(field.help)}</div>`;
    }
    html += '</div>';
    return html;
}




// cfgEdit writes a scalar leaf edit into the draft WITHOUT re-rendering, so the
// caret survives typing. It refreshes only the Save/Revert chrome.
function cfgEdit(bindIdx, el) {
    const b = state._cfgBind[bindIdx];
    if (!b) return;
    if (b.type === 'number') {
        const txt = el.value.trim();
        const bad = txt !== '' && Number.isNaN(Number(txt));
        el.classList.toggle('cfg-bad', bad);
        if (bad) { cfgRefreshChrome(b.svcId); return; }
    }
    cfgSetAt(cfgGetDraft(b.svcId), b.path, cfgCoerce(b.type, el));
    cfgRefreshChrome(b.svcId);
}

// cfgEditJson handles the raw-JSON leaf: parse on each keystroke, write the
// parsed value into the draft when valid, flag the field + block Save when not.
function cfgEditJson(bindIdx, el) {
    const b = state._cfgBind[bindIdx];
    if (!b) return;
    const txt = el.value.trim();
    if (txt === '') {
        delete state._cfgBadJson[bindIdx];
        el.classList.remove('cfg-bad');
        cfgSetAt(cfgGetDraft(b.svcId), b.path, null);
        cfgRefreshChrome(b.svcId);
        return;
    }
    let parsed;
    try {
        parsed = JSON.parse(txt);
    } catch (e) {
        state._cfgBadJson[bindIdx] = b.svcId;
        el.classList.add('cfg-bad');
        cfgRefreshChrome(b.svcId);
        return;
    }
    delete state._cfgBadJson[bindIdx];
    el.classList.remove('cfg-bad');
    cfgSetAt(cfgGetDraft(b.svcId), b.path, parsed);
    cfgRefreshChrome(b.svcId);
}

// cfgRefreshChrome updates Save/Revert enabled state and the inline note
// imperatively (no re-render) so scalar typing never loses focus.
function cfgRefreshChrome(svcId) {
    const dirty = cfgDirty(svcId);
    const bad = cfgHasBadJson(svcId);
    const save = document.getElementById('cfg-save-' + svcId);
    const revert = document.getElementById('cfg-revert-' + svcId);
    const note = document.getElementById('cfg-note-' + svcId);
    if (save) save.disabled = !(dirty && !bad);
    if (revert) revert.disabled = !dirty;
    if (note) {
        if (bad) {
            note.textContent = 'Fix invalid JSON before saving.';
        } else {
            const snap = state.serviceStatuses[svcId];
            const cfg = snap && snap.manifest && snap.manifest.config;
            note.textContent = (cfg && cfg.applyMode === 'live')
                ? 'Saved changes apply live.'
                : 'Saving restarts the service to apply.';
        }
    }
}

function cfgArrayAdd(bindIdx) {
    const b = state._cfgBind[bindIdx];
    if (!b) return;
    const draft = cfgGetDraft(b.svcId);
    let arr = cfgGetAt(draft, b.path);
    if (!Array.isArray(arr)) { arr = []; cfgSetAt(draft, b.path, arr); }
    const field = cfgFieldAt(b.svcId, b.path);
    arr.push(cfgDefaultFor((field && field.item) || { type: 'text' }));
    cfgSetExpanded(b.svcId, b.path, true);                        // keep the group open
    cfgSetExpanded(b.svcId, b.path.concat(arr.length - 1), true); // open the new item
    cfgRerender();
}

function cfgArrayRemove(bindIdx, i) {
    const b = state._cfgBind[bindIdx];
    if (!b) return;
    const arr = cfgGetAt(cfgGetDraft(b.svcId), b.path);
    if (Array.isArray(arr)) arr.splice(i, 1);
    cfgRerender();
}

function cfgMapAdd(bindIdx) {
    const b = state._cfgBind[bindIdx];
    if (!b) return;
    const draft = cfgGetDraft(b.svcId);
    let obj = cfgGetAt(draft, b.path);
    if (!obj || typeof obj !== 'object') { obj = {}; cfgSetAt(draft, b.path, obj); }
    let key = 'new-key', n = 2;
    while (key in obj) key = 'new-key-' + (n++);
    const field = cfgFieldAt(b.svcId, b.path);
    obj[key] = cfgDefaultFor((field && field.item) || { type: 'object', fields: [] });
    cfgSetExpanded(b.svcId, b.path, true);                  // keep the group open
    cfgSetExpanded(b.svcId, b.path.concat(key), true);      // open the new entry
    cfgRerender();
}

function cfgMapRemove(bindIdx, keyIndex) {
    const b = state._cfgBind[bindIdx];
    if (!b) return;
    const obj = cfgGetAt(cfgGetDraft(b.svcId), b.path);
    if (!obj || typeof obj !== 'object') return;
    const k = Object.keys(obj)[keyIndex];
    if (k !== undefined) delete obj[k];
    cfgRerender();
}

// cfgMapRename rekeys an entry, preserving insertion order (rebuild the object).
// keyIndex is resolved against the live object so arbitrary key strings never
// have to be embedded in HTML.
function cfgMapRename(bindIdx, keyIndex, el) {
    const b = state._cfgBind[bindIdx];
    if (!b) return;
    const obj = cfgGetAt(cfgGetDraft(b.svcId), b.path);
    if (!obj || typeof obj !== 'object') return;
    const keys = Object.keys(obj);
    const oldKey = keys[keyIndex];
    const newKey = el.value.trim();
    if (oldKey === undefined || newKey === oldKey) return;
    if (newKey === '' || newKey in obj) { el.value = oldKey; return; }
    const rebuilt = {};
    for (const k of keys) rebuilt[k === oldKey ? newKey : k] = obj[k];
    cfgSetAt(cfgGetDraft(b.svcId), b.path, rebuilt);
    cfgRerender();
}

// cfgFieldAt re-walks the SCHEMA (not the draft) to the FieldDecl at a path so
// add/remove know the item schema for defaults. Numeric steps descend an
// array's item; a key step under a map descends the map's item.
function cfgFieldAt(svcId, path) {
    const snap = state.serviceStatuses[svcId];
    const config = snap && snap.manifest && snap.manifest.config;
    if (!config) return null;
    let fields = config.schema || [];
    let field = null;
    for (const step of path) {
        if (typeof step === 'number') {
            field = field ? field.item : null;
        } else if (field && field.type === 'map') {
            field = field.item || null;
        } else {
            field = (fields || []).find(f => f.id === step) || null;
        }
        if (!field) return null;
        fields = (field.type === 'object') ? (field.fields || []) : [];
    }
    return field;
}

function cfgRerender() {
    if (state.page === 'inspector') render();
}

function toggleConfigSection(serviceId) {
    state.serviceConfigOpen[serviceId] = !state.serviceConfigOpen[serviceId];
    if (state.page === 'inspector') render();
}

function dispatchConfigOp(serviceId, op, text) {
    state.serviceConfigPending[serviceId] = true;
    const msg = { type: 'service_config', serviceId: serviceId, op: op };
    if (text !== null && text !== undefined) msg.text = text;
    ipc(JSON.stringify(msg));
}

function saveConfig(serviceId) {
    if (cfgHasBadJson(serviceId)) return;
    const draft = state.serviceConfigDraft[serviceId];
    if (draft === undefined) return;
    const missing = cfgFirstMissingRequired(serviceId);
    if (missing) {
        state.serviceConfigError[serviceId] = 'Required field missing: ' + missing;
        if (state.page === 'inspector') render();
        return;
    }
    state.serviceConfigError[serviceId] = null;
    state.serviceConfigApplyMsg[serviceId] = null;
    dispatchConfigOp(serviceId, 'save', JSON.stringify(draft, null, 2));
}

function revertConfig(serviceId) {
    const tree = state.serviceConfigTree[serviceId];
    state.serviceConfigDraft[serviceId] = (tree === undefined) ? undefined : JSON.parse(JSON.stringify(tree));
    state.serviceConfigError[serviceId] = null;
    if (state.page === 'inspector') render();
}

// cfgFirstMissingRequired walks the schema against the draft and returns the
// label of the first required leaf that is empty, or null. Checked at save time
// (the server validates parse only, not schema).
function cfgFirstMissingRequired(svcId) {
    const snap = state.serviceStatuses[svcId];
    const config = snap && snap.manifest && snap.manifest.config;
    const draft = state.serviceConfigDraft[svcId];
    if (!config || draft === undefined) return null;
    return cfgScanRequired(config.schema || [], draft);
}


window.onServiceConfigResult = function(result) {
    if (!result) return;
    const id = result.serviceId;
    state.serviceConfigPending[id] = false;
    // Clear any stale apply note ("Restarting…" / error) from a prior save now
    // that a fresh config op has completed; it is otherwise never reset.
    state.serviceConfigApplyMsg[id] = null;
    if (!result.ok) {
        state.serviceConfigError[id] = result.error || ((result.op || 'config') + ' failed');
        if (state.page === 'inspector') render();
        return;
    }
    state.serviceConfigError[id] = null;
    if (result.op === 'get') {
        state.serviceConfigLoaded[id] = true;
        const tree = cfgParseConfigText(result.text || '');
        if (tree === undefined) {
            state.serviceConfigError[id] = 'Could not parse config file as JSON.';
            state.serviceConfigTree[id] = undefined;
            state.serviceConfigDraft[id] = undefined;
        } else {
            state.serviceConfigTree[id] = tree;
            state.serviceConfigDraft[id] = JSON.parse(JSON.stringify(tree));
        }
    } else if (result.op === 'save') {
        if (state.serviceConfigDraft[id] !== undefined) {
            state.serviceConfigTree[id] = JSON.parse(JSON.stringify(state.serviceConfigDraft[id]));
        }
    }
    if (state.page === 'inspector') render();
};

window.onServiceConfigApplied = function(p) {
    if (!p) return;
    let msg = 'Saved.';
    if (p.mode === 'restarting') msg = 'Restarting service to apply…';
    else if (p.mode === 'error') msg = p.error || 'Restart failed.';
    state.serviceConfigApplyMsg[p.serviceId] = msg;
    if (state.page === 'inspector') render();
};



// renderStatusPayload walks a free-form JSON object and emits a key/value
// list for scalars + a table for any top-level array. forEach actions
// attach one button per row to the table whose key matches the action's
// `forEach` field.
function renderStatusPayload(serviceId, payload, actions) {
    let html = '';
    const scalarKeys = [];
    const arrayKeys = [];
    for (const k of Object.keys(payload)) {
        const v = payload[k];
        if (Array.isArray(v)) {
            arrayKeys.push(k);
        } else if (v !== null && typeof v !== 'object') {
            scalarKeys.push(k);
        }
    }

    if (scalarKeys.length > 0) {
        html += '<div class="svc-stats">';
        for (const k of scalarKeys) {
            html += `<div><div class="svc-stat-label">${esc(k)}</div><div class="svc-stat-value">${esc(formatScalar(payload[k]))}</div></div>`;
        }
        html += '</div>';
    }

    for (const k of arrayKeys) {
        html += renderArrayBlock(serviceId, k, payload[k], actions);
    }
    return html;
}

function renderArrayBlock(serviceId, arrayKey, rows, actions) {
    let html = `<div style="margin-top:12px"><div class="svc-stat-label" style="margin-bottom:4px">${esc(arrayKey)}</div>`;
    if (!rows || rows.length === 0) {
        html += '<div class="svc-empty">empty</div></div>';
        return html;
    }

    // Discover columns from the union of row keys, with a stable order
    // (insertion order of the first row, then any extras at the end).
    const columns = [];
    const seen = {};
    for (const row of rows) {
        if (row && typeof row === 'object') {
            for (const k of Object.keys(row)) {
                if (!seen[k]) { seen[k] = true; columns.push(k); }
            }
        }
    }

    const rowActions = actions.filter(a => a.forEach === arrayKey);

    html += '<table class="svc-table"><thead><tr>';
    for (const col of columns) {
        html += `<th>${esc(col)}</th>`;
    }
    if (rowActions.length > 0) {
        html += '<th style="width:1%">actions</th>';
    }
    html += '</tr></thead><tbody>';

    for (let i = 0; i < rows.length; i++) {
        const row = rows[i] || {};
        const rowKey = canonRowKey(row);
        const pendingClass = isAnyActionPending(serviceId, rowActions, rowKey) ? ' class="pending"' : '';
        html += `<tr${pendingClass}>`;
        for (const col of columns) {
            html += `<td>${esc(formatScalar(row[col]))}</td>`;
        }
        if (rowActions.length > 0) {
            html += '<td><div class="svc-actions">';
            for (const action of rowActions) {
                html += renderActionButton(serviceId, action, row);
            }
            html += '</div></td>';
        }
        html += '</tr>';
    }
    html += '</tbody></table></div>';
    return html;
}

// Buttons carry their dispatch payload as data-* attributes; a single
// delegated click listener (installed once on document) reads them. This
// keeps re-renders free of per-button handler wiring.
// canonRowKey builds the per-row pending key with sorted object keys so it
// matches whether the row came from the service's status JSON (insertion order)
// or was echoed back by Go (which marshals map keys alphabetically). A mismatch
// would leave the action button stuck disabled after its result arrives.
function canonRowKey(row) {
    if (!row || typeof row !== 'object') return '';
    const out = {};
    for (const k of Object.keys(row).sort()) out[k] = row[k];
    return JSON.stringify(out);
}

function renderActionButton(serviceId, action, row) {
    const rowJson = row ? JSON.stringify(row) : '';
    const pending = !!state.serviceActionPending[serviceId + '|' + action.id + '|' + canonRowKey(row)];
    const danger = String(action.method || '').toUpperCase() === 'DELETE';
    const cls = 'btn btn-sm' + (danger ? ' btn-danger' : '');
    const label = pending ? '<span class="spinner"></span>' + esc(action.label) : esc(action.label);
    return `<button class="${cls} svc-action-btn"`
        + ` data-svc="${esc(serviceId)}"`
        + ` data-action="${esc(action.id)}"`
        + ` data-row="${esc(rowJson)}"`
        + (pending ? ' disabled' : '')
        + `>${label}</button>`;
}

// Delegated handler for Service Inspector action buttons. Wrapped in
// try/catch so a malformed data-row (or a bug in dispatchServiceAction)
// can't poison the document click queue — the listener stays subscribed
// for subsequent clicks even when one click fails.
document.addEventListener('click', function(e) {
    try {
        const btn = e.target.closest && e.target.closest('.svc-action-btn');
        if (!btn || btn.disabled) return;
        let row = null;
        if (btn.dataset.row) {
            try {
                row = JSON.parse(btn.dataset.row);
            } catch (parseErr) {
                console.warn('svc-action-btn: bad data-row JSON', btn.dataset.row, parseErr);
                return;
            }
        }
        dispatchServiceAction(btn.dataset.svc, btn.dataset.action, row);
    } catch (err) {
        console.error('svc-action-btn click handler failed', err);
    }
});

function isAnyActionPending(serviceId, actions, rowKey) {
    for (const a of actions) {
        if (state.serviceActionPending[serviceId + '|' + a.id + '|' + rowKey]) return true;
    }
    return false;
}

function dispatchServiceAction(serviceId, actionId, row) {
    const rowKey = canonRowKey(row);
    state.serviceActionPending[serviceId + '|' + actionId + '|' + rowKey] = true;
    // Show the pending spinner immediately by re-rendering only this service's
    // status region — an open config editor below it is left intact.
    if (state.page === 'inspector') updateServiceStatusDOM(serviceId, state.serviceStatuses[serviceId]);
    ipc(JSON.stringify({
        type: 'service_action',
        serviceId: serviceId,
        actionId: actionId,
        row: row || undefined,
    }));
}




// ---------------------------------------------------------------------------
// Tool Calls tab — the audit log viewer.
//
// Filtering is two-tier. The recorder holds a bounded in-memory ring of recent
// events; that is what loads by default and what live events append to, and
// filtering it happens here in the page so typing stays instant. "Search
// history" re-runs the same filter server-side against the log file, for events
// older than the ring holds.
// ---------------------------------------------------------------------------

// 'throttled' is a budget refusal on a remote enrolment: the grant was
// legitimate and the pattern of use was not. 'pending' is the intent half of a
// remote call, written before the MCP runs and still awaiting its completion.
const AUDIT_OUTCOMES = ['ok', 'error', 'tool_error', 'denied', 'unauthorized', 'throttled', 'pending'];
const AUDIT_EVENT_KINDS = [
    ['call_tool', 'Tool calls'],
    ['list_tools', 'Tool lists'],
    ['list_skills', 'Skill lists'],
];
// Actor kinds. 'remote' is its own filter so "everything any VM did" is one
// question rather than an inference from which actor fields are populated.
const AUDIT_ACTOR_KINDS = [
    ['project', 'Project'],
    ['service', 'Service'],
    ['remote', 'Remote'],
    ['unknown', 'Unauthenticated'],
];

function queryAudit(deep) {
    const f = state.auditFilter;
    state.auditError = null;
    // Remember the mode so a subsequent dropdown change re-runs the same kind
    // of query rather than silently dropping the user back to the ring.
    f.deep = !!deep;
    ipc(JSON.stringify({
        type: 'query_audit',
        project_id: f.project_id || undefined,
        mcp_id: f.mcp_id || undefined,
        outcome: f.outcome || undefined,
        event: f.event || undefined,
        kind: f.kind || undefined,
        text: deep ? (f.text || undefined) : undefined,
        limit: deep ? 2000 : 0,
        deep: !!deep,
    }));
}

function exportAudit() {
    const f = state.auditFilter;
    ipc(JSON.stringify({
        type: 'export_audit',
        project_id: f.project_id || undefined,
        mcp_id: f.mcp_id || undefined,
        outcome: f.outcome || undefined,
        event: f.event || undefined,
        kind: f.kind || undefined,
        text: f.text || undefined,
    }));
}

function revealAuditLog() {
    ipc(JSON.stringify({ type: 'reveal_audit_log' }));
}

function setAuditFilter(key, value) {
    state.auditFilter[key] = value;
    // Server-side fields need a refetch; text is applied locally so each
    // keystroke doesn't cross the IPC boundary.
    if (key !== 'text') queryAudit(state.auditFilter.deep);
    render();
}

function toggleAuditFollow(on) {
    state.auditFollow = !!on;
    render();
}

function toggleAuditRow(id) {
    state.auditExpanded[id] = !state.auditExpanded[id];
    render();
}

// auditMatches applies the locally-evaluated part of the filter. The
// server-side fields are already applied by the query; text is not, so a
// keystroke re-filters what is loaded without a round trip.
function auditMatches(ev) {
    const f = state.auditFilter;
    if (f.project_id && (ev.actor || {}).project_id !== f.project_id) return false;
    if (f.mcp_id && ev.mcp_id !== f.mcp_id) return false;
    if (f.outcome && ev.outcome !== f.outcome) return false;
    if (f.event && ev.event !== f.event) return false;
    if (f.kind && (ev.actor || {}).kind !== f.kind) return false;
    if (f.text) {
        const a = ev.actor || {};
        const hay = [ev.tool, ev.mcp_id, ev.error, a.project_name, a.proc, a.parent,
                     a.client_id, a.remote_addr,
                     typeof ev.args === 'string' ? ev.args : JSON.stringify(ev.args || '')]
            .join('\u0000').toLowerCase();
        if (hay.indexOf(f.text.toLowerCase()) === -1) return false;
    }
    return true;
}

function auditVisible() {
    return state.auditEvents.filter(auditMatches);
}

function auditFmtTime(ts) {
    if (!ts) return '';
    const d = new Date(ts);
    if (isNaN(d.getTime())) return '';
    const p = (n) => String(n).padStart(2, '0');
    return p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds());
}

// auditCaller renders the actor as "parent \u2192 proc". The parent leads because
// `relay mcp` spawns a fresh child per call, so the process itself is usually a
// throwaway and the parent is the agent that actually asked.
function auditCaller(a) {
    a = a || {};
    // A remote caller has no process to name — pid attribution is meaningless
    // across a network — so the enrolled client stands in for it.
    if (a.client_id) return a.client_id;
    if (a.parent && a.proc) return a.parent + ' \u2192 ' + a.proc;
    return a.proc || a.parent || (a.pid ? 'pid ' + a.pid : '');
}

function auditDetail(ev) {
    if (ev.error) return ev.error;
    if (ev.args) return typeof ev.args === 'string' ? ev.args : JSON.stringify(ev.args);
    if (ev.tool_count) return ev.tool_count + ' tools visible';
    return '';
}

function auditPretty(args) {
    if (args === undefined || args === null) return '';
    if (typeof args === 'string') return args;
    try { return JSON.stringify(args, null, 2); } catch (e) { return String(args); }
}

function auditSelect(key, label, options, current) {
    let html = '<select onchange="setAuditFilter(\'' + key + '\', this.value)">';
    html += '<option value="">' + esc(label) + '</option>';
    for (const [val, text] of options) {
        html += '<option value="' + esc(val) + '"' + (current === val ? ' selected' : '') + '>' + esc(text) + '</option>';
    }
    return html + '</select>';
}

function renderAudit() {
    const st = state.auditStatus;
    let html = '<div class="page-header"><h2>Tool Calls</h2><div style="display:flex;gap:8px">';
    html += '<button class="btn btn-sm" onclick="queryAudit(false)">Refresh</button>';
    html += '<button class="btn btn-sm" onclick="queryAudit(true)">Search History</button>';
    html += '<button class="btn btn-sm" onclick="exportAudit()">Export</button>';
    html += '<button class="btn btn-sm" onclick="revealAuditLog()">Reveal Log</button>';
    html += '</div></div>';

    if (st && !st.enabled) {
        html += '<div class="audit-note warn">Auditing is disabled. Set <code>audit.enabled</code> to true in settings.json and restart Relay.</div>';
        return html;
    }
    if (state.auditError) {
        html += '<div class="audit-note warn">' + esc(state.auditError) + '</div>';
    }
    if (st && st.dropped > 0) {
        html += '<div class="audit-note warn">' + st.dropped + ' event(s) were dropped because the audit queue was full \u2014 this log is incomplete.</div>';
    }
    if (state.auditExportPath) {
        html += '<div class="audit-note">Exported to <code>' + esc(state.auditExportPath) + '</code></div>';
    }

    // Filter bar.
    const f = state.auditFilter;
    html += '<div class="audit-bar">';
    html += '<input type="search" class="grow" placeholder="Filter by tool, project, caller, arguments\u2026" value="' + esc(f.text) + '" id="auditText" oninput="setAuditFilter(\'text\', this.value)">';
    html += auditSelect('project_id', 'All projects', (state.projects || []).map(p => [p.id, p.name]), f.project_id);
    html += auditSelect('mcp_id', 'All MCPs', (state.externalMcps || []).map(m => [m.id, m.display_name || m.id]), f.mcp_id);
    html += auditSelect('outcome', 'Any outcome', AUDIT_OUTCOMES.map(o => [o, o]), f.outcome);
    html += auditSelect('kind', 'Any caller', AUDIT_ACTOR_KINDS, f.kind);
    html += auditSelect('event', 'All events', AUDIT_EVENT_KINDS, f.event);
    html += '<label style="font-size:12px;color:var(--text-2);display:flex;align-items:center;gap:5px">';
    html += '<input type="checkbox"' + (state.auditFollow ? ' checked' : '') + ' onchange="toggleAuditFollow(this.checked)">Follow</label>';
    html += '</div>';

    const rows = auditVisible();
    if (!rows.length) {
        html += '<div class="audit-empty">' + (state.auditLoaded ? 'No tool calls match this filter.' : 'Loading\u2026') + '</div>';
        if (st && !st.log_lists) {
            html += '<div class="audit-note">Tool-list events are not being recorded. Set <code>audit.log_lists</code> to true to include them.</div>';
        }
        return html;
    }

    html += '<table class="audit-table"><colgroup>';
    html += '<col style="width:70px"><col style="width:118px"><col style="width:14%"><col style="width:12%">';
    html += '<col style="width:18%"><col style="width:60px"><col style="width:16%"><col>';
    html += '</colgroup><thead><tr>';
    html += '<th>Time</th><th>Outcome</th><th>Project</th><th>MCP</th><th>Tool</th><th>ms</th><th>Caller</th><th>Detail</th>';
    html += '</tr></thead><tbody id="auditRows">';
    for (const ev of rows) html += renderAuditRow(ev);
    html += '</tbody></table>';
    return html;
}

function renderAuditRow(ev) {
    const a = ev.actor || {};
    const expanded = !!state.auditExpanded[ev.id];
    let html = '<tr class="row" onclick="toggleAuditRow(\'' + esc(ev.id) + '\')">';
    html += '<td class="audit-time">' + esc(auditFmtTime(ev.ts)) + '</td>';
    html += '<td class="audit-outcome-cell"><span class="audit-pill audit-' + esc(ev.outcome) + '">' + esc(ev.outcome) + '</span></td>';
    html += '<td title="' + esc(a.project_name || '') + '">' + esc(a.project_name || '\u2014') + '</td>';
    html += '<td>' + esc(ev.mcp_id || '\u2014') + '</td>';
    html += '<td class="audit-tool" title="' + esc(ev.tool || '') + '">' + esc(ev.tool || ev.event) + '</td>';
    html += '<td class="audit-ms">' + (ev.dur_ms || 0) + '</td>';
    html += '<td title="' + esc(auditCaller(a)) + '">' + esc(auditCaller(a) || '\u2014') + '</td>';
    const detail = auditDetail(ev);
    html += '<td class="audit-detail" title="' + esc(detail) + '">' + esc(detail) + '</td>';
    html += '</tr>';
    if (expanded) html += renderAuditDetail(ev);
    return html;
}

function renderAuditDetail(ev) {
    const a = ev.actor || {};
    const kv = [];
    const add = (k, v) => { if (v !== undefined && v !== null && v !== '') kv.push([k, String(v)]); };
    add('Event', ev.event);
    add('When', ev.ts);
    add('Duration', (ev.dur_ms || 0) + ' ms');
    add('Project', a.project_name ? a.project_name + ' (' + (a.project_id || '') + ')' : '');
    add('Actor', a.kind);
    add('Auth', a.auth);
    add('Working dir', a.cwd);
    add('Caller pid', a.pid);
    add('Process', a.proc);
    add('Parent', a.parent);
    add('Client', a.client_id);
    // The fingerprint is shown in full: after an enrolment is deleted it is the
    // only thing left that says which key made the call.
    add('Fingerprint', a.fingerprint);
    add('Remote address', a.remote_addr);
    add('MCP', ev.mcp_id);
    add('Tool', ev.tool);
    add('Outcome', ev.outcome);
    // An intent with no completion sharing this id means relay invoked an MCP
    // and never learned the outcome. Worth surfacing, not worth hiding.
    add('Phase', ev.phase);
    add('Error', ev.error);
    if (ev.result_bytes) add('Result', ev.result_bytes + ' bytes' + (ev.result_is_error ? ' (isError)' : ''));
    if (ev.tool_count) add('Tools visible', ev.tool_count);
    add('Event id', ev.id);

    let html = '<tr class="audit-expand"><td colspan="8">';
    html += '<dl class="audit-kv">';
    for (const [k, v] of kv) html += '<dt>' + esc(k) + '</dt><dd>' + esc(v) + '</dd>';
    html += '</dl>';
    if (ev.args !== undefined && ev.args !== null && ev.args !== '') {
        const label = ev.args_truncated
            ? 'Arguments (truncated from ' + (ev.args_bytes || 0) + ' bytes)'
            : 'Arguments';
        html += '<div style="margin-top:10px;font-size:11px;color:var(--text-2)">' + esc(label) + '</div>';
        html += '<div class="audit-args">' + esc(auditPretty(ev.args)) + '</div>';
    }
    if (ev.result_preview) {
        html += '<div style="margin-top:10px;font-size:11px;color:var(--text-2)">Result preview</div>';
        html += '<div class="audit-args">' + esc(ev.result_preview) + '</div>';
    }
    html += '</td></tr>';
    return html;
}

// restoreAuditFocus puts the caret back in the filter box after a re-render.
// Every keystroke rebuilds the table (filtering is local), which would
// otherwise blur the input on the first character typed.
function restoreAuditFocus() {
    const el = document.getElementById('auditText');
    if (!el || !state._auditTextFocused) return;
    el.focus();
    const n = el.value.length;
    try { el.setSelectionRange(n, n); } catch (e) { /* search inputs may refuse */ }
}

window.onAuditEvents = function(events, status) {
    state.auditEvents = events || [];
    state.auditStatus = status || null;
    state.auditLoaded = true;
    if (state.page === 'audit') render();
};

window.onAuditEvent = function(ev) {
    if (!ev) return;
    if (state.auditStatus) {
        state.auditStatus.recorded = (state.auditStatus.recorded || 0) + 1;
    }
    if (!state.auditFollow) return;
    state.auditEvents.unshift(ev);
    if (state.auditEvents.length > AUDIT_MAX_ROWS) state.auditEvents.length = AUDIT_MAX_ROWS;
    if (state.page !== 'audit') return;
    // Prepend surgically rather than re-rendering: a full repaint on every
    // inbound call would fight whatever the user is typing in the filter box.
    const tbody = document.getElementById('auditRows');
    if (!tbody || !auditMatches(ev)) { render(); return; }
    tbody.insertAdjacentHTML('afterbegin', renderAuditRow(ev));
    while (tbody.children.length > AUDIT_MAX_ROWS) tbody.removeChild(tbody.lastChild);
};

window.onAuditError = function(msg) {
    state.auditError = msg || 'audit error';
    if (state.page === 'audit') render();
};

window.onAuditExported = function(path) {
    state.auditExportPath = path;
    if (state.page === 'audit') render();
};

// Track focus on the filter input so restoreAuditFocus knows whether to
// reclaim it. Delegated at the document level because the input is destroyed
// and recreated on every render.
document.addEventListener('focusin', (e) => {
    if (e.target && e.target.id === 'auditText') state._auditTextFocused = true;
});
document.addEventListener('focusout', (e) => {
    if (e.target && e.target.id === 'auditText') state._auditTextFocused = false;
});

// setsEqual reports whether two Sets hold the same members.
function setsEqual(a, b) {
    if (a.size !== b.size) return false;
    for (const x of a) if (!b.has(x)) return false;
    return true;
}

window.onServiceStatusBatch = function(batch) {
    const next = {};
    for (const snap of (batch || [])) {
        next[snap.serviceId] = snap;
    }
    const prevIds = new Set(Object.keys(state.serviceStatuses));
    const nextIds = new Set(Object.keys(next));
    const changed = !setsEqual(prevIds, nextIds);
    state.serviceStatuses = next;
    if (changed) {
        // Drop cached config state for any service that deregistered, so when it
        // re-registers (e.g. after a save-triggered restart) its panel re-fetches
        // the file from disk instead of showing the pre-restart draft.
        // serviceConfigLoaded is otherwise never cleared.
        for (const id of prevIds) {
            if (nextIds.has(id)) continue;
            delete state.serviceConfigLoaded[id];
            delete state.serviceConfigTree[id];
            delete state.serviceConfigDraft[id];
        }
    }
    if (state.page !== 'inspector') return;
    if (changed) {
        // A service registered or deregistered: panels appear/disappear, so the
        // whole inspector must re-render. This rebuilds _cfgBind and drops any
        // open config editor — acceptable, since the changed panel is being
        // rebuilt anyway and any surviving service's edited draft is preserved.
        render();
        return;
    }
    // Steady state — same set of services. Update only each read-only status
    // region so an open config editor (and any focused input / uncommitted text
    // in it) is left completely untouched. This is the 2s-poll clobber fix.
    for (const id of nextIds) updateServiceStatusDOM(id, next[id]);
};

window.onServiceActionResult = function(result) {
    if (!result) return;
    const rowKey = canonRowKey(result.row);
    delete state.serviceActionPending[result.serviceId + '|' + result.actionId + '|' + rowKey];
    if (result.ok) {
        delete state.serviceActionError[result.serviceId];
    } else {
        state.serviceActionError[result.serviceId] = result.error || 'action failed';
    }
    // Refresh only this service's status region (clears the spinner / shows the
    // error), leaving an open config editor below it intact.
    if (state.page === 'inspector') updateServiceStatusDOM(result.serviceId, state.serviceStatuses[result.serviceId]);
};

render();

// Inline on* handlers in rendered HTML resolve against window. Bundling scopes
// these declarations to the module, so re-expose every top-level function (and
// the shared state object) on window — exactly the global surface the original
// classic <script> had.
Object.assign(window, {
    auditCaller, auditDetail, auditFmtTime, auditMatches, auditPretty, auditSelect, auditVisible, exportAudit, queryAudit, renderAudit, renderAuditDetail, renderAuditRow, restoreAuditFocus, revealAuditLog, setAuditFilter, toggleAuditFollow, toggleAuditRow,
    cancelEnrolment, dismissEnrolBundle, enrolBudgetText, enrolBytes, enrolGrantNames, enrolGrantSummary, newEnrolment, remoteDraft, remoteDraftSet, remoteGrantableProjects, remoteListenIsLoopback, removeRemoteConfig, renderEnrolBundleBanner, renderEnrolmentForm, renderEnrolments, renderRemoteListener, revokeEnrolment, saveEnrolment, saveRemoteConfig, toggleEnrolGrant,
    harvestProjectPermissions, mcpScopeFieldsFor, projAccessMode, projAllowedToolPatterns, projAllowedToolsText, projAuthorityRows, projFormAccessMode, projGrantedMcpIds, projMissingScopeFields, projNoun, projScopeGaps, projScopeText, projScopeValue, projToolAuthorityText, renderAuthorityRows, renderProjMcpPermissions, renderScopeFieldInput, renderScopeFieldPicker, renderScopeFieldTextInput, renderScopeChoices, renderScopeGapBanner, scopeTextFromValue, scopeValueFromText, scopeValueIsSet, scopeValueText, setProjAccess, setProjAllowedToolsText, setProjMcpGranted, setProjScopeText,
    captureProjectFormInputs, isPolicyEmpty, refreshDependentScopeFields, requestScopeEnum, retryScopeEnum, scopeDependencyValues, scopeEnumKey, scopeEnumValueKey, scopeFieldByName, scopeFieldIsOpen, scopeOpenKey, scopeSelectedValues, toggleProjScopeValueAt, toggleScopeFieldPicker, unrecognisedScopeValues,
    addExternalMcp, addExternalMcpFromJson, addExternalMcpHttp, addService, authenticateMcp, blankProjectForm, cancelMcpEdit, cancelProjectEdit, cancelServiceEdit, cfgArrayAdd, cfgArrayRemove, cfgBind, cfgChevron, cfgDirty, cfgEdit, cfgEditJson, cfgExpandKey, cfgFieldAt, cfgFirstMissingRequired, cfgGetDraft, cfgHasBadJson, cfgIsExpanded, cfgKvAdd, cfgKvRemove, cfgKvRename, cfgKvSetVal, cfgKvState, cfgMapAdd, cfgMapRemove, cfgMapRename, cfgNodeLabel, cfgRefreshChrome, cfgRerender, cfgSetExpanded, cfgToggleExpand, copyProjectToken, dispatchConfigOp, dispatchServiceAction, editProject, editService, harvestProjectForm, ipc, isAnyActionPending, isProjMcpWildcard, isProjModelsWildcard, isRemoteForm, isRemoteProject, newMcp, newProject, newService, projMcpState, projectFormFromExisting, pruneStaleDisabledTool, regenProjectSkill, removeExternalMcp, removeProject, removeService, render, renderActionButton, renderArrayBlock, renderConfigArray, renderConfigItem, renderConfigKeyValue, renderConfigLeaf, renderConfigMap, renderConfigNode, renderConfigObject, renderConfigSection, renderMcpForm, renderMcpPush, renderMcpServers, renderObjectFields, renderProjToolPicker, renderProjectForm, renderProjects, renderServiceForm, renderServiceInspector, renderServicePanel, renderServiceStatus, renderServices, renderStatusPayload, resetMcpPermissions, revertConfig, rotateProjectToken, saveConfig, saveProjectForm, saveServiceEdit, serviceBadgeHTML, setMcpAddMode, setMcpTransport, setProjKind, setProjMcpState, setProjMcpWildcard, setProjModelsWildcard, setsEqual, showPage, svcFormValues, toggleConfigSection, toggleProjTool, toggleProjectTokenVisible, toggleServiceRunning, updateServiceAutostart, updateServiceStatusDOM});
window.state = state;
