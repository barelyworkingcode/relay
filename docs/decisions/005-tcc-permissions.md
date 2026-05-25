# ADR-005: TCC Entitlements Live on Relay, MCPs Inherit via Attribution

**Status:** Accepted
**Date:** 2026-05-25

## Context

macMCP needs access to user-private data (Calendars, Contacts, Reminders,
Microphone) and to send AppleEvents (Mail, Messages). On macOS Sequoia under
hardened runtime, every protected service has two gates:

1. **A static entitlement** in the requesting bundle's signed
   `*.entitlements` (e.g. `com.apple.security.personal-information.calendars`).
   Without it, `tccd` silently denies any request to that service for that
   bundle — no prompt is ever shown to the user. Discovered in tccd's log as
   `Prompting policy for hardened runtime; service: kTCCServiceCalendar
   requires entitlement ... but it is missing`.
2. **A runtime grant** in TCC's database, established by an interactive
   user prompt and persisted by bundle identifier.

The natural-looking architecture — each MCP declares its own entitlements,
shows its own prompts, holds its own grants — fails on three fronts:

1. macMCP runs as `LSUIElement=true` and is spawned by Relay's tray (also
   `LSUIElement`) as a stdio subprocess. Modern macOS refuses to surface
   TCC prompts when both the requesting process and its responsible parent
   are background-only.
2. macMCP lives in `~/.local/bin/macmcp.app`, not `/Applications`.
   LaunchServices treats non-`/Applications` apps with extra suspicion;
   `open` returns `-600` without the right Info.plist scaffolding.
3. Adding `personal-information.*` entitlements to every MCP would make
   each MCP carry capability declarations it doesn't strictly need at the
   API-call layer (the API call is intermediated by Relay's primer, not
   issued directly by the MCP for new grants).

## Decision

**Relay carries the personal-information entitlements for the entire MCP
fleet. MCPs inherit the resulting TCC grants at runtime via TCC's
responsible-parent attribution.**

The flow:

1. MCPs declare which TCC services they need at registration time:
   `relay mcp register --name macMCP --command ... --tcc-services
   calendar,contacts,reminders,microphone,appleevents`. Stored on the
   `ExternalMcp.TccServices` field.
2. Relay's Settings UI surfaces a "Reset Permissions" button per
   TCC-using MCP. Clicking it runs the orchestrator in
   `mcp_permissions.go`:
   - `tccutil reset <Service> <bundleID>` per declared service to clear
     stale entries on the MCP's own bundle (those can override the
     inherited grant if left as `denied`).
   - `primeRelayTccPermissions` bumps Relay from `.accessory` to
     `.regular`, then calls `EKEventStore.requestFullAccess*` /
     `CNContactStore.requestAccess` from Relay's own process via the
     Cocoa primer functions in `cocoa_darwin.m`. Prompts surface labeled
     "Relay wants to access X", attributed to `com.barelyworkingcode.relay`.
   - Spawns the MCP with `--check-permissions` (using the same
     `exec.Command` shape as `external_mcp.go`'s normal stdio spawn) so
     the post-state status report comes from the runtime attribution
     context. The output is shown in the UI summary.
3. When the MCP actually serves a tool call at runtime, the
   responsible-parent chain `MCP → relay tray → ...` lets `tccd` use
   Relay's grant.

MCPs that don't need any TCC services (e.g. fsMCP) register without
`--tcc-services` and never see the button.

## Two distinct cases when adding a new capability

1. **TCC user-prompt service** (Camera, Photos, Screen Recording,
   Location, etc.): add to Relay. See the checklist below.
2. **An entitlement the MCP itself directly invokes** (e.g. macMCP's
   `com.apple.security.automation.apple-events` for spawning
   `osascript`, or hypothetically `com.apple.security.device.usb`):
   add to the MCP's own entitlements file. Relay can't help — the OS
   check happens at the MCP→syscall boundary.

## Consequences

**Positive:**

- Single source of truth for which TCC services Relay is willing to
  prompt for. Future MCPs add a `--tcc-services` flag and inherit
  Relay's grants for free.
- Users grant access once (to Relay) and every MCP that declares the
  service works. No per-MCP TCC inventory in System Settings.
- MCP authors don't need to write any TCC code — `PermissionsService`
  in macMCP is purely status-reporting (`--check-permissions`).

**Negative:**

- Adding a new TCC service is a 6-file change in Relay (entitlement,
  Info.plist, Cocoa primer, Go wrapper, primer dispatch, canonical
  name table). Tracked in the checklist below.
- TCC grants the user sees in System Settings say "Relay", not the
  individual MCP. Worth noting in user-facing docs.
- Bundles spawned by Relay must keep their own bundle's TCC entries
  clear of `denied` rows for the inherited grant to win — that's what
  the per-MCP `tccutil reset` step handles.

## Checklist: adding a new TCC service

Example — Photos (`com.apple.security.personal-information.photos-library`):

1. `Relay.entitlements`: add the entitlement key.
2. `Info.plist`: add `NSPhotoLibraryUsageDescription`.
3. `cocoa_darwin.{h,m}`: add `cocoa_request_tcc_photos(int timeoutSec)`
   (use `PHPhotoLibrary.requestAuthorization`). Add `-framework PhotoKit`
   to LDFLAGS in `cocoa_darwin.go`.
4. `cocoa_darwin.go`: add `cocoaRequestTccPhotos` wrapper.
5. `mcp_permissions.go`: confirm the service is in `canonicalTccService`
   (most are pre-registered) and `tccutilServiceName`.
6. `mcp_permissions_darwin.go`: add `case "photos":` arm to
   `primeRelayTccPermissions`.
7. MCP registers with `--tcc-services photos`. Nothing else on the MCP
   side.

## Why not other approaches

- **Each MCP carries its own entitlements + does its own prompts.**
  Tried; `LSUIElement`/background spawn chains suppress prompts even
  with the right entitlements. Also forces every MCP author to ship a
  TCC subsystem.
- **Have the user grant access via System Settings manually.** Modern
  System Settings doesn't have a "+" button for Calendars/Contacts/
  Reminders — apps only appear after a successful prompt has been
  shown. No manual fallback exists.
- **Use `open ~/.local/bin/macmcp.app --args --request-permissions`.**
  Tried during development. TCC walks the responsible-process chain
  back to the calling terminal (Terminal/iTerm), creating a grant for
  the wrong bundle.
- **Notarize and skip the entitlement story.** Notarization is
  orthogonal — we verified via spctl that an unnotarized Developer ID
  bundle with the right entitlements does get prompts.

## Reference

- `mcp_permissions.go` — orchestrator, tccutil reset loop,
  `bundleIDFromCommand`.
- `mcp_permissions_darwin.go` — primer loop dispatch.
- `cocoa_darwin.m` — `cocoa_request_tcc_*` functions, activation-policy
  helpers, `WKUIDelegate` for the confirm dialog.
- `Relay.entitlements` — the canonical entitlement set.
- `ipc_mcp_permissions.go` + `settings.html` — IPC handler and UI button.
- `../macMCP/Sources/macMCP/Services/PermissionsService.swift` — the
  read-only status surface from the MCP side.
