# Getting relay working on one Mac

You have installed relay. This gets you from there to an agent or a script
calling a real tool through it, on this Mac, with nothing on the network.

Everything here happens on one machine. If what you want is a second machine —
a VM, a Linux box, another laptop — reaching relay's tools over the network,
do this guide first and then
[install-remote-machine.md](install-remote-machine.md).

Four steps: start relay, register an MCP, create a project, call a tool.

---

## Before you start

**You need to be at this Mac's keyboard.** Not SSH, not a remote desktop
session that is really SSH. Two of the four steps below ask for your login
password on the Mac's own screen, and relay refuses them outright from a
session that cannot show that prompt. There is no queue and no
approve-it-later list.

**You need relay built and installed.** From the repo:

```
./build.sh
```

That installs `/Applications/Relay.app` and launches it.

**You need an MCP server to point relay at.** Relay orchestrates MCP servers;
with none registered there are no tools to call. Any MCP binary works —
[fsMCP](https://github.com/barelyworkingcode/fsmcp) (filesystem) and
[macMCP](https://github.com/barelyworkingcode/macMCP) (Mail, Calendar,
Contacts) are the two this project develops against.

### `relay` is not on your PATH

The binary lives inside the app bundle. Set this up once, in your shell
profile, or every command below will be `command not found`:

```
alias relay='/Applications/Relay.app/Contents/MacOS/relay'
```

The rest of this guide writes plain `relay`.

### One thing that will surprise you, said before it happens

`relay` is one binary with two personalities. With no arguments it is the tray
app. With a subcommand it is a CLI, and that CLI splits in two:

- **Read commands work with the tray stopped.** `relay grant`, `relay audit`,
  and every `list` subcommand read files directly. No socket, no prompt.
- **Mutating commands ask the running tray**, and a subset of those also
  demand your presence — a real macOS login-password prompt, answered at this
  Mac's own screen, before anything is written.

These are the ones that prompt:

| Command | Prompts | Works over SSH |
|---|---|---|
| `relay mcp register` | **yes** | no |
| `relay service register` | **yes** | no |
| `relay credential mint` / `revoke` | **yes** | no |
| `relay enrol create` / `sign` / `update` / `revoke` / `approve` | **yes** | no |
| `relay login enrol` / `revoke` | **yes** | no |
| `relay mcp unregister`, `service unregister`, `service restart` | no | yes |
| `relay grant`, `relay audit`, every `list` | no | yes |
| `relay mcpExec` (calling a tool) | no | yes |

The rule is: *any operation that issues a credential, widens one, or chooses
what runs.* Registering an MCP is on the list because you are telling relay
what binary to execute. Calling a tool is not — that is an ordinary act inside
a grant that already exists.

Full mechanism: [`presence-gate.md`](presence-gate.md). Full command
reference: [`cli.md`](cli.md).

---

## 1. Start relay and check it is running

Launch `/Applications/Relay.app` — or `open -a Relay`. It is a menubar app
with no dock icon and no window until you open Settings from the tray.

Check it is actually up:

```
$ ls ~/Library/Application\ Support/relay/relay.sock
/Users/you/Library/Application Support/relay/relay.sock
```

That socket is what every mutating command dials. If it is missing, relay is
not running, and every command in steps 2 and 3 will refuse by name:

```
error: relay is not running; `relay mcp register` requires the service.
  relay is the sole broker of its own credentials: the secrets are sealed and
  only the tray holds the key (ADR-017 decision 2). Start Relay and retry.
  Read commands still work with relay stopped: `relay credential list`,
  `relay grant`, `relay audit`.
```

---

## 2. Register an MCP, so there are tools at all

```
relay mcp register --id fsmcp --name "fsMCP" --command ~/.local/bin/fsmcp
```

**Pass `--id` explicitly.** Without it, relay derives the id from a slug of
`--name`, and the id is what a project's grant references. Re-registering the
same MCP later under a name that slugifies differently writes a *second*
record and leaves your grants pointing at the first one. Choose an id now and
keep it.

### The prompt

This is the first presence prompt you will see. A real macOS
login-password dialog appears on this Mac's screen:

> **Relay** — "Relay is trying to register the MCP "fsMCP" (fsmcp) that runs
> /Users/you/.local/bin/fsmcp. Enter the password for the user "you" to allow
> this."

Three things about it are deliberate:

- **It names the exact act**: the display name, the id, and the command relay
  will execute — never a generic "Relay wants to make a change". If the
  command in the dialog is not the command you meant to register, cancel.
- **It is single-use and expires in 120 seconds.** The confirmation is bound
  to this operation *and* to a digest of these arguments, including the id.
  An approval given for one registration cannot be replayed for another.
- **Cancelling changes nothing.** The command exits and `settings.json` is
  exactly as it was.

Cancel it and you get:

```
error: bridge error (code -32603): presence was refused
```

Answer it and:

```
registered mcp "fsMCP" (fsmcp)
```

### Check it landed

```
$ relay mcp list
ID      NAME   TRANSPORT  ENDPOINT
fsmcp   fsMCP  stdio      /Users/you/.local/bin/fsmcp
```

`relay mcp list` reads `settings.json` directly, so this works even with relay
stopped. That is the point of checking here rather than trusting the previous
command's own report.

---

## 3. Create a project

An MCP being registered does not mean anything may call it. A **project**
binds a directory to a set of permissions and a scoped token; the token is the
security boundary.

**There is no CLI door for creating one.** Open Settings from the tray icon,
then **Projects → + New → Local project**.

```
Kind             Local project
Project name     Acme Website
Project path     /Users/you/projects/acme
MCPs             fsmcp
Tools            fs_*
Operations       Write
```

A few things worth knowing while you are in that form:

- **Tools patterns are anchored.** `fs_*` admits `fs_read` and not
  `xfs_read`. A pattern that matches everything by shape — `*`, `*_*`,
  `[a-z]*` — is refused. Empty means no tools.
- **Operations: Read** admits only tools the MCP annotates
  `readOnlyHint: true`. A tool that is unannotated, or added by a later
  version of the MCP, is refused. That is what keeps a new mutating tool out
  of an old grant.
- For a local project, an unset Operations defaults to **write** and an unset
  Tools list means **all tools** — the opposite of an access profile, which
  fails closed on both. The asymmetry is deliberate; see
  [`access-profiles.md`](access-profiles.md#what-you-are-setting).

### Check what you actually granted

```
$ relay grant --project "Acme Website"
PROJECT  Acme Website  (id: 3f0c…, path: /Users/you/projects/acme)
  fsmcp          access=write  outbound=allowed  tools=fs_*
                 scope: (none set)

This is the grant as stored. Whether each MCP still declares these scope
fields is a live question — a value relay cannot place in an MCP's current
schema is refused at call time and shown by `relay audit --authority`.
```

(Shape reconstructed from `printGrantViews`; `--project` takes a name or an
id. `outbound=allowed` is the local-project default — an access profile
defaults the other way.)

`relay grant` prints the grant **as authored**, never redacted, and it never
prints the project's token.

---

## 4. Call a tool

Two ways in. Pick one.

### With the project's token

Settings → Projects → your project → **Bearer Token** → Show, Copy.

```
export RELAY_PROJECT_TOKEN=<the token>
relay mcpExec --list
```

```
TOOL      DESCRIPTION
fs_read   Read a file within the granted directory
fs_write  Write a file within the granted directory
fs_grep   Search file contents within the granted directory

6 tools available
```

(Tool names and counts are fsMCP's; the table format is `runMcpExec`'s. A
description longer than 80 characters is truncated with an ellipsis.)

Then call one:

```
relay mcpExec --tool fs_read --args '{"path":"README.md"}'
```

For arguments containing quotes, apostrophes or parentheses, use
`--args-file FILE` or `--args-file -` instead — relay forwards argument bytes
verbatim and shell quoting is the only thing likely to corrupt them.

`relay mcp call` is the same command under another spelling.

### Without a token, from inside the project directory

Settings → Projects → your project → **Directory Auth** → on.

Then any process running as you, with a working directory inside the project
path, gets exactly that project's tools with no token at all:

```
cd /Users/you/projects/acme
relay mcpExec --list
```

Convenient, and the trade is real: *any* process running as you gets them by
being in that directory, including agents you started for something else.
Leave it off unless you want that.

---

## What "it worked" looks like

The tool returning output is the weak evidence. The strong evidence is relay's
own record, which is written at the one chokepoint every transport funnels
through:

```
$ relay audit --tail 4
TIME      OUTCOME  PROJECT       MCP    TOOL     MS  CALLER     DETAIL
09:12:03  pending  Acme Website  fsmcp  fs_read  0   zsh→relay  {"path":"README.md"}
09:12:03  ok       Acme Website  fsmcp  fs_read  14  zsh→relay  {"path":"README.md"}
```

For a local caller the CALLER column names the calling process, taken from the
kernel by way of the bridge socket's peer credentials — never from anything
the caller sent. A remote client shows its enrolment id there instead.

Four outcomes are worth learning apart, because they are enforced in different
places:

| Outcome | Means |
|---|---|
| `ok` | allowed, and ran |
| `denied` | the grant never included the tool; relay refused before the MCP was reached |
| `tool_error` | the tool ran and reported a failure of its own |
| `throttled` | the grant was fine; the *pattern of use* was not |

Full field reference: [`audit-log.md`](audit-log.md).

---

## When it does not work

Keyed on the literal text you will see.

### `relay: command not found`

The binary is inside the app bundle and not on your PATH. See
[above](#relay-is-not-on-your-path).

### `error: relay is not running; ... requires the service.`

A mutating command with the tray stopped. Start Relay and retry. Read
commands — `relay grant`, `relay audit`, every `list` — are unaffected.

### `error: bridge error (code -32603): presence was refused`

The password prompt appeared and was cancelled or timed out. Nothing was
written. Retry and answer it.

### `refused: this needs your confirmation on the Mac's screen, ...`

```
refused: this needs your confirmation on the Mac's screen, and the session this
  command is running in cannot show a prompt (for example, you are over SSH).
  There is no queue and no pending-approval list.
  Run it from a terminal in the logged-in desktop session, or from the Relay
  Settings window.
  Read commands are unaffected: relay audit, relay grant, and every `list`.
```

Relay decided this from a kernel-attested property of your session, not from
an environment variable you can unset. Run the command from a terminal in the
logged-in desktop session, or do it from the Settings window instead.

### `error: bridge error (code -32001): no token supplied and working directory "/tmp" is not inside a project with directory auth enabled`

```
No token was supplied. Either:
  export RELAY_PROJECT_TOKEN=<project-token>   # Settings UI → Projects → Bearer Token
  or enable Settings UI → Projects → Directory Auth for the project containing this directory
```

Exactly what it says. Note that a *present but wrong* token never falls back
to directory auth — it fails as `invalid token`.

### `error: bridge error (code -32001): invalid token`

The token does not match any project. It may have been rotated: rotating a
project token invalidates the previous one immediately.

### `no tools available for this token`

The token authenticated, and the grant reaches nothing. Check
`relay grant --project <id>`. The usual causes are an empty **Tools** list, or
an MCP granted whose tools are all refused by a read-only **Operations**
setting.

### `access denied: no tool named 'fs_write' is available to this grant (granted: fsmcp)`

The grant does not include that tool. This is `denied`, not an error from the
MCP — relay refused it before the MCP was reached. Widen the project's tool
pattern, or its Operations mode, in Settings.

### `error: --name is required`

A required flag was omitted. Every subcommand's `-h` lists exactly what it
needs:

```
relay mcp register -h
```

### An MCP registered on the CLI does not appear in an open Settings window

It works immediately; only the display is stale. Reopen Settings.

### Registering an MCP a second time created a duplicate

You omitted `--id` and the name slugified to something different from the
existing record's id. Grants reference the id, so they still point at the old
record. Unregister the duplicate and re-register with the correct `--id`. See
[`cli.md`](cli.md), *"`--id` — read this before you register anything a second
time"*.

---

## Where to go next

- **A second machine that needs these tools** —
  [install-remote-machine.md](install-remote-machine.md).
- **Confining a grant properly** —
  [`access-profiles.md`](access-profiles.md): the five independent allowlists,
  resource scope, and how to check a confinement actually holds.
- **Every command, every flag, every output** — [`cli.md`](cli.md).
- **Why the prompts exist and what they do not protect against** —
  [`presence-gate.md`](presence-gate.md).
- **The credential inventory** — [`tokens.md`](tokens.md).
- **Reading the audit log** — [`audit-log.md`](audit-log.md).
