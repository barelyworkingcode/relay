# Relay

MCP orchestrator for macOS (Windows planned). Proxies external MCP servers through a single token-authenticated connection. Runs as a menubar app with a Unix socket bridge. Per-service and per-tool permissions. Built with Go.

## Prerequisites

- macOS 13+
- Go 1.22+

## Build & Install

```bash
./build.sh
```

Builds the Go binary with CGO, assembles `Relay.app` (with generated icon and codesigning), and installs to `/Applications/Relay.app`.

For notarized release builds:

```bash
./build.sh --release
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
- **`relay mcp register|unregister|list`** -- CLI for external MCP server management.
- **`relay service register|unregister|list`** -- CLI for background service management.

The tray app must be running before `relay mcp --token` can connect.

## Security

- All access requires a token. No tokens configured = all access blocked.
- Tokens are 32 random bytes (64 hex chars). Only SHA-256 hash + prefix/suffix stored.
- Per-token, per-service permissions: **Off** (hidden) or **On**.
- Individual tools can be disabled per-token.
- Tool calls proxy through Unix socket to the tray app, which holds macOS TCC permissions.

## External MCP Servers

Register external stdio MCP servers via CLI or Settings UI. Relay proxies their tools through the same authenticated connection.

### CLI Registration

```bash
relay mcp register --name macMCP --command ~/.local/bin/macmcp
relay mcp list
relay mcp unregister --name macMCP
```

`register` is idempotent -- re-running with the same name updates the existing entry. Sends a reconcile signal to the running tray app so changes take effect immediately.

Flags: `--name` (required), `--command` (required), `--args` (repeatable), `--env KEY=VALUE` (repeatable), `--id` (defaults to slugified name).

## Services

Manage background processes via Settings > Services or the CLI. Commands run through a login shell so your shell profile is available. Services with a URL field open the browser on tray click.

### CLI Registration

```bash
relay service register --name Eve --command node --args server.js --workdir . --url http://localhost:3000 --autostart
relay service list
relay service unregister --name Eve
```

`register` is idempotent -- re-running with the same name updates the existing entry. `--workdir` is resolved to an absolute path. The tray app picks up changes within ~2 seconds.

Flags: `--name` (required), `--command` (required), `--args` (repeatable), `--workdir`, `--url`, `--autostart`, `--env KEY=VALUE` (repeatable), `--id` (defaults to slugified name).

## macMCP

macOS-native tools (Calendar, Contacts, Mail, Messages, etc.) are provided by [macMCP](../macMCP/), a standalone Swift MCP server. It installs independently and self-registers with Relay via `relay mcp register`.

## License

MIT
