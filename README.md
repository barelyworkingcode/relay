# Relay

macOS MCP orchestrator and project manager. Manages external MCP servers, background services, and project-scoped access control through a menubar tray app with a Unix socket bridge. Built with Go.

## Architecture

```mermaid
graph TB
    subgraph Relay["Relay (tray app)"]
        Settings["settings.json<br/><i>projects, MCPs, services</i>"]
        Bridge["Bridge Socket<br/><i>relay.sock</i>"]
        Router["Router<br/><i>auth, filter, _meta inject</i>"]
        MCPMgr["ExternalMcpManager<br/><i>spawns MCP processes</i>"]
        SvcReg["ServiceRegistry<br/><i>spawns services</i>"]
    end

    subgraph MCPs["MCP Servers"]
        fsMCP["fsMCP<br/><i>file system tools</i>"]
        macMCP["macMCP<br/><i>macOS native tools</i>"]
        Other["searchMCP, Krisp, ..."]
    end

    subgraph Services["Managed Services"]
        relayLLM["relayLLM<br/><i>LLM execution engine</i>"]
        Eve["Eve<br/><i>browser frontend</i>"]
        Scheduler["relayScheduler"]
        Telegram["relayTelegram"]
    end

    Browser["Browser"] -->|WebAuthn| Eve
    Eve -->|RELAY_LLM_TOKEN| relayLLM
    relayLLM -->|"relay mcp (project token)"| Bridge
    Bridge --> Router
    Router --> MCPMgr
    MCPMgr --> fsMCP & macMCP & Other
    SvcReg -->|spawns| relayLLM & Eve & Scheduler & Telegram
    Settings -.->|config| Router & MCPMgr & SvcReg
```

## Request Flow

How a user prompt becomes a tool call:

```mermaid
sequenceDiagram
    participant B as Browser
    participant E as Eve
    participant L as relayLLM
    participant LLM as LLM API
    participant R as relay mcp
    participant Bridge as Relay Bridge
    participant MCP as MCP Server

    B->>E: user message (WebSocket)
    E->>L: forward message
    L->>LLM: prompt + tool definitions
    LLM-->>L: tool_use (e.g. fs_read)

    L->>R: JSON-RPC tool call (stdio)
    R->>Bridge: CallTool + project token
    Bridge->>Bridge: auth check, filter, inject _meta
    Bridge->>MCP: execute tool
    MCP-->>Bridge: result
    Bridge-->>R: result
    R-->>L: result
    L->>LLM: tool result → next turn
    LLM-->>L: text response
    L-->>E: stream events
    E-->>B: render in UI
```

## Projects

Projects are the primary unit of organization and security. Each project defines an infrastructure boundary.

```mermaid
graph LR
    subgraph Project
        Path["Path<br/><i>/Users/me/myapp</i>"]
        MCPs["Allowed MCPs<br/><i>[fsMCP, macMCP] or [*]</i>"]
        Models["Allowed Models<br/><i>[claude-opus] or [*]</i>"]
        Templates["Chat Templates<br/><i>Researcher, Assistant, ...</i>"]
        Token["Scoped Token<br/><i>auto-generated</i>"]
        Disabled["Disabled Tools<br/><i>fs_bash</i>"]
        Context["Context<br/><i>allowed_dirs → path</i>"]
    end

    Token -->|authenticates via| Bridge["Relay Bridge"]
    Bridge -->|derives permissions from| MCPs
    Bridge -->|injects| Context
    Bridge -->|filters| Disabled
```

- **`allowed_mcp_ids: ["*"]`** — access all registered MCPs
- **`allowed_models: ["*"]`** — use any model
- **`disabled_tools`** — `fs_bash` blocked by default for filesystem MCPs
- **`context`** — `allowed_dirs` auto-set to project path for fsMCP

The project token is the security boundary. Permissions are derived at auth time from `allowed_mcp_ids` — not stored separately.

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
3. Register MCP servers and create projects
4. Use Eve or connect external tools with a project token

## Execution Modes

- **`relay`** (default) — menubar tray app. Hosts bridge socket, manages services and projects.
- **`relay mcp --token <value>`** — stdio MCP server. Connects to bridge socket. Token determines which tools are visible.
- **`relay mcp register|unregister|list`** — CLI for external MCP server management.
- **`relay mcpList --token <value>`** — lists tools available to a token.
- **`relay mcpExec --token <value> --list|--tool <name> [--args '<json>']`** — calls tools directly over the bridge.
- **`relay service register|unregister|list`** — CLI for background service management.

## Security

- **Project tokens** — each project gets a scoped token. Permissions derived from `allowed_mcp_ids` at auth time. Token + SHA-256 hash stored in `settings.json` (mode 0600).
- **Service tokens** — ephemeral, in-memory. Injected into managed services at spawn. Full bridge access for administrative operations.
- **Eve ↔ relayLLM** — separate trust boundary. `RELAY_LLM_TOKEN` + Unix socket (mode 0600). Not reusable as MCP token.
- **OAuth 2.1** — HTTP MCPs use PKCE (S256), dynamic client registration, auto-refresh.
- **MCP data is runtime-only** — tool definitions and context schemas discovered during handshake, never persisted.

## External MCP Servers

Register via CLI or Settings UI. Relay proxies their tools through the authenticated bridge.

```bash
relay mcp register --name macMCP --command ~/.local/bin/macmcp
relay mcp register --name Krisp --transport http --url https://mcp.krisp.ai/mcp
relay mcp list
relay mcp unregister --name macMCP
```

`register` is idempotent. Sends reconcile signal to the running tray app.

## Services

Manage background processes via Settings or CLI. Commands run through a login shell.

```bash
relay service register --name Eve --command node --args server.js --workdir . --url http://localhost:3000 --autostart
relay service list
relay service unregister --name Eve
```

`register` is idempotent and hot-reloads running services.

## Logs

```bash
tail -f ~/Library/Application\ Support/Relay/logs/<service-id>.log
```

## Ecosystem

Relay orchestrates 6+ connected projects:

**Services** (managed via `relay service register`):
- **[relayLLM](https://github.com/barelyworkingcode/relayLLM)** — LLM execution engine. Receives directory + model + token, streams results.
- **[Eve](https://github.com/barelyworkingcode/eve)** — Browser-based frontend. Fetches projects from relay, resolves templates, manages file browser.
- **[relayScheduler](https://github.com/barelyworkingcode/relayScheduler)** — Task scheduler. Runs LLM prompts on schedule.
- **[relayTelegram](https://github.com/barelyworkingcode/relayTelegram)** — Telegram bot bridge.

**MCP Servers** (managed via `relay mcp register`):
- **[macMCP](https://github.com/barelyworkingcode/macMCP)** — 41 macOS-native tools (Calendar, Contacts, Mail, Messages, Maps, etc.).
- **[fsMCP](https://github.com/barelyworkingcode/fsmcp)** — File system tools with per-project directory scoping via `_meta.allowed_dirs`.

## License

MIT
