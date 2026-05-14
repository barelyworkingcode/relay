# Eve: drop hardcoded PTY fallback, trust the server

## Context

Now that relayLLM owns PTY templates in `config.json` under `pty: {...}` and ships them on demand, eve should stop carrying its own hardcoded list of built-ins. Today, `eve/public/dialogs/shell-launcher-dialog.js:101-105` defines an inline fallback array (`claude-code`, `opencode`, `shell`) that takes over whenever `state.terminalTemplates` is empty. This is the "static" thing the user wants to eliminate: it duplicates the server's source of truth, drifts independently (e.g. claude-code's `icon` differs between the fallback and the server seed), and would silently hide a misconfigured/disconnected relayLLM behind a plausible-looking default.

After this change, eve renders only what the server returns. If the server hasn't responded yet, the launcher shows a brief loading state.

## Decisions (locked from clarifying questions)

- **Fetch strategy:** Keep current behavior — one-shot `terminalManager.requestTemplates()` at WebSocket connect (`app.js:492`). No re-query on every dialog open.
- **Arrangement:** Render in whatever order the server returns. Server already sorts alphabetically by ID (`TemplateStore.List()` in relayLLM), so claude-code → opencode → shell → any custom UUIDs. Eve does no client-side sorting.

## Files to change

### `eve/public/dialogs/shell-launcher-dialog.js`

Single edit in `_renderNewTab()`, around lines 100-115:

**Before:**
```js
const templates = this.state.terminalTemplates.length > 0 ? this.state.terminalTemplates : [
  { id: 'claude-code', name: 'Claude Code', description: 'Claude Code CLI agent', icon: 'claude-code' },
  { id: 'opencode',    name: 'OpenCode',    description: 'OpenCode CLI agent',    icon: 'terminal' },
  { id: 'shell',       name: 'Shell',       description: 'Default system shell',  icon: 'shell' },
];

for (const tmpl of templates) {
  grid.appendChild(this._createCard({ ... }));
}
```

**After:**
```js
const templates = this.state.terminalTemplates;

if (templates.length === 0) {
  // Server hasn't responded yet (first-fetch in flight). The dialog
  // re-renders when TERMINAL_TEMPLATES_LOADED fires (init() line 19-21).
  const loading = document.createElement('div');
  loading.className = 'shell-launcher__empty';
  loading.textContent = 'Loading terminal templates…';
  grid.appendChild(loading);
} else {
  for (const tmpl of templates) {
    grid.appendChild(this._createCard({
      iconHtml: this._iconSVG(tmpl.icon || tmpl.id),
      name: tmpl.name,
      description: tmpl.description || '',
      onClick: () => this._launchTerminal(tmpl.id),
      testid: `shell-card-${tmpl.id}`,
    }));
  }
}
```

That's it for behavior. Everything else already works:

- `app.js:492` already calls `requestTemplates()` once when terminalManager becomes ready.
- `message-dispatcher.js:256-265` already populates `state.terminalTemplates` and emits `EVT.TERMINAL_TEMPLATES_LOADED` on every inbound `terminal_templates` message.
- The launcher already listens at `init()` (lines 19-21): `bus.on(EVT.TERMINAL_TEMPLATES_LOADED, () => { if (this.isVisible) this._showTab('new'); })` — so the loading placeholder gets replaced automatically when the response lands.
- Server-side ordering is already deterministic and applied (`TemplateStore.List()` sorts by ID).

### What does NOT change

- No new WS messages.
- No new HTTP routes.
- No changes to `terminal-manager.js`, `message-dispatcher.js`, `state-store.js`, `app.js`.
- `_iconSVG()` switch stays as-is — it handles `claude-code` / `shell` specially and falls back to `terminal` for everything else. The call site `this._iconSVG(tmpl.icon || tmpl.id)` already prefers the server's icon field, falling back to the ID.

## Observation (not blocking, log for later)

In the current relayLLM seed, `claude-code` ships with `icon: "terminal"`. That makes `_iconSVG('terminal')` resolve to the terminal icon. The deleted hardcoded fallback used `icon: 'claude-code'`, which resolved to the chat icon. Once the fallback is gone, claude-code will consistently show the terminal icon — minor visual change. If you want the chat-style icon back, edit the user's `config.json` to set `pty.claude-code.icon = "claude-code"` (the `_iconSVG` switch already handles that key). No code change needed.

## Verification

eve is browser-only HTML/JS — no build step for this change.

1. **First-load smoke** — restart eve + relayLLM. Open the shell launcher. Expect three cards (claude-code, opencode, shell) in that order. Optionally observe DevTools network panel for the `terminal_templates` WS round-trip.
2. **Disconnected state** — kill relayLLM's process, force-reload eve so `state.terminalTemplates` starts empty. Open the launcher. Expect "Loading terminal templates…" placeholder (instead of the old hardcoded three cards). This is the desired honest signal that the source of truth is offline.
3. **Custom template appears** — via the existing API: `curl --unix-socket … -X POST /api/terminal/templates -d '{"name":"My REPL","command":"/bin/echo","args":["hi"],"icon":"terminal"}'`. Reload eve, open launcher. Expect the custom card after the built-ins (alphabetical UUID sort places it wherever its ID falls — usually after `shell`).
4. **Server-edited config** — manually edit `~/Library/Application Support/relayLLM/config.json`, rename `pty.opencode` to `pty.aaa-test` and tweak its name. Restart relayLLM. Reload eve. Expect the renamed card to appear first (sorted alphabetically by ID).
5. **Spawning still works** — click any card. Terminal spawns as before. `_launchTerminal(templateId)` is unchanged.

## Out of scope (next conversation)

- Re-querying templates on every launcher open (so live config edits show without an eve reload). The user explicitly declined this for now.
- relayLLM broadcasting `terminal_templates` to all clients on pty config change.
- Section header "Terminals" in the launcher grid to separate from chat templates.
- Built-ins-first sort or visual "custom" tag.
- Per-template icon expansion (free-form icon strings, e.g. emoji or a server-supplied SVG URL).
