# Registering a new remote machine

A second machine (a VM, a Linux box, another laptop) reaching relay's tools
over the network. The client generates its own private key and asks to be
enrolled. You approve at the Mac, and the client collects its certificate.

Start from [install-single-machine.md](install-single-machine.md). This guide
assumes relay is running, at least one MCP is registered, and you know what a
presence prompt is.

There is one way to enrol a machine that can reach the Mac over the network:
**Allow Enrolment on the Mac, then `relayremote register` on the machine.**
The same window has a command-line form for scripts and SSH sessions. A
machine with no network path to the Mac has one fallback, where you carry the
signing request and the certificate by hand.

- **[The path](#the-path-allow-enrolment-then-register).** Tray > Allow
  Enrolment… > an access profile, then `relayremote register` on the machine.
  Compare one six-character code and approve.
- **[The power doors](#the-power-doors).** `relay enrol open --profile`,
  `relay enrol requests`, `relay enrol approve`, `relay enrol close`.
- **[The operator-carried fallback](#the-operator-carried-fallback).**
  `relayremote enrol`, `relay enrol sign --grant`, `relayremote install`. Only
  for a machine with no network path to the Mac.

---

## Which machine runs what

| Binary | Runs on | What it is |
|---|---|---|
| `relay` | **the Mac only** | the host. Signs certificates, approves requests, owns the grant. |
| `relayremote` | **the client only** | the remote end. Generates its key, asks to be enrolled, calls tools. |

`relay enrol` and `relayremote enrol` are **different commands that share a
word**:

| Command | Machine | Does |
|---|---|---|
| `relayremote register` | client | the whole of the path: key, request, code, collection, into a **named registration**. |
| `relayremote registrations` / `use` / `unregister` | client | the local registration store. `unregister` **revokes nothing.** |
| `relayremote enrol` | client | fallback: generates `client.key` and `client.csr`. Never touches the network. |
| `relayremote install` | client | fallback: files a certificate an operator carried here. Never touches the network. |
| `relay enrol open` / `close` | host | opens or closes the enrolment window for one access profile. |
| `relay enrol requests` | host | lists the request a remote has lodged, with its comparison code. |
| `relay enrol approve` | host | signs a lodged request into the window's profile. Prompts. |
| `relay enrol refuse` | host | declines one. Does **not** prompt. |
| `relay enrol sign` | host | fallback: signs a signing request you carried to the Mac. Prompts. |
| `relay enrol list` / `revoke` / `update` | host | the enrolment records themselves. |

Every command block below says which machine it runs on.

---

## Before you start

**On the Mac:**

1. Relay running, and you at its keyboard. `relay enrol open`,
   `relay enrol approve` and `relay enrol sign` raise a login-password prompt
   and refuse outright from a session that cannot show one.
2. **An access profile** the client will be granted. A profile is not a
   project: it has no directory, no shell, no models, just a capability
   grant. Create it in Settings > **Projects > + New > Access profile**;
   [`access-profiles.md`](access-profiles.md#creating-a-confined-client) is
   the reference for what each field does and why an empty field means "none"
   rather than "all". You need its id, which `relay grant` prints.
3. **Auditing on.** The remote listener refuses to start while
   `audit.enabled` is false. A remote grant is justified by the calls it
   records, so serving remote traffic unrecorded is not a degraded mode. It is
   on by default.

**On the client:** the `relayremote` binary. Prebuilt binaries for darwin,
linux and windows are in relayRemote's `dist/`, or build from source with
`./build.sh`.

### Turn the tool-plane listener on

**On the Mac.** No `remote` block in `settings.json` means **no socket at
all**, not a closed one and not one that refuses. An install that never
enrolled a client has no network surface, deliberately.

Settings > **Remote Clients** > *Accept remote clients on an mTLS listener* >
Save. Confirm:

```
$ lsof -nP -iTCP:9910 -sTCP:LISTEN
COMMAND   PID  USER   FD   TYPE             DEVICE SIZE/OFF NODE NAME
relay   36793 admin    8u  IPv4 0x24fb5ff6e193d9a4      0t0  TCP 127.0.0.1:9910 (LISTEN)
```

The default binds **loopback**, which is right for a same-machine test and
wrong for a real VM. Set the listen address to something the client can reach
before you expect this to work from another host. Settings warns when you bind
beyond loopback, and it is worth reading: every machine that can reach that
address can attempt a TLS handshake. Only a certificate relay signed gets past
it. An unenrolled one is closed before a single request is read. A tunnel is
a network path, never an identity.

That listener is the only setting you turn on. The **enrolment port** has no
switch. It does not exist until you open a window, and it is gone when the
window closes.

---

## The path: Allow Enrolment, then register

### 1. Open a window, on the Mac

Tray > **Allow Enrolment…** > pick the access profile this machine will get.
Relay asks for your login password, then opens a five-minute window for that
one profile. The tray shows the window while it is open, and **Cancel
Enrolment** closes it early.

You choose the profile here, and only here. The machine cannot ask for a
profile, and it cannot change the one you chose.

Confirm the socket exists, and only now:

```
$ lsof -nP -iTCP:9911 -sTCP:LISTEN
COMMAND   PID  USER   FD   TYPE             DEVICE SIZE/OFF NODE NAME
relay   36793 admin   13u  IPv4 0xfc385445e9cfd53a      0t0  TCP 127.0.0.1:9911 (LISTEN)
```

The default address is `127.0.0.1:9911`. To change it, set
`remote.enrolment_listen` in `settings.json`. The window binds nothing when
the remote listener is off or auditing is off.

What a window is, and is not:

- **It takes one request.** The first request a peer lodges closes the
  window. The timer and **Cancel Enrolment** close it too. A restart of
  relay closes it, because the window lives in memory only.
- **It is plain TCP, with no TLS, on purpose.** Nothing on this wire is a
  secret in either direction: inbound is a signing request (public by
  construction), outbound is a certificate and a CA certificate (both
  public). TLS here would read as a security property it cannot provide,
  because the client has no CA to verify a handshake against yet. The real
  control is the code comparison in step 3.
- **Lodging raises no *prompt*, ever.** No code path from an unauthenticated
  lodge reaches the presence gate, so nobody on the network can make your Mac
  ask you for a password. You initiate every approval, and *that* is what
  prompts.
- **A peer can lodge first.** While the window is open, any machine that can
  reach the port may lodge before yours does. That closes the window and
  costs you a retry. It does not enrol the peer: you compare the code, and
  the code will not match. Refuse its request, open a window again, and
  register again.
- **Lodging may raise a *notification*.** A new request can draw one
  dismissible tray banner: at most one a minute and six in a rolling hour,
  carrying a count and nothing the requesting machine supplied. A banner is
  not a sign that anything has gone wrong. It means a machine asked, and
  nothing is granted until you approve. Do not build a habit on it arriving:
  macOS can suppress it. The tray's `Pending enrolment requests: N` line and
  `relay enrol requests` are the surfaces that are always there.

### 2. Register, on the client

**On the client** (one command, run from a terminal):

```
relayremote register "Hermes Mail" --host 10.0.0.2
```

It generates a P-256 key that never leaves this machine, then waits for the
window. Start it before or after you open the window. While the port is
closed it prints one line and retries every two seconds, up to `--wait`:

```
Waiting for relay at 10.0.0.2 to open enrolment… (On the Mac: tray → Allow Enrolment → this machine's access profile)
```

When the port opens, it lodges the signing request together with a commitment
to a random value of its own, receives relay's CA certificate and a random
value of relay's, and prints a six-character comparison code:

```
  code: FF9GTV · waiting for approval…

On the Mac: Settings → Remote Clients → Pending requests.
The same six characters must be shown beside this request. If they differ, Refuse there —
something is on the network path between the two machines.
```

- **`--host` takes a bare host and nothing else.** Ports are `--port` (tool
  plane, 9910) and `--enrol-port` (enrolment, 9911). There is no discovery
  of any kind, and there will not be: a machine that announced itself is a
  machine an attacker can announce.
- **The positional argument is a display name.** It becomes the
  registration's local name (`"Hermes Mail"` becomes `hermes-mail`), which is
  what `--as` takes later.
- **The client never names a profile.** The window already did.
- **`--wait DURATION`** is how long to wait for the window, and then how long
  to wait for approval. The default is 10 minutes each.
- **One connection.** The lodge and every poll after it use the one connection
  the dial returned. If it drops before approval, the request goes with it:
  open a window again and run `register` again. If another machine's request
  took the window first, `register` says so and exits 8.

If the port never opens, `register` says which of two things happened and
exits 8: the host answered and nothing listened (the window is not open, or
the host or port is wrong), or the last attempt timed out (check the address
and any firewall).

### 3. Compare the code, then approve, on the Mac

The tray carries `Pending enrolment requests: N` while a request is waiting,
and it opens Settings > Remote Clients > **Pending requests**. Read the six
characters off the client's screen, read them off the Mac's, and approve only
if they are identical. Choose **Approve…**. There is no profile to pick: the
window named one. Relay asks for your login password. It is the same presence
gate `relay enrol sign` uses, and the digest binds the request's own key.

The code is not a hash of two public keys an attacker could grind against. It
is a commitment exchange: neither side can compute it while it still has
freedom to choose an input. It binds relay's CA and the requesting key, so a
machine in the middle changes it whichever it substitutes. A mismatch is the
signal this exchange exists to produce. **Refuse** and treat the network path
as suspect.

### 4. The client confirms, and reports what it got

The waiting `register` collects the certificate, checks that the CA it was
handed is the one the code was computed over, and asks you the one question
the ceremony exists for:

```
the Mac showed FF9GTV. Did it match? [y/N] y
registered "Hermes Mail" as hermes-mail
  relay:        10.0.0.2:9910
  certificate:  sha256:2cc63f1c222d095ecbcbc2ada28faf259adb44d9011a80548c6072587db21b40
  code matched: FF9GTV

  access:       Hermes Mail Inbox  (477d9a17-da03-45eb-a433-764f93fe96fc)
                5 tools — mail_get_email, mail_get_emails, mail_list_accounts, mail_list_mailboxes, mail_search

  use it with:  relayremote call --tool NAME --args '{"…":"…"}'
                (this is the only registration, so --as is not needed)
```

(Values substituted: the host, the code and the profile.) **Anything but a
typed `y` writes nothing** and leaves the key where it is. There is no flag,
environment variable or `--yes` that creates another answer. `register` with
a stdin that is not a terminal refuses before it generates a key. A machine
with no terminal uses [the fallback](#the-operator-carried-fallback).

### 5. The registration store on the client

`register` files everything into a **named registration**, so the client does
not need `--bundle` and `--addr` on every call:

```
$ relayremote registrations
  NAME         CLIENT ID    HOST            PROFILES                              DEFAULT
  hermes-cal   hermes-cal   127.0.0.1:9910  b0000000-0000-4000-8000-000000000001
* hermes-mail  hermes-mail  127.0.0.1:9910  477d9a17-da03-45eb-a433-764f93fe96fc  yes

Separate registrations mean separate audit trails and separate revocation on the host.
They are NOT isolation: anything running as this user can read every client.key above.
```

Register again with a different name for a second keypair, certificate and
enrolment. Each enrolment reaches one access profile, so a machine that needs
two surfaces holds two registrations.

```
relayremote --as hermes-cal list      # act as one, for one call
relayremote use hermes-cal            # move the * to it
relayremote unregister hermes-cal     # delete it HERE. This revokes nothing.
```

With several registrations and no selector, `relayremote` refuses rather than
guessing. `unregister` is local only: to cut access, run
`relay enrol revoke --client-id hermes-cal` on the Mac. The full precedence
order and the store's layout are in
[relayRemote's `README.md`](../../relayRemote/README.md#registrations-naming-an-identity-on-this-machine).

---

## The power doors

The tray item is one door onto a window. The same window opens from a
terminal on the Mac's own console, which is how a script or a test drives it.

**On the Mac**, open a window for one access profile:

```
relay enrol open --profile 477d9a17-da03-45eb-a433-764f93fe96fc
```

This prompts (*"open a five-minute window for one remote machine to request
enrolment into the access profile …"*) and refuses over SSH. It refuses
before the prompt for an id that is not an access profile, with the remote
listener off, and with auditing off. `--json` prints the window with its
profile and expiry. Opening another window replaces the first.

Then, once the client has lodged:

```
relay enrol requests
relay enrol approve --id REQ_ID --client-id hermes-mail
relay enrol refuse --id REQ_ID
relay enrol close
```

- `relay enrol requests` lists the pending request with its `SAS` column
  (the code to compare), its `PROFILE` and its key hash. `(waiting)` means the
  machine has not finished its half of the exchange, and approve is refused.
  `FAILED` means it finished it wrongly, and relay refuses to sign it from
  every door.
- `relay enrol approve` takes **no profile**. The certificate is issued for
  the window's profile, which the pending request carries. `--client-id` names
  the enrolment, must be unique, and is what appears in `relay enrol list` and
  in every audit row this client writes. The budget flags (`--window-seconds`,
  `--max-calls` and the others) are in [`cli.md`](cli.md#enrol-approve).
  Approve prompts, and refuses over SSH: use the Mac's screen.
- `relay enrol refuse` declines without a prompt.
- `relay enrol close` closes the window. It is ungated, because closing
  narrows what is reachable. A request already lodged stays in the table
  until it is approved, refused or expires (15 minutes).

On success:

```
approved enrolment request "req_b2bde2c8de5abc2a8eec7a653b64c1b" as "hermes-mail"
  fingerprint: sha256:9d1f529024017d974df5f1e71a94040b39a81f22f0711437d2dd647c83021f34
  profiles:    477d9a17-da03-45eb-a433-764f93fe96fc
  the certificate is delivered to the client on its next poll; nothing further to do on this host
```

---

## The operator-carried fallback

**Use this only for a machine with no network path to the Mac**, or a client
with no terminal to compare a code in. It needs no window, no listener and
nothing to compare. You carry the signing request to the Mac and the
certificate back, and copying `ca.crt` here by hand **is** the trust
decision.

The private key is generated **on this machine** and never leaves it. What
crosses to the Mac is a certificate signing request (public material only),
and what comes back is a certificate over it. Three commands, two run on the
client and one on the Mac.

**1. On the client:** generate the key and the signing request. This never
touches the network:

```
$ relayremote enrol --bundle ~/relay-bundles/vm-agent --client-id vm-agent
key: /home/you/relay-bundles/vm-agent/client.key
csr: /home/you/relay-bundles/vm-agent/client.csr

next, on the relay host (its own screen — enrolment asks for confirmation there):
  relay enrol sign --client-id vm-agent --csr client.csr --grant PROFILE_ID --out ./signed

then, back on this machine:
  relayremote install --bundle /home/you/relay-bundles/vm-agent --from ./signed
```

Copy `client.csr` to the Mac (USB stick, `scp`, paste, anything). It is public
by construction and proves possession of a key that never leaves the client.

**2. On the Mac,** at its own screen, sign it for one profile:

```
relay enrol sign --client-id vm-agent --csr client.csr \
  --grant 477d9a17-da03-45eb-a433-764f93fe96fc --out ./signed
```

This prompts (*"sign a certificate for client "vm-agent" with access to
477d9a17-…"*). `--grant` takes one profile: two `--grant` flags issue
nothing, and an enrolment reaches one profile. Nothing in `./signed` is a
secret, only `client.crt` and `ca.crt`.

**3. On the client:** file what came back:

```
$ relayremote install --bundle ~/relay-bundles/vm-agent --from ./signed
installed certificate for "vm-agent"
  fingerprint: sha256:…
```

`install` verifies before it writes anything: the certificate pairs with the
key already in the bundle, it chains to the supplied CA, and its public key
hashes to the same value the local key does. A refusal leaves the bundle as
it was.

The relay host also needs the tool-plane listener, as above.

> `relay enrol create` is an older command that generates the client's key
> **on the Mac** and hands you a bundle containing it. It is deprecated in
> its own output. Prefer `sign`: a key that never crosses a wire cannot be
> intercepted on one.

---

## Check that it worked

**On the client.** A machine enrolled with `register` needs no flags at all — the
registration carries the address and the profile:

```
$ relayremote list
MAIL (5)
  mail_get_email       Get full email by message ID, including the list of attachments (name, part_path, size, m…
  mail_get_emails      Get the most recent emails (newest first) from matching mailboxes across accounts. Return…
  mail_list_accounts   List all mail accounts configured in Mail.app Scope: Mail accounts this client may read f…
  mail_list_mailboxes  List mailboxes. Groups by account when no account specified. Includes Mail's app-level ma…
  mail_search          Search emails by subject and sender, optionally recipients and body (case-insensitive). S…

5 tools in 1 category · relay 127.0.0.1:9910 · project 477d9a17-da03-45eb-a433-764f93fe96fc
```

With more than one registration, name it: `relayremote --as hermes-mail list`.
From the fallback, name the directory instead:
`relayremote list --bundle ~/relay-bundles/vm-agent --addr 10.0.0.2:9910`.

Then call one:

```
$ relayremote call --tool mail_list_accounts --args '{}'
[
  "Alice",
  "Bob"
]
```

**Now go and check on the Mac, because the client's own account of what it
could reach is not evidence:**

```
$ relay audit --tail 2 --kind remote
TIME      OUTCOME  PROJECT      MCP     TOOL                MS   CALLER       DETAIL
07:09:33  pending  Hermes Mail  macmcp  mail_list_accounts  0    hermes-mail  {}
07:09:33  ok       Hermes Mail  macmcp  mail_list_accounts  149  hermes-mail  {}
```

(The blocks above were captured on a loopback host against the
registration made with `register`. CALLER carries the enrolment's `--client-id`,
resolved from the peer certificate — never from anything the client sent, and
never from the registration's local name, which may differ.)

Two rows, not one: for a remote caller the audit log is fail-closed. An
`intent` record is written and flushed **before** the MCP runs, a `completion`
record with the same id follows, and a call whose intent cannot be recorded is
refused outright.

For a bundle, setting the environment saves repeating the flags:

```
export RELAY_REMOTE_BUNDLE=~/relay-bundles/vm-agent
export RELAY_REMOTE_ADDR=10.0.0.2:9910
relayremote list
```

`RELAY_REMOTE_REGISTRATION=NAME` is the equivalent for a registration, and it
is consulted *before* `RELAY_REMOTE_BUNDLE` — it is the more specific
variable, and someone who sets it means it.

An enrolment reaches one access profile, so `--project ID` (or
`RELAY_REMOTE_PROJECT`) is optional, and a registration records the profile it
was granted. A legacy record that names several profiles is refused on every
call until `relay enrol update --grant` narrows it to one.

---

## Giving the machine configuration authority, temporarily

Normally only you edit a profile. The `cli_admin` bit lets one enrolment's own
certificate reach a second, small door on the remote listener and narrow **its
own** grant — useful while a client is setting itself up and deciding it needs
less than you gave it.

**On the Mac**, turn it on:

```
relay enrol update --client-id vm-agent --cli-admin
```

Reconstructed — this one is behind the prompt:

```
updated enrolment "vm-agent"
  cli-admin:   off -> ON
               this certificate may now narrow its own access profiles over the
               remote listener. It still cannot register a command, mint a
               credential, or touch any other enrolment. Turn it off when done:
               relay enrol update --client-id vm-agent --cli-admin=false
```

With the bit on, that certificate may ask its own posture back
(`DescribeGrant`) and replace its own MCP list, tool patterns, access mode and
external-access flag with a **strictly narrower** set (`NarrowGrant`). It can
never widen anything on any axis, and never touch another enrolment's profile.
It is a sandbox on a sandbox. **From the client machine itself:**

```
relayremote grant describe
relayremote grant narrow --read-only macmcp
```

(both read `RELAY_REMOTE_BUNDLE`/`RELAY_REMOTE_ADDR` from the environment, set
above). `describe` prints the same posture `relay grant` would show you.
`narrow` takes at least one of `--mcp-ids`, `--tools`, `--read-only`,
`--no-external`, each narrowing exactly one axis; `--read-only` and
`--no-external` are bare comma-separated MCP-id sets with no value syntax for
anything but `read` / `false`, so a widening on either axis cannot be sent,
not merely refused. Narrowing has no undo from this program — relay refuses
anything wider than what is already stored, and restoring access is an
operator act on the host: `relay grant`, or the Projects tab.

Take it back the moment the setup is done:

```
relay enrol update --client-id vm-agent --cli-admin=false
```

Both directions prompt — a host process that could flip it back on silently
would make turning it off meaningless. It takes effect on the client's next
request; there is no reconnect to force.

`relay grant --project <profile>` marks a `cli_admin` enrolment loudly beside
the profile it reaches, and `relay audit --grep cli_admin` finds both the
toggle and every narrowing a client made of its own grant. The detail is in
[`access-profiles.md`](access-profiles.md#letting-the-client-narrow-its-own-grant-cli-admin).

---

## The client's private key is yours to look after

`client.key` in the bundle directory **is** this client's identity to relay.
There is no bearer token anywhere on this path and no password over it —
possession of that file is the whole credential. Anyone who can read it can
call every tool the enrolment grants, as this client, in your audit log.

It is generated on the client by `relayremote register` or `relayremote enrol`, written `0600`, and never sent anywhere: relay only
ever sees the public half, inside the signing request. A registration keeps it
at `~/.config/relayremote/registrations/<name>/client.key` (the directory
`0700`, honouring `XDG_CONFIG_HOME`); a bundle keeps it wherever you pointed
`--bundle`. **Naming registrations does not separate them from each other** —
anything running as this user can read every one of those keys. What `--as`
buys is audit separation and revocation granularity on the host, not
isolation on the client. Relay's own copy of a CSR enrolment contains no
key at all, so there is no second copy to fall back on. `relayremote` refuses
to run at all if the file is group- or world-readable:

```
error: refusing to use /home/you/relay-bundles/vm-agent/client.key: mode 0644 is
readable by its group and every other user on this machine. This is the private key
that IS this client's identity to relay — anyone who can read it can call every tool
the enrolment grants, as this client, in the audit log. Fix it with:
chmod 600 /home/you/relay-bundles/vm-agent/client.key
```

There is no encrypted keystore and no OS keychain integration; the file
permission is the protection. If the key is lost or exposed, the fix is on the
Mac and is immediate: `relay enrol revoke --client-id vm-agent` deletes the
record and **closes any live connection** holding that certificate, not just
future ones. Enrolments are keyed by certificate, not by machine, so revoking
one leaves every other client — including others on the same VM — untouched.

---

## When it does not work

### `error: the codes did not match, so nothing was written.`

The client asked whether the code on the Mac matched and the answer was not a
typed `y`. Nothing was written and `client.key` is untouched, exit code 2.
Refuse the request on the Mac and treat the network path as suspect until you
know why the two values differed. Two innocent causes to eliminate first: you
compared against the wrong request, or relay's CA was regenerated between the
lodge and the approval.

### `error: stdin is not a terminal, so nobody can be asked to compare the code`

`register` from a script, a CI job or a pipe. Run it from a terminal, or use
[the fallback](#the-operator-carried-fallback). Refused at flag parse, exit
code 2: no key was generated and nothing was dialled.

### Relay at 10.0.0.2:9911 never accepted a connection

Exit code 8. The window is not open, or it closed (a request, the timer or a
restart closes it), or the host or port is wrong. Open a window on the Mac and
run `register` again. A window that closed because another peer lodged first
looks the same from here: open a fresh one.

### `error: 2 registrations on this machine and no way to tell which one you mean`

Exit code 2, and deliberate: `relayremote` does not pick between identities
that differ in what they may reach. Name one with `--as NAME`, or move the
default with `relayremote use NAME`.

### `error: pass --bundle or --as, not both`

They name two different identities. Exit code 2, nothing dialled.

### Exit code 12: registered, but relay's tool plane could not be reached

**The registration succeeded.** What failed is the first `list` against port
9910. Either `remote.listen` on the Mac is loopback and you are on another
host, or relay's server certificate carries only the names in `remote.listen`
and you arrived under a different one: pass `--server-name`. Fix either and
use the registration as it stands. Do not register again.

### `error: could not reach relay's enrolment listener`

Exit code 8. Usual causes, in order: no window is open; the remote listener
is off or auditing is off, so relay binds nothing; `remote.enrolment_listen`
is loopback and you are on another host; the address is firewalled.

### `still pending after 10m0s; giving up for now.`

Nobody approved it in time. Exit code 11. The request ended with its
connection, and the lodge closed the window, so there is nothing to pick up.
Open a window on the Mac and run `register` again.

### `relay no longer has a record of this request`

Exit code 10. The request expired, was already collected, or the operator
refused it. `client.key` is untouched. Open a window and register again.

### `error: bridge error (code -32603): enrolment request not found: req_…`

**On the Mac.** The request id does not exist. Requests expire 15 minutes
after arrival and `relay enrol requests` shows only live ones. The table lives
in the running tray's memory, so restarting relay empties it.

### `error: the requesting machine disconnected; ask it to register again`

**On the Mac.** The machine's connection closed, and a request does not
outlive it. Open a window and have the machine register again.

### `refused: approving needs your confirmation on the Mac's screen, ...`

Printed by `relay enrol approve` run from a session that cannot show a
prompt, such as SSH. Run it at the Mac, or approve from Settings > Remote
Clients > Pending requests. A lodged request does not expire because the
command refused.

`relay enrol open` from such a session prints the generic refusal instead:
`error: refused: this needs your confirmation on the Mac's screen, and the
session this command is running in cannot show a prompt (for example, you are
over SSH).` No window opens. Run it at the Mac's console or use the tray.

### `error: bridge error (code -32603): presence was refused`

The password prompt appeared and was cancelled or timed out. Nothing was
signed and no record was written.

### `error: … client.key already exists: refusing to overwrite it.`

`relayremote enrol` never overwrites a key. If you are genuinely re-enrolling,
remove it deliberately, and understand that the certificate you hold for that
bundle stops working the moment you do.

### `error: this certificate was not signed over this machine's key — it belongs to a different enrolment`

`relayremote install --from` pointed at a certificate signed for some other
client's key. Check which `signed` directory you copied.

### `error: enrolment bundle …: has client.key and no client.crt: the signing request has not been signed yet.`

The bundle is half-built: you generated a key but never installed a
certificate. Finish the fallback.

### `error: access denied: no tool named 'fs_write' is available to this grant (granted: macmcp)`

Exit code 3. The certificate is fine; the grant is not. Do not retry. Check
`relay grant --project <profile-id>` on the Mac and widen the profile if you
meant to. An unset tool list means **no tools**, and an unset scope field
denies every tool it governs.

### `throttled`

Exit code 4, and the only code here that means "wait and try again". Each
enrolment carries a rolling call-rate and result-volume budget: 120 calls and
64 MiB per hour by default. Adjust with
`relay enrol update --client-id vm-agent --max-calls N --window-seconds N`.

### Exit code 9: the wait ended mid-call

The request was sent and the answer never arrived. **Whether the tool ran is
unknown**, because relay records and budgets a call before invoking it. Do not
blindly retry anything with side effects. Check the Mac's audit log first.

### An enrolment made on the CLI does not show up in an open Settings window

It works immediately; only the display is stale. Reopen Settings.

---

## Where to go next

- **Confining the grant properly** — [`access-profiles.md`](access-profiles.md):
  the five independent allowlists, resource scope, how to check a confinement
  actually holds, and what none of it protects against.
- **Generating an agent skill** from what the grant really exposes:
  `relayremote skill --out ~/.claude/skills`. The generated `SKILL.md` carries
  the scope on each tool's description, so the agent reads its own limits
  before choosing arguments.
- **Reading the audit log** — [`audit-log.md`](audit-log.md), and
  `relay audit --kind remote` for anything a remote client did.
- **Every host command and flag** — [`cli.md`](cli.md).
- **Back to one machine** —
  [install-single-machine.md](install-single-machine.md).
- **Several identities on one client machine** — relayRemote's
  [`README.md`](../../relayRemote/README.md#registrations-naming-an-identity-on-this-machine):
  the store's layout, the full selection order, and what `--as` does not buy.
- **Why the transport and identity model is shaped this way** —
  ADR-010 (mTLS transport and certificate-based client identity),
  ADR-018 (configuration is a capability of an identity, narrowing only),
  and ADR-019 (registration is one command) for the
  comparison code and the registration store.
