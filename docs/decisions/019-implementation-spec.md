# ADR-019 Implementation Spec: Registration Is One Command and One Comparison

**Status:** Specification. Implements ADR-019 decisions **1–7** across two repositories.
**Date:** 2026-08-30
**Reads with:** [ADR-019](019-registration-is-one-command.md) — but is written so a developer can pick up one build-order step and work without reading it.

This document is the build instruction. Where it and ADR-019 disagree on a *fact*, this document has been measured against the code and the ADR has not; §9 lists every such case. Where they disagree on a *decision*, ADR-019 wins and this is a bug — **with one exception, §9.1, where ADR-019 §3's security argument does not hold and this spec changes the protocol to make the ADR's own claim true.** Read §9.1 before writing any code.

---

## 1. Summary and the end-to-end flow

### 1.1 In one paragraph

`relayremote register "Hermes Mail" --host 192.168.64.1` generates a P-256 key that never leaves the machine, lodges its CSR on relay's existing plain-TCP enrolment-request listener together with a **hiding commitment** to a fresh 128-bit client nonce, receives back relay's public CA certificate and a fresh 128-bit relay nonce, and immediately prints a six-character **short authentication string** derived from both public keys and both nonces. Relay's tray raises one coalesced, rate-limited notification; the operator opens Settings → Remote Clients, sees the same six characters beside the request, picks an access profile (required), answers the unchanged `enrolment.sign` presence prompt, and relay signs. The client collects the certificate, re-checks that the CA it is handed is byte-identical to the one it computed the code over, files the whole thing into a named registration under `~/.config/relayremote/`, and closes by reporting **what the grant actually reaches** — the profile names and the tool list — rather than "done".

### 1.2 The end-to-end flow, step by step

| # | Where | What happens | Wire |
|---|---|---|---|
| 1 | client | `register "Hermes Mail" --host 192.168.64.1`. Slug `hermes-mail`. Directory `~/.config/relayremote/registrations/hermes-mail/` created 0700; `client.key` 0600 generated; CSR built. | — |
| 2 | client | Picks `R_C` (16 random bytes). Computes `commit = SHA256("relay.sas.commit.v1\x00" ‖ SPKI_sha256(csr) ‖ R_C)`. Writes `pending.json` 0600. | — |
| 3 | client → host | `EnrolmentRequest{csr_pem, label:"hermes-mail", sas_commit, requested_profile?}` | lodge |
| 4 | host | Table inserts a row, mints `R_R` (16 random bytes), answers with `request_id`, `spki_sha256`, `poll_after_seconds`, `expires_in_seconds`, **`ca_pem`**, **`sas_nonce` = R_R**. Generation counter increments. | lodge ack |
| 5 | client | Computes `SAS = enc30(SHA256("relay.sas.v1\x00" ‖ H(CA SPKI) ‖ H(CSR SPKI) ‖ R_C ‖ R_R))`. **Prints `code: 7K3P4Q · waiting for approval…`** and records it in `pending.json`. | — |
| 6 | client → host | First `EnrolmentRequestPoll{request_id, sas_open: R_C}`. Host verifies the commitment, records `R_C` once, computes the same SAS, marks the row `sas_ready`. | poll |
| 7 | host | Tray tick (≤2 s later) sees the generation moved, raises **one** notification: *"Relay — 1 machine is waiting to be registered."* | — |
| 8 | host | Operator opens Settings → Remote Clients. Row shows `7K3P4Q` large, the key hash in full, the label marked *supplied by the requesting machine*, and the requested profile marked *a request, not a grant*. **Approve…** opens the sheet. | — |
| 9 | host | Sheet: client id pre-filled with a collision-free suggestion, **an access profile must be chosen** (or "enrol with no access for now" ticked explicitly), budget, then **Approve & issue certificate** → the unchanged `enrolment.sign` presence prompt → Touch ID → signed. | — |
| 10 | client | Next poll returns `approved` with `client_id`, `project_ids`, `relay_addr`, `cert_pem`, `ca_pem`. Client runs five checks (§5.6), writes `client.crt`, `ca.crt`, `registration.json`, deletes `pending.json`. | poll |
| 11 | client | Dials the tool plane, `ListTools` per granted project, prints what the grant reaches. | mTLS |
| 12 | client | `relayremote call --tool mail_search --args '{…}'` — no `--as` needed, this is the only registration. | mTLS |

### 1.3 `register` is a new verb; `request` is untouched

**Decided: `register` is added alongside `request`. `request` is not deprecated in this slice and its behaviour does not change by one byte.**

The reasoning is the reasoning `enrol_request.go`'s own doc comment already gives for `request` not being a flag on `enrol`: these are two different trust ceremonies over two different local stores, and folding them into one verb puts a flag between "the pin was carried by hand" and "the pin was replaced by a comparison" — a flag someone defaults wrong.

Concretely, they differ in four ways at once, and any one of them would be enough:

| | `request` | `register` |
|---|---|---|
| local store | a `--bundle` directory the caller names | a named registration under `~/.config/relayremote/` |
| server authentication | `--ca-fingerprint` carried out of band, or `--tofu` | the SAS comparison (or `--ca-fingerprint`, non-interactively) |
| wire | no commitment, no nonce | commitment + nonce + open |
| closing output | "enrolled as X, use it with --bundle …" | what the grant reaches |

`request` remains the documented path for anyone who already has a bundle directory wired up, and it is the path that still works against a relay that predates this change. `register` is the recommended path in `README.md` and in `docs/access-profiles.md`. A later ADR may retire `request`; this one does not.

The operator-carried path — `relayremote enrol`, `relay enrol sign`, `relayremote install` — is **not touched at all**. Not one line of `enrol.go` changes behaviour. It is the fallback that needs no listener and no comparison.

---

## 2. Wire protocol changes

All changes are on the **enrolment-request listener only** (`enrolment_request_server.go`, `bridge/enrolment_request.go`). Nothing on the tool plane changes. `remoteHandlers` stays at two entries, `remoteConfigHandlers` stays at two, and `enrolmentRequestHandlers` **stays at exactly two** — the SAS open rides on `EnrolmentRequestPoll` rather than becoming a third request type. That is a deliberate cost (Poll gains a bounded, single-use, self-verifying write) paid to keep a two-entry table that is visibly a security boundary; see §9.3.

### 2.1 `EnrolmentRequest` (lodge) — client → host

```go
// bridge/enrolment_request.go
type EnrolmentRequestWire struct {
    Type   string `json:"type"`
    CSRPEM string `json:"csr_pem"`
    Label  string `json:"label,omitempty"`

    // SASCommit is a hiding commitment to the client's SAS nonce:
    // lowercase hex of SHA256("relay.sas.commit.v1\x00" || spki || R_C),
    // exactly 64 characters. Present means "this is a `register`"; absent
    // means "this is a `relayremote request`" and the row gets no SAS.
    SASCommit string `json:"sas_commit,omitempty"`

    // RequestedProfile is a HINT, displayed to the operator as a request
    // and never honoured automatically (ADR-019 §7). Hostile input:
    // isSafeID charset, <= 64 bytes, same guard as Label.
    RequestedProfile string `json:"requested_profile,omitempty"`
}
```

Decode stays `DisallowUnknownFields`. Two new refusals inside `DecodeEnrolmentRequest`, before anything reaches the table:

- `sas_commit`, if present, must match `^[0-9a-f]{64}$`. Anything else → `CodeInvalidParams`, message `sas_commit must be 64 lowercase hex characters`.
- `requested_profile`, if present, must satisfy the same rule as `label` (`^[A-Za-z0-9._-]{1,64}$`).

### 2.2 Lodge result — host → client

```go
type enrolmentRequestLodgeResult struct {
    RequestID        string `json:"request_id"`
    SPKISHA256       string `json:"spki_sha256"`
    PollAfterSeconds int    `json:"poll_after_seconds"`
    ExpiresInSeconds int    `json:"expires_in_seconds"`

    // CAPEM is relay's CA certificate, PEM. Returned ONLY when the lodge
    // carried sas_commit. Public by construction (ADR-018 §8 P2), so
    // returning it to an unauthenticated peer leaks nothing — and the
    // client cannot print the comparison code without it.
    CAPEM string `json:"ca_pem,omitempty"`

    // SASNonce is R_R: 32 lowercase hex characters, minted per row by
    // relay AFTER the client's commitment is in hand. This is what stops
    // the attacker choosing the value the host will display.
    SASNonce string `json:"sas_nonce,omitempty"`
}
```

**`ca_pem` is returned at lodge time, not at collection time.** This resolves the open question, and the justification has three parts: (a) P2 — the CA certificate is public and relay hands it to anyone who collects an approval already, so early delivery leaks nothing new; (b) the client must print the code *before* the human walks to the Mac, otherwise the human arrives at a screen showing a value with nothing to compare it to and has to walk back; (c) the commit–reveal construction in §3 makes early disclosure harmless — an attacker holding relay's CA early still cannot compute the target, because it must commit before it learns `R_C`.

If `sas_commit` was supplied and relay holds no CA certificate, the lodge is **refused**. "Holds" is the material the reconcile tick last pushed into the table, not a read of disk — the table structurally cannot reach the filesystem, which is what keeps it holding nothing that can sign. A CA created between ticks is therefore invisible for at most one settings poll, which fails closed and is harmless. The lodge is refused (`CodeInternalError`, naming `relay enrol create` as the one-time fix). It is never answered with an empty `ca_pem`: a client that received no CA would either have to abort anyway or, worse, silently fall back to something weaker. A lodge without `sas_commit` is unaffected and still succeeds — `request` does not need the CA at lodge.

### 2.3 `EnrolmentRequestPoll` — client → host

```go
type EnrolmentRequestPollWire struct {
    Type      string `json:"type"`
    RequestID string `json:"request_id"`

    // SASOpen is R_C, 32 lowercase hex characters: the opening of the
    // commitment lodged earlier. Sent on the FIRST poll and on any retry
    // of it. Accepted at most once per row; a second, different value is
    // refused rather than overwriting.
    SASOpen string `json:"sas_open,omitempty"`
}
```

`sas_open`, if present, must match `^[0-9a-f]{32}$`, checked at decode.

### 2.4 Poll result — host → client

Shape is **unchanged**. In particular:

> **The poll response never carries the SAS.** Relay must not echo the code it displays. If it did, a man-in-the-middle would simply forward relay's value to the client and the comparison would be a comparison of one number with itself. This is an acceptance criterion (AC-14), not a style note.

### 2.5 Open-verification semantics on the host

`Poll(requestID, sasOpen)` on `enrolmentRequestTable`, under the table lock, after the sweep:

| row state | `sas_open` | behaviour |
|---|---|---|
| no `sasCommit` (legacy `request` row) | absent | unchanged |
| no `sasCommit` | present | refuse, `CodeInvalidParams`: *this request was not lodged with a comparison commitment* |
| has `sasCommit`, no `sasOpen` yet | absent | answer normally; row stays `sas_ready:false` |
| has `sasCommit`, no `sasOpen` yet | present, commitment verifies | record `sasOpen` once; row becomes `sas_ready:true`; answer normally |
| has `sasCommit`, no `sasOpen` yet | present, commitment fails | set `sasFailed`; refuse the poll, `CodeInvalidParams`. **`sasFailed` is terminal for opens**: a later correct opening does not rescue the row, or one blind guess would become a grind. A plain poll on a failed row still answers normally |
| has `sasOpen` | identical value | idempotent; answer normally (a redialled poll must not fail) |
| has `sasOpen` | different value | refuse; do not overwrite |

Verification is `SHA256("relay.sas.commit.v1\x00" ‖ spkiSHA256Bytes ‖ R_C) == sasCommit`, where `spkiSHA256Bytes` are the raw 32 bytes relay already computes from the *stored* CSR. Binding the SPKI into the commitment is what stops a MITM replaying a captured commitment against a different key.

### 2.6 Idempotent re-lodge with a commitment

The existing SPKI-keyed idempotent re-lodge (which is what makes "flood the table with one key" impossible) gains three rules:

- existing row has `sasCommit` **X**, new lodge carries **X** → return the existing row's `request_id`, `ca_pem` and `sas_nonce` unchanged. This is the resume path and it must produce the same code.
- existing row has `sasCommit` **X**, new lodge carries **Y ≠ X** → refuse. A key already has one live comparison bound to it; letting a second commitment attach would hand an attacker a second free attempt.
- existing row has no `sasCommit`, new lodge carries one (or vice versa) → refuse, naming the mismatch. A legacy row is never upgraded in place.

### 2.7 Backward and forward compatibility

- **Old client, new relay:** `relayremote request` sends no new fields; every new field is `omitempty` on the way out and optional on the way in. Unchanged.
- **New client, old relay:** `register` sends `sas_commit`, an old relay's `DisallowUnknownFields` rejects it with a clear decode error. The client detects this specific shape and prints: *this relay does not support `register`; use `relayremote request --ca-fingerprint …`, or update relay*. This is the intended coordination cost ADR-010 §4 already accepted for strict decoding.
- **`--bundle` and `RELAY_REMOTE_BUNDLE`:** entirely untouched by anything in §2. They do not participate in this protocol.

---

## 3. The SAS: algorithm, alphabet, bit count, threat justification

### 3.1 The algorithm, byte for byte

Both repositories implement this identically, in `enrolment_sas.go` (relay) and `sas.go` (relayRemote), and both are pinned to a **shared test-vector file committed byte-identically in both trees** (`testdata/sas_vectors.json`). That file is the contract; a divergence between the two implementations is otherwise invisible until an operator is staring at two different codes and concludes the network is under attack.

```
H            = SHA-256
domain       = "relay.sas.v1\x00"          (13 bytes, including the NUL)
commitDomain = "relay.sas.commit.v1\x00"   (20 bytes, including the NUL)

caSPKI   = the CA certificate's RawSubjectPublicKeyInfo  (DER)
csrSPKI  = the lodged CSR's RawSubjectPublicKeyInfo      (DER)
R_C      = 16 bytes, crypto/rand, generated by the client
R_R      = 16 bytes, crypto/rand, generated by relay per row

commit  = H( commitDomain || H(csrSPKI) || R_C )                    -> 32 bytes
digest  = H( domain || H(caSPKI) || H(csrSPKI) || R_C || R_R )      -> 32 bytes
SAS     = enc30( digest )                                           -> 6 characters
```

**Every field in the preimage is fixed-length** — two 32-byte hashes and two 16-byte nonces after a fixed-length domain tag — so there is no concatenation ambiguity and no length prefix is needed. Hashing each SPKI first, rather than concatenating the variable-length DER directly as ADR-019 §3 writes it, is a strictly stronger framing and a deliberate deviation; it also means both repositories reuse a digest they already compute (`SPKISHA256Hex` on relay, `keySPKIFingerprint` on the client), which is the single biggest reduction in cross-repo divergence risk available.

**Truncation — 30 bits, big-endian, from the front:**

```go
v := uint32(digest[0])<<22 | uint32(digest[1])<<14 | uint32(digest[2])<<6 | uint32(digest[3])>>2
// v now holds the first 30 bits of the digest in its low 30 bits.
for i := 5; i >= 0; i-- { out[5-i] = sasAlphabet[(v>>(5*uint(i)))&31] }
```

**Alphabet — 32 symbols, uppercase only:**

```
sasAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
```

Eight digits (`2`–`9`) and twenty-four letters (`A`–`Z` less `I` and `O`). This removes every member of the `0`/`O` and `1`/`I`/`l` confusion classes: `0` and `1` are absent, `I` and `O` are absent, and lowercase never appears, so `l` cannot. `L` survives and is unambiguous because `1` and `I` are both gone. Rendered as six characters with no separator and no lowercase, on both screens: `7K3P4Q`.

### 3.2 Where it is computed

| | inputs it holds | when |
|---|---|---|
| **client** | `caSPKI` from the `ca_pem` in the lodge ack; `csrSPKI` from its own CSR; `R_C` it generated; `R_R` from the lodge ack | immediately on the lodge ack, before the first poll. Printed at once. |
| **host** | `caSPKI` from `ca.crt` on disk; `csrSPKI` from the **stored** CSR; `R_C` from the verified open; `R_R` it minted | at `PendingRequests()` projection time, i.e. every time the operator surface renders. Absent (`sas_ready:false`) until the open arrives. |

The host computes it lazily at projection rather than storing it, so the CA it displays is always the CA currently on disk. A CA regenerated mid-flight makes the codes differ, which is the correct outcome.

### 3.3 The threat justification — and why the naked construction is not enough

ADR-019 §3 asserts that six characters suffice because "the attacker gets one guess per approval against a value it cannot observe … it does not become an offline grind: the target lives on the host's screen."

**That argument does not hold for the construction the ADR literally writes.** Under `SAS = truncate(H(CA_SPKI ‖ CSR_SPKI))`, both inputs are public and both are known to a network attacker:

- `CSR_SPKI` — the attacker is on the path and reads the CSR.
- `CA_SPKI` — relay hands its CA certificate to anyone who lodges (that is P2, and it is true whether or not this spec returns it at lodge time).

So the attacker computes the host-side target exactly, and then grinds its **own** CA keypair until `H(CA_attacker ‖ CSR_client)` collides with it on 30 bits. Each candidate costs one EC point addition, one DER marshal and one SHA-256 — roughly 1 µs single-core. 2^30 ≈ 1.07 × 10^9 candidates is **about 18 minutes on one core, under a minute on a laptop's worth of cores, and seconds on a GPU.** The `register` client waits ten minutes by default and the row lives fifteen. The naked six-character SAS is defeated inside the window it exists to protect.

**The fix is the commitment, and it is what makes the ADR's own claim true.** Vaudenay's SAS construction: neither party may be able to compute the comparison value while it still has freedom to choose an input.

- The attacker must fix `CA_attacker` and the `R_R'` it forwards **before** it learns `R_C` (the client reveals `R_C` only in the poll, after the ack).
- The attacker must fix its committed `R_M` **before** it learns relay's `R_R` (relay mints `R_R` only after the commitment is lodged).

So the client-side value depends on `R_C`, which the attacker does not know when its last free choice is made; and the host-side value depends on `R_R`, which the attacker does not know when its commitment is made. Neither side is grindable. The attacker is reduced to one blind guess per lodged row: **2^-30 ≈ 1 in 1.07 billion.**

**How many rows can the attacker hold?** The pending table caps live rows at 8, refuses rather than evicts, throttles one lodge per source host per 10 s, and rides the global `ceremonyLimiter`. A row lives 15 minutes and the attacker cannot delete it, so it cannot churn faster than the TTL. The realistic ceiling is **8 concurrent candidate rows**, giving 8 × 2^-30 ≈ **2^-27**, or about 1 in 134 million per approval ceremony. Eight simultaneous pending rows is also not subtle: the tray line says `Pending enrolment requests: 8` and the panel lists all eight.

That is the bound this spec claims, and it is the bound the ADR's Bluetooth-numeric-comparison analogy actually describes — Bluetooth NC gets its security from commitment too, not from the shortness of the number.

**Which substitutions this defeats, restated with the nonces:**

| attack | host displays | client computes | why they differ |
|---|---|---|---|
| attacker lodges its own key | `f(CA_relay, SPKI_att, R_M, R_R)` | `f(CA_att, SPKI_client, R_C, R_M')` | three of four inputs differ; `R_C` unknown when `CA_att`, `R_M'` were fixed |
| attacker forwards the real CSR, substitutes its CA on return | `f(CA_relay, SPKI_client, R_M, R_R)` | `f(CA_att, SPKI_client, R_C, R_M')` | `CA` differs and cannot be ground: the target depends on `R_C` |
| attacker re-lodges after learning `R_C`, grinding a fresh `R''` | `f(CA_relay, SPKI'', R'', R_R'')` | fixed already | `R''` must be committed before `R_R''` is minted |
| attacker replays a captured commitment under a different key | — | — | the commitment binds `H(csrSPKI)`; verification fails |

**Why SPKI and not the whole certificate.** The SAS binds the CA's *public key*, while `--ca-fingerprint` binds the whole certificate DER. An attacker cannot exploit the difference: to make a leaf verify, the CA certificate it presents must carry the public key that signed that leaf, and it does not hold relay's CA private key. Chain verification (check 3 in §5.6) plus SPKI binding closes it. Using SPKI also keeps the code stable across a cosmetic CA-certificate re-issue over the same key.

### 3.4 The SAS is mandatory; the flags that interact with it

| invocation | behaviour |
|---|---|
| `register …`, stdin is a TTY | code printed at lodge; at collection the client **requires an interactive `y` answer** to *"the Mac showed `7K3P4Q`. Did it match? [y/N]"*. `n`, EOF, or anything else → nothing is written, exit 2. |
| `register …`, stdin is not a TTY, no `--ca-fingerprint` | **refused at flag parse**, naming `--ca-fingerprint` and where to read it. No enrolment is attempted. |
| `register --ca-fingerprint sha256:…` | the pin is checked programmatically against the collected `ca_pem` and is authoritative. The SAS is still computed and **displayed**, but no confirmation is read. |
| `register --tofu` | **refused at flag parse.** `--tofu` does not exist on `register`; the message names the SAS as what replaced it and `relayremote request --tofu` as where it still lives. |
| `request --tofu` / `request --ca-fingerprint` | unchanged in every respect. |

`--ca-fingerprint` is not "a flag that skips the check". It is a *stronger* check supplied out of band — exactly what ADR-019 §3 means by "a non-interactive caller must supply `--ca-fingerprint` exactly as before". Prompting a human to eyeball a code a machine has already proven equal is how you train people to answer `y` without looking.

**And the host enforces the comparison independently of the client.** `EnrolmentOps.Approve` refuses a row whose `sasCommit` is set but whose `sasOpen` is missing or failed (`errEnrolmentRequestSASIncomplete`), from **every** door — CLI, IPC, HTTP. A client that lodged a commitment and then never opened it cannot be approved by an operator clicking past a disabled button.

---

## 4. Host-side changes, file by file

### 4.1 `enrolment_sas.go` — new

Pure functions, no store, no CA object, no filesystem:

```go
const sasAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
const sasNonceBytes = 16

func newSASNonce() (string, error)                       // 32 lowercase hex
func sasCommitment(csrSPKISHA256 [32]byte, rc []byte) string
func computeSAS(caSPKI, csrSPKI, rc, rr []byte) string   // 6 chars
func validSASHex(s string, wantBytes int) bool
```

Mirrored exactly in relayRemote's `sas.go`. Both read the same `testdata/sas_vectors.json`.

### 4.2 `enrolment_ca.go` — modified

Add, beside `caFingerprintFromDisk`, in the same "read commands work with the tray stopped, no sealer" style:

```go
func caCertPEMFromDisk() ([]byte, error)   // ca.crt bytes, verbatim
func caSPKIFromDisk() ([]byte, error)      // parsed cert's RawSubjectPublicKeyInfo
```

Neither touches `ca.key.sealed`. The CA certificate is public and stored clear.

### 4.3 `enrolment_requests.go` — modified

`enrolmentRequestRecord` gains, all set at most once and never mutated afterwards except as noted:

```go
sasCommit        string   // set at Lodge, immutable
sasNonce         string   // R_R, minted at Lodge, immutable
sasOpen          string   // R_C, set once by Poll after verification
sasFailed        bool     // set once by Poll on a commitment mismatch
requestedProfile string   // set at Lodge, immutable, hostile input
```

`enrolmentRequestTable` gains `caCertPEM []byte` and `caSPKI []byte` — **plain byte slices, set by `SetCACert(pem, spki []byte)`, never a `*RelayCA` and never a closure over one.** The table's structural proof (it holds nothing that can sign, read settings, or unseal) is unchanged and AC-31's reflection test still passes untouched. It also gains:

```go
func (t *enrolmentRequestTable) LodgeGeneration() uint64
```

a monotonic counter incremented **only** when a Lodge actually inserts a row (never on an idempotent re-lodge, a refusal, a throttle or a poll). This is the entire mechanism by which the tray learns something arrived — a **pull**, not a push. Nothing in this file gains a callback field, a channel, or a reference to anything UI-shaped; see §4.9.

`lodged` gains `CAPEM`, `SASNonce`. `Lodge` gains `requestedProfile, sasCommit` parameters and applies §2.6's three rules. `Poll` gains a `sasOpen` parameter and applies §2.5's table. `enrolmentRequestView` gains:

```go
SAS              string  // "" until sas_ready
SASReady         bool
SASFailed        bool
RequestedProfile string
IsLegacyRequest  bool    // no sasCommit: a `relayremote request` row
```

`List()` fills `SAS` by calling `computeSAS(t.caSPKI, csrSPKI, rc, rr)` when the row is ready. `Get()` is extended to also return `SASCommit`/`SASOpen`/`SASFailed` on `pendingRecordView` so `Approve` can enforce §3.4's server-side refusal.

Bounds unchanged: 8 rows, 15-minute TTL, 15-minute collect TTL, 16 connections, 64 frames, 64 KiB frames, 10 s per-source throttle.

### 4.4 `enrolment_request_server.go` — modified

`EnrolmentRequestSink` widens to carry the new parameters:

```go
type EnrolmentRequestSink interface {
    Lodge(csrPEM []byte, label, requestedProfile, sasCommit, remoteAddr string) (lodged, error)
    Poll(requestID, sasOpen string) (pollResult, error)
}
```

Still exactly two methods, still exactly two handlers in `enrolmentRequestHandlers`. `enrolmentRequestLodgeResult` gains `ca_pem` and `sas_nonce`. `enrolmentRequestPollResult` is unchanged. This file continues to import neither `crypto/tls` nor `relaygo/presence`.

### 4.5 `remote_reconcile.go` — modified

In `reconcileEnrolmentListenerLocked`, immediately before the bind (and on every tick, so a break-glass CA regeneration is picked up):

```go
if pem, spki, err := caMaterialFromDisk(); err == nil {
    sup.enrolTable.SetCACert(pem, spki)
} else {
    sup.enrolTable.SetCACert(nil, nil)   // register lodges refuse; request lodges unaffected
}
```

Ordering is safe: `resolveEnrolment` already refuses `enrolment_requests: true` with `enabled: false`, so the tool-plane listener is always converged first in the same tick and `NewRemoteServer`'s `LoadOrCreateCA` has already created `ca.crt`.

### 4.6 `enrolment_ops.go` — modified

- `Approve` refuses before the gate when `rec.SASCommit != "" && (rec.SASOpen == "" || rec.SASFailed)`, with `errEnrolmentRequestSASIncomplete` and an operator-facing message naming what happened. Before the gate, deliberately: an operator should not be asked for Touch ID for an act that is going to be refused.
- The presence digest is **unchanged**. It already binds `csr_spki_sha256`, and the CA is fixed, so binding the SAS as well would add nothing an attacker could exploit. Do not touch `enrolmentSignFields.presenceDigest`.
- New helper `suggestClientID(s *Settings, label string) string`: `label` if free, else `label-2`, `label-3`, … to `-99`, else `""`. Advisory only — `ValidateEnrolment`'s uniqueness check inside `store.With` stays the authority, and a race loses loudly with the existing message.
- `PendingRequests()` passes through the new view fields unchanged.

### 4.7 `enrol_cmd.go` — modified

- `relay enrol requests` gains a `SAS` column. Legacy rows show `-`; unopened rows show `(waiting)`; failed rows show `FAILED`.
- **`relay enrol approve` requires a grant.** With no `--grant` it refuses, naming `--no-grant` as the explicit way to enrol a machine with no access. This is ADR-019 §7's "never the silent default", applied to the CLI door as well as the sheet. It is a behaviour change to an existing command; the failure is loud and the fix is one flag.
- `relay enrol ca-fingerprint` unchanged.

### 4.8 `ipc_enrolments.go` — modified

`pendingEnrolmentRequestView` gains `sas`, `sas_ready`, `sas_failed`, `requested_profile`, `is_legacy_request`, `suggested_client_id`. **It gains no field that could carry the CSR** — `TestPendingEnrolmentRequestView_CarriesNoCSRBytes` keeps passing unmodified, and the new fields must be added in a way that keeps it passing.

### 4.9 `trayapp.go` + `tray_notify.go` — modified / new

**The notification is raised by the tray's existing 2-second poll, never by the lodge path.** This is the whole design and it is what preserves ADR-018 §8 P1's structural proof intact: `Lodge` still cannot reach anything but a map, and the *only* new fact is that a counter it increments is later *read* by code that already runs on a timer.

`tray_notify.go`, pure Go and unit-testable with an injected clock:

```go
type pendingEnrolmentNotifier struct {
    now      func() time.Time
    notify   func(title, body string)
    lastGen  uint64
    lastAt   time.Time
    inWindow []time.Time    // rolling hour
}

func (n *pendingEnrolmentNotifier) tick(gen uint64, unapproved, live int, settingsOpen bool)
```

Rules, and these are the concrete numbers:

1. **Only on a generation increase.** `gen == n.lastGen` → silence. An idempotent re-lodge, a poll, a refusal and a throttle all leave the generation alone, so none of them can produce a banner.
2. **Coalesced.** One notification regardless of how many rows are pending; the text names the count.
3. **`notifyMinInterval = 60s`** between notifications.
4. **`notifyMaxPerHour = 6`** in a rolling hour. On exceeding it, `n.lastGen` is still advanced — the flood is swallowed, not queued.
5. **Silent when the table is full:** `live >= maxPendingEnrolmentRequests` → no notification. A full table refuses lodges, so nothing new arrived to approve; this is the ADR's "once the bounded pending table is full, lodging is refused and no further notification is raised."
6. **Silent when the Settings window is open** (`a.settingsOpen.Load()`): the panel already updates live via `onEnrolmentRequestsChanged`, and a banner over the window you are already looking at is noise.
7. **Silent when `unapproved == 0`.**

Worst case for an attacker with an open enrolment port: **6 banners per hour, permanently**, each dismissible, each blocking nothing. That is the bounded cost ADR-019 §4 writes down.

Call site: one line inside `updateMenuWithSettings`, next to the existing `countUnapprovedEnrolmentRequests` call, so the count and the notification are derived from the same read.

`onMenuClick`'s `menuIDPendingEnrolments` arm and the notification-click callback both do the same thing and nothing else: `openSettingsWindow()` plus an emit that selects the Remote Clients page. Neither carries a request id and neither calls a gated method.

### 4.10 `platform.go`, `cocoa_darwin.{go,h,m}` — modified

```go
type Platform interface {
    …
    Notify(title, body string)
}
```

```c
void cocoa_notify(const char* title, const char* body);
extern void goOnNotificationClick(void);
```

Implementation notes that will otherwise cost a day:

- Use `UNUserNotificationCenter`. Request `UNAuthorizationOptionAlert` **once**, lazily, on the first `cocoa_notify`; cache the result.
- `UNUserNotificationCenter.currentNotificationCenter` **throws** for a process with no bundle identifier. Relay is normally `/Applications/Relay.app`, but it is also run bare from a terminal during development and under `go test`. Guard on `[[NSBundle mainBundle] bundleIdentifier] != nil` and no-op otherwise, logging once at debug.
- Use a fixed notification identifier (`relay.enrolment.pending`) so a second delivery **replaces** the banner rather than stacking — coalescing at the OS layer as well as in the notifier.
- Set a `UNUserNotificationCenterDelegate` whose `didReceiveNotificationResponse` calls `goOnNotificationClick`.
- If the user has denied notifications in System Settings there is no banner and no way to force one. **The tray's `Pending enrolment requests: N` line stays the guaranteed surface** and is not removed.
- The notification body carries **neither the label nor the SAS**. The label is hostile input from an unauthenticated peer and a banner is not a place to render it; the SAS belongs on the approval sheet, next to the refuse button and the key hash, and often does not exist yet when the banner fires (the open may not have arrived).

Text:

> **Relay**
> *1 machine is waiting to be registered.*
> Open Settings → Remote Clients to compare its code and approve. Nothing is granted until you do.

Plural: *"3 machines are waiting to be registered."*

### 4.11 The seam guards — `enrolment_sas_seam_test.go`, new

Following `presence/presence_seam_test.go`'s house pattern (AST/source scan over non-test files, module root resolved from the test file's own location):

- **No push.** `enrolment_requests.go` and `enrolment_request_server.go` contain no reference to the identifiers `Notify`, `Platform`, `App`, `cocoa`, `EnrolmentOps`, `presence` — proven by AST scan, not by grep on a string that could appear in a comment.
- **No func fields.** `reflect.TypeOf(enrolmentRequestTable{})` has no field of `reflect.Func` kind other than the two existing test seams `now` and `rand`, both explicitly allowlisted by name. A callback field is how a push would arrive.
- **Notification is called from exactly one place.** An AST scan of every non-test `.go` file finds exactly one call site of `pendingEnrolmentNotifier.tick`, and it is inside `trayapp.go`.
- **AC-29 extended**, behaviourally: a real `presencetest.NewRecording` provider, the table driven through the full new surface — lodge with commitment, lodge without, idempotent re-lodge, mismatched commitment, poll with a good open, poll with a bad open, table filled to cap, expired and refilled, malformed CSR, oversized label, oversized `requested_profile` — and `recording.Calls()` must be **0**.

---

## 5. Client-side changes, file by file

### 5.1 The registration store on disk

```
~/.config/relayremote/                       0700
  config.json                                0644   {"version":1,"default":"hermes-mail"}
  registrations/                             0700
    hermes-mail/                             0700
      client.key                             0600
      client.csr                             0644
      client.crt                             0644
      ca.crt                                 0644
      registration.json                      0644
      pending.json                           0600   (only while in flight; deleted on success)
```

The root honours `XDG_CONFIG_HOME` when set, falling back to `~/.config`. The per-registration directory layout is **deliberately identical to a bundle directory**, so `--bundle ~/.config/relayremote/registrations/hermes-mail` also works and `LoadBundle` is reused unchanged.

`registration.json`:

```json
{
  "version": 1,
  "name": "hermes-mail",
  "display_name": "Hermes Mail",
  "client_id": "hermes-mail",
  "host": "192.168.64.1",
  "addr": "192.168.64.1:9910",
  "enrol_addr": "192.168.64.1:9911",
  "server_name": "",
  "ca_fingerprint": "sha256:41c7…",
  "fingerprint": "sha256:9f2a…",
  "project_ids": ["proj_mail"],
  "default_project": "proj_mail",
  "registered_at": "2026-08-30T14:03:24Z",
  "sas": "7K3P4Q"
}
```

> **`registration.json` is a cache of the host's answer, not a source of truth, and nothing in it is consulted for a security decision.** `project_ids` and `default_project` only choose which `project_id` string to put on the wire; relay re-checks the grant at call time (ADR-010 §3, point 3) and a stale record therefore fails closed with `denied`. `ca_fingerprint` is display only — TLS verification uses `ca.crt` on disk, exactly as `--bundle` mode does, and there is no code path where a value read from `registration.json` can relax a check. A missing, truncated or unparseable `registration.json` beside a valid key/cert/CA triple is a **usable** registration: the client warns, loads the bundle, and requires `--project` if the enrolment turns out to hold several grants.

`name` is the local handle (`--as NAME`, the directory name, the client's slug). `client_id` is what the host assigned. They are different columns in `registrations` and may differ.

### 5.2 `registration.go` — new

```go
type Registration struct { /* the JSON above */ }

func registrationsRoot() string
func listRegistrations() ([]Registration, error)   // sorted by name
func loadRegistration(name string) (*Registration, error)
func saveRegistration(r *Registration) error       // atomic, temp+rename
func setDefaultRegistration(name string) error
func defaultRegistrationName() string
func slugForName(display string) (string, error)   // lowercase [a-z0-9]+ joined by '-'
```

`slugForName("Hermes Mail")` → `hermes-mail`. Anything slugifying to the empty string is refused, naming the rule. Truncated to 64 bytes. The slug is what goes on the wire as `label`, so it satisfies the host's existing `^[A-Za-z0-9._-]{1,64}$` guard by construction — **no new hostile free-text field is added to the wire.** The display name stays local; the operator sees `hermes-mail` on the Mac, which is also the id they are about to assign.

### 5.3 Selection order — the precedence table

Implemented in `options.resolve()`, replacing today's "default the flag from the env" binding. `--bundle` and `--as` are bound with **empty** defaults and the environment is consulted afterwards, so `flag.FlagSet.Visit` can distinguish "explicitly passed" from "unset". That distinction is load-bearing and is where the compatibility bug will be if it is got wrong.

| # | condition | result |
|---|---|---|
| 0 | `--bundle` and `--as` both explicitly passed | **error**, exit 2: *pass one or the other* |
| 1 | `--bundle DIR` explicitly passed | bundle mode, `DIR` |
| 2 | `--as NAME` explicitly passed | registration `NAME`; unknown name → error listing the known names |
| 3 | `RELAY_REMOTE_REGISTRATION` set | that registration; unknown → error listing names |
| 4 | `RELAY_REMOTE_BUNDLE` set | **bundle mode** — this is hermes's wiring and it keeps working |
| 5 | `config.json`'s `default` set and that registration exists | that one |
| 6 | exactly one registration exists | that one |
| 7 | several registrations, no selector | **error**, exit 2, listing every name and showing `--as NAME` and `use NAME` |
| 8 | no registrations and no bundle | today's "no enrolment bundle configured" error, with one added line naming `relayremote register` |

Row 3 above row 4 is deliberate: `RELAY_REMOTE_REGISTRATION` is the new, more specific variable, and a user who sets it means it. Rows 1 and 2 above both env vars is the ordinary flag-beats-environment rule the CLI already documents.

Row 7 is the one this store must never get wrong. **It does not pick.** Guessing between identities that differ precisely in what they may reach is the failure this whole store exists to avoid.

### 5.4 New verbs

```
relayremote registrations [--json]
relayremote use NAME
relayremote unregister NAME [--yes]
```

`registrations` renders the ADR's table exactly:

```
  NAME          CLIENT ID     HOST                 PROFILES              DEFAULT
* hermes-mail   hermes-mail   192.168.64.1:9910    mail                  yes
  hermes-cal    hermes-cal    192.168.64.1:9910    calendar, contacts
```

A directory with a key and no certificate renders `PROFILES` as `(registration in progress)`. `--json` emits the array of `Registration` values.

`use NAME` writes `default`; an unknown name is refused with the list.

`unregister NAME` prompts unless `--yes`, deletes the directory, and prints — **unconditionally, last, and never softened:**

```
Removed the LOCAL registration "hermes-mail" from this machine.

THIS DID NOT REVOKE ANYTHING. The enrolment "hermes-mail" still exists on
192.168.64.1, still counts against its budget, and still appears in the audit
log. To actually cut its access, run on the Mac:

    relay enrol revoke --client-id hermes-mail
```

If it was the default, the pointer is cleared and that is said too. A client-side delete that read as a revocation would be the worst possible lie in this system.

### 5.5 `register.go` — new

```
relayremote register "Hermes Mail" --host 192.168.64.1
    [--port 9910] [--enrol-port 9911] [--server-name NAME]
    [--profile PROFILE_ID] [--ca-fingerprint sha256:…]
    [--wait 10m] [--fresh] [--yes] [--json]
```

`--host` is **required** and takes a bare host — an address with a colon is refused, naming `--port` and `--enrol-port`. There is no discovery of any kind. Positional argument is the display name; if omitted, defaults to the machine's hostname slugified.

Flow: slug → directory → key+CSR (reusing `GenerateKeyAndCSR`, whose `O_EXCL` refusal already protects an existing key) → `R_C` → commitment → `pending.json` → lodge → compute and print the SAS → poll loop, sending `sas_open` on the first poll and on any retry → on `approved`, the five checks → write → report.

**Interruption and re-run.** `pending.json` is written *before* the lodge frame goes out and deleted only on success. Re-running `register "Hermes Mail" --host H`:

- in-progress directory for the same slug **and the same `enrol_addr`** → **resume**: reprint the *same* code from `pending.json` and resume polling with the same `request_id`. It does not lodge again. Lodging again would take a second of eight slots, put a second row in front of the operator, and — most importantly — produce a *second* code, which is exactly the confusion the one-comparison flow exists to remove.
- resume finds `unknown` on the first poll (the row expired) → says so and lodges a fresh one automatically. The intent is unambiguous at that point.
- in-progress directory for a **different** host → refused, naming `--fresh`.
- `--fresh` → deletes the whole directory (confirmed, or `--yes`) and starts over.

A completed registration of the same slug is never overwritten: `register` refuses and names `--as`, `use` and `unregister`.

`--wait` default 10m. On expiry: exit `exitEnrolPending` (11), telling the operator that re-running the identical command resumes.

### 5.6 The five collection checks, in order, before a byte reaches disk

Three are today's `verifyIssuedBundle`; two are new.

1. **CA continuity (new).** `SPKI(collected ca_pem)` must equal the CA SPKI the SAS was computed over at lodge time (cached in `pending.json` as `ca_spki_sha256`). A relay that answers a collection with a *different* CA than it answered the lodge with is a substitution the SAS cannot see, because the SAS was computed over the earlier one. Refuse loudly, write nothing.
2. **The comparison.** Interactive: require `y` to *"the Mac showed `7K3P4Q`. Did it match? [y/N]"*. With `--ca-fingerprint`: check `sha256(ca_pem DER)` against the pin, byte for byte, instead.
3. **Chain.** The leaf verifies against the collected CA for `ExtKeyUsageClientAuth`.
4. **Key pairing.** The certificate is over this machine's key.
5. **SPKI equality.** Restated inside `verifyIssuedBundle`.

Only then: `client.crt`, `ca.crt`, `registration.json` written atomically; `pending.json` removed; `config.json`'s `default` set if this is the first registration.

### 5.7 The closing report

The tool-plane address is **derived from `--host` and the port of the poll's `relay_addr`**, never from `relay_addr`'s host. `remote.listen` is very often `127.0.0.1:9910` or `0.0.0.0:9910`, which is meaningless to a remote machine, and following a host supplied over the wire would be a redirection primitive even where the pin has closed. A `relay_addr` whose host is neither loopback nor `0.0.0.0` and differs from `--host` is printed as an informational note and **not** followed.

Then `ListTools` once per granted project id:

```
registered "Hermes Mail" as hermes-mail
  relay:        192.168.64.1:9910
  certificate:  sha256:9f2a…
  code matched: 7K3P4Q

  access:       Hermes Mail Inbox  (proj_mail)
                14 tools — mail_search, mail_get_emails, mail_get_email, …

  use it with:  relayremote call --tool mail_search --args '{"query":"…"}'
                (this is the only registration, so --as is not needed)
```

Zero grants:

```
  access:       NOTHING YET.
                The operator enrolled this machine without an access profile, so
                every call will be denied until they grant one on the Mac:
                    relay enrol update --client-id hermes-mail --grant PROFILE_ID
```

If the tool plane cannot be reached, **the registration still succeeded and is on disk**; the client says so and exits with a new code:

```
exitRegisteredButUnreachable = 12
```

with the two likely fixes named: widen `remote.listen` on the Mac past loopback, or pass `--server-name` because relay's server certificate carries only the names in `remote.listen`. This is the single most common way a first registration "half works", and it earns its own exit code so a script can tell it apart from a failed registration.

### 5.8 Other client files

- `main.go` — verb table gains `register`, `registrations`, `use`, `unregister`; `options.bind`/`resolve` implement §5.3; usage text; `envRegistration = "RELAY_REMOTE_REGISTRATION"`.
- `wire.go` — the new optional lodge/poll fields, mirroring §2.
- `enrol_request.go` — **no behaviour change.** The connection plumbing (`dialEnrolListener`, `enrolConn`, `readEnrolFrame`, `enrolmentRefusalError`, `sanitizeForTerminal`) is reused by `register.go` in the same package. `runRequest` itself is not edited beyond a usage-text cross-reference.
- `bundle.go` — unchanged. Registration directories load through `LoadBundle`.
- `enrol.go` — unchanged.
- `exitcodes.go` — `exitRegisteredButUnreachable = 12`, plus its `exitCodeTable` row.
- `skills.go` — `skillContext` gains a `Selector` string (`--as hermes-mail` or `--bundle /path`) used in every rendered invocation, so a skill generated from a registration teaches `--as` and one generated from a bundle keeps teaching `--bundle`. `published_invocation_test.go` is extended to run both.
- `README.md` — `register` as the primary path, `request` as the bundle-mode sibling, the operator-carried path as the fallback that always works.

---

## 6. UI: what the notification says, what the sheet shows and requires

### 6.1 Is this new UI or the existing flow?

**It is a modification of the existing Settings → Remote Clients → Pending requests panel and its "Approve Enrolment Request" form.** No new tab, no new window, no new modal. That panel and form already exist in `web/src/app.js` (`renderPendingEnrolmentRequests`, `renderPendingRequestFields`, `renderEnrolmentForm`, `approveEnrolmentRequestForm`) and already carry the right copy about lodging never raising a prompt. Building a second surface would leave two places that can approve a request and two sets of copy to keep true.

### 6.2 The pending-request row

Above everything else in the row, and the largest thing on it:

```
+-------------------------------------------------------------+
|  7K3P4Q                                                     |
|  Compare this with the code shown on the machine asking.    |
|  If they differ, Refuse — something is on the network path. |
+-------------------------------------------------------------+
key:    sha256:9f2a41c7…                (full, never truncated)
label:  hermes-mail   (supplied by the requesting machine)
asked for: proj_mail  (a request from that machine, not a grant)
from:   10.0.0.7:51422   arrived: …   expires: …
                                  [ Approve… ]  [ Refuse ]
```

States:

- **`sas_ready:false`** — code area reads *waiting for that machine to complete the comparison handshake*, and **Approve… is disabled**. Approving a row with no comparison is approving without the control.
- **`sas_failed:true`** — code area reads *that machine failed its comparison handshake; this request cannot be approved*, in the error style, Approve… disabled, Refuse enabled.
- **`is_legacy_request:true`** — no code area at all; a line reads *carried-pin request (`relayremote request`) — the control here is the CA fingerprint the operator carried, shown in the tab header*. Approve… enabled, exactly as today. This is what keeps `request` working unchanged.

Existing guarantees kept: the label is rendered through `esc()` as text and is marked as machine-supplied; `requested_profile` is rendered the same way and marked as a request; no CSR bytes reach the view.

### 6.3 The approval sheet

The existing form, with four changes:

1. The code block from §6.2 repeats at the top of the sheet, above the identity section, plus the CA fingerprint line that is already there.
2. **Client id is pre-filled** with `suggested_client_id` (the host's collision-free suggestion) instead of being offered as a placeholder only, with the help text: *relay suggests this; it is yours to change. It names the enrolment in `relay enrol list` and in every audit record.* This is a deliberate change from the existing comment's stance and §9.5 records why.
3. **Choosing an access profile is required.** `saveEnrolment` refuses when `project_ids.length === 0` unless a distinct checkbox — *"Enrol with no access for now (nothing will work until I grant a profile)"* — is ticked. The checkbox is never pre-ticked and the button stays disabled until one or the other is true. A newly signed enrolment holding nothing connects successfully and lists zero tools, which reads as a broken install rather than an incomplete one; making the operator say which they mean is the whole of ADR-019 §7.
4. Requested profile, if any, renders read-only beside the checkbox list — *this machine asked for `proj_mail`* — and **never pre-ticks it.**

Everything else on the sheet is unchanged: budget, the presence prompt, the digest over the stored CSR.

---

## 7. Out of scope

**Do not design these; do not let them creep in.**

- **`cli-admin`, timed grants, the control surface.** Nothing here touches `remoteConfigHandlers`, `DescribeGrant`, or `api_credentials`.
- **Widening `NarrowGrant`.** It stays narrowing-only. The registration store must never gain a way to ask for more.
- **An IPC or HTTP update door for enrolments.** `relay enrol update` stays the only way to change a grant.
- **mDNS, Bonjour, broadcast, or any host discovery.** `--host` is a required flag with no default and no fallback.
- **Moving `ClassExecute` or `ClassProxy` off the socket.** Nothing in this work goes near that line.
- **Deprecating or altering `relayremote request`, `relayremote enrol`, `relayremote install`, `relay enrol sign`.** They must keep working byte for byte.
- **Deprecating `--bundle` or `RELAY_REMOTE_BUNDLE`.** Load-bearing for hermes.
- **A registration store with any authority.** `registration.json` is a cache. No field in it may ever be read to relax a check.
- **Any second approval door.** Approval stays `enrolment.sign`'s single gate and digest; there is no `enrolment.approve` in `presence.GatedOps` and this work does not add one.
- **Notification content beyond a count.** No label, no code, no request id in a banner.

---

## 8. Acceptance criteria

Each is written so it can **fail**. Unless stated otherwise, each is a hermetic test (`go test ./...` in the named repository), and every host-side test routes through `mkSandboxRelayHome(t)`.

### The SAS

**AC-1 — The two implementations agree, byte for byte.** `testdata/sas_vectors.json` is committed identically in both repositories (identical SHA-256) and contains at least 16 vectors, each giving `ca_spki`, `csr_spki`, `rc`, `rr` as hex and the expected 6-character SAS. Both repositories' tests iterate every vector. *Fails if* either implementation disagrees on any vector, or if the two files differ by a byte.

**AC-2 — The alphabet contains no confusable pair.** `sasAlphabet` has length 32, is all uppercase or digit, contains no `0`, `1`, `I`, `O`, and has no duplicate. *Fails if* any of those is false.

**AC-3 — 30 bits and exactly six characters.** For 10,000 random digests, the encoder emits exactly 6 characters, every character is in the alphabet, and the output depends only on the first 30 bits: flipping any of digest bits 0–29 changes the output, and flipping any of bits 30–255 does not. *Fails if* the length varies, a character escapes the alphabet, or the bit dependency is wrong in either direction.

**AC-4 — Domain separation is real.** `computeSAS(a,b,c,d)` differs from a hand-rolled SHA-256 of the same four values without the `"relay.sas.v1\x00"` prefix, and `sasCommitment` uses a different domain tag from `computeSAS`. *Fails if* either domain tag is absent or the two are equal.

**AC-5 — The commitment binds the key.** A commitment computed over CSR A's SPKI does not verify against a row holding CSR B, for 1,000 random pairs. *Fails if* any verifies.

**AC-6 — Both codes match on the honest path.** An end-to-end fixture: real key, real CSR, real lodge into a real `enrolmentRequestTable` seeded with a real CA, a real poll carrying the open. The client-side `computeSAS` over the ack's `ca_pem`/`sas_nonce` and the host-side `enrolmentRequestView.SAS` are the same 6 characters. *Fails if* they differ.

**AC-7 — The attacker's substitution changes the code.** Same fixture, but (a) the lodged CSR is replaced with an attacker key, and (b) the `ca_pem` handed back to the client is replaced with a foreign CA. In both cases the two codes differ. Repeated over 1,000 random attacker keys/CAs, the collision count is 0. *Fails if* any pair matches, or if the test is written so that a match would go unnoticed.

**AC-8 — The nonce cannot be chosen after the fact.** `sas_nonce` for a row is generated inside `Lodge`, before `Lodge` returns, and is never written again: a reflection or source check confirms `sasNonce` has exactly one assignment in `enrolment_requests.go`, and a behavioural check confirms two lodges of different keys get different nonces. *Fails if* the nonce is settable after the row exists.

**AC-9 — A commitment can be opened once, correctly, and never twice differently.** A row accepts its correct open, accepts the *identical* open again (a redialled poll), refuses a *different* open, and refuses an incorrect open while setting `sas_failed`. *Fails if* any of the four behaves otherwise.

**AC-10 — A failed comparison is unapprovable from every door.** With a row whose open failed, `EnrolmentOps.Approve` returns `errEnrolmentRequestSASIncomplete` from the CLI path, the IPC path and the HTTP path, **before** the presence gate is reached — proven with a recording presence provider showing 0 `Evaluate` calls. *Fails if* any door signs, or if the gate is reached first.

**AC-11 — An unopened comparison is unapprovable.** Same as AC-10 with `sas_commit` present and no open ever sent. *Fails if* it can be approved.

**AC-12 — A legacy `request` row is still approvable.** A row lodged with no `sas_commit` has `sas_ready:false`, `is_legacy_request:true`, and **approves normally** through the unchanged gate. *Fails if* the new refusal catches it.

**AC-13 — `register` cannot be run without an answer.** (a) Non-interactive stdin and no `--ca-fingerprint` → refused at flag parse, nothing dialled, exit 2. (b) Interactive, `n` at the confirmation → nothing written to the registration directory beyond the key/CSR that already existed, exit 2. (c) `--tofu` → refused at flag parse, naming the SAS. *Fails if* any of the three completes a registration.

**AC-14 — Relay never echoes the SAS to the client.** `enrolmentRequestPollResult` and `enrolmentRequestLodgeResult` have no field whose name or JSON tag contains `sas` other than `sas_nonce`; marshalling a fully populated instance of each produces JSON containing no 6-character alphabet string equal to the row's SAS. *Fails if* the code appears on the wire in either direction.

**AC-15 — CA continuity is enforced at collection.** A collection whose `ca_pem` differs from the lodge-time CA (same relay, different CA) is refused with a named error and writes nothing, even when the certificate chains and pairs correctly. *Fails if* it installs.

### The happy path

**AC-16 — One command, one comparison, one approval.** End to end against a real `EnrolmentRequestServer` and a real `RemoteServer`: `register "Hermes Mail" --host 127.0.0.1` prints a 6-character code; the host's pending view carries the identical code; approving with one grant makes the client write `client.key` (0600), `client.crt`, `ca.crt` and `registration.json`; a following `relayremote list` with no selector lists that grant's tools; exit 0. *Fails if* any file is missing or wrongly permissioned, the codes differ, or the list is empty.

**AC-17 — The close reports what the grant reaches.** The final stdout names each granted profile's id and its tool count, and with **zero** grants says so explicitly and names `relay enrol update --grant`. *Fails if* the zero-grant case prints anything that reads like success, or if it prints "done".

**AC-18 — The tool-plane address is derived from `--host`, never from the wire.** With `relay_addr` reported as `127.0.0.1:9910` and `--host 10.0.0.2`, `registration.json.addr` is `10.0.0.2:9910`. With `relay_addr` reported as `evil.example:9910`, the client still records `10.0.0.2:9910` and prints a note. *Fails if* the wire-supplied host is ever dialled.

**AC-19 — A `--server-name` mismatch is diagnosed, not swallowed.** When the tool-plane connection fails TLS name verification, `register` exits `12`, the registration is on disk and loadable, and stderr names both `remote.listen` and `--server-name`. *Fails if* it exits 0, exits with a transport code, or deletes the registration.

### Selection order

**AC-20 — The precedence table, all nine rows.** A table-driven test over §5.3, with every combination of `--bundle`, `--as`, `RELAY_REMOTE_BUNDLE`, `RELAY_REMOTE_REGISTRATION`, `default`, and a store holding 0, 1 or 2 registrations. Each row asserts the exact resolved source. *Fails if* any row resolves differently, and in particular if a set `RELAY_REMOTE_BUNDLE` fails to select bundle mode when no flag and no `RELAY_REMOTE_REGISTRATION` are present.

**AC-21 — With several and no selector, it errors and never guesses.** Two registrations, no `default`, no flag, no env: `list`, `call`, `skill`, `grant describe` and `serve` each exit 2 with a message naming **both** registration names and showing `--as` and `use`. *Fails if* any of them picks one, or if the message omits a name.

**AC-22 — `--bundle` and `--as` together are refused.** Exit 2, nothing dialled. *Fails if* one silently wins.

**AC-23 — An explicit flag beats the environment.** `RELAY_REMOTE_BUNDLE=/b relayremote --as hermes-cal list` uses `hermes-cal`. *Fails if* the bundle wins.

### Backward compatibility

**AC-24 — `--bundle` and `RELAY_REMOTE_BUNDLE` are untouched.** Every existing bundle-mode test in relayRemote passes unmodified, and a fixture reproducing hermes's exact wiring (`RELAY_REMOTE_BUNDLE` + `RELAY_REMOTE_ADDR`, no flags, no `~/.config/relayremote` directory at all) lists and calls successfully. *Fails if* any existing test needed editing, or if the absence of the config directory produces an error.

**AC-25 — The operator-carried path is byte-identical.** `relayremote enrol` → `relay enrol sign` → `relayremote install` → `list` works with the enrolment listener **turned off entirely**, and a source-level diff shows no behavioural change in `enrol.go` or in `relay enrol sign`'s path. *Fails if* any of the three needs the new listener, the new store, or a CA at lodge time.

**AC-26 — `relayremote request` is unchanged.** Every existing `enrol_request_test.go` and `adversarial_test.go` case passes unmodified, including `--tofu`, `--resume`, `--ca-fingerprint`, the mismatch refusal, the terminal-sanitization cases, and every exit code. *Fails if* any test needed editing.

**AC-27 — An old client against a new relay still works.** A lodge frame carrying no `sas_commit` and no `requested_profile` is accepted and behaves exactly as before, including the idempotent re-lodge and the poll statuses. *Fails if* the new fields are required anywhere.

**AC-28 — A new client against an old relay fails legibly.** Given a decode error naming an unknown field, `register` prints a message naming `relayremote request --ca-fingerprint` and the need to update relay, and exits 2. *Fails if* it prints a raw decode error or retries forever.

### No prompt from a lodge

**AC-29 — No presence call, ever, for any number of lodges.** The existing lodge/presence test, extended to the full new surface (§4.11), with a real `presencetest.NewRecording` provider. `Calls()` must be 0. *Fails if* it is anything else.

**AC-30 — Structurally no path from lodge to `presence.Gate`.** AST scan: `enrolment_requests.go` and `enrolment_request_server.go` import neither `relaygo/presence` nor anything UI-shaped, and reference none of the identifiers `Gate`, `Require`, `Evaluate`, `Notify`, `Platform`, `App`, `cocoa`. *Fails if* any appears in a non-test file.

**AC-31 — The table has no push.** Reflection over `enrolmentRequestTable`: no field of kind `Func` other than the allowlisted `now` and `rand`; no field of kind `Chan`; and `EnrolmentRequestServer` still holds no `RemoteToolRouter`, `RemoteConfigurer`, `SettingsStore`, `sealed.Sealer` or `*RelayCA`. *Fails if* a callback or channel field exists — that is how a push would arrive.

**AC-32 — The notification is raised from exactly one place.** AST scan over every non-test `.go` file finds exactly one call of `pendingEnrolmentNotifier.tick`, in `trayapp.go`. *Fails if* there are zero or more than one.

**AC-33 — The rate limit holds under a flood.** With an injected clock: 200 successful inserts inside one minute produce **one** notification; over one hour they produce at most **6**; a generation that does not move produces none; `live == 8` produces none; `settingsOpen == true` produces none; `unapproved == 0` produces none. *Fails if* any bound is exceeded, and in particular if the count is per-request rather than coalesced.

**AC-34 — The notification body carries no attacker-supplied text.** For a lodge whose label and requested profile are the maximum-length hostile strings the server accepts, the notification title and body contain neither, and contain no SAS. *Fails if* either appears.

**AC-35 — Clicking the notification opens a window and nothing else.** The click handler's call graph reaches `openSettingsWindow` and an emit, and reaches no gated core method — proven by AST scan of the handler's transitive call set within the package. *Fails if* any `requireGate` call is reachable.

### Stale registration fails closed

**AC-36 — A revoked enrolment's registration fails closed.** Register, then `relay enrol revoke` on the host, leaving `registration.json` untouched. The next `list` and `call` are refused by relay (connection closed at resolution, before a request is read), the client reports it with the transport/denied code, and **the client does not fall back to anything**. *Fails if* the call succeeds or the client degrades to a different identity.

**AC-37 — A narrowed grant fails closed.** Register with `[proj_mail, proj_cal]`, then remove `proj_cal` on the host. `call --project proj_cal` is denied by relay even though `registration.json` still lists it, and the client's cached list is not consulted for the decision. *Fails if* the client refuses locally *instead of* being refused by relay — the point is that relay is the one saying no.

**AC-38 — No field of `registration.json` relaxes a check.** A registration whose `ca_fingerprint` names a foreign CA but whose `ca.crt` is correct still verifies against `ca.crt`; a registration whose `ca.crt` is foreign fails TLS regardless of what `ca_fingerprint` says. A source scan finds no read of a `Registration` field inside `TLSConfig`, `LoadBundle` or `verifyIssuedBundle`. *Fails if* any of the three is false.

**AC-39 — A corrupt cache is still a usable registration.** With `registration.json` truncated to `{`, the client warns and still loads key/cert/CA and calls successfully (requiring `--project` if several grants exist). *Fails if* it refuses to run.

### The store

**AC-40 — Permissions.** After `register`: the root and each registration directory are 0700, `client.key` is 0600, `pending.json` is 0600 while it exists, and `client.crt`/`ca.crt`/`registration.json`/`config.json` are 0644. `checkKeyPermissions` refuses a widened key exactly as today. *Fails if* any mode differs.

**AC-41 — `unregister` says what it did not do.** Output contains the words "did not revoke" and the exact `relay enrol revoke --client-id NAME` command; the host-side enrolment is verified still present afterwards. *Fails if* the sentence is absent, softened, or the enrolment is somehow gone.

**AC-42 — Two registrations, two identities.** Register twice against one host with two names; `registrations` lists both with distinct `CLIENT ID`s and distinct certificate fingerprints; `--as` selects each; each is independently revocable and each appears under its own `client_id` in the host's audit log. *Fails if* they share a fingerprint or a client id.

**AC-43 — Interruption resumes rather than re-lodges.** Kill `register` after the lodge; re-run the identical command. It prints the **same** code, sends no second lodge (asserted by a counting fake listener), and completes on approval. *Fails if* a second row appears in the table or the code changes.

**AC-44 — Expired-in-flight lodges fresh, and says so.** With the row swept past its TTL, the re-run reports the expiry and lodges a new request with a **new** code. *Fails if* it silently polls a dead id forever or reuses the stale code.

**AC-45 — Slug and collision.** `"Hermes Mail"` → `hermes-mail`; a name slugifying to empty is refused; a slug colliding with a completed registration is refused naming `--as`/`use`/`unregister`; on the host, `suggestClientID` returns `hermes-mail-2` when `hermes-mail` is taken, and the authoritative uniqueness refusal still comes from `ValidateEnrolment` inside `store.With`. *Fails if* the client overwrites a registration, or if the host's suggestion is treated as authoritative anywhere.

### The approval sheet

**AC-46 — A grant is required.** The approval sheet's save is refused with zero profiles selected unless the explicit "no access for now" checkbox is ticked; the checkbox is never pre-ticked. `relay enrol approve` with no `--grant` is refused, naming `--no-grant`. *Fails if* either can silently issue an empty grant.

**AC-47 — The requested profile is displayed and never honoured.** A lodge carrying `requested_profile: proj_mail` renders as a request in both the row and the sheet, and the corresponding checkbox is **unticked** on first render. Approving without touching it issues no grant. *Fails if* it pre-ticks, pre-selects, or reaches `approveFields.ProjectIDs`.

**AC-48 — Approve is disabled without a comparison.** In the rendered panel, a row with `sas_ready:false` or `sas_failed:true` renders no enabled Approve control; a legacy row does. *Fails if* the button is enabled for the first two.

**AC-49 — The view still carries no CSR.** `TestPendingEnrolmentRequestView_CarriesNoCSRBytes` passes unmodified against the widened view — both the reflection scan and the marshalled-value scan. *Fails if* it needed editing.

### Documents

**AC-50 — Every document that asserts "no notification" is corrected.** A grep over `docs/`, `CLAUDE.md` and `README.md` finds no surviving claim that lodging raises no notification; each such place instead says that lodging may raise a **coalesced, rate-limited, dismissible notification** and never a prompt. *Fails if* any stale sentence remains — this is the property ADR-019 §4 knowingly weakened, and a document still claiming the old one is worse than no document.

---

## 9. Risks, and what I think the design gets wrong

### 9.1 ADR-019 §3's security argument is wrong, and the protocol must change to rescue it

This is the finding that matters. **The ADR's claim that a naked six-character `truncate(H(CA SPKI ‖ CSR SPKI))` resists a man-in-the-middle "because the target lives on the host's screen" is false.** Both inputs are public. The attacker reads the CSR off the wire, and it obtains relay's CA simply by lodging a request of its own — P2 says the CA is public and relay hands it out. So the attacker computes the host-side value exactly, then grinds *its own* CA keypair for a 30-bit collision: about one microsecond a try, **18 minutes on a single core, seconds on a GPU**, against a client that waits ten minutes and a row that lives fifteen.

The ADR reaches for Bluetooth numeric comparison as the precedent, and that is the right precedent — but BNC's security comes from *commitment*, not from the number being short. This spec therefore adds a commit–reveal (§3), which costs one 64-hex field on the lodge, two fields on the ack, one field on the poll, and no extra round trip. With it, the ADR's own claim becomes true and the bound is ≈2^-27 per approval ceremony. Without it, this feature would ship a comparison that reads as a security control and is not one — which is the precise failure mode ADR-018 §8 refused TLS on this listener to avoid.

**I recommend ADR-019 §3 be amended** to state the construction with the nonces and the commitment. The decision it records is right; the mechanism as written is not buildable as secure.

### 9.2 The two implementations of the SAS will drift, and only the test vectors stop it

Thirty lines of bit-twiddling duplicated across two repositories with no shared module is the highest-probability defect in this work, and its symptom is the worst possible one: two different six-character codes on two screens, which an operator is *trained by this very feature* to read as "you are under attack". They will refuse a legitimate registration and then start looking for an intruder. The `testdata/sas_vectors.json` contract (AC-1) is not optional and the file must be byte-identical, not merely equivalent. If a shared module ever becomes possible between the two repos, this is the first thing to move into it.

### 9.3 `Poll` stops being a pure read

To keep `enrolmentRequestHandlers` at exactly two entries — a stated security property — the commitment open is folded into `Poll`. `Poll` now has a bounded, single-use, self-verifying write side effect. This is the right trade against adding a third entry to a two-entry table that reviewers are taught to read as a boundary, but it *is* a weakening of a simple invariant ("Lodge writes, Poll reads") and it needs the comment to say so. If a future reviewer prefers three entries to an impure Poll, that is a defensible reversal and nothing else in this spec changes.

### 9.4 The notification will not always appear, and the design must not depend on it

macOS notifications can be denied in System Settings, suppressed in Focus, and are unavailable entirely to a process with no bundle identifier (`go test`, a bare `./relay` in a terminal). There is no way to force one. The tray's `Pending enrolment requests: N` line therefore stays as the guaranteed surface and must not be removed as "redundant". The banner is an improvement to discoverability, not a delivery guarantee, and every document should say so rather than promising the operator a banner.

### 9.5 Pre-filling the client id contradicts a deliberate existing choice

`web/src/app.js` currently offers the request's label as a *placeholder only*, with a comment explaining that the client id is the human's naming decision and not an echo of hostile input. This spec pre-fills it with the host's collision-free suggestion. ADR-019 §5 requires that — "the host owns its own `client_id` namespace and resolves collisions" is not a thing an operator can do by typing — and the risk is contained because the value is `isSafeID`-constrained, rendered escaped, obviously editable, and never an authentication factor. But it is a reversal of a written decision and the person who wrote that comment should see this section.

### 9.6 The notification weakens a property that a lot of prose currently promises

ADR-018 §8, `docs/access-profiles.md` and `CLAUDE.md` all state, in strong terms, that no notification appears because a stranger lodged a request. That is now false wherever the listener is on. ADR-019 §4 writes the weakening down honestly, but **the documents have not been updated and a document that still promises the old property is worse than no document at all** — an operator reading it will conclude a banner means something has gone badly wrong. AC-50 exists for this and it should be treated as a blocking criterion, not a docs-tidy afterthought.

### 9.7 Eight concurrent rows is the attacker's parallelism, and the bound depends on it

The 2^-27 figure in §3.3 is 2^-30 multiplied by the eight-row table cap. That cap is load-bearing for the *security* claim now, not only for the availability claim it was originally sized for. If anyone later raises `maxPendingEnrolmentRequests` for convenience, the SAS margin degrades linearly. The constant needs a comment saying so, and a reviewer needs to know that "8" is now two arguments, not one.

### 9.8 The registration store is new persistent state on the client, and it can rot

ADR-019 says this itself and the spec answers it structurally: `registration.json` is never read for a decision (AC-38), a corrupt one is still usable (AC-39), and relay re-checks at call time (AC-36, AC-37). The residual is honest: a half-written registration directory (key present, certificate absent) is a state that now exists in a place the user did not choose, and `registrations` must render it as *in progress* rather than as a broken row. If that rendering is got wrong, the first support question will be "why does `registrations` show a machine I never finished setting up".

### 9.9 What is not defended, restated

`--as` buys audit separation and revocation granularity, **not isolation**. Agents running as the same user on one machine can read each other's `client.key` — that is ADR-010 §2's limit and giving the registrations friendly names does not soften it by one bit. `registrations` output and the `README` must both say so, because a named list of identities is exactly the surface that invites the assumption that they are separated.
