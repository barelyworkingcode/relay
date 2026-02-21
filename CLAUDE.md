# Relay (Go)

Cross-platform MCP orchestrator. Tray app with token-authenticated Unix socket bridge, external MCP proxy, and background service management. macOS now, Windows later.

## Modes

- `relay` -- tray app (default). Hosts bridge socket, manages services, shows settings UI.
- `relay mcp --token TOKEN` -- stdio MCP server. Connects to tray app's bridge socket, proxies tool calls.
- `relay mcp register|unregister|list` -- CLI for external MCP server management. Writes settings.json directly; sends reconcile to tray app.
- `relay service register|unregister|list` -- CLI for service self-registration. Writes settings.json directly; tray app picks up changes via 2-second poll.

## Architecture

Zero built-in services or MCPs. All tools come from external MCP servers registered via CLI or settings UI.

```
main            Entry point
platform.go     Platform interface (UI abstraction)
cocoa_darwin.*  macOS Platform implementation (cgo + Obj-C)
trayapp.go      App lifecycle, menu, settings IPC, ToolRouter
settings.go     Config, token auth, permissions, ServiceConfig
settings_html.go  Settings WKWebView HTML/JS
external_mcp.go   Stdio MCP client (JSON-RPC 2.0)
mcp_cmd.go        CLI for mcp register/unregister/list
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

Token-based. SHA-256 hashed, only prefix/suffix stored. Per-service permissions: Off or On. Per-tool disable. Per-token context injected as `_meta` in tool calls to external MCPs. Context schema discovered from MCP's `serverInfo.contextSchema` during `initialize` handshake -- settings UI renders editors dynamically based on the schema. No tokens = all access blocked.

## Settings UI

IPC: `ipc(json)` JS wrapper -> `window.webkit.messageHandlers.ipc.postMessage` (macOS) or `window.chrome.webview.postMessage` (Windows).
Tabs: Services, MCP Servers, Security.

## Sibling Projects

- `../macMCP/` -- standalone Swift MCP server with 41 macOS-native tools. Installs to `~/.local/bin/macmcp`, self-registers via `relay mcp register`.
- `../fsMCP/` -- cross-platform TypeScript MCP server with 6 file system tools (read, write, edit, glob, grep, bash). Installs to `~/.local/bin/fsmcp`, self-registers via `relay mcp register`. Uses per-token `_meta.allowed_dirs` context for directory scoping.

## Key Files

- `cocoa_darwin.h/m` -- Obj-C: NSApplication, NSStatusBar, NSWindow+WKWebView, clipboard, dispatch, openURL
- `cocoa_darwin.go` -- cgo bridge: Go wrappers, DarwinPlatform, //export callbacks
- `trayapp.go` -- app lifecycle, menu, settings IPC, ToolRouter (external MCPs only)
- `settings_html.go` -- HTML/JS for settings WKWebView
- `settings.go` -- config, token auth, permissions, ServiceConfig (with URL field)
- `mcp_cmd.go` -- CLI: `relay mcp register|unregister|list`
- `service_cmd.go` -- CLI: `relay service register|unregister|list`
- `service_registry.go` -- background process management
- `external_mcp.go` -- stdio MCP client (JSON-RPC)

## Build

```bash
./build.sh   # builds relay -> /Applications/Relay.app
```

Requires: Go 1.22+, macOS.
