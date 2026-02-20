package main

import (
	"encoding/json"
	"os"
)

// renderSettingsHTML generates the complete HTML for the settings webview.
// It embeds the current settings as JavaScript variables.
func renderSettingsHTML(settings *Settings) string {
	tokensJSON, _ := json.Marshal(settings.Tokens)
	externalMcpsJSON, _ := json.Marshal(settings.ExternalMcps)
	userServicesJSON, _ := json.Marshal(settings.Services)

	exePath, err := os.Executable()
	if err != nil {
		exePath = ""
	}
	exeJSON, _ := json.Marshal(exePath)

	return `<!DOCTYPE html>
<html>
<head><style>
* { margin: 0; padding: 0; box-sizing: border-box; }
body {
    font-family: -apple-system, BlinkMacSystemFont, sans-serif;
    background: #1e1e1e;
    color: #e0e0e0;
    display: flex;
    height: 100vh;
    overflow: hidden;
}
.sidebar {
    width: 200px;
    background: #181818;
    border-right: 1px solid #333;
    padding: 16px 0;
    flex-shrink: 0;
}
.sidebar-item {
    padding: 10px 20px;
    cursor: pointer;
    font-size: 13px;
    color: #aaa;
    border-left: 3px solid transparent;
}
.sidebar-item:hover { background: #252525; color: #e0e0e0; }
.sidebar-item.active {
    background: #252525;
    color: #fff;
    border-left-color: #0078d4;
}
.content {
    flex: 1;
    padding: 24px 32px;
    overflow-y: auto;
}
h2 { font-size: 18px; font-weight: 600; margin-bottom: 20px; }
h3 { font-size: 14px; font-weight: 600; margin-bottom: 12px; color: #ccc; }
label {
    display: block;
    margin-top: 12px;
    font-size: 13px;
    color: #999;
}
input[type="text"] {
    width: 100%;
    padding: 6px 8px;
    margin-top: 4px;
    background: #2d2d2d;
    border: 1px solid #444;
    border-radius: 4px;
    color: #e0e0e0;
    font-size: 13px;
}
input[type="text"]:focus { outline: none; border-color: #0078d4; }
.btn {
    padding: 6px 16px;
    background: #0078d4;
    color: white;
    border: none;
    border-radius: 4px;
    font-size: 13px;
    cursor: pointer;
}
.btn:hover { background: #006cbd; }
.btn-sm { padding: 4px 10px; font-size: 12px; }
.btn-danger { background: transparent; color: #e55; border: 1px solid #e55; }
.btn-danger:hover { background: #e55; color: #fff; }
.link-danger {
    color: #e55;
    font-size: 12px;
    cursor: pointer;
    text-decoration: underline;
    background: none;
    border: none;
}
.link-danger:hover { color: #f77; }
.token-banner {
    background: #1a2e1a;
    border: 1px solid #2a5a2a;
    border-radius: 6px;
    padding: 12px 16px;
    margin: 12px 0;
}
.token-banner code {
    display: block;
    background: #111;
    padding: 8px;
    border-radius: 4px;
    font-size: 12px;
    word-break: break-all;
    margin: 8px 0;
    color: #8f8;
    font-family: 'SF Mono', Monaco, monospace;
}
.token-banner .warning {
    color: #e55;
    font-size: 11px;
    margin-top: 6px;
}
.token-list {
    margin-top: 16px;
}
.token-row {
    display: flex;
    align-items: center;
    padding: 10px 12px;
    border: 1px solid #333;
    border-radius: 4px;
    margin-bottom: 6px;
    cursor: pointer;
    font-size: 13px;
}
.token-row:hover { border-color: #555; }
.token-row.selected { border-color: #0078d4; background: #1a2a3a; }
.token-name { flex: 1; font-weight: 500; }
.token-hash { color: #888; font-family: 'SF Mono', Monaco, monospace; font-size: 11px; margin: 0 12px; }
.token-date { color: #666; font-size: 11px; }
.perms-section { margin-top: 20px; }
.perm-row {
    display: flex;
    align-items: center;
    padding: 8px 0;
    border-bottom: 1px solid #2a2a2a;
    font-size: 13px;
}
.perm-row:last-child { border-bottom: none; }
.perm-name { flex: 1; text-transform: capitalize; display: flex; align-items: center; gap: 10px; }
.perm-name.set-all { font-weight: 600; color: #aaa; }
.svc-icon {
    width: 24px;
    height: 24px;
    border-radius: 6px;
    display: flex;
    align-items: center;
    justify-content: center;
    flex-shrink: 0;
}
.perm-btns { display: flex; gap: 4px; }
.perm-btn {
    padding: 3px 10px;
    font-size: 11px;
    border: 1px solid #444;
    border-radius: 3px;
    background: #2d2d2d;
    color: #aaa;
    cursor: pointer;
}
.perm-btn:hover { border-color: #666; color: #e0e0e0; }
.perm-btn.active { background: #0078d4; border-color: #0078d4; color: #fff; }
.gen-row {
    display: flex;
    gap: 8px;
    margin-bottom: 8px;
}
.gen-row input { flex: 1; }
.banner-btns {
    display: flex;
    gap: 8px;
    margin-top: 4px;
}
textarea {
    width: 100%;
    padding: 6px 8px;
    margin-top: 4px;
    background: #2d2d2d;
    border: 1px solid #444;
    border-radius: 4px;
    color: #e0e0e0;
    font-size: 13px;
    font-family: 'SF Mono', Monaco, monospace;
    resize: vertical;
    min-height: 60px;
}
textarea:focus { outline: none; border-color: #0078d4; }
.mcp-card {
    border: 1px solid #333;
    border-radius: 6px;
    padding: 12px 16px;
    margin-bottom: 8px;
}
.mcp-card-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 4px;
}
.mcp-card-name { font-weight: 500; font-size: 13px; }
.mcp-card-cmd { color: #888; font-size: 11px; font-family: 'SF Mono', Monaco, monospace; }
.mcp-card-tools { color: #666; font-size: 11px; margin-top: 2px; }
.error-msg { color: #e55; font-size: 12px; margin-top: 8px; }
.spinner { display: inline-block; width: 14px; height: 14px; border: 2px solid #444; border-top-color: #0078d4; border-radius: 50%; animation: spin 0.6s linear infinite; vertical-align: -2px; margin-right: 6px; }
@keyframes spin { to { transform: rotate(360deg); } }
.empty-state {
    color: #666;
    font-size: 13px;
    padding: 24px 0;
    text-align: center;
}
.toggle-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 10px 0;
    margin-bottom: 8px;
    font-size: 13px;
}
.switch {
    position: relative;
    width: 36px;
    height: 20px;
    flex-shrink: 0;
}
.switch input {
    opacity: 0;
    width: 0;
    height: 0;
}
.switch .slider {
    position: absolute;
    inset: 0;
    background: #444;
    border-radius: 10px;
    cursor: pointer;
    transition: background 0.2s;
}
.switch .slider::before {
    content: '';
    position: absolute;
    width: 14px;
    height: 14px;
    left: 3px;
    bottom: 3px;
    background: #fff;
    border-radius: 50%;
    transition: transform 0.2s;
}
.switch input:checked + .slider {
    background: #0078d4;
}
.switch input:checked + .slider::before {
    transform: translateX(16px);
}
.switch input:disabled + .slider {
    opacity: 0.4;
    cursor: default;
}
.tool-count {
    color: #666;
    font-size: 11px;
    cursor: pointer;
    display: inline-flex;
    align-items: center;
    gap: 4px;
    margin-left: 8px;
}
.tool-count:hover { color: #999; }
.chevron {
    display: inline-block;
    transition: transform 0.2s;
    font-size: 10px;
}
.chevron.open { transform: rotate(90deg); }
.tool-expansion {
    padding: 8px 0 8px 34px;
}
.tool-actions {
    display: flex;
    gap: 8px;
    margin-bottom: 8px;
}
.tool-actions button {
    padding: 2px 8px;
    font-size: 11px;
    border: 1px solid #444;
    border-radius: 3px;
    background: #2d2d2d;
    color: #aaa;
    cursor: pointer;
}
.tool-actions button:hover { border-color: #666; color: #e0e0e0; }
.tool-row {
    display: flex;
    align-items: center;
    padding: 4px 0;
    font-size: 12px;
}
.tool-row .tool-name { font-weight: 500; min-width: 150px; }
.tool-row .tool-desc { color: #666; flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; margin: 0 12px; }
.cat-header {
    display: flex;
    align-items: center;
    padding: 8px 0 4px 0;
    font-size: 13px;
    font-weight: 600;
    color: #ccc;
    gap: 8px;
    border-bottom: 1px solid #2a2a2a;
    margin-top: 4px;
}
.cat-header .cat-icon {
    width: 24px;
    height: 24px;
    border-radius: 6px;
    display: flex;
    align-items: center;
    justify-content: center;
    flex-shrink: 0;
}
.cat-header .cat-name { flex: 1; }
.cat-header .cat-count { color: #666; font-size: 11px; font-weight: 400; }
.cat-header .cat-actions { display: flex; gap: 4px; }
.cat-header .cat-actions button {
    padding: 2px 8px;
    font-size: 11px;
    border: 1px solid #444;
    border-radius: 3px;
    background: #2d2d2d;
    color: #aaa;
    cursor: pointer;
}
.cat-header .cat-actions button:hover { border-color: #666; color: #e0e0e0; }
</style></head>
<body>
<div class="sidebar">
    <div class="sidebar-item active" onclick="showPage('services')"><svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="vertical-align:-2px;margin-right:6px"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83-2.83l.06-.06A1.65 1.65 0 0 0 4.68 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 2.83-2.83l.06.06A1.65 1.65 0 0 0 9 4.68a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 2.83l-.06.06A1.65 1.65 0 0 0 19.4 9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg>Services</div>
    <div class="sidebar-item" onclick="showPage('mcps')"><svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="vertical-align:-2px;margin-right:6px"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><polyline points="14 2 14 8 20 8"/><line x1="12" y1="18" x2="12" y2="12"/><line x1="9" y1="15" x2="15" y2="15"/></svg>MCP Servers</div>
    <div class="sidebar-item" onclick="showPage('security')"><svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="vertical-align:-2px;margin-right:6px"><path d="M12 2L3 7v5c0 5.55 3.84 10.74 9 12 5.16-1.26 9-6.45 9-12V7l-9-5z"/></svg>Security</div>
</div>
<div class="content" id="content"></div>

<script>
function ipc(msg) {
    if (window.webkit && window.webkit.messageHandlers && window.webkit.messageHandlers.ipc)
        window.webkit.messageHandlers.ipc.postMessage(msg);
    else if (window.chrome && window.chrome.webview)
        window.chrome.webview.postMessage(msg);
}
const EXE_PATH = ` + string(exeJSON) + `;
const EXTERNAL_MCPS_INIT = ` + string(externalMcpsJSON) + `;
const SERVICES_INIT = ` + string(userServicesJSON) + `;

const CATEGORY_ICONS = {
    Calendar:  { color: '#E8453C', svg: '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="4" width="18" height="18" rx="2" ry="2"/><line x1="16" y1="2" x2="16" y2="6"/><line x1="8" y1="2" x2="8" y2="6"/><line x1="3" y1="10" x2="21" y2="10"/></svg>' },
    Capture:   { color: '#6B7280', svg: '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M23 19a2 2 0 0 1-2 2H3a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h4l2-3h6l2 3h4a2 2 0 0 1 2 2z"/><circle cx="12" cy="13" r="4"/></svg>' },
    Contacts:  { color: '#A0855B', svg: '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2"/><circle cx="12" cy="7" r="4"/></svg>' },
    Location:  { color: '#3B82F6', svg: '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 10c0 7-9 13-9 13s-9-6-9-13a9 9 0 0 1 18 0z"/><circle cx="12" cy="10" r="3"/></svg>' },
    Mail:      { color: '#3B82F6', svg: '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M4 4h16c1.1 0 2 .9 2 2v12c0 1.1-.9 2-2 2H4c-1.1 0-2-.9-2-2V6c0-1.1.9-2 2-2z"/><polyline points="22,6 12,13 2,6"/></svg>' },
    Maps:      { color: '#34D399', svg: '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polygon points="1 6 1 22 8 18 16 22 23 18 23 2 16 6 8 2 1 6"/><line x1="8" y1="2" x2="8" y2="18"/><line x1="16" y1="6" x2="16" y2="22"/></svg>' },
    Messages:  { color: '#34D399', svg: '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/></svg>' },
    Reminders: { color: '#F59E0B', svg: '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M22 11.08V12a10 10 0 1 1-5.93-9.14"/><polyline points="22 4 12 14.01 9 11.01"/></svg>' },
    Shortcuts: { color: '#8B5CF6', svg: '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polygon points="13 2 3 14 12 14 11 22 21 10 12 10 13 2"/></svg>' },
    Utilities: { color: '#6B7280', svg: '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83-2.83l.06-.06A1.65 1.65 0 0 0 4.68 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 2.83-2.83l.06.06A1.65 1.65 0 0 0 9 4.68a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 2.83l-.06.06A1.65 1.65 0 0 0 19.4 9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg>' },
    Weather:   { color: '#F59E0B', svg: '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="5"/><line x1="12" y1="1" x2="12" y2="3"/><line x1="12" y1="21" x2="12" y2="23"/><line x1="4.22" y1="4.22" x2="5.64" y2="5.64"/><line x1="18.36" y1="18.36" x2="19.78" y2="19.78"/><line x1="1" y1="12" x2="3" y2="12"/><line x1="21" y1="12" x2="23" y2="12"/><line x1="4.22" y1="19.78" x2="5.64" y2="18.36"/><line x1="18.36" y1="5.64" x2="19.78" y2="4.22"/></svg>' },
};
const DEFAULT_CAT_ICON = { color: '#6B7280', svg: '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><polyline points="14 2 14 8 20 8"/><line x1="12" y1="18" x2="12" y2="12"/><line x1="9" y1="15" x2="15" y2="15"/></svg>' };

function getCatIcon(category) {
    return CATEGORY_ICONS[category] || DEFAULT_CAT_ICON;
}

let state = {
    page: 'services',
    tokens: ` + string(tokensJSON) + `,
    selectedHash: null,
    newToken: null,
    externalMcps: EXTERNAL_MCPS_INIT,
    discovering: false,
    discoveryError: null,
    mcpAddMode: 'form',
    services: SERVICES_INIT,
    editingServiceId: null,
    expandedMcps: {},
};

function showPage(page) {
    state.page = page;
    state.newToken = null;
    const pages = ['services', 'mcps', 'security'];
    document.querySelectorAll('.sidebar-item').forEach((el, i) => {
        el.classList.toggle('active', pages[i] === page);
    });
    render();
}

const JSON_PLACEHOLDER = JSON.stringify({"my-server": {"command": "npx", "args": ["-y", "@example/server"], "env": {"API_KEY": "..."}}}, null, 2);

function render() {
    const el = document.getElementById('content');
    if (state.page === 'services') {
        el.innerHTML = renderServices();
    } else if (state.page === 'mcps') {
        el.innerHTML = renderMcpServers();
        const ta = document.getElementById('mcpJson');
        if (ta) ta.placeholder = JSON_PLACEHOLDER;
    } else {
        el.innerHTML = renderSecurity();
    }
}

function renderMcpServers() {
    let html = '<h2>MCP Servers</h2>';
    html += '<p style="color:#888;font-size:12px;margin-bottom:16px">Add external MCP servers so clients only need to connect to Relay.</p>';

    if (state.externalMcps.length > 0) {
        for (const mcp of state.externalMcps) {
            const toolCount = (mcp.discovered_tools || []).length;
            const cmdDisplay = mcp.command.length > 40 ? '...' + mcp.command.slice(-37) : mcp.command;
            const argsDisplay = mcp.args && mcp.args.length > 0 ? ' ' + mcp.args.join(' ') : '';
            html += ` + "`" + `<div class="mcp-card">
                <div class="mcp-card-header">
                    <span class="mcp-card-name">${esc(mcp.display_name)}</span>
                    <button class="btn btn-sm btn-danger" onclick="removeExternalMcp('${esc(mcp.id)}')">Remove</button>
                </div>
                <div class="mcp-card-cmd">${esc(cmdDisplay + argsDisplay)}</div>
                <div class="mcp-card-tools">${toolCount} tool${toolCount !== 1 ? 's' : ''}</div>
            </div>` + "`" + `;
        }
    } else {
        html += '<div class="empty-state">No external MCP servers configured.</div>';
    }

    html += '<div style="margin-top:20px;border-top:1px solid #333;padding-top:16px">';
    html += '<h3>Add MCP Server</h3>';

    const formActive = state.mcpAddMode === 'form';
    html += ` + "`" + `<div style="display:flex;gap:4px;margin-bottom:12px">
        <button class="perm-btn ${formActive ? 'active' : ''}" onclick="setMcpAddMode('form')">Form</button>
        <button class="perm-btn ${!formActive ? 'active' : ''}" onclick="setMcpAddMode('json')">Paste JSON</button>
    </div>` + "`" + `;

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
        html += '<p style="color:#666;font-size:11px;margin-top:4px">Accepts <code style="color:#aaa">&lbrace; "name": &lbrace; "command", "args", "env" &rbrace; &rbrace;</code></p>';
    }

    html += '<div style="margin-top:12px">';
    if (state.discovering) {
        html += '<button class="btn" disabled><span class="spinner"></span>Discovering...</button>';
    } else {
        html += ` + "`" + `<button class="btn" onclick="${formActive ? 'addExternalMcp()' : 'addExternalMcpFromJson()'}">Add</button>` + "`" + `;
    }
    html += '</div>';

    if (state.discoveryError) {
        html += ` + "`" + `<div class="error-msg">${esc(state.discoveryError)}</div>` + "`" + `;
    }

    html += '</div>';
    return html;
}

function renderSecurity() {
    let html = '<div style="display:flex;align-items:center;justify-content:space-between"><h2>Auth Tokens</h2>';
    if (state.tokens.length > 0) {
        html += '<button class="link-danger" onclick="revokeAll()">Revoke All</button>';
    }
    html += '</div>';

    html += ` + "`" + `<div class="gen-row">
        <input type="text" id="tokenName" placeholder="Token name (e.g. Claude Desktop)" />
        <button class="btn" onclick="generateToken()">Generate</button>
    </div>` + "`" + `;

    if (state.newToken) {
        const mcpConfig = JSON.stringify({
            relay: {
                command: EXE_PATH,
                args: ["mcp", "--token", state.newToken.plaintext]
            }
        }, null, 2);
        html += ` + "`" + `<div class="token-banner">
            <strong>Token generated</strong>
            <code id="tokenPlaintext">${esc(state.newToken.plaintext)}</code>
            <div class="banner-btns">
                <button class="btn btn-sm" onclick="copyText('${esc(state.newToken.plaintext)}')">Copy Token</button>
                <button class="btn btn-sm" onclick="copyMcpConfig()">Copy MCP Config</button>
            </div>
            <div class="warning">Copy this token now. It will not be shown again.</div>
            <pre id="mcpConfigData" style="display:none">${esc(mcpConfig)}</pre>
        </div>` + "`" + `;
    }

    if (state.tokens.length === 0) {
        html += '<div class="empty-state">No tokens configured. All bridge access is blocked until a token is generated.</div>';
    } else {
        html += '<div class="token-list">';
        for (const t of state.tokens) {
            const sel = state.selectedHash === t.hash ? 'selected' : '';
            const date = t.created_at ? t.created_at.split('T')[0] : '';
            html += ` + "`" + `<div class="token-row ${sel}" onclick="selectToken('${t.hash}')">
                <span class="token-name">${esc(t.name || 'Unnamed')}</span>
                <span class="token-hash">${t.prefix}...${t.suffix}</span>
                <span class="token-date">${date}</span>
            </div>` + "`" + `;
        }
        html += '</div>';

        const selected = state.tokens.find(t => t.hash === state.selectedHash);
        if (selected) {
            html += renderPermissions(selected);
        }
    }

    return html;
}

function renderPermissions(token) {
    let html = ` + "`" + `<div class="perms-section">
        <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:12px">
            <h3 style="margin:0">Permissions for ${esc(token.name || 'Unnamed')}</h3>
            <button class="btn btn-sm btn-danger" onclick="deleteToken('${token.hash}')">Revoke</button>
        </div>` + "`" + `;

    html += ` + "`" + `<div class="perm-row">
        <span class="perm-name set-all">Set All</span>
        <div class="perm-btns">
            <button class="perm-btn" onclick="setAllPerms('${token.hash}','off')">Off</button>
            <button class="perm-btn" onclick="setAllPerms('${token.hash}','on')">On</button>
        </div>
    </div>` + "`" + `;

    for (const mcp of state.externalMcps) {
        const rawPerm = (token.permissions && token.permissions[mcp.id]) || 'on';
        const perm = rawPerm === 'off' ? 'off' : 'on';
        const plugIcon = ` + "`" + `<div class="svc-icon" style="background:#6B7280"><svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><polyline points="14 2 14 8 20 8"/><line x1="12" y1="18" x2="12" y2="12"/><line x1="9" y1="15" x2="15" y2="15"/></svg></div>` + "`" + `;
        const tools = mcp.discovered_tools || [];
        const expanded = state.expandedMcps[mcp.id];
        const chevronCls = expanded ? 'chevron open' : 'chevron';
        const toolCountLabel = tools.length > 0 ? ` + "`" + `<span class="tool-count" onclick="event.stopPropagation();toggleMcpExpand('${esc(mcp.id)}')"><span class="${chevronCls}">&#9654;</span>${tools.length} tool${tools.length !== 1 ? 's' : ''}</span>` + "`" + ` : '';
        html += ` + "`" + `<div class="perm-row">
            <span class="perm-name">${plugIcon}${esc(mcp.display_name)}${toolCountLabel}</span>
            <div class="perm-btns">
                <button class="perm-btn ${perm === 'off' ? 'active' : ''}" onclick="setPerm('${token.hash}','${esc(mcp.id)}','off')">Off</button>
                <button class="perm-btn ${perm === 'on' ? 'active' : ''}" onclick="setPerm('${token.hash}','${esc(mcp.id)}','on')">On</button>
            </div>
        </div>` + "`" + `;

        if (expanded && tools.length > 0) {
            const isOff = perm === 'off';
            const disabledList = (token.disabled_tools && token.disabled_tools[mcp.id]) || [];

            // Group tools by category
            const groups = {};
            for (const tool of tools) {
                const cat = tool.category || 'Other';
                if (!groups[cat]) groups[cat] = [];
                groups[cat].push(tool);
            }
            const catOrder = Object.keys(groups).sort((a, b) => a === 'Other' ? 1 : b === 'Other' ? -1 : a.localeCompare(b));

            html += '<div class="tool-expansion">';
            html += ` + "`" + `<div class="tool-actions">
                <button onclick="setAllTools('${token.hash}','${esc(mcp.id)}',false)" ${isOff ? 'disabled style="opacity:0.4;cursor:default"' : ''}>Enable All</button>
                <button onclick="setAllTools('${token.hash}','${esc(mcp.id)}',true)" ${isOff ? 'disabled style="opacity:0.4;cursor:default"' : ''}>Disable All</button>
            </div>` + "`" + `;

            for (const cat of catOrder) {
                const catTools = groups[cat];
                const catInfo = getCatIcon(cat);
                html += ` + "`" + `<div class="cat-header">
                    <div class="cat-icon" style="background:${catInfo.color}">${catInfo.svg}</div>
                    <span class="cat-name">${esc(cat)}</span>
                    <span class="cat-count">${catTools.length} tool${catTools.length !== 1 ? 's' : ''}</span>
                    <div class="cat-actions">
                        <button onclick="setAllToolsInCategory('${token.hash}','${esc(mcp.id)}','${esc(cat)}',false)" ${isOff ? 'disabled style="opacity:0.4;cursor:default"' : ''}>Enable</button>
                        <button onclick="setAllToolsInCategory('${token.hash}','${esc(mcp.id)}','${esc(cat)}',true)" ${isOff ? 'disabled style="opacity:0.4;cursor:default"' : ''}>Disable</button>
                    </div>
                </div>` + "`" + `;
                for (const tool of catTools) {
                    const isDisabled = disabledList.indexOf(tool.name) !== -1;
                    const desc = tool.description ? (tool.description.length > 60 ? tool.description.slice(0, 57) + '...' : tool.description) : '';
                    html += ` + "`" + `<div class="tool-row">
                        <span class="tool-name">${esc(tool.name)}</span>
                        <span class="tool-desc" title="${esc(tool.description || '')}">${esc(desc)}</span>
                        <label class="switch">
                            <input type="checkbox" ${!isDisabled ? 'checked' : ''} ${isOff ? 'disabled' : ''} onchange="setToolDisabled('${token.hash}','${esc(mcp.id)}','${esc(tool.name)}',!this.checked)" />
                            <span class="slider"></span>
                        </label>
                    </div>` + "`" + `;
                }
            }
            html += '</div>';
        }
    }

    html += '</div>';
    return html;
}

function esc(s) {
    return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;').replace(/'/g,'&#39;');
}

function generateToken() {
    const name = document.getElementById('tokenName').value.trim();
    if (!name) return;
    ipc(JSON.stringify({ type: 'generate_token', name }));
}

function deleteToken(hash) {
    ipc(JSON.stringify({ type: 'delete_token', hash }));
}

function revokeAll() {
    ipc(JSON.stringify({ type: 'revoke_all' }));
}

function setPerm(hash, service, permission) {
    const token = state.tokens.find(t => t.hash === hash);
    if (token) {
        if (!token.permissions) token.permissions = {};
        token.permissions[service] = permission;
    }
    ipc(JSON.stringify({ type: 'update_permission', hash, service, permission }));
    render();
}

function setAllPerms(hash, permission) {
    const token = state.tokens.find(t => t.hash === hash);
    if (token) {
        if (!token.permissions) token.permissions = {};
        for (const mcp of state.externalMcps) {
            token.permissions[mcp.id] = permission;
            ipc(JSON.stringify({ type: 'update_permission', hash, service: mcp.id, permission }));
        }
    }
    render();
}

function toggleMcpExpand(mcpId) {
    state.expandedMcps[mcpId] = !state.expandedMcps[mcpId];
    render();
}

function setToolDisabled(hash, mcpId, toolName, disabled) {
    const token = state.tokens.find(t => t.hash === hash);
    if (token) {
        if (!token.disabled_tools) token.disabled_tools = {};
        if (!token.disabled_tools[mcpId]) token.disabled_tools[mcpId] = [];
        if (disabled) {
            if (token.disabled_tools[mcpId].indexOf(toolName) === -1) {
                token.disabled_tools[mcpId].push(toolName);
            }
        } else {
            token.disabled_tools[mcpId] = token.disabled_tools[mcpId].filter(n => n !== toolName);
            if (token.disabled_tools[mcpId].length === 0) delete token.disabled_tools[mcpId];
        }
    }
    ipc(JSON.stringify({ type: 'set_tool_disabled', hash, mcp_id: mcpId, tool_name: toolName, disabled }));
    render();
}

function setAllTools(hash, mcpId, disabled) {
    const token = state.tokens.find(t => t.hash === hash);
    const mcp = state.externalMcps.find(m => m.id === mcpId);
    if (token && mcp) {
        const tools = mcp.discovered_tools || [];
        if (!token.disabled_tools) token.disabled_tools = {};
        if (disabled) {
            token.disabled_tools[mcpId] = tools.map(t => t.name);
        } else {
            delete token.disabled_tools[mcpId];
        }
    }
    ipc(JSON.stringify({ type: 'set_all_tools_disabled', hash, mcp_id: mcpId, disabled }));
    render();
}

function setAllToolsInCategory(hash, mcpId, category, disabled) {
    const token = state.tokens.find(t => t.hash === hash);
    const mcp = state.externalMcps.find(m => m.id === mcpId);
    if (token && mcp) {
        const tools = (mcp.discovered_tools || []).filter(t => (t.category || 'Other') === category);
        if (!token.disabled_tools) token.disabled_tools = {};
        if (!token.disabled_tools[mcpId]) token.disabled_tools[mcpId] = [];
        for (const tool of tools) {
            const idx = token.disabled_tools[mcpId].indexOf(tool.name);
            if (disabled && idx === -1) {
                token.disabled_tools[mcpId].push(tool.name);
            } else if (!disabled && idx !== -1) {
                token.disabled_tools[mcpId].splice(idx, 1);
            }
            ipc(JSON.stringify({ type: 'set_tool_disabled', hash, mcp_id: mcpId, tool_name: tool.name, disabled }));
        }
        if (token.disabled_tools[mcpId].length === 0) delete token.disabled_tools[mcpId];
    }
    render();
}

function selectToken(hash) {
    state.selectedHash = state.selectedHash === hash ? null : hash;
    render();
}

function copyText(text) {
    ipc(JSON.stringify({ type: 'copy_to_clipboard', text }));
}

function copyMcpConfig() {
    const el = document.getElementById('mcpConfigData');
    if (el) ipc(JSON.stringify({ type: 'copy_to_clipboard', text: el.textContent }));
}

// Callbacks from native side
window.onTokenGenerated = function(data) {
    state.newToken = data;
    state.tokens.push(data.token);
    state.selectedHash = data.token.hash;
    render();
};

window.onTokenDeleted = function(hash) {
    state.tokens = state.tokens.filter(t => t.hash !== hash);
    if (state.selectedHash === hash) state.selectedHash = null;
    render();
};

window.onAllRevoked = function() {
    state.tokens = [];
    state.selectedHash = null;
    state.newToken = null;
    render();
};

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

function removeExternalMcp(id) {
    ipc(JSON.stringify({ type: 'remove_external_mcp', id }));
}

window.onDiscoveryStarted = function() {
    state.discovering = true;
    state.discoveryError = null;
    render();
};

window.onExternalMcpAdded = function(mcp) {
    state.discovering = false;
    state.discoveryError = null;
    state.externalMcps.push(mcp);
    render();
};

window.onExternalMcpError = function(msg) {
    state.discovering = false;
    state.discoveryError = msg;
    render();
};

window.onExternalMcpRemoved = function(id) {
    state.externalMcps = state.externalMcps.filter(m => m.id !== id);
    render();
};

function renderServices() {
    let html = '<h2>Services</h2>';
    html += '<p style="color:#888;font-size:12px;margin-bottom:16px">Manage background processes. These appear in the tray menu for quick start/stop.</p>';

    if (state.services.length > 0) {
        for (const svc of state.services) {
            const cmdDisplay = svc.command.length > 40 ? '...' + svc.command.slice(-37) : svc.command;
            const argsDisplay = svc.args && svc.args.length > 0 ? ' ' + svc.args.join(' ') : '';
            html += ` + "`" + `<div class="mcp-card">
                <div class="mcp-card-header">
                    <span class="mcp-card-name">${esc(svc.display_name)}</span>
                    <div style="display:flex;gap:4px">
                        <button class="btn btn-sm" onclick="editService('${esc(svc.id)}')">Edit</button>
                        <button class="btn btn-sm btn-danger" onclick="removeService('${esc(svc.id)}')">Remove</button>
                    </div>
                </div>
                <div class="mcp-card-cmd">${esc(cmdDisplay + argsDisplay)}</div>
                ${svc.working_dir ? ` + "`" + `<div class="mcp-card-tools">cwd: ${esc(svc.working_dir)}</div>` + "`" + ` : ''}
                ${svc.url ? ` + "`" + `<div class="mcp-card-tools">url: ${esc(svc.url)}</div>` + "`" + ` : ''}
                <div class="toggle-row" style="margin-bottom:0;padding:6px 0 0">
                    <span style="font-size:12px;color:#888">Autostart on launch</span>
                    <label class="switch">
                        <input type="checkbox" ${svc.autostart ? 'checked' : ''} onchange="updateServiceAutostart('${esc(svc.id)}', this.checked)" />
                        <span class="slider"></span>
                    </label>
                </div>
            </div>` + "`" + `;
        }
    } else {
        html += '<div class="empty-state">No services configured.</div>';
    }

    const editing = state.editingServiceId ? state.services.find(s => s.id === state.editingServiceId) : null;
    const title = editing ? 'Edit Service' : 'Add Service';
    const dn = editing ? esc(editing.display_name) : '';
    const cm = editing ? esc(editing.command) : '';
    const ar = editing ? esc((editing.args || []).join(' ')) : '';
    const wd = editing ? esc(editing.working_dir || '') : '';
    const ev = editing ? esc(Object.entries(editing.env || {}).map(([k,v]) => k + '=' + v).join('\n')) : '';
    const as_ = editing ? editing.autostart : false;
    const ur = editing ? esc(editing.url || '') : '';

    html += '<div style="margin-top:20px;border-top:1px solid #333;padding-top:16px">';
    html += ` + "`" + `<h3>${title}</h3>` + "`" + `;
    html += ` + "`" + `<label>Display name${editing ? ' <span style="color:#666;font-size:11px">(id: ' + esc(editing.id) + ')</span>' : ''}</label>` + "`" + `;
    html += ` + "`" + `<input type="text" id="svcDisplayName" value="${dn}" placeholder="e.g. My API Server" />` + "`" + `;
    html += '<label>Command</label>';
    html += ` + "`" + `<input type="text" id="svcCommand" value="${cm}" placeholder="e.g. node or /usr/local/bin/my-server" />` + "`" + `;
    html += '<label>Arguments (space-separated)</label>';
    html += ` + "`" + `<input type="text" id="svcArgs" value="${ar}" placeholder="e.g. server.js --port 8080" />` + "`" + `;
    html += '<label>Working directory (optional)</label>';
    html += ` + "`" + `<input type="text" id="svcWorkingDir" value="${wd}" placeholder="e.g. /Users/you/project" />` + "`" + `;
    html += '<label>URL (optional, opens in browser on tray click)</label>';
    html += ` + "`" + `<input type="text" id="svcUrl" value="${ur}" placeholder="e.g. http://localhost:3000" />` + "`" + `;
    html += '<label>Environment variables (KEY=VALUE per line)</label>';
    html += ` + "`" + `<textarea id="svcEnv" rows="3" placeholder="API_KEY=abc123&#10;PORT=8080">${ev}</textarea>` + "`" + `;
    html += ` + "`" + `<div class="toggle-row" style="margin-top:8px;margin-bottom:4px">
        <span>Autostart on launch</span>
        <label class="switch">
            <input type="checkbox" id="svcAutostart" ${as_ ? 'checked' : ''} />
            <span class="slider"></span>
        </label>
    </div>` + "`" + `;
    if (editing) {
        html += ` + "`" + `<div style="margin-top:8px;display:flex;gap:8px">
            <button class="btn" onclick="saveServiceEdit()">Save</button>
            <button class="btn btn-danger" onclick="cancelServiceEdit()">Cancel</button>
        </div>` + "`" + `;
    } else {
        html += '<div style="margin-top:8px"><button class="btn" onclick="addService()">Add</button></div>';
    }
    html += '</div>';
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
    render();
};

window.onServiceRemoved = function(id) {
    state.services = state.services.filter(s => s.id !== id);
    render();
};

render();
</script>
</body>
</html>`
}
