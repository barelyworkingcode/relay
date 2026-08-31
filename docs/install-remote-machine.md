# Registering a new remote machine

A second machine — a VM, a Linux box, another laptop — reaching relay's tools
over the network. The client generates its own private key, asks to be
enrolled, you approve at the Mac, and the client collects its certificate.

Start from [install-single-machine.md](install-single-machine.md): this guide
assumes relay is running, at least one MCP is registered, and you know what a
presence prompt is.

Output shown here is captured from a real run unless a block says otherwise.
A block that was reconstructed from the command's own print format says so,
and a block whose values were substituted (a host, a path, a comparison code)
says that too. The reconstructed ones are all in Path C and in the rarer
`relay enrol approve` notes: everything on Path A, including the approval
behind the presence prompt, is a real run on a loopback host.

There are three ways to get a client enrolled. All are first-class:

- **[Path A](#path-a--register-one-command-one-comparison) —
  `relayremote register`.** One command on the client. It prints a
  six-character code, you compare that code against the one this Mac shows,
  and you approve. Nothing is carried by hand. **This is the normal path.**
- **[Path B](#path-b--relayremote-request-the-carried-pin-sibling) —
  `relayremote request`.** The same network exchange, but the client
  authenticates relay with a CA fingerprint you carried here out of band
  instead of with a code you compare. It writes into a `--bundle` directory
  rather than a named registration. Reach for it when you already have a
  bundle wired up, or when the relay host predates `register`.
- **[Path C](#path-c--operator-carried) — operator-carried.** You copy the
  signing request to the Mac and the certificate back. Needs no listener, no
  network path between the machines, and nothing to compare. **This is the
  one that always works**; reach for it whenever the network path is down,
  untrusted, or not worth opening for one enrolment.

---

## Which machine runs what

This is the single biggest source of confusion here, so it comes first.

| Binary | Runs on | What it is |
|---|---|---|
| `relay` | **the Mac only** | the host. Signs certificates, approves requests, owns the grant. |
| `relayremote` | **the client only** | the remote end. Generates its key, asks to be enrolled, calls tools. |

`relay enrol` and `relayremote enrol` are **different commands that share a
word**:

| Command | Machine | Does |
|---|---|---|
| `relayremote register` | client | the whole of Path A: key, request, code, collection, into a **named registration**. |
| `relayremote registrations` / `use` / `unregister` | client | the local registration store. `unregister` **revokes nothing.** |
| `relayremote enrol` | client | generates `client.key` + `client.csr`. Never touches the network. |
| `relayremote request` | client | lodges a signing request with relay over the network and waits. Pin carried by hand. |
| `relayremote install` | client | files a certificate an operator carried here. Never touches the network. |
| `relay enrol sign` | host | signs a signing request you carried to the Mac. Prompts. |
| `relay enrol requests` | host | lists what a remote has lodged over the network, with the comparison code. |
| `relay enrol approve` | host | signs a lodged request. Prompts. Requires a grant. |
| `relay enrol refuse` | host | declines one. Does **not** prompt. |
| `relay enrol ca-fingerprint` | host | prints the CA hash a Path B client pins. |
| `relay enrol list` / `revoke` / `update` | host | the enrolment records themselves. |

Every command block below says which machine it runs on.

---

## Before you start

**On the Mac:**

1. Relay running, and you at its keyboard. `relay enrol sign` and
   `relay enrol approve` raise a login-password prompt and refuse outright
   from a session that cannot show one.
2. **An access profile** the client will be granted. A profile is not a
   project: it has no directory, no shell, no models — just a capability
   grant. Create it in Settings → **Projects → + New → Access profile**;
   [`access-profiles.md`](access-profiles.md#creating-a-confined-client) is
   the reference for what each field does and why an empty field means "none"
   rather than "all". You need its id, which `relay grant` prints.
3. **Auditing on.** The remote listener refuses to start while
   `audit.enabled` is false — a remote grant is justified by the calls it
   records, so serving remote traffic unrecorded is not a degraded mode. It is
   on by default.

**On the client:** the `relayremote` binary. Prebuilt binaries for darwin,
linux and windows are in relayRemote's `dist/`, or build from source with
`./build.sh`.

### Turn the tool-plane listener on

**On the Mac.** No `remote` block in `settings.json` means **no socket at
all** — not a closed one, not one that refuses. An install that never enrolled
a client has no network surface, deliberately.

Settings → **Remote Clients** → *Accept remote clients on an mTLS listener* →
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
it — an unenrolled one is closed before a single request is read — but a
tunnel is a network path, never an identity.

---

## Path A — `register`: one command, one comparison

### A1. Turn the enrolment channel on

**On the Mac.** The enrolment-request channel is a *third* listener, separate
from the tool plane, and it is off unless you say otherwise — same rule as
`remote.enabled`, for the same reason.

Settings → **Remote Clients** → *Accept enrolment requests on a separate
listener* → Save. It sits directly beneath the mTLS toggle you turned on
above, with its own **Enrolment listen address** field; leave that blank for
the default, `127.0.0.1:9911`.

It is a **companion** to the tool-plane listener, never a substitute. Turn the
mTLS listener on first; the form warns before you save the other combination,
and relay refuses it outright when it tries to bind:

```
remote.enrolment_requests is true but remote.enabled is false: the enrolment-request
channel is a companion to the remote tool-plane listener, not a replacement for it —
set remote.enabled to true as well, or turn enrolment_requests off
```

The two toggles write four fields into the `remote` block:

```json
"remote": {
  "enabled": true,
  "listen": "127.0.0.1:9910",
  "enrolment_requests": true,
  "enrolment_listen": "127.0.0.1:9911"
}
```

Editing that by hand works too, and relay picks it up without a restart — it
watches the file's modification time and reconverges both listeners on its
next poll.

Confirm the socket exists:

```
$ lsof -nP -iTCP:9911 -sTCP:LISTEN
COMMAND   PID  USER   FD   TYPE             DEVICE SIZE/OFF NODE NAME
relay   36793 admin   13u  IPv4 0xfc385445e9cfd53a      0t0  TCP 127.0.0.1:9911 (LISTEN)
```

**Remove block** removes all four fields together and leaves no listener of
either kind. Enrolments are not touched by it.

Three facts about this listener, because they decide how much you should worry
about opening it:

- **It is plain TCP, with no TLS, on purpose.** Nothing on this wire is a
  secret in either direction: inbound is a signing request (public by
  construction), outbound is a certificate and a CA certificate (both
  public). TLS here would be decoration reading as a security property it
  cannot provide — the client has no CA to verify a handshake against yet.
  The real control is the comparison in A3, which plain TCP forces to be
  visible and mandatory.
- **Lodging raises no *prompt*, ever.** A peer that can reach this port can
  add a row to a table capped at 8 entries and nothing else. No code path
  from an unauthenticated lodge reaches the presence gate, so nobody on the
  network can make your Mac ask you for a password. You initiate every
  approval, and *that* is what prompts.
- **Lodging may raise a *notification*.** Where this listener is on, a new
  request can draw one dismissible tray banner: at most one a minute and six
  in a rolling hour, one regardless of how many are pending, none once the
  table is full, none while the Settings window is open, and carrying a count
  and nothing the requesting machine supplied. **A banner is not a sign that
  anything has gone wrong** — it means a machine asked, and nothing is
  granted until you approve.

  **Do not build a habit on the banner arriving.** macOS notifications can be
  turned off in System Settings, suppressed by Focus, and are unavailable to
  a relay running without its app bundle, and there is no way to force one.
  The tray menu's `Pending enrolment requests: N` line is the surface that is
  always there while something is waiting, and `relay enrol requests` reads
  the same table from a terminal.

  macOS can also refuse Relay notification authorization outright, with no
  prompt and nothing for you to have clicked — an unsigned or non-notarised
  build is refused as a matter of course. Relay logs that once, so it is not
  a silent nothing:

  ```
  grep "user notifications are disabled" ~/Library/Application\ Support/relay/logs/relay.log
  ```

  If that line is there, `System Settings → Notifications → Relay → Allow
  notifications` is off. It is a per-user setting, so turn it on in the
  account that runs the tray. Nothing else changes either way: the requests
  are in the table regardless.

### A2. Register, from the client

**On the client** — one command:

```
relayremote register "Hermes Mail" --host 10.0.0.2
```

It generates a P-256 key that never leaves this machine, lodges the signing
request together with a commitment to a random value of its own, receives
relay's CA certificate and a random value of relay's, and prints a
six-character comparison code straight away:

```
  code: FF9GTV · waiting for approval…

On the Mac: Settings → Remote Clients → Pending requests.
The same six characters must be shown beside this request. If they differ, Refuse there —
something is on the network path between the two machines.
```

then waits, reprinting the code with each heartbeat:

```
waiting… code FF9GTV (elapsed 30s, expires in 14m29s)
waiting… code FF9GTV (elapsed 1m0s, expires in 13m59s)
```

- **`--host` takes a bare host and nothing else.** Ports are `--port` (tool
  plane, 9910) and `--enrol-port` (enrolment requests, 9911). Passing an
  address with a port is refused rather than guessed at:

  ```
  $ relayremote register "X" --host 127.0.0.1:9911
  error: --host "127.0.0.1:9911" looks like an address with a port or a path: pass the bare host, and name the ports with --port (tool plane, default 9910) and --enrol-port (enrolment requests, default 9911)
  ```

  There is no discovery of any kind, and there will not be: a machine that
  announced itself is a machine an attacker can announce.
- **The positional argument is a display name.** It is slugified into the
  registration's local name (`"Hermes Mail"` → `hermes-mail`) and that slug is
  what goes on the wire as the label — so the only free text an unauthenticated
  peer can put in front of you is a string that already satisfies relay's
  existing label rule. Omit it and the machine's hostname is used.
- **`--profile ID` is a request, never a grant.** It is shown to you at
  approval, marked as a request, and never pre-selects anything.
- **Re-running the identical command resumes.** The in-flight state is written
  before the lodge frame goes out, so a killed `register` re-run reprints the
  *same* code and keeps polling the *same* request rather than lodging a
  second one — a second row in front of you, with a second code, is exactly
  the confusion one comparison exists to remove. `--fresh` deletes an
  in-progress registration and starts over.
- **`--wait` defaults to 10 minutes.** Relay's own request TTL is 15.

### A3. Compare the code on the Mac, then approve

**On the Mac.** The tray menu carries `Pending enrolment requests: N` while
any are waiting, and clicking it opens Settings → Remote Clients. From a
terminal:

```
$ relay enrol requests
REQUEST ID                            SAS     KEY                                                                      LABEL        FROM             ARRIVED               EXPIRES               STATUS
req_b2bde2c8de5abc2a8eec7a653b64c1ba  FF9GTV  sha256:72393466846801fc3cf664adaa64bb639d1bb489a1e7333f9ef2ab3fc95430c7  hermes-mail  127.0.0.1:65253  2026-08-31T13:56:19Z  2026-08-31T14:11:19Z  pending
req_b2cdbac96570e0c97a3d033c333c635b  -       sha256:3c086d09e813831e03cf55462330f1eb693ae1a88e79cc62f01ed3b7091632dc  vm-carried   127.0.0.1:65255  2026-08-31T13:57:07Z  2026-08-31T14:12:07Z  pending
```

**The `SAS` column is the whole of your job at this step.** Read the six
characters off the requesting machine's screen, read them off this one, and
approve only if they are identical. The second row above is a Path B request:
it carries no code, shows `-`, and is authenticated by the fingerprint its
operator carried instead.

The column has two other values, and they are not the same as `-`:

| value | means |
|---|---|
| `FF9GTV` | the code. Compare it. |
| `(waiting)` | that machine has not completed its half of the exchange yet. **Approve is disabled.** |
| `FAILED` | it completed it wrongly. Relay refuses to sign this row from every door. |
| `-` | a carried-pin `relayremote request` row. No code, and there never will be one. |

Then approve, choosing the grant yourself:

```
relay enrol approve \
  --id req_b2bde2c8de5abc2a8eec7a653b64c1ba \
  --client-id hermes-mail \
  --grant 477d9a17-da03-45eb-a433-764f93fe96fc
```

**`--grant` is required.** With none, this refuses and names `--no-grant` as
the deliberate way to enrol a machine that reaches nothing:

```
error: --grant is required: approving with no access profile issues a certificate that can reach nothing,
  which reads on the client machine as a broken install rather than a deliberate one.
  Name the profile it should reach:  relay enrol approve --id ID --client-id NAME --grant PROFILE_ID
  Or enrol it with no access on purpose, and grant later with `relay enrol update`:
    relay enrol approve --id ID --client-id NAME --no-grant
  See the profiles you can name with: relay grant
```

**The request names no grants and no budget.** A request that could ask for
its own authority would put an attacker-supplied claim on your screen, which
is the shape phishing takes. You choose, at approval, every time.

`--client-id` names the enrolment and must be unique. It, not the request's
label and not the signing request's own subject, is what appears in
`relay enrol list` and in every audit row this client ever writes.

This prompts, and the dialog names the network origin:

> **Relay** — "Relay is trying to approve an enrolment request from
> 127.0.0.1:65253 and sign a certificate for client "hermes-mail" with access
> to 477d9a17-da03-45eb-a433-764f93fe96fc. Enter the password for the user
> "…" to allow this."

It is the *same* presence gate `relay enrol sign` uses — same operation, same
digest, over the request's stored signing-request bytes. There is deliberately
no separate "approve" gate, because a second door into certificate issuance is
exactly what this design spends its effort closing on the first one. The
digest binds the stored public key, so an approval answered for one key can
never be redeemed for another.

On success:

```
approved enrolment request "req_b2bde2c8de5abc2a8eec7a653b64c1ba" as "hermes-mail"
  fingerprint: sha256:9d1f529024017d974df5f1e71a94040b39a81f22f0711437d2dd647c83021f34
  profiles:    477d9a17-da03-45eb-a433-764f93fe96fc
  the certificate is delivered to the client on its next poll; nothing further to do on this host
```

If the presence prompt sits open long enough to cross the request's own
15-minute TTL, this reports differently — see [When it does not
work](#when-it-does-not-work).

**Or do it from the window.** Settings → Remote Clients → **Pending
requests** shows the code in the largest type on the row, the key hash in
full, the label marked *(supplied by the requesting machine)*, any requested
profile marked *a request, not a grant*, origin address, arrival and expiry —
with **Approve…** and **Refuse** buttons. Approve opens the create-enrolment
form, where choosing a profile is a required step and "enrol with no access
for now" is a checkbox you have to tick on purpose. Same gate either way.

### A4. The client confirms, and reports what it got

The waiting `register` collects the certificate on its next poll, checks that
the CA it was handed is byte-identical to the one the code was computed over,
checks the chain and the key pairing, and then asks you the one question the
whole ceremony exists for:

```
the Mac showed N2Y7F5. Did it match? [y/N] y
registered "Hermes Mail" as hermes-mail
  relay:        127.0.0.1:9910
  certificate:  sha256:2cc63f1c222d095ecbcbc2ada28faf259adb44d9011a80548c6072587db21b40
  code matched: N2Y7F5

  access:       477d9a17-da03-45eb-a433-764f93fe96fc
                5 tools — mail_get_email, mail_get_emails, mail_list_accounts, mail_list_mailboxes, mail_search

  use it with:  relayremote call --tool NAME --args '{"…":"…"}'
                (this is the only registration, so --as is not needed)
```

(That block is a real run on a loopback host, so its code and `relay:` line
differ from the `10.0.0.2` / `FF9GTV` this walkthrough otherwise uses. The
`relay:` address is derived from the `--host` you passed.)

**Anything but a typed `y` writes nothing** and leaves the key where it is:

```
error: the codes did not match, so nothing was written. Refuse the request on the Mac:
something is on the network path between this machine and 10.0.0.2. The key in
~/.config/relayremote/registrations/hermes-mail is untouched
```

(Real output, with the host and the registration path substituted for the
loopback ones it was captured with.)

Two things about that question, because they are the point of Path A:

- **The address `register` reports comes from your `--host`, never from the
  wire.** Relay reports its own `remote.listen`, which is usually
  `127.0.0.1:9910` and meaningless on another machine; following a host
  supplied over an unauthenticated channel would be a redirection primitive
  even after the comparison has closed. Only the *port* is ever taken from
  the wire, and only when you did not type `--port` yourself. A `relay_addr`
  naming some other host is printed as a note and not followed.
- **A headless client passes `--ca-fingerprint sha256:…` instead** — read it
  on the Mac with `relay enrol ca-fingerprint`. That is the same check done by
  machine rather than by eye, and it is *stronger*, not a way to skip one:
  prompting a human to eyeball a code a machine has already proven equal is
  how you train people to answer `y` without looking. With stdin not a
  terminal and no pin, `register` refuses before it generates a key:

  ```
  error: stdin is not a terminal, so nobody can be asked to compare the code — pass
  --ca-fingerprint sha256:<64 hex characters> instead. Read it on the relay host with
  `relay enrol ca-fingerprint`, or from the header in Settings → Remote Clients.
  There is no flag that completes a registration without one of the two.
  ```

  `--tofu` does not exist on `register` and is refused by name.

### A5. The registration store on the client

`register` files everything into a **named registration**, so the client no
longer needs `--bundle` and `--addr` on every call:

```
$ relayremote registrations
  NAME         CLIENT ID    HOST            PROFILES                              DEFAULT
  hermes-cal   hermes-cal   127.0.0.1:9910  b0000000-0000-4000-8000-000000000001
* hermes-mail  hermes-mail  127.0.0.1:9910  477d9a17-da03-45eb-a433-764f93fe96fc  yes

Separate registrations mean separate audit trails and separate revocation on the host.
They are NOT isolation: anything running as this user can read every client.key above.
```

Register again with a different name and you get a second keypair, a second
certificate and a second enrolment on the host — which is the shape relay
already expects: several agents on one VM, each granted, audited and revoked
independently. Then:

```
relayremote --as hermes-cal list      # act as one, for one call
relayremote use hermes-cal            # move the * to it
relayremote unregister hermes-cal     # delete it HERE. This revokes nothing.
```

**With several registrations and no selector, `relayremote` refuses rather
than guessing** — these identities differ precisely in what they may reach,
and a wrong guess is silent because relay answers the wrong identity
perfectly well:

```
error: 2 registrations on this machine and no way to tell which one you mean:
  hermes-cal
  hermes-mail
  choose one for this call:      relayremote --as NAME ...
  or make one the default:       relayremote use NAME
refusing to pick: these identities differ in what they are allowed to reach
```

**`unregister` is local only, and says so at length**, because a client-side
delete that read as a revocation would be the worst possible lie in this
system:

```
$ relayremote unregister hermes-cal --yes
Removed the LOCAL registration "hermes-cal" from this machine.

THIS DID NOT REVOKE ANYTHING. The enrolment "hermes-cal" still exists on
127.0.0.1, still counts against its budget, and still appears in the audit
log. To actually cut its access, run on the Mac:

    relay enrol revoke --client-id hermes-cal

It was the default registration; that pointer is now cleared. Name one with `relayremote use NAME`.
```

The full precedence order, the store's on-disk layout and what `--as` does
*not* buy you are in
[relayRemote's `README.md`](../../relayRemote/README.md#registrations-naming-an-identity-on-this-machine).
`--bundle` and `RELAY_REMOTE_BUNDLE` keep working exactly as before and
bypass the store entirely.

---

## Path B — `relayremote request`: the carried-pin sibling

Same listener, same approval, same presence prompt. The difference is what
the client trusts: a fingerprint you carried here by hand, instead of a code
you compare. Use it when a bundle directory is already wired up, or against a
relay that predates `register`.

Turn the channel on exactly as in [A1](#a1-turn-the-enrolment-channel-on)
first.

### B1. Read the CA fingerprint on the Mac

**On the Mac:**

```
$ relay enrol ca-fingerprint
sha256:42d03e331c5be0a449ea8bb2e1d6562d818969fea8df395772066398064c1f78
```

Settings → Remote Clients shows the same value in its header. This reads
`ca.crt` off disk and works with the tray stopped.

**Why the client demands this.** The client has no way to tell a real relay
from an impostor at connection time — that is what "no TLS" means. An attacker
on the network path can pass the client's real signing request straight through
to the real relay, so the key hash you compare in B3 matches and you approve in
good faith. The attacker then returns its **own** CA certificate to the
waiting client. Every check the client runs on what it receives — that the
certificate chains, that it pairs with the local key — passes, because the
certificate really is valid, just for the wrong CA. From then on every tool
call, arguments included, goes to whoever answered instead of to your Mac.

Comparing this fingerprint is the only thing that closes that on this path. So
`relayremote request` refuses to run without either `--ca-fingerprint` or
`--tofu`; neither has a default. (Path A closes the same gap with the
comparison code, which is why `register` needs no carried value.)

Carry the string to the client however you like. It never needs to cross the
network between the two machines, and it is not a secret — it just has to
arrive unaltered.

### B2. Lodge the request from the client

**On the client:**

```
relayremote request \
  --addr 10.0.0.2:9911 \
  --bundle ~/relay-bundles/vm-agent \
  --ca-fingerprint sha256:42d03e331c5be0a449ea8bb2e1d6562d818969fea8df395772066398064c1f78 \
  --label vm-agent
```

It generates a P-256 key in `--bundle` if there is not one already, writes a
signing request, lodges it, and polls:

```
lodged enrolment request req_b2cdbac96570e0c97a3d033c333c635b with relay at 127.0.0.1:9911
  this machine's public key: sha256:3c086d09e813831e03cf55462330f1eb693ae1a88e79cc62f01ed3b7091632dc      ← compare this on the host
  relay's CA fingerprint will be checked against: sha256:42d03e331c5be0a449ea8bb2e1d6562d818969fea8df395772066398064c1f78
approve it on the relay host: Settings → Remote Clients → Pending requests,
  or:  relay enrol approve --id req_b2cdbac96570e0c97a3d033c333c635b --client-id ID --grant PROFILE_ID
waiting… (expires in 15m0s)
```

All of that is on stderr; stdout stays clean for `--json`. A heartbeat line
follows every 30 seconds.

- `--addr` here is the **enrolment** listener, port 9911 by default. It is a
  different flag from the `--addr` that `list` and `call` use, and a different
  environment variable (`RELAY_REMOTE_ENROL_ADDR`, not
  `RELAY_REMOTE_ADDR`). They name two listeners; conflating them makes
  "connection refused" ambiguous.
- `--label` is at most 64 bytes of letters, digits, `.`, `_` and `-`. It is
  shown to you at approval, clearly marked as supplied by the requesting
  machine — it is never relay's own assertion about who this is.
- `--wait` defaults to 10 minutes. Relay's own request TTL is 15.
- `--tofu` is the alternative to `--ca-fingerprint`: it prints the CA
  fingerprint at collection time and asks you to confirm it interactively. It
  requires a terminal and refuses on a non-interactive stdin, and there is no
  flag that skips the confirmation. Use it only from a terminal you are
  actually watching. A headless VM wants `--ca-fingerprint` — or Path A.

### B3. Compare the key hash, then approve

**On the Mac.** `relay enrol requests` lists it beside anything else pending —
it is the `vm-carried` row in [A3's capture](#a3-compare-the-code-on-the-mac-then-approve),
and it shows `-` in the `SAS` column because this path carries no comparison
code and never will.

**Compare that KEY against the hash the client printed in B2.** It is the
whole 64 hex characters, never truncated, on both screens on purpose — a
prefix answers "probably that key" where the point is "that key". If they do
not match, refuse.

Then approve exactly as in [A3](#a3-compare-the-code-on-the-mac-then-approve)
— same command, same required `--grant`, same presence prompt, same output.

### B4. The client collects

Within a couple of seconds the waiting `relayremote request` verifies what
arrived — CA fingerprint against the pin, then chain, key pairing, and public
key equality, all before a byte reaches disk — writes `client.crt` and
`ca.crt` beside the key, and prints (reconstructed, downstream of the prompt):

```
enrolled as "vm-agent"
  fingerprint: sha256:…
  relay:       10.0.0.2:9910
  projects:    477d9a17-da03-45eb-a433-764f93fe96fc

use it with:
  relayremote list --bundle ~/relay-bundles/vm-agent --addr 10.0.0.2:9910
```

Note the address it gives you is the **tool plane**, 9910 — not the enrolment
listener you have been talking to. No registration is created on this path;
the bundle directory is the identity, exactly as it was.

---

## Path C — operator-carried

No listener, no network path between the machines, nothing to compare. Three
commands.

**1. On the client** — generate the key and the signing request. This never
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

Copy `client.csr` to the Mac — USB stick, `scp`, paste, anything. It is public
by construction and proves possession of a key that never leaves the client.
`client.key` stays where it is.

**2. On the Mac** — sign it, at the Mac's own screen:

```
relay enrol sign --client-id vm-agent --csr client.csr \
  --grant 477d9a17-da03-45eb-a433-764f93fe96fc --out ./signed
```

This prompts: *"sign a certificate for client "vm-agent" with access to
477d9a17-…"*. A signing request's natural habitat is an SSH session or a USB
stick, so expect to run this one at the console even when the file itself
arrived over the network.

Reconstructed — this one is behind the prompt:

```
signed enrolment "vm-agent"
  fingerprint: sha256:…
  profiles:    477d9a17-da03-45eb-a433-764f93fe96fc
  certificate: /Users/you/Library/Application Support/relay/enrolments/vm-agent
  copy client.crt and ca.crt to the client machine, beside the client.key it generated;
  no private key was written on this host
  copies also written to: ./signed
```

Nothing in that directory is a secret — `client.crt` and `ca.crt` only. Relay
never held a private key for this enrolment and has none to lose.

**3. On the client** — file what came back:

```
$ relayremote install --bundle ~/relay-bundles/vm-agent --from ./signed
installed certificate for "vm-agent"
  fingerprint: sha256:…
```

(Reconstructed, since step 2 is gated. If the bundle already held a
certificate, a `previous:` line names the fingerprint this replaced.)

`install` verifies before it writes anything: the certificate pairs with the
key already in the bundle, it chains to the supplied CA for client
authentication, and its public key hashes to the same value the local key
does. A refusal leaves the bundle exactly as it was.

There is no CA fingerprint to pin on this path — you carried `ca.crt` by hand,
and copying it *is* the trust decision.

> `relay enrol create` is a third, older path that generates the client's key
> **on the Mac** and hands you a bundle containing it. It still works and says
> so in its own output, but prefer `sign`: a key that never crosses a wire is
> a key that cannot be intercepted on one.

---

## Check that it worked

**On the client.** A machine from Path A needs no flags at all — the
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
From Path B or C, name the directory instead:
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

(The three blocks above were captured on a loopback host against the
registration made in Path A. CALLER carries the enrolment's `--client-id`,
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

If the enrolment holds more than one grant, add `--project ID` (or
`RELAY_REMOTE_PROJECT`) to say which one you are acting as. With exactly one,
it is optional; a registration records the default it was granted, so it is
optional there too.

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

It is generated on the client by `relayremote register`, `relayremote enrol`
or `relayremote request`, written `0600`, and never sent anywhere: relay only
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

**Path A, and the one message here you should not work around.** The client
asked whether the code on the Mac matched and you said no (or the answer was
not a typed `y`). Nothing was written and `client.key` is untouched, exit code
2. Refuse the request on the Mac and treat the network path between the two
machines as suspect until you know why the two values differed — a mismatch is
the signal this exchange exists to produce, not a glitch to retry past. Two
innocent causes worth eliminating first: you compared against the wrong row
(there can be several pending), or relay's CA was regenerated between the
lodge and the approval.

### `error: stdin is not a terminal, so nobody can be asked to compare the code`

`register` from a script, a CI job or a pipe. Pass `--ca-fingerprint
sha256:…`, read on the Mac with `relay enrol ca-fingerprint` — the same check
done by machine instead of by eye, and a stronger one. Refused at flag parse,
exit code 2: no key was generated and nothing was dialled.

### `invalid boolean flag tofu: --tofu does not exist on `register``

It never did. The comparison code replaced it, and it is a stronger control
than trusting whatever CA answered. `--tofu` still exists on `relayremote
request`, where there is no code to compare.

### `error: 2 registrations on this machine and no way to tell which one you mean`

Exit code 2, and deliberate: `relayremote` does not pick between identities
that differ in what they may reach, because a wrong guess is silent — relay
answers the wrong identity perfectly well. Name one with `--as NAME` for this
call, or `relayremote use NAME` to move the default. The message lists every
name it knows.

### `error: pass --bundle or --as, not both`

They name two different identities: a directory, and a registration in the
store. Exit code 2, nothing dialled.

### Exit code 12 — registered, but relay's tool plane could not be reached

**The registration succeeded.** The certificate is signed, filed and listed by
`relayremote registrations`; what failed is the first `list` against port
9910. The two usual causes are named in the message: `remote.listen` on the
Mac is loopback and you are on another host, or relay's server certificate
carries only the names in `remote.listen` and you arrived under a different
one — pass `--server-name`. Fix either and use the registration as it stands;
do not re-register.

### `error: this relay does not support register`

The host predates ADR-019 and its enrolment listener rejects the extra field
`register` sends. Use Path B (`relayremote request --ca-fingerprint …`), or
update relay. Exit code 2.

### `error: pass either --ca-fingerprint sha256:<64 hex characters> or --tofu — one of the two is mandatory, because this channel is plain TCP and authenticates nothing about the server on its own.`

**Path B only.** Neither has a default and neither can be omitted. Read the fingerprint on the
Mac with `relay enrol ca-fingerprint`, or use `--tofu` from a terminal you are
watching. Exit code 2 — nothing was sent.

### `error: could not reach relay's enrolment-request listener at 127.0.0.1:9911: … connection refused`

The message continues with `Check that the relay host has
remote.enrolment_requests: true set and is reachable at that address`, and
then always prints the operator-carried fallback. Usual causes, in order: the
enrolment-request toggle is off (see [A1](#a1-turn-the-enrolment-channel-on));
it is on but the mTLS listener beside it is not, so relay refuses to serve
either; `enrolment_listen` is loopback and you are on another host; the
address is firewalled. Exit code 8.

### `error: relay unreachable: cannot reach relay at 10.0.0.2:9910: … connection refused`

This is the **tool plane**, not the enrolment channel — a different port and a
different cause. Relay's `remote.enabled` is false, or the listener is bound
to loopback and you are elsewhere. The message says so:

```
Check that the host is up, that its settings.json has a `remote` block with
`enabled: true`, and that the listen address is reachable from here (it
defaults to 127.0.0.1, which is not)
```

### `still pending after 10m0s; giving up for now.`

Nobody approved it yet. This is not a refusal; the request lives 15 minutes on
relay's side. Exit code 11, and the message names the resume command:

```
resume later with:
  relayremote request --resume req_3e4501d2… --bundle ~/relay-bundles/vm-agent
```

`--resume` polls only and lodges nothing.

For `register` there is no `--resume` flag and none is needed: **re-running
the identical command resumes**, reprinting the *same* code and polling the
*same* request. It lodges again only if relay has swept the row, and says so
when it does. Exit code 11 either way.

### `relay no longer has a record of this request (expired, already collected, or an unrecognised id).`

Exit code 10. The row is genuinely gone: it expired 15 minutes after arrival
(or, if it had already been approved, 15 minutes after that without being
collected), it was already collected, or the `--resume` id was never a real
one. `client.key` is untouched; re-run without `--resume` to lodge a fresh
request. If you expected it to have been collected already, check `relay
enrol list` on the Mac.

### `the operator refused this request. client.key was not touched; re-run relayremote request to lodge a fresh one.`

Also exit code 10, but a distinct message from the one above: refusing an
enrolment request keeps its row until the row's own TTL rather than deleting
it, specifically so the client's next poll can say "refused" rather than
falling through to "no record of this request" and reading as an expiry or a
typo'd id. `client.key` is untouched; re-run without `--resume` to lodge a
fresh request.

### `error: bridge error (code -32603): enrolment request not found: req_…`

**On the Mac.** The request id does not exist. Requests expire 15 minutes
after arrival, and `relay enrol requests` only ever shows live ones. The table
lives in the running tray's memory and is never persisted, so restarting relay
empties it.

### `note: the pending request row expired before this approval finished …`

Rare, and not an error — it is part of `relay enrol approve`'s own success
output, printed when the presence prompt sat open long enough (past the
request's 15-minute TTL) to cross paths with some other table activity that
swept the row while the prompt was still open. By the time it prints, the
enrolment has **already committed**: it is real, signed, and in `relay enrol
list`. What is gone is only the row the client would have collected it
through — its next poll now sees "unknown", not "approved", because there is
no row left to answer either way. The output names both facts and the two
ways to recover:

```
approved enrolment request "req_3e4501d2142dc76a99399399b16c727b" as "vm-agent"
  fingerprint: sha256:…
  profiles:    477d9a17-da03-45eb-a433-764f93fe96fc
  note: the pending request row expired before this approval finished (the presence prompt
  was open long enough to cross its TTL) — the client's poll will now see "unknown", not "approved"
  the enrolment itself is real and already recorded; see it in `relay enrol list`
  the client cannot collect it through this request anymore: deliver the certificate via the
  operator-carried path (`relay enrol sign` + `relayremote install --from`), or run
  `relay enrol revoke --client-id vm-agent` to undo it
```

Recover with one of the two named paths: hand the client its certificate the
operator-carried way (Path C, above), or `relay enrol revoke --client-id
vm-agent` to undo the enrolment and let the client lodge a fresh request.

### `note: you refused this exact request from the pending list while this approval's presence prompt was still open …`

Also part of `relay enrol approve`'s own success output, and easy to confuse
with the expiry note above — read it carefully, because the fix is different.
This one prints when the pending row is found **refused**, not swept: someone
at this Mac declined this exact request by name (`relay enrol refuse`, or
Settings → Remote Clients → Pending requests) while *this* `relay enrol
approve` run's own presence prompt was still open, waiting on a fingerprint or
password. By the time it prints, the certificate has already been signed and
committed — real, and in `relay enrol list` — for the CSR the refusal named.
The row is not gone, either: the client's poll answers "refused", exactly as
it would have without the race. The output names the refusal plainly and the
one way to undo it:

```
approved enrolment request "req_3e4501d2142dc76a99399399b16c727b" as "vm-agent"
  fingerprint: sha256:…
  profiles:    477d9a17-da03-45eb-a433-764f93fe96fc
  note: you refused this exact request from the pending list while this approval's presence
  prompt was still open — the certificate was signed and committed anyway, for the CSR that
  refusal named. It is real and already recorded; see it in `relay enrol list`. If your refusal
  still stands, run `relay enrol revoke --client-id vm-agent` to undo it
```

If the refusal still stands, `relay enrol revoke --client-id vm-agent` is the
recovery — there is no operator-carried delivery path to offer here, unlike
the expiry case: nothing about this certificate needs a different transport,
it needs to not exist.

### `refused: approving needs your confirmation on the Mac's screen, ...`

`relay enrol approve` run from a session that cannot show a prompt:

```
refused: approving needs your confirmation on the Mac's screen, and the session this
  command is running in cannot show a prompt (for example, you are over SSH).
  The request is still waiting — it does not expire because this command refused.
  Approve it from the Mac's own screen instead: open the Relay tray ->
  Settings -> Remote Clients -> Pending requests.
  Read commands are unaffected: relay enrol requests, relay audit, relay grant.
```

Unlike every other gated command, this one *does* have something queued —
which is why it names the working door instead of the usual "there is no
queue".

### `error: bridge error (code -32603): presence was refused`

The password prompt appeared and was cancelled or timed out. Nothing was
signed and no record was written.

### `error: … client.key already exists: refusing to overwrite it.`

`relayremote enrol` never overwrites a key. If you are genuinely re-enrolling,
remove it deliberately — and understand that the certificate you currently
hold for that bundle stops working the moment you do. (`relayremote request`
in a bundle that has a key and no certificate **reuses** that key and its
signing request instead of failing — that is the intended
resume-after-failure path.)

### `error: this certificate was not signed over this machine's key — it belongs to a different enrolment`

`relayremote install --from` pointed at a certificate signed for some other
client's key. Two enrolments' `signed` directories look identical; check which
one you copied.

### `error: relay's CA fingerprint does not match --ca-fingerprint:`

```
  expected sha256:…
  received sha256:…
refusing to write anything — this is exactly the mismatch pinning exists to catch
```

Either you mistyped the fingerprint, or you are not talking to the relay you
think you are. Re-read it on the Mac with `relay enrol ca-fingerprint` before
assuming the first. Nothing was written.

### `error: enrolment bundle …: has client.key and no client.crt: the signing request has not been signed yet.`

The bundle is half-built: you generated a key but never installed a
certificate. Finish Path A, B or C.

### `error: access denied: no tool named 'fs_write' is available to this grant (granted: macmcp)`

Exit code 3. The certificate is fine; the grant is not. Do not retry — check
`relay grant --project <profile-id>` on the Mac and widen the profile if you
meant to. The most common cause is an access profile's fail-closed defaults:
an unset tool list means **no tools**, and an unset scope field denies every
tool it governs.

### `throttled`

Exit code 4, and the only code here that means "wait and try again". Each
enrolment carries a rolling call-rate and result-volume budget — 120 calls and
64 MiB per hour by default. Adjust with
`relay enrol update --client-id vm-agent --max-calls N --window-seconds N`.

### Exit code 9 — the wait ended mid-call

The request was sent and the answer never arrived. **Whether the tool ran is
unknown** — relay records and budgets a call before invoking it, so it may
well have. Do not blindly retry anything with side effects; check the Mac's
audit log first. Deliberately distinct from exit code 8, which means nothing
happened.

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
  [ADR-010](decisions/010-remote-client-transport-and-identity.md),
  [ADR-018](decisions/018-configuration-is-a-capability-of-an-identity.md),
  and [ADR-019](decisions/019-registration-is-one-command.md) for the
  comparison code and the registration store.
