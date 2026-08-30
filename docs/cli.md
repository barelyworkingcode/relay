# The `relay` command line

`relay` is one binary with two personalities. Run with no arguments, it is the
tray app — the thing that owns `settings.json`, holds the sealing key, and
answers presence prompts. Run with a subcommand, it is a CLI that either reads
`settings.json` directly, or asks the running tray to make a change on its
behalf.

That split is the one fact this whole document keeps coming back to, so it
comes first.

## The mental model: read vs. write

**Every read command works with the tray stopped.** `relay grant`,
`relay audit`, and every `list` subcommand (`credential list`, `enrol list`,
`login list`, `mcp list`, `service list`) open `settings.json` or the audit
log directly. They need no socket, no key, and no presence prompt.

**Every mutating command asks the running tray**, over relay's own
Unix-socket bridge (`admin_op`), and refuses by name if the tray is not
running. Nothing in this program writes `settings.json` from a CLI process
any more: the tray is the only process holding the keychain key that unseals
its sealed fields (project tokens, the admin secret, OAuth bearers, the CA
key — see [`docs/sealed-config.md`](sealed-config.md)), so it has to be the
only process writing them. Stop relay and try one:

```
$ relay credential mint --name test --class read
error: relay is not running; `relay credential mint` requires the service.
  relay is the sole broker of its own credentials: the secrets are sealed and
  only the tray holds the key (ADR-017 decision 2). Start Relay and retry.
  Read commands still work with relay stopped: `relay credential list`,
  `relay grant`, `relay audit`.
```

**A subset of the mutating commands also demand presence** — a real
login-password prompt, answered at the Mac's own screen, before the tray
commits anything. The rule (from [`docs/presence-gate.md`](presence-gate.md)):
*any operation that issues a credential, widens one, or chooses what runs.*
Concretely, every command in the table below marked **prompts: yes**.
`service restart` is the one mutating, brokered command that is *not*
gated — it restarts a process from a configuration that was already approved
when it was registered, and changes no settings.

Two consequences follow immediately, and both are covered in full below:

- The prompt names the exact act and its arguments, expires in 120 seconds,
  and can be spent exactly once.
- Over SSH, a gated command refuses instantly rather than queuing, because
  relay can tell — from the kernel, not from anything the caller sends — that
  the session it is running in cannot show a prompt on the console.

### Quick reference

| Command | Needs service | Prompts | Works over SSH |
|---|---|---|---|
| `relay grant` | no | no | yes |
| `relay audit` | no | no | yes |
| `relay credential list` | no | no | yes |
| `relay credential mint` | yes | **yes** | no |
| `relay credential revoke` | yes | **yes** | no |
| `relay enrol list` | no | no | yes |
| `relay enrol create` | yes | **yes** | no |
| `relay enrol sign` | yes | **yes** | no |
| `relay enrol update` | yes | **yes** | no |
| `relay enrol revoke` | yes | **yes** | no |
| `relay login list` | no | no | yes |
| `relay login enrol` | yes | **yes** | no |
| `relay login revoke` | yes | **yes** | no |
| `relay mcp list` | no | no | yes |
| `relay mcp register` | yes | **yes** | no |
| `relay mcp unregister` | yes | **yes** | no |
| `relay service list` | no | no | yes |
| `relay service register` | yes | **yes** | no |
| `relay service unregister` | yes | **yes** | no |
| `relay service restart` | yes | no | yes |
| `relay mcpExec` / `relay mcp call` | yes (dials the bridge) | no | yes |
| `relay mcp --token TOKEN` (stdio server) | yes | no | yes |

"Works over SSH" here means "does not refuse *because it is gated*." A
mutating command still needs the service reachable either way; only the
presence-gated ones add the console-session check.

## The global `--config-dir` flag

Every subcommand accepts `--config-dir DIR` (or `--config-dir=DIR`) *before*
the subcommand name — it has to be stripped out ahead of every subcommand's
own flag parsing, since each owns its own `flag.FlagSet`:

```
relay --config-dir /path/to/alt-config grant
```

This is the only flag that is not owned by a subcommand.

## Privileged commands prompt — and here is what that looks like

For any command marked **prompts: yes** above, the tray raises a real
`LocalAuthentication` login-password prompt before it touches
`settings.json`. This machine has no Touch ID and no Secure Enclave, so
"presence" here is a typed password, not a fingerprint or a click. A verified
example, captured on this machine's console for the request

```
relay mcp register --id fsmcp3 --name "fsMCP v3 (testfolder)" --command /Users/admin/.local/bin/fsmcp
```

— note this asks to re-point the already-registered id `fsmcp3` at a
*different* binary, dropping its `--root` argument entirely:

> **Relay** — "Relay is trying to register the MCP "fsMCP v3 (testfolder)"
> (fsmcp3) that runs /Users/admin/.local/bin/fsmcp. Enter the password for
> the user "Managed via Tart" to allow this."

**The prompt describes the change being requested, not the record as it
stands.** `fsmcp3`'s actual, stored command — unchanged, per `relay mcp
list` — is `/Users/admin/.local/bin/fsmcp3 --root
/Users/admin/source/barelyworkingcode/testfolder`. The command the dialog
names, `/Users/admin/.local/bin/fsmcp` with no `--root` at all, is what the
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
  --command /Users/admin/.local/bin/fsmcp3 --args --root --args /Users/admin/source/barelyworkingcode/testfolder
```

That prompt would read "...that runs /Users/admin/.local/bin/fsmcp3
--root /Users/admin/source/barelyworkingcode/testfolder" — recognizably the
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
| `mcp register` | `register the MCP "NAME" (id) that runs COMMAND` (or `at URL` for HTTP) |
| `mcp unregister` | `unregister the MCP "ID"` |
| `service register` | `register the service "NAME" (id) that runs COMMAND` |
| `service unregister` | `unregister the service "ID"` |

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
Reads are entirely unaffected: `relay audit`, `relay grant`, and every `list`
subcommand read a file directly and never touch the gate.

---

# Command reference

## `relay grant`

Prints a project's or access profile's grant **as authored** — every MCP it
reaches, its access mode, its outbound (external-network) permission, its
tool pattern, and the real resource-scope values, never redacted. It reads
`settings.json` directly, so it works with the tray stopped, and it is
deliberately blind to `disclose`: that field governs what a *client* sees,
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

This is the grant as stored. Whether each MCP still declares these scope
fields is a live question — a value relay cannot place in an MCP's current
schema is refused at call time and shown by `relay audit --authority`.
```

A scope value that reaches an entire filesystem root (an `allowed_dirs` of
`"/"`) or a whole home directory is called out in the table with a line in
`** LOUD CAPS **`, because those are exactly the two shapes `disclose` would
otherwise let a client under-report (see `docs/tokens.md` and
`scope_breadth.go`). Nothing on this machine currently triggers that warning
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
gates. Reads the JSONL file directly (`readAuditTail`), so, like `relay
grant`, it works with the tray stopped and needs nothing sealed.

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
/Users/admin/Library/Application Support/relay/logs/audit/toolcalls.jsonl
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

## `relay credential`

Control-plane API credentials — the bearer that authenticates a caller to
relay's control-plane HTTP API (the frontend socket, and `RELAY_API_LISTEN`
if bound). See [`docs/tokens.md`](tokens.md#control-plane-credentials-adr-015)
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

`legacy-frontend-token` is reserved: the frontend-token migration rewrites
its hash on every relay start, so both `mint` and `revoke` refuse that name.

### `credential list`

Real capture from this machine:

```
$ relay credential list
ID                                    NAME                   CLASSES               CREATED               EXPIRES
bacf762f-6eb1-425b-bd20-20756a97b6c5  legacy-frontend-token  read,configure,proxy  2026-08-28T21:34:57Z  never
```

`--include-expired` also shows expired records — otherwise they're hidden,
awaiting the next mint's lazy reap. `EXPIRES` prints `never` for a credential
minted with no `--ttl`, and `<timestamp> (expired)` for one whose time has
passed. Needs service: no. Prompts: no. Works over SSH: yes.

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
[`docs/decisions/009-remote-projects.md`](decisions/009-remote-projects.md)
and [`docs/decisions/011-resource-scope.md`](decisions/011-resource-scope.md)
for the remote model this feeds). There is no self-service path and no
bootstrap token by design — every enrolment is a host-side operator act.

Two ways to get there. `enrol create` generates the client's private key on
this host and emits a bundle containing it — the legacy path, kept working
but deprecated in its own output. `enrol sign` takes a certificate signing
request the client generated on its own machine (`relayremote enrol`) and
returns only certificates: the private key never leaves the client, and
never exists in this process at all. Prefer `sign`.

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
  bundle:      /Users/admin/Library/Application Support/relay/enrolments/hermes
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
  certificate: /Users/admin/Library/Application Support/relay/enrolments/hermes
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
CLIENT ID        PROFILES                              CALLS/WINDOW  BYTES/WINDOW  CREATED               FINGERPRINT
hermes           477d9a17-da03-45eb-a433-764f93fe96fc  120/3600s     67108864      2026-08-26T00:02:54Z  sha256:a44f923fa5f84970facc53f83d16c72cc2123dd8104703162a59f761fbb5dc31
hermes-files     59c19c5b-b248-493c-a094-4397a56c8693  120/3600s     67108864      2026-08-26T14:32:16Z  sha256:9820e514f38b35d2b1af8687260125853b9e7577b3e37036224ad438f1379bb1
hermes-files-ro  aaaabf48-95c9-4d72-97b8-7060138930f1  120/3600s     67108864      2026-08-26T14:32:16Z  sha256:d1846a1b393e738dfe7043a5299cd1f67a9c203bdb01d27cb070c19228f4c6ca
hermes-v3        b0000000-0000-4000-8000-000000000001  120/3600s     67108864      2026-08-26T18:55:54Z  sha256:79129197d148052d196e1d4ad2fbc4b4a64d770024601043a7943f5b9b5fcaa0
hermes-v3-ro     b0000000-0000-4000-8000-000000000002  120/3600s     67108864      2026-08-26T19:08:37Z  sha256:46c0903492ef4d091cfc704d92fa079cd882d4c53ed750a513a1001853adbff3
```

The fingerprint is printed in full (all 64 hex characters), deliberately: an
enrolment's audit history stays legible after it's revoked, and a shortened
listing is the obvious place someone starts copying a truncated form from.
Needs service: no. Prompts: no. Works over SSH: yes.

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

## `relay login`

Host-side anchor for interactive passkey login (ADR-016). There is no
self-registration: an operator mints a single-use, two-minute bootstrap code
and redeems it at the login page to register a passkey. The bootstrap code
is **not** a control-plane credential and authorises exactly one thing.

```
relay login enrol
relay login list
relay login revoke --id ID
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
  open http://localhost:<RELAY_API_LISTEN port>/relay/login and enter it to register a passkey
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
show it. Needs service: no. Prompts: no. Works over SSH: yes.

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

## `relay mcp`

External MCP registration: what relay connects to or spawns, over stdio or
HTTP.

```
relay mcp register --name NAME [--id ID] --command CMD [--args ARG...] [--env K=V...]
                    [--transport stdio|http] [--url URL] [--tcc-services LIST]
relay mcp unregister --id ID | --name NAME
relay mcp list
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
the record — you then finish authentication from the Settings window's
**Authenticate** button; there is no CLI door for that step, because OAuth
here means opening a real browser and running a local callback listener.

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
macmcp    macMCP                            stdio      /Users/admin/source/barelyworkingcode/macMCP/.build/release/macmcp
fsmcp     fsMCP                             stdio      /Users/admin/.local/bin/fsmcp
fsmcp3    fsMCP v3 (testfolder)             stdio      /Users/admin/.local/bin/fsmcp3 --root /Users/admin/source/barelyworkingcode/testfolder
fsmcp3ro  fsMCP v3 (testfolder, read-only)  stdio      /Users/admin/.local/bin/fsmcp3 --root /Users/admin/source/barelyworkingcode/testfolder --read-only
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
matches on both). Needs service: yes. Prompts: yes. Works over SSH: no.

### `mcp list`

Real capture, this machine's four registered MCPs:

```
$ relay mcp list
ID        NAME                              TRANSPORT  ENDPOINT
macmcp    macMCP                            stdio      /Users/admin/source/barelyworkingcode/macMCP/.build/release/macmcp
fsmcp     fsMCP                             stdio      /Users/admin/.local/bin/fsmcp
fsmcp3    fsMCP v3 (testfolder)             stdio      /Users/admin/.local/bin/fsmcp3 --root /Users/admin/source/barelyworkingcode/testfolder
fsmcp3ro  fsMCP v3 (testfolder, read-only)  stdio      /Users/admin/.local/bin/fsmcp3 --root /Users/admin/source/barelyworkingcode/testfolder --read-only
```

Needs service: no. Prompts: no. Works over SSH: yes.

## `relay service`

Background service self-registration — the mechanism relayLLM,
relayScheduler and similar enhanced services use to tell relay what to run
and how to reach it.

```
relay service register --name NAME [--id ID] --command CMD [--args ARG...]
                        [--env K=V...] [--workdir DIR] [--url URL]
                        [--autostart[=true|false]] [--no-frontend-creds]
relay service unregister --id ID | --name NAME
relay service restart --id ID | --name NAME
relay service list
```

### `service register`

```
$ relay service register -h
Usage of service register:
  -args value
    	command arguments (repeatable)
  -autostart
    	start automatically
  -command string
    	command to run (required)
  -env value
    	environment KEY=VALUE (repeatable)
  -id string
    	record id (default: slugified --name)
  -name string
    	display name (required)
  -no-frontend-creds
    	do not inject relay front-door creds (RELAY_FRONTEND_SOCKET/TOKEN); set for backends that never dial the front door, so the bearer can't leak into spawned shells
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

**`--no-frontend-creds` is the one control on the frontend socket
credential, and it survives a re-register.** It is one-directional: there is
no `--frontend-creds` flag to turn injection back on from the CLI. Passing
`--no-frontend-creds` once on a service that never dials relay's front door
opts it out of `RELAY_FRONTEND_SOCKET`/`RELAY_FRONTEND_TOKEN` injection, and
every later `service register` call that doesn't repeat the flag leaves that
opt-out in place — it is not something a later register accidentally
resets.

### `service unregister`

```
$ relay service unregister -h
Usage of service unregister:
  -id string
    	service ID
  -name string
    	service display name
```

Needs service: yes. Prompts: yes. Works over SSH: no.

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
services; nothing is currently registered as a background service.) Needs
service: no. Prompts: no. Works over SSH: yes.

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
| `--token` | Project token. Prefer setting `RELAY_PROJECT_TOKEN` in the environment instead (the legacy name `RELAY_TOKEN` is still accepted, for one release). |
| `--list` | List available tools. |
| `--schema` | With `--list`, emit full JSON including each tool's input schema — what a SKILL.md generator consumes. |
| `--tool` | Tool name to call. |
| `--args` | Tool arguments as a JSON string. |
| `--args-file` | Read arguments JSON from a file, or `-` for stdin — the shell-quoting-safe path for arguments containing quotes, apostrophes, or parentheses. |

An empty token is not automatically fatal: relay falls back to directory
auth (`allow_cwd_auth`) for any project that opted in from the calling
directory. Needs service: yes (it dials the bridge socket). Prompts: no —
this is an ordinary, unfiltered tool call inside a grant that already
exists, not an act that widens one. Works over SSH: yes.

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
  --command /Users/admin/source/barelyworkingcode/macMCP/.build/release/macmcp
```

This prompts — "Relay is trying to register the MCP "macMCP" (macmcp) that
runs /Users/admin/.../macmcp." — because the caller is choosing what relay
will run. `--id macmcp` is explicit here on purpose, for the same reason
argued above: `macMCP` already slugifies to `macmcp`, so in this particular
case the default would have matched anyway, but naming it removes the
question.

**2. Create an access profile that reaches it**, from the Settings window
(kind: Access profile — there is no CLI door to create a project or profile
itself; `relay grant` and `relay enrol` only read and enrol against one that
already exists):

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

**5. Read back what was actually granted**, with the tray stopped if you
like — this is the real, captured output for the profile above:

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
- **A project's or access profile's token.** `relay grant` shows the grant's
  *shape* — MCPs, mode, tools, real scope values — deliberately without ever
  reading or printing the sealed token. If you need a project's actual
  bearer token, the legitimate place to get it is the Settings window
  (Projects → Bearer Token), which is a different reveal path from anything
  the CLI does.
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

A mutating command was run with the tray stopped. Start Relay (open the app,
or run it) and retry. Every read command — `relay grant`, `relay audit`, and
every `list` — is unaffected and needs no service at all.

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

### `"legacy-frontend-token" is reserved for the RELAY_FRONTEND_TOKEN migration...`

Attempted `credential mint --name legacy-frontend-token` or
`credential revoke` against it. That record is rewritten by relay itself on
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
tray's **Reset Sealed Store…** menu item — behind its own presence prompt,
naming exactly what it is about to destroy (every project and its token,
every control-plane credential, every enrolment and the CA that signed
them, every passkey) — which deletes `settings.json`, the CA files, and the
keychain item together, then re-initializes from nothing.

There is deliberately **no CLI equivalent, no flag, and no offline recovery
code** for this. A second door into the sealed store is exactly what the
whole design spends its effort closing on the first one; see
[`docs/sealed-config.md`](sealed-config.md#break-glass-and-why-there-is-no-offline-recovery-code).

---

# Further reading

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
- [`docs/decisions/017-implementation-spec.md`](decisions/017-implementation-spec.md)
  §6.4 — the normative table of every gated operation and the exact
  argument set its presence digest covers.
