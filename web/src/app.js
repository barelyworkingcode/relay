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
    editingProjectId: null,                 // null = list, 'new' = create form, '<id>' = edit
    projectForm: null,                      // in-flight form values (kept out of state.projects until Save)
    projectFormError: null,
    projectTokenVisible: {},                // id -> bool (eye toggle)
    projectFreshToken: {},                  // id -> plaintext shown once after rotate
    projectSkillRegen: {},                  // id -> { ok, message, t } (last regen result)
    projectError: null,
    rotatingProjectId: null,
};

function showPage(page) {
    state.page = page;
    const pages = ['services', 'mcps', 'projects', 'inspector'];
    document.querySelectorAll('.sidebar-item').forEach((el, i) => {
        el.classList.toggle('active', pages[i] === page);
    });
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

function renderProjects() {
    if (state.editingProjectId) return renderProjectForm();

    let html = '<div class="page-header">';
    html += '<h2>Projects</h2>';
    html += '<button class="btn btn-primary" onclick="newProject()">+ New Project</button>';
    html += '</div>';
    html += '<p class="page-intro">Projects are the security boundary: each gets a scoped bearer token, an allowed-MCP list, per-tool selection, and optional auto-generated SKILL.md.</p>';

    if (state.projectError) {
        html += '<div class="proj-error">' + esc(state.projectError) + '</div>';
    }

    if (state.projects.length === 0) {
        html += '<div class="empty-state">No projects yet. Click <strong>+ New Project</strong> to create one.</div>';
    } else {
        for (const p of state.projects) {
            const allowedCount = p.allowed_mcp_ids && p.allowed_mcp_ids.length > 0
                ? (p.allowed_mcp_ids[0] === PROJ_MCP_WILDCARD ? 'all' : String(p.allowed_mcp_ids.length))
                : '0';
            const modelsCount = p.allowed_models && p.allowed_models.length > 0
                ? (p.allowed_models[0] === PROJ_MCP_WILDCARD ? 'all' : String(p.allowed_models.length))
                : '0';
            const skillState = p.generate_skill ? 'auto' : 'off';
            const policy = (p.permission_policy && p.permission_policy.default_mode) || '—';
            const regen = state.projectSkillRegen[p.id];
            html += '<div class="proj-card">';
            html += '<div class="proj-card-header">';
            html += '<span class="proj-card-name">' + esc(p.name) + '</span>';
            html += '<div style="display:flex;gap:4px">';
            html += '<button class="btn btn-sm" onclick="editProject(\'' + esc(p.id) + '\')">Edit</button>';
            html += '<button class="btn btn-sm" onclick="regenProjectSkill(\'' + esc(p.id) + '\')" title="Regenerate SKILL.md now">Regen Skill</button>';
            html += '<button class="btn btn-sm btn-danger" onclick="removeProject(\'' + esc(p.id) + '\', \'' + esc(p.name) + '\')">Delete</button>';
            html += '</div></div>';
            html += '<div class="proj-card-path">' + esc(p.path || '(no path)') + '</div>';
            html += '<div class="proj-card-meta">';
            html += '<span>MCPs: <strong>' + esc(allowedCount) + '</strong></span>';
            html += '<span>Models: <strong>' + esc(modelsCount) + '</strong></span>';
            html += '<span>Policy: <strong>' + esc(policy) + '</strong></span>';
            html += '<span>Skill: <strong>' + esc(skillState) + '</strong></span>';
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

function blankProjectForm() {
    return {
        id: null,
        name: '',
        path: '',
        allowed_mcp_ids: [PROJ_MCP_WILDCARD],   // wildcard by default
        allowed_models: [PROJ_MCP_WILDCARD],
        chat_templates: [],
        permission_policy: { default_mode: '', allowed_tools: [], denied_tools: [] },
        generate_skill: false,
        disabled_tools: {},                      // mcpID -> [toolName, ...]
    };
}

function projectFormFromExisting(p) {
    // Deep-clone so edits don't mutate state.projects until Save.
    const policy = p.permission_policy || {};
    return {
        id: p.id,
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
        disabled_tools: JSON.parse(JSON.stringify(p.disabled_tools || {})),
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
    if (!confirm('Delete project "' + name + '"?\nThis revokes its token immediately and removes its SKILL.md.')) return;
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
    const isNew = !f.id;
    const title = isNew ? 'New Project' : 'Edit Project';

    let html = '<h2>' + esc(title) + '</h2>';
    if (state.projectFormError) {
        html += '<div class="proj-error">' + esc(state.projectFormError) + '</div>';
    }

    // ---- Identity ----
    html += '<div class="proj-section">';
    html += '<div class="proj-section-title">Identity</div>';
    html += '<label>Project name</label>';
    html += '<input type="text" id="projName" value="' + esc(f.name) + '" placeholder="e.g. Acme Website" />';
    html += '<label>Project path</label>';
    html += '<input type="text" id="projPath" value="' + esc(f.path) + '" placeholder="/Users/you/projects/acme" />';
    html += '<p class="proj-section-help">Absolute path. Filesystem MCPs are auto-scoped to this directory.</p>';
    html += '</div>';

    // ---- Allowed MCPs + tri-state picker ----
    const wild = isProjMcpWildcard(f);
    html += '<div class="proj-section">';
    html += '<div class="proj-section-title">Allowed MCPs &amp; Tools</div>';
    html += '<div class="toggle-row" style="padding:4px 0;margin:0">';
    html += '<span>Allow all registered MCPs (wildcard <code>*</code>)</span>';
    html += '<label class="switch"><input type="checkbox" ' + (wild ? 'checked' : '') + ' onchange="setProjMcpWildcard(this.checked)" /><span class="slider"></span></label>';
    html += '</div>';

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
            html += '<div class="proj-mcp-row">';
            html += '<span class="proj-mcp-name">' + esc(mcp.display_name || mcp.id) + ' <span style="color:var(--text-3);font-size:11px">(' + esc(mcp.id) + ')</span></span>';
            html += '<div class="perm-btns">';
            html += '<button class="perm-btn ' + (st === 'all' ? 'active' : '') + '" onclick="setProjMcpState(\'' + esc(mcp.id) + '\', \'all\')">All tools</button>';
            html += '<button class="perm-btn ' + (st === 'selected' ? 'active' : '') + '" onclick="setProjMcpState(\'' + esc(mcp.id) + '\', \'selected\')">Selected</button>';
            html += '<button class="perm-btn ' + (st === 'none' ? 'active' : '') + '" onclick="setProjMcpState(\'' + esc(mcp.id) + '\', \'none\')">No tools</button>';
            html += '</div>';
            html += '</div>';
            if (st === 'selected') {
                html += renderProjToolPicker(mcp.id, f);
            }
        }
        for (const id of dangling) {
            html += '<div class="proj-mcp-row">';
            html += '<span class="proj-mcp-name dangling">' + esc(id) + ' (no longer registered)</span>';
            html += '<button class="perm-btn" onclick="setProjMcpState(\'' + esc(id) + '\', \'none\')">Remove</button>';
            html += '</div>';
        }
    }
    html += '</div>';

    // ---- Allowed models ----
    const modelsWild = isProjModelsWildcard(f);
    html += '<div class="proj-section">';
    html += '<div class="proj-section-title">Allowed Models</div>';
    html += '<div class="toggle-row" style="padding:4px 0;margin:0">';
    html += '<span>Allow all models (wildcard <code>*</code>)</span>';
    html += '<label class="switch"><input type="checkbox" ' + (modelsWild ? 'checked' : '') + ' onchange="setProjModelsWildcard(this.checked)" /><span class="slider"></span></label>';
    html += '</div>';
    if (!modelsWild) {
        const csv = f.allowed_models.filter(m => m !== PROJ_MCP_WILDCARD).join(', ');
        html += '<label>Model IDs (comma-separated)</label>';
        html += '<input type="text" id="projModels" value="' + esc(csv) + '" placeholder="claude-opus, claude-sonnet, gpt-4" />';
    }
    html += '</div>';

    // ---- Chat templates (read-only; relay stores them, Eve edits them) ----
    html += '<div class="proj-section">';
    html += '<div class="proj-section-title">Chat Templates</div>';
    html += '<p class="proj-section-help">Project-scoped chat presets stored with the project. Create and edit them in Eve\'s project dialog, which offers live model selection.</p>';
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

    // ---- Permission policy ----
    const pol = f.permission_policy;
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

    // ---- Skill ----
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

function harvestProjectForm() {
    const f = state.projectForm;
    if (!f) return null;
    const name = (document.getElementById('projName') || {}).value || f.name;
    const path = (document.getElementById('projPath') || {}).value || f.path;
    let allowedModels = f.allowed_models;
    if (!isProjModelsWildcard(f)) {
        const csv = (document.getElementById('projModels') || {}).value || '';
        allowedModels = csv.split(',').map(s => s.trim()).filter(Boolean);
    }
    const allowedToolsTA = document.getElementById('projAllowedTools');
    const deniedToolsTA = document.getElementById('projDeniedTools');
    const policy = {
        default_mode: f.permission_policy.default_mode,
        allowed_tools: allowedToolsTA ? allowedToolsTA.value.split('\n').map(s => s.trim()).filter(Boolean) : f.permission_policy.allowed_tools,
        denied_tools: deniedToolsTA ? deniedToolsTA.value.split('\n').map(s => s.trim()).filter(Boolean) : f.permission_policy.denied_tools,
    };
    // chat_templates is intentionally absent: the form is read-only for
    // templates (Eve owns editing), and omitting the field makes
    // update_project leave the stored list untouched.
    return {
        name: name.trim(),
        path: path.trim(),
        allowed_mcp_ids: f.allowed_mcp_ids,
        allowed_models: allowedModels,
        permission_policy: policy,
        generate_skill: f.generate_skill,
        disabled_tools: f.disabled_tools,
    };
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
    if (!payload.path) {
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

window.onProjectError = function(msg) {
    state.projectError = msg;
    state.projectFormError = msg;
    if (state.page === 'projects') render('push');
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
    addExternalMcp, addExternalMcpFromJson, addExternalMcpHttp, addService, authenticateMcp, blankProjectForm, cancelMcpEdit, cancelProjectEdit, cancelServiceEdit, cfgArrayAdd, cfgArrayRemove, cfgBind, cfgChevron, cfgDirty, cfgEdit, cfgEditJson, cfgExpandKey, cfgFieldAt, cfgFirstMissingRequired, cfgGetDraft, cfgHasBadJson, cfgIsExpanded, cfgKvAdd, cfgKvRemove, cfgKvRename, cfgKvSetVal, cfgKvState, cfgMapAdd, cfgMapRemove, cfgMapRename, cfgNodeLabel, cfgRefreshChrome, cfgRerender, cfgSetExpanded, cfgToggleExpand, copyProjectToken, dispatchConfigOp, dispatchServiceAction, editProject, editService, harvestProjectForm, ipc, isAnyActionPending, isProjMcpWildcard, isProjModelsWildcard, newMcp, newProject, newService, projMcpState, projectFormFromExisting, pruneStaleDisabledTool, regenProjectSkill, removeExternalMcp, removeProject, removeService, render, renderActionButton, renderArrayBlock, renderConfigArray, renderConfigItem, renderConfigKeyValue, renderConfigLeaf, renderConfigMap, renderConfigNode, renderConfigObject, renderConfigSection, renderMcpForm, renderMcpPush, renderMcpServers, renderObjectFields, renderProjToolPicker, renderProjectForm, renderProjects, renderServiceForm, renderServiceInspector, renderServicePanel, renderServiceStatus, renderServices, renderStatusPayload, resetMcpPermissions, revertConfig, rotateProjectToken, saveConfig, saveProjectForm, saveServiceEdit, serviceBadgeHTML, setMcpAddMode, setMcpTransport, setProjMcpState, setProjMcpWildcard, setProjModelsWildcard, setsEqual, showPage, svcFormValues, toggleConfigSection, toggleProjTool, toggleProjectTokenVisible, toggleServiceRunning, updateServiceAutostart, updateServiceStatusDOM
});
window.state = state;
