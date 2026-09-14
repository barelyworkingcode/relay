# Eve passkey enrolment — adding a second browser

Design and contract. Two programs implement it — relay and eve — and this
document is the one place the wire shapes and the reasoning live. Code
carries the present tense; the *why* is here.

## The idea

Eve's first visitor enrols a passkey and becomes the owner. Until now that
was also the *last* passkey: `data/auth.json` held one credential, the
enrol routes refused once it existed, and a second browser (a phone, a
laptop, a fresh profile) could only get in by deleting the file and
re-bootstrapping.

The fix is a **window**. An operator standing at the console opens it — from
the relay tray or `relay eve enrol` — and for the next five minutes exactly
one new browser may register a passkey with eve. The first successful
registration closes it; so does the clock. Relay owns the window because
relay is the thing on this machine that already knows how to ask "is a human
really here?" (the presence gate) and already anchors its own passkey
registration the same way (`relay login enrol`). Eve owns the passkeys
because eve is the thing the browser authenticates to.

## Decisions

**1. A window, not a code.** `relay login enrol` mints a code the operator
types into relay's own login page. Eve's flow is a flag with a deadline
instead: the operator opens it, walks to the other device, and taps *Add
this browser*. Nothing to transcribe. The price is that during those five
minutes any client that can reach eve's enrol routes could take the slot.
That is bounded three ways: the window is presence-gated on the console, it
is single-use so a stranger taking it is *visible* (the operator's own
attempt then fails with "not open"), and the enrol routes stay rate-limited
per source IP. Relay notifies the console when the slot is taken and records
who took it, so a surprise enrolment is loud rather than silent.

**2. Relay holds the flag; eve asks.** The flag lives in relay's settings
store next to `login_bootstrap`, with the same shape of lifetime: an RFC 3339
expiry, at most one at a time, opening a new one replaces the old. Eve never
caches "open" beyond a couple of seconds and never decides on its own — every
enrol request while enrolled asks relay, and the successful one *consumes*
the flag on relay before eve writes the credential. Consume is atomic in
relay (`store.With`), so two browsers racing the same window cannot both
win.

**3. Order on the eve side is verify → consume → save.** Eve verifies the
WebAuthn registration first, then asks relay to consume, then appends the
credential. A failed ceremony therefore does not burn the window, and a
window that closed between `start` and `finish` refuses before anything is
written. The gap between consume and save is one process and microseconds.

**4. Additional enrolment ignores the first-passkey network rules.** The
pre-enrolment gate (`enrollment-gate.js`) — loopback or trusted subnet only,
never a public IP — protects an *unowned* box from being claimed. An owned
box adding a browser is a different act: the operator authorised it at the
console seconds ago, and the browser being added is, by construction,
somewhere the trusted-subnet bypass does *not* reach (otherwise it would
never see the login screen). So while the window is open a request from any
source may enrol, subject to rate limiting. Public-IP bootstrap of the
*first* passkey stays impossible.

**5. One user handle, many credentials.** All of eve's credentials belong to
one WebAuthn user. Eve persists a random `userId` in `auth.json` at first
enrolment (and back-fills one for a pre-existing file), reuses it for every
later registration, and sends the existing credential ids as
`excludeCredentials`, so an authenticator that already holds a passkey for
this eve refuses to mint a duplicate instead of silently creating a second.
Login is unchanged: it already uses discoverable credentials
(`allowCredentials: []`) and looks the presented id up in the list.

**6. The RP ID is the one recorded at first enrolment.** A second browser may
reach eve by a different hostname than the first did. The credential file's
`rpId` wins for every later registration, exactly as it already does for
login; otherwise the two browsers would hold passkeys for two different
relying parties and one of them could never sign in.

**7. Listing and revoking live in relay's Passkeys tab.** Eve owns the
credentials, but the operator's one place for "who can sign in" is relay's
Settings → Passkeys, and its CLI twins. So eve *reports* its credential list
to relay (public metadata only), relay *mirrors* it for display, and revoking
is relay's act behind the same presence gate as revoking one of relay's own.
Eve then *pulls* the revocation — on a slow poll and, decisively, on every
login attempt — so a revoked passkey fails on its very next use. The whole
second half of this document is that design.

## Relay: the window

### Settings

```jsonc
// settings.json — absent means closed, which is both the pre-feature state
// and the state the moment after the slot is consumed or expires.
"eve_enrolment": { "expires": "2026-09-07T10:15:00Z" }
```

An `expires` relay cannot parse reads as expired (the same rule
`APICredential.Expired` and `consumeBootstrapCode` follow).

Window lifetime: **5 minutes** (`eveEnrolmentTTL`). Long enough to pick up a
phone and tap; short enough that an operator does not forget it is open.

### Core: `EveEnrolmentOps`

Same shape as `LoginOps`: `Store`, `Audit`, `Gate`, `OnChange`, every method
nil-safe. Three methods.

| Method | Gate | Effect |
|---|---|---|
| `Open(ctx, via)` | **yes** — `eve.enrolment.open` | Writes a fresh `eve_enrolment` (replacing any), records the issuance in the audit log, returns `{expires, ttl}`. Withheld if it cannot be recorded, as `MintBootstrap` is. |
| `Status()` | no | `{open bool, expires string}` — open iff a record exists and has not expired. |
| `Consume(ctx, claim)` | no | Inside one `store.With`: if open, delete it and return the record; else `errEveEnrolmentClosed`. Records the consumption (source IP + label from `claim`) in the audit log and notifies the tray. |

Presence prompt reason for `eve.enrolment.open`:
`open a five-minute window for one new browser to register an Eve passkey`.
Add it to the gated-operation list in `docs/presence-gate.md` and the prompt
table in `docs/cli.md`; `doc_reason_strings_test.go` pins the two together.

### Doors

Three, matching `login.bootstrap.mint`'s three (tray, CLI, HTTP) — except
that here the HTTP door is *eve's*, so it carries status/consume rather than
open. Opening is deliberately reachable only from a surface with a human at
it.

- **Tray menu** — `Allow Eve Passkey Enrolment…` beneath `Show Login Code...`.
  Runs `Open` in a tracked goroutine (never on the Cocoa main thread — see
  `showLoginCode`'s comment) and posts a notification: *Eve passkey enrolment
  open for 5 minutes*. While the window is open, the menu shows a disabled
  line `Eve enrolment open — 4m 12s left`, refreshed by the existing 2-second
  poll; it disappears when the slot is consumed or expires.
- **CLI** — `relay eve enrol`, brokered over `admin_op` (`eve.enrolment.open`)
  like `relay login enrol`, because this process holds no gate. Prints the
  expiry and one line saying what to do next:
  ```
  eve passkey enrolment open until 10:15:00 (5m0s, single use)
    on the new browser, open Eve, and tap "Add this browser"
  ```
- **HTTP (frontend socket, for eve)**

  | Route | Class | Response |
  |---|---|---|
  | `GET /api/eve/passkey-enrolment` | `read` | `200 {"open": true, "expires": "…"}` or `{"open": false}` |
  | `POST /api/eve/passkey-enrolment/consume` | `configure` | body `{"ip": "…", "label": "…"}` → `200 {"expires": "…"}` when consumed, `409 {"error": "…"}` when closed |

  Eve's launch identity, holding the `frontend` capability, has `read`, `configure` and
  `proxy`, so both are reachable with no `Authorization` header. They are registered
  through `RouteRegistrar.Handle`, so they are socket-only unless a class
  says otherwise, and they reserve their paths ahead of the manifest
  dispatcher like every other relay-served route.

On consume, relay posts a notification — *Eve: a new browser registered a
passkey (from 10.0.1.7)* — so the operator sees the slot go, whether or not
it was them.

## Eve: the passkeys

### `auth.json`

```jsonc
{
  "rpId": "eve.lan",
  "userId": "<base64url, 32 random bytes>",       // new; back-filled on first use
  "credentials": [
    { "id": "…", "publicKey": "…", "counter": 0, "transports": ["internal"],
      "createdAt": "2026-09-01T09:00:00Z", "label": "Mozilla/5.0 (Macintosh…)" },
    { "id": "…", "publicKey": "…", "counter": 0, "transports": ["internal", "hybrid"],
      "createdAt": "2026-09-07T10:12:31Z", "label": "Mozilla/5.0 (iPhone…)" }
  ],
  "createdAt": "2026-09-01T09:00:00Z"
}
```

Pre-existing files (one credential, no `userId`, no per-credential
`createdAt`/`label`) load unchanged; the missing fields are filled in on the
next save.

### `EnrollmentWindow` (`enrollment-window.js`)

The only eve code that knows the relay routes above. `isOpen()` calls the
GET and caches the answer for **2 seconds** (the login screen polls every 3,
and every enrol request asks again). `consume({ip, label})` POSTs and
resolves `true` on 200, `false` on 409, throws on anything else. With no
`RelayTransport` (eve started without relay), both report closed.

### Routes (`routes/auth.js`)

`requireNotEnrolled` on `enroll/start` and `enroll/finish` becomes
`requireEnrollable`:

- not enrolled → proceed (the pre-enrolment gate has already applied its
  network rules upstream);
- enrolled and `enrollmentWindow.isOpen()` → proceed as an *additional*
  enrolment;
- enrolled and closed → `403 {"error": "Enrollment is not open. Open it from the Relay tray or with `relay eve enrol`."}`.

`enroll/finish` for an additional enrolment: verify → `consume({ip: getClientIp(req), label: User-Agent})` → on `false`, `403` with the same message and nothing saved → append credential → mint session token.

`GET /auth/status` gains `enrollmentOpen` (boolean; only computed when
`enrolled && !authenticated`, so an authenticated tab never polls relay).

### Client (`public/auth.js`, `index.html`)

The login screen keeps its primary *Sign In* button and gains a secondary
*Add this browser* button that is hidden unless `status.enrollmentOpen`.
While the login screen is visible the client re-fetches `/api/auth/status`
every 3 seconds so the button appears within a few seconds of the operator
opening the window from the tray. After three minutes on the login screen
the poll slows to every 15 seconds, and a tab the browser reports as hidden
skips the fetch entirely: every poll is a relay round-trip that lands in
relay's audit log, and a tab left on the login screen overnight has no
operator walking towards it. Polling stops when the screen hides.
Clicking it runs the existing `enroll()` ceremony. Message under the
buttons when the window is open: *Enrolment is open for a few minutes.*

`index.html` is cached at startup — the new button needs an eve restart
(`npm run relay:restart`).

## Operator flow

1. Open Eve on the new device. It shows *Sign In* (no passkey here yet).
2. At the console: tray → **Allow Eve Passkey Enrolment…**, answer the
   presence prompt. (Or `relay eve enrol` over SSH — refused, like every
   gated op, when no prompt can be shown.)
3. Within three seconds the device shows **Add this browser**. Tap it, do
   Face ID / Touch ID / PIN.
4. Signed in. The window is closed; the console gets a notification saying
   which address enrolled.

If step 3 says *Enrollment is not open*, the window expired or something
else consumed it — check the notification and `relay audit`, then open it
again.

## Tests

Relay: `EveEnrolmentOps` open/status/consume (expiry, unparseable expiry,
single use under concurrent consume, nil-safety), route class/transport
reachability, admin op, and the doc/gate structural suites that already
police every gated op.

Eve: `auth.js` multi-credential (append, `excludeCredentials`, stable
`userId`, legacy-file back-fill, recorded `rpId` reused), `routes/auth.js`
gating with a fake window (closed → 403 and nothing written; open → verify
before consume; consume `false` → 403 and nothing written), status field,
`EnrollmentWindow` against the integration harness's fake relay, and the
login-screen button visibility in Playwright.

---

# Listing and revoking eve passkeys

## Decisions

**8. Eve reports, relay mirrors, never the other way round.** Relay cannot
read `auth.json` — it is eve's file, in eve's data dir, and a second reader
of a credential file is a second place it can be wrong. Instead eve sends
relay its credential list at startup and after every change (an enrolment, a
revocation it applied, a login that bumped `lastUsedAt`). Only display
metadata travels: id, label, created, last used. Never a public key, never a
counter. Relay stores the mirror in settings so the Passkeys tab and
`relay eve list` read it the way `relay login list` reads relay's own —
straight off disk, with the tray stopped.

**9. Revoke is relay's act, gated like its own.** `eve.passkey.revoke` sits
behind the presence gate exactly as `login.passkey.revoke` does. Relay
records the revocation as *pending* and notifies the console. It does not
touch eve; it cannot.

**10. Eve pulls, and checks at the moment that matters.** Eve asks relay for
pending revocations every 30 seconds and — the part that makes this a
security control rather than housekeeping — on every login attempt, before
it accepts the assertion. A revoked passkey therefore stops working on its
next use, whatever the poll timing. When eve applies a revocation it deletes
the credential, ends every session that credential minted, and re-reports;
the report is the acknowledgement (decision 12).

**11. Sessions know their parent.** Eve's session store records which
credential minted each token. That is what turns "revoke" into "and sign
that device out", the distinction relay's own tab already draws between a
passkey and the sessions it minted. Sessions minted before this change have
no parent and are left alone by revocation; they expire on their own.

**12. The report is the acknowledgement.** Relay drops a pending revocation
when a report arrives that no longer contains the id. One PUT is therefore
both "here is my list" and "I did what you asked", and there is no separate
ack that can be forgotten or replayed.

**13. The last eve passkey cannot be revoked from relay.** Relay's own tab
allows revoking to zero. Eve does not: an owned box with no passkey is only
reachable again through the first-enrolment path, which is loopback or
trusted subnet only — from a phone on the far side of a NAT that is a
lock-out. Relay refuses the revoke before the gate when it would leave the
mirror with nothing (counting revocations already pending). Eve enforces the
same rule when applying, and relay drops any pending revocation that a
report shows would empty the list, so the two sides can never disagree for
long. Deleting `auth.json` on the console remains the break-glass.

**14. Login checks fail open when relay is unreachable.** If eve cannot ask
relay during a login, it logs loudly and accepts the assertion. Relay being
down already makes eve nearly useless (sessions live behind it), and a hard
fail here would turn a relay restart into a lock-out on every device. The
30-second poll catches up as soon as relay is back.

## Relay: the mirror and the revocation

### Settings

```jsonc
"eve_passkeys": [
  { "id": "…", "label": "Mozilla/5.0 (iPhone…)", "created": "2026-09-07T10:12:31Z",
    "last_used": "2026-09-07T18:02:11Z", "reported": "2026-09-07T18:02:12Z" }
],
"eve_passkey_revocations": [
  { "id": "…", "requested": "2026-09-07T18:05:00Z" }
]
```

Both `omitempty`. `reported` is when relay last heard about this credential,
so a stale mirror (eve down for a week) is visibly stale in the tab.

### Core: `EvePasskeyOps`

`Store`, `Audit`, `Gate`, `OnChange`, `Notify`; nil-safe. Five methods.

| Method | Gate | Effect |
|---|---|---|
| `Report(list)` | no | Replaces `eve_passkeys` (stamping `reported`), drops every pending revocation whose id is absent from `list` or whose id is the only entry in `list`. Fires `OnChange`. |
| `List()` | no | The mirror, each entry with `revocation_pending: bool`. |
| `Revocations()` | no | Pending ids. |
| `Revoke(ctx, id, via)` | **yes** — `eve.passkey.revoke` | Refuses before the gate if `id` is not in the mirror, is already pending, or is the last non-pending credential. Otherwise appends a pending revocation, records it in the audit log, notifies the console. |
| `Unrevoke(id)` | no | Withdraws a pending revocation eve has not yet applied. Narrowing nothing, so ungated, like unregistering an MCP. |

Presence prompt reason for `eve.passkey.revoke`: `revoke the Eve passkey %s`
where `%s` is the abbreviated credential id (`abbreviatePasskeyID`). Add it
to `docs/presence-gate.md` and the prompt table in `docs/cli.md`.

Refusal text for the last credential: `the last Eve passkey cannot be revoked
from relay; enrol another browser first, or delete eve's auth.json on the
console to start over`.

### Doors

- **Passkeys tab** — a second section, *Eve passkeys*, under relay's own:
  label, short id, created, last used, and a *Revoke* button per row. A row
  with a pending revocation shows *revocation pending* in place of the
  button until eve's next report removes it. First paint carries the list
  as `__EVE_PASSKEYS_JSON__`; live updates ride the existing
  `onPasskeysReloaded` emit, which gains the eve list as a third argument.
  IPC: `revoke_eve_passkey` (gated, off the main thread like
  `revoke_passkey`), answering `onEvePasskeyRevoked` / `onPasskeyError`.
- **CLI** — `relay eve list` (reads settings directly; columns LABEL,
  CREDENTIAL ID, CREATED, LAST USED, STATUS where STATUS is `-` or
  `revocation pending`) and `relay eve revoke --id ID`, brokered over
  `admin_op` (`eve.passkey.revoke`). Revoke prints that the passkey will
  stop working on its next use and that eve signs its sessions out when it
  applies the revocation.
- **HTTP (frontend socket, for eve)**

  | Route | Class | Body / response |
  |---|---|---|
  | `PUT /api/eve/passkeys` | `configure` | body `{"passkeys": [{id, label, created, last_used}]}` → `200 {"revocations": [ids]}` (the still-pending set after this report, so eve learns of new revocations on the same round-trip) |
  | `GET /api/eve/passkeys/revocations` | `read` | `200 {"revocations": [ids]}` |

Relay notifies the console on `Revoke` — *Eve passkey revoked: it stops
working on its next use* — and again when a report confirms it is gone.

## Eve: applying revocations

### `auth.json` and sessions

Each credential gains `lastUsedAt` (stamped on every successful login).
`sessions.json` entries gain `credentialId` (absent for tokens minted before
this change). `SessionStore.create(credentialId)` records it;
`SessionStore.revokeByCredential(credentialId)` deletes every token minted by
it and returns how many.

`AuthService.removeCredential(id)` deletes the credential, ends its sessions,
saves, and returns `{ removed, sessionsEnded }`. It refuses to remove the
last credential (decision 13).

### `PasskeySync` (`passkey-sync.js`)

The only eve code that knows the two routes above. It owns:

- `report()` — PUT the current list; apply whatever revocations come back;
  called at startup, after every `addCredential` / `removeCredential` /
  login, and on the 30-second poll timer (`.unref()`'d).
- `checkRevoked(credentialId)` — GET revocations; `true` if the id is
  pending. Used by the login route before accepting an assertion. On a relay
  error: log at error level and return `false` (decision 14).
- `apply(ids)` — for each id present in `auth.json` and not the last
  credential: `removeCredential`, log which sessions ended. Then `report()`
  once, which is the acknowledgement.

With no `RelayTransport`, every method is a no-op that reports nothing.

### Login route

`login/finish`: resolve the credential id from the assertion, then
`checkRevoked(id)` **before** `verifyLogin`. Pending → apply the revocation
and answer `401 {"error": "This passkey has been revoked."}`. Otherwise
verify as today and mint the session with its `credentialId`.

## Operator flow

1. Settings → Passkeys → *Eve passkeys*, or `relay eve list`. Every browser
   that can sign in to eve, with when it last did.
2. *Revoke* on a row, answer the presence prompt. The row says *revocation
   pending*.
3. Within 30 seconds — or immediately, if that browser tries to sign in —
   eve deletes the credential and signs that browser out. The row
   disappears and the console gets a notification.

## Tests

Relay: `EvePasskeyOps` (report replaces and stamps; report drops absent and
last-standing revocations; revoke refuses unknown / pending / last; revoke
under a concurrent burst leaves exactly the expected pending set; unrevoke),
route classes and shapes, admin op, CLI list/revoke output, the IPC door, and
the structural/doc guards.

Eve: `SessionStore` parent tracking and `revokeByCredential`;
`AuthService.removeCredential` including the last-credential refusal;
`PasskeySync` against a fake transport (report body shape, applies returned
revocations, never removes the last, `checkRevoked` fails open on error, no
transport → no-op); the login route refusing a pending id before
verification; an integration test through the harness's fake relay covering
enrol → report → revoke → next login refused → sessions gone.
