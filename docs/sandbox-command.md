# `relay sandbox`: a template in your own terminal

`relay sandbox <template> [--project <name-or-id>]` runs from a terminal inside
a project folder. It asks the running relay to launch a terminal session from
the named terminal template for the project that holds the current directory,
then wires your terminal to that session's PTY. Keys, resize and Ctrl-C behave
as normal, and the command's exit status is the tool's.

It is a subcommand of the one `relay` binary, so there is nothing to install.
Operator-facing reference: [`docs/cli.md`](cli.md#relay-sandbox).

## Why it exists

A template with `sandbox: true` runs under Seatbelt with only the folders the
template and project grant ([`docs/session-host.md`](session-host.md#what-a-sandboxed-session-can-reach)).
Before this command the only way to start one was from a frontend such as Eve.
`relay sandbox` starts the same launch from a shell. A template with
`sandbox: false` is allowed too, deliberately: the command is then the
supported way to start a tool from a terminal wired to relay's MCP and model
proxies. A template that omits `sandbox` counts as `sandbox: true`; only an
explicit `false` opts out. Nothing about how sandboxing works changes.

## Shape

```
relay sandbox (CLI)                       relay app                       relay-sessions
  preflight, raw mode
  bridge socket ── SandboxAttach ───────▶ handleSandboxAttach
                                           membership refusal
                                           resolve project (server side)
                                           sessionRouteDeps.launch ───────▶ POST /launch
                                           DialWS, join_terminal ─────────▶ /ws (viewer)
  ◀──────────── Attached ack ──────────── SetTakeover
  input/resize frames ─────────────────▶ pump ── terminal_input/resize ──▶ PTY
  ◀──────── output/exit frames ────────── pump ◀── terminal_output/exit ──
```

Relay does not own the PTY; `relay-sessions` does. Relay is one more viewer of
the session, over the same internal `/ws` it dials on Eve's behalf
(`sessionHostClient.DialWS`: the peer is verified by kernel audit token exactly
as `/launch` and `/terminate` verify it, and the bearer is the one the host
registered).

### One launch core

`sessionRouteDeps.launch` (`cmd/relay/session_routes.go`) is `AuthorizeLaunch`,
the host round trip, the ledger commit and the `session_launch` audit record.
The HTTP create routes and this command both go through it; a door only phrases
the outcome. New behaviour belongs in that core, not in a door.

`LaunchCaller` gained an `Operator` variant next to the identity and credential
callers; exactly one field is still set. It grants execute unconditionally
and is constructed in one place, `appRouter.SandboxAttach`, from a connection
that already passed the membership refusal. The audit actor is
`kind: "operator"`, `auth: "none"`, with the peer's `pid`, `proc` and `parent`,
the same shape `credential_issued` uses for a CLI door
([`docs/audit-log.md`](audit-log.md)).

## Decisions

**Ungated.** No presence prompt. The command launches something the operator's
own project configuration already permits (`AuthorizeLaunch` still checks the
project, the directory and `allowed_templates`), and it issues no credential
and widens no grant. What guards it is the refusal below.

**Refused from inside a relay session.** Three checks, of different strength.

- The client refuses when `RELAY_SESSION_ID` is set. This is a courtesy for a
  readable early error and is not a security boundary: the variable is
  unset-able.
- Relay refuses server-side using `bridge.ConnMembershipFromContext(ctx).Session()`,
  the kernel-attested answer to "is the caller a descendant of a live relay
  session", walking process ancestry from the peer up. This is because the
  sandbox profile allows `relay.sock`, so a sandboxed agent could otherwise ask
  relay to launch an *unsandboxed* template for it.
- Relay also refuses server-side whenever the peer itself, right now, is
  confined by any Seatbelt sandbox — independent of the ancestry walk above.
  This is `bridge.PeerConfined`, resolved via `sandbox_check_by_audit_token`
  against the same kernel-attested peer audit token the ancestry check uses.

The second check exists because the first is not, by itself, a full boundary.
Ancestry membership is recognised only for a session bound with a
`project_session` launch identity, and even then only while the walk can
still reach the session's root process: a process inside a sandboxed session
that double-forks is reparented to launchd, and the walk stops at `ppid <= 1`
before it ever reaches a live root, so the reparented process reads as an
ordinary, unconfined operator. The confinement check catches exactly that
case, because it asks a different question with a different answer: a plain
fork inherits its parent's Seatbelt profile, so nothing short of leaving the
sandbox — which the sandbox itself is what prevents — sheds it. A caller is
refused when *either* check says so; an ordinary operator's own terminal is
unconfined and outside every session, so neither check ever fires for it.

Both checks read only kernel-attested state; neither trusts anything the peer
asserts about itself. The design accepts what's still not covered: a
confined process running as the *same* user, outside the sandbox templates
this file describes, that separately gains access to the 0600 bridge socket
is untested territory, as is behaviour under SIP-disabled configurations —
`sandbox_check_by_audit_token` is a private, unentitled API with no header on
this system, kept working by symbol name rather than a link-time contract. If
a future OS removes or renames it, relay fails closed: every `relay sandbox`
attach is refused, not silently downgraded to ancestry-only, and the failure
is logged once rather than per request.

**Project resolution is server-side.** The CLI cannot read `settings.json`
(sealed configuration). It sends the real path of its working directory in the
request body (`SandboxAttachRequest.Cwd`; `BridgeRequest.Cwd` stays unread on
purpose) and relay resolves it with `project.DirWithin` over the local
projects. Remote and SSH-hosted projects are skipped: their path is not on this
disk. `DirWithin` answers true for an empty directory, so the resolver never
passes one.

**Ambiguity is refused.** If more than one project holds the directory the
command refuses, lists the matching projects and requires
`--project <name-or-id>`. The flag can only choose among the projects that hold
the directory; it never reaches another. No matching project is a refusal too,
and nothing is launched.

**Eve can see these sessions, and that is accepted.** A sandbox session is an
ordinary terminal of `relay-sessions`. Any frontend client can list it, join
it, type into it, resize it or close it, because the host has no per-terminal
ownership ([`docs/session-host.md`](session-host.md)). Eve closing a sandbox
session ends the command with exit 1 and the message "the session ended
(connection dropped)"; relay still runs the launch's accounting cleanup.

## Attach and teardown

The bridge is request/response: `FrameConn.Serve` reads one line and writes one
`BridgeResponse`. A handler that wants the connection as a stream calls
`bridge.SetTakeover(ctx, fn)`. `Serve` then writes the handler's response and
runs `fn` on the same goroutine instead of reading the next request, so the
connection is never read by two loops, and returns when `fn` does. Requests
that do not set a takeover behave exactly as before. `FrameConn.WriteValue` and
`ReadValue` write and read further newline-JSON frames through the same write
lock and the same scanner (which may already hold buffered bytes). Only
`bridgeHandlers` has the request type; the remote and enrolment listeners have
no entry for it.

After the ack (`Attached`, carrying `SandboxAttachResult`) each side sends
`StreamFrame`s: `input` (bytes) and `resize` (cols, rows) from the client;
`output` (bytes) and `exit` (code) from relay. Bytes are base64 on the wire.

Relay joins the terminal before it acks, and the handshake matters:

- The host registers the viewer *before* it snapshots scrollback, so a chunk can
  appear both in the scrollback and as a live `terminal_output`. Relay buffers
  live output until `terminal_joined`, writes the scrollback first, then the
  buffered and live output.
- The host broadcasts frames for every terminal to every viewer, so relay
  filters on the session id.
- The host cuts a viewer that stops reading, so a reader goroutine drains the
  WebSocket into a bounded queue and a separate writer feeds the client. If the
  client stalls until the queue is full the session is ended rather than losing
  bytes.
- gorilla allows one writer, so relay's writes to the WebSocket are serialised.

**Disconnect ends the session.** A closed terminal, SIGHUP, SIGTERM, a killed
client, EOF on the bridge connection, a failed write to it, or a failure to
join all end with `endSandboxSession`: `POST /terminate` (the reason names why)
and the launch's identity and model-key bookkeeping. This covers a launch whose
attach never completed, because the takeover always runs once a launch has
committed, and a dead peer fails its first read. A session nobody is joined to
has no idle timer, and one that lost its last viewer waits out the template's
idle timeout (24 hours by default), so relay must terminate it itself; it does.
When the tool exits, relay sends the `exit` frame and does not terminate.

**Sleep and wake do not end it.** Nothing on the attach path has a read, write,
idle or pong deadline. The bridge connection is a Unix socket, which survives
sleep, and the WebSocket to the host is a Unix socket too. Do not add a
wall-clock deadline here, and do not route this through `bridge.Client.send`,
which has a ten-minute inactivity deadline. The only bounded waits are the
upgrade handshake (`sessionHostRequestTimeout`) and the one-shot `/terminate`
request; neither is an idle timer on an attached session.

**Exit status.** The status is the shim's: the tool's own code, or 128 plus the
signal number if the tool was signalled. The host reports a shim that was itself
signalled as -1, which has no shell form, so the client exits 1 for anything
outside 0-255. If the stream ends with no `exit` frame (Eve closed the session,
or relay went away) the client says so and exits 1.

## The client

Preflight, in order, each a plain message and a non-zero exit: inside a relay
session; stdin and stdout are terminals; relay is running; a template was
named. The client dials the bridge socket itself, with no deadlines, sends the
request, and reads the ack or the refusal. On an ack it puts the terminal in raw
mode, sends its size and every SIGWINCH, and copies stdin and output. It
restores the terminal on every exit path, including the signal handler. Ctrl-C
is an ordinary byte in raw mode.

Terminal handling uses `golang.org/x/sys/unix` (already a dependency), not
`golang.org/x/term`.

## Files

| File | Role |
|---|---|
| `cmd/relay/sandbox_cmd.go` | the CLI: preflight, raw mode, stream |
| `cmd/relay/sandbox_attach.go` | project resolution, `appRouter.SandboxAttach`, the viewer and pump |
| `internal/bridge/sandbox.go` | request/result/frame types, refusal reasons, `handleSandboxAttach` |
| `internal/bridge/peer_confined_darwin.go` | `PeerConfined`: `sandbox_check_by_audit_token` via `dlopen`/`dlsym` |
| `internal/bridge/frameconn.go` | `SetTakeover`, `WriteValue`, `ReadValue` |
| `cmd/relay/session_routes.go` | `launch`, the shared launch core |
| `cmd/relay/sessionhost_client.go` | `DialWS` |
