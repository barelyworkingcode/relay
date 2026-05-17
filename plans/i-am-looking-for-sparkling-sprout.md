# Inline per-service RSS readout in the tray menu

## Context

When relay is running with several subprocesses (relayLLM, eve, scheduler, telegram, plus any stdio MCPs), it's currently impossible to tell from the tray which one is consuming memory — relayLLM is the expected hot spot but there's no visibility. The goal is a low-friction, glanceable readout: when the user opens the tray menu, each service row shows the resident memory used by that service **and all of its descendants** (services run via `/bin/sh -l -c` so the direct PID is a shell — the real workload is one level down).

Out of scope for this pass: stdio MCPs (fsMCP, macMCP — spawned by `ExternalMcpManager`, no tray row), CPU %, history/sparklines. Leave room to add a footer aggregate later if MCP overhead becomes interesting.

## Target UI

```
Relay
  ●  relayLLM         412 MB
  ●  eve               58 MB
  ○  relayScheduler         (stopped — no number)
  ●  relayTelegram     31 MB
  ─────────
  Settings...
  Exit
```

Dim, right-aligned, monospaced digits so values don't kern as they change. Refreshes every 2s via the existing `statusPoller`.

## Approach

1. **Sampler (one shell-out per tick).** `ps -axo pid=,ppid=,rss=` returns the whole process table in ~5ms on macOS. Parse once, build a PPID→children map, then BFS from each managed service PID summing RSS for the subtree. Naturally handles the `/bin/sh` wrapper case.

2. **Sample off the main thread in `statusPoller`**, cache the per-service result on `App`, then dispatch a menu rebuild on every tick (today the menu only rebuilds when settings reload from disk — that gating goes away).

3. **Extend the menu JSON wire format** with an `aux` string field. Cocoa renders it as a second right-aligned `NSTextField` inside `ToggleRowView`. Main label gets truncate-with-ellipsis so long service names don't collide.

## Files

| Action | Path | Notes |
|---|---|---|
| New | `process_stats_darwin.go` | `SampleRSSByRoot(roots []int) map[int]uint64` — shells out to `ps`, BFS subtree sum; 1s `CommandContext` timeout; warn-and-return-empty on failure |
| New | `process_stats_other.go` | `//go:build !darwin` stub returning empty map (keeps cross-compile clean) |
| Edit | `service_registry.go` | Add `PIDsByServiceID() map[string]int` to `ServiceManager` interface (line 17) and `*ServiceRegistry` impl. Skip dead/`Process == nil` entries while holding `r.mu`. No test fakes need updating (verified: only `*ServiceRegistry` implements the interface) |
| Edit | `trayapp.go` | (a) Add `rssMu sync.RWMutex` + `rssByID map[string]uint64` fields on `App` (~line 22). (b) In `statusPoller` (line 234): sample off-main-thread, store via setter, then *unconditionally* call `updateMenuWithSettings` (use `a.store.Get()` when `s == nil`). (c) In `updateMenuWithSettings` (line 263): add `Aux string` to `menuItem`, populate from snapshot for running services |
| Edit | `helpers.go` | Add `formatBytes(uint64) string` — emits `"412 MB"` / `"1.2 GB"`; one-decimal for ≥ GiB, integer for MiB, short enough to keep the right column narrow |
| Edit | `cocoa_darwin.m` | In the `isToggle` branch of `cocoa_update_menu` (lines 287–379): widen row from 250 → 290 pt; when `aux` non-empty, add a right-aligned `NSTextField` with `secondaryLabelColor` and `monospacedDigitSystemFontOfSize:11`; clamp main label/button width and set `NSLineBreakByTruncatingTail` so it can't overlap |

## Existing pieces being reused

- `cmd.Process.Pid` from `serviceProcess.cmd` (`service_registry.go:34`) — canonical PID source, no extra discovery needed.
- `StatusPollInterval = 2s` (`timeouts.go:43`) — already the desired refresh rate.
- `ServiceRegistry.OnProcessExit` callback (wired in `trayapp.go:102`) already triggers a menu rebuild when a process dies, so the "number disappears" path is free.
- `platform.UpdateMenu(json)` (`cocoa_darwin.go:93`) — only wire crossing into Cocoa; no new Cgo surface needed.

## Risks worth flagging before coding

1. **Menu flicker.** macOS rebuilds `NSMenu` via `removeAllItems` on each update. If the menu is open while a 2s tick fires, the visible menu may redraw. Mitigation: in `statusPoller`, marshal the new menu JSON and compare to a cached `lastMenuJSON` on `App`; skip `platform.UpdateMenu` if identical. Watch for hover-flicker during dogfood — if it persists, second-pass fix is patching aux labels in place by tag rather than tearing down (defer until/unless observed).

2. **Width jump.** 250 → 290 pt row is unconditional. Should be fine; verify visually against the Settings/Exit rows.

3. **`ps` stalls** (sleep/wake, system pressure). Sampler runs in a goroutine off the main thread with a 1s timeout — UI cannot stall on it.

## Verification

1. `./build.sh` (writes `/Applications/Relay.app`), launch Relay, open the tray menu.
2. Each running service shows a memory figure that updates within 2s. Toggling a service off removes the number; toggling on shows it within one tick.
3. Cross-check one service against `ps -axo pid,ppid,rss,command | grep <pid>` — values should match the subtree sum (within ~1 MB shell overhead).
4. Kill a service PID directly (`kill <pid>`): the toggle should flip off and the aux number should disappear within 2s (driven by the existing `OnProcessExit` callback, not the poll).
5. Add a fake service with a very long display name to confirm truncate-with-ellipsis behavior; aux text should stay right-aligned and readable.
6. Force `SampleRSSByRoot` to return `nil` once: menu still builds, aux blank, no crash.
