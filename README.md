# Relay

MCP orchestrator for macOS (Windows planned). Proxies external MCP servers through a single token-authenticated connection. Runs as a menubar app with a Unix socket bridge. Per-service read/write permissions. Built with Go.

macOS-native tools (Calendar, Contacts, Mail, Messages, etc.) are provided by [macMCP](../macMCP/), a standalone Swift MCP server bundled into the app.

## Prerequisites

- macOS 13+
- Go 1.22+
- Swift 5.9+ (included with Xcode 15+)
- Xcode Command Line Tools (`xcode-select --install`)

## Repository Layout

`build.sh` expects macMCP as a sibling directory:

```
parent/
  relayGo/       # this repo
  macMCP/         # Swift MCP server (../macMCP relative to relayGo)
```

## Build & Install

```bash
./build.sh
```

This does four things in order:

1. **Builds macMCP** -- `swift build -c release` in `../macMCP/`. Produces `.build/release/macmcp`. No external Swift dependencies; links system frameworks (EventKit, Contacts, CoreLocation, Foundation).
2. **Builds Relay** -- `CGO_ENABLED=1 go build` in the current directory. Links Cocoa and WebKit via cgo.
3. **Assembles `Relay.app`** -- Copies both binaries into `Contents/MacOS/`, generates the app icon via `gen-icon.swift`, codesigns with your first available identity (falls back to ad-hoc).
4. **Installs** -- Moves the bundle to `/Applications/Relay.app`.

### Building macMCP standalone

If you only need to rebuild macMCP:

```bash
cd ../macMCP
swift build -c release
# binary at .build/release/macmcp
```

Test it directly over stdio:

```bash
echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}' | .build/release/macmcp
```

## Usage

1. Launch Relay from `/Applications/Relay.app`
2. Open Settings from the menubar icon
3. Generate a token in the Security tab
4. Click "Copy MCP Config" and paste into your Claude Desktop config:

```json
{
  "relay": {
    "command": "/Applications/Relay.app/Contents/MacOS/relay",
    "args": ["mcp", "--token", "YOUR_TOKEN"]
  }
}
```

## Execution Modes

- **`relay`** (default) -- menubar tray app. Hosts a bridge socket for MCP.
- **`relay mcp --token <value>`** -- stdio MCP server. Connects to the tray app's bridge socket.

The tray app must be running before `relay mcp` can connect.

## Security

- All access requires a token. No tokens configured = all access blocked.
- Tokens are 32 random bytes (64 hex chars). Only SHA-256 hash + prefix/suffix stored.
- Per-token, per-service permissions: **Off** (hidden), **Read-only**, **Full**.
- Tool calls proxy through Unix socket to the tray app, which holds macOS TCC permissions.

## Registering macMCP

macMCP is bundled at `Relay.app/Contents/MacOS/macmcp` but Relay does not auto-register it. After first launch:

1. Open **Settings > MCP Servers**
2. Fill in the form:
   - **Display name:** `macMCP`
   - **Command:** `/Applications/Relay.app/Contents/MacOS/macmcp`
3. Click **Add**

Relay will spawn macmcp, run tool discovery, and list all 41 tools. Token permissions for macMCP default to Full for all existing tokens.

## macMCP Tools

Provides 41 tools across 11 services:

| Service | Tools | Implementation |
|---------|-------|----------------|
| Calendar | list, list_events, create_event | EventKit |
| Contacts | list, get, create, update, delete, groups, search | CNContactStore |
| Reminders | list, create, complete | EventKit |
| Location | current, geocode, reverse_geocode | CoreLocation |
| Maps | search, open, directions | CLGeocoder + Apple Maps |
| Capture | screenshot, audio | screencapture, afrecord |
| Mail | accounts, mailboxes, emails, search, send, move, mark | JXA via osascript |
| Messages | list_chats, get_chat, send | SQLite + AppleScript |
| Shortcuts | list, run | /usr/bin/shortcuts |
| Utilities | play_sound | afplay |
| Weather | current, forecast, hourly | Open-Meteo API |

## Services

Manage background processes via Settings > Services. Commands run through a login shell so your shell profile is available. Services with a URL field open the browser on tray click. Logs: `~/Library/Application Support/relay/logs/<id>.log`.

## External MCP Servers

Add third-party stdio MCP servers via Settings > MCP Servers. Relay proxies their tools with the same permission model. Supports tool annotations (`readOnlyHint`) for granular read-only permissions.

## License

MIT
