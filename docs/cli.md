# The `relay` command line

`relay` is one binary with three personalities. Run with no arguments, it is the
tray app — the thing that owns `settings.json`, holds the sealing key, and
answers presence prompts. Run as `relay serve`, it is the same server with no
tray and no window. Run with any other subcommand, it is a CLI that asks the
running server — to show its configuration or to change it — or reads the
audit log. Wherever this document says "the tray", a `relay serve` instance
behaves the same.

That split is the one fact this whole document keeps coming back to, so it
comes first.

## The mental model: read vs. write

**The running tray is the only reader of configuration.** `relay grant` and
every `list` subcommand (`credential list`, `enrol list`, `login list`,
`eve list`, `mcp list`, `service list`) ask the tray over the same bridge
socket the mutating commands use (`admin_op`), and the tray answers from a
fresh snapshot of its own settings. No CLI process opens `settings.json` for
these commands, so what they print is what the tray holds now, never a stale
file. Reads never prompt for presence, and the answer is a purpose-built view
per command: token hashes, sealed values, environment values and public-key
coordinates have no field to travel in. With the tray stopped they refuse by
name, exactly like the mutating commands. The exceptions are the commands
that read their own files: `relay audit` (the audit log) and
`relay enrol ca-fingerprint` (the public `ca.crt`) work with the tray stopped.

**Every mutating command asks the running tray**, the same way, and refuses
by name if the tray is not running. Nothing in this program writes
`settings.json` from a CLI process any more: the tray is the only process
holding the keychain key that unseals its sealed fields (project tokens, the
admin secret, OAuth bearers, the CA key — see
[`docs/sealed-config.md`](sealed-config.md)), so it has to be the only
process writing them. Stop relay and try one (a read refuses the same way):

```
$ relay credential mint --name test --class read
error: relay is not running; `relay credential mint` requires the service.
  relay is the sole broker of its own credentials: the secrets are sealed and
  only the tray holds the key (ADR-017 decision 2), and it is the only reader of
  the configuration for `list`, `grant` and every other command that shows it.
  Start Relay and retry. `relay audit` and `relay enrol ca-fingerprint` read
  their own files and still work with relay stopped.
```

**A subset of the mutating commands also demand presence** — a real
login-password prompt, answered at the Mac's own screen, before the tray
commits anything. The rule (from [`docs/presence-gate.md`](presence-gate.md)):
*any operation that issues a credential, widens one, or chooses what runs.*
Concretely, every command in the table below marked **prompts: yes**.
`service restart` is not gated — it restarts a process from a configuration
that was already approved when it was registered, and changes no settings.
`mcp unregister` and `service unregister` are not gated either (ADR-018
step 3): removal only narrows what a caller already reaches, and
re-registering under the same id still has to pass the register command's
gate. All three are still brokered — they still need the service running —
and `mcp unregister`/`service unregister` still leave an audit record; see
[`docs/presence-gate.md`](presence-gate.md#what-is-not-gated-and-why-removal-is-not-escalation).

Two consequences follow immediately, and both are covered in full below:

- The prompt names the exact act and its arguments, expires in 120 seconds,
  and can be spent exactly once.
- Over SSH, a gated command refuses instantly rather than queuing, because
  relay can tell — from the kernel, not from anything the caller sends — that
  the session it is running in cannot show a prompt on the console.

**Verbs for the Settings window and the tray are operator-only.** Every verb
added to reach what only the Settings window or the tray did before
(`relay project create`, `relay status`, `relay session start` and the rest of
this document's later sections) runs only from your own terminal. The server
refuses it from inside a relay session and from any sandboxed process, with
`this command cannot be run from inside a relay session or a sandbox`, and
writes a denied `control_decision` row. The same peer check `relay sandbox`
makes decides it. The verbs that predate this rule (`relay credential`,
`relay enrol`, `relay mcp register` and the others above) keep the caller rule
they had. `relay doors` lists every door, its credential class and its gates.

### Quick reference

| Command | Needs service | Prompts | Works over SSH |
|---|---|---|---|
| `relay grant` | yes | no | yes |
| `relay audit` | no | no | yes |
| `relay logs` | no | no | yes |
| `relay credential list` | yes | no | yes |
| `relay credential mint` | yes | **yes** | no |
| `relay credential revoke` | yes | **yes** | no |
| `relay enrol list` | yes | no | yes |
| `relay enrol create` | yes | **yes** | no |
| `relay enrol sign` | yes | **yes** | no |
| `relay enrol update` | yes | **yes** | no |
| `relay enrol revoke` | yes | **yes** | no |
| `relay enrol requests` | yes | no | yes |
| `relay enrol approve` | yes | **yes** | no |
| `relay enrol refuse` | yes | no | yes |
| `relay enrol ca-fingerprint` | no | no | yes |
| `relay login list` | yes | no | yes |
| `relay login enrol` | yes | **yes** | no |
| `relay login revoke` | yes | **yes** | no |
| `relay eve enrol` | yes | **yes** | no |
| `relay eve list` | yes | no | yes |
| `relay eve revoke` | yes | **yes** | no |
| `relay project update` | yes | no | yes |
| `relay mcp list` | yes | no | yes |
| `relay mcp register` | yes | **yes** | no |
| `relay mcp unregister` | yes | no | yes |
| `relay service list` | yes | no | yes |
| `relay service register` | yes | **yes** | no |
| `relay service unregister` | yes | no | yes |
| `relay service restart` | yes | no | yes |
| `relay status` | yes | no | yes |
| `relay doors` | yes | no | yes |
| `relay debug clock` (test build only) | yes | no | yes |
| `relay remote show` | yes | no | yes |
| `relay remote set` | yes | **yes** | no |
| `relay host probe` | yes | no | yes |
| `relay host disconnect` | yes | no | yes |
| `relay sealed reset` | yes | **yes** | no |
| `relay login sessions` | yes | no | yes |
| `relay login sign-out` | yes | no | yes |
| `relay project create` | yes | **yes** | no |
| `relay project edit` | yes | **yes** when it widens a grant | no when it widens, yes otherwise |
| `relay project remove` | yes | no | yes |
| `relay project rotate-token` | yes | **yes** | no |
| `relay project token` | yes | **yes** | no |
| `relay project regen-skill` | yes | no | yes |
| `relay mcp authenticate` | yes | **yes** | no |
| `relay mcp reset-permissions` | yes | no | yes |
| `relay mcp scope-fields` | yes | no | yes |
| `relay service start / stop` | yes | no | yes |
| `relay service action` | yes | no | yes |
| `relay service config` | yes | no | yes |
| `relay model list` | yes | no | yes |
| `relay session start / list / message / stop / resume / mode` | yes | no | yes |
| `relay terminal start / list / log / stop` | yes | no | yes |
| `relay terminal persistent-list / persistent-kill` | yes | no | yes |
| `relay files watch` | yes | no | yes |
| `relay sandbox` | yes | no | no (needs an interactive terminal) |
| `relay drop-in` | yes | no | no (needs an interactive terminal) |
| `relay mcpExec` / `relay mcp call` | yes (dials the bridge) | no | yes |
| `relay mcp --token TOKEN` (stdio server) | yes | no | yes |

"Works over SSH" here means "does not refuse *because it is gated*." A
mutating command still needs the service reachable either way; only the
presence-gated ones add the console-session check.

### Machine-readable output

`relay audit`, `relay grant`, `relay enrol requests`, `relay logs` and every
verb added for the Settings window and the tray accept `--json` and emit the
same data as structured JSON instead of a text line or table
(`relay logs --json` prints each log line exactly as stored). A `--json` run
prints one line on stdout. On failure stdout is empty, stderr carries
`error: ...` and the exit code is 1. A verb that takes a request body takes
`--file F`, the matching HTTP route's JSON body, or `-` for stdin.
`relay mcpExec --list` (and its `relay mcp call --list` spelling) spells its
machine-readable form `--schema` instead: plain `--list` prints a table,
and `--schema` switches it to JSON that also includes each tool's input
schema — what a SKILL.md generator consumes. The older
`list` subcommands print tables only.

## `relay serve`

```
relay [--config-dir DIR] serve
```

Runs relay's full server — every listener, service and loop the tray runs — for
one config dir, with no tray and no window. It takes no other argument (any
other prints usage and exits `2`). It creates DIR if it is missing, takes the
per-directory lock, and when every listener is up prints exactly one line on
stdout, the path of `DIR/ready.json`, and nothing else. `SIGTERM` or `SIGINT`
cleans up and exits `0`. A DIR other than the default one keeps its sealing key
in its own login-keychain item (`docs/sealed-config.md`), so a reset there never
touches the tray's key. A start failure exits `1` naming DIR; a second server
on the same DIR fails with `another relay server already owns this
configuration directory (DIR)`.

DIR must be short enough for its sockets to bind: the longest socket path may
be 103 bytes, else the start fails with `config dir DIR is too long: socket
PATH is N bytes and macOS allows 103; choose a shorter directory`. Use a short
path such as one under `/tmp`.

### `ready.json`

Written (mode `0600`, atomically) after every listener is bound and serving,
rewritten when a listener reconcile moves one, and removed first thing on a
clean shutdown. The tray writes it too. It holds no credential.

```json
{"schema":1,"pid":41234,"version":"dev","config_dir":"/tmp/rr.Ab12/a",
 "sockets":{"bridge":"/tmp/rr.Ab12/a/relay.sock","frontend":"/tmp/rr.Ab12/a/relay-frontend-41234.sock","model":"/tmp/rr.Ab12/a/model.sock"},
 "listeners":{"api":"127.0.0.1:53001","model":"127.0.0.1:53002","remote":"127.0.0.1:53003","enrolment":"127.0.0.1:53004"}}
```

`listeners` is always present; a listener that is not bound has no key, and
the addresses are the ones actually bound, so a port-0 request shows its real
port. Do not use `ready.json` to decide whether a server is live — a crash
leaves it behind until the next start removes it. Dial `DIR/relay.sock`.

### Listener addresses

Every TCP listener takes its address from `settings.json` and accepts port `0`:

| Listener | Key | Absent means |
|---|---|---|
| Control-plane API | `api.listen` | `RELAY_API_LISTEN`; neither set: no listener |
| Model endpoint | `model_endpoint.listen` | no listener |
| Remote mTLS | `remote.listen` | `127.0.0.1:9910` when remote is enabled |
| Enrolment requests | `remote.enrolment_listen` | `127.0.0.1:9911` when enabled |

`api.listen` beats `RELAY_API_LISTEN`; both are read at start and both must be
loopback. A refusal names the key or variable that supplied the address. The
sandbox's loopback port denial list is `session_sandbox.denied_loopback_ports`
in `settings.json` (default `[3000, 8181]`); an explicit list replaces the
default, an entry outside 1-65535 refuses the launch naming the key and value,
and the instance's own bound API port is always denied.

## The global `--config-dir` flag and `RELAY_CONFIG_DIR`

Every subcommand takes the config dir from the first rule that applies:

1. `--config-dir DIR` or `--config-dir=DIR` anywhere in the arguments before a
   bare `--`. A relative DIR becomes absolute. Given twice: `--config-dir given
   more than once`. A missing, empty or dash-led value: `--config-dir needs a
   directory path`. To pass a literal `--config-dir` through to a registered
   command's argv, write it `--args=--config-dir`.
2. `RELAY_CONFIG_DIR`, when non-empty. It must be an absolute path, else
   `RELAY_CONFIG_DIR must be an absolute path, got "x"`.
3. The default config dir.

The flag is stripped out ahead of every subcommand's own flag parsing, since
each owns its own `flag.FlagSet`. Both errors exit `1`. A server started under a
non-default dir exports `RELAY_CONFIG_DIR` to the services it spawns, so a
`relay` run inside one reaches the same instance.

```
relay grant --config-dir /path/to/alt-config
```

**A client never creates DIR.** A verb that needs the service dials
`DIR/relay.sock` (2 s limit) before doing anything else. With no server there
it exits `1` and names DIR:

```
error: relay is not running at /tmp/acme; `relay service list` requires the service.
```

`relay sandbox`, `relay drop-in`, `relay mcpExec` and `relay mcp call` do the
same and never fall back to the default dir. Only the tray and `relay serve`
create DIR.

### Which socket the `relay mcp` stdio server dials

`relay mcp --token TOKEN` (the stdio server, not its `register`, `unregister`,
`list` or `call` subcommands) picks its bridge socket by the first rule that
applies:

1. `--config-dir DIR`: `DIR/relay.sock`.
2. `RELAY_BRIDGE_SOCKET`, when set and non-empty. It must be an absolute path;
   a relative value is refused, never resolved against the working directory.
3. `RELAY_CONFIG_DIR`: `$RELAY_CONFIG_DIR/relay.sock`.
4. The default, `<config dir>/relay.sock`.

The environment variable is what makes a session's tool child reach the relay
that launched it. relay-sessions is a separate binary with no config dir of its
own: it learns everything from the bridge socket path it was started with, and
that path already reaches every `relay mcp` it spawns as `RELAY_BRIDGE_SOCKET`.
Without rule 2, a relay run under `--config-dir` would hand its sessions a tool
child that dials the default relay instead. The path is not a credential:
authentication is decided on relay's side, and nothing new enters any child's
environment. An explicit flag outranks the variable because the operator typed
it.

Once the socket is chosen, and before reading stdin, the server dials it once
and closes the connection. If that fails it exits `1` with nothing on stdout
and one line on stderr naming the path and the rule that chose it:

```
error: relay mcp: bridge socket /tmp/acme/relay.sock (from RELAY_BRIDGE_SOCKET) is unreachable: dial unix /tmp/acme/relay.sock: connect: no such file or directory
error: relay mcp: RELAY_BRIDGE_SOCKET must be an absolute path, got "relay.sock"
```

The source is `--config-dir`, `RELAY_BRIDGE_SOCKET`, `RELAY_CONFIG_DIR` or `default`. A set
variable never falls back to the default socket: a tool child that silently
reached a different relay would act under that relay's grants. The same holds
on the default path, so `relay mcp` started while relay is down exits at
startup instead of failing each call. A session reports the early exit as
`relay_server_failed` (Claude) or `mcp_start_failed` (chat).

The admin subcommands (`relay mcp register`, `relay mcp call`, `relay
mcpExec` and the rest) do not read `RELAY_BRIDGE_SOCKET`; they dial the socket
the config dir rules above name. The stdio server honours the variable
because it is the one command relay's own children spawn with it set to the
socket of the relay that launched them. Whether each admin command should
honour it is a separate decision.

## The global `--trace` flag

```
relay --trace rlcheck-ok-0001 service restart --id acme
```

`--trace ID` or `--trace=ID` anywhere in the arguments before a bare `--` names
the trace of this call. Every bridge request the command sends carries it, so
the event lines and request lines it causes in relay's log carry it as
`trace_id`, and `relay logs --trace ID` finds them. ID is 8 to 64 characters
from `A-Z a-z 0-9 _ -` and does not start with `-`; otherwise the command exits
`1` with `--trace needs a trace ID of 8 to 64 characters from A-Z a-z 0-9 _ -`.
Given twice: `--trace given more than once`. Without the flag the server makes
up a trace for the call. Like `--config-dir`, the flag is stripped before the
subcommand parses; to pass a literal `--trace` through to a registered
command's argv, write it `--args=--trace`. For `relay logs` the same value is
the trace filter. A trace joins log lines and proves nothing about who acted.

## Privileged commands prompt — and here is what that looks like

For any command marked **prompts: yes** above, the tray raises a real
`LocalAuthentication` login-password prompt before it touches
`settings.json`. This machine has no Touch ID and no Secure Enclave, so
"presence" here is a typed password, not a fingerprint or a click. A verified
example, captured on this machine's console for the request

```
relay mcp register --id fsmcp3 --name "fsMCP v3 (testfolder)" --command /Users/you/.local/bin/fsmcp
```

— note this asks to re-point the already-registered id `fsmcp3` at a
*different* binary, dropping its `--root` argument entirely:

> **Relay** — "Relay is trying to register the MCP "fsMCP v3 (testfolder)"
> (fsmcp3) that runs /Users/you/.local/bin/fsmcp. Enter the password for
> the user "Managed via Tart" to allow this."

**The prompt describes the change being requested, not the record as it
stands.** `fsmcp3`'s actual, stored command — unchanged, per `relay mcp
list` — is `/Users/you/.local/bin/fsmcp3 --root
/Users/you/src/testfolder`. The command the dialog
names, `/Users/you/.local/bin/fsmcp` with no `--root` at all, is what the
record would *become* if this were approved. This is the whole reason the
prompt spells out the command rather than just the display name: an
operator who did not intend to re-point `fsmcp3` at a different binary sees
that mismatch in the dialog itself and can catch it before it lands, not
after.

This example was in fact cancelled — password prompt dismissed, no
password entered. The CLI exited refused, and `relay mcp list` immediately
afterward was unchanged: all four MCPs, same ids, same commands, `fsmcp3`
still running its original binary with its original `--root`. Nothing was
written.

The correct way to re-register the same record — reasserting its real
command, which is a legitimate and common act (confirming a grant still
points where you think it does, after an upgrade or a path change) — names
the same id with the same command it already runs:

```
relay mcp register --id fsmcp3 --name "fsMCP v3 (testfolder)" \
  --command /Users/you/.local/bin/fsmcp3 --args --root --args /Users/you/src/testfolder
```

That prompt would read "...that runs /Users/you/.local/bin/fsmcp3" — the
reason string names the command only, never its arguments — recognizably the
record as it already is — and approving it changes nothing an operator did
not already expect.

Three things about the prompt's wording are deliberate, not incidental:

- **It names the exact act and its arguments** — the display name, the id,
  and the command it will run (or, for an HTTP MCP, the URL it will connect
  to) — never a generic "Relay wants to make a change." The reason string is
  built from the same request the digest is computed from, so the human
  answering it is approving the literal act being requested, not a
  paraphrase of it and not the record's current state.
- **The confirmation is single-use and expires in 120 seconds.** Behind the
  scenes, a successful prompt mints a nonce bound to both the operation name
  and a cryptographic digest of its normalised arguments; redeeming it burns
  it immediately. A confirmation given for `mcp register --name X` cannot be
  replayed for a second registration, and cannot be redeemed for a different
  operation entirely — the digest binds the *id*, too, which is why
  re-registering under a different id (see below) is never quietly folded
  into an earlier approval.
- **Cancelling changes nothing**, exactly as demonstrated above: the CLI
  command that asked for it exits with `presence was refused`-shaped output
  and `settings.json` is untouched.

Every gated command has its own version of this reason string:

| Command | Prompt names |
|---|---|
| `credential mint` | `mint a control-plane credential named "NAME" with classes read and configure` |
| `credential revoke` | `revoke the control-plane credential "ID"` |
| `enrol create` | `create an enrolment for client "ID" with access to PROFILE` |
| `enrol sign` | `sign a certificate for client "ID" with access to PROFILE` |
| `enrol update` | `update the enrolment "ID"'s grant` |
| `enrol revoke` | `revoke the enrolment "ID"` |
| `login enrol` | `mint a login bootstrap code` |
| `login revoke` | `revoke the passkey "ID"` |
| `eve enrol` | `open a five-minute window for one new browser to register an Eve passkey` |
| `eve revoke` | `revoke the Eve passkey ID` |
| `mcp register` | `register the MCP "NAME" (id) that runs COMMAND` (or `at URL` for HTTP) |
| `service register` | `register the service "NAME" (id) that runs COMMAND` (or, when the id already exists and this is an update, `update the service "ID" to run COMMAND`) |

## Privileged commands over SSH refuse — they do not queue

Relay decides whether a prompt could be *seen* from a kernel-attested
property of the caller's own session (`getsockopt(LOCAL_PEERTOKEN)` →
`auditon(A_GETSINFO_ADDR)`'s graphic-access bit) — never from an environment
variable the caller could unset, and never from anything the caller can set
itself. An SSH session has no console access, so every gated command run
from one refuses immediately:

```
$ relay credential mint --name test --class read
error: refused: this needs your confirmation on the Mac's screen, and the session this
  command is running in cannot show a prompt (for example, you are over SSH).
  There is no queue and no pending-approval list.
  Run it from a terminal in the logged-in desktop session, or from the Relay
  Settings window.
  Read commands are unaffected: relay audit, relay grant, and every `list`.
```

There is no pending-approval list to check later and no way to pre-authorize
a run from an unattended session — a headless install has no working path to
any gated command at all, by design (see
[`docs/tokens.md`](tokens.md#the-login-bootstrap-code-is-not-a-credential)).
Reads never touch the gate: `relay audit`, `relay grant` and every `list`
subcommand never prompt (all but `audit` still need the tray running).

---

# Command reference

## `relay grant`

Prints a project's or access profile's grant **as authored** — every MCP it
reaches, its access mode, its outbound (external-network) permission, its
tool pattern, and the real resource-scope values, never redacted. The running
tray builds the answer from a fresh snapshot (`grant.view`), so it needs the
tray running, and it is deliberately blind to `disclose`: that field governs what a *client* sees,
never what this command shows an operator.

```
relay grant [--project ID-OR-NAME] [--json]
```

| Flag | Meaning |
|---|---|
| `--project` | Show one record by id or name. Default: every project and access profile. |
| `--json` | Emit the same data as JSON instead of a table. |

Needs service: no. Prompts: no. Works over SSH: yes.

Example — one access profile on this machine:

```
$ relay grant --project 477d9a17-da03-45eb-a433-764f93fe96fc
ACCESS PROFILE  Hermes Mail  (id: 477d9a17-da03-45eb-a433-764f93fe96fc)
  macmcp         access=read   outbound=blocked  tools=mail_*
                 scope: mail_accounts = [
            "Alice",
            "Bob"
          ]
                 scope: mail_mailboxes = [
            "Archive",
            "INBOX"
          ]
  enrolments:
    hermes               cli-admin: off

This is the grant as stored. Whether each MCP still declares these scope
fields is a live question — a value relay cannot place in an MCP's current
schema is refused at call time and shown by `relay audit --authority`.
```

A scope value that reaches an entire filesystem root (an `allowed_dirs` of
`"/"`) or a whole home directory is called out in the table with a line in
`** LOUD CAPS **`, because those are exactly the two shapes `disclose` would
otherwise let a client under-report (see `docs/tokens.md` and
`internal/project/scope_breadth.go`). Nothing on this machine currently triggers that warning
— every registered scope names a bounded subfolder.

`relay grant` **never prints a secret**. It builds a `StoredToken` from the
clear fields already in `settings.json` and reads through the same
permission-derivation code the router uses at call time — it does not, and
cannot, print a project's token.

An access profile's record also names every enrolment that reaches it, and
marks a `cli_admin` one loudly — the same posture `DescribeGrant` shows the
enrolment itself (ADR-018 decision 5, symmetrically). An enrolment with the
bit off is still listed, never omitted:

```
ACCESS PROFILE  Hermes Mail  (id: 477d9a17-da03-45eb-a433-764f93fe96fc)
  macmcp         access=read   outbound=blocked  tools=mail_*
                 scope: (none set)
  enrolments:
    hermes-mail          ** CLI-ADMIN: ON — this certificate may narrow this profile's own grant **
    hermes-ro            cli-admin: off
```

`--json` carries the same enrolments as a structured `enrolments` array on
each record.

## `relay audit`

Tails the tool-call audit log — relay's own ground truth for anything it
gates. Reads the JSONL file directly (`readAuditTail`), so unlike `relay
grant` it works with the tray stopped and needs nothing sealed.

```
relay audit [--tail N] [--project ID] [--mcp ID] [--outcome OUTCOME]
            [--kind KIND] [--event EVENT] [--grep TEXT] [--json]
            [--path] [--authority]
```

| Flag | Meaning |
|---|---|
| `--tail` | Show the most recent N matching events (default 50). |
| `--project` | Filter by project / access profile id. A remote actor's `project_id` names an access profile — same field, same ids. |
| `--mcp` | Filter by MCP id. |
| `--outcome` | `ok`, `error`, `tool_error`, `denied`, `unauthorized`, `throttled`, `pending`. `scope_violation` is also accepted here even though it is a *field*, not an outcome — it selects `tool_error` rows the MCP itself marked as a resource-scope refusal. |
| `--kind` | Actor kind: `project`, `service`, `remote`, `relay`, `control`, `operator`, `unknown`. |
| `--event` | `call_tool`, `list_tools`, `list_skills`, `mcp_down`, `mcp_up`, `control_decision`, `credential_issued`, `credential_revoked`. |
| `--grep` | Substring match over tool, MCP, error, project/profile, caller, args, and an issuance record's kind/identifier/name/grants. |
| `--json` | Emit raw JSONL (oldest first) instead of a table. |
| `--path` | Print the log file's path and exit. |
| `--authority` | Print a second line per call with the access mode, outbound grant, and injected scope — off by default so scripts parsing the table's columns never see the shape change. |

Needs service: no. Prompts: no. Works over SSH: yes.

```
$ relay audit --tail 5
TIME      OUTCOME  PROJECT                      MCP     TOOL      MS  CALLER                                DETAIL
15:49:55  pending  Hermes Files v3              fsmcp3  fs_grep   0   hermes-v3                             {"pattern":"api_key"}
15:49:55  ok       Hermes Files v3              fsmcp3  fs_grep   12  hermes-v3                             {"pattern":"api_key"}
15:49:55  denied   Hermes Files v3 (read-only)  -       fs_write  0   hermes-v3-ro                          access denied: no tool named 'fs_write' is available to this grant (granted: fsmcp3ro)
14:36:58  ok       -                            -       -         0   81d25ca0-23dd-45a0-ab75-f51925ec2df4  GET /api/projects  class=read  transport=tcp
14:36:58  denied   -                            -       -         0   81d25ca0-23dd-45a0-ab75-f51925ec2df4  POST /api/projects  class=configure  transport=tcp  class not granted
```

`--path`:

```
$ relay audit --path
/Users/you/Library/Application Support/relay/logs/audit/toolcalls.jsonl
```

`--json` (one line per event, real capture):

```
$ relay audit --tail 2 --json
{"id":"dc0846d4-...","ts":"2026-08-28T21:36:58.793088Z","dur_ms":0,"event":"control_decision","actor":{"kind":"control","auth":"token","cred_id":"81d25ca0-..."},"outcome":"ok","scope":null,"method":"GET","path":"/api/projects","class":"read","transport":"tcp"}
{"id":"f32f7d1a-...","ts":"2026-08-28T21:36:58.800708Z","dur_ms":0,"event":"control_decision","actor":{"kind":"control","auth":"token","cred_id":"81d25ca0-..."},"outcome":"denied","error":"class not granted","scope":null,"method":"POST","path":"/api/projects","class":"configure","transport":"tcp"}
```

Three outcomes worth internalising, because they mean different things and
are enforced in different places:

| Outcome | Means | Enforced by |
|---|---|---|
| `ok` | allowed and ran | — |
| `tool_error` + `scope_violation: true` | the tool was granted; the **resource** was not | the MCP itself |
| `denied` | the grant never included the tool at all | relay, before the MCP is even reached |
| `throttled` | the grant was legitimate; the *pattern of use* was not | relay's enrolment budget |

A scope violation is always an error, never a quietly-empty result — an
empty list from a granted tool means something else broke.

## `relay logs`

```
relay [--config-dir DIR] [--trace ID] logs [--json] [--event KEY] [--since TIME] [--follow [--timeout DUR]]
```

Prints relay's log lines, filtered. It reads files, not the server, so it works
with the server stopped and over SSH, and it never creates DIR or a log file.
Event lines and their keys are described in [`events.md`](events.md).

**Files read**, under `DIR/logs`: `relay.log.1`, `relay.log`,
`relaysessions.log.1`, `relaysessions.log`. A missing one is skipped. The lines
are merged and stable-sorted by `ts`. If neither relay log exists the command
prints `error: no relay log in DIR/logs; is DIR a relay config dir?` and exits
`2`.

| Flag | Meaning |
|---|---|
| `--trace ID` (global) | Only lines whose `trace_id` is ID. |
| `--event KEY` | Only lines whose `event` is KEY. KEY must be an event key such as `service.restart`, else exit `2`. |
| `--since TIME` | Only lines at or after TIME: an RFC 3339 time (fraction and zone optional; no zone means UTC) or a positive Go duration such as `1h`, meaning now minus it. A line whose `ts` cannot be read fails the filter. |
| `--json` | Print each matching line exactly as stored, one per line. Text mode is for people and is not a stable interface. |
| `--follow` | Print the existing matches, then new lines as they are appended, until `--timeout` or SIGINT or SIGTERM. |
| `--timeout DUR` | A positive duration. Only valid with `--follow`, else exit `2`. |

Filters combine with AND. With no filter, lines that are not JSON (a panic,
third-party output) print as stored; with any filter they never match.

`--follow` reads from the descriptors the first read opened, at the offsets it
stopped at, so no line is missed or repeated. It wakes on file changes (kqueue),
not on a timer. When relay rotates a log it reads the old file to its end, opens
the new one from the start, and keeps reading an old file a writer still holds
until that file is deleted. `--follow --event KEY` prints the first match and
exits `0`; a match already in the files counts, so it cannot lose a race with
the operation that wrote it.

| Exit | Meaning |
|---|---|
| `0` | At least one line was printed (with `--follow --event`: the first match). |
| `1` | Nothing matched, or the timeout or a signal came before any match. |
| `2` | Usage error, an unreadable log, or no relay log at DIR (the message names DIR). |

An invalid `--trace` exits `1` from the global parser before `logs` runs; its
message tells it apart from "no match".

## `relay credential`

Control-plane API credentials — the bearer that authenticates a caller to
relay's control-plane HTTP API (the frontend socket, and the `api.listen` or `RELAY_API_LISTEN`
listener if bound). See [`docs/tokens.md`](tokens.md#control-plane-credentials-adr-015)
for the five classes and what each reaches.

```
relay credential mint --name NAME --class CLASS [--class CLASS...] [--ttl DURATION]
relay credential list [--include-expired]
relay credential revoke --id ID
```

### `credential mint`

| Flag | Meaning |
|---|---|
| `--name` | Human-readable name (required). |
| `--class` | Repeatable. One of `read`, `configure`, `grant`, `execute`, `proxy`. At least one required — an unrecognized class or an empty set is refused. |
| `--ttl` | How long the credential lives (e.g. `12h`). Omit for one that never expires. A negative value is refused. |

Needs service: yes. Prompts: yes. Works over SSH: no.

```
$ relay credential mint -h
Usage of credential mint:
  -class value
    	capability class this credential may exercise (repeatable): read,configure,grant,execute,proxy
  -name string
    	human-readable name for this credential (required)
  -ttl duration
    	how long this credential lives (e.g. 12h); omit for one that never expires
```

On success it prints something shaped like:

```
minted credential "eve-view"
  id:      5d6dad87-4a31-40f6-88f8-9193adcba554
  classes: read,configure
  created: 2026-08-28T21:00:00Z
  expires: never
  token:   <64 hex characters>
  this token is shown ONCE and is not recoverable — only its SHA-256 is stored
  present it as: Authorization: Bearer <token>
```

**The plaintext token is shown exactly once, right here, and nowhere else.**
`relay credential list` never prints it, and neither does the Settings
window — only its SHA-256 hash is ever stored. Losing it means revoking and
minting again.

`legacy-frontend-token` is reserved: relay deletes every credential under that
name on start, so both `mint` and `revoke` refuse that name.

### `credential list`

Example:

```
$ relay credential list
ID                                    NAME      CLASSES         CREATED               EXPIRES
bacf762f-6eb1-425b-bd20-20756a97b6c5  eve-view  read,configure  2026-08-28T21:34:57Z  never
```

`--include-expired` also shows expired records — otherwise they're hidden,
awaiting the next mint's lazy reap. `EXPIRES` prints `never` for a credential
minted with no `--ttl`, and `<timestamp> (expired)` for one whose time has
passed. Needs service: yes (`credential.list`; the answer carries no hash).
Prompts: no. Works over SSH: yes.

### `credential revoke`

```
$ relay credential revoke -h
Usage of credential revoke:
  -id string
    	id of the credential to revoke (required)
```

Needs service: yes. Prompts: yes. Works over SSH: no. Illustrative output,
built from `credentialRevoke`'s print format — not run here, since it would
both prompt and destroy the credential minted above:

```
revoked credential "5d6dad87-4a31-40f6-88f8-9193adcba554"
  name:    eve-view
  classes: read,configure
  its token stops authenticating on the next request; nothing else was touched
```

## `relay enrol`

Remote-client enrolment: signs a client certificate off relay's own CA and
hands back a certificate the client can use (see
ADR-009 (a remote project is a capability grant to a client on another machine, not a directory)
and ADR-011 (resource scope: relay tracks values, never their meaning)
for the remote model this feeds). There is no self-service path and no
bootstrap token by design — every enrolment is a host-side operator act.

Three ways to get there. `enrol create` generates the client's private key on
this host and emits a bundle containing it — the legacy path, kept working
but deprecated in its own output. `enrol sign` takes a certificate signing
request the client generated on its own machine (`relayremote enrol`) and
returns only certificates: the private key never leaves the client, and
never exists in this process at all. Prefer `sign`. `enrol requests` /
`enrol approve` / `enrol refuse` are the network CSR path's operator
surface (ADR-018 decision 8, narrowed by ADR-019): an unenrolled remote
lodges a CSR over the enrolment-request listener, a six-character SAS
comparison code identifies it, and the operator approves (signing it, same
gate and digest as `sign`) or refuses it from here — never self-service.

```
relay enrol create --client-id ID --grant PROFILE-ID [--grant PROFILE-ID...]
                    [--window-seconds N] [--max-calls N] [--max-result-bytes N]
relay enrol sign --client-id ID --csr PATH|- [--grant PROFILE-ID...]
                  [--window-seconds N] [--max-calls N] [--max-result-bytes N]
                  [--out DIR]
relay enrol list
relay enrol update --client-id ID [--window-seconds N] [--max-calls N]
                    [--max-result-bytes N] [--grant PROFILE-ID...] | [--clear-grants]
                    [--cli-admin[=true|false]]
relay enrol revoke --client-id ID
relay enrol requests [--json]
relay enrol approve --id REQUEST_ID --client-id ID (--grant PROFILE-ID [--grant PROFILE-ID...] | --no-grant)
                     [--window-seconds N] [--max-calls N] [--max-result-bytes N]
relay enrol refuse --id REQUEST_ID
relay enrol ca-fingerprint
```

### `enrol create`

| Flag | Meaning |
|---|---|
| `--client-id` | Human-readable, unique id for this enrolment (required). |
| `--grant` | Repeatable. Id of an **access profile** (a `kind: remote` record) this certificate may use — never a local project. |
| `--window-seconds` | Budget window, seconds (default 3600). |
| `--max-calls` | Max tool calls per window (default 120). |
| `--max-result-bytes` | Max cumulative result bytes per window (default 67108864, 64 MiB). |

Needs service: yes. Prompts: yes. Works over SSH: no.

```
$ relay enrol create -h
Usage of enrol create:
  -client-id string
    	human-readable id for this enrolment (required, unique)
  -grant value
    	access profile id this certificate may use (repeatable); a grant must name an access profile (a remote-kind record), never a local project
  -max-calls int
    	max tool calls per window (default 120)
  -max-result-bytes int
    	max cumulative result bytes per window (default 67108864)
  -window-seconds int
    	budget window in seconds (default 3600)
```

This machine's `hermes` enrolment was created with exactly this shape (see
[RUNBOOK-vm-stack.md](../../RUNBOOK-vm-stack.md)):

```
relay enrol create --client-id hermes --grant 477d9a17-da03-45eb-a433-764f93fe96fc
```

Illustrative output — reconstructed from `enrolCreate`'s print format plus
this enrolment's own real, already-stored fingerprint (`relay enrol list`
below); the command was not re-run, since `hermes` already exists and this
is a mutating command:

```
created enrolment "hermes"
  fingerprint: sha256:a44f923fa5f84970facc53f83d16c72cc2123dd8104703162a59f761fbb5dc31
  profiles:    477d9a17-da03-45eb-a433-764f93fe96fc
  bundle:      /Users/you/Library/Application Support/relay/enrolments/hermes
  copy this directory to the client machine; the private key inside it is never recoverable
  this bundle's private key was generated on this host — prefer `relay enrol sign`, where the key never leaves the client machine.
```

### `enrol sign`

The CSR flow (ADR-018 decision 6 step 1): the client generates its own
keypair and sends only a signing request across, so relay never holds — and
never writes to disk — a private key it did not generate itself.

| Flag | Meaning |
|---|---|
| `--client-id` | Human-readable, unique id for this enrolment (required). Names the enrolment; the CSR's own CN is advisory and is overridden. |
| `--csr` | Path to the signing request, or `-` for stdin (required). |
| `--grant` | Repeatable. Id of an **access profile** this certificate may use — never a local project. |
| `--window-seconds` | Budget window, seconds (default 3600). |
| `--max-calls` | Max tool calls per window (default 120). |
| `--max-result-bytes` | Max cumulative result bytes per window (default 67108864, 64 MiB). |
| `--out` | Also write `client.crt` and `ca.crt` into this directory, for copying to the client machine. |

Needs service: yes. Prompts: yes. Works over SSH: no — and a CSR's natural
habitat is an SSH session or a USB stick carried to this machine, so expect
to run this one at the Mac's own screen even when the CSR itself arrived
over the network.

The client side of this flow is `relayremote enrol` (generates the keypair
and the CSR) and `relayremote install` (installs the certificate this
command hands back) — see relayRemote's own docs.

```
relay enrol sign --client-id hermes --csr client.csr --grant 477d9a17-da03-45eb-a433-764f93fe96fc --out ./signed
```

Illustrative output:

```
signed enrolment "hermes"
  fingerprint: sha256:a44f923fa5f84970facc53f83d16c72cc2123dd8104703162a59f761fbb5dc31
  profiles:    477d9a17-da03-45eb-a433-764f93fe96fc
  certificate: /Users/you/Library/Application Support/relay/enrolments/hermes
  copy client.crt and ca.crt to the client machine, beside the client.key it generated;
  no private key was written on this host
  copies also written to: ./signed
```

Nothing under `<config>/enrolments/hermes/` for a CSR enrolment is a
secret: `client.crt` and `ca.crt` only. `--out` copies are public
certificates too, so they are written `0644` rather than the config-dir
copy's `0600`.

### `enrol list`

Real capture — this machine's five enrolments, one per project + read/write
mode combination:

```
$ relay enrol list
CLIENT ID        PROFILES                              CLI-ADMIN  CALLS/WINDOW  BYTES/WINDOW  CREATED               FINGERPRINT
hermes           477d9a17-da03-45eb-a433-764f93fe96fc  -          120/3600s     67108864      2026-08-26T00:02:54Z  sha256:a44f923fa5f84970facc53f83d16c72cc2123dd8104703162a59f761fbb5dc31
hermes-files     59c19c5b-b248-493c-a094-4397a56c8693  -          120/3600s     67108864      2026-08-26T14:32:16Z  sha256:9820e514f38b35d2b1af8687260125853b9e7577b3e37036224ad438f1379bb1
hermes-files-ro  aaaabf48-95c9-4d72-97b8-7060138930f1  -          120/3600s     67108864      2026-08-26T14:32:16Z  sha256:d1846a1b393e738dfe7043a5299cd1f67a9c203bdb01d27cb070c19228f4c6ca
hermes-v3        b0000000-0000-4000-8000-000000000001  -          120/3600s     67108864      2026-08-26T18:55:54Z  sha256:79129197d148052d196e1d4ad2fbc4b4a64d770024601043a7943f5b9b5fcaa0
hermes-v3-ro     b0000000-0000-4000-8000-000000000002  -          120/3600s     67108864      2026-08-26T19:08:37Z  sha256:46c0903492ef4d091cfc704d92fa079cd882d4c53ed750a513a1001853adbff3
```

The fingerprint is printed in full (all 64 hex characters), deliberately: an
enrolment's audit history stays legible after it's revoked, and a shortened
listing is the obvious place someone starts copying a truncated form from.
Needs service: yes (`enrolment.list`). Prompts: no. Works over SSH: yes.

### `enrol update`

Every flag is optional; an unset one leaves the stored value alone.
`--grant`, passed at all, **replaces the whole grant list** — same rule as
`create`. `--clear-grants` empties it explicitly and is mutually exclusive
with `--grant`. `--cli-admin` (`--cli-admin=false` to withdraw it) lets this
certificate narrow its own access profiles over the remote listener
(ADR-018 decision 4) — never widen them, and never anything outside its own
sandbox. It rides on `enrol update` rather than a new subcommand: `enrol
update` is already the one door for "change what this certificate reaches
without touching the certificate". Turning it off prompts too, same as
turning it on.

```
$ relay enrol update -h
Usage of enrol update:
  -clear-grants
    	remove every access profile grant, leaving the certificate enrolled but able to reach nothing; mutually exclusive with --grant
  -cli-admin
    	let this certificate adjust its OWN access profiles over the remote listener (narrowing only); --cli-admin=false withdraws it. Effective on the client's next request.
  -client-id string
    	client id of the enrolment to update (required)
  -grant value
    	access profile id this certificate may use (repeatable); passing --grant at all REPLACES the whole grant list, same as create
  -max-calls int
    	new max tool calls per window (0 resets to the default; omit to leave unchanged)
  -max-result-bytes int
    	new max cumulative result bytes per window (0 resets to the default; omit to leave unchanged)
  -window-seconds int
    	new budget window in seconds (0 resets to the default; omit to leave unchanged)
```

Needs service: yes. Prompts: yes. Works over SSH: no. It is gated even
though it only replaces an existing grant list, because replacing a grant
list is exactly the "widens one" case the gate exists for. Toggling
`cli-admin` is gated the same way, in both directions.

With `cli_admin` on, the enrolment's own certificate reaches a second
request table over the remote listener — `DescribeGrant` (its own posture,
the same view `relay grant` shows) and `NarrowGrant` (replace its own
`allowed_mcp_ids` / `allowed_tools` / `access` / `allow_external` with a
strictly narrower set — never wider, on any axis, and never another
enrolment's profile). Neither is a CLI subcommand; both are wire requests
the client sends itself. `relay grant` names every enrolment reaching a
profile and marks a `cli_admin` one loudly, and `relay audit --grep
cli_admin` finds both the toggle and every narrowing an enrolment made of
its own grant.

### `enrol revoke`

```
$ relay enrol revoke -h
Usage of enrol revoke:
  -client-id string
    	client id to revoke
```

Needs service: yes. Prompts: yes. Works over SSH: no. Revoking removes the
grant record; the signed certificate itself is unaffected (it just stops
being able to reach anything, since nothing recognizes it any more).

### `enrol requests`

Lists live pending CSR requests lodged over the enrolment-request listener.
Brokered like every other mutation here: the pending table lives only in
the running tray's memory, so this needs the service up even though it is
read-only.

| Flag | Meaning |
|---|---|
| `--json` | Print machine-readable JSON instead of a table. |

```
$ relay enrol requests -h
Usage of enrol requests:
  -json
    	print machine-readable JSON
```

The table form's `SAS` column carries the six-character comparison code: a
value once both sides have completed it, `(waiting)` before the client
opens its commitment, `FAILED` if it opened it wrongly, and `-` for a
legacy carried-pin `relayremote request` row that has no comparison at all.

Needs service: yes. Prompts: no. Works over SSH: yes.

### `enrol approve`

Signs a pending request exactly as `enrol sign` does — same op, same gate,
same digest — over the request's own stored CSR bytes, so there is no
`--csr` flag here: the CSR is whatever the client already lodged. Requires
a grant: with no `--grant` it refuses with a message naming `--no-grant` as
the explicit way to enrol a machine with no access (ADR-019 decision 7 — an
enrolment that reaches nothing reads on the client as a broken install, so
the operator says which they meant). Also refuses, before the gate, a
request whose comparison was never completed or failed.

| Flag | Meaning |
|---|---|
| `--id` | Pending request id to approve (required). |
| `--client-id` | Human-readable, unique id for this enrolment (required); names the enrolment, not the request's label. |
| `--grant` | Repeatable. Id of an **access profile** this certificate may use — never a local project. Mutually exclusive with `--no-grant`. |
| `--no-grant` | Enrol this machine with no access at all; nothing works until a later `relay enrol update --grant`. |
| `--window-seconds` | Budget window, seconds (default 3600). |
| `--max-calls` | Max tool calls per window (default 120). |
| `--max-result-bytes` | Max cumulative result bytes per window (default 67108864, 64 MiB). |

```
$ relay enrol approve -h
Usage of enrol approve:
  -client-id string
    	human-readable id for this enrolment (required, unique); this, not the request's label, names the enrolment
  -grant value
    	access profile id this certificate may use (repeatable); a grant must name an access profile (a remote-kind record), never a local project
  -id string
    	pending request id to approve (required)
  -max-calls int
    	max tool calls per window (default 120)
  -max-result-bytes int
    	max cumulative result bytes per window (default 67108864)
  -no-grant
    	enrol this machine with no access at all (nothing will work until `relay enrol update --grant` adds one); mutually exclusive with --grant
  -window-seconds int
    	budget window in seconds (default 3600)
```

Needs service: yes. Prompts: yes — and unlike every other gated mutation in
this doc, refusing over SSH names the working door instead of the generic
advice, because a request genuinely sits in a queue: approve it from the
Mac's own screen (Settings → Remote Clients → Pending requests) instead.
Works over SSH: no.

### `enrol refuse`

Declines a pending request without ever reaching the presence gate —
declining a stranger is not the act the gate protects. The request keeps
its own expiry; refusing it does not shorten that.

| Flag | Meaning |
|---|---|
| `--id` | Pending request id to refuse (required). |

```
$ relay enrol refuse -h
Usage of enrol refuse:
  -id string
    	pending request id to refuse (required)
```

Needs service: yes. Prompts: no. Works over SSH: yes.

### `enrol ca-fingerprint`

Prints relay's CA certificate hash — the value a client pins with
`relayremote enrol --ca-fingerprint` so it can tell the real relay from an
impostor on the network. Reads `ca.crt` straight off disk, so it is the
one `enrol` subcommand that works with the tray stopped; the certificate is public and the
key it corresponds to is not needed to fingerprint it.

Needs service: no. Prompts: no. Works over SSH: yes.

## `relay login`

Host-side anchor for interactive passkey login (ADR-016). There is no
self-registration: an operator mints a single-use, two-minute bootstrap code
and redeems it at the login page to register a passkey. The bootstrap code
is **not** a control-plane credential and authorises exactly one thing.

```
relay login enrol
relay login list
relay login revoke --id ID
relay login sessions [--json]
relay login sign-out --id ID [--json]
```

### `login enrol`

Needs service: yes. Prompts: yes. Works over SSH: no. Mints (and replaces
any existing) bootstrap code. Illustrative output — reconstructed from
`loginEnrol`'s print format; not run here, since it is gated and would
raise a real password prompt:

```
$ relay login enrol
login code: <16 hex characters>
  expires:   <timestamp> (valid for 2m0s, single use)
  this code registers a passkey — it is NOT a password and is never accepted in place of one
  open http://localhost:<API listener port>/relay/login and enter it to register a passkey
  this code is shown ONCE and is not recoverable
```

The tray's own **Show Login Code…** menu item reaches the identical gated
core method — this command is not a weaker second door, it is the same
door, useful when the tray's menu is unreachable (e.g. no one is at the
keyboard to click it, but someone is running the CLI in the desktop
session).

### `login list`

Real capture:

```
$ relay login list
NAME                                  CREDENTIAL ID  CREATED               SIGN COUNT
browser passkey 2026-08-28T21:35:51Z  xNtuo_H_0XSA…  2026-08-28T21:35:51Z  2
```

Never prints the public key — there's no legitimate reason for a listing to
show it. Needs service: yes (`login.list`). Prompts: no. Works over SSH: yes.

### `login revoke`

```
$ relay login revoke -h
Usage of login revoke:
  -id string
    	credential id of the passkey to revoke (required)
```

Needs service: yes. Prompts: yes. Works over SSH: no.

**Revoking a passkey does not sign anyone out.** The passkey record and any
browser session it already minted are separate objects with separate
lifetimes — a browser that logged in earlier keeps working, up to twelve
hours, until its own control-plane credential expires or is revoked with
`relay credential revoke` (or Settings → Passkeys → Signed-in Browsers →
Sign out).

### `login sessions`

Needs service: yes. Prompts: no. Works over SSH: yes. Operator-only. Lists the
signed-in browser sessions the Passkeys tab shows under **Signed-in
Browsers**: name, credential id, created and expires. `--json` prints
`{"sessions":[{"id","name","created","expires"}]}`. It never prints a token
or a hash.

```
$ relay login sessions
NAME                 ID        CREATED               EXPIRES
browser (Acme Mac)   cr_a1b2   2026-10-09T08:00:00Z  2026-10-09T20:00:00Z
```

### `login sign-out`

Needs service: yes. Prompts: no. Works over SSH: yes. Operator-only. Ends one
browser session by its id from `relay login sessions`, the same act as
**Sign out** in the Passkeys tab. It refuses an id that is not a browser login
session; use `relay credential revoke` for that. `--json` prints
`{"id","name"}`.

## `relay eve`

Host-side anchor for a second browser to register an eve passkey
([`docs/eve-passkey-enrolment.md`](eve-passkey-enrolment.md)). Eve's first
visitor enrols directly; every later browser needs an operator to open a
five-minute, single-use window first — from the tray's **Allow Eve Passkey
Enrolment…** item or this command.

```
relay eve enrol
relay eve list
relay eve revoke --id ID
```

### `eve enrol`

Needs service: yes. Prompts: yes. Works over SSH: no. Opens (and replaces
any existing) enrolment window. Illustrative output — reconstructed from
`eveEnrol`'s print format; not run here, since it is gated and would raise a
real password prompt:

```
$ relay eve enrol
eve passkey enrolment open until 10:15:00 (5m0s, single use)
  on the new browser, open Eve, and tap "Add this browser"
```

The tray's own **Allow Eve Passkey Enrolment…** menu item reaches the
identical gated core method — this command is not a weaker second door, it
is the same door, useful over a terminal in the desktop session when no one
is at the keyboard to click the tray. Relay notifies the console when the
window opens and again when it is consumed, naming the source address that
took it.

### `eve list`

Shows relay's mirror of eve's own credential list, as the running tray holds
it ([`docs/eve-passkey-enrolment.md`](eve-passkey-enrolment.md) decision 8). STATUS is `-` for an ordinary credential or
`revocation pending` for one relay has revoked that eve has not yet applied.

```
$ relay eve list
LABEL                    CREDENTIAL ID  CREATED               LAST USED             STATUS
Mozilla/5.0 (iPhone...)  xNtuo_H_0XSA…  2026-09-07T10:12:31Z  2026-09-07T18:02:11Z  -
```

Needs service: yes (`eve.list`). Prompts: no. Works over SSH: yes.

### `eve revoke`

```
$ relay eve revoke -h
Usage of eve revoke:
  -id string
    	credential id of the eve passkey to revoke (required)
```

Needs service: yes. Prompts: yes. Works over SSH: no. Relay records the
revocation as pending and never touches eve directly -- eve applies it on
its own next 30-second poll, or immediately if that browser tries to sign in
first, and signs out every session that passkey minted.

## `relay project`

```
relay project update --id ID --files-read-only=true|false
relay project create (--name N --path P | --file F) [--json]
relay project edit --id ID --file F [--json]
relay project remove --id ID [--json]
relay project rotate-token --id ID [--json]
relay project token --id ID [--json]
relay project regen-skill --id ID [--json]
```

The verbs after `project update` are operator-only and call the cores the
Projects tab and the `/api/projects` routes call.

### `project update`

Needs service: yes (`project.update`). Prompts: no. Works over SSH: yes.
Sets `files_read_only` on a project: `true` makes relay refuse write, rename,
move, delete and mkdir on its files (`docs/project-files.md`), `false`
clears it. It goes through the same core as `PUT /api/projects/{id}`. It only
narrows what eve may do, so it raises no presence prompt.

```
$ relay project update --id p_acme --files-read-only=true
project Acme (p_acme): file changes are refused (files_read_only)
```

### `project create`

Needs service: yes. Prompts: yes (`project.grant`). Works over SSH: no.
`--name` and `--path` create a project with no MCP access; `--file F` takes the
`POST /api/projects` body for everything else (`-` reads stdin). A relative
`--path` resolves against the current directory. `--json` prints the project as
`POST /api/projects` returns it.

### `project edit`

Needs service: yes. Prompts: yes when the body widens a grant, no when it only
narrows. Works over SSH: only when it narrows. `--file F` takes the
`PUT /api/projects/{id}` body, which includes `kind` for a local-to-remote
conversion and the scope values (`contextSchema`) of each MCP. `relay mcp
scope-fields` lists the fields a scope accepts. `--json` prints the project.

### `project remove`

Needs service: yes. Prompts: no. Works over SSH: yes. Removes the project.
`--json` prints `{"id","name"}`.

### `project rotate-token`

Needs service: yes. Prompts: yes (`project.rotate_token`). Works over SSH: no.
Rotates the project's bearer token and prints the new token once, alone on one
line so `$(...)` captures it; `--json` prints `{"token"}`. The old token stops
working. The issuance row in the audit log carries the presence id.

### `project token`

Needs service: yes. Prompts: yes (`project.reveal_token`). Works over SSH: no.
Prints the project's current bearer token alone on one line; `--json` prints
`{"token"}`. It is the CLI reveal of what the Settings window shows behind
the eye icon, and it is stricter: operator-only, behind a presence prompt, and
recorded as `credential_disclosed` in the audit log before the token prints. A
cancelled prompt prints nothing and records a denied `control_decision` row. A
degraded sealed store refuses with `the project token cannot be unsealed`.
There is no HTTP route for it.

### `project regen-skill`

Needs service: yes. Prompts: no. Works over SSH: yes. Rewrites the project's
SKILL.md from its current grant. `--json` prints `{"path"}`.

## `relay mcp`

External MCP registration: what relay connects to or spawns, over stdio or
HTTP.

```
relay mcp register --name NAME [--id ID] --command CMD [--args ARG...] [--env K=V...]
                    [--transport stdio|http] [--url URL] [--tcc-services LIST]
relay mcp unregister --id ID | --name NAME
relay mcp list
relay mcp authenticate --id ID [--json]
relay mcp reset-permissions --id ID [--json]
relay mcp scope-fields --id ID [--json]
relay mcp call --token TOKEN --list | --tool NAME [--args JSON]   # see relay mcpExec, below
```

### `mcp register`

```
$ relay mcp register -h
Usage of mcp register:
  -args value
    	command arguments (repeatable)
  -command string
    	command to run (required for stdio transport)
  -env value
    	environment KEY=VALUE (repeatable)
  -id string
    	record id (default: slugified --name)
  -name string
    	display name (required)
  -tcc-services string
    	comma-separated TCC services the MCP needs (e.g. calendar,contacts,reminders,microphone,appleevents)
  -transport string
    	transport type (stdio or http) (default "stdio")
  -url string
    	MCP endpoint URL (required for http)
```

Needs service: yes. Prompts: yes. Works over SSH: no.

Registering an HTTP MCP that answers 401 during discovery still persists
the record — you then finish authentication with `relay mcp authenticate`
(below) or the Settings window's **Authenticate** button.

### `mcp authenticate`

Needs service: yes. Prompts: yes (`mcp.oauth.start`). Works over SSH: no.
Operator-only. Runs the OAuth flow of the Authenticate button for an HTTP MCP.
Under the tray it opens your browser. Under `relay serve` there is no browser,
so it prints the authorization URL on its own line as soon as it has one, then
`authenticated ID` when the callback lands. `--json` prints one line
`{"id","authenticated":true}` under the tray, and two lines under `relay
serve`: `{"id","authorization_url"}` on arrival, then
`{"id","authenticated":true,"authorization_url"}`. It never prints the OAuth
state or a token.

### `mcp reset-permissions`

Needs service: yes. Prompts: no. Works over SSH: yes. Operator-only. Resets the
macOS privacy (TCC) grants for an MCP registered with `--tcc-services`, the
act of **Reset Permissions** in the MCP Servers tab. It refuses an MCP with no
such services. `--json` prints the result the tab shows.

### `mcp scope-fields`

Needs service: yes. Prompts: no. Works over SSH: yes. Operator-only. Lists the
scope fields an MCP's `contextSchema` declares, with their allowed values, as
`GET /api/mcps/{id}/scope_fields` returns them. A project's scope values are
written with `relay project edit`.

### `--id` — read this before you register anything a second time

`--id` is optional and defaults to a slug of `--name`: lowercase, every
non-alphanumeric run collapsed to a single `-`. That default is exactly why
you should usually **pass `--id` explicitly** for anything you might ever
re-register.

This machine has the concrete case that makes the warning real.
`fsMCP v3 (testfolder)` is registered under the id `fsmcp3` — chosen
deliberately when it was first created — but its display name slugifies to
`fsmcp-v3-testfolder`:

```
$ relay mcp list
ID        NAME                              TRANSPORT  ENDPOINT
macmcp    macMCP                            stdio      /Users/you/src/macMCP/.build/release/macmcp
fsmcp     fsMCP                             stdio      /Users/you/.local/bin/fsmcp
fsmcp3    fsMCP v3 (testfolder)             stdio      /Users/you/.local/bin/fsmcp3 --root /Users/you/src/testfolder
fsmcp3ro  fsMCP v3 (testfolder, read-only)  stdio      /Users/you/.local/bin/fsmcp3 --root /Users/you/src/testfolder --read-only
```

Project and access-profile grants reference the MCP by **id**
(`allowed_mcp_ids`), never by name. Re-running
`relay mcp register --name "fsMCP v3 (testfolder)" --command ...` *without*
`--id fsmcp3` would not update the existing record — it would write a
**second** MCP under `fsmcp-v3-testfolder`, and every access profile that
grants `fsmcp3` would go on pointing at the original record, oblivious to
the new one. The presence prompt makes this concrete, too: the digest a
grant is bound to includes `id`, precisely so an approval given for one id
can never land on a different one.

The rule in one sentence: **if you are re-registering something that
already has grants pointing at it, always pass the id it already has.**

### `mcp unregister`

```
$ relay mcp unregister -h
Usage of mcp unregister:
  -id string
    	MCP ID
  -name string
    	MCP display name
```

Either `--id` or `--name` resolves to the same record (`ResolveMcpID`
matches on both). Needs service: yes. **Prompts: no.** Works over SSH: yes.
Unregistering only narrows what a caller already reaches — re-registering
under the same id still has to pass `mcp register`'s gate — so this command
is not presence-gated (ADR-018 step 3); it still writes a `config_change`
audit record and still refuses if issuance auditing is off, just with no
`presence_id` on the record.

### `mcp list`

Real capture, this machine's four registered MCPs:

```
$ relay mcp list
ID        NAME                              TRANSPORT  ENDPOINT
macmcp    macMCP                            stdio      /Users/you/src/macMCP/.build/release/macmcp
fsmcp     fsMCP                             stdio      /Users/you/.local/bin/fsmcp
fsmcp3    fsMCP v3 (testfolder)             stdio      /Users/you/.local/bin/fsmcp3 --root /Users/you/src/testfolder
fsmcp3ro  fsMCP v3 (testfolder, read-only)  stdio      /Users/you/.local/bin/fsmcp3 --root /Users/you/src/testfolder --read-only
```

Needs service: yes (`mcp.list`; environment values are never sent). Prompts:
no. Works over SSH: yes.

## `relay service`

Background service self-registration — the mechanism relayLLM,
relayScheduler and similar enhanced services use to tell relay what to run
and how to reach it.

```
relay service register --name NAME [--id ID] --command CMD [--args ARG...]
                        [--env K=V...] [--workdir DIR] [--url URL]
                        [--autostart[=true|false]]
                        [--capability frontend|manifest|models|model_host ...]
relay service unregister --id ID | --name NAME
relay service restart --id ID | --name NAME
relay service start --id ID | --name NAME [--json]
relay service stop --id ID | --name NAME [--json]
relay service action --id SVC --action ACT [--row JSON] [--json]
relay service config --id SVC [--set FILE] [--json]
relay service list
```

`start`, `stop`, `action` and `config` are operator-only. `start` and `stop`
are the Start and Stop buttons and the tray's service rows; they print
`{"id"}` with `--json`. `action` runs one action a service's manifest declares,
as the Service Inspector does, and prints `{"service_id","action_id","ok":true}`.
`config` prints a service's config file as `{"service_id","text"}`; with `--set
FILE` (`-` for stdin) it saves the file and prints `{"service_id","restarted"}`.
None of them prompts.

### `service register`

```
$ relay service register -h
Usage of service register:
  -args value
    	command arguments (repeatable)
  -autostart
    	start automatically
  -capability value
    	grant this service's launch identity a capability, repeatable: frontend (the frontend socket as read+configure+proxy+execute), manifest (RegisterManifest), models (model-endpoint calls, limited by --allowed-model), model_host (RegisterModelHost); sessions (SessionExited, the unfiltered model list) is refused on any service but the built-in relaysessions one; none given means none held
  -command string
    	command to run (required)
  -env value
    	environment KEY=VALUE (repeatable)
  -id string
    	record id (default: slugified --name)
  -name string
    	display name (required)
  -url string
    	service URL
  -workdir string
    	working directory
```

Needs service: yes. Prompts: yes. Works over SSH: no. The same `--id`
warning as `mcp register` applies here — an operator-chosen id survives a
re-register; a name-derived one might not match what a grant already
references.

**This one command covers both create and update.** `relay service
register` dispatches to an update when the resolved id already exists, and
to a create otherwise — the same split `POST /api/services` /
`PUT /api/services/{id}` make over HTTP, folded into one CLI verb.

**A flag you don't type leaves the stored value alone.** `--workdir`,
`--url`, and `--autostart` are absent-aware: on an update, a flag you did
not repeat on the command line carries the existing stored value forward
unchanged. A flag you *do* type is applied at exactly the value given,
including its zero value — so `--autostart=false`, typed explicitly, does
turn autostart off, and is different from never mentioning `--autostart` at
all. This is not a convenience shortcut; the presence-gate digest itself
encodes the presence bit, so "leave autostart alone" and "set autostart to
false" are bound to different digests and a prompt answered for one can
never be redeemed for the other.

**`--capability` sets what the service's launch identity may do, and every
register restates the whole set.** Repeat it once per capability — `frontend`
(the frontend socket as `read`+`configure`+`proxy`+`execute`, and
`RELAY_FRONTEND_SOCKET` in the service's environment), `manifest`
(`RegisterManifest` under the service's own id), `models` (model-endpoint
calls, limited by `--allowed-model`), `model_host` (`RegisterModelHost`);
`sessions` (`SessionExited`, the unfiltered model list, no calls) is refused
on any service but the built-in `relaysessions` one; see
[`docs/launch-identity.md`](launch-identity.md#identity-kinds-and-capabilities).
An unknown name is refused before anything reaches the tray. Unlike
`--workdir`, `--url` and `--autostart`, capabilities are not absent-aware: a
register with no `--capability` sets the **empty set** — the service can
start and say `Hello` and can do nothing else through relay — and the command
says so in its output rather than leaving it silent. A relayScheduler-style
service that both runs work through the front door and serves routes is
`--capability frontend --capability manifest`.

### `service unregister`

```
$ relay service unregister -h
Usage of service unregister:
  -id string
    	service ID
  -name string
    	service display name
```

Needs service: yes. **Prompts: no.** Works over SSH: yes. Not presence-gated
(ADR-018 step 3), on the same footing as `mcp unregister`: removing a
service record only narrows what a caller already reaches, and stopping the
running process is already ungated `configure`
(`POST /api/services/{id}/stop`). Still writes a `config_change` audit
record, still refuses if issuance auditing is off, just with no
`presence_id`.

### `service restart`

```
$ relay service restart -h
Usage of service restart:
  -id string
    	service ID
  -name string
    	service display name
```

Needs service: yes. **Prompts: no.** Works over SSH: yes (as far as the
gate is concerned — it still needs the bridge socket reachable). It is
brokered like every other mutating command — this CLI process cannot reach
the process registry that owns the running service, only the tray can — but
it changes no settings and restarts exactly what was already configured, so
it carries no presence check. Internally it is the same stop-then-start the
tray always did in place.

### `service list`

```
$ relay service list
no services registered
```

(This machine's stack — macMCP, fsMCP — is registered as MCPs, not
services; nothing is currently registered as a background service.) When a
service is registered, the table carries a `CAPABILITIES` column listing the
record's capabilities, or `none`. Needs service: yes (`service.list`). Prompts: no. Works over SSH: yes.

The table also carries a `STATE` column reporting relay's restart-supervision
state for the row (docs/service-manifest.md#restart-supervision): `running`,
`restarting (attempt N, next in Xs)`, `failed (exit E)`, or `-` when relay is
not supervising the service (never started this session, or the operator
stopped it). Supervision state exists only in the tray's memory, never in
`settings.json`; `service.list` (ungated) carries it beside the records, and
neither `env` nor `working_dir` is sent.

## `relay status`

```
relay [--config-dir DIR] status [--json]
```

Needs service: yes. Prompts: no. Works over SSH: yes. Operator-only. Prints what
the Overview tab shows: the version, the sealed-store warning, the config and
logs folders, each MCP's health and each service's runtime and restart
supervision. The text form is one line. `--json` prints:

```
{"version","seal_status","paths":{"config","logs"},
 "mcp_health":{ID:{...}},"service_runtime":{ID:{"pid","started_at"}},
 "service_supervision":{ID:{"phase","attempt",...}}}
```

`seal_status` is empty when the sealed store is healthy and the reason when it
is degraded. A service's log is `paths.logs` plus `<id>.log`.

## `relay remote`

```
relay remote show [--json]
relay remote set --file F [--json]
```

`show` needs the service, never prompts and prints the remote-listener
settings the Remote Clients tab shows (`GET /api/remote`): `enabled`, `listen`,
the `effective` address, `audit_enabled`, the enrolment-request listener and the
CA fingerprint. `set` replaces the whole record with a `PUT /api/remote` body
(`enabled`, `listen`, `enrolment_requests`, `enrolment_listen`, `remove`), so a
field the body leaves out is cleared. It prompts (`remote.configure`) when the
change widens what a remote client reaches, and then refuses over SSH. Both are
operator-only.

## `relay host`

```
relay host probe --id ID [--json]
relay host disconnect --id ID [--json]
```

Needs service: yes. Prompts: no. Works over SSH: yes. Operator-only. `probe`
re-runs the SSH discovery of the Hosts tab's Probe button and stores the
result; an unreachable host is not an error, it reads `unreachable`.
`disconnect` closes the host's live SSH connection. Both print the host as
`POST /api/hosts/{id}/probe` returns it with `--json`, and the text form is one
line with the host's status. An unknown id exits 1 with `host not found`.

## `relay sealed`

```
relay sealed reset [--json]
```

Needs service: yes. Prompts: yes (`sealed.reset`). Works over SSH: no.
Operator-only. The same act as the tray's **Reset Sealed Store…**:
it permanently deletes `settings.json`, the CA files and the keychain item,
and starts over with a fresh key. The prompt names what it destroys and is the
only confirmation; there is no `--yes` and no `--force`. A cancelled prompt
deletes nothing. `--json` prints `{"reset":true}`. The keychain item is shared
by every instance that runs as the same user, so approving it from a test
instance destroys the installed store too. See
[`docs/sealed-config.md`](sealed-config.md#break-glass-and-why-there-is-no-offline-recovery-code).

## `relay model`

```
relay model list [--json]
```

Needs service: yes. Prompts: no. Works over SSH: yes. Operator-only. Lists the
models the model picker offers, as the Settings window shows them (system-only
models omitted). `--json` prints the catalogue view.

## `relay session`

Sessions of the session host (`docs/session-host.md`). All operator-only, none
gated: a session launches as the operator caller, the one `relay sandbox` uses,
and the launch is audited.

```
relay session start --project ID --model M [--name N] [--directory D]
                    [--settings JSON] [--system-prompt S] [--append-claude-md]
relay session start --file F            # POST /api/sessions body
relay session list
relay session message --id ID (--text T | --file F)
relay session stop --id ID
relay session resume --id ID
relay session mode --id ID --mode M
```

`start --json` prints the `POST /api/sessions` body. `list --json` prints
`GET /api/sessions`. `message` waits for the reply and prints `{"text","stats"}`
with `--json`; `--file` takes `{"text","files"}`. `stop` prints `{"id"}`.
`resume` prints `{"session_id","resumed"}`: `resumed` is false when the session
is already live. `mode` changes a session's permission mode through the session
host and prints `{"session_id","mode"}`; for a local claude session the host
answers `resume_required`.

## `relay terminal`

```
relay terminal start --project ID --template T [--name N] [--directory D]
                     [--cols C] [--rows R] [--persist-session NAME]
                     [--extra-arg A]...
relay terminal start --file F           # POST /api/terminals body
relay terminal list
relay terminal log --id ID
relay terminal stop --id ID
relay terminal persistent-list --project ID
relay terminal persistent-kill --project ID --name NAME
```

All operator-only, none gated. `start --json` prints the `POST /api/terminals`
body and `list --json` the `GET /api/terminals` body. `log` prints the raw
terminal log; `--json` prints `{"id","log"}`. `stop` prints `{"id"}`.
`persistent-list` prints `GET /api/projects/{id}/persistent-sessions` for a
project on an SSH host, and `persistent-kill` prints `{"project_id","name"}`.

## `relay files`

```
relay files watch --project ID [--until TYPE] [--timeout DURATION] [--json]
```

Needs service: yes. Prompts: no. Works over SSH: yes. Operator-only. Streams the
frames eve's file tree receives on `/ws/files` for one project (`host_status`,
`watch_ok`, `watch_error`, `fs_event`), one JSON line per frame, from the same
watch hub. It runs until you interrupt it, `--timeout` passes, or a frame of
`--until TYPE` arrives. `watch_ok` is the signal that the watch is live.

## `relay debug` (test build only)

A build made with `-tags relaytest` (`./build.sh --test-build`) has one more
command. In a release build `relay debug` is `unknown command: debug`, exit
1, and `relay doors` lists no door for it. See
[`docs/testing.md`](testing.md#the-test-build).

### `debug clock`

```
relay [--config-dir X] debug clock [--json]
relay [--config-dir X] debug clock set <RFC3339> [--json]
relay [--config-dir X] debug clock advance <duration> [--json]
```

Reads or moves the clock the running server judges time by: credential
expiry, login and enrolment windows, restart backoff, token expiry and the
other decisions [`docs/testing.md`](testing.md#the-test-build) lists. It
reaches the server through the `debug.clock` admin op, which only an operator
terminal may call. A relay session or a sandbox is refused.

Text output is two lines, `now:` (UTC, RFC 3339 with fraction) and `offset:`
(a Go duration from wall time). `--json` prints
`{"now":"2026-10-01T01:30:00.000Z","offset_ms":-741600000}`. `advance` takes a
Go duration greater than 0; go back with `set`. The clock lives in memory, so
a server restart returns it to wall time. On the default config dir it
refuses: the clock is fixed there.

Exit 0 on success. Exit 1 when there is no server, the caller is refused, or
the server errors. Exit 2 on a usage error.

## `relay doors`

```
relay [--config-dir DIR] doors [--json]
```

Needs service: yes. Prompts: no. Works over SSH: yes. Operator-only. Lists every
HTTP route, IPC op, bridge request and CLI verb of the running server, built
from the tables the server dispatches from. Each door names its credential
class and the presence gates its core may require. It lists no id, path,
address or config value. The model endpoint, remote mTLS and enrolment-request
listeners are not listed.

## `relay sandbox`

Runs a terminal template in your own terminal, for the project holding the
current directory. It asks the running tray to launch the session and attaches
this terminal to it; the tool's exit status is the command's. Design and
rationale: [`docs/sandbox-command.md`](sandbox-command.md).

```
relay sandbox <template> [--project NAME-OR-ID]
```

| Flag | Meaning |
|---|---|
| `--project` | Choose among the projects that hold the current directory, by name or id. Required only when more than one does. |

Run it from the project root or any folder under it. It is not gated: there is
no presence prompt. A template with `sandbox: true` runs under Seatbelt as it
does from Eve; one with `sandbox: false` runs unconfined but wired to relay's
proxies. A template that omits `sandbox` is sandboxed; only `sandbox: false`
opts out. Closing the terminal, SIGHUP or killing the command ends the session;
sleeping the Mac does not.

It refuses, with a message and a non-zero exit, when: it is run inside a relay
session; stdin or stdout is not a terminal; relay is not running; the directory
is in no registered project; the directory is in several projects and
`--project` is missing (the matching projects are listed) or names one that does
not hold it; the template does not exist; or the template is not in the
project's allowed templates.

## `relay drop-in`

Takes a headless Claude session over in your own terminal. Relay waits for the
session's current turn to end, stops its headless process, and starts an
ordinary terminal from the project's `claude-code` template that resumes the
same conversation (`claude --resume`). This terminal shows that one. Design:
[`docs/session-host.md`](session-host.md).

```
relay drop-in <session-id>
```

There are no flags. Before relay answers, it prints
`relay: handing over <id>; waiting up to 60 s for the current turn to end` on
stderr; on exit it prints `relay: handed back <id>`. The exit status is
Claude's. Ending the terminal, by quitting Claude, closing the window or
SIGHUP, hands the session back as idle. It is not gated: there is no presence
prompt, as for `relay sandbox`. The same drop-in is `POST
/api/sessions/{id}/drop-in` (class `execute`), which answers with the new
terminal for a client to join.

It refuses, with a message and a non-zero exit, when: it is run inside a relay
session; stdin or stdout is not a terminal; relay is not running; no id is
given; the session does not exist, is not a Claude session, is not headless or
has not run a turn yet; a tool is running (the message names it) or the turn
does not end within 60 s; a terminal already has the session; or the
project's authorization refuses a `claude-code` terminal. A refusal stops
nothing: the session keeps running.

## `relay mcpExec` (also `relay mcp call`)

One-shot tool listing and invocation over the bridge, using a **project**
token — this is the door an agent or a script actually calls tools through,
as distinct from every command above, which is an *operator* configuring
relay itself.

```
relay mcpExec --token TOKEN --list [--schema]
relay mcpExec --token TOKEN --tool NAME [--args JSON] [--args-file FILE|-]
```

`relay mcp call ...` is the identical command under `main.go`'s other
spelling.

| Flag | Meaning |
|---|---|
| `--token` | Project token. Prefer setting `RELAY_PROJECT_TOKEN` in the environment instead (the legacy `RELAY_TOKEN` name is no longer accepted). |
| `--list` | List available tools. |
| `--schema` | With `--list`, emit full JSON including each tool's input schema — what a SKILL.md generator consumes. |
| `--tool` | Tool name to call. |
| `--args` | Tool arguments as a JSON string. |
| `--args-file` | Read arguments JSON from a file, or `-` for stdin — the shell-quoting-safe path for arguments containing quotes, apostrophes, or parentheses. |

An empty token is not automatically fatal: relay falls back to membership
auth (plan-broker-and-sessions.md §2 C3) when the calling process is a
verified descendant of a live project session's root — never to an
asserted working directory, which is never authenticated. Needs service:
yes (it dials the bridge socket). Prompts: no — this is an ordinary,
unfiltered tool call inside a grant that already exists, not an act that
widens one. Works over SSH: yes, when the SSH session is itself a member of
a live project session; otherwise it needs `--token`/`RELAY_PROJECT_TOKEN`.

Illustrative output, shaped like this machine's Hermes Files v3 profile
(`fs_*` tools) — a live project token is required and none was retrieved to
run this for real, since tokens are sealed and the CLI has no path to read
one back out:

```
$ relay mcpExec --token "$RELAY_PROJECT_TOKEN" --list
TOOL      DESCRIPTION
fs_read   Read a file within the granted directory
fs_write  Write a file within the granted directory
fs_grep   Search file contents within the granted directory
...

7 tools available
```

---

# Worked example: registering macMCP end to end

This machine already runs the full sequence below in production (see
[`RUNBOOK-vm-stack.md`](../../RUNBOOK-vm-stack.md)); the commands here
reconstruct the same spine using this document's own captured output where
the step is read-only, and the exact command line where it is not (without
re-running it).

**1. Register the MCP.**

```
relay mcp register --id macmcp --name "macMCP" \
  --command /Users/you/src/macMCP/.build/release/macmcp
```

This prompts — "Relay is trying to register the MCP "macMCP" (macmcp) that
runs /Users/you/.../macmcp." — because the caller is choosing what relay
will run. `--id macmcp` is explicit here on purpose, for the same reason
argued above: `macMCP` already slugifies to `macmcp`, so in this particular
case the default would have matched anyway, but naming it removes the
question.

**2. Create an access profile that reaches it**, from the Settings window
(kind: Access profile — `relay project create` and `relay project edit` make
and change a project from the CLI; the Settings window is the everyday way):

```
name             Hermes Mail
allowed_mcp_ids  macmcp
allowed_tools    mail_*
access           read
mail_accounts    Alice, Bob
mail_mailboxes   Archive, INBOX
```

**3. Enrol a remote client against it** — this is the CLI act, and it
prompts:

```
relay enrol create --client-id hermes --grant 477d9a17-da03-45eb-a433-764f93fe96fc
```

("create an enrolment for client "hermes" with access to
477d9a17-da03-45eb-a433-764f93fe96fc" is what the prompt names.)

**4. Mint whatever control-plane credential the consuming tool needs** —
not required for a remote mTLS client like this one (its identity is the
enrolment's certificate, not a bearer credential), but the same act for a
browser or HTTP consumer:

```
relay credential mint --name eve-view --class read --class configure
```

**5. Read back what was actually granted** — this is the real, captured output for the profile above:

```
$ relay grant --project 477d9a17-da03-45eb-a433-764f93fe96fc
ACCESS PROFILE  Hermes Mail  (id: 477d9a17-da03-45eb-a433-764f93fe96fc)
  macmcp         access=read   outbound=blocked  tools=mail_*
                 scope: mail_accounts = [
            "Alice",
            "Bob"
          ]
                 scope: mail_mailboxes = [
            "Archive",
            "INBOX"
          ]
  enrolments:
    hermes               cli-admin: off
```

**6. Confirm it in the audit log — ground truth, never the agent's own
account of what it could reach:**

```
$ relay audit --tail 5
TIME      OUTCOME  PROJECT                      MCP     TOOL      MS  CALLER                                DETAIL
15:49:55  pending  Hermes Files v3              fsmcp3  fs_grep   0   hermes-v3                             {"pattern":"api_key"}
15:49:55  ok       Hermes Files v3              fsmcp3  fs_grep   12  hermes-v3                             {"pattern":"api_key"}
15:49:55  denied   Hermes Files v3 (read-only)  -       fs_write  0   hermes-v3-ro                          access denied: no tool named 'fs_write' is available to this grant (granted: fsmcp3ro)
14:36:58  ok       -                            -       -         0   81d25ca0-23dd-45a0-ab75-f51925ec2df4  GET /api/projects  class=read  transport=tcp
14:36:58  denied   -                            -       -         0   81d25ca0-23dd-45a0-ab75-f51925ec2df4  POST /api/projects  class=configure  transport=tcp  class not granted
```

(This tail happens to show a different profile's traffic — `Hermes Files
v3` — because that is what most recently ran on this machine; the mechanism
is identical for `Hermes Mail`. Note the third row: `fs_write` was refused
before it ever reached the MCP, because the grant is read-only. That is
`denied`, not `tool_error` — the distinction that matters when reading this
table, covered under `relay audit` above.)

---

# What relay never prints

- **A credential's plaintext.** `relay credential mint` shows the token
  exactly once, in its own output, and it is not recoverable — only its
  SHA-256 is ever stored. `relay credential list` shows neither the
  plaintext nor the hash.
- **A login bootstrap code's plaintext**, likewise shown once by
  `relay login enrol` and never again.
- **A passkey's public key.** `relay login list` shows name, an abbreviated
  credential id, creation time, and sign count — never the key itself.
- **A project's or access profile's token, except on request.** `relay grant`
  shows the grant's *shape* — MCPs, mode, tools, real scope values —
  deliberately without ever reading or printing the sealed token. A project's
  bearer token reaches a terminal in two cases only: `relay project rotate-token`
  prints the new token once, and `relay project token` reveals the current one
  behind a presence prompt and records it in the audit log before it prints.
  The Settings window (Projects → Bearer Token) shows it behind the eye icon.
  An access profile's token is never printed.
- **An enrolment's private key**, after the moment `enrol create` writes its
  bundle to disk. The bundle directory is the only copy; losing it means
  revoking and re-enrolling. `enrol sign` never has one to withhold in the
  first place — the client generated its own key, and relay only ever sees
  the public half in the CSR.

None of this is enforced by convention — `Secret.MarshalJSON` refuses to
serialize a field that has not been through the sealing step, so a stray
`json.Marshal(settings)` or a debug log line hits a loud error before a
plaintext could reach any output at all. See
[`docs/sealed-config.md`](sealed-config.md#the-trap-the-field-list-exists-to-avoid).

---

# Troubleshooting

Keyed on the literal strings you will see.

### `relay is not running; ... requires the service.`

A command that mutates or shows configuration was run with the tray stopped
— that includes `relay grant` and every `list`, because the running tray is
the only reader of the configuration. Start Relay (open the app, or run it)
and retry. Only `relay audit` and `relay enrol ca-fingerprint`, which read
their own files, work with the tray stopped.

### `refused: this needs your confirmation on the Mac's screen, ...`

A gated command was run from a session relay's kernel-level check
determined cannot show a prompt on the console — an SSH session, almost
always. There is no queue: run the same command from a terminal inside the
logged-in desktop session, or use the Relay Settings window instead.

### `presence was refused`-shaped exit after a prompt appears

The login-password prompt was cancelled rather than answered. Nothing was
written; retry the command and enter the password, or don't — either way
`settings.json` is exactly as it was before you ran it.

### `error: --name is required` / `--client-id is required` / `--id is required` (etc.)

A required flag was omitted. Every subcommand's own `-h` output (shown for
each command above) lists exactly what it needs and what's optional.

### `no mcp found with id "..."` / `no service found with name "..."`

`mcp unregister`, `service unregister`, and `service restart` resolve
`--id`/`--name` against `settings.json` themselves, before ever dialing the
service, so this is reported without needing relay running for the lookup
itself (though the actual unregister/restart still needs the service to
carry it out).

### `"legacy-frontend-token" is reserved: relay deletes every credential under that name on start...`

Attempted `credential mint --name legacy-frontend-token` or
`credential revoke` against it. Relay deletes any record under that name on
every start; pick a different name.

### `mcpList has been removed. Use: relay mcpExec --token <TOKEN> --list`

An old script or muscle memory reached for `relay mcpList`. It no longer
exists; `relay mcpExec --list` (or `relay mcp call --list`) replaces it.

### A degraded sealed store: mutations refuse, reads keep working

If the keychain key relay expects is missing, mismatched, or unreadable,
relay still starts. Every read command keeps working in full — `relay
audit`, `relay grant`, every `list`, and the Settings window rendering
sealed fields as unavailable rather than failing to load. Every mutating
command refuses by name (the same "relay is not running"-shaped family of
messages, or a more specific one naming the sealed value it could not
reach) rather than writing around the problem. The only way out is the
tray's **Reset Sealed Store…** menu item or `relay sealed reset` — behind its own presence prompt,
naming exactly what it is about to destroy (every project and its token,
every control-plane credential, every enrolment and the CA that signed
them, every passkey) — which deletes `settings.json`, the CA files, and the
keychain item together, then re-initializes from nothing.

**Hand-editing `settings.json`.** A running tray watches the file. A valid edit
is imported through the tray's config queue, in order with other changes, and
takes effect without a restart. An invalid edit (unparseable, or a sealed value
that will not open) is refused: the tray logs why and keeps its current
settings. A change the tray makes at the same moment builds on your valid edit
rather than overwriting it. There is no import command.

The same reset is `relay sealed reset`, through the same presence prompt. There
is deliberately **no `--yes`, no `--force` flag, and no offline recovery code**
for this: a second door into the sealed store that skips the prompt is exactly
what the whole design spends its effort closing; see
[`docs/sealed-config.md`](sealed-config.md#break-glass-and-why-there-is-no-offline-recovery-code).

---

# Further reading

- [`docs/install-single-machine.md`](install-single-machine.md) — the
  task-ordered version of this document's worked example: start relay,
  register an MCP, create a project, call a tool, on one Mac.
- [`docs/install-remote-machine.md`](install-remote-machine.md) — the same
  for a second machine: which command runs where, the enrolment-request
  channel, and the operator-carried fallback.
- [`docs/sealed-config.md`](sealed-config.md) — why project tokens, the
  admin secret, OAuth bearers and the CA key are sealed at rest, why the
  rest of `settings.json` deliberately is not, and the break-glass recovery
  path referenced above.
- [`docs/presence-gate.md`](presence-gate.md) — the full mechanism behind
  every prompt in this document: which operations are gated and why, the
  nonce model, the SSH refusal, and the residual risks stated plainly
  (the prompt is impersonable by another local process; `proxy` remains
  ungated).
- [`docs/tokens.md`](tokens.md) — the canonical inventory of every
  credential kind in the relay ecosystem, including the five control-plane
  classes `relay credential mint --class` accepts and what each one
  actually reaches.
- [`docs/access-profiles.md`](access-profiles.md) — the operator's model
  for remote access profiles and enrolments, including the "the agent's own
  account of what it could reach is not evidence" lesson `relay audit`
  exists to settle.
- The ADR-017 implementation spec §6.4 — the normative table of every gated
  operation and the exact argument set its presence digest covers.
