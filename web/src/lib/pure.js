// Pure, DOM-free settings logic — JSONC parsing, config-tree path ops, value
// coercion, schema-required scanning, and value formatting. Extracted verbatim
// from settings.html so it can be unit-tested in isolation (see
// settings_js_test.go, which runs these under goja). app.js imports them; the
// bundler inlines everything back into one script for the WKWebView.

// Loose ISO 8601 detector — matches what Go's time.Format(time.RFC3339)
// emits ("2026-05-24T03:14:01Z" or with offset). Restrictive enough that
// an alias like "foo-bar" or a port like "8090" won't accidentally match.
const ISO_8601_RE = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})$/;

function esc(s) {
    return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;').replace(/'/g,'&#39;');
}

function formatScalar(v) {
    if (v === null || v === undefined) return '';
    if (typeof v === 'boolean') return v ? 'true' : 'false';
    if (typeof v === 'string' && ISO_8601_RE.test(v)) return formatRelativeTime(v);
    return String(v);
}

// Render an RFC3339 timestamp as "5m ago" / "2h 14m ago" / "3d ago" so
// humans can read it. Falls back to the raw string on parse failure.
function formatRelativeTime(iso) {
    const t = Date.parse(iso);
    if (isNaN(t)) return iso;
    let delta = Math.max(0, Math.floor((Date.now() - t) / 1000));
    if (delta < 5) return 'just now';
    if (delta < 60) return delta + 's ago';
    const m = Math.floor(delta / 60);
    if (m < 60) return m + 'm ago';
    const h = Math.floor(m / 60);
    const remM = m % 60;
    if (h < 24) return remM > 0 ? h + 'h ' + remM + 'm ago' : h + 'h ago';
    const d = Math.floor(h / 24);
    const remH = h % 24;
    return remH > 0 ? d + 'd ' + remH + 'h ago' : d + 'd ago';
}

function cfgStripJsonComments(s) {
    let out = '';
    let i = 0;
    const n = s.length;
    let inStr = false, q = '', esc = false;
    while (i < n) {
        const c = s[i], c2 = s[i + 1];
        if (inStr) {
            out += c;
            if (esc) esc = false;
            else if (c === '\\') esc = true;
            else if (c === q) inStr = false;
            i++; continue;
        }
        if (c === '"' || c === "'") { inStr = true; q = c; out += c; i++; continue; }
        if (c === '/' && c2 === '/') { i += 2; while (i < n && s[i] !== '\n') i++; continue; }
        if (c === '/' && c2 === '*') { i += 2; while (i < n && !(s[i] === '*' && s[i + 1] === '/')) i++; i += 2; continue; }
        out += c; i++;
    }
    return out;
}

// cfgParseConfigText parses raw (possibly JSONC) file text: strip // and /* */
// comments outside strings, then JSON.parse. Returns undefined on failure (the
// server is the authority on validity; this is best-effort for rendering).
// Comments do not survive a save — the draft is re-serialized as plain JSON.
function cfgParseConfigText(text) {
    try {
        return JSON.parse(cfgStripJsonComments(text));
    } catch (e) {
        return undefined;
    }
}

// cfgGetAt / cfgSetAt walk a path (array of string keys / numeric indices)
// into the draft tree. cfgSetAt lazily creates intermediate containers so an
// edit to a leaf in an absent section materializes only that chain.
function cfgGetAt(obj, path) {
    let cur = obj;
    for (const k of path) {
        if (cur === null || cur === undefined) return undefined;
        cur = cur[k];
    }
    return cur;
}

function cfgSetAt(obj, path, value) {
    let cur = obj;
    for (let i = 0; i < path.length - 1; i++) {
        const k = path[i];
        if (cur[k] === null || typeof cur[k] !== 'object') {
            cur[k] = (typeof path[i + 1] === 'number') ? [] : {};
        }
        cur = cur[k];
    }
    cur[path[path.length - 1]] = value;
}

function cfgDefaultFor(field) {
    switch (field.type) {
        case 'object':    return {};
        case 'array':     return [];
        case 'map':       return {};
        case 'bool':      return false;
        case 'stringMap': return {};
        case 'string[]':  return [];
        case 'number':    return null;
        case 'json':      return null;
        default:          return ''; // text/textarea/select/secret
    }
}

// cfgCoerce converts a leaf input's raw DOM value into the typed JS value the
// schema expects (mirrors the old resource-form harvest logic).
function cfgCoerce(type, el) {
    switch (type) {
        case 'bool':
            return el.checked;
        case 'number': {
            const txt = el.value.trim();
            return txt === '' ? null : Number(txt);
        }
        case 'string[]':
            return el.value.split('\n').map(s => s.trim()).filter(Boolean);
        case 'stringMap': {
            const out = {};
            for (const line of el.value.split('\n')) {
                const eq = line.indexOf('=');
                if (eq <= 0) continue;
                out[line.slice(0, eq).trim()] = line.slice(eq + 1).trim();
            }
            return out;
        }
        default:
            return el.value;
    }
}

// cfgKvCoerce infers a typed value from text so booleans and numbers survive the
// round-trip to JSON (a llama bool flag must stay a real bool, not "true").
function cfgKvCoerce(text) {
    const t = text.trim();
    if (t === '') return '';
    if (t === 'true') return true;
    if (t === 'false') return false;
    if (/^-?\d+(\.\d+)?$/.test(t)) return Number(t);
    return t;
}

function cfgKvDisplay(v) {
    if (v === true) return 'true';
    if (v === false) return 'false';
    if (v === null || v === undefined) return '';
    return String(v);
}

function cfgScanRequired(fields, value) {
    const obj = (value && typeof value === 'object') ? value : {};
    for (const f of fields) {
        const v = obj[f.id];
        if (f.type === 'object') {
            const m = cfgScanRequired(f.fields || [], v);
            if (m) return m;
        } else if (f.type === 'array') {
            if (Array.isArray(v)) for (const item of v) {
                const m = cfgScanRequired((f.item && f.item.fields) || [], item);
                if (m) return m;
            }
        } else if (f.type === 'map') {
            if (v && typeof v === 'object') for (const k of Object.keys(v)) {
                const m = cfgScanRequired((f.item && f.item.fields) || [], v[k]);
                if (m) return m;
            }
        } else if (f.required) {
            if (v === undefined || v === null || v === '' || (Array.isArray(v) && v.length === 0)) {
                return f.label || f.id;
            }
        }
    }
    return null;
}

// cfgSummary returns a one-line preview of a collection item (the first
// non-empty scalar field) so a collapsed row is identifiable at a glance.
function cfgSummary(itemField, value) {
    if (value && typeof value === 'object' && itemField && itemField.fields) {
        for (const f of itemField.fields) {
            const v = value[f.id];
            if (typeof v === 'string' && v.trim()) return v;
            if (typeof v === 'number') return String(v);
        }
    }
    return '';
}

function cfgFormatStringMap(v) {
    if (!v || typeof v !== 'object') return '';
    return Object.keys(v).map(k => k + '=' + v[k]).join('\n');
}

function cfgFormatJson(v) {
    if (v === undefined || v === null) return '';
    try { return JSON.stringify(v, null, 2); } catch (e) { return ''; }
}

function oneLineProj(s) {
    return String(s).replace(/\s+/g, ' ').slice(0, 140);
}

export {
    esc, formatScalar, formatRelativeTime, cfgStripJsonComments, cfgParseConfigText, cfgGetAt, cfgSetAt, cfgDefaultFor, cfgCoerce, cfgKvCoerce, cfgKvDisplay, cfgScanRequired, cfgSummary, cfgFormatStringMap, cfgFormatJson, oneLineProj, ISO_8601_RE
};
