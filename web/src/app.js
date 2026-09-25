// Settings UI application module: state, the render dispatcher, every tab
// renderer, IPC bridge, and all event handlers. Pure helpers live in
// ./lib/pure.js. Bundled (esbuild) and inlined into internal/webassets/settings.html.
import {
    esc, formatScalar, cfgParseConfigText, cfgGetAt, cfgSetAt, cfgDefaultFor, cfgCoerce, cfgKvCoerce, cfgKvDisplay, cfgScanRequired, cfgSummary, cfgFormatStringMap, cfgFormatJson, oneLineProj
} from './lib/pure.js';
import {
    groupModelCatalog, filterModelGroups, renderModelPickerBanner, renderModelPickerList
} from './lib/model_picker.js';

// Initial data injected by relay's renderSettingsHTML via the shell template.
const EXTERNAL_MCPS_INIT = window.__RELAY_INIT__.externalMcps;
const SERVICES_INIT = window.__RELAY_INIT__.services;
const RUNNING_IDS_INIT = window.__RELAY_INIT__.runningIds;
const PROJECTS_INIT = window.__RELAY_INIT__.projects;
// Hosts — machines reached over ssh a project's directory can live on
// (docs/ssh-hosts.md). Seeded like projects: the list is small, and both the
// Hosts tab and the project form's Where control need it on the first paint.
const HOSTS_INIT = window.__RELAY_INIT__.hosts || [];
// Terminal launch templates (internal/config/templates.go). Seeded like
// Hosts: the list is small (five built-ins plus any override) and this tab
// is read-only, so there is no form to protect from a push-sourced repaint.
const TEMPLATES_INIT = window.__RELAY_INIT__.templates || [];
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
const PASSKEYS_INIT = window.__RELAY_INIT__.passkeys || [];
const LOGIN_SESSIONS_INIT = window.__RELAY_INIT__.loginSessions || [];
const EVE_PASSKEYS_INIT = window.__RELAY_INIT__.evePasskeys || [];
// A bootstrap code minted by the tray's "Show Login Code..." item, seeded into
// the first paint because the window it is meant for did not exist when the
// code was minted. Null on every ordinary open, and never persisted anywhere:
// it lives in this page for two minutes and is not recoverable afterwards.
const LOGIN_CODE_INIT = window.__RELAY_INIT__.loginCode || null;
// The page the tray wants this window to open on. Seeded into the first paint
// rather than emitted, because a window that is not up yet has no document to
// receive an emit — see App.openRemoteClientsPage for the other arm.
const INITIAL_PAGE = window.__RELAY_INIT__.initialPage || null;

// Overview tab. mcpHealth and serviceRuntime are seeded like everything else
// above and refreshed by their own push events (onMcpHealth, onServiceStatus)
// so the tab never has a loading state. sealStatus/version/paths are fixed at
// boot (or, for sealStatus, effectively so — see App.sealStatus) and only
// change if the window is reopened or onSettingsReloaded fires.
const MCP_HEALTH_INIT = window.__RELAY_INIT__.mcpHealth || {};
const SERVICE_RUNTIME_INIT = window.__RELAY_INIT__.serviceRuntime || {};
const SEAL_STATUS_INIT = window.__RELAY_INIT__.sealStatus || '';
const VERSION_INIT = window.__RELAY_INIT__.version || 'dev';
const PATHS_INIT = window.__RELAY_INIT__.paths || { config: '', logs: '' };

function ipc(msg) {
    if (window.webkit && window.webkit.messageHandlers && window.webkit.messageHandlers.ipc)
        window.webkit.messageHandlers.ipc.postMessage(msg);
    else if (window.chrome && window.chrome.webview)
        window.chrome.webview.postMessage(msg);
}

// bind() is how a rendered control reaches a handler that takes a dynamic
// argument -- a project/host/MCP/service id OR a free-text display name the
// operator typed. An onclick attribute built as fn('...' + esc(value) + '...')
// is not safe for a free-text value even through esc(): the HTML parser
// decodes entities in an attribute value BEFORE the inline script text is
// compiled, so a name containing a quote closes the JS string early and runs
// as script in this privileged WebView (the one holding the ipc() bridge). A
// plain id (drawn from a validated, quote-free charset) was once considered
// safe enough to skip this and stay a literal onclick="fn('...')" -- that
// exception is gone: one convention for every dynamic value is simpler than
// two, and TestIPCContract_NoOnclickBuiltByConcatenatingAQuotedValue holds
// the line. bind() keeps the real function and its arguments -- as live
// values, never serialized to text -- in a table cleared on every render(),
// and the markup carries only the table index as data-act; the delegated
// listener near dispatchServiceAction() looks the index up and calls it. The
// one thing still built by plain concatenation is a bind-table INDEX (a
// number, never a value that could carry a quote) -- see e.g.
// renderScopeChoices. Never interpolate a dynamic value into an on* attribute.
function bind(fn, ...args) {
    const idx = state._actBind.length;
    state._actBind.push([fn, args]);
    return 'data-act="' + idx + '"';
}

let state = {
    page: 'overview',
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
    serviceSavePending: false,          // true from Add Service click until onServiceAdded/onSettingsError
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
    // Set only when the refusal localizes to one control (name, path) so the
    // message can render next to it instead of only in the top banner, and so
    // focusProjectFormIssue() knows where to put the cursor. null for a
    // refusal this form can't localize (a server-side shape/permissions
    // refusal) -- that one still shows in the banner, which is why the two
    // fields travel separately rather than as one.
    projectFormErrorField: null,
    projectSavePending: false,              // true from Create/Save click until onProjectAdded/Updated/onSettingsError
    projectTokenVisible: {},                // id -> bool (eye toggle)
    projectFreshToken: {},                  // id -> plaintext shown once after rotate
    projectSkillRegen: {},                  // id -> { ok, message, t } (last regen result)
    projectError: null,
    rotatingProjectId: null,

    // Hosts tab (docs/ssh-hosts.md).
    hosts: HOSTS_INIT,
    editingHostId: null,      // null = list, 'new' = add form, '<id>' = edit form
    hostForm: null,
    hostFormError: null,
    // A host's terminal templates (docs/ssh-hosts.md), edited inside the host
    // form. Same null/'new'/'<id>' convention as editingTemplateId.
    editingHostTemplateId: null,
    hostTemplateForm: null,
    hostTemplateFormError: null,
    hostTemplateSaving: false,  // the next onHostTemplatesListed closes the sub-form
    hostProbePending: {},     // id -> true while a probe/create/re-probe is in flight ('new' for the add form)
    hostError: null,

    // Templates tab (internal/config/templates.go).
    templates: TEMPLATES_INIT,
    editingTemplateId: null,  // null = list, 'new' = add form, '<id>' = edit form
    templateForm: null,
    templateFormError: null,
    templateSaving: false,    // a save is in flight; the next onTemplatesListed closes the form
    templateError: null,

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
    _actBind: [],             // per-render bindings for bind()/data-act controls (see bind())

    // Remote Clients tab. Enrolments and the remote block are seeded by the
    // initial payload like projects are — the list is one row per enrolled
    // certificate, and a credential you cannot see is one you will not revoke,
    // so it should be on screen the moment the tab is.
    enrolments: ENROLMENTS_INIT,
    remote: REMOTE_INIT,                    // remoteConfigView from Go, or null
    enrolmentBudgetDefaults: ENROLMENT_BUDGET_DEFAULTS_INIT,
    enrolForm: null,                        // null = list, object = create form; object.request_id set = approving a pending request
    enrolmentError: null,
    enrolBundle: null,                      // {client_id, dir} — DIRECTORY only, never key material
    enrolRevoked: null,                     // {client_id, fingerprint} shown after a revoke
    remoteDraft: null,                      // uncommitted edit of the remote block
    remoteDirty: false,
    remoteConfigSavePending: false,         // true from Save click until onRemoteConfigUpdated/onSettingsError
    remoteError: null,

    // Pending enrolment requests (spec §3). NOT seeded by the initial
    // payload the way enrolments are — this table lives only in the running
    // tray process and a network peer can change it between one paint and
    // the next, so a value seeded once would go stale in a way nothing here
    // would ever correct. Fetched fresh every time the tab is shown
    // (list_enrolment_requests) and kept live afterward by
    // onEnrolmentRequestsChanged, which the tray also pushes on its own
    // poll tick so a request that arrives while the tab is already open
    // still appears.
    pendingEnrolmentRequests: [],

    // Passkeys tab. Seeded like enrolments, and for the same reason — a
    // credential you cannot see is one you will not revoke.
    passkeys: PASSKEYS_INIT,
    loginSessions: LOGIN_SESSIONS_INIT,
    loginCode: LOGIN_CODE_INIT,
    passkeyError: null,
    passkeyRevoked: null,                   // {name, short} shown after a revoke
    loginSignedOut: null,                   // credential name shown after a sign-out

    // The eve section of the same tab (docs/eve-passkey-enrolment.md decision
    // 7): eve's own credential mirror, reported by eve and never edited here.
    evePasskeys: EVE_PASSKEYS_INIT,

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

    // Overview tab. mcpHealth and serviceRuntime are live snapshots kept
    // current by onMcpHealth / onServiceStatus / onSettingsReloaded; version,
    // sealStatus and paths change only if the window is reopened.
    mcpHealth: MCP_HEALTH_INIT,           // mcpId -> {connected, state, attempt, downtime_ms, error}
    serviceRuntime: SERVICE_RUNTIME_INIT, // serviceId -> {pid, started_at}
    sealStatus: SEAL_STATUS_INIT,         // '' when healthy, else store.SealStatus()'s reason
    version: VERSION_INIT,
    paths: PATHS_INIT,                    // {config, logs}
    mcpToolsOpen: {},                     // mcpId -> bool (the "N tools" disclosure)

    // The project form's Allowed Models picker. The catalog is a live read of
    // relay-sessions' model list, fetched once per form open and never polled;
    // the selection itself lives in projectForm.allowed_models.
    modelCatalog: null,                   // ModelCatalogView from list_models, or null before the first answer
    modelCatalogPending: false,           // true from requestModelCatalog until onModelsListed
    projModelSearch: '',
    projModelsOtherOpen: false,
};

// How many live events the Tool Calls tab keeps in the DOM. The Go-side ring
// is the real buffer; this only bounds what one open window renders.
const AUDIT_MAX_ROWS = 500;

function showPage(page) {
    state.page = page;
    // Positional against the sidebar items in web/shell.html — adding one
    // there without adding it here highlights the wrong row.
    const pages = ['overview', 'services', 'mcps', 'projects', 'hosts', 'templates', 'remote', 'passkeys', 'inspector', 'audit'];
    document.querySelectorAll('.sidebar-item').forEach((el, i) => {
        const selected = pages[i] === page;
        el.classList.toggle('active', selected);
        el.setAttribute('aria-selected', selected ? 'true' : 'false');
    });
    // The Tool Calls tab is the only one not seeded by the initial payload:
    // the log can be large, so it's fetched the first time it's shown. The
    // Overview tab's "Recent tool calls" needs the same ring, so it shares
    // the fetch-once-then-live-tail behaviour.
    if ((page === 'audit' || page === 'overview') && !state.auditLoaded) queryAudit();
    // Pending enrolment requests are never seeded (state.pendingEnrolmentRequests'
    // own comment) — refetched on every visit, not just the first, since a
    // network peer can change the table while the operator is on another tab.
    // The Overview tab's "Needs attention" list counts them too.
    if (page === 'remote' || page === 'overview') listEnrolmentRequests();
    // A hand-edited settings.json or an HTTP edit changes the list behind the
    // UI's back, so the tab re-fetches on every visit — cheap, and no restart.
    if (page === 'templates') listTemplates();
    render();
}

function listTemplates() {
    ipc(JSON.stringify({ type: 'list_templates' }));
}

const JSON_PLACEHOLDER = JSON.stringify({"my-server": {"command": "npx", "args": ["-y", "@example/server"], "env": {"API_KEY": "..."}}}, null, 2);

// render(source) repaints #content. When source === 'push' (IPC-driven),
// skip the repaint if a form for the current tab is open so we don't wipe
// keystrokes mid-edit. User-initiated renders always proceed — tab switches
// and explicit form mutations must always reflect on screen.
//
// The project form's name and path inputs live only in the DOM between
// renders (see captureProjectFormInputs), so ANY repaint that rebuilds them
// from state.projectForm without reading the DOM first erases whatever was
// typed. That used to be one call site's problem (the picker's async answer)
// and a special case was added there; granting an MCP re-renders too, and it
// had no such call, so the name field went empty (relay#25). Rather than find
// every mutator that can trigger a repaint and add the same line to each,
// capture-and-restore is done exactly once, here, unconditionally, before any
// page-specific branch runs — which is also what makes it safe: it is a
// harmless no-op on every page but Projects, and a no-op there too when no
// form is open or the DOM hasn't rendered the form's inputs yet.
function render(source) {
    if (state.projectForm) captureProjectFormInputs();
    if (state.enrolForm) captureEnrolFormInputs();
    if (state.hostForm) captureHostFormInputs();
    if (state.templateForm) captureTemplateFormInputs();
    if (state.hostTemplateForm) captureHostTemplateFormInputs();
    const el = document.getElementById('content');
    const fromPush = source === 'push';
    if (state.page === 'overview') {
        state._actBind = [];
        el.innerHTML = renderOverview();
    } else if (state.page === 'services') {
        if (fromPush && state.editingServiceId) return;
        state._actBind = [];
        el.innerHTML = renderServices();
    } else if (state.page === 'inspector') {
        // The 2s status poll updates status regions surgically
        // (updateServiceStatusDOM) and never routes here. But other push sources
        // (e.g. onSettingsReloaded after an external service/MCP change) still
        // call render('push'); skip the full inspector rebuild while a config
        // editor is open so it can't wipe in-flight keystrokes there.
        if (fromPush && anyConfigEditorOpen()) return;
        state._actBind = [];
        el.innerHTML = renderServiceInspector();
    } else if (state.page === 'projects') {
        if (fromPush && state.editingProjectId) return;
        state._actBind = [];
        el.innerHTML = renderProjects();
    } else if (state.page === 'hosts') {
        if (fromPush && state.editingHostId) return;
        state._actBind = [];
        el.innerHTML = renderHosts();
    } else if (state.page === 'templates') {
        state._actBind = [];
        el.innerHTML = renderTemplates();
    } else if (state.page === 'remote') {
        // Skip a push-sourced repaint while the create form is open or the
        // listener block has uncommitted edits, for the same reason the
        // Projects tab does: an external change must not eat keystrokes.
        if (fromPush && (state.enrolForm || state.remoteDirty)) return;
        state._actBind = [];
        el.innerHTML = renderEnrolments();
    } else if (state.page === 'passkeys') {
        state._actBind = [];
        el.innerHTML = renderPasskeys();
    } else if (state.page === 'audit') {
        state._actBind = [];
        el.innerHTML = renderAudit();
        restoreAuditFocus();
    } else {
        if (fromPush && state.editingMcpId) return;
        state._actBind = [];
        el.innerHTML = renderMcpServers();
        const ta = document.getElementById('mcpJson');
        if (ta) ta.placeholder = JSON_PLACEHOLDER;
    }
}

// ---------------------------------------------------------------------------
// Overview tab
// ---------------------------------------------------------------------------
//
// The landing page answers, on arrival, the questions every other tab makes
// an operator go find: is everything up, can anything reach something it
// shouldn't, is anything asking for me. Every number here is derived from
// state this page already has (or fetches once, like audit/pending
// enrolments) -- nothing is fetched solely for this tab.

function serviceCounts() {
    let running = 0, stopped = 0, autostartStopped = 0;
    for (const svc of (state.services || [])) {
        const isRunning = !!state.runningServices[svc.id];
        if (isRunning) running++; else stopped++;
        if (svc.autostart && !isRunning) autostartStopped++;
    }
    return { running, stopped, autostartStopped, autostartDown: autostartStopped > 0 };
}

// mcpHealthCounts breaks the warn/danger tint down by the same states
// mcpHealthPillFor names on each card, so the Overview tile can say *why*
// it's amber or red instead of just "N connected".
function mcpHealthCounts() {
    const list = state.externalMcps || [];
    let connected = 0, down = 0, restarting = 0, abandoned = 0, unauthenticated = 0;
    for (const mcp of list) {
        const pill = mcpHealthPillFor(mcp);
        if (pill.cls === 'ok') { connected++; continue; }
        if (pill.cls === 'danger') { abandoned++; continue; }
        if (pill.cls === 'warn') {
            if (mcp.transport === 'http') { unauthenticated++; continue; }
            const h = (state.mcpHealth || {})[mcp.id];
            if (h && h.state === 'restart_failed') restarting++; else down++;
        }
    }
    return {
        total: list.length, connected, down, restarting, abandoned, unauthenticated,
        danger: abandoned > 0, warn: (down + restarting + unauthenticated) > 0,
    };
}

// mcpTileValue and serviceTileValue render the Overview tile text for their
// tab: the headline count plus, only when non-zero, the breakdown that
// explains an amber/red tint -- an operator shouldn't have to open the tab
// to learn why it isn't plain green.
function mcpTileValue(mcp) {
    let value = mcp.connected + ' connected';
    if (mcp.down) value += ' · ' + mcp.down + ' down';
    if (mcp.restarting) value += ' · ' + mcp.restarting + ' restarting';
    if (mcp.unauthenticated) value += ' · ' + mcp.unauthenticated + ' not authenticated';
    if (mcp.abandoned) value += ' · ' + mcp.abandoned + ' abandoned';
    return value;
}

function serviceTileValue(svc) {
    let value = svc.running + ' running · ' + svc.stopped + ' stopped';
    if (svc.autostartStopped > 0) {
        value += svc.autostartStopped === svc.stopped ? ' (autostart)' : ' (' + svc.autostartStopped + ' autostart)';
    }
    return value;
}

function projectCounts() {
    let projects = 0, profiles = 0;
    for (const p of (state.projects || [])) {
        if (isRemoteProject(p)) profiles++; else projects++;
    }
    return { projects, profiles };
}

function hostCounts() {
    let connected = 0, unreachable = 0;
    for (const h of (state.hosts || [])) {
        if (h.status === 'connected') connected++;
        else if (h.status === 'unreachable') unreachable++;
    }
    return { connected, unreachable, total: (state.hosts || []).length };
}

function pendingEnrolmentCount() {
    return (state.pendingEnrolmentRequests || []).filter(r => !r.approved).length;
}

function remoteTileValue() {
    const r = state.remote;
    if (!r || !r.configured || !r.enabled || !r.audit_enabled) return 'Off';
    const n = pendingEnrolmentCount();
    return 'On ' + (r.effective || '127.0.0.1:9910') + (n ? ' · ' + n + ' pending' : '');
}

// auditTile returns the Overview Audit tile's text and, when the state is
// notable, a color class -- the same enabled/dropped facts auditStatusOf
// exposes over IPC, read from the ring query every settings-window open
// already makes.
function auditTile() {
    const st = state.auditStatus;
    if (!st) return { text: '—', cls: '' };
    if (!st.enabled) return { text: 'Off', cls: 'danger' };
    if (st.dropped > 0) return { text: 'Dropped ' + st.dropped, cls: 'warn' };
    return { text: 'Recording', cls: '' };
}

function pluralize(n, noun) {
    return n + ' ' + noun + (n === 1 ? '' : 's');
}

function renderOverviewTiles() {
    const svc = serviceCounts();
    const mcp = mcpHealthCounts();
    const proj = projectCounts();
    const hosts = hostCounts();
    const passkeysCount = (state.passkeys || []).length;
    const sessionsCount = (state.loginSessions || []).length;
    const evePasskeysCount = (state.evePasskeys || []).length;
    const audit = auditTile();

    const tiles = [
        { label: 'Services', value: serviceTileValue(svc), cls: svc.autostartDown ? 'danger' : '', tab: 'services' },
        { label: 'MCP Servers', value: mcpTileValue(mcp), cls: mcp.danger ? 'danger' : (mcp.warn ? 'warn' : ''), tab: 'mcps' },
        { label: 'Projects', value: pluralize(proj.projects, 'project') + ' · ' + pluralize(proj.profiles, 'access profile'), cls: '', tab: 'projects' },
        { label: 'Hosts', value: hosts.connected + ' connected · ' + hosts.unreachable + ' unreachable', cls: '', tab: 'hosts' },
        { label: 'Remote listener', value: remoteTileValue(), cls: '', tab: 'remote' },
        { label: 'Audit', value: audit.text, cls: audit.cls, tab: 'audit' },
        { label: 'Passkeys', value: pluralize(passkeysCount, 'passkey') + ' · ' + sessionsCount + ' browser' + (sessionsCount === 1 ? '' : 's') + ' signed in' + ' · ' + pluralize(evePasskeysCount, 'eve passkey'), cls: '', tab: 'passkeys' },
    ];

    let html = '<div class="ov-grid">';
    for (const t of tiles) {
        html += '<button type="button" class="ov-tile' + (t.cls ? ' ' + t.cls : '') + '" ' + bind(showPage, t.tab) + '>';
        html += '<div class="ov-tile-label">' + esc(t.label) + '</div>';
        html += '<div class="ov-tile-value">' + esc(t.value) + '</div>';
        html += '</button>';
    }
    html += '</div>';
    return html;
}

// overviewAttentionRows aggregates every source the design calls out, each
// row carrying the sentence and (when there's an obvious tab for it) a link
// to see more. Client-side only, from state already on the page — nothing
// here issues a call the tab wouldn't otherwise have made.
function overviewAttentionRows() {
    const rows = [];

    for (const p of (state.projects || [])) {
        for (const gap of projScopeGaps(p)) {
            rows.push({ text: esc(p.name) + ' (' + esc(projNoun(p)) + ') needs a scope value for ' + esc(gap.mcp) + '.', tab: 'projects' });
        }
    }

    for (const mcp of (state.externalMcps || [])) {
        const h = (state.mcpHealth || {})[mcp.id];
        if (!h || mcp.transport === 'http') continue;
        if (h.state === 'abandoned') {
            rows.push({ text: esc(mcp.display_name) + ' was abandoned after ' + h.attempt + ' restart attempts.', tab: 'mcps' });
        } else if (h.state === 'down' || h.state === 'restart_failed') {
            rows.push({ text: esc(mcp.display_name) + ' is down and restarting (attempt ' + h.attempt + ').', tab: 'mcps' });
        }
    }

    for (const h of (state.hosts || [])) {
        if (h.status === 'unreachable') {
            rows.push({ text: esc(h.name) + ' is unreachable.', tab: 'hosts' });
        } else if (h.probe && h.probe.ok && !h.probe.node_path) {
            rows.push({ text: esc(h.name) + ' has no node on the path relay probed.', tab: 'hosts' });
        }
    }

    for (const svc of (state.services || [])) {
        if (svc.autostart && !state.runningServices[svc.id]) {
            rows.push({ text: esc(svc.display_name) + ' is set to start with Relay but is not running.', tab: 'services' });
        }
    }

    const st = state.auditStatus;
    if (st && !st.enabled) {
        rows.push({ text: 'Tool-call auditing is off.', tab: 'audit' });
    } else if (st && st.dropped > 0) {
        rows.push({ text: 'The audit log has dropped ' + pluralize(st.dropped, 'event') + '.', tab: 'audit' });
    }

    if (state.sealStatus) {
        rows.push({ text: 'Sealed store: ' + esc(state.sealStatus), tab: null });
    }

    const pending = pendingEnrolmentCount();
    if (pending > 0) {
        rows.push({ text: pluralize(pending, 'pending enrolment request') + '.', tab: 'remote' });
    }

    return rows;
}

function renderOverviewAttention() {
    const rows = overviewAttentionRows();
    if (!rows.length) return '';
    let html = '<div class="ov-section"><h3>Needs attention</h3>';
    for (const row of rows) {
        html += '<div class="ov-attention-row"><span aria-hidden="true">⚠</span><span>' + row.text + '</span>';
        if (row.tab) html += '<button type="button" class="btn btn-sm ov-attention-link" ' + bind(showPage, row.tab) + '>Open</button>';
        html += '</div>';
    }
    html += '</div>';
    return html;
}

function renderOverviewRecentToolCalls() {
    const rows = (state.auditEvents || []).slice(0, 6);
    let html = '<div class="ov-section"><div class="page-header" style="margin-bottom:8px"><h3 style="margin:0">Recent tool calls</h3>';
    html += '<button type="button" class="btn btn-sm" ' + bind(showPage, 'audit') + '>See all</button></div>';
    if (!rows.length) {
        html += '<div class="empty-state">No tool calls recorded yet.</div>';
    } else {
        for (const ev of rows) {
            const a = ev.actor || {};
            html += '<div class="ov-recent-row">';
            html += '<span class="audit-time">' + esc(auditFmtTime(ev.ts)) + '</span>';
            html += '<span class="audit-pill audit-' + esc(ev.outcome) + '">' + esc(ev.outcome) + '</span>';
            html += '<span>' + esc(ev.tool || ev.event) + '</span>';
            html += '<span style="color:var(--text-2)">' + esc(a.project_name || '—') + '</span>';
            html += '</div>';
        }
    }
    html += '</div>';
    return html;
}

function renderOverviewFooter() {
    let html = '<div class="ov-footer">';
    html += '<span>relay ' + esc(state.version) + '</span>';
    html += '<span>' + esc(state.paths.config || '—') + ' <button type="button" class="btn-link" onclick="revealConfigDir()">Reveal</button></span>';
    html += '<button type="button" class="btn-link" onclick="revealLogsDir()">Reveal logs</button>';
    html += '</div>';
    return html;
}

function renderOverview() {
    let html = '<div class="page-header"><h2>Overview</h2></div>';
    html += renderOverviewTiles();
    html += renderOverviewAttention();
    html += renderOverviewRecentToolCalls();
    html += renderOverviewFooter();
    return html;
}

// mcpHealthPillFor is the single source for an MCP's health badge, used by
// both this tab's card and the Overview tile aggregate. An HTTP MCP's
// pill folds in its auth state (there is no supervisor health for those --
// they have no child process to restart) rather than rendering a second,
// separate badge next to it.
function mcpHealthPillFor(mcp) {
    if (mcp.transport === 'http') {
        const authed = !!(mcp.oauth_state && mcp.oauth_state.access_token);
        return { label: authed ? 'http · authenticated' : 'http · not authenticated', cls: authed ? 'ok' : 'warn' };
    }
    const h = (state.mcpHealth || {})[mcp.id];
    if (h) {
        if (h.state === 'abandoned') return { label: 'abandoned after ' + h.attempt + ' attempts', cls: 'danger' };
        if (h.state === 'down' || h.state === 'restart_failed') return { label: 'down · restarting (attempt ' + h.attempt + ')', cls: 'warn' };
        if (h.connected) return { label: 'connected', cls: 'ok' };
    }
    return { label: 'stopped', cls: 'muted' };
}

function toggleMcpToolsDisclosure(mcpId) {
    state.mcpToolsOpen[mcpId] = !state.mcpToolsOpen[mcpId];
    render();
}

// renderMcpToolsDisclosure turns the "N tools" line into a toggle that lists
// tool names from mcpToolCache -- the same data the count was already
// reading, so there is nothing new to fetch.
function renderMcpToolsDisclosure(mcp, toolCount) {
    const open = !!state.mcpToolsOpen[mcp.id];
    const label = toolCount + ' tool' + (toolCount !== 1 ? 's' : '') + (toolCount ? (open ? ' ▾' : ' ▸') : '');
    let html = `<button type="button" class="mcp-card-tools mcp-tools-toggle" aria-expanded="${open}" ${bind(toggleMcpToolsDisclosure, mcp.id)}>${esc(label)}</button>`;
    if (open && toolCount) {
        html += '<ul class="mcp-tools-list">';
        for (const t of (state.mcpToolCache[mcp.id] || [])) html += '<li>' + esc(t.name) + '</li>';
        html += '</ul>';
    }
    return html;
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
        const pill = mcpHealthPillFor(mcp);
        html += '<div class="mcp-card">';
        html += '<div class="mcp-card-header">';
        html += '<div style="display:flex;gap:8px;align-items:center;min-width:0">';
        html += `<span class="mcp-card-name">${esc(mcp.display_name)}</span>`;
        html += `<span class="pill ${pill.cls}">${esc(pill.label)}</span>`;
        html += '</div>';
        html += '<div style="display:flex;gap:4px;align-items:center;flex-shrink:0">';
        if (mcp.tcc_services && mcp.tcc_services.length > 0) {
            const busy = state.resettingMcpPermissions === mcp.id;
            const label = busy ? 'Resetting…' : 'Reset Permissions';
            html += `<button class="btn btn-sm" ${bind(resetMcpPermissions, mcp.id)} ${busy ? 'disabled' : ''}>${label}</button>`;
        }
        html += `<button class="btn btn-sm btn-danger" ${bind(removeExternalMcp, mcp.id, mcp.display_name)}>Remove</button>`;
        html += '</div></div>';
        if (isHTTP) {
            html += `<div class="mcp-card-cmd">${esc(mcp.url || '')}</div>`;
            html += '<div style="display:flex;align-items:center;gap:8px;margin-top:4px">';
            html += renderMcpToolsDisclosure(mcp, toolCount);
            if (authenticating) {
                html += '<button class="btn btn-sm" disabled><span class="spinner"></span>Authenticating...</button>';
            } else {
                html += `<button class="btn btn-sm" ${bind(authenticateMcp, mcp.id)}>Authenticate</button>`;
            }
            html += '</div>';
        } else {
            const cmd = mcp.command || '';
            const cmdDisplay = cmd.length > 40 ? '...' + cmd.slice(-37) : cmd;
            const argsDisplay = mcp.args && mcp.args.length > 0 ? ' ' + mcp.args.join(' ') : '';
            html += `<div class="mcp-card-cmd">${esc(cmdDisplay + argsDisplay)}</div>`;
            html += renderMcpToolsDisclosure(mcp, toolCount);
        }
        html += '</div>';
    }
    return html;
}

// Form view for adding an MCP server. There is no edit flow today — MCPs are
// add-or-remove; editingMcpId is always 'new' while this is rendered.
function renderMcpForm() {
    let html = '<h2>New MCP Server</h2>';

    const isStdio = state.mcpTransport === 'stdio';
    const formActive = state.mcpAddMode === 'form';

    html += '<div class="proj-section">';
    html += '<div class="proj-section-title">Transport</div>';
    html += `<div style="display:flex;gap:4px;margin-bottom:${isStdio ? '8px' : '0'}">
        <button class="perm-btn ${isStdio ? 'active' : ''}" onclick="setMcpTransport('stdio')">Stdio</button>
        <button class="perm-btn ${!isStdio ? 'active' : ''}" onclick="setMcpTransport('http')">HTTP</button>
    </div>`;
    if (isStdio) {
        html += `<div style="display:flex;gap:4px">
            <button class="perm-btn ${formActive ? 'active' : ''}" onclick="setMcpAddMode('form')">Form</button>
            <button class="perm-btn ${!formActive ? 'active' : ''}" onclick="setMcpAddMode('json')">Paste JSON</button>
        </div>`;
    }
    html += '</div>';

    if (!isStdio) {
        html += '<div class="proj-section">';
        html += '<div class="proj-section-title">Identity</div>';
        html += '<label for="mcpDisplayName">Display name</label>';
        html += '<input type="text" id="mcpDisplayName" placeholder="e.g. Krisp" />';
        html += '</div>';
        html += '<div class="proj-section">';
        html += '<div class="proj-section-title">Endpoint</div>';
        html += '<label for="mcpUrl">URL</label>';
        html += '<input type="text" id="mcpUrl" placeholder="e.g. https://mcp.krisp.ai/mcp" />';
        html += '</div>';
    } else if (formActive) {
        html += '<div class="proj-section">';
        html += '<div class="proj-section-title">Identity</div>';
        html += '<label for="mcpDisplayName">Display name</label>';
        html += '<input type="text" id="mcpDisplayName" placeholder="e.g. Everything Server" />';
        html += '</div>';
        html += '<div class="proj-section">';
        html += '<div class="proj-section-title">Command</div>';
        html += '<label for="mcpCommand">Command</label>';
        html += '<input type="text" id="mcpCommand" placeholder="e.g. npx or /usr/local/bin/my-server" />';
        html += '<label for="mcpArgs">Arguments (space-separated)</label>';
        html += '<input type="text" id="mcpArgs" placeholder="e.g. @modelcontextprotocol/server-everything" />';
        html += '</div>';
        html += '<div class="proj-section">';
        html += '<div class="proj-section-title">Environment</div>';
        html += '<label for="mcpEnv">Environment variables (KEY=VALUE per line)</label>';
        html += '<textarea id="mcpEnv" rows="3" placeholder="API_KEY=abc123&#10;DEBUG=true"></textarea>';
        html += '</div>';
    } else {
        html += '<div class="proj-section">';
        html += '<div class="proj-section-title">Configuration JSON</div>';
        html += '<label for="mcpJson">Paste a Claude Desktop-style JSON config snippet</label>';
        html += '<textarea id="mcpJson" rows="8"></textarea>';
        html += '<p style="color:var(--text-3);font-size:11px;margin-top:4px">Accepts <code style="color:var(--text-2)">&lbrace; "name": &lbrace; "command", "args", "env" &rbrace; &rbrace;</code></p>';
        html += '</div>';
    }

    html += '<div class="proj-form-actions">';
    if (state.discovering) {
        html += '<button class="btn" disabled><span class="spinner"></span>Discovering...</button>';
    } else {
        if (!isStdio) {
            html += '<button class="btn btn-primary" onclick="addExternalMcpHttp()">Add MCP Server</button>';
        } else {
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

function removeExternalMcp(id, name) {
    if (!confirm('Remove MCP server "' + name + '"?\n\nEvery project granting it loses its tools immediately, and any tool authority recorded for it becomes unreachable.')) return;
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

// capabilitiesPillHTML mirrors relay's `capabilitiesColumn` (cmd/relay/service_cmd.go):
// what the service's launch identity may do, docs/launch-identity.md.
function capabilitiesPillHTML(svc) {
    const caps = svc.capabilities || [];
    return '<div class="mcp-card-tools">capabilities: ' + (caps.length ? esc(caps.join(', ')) : 'none') + '</div>';
}

// formatUptime turns an RFC3339 started_at into "2h 14m" (or "14m" under an
// hour). Returns '' on anything unparseable so a malformed timestamp fails
// silent rather than rendering "NaNh NaNm".
function formatUptime(startedAt) {
    const start = Date.parse(startedAt);
    if (isNaN(start)) return '';
    let secs = Math.max(0, Math.floor((Date.now() - start) / 1000));
    const h = Math.floor(secs / 3600);
    const m = Math.floor((secs % 3600) / 60);
    return h > 0 ? (h + 'h ' + m + 'm') : (m + 'm');
}

// serviceStatusLineHTML is the Services card's muted second line: pid + uptime
// while running (from state.serviceRuntime, kept current by onServiceStatus
// and onSettingsReloaded), or "stopped" otherwise -- there is no data source
// that distinguishes "never started" from "exited", so both read the same.
function serviceStatusLineHTML(svc, running) {
    if (!running) return 'stopped';
    const rt = (state.serviceRuntime || {})[svc.id];
    if (!rt) return 'running';
    const up = formatUptime(rt.started_at);
    return 'pid ' + rt.pid + (up ? ' · up ' + up : '');
}

function renderServices() {
    if (state.editingServiceId) return renderServiceForm();

    let html = '<div class="page-header">';
    html += '<h2>Services</h2>';
    html += '<button class="btn btn-primary" onclick="newService()">+ New Service</button>';
    html += '</div>';
    html += '<p class="page-intro">Manage background processes. They appear in the tray menu in this order for quick start/stop, unless hidden from it.</p>';

    if (state.services.length === 0) {
        html += '<div class="empty-state">No services configured. Click <strong>+ New Service</strong> to add one.</div>';
        return html;
    }

    const lastIdx = state.services.length - 1;
    state.services.forEach((svc, idx) => {
        const running = !!state.runningServices[svc.id];
        const cmdBase = (svc.command || '').split('/').pop();
        const fullCmd = (svc.command || '') + (svc.args && svc.args.length > 0 ? ' ' + svc.args.join(' ') : '');
        html += `<div class="mcp-card">
            <div class="mcp-card-header">
                <div style="display:flex;align-items:center;gap:8px;min-width:0">
                    <span class="status-dot ${running ? 'ok' : 'muted'}" data-svc-dot="${esc(svc.id)}" aria-hidden="true"></span>
                    <span class="mcp-card-name">${esc(svc.display_name)}</span>
                    <span class="mono-inline" title="${esc(fullCmd)}">${esc(cmdBase)}</span>
                </div>
                <div style="display:flex;gap:4px;flex-shrink:0">
                    <button class="btn btn-sm" aria-label="Move up" title="Move up" ${idx === 0 ? 'disabled' : ''} ${bind(moveService, svc.id, -1)}>↑</button>
                    <button class="btn btn-sm" aria-label="Move down" title="Move down" ${idx === lastIdx ? 'disabled' : ''} ${bind(moveService, svc.id, 1)}>↓</button>
                    <button class="btn btn-sm" data-svc-startstop="${esc(svc.id)}" ${bind(toggleServiceRunning, svc.id)}>${running ? 'Stop' : 'Start'}</button>
                    <button class="btn btn-sm" ${bind(editService, svc.id)}>Edit</button>
                    <button class="btn btn-sm btn-danger" ${bind(removeService, svc.id, svc.display_name)}>Remove</button>
                </div>
            </div>
            <div class="mcp-card-tools" data-svc-runtime="${esc(svc.id)}">${esc(serviceStatusLineHTML(svc, running))}</div>
            ${capabilitiesPillHTML(svc)}
            <div class="mcp-card-tools"><button type="button" class="btn-link" ${bind(revealServiceLog, svc.id)}>Logs</button></div>
            ${svc.working_dir ? `<div class="mcp-card-tools">cwd: ${esc(svc.working_dir)}</div>` : ''}
            ${svc.url ? `<div class="mcp-card-tools">url: ${esc(svc.url)}</div>` : ''}
            <label class="toggle-row" style="margin-bottom:0;padding:6px 0 0;cursor:pointer">
                <span style="font-size:12px;color:var(--text-2)">Start with Relay</span>
                <span class="switch">
                    <input type="checkbox" aria-label="Start with Relay" ${svc.autostart ? 'checked' : ''} onchange="updateServiceAutostart('${esc(svc.id)}', this.checked)" />
                    <span class="slider"></span>
                </span>
            </label>
            <label class="toggle-row" style="margin-bottom:0;padding:6px 0 0;cursor:pointer">
                <span style="font-size:12px;color:var(--text-2)">Show in menu</span>
                <span class="switch">
                    <input type="checkbox" aria-label="Show in menu" ${svc.hide_from_menu ? '' : 'checked'} onchange="updateServiceMenuHidden('${esc(svc.id)}', this.checked)" />
                    <span class="slider"></span>
                </span>
            </label>
        </div>`;
    });
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
    const as_ = editing ? editing.autostart : false;
    const ur = editing ? esc(editing.url || '') : '';
    const caps = editing ? (editing.capabilities || []) : [];

    let html = '<h2>' + esc(title) + (editing ? ' <span style="color:var(--text-3);font-size:12px;font-weight:400">(id: ' + esc(editing.id) + ')</span>' : '') + '</h2>';

    html += '<div class="proj-section">';
    html += '<div class="proj-section-title">Identity</div>';
    html += '<label for="svcDisplayName">Display name</label>';
    html += `<input type="text" id="svcDisplayName" value="${dn}" placeholder="e.g. My API Server" />`;
    html += '</div>';

    html += '<div class="proj-section">';
    html += '<div class="proj-section-title">Command</div>';
    html += '<label for="svcCommand">Command</label>';
    html += `<input type="text" id="svcCommand" value="${cm}" placeholder="e.g. node or /usr/local/bin/my-server" />`;
    html += '<label for="svcArgs">Arguments (space-separated)</label>';
    html += `<input type="text" id="svcArgs" value="${ar}" placeholder="e.g. server.js --port 8080" />`;
    html += '<label for="svcWorkingDir">Working directory (optional)</label>';
    html += `<input type="text" id="svcWorkingDir" value="${wd}" placeholder="e.g. /Users/you/project" />`;
    html += '<label for="svcUrl">URL (optional, opens in browser on tray click)</label>';
    html += `<input type="text" id="svcUrl" value="${ur}" placeholder="e.g. http://localhost:3000" />`;
    html += '</div>';

    html += '<div class="proj-section">';
    html += '<div class="proj-section-title">Environment</div>';
    html += '<p class="proj-section-help">Values are sealed at rest and this window never shows a stored one again. Replace a value to change it, or remove a variable entirely.</p>';
    html += '<div id="svcEnvRows">' + renderServiceEnvRows() + '</div>';
    html += '<div style="display:flex;gap:6px;margin-top:8px">';
    html += '<input type="text" id="svcEnvNewKey" placeholder="KEY" style="width:160px" />';
    html += '<input type="text" id="svcEnvNewValue" placeholder="value" style="flex:1" />';
    html += '<button type="button" class="btn btn-sm" onclick="svcEnvAddRow()">Add</button>';
    html += '</div>';
    html += '</div>';

    html += '<div class="proj-section">';
    html += '<div class="proj-section-title">Capabilities</div>';
    html += '<p class="proj-section-help">What this service\'s launch identity may do through relay once it says Hello (docs/launch-identity.md). None held is legitimate: the service can start and say Hello and reach nothing else through relay.</p>';
    html += serviceCapabilityNames.map(function(cap) {
        // 'models' is the one capability with dependent UI (the Allowed
        // Models section below), so its checked state comes from
        // state.svcModelsCapOn -- kept alive across the full-form
        // re-renders that adding/removing a model row triggers -- rather
        // than from `caps`, which only reflects what was last saved.
        const checked = cap === 'models' ? !!state.svcModelsCapOn : caps.indexOf(cap) >= 0;
        // Reads the box itself rather than taking `this.checked` as an
        // argument -- the literal substring "checked" inside an inline
        // handler attribute is indistinguishable, to a naive
        // checked-attribute regex, from the checkbox's own checked state
        // (settings_service_form_ui_test.go's capabilities regex matches
        // exactly that way).
        const onchange = cap === 'models' ? ' onchange="svcModelsCapChanged()"' : '';
        return '<label class="toggle-row" style="padding:4px 0;margin:0;cursor:pointer">' +
            '<span>' + esc(cap) + '</span>' +
            '<span class="switch">' +
            '<input type="checkbox" id="svcCap_' + cap + '" aria-label="' + esc(cap) + ' capability" ' + (checked ? 'checked' : '') + onchange + ' />' +
            '<span class="slider"></span>' +
            '</span>' +
            '</label>';
    }).join('');
    html += '</div>';

    if (state.svcModelsCapOn) {
        html += '<div class="proj-section">';
        html += '<div class="proj-section-title">Allowed Models</div>';
        html += '<p class="proj-section-help">Which models this service may reach through relay\'s model endpoint (docs/model-endpoint.md). An <strong>empty list means NO models</strong> -- the opposite of a project\'s own default, since a service like TTS or STT is normally meant to reach exactly one model, not everything relayLLM serves. A single <code>*</code> entry means every model.</p>';
        html += '<div id="svcModelsRows">' + renderServiceModelRows() + '</div>';
        html += '<div style="display:flex;gap:6px;margin-top:8px">';
        html += '<input type="text" id="svcModelNewId" placeholder="model id, or * for every model" style="flex:1" />';
        html += '<button type="button" class="btn btn-sm" onclick="svcModelAddRow()">Add</button>';
        html += '</div>';
        html += '</div>';
    }

    html += '<div class="proj-section">';
    html += '<div class="proj-section-title">Options</div>';
    html += `<div class="toggle-row" style="padding:4px 0;margin:0">
        <span>Autostart on launch</span>
        <label class="switch">
            <input type="checkbox" id="svcAutostart" aria-label="Autostart on launch" ${as_ ? 'checked' : ''} />
            <span class="slider"></span>
        </label>
    </div>`;
    html += '</div>';

    html += '<div class="proj-form-actions">';
    if (editing) {
        html += '<button class="btn btn-primary" onclick="saveServiceEdit()">Save</button>';
    } else {
        html += '<button class="btn btn-primary" onclick="addService()" ' + (state.serviceSavePending ? 'disabled' : '') + '>' + (state.serviceSavePending ? 'Adding…' : 'Add Service') + '</button>';
    }
    html += '<button class="btn btn-danger" onclick="cancelServiceEdit()">Cancel</button>';
    html += '</div>';
    return html;
}

// serviceCapabilityNames is the whole vocabulary (docs/launch-identity.md)
// and the ONE place this form spells it: the checkbox loop below and
// svcFormValues' harvest both read this same array, never a second literal.
// Its mirror on the Go side is config.ServiceCapabilities
// (internal/config/models.go) — a new capability is a rare, deliberate
// protocol change, so it is hand-kept in sync with that list rather than
// fetched, the same way the CLI's own --capability flag help text names
// them by hand too.
const serviceCapabilityNames = ['frontend', 'manifest', 'models', 'model_host'];

// renderServiceEnvRows renders state.svcEnvDraft, the edit session's own env
// draft (seeded by editService/newService, never derived from `editing`
// directly): each row's stored value is masked, never displayed or
// resubmitted, and the row itself carries what the operator has done to it
// ('keep', 'replace' with a typed value, or 'removed') so svcFormValues can
// build the wire's per-key optional-value map straight off this array
// without ever having held a stored value in the clear.
function renderServiceEnvRows() {
    const rows = state.svcEnvDraft || [];
    let html = '';
    let shown = 0;
    for (let i = 0; i < rows.length; i++) {
        const row = rows[i];
        if (row.mode === 'removed') continue;
        shown++;
        html += '<div class="svc-env-row" style="display:flex;gap:6px;align-items:center;margin-bottom:6px">';
        html += '<span class="mono-inline" style="min-width:140px;word-break:break-all" title="' + esc(row.key) + '">' + esc(row.key) + '</span>';
        if (row.mode === 'replace') {
            html += '<input type="text" value="' + esc(row.value || '') + '" placeholder="new value" style="flex:1" onchange="svcEnvSetValue(' + i + ', this.value)" />';
        } else {
            html += '<span class="mono-inline" style="flex:1;color:var(--text-3)">••••••••</span>';
            html += '<button type="button" class="btn btn-sm" ' + bind(svcEnvSetMode, i, 'replace') + '>Replace</button>';
        }
        html += '<button type="button" class="btn btn-sm btn-danger" ' + bind(svcEnvRemoveRow, i) + '>Remove</button>';
        html += '</div>';
    }
    if (shown === 0) html += '<div class="empty-state">No environment variables.</div>';
    return html;
}

function svcEnvSetMode(i, mode) {
    const row = (state.svcEnvDraft || [])[i];
    if (!row) return;
    row.mode = mode;
    if (mode === 'replace' && row.value == null) row.value = '';
    render();
}

// svcEnvSetValue is wired to onchange (fires on blur/commit), not oninput,
// the same discipline the project form's mount id/path fields already use
// (renderProjMounts): render() rebuilds this whole form from state, and a
// field that only syncs on commit survives that rebuild instead of dropping
// whatever the operator was mid-typing when some other control's click
// triggered it.
function svcEnvSetValue(i, value) {
    const row = (state.svcEnvDraft || [])[i];
    if (!row) return;
    row.value = value;
}

function svcEnvRemoveRow(i) {
    const row = (state.svcEnvDraft || [])[i];
    if (!row) return;
    row.mode = 'removed';
    render();
}

function svcEnvAddRow() {
    const keyEl = document.getElementById('svcEnvNewKey');
    const valEl = document.getElementById('svcEnvNewValue');
    if (!keyEl) return;
    const key = keyEl.value.trim();
    if (!key) return;
    if (!state.svcEnvDraft) state.svcEnvDraft = [];
    const row = { key: key, mode: 'replace', value: valEl ? valEl.value : '' };
    // A key re-added after being marked removed, or typed twice, replaces
    // the earlier row rather than producing two -- the wire map can only
    // ever hold one value per key.
    const existingIdx = state.svcEnvDraft.findIndex(function(r) { return r.key === key; });
    if (existingIdx >= 0) state.svcEnvDraft[existingIdx] = row;
    else state.svcEnvDraft.push(row);
    keyEl.value = '';
    if (valEl) valEl.value = '';
    render();
}

// svcEnvWireValue turns the draft into env's wire shape (serviceFields.Env):
// null for a kept key, the typed string for a replaced one, and a removed
// row is simply absent -- Env's own whole-map-replace semantics read an
// absent key as "gone".
function svcEnvWireValue() {
    const env = {};
    for (const row of (state.svcEnvDraft || [])) {
        if (row.mode === 'removed') continue;
        env[row.key] = row.mode === 'replace' ? row.value : null;
    }
    return env;
}

// svcEnvMergedForDisplay answers what env WILL hold after this save lands,
// in the clear -- used only for the optimistic local patch in
// saveServiceEdit, the same "show it now, the next real read will confirm
// it" convenience every other field in this form's optimistic update
// already relies on. A kept row's plaintext comes from the record's
// existing env (already revealed, never resealed by this window merely
// displaying it — see native_view.go); nothing here goes over the wire.
function svcEnvMergedForDisplay(existingEnv) {
    const env = {};
    for (const row of (state.svcEnvDraft || [])) {
        if (row.mode === 'removed') continue;
        env[row.key] = row.mode === 'replace' ? row.value : ((existingEnv || {})[row.key] || '');
    }
    return env;
}

// renderServiceModelRows renders state.svcModelsDraft, the edit session's own
// allowed-models draft (seeded by editService/newService from the stored
// record's allowed_models, unlike svcEnvDraft there is no masking concern —
// a model id is not a secret — so each row is just the string itself.
function renderServiceModelRows() {
    const rows = state.svcModelsDraft || [];
    let html = '';
    for (let i = 0; i < rows.length; i++) {
        html += '<div class="svc-env-row" style="display:flex;gap:6px;align-items:center;margin-bottom:6px">';
        html += '<input type="text" value="' + esc(rows[i]) + '" style="flex:1" onchange="svcModelSetValue(' + i + ', this.value)" />';
        html += '<button type="button" class="btn btn-sm btn-danger" ' + bind(svcModelRemoveRow, i) + '>Remove</button>';
        html += '</div>';
    }
    if (rows.length === 0) html += '<div class="empty-state">No models: this service holds the models capability but can reach none until at least one model id (or *) is added.</div>';
    return html;
}

function svcModelSetValue(i, value) {
    const rows = state.svcModelsDraft || [];
    if (i < 0 || i >= rows.length) return;
    rows[i] = value;
}

function svcModelRemoveRow(i) {
    const rows = state.svcModelsDraft || [];
    if (i < 0 || i >= rows.length) return;
    rows.splice(i, 1);
    render();
}

// svcModelsCapChanged is the models checkbox's onchange: it drives the
// Allowed Models section's visibility on its own, separate from
// svcModelsDraft, so unticking never touches rows the operator already
// typed this session -- re-ticking finds the section exactly as left.
function svcModelsCapChanged() {
    const el = document.getElementById('svcCap_models');
    state.svcModelsCapOn = !!(el && el.checked);
    render();
}

function svcModelAddRow() {
    const idEl = document.getElementById('svcModelNewId');
    if (!idEl) return;
    const id = idEl.value.trim();
    if (!id) return;
    if (!state.svcModelsDraft) state.svcModelsDraft = [];
    state.svcModelsDraft.push(id);
    idEl.value = '';
    render();
}

function svcFormValues() {
    const displayName = document.getElementById('svcDisplayName').value.trim();
    const command = document.getElementById('svcCommand').value.trim();
    const argsStr = document.getElementById('svcArgs').value.trim();
    const workingDir = document.getElementById('svcWorkingDir').value.trim();
    const autostart = document.getElementById('svcAutostart').checked;
    const url = document.getElementById('svcUrl').value.trim();

    const args = argsStr ? argsStr.split(/\s+/) : [];
    const env = svcEnvWireValue();
    const capabilities = serviceCapabilityNames.filter(function(cap) {
        const el = document.getElementById('svcCap_' + cap);
        return !!(el && el.checked);
    });

    const result = { displayName, command, args, env, workingDir, autostart, url, capabilities };
    // allowedModels rides along only when the form is actually claiming the
    // models capability -- the Allowed Models section does not even exist in
    // the DOM otherwise (see renderServiceForm), and leaving the key off the
    // returned object entirely (rather than an empty array) is what lets
    // JSON.stringify drop it from the wire payload, so an update that never
    // touches this capability leaves the stored grant alone (serviceFields.
    // AllowedModels nil-preserves-existing, the same rule Capabilities uses).
    if (capabilities.indexOf('models') >= 0) {
        result.allowedModels = (state.svcModelsDraft || []).slice();
    }
    return result;
}

function newService() {
    state.editingServiceId = 'new';
    state.svcEnvDraft = [];
    state.svcModelsDraft = [];
    state.svcModelsCapOn = false;
    render();
}

function addService() {
    const v = svcFormValues();
    if (!v.displayName || !v.command) return;

    state.serviceSavePending = true;
    ipc(JSON.stringify({
        type: 'add_service',
        display_name: v.displayName,
        command: v.command,
        args: v.args,
        env: v.env,
        working_dir: v.workingDir || null,
        autostart: v.autostart,
        url: v.url || null,
        capabilities: v.capabilities,
        allowed_models: v.allowedModels,
    }));
    // Form stays open until onServiceAdded confirms — that handler clears
    // editingServiceId. If the add fails (onSettingsError), the form stays
    // up so the user can fix and retry.
    render();
}

function editService(id) {
    state.editingServiceId = id;
    const svc = state.services.find(s => s.id === id);
    // Every existing key starts 'keep': masked, and never resubmitted unless
    // the operator explicitly clicks Replace.
    state.svcEnvDraft = Object.keys((svc && svc.env) || {}).sort().map(function(k) {
        return { key: k, mode: 'keep', value: '' };
    });
    state.svcModelsDraft = ((svc && svc.allowed_models) || []).slice();
    state.svcModelsCapOn = ((svc && svc.capabilities) || []).indexOf('models') >= 0;
    render();
}

function cancelServiceEdit() {
    state.editingServiceId = null;
    state.svcEnvDraft = null;
    state.svcModelsDraft = null;
    state.svcModelsCapOn = null;
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
        capabilities: v.capabilities,
        allowed_models: v.allowedModels,
    }));

    const svc = state.services.find(s => s.id === state.editingServiceId);
    if (svc) {
        const mergedEnv = svcEnvMergedForDisplay(svc.env);
        svc.display_name = v.displayName;
        svc.command = v.command;
        svc.args = v.args;
        svc.env = mergedEnv;
        svc.working_dir = v.workingDir || null;
        svc.autostart = v.autostart;
        svc.url = v.url || null;
        svc.capabilities = v.capabilities;
        // v.allowedModels is undefined exactly when this save left the grant
        // untouched (see svcFormValues) -- an unconditional assignment here
        // would optimistically show "no models" for a service whose stored
        // grant this save never mentioned.
        if (v.allowedModels !== undefined) svc.allowed_models = v.allowedModels;
    }
    state.editingServiceId = null;
    state.svcEnvDraft = null;
    state.svcModelsDraft = null;
    state.svcModelsCapOn = null;
    render();
}

function removeService(id, name) {
    if (!confirm('Remove service "' + name + '"?\n\nIf it is running, it is stopped immediately, and it disappears from the tray menu.')) return;
    ipc(JSON.stringify({ type: 'remove_service', id }));
}

function updateServiceAutostart(id, checked) {
    const svc = state.services.find(s => s.id === id);
    if (svc) svc.autostart = checked;
    ipc(JSON.stringify({ type: 'update_service_autostart', id, autostart: checked }));
}

function updateServiceMenuHidden(id, checked) {
    const svc = state.services.find(s => s.id === id);
    if (svc) svc.hide_from_menu = !checked;
    ipc(JSON.stringify({ type: 'update_service_menu_hidden', id, hidden: !checked }));
}

function moveService(id, delta) {
    const from = state.services.findIndex(s => s.id === id);
    if (from < 0) return;
    const index = from + delta;
    if (index < 0 || index >= state.services.length) return;
    const [svc] = state.services.splice(from, 1);
    state.services.splice(index, 0, svc);
    render();
    ipc(JSON.stringify({ type: 'move_service', id, index }));
}

window.onServiceAdded = function(config) {
    state.services.push(config);
    state.serviceSavePending = false;
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

// toggleServiceRunning reads the CURRENT state rather than taking a target
// value, because the Start/Stop button's bind() captures its handler at
// render time -- a live status update (onServiceStatus) updates the card's
// button label surgically without a full re-render, and a stale captured
// boolean would send the opposite of what the button now reads.
function toggleServiceRunning(id) {
    const next = !state.runningServices[id];
    state.runningServices[id] = next;
    ipc(JSON.stringify({ type: next ? 'start_service' : 'stop_service', id: id }));
    render();
}

window.onServiceStatus = function(data) {
    var runningIds = data.running_ids || [];
    var m = {};
    for (var i = 0; i < runningIds.length; i++) m[runningIds[i]] = true;
    state.runningServices = m;
    state.serviceRuntime = data.runtime || {};
    if (state.page === 'overview') render('push');
    if (state.page !== 'services') return;
    // Surgically update each card's status region in place rather than a
    // full re-render, which would wipe text the user is typing into the
    // Add/Edit Service form -- this event fires on every start/stop/add/
    // remove/update, not only on the 2s inspector poll.
    for (var i = 0; i < state.services.length; i++) {
        var svc = state.services[i];
        var running = !!m[svc.id];
        var dot = document.querySelector('[data-svc-dot="' + svc.id + '"]');
        if (dot) dot.className = 'status-dot ' + (running ? 'ok' : 'muted');
        var btn = document.querySelector('[data-svc-startstop="' + svc.id + '"]');
        if (btn) btn.textContent = running ? 'Stop' : 'Start';
        var meta = document.querySelector('[data-svc-runtime="' + svc.id + '"]');
        if (meta) meta.textContent = serviceStatusLineHTML(svc, running);
    }
};

window.onSettingsError = function(msg) {
    console.error('Settings save error:', msg);
    // Whichever save was in flight failed; this handler has no way to tell
    // which, so it clears every save guard rather than leave one stuck
    // permanently disabled, then re-renders so the button reflects it.
    state.projectSavePending = false;
    state.serviceSavePending = false;
    state.remoteConfigSavePending = false;
    render();
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
    if (data.passkeys) state.passkeys = data.passkeys;
    if (data.login_sessions) state.loginSessions = data.login_sessions;
    if (data.eve_passkeys) state.evePasskeys = data.eve_passkeys;
    if (data.remote) {
        state.remote = data.remote;
        // Re-seed the listener draft from the server's answer unless the user
        // is mid-edit; clobbering a half-typed address would be the same bug
        // render('push') avoids for the project form.
        if (!state.remoteDirty) state.remoteDraft = null;
    }
    if (data.mcp_health) state.mcpHealth = data.mcp_health;
    if (data.service_runtime) state.serviceRuntime = data.service_runtime;
    if ('seal_status' in data) state.sealStatus = data.seal_status || '';
    if (data.version) state.version = data.version;
    if (data.paths) state.paths = data.paths;
    // Push-sourced repaint of the currently visible tab; render() itself
    // skips if a form is mid-edit. Other tabs pick up the fresh state on
    // next switch — no need to repaint them now.
    render('push');
};

// onMcpHealth is pushed whenever any external MCP's supervised health
// changes (a death, a restart, an abandonment) — the whole map, not a diff,
// because the map is small and a diff would need its own drift guard.
window.onMcpHealth = function(mcpHealth) {
    state.mcpHealth = mcpHealth || {};
    if (state.page === 'mcps' || state.page === 'overview') render('push');
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

// projAllowExternal mirrors StoredToken.ExternalAllowed against a stored
// record: an explicit value wins in either direction, and the default is the
// same asymmetry projAccessMode has — a profile refuses, a local project
// allows, because a local agent already has the host's network and a remote
// client has no path off this host except through relay.
function projAllowExternal(p, mcpID) {
    const explicit = (p && p.allow_external) ? p.allow_external[mcpID] : undefined;
    if (explicit === true || explicit === false) return explicit;
    return !isRemoteProject(p);
}

// scopeValueIsSet mirrors hasScopeValue on the Go side: absent, null, empty
// string, empty list and empty object are all ABSENT. Used only where Go
// uses hasScopeValue too -- scopeDependencyValues, the picker's enumerate
// filter, which is a DIFFERENT axis (a query, not an authorisation) and was
// never about decision 4. For "is this field's own value a live
// authorisation the operator made", see scopeValueIsAsserted.
function scopeValueIsSet(v) {
    if (v === undefined || v === null) return false;
    if (Array.isArray(v)) return v.length > 0;
    if (typeof v === 'string') return v.trim() !== '';
    if (typeof v === 'object') return Object.keys(v).length > 0;
    return true;
}

// scopeValueIsAsserted mirrors hasScopeAssertion on the Go side (ADR-011
// addendum, "A star and an empty array"): a present, explicit empty ARRAY
// counts as a live authorisation -- the confirmed-empty grant, distinct from
// the field being unset -- while null / empty string / empty object still do
// not. Used everywhere a value being present is what governs whether a tool
// is refused: the "needs a scope value" banner (projMissingScopeFields), the
// authority summary, and -- the one that matters most -- harvesting the form
// for save, where treating [] as unset would silently discard a
// "confirm nothing to grant" click on the way to the wire.
function scopeValueIsAsserted(v) {
    if (Array.isArray(v)) return true;
    return scopeValueIsSet(v);
}

function projScopeValue(p, mcpID, fieldName) {
    const perMcp = (p && p.context) ? p.context[mcpID] : null;
    return perMcp ? perMcp[fieldName] : undefined;
}

// scopeValueText renders a stored value for a human. Arrays are the shape
// every scope field met so far declares; anything else prints as its JSON,
// which is honest about a shape this UI does not model.
//
// An empty array is the confirmed-empty grant (ADR-011 addendum, "A star
// and an empty array") and gets its own phrase, mirroring Go's
// renderScopeValue -- the alternative is an authority row reading
// "mail_accounts: ", which looks like a rendering bug rather than a
// deliberate choice.
function scopeValueText(v) {
    if (Array.isArray(v)) return v.length ? v.join(', ') : 'confirmed empty — confined to nothing';
    if (typeof v === 'string') return v;
    if (v === undefined || v === null) return '';
    return JSON.stringify(v);
}

// ---- How much of the host one scope value reaches (issue #41) --------------
//
// Mirrors scope_breadth.go, entry for entry. A count is not a measure of
// confinement: "1 value" is true of /Users/me/project and equally true of "/",
// and the profile card rendered the second as a single character inline. It is
// a question about the VALUE and never about the field name — ADR-011 decision
// 3 refuses relay a registry of known field names, and this does not smuggle
// one back in.

const SCOPE_BREADTH_ROOT = 'root';
const SCOPE_BREADTH_HOME = 'home';
// SCOPE_BREADTH_WILDCARD mirrors Go's scopeBreadthWildcard (ADR-011
// addendum, "A star and an empty array"): a resource-scope field's value of
// exactly ["*"], grouped with root rather than home because it discloses
// nothing about this host a client could not already learn by calling the
// field's own enumerator.
const SCOPE_BREADTH_WILDCARD = 'wildcard';
// SCOPE_WILDCARD_VALUE mirrors Go's ContextWildcardValue.
const SCOPE_WILDCARD_VALUE = '*';

function scopeBreadthPhrase(kind) {
    if (kind === SCOPE_BREADTH_ROOT) return 'unrestricted (the whole filesystem)';
    if (kind === SCOPE_BREADTH_HOME) return 'a whole home directory';
    if (kind === SCOPE_BREADTH_WILDCARD) {
        return 'unrestricted (every value, resolved fresh on every call -- including one added after this grant was made)';
    }
    return '';
}

// scopeCleanPath is the small part of Go's filepath.Clean this needs: collapse
// repeated slashes and resolve "." / ".." segments, so "/", "//", "/.." and
// "/Users/admin/../.." are one value rather than four spellings one of which
// gets past the check.
function scopeCleanPath(v) {
    const out = [];
    for (const seg of v.split('/')) {
        if (seg === '' || seg === '.') continue;
        if (seg === '..') { out.pop(); continue; }
        out.push(seg);
    }
    return '/' + out.join('/');
}

function scopeEntryBreadth(entry) {
    if (typeof entry !== 'string') return '';
    const v = entry.trim();
    if (v === '') return '';
    // "~" is a home directory to every shell and to a good many MCPs. Relay
    // does not expand it and cannot know whether the MCP receiving it will.
    if (v === '~' || v === '~/') return SCOPE_BREADTH_HOME;
    if (v.charAt(0) !== '/') return '';
    const clean = scopeCleanPath(v);
    if (clean === '/') return SCOPE_BREADTH_ROOT;
    const parts = clean.slice(1).split('/');
    if (parts.length <= 2 && (parts[0] === 'Users' || parts[0] === 'home')) return SCOPE_BREADTH_HOME;
    return '';
}

// scopeValueBreadth returns the WIDEST breadth any entry has: a list is a
// union, so ["/Users/me/proj", "/"] reaches everything and a card that
// reported the first entry would describe the confinement the operator meant
// instead of the one in force.
function scopeValueBreadth(v) {
    const entries = Array.isArray(v) ? v : (typeof v === 'string' ? [v] : []);
    // Checked against the WHOLE value first, not per entry: the wildcard is
    // recognised only as the array's sole element (ADR-011 addendum), the
    // same rule the save-time validator enforces, so a mixed array can never
    // reach here already stored.
    if (entries.length === 1 && entries[0] === SCOPE_WILDCARD_VALUE) return SCOPE_BREADTH_WILDCARD;
    let widest = '';
    for (const e of entries) {
        const kind = scopeEntryBreadth(e);
        if (kind === SCOPE_BREADTH_ROOT) return SCOPE_BREADTH_ROOT;
        if (kind === SCOPE_BREADTH_HOME) widest = SCOPE_BREADTH_HOME;
    }
    return widest;
}

// projScopeBreadthWarnings names every field of one MCP's scope on this record
// whose value reaches further than a folder. Used by the list row, the editor
// and the save-time confirmation, so all three say the same thing.
function projScopeBreadthWarnings(p, mcpID) {
    const fields = mcpScopeFieldsFor(mcpID) || [];
    const out = [];
    for (const f of fields) {
        const phrase = scopeBreadthPhrase(scopeValueBreadth(projScopeValue(p, mcpID, f.name)));
        if (phrase) out.push(f.name + ' is ' + phrase);
    }
    return out;
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
        .filter(f => !scopeValueIsAsserted(projScopeValue(p, mcpID, f.name)))
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
            if (scopeValueIsAsserted(v)) scope.push(f.name + ': ' + scopeValueText(v));
        }
        return {
            mcp: mcpID,
            mode: projAccessMode(p, mcpID),
            external: projAllowExternal(p, mcpID),
            tools: projToolAuthorityText(p, mcpID),
            scope: scope,
            derived: derived,
            missing: projMissingScopeFields(p, mcpID),
            // Issue #41: the row already showed the real value — "/" is one
            // character and every reviewer's eye went past it. The finding
            // gets a line of its own, in the same words the CLI and the audit
            // authority line use.
            breadth: projScopeBreadthWarnings(p, mcpID),
            schemaUnknown: fields === null,
        };
    });
}

// renderProjMountRows is renderAuthorityRows' counterpart for the
// mount-plane grant — the list-card summary of what `relay grant`'s own
// printGrantViews shows on the CLI, so the two surfaces never disagree
// about whether a profile with zero MCPs still reaches something.
function renderProjMountRows(p) {
    const mounts = p.mounts || [];
    let html = '';
    for (const m of mounts) {
        html += '<div class="proj-auth-row">';
        html += '<span class="proj-auth-mcp">mount ' + esc(m.id) + '</span>';
        const access = m.access === 'write' ? 'write' : 'read';
        html += '<span class="proj-auth-mode ' + esc(access) + '">' + esc(access) + '</span>';
        html += '<span class="proj-auth-scope">' + esc(m.path || '') + '</span>';
        const phrase = scopeBreadthPhrase(scopeEntryBreadth(m.path || ''));
        if (phrase) html += '<span class="proj-auth-unrestricted">' + esc(phrase) + '</span>';
        html += '</div>';
    }
    return html;
}

function renderAuthorityRows(p) {
    const rows = projAuthorityRows(p);
    const mounts = p.mounts || [];
    if (!rows.length && !mounts.length) {
        if (p.host_id) {
            return '<div class="proj-auth-row"><span class="proj-auth-none">on ' + esc(hostNameFor(p.host_id)) + ' — the agent uses its built-in tools there; relay tools aren\'t available on a host yet</span></div>';
        }
        return '<div class="proj-auth-row"><span class="proj-auth-none">no MCPs or mounts granted — this ' + esc(projNoun(p)) + ' reaches nothing</span></div>';
    }
    let html = '';
    for (const r of rows) {
        html += '<div class="proj-auth-row">';
        html += '<span class="proj-auth-mcp">' + esc(r.mcp) + '</span>';
        html += '<span class="proj-auth-mode ' + esc(r.mode) + '">' + esc(r.mode) + '</span>';
        // The outbound grant is on the row because it is half of what relay
        // decides by itself, and a row that showed only the mode would answer
        // "what can this client do" with the axis that does not mention the
        // network. The refused state is printed too — this is the one summary
        // an operator reads instead of opening the editor.
        html += '<span class="proj-auth-external ' + (r.external ? 'on' : 'off') + '">' + (r.external ? 'may reach outside' : 'local only') + '</span>';
        html += '<span class="proj-auth-tools">' + esc(r.tools) + '</span>';
        if (r.scope.length) html += '<span class="proj-auth-scope">' + esc(r.scope.join(' · ')) + '</span>';
        for (const warning of r.breadth) {
            html += '<span class="proj-auth-unrestricted">' + esc(warning) + '</span>';
        }
        if (r.missing.length) {
            html += '<span class="proj-auth-missing">needs a scope value for ' + esc(r.missing.join(', ')) + '</span>';
        } else if (!r.scope.length && !r.schemaUnknown) {
            html += '<span class="proj-auth-scope none">no resource scope declared by this MCP</span>';
        }
        if (r.schemaUnknown) html += '<span class="proj-auth-scope none">not connected — scope unknown</span>';
        html += '</div>';
    }
    html += renderProjMountRows(p);
    return html;
}

function renderProjects() {
    if (state.editingProjectId) return renderProjectForm();

    let html = '<div class="page-header">';
    html += '<h2>Projects &amp; Access Profiles</h2>';
    html += '<button class="btn btn-primary" onclick="newProject()">+ New</button>';
    html += '</div>';
    html += '<p class="page-intro">Both kinds are the security boundary and both get a scoped bearer token. A <strong>project</strong> is bound to a host directory and has models, shell templates and skills. An <strong>access profile</strong> is a capability grant to a client on another machine: no directory, no skills, no shell, no models — just which MCPs, which tools, which operations, whether it may reach outside this Mac, and which resources.</p>';

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
            // The host chip's absence IS the design (docs/ssh-hosts.md): a
            // console project shows nothing here at all.
            if (p.host_id) html += '<span class="proj-host-chip">⌁ ' + esc(hostNameFor(p.host_id)) + '</span>';
            html += '</div>';
            html += '<div style="display:flex;gap:4px">';
            html += '<button class="btn btn-sm" ' + bind(editProject, p.id) + '>Edit</button>';
            // Regen Skill is absent for a profile rather than disabled:
            // validateProjectShape refuses generate_skill on a remote record,
            // and the regen handler refuses a record with no path, so the
            // button could never do anything. ADR-009 decision 2's argument
            // applies to the control as much as to the flag — refusing at the
            // door is more honest than something that quietly no-ops. A host
            // project is refused for the same reason: the generator writes into
            // a directory that is not on this Mac.
            if (!remote && !p.host_id) {
                html += '<button class="btn btn-sm" ' + bind(regenProjectSkill, p.id) + ' title="Regenerate SKILL.md now">Regen Skill</button>';
            }
            html += '<button class="btn btn-sm btn-danger" ' + bind(removeProject, p.id, p.name) + '>Delete</button>';
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
        // host_id names a Host this project's Path lives on instead of this
        // Mac (docs/ssh-hosts.md). '' is the console — see setProjWhere.
        host_id: '',
        allowed_mcp_ids: [PROJ_MCP_WILDCARD],   // wildcard by default
        allowed_models: [PROJ_MCP_WILDCARD],
        allowed_templates: [],                  // none until the operator opts in
        chat_templates: [],
        permission_policy: { default_mode: '', allowed_tools: [], denied_tools: [] },
        generate_skill: false,
        disabled_tools: {},                      // mcpID -> [toolName, ...]
        // The ADR-011 permission set. access and allowed_tools are what relay
        // enforces at its own chokepoint; context is what it injects and
        // cannot verify. All three are absent by default, which for an access
        // profile means read, no tools, and no scope — every layer failing
        // closed until someone widens it deliberately.
        access: {},                              // mcpID -> 'read' | 'write'
        allowed_tools: {},                       // mcpID -> [pattern, ...]
        context: {},                             // mcpID -> { field: value }
        // The second axis (ADR-011 decision 2c). Only DISSENT from the kind's
        // default is ever stored here — refused for an access profile, allowed
        // for a local project, the same asymmetry `access` has and for the same
        // reason: a remote client has no way off this Mac except through relay,
        // and a local agent already has one.
        allow_external: {},                      // mcpID -> true | false
        // The mount-plane grant (remote/access-profile only): each row is
        // { id, path, access }. Unlike allowed_tools/context, id and path are
        // live-bound straight into this array on blur (onchange), not
        // deferred through a _xxxText sibling — a mount row's own two fields
        // have no picker and no per-key structure to reconcile, so there is
        // nothing captureProjectFormInputs needs to do for them.
        mounts: [],
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
        host_id: p.host_id || '',
        allowed_mcp_ids: (p.allowed_mcp_ids || []).slice(),
        allowed_models: (p.allowed_models || []).slice(),
        allowed_templates: (p.allowed_templates || []).slice(),
        chat_templates: JSON.parse(JSON.stringify(p.chat_templates || [])),
        permission_policy: {
            default_mode: policy.default_mode || '',
            allowed_tools: (policy.allowed_tools || []).slice(),
            denied_tools: (policy.denied_tools || []).slice(),
        },
        generate_skill: !!p.generate_skill,
        disabled_tools: JSON.parse(JSON.stringify(p.disabled_tools || {})),
        access: JSON.parse(JSON.stringify(p.access || {})),
        allowed_tools: JSON.parse(JSON.stringify(p.allowed_tools || {})),
        context: JSON.parse(JSON.stringify(p.context || {})),
        allow_external: JSON.parse(JSON.stringify(p.allow_external || {})),
        mounts: JSON.parse(JSON.stringify(p.mounts || [])),
        _scopeText: {},
        _toolsText: {},
        token: p.token || '',
    };
}

function newProject() {
    state.editingProjectId = 'new';
    state.projectForm = blankProjectForm();
    state.projectFormError = null;
    state.projectFormErrorField = null;
    openProjModelPicker();
    render();
}

function editProject(id) {
    const p = state.projects.find(x => x.id === id);
    if (!p) return;
    state.editingProjectId = id;
    state.projectForm = projectFormFromExisting(p);
    state.projectFormError = null;
    state.projectFormErrorField = null;
    openProjModelPicker();
    render();
}

function cancelProjectEdit() {
    state.editingProjectId = null;
    state.projectForm = null;
    state.projectFormError = null;
    state.projectFormErrorField = null;
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

// copyToClipboard backs every Copy button on this page (a project token, a
// login code, the CA fingerprint). WKWebView supports clipboard writes from
// a user gesture either way; execCommand is the fallback for the rare
// embedding where navigator.clipboard is absent or refuses.
function copyToClipboard(text) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text);
        return;
    }
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.focus();
    ta.select();
    try { document.execCommand('copy'); } catch (e) { /* nothing further to try */ }
    document.body.removeChild(ta);
}

// ---- Kind helpers ----
//
// A remote-kind record is an ACCESS PROFILE (ADR-011 decision 1): a capability
// grant to an agent on another machine, not a host directory. It carries no
// path, can't use generate_skill (directory-flavored), can't use
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

// isHostedForm mirrors config.Project.IsHosted (docs/ssh-hosts.md): a local
// project whose directory lives on another machine. A remote (access
// profile) form is never hosted — the Where control only appears for a
// local-kind form (see renderProjectForm).
function isHostedForm(f) {
    return !!f && !isRemoteForm(f) && !!f.host_id;
}

// setProjWhere is the project form's Where segmented control: '' selects
// This Mac, anything else names a Host. Switching TO a host clears the
// fields a host project may not carry (docs/ssh-hosts.md) so a stale value
// from a This-Mac edit can't ride along into harvestProjectForm.
function setProjWhere(hostId) {
    const f = state.projectForm;
    if (!f) return;
    f.host_id = hostId || '';
    if (f.host_id) {
        f.allowed_mcp_ids = [];
        f.generate_skill = false;
    }
    render();
}

function setProjKind(kind) {
    const f = state.projectForm;
    if (!f) return;
    f.kind = kind;
    if (kind === 'remote') {
        // host_id and kind:remote are mutually exclusive (docs/ssh-hosts.md)
        // — an access profile has no directory, hosted or otherwise.
        f.host_id = '';
        // The wildcard means "every MCP relay currently knows about" — on a
        // remote grant that would let a future MCP registration silently
        // widen what the remote client can reach, so it's not offered (see
        // the MCP section below). If the user had it set (the new-project
        // default), drop to an empty explicit list rather than letting Save
        // send something the server will reject.
        if (isProjMcpWildcard(f)) f.allowed_mcp_ids = [];
        // Remote projects always carry an empty model allowlist.
        f.allowed_models = [];
        f.allowed_templates = [];
    } else {
        openProjModelPicker();
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

// projFormAllowExternal is projAllowExternal against the in-flight form, which
// carries `kind` as a string rather than as the stored ProjectKind.
function projFormAllowExternalDefault(f) {
    return !isRemoteForm(f);
}

function projFormAllowExternal(f, mcpID) {
    const explicit = (f.allow_external || {})[mcpID];
    if (explicit === true || explicit === false) return explicit;
    return projFormAllowExternalDefault(f);
}

// setProjAllowExternal stores only DISSENT from the kind's default, and the
// deletion is load-bearing rather than tidiness. A local project's default is
// allowed, so a click on Allow that wrote an explicit true would survive a
// later conversion to an access profile — where the default is refused — and
// hand the converted profile an outbound channel nobody granted it. Storing
// only what differs from the default means settings.json carries what an
// operator actually decided, and a conversion lands on the new kind's default.
function setProjAllowExternal(mcpID, allow) {
    const f = state.projectForm;
    if (!f) return;
    if (!f.allow_external) f.allow_external = {};
    if (allow === projFormAllowExternalDefault(f)) delete f.allow_external[mcpID];
    else f.allow_external[mcpID] = allow;
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
        delete (f.allow_external || {})[mcpID];
        delete (f.allowed_tools || {})[mcpID];
        delete (f.context || {})[mcpID];
        delete (f._scopeText || {})[mcpID];
        delete (f._toolsText || {})[mcpID];
    }
    render();
}

// ---- Mounts (mount-plane grant) --------------------------------------------

function addProjMount() {
    const f = state.projectForm;
    if (!f) return;
    f.mounts.push({ id: '', path: '', access: 'read' });
    render();
}

function removeProjMount(index) {
    const f = state.projectForm;
    if (!f || !f.mounts[index]) return;
    f.mounts.splice(index, 1);
    render();
}

function setProjMountAccess(index, access) {
    const f = state.projectForm;
    if (!f || !f.mounts[index]) return;
    f.mounts[index].access = access;
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

// SCOPE_CONFIRMED_EMPTY_TEXT is an internal-only sentinel scopeFieldWasEverAsserted
// reads and confirmScopeFieldEmpty writes -- never sent to the wire, since
// scopeValueFromText parses it to [] like any other blank-ish text before
// harvest ever looks at the VALUE. What it preserves is the one bit the
// value alone cannot carry: that this particular blank was an explicit
// "I looked, there is nothing here" rather than "nothing typed". A single
// space rather than empty string for exactly that reason -- ''.trim() === ''
// is indistinguishable from never having touched the control, so the
// sentinel has to be a string that is NOT ''.
const SCOPE_CONFIRMED_EMPTY_TEXT = ' ';

// scopeFieldWasEverAsserted answers a question the text round-trip cannot on
// its own: for an array field, blank text and never-configured are the same
// string (scopeValueFromText always returns [] for empty-ish text, never
// undefined). Before the ADR-011 addendum ("A star and an empty array") that
// did not matter: [] and absent meant the same thing, so harvesting a blank
// field as [] and then dropping it (the old scopeValueIsSet) was harmless.
// Now they do not, so this has to resolve three different things a blank
// control can mean, in this order:
//
//  1. Touched THIS session, and cleared (blank, or the dedicated Clear
//     button) -- an explicit "forget this", whatever was stored before.
//     Omitted, regardless of what scopeFieldWasEverAsserted's caller would
//     otherwise do with an old stored value; see clearScopeValues.
//  2. Touched this session via confirmScopeFieldEmpty (the
//     SCOPE_CONFIRMED_EMPTY_TEXT sentinel) or with real values typed/picked
//     -- an assertion, stored as [] or as the list respectively.
//  3. Not touched this session at all -- whatever the record already held
//     stands, confirmed-empty included, which is what makes reopening an
//     already-confirmed-empty project and saving without touching it a
//     no-op rather than a silent reversion to "unset".
function scopeFieldWasEverAsserted(f, mcpID, field) {
    const typed = (f._scopeText || {})[mcpID] || {};
    if (Object.prototype.hasOwnProperty.call(typed, field.name)) {
        return typed[field.name] !== '';
    }
    return Object.prototype.hasOwnProperty.call((f.context || {})[mcpID] || {}, field.name);
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
    html += '<button class="perm-btn ' + (mode === 'read' ? 'active' : '') + '" ' + bind(setProjAccess, mcpID, 'read') + '>Read</button>';
    html += '<button class="perm-btn ' + (mode === 'write' ? 'active' : '') + '" ' + bind(setProjAccess, mcpID, 'write') + '>Write</button>';
    html += '</div>';
    html += '<p class="proj-section-help">' + (mode === 'read'
        ? 'Only tools this MCP annotates <code>readOnlyHint: true</code>. A tool that is unannotated, malformed, or added later is refused — that is what keeps a new mutating tool out of an old grant.'
        : 'Every tool this grant admits, mutating included. Write implies read.')
        + ' Unset defaults to <strong>' + (remote ? 'read' : 'write') + '</strong> for ' + (remote ? 'an access profile' : 'a local project') + '.</p>';
    html += '</div>';

    // Which side of this Mac. A separate block from Operations rather than a
    // third button in it: the two axes cross, and a control that reads as a
    // third mode would teach an operator that they are one.
    const external = projFormAllowExternal(f, mcpID);
    html += '<div class="proj-perm-block">';
    html += '<div class="proj-perm-label">Outside this Mac</div>';
    html += '<div class="perm-btns">';
    html += '<button class="perm-btn ' + (external ? '' : 'active') + '" ' + bind(setProjAllowExternal, mcpID, false) + '>Refuse</button>';
    html += '<button class="perm-btn ' + (external ? 'active' : '') + '" ' + bind(setProjAllowExternal, mcpID, true) + '>Allow</button>';
    html += '</div>';
    html += '<p class="proj-section-help">' + (external
        ? 'Tools that reach outside this Mac are allowed, <code>mail_send</code> and <code>web_fetch</code> among them. Anything this grant can read, it can send somewhere you cannot see.'
        : 'Tools that reach outside this Mac are refused — <code>mail_send</code>, <code>web_fetch</code>, anything that talks to a network or a mail server. <strong>Drafting still works, and what it buys is that nothing is delivered:</strong> <code>mail_create_draft</code> cannot reach a recipient, where <code>mail_send</code> reaches any address — a person has to open the draft and send it. It does <em>not</em> stay on this Mac: Mail uploads a draft to the account\'s own mail server, so anyone who can read that account reads it. A write profile with this refused still has an outbound write to its own account; if that matters, use a read-only profile, which has no compose tool at all.')
        + ' This is a <em>separate</em> question from Read/Write above and neither answers the other: <code>web_fetch</code> is read-only and reaches outside, <code>mail_create_draft</code> changes something and is annotated as not reaching outside.'
        + ' Unset defaults to <strong>' + (remote ? 'refused' : 'allowed') + '</strong> for ' + (remote ? 'an access profile' : 'a local project') + '. '
        + (remote
            ? 'A client on another machine has no way off this Mac except through relay, so these tools are capability it does not otherwise have. Note a tool whose MCP declares no <code>openWorldHint</code> counts as reaching outside — that is the MCP specification\'s own default — so while an MCP is unannotated, refusing this costs a profile <em>every</em> tool of it, not only the networked ones.'
            : 'An agent running here already has this Mac\'s network — it runs as you, usually with a shell, so <code>web_fetch</code> gives it nothing <code>curl</code> would not. Refuse it anyway if this project\'s agent genuinely has no other way out; that is what the explicit setting is for.')
        + '</p>';
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
    const scopeInputId = 'scopeField-' + scopeEnumValueKey([mcpID, field.name]);
    html += '<label for="' + esc(scopeInputId) + '">' + esc(field.name) + ' <span class="proj-scope-type">' + esc(typeLabel) + '</span></label>';
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
        html += '<input type="text" id="' + esc(scopeInputId) + '" readonly value="' + esc(shown) + '" placeholder="(nothing derived)" />';
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
        html += renderScopeFieldTextInput(mcpID, field, text, scopeInputId);
        if (field.enumerable) {
            html += '<div class="proj-scope-desc">' + esc(mcpID) + ' cannot list this field\'s values, so it is typed by hand. Spelling counts: a value that matches nothing refuses every tool the field governs, silently.</div>';
        }
    }
    if (!String(text).trim()) {
        html += '<div class="proj-scope-missing">No value: every tool this field governs is denied at call time.</div>';
    }
    // Issue #41: an entry that resolves to a filesystem root is not a
    // confinement, and it is one character to type. It is named here, while
    // the operator is looking at the box, as well as on the row and in the
    // save confirmation — the whole finding is that nothing said so anywhere.
    const breadth = scopeBreadthPhrase(scopeValueBreadth(scopeValueFromText(field, text)));
    if (breadth) {
        html += '<div class="proj-scope-unrestricted">This value is ' + esc(breadth) + '. '
            + 'Everything below it is in scope, including files this grant has no reason to reach. '
            + 'Set it to the narrowest directory that works unless you mean the whole tree.</div>';
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
function renderScopeFieldTextInput(mcpID, field, text, inputId) {
    const idAttr = inputId ? ' id="' + esc(inputId) + '"' : '';
    if (field.type === 'array') {
        return '<textarea' + idAttr + ' rows="3" oninput="setProjScopeText(\'' + esc(mcpID) + '\', \'' + esc(field.name) + '\', this.value)">' + esc(text) + '</textarea>';
    }
    return '<input type="text"' + idAttr + ' value="' + esc(text) + '" oninput="setProjScopeText(\'' + esc(mcpID) + '\', \'' + esc(field.name) + '\', this.value)" />';
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
    const openKey = scopeOpenKey(mcpID, fieldName);
    state.scopeEnumOpen[openKey] = !state.scopeEnumOpen[openKey];
    if (state.scopeEnumOpen[openKey]) requestScopeEnum(mcpID, field);
    render(); // captures the DOM-only name/path fields before repainting — see render()
}

function retryScopeEnum(mcpID, fieldName) {
    const field = scopeFieldByName(mcpID, fieldName);
    if (!field) return;
    requestScopeEnum(mcpID, field, true);
    render(); // captures the DOM-only name/path fields before repainting — see render()
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
    // Always visible, open or closed: what is stored is what confines the
    // client, so it never sits behind a control someone has to open.
    //
    // Zero selected is two different facts now (ADR-011 addendum, "A star
    // and an empty array"), and scopeFieldWasEverAsserted is the only way to
    // tell them apart -- see its own comment for why selected.length alone
    // cannot: never having touched this field's control refuses every tool
    // it governs, same as always; having touched it and landed on nothing
    // (an explicit "Confirm: nothing to grant here", or every box unticked)
    // is a reviewed decision that SUCCEEDS emptily instead.
    let noneMessage = 'nothing selected — every tool this field governs is refused';
    if (selected.length === 0 && scopeFieldWasEverAsserted(f, mcpID, field)) {
        noneMessage = 'confirmed empty — every tool this field governs succeeds, reaching nothing';
    }
    let html = '<div class="proj-scope-summary">' + (selected.length
        ? esc(selected.map(scopeEnumValueKey).join(', '))
        : '<span class="proj-scope-none">' + esc(noneMessage) + '</span>') + '</div>';

    const unknown = (res && res.status === 'ok') ? unrecognisedScopeValues(selected, res.values) : [];
    if (unknown.length) {
        html += '<div class="proj-scope-unrecognised">' + esc(mcpID) + ' does not offer ' + esc(unknown.map(scopeEnumValueKey).join(', '))
            + '. Kept and still in force — it may have been renamed on the host, or this MCP may be reading a different one. Untick it to remove it.</div>';
    }

    html += '<button class="btn btn-sm" ' + bind(toggleScopeFieldPicker, mcpID, field.name) + '>'
        + (open ? 'Done' : 'Choose values…') + '</button>';
    if (!open) return html;

    if (pending) return html + '<div class="proj-scope-pending">Listing values from ' + esc(mcpID) + '…</div>';
    if (!res) {
        // Open, with no answer for THESE dependency values: something the list
        // is read within was edited by hand since it was last asked. Offered as
        // a button rather than fetched from here, because a paint must never
        // start a live call — a re-render loop would be one call per frame.
        return html + '<div class="proj-scope-pending">The values this list is read within have changed.</div>'
            + '<button class="btn btn-sm" ' + bind(retryScopeEnum, mcpID, field.name) + '>List values</button>';
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
        html += '<button class="btn btn-sm" ' + bind(retryScopeEnum, mcpID, field.name) + '>Try again</button>';
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
        let html = '<div class="proj-scope-desc">' + esc(mcpID) + ' offers no values for this field'
            + (Object.keys(deps).length ? ' within the values chosen above' : '')
            + '. That is its answer, not a failure — there is nothing here to grant.</div>';
        // There is nothing to pick from, so there is no picker for "select
        // all" to operate on -- but the operator can still record that they
        // looked (ADR-011 addendum, "A star and an empty array"), which is
        // what turns this field's governed tools from a refusal into an
        // ordinary, successful empty result.
        if (multi) {
            html += '<button type="button" class="btn btn-sm" ' + bind(confirmScopeFieldEmpty, mcpID, field.name) + '>'
                + 'Confirm: nothing to grant here</button>';
        }
        return html;
    }

    const selectedKeys = new Set(selected.map(scopeEnumValueKey));
    let html = '<div class="proj-scope-choices">';
    for (const row of rows) {
        const idx = state._scopeBind.length;
        state._scopeBind.push({ mcpID: mcpID, field: field.name, value: row.value });
        const checked = selectedKeys.has(scopeEnumValueKey(row.value)) ? ' checked' : '';
        html += '<div class="proj-scope-choice' + (row.unknown ? ' unrecognised' : '') + '">';
        html += '<input type="' + (multi ? 'checkbox' : 'radio') + '" id="scopeChoice' + idx + '" name="scope-' + esc(mcpID) + '-' + esc(field.name) + '"'
            + checked + ' onchange="toggleProjScopeValueAt(' + idx + ', this.checked)" />';
        html += '<label for="scopeChoice' + idx + '">' + esc(row.label) + '</label>';
        if (row.unknown) html += '<span class="desc">not offered by this MCP</span>';
        html += '</div>';
    }
    html += '</div>';
    if (multi) {
        // Appended AFTER every per-row bind, not before: a bind index is a
        // stable handle other code (and every test pinning
        // toggleProjScopeValueAt(0, ...)) reads as "the Nth offered row",
        // and pushing this one first would shift all of them by one. The
        // value list reaches its own handler through a bind index for the
        // same reason an individual choice's does -- a mailbox path or
        // account name can carry quotes, apostrophes and emoji, and
        // building a JS array literal out of one in an HTML attribute is
        // how that becomes a bug.
        const allIdx = state._scopeBind.length;
        state._scopeBind.push({ mcpID: mcpID, field: field.name, value: rows.map(r => r.value) });
        html += '<div class="proj-scope-bulk">'
            + '<button type="button" class="btn btn-sm" ' + bind(selectAllScopeValuesAt, allIdx) + '>Select all (' + rows.length + ')</button> '
            + '<button type="button" class="btn btn-sm" ' + bind(clearScopeValues, mcpID, field.name) + '>Clear all</button>'
            + '</div>';
    }
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

// selectAllScopeValuesAt sets a field to every value CURRENTLY offered --
// no live call of its own, since the values are already in state.scopeEnum
// from the fetch that opened the picker. This is deliberately a snapshot:
// the stored array is a concrete list an operator can review, exactly what
// typing every name by hand would produce, never a stored "*" that widens on
// its own (ADR-011 decision 3 -- the addendum this button exists under
// changes what an EMPTY array means, not that). An account or a mailbox
// added to the host later does not silently join the grant; re-opening this
// picker and clicking the button again is how an operator deliberately
// re-syncs it.
function selectAllScopeValuesAt(index) {
    const bind = state._scopeBind[index];
    if (!bind) return;
    const f = state.projectForm;
    if (!f) return;
    const field = scopeFieldByName(bind.mcpID, bind.field);
    if (!field) return;
    setProjScopeText(bind.mcpID, bind.field, scopeTextFromValue(field, bind.value));
    refreshDependentScopeFields(bind.mcpID, bind.field);
    render();
}

// clearScopeValues empties a field back to nothing selected.
function clearScopeValues(mcpID, fieldName) {
    const field = scopeFieldByName(mcpID, fieldName);
    if (!field) return;
    setProjScopeText(mcpID, fieldName, scopeTextFromValue(field, field.type === 'array' ? [] : ''));
    refreshDependentScopeFields(mcpID, fieldName);
    render();
}

// confirmScopeFieldEmpty is what a field with ZERO offered values needs
// (ADR-011 addendum, "A star and an empty array"): there is nothing to
// check, so there is no picker for "Select all" to run against, but the
// operator can still explicitly record "I looked, and there is nothing
// here" -- which is what lets this field's governed tools answer emptily
// instead of refusing.
//
// Deliberately NOT the same write clearScopeValues makes, even though both
// end up meaning "the value is []": this one writes
// SCOPE_CONFIRMED_EMPTY_TEXT rather than '', so scopeFieldWasEverAsserted can
// tell "I confirmed nothing here" apart from "I cleared this back to
// unconfigured" for a field this SESSION touches. Collapsing the two was
// tried and breaks a real, deliberate behaviour: clearing a field that
// already held a real value (typed blank, or the Clear button) has to omit
// it from the payload, not silently downgrade it to a confirmed-empty grant.
function confirmScopeFieldEmpty(mcpID, fieldName) {
    const field = scopeFieldByName(mcpID, fieldName);
    if (!field) return;
    setProjScopeText(mcpID, fieldName, SCOPE_CONFIRMED_EMPTY_TEXT);
    refreshDependentScopeFields(mcpID, fieldName);
    render();
}

// captureProjectFormInputs writes back the values that live only in the DOM.
//
// A repaint rebuilds every input from state.projectForm, but the name and
// path are read from the DOM at harvest and nowhere else — nothing keeps
// state.projectForm.name in sync with a keystroke as it happens. Called from
// render() itself (the one place that repaints the form), unconditionally and
// before ANY page-specific branch, so every path that can trigger a repaint —
// an async enumeration answer, a click that grants an MCP, a tab switch, a
// picker opening — is covered by construction rather than by whichever call
// sites remembered to ask for it. relay#25 was granting an MCP re-rendering
// without asking: the picker's async answer had its own explicit call for
// exactly this, and nothing else did. Empty is treated as "leave it", the
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
    // A field-level refusal (see saveProjectForm) clears itself the moment its
    // own field holds something again, rather than sitting there stale — red
    // border and all — until the operator clicks Create a second time.
    if (state.projectFormErrorField === 'projName' && f.name.trim()) {
        state.projectFormError = null;
        state.projectFormErrorField = null;
    } else if (state.projectFormErrorField === 'projPath' && f.path && f.path.trim()) {
        state.projectFormError = null;
        state.projectFormErrorField = null;
    }
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

// openProjModelPicker starts the picker afresh for the form just opened. An
// access profile carries no models, so it asks relay for nothing.
function openProjModelPicker() {
    state.projModelSearch = '';
    state.projModelsOtherOpen = false;
    if (!isRemoteForm(state.projectForm)) requestModelCatalog();
}

// requestModelCatalog swaps only the banner for its loading state, so a Retry
// click shows progress without rebuilding the form under the operator.
function requestModelCatalog() {
    state.modelCatalogPending = true;
    ipc(JSON.stringify({ type: 'list_models' }));
    const banner = document.getElementById('projModelsBanner');
    if (banner) banner.outerHTML = renderModelPickerBanner(state.modelCatalog, true);
}

window.onModelsListed = function(view) {
    state.modelCatalog = view || null;
    state.modelCatalogPending = false;
    // A full repaint, not render('push'): the push guard would swallow this
    // answer for the whole time the form it was fetched for is open.
    if (state.page === 'projects' && state.projectForm && !isRemoteForm(state.projectForm)) render();
};

// setProjModelSearch repaints only the list so the search box keeps focus.
// Its toggles append to the live _actBind table, which must not be cleared
// here: every other control on the form still points into it.
function setProjModelSearch(text) {
    state.projModelSearch = String(text || '');
    const list = document.getElementById('projModelsList');
    if (list && state.projectForm) list.innerHTML = renderProjModelList(state.projectForm);
}

function toggleProjModel(id) {
    const f = state.projectForm;
    if (!f) return;
    f.allowed_models = f.allowed_models.includes(id)
        ? f.allowed_models.filter(x => x !== id)
        : f.allowed_models.concat(id);
    render();
}

function toggleProjModelsOther() {
    state.projModelsOtherOpen = !state.projModelsOtherOpen;
    render();
}

function renderProjModelList(f) {
    const groups = filterModelGroups(groupModelCatalog(state.modelCatalog, f.allowed_models), state.projModelSearch);
    return renderModelPickerList(groups, {
        selected: f.allowed_models,
        bindToggle: id => bind(toggleProjModel, id),
        otherOpen: state.projModelsOtherOpen,
        searching: state.projModelSearch.trim() !== '',
    });
}

function renderProjModelPicker(f) {
    let html = renderModelPickerBanner(state.modelCatalog, state.modelCatalogPending);
    html += '<input type="text" id="projModelsSearch" aria-label="Search models" placeholder="Search models" value="' + esc(state.projModelSearch) + '" oninput="setProjModelSearch(this.value)" />';
    html += '<div id="projModelsList">' + renderProjModelList(f) + '</div>';
    if (f.allowed_models.length === 0) {
        html += '<p id="projModelsEmptyNote" class="proj-section-help">Nothing selected: an empty list lets this project use every model, the same as the wildcard. Select at least one model to restrict it.</p>';
    }
    return html;
}

function isProjTemplatesWildcard(f) {
    return f.allowed_templates.length === 1 && f.allowed_templates[0] === PROJ_MCP_WILDCARD;
}

function setProjTemplatesWildcard(checked) {
    const f = state.projectForm;
    if (!f) return;
    f.allowed_templates = checked ? [PROJ_MCP_WILDCARD] : [];
    render();
}

function toggleProjTemplate(id) {
    const f = state.projectForm;
    if (!f) return;
    f.allowed_templates = f.allowed_templates.includes(id)
        ? f.allowed_templates.filter(x => x !== id)
        : f.allowed_templates.concat(id);
    render();
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
    // A refusal localized to one control (name, path — see saveProjectForm)
    // renders next to that control instead, where focusProjectFormIssue()
    // sends the cursor; this banner is for the rest — a server-side
    // validateProjectShape / validateProjectPermissions refusal keyed by MCP
    // id and field name, which has no single DOM control of its own to sit
    // beside. tabindex="-1" + the id is what lets focusProjectFormIssue()
    // still land the operator on it wherever the form is scrolled to.
    if (state.projectFormError && !state.projectFormErrorField) {
        html += '<div class="proj-error" id="projFormBanner" tabindex="-1">' + esc(state.projectFormError) + '</div>';
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
    html += '<label for="projName">' + (isRemote ? 'Profile name' : 'Project name') + '</label>';
    html += '<input type="text" id="projName" class="' + (state.projectFormErrorField === 'projName' ? 'proj-field-invalid' : '') + '" value="' + esc(f.name) + '" placeholder="' + (isRemote ? 'e.g. Hermes — Bob INBOX (read-only)' : 'e.g. Acme Website') + '" />';
    // Next to the field, not only in the top banner (relay#25): a required-
    // field refusal is common enough, and this field specifically is easy
    // enough to lose (see captureProjectFormInputs / render()), that it earns
    // its own message where the fix actually happens. focusProjectFormIssue()
    // is what puts the cursor here in the first place.
    if (state.projectFormErrorField === 'projName') {
        html += '<div class="proj-field-error">' + esc(state.projectFormError) + '</div>';
    }
    if (!isRemote) {
        const hosted = isHostedForm(f);
        // ---- Where (docs/ssh-hosts.md) ----
        // Chosen at any time, unlike Kind: moving a project's directory
        // between the console and a host is an ordinary edit, not a
        // conversion with the consequences Kind's read-only-after-create
        // rule exists for.
        html += '<label>Where</label>';
        html += '<div class="perm-btns">';
        html += '<button class="perm-btn ' + (!f.host_id ? 'active' : '') + '" ' + bind(setProjWhere, '') + '>This Mac</button>';
        for (const h of (state.hosts || [])) {
            html += '<button class="perm-btn ' + (f.host_id === h.id ? 'active' : '') + '" ' + bind(setProjWhere, h.id) + '>' + esc(h.name) + '</button>';
        }
        html += '</div>';
        // No for="projPath" here: an existing test asserts this exact literal
        // <label>...</label> text (both the console and hosted variants) and
        // is outside this change's ownership. The <input id="projPath"> right
        // below is still reachable by name via Tab order; only the explicit
        // label/control pairing is missing for this one field.
        html += '<label>' + (hosted ? 'Path on ' + esc(hostNameFor(f.host_id)) : 'Project path') + '</label>';
        html += '<input type="text" id="projPath" class="' + (state.projectFormErrorField === 'projPath' ? 'proj-field-invalid' : '') + '" value="' + esc(f.path) + '" placeholder="' + (hosted ? '/home/you/projects/acme' : '/Users/you/projects/acme') + '" />';
        if (state.projectFormErrorField === 'projPath') {
            html += '<div class="proj-field-error">' + esc(state.projectFormError) + '</div>';
        }
        html += '<p class="proj-section-help">' + (hosted
            ? 'Absolute path on ' + esc(hostNameFor(f.host_id)) + '. Relay never checks whether it exists — the host does that when a session or terminal opens it.'
            : 'Absolute path. Filesystem MCPs are auto-scoped to this directory.') + '</p>';
    } else {
        html += '<p class="proj-section-help">An access profile is a capability grant to an agent on another machine. It has no host directory, so path, skills, shell templates and models do not apply — what it carries is which MCPs, which tools, which operations, whether it may reach outside this Mac, and which resources.</p>';
    }
    html += '</div>';

    // ---- Allowed MCPs + tri-state picker ----
    // Absent (not disabled) for a hosted project, matching how this whole
    // section is already absent for an access profile below: relay-brokered
    // tools live on the console only in v1 (docs/ssh-hosts.md decision 6),
    // so the picker would offer a grant that could never do anything.
    if (isHostedForm(f)) {
        html += '<div class="proj-section">';
        html += '<div class="proj-section-title">MCPs, Tools, Operations, External Access &amp; Resources</div>';
        html += '<p class="proj-section-help">Relay tools aren\'t available on a host yet — the agent uses its built-in tools there.</p>';
        html += '</div>';
    } else {
    const wild = !isRemote && isProjMcpWildcard(f);
    html += '<div class="proj-section">';
    html += '<div class="proj-section-title">MCPs, Tools, Operations, External Access &amp; Resources</div>';
    if (!isRemote) {
        html += '<div class="toggle-row" style="padding:4px 0;margin:0">';
        html += '<span>Allow all registered MCPs (wildcard <code>*</code>)</span>';
        html += '<label class="switch"><input type="checkbox" aria-label="Allow all registered MCPs (wildcard *)" ' + (wild ? 'checked' : '') + ' onchange="setProjMcpWildcard(this.checked)" /><span class="slider"></span></label>';
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
                html += '<button class="perm-btn ' + (granted ? 'active' : '') + '" ' + bind(setProjMcpGranted, mcp.id, true) + '>Granted</button>';
                html += '<button class="perm-btn ' + (!granted ? 'active' : '') + '" ' + bind(setProjMcpGranted, mcp.id, false) + '>Not granted</button>';
            } else {
                html += '<button class="perm-btn ' + (st === 'all' ? 'active' : '') + '" ' + bind(setProjMcpState, mcp.id, 'all') + '>All tools</button>';
                html += '<button class="perm-btn ' + (st === 'selected' ? 'active' : '') + '" ' + bind(setProjMcpState, mcp.id, 'selected') + '>Selected</button>';
                html += '<button class="perm-btn ' + (st === 'none' ? 'active' : '') + '" ' + bind(setProjMcpState, mcp.id, 'none') + '>No tools</button>';
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
            html += '<button class="perm-btn" ' + bind(setProjMcpState, id, 'none') + '>Remove</button>';
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
    }

    // ---- Mounts (mount-plane grant) ----
    // Remote-only, the same way path is local-only: a mount exposes a host
    // directory to a client on another machine as a real filesystem, over
    // relayfs — the counterpart to a local project already having shell +
    // fsMCP access to its own path. ValidateMounts refuses a non-empty list
    // on a kind:local project, so the section is absent here rather than
    // shown-then-refused-on-save.
    if (isRemote) {
        html += '<div class="proj-section">';
        html += '<div class="proj-section-title">Mounts</div>';
        html += '<p class="proj-section-help">Exposes a host directory to this client as a real POSIX filesystem mount (relayfs), instead of through an MCP\'s curated tool surface. Each mount needs an id unique on this profile (what the client names in its attach request), an absolute host path, and read or write.</p>';
        if (f.mounts.length === 0) {
            html += '<div class="proj-tool-empty">No mounts yet.</div>';
        }
        for (let i = 0; i < f.mounts.length; i++) {
            const m = f.mounts[i];
            const breadth = scopeBreadthPhrase(scopeEntryBreadth(m.path || ''));
            html += '<div class="proj-mcp-row" style="align-items:flex-start;flex-wrap:wrap;gap:8px">';
            html += '<div style="display:flex;flex-direction:column;gap:4px;flex:1;min-width:220px">';
            html += '<input type="text" id="projMountId_' + i + '" value="' + esc(m.id || '') + '" placeholder="mount id, e.g. src" onchange="state.projectForm.mounts[' + i + '].id = this.value" />';
            html += '<input type="text" id="projMountPath_' + i + '" value="' + esc(m.path || '') + '" placeholder="/absolute/host/path" onchange="state.projectForm.mounts[' + i + '].path = this.value" />';
            if (breadth) html += '<span style="color:#b45309;font-size:12px">' + esc(breadth) + '</span>';
            html += '</div>';
            html += '<div class="perm-btns">';
            html += '<button class="perm-btn ' + (m.access !== 'write' ? 'active' : '') + '" ' + bind(setProjMountAccess, i, 'read') + '>Read</button>';
            html += '<button class="perm-btn ' + (m.access === 'write' ? 'active' : '') + '" ' + bind(setProjMountAccess, i, 'write') + '>Write</button>';
            html += '</div>';
            html += '<button class="btn btn-sm btn-danger" ' + bind(removeProjMount, i) + '>Remove</button>';
            html += '</div>';
        }
        html += '<div style="margin-top:8px"><button class="btn btn-sm" onclick="addProjMount()">Add mount</button></div>';
        html += '</div>';
    }

    // ---- Allowed models ----
    html += '<div class="proj-section">';
    html += '<div class="proj-section-title">Allowed Models</div>';
    if (isRemote) {
        html += '<p class="proj-section-help">Not applicable to an access profile — the model allowlist stays empty.</p>';
    } else {
        const modelsWild = isProjModelsWildcard(f);
        html += '<div class="toggle-row" style="padding:4px 0;margin:0">';
        html += '<span>Allow all models (wildcard <code>*</code>)</span>';
        html += '<label class="switch"><input type="checkbox" aria-label="Allow all models (wildcard *)" ' + (modelsWild ? 'checked' : '') + ' onchange="setProjModelsWildcard(this.checked)" /><span class="slider"></span></label>';
        html += '</div>';
        if (!modelsWild) html += renderProjModelPicker(f);
    }
    html += '</div>';

    // ---- Allowed templates ----
    html += '<div class="proj-section">';
    html += '<div class="proj-section-title">Templates</div>';
    if (isRemote) {
        html += '<p class="proj-section-help">Not applicable to an access profile — it launches nothing.</p>';
    } else if (isHostedForm(f)) {
        // allowed_templates gates console templates only; a host project
        // may launch every template of its host (docs/ssh-hosts.md).
        html += '<p class="proj-section-help">A host project uses its host\'s terminal templates — edit them on the Hosts tab.</p>';
    } else {
        const tplWild = isProjTemplatesWildcard(f);
        html += '<p class="proj-section-help">The launch templates this project may run: terminals, and the claude-code, pi and chat templates that claude, pi and chat sessions read. None selected means the project can launch nothing.</p>';
        html += '<div class="toggle-row" style="padding:4px 0;margin:0">';
        html += '<span>Allow all templates (wildcard <code>*</code>)</span>';
        html += '<label class="switch"><input type="checkbox" aria-label="Allow all templates (wildcard *)" ' + (tplWild ? 'checked' : '') + ' onchange="setProjTemplatesWildcard(this.checked)" /><span class="slider"></span></label>';
        html += '</div>';
        if (!tplWild) {
            for (const t of state.templates || []) {
                html += '<label class="proj-mcp-row"><input type="checkbox" ' + bind(toggleProjTemplate, t.id) + ' ' + (f.allowed_templates.includes(t.id) ? 'checked' : '') + ' /> ' + esc(t.name) + ' <code>' + esc(t.id) + '</code></label>';
            }
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
    // Absent for an access profile, for the same reason the skill toggle is:
    // the model refuses it now, so a control here would be one whose only
    // outcome is a refusal on Save. A profile that still carries a policy is
    // told, because saving clears it.
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
    html += '<label for="projPolicyMode">Default mode</label>';
    html += '<select id="projPolicyMode" onchange="state.projectForm.permission_policy.default_mode = this.value">';
    for (const m of ['', 'default', 'acceptEdits', 'plan', 'bypassPermissions']) {
        const sel = pol.default_mode === m ? 'selected' : '';
        html += '<option value="' + esc(m) + '" ' + sel + '>' + (m || '(inherit)') + '</option>';
    }
    html += '</select>';
    html += '<label for="projAllowedTools">Allowed tools (one per line)</label>';
    html += '<textarea id="projAllowedTools" rows="3" placeholder="Read&#10;Grep&#10;Bash(ls *)">' + esc(pol.allowed_tools.join('\n')) + '</textarea>';
    html += '<label for="projDeniedTools">Denied tools (one per line)</label>';
    html += '<textarea id="projDeniedTools" rows="3" placeholder="Bash(rm *)&#10;Write">' + esc(pol.denied_tools.join('\n')) + '</textarea>';
    html += '</div>';
    }

    // ---- Skill ----
    // Skills are written under <path>/.claude/skills — no path, no skill, so
    // the whole section is absent (not disabled) for a remote project rather
    // than showing a toggle that would lie about what it does.
    if (!isRemote && !isHostedForm(f)) {
        html += '<div class="proj-section">';
        html += '<div class="proj-section-title">Skill (CLAUDE.md / SKILL.md)</div>';
        html += '<p class="proj-section-help">When enabled, relay regenerates <code>&lt;path&gt;/.claude/skills/relay/SKILL.md</code> on project save and MCP changes so Claude Code can discover this project\'s tools.</p>';
        html += '<div class="toggle-row" style="padding:4px 0;margin:0">';
        html += '<span>Auto-generate SKILL.md</span>';
        html += '<label class="switch"><input type="checkbox" aria-label="Auto-generate SKILL.md" ' + (f.generate_skill ? 'checked' : '') + ' onchange="state.projectForm.generate_skill = this.checked" /><span class="slider"></span></label>';
        html += '</div>';
        if (!isNew) {
            html += '<div style="margin-top:8px"><button class="btn btn-sm" ' + bind(regenProjectSkill, f.id) + '>Regenerate now</button></div>';
            const regen = state.projectSkillRegen[f.id];
            if (regen) {
                const cls = regen.ok ? 'proj-ok' : 'proj-error';
                html += '<div class="' + cls + '">' + (regen.ok ? '✓ Regenerated: ' : '✗ Regen failed: ') + esc(regen.message) + '</div>';
            }
        }
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
        html += '<button class="btn btn-sm" ' + bind(toggleProjectTokenVisible, f.id) + '>' + (visible ? 'Hide' : 'Show') + '</button>';
        html += '<button class="btn btn-sm" ' + bind(copyToClipboard, f.token) + '>Copy</button>';
        html += '<button class="btn btn-sm btn-danger" ' + bind(rotateProjectToken, f.id, f.name) + '>Rotate</button>';
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
    html += '<button class="btn btn-primary" onclick="saveProjectForm()" ' + (state.projectSavePending ? 'disabled' : '') + '>' + (state.projectSavePending ? 'Saving…' : (isNew ? 'Create' : 'Save')) + '</button>';
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
    const hosted = isHostedForm(f);
    const name = (document.getElementById('projName') || {}).value || f.name;
    // A remote project has no path control in the form (see renderProjectForm)
    // and must not send one — validateProjectShape rejects any non-empty path
    // on a remote project.
    let allowedModels = f.allowed_models.slice();
    if (isRemote) {
        allowedModels = [];
    } else if (isProjModelsWildcard(f)) {
        allowedModels = [PROJ_MCP_WILDCARD];
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
        // host_id: '' moves (or keeps) the project on the console —
        // validateHostShape refuses allowed_mcp_ids and the two flags below
        // on a host project, so this harvest forces all three to the value
        // that will pass regardless of stale form state, the same discipline
        // the remote branch already applies to itself.
        host_id: isRemote ? '' : f.host_id,
        allowed_mcp_ids: hosted ? [] : f.allowed_mcp_ids,
        allowed_models: allowedModels,
        allowed_templates: isRemote ? [] : f.allowed_templates,
        permission_policy: policy,
        // Directory-flavored and meaningless without a console path; force
        // it off for remote AND for a hosted project regardless of stale
        // form state.
        generate_skill: (isRemote || hosted) ? false : f.generate_skill,
        disabled_tools: f.disabled_tools,
        // Local-project mounts must not be sent even if a stray row survived
        // a kind switch on a still-open new-project form — ValidateMounts
        // refuses non-empty mounts on kind:local, and this is the harvest
        // that has to make that unreachable rather than an error to hit.
        // Blank rows (Add clicked, never filled in) are dropped rather than
        // sent — a convenience, same as everywhere else in this function;
        // the server is still what actually validates a filled-in row.
        mounts: isRemote
            ? f.mounts
                .filter(m => (m.id || '').trim() && (m.path || '').trim())
                .map(m => ({ id: m.id.trim(), path: m.path.trim(), access: m.access === 'write' ? 'write' : 'read' }))
            : [],
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
        payload.allow_external = perms.allow_external;
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
    const allowExternal = {};
    const allowedTools = {};
    const context = {};

    for (const mcpID of granted) {
        const mode = (f.access || {})[mcpID];
        if (mode === 'read' || mode === 'write') access[mcpID] = mode;
        // Whatever the form holds, both values — setProjAllowExternal has
        // already reduced it to dissent from the kind's default, so an entry
        // here is something an operator said rather than something a click
        // happened to write.
        const ext = (f.allow_external || {})[mcpID];
        if (ext === true || ext === false) allowExternal[mcpID] = ext;

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
            // A field this session never touched AND that was never stored
            // stays omitted, full stop -- scopeFieldWasEverAsserted is what
            // keeps that true now that blank text and confirmed-empty are
            // both spelled [] (see its own comment).
            if (!scopeFieldWasEverAsserted(f, mcpID, field)) { delete existing[field.name]; continue; }
            const value = scopeValueFromText(field, projScopeText(f, mcpID, field));
            // scopeValueIsAsserted, not scopeValueIsSet: an explicit empty
            // array is the confirmed-empty grant and must reach the wire as
            // [], not be silently dropped back to "field absent" here.
            if (scopeValueIsAsserted(value)) existing[field.name] = value;
            else delete existing[field.name];
        }
        if (Object.keys(existing).length) context[mcpID] = existing;
    }
    return { access: access, allow_external: allowExternal, allowed_tools: allowedTools, context: context, complete: complete };
}

// focusProjectFormIssue puts the cursor, and the scroll position, on whatever
// a refused Create/Save just explained — wherever in a long, scrolled form
// the operator happened to be. A refusal localized to one control (name,
// path — see saveProjectForm) focuses that control directly, next to which
// renderProjectForm has already printed the reason. A refusal this form
// cannot localize to a single input (a server-side validateProjectShape /
// validateProjectPermissions refusal, keyed by MCP id and field name rather
// than by a DOM id this form controls) focuses the banner instead, via the
// tabindex it carries for exactly this. Either way the point is the same one
// relay#25 was filed over: an error string sitting off-screen at the top of a
// tall dialog is indistinguishable, to the operator looking at the Create
// button, from no error at all.
function focusProjectFormIssue() {
    const id = state.projectFormErrorField;
    const el = (id && document.getElementById(id)) || document.getElementById('projFormBanner');
    if (!el) return;
    if (typeof el.scrollIntoView === 'function') el.scrollIntoView({ block: 'center' });
    if (typeof el.focus === 'function') el.focus({ preventScroll: true });
}

function saveProjectForm() {
    const f = state.projectForm;
    if (!f) return;
    const payload = harvestProjectForm();
    if (!payload) return;
    if (!payload.name) {
        state.projectFormError = 'Project name is required';
        state.projectFormErrorField = 'projName';
        render();
        focusProjectFormIssue();
        return;
    }
    if (payload.kind !== 'remote' && !payload.path) {
        state.projectFormError = 'Project path is required';
        state.projectFormErrorField = 'projPath';
        render();
        focusProjectFormIssue();
        return;
    }
    state.projectFormError = null;
    state.projectFormErrorField = null;

    // Issue #41, priority 3: a scope entry that resolves to a filesystem root
    // must be spelled out, not typed past. fsMCP documents `--allowed-dir /`
    // as a deliberate opt-out that "must be spelled out explicitly"; on a CLI
    // typing it IS the spelling out, and in a text box it is not. This is the
    // UI's equivalent. It is a confirmation and never a refusal — an operator
    // who means it can mean it, and a grant of "/" is legal.
    if (!confirmBroadScope(payload)) return;

    state.projectSavePending = true;
    if (!f.id) {
        ipc(JSON.stringify(Object.assign({ type: 'create_project' }, payload)));
    } else {
        ipc(JSON.stringify(Object.assign({ type: 'update_project', id: f.id }, payload)));
    }
    render();
}

// confirmBroadScope asks once, before saving, about every scope value in the
// payload that reaches further than a folder — naming the MCP, the field and
// the phrase every other surface uses. Returns true when there is nothing to
// ask about, so the ordinary save path is unchanged.
//
// It reads the PAYLOAD rather than the form state, so what it asks about is
// exactly what is about to be stored.
function confirmBroadScope(payload) {
    const findings = [];
    const context = (payload && payload.context) || {};
    for (const mcpID of Object.keys(context).sort()) {
        const values = context[mcpID] || {};
        for (const field of Object.keys(values).sort()) {
            const phrase = scopeBreadthPhrase(scopeValueBreadth(values[field]));
            if (phrase) findings.push(mcpID + ' · ' + field + ' is ' + phrase);
        }
    }
    const mounts = (payload && payload.mounts) || [];
    for (const m of mounts) {
        const phrase = scopeBreadthPhrase(scopeEntryBreadth(m.path || ''));
        if (phrase) findings.push('mount ' + m.id + ' is ' + phrase);
    }
    if (!findings.length) return true;
    return confirm(
        'This grant is broader than a folder:\n\n  ' + findings.join('\n  ') + '\n\n'
        + 'Everything below those roots is in scope for every tool the field governs — '
        + 'for an access profile, that is a client on another machine.\n\n'
        + 'Save it anyway?');
}

// ---- Project IPC event handlers ----

window.onProjectAdded = function(p) {
    if (!p || !p.id) return;
    // Replace any provisional entry with the real persisted row.
    state.projects = state.projects.filter(x => x.id !== p.id).concat(p);
    state.projectSavePending = false;
    state.editingProjectId = null;
    state.projectForm = null;
    state.projectError = null;
    if (state.page === 'projects') render('push');
};

window.onProjectUpdated = function(p) {
    if (!p || !p.id) return;
    state.projects = state.projects.map(x => x.id === p.id ? p : x);
    state.projectSavePending = false;
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
        // Not render('push'): the push guard exists to stop an UNRELATED
        // external change from wiping keystrokes mid-edit, but this answer is
        // the direct result of the operator's own click ("Selected" on this
        // MCP) — routed through 'push' it would never appear at all while the
        // form is open, since render()'s projects branch bails out before
        // painting anything whenever fromPush && editingProjectId. render()
        // itself now captures the DOM-only name/path fields before every
        // repaint, so a plain render() here is exactly as safe as a push one.
        render();
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
    // the operator opening a control and it must appear, and the push guard
    // would otherwise swallow it while the form is open. render() itself now
    // captures the DOM-only name/path fields before every repaint (see
    // render()), which is what makes a plain repaint here safe.
    render();
};

window.onProjectError = function(msg) {
    state.projectError = msg;
    state.projectFormError = msg;
    state.projectFormErrorField = null;
    if (state.page !== 'projects') return;
    // Not render('push'). This event is the direct answer to the operator's
    // own Create/Save click, not an unrelated external change — but routed
    // through 'push' it hit the exact guard that change is there to enforce:
    // render()'s projects branch returns before painting anything whenever
    // fromPush && editingProjectId, which is true for the entire time a
    // refused Create's response is in flight. So a security refusal from
    // validateProjectShape / validateProjectPermissions (or a plain client-
    // side "name is required") reached state.projectFormError correctly and
    // was never painted: the dialog sat there looking complete (relay#25).
    // render() itself now captures the DOM-only name/path fields before every
    // repaint, so a plain render() here costs nothing a push would have saved.
    render();
    focusProjectFormIssue();
};

// ---------------------------------------------------------------------------
// Hosts tab (docs/ssh-hosts.md) — a host is a machine reached over ssh that a
// project's directory can live on instead of the console. Rows are seeded by
// the initial payload like projects and enrolments are: the list is small,
// and there is no loading state worth showing for it.
// ---------------------------------------------------------------------------

function hostNameFor(hostId) {
    if (!hostId) return '';
    const h = (state.hosts || []).find(x => x.id === hostId);
    return h ? h.name : hostId;
}

function renderHosts() {
    if (state.editingHostId) return renderHostForm();

    let html = '<div class="page-header">';
    html += '<h2>Hosts</h2>';
    html += '<button class="btn btn-primary" onclick="newHost()">+ Add host</button>';
    html += '</div>';
    html += '<p class="page-intro">A host is a machine you reach over ssh; projects can live on one.</p>';

    if (state.hostError) html += '<div class="proj-error">' + esc(state.hostError) + '</div>';

    if ((state.hosts || []).length === 0) {
        html += '<div class="empty-state">No hosts yet. A host is a machine you reach over ssh; projects can live on one. <button class="btn btn-sm btn-primary" onclick="newHost()">Add host</button></div>';
        return html;
    }

    for (const h of state.hosts) {
        const pending = !!state.hostProbePending[h.id];
        html += '<div class="proj-card">';
        html += '<div class="proj-card-header">';
        html += '<div style="display:flex;align-items:center;gap:8px">';
        html += '<span class="proj-card-name">' + esc(h.name) + '</span>';
        html += renderHostStatus(h.status);
        html += '</div>';
        html += '<div style="display:flex;gap:4px">';
        html += '<button class="btn btn-sm" ' + bind(probeHost, h.id) + ' ' + (pending ? 'disabled' : '') + '>' + (pending ? 'Probing…' : 'Probe') + '</button>';
        if (h.status === 'connected') {
            html += '<button class="btn btn-sm" ' + bind(disconnectHost, h.id) + '>Disconnect</button>';
        }
        html += '<button class="btn btn-sm" ' + bind(editHost, h.id) + '>Edit</button>';
        html += '<button class="btn btn-sm btn-danger" ' + bind(removeHost, h.id, h.name) + '>Remove</button>';
        html += '</div></div>';
        html += '<div class="proj-card-path">' + esc(h.target) + (h.port ? ':' + h.port : '') + '</div>';
        html += '<div class="proj-card-meta"><span>' + renderHostProbeSummary(h) + '</span></div>';
        html += '</div>';
    }
    return html;
}

function renderHostStatus(status) {
    const label = status || 'unknown';
    return '<span class="host-status"><span class="host-status-dot ' + esc(label) + '"></span>' + esc(label) + '</span>';
}

// renderHostProbeSummary is the list row's one-line answer to "does this
// still work" — the probe's own error when the last one failed, or three
// pills (docs/ssh-hosts.md's "OS/arch · node vX · claude vY") when it
// didn't: a plain fact (OS/arch), then node and claude each colored by
// whether relay can actually use them here.
function renderHostProbeSummary(h) {
    const p = h.probe;
    if (!p) return 'Never probed.';
    if (!p.ok) return '<span class="proj-error" style="margin:0">' + esc(p.error || 'unreachable') + '</span>';
    let html = '';
    if (p.os) html += '<span class="pill muted">' + esc(p.os) + (p.arch ? '/' + esc(p.arch) : '') + '</span> ';
    html += p.node_path
        ? '<span class="pill ok">node ' + esc(p.node_version || '') + '</span> '
        : '<span class="pill warn">node missing</span> ';
    html += p.claude_path
        ? '<span class="pill ok">claude ' + esc(p.claude_version || '') + '</span>'
        : '<span class="pill muted">claude missing</span>';
    return html;
}

// ---------------------------------------------------------------------------
// Templates tab (internal/config/templates.go)
// ---------------------------------------------------------------------------
//
// Edits Settings.TerminalTemplates. A project's own ShellTemplates have no
// editor. Every successful mutation answers with a fresh onTemplatesListed, so
// that event is also what closes the form.

function templateCommandLine(t) {
    const parts = [t.command || '(default shell)'].concat(t.args || []);
    return parts.map(esc).join(' ');
}

function renderTemplates() {
    if (state.editingTemplateId) return renderTemplateForm();

    let html = '<div class="page-header"><h2>Templates</h2>';
    html += '<button class="btn btn-primary" onclick="newTemplate()">+ Add template</button></div>';
    html += '<p class="page-intro">Launch configs for project terminals: argv, env passthrough and the sandbox/model-key defaults a session inherits unless a project\'s own Shell Templates override them.</p>';
    if (state.templateError) html += '<div class="proj-error">' + esc(state.templateError) + '</div>';

    if ((state.templates || []).length === 0) {
        html += '<div class="empty-state">No templates.</div>';
        return html;
    }

    for (const t of state.templates) {
        html += '<div class="proj-card">';
        html += '<div class="proj-card-header">';
        html += '<div style="display:flex;align-items:center;gap:8px">';
        html += '<span class="proj-card-name">' + esc(t.name) + '</span>';
        if (t.builtIn) html += '<span class="pill muted">built-in</span>';
        if (t.sandbox) html += '<span class="pill ok">sandboxed</span>';
        if (t.model_key) html += '<span class="pill ok">model key</span>';
        html += '</div>';
        html += '<div style="display:flex;gap:4px">';
        html += '<button class="btn btn-sm" ' + bind(editTemplate, t.id) + '>Edit</button>';
        html += '<button class="btn btn-sm btn-danger" ' + bind(removeTemplate, t.id, t.name) + '>Remove</button>';
        html += '</div></div>';
        html += '<div class="proj-card-path">' + templateCommandLine(t) + '</div>';
        if (t.description) html += '<div class="proj-card-meta"><span>' + esc(t.description) + '</span></div>';
        html += '</div>';
    }
    return html;
}

function templateLines(text) {
    return text.split('\n').map(s => s.trim()).filter(Boolean);
}

function blankTemplateForm() {
    return { id: '', name: '', description: '', icon: '', command: '', args: '', env: '', env_passthrough: '', idleTimeout: '', sandbox: false, model_key: false, read: '', read_write: '', deny: '' };
}

function templateFormFromExisting(t) {
    const lines = a => (a || []).join('\n');
    return {
        id: t.id, name: t.name || '', description: t.description || '', icon: t.icon || '',
        command: t.command || '', args: lines(t.args),
        env: Object.keys(t.env || {}).map(k => k + '=' + t.env[k]).join('\n'),
        env_passthrough: lines(t.env_passthrough),
        idleTimeout: t.idleTimeout ? String(t.idleTimeout) : '',
        sandbox: !!t.sandbox, model_key: !!t.model_key,
        read: lines(t.read), read_write: lines(t.read_write), deny: lines(t.deny),
    };
}

function newTemplate() {
    state.editingTemplateId = 'new';
    state.templateForm = blankTemplateForm();
    state.templateFormError = null;
    render();
}

function editTemplate(id) {
    const t = (state.templates || []).find(x => x.id === id);
    if (!t) return;
    state.editingTemplateId = id;
    state.templateForm = templateFormFromExisting(t);
    state.templateFormError = null;
    render();
}

function cancelTemplateEdit() {
    state.editingTemplateId = null;
    state.templateForm = null;
    state.templateFormError = null;
    render();
}

// Same reason as captureHostFormInputs: the inputs live only in the DOM
// between renders.
function captureTemplateFormInputs() {
    const f = state.templateForm;
    for (const k of Object.keys(f)) {
        const el = document.getElementById('tpl_' + k);
        if (!el) continue;
        f[k] = el.type === 'checkbox' ? el.checked : el.value;
    }
}

function removeTemplate(id, name) {
    if (!confirm('Remove template "' + name + '"?')) return;
    ipc(JSON.stringify({ type: 'remove_template', id }));
}

function saveTemplateForm() {
    const f = state.templateForm;
    if (!f) return;
    captureTemplateFormInputs();
    const env = {};
    for (const line of templateLines(f.env)) {
        const i = line.indexOf('=');
        if (i < 1) {
            state.templateFormError = 'Env lines must be KEY=value: ' + line;
            render();
            return;
        }
        env[line.slice(0, i).trim()] = line.slice(i + 1);
    }
    const isNew = state.editingTemplateId === 'new';
    const payload = {
        type: isNew ? 'create_template' : 'update_template',
        id: f.id.trim(), name: f.name.trim(), description: f.description.trim(), icon: f.icon.trim(),
        command: f.command.trim(), args: templateLines(f.args), env,
        env_passthrough: templateLines(f.env_passthrough),
        sandbox: f.sandbox, model_key: f.model_key,
        read: templateLines(f.read), read_write: templateLines(f.read_write), deny: templateLines(f.deny),
    };
    const idle = parseInt(f.idleTimeout, 10);
    if (!isNaN(idle)) payload.idleTimeout = idle;
    state.templateFormError = null;
    state.templateSaving = true;
    ipc(JSON.stringify(payload));
    render();
}

function renderTemplateForm() {
    const f = state.templateForm;
    if (!f) return '<div class="empty-state">No form state.</div>';
    const isNew = state.editingTemplateId === 'new';
    const input = (key, label, placeholder, disabled) =>
        '<label for="tpl_' + key + '">' + label + '</label>' +
        '<input type="text" id="tpl_' + key + '" value="' + esc(f[key]) + '" placeholder="' + esc(placeholder || '') + '"' + (disabled ? ' disabled' : '') + ' />';
    const lines = (key, label, placeholder) =>
        '<label for="tpl_' + key + '">' + label + '</label>' +
        '<textarea id="tpl_' + key + '" rows="3" placeholder="' + placeholder + '">' + esc(f[key]) + '</textarea>';
    const toggle = (key, label) =>
        '<div class="toggle-row"><span>' + label + '</span><label class="switch"><input type="checkbox" id="tpl_' + key + '" aria-label="' + esc(label) + '" ' + (f[key] ? 'checked' : '') + ' /><span class="slider"></span></label></div>';

    let html = '<h2>' + (isNew ? 'Add template' : 'Edit template') + '</h2>';
    if (state.templateFormError) html += '<div class="proj-error" tabindex="-1">' + esc(state.templateFormError) + '</div>';

    html += '<div class="proj-section"><div class="proj-section-title">Identity</div>';
    html += input('id', 'ID', 'shell', !isNew);
    html += input('name', 'Name', 'Shell');
    html += input('description', 'Description', 'optional');
    html += input('icon', 'Icon', 'optional, e.g. shell');
    html += '</div>';

    html += '<div class="proj-section"><div class="proj-section-title">Launch</div>';
    html += input('command', 'Command', 'empty = the default shell');
    html += lines('args', 'Arguments (one per line)', '--flag&#10;${PROJECT_PATH}');
    html += lines('env', 'Environment (KEY=value per line)', 'DEBUG=true');
    html += lines('env_passthrough', 'Host variables to pass through (one per line)', 'ANTHROPIC_API_KEY');
    html += input('idleTimeout', 'Idle timeout (minutes)', '1440');
    html += toggle('model_key', 'Mint a model key (${MODEL_KEY} in env)');
    html += '</div>';

    html += '<div class="proj-section"><div class="proj-section-title">Sandbox</div>';
    html += '<p class="proj-section-help">A sandboxed launch can reach only the project directory, the system baseline and the folders below. Entries are absolute paths or start with ~. Deny wins over every grant.</p>';
    html += toggle('sandbox', 'Sandboxed');
    html += lines('read', 'Read-only folders (one per line)', '~/.local/bin');
    html += lines('read_write', 'Read-write folders (one per line)', '~/.claude');
    html += lines('deny', 'Denied paths (one per line)', '~/.ssh');
    html += '</div>';

    html += '<div class="proj-form-actions">';
    html += '<button class="btn btn-primary" onclick="saveTemplateForm()" ' + (state.templateSaving ? 'disabled' : '') + '>Save</button>';
    html += '<button class="btn btn-danger" onclick="cancelTemplateEdit()">Cancel</button>';
    html += '</div>';
    return html;
}

window.onTemplatesListed = function(templates) {
    state.templates = templates || [];
    if (state.templateSaving) {
        state.templateSaving = false;
        state.editingTemplateId = null;
        state.templateForm = null;
    }
    state.templateError = null;
    render('push');
};

window.onTemplateError = function(msg) {
    state.templateSaving = false;
    state.templateError = msg;
    state.templateFormError = msg;
    render();
};

function blankHostForm() {
    return { id: null, name: '', target: '', port: '', identity_file: '', tmux_path: '' };
}

function hostFormFromExisting(h) {
    return {
        id: h.id,
        name: h.name || '',
        target: h.target || '',
        port: h.port ? String(h.port) : '',
        identity_file: h.identity_file || '',
        tmux_path: h.tmux_path || '',
    };
}

function newHost() {
    state.editingHostId = 'new';
    state.hostForm = blankHostForm();
    state.hostFormError = null;
    render();
}

function editHost(id) {
    const h = (state.hosts || []).find(x => x.id === id);
    if (!h) return;
    state.editingHostId = id;
    state.hostForm = hostFormFromExisting(h);
    state.hostFormError = null;
    closeHostTemplateForm();
    render();
    // Same reason the Templates tab re-fetches on every visit: an HTTP edit
    // or a probe's seeding changes the list behind the UI's back.
    ipc(JSON.stringify({ type: 'list_host_templates', host_id: id }));
}

function cancelHostEdit() {
    state.editingHostId = null;
    state.hostForm = null;
    state.hostFormError = null;
    closeHostTemplateForm();
    render();
}

// captureHostFormInputs mirrors captureProjectFormInputs: the form's inputs
// live only in the DOM between renders, so any repaint that rebuilds them
// from state.hostForm without reading the DOM first would erase whatever was
// typed (relay#25's argument, applied here too).
function captureHostFormInputs() {
    const f = state.hostForm;
    if (!f) return;
    const val = id => {
        const el = document.getElementById(id);
        return el && typeof el.value === 'string' ? el.value : '';
    };
    f.name = val('hostName') || f.name;
    f.target = val('hostTarget') || f.target;
    f.port = val('hostPort');
    f.identity_file = val('hostIdentityFile');
    f.tmux_path = val('hostTmuxPath');
}

function harvestHostForm() {
    const name = (document.getElementById('hostName') || {}).value || '';
    const target = (document.getElementById('hostTarget') || {}).value || '';
    const portStr = (document.getElementById('hostPort') || {}).value || '';
    const identityFile = (document.getElementById('hostIdentityFile') || {}).value || '';
    const tmuxPath = (document.getElementById('hostTmuxPath') || {}).value || '';
    // Always sent, even empty: on update an empty value is how the operator
    // clears the override and falls back to the probed tmux.
    const payload = { name: name.trim(), target: target.trim(), tmux_path: tmuxPath.trim() };
    const port = parseInt(portStr, 10);
    if (portStr.trim() && !isNaN(port)) payload.port = port;
    if (identityFile.trim()) payload.identity_file = identityFile.trim();
    return payload;
}

// saveHostForm creates or updates the host. Create always probes
// synchronously server-side; update re-probes only when target/port/
// identity_file changed (docs/ssh-hosts.md) — either way the result rides
// back on onHostAdded/onHostUpdated, so there is nothing more to do here but
// wait.
function saveHostForm() {
    const f = state.hostForm;
    if (!f) return;
    const payload = harvestHostForm();
    if (!payload.name) {
        state.hostFormError = 'Host name is required';
        render();
        return;
    }
    if (!payload.target) {
        state.hostFormError = 'SSH target is required';
        render();
        return;
    }
    const isNew = !f.id;
    state.hostProbePending[isNew ? 'new' : f.id] = true;
    if (isNew) {
        ipc(JSON.stringify(Object.assign({ type: 'create_host' }, payload)));
    } else {
        ipc(JSON.stringify(Object.assign({ type: 'update_host', id: f.id }, payload)));
    }
    render();
}

// testHostConnection is docs/ssh-hosts.md's "Test connection" button: for an
// existing host it just re-probes (probe_host); for one still being created
// there is no host to probe yet, so it saves — Create already probes
// synchronously and the result lands in the same probe-result card either
// way.
function testHostConnection() {
    const f = state.hostForm;
    if (!f) return;
    if (f.id) {
        probeHost(f.id);
        return;
    }
    saveHostForm();
}

function removeHost(id, name) {
    if (!confirm('Remove host "' + name + '"?\n\nAny project on it must be moved back to this Mac or another host first.')) return;
    ipc(JSON.stringify({ type: 'remove_host', id }));
}

function probeHost(id) {
    state.hostProbePending[id] = true;
    render();
    ipc(JSON.stringify({ type: 'probe_host', id }));
}

function disconnectHost(id) {
    ipc(JSON.stringify({ type: 'disconnect_host', id }));
}

function renderHostForm() {
    const f = state.hostForm;
    if (!f) return '<div class="empty-state">No form state.</div>';
    const isNew = !f.id;
    const pendingKey = isNew ? 'new' : f.id;
    const pending = !!state.hostProbePending[pendingKey];

    let html = '<h2>' + (isNew ? 'Add host' : 'Edit host') + '</h2>';
    if (state.hostFormError) html += '<div class="proj-error" id="hostFormBanner" tabindex="-1">' + esc(state.hostFormError) + '</div>';

    html += '<div class="proj-section">';
    html += '<div class="proj-section-title">Identity</div>';
    html += '<label for="hostName">Name</label>';
    html += '<input type="text" id="hostName" value="' + esc(f.name) + '" placeholder="devbox" />';
    html += '<label for="hostTarget">SSH target</label>';
    html += '<input type="text" id="hostTarget" value="' + esc(f.target) + '" placeholder="user@host or ssh-config alias" />';
    html += '<label for="hostPort">Port</label>';
    html += '<input type="text" id="hostPort" value="' + esc(f.port) + '" placeholder="22" />';
    html += '<label for="hostIdentityFile">Identity file</label>';
    html += '<input type="text" id="hostIdentityFile" value="' + esc(f.identity_file) + '" placeholder="optional, absolute path" />';

    // The freshest record lives in state.hosts — onHostAdded/onHostUpdated
    // already replaced it there by the time this re-renders, so the form
    // reads the probe result from the list rather than keeping a second copy.
    const existing = !isNew ? (state.hosts || []).find(x => x.id === f.id) : null;
    const probedTmux = existing && existing.probe && existing.probe.tmux_path;
    html += '<label for="hostTmuxPath">tmux path</label>';
    html += '<input type="text" id="hostTmuxPath" value="' + esc(f.tmux_path) + '" placeholder="' + esc(probedTmux || 'not found by probe') + '" />';
    html += '<p class="proj-section-help">Used by persist templates. Leave empty to use the probed path.</p>';
    html += '</div>';

    if (existing && existing.probe) {
        html += renderHostProbeCard(existing.probe, existing.target);
    }
    if (existing) html += renderHostTemplates(existing);

    html += '<div class="proj-form-actions">';
    html += '<button class="btn btn-primary" onclick="saveHostForm()" ' + (pending ? 'disabled' : '') + '>Save</button>';
    html += '<button class="btn btn-sm" onclick="testHostConnection()" ' + (pending ? 'disabled' : '') + '>' + (pending ? 'Testing…' : 'Test connection') + '</button>';
    html += '<button class="btn btn-danger" onclick="cancelHostEdit()">Cancel</button>';
    html += '</div>';
    return html;
}

// renderHostProbeCard is docs/ssh-hosts.md's three-line result: reachability
// (named by the ssh target, not the discovered $HOME — the target is what
// the operator typed and can check against), then node, then claude, each
// ✓ or ✗. A missing claude gets the doc's own remedy sentence rather than a
// bare "not found", since it is the one gap the operator can't fix by
// editing this form.
function renderHostProbeCard(p, target) {
    let html = '<div class="proj-section">';
    html += '<div class="proj-section-title">Probe result</div>';
    if (!p.ok) {
        html += '<div class="proj-error">✗ ' + esc(p.error || 'unreachable') + '</div>';
        html += '</div>';
        return html;
    }
    html += '<div class="proj-ok">✓ Reachable as ' + esc(target || '') + '</div>';
    if (p.node_path) {
        html += '<div class="proj-ok">✓ node ' + esc(p.node_version || '') + ' at ' + esc(p.node_path) + '</div>';
    } else {
        html += '<div class="proj-error">✗ node not found</div>';
    }
    if (p.claude_path) {
        html += '<div class="proj-ok">✓ claude ' + esc(p.claude_version || '') + ' at ' + esc(p.claude_path) + '</div>';
    } else {
        html += '<div class="proj-error">✗ claude not found — install Claude Code on this host and run Probe again</div>';
    }
    html += '</div>';
    return html;
}

// ---- Host IPC event handlers ----

window.onHostsListed = function(hosts) {
    state.hosts = hosts || [];
    if (state.page === 'hosts') render('push');
};

window.onHostAdded = function(h) {
    if (!h || !h.id) return;
    state.hosts = state.hosts.filter(x => x.id !== h.id).concat(h);
    delete state.hostProbePending['new'];
    state.editingHostId = null;
    state.hostForm = null;
    state.hostError = null;
    if (state.page === 'hosts') render('push');
};

window.onHostUpdated = function(h) {
    if (!h || !h.id) return;
    state.hosts = state.hosts.map(x => x.id === h.id ? h : x);
    delete state.hostProbePending[h.id];
    state.hostError = null;
    // Stay on the form after a probe/re-probe (the result card is what the
    // operator is looking at); a plain Save closes it, same as projects.
    if (state.page === 'hosts') render('push');
};

window.onHostRemoved = function(id) {
    state.hosts = state.hosts.filter(x => x.id !== id);
    if (state.editingHostId === id) {
        state.editingHostId = null;
        state.hostForm = null;
    }
    if (state.page === 'hosts') render('push');
};

window.onHostError = function(msg) {
    state.hostError = msg;
    state.hostFormError = msg;
    // Every host mutation this UI can trigger sets hostProbePending['new'] or
    // hostProbePending[id]; a refusal clears whichever key was in flight so
    // the button doesn't stay stuck reading "Probing…"/"Testing…".
    state.hostProbePending = {};
    if (state.page !== 'hosts') return;
    render();
};

// ---- Host terminal templates (docs/ssh-hosts.md) ----
//
// A host carries its own TerminalTemplates: command and args run ON the host,
// so there is no sandbox, env passthrough or model key to offer. Every
// successful mutation answers with onHostTemplatesListed, which is also what
// closes the sub-form — the Templates tab's shape, scoped to one host.

function hostTemplateCommandLine(t) {
    const parts = [t.command || '(host login shell)'].concat(t.args || []);
    return parts.map(esc).join(' ');
}

function renderHostTemplates(h) {
    let html = '<div class="proj-section">';
    html += '<div class="proj-section-title">Terminal templates</div>';
    html += '<p class="proj-section-help">Commands run on the host. Empty command opens the host\'s login shell.</p>';
    if (state.editingHostTemplateId) {
        html += renderHostTemplateForm();
        html += '</div>';
        return html;
    }
    const templates = h.terminal_templates || [];
    if (templates.length === 0) {
        html += '<div class="proj-tool-empty">No templates yet.</div>';
    }
    for (const t of templates) {
        html += '<div class="proj-card">';
        html += '<div class="proj-card-header">';
        html += '<span class="proj-card-name">' + esc(t.name) + ' <code>' + esc(t.id) + '</code>' + (t.persist ? ' <span class="proj-host-chip">persist</span>' : '') + '</span>';
        html += '<div style="display:flex;gap:4px">';
        html += '<button class="btn btn-sm" ' + bind(editHostTemplate, t.id) + '>Edit</button>';
        html += '<button class="btn btn-sm btn-danger" ' + bind(removeHostTemplate, t.id, t.name) + '>Remove</button>';
        html += '</div></div>';
        html += '<div class="proj-card-path">' + hostTemplateCommandLine(t) + '</div>';
        html += '</div>';
    }
    html += '<button class="btn btn-sm btn-primary" onclick="newHostTemplate()">+ Add template</button>';
    html += '</div>';
    return html;
}

function editingHostRecord() {
    const f = state.hostForm;
    return f && f.id ? (state.hosts || []).find(x => x.id === f.id) : null;
}

function closeHostTemplateForm() {
    state.editingHostTemplateId = null;
    state.hostTemplateForm = null;
    state.hostTemplateFormError = null;
    state.hostTemplateSaving = false;
}

function newHostTemplate() {
    state.editingHostTemplateId = 'new';
    state.hostTemplateForm = { id: '', name: '', command: '', args: '', env: '', persist: false };
    state.hostTemplateFormError = null;
    render();
}

function editHostTemplate(id) {
    const h = editingHostRecord();
    const t = h && (h.terminal_templates || []).find(x => x.id === id);
    if (!t) return;
    state.editingHostTemplateId = id;
    state.hostTemplateForm = {
        id: t.id, name: t.name || '', command: t.command || '',
        args: (t.args || []).join('\n'),
        env: Object.keys(t.env || {}).map(k => k + '=' + t.env[k]).join('\n'),
        persist: !!t.persist,
    };
    state.hostTemplateFormError = null;
    render();
}

function cancelHostTemplateEdit() {
    closeHostTemplateForm();
    render();
}

// Same reason as captureTemplateFormInputs: the inputs live only in the DOM
// between renders.
function captureHostTemplateFormInputs() {
    const f = state.hostTemplateForm;
    for (const k of Object.keys(f)) {
        const el = document.getElementById('htpl_' + k);
        if (el) f[k] = el.type === 'checkbox' ? el.checked : el.value;
    }
}

function removeHostTemplate(id, name) {
    const h = editingHostRecord();
    if (!h) return;
    if (!confirm('Remove template "' + name + '" from ' + h.name + '?')) return;
    ipc(JSON.stringify({ type: 'remove_host_template', host_id: h.id, id }));
}

function saveHostTemplateForm() {
    const f = state.hostTemplateForm;
    const h = editingHostRecord();
    if (!f || !h) return;
    captureHostTemplateFormInputs();
    const env = {};
    for (const line of templateLines(f.env)) {
        const i = line.indexOf('=');
        if (i < 1) {
            state.hostTemplateFormError = 'Env lines must be KEY=value: ' + line;
            render();
            return;
        }
        env[line.slice(0, i).trim()] = line.slice(i + 1);
    }
    const isNew = state.editingHostTemplateId === 'new';
    const template = {
        id: f.id.trim(), name: f.name.trim(),
        command: f.command.trim(), args: templateLines(f.args), env,
        persist: !!f.persist,
    };
    state.hostTemplateFormError = null;
    state.hostTemplateSaving = true;
    ipc(JSON.stringify({ type: isNew ? 'create_host_template' : 'update_host_template', host_id: h.id, template }));
    render();
}

function renderHostTemplateForm() {
    const f = state.hostTemplateForm;
    if (!f) return '';
    const isNew = state.editingHostTemplateId === 'new';
    const input = (key, label, placeholder, disabled) =>
        '<label for="htpl_' + key + '">' + label + '</label>' +
        '<input type="text" id="htpl_' + key + '" value="' + esc(f[key]) + '" placeholder="' + esc(placeholder || '') + '"' + (disabled ? ' disabled' : '') + ' />';
    const lines = (key, label, placeholder) =>
        '<label for="htpl_' + key + '">' + label + '</label>' +
        '<textarea id="htpl_' + key + '" rows="3" placeholder="' + placeholder + '">' + esc(f[key]) + '</textarea>';

    let html = '<div class="proj-template-card">';
    html += '<div class="proj-section-title">' + (isNew ? 'Add template' : 'Edit template') + '</div>';
    if (state.hostTemplateFormError) html += '<div class="proj-error" tabindex="-1">' + esc(state.hostTemplateFormError) + '</div>';
    html += input('id', 'ID', 'shell', !isNew);
    html += input('name', 'Name', 'Shell');
    html += input('command', 'Command', 'empty = host login shell');
    html += lines('args', 'Arguments (one per line)', '--flag&#10;${PROJECT_PATH}');
    html += lines('env', 'Environment (KEY=value per line)', 'DEBUG=true');
    html += '<label class="proj-mcp-row"><input type="checkbox" id="htpl_persist"' + (f.persist ? ' checked' : '') + ' /> Persist (run inside tmux on the host; survives disconnects and relay restarts)</label>';
    html += '<div class="proj-form-actions">';
    html += '<button class="btn btn-primary" onclick="saveHostTemplateForm()" ' + (state.hostTemplateSaving ? 'disabled' : '') + '>Save template</button>';
    html += '<button class="btn btn-danger" onclick="cancelHostTemplateEdit()">Cancel</button>';
    html += '</div></div>';
    return html;
}

// onHostTemplatesListed answers list_host_templates and every host template
// mutation with {host_id, templates}. The list is folded into the host's
// record in state.hosts, where renderHostTemplates reads it. Not a 'push'
// render: the host form is open, and render() captures its inputs first.
window.onHostTemplatesListed = function(msg) {
    const hostId = msg && msg.host_id;
    const templates = (msg && msg.templates) || [];
    state.hosts = (state.hosts || []).map(x => x.id === hostId ? Object.assign({}, x, { terminal_templates: templates }) : x);
    if (state.hostTemplateSaving) closeHostTemplateForm();
    if (state.page === 'hosts') render();
};

window.onHostTemplateError = function(msg) {
    state.hostTemplateSaving = false;
    state.hostTemplateFormError = msg;
    // With no sub-form open (a Remove), the host form's banner carries it.
    if (!state.hostTemplateForm) state.hostFormError = msg;
    if (state.page === 'hosts') render();
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

// renderCAFingerprintLine is the header line spec §6 calls for: relay's CA
// certificate fingerprint, the value `relayremote request --ca-fingerprint`
// must be given and the one comparison that closes the request channel's
// MITM ("only the CA pin stops this"). Read from state.remote.ca_fingerprint
// — the same field the approval panel below reads, so the two are never two
// different numbers on screen.
// renderCAFingerprintLine shows the fingerprint itself and nothing else —
// the explanation of what it's for lives in renderEnrolments' "How
// enrolment works" details, once, rather than repeated everywhere this
// value appears (the list and the approval sheet both call this).
function renderCAFingerprintLine() {
    const fp = state.remote && state.remote.ca_fingerprint;
    if (!fp) {
        return '<p class="proj-section-help">Relay\'s CA fingerprint is not available yet — create or sign one enrolment to generate the CA, then it will show here.</p>';
    }
    return '<div class="ca-fingerprint-row">'
        + '<span class="ca-fingerprint-label">CA fingerprint</span>'
        + '<code class="ca-fingerprint-value">' + esc(fp) + '</code>'
        + '<button type="button" class="btn btn-sm" ' + bind(copyToClipboard, fp) + '>Copy</button>'
        + '</div>';
}

// renderPendingEnrolmentRequests is the Pending requests panel (spec §3),
// shown above the enrolment list. Every row an unauthenticated network peer
// can cause to exist here carries exactly four things, key first and in
// full (spec: "the point is 'that key'"), and nothing that could read as
// relay's own assertion about who is asking — see renderPendingRequestFields.
function renderPendingEnrolmentRequests() {
    const list = state.pendingEnrolmentRequests || [];
    let html = '<div class="proj-section" style="margin-top:0">';
    html += '<div class="proj-section-title">Pending requests' + (list.length ? ' <span class="remote-state on">' + list.length + '</span>' : '') + '</div>';
    html += '<p class="proj-section-help">A machine that can reach the enrolment-request listener can add a row here and nothing else — see "How enrolment works" above.</p>';
    if (!list.length) {
        html += '<div class="empty-state">No pending enrolment requests.</div>';
    }
    for (const r of list) {
        html += '<div class="enrol-card">';
        html += renderRequestComparison(r);
        html += '<div class="enrol-card-header">';
        html += '<span class="enrol-card-name">' + esc(r.request_id) + '</span>';
        if (r.approved) {
            html += '<span class="remote-state on">approved: ' + esc(r.approved_client_id) + '</span>';
        } else {
            html += '<span>';
            if (enrolRequestApprovable(r)) {
                html += '<button class="btn btn-sm btn-primary" ' + bind(approveEnrolmentRequestForm, r.request_id) + '>Approve…</button> ';
            } else {
                // Disabled and carrying no handler: approving a row with no
                // completed comparison is approving without the control, and
                // the host refuses it from every door anyway.
                html += '<button class="btn btn-sm btn-primary" disabled title="This request has no completed comparison code, so it cannot be approved.">Approve…</button> ';
            }
            html += '<button class="btn btn-sm btn-danger" ' + bind(refuseEnrolmentRequest, r.request_id) + '>Refuse</button>';
            html += '</span>';
        }
        html += '</div>';
        html += renderPendingRequestFields(r);
        html += '</div>';
    }
    html += '</div>';
    return html;
}

// enrolRequestApprovable is the one rule the Approve control obeys: a row
// that committed to a comparison must have opened it, and opened it
// correctly. A legacy `relayremote request` row carries no comparison and is
// approvable exactly as it always was — that is what keeps the
// operator-carried and carried-pin paths working. This mirrors, and never
// replaces, EnrolmentOps.Approve's own refusal: the host enforces the
// comparison independently of anything rendered here.
function enrolRequestApprovable(r) {
    if (r.is_legacy_request) return true;
    return !!r.sas_ready && !r.sas_failed;
}

// renderRequestComparison is the comparison code and its three non-code
// states, rendered above everything else on the row and repeated at the top
// of the approval sheet so the operator is looking at it at the moment they
// decide. The code is derived host-side from the CA, the CSR's public key and
// two nonces; it is not a secret and not a password — its whole job is to be
// read aloud and compared.
function renderRequestComparison(r) {
    if (r.is_legacy_request) {
        return '<div class="enrol-sas legacy"><div class="enrol-sas-state">carried-pin request (<code>relayremote request</code>)</div>'
            + '<div class="enrol-sas-help">There is no comparison code on this path. The control here is the CA fingerprint the operator carried to that machine, shown in the tab header.</div></div>';
    }
    if (r.sas_failed) {
        return '<div class="enrol-sas failed"><div class="enrol-sas-state">Comparison failed</div>'
            + '<div class="enrol-sas-help">That machine failed its comparison handshake; this request cannot be approved. Refuse it and register again — if it fails a second time, something is on the network path.</div></div>';
    }
    if (!r.sas_ready) {
        return '<div class="enrol-sas waiting"><div class="enrol-sas-state">Waiting for the comparison code…</div>'
            + '<div class="enrol-sas-help">Waiting for that machine to complete the comparison handshake. Approving is disabled until it does.</div></div>';
    }
    return '<div class="enrol-sas"><div class="enrol-sas-code">' + esc(r.sas) + '</div>'
        + '<div class="enrol-sas-help">Compare this with the code shown on the machine asking. If they differ, Refuse — something is on the network path.</div></div>';
}

// renderPendingRequestFields is the exact four things spec §3 names, key
// first and never truncated. label is marked "supplied by the requesting
// machine" so it can never read as relay's own assertion — it is hostile
// input from an unauthenticated peer, already restricted server-side to
// [A-Za-z0-9._-]{1,64} and rendered here as text via esc(), never as markup.
function renderPendingRequestFields(r) {
    let html = '<div class="enrol-fp"><span class="enrol-fp-label">key: </span>sha256:' + esc(r.spki_sha256) + '</div>';
    html += '<div class="enrol-meta">';
    html += '<span>label: <strong>' + (r.label ? esc(r.label) : '(none)') + '</strong> <em>(supplied by the requesting machine)</em></span>';
    html += '</div>';
    // Marked as a request, never as a grant: it is the same hostile input the
    // label is, and nothing on this path acts on it.
    if (r.requested_profile) {
        html += '<div class="enrol-meta">';
        html += '<span>asked for: <strong>' + esc(r.requested_profile) + '</strong> <em>(a request from that machine, not a grant)</em></span>';
        html += '</div>';
    }
    html += '<div class="enrol-meta">';
    html += '<span>from: <strong>' + esc(r.remote_addr) + '</strong></span>';
    html += '<span>arrived: <strong>' + esc(r.arrived_at) + '</strong></span>';
    html += '<span>expires: <strong>' + esc(r.expires_at) + '</strong></span>';
    html += '</div>';
    return html;
}

function renderEnrolments() {
    if (state.enrolForm) return renderEnrolmentForm();

    let html = '<div class="page-header"><h2>Remote Clients</h2>';
    html += '<button class="btn btn-primary" onclick="newEnrolment()">+ New Enrolment</button></div>';
    html += '<p class="page-intro">An enrolment binds one client certificate to the access profiles it may use.</p>';
    html += '<details class="learn-more"><summary>How enrolment works</summary>';
    html += '<p>The certificate <em>is</em> the identity — there is no bearer token on this path, so a copy of <code>settings.json</code> grants no remote access at all. Enrolments are keyed by certificate, not by machine: several agents on one VM each hold their own, granted and revoked independently.</p>';
    html += '<p>Relay\'s CA fingerprint is what a client pins (<code>relayremote request --ca-fingerprint ...</code>) — see the value below.</p>';
    html += '<p>A machine that can reach the enrolment-request listener can add a row to Pending requests below and nothing else. Lodging never raises a prompt; approving does, and it is the same <code>enrolment.sign</code> prompt <code>relay enrol sign</code> already uses. The request carries no grant and no budget: those are chosen at approval.</p>';
    html += '</details>';
    html += renderCAFingerprintLine();

    if (state.enrolBundle) html += renderEnrolBundleBanner(state.enrolBundle);
    if (state.enrolRevoked) {
        html += '<div class="audit-note">Revoked <strong>' + esc(state.enrolRevoked.client_id) + '</strong>. Its calls remain in the Tool Calls log under fingerprint <code>' + esc(state.enrolRevoked.fingerprint) + '</code> — now the only thing that names them.</div>';
    }
    if (state.enrolmentError) html += '<div class="proj-error">' + esc(state.enrolmentError) + '</div>';

    html += renderPendingEnrolmentRequests();

    const list = state.enrolments || [];
    if (!list.length) {
        html += '<div class="empty-state">No enrolled clients. Click <strong>+ New Enrolment</strong>, or run <code>relay enrol create</code>.</div>';
    }
    for (const e of list) {
        html += '<div class="enrol-card">';
        html += '<div class="enrol-card-header">';
        html += '<span class="enrol-card-name">' + esc(e.client_id) + '</span>';
        html += '<button class="btn btn-sm btn-danger" ' + bind(revokeEnrolment, e.client_id) + '>Revoke</button>';
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
    const approving = !!f.request_id;
    let html = '<h2>' + (approving ? 'Approve Enrolment Request' : 'New Enrolment') + '</h2>';
    if (state.enrolmentError) html += '<div class="proj-error">' + esc(state.enrolmentError) + '</div>';

    // Approving pre-fills nothing but the identity this certificate is over
    // — grants and budget are the human's choice below, same as a plain
    // create (spec §3: "the request carries no grant field and no budget
    // field"). The request's own fields render read-only above the form so
    // the operator is looking at exactly what they are about to sign over.
    if (approving && f.pendingRequest) {
        html += '<div class="proj-section">';
        html += '<div class="proj-section-title">Request ' + esc(f.request_id) + '</div>';
        // The code repeats here, above the identity section: this is the panel
        // open at the moment the decision is made, and a comparison the
        // operator has to scroll back to is a comparison nobody makes.
        html += renderRequestComparison(f.pendingRequest);
        html += renderPendingRequestFields(f.pendingRequest);
        html += '<p class="proj-section-help">Approving raises the same presence prompt <code>relay enrol sign</code> already uses — there is no second door into issuance. The certificate is issued over exactly the public key above; nothing chosen below can redirect it to a different key.</p>';
        // The client pins this value at collection time (spec §6: "only the
        // CA pin stops this") — repeated here, not just in the tab header,
        // because this is the panel open at the moment it needs relaying to
        // whoever is running `relayremote request` on the other machine.
        html += renderCAFingerprintLine();
        html += '</div>';
    }

    html += '<div class="proj-section">';
    html += '<div class="proj-section-title">Identity</div>';
    html += '<p class="proj-section-help">The client id is the certificate\'s Common Name and the bundle\'s directory name, so it is limited to letters, digits, <code>.</code>, <code>_</code> and <code>-</code>. It must be unique: to re-issue a certificate, revoke the existing enrolment first.</p>';
    html += '<label for="enrolClientId">Client id</label>';
    // The value, when there is one, is the HOST's collision-free suggestion
    // (suggested_client_id), never an echo of the request's label: the host
    // owns its own client_id namespace and resolving a collision is not
    // something an operator can do by typing. The label survives only as the
    // placeholder, for a request the host had no suggestion for.
    const idPlaceholder = (approving && f.pendingRequest && f.pendingRequest.label) || 'hermes-mail';
    html += '<input type="text" id="enrolClientId" value="' + esc(f.client_id) + '" placeholder="' + esc(idPlaceholder) + '" />';
    if (approving && f.client_id) {
        html += '<p class="proj-section-help">Relay suggests this; it is yours to change. It names the enrolment in <code>relay enrol list</code> and in every audit record.</p>';
    }
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
    // The requested profile renders read-only beside the list and its
    // checkbox is NEVER pre-ticked: it is an unauthenticated peer's request,
    // and a pre-ticked box is a grant issued by a machine rather than by the
    // operator. Approving without touching the list issues nothing.
    if (approving && f.pendingRequest && f.pendingRequest.requested_profile) {
        html += '<p class="proj-section-help">This machine asked for <code>' + esc(f.pendingRequest.requested_profile) + '</code> — a request from that machine, not a grant. Nothing is ticked for you; tick it above only if you mean to.</p>';
    }
    if (f.project_ids.length === 0) {
        // Same note `relay enrol create` prints: enrolling with nothing is
        // legal and is the expected "enrol now, widen deliberately later"
        // resting state. Say so rather than emitting a certificate that
        // silently reaches nothing.
        html += '<p class="proj-section-help">No grant selected: this client will be enrolled but can reach no access profile until one is added.</p>';
        // Never pre-ticked, and the only way past the requirement. A newly
        // signed enrolment holding nothing connects successfully and lists
        // zero tools, which reads as a broken install rather than an
        // incomplete one — so the operator says which they mean.
        html += '<label class="proj-tool-row">';
        html += '<input type="checkbox" ' + (f.no_grant ? 'checked' : '') + ' onchange="toggleEnrolNoGrant(this.checked)" />';
        html += '<div><div>Enrol with no access for now (nothing will work until I grant a profile)</div></div>';
        html += '</label>';
    }
    html += '</div>';

    // ---- Budget ----
    html += '<div class="proj-section">';
    html += '<div class="proj-section-title">Budget</div>';
    html += '<p class="proj-section-help">The enrolment is the unit of compromise, so it is the unit that carries the cap. Rate and volume are capped together because they fail differently — a call limit alone does not stop a slow drain. Leave a field blank for the conservative default; <strong>zero is never unlimited</strong>, there is no way to switch a budget off.</p>';
    html += '<label for="enrolWindow">Window (seconds)</label>';
    html += '<input type="number" id="enrolWindow" value="' + esc(f.window_seconds) + '" placeholder="' + esc(d.window_seconds || '') + '" />';
    html += '<label for="enrolMaxCalls">Max tool calls per window</label>';
    html += '<input type="number" id="enrolMaxCalls" value="' + esc(f.max_calls) + '" placeholder="' + esc(d.max_calls || '') + '" />';
    html += '<label for="enrolMaxBytes">Max cumulative result bytes per window</label>';
    html += '<input type="number" id="enrolMaxBytes" value="' + esc(f.max_result_bytes) + '" placeholder="' + esc(d.max_result_bytes || '') + '" />';
    html += '</div>';

    html += '<div class="proj-form-actions">';
    const grantChosen = f.project_ids.length > 0 || !!f.no_grant;
    html += '<button class="btn btn-primary"' + (grantChosen ? '' : ' disabled') + ' onclick="saveEnrolment()">' + (approving ? 'Approve &amp; issue certificate' : 'Create &amp; issue certificate') + '</button>';
    html += '<button class="btn btn-danger" onclick="cancelEnrolment()">Cancel</button>';
    html += '</div>';
    return html;
}

function newEnrolment() {
    state.enrolForm = { client_id: '', project_ids: [], window_seconds: '', max_calls: '', max_result_bytes: '', no_grant: false };
    state.enrolmentError = null;
    state.enrolBundle = null;
    state.enrolRevoked = null;
    render();
}

// approveEnrolmentRequestForm opens the SAME create-enrolment form
// newEnrolment does, pre-filled with the pending request's identity (spec
// §3: "Approving opens the existing create-enrolment form pre-filled, so
// the human picks grants and budget in the UI they already know"). f.request_id
// is what saveEnrolment below reads to send an approval instead of a plain
// create; f.pendingRequest carries the request's own fields so the form can
// show them without a second round trip.
function approveEnrolmentRequestForm(requestID) {
    const r = (state.pendingEnrolmentRequests || []).find(x => x.request_id === requestID);
    if (!r) return;
    // Belt and braces with the disabled button in the row above, and with
    // EnrolmentOps.Approve's own refusal on the host below. The failure mode
    // being guarded is a silent widening — a future edit that re-enables the
    // button would otherwise open a sheet with nothing behind it on this side
    // — so the rule is asserted at the door as well as on the control.
    if (!enrolRequestApprovable(r)) {
        state.enrolmentError = 'this request has not completed its comparison handshake, so it cannot be approved';
        render();
        return;
    }
    state.enrolForm = {
        // The host's own suggestion, not the request's label — see the form's
        // client-id help text. Absent (an unusable label, or every suffix to
        // -99 taken) leaves the field empty rather than guessing.
        client_id: r.suggested_client_id || '',
        project_ids: [], window_seconds: '', max_calls: '', max_result_bytes: '', no_grant: false,
        request_id: requestID, pendingRequest: r,
    };
    state.enrolmentError = null;
    state.enrolBundle = null;
    state.enrolRevoked = null;
    render();
}

// refuseEnrolmentRequest is the operator's explicit decline (spec §2, §3) —
// deliberately not routed through the presence-gated approval path at all;
// EnrolmentOps.Refuse never raises the prompt (see its own doc comment).
function refuseEnrolmentRequest(requestID) {
    const msg = 'Refuse enrolment request "' + requestID + '"?\n\n'
        + 'The request is removed. Re-lodging from the client machine starts a fresh one.';
    if (!confirm(msg)) return;
    ipc(JSON.stringify({ type: 'refuse_enrolment_request', request_id: requestID }));
}

function listEnrolmentRequests() {
    ipc(JSON.stringify({ type: 'list_enrolment_requests' }));
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
    // Ticking a profile answers the question the checkbox asks, so the
    // checkbox stops claiming the opposite.
    if (f.project_ids.length > 0) f.no_grant = false;
    render();
}

function toggleEnrolNoGrant(checked) {
    const f = state.enrolForm;
    if (!f) return;
    f.no_grant = !!checked;
    render();
}

// captureEnrolFormInputs is the enrolment form's half of what
// captureProjectFormInputs does for the project form: the client id and the
// three budget fields live only in the DOM between renders, and every
// checkbox on this form re-renders it. Without this, ticking a profile
// erases whatever the operator had typed above it. Empty is treated as
// "leave it", the same guard captureProjectFormInputs uses, so a field the
// browser has not rendered yet cannot blank a stored value.
function captureEnrolFormInputs() {
    const f = state.enrolForm;
    if (!f) return;
    const val = function(id) {
        const el = document.getElementById(id);
        return el && typeof el.value === 'string' ? el.value : '';
    };
    f.client_id = val('enrolClientId') || f.client_id;
    f.window_seconds = val('enrolWindow') || f.window_seconds;
    f.max_calls = val('enrolMaxCalls') || f.max_calls;
    f.max_result_bytes = val('enrolMaxBytes') || f.max_result_bytes;
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
    // ADR-019 §7: never the silent default. Zero profiles is a legal and
    // sometimes correct answer, but it is one the operator has to give out
    // loud — the certificate issued below connects successfully and reaches
    // nothing, which reads on the client machine as a broken install.
    if (f.project_ids.length === 0 && !f.no_grant) {
        state.enrolmentError = 'choose at least one access profile, or tick "Enrol with no access for now"';
        render();
        return;
    }
    // A blank or unparseable field sends 0, which normalizeEnrolmentBudget
    // reads as "unset" and fills with the conservative default. Zero never
    // means unlimited anywhere on this path.
    const num = function(id) {
        const raw = (((document.getElementById(id) || {}).value) || '').trim();
        const n = parseInt(raw, 10);
        return (raw === '' || isNaN(n) || n < 0) ? 0 : n;
    };
    state.enrolmentError = null;
    const budget = {
        window_seconds: num('enrolWindow'),
        max_calls: num('enrolMaxCalls'),
        max_result_bytes: num('enrolMaxBytes'),
    };
    // f.request_id set = this form was opened via "Approve…" on a pending
    // request (approveEnrolmentRequestForm), not "+ New Enrolment" — same
    // fields, different IPC message, so the tray core that runs is
    // EnrolmentOps.Approve (spec §3's second door onto enrolment.sign)
    // rather than Create.
    if (f.request_id) {
        ipc(JSON.stringify({
            type: 'approve_enrolment_request',
            request_id: f.request_id,
            client_id: clientID,
            project_ids: f.project_ids,
            budget: budget,
        }));
        return;
    }
    ipc(JSON.stringify({
        type: 'create_enrolment',
        client_id: clientID,
        project_ids: f.project_ids,
        budget: budget,
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
        state.remoteDraft = {
            enabled: !!r.enabled, listen: r.listen || '',
            enrolmentRequests: !!r.enrolment_requests, enrolmentListen: r.enrolment_listen || '',
        };
    }
    return state.remoteDraft;
}

function remoteDraftSet(key, value) {
    const d = remoteDraft();
    d[key] = value;
    state.remoteDirty = true;
    // The address fields re-render nothing (a repaint on every keystroke
    // would fight the caret); the toggles do, because the consequence text
    // below each changes with it.
    if (key === 'enabled' || key === 'enrolmentRequests') render();
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
    html += '<label class="switch"><input type="checkbox" aria-label="Accept remote clients on an mTLS listener" ' + (d.enabled ? 'checked' : '') + ' onchange="remoteDraftSet(\'enabled\', this.checked)" /><span class="slider"></span></label>';
    html += '</div>';

    html += '<label for="remoteListen">Listen address</label>';
    html += '<input type="text" id="remoteListen" value="' + esc(d.listen) + '" placeholder="' + esc(r.effective || '') + '" oninput="remoteDraftSet(\'listen\', this.value)" />';
    html += '<p class="proj-section-help">Leave blank for the default, <code>' + esc(r.effective || '127.0.0.1:9910') + '</code>. ' + esc(REMOTE_NOTE) + '</p>';

    const effective = (d.listen || '').trim() || r.effective || '';
    if (effective && !remoteListenIsLoopback(effective)) {
        html += '<div class="remote-note warn">' + esc(effective) + ' binds beyond loopback: every machine that can reach that address can attempt a TLS handshake. Only a certificate relay signed gets past it, and an unenrolled one is closed before a single request is read — but the default binds loopback precisely so that reaching relay from another machine is a deliberate act. A tunnel is a network path, never an identity: never forward the bridge socket in its place.</div>';
    }

    // The enrolment-request listener (spec §1): a separate, unauthenticated
    // mailbox a network peer can drop a CSR into, never a door into a tool
    // call — see renderPendingEnrolmentRequests' own help text for what it
    // can and can't reach. Off by default, alongside the mTLS toggle rather
    // than folded into it, because it is a second network door and opening
    // one is always the operator's own act.
    html += '<div class="toggle-row" style="padding:4px 0;margin:0">';
    html += '<span>Accept enrolment requests on a separate listener</span>';
    html += '<label class="switch"><input type="checkbox" aria-label="Accept enrolment requests on a separate listener" ' + (d.enrolmentRequests ? 'checked' : '') + ' onchange="remoteDraftSet(\'enrolmentRequests\', this.checked)" /><span class="slider"></span></label>';
    html += '</div>';

    html += '<label for="remoteEnrolmentListen">Enrolment listen address</label>';
    html += '<input type="text" id="remoteEnrolmentListen" value="' + esc(d.enrolmentListen) + '" placeholder="' + esc(r.enrolment_effective || '') + '" oninput="remoteDraftSet(\'enrolmentListen\', this.value)" />';
    html += '<p class="proj-section-help">Leave blank for the default, <code>' + esc(r.enrolment_effective || '127.0.0.1:9911') + '</code>. This listener takes no client certificate — see the Pending requests panel above for what it can reach.</p>';

    if (d.enrolmentRequests && !d.enabled) {
        html += '<div class="remote-note warn">Enrolment requests will not be served until the mTLS listener above is also on: the enrolment-request channel is a companion to it, never a replacement — see the panel above for the exact refusal.</div>';
    }

    const enrolEffective = (d.enrolmentListen || '').trim() || r.enrolment_effective || '';
    if (d.enrolmentRequests && enrolEffective && !remoteListenIsLoopback(enrolEffective)) {
        html += '<div class="remote-note warn">' + esc(enrolEffective) + ' binds beyond loopback: any machine that can reach it can lodge an enrolment request. Lodging alone never raises a prompt and reaches nothing but a bounded table — but the default binds loopback so that widening it is a deliberate act.</div>';
    }

    html += '<div class="proj-form-actions">';
    html += '<button class="btn btn-primary" onclick="saveRemoteConfig()" ' + (state.remoteConfigSavePending ? 'disabled' : '') + '>' + (state.remoteConfigSavePending ? 'Saving…' : 'Save') + '</button>';
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
    state.remoteConfigSavePending = true;
    ipc(JSON.stringify({
        type: 'update_remote_config',
        enabled: !!d.enabled,
        listen: String(d.listen || '').trim(),
        enrolment_requests: !!d.enrolmentRequests,
        enrolment_listen: String(d.enrolmentListen || '').trim(),
    }));
    render();
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
    state.remoteConfigSavePending = false;
    if (state.page === 'remote') render('push');
};

window.onRemoteConfigError = function(msg) {
    state.remoteError = msg || 'could not save the remote block';
    state.remoteConfigSavePending = false;
    if (state.page === 'remote') render('push');
};

// onEnrolmentRequestsChanged is the Pending requests panel's one data
// source: fired in answer to list_enrolment_requests, after an approve or a
// refuse, and on the tray's own poll tick while this window is open (so a
// request that arrives while the operator is already looking at this tab
// still appears). render('push') is a no-op while the approve/create form
// is open (the render() guard for state.enrolForm), so a background refresh
// can never wipe an in-progress approval.
window.onEnrolmentRequestsChanged = function(list) {
    state.pendingEnrolmentRequests = list || [];
    if (state.page === 'remote') render('push');
};

// ---------------------------------------------------------------------------
// Passkeys tab — the registrations that can sign a browser in, and the browser
// sessions they have already signed in.
//
// Both lists are on this one screen because they are separate records with
// separate lifetimes, and the difference is exactly what an operator gets
// wrong: revoking a passkey stops the NEXT login and does nothing to a session
// already minted, which lives out its twelve hours regardless (ADR-016
// decision 3). A tab that showed only the registrations would let "revoked"
// read as "signed out", and the sign-out button below is the other half of
// that sentence.
//
// Nothing here renders a public key. The Go side has no field carrying one
// (passkeyView), which is what makes that true by construction rather than by
// remembering not to print it; the same discipline enrolmentBundleView uses
// for the client private key.
//
// The credential id renders ABBREVIATED, the opposite of the certificate
// fingerprint one tab over. That is deliberate: a fingerprint outlives the
// enrolment it names and is the only thing identifying that client's past
// calls, while a passkey id names nothing once revoked — so a full one on
// screen would add a long random-looking string beside the word "credential"
// and buy nothing.
// ---------------------------------------------------------------------------

// The one place the tray item's name is written in the page. Whatever an
// operator with no passkeys is told to click has to match the menu exactly.
const LOGIN_CODE_MENU_ITEM = 'Show Login Code...';

function pkSignCountText(p) {
    if (!p.counter_supported) return 'This authenticator does not count signatures — the ordinary case for a synced passkey, and not a fault.';
    return 'Signature counter at last accepted assertion: ' + (p.sign_count || 0) + '.';
}

// renderLoginCodeBanner shows a code that exists nowhere else. Three facts
// have to be on screen with it, and each one is a mistake if it is missing:
// what it does (registers a passkey, and is never a password), how long it
// lasts, and that asking for another one kills this one — mintBootstrapCode
// replaces rather than accumulates, so a second banner would otherwise leave
// the operator with two codes on screen and one that works.
function renderLoginCodeBanner(c) {
    let html = '<div class="pk-code-banner">';
    if (c.error) {
        html += '<strong>Could not mint a login code.</strong>';
        html += '<div class="pk-code-line">' + esc(c.error) + '</div>';
        html += '<div class="pk-code-line">Nothing was changed. Try the tray item again, or run <code>relay login enrol</code> in a terminal — it writes through the same store and will report the same failure with more detail.</div>';
        html += '<div style="margin-top:8px"><button class="btn btn-sm" onclick="dismissLoginCode()">Done</button></div>';
        html += '</div>';
        return html;
    }
    html += '<strong>Login code</strong>';
    html += '<div class="pk-code">' + esc(c.code) + '</div>';
    html += '<div class="pk-code-line">Expires <strong>' + esc(c.expires || '') + '</strong> — valid for ' + esc(c.ttl || '') + ', single use.</div>';
    html += '<div class="pk-code-line">This code registers a passkey. It is <strong>NOT a password</strong> and is never accepted in place of a passkey assertion.</div>';
    if (c.url) {
        html += '<div class="pk-code-line">Open <code>' + esc(c.url) + '</code> and enter it.</div>';
    } else {
        html += '<div class="pk-code-line warn">There is no login page to enter it into: relay has no TCP listener, so <code>RELAY_API_LISTEN</code> is unset and the login routes are registered nowhere. Set it and relaunch relay, then mint a fresh code.</div>';
    }
    html += '<div class="pk-code-line warn">Showing another code replaces this one — there is only ever one live at a time, and this one stops working the moment the next is minted.</div>';
    html += '<div class="pk-code-line">It is shown here once and is not recoverable. Closing this window loses it.</div>';
    html += '<div style="margin-top:8px"><button class="btn btn-sm" onclick="copyLoginCode()">Copy</button> <button class="btn btn-sm" onclick="dismissLoginCode()">Done</button></div>';
    html += '</div>';
    return html;
}

function renderPasskeys() {
    let html = '<div class="page-header"><h2>Passkeys</h2>';
    html += '<button class="btn" onclick="refreshPasskeys()">Refresh</button></div>';
    html += '<p class="page-intro">A passkey is how you sign in to relay from a browser. There is no password: the login page at <code>/relay/login</code> accepts a passkey assertion and nothing else. Registering one is a host-side act — it needs a single-use code minted on this machine, so a page in your browser cannot register itself.</p>';

    if (state.loginCode) html += renderLoginCodeBanner(state.loginCode);
    if (state.passkeyError) html += '<div class="proj-error">' + esc(state.passkeyError) + '</div>';
    if (state.passkeyRevoked) {
        html += '<div class="audit-note">Revoked passkey <strong>' + esc(state.passkeyRevoked.name || state.passkeyRevoked.short) + '</strong>. It can no longer complete a login. Any browser it already signed in keeps its session below until you end it or it expires.</div>';
    }
    if (state.loginSignedOut) {
        html += '<div class="audit-note">Signed out <strong>' + esc(state.loginSignedOut) + '</strong>. Its token stops authenticating on the next request; that browser must run the ceremony again.</div>';
    }

    const list = state.passkeys || [];
    if (!list.length) {
        html += '<div class="empty-state">No passkeys are registered, so nothing can sign in to relay from a browser yet.<br><br>'
            + 'To register one, choose <strong>' + esc(LOGIN_CODE_MENU_ITEM) + '</strong> in the Relay tray menu, or run <code>relay login enrol</code> in a terminal. Either mints a single-use code that lasts two minutes; open the login page, enter it, and your authenticator registers a passkey.<br><br>'
            + 'The code exists so registration cannot be self-service: it can only be minted by something already running as you on this machine, never by a page that reached the port.</div>';
    }
    for (const p of list) {
        html += '<div class="pk-card">';
        html += '<div class="pk-card-header">';
        html += '<span class="pk-card-name">' + esc(p.name || '(unnamed passkey)') + '</span>';
        html += '<button class="btn btn-sm btn-danger" ' + bind(revokePasskey, p.id) + '>Revoke</button>';
        html += '</div>';
        html += '<div class="pk-id">Credential: ' + esc(p.short || '') + '</div>';
        html += '<div class="pk-counter">' + esc(pkSignCountText(p)) + '</div>';
        html += '<div class="pk-meta"><span>Registered: <strong>' + esc(p.created || '—') + '</strong></span></div>';
        html += '</div>';
    }

    html += renderEvePasskeys();
    html += renderLoginSessions();
    return html;
}

// renderEvePasskeys is the Passkeys tab's second section
// (docs/eve-passkey-enrolment.md decisions 7-13): eve owns these
// credentials and reports the list here, so this section never mints or
// edits one -- only revoke, which relay records as pending and never
// applies itself.
function renderEvePasskeys() {
    const list = state.evePasskeys || [];
    let html = '<div class="proj-section" style="margin-top:24px">';
    html += '<div class="proj-section-title">Eve passkeys</div>';
    html += '<p class="proj-section-help">Browsers that can sign in to Eve. Eve owns these credentials and reports this list to relay; revoking one here does not touch Eve directly — Eve applies it on its own next poll, or immediately if that browser tries to sign in, and signs out every session it minted.</p>';

    if (!list.length) {
        html += '<div class="empty-state">Eve has not reported any passkeys yet.</div>';
        html += '</div>';
        return html;
    }
    for (const p of list) {
        html += '<div class="pk-card">';
        html += '<div class="pk-card-header">';
        html += '<span class="pk-card-name">' + esc(p.label || '(unnamed passkey)') + '</span>';
        if (p.revocation_pending) {
            html += '<span class="pk-pending">revocation pending</span>';
        } else {
            html += '<button class="btn btn-sm btn-danger" ' + bind(revokeEvePasskey, p.id) + '>Revoke</button>';
        }
        html += '</div>';
        html += '<div class="pk-id">Credential: ' + esc(p.short || '') + '</div>';
        html += '<div class="pk-meta"><span>Registered: <strong>' + esc(p.created || '—') + '</strong></span> <span>Last used: <strong>' + esc(p.last_used || '—') + '</strong></span></div>';
        html += '</div>';
    }
    html += '</div>';
    return html;
}

// renderLoginSessions is the sign-out half. Each row is one APICredential the
// ceremony minted — one per login, which is what makes signing a single
// browser out a real operation rather than a global switch (ADR-016
// decision 3).
function renderLoginSessions() {
    const sessions = state.loginSessions || [];
    let html = '<div class="proj-section" style="margin-top:24px">';
    html += '<div class="proj-section-title">Signed-in Browsers</div>';
    html += '<p class="proj-section-help">Each completed login mints its own short-lived control-plane credential, held in the browser\'s memory and nowhere else — not <code>localStorage</code>, not a cookie — so a reload runs the ceremony again. It reaches <code>read</code> and <code>configure</code> and nothing else: no project-token rotation, no terminal, no session. Signing out revokes that one credential; every other browser and every credential relay injects into a service is untouched.</p>';

    if (!sessions.length) {
        html += '<div class="empty-state">No browser is signed in. A session appears here the moment one completes the ceremony at the login page, and disappears when it expires — twelve hours at most.</div>';
        html += '</div>';
        return html;
    }
    for (const c of sessions) {
        html += '<div class="pk-session">';
        html += '<div>';
        html += '<div class="pk-session-name">' + esc(c.name || c.id) + '</div>';
        html += '<div class="pk-session-meta">Signed in ' + esc(c.created || '—') + ' · expires ' + esc(c.expires || 'never') + '</div>';
        html += '</div>';
        html += '<button class="btn btn-sm btn-danger" ' + bind(signOutLogin, c.id) + '>Sign out</button>';
        html += '</div>';
    }
    // Named so the CLI is not a hidden second path: the same records are in
    // `relay credential list`, and this tab is the door onto them that is
    // where the person at the machine already is.
    html += '<p class="proj-section-help">These are ordinary control-plane credentials, so <code>relay credential list</code> shows them too and <code>relay credential revoke --id ID</code> ends one from a terminal. This button refuses anything that is not a login session — a credential you minted for a script is not sign-out-able from a browser view.</p>';
    html += '</div>';
    return html;
}

function refreshPasskeys() {
    state.passkeyError = null;
    ipc(JSON.stringify({ type: 'list_passkeys' }));
}

function copyLoginCode() {
    if (state.loginCode && state.loginCode.code) copyToClipboard(state.loginCode.code);
}

function dismissLoginCode() {
    state.loginCode = null;
    render();
}

// revokePasskey names what survives the act as well as what it cuts. "Are you
// sure?" over a credential id is not a decision anyone can make, and the
// surviving session is the half an operator will otherwise assume is gone.
function revokePasskey(id) {
    const p = (state.passkeys || []).find(x => x.id === id);
    if (!p) return;
    const live = (state.loginSessions || []).length;
    let msg = 'Revoke the passkey "' + (p.name || p.short) + '"?\n\n'
        + 'It can no longer sign in to relay from any browser. The passkey stays on your authenticator — relay just stops recognising it — and nothing else in settings is touched.\n\n';
    msg += live
        ? ('This does NOT sign anyone out: ' + live + ' browser session(s) are live and stay live until they expire. Sign them out separately below.')
        : 'No browser session is currently live, so nothing is signed in with it.';
    if (!confirm(msg)) return;
    state.passkeyError = null;
    ipc(JSON.stringify({ type: 'revoke_passkey', id: id }));
}

function signOutLogin(id) {
    const c = (state.loginSessions || []).find(x => x.id === id);
    if (!c) return;
    const msg = 'Sign out "' + (c.name || c.id) + '"?\n\n'
        + 'That browser\'s credential stops authenticating on its next request and it has to run the login ceremony again. No passkey is revoked and no other session is affected.';
    if (!confirm(msg)) return;
    state.passkeyError = null;
    ipc(JSON.stringify({ type: 'sign_out_login', id: id }));
}

// revokeEvePasskey names what changes, the same discipline revokePasskey
// follows: relay only records the revocation as pending here -- it never
// touches eve directly (decision 9).
function revokeEvePasskey(id) {
    const p = (state.evePasskeys || []).find(x => x.id === id);
    if (!p) return;
    const msg = 'Revoke the Eve passkey "' + (p.label || p.short) + '"?\n\n'
        + 'It stops working on its next use. Eve signs out every browser session it minted with this passkey when it applies the revocation.';
    if (!confirm(msg)) return;
    state.passkeyError = null;
    ipc(JSON.stringify({ type: 'revoke_eve_passkey', id: id }));
}

// ---- Passkeys IPC event handlers ----

window.onPasskeysReloaded = function(passkeys, sessions, evePasskeys) {
    state.passkeys = passkeys || [];
    state.loginSessions = sessions || [];
    state.evePasskeys = evePasskeys || [];
    state.passkeyError = null;
    if (state.page === 'passkeys') render('push');
};

window.onPasskeyRevoked = function(id, name) {
    const p = (state.passkeys || []).find(x => x.id === id);
    state.passkeys = (state.passkeys || []).filter(x => x.id !== id);
    state.passkeyError = null;
    state.loginSignedOut = null;
    state.passkeyRevoked = { name: name || '', short: p ? p.short : '' };
    if (state.page === 'passkeys') render('push');
};

window.onLoginSessionRevoked = function(id, name) {
    state.loginSessions = (state.loginSessions || []).filter(x => x.id !== id);
    state.passkeyError = null;
    state.passkeyRevoked = null;
    state.loginSignedOut = name || id || '';
    if (state.page === 'passkeys') render('push');
};

// onEvePasskeyRevoked marks the row pending rather than removing it: the
// credential is still in eve's mirror until eve's own next report drops it
// (decision 12) -- removing it here early would say "gone" before it is.
window.onEvePasskeyRevoked = function(id) {
    state.evePasskeys = (state.evePasskeys || []).map(function(p) {
        return p.id === id ? Object.assign({}, p, { revocation_pending: true }) : p;
    });
    state.passkeyError = null;
    if (state.page === 'passkeys') render('push');
};

window.onPasskeyError = function(msg) {
    state.passkeyError = msg || 'the passkey operation failed';
    if (state.page === 'passkeys') render('push');
};

// onLoginCodeMinted is the tray's channel into a window that was ALREADY open
// when the menu item was clicked. A window that was not open gets the same
// value seeded into its first paint instead (LOGIN_CODE_INIT) — see
// App.showLoginCode for why the two cases cannot share one mechanism.
window.onLoginCodeMinted = function(code) {
    state.loginCode = code || null;
    state.passkeyError = null;
    showPage('passkeys');
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
    html += `<div class="svc-resource-header ${open ? 'open' : 'closed'}" tabindex="0" role="button" aria-expanded="${open}" ${bind(toggleConfigSection, serviceId)}>`;
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
            html += `<button class="btn btn-primary" id="cfg-save-${esc(serviceId)}" ${dirty ? '' : 'disabled'} ${bind(saveConfig, serviceId)}>Save</button>`;
            html += `<button class="btn btn-danger" id="cfg-revert-${esc(serviceId)}" ${dirty ? '' : 'disabled'} ${bind(revertConfig, serviceId)}>Revert</button>`;
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
    html += `<div class="cfg-node-head" tabindex="0" role="button" aria-expanded="${expanded}" onclick="cfgToggleExpand(${bindIdx})">`;
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
    html += `<div class="cfg-node-head" tabindex="0" role="button" aria-expanded="${expanded}" onclick="cfgToggleExpand(${bindIdx})">`;
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
    html += `<div class="cfg-node-head" tabindex="0" role="button" aria-expanded="${expanded}" onclick="cfgToggleExpand(${bindIdx})">`;
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
    html += `<span class="cfg-item-toggle" tabindex="0" role="button" aria-expanded="${expanded}" onclick="cfgToggleExpand(${bindIdx})">${cfgChevron(expanded)}<span class="cfg-item-title">${esc(title || 'item')}</span></span>`;
    html += `<button class="btn btn-sm btn-danger cfg-item-remove" onclick="${removeCall}">Remove</button>`;
    html += '</div>';
    if (expanded) {
        html += '<div class="cfg-item-body">';
        if (isMap) {
            const curKey = path[path.length - 1];
            html += '<div class="cfg-leaf">';
            html += `<label for="cfgMapKey${bindIdx}">${esc(keyLabel)}</label>`;
            html += `<input type="text" id="cfgMapKey${bindIdx}" value="${esc(String(curKey))}" autocorrect="off" autocapitalize="off" spellcheck="false" onchange="cfgMapRename(${containerBindIdx}, ${indexOrKey}, this)"/>`;
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
    html += `<label for="${inputId}">${cfgNodeLabel(field)}${field.required ? ' *' : ''}</label>`;
    switch (field.type) {
        case 'bool':
            html += `<div class="toggle-row" style="margin-top:4px"><span style="font-size:12px;color:var(--text-2)">${esc(field.help || '')}</span><label class="switch"><input type="checkbox" id="${inputId}" aria-label="${cfgNodeLabel(field)}" ${value ? 'checked' : ''} onchange="cfgEdit(${bindIdx}, this)"/><span class="slider"></span></label></div>`;
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

// Delegated handler for Service Inspector action buttons and for every
// bind()-based control (see bind() near the top of the file). Wrapped in
// try/catch so a malformed data-row (or a bug in a handler) can't poison the
// document click queue — the listener stays subscribed for subsequent
// clicks even when one click fails.
document.addEventListener('click', function(e) {
    try {
        const actEl = e.target.closest && e.target.closest('[data-act]');
        if (actEl) {
            const entry = state._actBind[Number(actEl.dataset.act)];
            if (entry) entry[0].apply(null, entry[1]);
            return;
        }
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

// Enter/Space activates any keyboard-focusable control that isn't a native
// button — collapsible section headers and the audit table's expandable
// rows — by replaying it as a real click, which reaches whichever mechanism
// (bind()'s data-act or a data-* delegated handler) that element already
// uses. Space also scrolls the page by default; that's suppressed here.
document.addEventListener('keydown', function(e) {
    if (e.key !== 'Enter' && e.key !== ' ' && e.key !== 'Spacebar') return;
    const el = e.target.closest && e.target.closest(
        '.svc-resource-header, .cfg-node-head, .cfg-item-toggle, tr[data-act], [role="button"][tabindex]'
    );
    if (!el) return;
    e.preventDefault();
    el.click();
});

document.addEventListener('keydown', function(e) {
    if (!e.target.classList || !e.target.classList.contains('sidebar-item')) return;
    const items = Array.from(document.querySelectorAll('.sidebar-item'));
    const i = items.indexOf(e.target);
    if (i < 0) return;
    let next = -1;
    if (e.key === 'ArrowDown') next = (i + 1) % items.length;
    else if (e.key === 'ArrowUp') next = (i - 1 + items.length) % items.length;
    else if (e.key === 'Home') next = 0;
    else if (e.key === 'End') next = items.length - 1;
    else return;
    e.preventDefault();
    items[next].focus();
    items[next].click();
});

// Escape closes whichever inline form is open, through the same cancel
// function its own Cancel button uses -- one listener rather than a
// per-form keydown handler, since at most one of these forms is ever open
// at a time.
document.addEventListener('keydown', function(e) {
    if (e.key !== 'Escape') return;
    if (state.editingProjectId) { cancelProjectEdit(); return; }
    if (state.editingHostTemplateId) { cancelHostTemplateEdit(); return; }
    if (state.editingHostId) { cancelHostEdit(); return; }
    if (state.editingTemplateId) { cancelTemplateEdit(); return; }
    if (state.editingServiceId) { cancelServiceEdit(); return; }
    if (state.editingMcpId) { cancelMcpEdit(); return; }
    if (state.enrolForm) { cancelEnrolment(); return; }
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
//
// 'scope_violation' is last and spelled out differently on purpose: it is not
// a stored outcome (ADR-011 decision 7 keeps it a field on tool_error, not a
// fifth thing next to denied), but it is the query a security review reaches
// for right beside "denied", so the filter accepts it anyway — auditMatches
// and the Go side (AuditQuery.matches) both special-case this exact value.
const AUDIT_OUTCOMES = ['ok', 'error', 'tool_error', 'denied', 'unauthorized', 'throttled', 'pending', 'scope_violation'];
const AUDIT_OUTCOME_LABELS = { scope_violation: 'scope_violation (field, not an outcome)' };
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

// revealConfigDir/revealLogsDir back the Overview footer's two "Reveal"
// buttons; revealServiceLog backs each Services card's "Logs" link. All
// three just open Finder on a directory (ipc_overview.go) -- see
// ipcRevealServiceLog for why the id still has to be a real service's.
function revealConfigDir() {
    ipc(JSON.stringify({ type: 'reveal_config_dir' }));
}

function revealLogsDir() {
    ipc(JSON.stringify({ type: 'reveal_logs_dir' }));
}

function revealServiceLog(id) {
    ipc(JSON.stringify({ type: 'reveal_service_log', id: id }));
}

function setAuditFilter(key, value) {
    state.auditFilter[key] = value;
    // A text keystroke re-renders the whole Tool Calls page (render() always
    // replaces #content's innerHTML), which recreates the <input> and would
    // otherwise always land the caret at the end -- capture where it
    // actually was so restoreAuditFocus can put it back.
    if (key === 'text') {
        const el = document.getElementById('auditText');
        if (el) {
            state._auditTextSelStart = el.selectionStart;
            state._auditTextSelEnd = el.selectionEnd;
        }
    }
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
    if (f.outcome === 'scope_violation') {
        if (!ev.scope_violation) return false;
    } else if (f.outcome && ev.outcome !== f.outcome) {
        return false;
    }
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

// A scope violation is the one signal a reviewer must not have to expand the
// row to see (docs/access-profiles.md's "Checking that it worked"), and
// ev.error is typically empty for a tool_error in the first place — the MCP's
// reason lives in the result content, not in this field — so without the
// marker a scope-violating row can render identically to an ordinary one.
// Mirrors the Go CLI's auditDetail, deliberately: `grep scope_violation`
// should find the same calls whichever surface is being read.
function auditDetail(ev) {
    const base = auditBaseDetail(ev);
    if (!ev.scope_violation) return base;
    return base ? 'scope_violation: true  ' + base : 'scope_violation: true';
}

// auditScopeText renders the injected scope for the expanded row. null/absent
// and an empty object are different facts and must read as different
// sentences: null means this MCP declares no scope: "restrict" field at all,
// so there was nothing to inject; an empty object means it does declare one
// and this call's grant supplied no value for it — on a denied record that is
// the finding itself (ADR-011 decision 4's third defence). Mirrors the Go
// CLI's auditScopeSummary.
function auditScopeText(scope) {
    if (scope === undefined || scope === null) return '(no scope declared for this MCP)';
    const keys = Object.keys(scope).sort();
    if (keys.length === 0) return '(declared, but nothing was injected for this call)';
    return keys.map(function(k) {
        const v = scope[k];
        return k + '=' + (typeof v === 'string' ? v : JSON.stringify(v));
    }).join(', ');
}

// auditScopeBreadthText names every injected scope value that reaches further
// than a folder, in the same words the profile card and `relay grant` use.
// Mirrors Go's scopeBreadthWarnings.
function auditScopeBreadthText(scope) {
    if (scope === undefined || scope === null) return '';
    const out = [];
    for (const k of Object.keys(scope).sort()) {
        const phrase = scopeBreadthPhrase(scopeValueBreadth(scope[k]));
        if (phrase) out.push(k + ' is ' + phrase);
    }
    return out.join('; ');
}

function auditBaseDetail(ev) {
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
    let html = '<select id="auditFilter-' + esc(key) + '" aria-label="' + esc(label) + '" onchange="setAuditFilter(\'' + key + '\', this.value)">';
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
    html += auditSelect('outcome', 'Any outcome', AUDIT_OUTCOMES.map(o => [o, AUDIT_OUTCOME_LABELS[o] || o]), f.outcome);
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
    let html = '<tr class="row" tabindex="0" role="button" aria-expanded="' + expanded + '" ' + bind(toggleAuditRow, ev.id) + '>';
    html += '<td class="audit-time">' + esc(auditFmtTime(ev.ts)) + '</td>';
    html += '<td class="audit-outcome-cell"><span class="audit-pill audit-' + esc(ev.outcome) + '">' + esc(ev.outcome) + '</span>';
    // A scope violation is a distinct finding from an ordinary tool_error — a
    // client probed a resource boundary and the MCP refused it — and it must
    // be visible at a glance, not only after expanding the row.
    if (ev.scope_violation) {
        html += ' <span class="audit-badge audit-badge-scope" title="a resource boundary was probed and refused">scope</span>';
    }
    html += '</td>';
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
    // The authority this call actually ran with (ADR-011 decision 7): the
    // mode, the outbound grant, and the injected scope. ev.access is set only
    // when setAuthority ran — every project/remote call_tool, never a service
    // token or an event that named no MCP — so its presence is what decides
    // whether there is anything truthful to say here at all.
    if (ev.access !== undefined && ev.access !== null && ev.access !== '') {
        add('Access mode', ev.access);
        if (ev.allow_external === true) add('Outbound', 'allowed — this grant may reach outside the host');
        else if (ev.allow_external === false) add('Outbound', 'blocked — confined to this host');
        add('Scope', auditScopeText(ev.scope));
        // Issue #42. This is NOT a fourth reading of Scope: Scope's three
        // readings are about what the MCP declares, and this is about what the
        // OPERATOR declared and relay could not place. It used to be invisible
        // — the value was dropped, the call dispatched, and the record said
        // "(no scope declared for this MCP)", the reassuring one of the two.
        if (ev.scope_unplaced && ev.scope_unplaced.length) {
            add('Scope NOT applied', ev.scope_unplaced.join(', ')
                + ' — set by this grant, not declared by this MCP, so the call was denied');
        }
        // The other half of a truthful scope line (issue #41): a value can be
        // present, injected and enforced and still not be a confinement.
        const breadth = auditScopeBreadthText(ev.scope);
        if (breadth) add('Scope breadth', breadth);
    }
    add('Outcome', ev.outcome);
    // An intent with no completion sharing this id means relay invoked an MCP
    // and never learned the outcome. Worth surfacing, not worth hiding.
    add('Phase', ev.phase);
    add('Error', ev.error);
    // A scope violation is a distinct finding from an ordinary tool_error — it
    // means an MCP itself refused a resource-boundary probe (ADR-011 decision
    // 7) — so it gets its own line rather than folding into Outcome or Error.
    if (ev.scope_violation) add('Scope violation', 'yes — a resource boundary was probed and refused');
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
    const hasCaptured = state._auditTextSelStart !== undefined && state._auditTextSelEnd !== undefined;
    const start = hasCaptured ? Math.min(state._auditTextSelStart, n) : n;
    const end = hasCaptured ? Math.min(state._auditTextSelEnd, n) : n;
    try { el.setSelectionRange(start, end); } catch (e) { /* search inputs may refuse */ }
}

window.onAuditEvents = function(events, status) {
    state.auditEvents = events || [];
    state.auditStatus = status || null;
    state.auditLoaded = true;
    // The Overview tab's Audit tile and "Recent tool calls" read the same
    // two fields, so the first answer (fetched on landing there, same as
    // the Tool Calls tab) has to repaint it too.
    if (state.page === 'audit' || state.page === 'overview') render();
};

window.onAuditEvent = function(ev) {
    if (!ev) return;
    if (state.auditStatus) {
        state.auditStatus.recorded = (state.auditStatus.recorded || 0) + 1;
    }
    if (!state.auditFollow) return;
    state.auditEvents.unshift(ev);
    if (state.auditEvents.length > AUDIT_MAX_ROWS) state.auditEvents.length = AUDIT_MAX_ROWS;
    if (state.page === 'overview') render('push');
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

// A tray-minted code arrives with the first paint, so the window opens on the
// tab that shows it rather than on Services with the code out of sight. A
// tray-selected page does the same for a pending enrolment request, and loses
// to the code: the code is unrecoverable once this window closes and the
// request is not.
if (LOGIN_CODE_INIT) showPage('passkeys');
else showPage(INITIAL_PAGE || 'overview');

const sidebarVersionEl = document.getElementById('sidebarVersion');
if (sidebarVersionEl) sidebarVersionEl.textContent = 'relay ' + VERSION_INIT;

// Inline on* handlers in rendered HTML resolve against window. Bundling scopes
// these declarations to the module, so re-expose every top-level function (and
// the shared state object) on window — exactly the global surface the original
// classic <script> had.
Object.assign(window, {
    renderOverview, renderOverviewTiles, renderOverviewAttention, renderOverviewRecentToolCalls, renderOverviewFooter, overviewAttentionRows, serviceCounts, mcpHealthCounts, projectCounts, hostCounts, remoteTileValue, auditTile, pendingEnrolmentCount, revealConfigDir, revealLogsDir, revealServiceLog,
    auditBaseDetail, auditCaller, auditDetail, auditFmtTime, auditMatches, auditPretty, auditScopeBreadthText, auditScopeText, auditSelect, auditVisible, exportAudit, queryAudit, renderAudit, renderAuditDetail, renderAuditRow, restoreAuditFocus, revealAuditLog, setAuditFilter, toggleAuditFollow, toggleAuditRow,
    copyLoginCode, dismissLoginCode, pkSignCountText, refreshPasskeys, renderLoginCodeBanner, renderLoginSessions, renderPasskeys, revokePasskey, signOutLogin, renderEvePasskeys, revokeEvePasskey,
    approveEnrolmentRequestForm, cancelEnrolment, dismissEnrolBundle, enrolBudgetText, enrolBytes, enrolGrantNames, enrolGrantSummary, listEnrolmentRequests, newEnrolment, refuseEnrolmentRequest, remoteDraft, remoteDraftSet, remoteGrantableProjects, remoteListenIsLoopback, removeRemoteConfig, renderCAFingerprintLine, renderEnrolBundleBanner, renderEnrolmentForm, renderEnrolments, renderPendingEnrolmentRequests, renderPendingRequestFields, renderRemoteListener, renderRequestComparison, enrolRequestApprovable, toggleEnrolNoGrant, captureEnrolFormInputs, revokeEnrolment, saveEnrolment, saveRemoteConfig, toggleEnrolGrant,
    harvestProjectPermissions, mcpScopeFieldsFor, projAccessMode, projAllowExternal, projAllowedToolPatterns, projAllowedToolsText, projAuthorityRows, projFormAccessMode, projFormAllowExternal, projFormAllowExternalDefault, projGrantedMcpIds, projMissingScopeFields, projNoun, projScopeBreadthWarnings, projScopeGaps, projScopeText, projScopeValue, projToolAuthorityText, renderAuthorityRows, renderProjMcpPermissions, renderScopeFieldInput, renderScopeFieldPicker, renderScopeFieldTextInput, renderScopeChoices, renderScopeGapBanner, scopeBreadthPhrase, scopeCleanPath, scopeEntryBreadth, scopeTextFromValue, scopeValueBreadth, scopeValueFromText, scopeValueIsSet, scopeValueIsAsserted, scopeValueText, setProjAccess, setProjAllowExternal, setProjAllowedToolsText, setProjMcpGranted, setProjScopeText,
    captureProjectFormInputs, clearScopeValues, confirmScopeFieldEmpty, focusProjectFormIssue, isPolicyEmpty, refreshDependentScopeFields, requestScopeEnum, retryScopeEnum, scopeDependencyValues, scopeEnumKey, scopeEnumValueKey, scopeFieldByName, scopeFieldIsOpen, scopeFieldWasEverAsserted, scopeOpenKey, scopeSelectedValues, selectAllScopeValuesAt, toggleProjScopeValueAt, toggleScopeFieldPicker, unrecognisedScopeValues,
    addProjMount, removeProjMount, setProjMountAccess,
    isProjTemplatesWildcard, setProjTemplatesWildcard, toggleProjTemplate,
    openProjModelPicker, requestModelCatalog, setProjModelSearch, toggleProjModel, toggleProjModelsOther, renderProjModelList, renderProjModelPicker,
    blankTemplateForm, cancelTemplateEdit, captureTemplateFormInputs, editTemplate, newTemplate, removeTemplate, renderTemplateForm, saveTemplateForm, templateFormFromExisting, templateLines,
    cancelHostTemplateEdit, captureHostTemplateFormInputs, closeHostTemplateForm, editHostTemplate, editingHostRecord, hostTemplateCommandLine, newHostTemplate, removeHostTemplate, renderHostTemplateForm, renderHostTemplates, saveHostTemplateForm,
    blankHostForm, cancelHostEdit, captureHostFormInputs, disconnectHost, editHost, harvestHostForm, hostFormFromExisting, hostNameFor, isHostedForm, newHost, probeHost, removeHost, renderHostForm, renderHostProbeCard, renderHostProbeSummary, renderHostStatus, renderHosts, saveHostForm, setProjWhere, testHostConnection,
    mcpHealthPillFor, toggleMcpToolsDisclosure, renderMcpToolsDisclosure, formatUptime, serviceStatusLineHTML,
    addExternalMcp, addExternalMcpFromJson, addExternalMcpHttp, addService, authenticateMcp, blankProjectForm, cancelMcpEdit, cancelProjectEdit, cancelServiceEdit, confirmBroadScope, cfgArrayAdd, cfgArrayRemove, cfgBind, cfgChevron, cfgDirty, cfgEdit, cfgEditJson, cfgExpandKey, cfgFieldAt, cfgFirstMissingRequired, cfgGetDraft, cfgHasBadJson, cfgIsExpanded, cfgKvAdd, cfgKvRemove, cfgKvRename, cfgKvSetVal, cfgKvState, cfgMapAdd, cfgMapRemove, cfgMapRename, cfgNodeLabel, cfgRefreshChrome, cfgRerender, cfgSetExpanded, cfgToggleExpand, copyToClipboard, dispatchConfigOp, dispatchServiceAction, editProject, editService, harvestProjectForm, ipc, isAnyActionPending, isProjMcpWildcard, isProjModelsWildcard, isRemoteForm, isRemoteProject, moveService, newMcp, newProject, newService, projMcpState, projectFormFromExisting, pruneStaleDisabledTool, regenProjectSkill, removeExternalMcp, removeProject, removeService, render, renderActionButton, renderArrayBlock, renderConfigArray, renderConfigItem, renderConfigKeyValue, renderConfigLeaf, renderConfigMap, renderConfigNode, renderConfigObject, renderConfigSection, renderMcpForm, renderMcpPush, renderMcpServers, renderObjectFields, renderProjToolPicker, renderProjectForm, renderProjects, renderServiceEnvRows, renderServiceForm, renderServiceInspector, renderServicePanel, renderServiceStatus, renderServices, renderStatusPayload, resetMcpPermissions, revertConfig, rotateProjectToken, saveConfig, saveProjectForm, saveServiceEdit, serviceBadgeHTML, setMcpAddMode, setMcpTransport, setProjKind, setProjMcpState, setProjMcpWildcard, setProjModelsWildcard, setsEqual, showPage, svcEnvAddRow, svcEnvMergedForDisplay, svcEnvRemoveRow, svcEnvSetMode, svcEnvSetValue, svcEnvWireValue, svcFormValues, svcModelAddRow, svcModelRemoveRow, svcModelSetValue, svcModelsCapChanged, renderServiceModelRows, toggleConfigSection, toggleProjTool, toggleProjectTokenVisible, toggleServiceRunning, updateServiceAutostart, updateServiceMenuHidden, updateServiceStatusDOM});
window.state = state;
