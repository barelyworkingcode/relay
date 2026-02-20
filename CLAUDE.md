# Relay (Go)

Cross-platform MCP orchestrator. Tray app with token-authenticated Unix socket bridge, external MCP proxy, and background service management. macOS now, Windows later.

## Modes

- `relay` -- tray app (default). Hosts bridge socket, manages services, shows settings UI.
- `relay mcp --token TOKEN` -- stdio MCP server. Connects to tray app's bridge socket, proxies tool calls.
- `relay service register|unregister|list` -- CLI for service self-registration. Writes settings.json directly; tray app picks up changes via 2-second poll.

## Architecture

Zero built-in services. All tools come from external MCP servers (like macMCP).

```
main            Entry point
platform.go     Platform interface (UI abstraction)
cocoa_darwin.*  macOS Platform implementation (cgo + Obj-C)
trayapp.go      App lifecycle, menu, settings IPC, ToolRouter
settings.go     Config, token auth, permissions, ServiceConfig
settings_html.go  Settings WKWebView HTML/JS
external_mcp.go   Stdio MCP client (JSON-RPC 2.0)
service_cmd.go    CLI for service register/unregister/list
service_registry.go  Background process management
bridge/         Unix socket IPC. Newline-delimited JSON. No internal deps.
mcp/            MCP types + stdio server (proxies to bridge)
```

## Platform Abstraction

`Platform` interface in `platform.go`: Init, Run, SetupTray, UpdateMenu, OpenSettings, EvalSettingsJS, CopyToClipboard, DispatchToMain, OpenURL.

- `cocoa_darwin.go` -- `DarwinPlatform` (macOS, cgo)
- `platform_windows.go` -- `WindowsPlatform` (stub)
- `init_darwin.go` -- `runtime.LockOSThread` (darwin build tag)
- `service_registry_unix.go` / `service_registry_windows.go` -- platform shell/process helpers

## Bridge

Socket: `<UserConfigDir>/relay/relay.sock`
Wire: newline-delimited JSON. Request types: `ListTools`, `CallTool`, `ReconcileExternalMcps`. Auth via token on every request.

## Security

Token-based. SHA-256 hashed, only prefix/suffix stored. Per-service permissions: Off, Read-only, Full. No tokens = all access blocked.

## Settings UI

IPC: `ipc(json)` JS wrapper -> `window.webkit.messageHandlers.ipc.postMessage` (macOS) or `window.chrome.webview.postMessage` (Windows).
Tabs: Services, MCP Servers, Security.

## macMCP (Sibling Project)

`../macMCP/` -- standalone Swift MCP server with all macOS-native tools (41 tools across 11 services). Registered in Relay as an external MCP server. Built and bundled into `Relay.app/Contents/MacOS/macmcp` by `build.sh`.

## Key Files

- `cocoa_darwin.h/m` -- Obj-C: NSApplication, NSStatusBar, NSWindow+WKWebView, clipboard, dispatch, openURL
- `cocoa_darwin.go` -- cgo bridge: Go wrappers, DarwinPlatform, //export callbacks
- `trayapp.go` -- app lifecycle, menu, settings IPC, ToolRouter (external MCPs only)
- `settings_html.go` -- HTML/JS for settings WKWebView
- `settings.go` -- config, token auth, permissions, ServiceConfig (with URL field)
- `service_cmd.go` -- CLI: `relay service register|unregister|list`
- `service_registry.go` -- background process management
- `external_mcp.go` -- stdio MCP client (JSON-RPC)

## Build

```bash
./build.sh   # builds macMCP + relay -> /Applications/Relay.app
```

Requires: Go 1.22+, Swift 5.9+, Xcode command line tools, macOS.
