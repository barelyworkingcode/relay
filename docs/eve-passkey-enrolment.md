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

**7. No revoke, no list, in this change.** Eve has never had a way to remove a
credential other than deleting `auth.json`; that is unchanged here. The
credential record now carries `createdAt` and a `label` (the enrolling
browser's User-Agent, truncated) so a later listing surface has something to
show.

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

  Eve's legacy frontend credential grants `read`, `configure` and `proxy`, so
  both are reachable with `RELAY_FRONTEND_TOKEN` as-is. They are registered
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
