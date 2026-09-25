// The Projects form's model picker, as pure functions over the catalog view
// relay emits through window.onModelsListed. No state, window or ipc here:
// app.js owns the selection and passes it in, so every function is testable
// under goja on its own.
//
// A catalog row is { id, label, group, provider, kind, target? }. A picker
// group is { label, kind, rows }, where kind is "unavailable" | "chat" |
// "other" | "saved", and each row is { id, label, target, unavailable }.

import { esc } from './pure.js';

const MODEL_WILDCARD = '*';
const GROUP_UNAVAILABLE = 'Not currently available';
const GROUP_OTHER = 'Other';
const GROUP_SAVED = 'Saved';

function viewIsOk(view) {
    return !!view && view.status === 'ok';
}

function savedModelIds(saved) {
    const seen = new Set();
    const out = [];
    for (const id of (saved || [])) {
        if (id === MODEL_WILDCARD || seen.has(id)) continue;
        seen.add(id);
        out.push(id);
    }
    return out;
}

// unavailableSavedModels answers only against an ok view: with no list to
// compare against, every saved id would read as missing, which is not true.
function unavailableSavedModels(saved, view) {
    if (!viewIsOk(view)) return [];
    const known = new Set((view.models || []).map(m => m.id));
    return savedModelIds(saved).filter(id => !known.has(id));
}

function savedOnlyRow(id, unavailable) {
    return { id: id, label: id, target: '', unavailable: unavailable };
}

function catalogRow(m) {
    return { id: m.id, label: m.label || m.id, target: m.target || '', unavailable: false };
}

function groupModelCatalog(view, saved) {
    if (!viewIsOk(view)) {
        const rows = savedModelIds(saved).map(id => savedOnlyRow(id, false));
        return rows.length ? [{ label: GROUP_SAVED, kind: 'saved', rows: rows }] : [];
    }
    const groups = [];
    const missing = unavailableSavedModels(saved, view);
    if (missing.length) {
        groups.push({ label: GROUP_UNAVAILABLE, kind: 'unavailable', rows: missing.map(id => savedOnlyRow(id, true)) });
    }
    const chatByLabel = {};
    const other = [];
    for (const m of (view.models || [])) {
        if (m.kind === 'other') {
            other.push(catalogRow(m));
            continue;
        }
        const label = m.group || '';
        if (!chatByLabel[label]) {
            chatByLabel[label] = { label: label, kind: 'chat', rows: [] };
            groups.push(chatByLabel[label]);
        }
        chatByLabel[label].rows.push(catalogRow(m));
    }
    if (other.length) groups.push({ label: GROUP_OTHER, kind: 'other', rows: other });
    return groups;
}

function containsFolded(haystack, needle) {
    return String(haystack || '').toLowerCase().indexOf(needle) >= 0;
}

function filterModelGroups(groups, text) {
    const needle = String(text || '').trim().toLowerCase();
    const out = [];
    for (const g of (groups || [])) {
        const rows = (!needle || containsFolded(g.label, needle))
            ? g.rows
            : g.rows.filter(r => containsFolded(r.id, needle) || containsFolded(r.label, needle) || containsFolded(r.target, needle));
        if (rows.length) out.push({ label: g.label, kind: g.kind, rows: rows });
    }
    return out;
}

function renderModelPickerBanner(view, pending) {
    if (pending) {
        return '<div id="projModelsBanner" class="model-banner pending">Loading models…</div>';
    }
    if (!view) return '';
    const retry = ' <button type="button" class="btn btn-sm" onclick="requestModelCatalog()">Retry</button>';
    if (!viewIsOk(view)) {
        return '<div id="projModelsBanner" class="model-banner failed">Could not list models: '
            + esc(view.error || 'unavailable')
            + '. Saved models are kept.' + retry + '</div>';
    }
    const warnings = view.warnings || [];
    if (!warnings.length) return '';
    return '<div id="projModelsBanner" class="model-banner warn">' + esc(warnings.join('; ')) + '.' + retry + '</div>';
}

// opts: { selected: string[], bindToggle(id) -> attribute text,
//         otherOpen: bool, searching: bool }
function renderModelPickerList(groups, opts) {
    const o = opts || {};
    if (!groups || !groups.length) {
        return '<div class="model-empty">' + (o.searching ? 'No models match.' : 'No models listed.') + '</div>';
    }
    const selected = new Set(o.selected || []);
    let html = '';
    for (const g of groups) {
        html += '<div class="model-group" data-model-group="' + esc(g.label) + '" data-model-group-kind="' + esc(g.kind) + '">';
        if (g.kind === 'other') {
            const open = !!(o.otherOpen || o.searching);
            html += '<button type="button" id="projModelsOtherToggle" class="model-group-toggle" onclick="toggleProjModelsOther()" aria-expanded="' + (open ? 'true' : 'false') + '">'
                + (open ? '▾ ' : '▸ ') + esc(g.label) + ' <span class="model-group-count">(' + g.rows.length + ')</span></button>';
            if (!open) {
                html += '</div>';
                continue;
            }
        } else {
            html += '<div class="model-group-title">' + esc(g.label) + '</div>';
        }
        for (const r of g.rows) html += renderModelRow(r, selected.has(r.id), o.bindToggle);
        html += '</div>';
    }
    return html;
}

function renderModelRow(row, checked, bindToggle) {
    const act = bindToggle ? ' ' + bindToggle(row.id) : '';
    let html = '<label class="model-row' + (row.unavailable ? ' unavailable' : '') + '">';
    html += '<input type="checkbox" data-model-id="' + esc(row.id) + '"' + (checked ? ' checked' : '') + act + ' />';
    html += '<span class="model-label">' + esc(row.label) + '</span>';
    if (row.label.indexOf(row.id) < 0) html += '<span class="model-id">' + esc(row.id) + '</span>';
    if (row.target) html += '<span class="model-alias-tag">alias</span>';
    if (row.unavailable) html += '<span class="model-unavailable">not currently available</span>';
    html += '</label>';
    return html;
}

export {
    unavailableSavedModels, groupModelCatalog, filterModelGroups, renderModelPickerBanner, renderModelPickerList
};
