# ADR-017 Implementation Spec: Sealed Config, Brokered Mutation, Presence Gate

**Status:** Specification. Implements ADR-017 decisions **1, 2, 3, 4, 5 and 7**.
**Date:** 2026-08-28
**Reads with:** [ADR-017](017-config-dir-is-not-a-boundary.md) — but is written so a
developer can pick up one build-order step (§10) and work without reading it.

This document is the build instruction. Where it and ADR-017 disagree on a fact,
this document has been measured and the ADR has not; §9 lists every such case.
Where it and ADR-017 disagree on a *decision*, ADR-017 wins and this is a bug.

---

## 1. What is being built, in one paragraph

Four secrets move off disk and into an AES-256-GCM envelope whose key lives in
the login keychain, where the hardened runtime is a boundary and `0600` is not:
project token plaintexts, `admin_secret`, stored OAuth tokens, and `ca.key`.
Every verifier and all structure stays in the clear, so `relay grant`,
`relay audit` and a hand-edit keep working against a stopped relay. Every
mutating CLI command stops writing `settings.json` and asks the tray over the
bridge socket instead, so the tray is the only writer and the only holder of the
key. And every operation that issues a credential, widens one, or chooses what
runs demands a **LocalAuthentication user-presence check bound to that exact
operation and those exact arguments**, placed in the operation core so that no
door — CLI, IPC, HTTP or tray — can reach the act without passing it.

## 2. Scope

**In scope:** ADR-017 decisions 1 (field-level sealing), 2 (the service is the
sole broker and the sole decision point), 3 (the gate is on the operation),
4 (the gate demands user presence), 5 (stated as non-goals, §8), 7 (the keychain
unlocks at GUI login; SSH refuses).

**Explicitly out of scope. Do not design these; do not let them creep in:**

- **Decision 6's `relay run` sandbox launcher.** No `sandbox-exec` profile, no
  `relay run` subcommand, no spawn-under-confinement work of any kind. ADR-017
  records it as deliberately unresolved and it stays that way.
- **Per-route classing in the service manifest (issue #50).** The `proxy` class
  remains unclassified and **ungated**. ADR-017 decision 3 names this as "a hole
  named rather than hidden": gating `proxy` would put a prompt in front of every
  Eve session and every relayScheduler poll. The hole stays open, stays named in
  `docs/tokens.md`, and closing it needs a cross-repository protocol change that
  is not this work.
- **Secure Enclave key storage.** This machine has none (§9.6). The key is an
  ordinary login-keychain generic-password item. Write no enclave code; write no
  branch that would use one if present.
*(Sealing `external_mcps[].env` / `services[].env` **values** was an open
question in an earlier draft of this spec. The owner has decided it: they are
**in scope and sealed**. See §4.3.)*

---

## 3. The four settled answers

ADR-017 left four questions open. They are decided. Design to these; do not
relitigate them.

### 3.1 Presence grant lifetime: single-use, operation-bound, 120 seconds

A presence check yields a nonce bound to **both** the operation name **and** a
digest of that operation's arguments. It expires 120s after issue, is burned on
first use, and cannot be spent on a different operation or on the same operation
with different arguments. There is **no ambient session, no sudo-style
timestamp, no per-class window, no "remember for 5 minutes"**.

This is the direct answer to ADR-017's residual risk "wait for a prompt the owner
is about to answer anyway": a prompt answered for an enrolment cannot be spent on
a mint, and a prompt answered for `mint --class read` cannot be spent on
`mint --class grant --class execute`.

Full mechanism in §6. The digest rules are in §6.3 and a digest that omits a
security-relevant argument is a hole — §6.4 lists the exact argument set per
operation.

### 3.2 SSH: refuse, name the reason

A privileged operation from a session that cannot display a GUI prompt refuses
**immediately**, with a message naming why and what to do instead. No queue, no
pending-approval list, no blocking-with-timeout dressed as a security decision.

**No exemption for `relay login enrol`.** ADR-016 decision 2 kept that
subcommand specifically because "the tray is `LSUIElement`, its menu is
unreachable over SSH". That affordance is **knowingly withdrawn**. §7.5 names
the documents that must be updated and §9.8 names the sharpest cost.

The read half is untouched: `relay audit`, `relay grant`, and every `list`
subcommand keep working over SSH exactly as they do today.

**The determination must rest on something the caller cannot fake.** `SSH_TTY`
and `SSH_CONNECTION` are environment variables belonging to the caller and are
worthless as a security signal — a rogue agent unsets them. Relay must never
read them. §6.6 specifies the mechanism: the **peer's** kernel-supplied audit
session. Named limitation, not designed around — `open -a` from an SSH shell
launches into the console session and passes the check, so it is honesty and
prompt-spam reduction rather than a boundary; the boundary is the password inside
the prompt.

### 3.3 Break-glass: GUI-only reset, detect the mismatch

The sealed store carries a key id; `settings.json` carries the same key id
**unsealed**, in a new top-level `sealed_key_id` field. On load, a missing
keychain key or a key-id mismatch is detected and named **exactly** — "the sealed
store is bound to key X, settings.json expects Y" — rather than surfacing as a
parse error or a decrypt error.

Recovery is: delete `settings.json` **and** the keychain item, offered by the
tray behind a presence prompt. There is **no offline recovery code and no escape
hatch**, because an escape hatch is a second door into the sealed store.

§5.6 specifies exactly what relay does when it finds this state: it still
starts, still serves the read half, and refuses every sealed operation with the
named reason.

### 3.4 Issuance auditing becomes a hard dependency

`recordIssuance` returning `nil` when auditing is disabled is a hole this design
leans on (ADR-017 Consequences). With sealing active, issuance auditing is a hard
dependency, not a courtesy — ADR-010's rule applied to a new surface.

**Recommendation, and the one to build: refuse at the operation, not at
startup.** §7.4 gives the argument and the exact behaviour.

---

## 4. Decision 1 — what is sealed, what is not, and the residue rule

### 4.1 The sealed set, enumerated by field

The rule is ADR-017's: **what relay hands out is sealed; what relay only checks
stays readable.** Apply the rule per field. Do **not** apply a name heuristic.
`projects[].token` and `projects[].token_hash` are both 64 hex characters and are
indistinguishable by eye; a rule like "leave 64-hex values alone, they look like
hashes" leaves every project token in the clear, and a rule like "seal anything
called `*_token`" seals the verifier and misses the plaintext.

**Sealed** (`Secret`-typed; see §4.4):

| location | Go field | why |
|---|---|---|
| `projects[].token` | `Project.Token` | the plaintext project token relay injects into children |
| `admin_secret` | `Settings.AdminSecret` | a plaintext bearer the bridge accepts for `ReconcileExternalMcps` / `ReloadExternalMcp` / `ReloadService` |
| `external_mcps[].oauth_state.access_token` | `OAuthState.AccessToken` | a bearer relay presents upstream |
| `external_mcps[].oauth_state.refresh_token` | `OAuthState.RefreshToken` | mints the above indefinitely |
| `external_mcps[].oauth_state.client_secret` | `OAuthState.ClientSecret` | a bearer from dynamic registration |
| `external_mcps[].env` **values** | `ExternalMcp.Env` values | injected verbatim into the spawned child — handing them out is the definition of the sealed set |
| `services[].env` **values** | `ServiceConfig.Env` values | same |
| `ca.key` (separate file) | — | signs client certificates; escalation door three |

ADR-017 names "stored OAuth refresh tokens". `access_token` and `client_secret`
are the same kind of value by the same test and leaving them clear beside a
sealed `refresh_token` would be an inconsistency an attacker uses; sealing all
three is a deliberate widening and is recorded in §11 as an assumption.

**Clear** (verifiers and structure; sealing any of these is a defect):

- `projects[].token_hash` — SHA-256, the thing `AuthenticateProject` actually
  resolves against.
- `api_credentials[].hash` — the field is named `hash`, not `token_hash`.
- `login_bootstrap.hash` and `login_bootstrap.expires`.
- `passkeys[].x`, `passkeys[].y` (public key coordinates), `user_handle`,
  `sign_count`, `counter_supported`.
- `enrolments[].fingerprint`, `client_id`, `project_ids`, `budget`.
- `external_mcps[].oauth_state.client_id` and `.token_expiry`.
- **Env *keys*** on MCPs and services — see §4.3 for why the key/value split is
  the whole point.
- Everything else: every name, path, command, argv,
  `allowed_mcp_ids`, `allowed_tools`, `access`, `context` (the resource scope),
  `allow_cwd_auth`, the `audit` block, the `remote` block, `version`.

### 4.2 The invariant that makes migration self-checking

For every project: **`sha256(Reveal(token)) == token_hash`.** This holds on all
five records in the live config dir right now (verified by computing the digest,
not by looking). Migration must assert it before writing, and unsealing must
assert it on load.

- On **migration**, a project whose plaintext does not hash to its stored hash is
  a file that is not what relay thinks it is. Refuse the whole migration, name
  the project by id and name, write nothing.
- On **load**, a project whose unsealed token does not hash to its stored hash
  means the seal, the key, or the file is wrong. Name it as
  `sealed value for projects/<id>/token does not match its stored token_hash`
  and enter the degraded state of §5.6 — do not silently serve a project whose
  token will not authenticate.

This is acceptance criterion **AC-6**.

### 4.3 Env values are sealed; env keys are not

`external_mcps[].env` and `services[].env` **values** are injected verbatim into
the children relay spawns, which is precisely ADR-017 decision 1's test — *what
relay hands out is sealed*. They are sealed.

**The keys stay in the clear, and that split is the whole design.** An operator
reading `settings.json`, or running a hand-edit, still sees *which* environment
variables an MCP or a service receives; only the contents are opaque. So decision
1's two protected affordances survive intact: the file is still a file an
operator can read and reason about, and a hand-edit can still add, rename or
remove a variable. `relay grant` never showed env at all and is unaffected.

On disk this means `env` stays a JSON object whose keys are strings and whose
values are envelope objects:

```json
"env": {
  "OPENAI_API_KEY": { "sealed": "v1", "key": "8f3a…", "n": "…", "ct": "…" },
  "LOG_LEVEL":      { "sealed": "v1", "key": "8f3a…", "n": "…", "ct": "…" }
}
```

Every value is sealed, including the ones that are obviously not secret
(`LOG_LEVEL` above). **Do not add a heuristic** that seals only values whose key
looks credential-ish: a rule that reads names is a rule an attacker chooses the
name to defeat, and it is the same class of mistake as sealing by field-name
pattern (§4.1). Uniformity is what makes the property checkable.

`Secret` is the field type (§4.4), so `map[string]Secret` replaces
`map[string]string` on both `ExternalMcp.Env` and `ServiceConfig.Env`. The AAD
paths are `external_mcps/<mcp-id>/env/<KEY>` and `services/<id>/env/<KEY>`, so a
value cannot be moved between variables or between records.

**Migration cost is zero today**: every `env` on this machine is empty on every
registered MCP, and no services are registered at all (§9.10). Deferring would
have meant migrating populated envs later, under worse conditions.

Consumers to update: `service_registry.go`'s spawn path and
`external_mcp.go`'s `prepareStdioLaunch` build the child's environment and must
call `Reveal()` at the point of spawn. `mcp list` and `service list` do not print
env values and stay unchanged. `mcpFields` / `serviceFields` carry
`map[string]string` on the wire (an operator types a plaintext value into the
Settings window or passes `--env K=V`) and convert at the core boundary — the
digest of §6.4 covers the plaintext values, since that is what the human is
approving.

### 4.4 In-memory representation: the `Secret` type

The single highest-value change in this spec. ADR-017: *"plaintext must never be
what lands on disk, at any stage, rather than something re-secured after the
fact. An implementation that leaves a plaintext residue path open has not
implemented this ADR."*

Introduce, in a new `settings_secret.go`:

```go
// Secret is a value that must be sealed before it is serialised.
type Secret struct {
    plain string
    env   *sealed.Envelope
}

func (s Secret) Reveal() (string, bool)          // plaintext, ok
func (s Secret) String() string                  // always "<sealed>"
func (s Secret) MarshalJSON() ([]byte, error)    // errUnsealedSecret when env == nil
func (s *Secret) UnmarshalJSON(b []byte) error   // envelope object, OR a bare string (legacy)
```

Three properties, and each is load-bearing:

1. **`MarshalJSON` returns an error when the value has not been sealed.** Every
   residue path in the program — `json.Marshal(settings)`, `json.Marshal(proj)`,
   an `slog` call with `"settings", s`, a future export route someone adds — stops
   being a silent leak and becomes a loud failure at the first sealed field. This
   is what makes the residue rule enforced by the type system rather than by
   review.
2. **`String()` returns `<sealed>`**, so `%v`, `%s` and every `slog` value
   render a placeholder. The plaintext leaves only through `Reveal()`, which is
   greppable and is the audit surface for "who reads a secret".
3. **`UnmarshalJSON` accepts a bare JSON string** and records it as a *legacy
   plaintext* value with `env == nil`. This is exactly how migration reads
   today's file, and it is why migration needs no separate parser.

**Consequence — `deepCopySettings` must stop round-tripping through JSON.**
It currently marshals and unmarshals, and it panics on marshal failure. With
`Secret` it would panic on every copy. Replace it with an explicit
`func (s *Settings) Clone() *Settings`, hand-written, copying every field
including both halves of every `Secret`. Guard it with a reflect-based
completeness test (`TestSettings_CloneCopiesEveryField`) that populates a
`Settings` with distinct non-zero values via reflection, clones it, and compares
field by field — so a field added later and not cloned fails the suite. This is
build-order step **S0** and it lands on its own, green, before anything else.

### 4.5 The write path: sealing happens before serialisation

`FileSettingsStore.save` becomes:

```
1. s.normalize()                      (unchanged)
2. sealAllSecrets(s, sealer)          NEW — fresh nonce per field, per write
3. json.MarshalIndent(s)              (unchanged; Secret marshals its envelope)
4. atomicWriteFile(path, data, 0600)  (UNCHANGED — no changes to this function)
```

**`atomicWriteFile` is not modified.** The residue rule is satisfied at step 2,
not at the file layer: the bytes handed to step 4 already contain no plaintext,
so the `settings.json.*.tmp` staging file never contains any either, and a
process killed between create and rename leaves a sealed file behind. State this
in the code comment, because the obvious wrong fix is to add sweeping or
shredding to `atomicWriteFile` and that would be machinery in the wrong place.

**If any `Secret` cannot be sealed, the whole save is refused** — before the
temp file is created, before anything is staged. `sealAllSecrets` returns an
error, `save` returns it, `WithDeclinable` returns it, and `settings.json` is
left byte for byte as it was. Same shape as `errSettingsUnreadable`.

**Every write re-seals every sealed field, from the plaintext held in memory.**
Not because the AES-GCM nonce would otherwise repeat — a fresh 96-bit random
nonce is generated inside each `Seal` call, so repetition is impossible by
construction — but because the alternative is a branch that decides between
"re-seal this one" and "carry the old envelope forward", and that branch is
where a plaintext leak lives. There is no such branch. Cost is a few AES
operations per write on a file written a handful of times a day.

### 4.6 The envelope format

A sealed field is a JSON **object** where a JSON **string** used to be:

```json
{
  "sealed": "v1",
  "key": "8f3a1c2e5b704d91",
  "n":   "Yk3vQ1p8Lm2tR9dC",
  "ct":  "3Qm1…"
}
```

| key | meaning |
|---|---|
| `sealed` | format discriminator and version. `"v1"` is the only accepted value. |
| `key` | the id of the key this envelope was sealed with — 16 hex chars. Duplicated per field on purpose: a field sealed under an older key is individually identifiable, and the top-level `sealed_key_id` alone could not say which field disagreed. |
| `n` | base64 (std, padded) of the 12-byte GCM nonce |
| `ct` | base64 of ciphertext ‖ 16-byte GCM tag |

**Self-describing enough to tell sealed from corrupt.** An operator reading
`settings.json` sees an object with `"sealed": "v1"` and knows the value is
sealed. Relay's own classification, in order:

- not a JSON object, and a JSON string → **legacy plaintext** (migration input).
- a JSON object with `"sealed": "v1"` and all four keys present → **sealed**.
- a JSON object with `sealed` present but any other value → **unsupported seal
  format**, named as such, degraded state.
- anything else → **corrupt**, named with its field path, degraded state.

Never conflate these three refusals into one message. The whole point of
decision 1 is that "the file is corrupt" and "the key is wrong" stay
distinguishable from the outside.

**AEAD: AES-256-GCM** (`crypto/aes` + `crypto/cipher`, stdlib). Chosen over
XChaCha20-Poly1305 because the latter needs `golang.org/x/crypto`, which is not a
dependency of this module and adding one to the sealing layer is a supply-chain
cost this design does not need to pay. GCM's 96-bit random nonce is safe far past
relay's write volume, and the nonce is per-`Seal`-call so reuse is unreachable.

**AAD binds an envelope to its location.** `Seal`/`Unseal` take
`aad = "relay-settings-v1\x00" + <field path>`, where field path is:

```
admin_secret
projects/<project-id>/token
external_mcps/<mcp-id>/oauth_state/access_token
external_mcps/<mcp-id>/oauth_state/refresh_token
external_mcps/<mcp-id>/oauth_state/client_secret
```

So an attacker who can write the file cannot move a sealed `admin_secret` into a
project's `token`, or one project's token into another's. Since every write
re-seals from plaintext with the *current* path, a record that legitimately
changes id (an MCP renamed, which produces a new id via `slugify`) re-seals under
its new path on the next write and never carries a stale AAD.

`ca.key` gets its own file, `ca.key.sealed`, holding one envelope object with
`aad = "relay-ca-v1\x00ca.key"`. Written with the same `atomicWriteFile`, 0600.
The plaintext `ca.key` is not written after migration (§4.7).

### 4.7 Migration, against a real dirty config dir

Migration runs inside `EnsureInitialized`, once, when the loaded settings contain
at least one legacy-plaintext `Secret` (`env == nil`, `plain != ""`) or when
`ca.key` exists and `ca.key.sealed` does not.

```
1. Load or create the keychain key (§5.5). If it cannot be created, refuse:
   write nothing, name the keychain error, exit. A half-migration is worse
   than none.
2. Assert sha256(token) == token_hash for every project (§4.2). On failure,
   refuse and name the project.
3. Seal every Secret and save. One atomic write.
4. If ca.key exists: read it, seal it to ca.key.sealed, fsync, THEN remove
   ca.key. In that order — a crash between 4 and 5 leaves both files, which
   loadCA resolves in favour of the sealed one; the reverse order can lose
   the CA and every enrolment with it.
5. Scan the config dir for stale plaintext copies (below) and NAME them.
```

**Pre-existing plaintext copies.** The live config dir contains, right now,
`settings.json.bak-fsreview` (1657 bytes, 1 project token, `admin_secret`) and
`settings.json.pre-v3-backup` (4041 bytes, 3 project tokens, `admin_secret`).
Sealing the live file and leaving these is exactly the failure ADR-017 names:
"a design that seals the live file and not its copies has sealed one of them."

**Recommendation: relay names them and deletes nothing.** Migration scans the
config dir for files matching `settings.json*` that are not `settings.json`
itself, parses each as JSON, and reports any that contains a bare-string value at
a sealed field path. For each, one `slog.Warn` and one line on stdout:

```
warning: /Users/…/relay/settings.json.pre-v3-backup holds 3 project tokens and
  admin_secret in PLAINTEXT. This migration removed them from settings.json; it
  did not touch this file. Those tokens are still live. Rotate them (Settings →
  Projects → Rotate) and delete the file, or delete the file and rotate anyway.
```

Relay may **not** delete a file it did not create. It is a destructive act relay
has no mandate for, against a copy the operator deliberately made, and ADR-017's
own recovery story depends on an operator being able to keep a copy aside. This
also matches `docs/tokens.md`'s existing refusal to add a sweeper for `*.tmp`
leftovers: "a timer rewriting this directory is the writer nobody asked for."
§11 records "should relay offer to delete them behind the presence prompt?" as an
owner question, recommending no.

Relay likewise does not delete `settings.json.*.tmp` leftovers. It counts them
and names them in the same notice, since after migration they are sealed and
therefore harmless — a pre-migration leftover is not, and gets the same warning.

The migration is written to be run **against this machine's real config dir**,
not only against fixtures. A full backup exists outside the repo, so the
implementation and test work may tamper with, reset or destroy it.

---

## 5. The `sealed` package, the store, and the break-glass

### 5.1 Package boundary

New package `relaygo/sealed`. It exists as a separate package, not as files in
`main`, for one reason that matters and one that is convenience: the envelope
type has unexported fields that `main` must not be able to construct by hand,
and the cgo keychain code is easier to build-tag in isolation.

```go
package sealed

type Envelope struct{ /* unexported; marshals to the §4.6 object */ }
func (e Envelope) KeyID() string
func (e Envelope) Valid() bool

type Sealer interface {
    KeyID() string
    Seal(plaintext, aad []byte) (Envelope, error)
    Unseal(e Envelope, aad []byte) ([]byte, error)
}

var (
    ErrKeyMissing   = errors.New("sealing key not found")
    ErrKeyMismatch  = errors.New("sealing key id does not match")
    ErrCorrupt      = errors.New("sealed value is corrupt")
    ErrUnsupported  = errors.New("unsupported seal format")
)

func NewAESSealer(keyID string, key []byte) (Sealer, error)  // 32-byte key
```

Constructed at exactly one place in production: `runTrayApp`, from the keychain
key. Tests construct `NewAESSealer` with a fixed key directly — that is the
ADR-002 seam pattern (`NewSettingsStoreAt(dir)`), an injected value, not a
global switch.

### 5.2 The keyring

```go
package sealed

// Keyring is where the 32-byte key lives between runs.
type Keyring interface {
    Load() (keyID string, key []byte, err error)   // ErrKeyMissing if absent
    Create() (keyID string, key []byte, err error) // generate + store; refuses to overwrite
    Destroy() error                                // break-glass only
}
```

- `keychain_darwin.go` — cgo, `Security.framework`. Item class
  `kSecClassGenericPassword`, service `com.barelyworkingcode.relay`, account
  `config-seal-key`, accessibility
  `kSecAttrAccessibleWhenUnlockedThisDeviceOnly` (never syncs to iCloud, never
  restores onto another machine). Payload is
  `{"key_id":"<16 hex>","key":"<base64 32 bytes>"}` so the id travels with the
  key and cannot drift from it.
- `keychain_other.go` — `//go:build !darwin`, every method returns
  `ErrKeyMissing` with "sealed config requires macOS". Keeps `go build` honest on
  other platforms, which the repo already does for `audit_process_other.go`.
- `memory.go` — `NewMemoryKeyring(keyID string, key []byte)`, an ordinary
  exported constructor used by tests and by nothing in production. It is not a
  seam that weakens anything: a caller must supply the key.

### 5.3 The ACL: the mechanism, measured, and the structural requirement it creates

Everything in this section was measured on this machine by the feasibility
spike. None of it is a spike input any more.

#### 5.3.1 The modern API is unavailable. Use the deprecated one, deliberately.

`kSecAttrAccessControl` — and therefore `kSecAccessControlUserPresence` — is
**not usable**. Every `SecItemAdd` carrying it returns **`-34018`
`errSecMissingEntitlement`**, with and without `kSecUseDataProtectionKeychain`.
Getting past it needs `keychain-access-groups`, which is a restricted
entitlement: signing with it under all three identities available here produced
**SIGKILL at exec** (`Taskgated Invalid Signature`). It needs a portal-issued
provisioning profile embedded in the bundle, which relay does not have and is not
going to acquire for this.

**Do not spec, build, or leave a TODO for `kSecAttrAccessControl`.** It is not an
alternative and it is not a future simplification.

**The mechanism to build** is the legacy file-keychain path:

```
SecTrustedApplicationCreateFromPath(<path to the tray binary>, &app)
SecAccessCreate(CFSTR("relay config seal key"), <array of one app>, &access)
SecItemAdd(… kSecAttrAccess: access …)          // measured: OSStatus 0 on 26.4
```

Deprecated since 10.10, compiles with deprecation warnings, and **works** on
26.4. This is a **deliberate deprecated-API bet** and it must be named as one in
`docs/sealed-config.md`, alongside what the fallback is if it is ever withdrawn
(a keychain item with no trusted-application list, §5.3.4). Suppress the
deprecation warnings at the one cgo file with a scoped pragma, never module-wide,
so the next deprecated call anyone adds is still noisy.

#### 5.3.2 The ACL binds to code identity, not to a path

Measured against a Developer-ID-signed test app that sealed an item naming
itself:

| reader | result |
|---|---|
| the app itself | **silent unlock** |
| the same app copied to `/tmp/Moved.app` | **silent unlock** — not path-bound |
| a different build, different cdhash, same team + same `CFBundleIdentifier` | **silent unlock** — survives rebuilds |
| the same path, re-signed ad-hoc | `-25308` |
| the same team, a different bundle identifier | `-25308` |
| an unrelated binary | `-25308`, and **no prompt at all** where no interaction is possible |

The enforced rule is *"anything carrying Relay's code identity, anywhere on
disk"*, not *"`/Applications/Relay.app`"*. The path handed to
`SecTrustedApplicationCreateFromPath` is only read at seal time, to extract the
identity.

Two consequences for §5.3.1's wiring:

- **The grant survives every rebuild signed by the same identity, with no
  maintenance.** The installed bundle is Developer ID signed (§9.2), so this is
  the steady state here. ADR-017's Consequence "a locally rebuilt relay
  re-prompts" does not apply on this machine, and **AC-24b** rejects any
  dev-mode ACL relaxation added to solve a problem that is not there.
- **The tamper property holds, and it fails as a refusal rather than as an
  unlock.** A modified or re-signed bundle is a different identity and gets
  `-25308` (`errSecInteractionNotAllowed`) where no session can prompt, or the
  keychain's own consent dialog where one can. Either way, no silent unlock.

#### 5.3.3 The CLI carries Relay's code identity. This is now structural.

`/Applications/Relay.app/Contents/MacOS/relay credential mint` **is** Relay's
code identity, so it satisfies the ACL and would silently unlock the key.
ADR-017 decision 2 waves this away — *"under brokering the question does not
arise: the tray is the only process that ever asks the keychain"* — as an
observation. **It is a load-bearing structural requirement and must be built as
one.**

The requirement, in ADR-015 decision 2's spirit — make it unreachable, do not
check and refuse:

1. **The keychain keyring is constructed at exactly one call site**, on the tray
   path in `runTrayApp`, and nowhere else.
2. **Every CLI entry point holds a store with a nil sealer** (§5.4), which
   reveals nothing and refuses every write. No CLI code path constructs a
   `Keyring`, a `Sealer`, or reaches `Unseal`.
3. **A structural test proves it.** `TestSeal_NoCLIPathReachesTheKeychain` builds
   the call graph of `package main` with `go/ast` from each CLI entry point
   (`runCredentialCommand`, `runEnrolCommand`, `runLoginCommand`,
   `runMcpCommand`, `runServiceCommand`, `runGrantCommand`, `runAuditCommand`,
   `runMcpExec`, `runMcpOrServer`) and fails if any of them can reach
   `sealed.NewKeychainKeyring`, `Sealer.Unseal`, or `Secret.Reveal`. It fails by
   name, so a future `credential_cmd.go` change that reintroduces a direct call
   is caught at the commit that makes it. **AC-29.**

This is why brokering (decision 2) is not merely a tidiness argument: without it
the ACL is satisfied by exactly the process the ADR is defending against.

#### 5.3.4 The fallback, if `SecAccess` is ever withdrawn

Store the item with no trusted-application list. The tray then gets one keychain
consent dialog on first unlock per login session and the operator ticks *Always
Allow*, which installs the same ACL by hand — and that dialog was measured to
require the login password even to click Allow, so decision 4's ACL-fallback arm
is a password boundary rather than merely a click boundary. Ergonomics are worse
by one click per login; the security property is unchanged. **Do not fall back to
storing the key in a file.**

### 5.4 Wiring into `FileSettingsStore`

`FileSettingsStore` grows one field and one constructor argument:

```go
func NewSettingsStoreSealed(dir string, sealer sealed.Sealer) *FileSettingsStore
```

`NewSettingsStore()` and `NewSettingsStoreAt(dir)` keep their signatures and
leave the sealer nil. **A nil sealer is the read-only-half store**: it loads,
leaves every `Secret` closed (`Reveal()` returns `("", false)`), serves every
clear field, and **refuses every write** with `errSealerRequired`. That is
precisely the shape the CLI's read commands need, and it means a CLI process can
never write `settings.json` even if someone reintroduces a direct call — a
second, structural guarantee behind brokering.

`load()` gains, after `json.Unmarshal` and before `normalize()`:

```
openAllSecrets(&s, ss.sealer)  // nil sealer: leave closed, no error
```

`save()` gains `sealAllSecrets` per §4.5.

`forEachSecret(s *Settings, fn func(path string, sec *Secret) error) error` is
the single place the sealed set is enumerated. `sealAllSecrets` and
`openAllSecrets` are both one line over it. A reflect-based test
(`TestSecrets_ForEachVisitsEverySecretField`) walks the `Settings` type, finds
every field of type `Secret` reachable from the root including through slices and
pointers, and asserts `forEachSecret` visits exactly that set on a populated
value — so a `Secret` added later and not enumerated fails the suite rather than
reaching disk in the clear. This is acceptance criterion **AC-4**.

### 5.5 Key lifecycle, and the one rule that must never be broken

- **First run, no key AND no `sealed_key_id` in `settings.json`:**
  `EnsureInitialized` calls `Keyring.Create()`, writes `sealed_key_id`, seals and
  saves. This is the **only** condition under which relay creates a key.
- **Normal run:** `Keyring.Load()`, compare its id with `settings.json`'s
  `sealed_key_id`.
- **Key present, `sealed_key_id` absent, no sealed fields:** a file written
  before this feature. Migrate (§4.7).
- **Key present, `sealed_key_id` absent, sealed fields present:** corrupt
  combination. Degraded state (§5.6), named.
- **No key, `sealed_key_id` present:** degraded state. **Never create one.**

#### 5.5.1 Never generate a replacement key

The ACL protects reading, not deletion: any local process can delete the keychain
item. Deletion is denial of service and is conceded. **Substitution is not**: if
relay's answer to "the key I expected is gone" were to create a new one, an
attacker could plant their own key and have relay re-seal every secret under it.

Two rules close that, and both fall out of the key-id design decision 3 already
requires:

1. **A missing or mismatched key is a loud, named refusal (§5.6). Relay never
   generates a replacement and never adopts a key whose id is not the one
   `settings.json` names.** There is no flag, prompt or affordance that accepts
   one. Re-keying is the break-glass reset of §5.6 clause 5 and nothing else.
2. **The key id is part of the AEAD's associated data.** §4.6's AAD becomes
   `"relay-settings-v1\x00" + keyID + "\x00" + <field path>`, so a value cannot
   be opened under a substituted key, and an envelope's `key` field cannot be
   edited independently of its ciphertext.

**AC-25e** and **AC-25f** are the whole test surface for this. Build no other
integrity machinery: no key rotation ceremony, no attestation, no tamper
monitoring.

### 5.6 The degraded state: sealed store unavailable

Four conditions produce it, and each is named distinctly. Note that the first
two are also what **key substitution** (§5.5.1) looks like from the inside, which
is why neither may ever resolve to "create a key and carry on":

| condition | message |
|---|---|
| keychain item absent, `sealed_key_id` present | `the sealed store expects key <Y>, but no such key is in the login keychain; settings.json cannot be unsealed on this machine. relay will not create a replacement — a new key would re-seal your secrets under a key you did not choose.` |
| keychain key id ≠ `sealed_key_id` | `the sealed store is bound to key <X>, settings.json expects key <Y>` |
| the AEAD tag fails despite matching ids | `sealed value for <field path> could not be opened: the key or the file has been altered` |
| envelope fails to decrypt, or `token` fails its `token_hash` check | `sealed value for <field path> could not be opened (<reason>)` |

Behaviour in this state — and every clause here is a requirement, not a
preference:

1. **Relay starts.** It does not exit. This is a deliberate departure from
   `EnsureInitialized`'s existing rule that the tray exits over an unreadable
   file, and the reason is that the two states are opposites: an unreadable file
   has *unknown* contents and writing over it is catastrophic, whereas a
   key-mismatched file has *known* contents that relay can still read and show.
   The recovery UI lives in the tray. Exiting would leave the operator with a
   machine whose only recovery surface is the thing that will not start.
2. **The read half works in full.** `relay audit`, `relay grant`, every `list`,
   every `ClassRead` HTTP route, the Settings window (rendering sealed values as
   `unavailable — the sealed store cannot be opened`), the tray menu.
3. **Every write refuses.** `WithDeclinable` returns `errSealUnavailable`
   without running the callback; `settings.json` is left byte for byte as it was.
   A write in this state would re-seal under a key relay does not have and
   destroy every sealed value it could not read.
4. **Every operation that needs a sealed value refuses, naming the state.**
   `ResolvePtyEnv` (no token to hand out), `RemoteServer.resolveGrant` and
   therefore the remote listener (which closes, on the same shape as its existing
   audit refusal in `remote_reconcile.go`), the three admin bridge ops (no
   `admin_secret` to compare against), skill regeneration.
5. **The tray offers the reset.** A menu item **"Reset Sealed Store…"** opens the
   Settings window on a panel that states, before anything happens, exactly what
   is lost: every project and its token, every control-plane credential, every
   enrolment and the CA that signed them, every registered passkey, every stored
   OAuth authorisation. Confirming demands a presence check
   (`op = "sealed.reset"`, digest over the key ids involved), then deletes
   `settings.json`, `ca.key.sealed`, `ca.crt` and the keychain item, and re-runs
   `EnsureInitialized`. This is the second half of the recovery
   `docs/tokens.md` already documents as "deleting settings.json is how an
   operator locks the control plane out".
6. **There is no CLI reset, no `--force-reset` flag, no env var, no offline
   recovery code.** Each of those is a second door into the sealed store. Over
   SSH the reset refuses like every other privileged operation, and the honest
   answer to "my Mac is headless and the key is gone" is `rm` at the shell,
   which was always available and is not a door relay built.

### 5.7 `ca.key`

`LoadOrCreateCA` resolves in this order:

1. `ca.key.sealed` exists → unseal it. Sealer nil or unseal fails → return an
   error naming the degraded state; **do not** fall through to generating a new
   CA, which would silently invalidate every enrolment on the host (the existing
   comment in `enrolment_ca.go` already makes exactly this argument for an
   unreadable `ca.key`).
2. `ca.key` exists and `ca.key.sealed` does not → legacy; migrate per §4.7.
3. Neither exists → generate, seal, write `ca.key.sealed` and `ca.crt`. **Never
   write a plaintext `ca.key` again.**

`ca.crt` stays in the clear: it is a certificate, it is handed to every client in
the enrolment bundle, and `relay enrol` prints its fingerprint.

---

## 6. Decisions 3 and 4 — the presence gate

### 6.1 The rule, and why it is a package

ADR-017 decision 3: *"a gate that fires on `relay credential mint` is skipped by
talking to the `0600` socket directly, and a gate that fires on the socket is
skipped by whatever tomorrow's door is."* The gate lives **in the operation core**
— the one core each capability has per ADR-014 — never on the transport and never
in the CLI.

New package `relaygo/presence`. The package boundary is doing security work, not
tidiness: a `Grant` has only unexported fields, so `package main` cannot
construct a meaningful one. Inside one flat package that protection does not
exist.

```go
package presence

// Provider asks the operating system for a user-presence act.
type Provider interface {
    // Evaluate blocks until the user is present, refuses, or the OS says
    // this session cannot show a prompt. reason is shown in the OS dialog.
    Evaluate(ctx context.Context, reason string) error
}

var (
    ErrNoSession  = errors.New("no session can display a presence prompt")
    ErrRefused    = errors.New("presence was refused")
    ErrUnavailable= errors.New("presence checking is unavailable")
)

type Digest [32]byte

// Grant is proof that a presence act was completed for exactly one
// operation and one argument digest, and has not been spent.
type Grant struct{ id, op string; digest Digest }
func (g Grant) ID() string    // the nonce id, for the audit record
func (g Grant) Valid() bool   // false for the zero value

type Gate struct{ /* provider, clock, nonce table */ }
func NewGate(p Provider) (*Gate, error)   // refuses a nil provider

// Require is the whole production path: prompt, mint a nonce, redeem it.
func (g *Gate) Require(ctx context.Context, op string, d Digest, reason string) (Grant, error)

// Request and Redeem are Require's two halves, exported so the nonce
// table's properties are testable on their own.
func (g *Gate) Request(ctx context.Context, op string, d Digest, reason string) (Grant, error)
func (g *Gate) Redeem(gr Grant, op string, d Digest) error

// GatedOps is every operation name this package knows about. The gate
// refuses an op that is not in it.
var GatedOps = []string{ /* §6.4 */ }
```

### 6.2 The nonce, and why 120s almost never bites

`Request` prompts first and mints the nonce **after** the prompt succeeds:

```
Request(op, digest) -> Provider.Evaluate(...) -> nonce{id, op, digest, exp = now+120s, uses = 1}
```

`Redeem(grant, op, digest)` refuses unless a live nonce exists with **the same
id, the same op and a constant-time-equal digest**, then burns it. Refusals are
uniform — expired, unknown, already burned, wrong op and wrong digest all return
the same `ErrGrantInvalid`, for the oracle reason `AuthenticateAPICredential`
gives.

`Require` is `Request` immediately followed by `Redeem`, and it is the only path
production uses, so the live window between a successful prompt and the act is
microseconds. The 120s bound is the outer limit for a caller that splits the two
— nothing does today, and the tray may later. This is a strengthening of the
owner's shape, not a deviation from it: the specified properties (single use,
operation-bound, argument-bound, 120s) are all present and the realised window is
smaller.

The nonce table is in-memory, bounded (64 entries, oldest evicted), and dies with
the process. There is no persistence and no cross-process nonce.

### 6.3 Digest canonicalisation

The digest is computed **once, inside the core, from the already-validated and
already-normalised request struct**, by a method on that struct:

```go
func (r credentialMintRequest) presenceDigest() presence.Digest
```

**There is exactly one implementation.** Doors never compute a digest. Under
brokering the door hands the core a request struct and the core does everything,
so there is no second site to drift. This is the rule that keeps §3.1's
guarantee true, and it is worth more than any amount of care at each door.

Encoding is **length-prefixed**, never delimiter-joined, so no argument value can
impersonate a field boundary:

```
H = SHA-256 over:
    "relay-presence-v1\x00"
    uvarint(len(op))   op
    uvarint(numFields)
    for each field, in the fixed order the operation declares:
        uvarint(len(name)) name
        0x00 if the value is ABSENT (nil), else 0x01
        encode(value)
```

`encode` by kind, and the kind is fixed per field, not inferred:

| kind | encoding |
|---|---|
| string | `uvarint(len(b))` then the UTF-8 bytes, **after** the core's own normalisation (e.g. `strings.TrimSpace`) |
| bool | one byte, `0x00` / `0x01` |
| duration | `uvarint(len(s))` then `strconv.FormatInt(int64(d), 10)`; zero means "never" and is encoded as `"0"`, distinct from absent |
| set of strings (classes, grants) | deduplicated and sorted lexicographically, then `uvarint(n)` then each element string-encoded. Must use the **same dedup the core applies**. |
| sequence of strings (argv) | order preserved, `uvarint(n)` then each element string-encoded |
| map[string]T | keys sorted, `uvarint(n)` then each `(key, value)` pair encoded |
| raw JSON (`context` values) | `uvarint(len(b))` then the bytes **as relay stores them** — relay forwards arguments verbatim (ADR-013) and must not re-spell them here either |

**Absent is not empty.** Relay distinguishes nil from empty everywhere (a nil
`allowed_tools` map means "no change"; an empty one means "clear"), and the
digest must too. That is what the `0x00`/`0x01` presence byte is for.

### 6.4 The gated operations, and the exact digest input for each

This table is normative. A digest that omits a listed argument is a hole; adding
an argument to an operation without adding it here is a hole.

| op name | core method | digested arguments (in this order) |
|---|---|---|
| `credential.mint` | `CredentialOps.Mint` | `name` (string), `classes` (set), `ttl` (duration) |
| `credential.revoke` | `CredentialOps.Revoke` | `id` (string) |
| `enrolment.create` | `EnrolmentOps.Create` | `client_id` (string), `project_ids` (set), `budget.window_seconds`, `budget.max_calls`, `budget.max_result_bytes` (each duration-kind int, absent-aware) |
| `enrolment.update` | `EnrolmentOps.Update` | `client_id`, `project_ids` (set, absent-aware), the three budget fields (absent-aware) |
| `enrolment.revoke` | `EnrolmentOps.Revoke` | `client_id` (string) |
| `login.bootstrap.mint` | `LoginOps.MintBootstrap` | *(no arguments — digest over an empty field list)* |
| `login.passkey.revoke` | `LoginOps.RevokePasskey` | `id` (string) |
| `mcp.register` | `McpOps.Add` | `display_name`, `transport`, `url`, `command` (strings), `args` (sequence), `env` (map), `tcc_services` (set) |
| `mcp.unregister` | `McpOps.Remove` | `id` (string) |
| `mcp.oauth.start` | `McpOps.StartOAuth` | `id` (string) |
| `service.register` | `ServiceOps.Create` / `Update` | `id`, `display_name`, `command`, `working_dir`, `url` (strings), `args` (sequence), `env` (map), `autostart` (bool), `frontend_consumer` (bool, absent-aware) |
| `service.unregister` | `ServiceOps.Remove` | `id` (string) |
| `project.rotate_token` | `ProjectOps.RotateToken` | `project_id` (string) |
| `project.grant` | `ProjectOps.Create` / `Update` | `project_id` (string, absent on create), `allowed_mcp_ids` (set, absent-aware), `allowed_tools` (map of set, absent-aware), `access` (map, absent-aware), `context` (map of raw JSON, absent-aware), `allow_cwd_auth` (bool, absent-aware), `kind` (string), `path` (string) |
| `sealed.reset` | tray only | `settings_key_id`, `keychain_key_id` (strings, either may be absent) |

Notes on the boundaries of that table, each of which is a decision:

- **`enrolment.update` is gated** though ADR-017's table names only create and
  revoke. `relay enrol update --grant` replaces the whole grant list, which is
  the rule's "widens one" case exactly.
- **`mcp.oauth.start` is gated** because it persists a bearer relay will present
  upstream, and because it opens a browser the caller chose the target of.
- **`project.grant` is the named `configure` subset** ADR-017 decision 3
  requires. It fires on any project create or update whose request *sets* any of
  the listed fields. A project update that touches only `name`,
  `chat_templates`, `session_folders`, `generate_skill`, `permission_policy`,
  `allowed_models` or `shell_templates` is **not** gated — those do not widen the
  grant. `allow_cwd_auth` is in the gated set on its own line because turning it
  on hands the project's whole tool set to any process standing in the directory,
  with no token at all.
- **`disabled_tools`** is not gated: it is a denylist and can only narrow.
- **The `proxy` catch-all is not gated** and stays a named hole (§2).
- **`service.restart` is not gated.** It changes no settings; it restarts what
  is already configured, and the edit that configured it was gated.

### 6.5 The provider

`presence/localauth_darwin.go`, cgo, `-framework LocalAuthentication`. Everything
here was measured by the feasibility spike; none of it is guesswork.

- **`LAPolicyDeviceOwnerAuthentication`**, never
  `deviceOwnerAuthenticationWithBiometrics`. Measured: the former returns YES
  from `canEvaluatePolicy` and raises a real login-password prompt; the latter
  fails `-7` *BiometryNotEnrolled*. This machine has no Touch ID and no Secure
  Enclave (ADR-016 decision 1 priced the same fact), so presence is a
  **login-password prompt**. The boundary holds — a password prompt is not
  clickable — and the cost is that every mint and every enrolment becomes a typed
  password.
- **Do not probe for biometry.** `biometryType` misreports `1` (TouchID) on this
  VM, and `canEvaluatePolicy(...WithBiometrics)` fails for a reason that is not
  "no biometry". Neither is a usable probe; there is no branch to write.
- **This is not a reversal of ADR-016 decision 1.** That decision refused
  *storing* a password verifier. Asking the OS to *check* the login password
  stores nothing, adds no verifier and adds no lockout machinery. Say so in the
  document owed at §10.4, not in a comment.
- A fresh `LAContext` per evaluation, with
  `touchIDAuthenticationAllowableReuseDuration` left at its default of zero. A
  reuse duration is an ambient session and §3.1 forbids one.
- `evaluatePolicy` is asynchronous with a completion handler. Call it from a
  tracked goroutine and block on a channel; **never** from the Cocoa main thread,
  which the tray's run loop owns and which the dialog needs.
- `presence/localauth_other.go`, `//go:build !darwin`, returns `ErrUnavailable`.

#### 6.5.1 Error handling: branch on numeric codes, never on messages

Two measured traps, and an implementation that misses either is wrong in a way
that will not show up in testing:

1. **`canEvaluatePolicy` is worthless as a pre-flight check.** It returned
   **YES inside a LaunchDaemon with no session at all**, where `evaluatePolicy`
   then failed in 265 ms. **Gate nothing on it.** It is the obvious thing an
   implementer reaches for and it does not answer the question it appears to.
2. **In a sessionless context `evaluatePolicy` returns undocumented `-1000`
   "Authentication failure."**, not the documented `kLAErrorNotInteractive`
   (`-1004`). The *message* is **indistinguishable from a wrong password.**

The rules that follow:

- Branch on the numeric `LAError` code. Never parse, compare or log-and-branch on
  the message.
- Treat everything outside `{success, -2 LAErrorUserCancel}` as failure.
- **Never count `-1000` toward failed-attempt logic**, and do not build
  failed-attempt logic that could not tell it apart from a wrong password anyway.
  Relay has no lockout of its own; the OS owns that.
- `-1000` in this context is **not a public contract.** Write the code so that
  the day it changes, relay refuses cleanly rather than mis-classifying. That
  means: everything-but-the-allowlist is failure, and no code path depends on
  `-1000` specifically.

#### 6.5.2 The reason string is a rendering constraint

The dialog renders literally as `"<name> is trying to <reason>."`, so
`localizedReason` must be a **lowercase verb phrase**, one per gated operation,
naming the actual act and the arguments the digest covers:

```
mint a control-plane credential named "eve-view" with classes grant and execute
revoke the enrolment "vm-agent-3"
register an MCP that runs /usr/local/bin/thing
turn on directory authentication for the project "work"
```

A prompt reading "Relay wants to make a change" is a defect: the human answering
it must be approving the same act the gate is binding.

**The keychain consent dialog is not presence.** ADR-017 decision 4 is explicit:
a consent dialog is answered by clicking Allow. The consent dialog is the ACL's
decryption fallback (§5.3) and nothing more; it governs decryption rather than
the operation. Any implementation that satisfies the *gate* with a keychain
prompt has not implemented decision 4 — even though §5.3.4 notes that dialog
to be stronger than ADR-017 assumed.

### 6.6 Refusing a caller that cannot be prompted

The owner chose refuse-over-SSH (§3.2), so this is core functionality, not
hardening. **LocalAuthentication does not refuse a remote caller** — measured, an
SSH-launched binary raised a live password prompt onto the physical console and
blocked there. The refusal has to be built.

**Do not build it by asking the tray about its own session.** The tray is always
in Aqua with graphic access, so it would always believe it can prompt and the
check would fail open. Ask about the **peer**, and take the answer from the
kernel:

```
getsockopt(fd, SOL_LOCAL, LOCAL_PEERTOKEN, &audit_token_t, &len)
  -> audit_token_to_asid(token)
  -> auditon(A_GETSINFO_ADDR, &auditinfo_addr_t{ai_asid}, ...)
  -> ai_flags & AU_SESSION_FLAG_HAS_GRAPHIC_ACCESS
```

Measured: over SSH `asid=100211, graphic=0`; in Aqua `asid=100002, graphic=1`.
Unprivileged, and unforgeable — `setaudit_addr` to set the bit or join another
session returns EPERM for uid 501, and `sudo` inherits the audit session.
`SSH_TTY` / `SSH_CONNECTION` belong to the caller and must not appear anywhere
(**AC-19**).

Keep it small: one darwin-only helper, `presence/session_darwin.go`, with a
`!darwin` stub reporting "not determinable". Resolve it **once per connection**
where `bridge.PeerPID` already is (`bridge/server.go`'s `handleConn`) and carry
it on the context.

The gate reads it:

| what the gate finds on the context | behaviour |
|---|---|
| session present, `graphic = 1` | prompt |
| session present, `graphic = 0` | refuse with the text below, **without calling the provider** |
| no session on the context at all | prompt |

The third row covers the doors with no peer to ask about: the WebView IPC (it
*is* the tray), the tray menu, and HTTP on the loopback TCP mux. The frontend
HTTP socket is a Unix socket and does have a peer, so it behaves like the bridge.

**Named limitation, not designed around:** `open -a <bundle>` from an SSH shell
launches into the console session and reports `graphic=1`, so this check answers
"can this process display UI?" and not "did this come from SSH?". It is honesty
and prompt-spam reduction; the boundary is the password inside the prompt. Say
that once in `docs/presence-gate.md` and stop.

The refusal, shared by every gated operation:

```
refused: this needs your confirmation on the Mac's screen, and the session this
  command is running in cannot show a prompt (for example, you are over SSH).
  There is no queue and no pending-approval list.
  Run it from a terminal in the logged-in desktop session, or from the Relay
  Settings window.
  Read commands are unaffected: relay audit, relay grant, and every `list`.
```

### 6.7 Structural unavoidability, and how it is proven

Three mechanisms, and they are cumulative.

1. **The gate is inside the core, not passed to it.** Each gated core carries a
   `Gate *presence.Gate` field and calls `Require` itself. A door cannot choose
   not to. **A nil `Gate` refuses**, with `errPresenceGateNotWired` — so a
   shipped build that forgot to wire one refuses everything rather than allowing
   everything. Fail closed, at the one place where the failure would otherwise be
   silent.
2. **A structural test pins the set of files allowed to mutate a gated field.**
   `TestGate_NoDoorReachesAGatedMutationOutsideItsCore` parses every non-test
   `.go` file in `package main` with `go/ast` and finds every call to
   `store.With`, `store.WithDeclinable`, `withDeclinable`, and to each mutator in
   the gated set (`MintFor`, `AddAPICredential`, `RemoveAPICredential`,
   `RotateProjectToken`, `UpsertExternalMcp`, `RemoveExternalMcp`,
   `UpsertService`, `RemoveService`, `AddEnrolment`, `RemoveEnrolment`,
   `mintBootstrapCode`, `applyProjectCreate`, `applyProjectUpdate`, and the
   `UpdateProject*` grant-shape mutators). Every such call site must be in a file
   on an allowlist held in the test: the six ops cores, plus the files where the
   mutators are *defined*. A new route, a new IPC handler or a new bridge handler
   that reaches around a core fails the suite by name. This is ADR-015 decision
   2's "do not register the route" argument turned into a build-time check, and
   it is acceptance criterion **AC-15**.
3. **A completeness test pins the gated set itself.**
   `TestGate_EveryGatedOpRefusesWithoutPresence` is table-driven over every entry
   in `presence.GatedOps`, constructs its core with a denying provider, asserts
   the refusal, and asserts `settings.json` is byte-identical afterwards. A
   companion assertion requires the table's key set to equal `presence.GatedOps`
   exactly, so adding a gated op without adding a test fails. **AC-16.**

### 6.8 The hermetic test seam

ADR-016 decision 8: *"a weakening introduced for a test is the weakening most
likely to survive."* ADR-002's criteria for a production seam are the bar. This
design meets it by **having no production seam at all**.

- The fake provider lives in a **separate package**, `relaygo/presence/presencetest`,
  exporting `presencetest.Allow()`, `presencetest.Deny()`,
  `presencetest.NoSession()` and a recording variant.
- **`package main`'s non-test code never imports it.** That is not a convention
  — it is checkable, and it is checked: `TestPresence_SeamIsNotLinkedIntoTheBinary`
  parses every non-test `.go` file in the module with `go/parser` and fails if
  any of them imports `relaygo/presence/presencetest`. Because Go links only what
  is imported, a passing test is a proof that the shipped `relay` binary does not
  contain the fake. **AC-17.**
- **Belt and braces:** `presencetest` has an `init()` that panics unless
  `testing.Testing()` (Go 1.25; the module is on 1.25). If it is ever linked into
  a real binary, that binary dies loudly at start rather than running with the
  gate stubbed.
- **There is no env var, no settings field, no build tag and no `Set*ForTest`
  global anywhere in this feature.** `TestPresence_NoGlobalSwitchExists` greps
  non-test sources for `RELAY_.*PRESENCE`, `skip_presence`, `presence_disabled`
  and `SetPresence` and fails on a hit. **AC-18.**
- Tests wire a fake exactly the way they already wire a config dir: by
  constructing the thing under test with it. That is ADR-002 seam #2's pattern
  (`NewSettingsStoreAt(dir)`), and it adds no production API.

The suite must never raise a prompt. Since a nil gate refuses and the real
provider is only ever constructed in `runTrayApp` — which the hermetic tier does
not call — a test that forgets to wire a fake gets a refusal, not a dialog.
**AC-20** pins that: the whole hermetic suite runs with no window server
interaction, verified by asserting the real provider's constructor is never
reached (a counter in `runTrayApp`'s wiring path that no test exercises).

---

## 7. Decision 2 — brokering

### 7.1 The transport: one new bridge request type

The bridge dispatch table (`bridge/server.go`, `bridgeHandlers`) has ten entries
and ADR-010 makes the point that it is "visibly a security boundary". Add
**one** entry, not thirteen:

```go
ReqAdminOp = "admin_op"   // BridgeRequest{Type: ReqAdminOp, Name: <op>, Arguments: <json>}
```

with the inner table in `package main`, beside the ops cores it dispatches to:

```go
// admin_ops.go
var adminOps = map[string]adminOpHandler{ /* §7.2 */ }
```

Two entries would not be better than thirteen if the inner table were invisible.
It is not: it is one map literal, in one file, next to the cores, and it is the
thing the structural test of §6.7 reads. The reasons for one bridge entry rather
than thirteen:

- `remoteHandlers` (`remote_server.go`) stays a two-entry allowlist and gains
  nothing. **A test asserts `ReqAdminOp` is absent from `remoteHandlers`**
  (**AC-13**), so no admin operation is reachable from a VM — the exact property
  ADR-010 decision 1 buys.
- The `bridge` package stays free of relay's domain types, which it is today.

**`admin_op` carries no bearer, and that is deliberate.** ADR-015 decision 2 and
ADR-016 decision 5 both refuse to spend the `0600` property twice, and ADR-017
decision 2 is explicit that a determination based on *who the caller is* at a
`0600` socket "is a rubber stamp with extra steps". The socket admits the owner;
the gate is what stops the owner's agent. Requiring `admin_secret` here would be
worse than pointless: it is a sealed value the CLI can no longer read, so the
check would be circular.

**Consequence worth naming: after brokering, `admin_secret` has no non-tray
reader.** Its only external readers today are `relay mcp register|unregister`,
`relay service register|unregister|restart` and `relay credential`-adjacent
helper code reaching `s.AdminSecret` to call `bridge.SendReconcile` /
`SendReloadMcp` / `SendReloadService`. Under brokering the tray reconciles itself
after its own core mutates, so the CLI stops reading the secret entirely. That is
what makes sealing it free.

### 7.2 The operation table

| `admin_op` name | CLI command | gated | core |
|---|---|---|---|
| `credential.mint` | `relay credential mint` | ✔ | `CredentialOps.Mint` |
| `credential.revoke` | `relay credential revoke` | ✔ | `CredentialOps.Revoke` |
| `enrolment.create` | `relay enrol create` | ✔ | `EnrolmentOps.Create` |
| `enrolment.update` | `relay enrol update` | ✔ | `EnrolmentOps.Update` |
| `enrolment.revoke` | `relay enrol revoke` | ✔ | `EnrolmentOps.Revoke` |
| `login.bootstrap.mint` | `relay login enrol` | ✔ | `LoginOps.MintBootstrap` |
| `login.passkey.revoke` | `relay login revoke` | ✔ | `LoginOps.RevokePasskey` |
| `mcp.register` | `relay mcp register` | ✔ | `McpOps.Add` |
| `mcp.unregister` | `relay mcp unregister` | ✔ | `McpOps.Remove` |
| `service.register` | `relay service register` | ✔ | `ServiceOps.Create`/`Update` |
| `service.unregister` | `relay service unregister` | ✔ | `ServiceOps.Remove` |
| `service.restart` | `relay service restart` | ✘ | `ServiceOps` (registry only) |
| `project.rotate_token` | — (IPC + HTTP today) | ✔ | `ProjectOps.RotateToken` |

Read commands keep their direct reads and are **not** in this table:
`relay audit`, `relay grant`, `relay credential list`, `relay enrol list`,
`relay login list`, `relay mcp list`, `relay service list`. Verified: none of
them prints or reads a sealed value. `credential list` prints id, name, classes,
created and expires — never the hash. `enrol list` prints the fingerprint.
`login list` prints neither X nor Y. `mcp list` prints command and args, never
env values. `grant` builds a `StoredToken` from the project's permission fields
and never touches `Token`.

### 7.3 The refusal when relay is stopped

`net.Dial` on the socket fails today with a generic bridge error. Requirement:
each mutating command refuses **by its own name**, not with a settings error.
Add `cli_service.go`:

```go
// requireService returns a bridge client, or exits naming the command.
func requireService(command string) *bridge.Client
```

producing exactly:

```
error: relay is not running; `relay credential mint` requires the service.
  relay is the sole broker of its own credentials: the secrets are sealed and
  only the tray holds the key (ADR-017 decision 2). Start Relay and retry.
  Read commands still work with relay stopped: `relay credential list`,
  `relay grant`, `relay audit`.
```

Exit code 1. **`settings.json` must be byte-identical and its mtime unchanged
after the refusal** — the command must dial before it does anything else.
**AC-11.**

### 7.4 Issuance auditing as a hard dependency

**Recommendation: refuse at the operation, not at startup.** The argument:

- ADR-010 refuses the *remote listener* at startup because the listener is a
  distinct, optional subsystem that can be absent while everything else works.
  Issuance is not a subsystem — it is spread across six cores.
- Refusing at startup means `"audit": {"enabled": false}` makes relay refuse to
  start, which destroys the read half, destroys the tray's recovery UI, and
  makes the misconfiguration unfixable except by hand-editing the file relay is
  refusing to open. That is a worse failure than the one it prevents.
- ADR-010 *also* re-checks per connection and per request, with the comment
  "auditing could have been turned off since this listener bound". The
  per-request check is exactly the shape issuance needs, and it is the half of
  ADR-010's rule that transfers.

Behaviour:

```go
// audit_issuance.go
var errIssuanceAuditingRequired = errors.New("issuance auditing is required")

// requireIssuanceAuditor refuses when there is no sink an act could be
// recorded in. Called by every gated core BEFORE it asks for presence.
func requireIssuanceAuditor(a IssuanceAuditor) error
```

- `a == nil`, or a recorder reporting `!Enabled()`, or a recorder with no sink →
  `errIssuanceAuditingRequired`.
- Called **before** `Gate.Require`, so the operator is not made to type a
  password for an act that was going to refuse anyway.
- The existing post-act fail-closed logic (`refuseUnrecordedIssuance`,
  `recordEnrolmentIssued`'s undo, the `rotate_token` withhold) is unchanged.
- `recordIssuance(nil, iss)` keeps returning nil for **ungated** callers. Nothing
  ungated changes.

Message:

```
error: refusing to issue — the tool-call audit log is disabled
  ("audit": {"enabled": false} in settings.json), and with sealed config active
  issuance auditing is a hard dependency, not a courtesy (ADR-017 Consequences;
  the same rule ADR-010 applies to the remote listener).
  Set "audit": {"enabled": true} and restart relay. `relay audit --path` names
  the file relay would write to.
```

### 7.5 The record every gated operation leaves

The ADR's detection argument — *"an `api_credential` issuance with no matching
presence event is the signal this ADR's entire mechanism exists to produce"* —
only works if every gated operation leaves a record. Today `relay mcp register`
and `relay service register` leave none, and neither does a project grant edit.
Two additions:

1. **`CredentialIssuance.PresenceID string`** → **`AuditEvent.PresenceID string
   json:"presence_id,omitempty"`**, set from `Grant.ID()` on every gated act.
   It is a nonce id, not a secret; it carries no plaintext and no hash, and
   `CredentialIssuance` still has no field that could carry one.
2. **A new event kind, `config_change`**, written through the same durable,
   fail-closed `RecordIssuance` path, for gated operations that are not
   issuances: MCP register/unregister, service register/unregister, OAuth start,
   and project grant-shape mutations. Fields: `credential` from a widened
   vocabulary (`external_mcp`, `service`, `project_grant`), `subject` the id,
   `grants` the changed field names for a project grant, `via`, `presence_id`.
   Calling these `credential_issued` would be a lie; giving them no record would
   break the ADR's premise. `relay audit --event config_change` is the operator
   surface. Document in `docs/audit-log.md`.

### 7.6 One writer — what may be retired, and what must not

ADR-017's Consequences say a class of defect stops existing. Precisely which
machinery may go:

| piece | verdict |
|---|---|
| `FileSettingsStore.With`'s reload-before-callback | **Keep.** A hand-edit is still a documented recovery, and the reload is what makes the tray see one. It also still protects the `--config-dir` and test-process cases. |
| resolve-inside-the-callback in every ops core | **Keep, and correct the ADR.** Single-*process* is not single-*goroutine*: HTTP handlers, IPC handlers, bridge handlers, the OAuth refresh callback and the status poller all mutate settings concurrently inside the tray. The TOCTOU window the discipline closes is now intra-process instead of inter-process, and it is exactly as real. |
| `WithDeclinable` | **Keep.** `POST /relay/login/verify` is unauthenticated by design (ADR-016 decision 5) and its refusal path must not write. That is in-process and unaffected by brokering. |
| `withDeclinable`'s non-declinable fallback | **Keep.** Test fakes still implement `SettingsStore` without `DeclinableSettingsStore`. Retiring it is a test-refactor with no security content. |
| `atomicWriteFile`'s unique staging name | **Keep.** Shared with `service_config_file.go`, costs nothing, and a uniquely-named crash leftover is still better than a shared one. |
| `freshSettings` | **Keep.** Still needed: the tray's own cache is refreshed by its 2s poll, and an authorization decision must not answer from it. |

**Retire nothing in this work.** The diff is already large enough that widening
it here is a real risk. The honest change is a documentation one: rewrite
`docs/tokens.md`'s "The settings file has more than one writer" as "settings.json
has one writer and several goroutines", narrowing the last-writer-wins caveat to
hand-edits, and note that a cross-process advisory lock is now unnecessary. If a
follow-on wants to remove machinery, it should be a separate change with its own
justification. **Follow-on, not this work.**

### 7.7 A bug this work forces, and must fix

`appRouter.ListProjects` and `appRouter.GetProject` do `json.Marshal` on the raw
`Project`, **including its plaintext `Token`**, to any holder of a service token.
`ResolveProjectTemplate`'s own comment already asserts that "`ResolvePtyEnv` is
the sole plaintext-token egress over the bridge" — it is not, and has not been.
This is **issue #64**: a live leak, not a hypothetical.

With `Secret` typed, `json.Marshal(proj)` stops compiling-through and starts
erroring at runtime (§4.4), so this cannot be left alone. Fix: give both a
bridge-side projection that strips `Token` and `TokenHash`, the way `projectView`
does for HTTP. `ResolvePtyEnv` then genuinely is the sole plaintext-token egress
over the bridge, which narrows ADR-007's "any service token can read any
project's plaintext" finding by two of its three paths. **AC-8.**

Cross-repository risk: a consumer that reads `.token` out of `ListProjects`
breaks. `../relayLLM` is not checked out on this machine, so this could not be
verified. ADR-007 states relayLLM "resolves the token just-in-time from relay's
bridge by `projectId` (`ResolvePtyEnv`)", which says it does not. **Verify before
merging S4** by grepping the relayLLM tree for `list_projects` and `"token"`.
Recorded in §11.

---

## 8. Decision 5 — non-goals, stated so nobody builds them

These are **non-criteria**. A reviewer must reject work that adds any of them
under this ADR.

- **This does not sandbox a granted agent.** An agent holding a project token
  keeps every tool that grant carries, for as long as the grant lasts, with no
  prompt and no record beyond the ordinary tool-call audit. Prompting per tool
  call is exactly what relay's grant model exists to avoid.
- **This does not shorten an existing grant.** No expiry is added to a project
  token, no grant is narrowed, no session is ended.
- **This does not touch exfiltration.** A granted `mail_search` is a granted
  `mail_search` (ADR-009, ADR-010).
- **This does not stop `rm`.** Revocation is gated; deleting `settings.json`
  remains free to anything running as the owner, and remains the documented way
  to lock the control plane out.
- **This does not close `proxy`.** See §2.
- **This does not defend against a process the owner starts.** It changes what
  such a process can *become*: an agent with a mailbox cannot turn itself into an
  agent with a certificate.

### 8.1 Two residual risks, recorded and not solved here

**The presence prompt is impersonable** — the name in the dialog comes from the
process name (or `CFBundleDisplayName`), never from the code signature, so an
Accessibility-capable attacker on a developer machine can phish the login
password with a convincing fake. Tracked as **issue #65** and explicitly not
solved by this work. The only requirement it places on this spec is negative and
is stated as **NC-7**: no document, comment, prompt string or UI copy may claim a
user can tell a genuine Relay prompt from a fake one.

**The sealing key can be deleted by any local process**, which is denial of
service and is already conceded by ADR-017 decision 5. Substitution is defended
by §5.5.1 and needs nothing further.

---

## 9. What the code says that ADR-017 does not

Every item below was measured, not assumed.

**9.1 The ADR's Context is accurate on the plaintext inventory.** `settings.json`
holds **five plaintext project tokens** and `admin_secret`, right now.
Verified by computing `sha256(token) == token_hash` for all five (true in every
case) and by confirming `admin_secret` is 32 hex characters of a bearer the
bridge compares with `ValidateAdmin`. Door one is real and open today. `ca.key`
is present, 227 bytes, 0600.

**9.2 The binary is Developer ID signed, not ad-hoc.** ADR-017's Context says
`flags=0x10002(adhoc,runtime)`; the installed bundle is `flags=0x10000(runtime)`
with `Authority=Developer ID Application: Example Developer (ABCDE12345)`. This
**removes** the ADR's Consequence "a locally rebuilt relay re-prompts" on this
machine and removes the associated temptation to loosen the ACL. §5.3.

**9.3 The CLI does not go through the ops cores today.** ADR-017 decision 3
places the gate "in the one core each capability already has, per ADR-014", and
assumes every door reaches it. `relay mcp register`, `relay mcp unregister`,
`relay service register|unregister|restart` do **not** call `McpOps` or
`ServiceOps` — they call `store.With` directly through `upsertAndPrint` /
`resolveAndRemove` in `cli_helpers.go`. So a gate placed in the cores today would
be reachable from HTTP and IPC and skipped entirely by the CLI. **Brokering
(decision 2) is therefore a prerequisite for decision 3, not a companion to it**,
and the build order reflects that.

**9.4 Two capabilities have no core at all.** Credential mint/revoke lives in
`credential_cmd.go` as free functions over the store, reached by the CLI and by
`LoginOps.SignOut`. Project mutation lives in `applyProjectCreate` /
`applyProjectUpdate` in `project_apply.go`, reached by `project_routes.go` and
`ipc_projects.go`. Both must be given a core (`CredentialOps`, `ProjectOps`)
before a gate has anywhere to live. This is real work the ADR does not name.

**9.5 `ListProjects` / `GetProject` leak every project's plaintext token over
the bridge.** §7.7. This contradicts the invariant `ResolveProjectTemplate`'s
comment asserts and narrows what ADR-007's brokering claim is currently worth.

**9.6 There is no keychain, Security.framework or LocalAuthentication code in
the repo at all.** Every keychain and presence line is new. The cgo pattern to
follow is `cocoa_darwin.go` (`#cgo LDFLAGS: -framework …`, `_darwin` filename
suffix for the implicit build tag) plus `audit_process_darwin.go` /
`audit_process_other.go` for the explicit-tag split.

**9.7 Two of the five plaintext-token readers are relay authenticating to
itself.** `skills.go`'s `EmitSkills` passes `proj.Token` to
`lister.ListSkillBuckets`, and `remote_server.go`'s `resolveGrant` returns
`proj.Token` to hand to `router.CallTool`. Both re-enter relay's own router
holding a plaintext it could have resolved by hash — `AuthenticateProjectByHash`
exists and takes `proj.TokenHash`. Converting those two would remove two unseal
calls from hot paths and shrink the blast radius of a bug in the sealing layer.
**It is a smaller and better change than sealing would be if it removed the need
for the plaintext entirely — but it does not: `ResolvePtyEnv` and the Projects
tab reveal still need it, so `Project.Token` cannot go away and must be sealed.**
Recorded as a **follow-on**, not part of this work.

**9.8 ADR-017 decision 7's SSH cost is sharper than the ADR states.** With
`relay login enrol` refusing over SSH and `relay credential mint` refusing too, a
**fully headless macOS install cannot bootstrap at all** — there is no way to
register a passkey, and no way to mint a credential to work around it. ADR-017
frames this as "`relay login enrol` over SSH stops working"; the true statement
is that relay stops being administrable without a desktop session. This is
accepted per §3.2 and there is no mitigation, by decision. It must be stated in
`CLAUDE.md` and `docs/tokens.md`.

**9.9 ADR-017's "one writer retires a class of defect" overstates the win.**
§7.6. Single-process is not single-goroutine, and the resolve-inside-the-callback
discipline is still load-bearing.

**9.10 No `services` are registered on the live machine**, and every registered
`external_mcps` entry carries an empty `env` map. So both `service register` and
§4.3's env-value sealing have zero live migration exposure — which is the reason
to do the env work now rather than against populated envs later.

---

## 10. Files, comments, documents, and the build order

### 10.1 Files created

| path | contents |
|---|---|
| `sealed/sealed.go` | `Envelope`, `Sealer`, `NewAESSealer`, the §4.6 format, sentinel errors |
| `sealed/keyring.go` | `Keyring` interface, `NewMemoryKeyring` |
| `sealed/keychain_darwin.go` | cgo Security.framework keyring + the legacy `SecAccess` ACL (§5.2, §5.3.1) |
| `sealed/keychain_other.go` | `//go:build !darwin` stub |
| `sealed/sealed_test.go`, `sealed/keychain_darwin_test.go` | round-trip, AAD binding, corrupt/unsupported classification, key-id mismatch, tamper→prompt (**AC-24**) |
| `presence/presence.go` | `Provider`, `Grant`, `Gate`, `Digest`, `GatedOps`, sentinels |
| `presence/digest.go` | the §6.3 length-prefixed encoder |
| `presence/localauth_darwin.go` | cgo LocalAuthentication provider; numeric `LAError` handling (§6.5) |
| `presence/localauth_other.go` | `//go:build !darwin` stub |
| `presence/session_darwin.go` | cgo `LOCAL_PEERTOKEN` → `audit_token_to_asid` → `auditon(A_GETSINFO_ADDR)` → graphic-access bit (§6.6) |
| `presence/session_other.go` | `//go:build !darwin` stub; reports "not determinable" |
| `presence/presencetest/presencetest.go` | `Allow`, `Deny`, `NoSession`, recording fake; `init()` panics unless `testing.Testing()` |
| `presence/presence_test.go`, `presence/digest_test.go` | nonce single-use/expiry/op-binding/digest-binding; digest determinism, absent-vs-empty, separator-injection resistance |
| `presence/session_darwin_test.go` | the Aqua/no-session split; runs and skips gracefully where it cannot tell |
| `seal_keychain_reach_test.go` | **AC-29** — no CLI entry point reaches the keyring |
| `settings_secret.go` | the `Secret` type, `forEachSecret`, `sealAllSecrets`, `openAllSecrets`, the field-path builder |
| `settings_clone.go` | `(*Settings).Clone` replacing `deepCopySettings`'s JSON round-trip |
| `settings_migrate.go` | §4.7 migration, the `token_hash` self-check, the stale-copy scanner |
| `credential_ops.go` | `CredentialOps` — the core `credential_cmd.go` never had (§9.4) |
| `project_ops.go` | `ProjectOps` — wraps `applyProjectCreate` / `applyProjectUpdate` / `RotateProjectToken`, holds the `project.grant` gate |
| `admin_ops.go` | the `admin_op` inner dispatch table (§7.2) and its handlers |
| `cli_service.go` | `requireService` and the §7.3 refusal text |
| `presence_seam_test.go` | **AC-17**, **AC-18**, **AC-19** |
| `gate_structural_test.go` | **AC-15**, **AC-16** |
| `seal_residue_test.go` | **AC-9**, **AC-10** |
| `docs/sealed-config.md` | the durable *why* for §4 and §5 |
| `docs/presence-gate.md` | the durable *why* for §6 |

### 10.2 Files modified

| path | change |
|---|---|
| `types.go` | `Project.Token`, `OAuthState.{AccessToken,RefreshToken,ClientSecret}` become `Secret`; `ExternalMcp.Env` and `ServiceConfig.Env` become `map[string]Secret` |
| `settings.go` | `Settings.AdminSecret` becomes `Secret`; new `Settings.SealedKeyID string` |
| `settings_store.go` | sealer field + `NewSettingsStoreSealed`; seal in `save`, open in `load`; `errSealUnavailable` / `errSealerRequired`; `EnsureInitialized` gains migration and the degraded-state branch; `deepCopySettings` → `Clone` |
| `enrolment_ca.go` | `ca.key.sealed` (§5.7) |
| `mcp_ops.go`, `service_ops.go`, `enrolment_ops.go`, `login_ops.go` | `Gate` + `Issuance` fields; `requireIssuanceAuditor` then `Gate.Require` at the top of each gated method; `presence_id` on the record |
| `credential_cmd.go` | mint/revoke logic moves to `CredentialOps`; the file becomes flag parsing plus an `admin_op` call |
| `enrol_cmd.go`, `login_cmd.go`, `mcp_cmd.go`, `service_cmd.go` | mutating subcommands become `admin_op` calls through `requireService`; `list` subcommands untouched |
| `cli_helpers.go` | `upsertAndPrint` / `resolveAndRemove` lose their CLI callers; keep or delete with the last caller |
| `project_routes.go`, `ipc_projects.go` | call `ProjectOps` instead of `applyProject*` directly |
| `bridge/types.go`, `bridge/server.go`, `bridge/client.go` | `ReqAdminOp` + `Client.AdminOp(op string, args json.RawMessage)` |
| `router.go` | `ToolRouter` gains `AdminOp`; `ListProjects`/`GetProject` strip the token (§7.7) |
| `remote_server.go` | assert `ReqAdminOp ∉ remoteHandlers`; degraded-state refusal in `resolveGrant` |
| `trayapp.go` | construct the keyring, sealer, provider and gate; pass the gate into every core; the **Reset Sealed Store…** menu item; the degraded-state start path |
| `audit.go`, `audit_issuance.go` | `PresenceID`; `config_change` event; `requireIssuanceAuditor` |
| `skills.go` | `proj.Token` → `proj.Token.Reveal()` |
| `service_registry.go`, `external_mcp.go` | reveal env values at the point of spawn (§4.3) |
| `bridge/server.go`, `bridge/types.go` | resolve the peer's audit session once per connection beside `PeerPID`; `WithCallerSession` (§6.6) |
| `CLAUDE.md` | the CLI table: which commands need the service; the SSH withdrawal; sealed config |
| `docs/tokens.md` | the plaintext inventory becomes a sealed inventory; the one-writer section; the SSH withdrawal; a pointer to `docs/sealed-config.md` |
| `docs/audit-log.md` | `config_change`, `presence_id`, issuance as a hard dependency |
| `docs/decisions/016-…md` | a "superseded in part" note on decision 2's SSH affordance |
| `docs/testing-roadmap.md` | the named gaps: `proxy`, no real Secure Enclave, no headless bootstrap, prompt impersonation (issue #65), and AC-24's SIP caveat |

### 10.3 Comments — what earns its place here

The house rule is that a comment is either *this is subtle* or *this is
deliberate*, and durable *why* goes in a document beside the code. Match the
register of `capability.go` and `settings_store.go`. Specifically:

**Write these comments** (each names a constraint someone will "fix" back into a
bug):

- On `atomicWriteFile`'s caller in `save`: that sealing happens **before**
  serialisation, so the staging file never holds plaintext and nothing at the
  file layer needs to sweep or shred. Someone will try to add sweeping.
- On `Secret.MarshalJSON`: that returning an error is the point — it is what
  turns every residue path into a loud failure instead of a silent leak.
- On the nil-`Gate`-refuses branch in each core: that a missing gate refuses
  rather than allows, and why fail-closed is the only safe default at a wiring
  site.
- On `Gate.Require`'s use of a fresh `LAContext` with no reuse duration: that an
  allowable reuse duration is an ambient session and §3.1 forbids one.
- On the AAD field path: that it binds an envelope to its location so a sealed
  value cannot be moved between fields.
- On the degraded-state branch in `EnsureInitialized`: that it deliberately does
  **not** exit, unlike the unreadable-file branch immediately above it, and why
  the two states are opposites.
- On `presencetest`'s `init()` panic: that it is the last line of defence if the
  import guard ever regresses.

**Do not write these** — they belong in `docs/sealed-config.md` and
`docs/presence-gate.md`: why field-level rather than whole-file; why AES-GCM;
why 120 seconds; why the digest is length-prefixed; why the CLI stopped writing;
why SSH refuses and why that refusal is not a boundary; what a keychain consent
dialog is and is not; why `kSecAttrAccessControl` is unusable and the legacy
`SecAccess` path is a deliberate bet; why relay never creates a replacement key.
And never: change history, issue or PR numbers, what the previous version got
wrong.

### 10.4 The two documents that are owed

- **`docs/sealed-config.md`** — the sealed field list and the "is it a verifier?"
  test; the `token_hash` invariant; the envelope format and the AAD binding; the
  key id and the three degraded states; why every write re-seals; why the residue
  rule is satisfied before serialisation; the ACL, the Developer ID requirement,
  and what an ad-hoc build costs; the break-glass and why there is no offline
  escape hatch.
- **`docs/presence-gate.md`** — why the gate is on the operation; the gated
  operation table and each one's digested arguments; single-use/op-bound/120s and
  why there is no session; why the seam is a separate package and how that is
  proven; the SSH refusal and why it rests on the tray's own session rather than
  on anything a caller supplies — **and why the graphic-access check is honesty
  rather than a boundary, naming `open -a` as the bypass**; why a keychain
  consent dialog is not presence and why ADR-016 decision 1 is not reversed;
  the prompt-impersonation residual risk of §8.1, stated without any claim that
  a user can tell a real prompt from a fake.

### 10.5 Build order

Each step leaves the tree green (`go test ./...` and `go test -race ./...`) and
is independently committable.

```
S0 ──┬── S1 ──┬── S5 ──┬── S6a S6b S6c S6d ──┬── S8
     │        │        │                     │
     ├── S2 ──┴── S4 ──┘                     │
     │                                       │
     └── S3 ─────────────────────────────────┘
                   S7 (after S4 + S5) ───────┘
```

**S0 — `Settings.Clone()`.** Replace `deepCopySettings`'s JSON round-trip with a
hand-written deep copy; add the reflect completeness test. No behaviour change.
Blocks S4. *Small. One developer.*

**S1 — the `presence` package.** `Provider`, `Gate`, `Grant`, `Digest`, the
length-prefixed encoder, `GatedOps`, the darwin provider with §6.5.1's numeric
error handling, the peer-session probe of §6.6, both `!darwin` stubs,
`presencetest`, and the seam guard tests (**AC-17/18/19**). Wired into nothing.
*Parallel with S2 and S3. The session probe and the LA provider are separable and
can be two developers.*

**S2 — the `sealed` package.** `Envelope`, `Sealer`, `NewAESSealer` with the key
id in the AAD, the keyring interface, the memory keyring, the darwin keychain
keyring and its legacy `SecAccess` ACL, the `!darwin` stub. Wired into nothing.
*Parallel with S1 and S3. The ACL mechanism is measured (§5.3.1), so this no
longer carries spike risk — but it does carry the deprecated-API bet, so land the
document paragraph with the code.*

**S3 — the `admin_op` transport.** `ReqAdminOp` in `bridge`, `Client.AdminOp`,
`ToolRouter.AdminOp`, an **empty** inner table in `admin_ops.go`, `requireService`
and its refusal text, and **AC-13** (`admin_op ∉ remoteHandlers`). Nothing is
registered yet, so nothing changes. *Parallel with S1 and S2.*

**S4 — sealing (needs S0, S2).** The `Secret` type; `forEachSecret` and its
reflect completeness test; store seal/open; `sealed_key_id`; env-value sealing and
its spawn-path consumers (§4.3); the four degraded states, the never-create-a-key
rule (§5.5.1) and the degraded start path; migration and the stale-copy scanner;
`ca.key.sealed`; the `ListProjects`/`GetProject` fix (§7.7) with its pre-merge
check (**AC-8b**); the CLI-cannot-reach-the-keyring test (**AC-29**). **The
largest step.** *One developer, or two splitting migration from the store
change.*

**S5 — the gate (needs S1).** Create `CredentialOps` and `ProjectOps`; add
`Gate` and `Issuance` to all six cores; `requireIssuanceAuditor`; `config_change`
and `presence_id`; move `project_routes.go` and `ipc_projects.go` onto
`ProjectOps`; update every test that constructs a core to pass a fake (41 struct
literals and ~56 direct calls to the free functions, so budget for churn); the
structural and completeness tests (**AC-15/16**). *Parallel with S4 — they touch
different files, with `settings.go`'s mutators the one contact point. Coordinate
there.*

**S6 — brokering (needs S3, S5).** Register the inner table; convert each
mutating CLI command. **Fans out four ways once the table exists:**
- **S6a** `credential mint|revoke`
- **S6b** `enrol create|update|revoke`
- **S6c** `login enrol|revoke`
- **S6d** `mcp register|unregister`, `service register|unregister|restart`

**S7 — the tray (needs S4, S5).** Construct keyring → sealer → provider → gate in
`runTrayApp`; pass the gate into every core; the **Reset Sealed Store…** menu
item behind `sealed.reset`; the ACL tamper test (**AC-24**). *Parallel with S6.*

**S8 — documents (needs all).** `docs/sealed-config.md`, `docs/presence-gate.md`,
`CLAUDE.md`, `docs/tokens.md`, `docs/audit-log.md`, the ADR-016 note,
`docs/testing-roadmap.md`. *One developer, last.*

**Genuinely parallelisable:** {S1, S2, S3} after S0; {S4, S5} after those;
{S6a, S6b, S6c, S6d, S7} after those. Peak useful concurrency is four to five
developers.

**The end-to-end acceptance run against the live config dir (§12, AC-28) happens
after S7 and before S8**, so the documents describe what was measured.

---

## 11. Assumptions I made rather than blocking

Each of these would normally be a question. The safer available answer was
taken; the reasoning is here so it can be overturned cheaply.

1. **Seal `access_token` and `client_secret` alongside `refresh_token`.** ADR-017
   names only refresh tokens. All three are bearers relay presents upstream and
   leaving two clear beside one sealed is an inconsistency. Safer direction, no
   cost. *(Not an owner question — just a widening.)*
2. ~~Do not seal `env` values.~~ **Decided by the owner: seal the values, leave
   the keys in the clear.** §4.3. No longer an open question and no longer a
   named gap. Env keys stay in the clear, which is what preserves the hand-edit
   affordance the earlier draft was protecting.
3. **Relay deletes no file it did not create**, including
   `settings.json.bak-fsreview` and `settings.json.pre-v3-backup`. It names them
   and says the tokens in them are live. **Confirmed by the owner: this stands.**
   Relay may not delete a file it did not create. The two files on this machine
   will be cleaned up by hand — that is an operator act and the spec does not
   encode it.
4. **Issuance auditing refuses at the operation, not at startup.** §7.4. Refusing
   at startup would take out the read half and the recovery UI.
5. **Relay starts in the degraded state rather than exiting.** §5.6 clause 1.
   Departs from `EnsureInitialized`'s existing "the tray exits" rule for this one
   new state, because the recovery surface is the tray.
6. **`admin_op` requires no bearer.** §7.1. ADR-016 decision 5's rule; the
   alternative is circular anyway.
7. **A new event kind `config_change` rather than stretching
   `credential_issued`.** §7.5. Goes slightly beyond the ADR's letter and is
   required by its detection argument.
8. **`enrolment.update` and `mcp.oauth.start` are gated** though ADR-017's table
   names neither. Both fit "widens one" / "issues a credential". §6.4.
9. **Retire no settings-store machinery in this work.** §7.6. Recommended as a
   follow-on if anything.
10. **§7.7's token-strip: searched, and nothing on this machine consumes it.**
    `~/source/barelyworkingcode/` holds `relay`, `relayRemote`, `macMCP`,
    `fsmcp`, `testMail` and `testfolder`. Grepping all of them for
    `ListProjects` / `list_projects` / `GetProject` / `get_project` and for any
    reader of a project `token` field returns **nothing**. `relayRemote`'s only
    hits are three test assertions that `ResolvePtyEnv` is *refused* to remote
    clients, which is the opposite of a consumer. So the only consumer that could
    break is `../relayLLM`, which is **not checked out on this box** — ADR-007
    states it resolves by `projectId` through `ResolvePtyEnv` and never reads
    `ListProjects` for a token. **Kept as a pre-merge check on S4** and written
    into the criteria as **AC-8b**.
11. ~~The keychain ACL mechanism is a spike.~~ **Resolved by measurement.** The
    legacy `SecTrustedApplicationCreateFromPath` + `SecAccessCreate` +
    `kSecAttrAccess` path works on 26.4; `kSecAttrAccessControl` is unusable
    (`-34018`). §5.3.1. This is now a deliberate deprecated-API bet, not an open
    choice.
12. ~~The no-GUI-session signal is a spike.~~ **Resolved by measurement, and it
    corrected the design.** The tray consulting its own session would never
    refuse; the gate must consult the **peer's** kernel-supplied audit session.
    §6.6. The refusal is honesty and prompt-spam reduction, not a security
    boundary, and the document must say so.

---

## 12. Acceptance criteria

Each is written so it can **fail**. A criterion with no failing case is not a
criterion. Unless stated otherwise, each is a hermetic test (`go test ./...`).

### Sealing (decision 1)

**AC-1 — The sealed set is sealed.** After a save, `settings.json` on disk
contains none of: any project's plaintext token, `admin_secret`, any
`access_token`, `refresh_token` or `client_secret`, or **any `env` value on any
MCP or service**. *Fails if* a byte-scan of the file for the seeded sentinel
plaintexts finds any of them.

**AC-1b — Env keys stay readable and every env value is sealed, uniformly.**
Every key of every `env` map is a plain JSON string; every value is an envelope
object — including values that are obviously not secret. *Fails if* any key is
sealed, if any value is not, or if a name-based heuristic leaves some values in
the clear.

**AC-2 — The clear set stays clear and readable.** After the same save, a plain
`json.Unmarshal` into `map[string]any` reads, as ordinary strings and structures:
every `projects[].token_hash`, every `api_credentials[].hash`,
`login_bootstrap.hash`, every `passkeys[].x` / `.y`, every
`enrolments[].fingerprint`, every project name/path/`allowed_mcp_ids`/
`allowed_tools`/`access`/`context`/`allow_cwd_auth`, every MCP command, args and
**env keys**, the `audit` and `remote` blocks. *Fails if* any of these is an envelope object.

**AC-3 — `relay grant` works against a stopped relay and touches no plaintext.**
With relay not running and a sealed `settings.json`, `relay grant --json`
produces the same output it produced before sealing (same MCPs, mode, outbound,
tools, and the **real** scope values, with a filesystem-root scope still called
out). *Fails if* the output differs, if the command errors, or if a source-level
check finds a `Reveal()` call reachable from `runGrantCommand`.

**AC-3b — `relay audit` is unaffected.** `relay audit --tail 20 --json` against a
sealed install returns the same records as before. *Fails if* it errors or if it
reads `settings.json` for anything but the audit config.

**AC-4 — The sealed set is enumerated in exactly one place, completely.**
`forEachSecret` visits every field of type `Secret` reachable from `*Settings`,
proven by a reflection walk of the type. *Fails if* a `Secret` field exists that
`forEachSecret` does not visit — which is how a field added later reaches disk in
the clear.

**AC-5 — An unsealed `Secret` cannot be serialised.** `json.Marshal` of a
`Settings` (or a `Project`, or an `OAuthState`) holding an unsealed `Secret`
returns an error. *Fails if* it succeeds, or if it emits the plaintext, or if it
silently emits a placeholder.

**AC-5b — A `Secret` never renders its plaintext.** `fmt.Sprintf("%v %s %+v", …)`
and an `slog` call carrying a `Settings` produce `<sealed>` and never the
plaintext. *Fails if* any format verb reveals it.

**AC-6 — The `token_hash` invariant is checked, both ways.** (a) Migration
refuses, writes nothing and names the project when a project's plaintext does not
hash to its stored `token_hash`. (b) Load enters the degraded state and names the
field when an unsealed token does not hash to its stored hash. *Fails if* either
proceeds silently.

**AC-7 — The envelope tells sealed from corrupt from wrong-key.** Four fixtures —
a valid envelope, an object with `"sealed":"v2"`, an object missing `ct`, and a
valid envelope under a different key id — produce four distinguishable, named
errors. *Fails if* any two collapse into the same message, or if any surfaces as
a generic JSON parse error.

**AC-8 — `ResolvePtyEnv` is the sole plaintext-token egress over the bridge.**
`ListProjects` and `GetProject` responses contain neither `token` nor
`token_hash`. *Fails if* either appears in the marshalled response.

**AC-8b — Pre-merge check on S4, not a test.** Grep `../relayLLM` for
`list_projects` / `ListProjects` / `get_project` and for any reader of a project
`token` field, and confirm it resolves through `ResolvePtyEnv` by `projectId`.
Every other repository on this machine has already been searched and none
consumes it (§11.10). *Blocks the merge of S4 if relayLLM reads a token out of
`ListProjects`* — in which case the fix is on relayLLM's side, and this criterion
is the record that it was checked rather than assumed.

### The residue rule

**AC-9 — No plaintext ever lands on the filesystem.** Seed a store with sentinel
plaintexts; perform a mint, an enrolment create, an MCP register, a project
update and an OAuth-state write; then scan **every file in the config dir**,
including every `settings.json.*.tmp` left behind, for every sentinel. *Fails if*
any sentinel is found in any file.

**AC-10 — Not even transiently.** Inject a failure into `atomicWriteFile` between
`CreateTemp` and `Rename`, so the staging file is left on disk, and scan it for
every sentinel. *Fails if* any sentinel is found. This is the criterion the
design most easily gets wrong, and it is the one that distinguishes sealing
before serialisation from re-securing after the fact.

**AC-10b — Migration names pre-existing plaintext and deletes nothing.** Given a
config dir containing `settings.json.bak-fsreview` and
`settings.json.pre-v3-backup` with plaintext tokens, migration (a) seals
`settings.json`, (b) emits one warning per stale copy naming the file and the
count of live tokens in it, and (c) leaves both files byte-identical. *Fails if*
either file is modified or removed, or if no warning names it.

### Brokering (decision 2)

**AC-11 — Every mutating command refuses by name with relay stopped.** With no
bridge socket, each of `relay credential mint`, `relay credential revoke`,
`relay enrol create`, `relay enrol update`, `relay enrol revoke`,
`relay login enrol`, `relay login revoke`, `relay mcp register`,
`relay mcp unregister`, `relay service register`, `relay service unregister`
exits non-zero with a message **containing that command's own name** and the
words "requires the service". *Fails if* the message is a settings error, a
generic bridge error, or omits the command name.

**AC-12 — And touches nothing.** After each refusal in AC-11, `settings.json` is
byte-identical and its mtime is unchanged. *Fails if* either moves.

**AC-13 — No admin operation is reachable from a VM.** `ReqAdminOp` is absent
from `remoteHandlers`, asserted directly against the map. *Fails if* it is
present, which is also the test that catches someone adding it later.

**AC-14 — Read commands keep their direct reads.** With relay stopped and a
sealed file, `relay credential list`, `relay enrol list`, `relay login list`,
`relay mcp list`, `relay service list`, `relay grant` and `relay audit` all
succeed and print no `<sealed>` placeholder. *Fails if* any needs the service or
prints a placeholder where a real value belongs.

**AC-14b — A CLI store cannot write.** A `FileSettingsStore` constructed without
a sealer refuses every `With` / `WithDeclinable` with `errSealerRequired` and
leaves the file untouched. *Fails if* a write succeeds.

### The gate (decision 3)

**AC-15 — No door reaches a gated mutation outside its core.** The `go/ast` walk
of §6.7 finds every call to `store.With`, `withDeclinable` and each gated mutator
in non-test `package main` sources, and every call site is on the allowlist.
*Fails, naming the file and the mutator,* when a new route, IPC handler or bridge
handler reaches around a core. **This is the criterion that answers "a NEW route
registered without the gate cannot reach a gated operation."**

**AC-16 — Every gated operation refuses without presence, and the set is
complete.** Table-driven over `presence.GatedOps`: each core method with a
denying provider refuses, and `settings.json` is byte-identical afterwards. The
table's key set equals `presence.GatedOps` exactly. *Fails if* any operation
proceeds, if any mutates the file, or if the two sets differ — which is how a
gated op added without a test is caught.

**AC-16b — A nil gate refuses.** Each core with `Gate == nil` refuses every gated
method with `errPresenceGateNotWired`. *Fails if* any allows.

**AC-16c — The `configure` subset is exactly right.** A project update setting
only `name`, `chat_templates`, `session_folders`, `generate_skill`,
`permission_policy`, `allowed_models` or `shell_templates` does **not** prompt. A
project update setting any of `allowed_mcp_ids`, `allowed_tools`, `access`,
`context`, `allow_cwd_auth`, `kind` or `path` **does**. *Fails if* either
direction is wrong. `allow_cwd_auth` gets its own case: turning it on must
prompt, in both the HTTP and the IPC door.

**AC-16d — `proxy` is not gated.** A request through the `/` catch-all raises no
prompt. *Fails if* it does — the hole is deliberate and a gate there is a
regression, not a fix.

### Presence (decision 4)

**AC-21 — A grant is single-use.** Redeeming the same grant twice: the second
refuses. *Fails if* it succeeds.

**AC-22 — A grant is operation-bound and argument-bound.** A grant for
`credential.mint` with digest D refuses for `enrolment.create`, and refuses for
`credential.mint` with digest D′. Concretely: a prompt answered for
`mint --name x --class read` cannot mint `--name x --class grant --class execute`,
and a prompt answered for an enrolment cannot be spent on a mint. *Fails if*
either is accepted.

**AC-22b — A grant expires at 120s.** With a fake clock, a grant redeemed at
119s succeeds and one redeemed at 121s refuses. *Fails if* either is wrong.

**AC-22c — Refusals are uniform.** Expired, unknown, already-burned, wrong-op and
wrong-digest all return the identical error value and message. *Fails if* any is
distinguishable — that is an oracle.

**AC-22d — The digest covers every listed argument.** For each operation in
§6.4, changing each digested argument in turn changes the digest; changing an
argument that is deliberately not digested does not. *Fails if* any listed
argument does not move the digest. **AC-22e — absent is not empty:** a nil map
and an empty map produce different digests, as do an absent duration and a zero
one. **AC-22f — no separator injection:** two distinct argument sets whose naive
concatenation would collide produce different digests.

**AC-23 — Presence, not a consent dialog.** A source-level check finds no
keychain-consent path reachable from `Gate.Require`, and the darwin provider
calls `evaluatePolicy` with `LAPolicyDeviceOwnerAuthentication`. *Fails if* the
gate is satisfied by anything that governs decryption rather than the operation.
(§5.3.4 notes that the consent dialog also demands the password, which makes
the ACL arm stronger than ADR-017 assumed — it does not make it an acceptable
substitute for the gate, because it gates the wrong act.)

**AC-23b — Reason strings render correctly and name the act.** Every gated
operation's `localizedReason` is a lowercase verb phrase that reads correctly in
`"<name> is trying to <reason>."`, and names at least the arguments the digest
covers. *Fails if* any is capitalised, is a sentence, or is generic
("make a change", "continue").

**AC-24 — The ACL degrades in the safe direction (manual, `-tags=live`).**
Re-sign `/Applications/Relay.app` ad-hoc and read the item: measured, this
returns `-25308` where no interaction is possible and the keychain's own consent
dialog where it is. A build with a different cdhash but the same team and bundle
identifier still unlocks silently — the ACL is identity-bound, not path-bound,
which is why AC-29 is not optional. *Fails if* the unlock stays silent for a
different identity, or if relay fails open. *(SIP is disabled on this VM and an
unsigned binary runs here, so this criterion measures something weaker than it
would on a production Mac; re-measure before treating it as a security
property.)*

**AC-24b — No dev-mode ACL relaxation exists.** A grep of non-test sources and
`build.sh` finds no branch that weakens the ACL for an unsigned or ad-hoc build.
*Fails on a hit.*

**AC-29 — No CLI code path can reach the keychain. ⚠ This is the criterion that
makes §5.3.3 real.** A `go/ast` call-graph walk from every CLI entry point
(`runCredentialCommand`, `runEnrolCommand`, `runLoginCommand`, `runMcpCommand`,
`runServiceCommand`, `runGrantCommand`, `runAuditCommand`, `runMcpExec`,
`runMcpOrServer`) reaches none of `sealed.NewKeychainKeyring`, `Sealer.Unseal`,
or `Secret.Reveal`. *Fails, naming the entry point and the call chain,* if a
future change to `credential_cmd.go`, `enrol_cmd.go`, `login_cmd.go`,
`mcp_cmd.go` or `service_cmd.go` reintroduces one. `relay` the CLI satisfies the
ACL by code identity, so this is unreachability in ADR-015 decision 2's spirit —
not a check that could be inverted.

### SSH (decision 7)

**AC-19 — Relay never reads the caller's environment for this.** A grep of all
non-test sources finds no `SSH_TTY` and no `SSH_CONNECTION`. *Fails on a hit.*

**AC-19b — A peer with no graphic access is refused, without a prompt.** Every
gated operation refuses within milliseconds with the §6.6 text, **and the
presence provider is never called** (asserted with a counting fake). *Fails if*
it queues, blocks, retries, or raises a prompt on the console.

**AC-19c — The probe asks about the peer, not about relay.** A source-level check
finds no path running the graphic-access probe against relay's own pid or
session. *Fails on a hit* — that is the wrong turn §6.6 documents, and it fails
open.

**AC-19d — No peer means prompt, not refuse.** A gated operation arriving with no
caller session on the context (the WebView IPC, the tray menu, the loopback TCP
listener) prompts. *Fails if* it refuses — that breaks the Settings window and
the browser view for no gain.

**AC-19e — The read half survives.** `relay audit`, `relay grant` and every
`list` succeed from a peer with no graphic access. *Fails if* any refuses.

**AC-19f — `relay login enrol` refuses from a session with no graphic access.**
A named test, because ADR-016 decision 2's affordance is withdrawn on purpose and
this is the record of it. *Fails if* it succeeds.

**AC-19g — The provider classifies errors numerically and gates nothing on
`canEvaluatePolicy`.** No call to `canEvaluatePolicy` is used as a pre-flight
condition; the provider branches on the numeric `LAError` code and never on the
message; everything outside `{success, -2}` is failure; removing the `-1000` case
changes no behaviour. *Fails on any of the four.*

### Break-glass (decision 3, settled)

**AC-25 — Key-id mismatch is detected and named exactly.** With a
`settings.json` carrying `sealed_key_id: Y` and a keychain holding key X, relay
(a) starts, (b) reports `the sealed store is bound to key X, settings.json
expects Y`, (c) serves `relay grant` and `relay audit`, (d) refuses every write
with `errSealUnavailable` leaving the file byte-identical, and (e) refuses
`ResolvePtyEnv` and closes the remote listener with named reasons. *Fails if*
relay exits, if the message is a parse or decrypt error, if any write lands, or
if any sealed operation proceeds.

**AC-25b — Missing key is a distinct message.** Same shape with no keychain item
at all: `no such key is in the login keychain`. *Fails if* it is
indistinguishable from AC-25.

**AC-25c — Reset is GUI-only and behind presence.** The tray's reset demands a
`sealed.reset` grant and, on success, removes `settings.json`, `ca.key.sealed`,
`ca.crt` and the keychain item, then re-initialises. *Fails if* it proceeds
without presence, or if it leaves any of the four behind.

**AC-25d — There is no second door.** A grep finds no CLI reset subcommand, no
`--force-reset`, no `RELAY_*` env var and no settings field that resets or
bypasses the sealed store. *Fails on a hit.*

**AC-25e — A deleted key is never silently replaced.** Delete the keychain item
behind relay's back and restart. Relay (a) reports the §5.6 "no such key" message
naming the expected id, (b) refuses every sealed operation, (c) keeps
`relay grant` and `relay audit` working, and (d) **creates no replacement key** —
asserted by checking the keychain is still empty after the run. *Fails if* a key
appears. **This is the single most dangerous failure in the whole design: an
implementation that helpfully creates a key when it cannot find one has handed
an attacker the store.**

**AC-25f — A planted foreign key is never used.** Delete relay's item, plant a
different 32-byte key under the same service and account name with a different
key id, and restart. Relay reports the key-id mismatch, refuses sealed
operations, and never seals or unseals anything with the foreign key — asserted
by confirming `settings.json` is byte-identical after the run and that no
envelope carries the foreign id. *Fails if* relay adopts it, re-seals under it,
or offers any affordance to accept it.

### Issuance auditing as a hard dependency

**AC-26 — With auditing off, gated operations refuse.** With
`"audit": {"enabled": false}`, every gated operation refuses with
`errIssuanceAuditingRequired` and the §7.4 message. *Fails if* any proceeds.

**AC-26b — And refuses before prompting.** The refusal happens with zero calls to
the presence provider, asserted with a counting fake. *Fails if* the provider was
called — an operator must not type a password for an act that cannot proceed.

**AC-26c — Relay still starts with auditing off.** The tray comes up, the read
half works, and non-gated operations work. *Fails if* relay exits — that is the
startup-refusal design this spec rejected.

**AC-27 — Every gated act carries its nonce id.** Each gated operation's audit
record (`credential_issued`, `credential_revoked` or `config_change`) carries a
`presence_id` equal to the `Grant.ID()` that authorised it. *Fails if* any is
empty, or if two acts share one.

**AC-27b — Gated non-issuances are recorded.** `mcp.register`,
`mcp.unregister`, `mcp.oauth.start`, `service.register`, `service.unregister`
and `project.grant` each write a `config_change` record. *Fails if* any leaves no
record — that is the gap that would break the ADR's detection argument.

**AC-27c — No record carries a secret.** No issuance or `config_change` record
contains a plaintext, a hash, a public key coordinate or an envelope. *Fails on
any.*

### The hermetic test seam

**AC-17 — The fake cannot be in a shipped build.** No non-test `.go` file in the
module imports `relaygo/presence/presencetest`, asserted by parsing every source
file. Because Go links only what is imported, a pass is a proof of absence.
*Fails on any import* — including a transitive one through another package.

**AC-17b — And it defends itself.** `presencetest`'s `init()` panics unless
`testing.Testing()`. *Fails if* the guard is absent.

**AC-18 — There is no switch.** A grep of non-test sources finds no
`RELAY_.*PRESENCE`, `skip_presence`, `presence_disabled`, `SetPresence*`,
`*ForTest` or build tag that disables or weakens the gate. *Fails on a hit.*

**AC-20 — The suite never raises a prompt.** The real LocalAuthentication
provider's constructor is never reached during `go test ./...`, asserted with a
counter the production wiring increments. *Fails if* the count is non-zero — and
the developer-visible symptom of a regression here is a password dialog during a
test run, which is what this criterion exists to prevent.

### Non-criteria (decision 5) — reject work that adds these

**NC-1** A granted agent is not sandboxed, and no tool call raises a prompt.
**NC-2** No existing grant is shortened, expired or narrowed.
**NC-3** Nothing here addresses exfiltration.
**NC-4** `rm settings.json` still locks the control plane out, and remains the
documented recovery.
**NC-5** `proxy` remains ungated (AC-16d asserts it).
**NC-6** No `relay run`, no `sandbox-exec` profile, no Secure Enclave code, and
no `kSecAttrAccessControl` (§5.3.1 — it is unusable, not deferred).
**NC-7** No mitigation for prompt impersonation (§8.1, issue #65), and no
language anywhere — document, comment, prompt string or UI copy — claiming a user
can tell a genuine Relay prompt from a fake one.
**NC-8** No failed-attempt counting, lockout or rate limiting around the presence
prompt, and no key-integrity machinery beyond §5.5.1's two rules.

### End-to-end, against the real machine

**AC-28 (manual, once, after S7).** Against the live config dir — five plaintext
project tokens, `admin_secret`, `ca.key`, two stale plaintext copies, relay
running from `/Applications/Relay.app`:

1. Stop relay, back up the config dir, start relay.
2. Migration seals the file in one write; all five `token_hash` checks pass; the
   two stale copies are named and untouched; `ca.key.sealed` exists and `ca.key`
   is gone.
3. `relay grant` and `relay audit` produce the same output as before, with relay
   stopped.
4. The Hermes MCP stack still works end to end (see `RUNBOOK-vm-stack.md`) —
   project tokens still resolve, `relay audit` still records the calls.
5. `relay credential mint --name t --class read` raises exactly one
   login-password prompt and mints; the audit record carries a `presence_id`.
6. The same command over SSH refuses immediately with the §6.6 text and
   **raises no prompt on the console**.
7. Deleting the keychain item and restarting produces AC-25b's named message,
   the read half still works, the tray offers the reset, and **no replacement key
   is created**.
8. Planting a foreign key under the same service and account name produces the
   key-id mismatch and `settings.json` is byte-identical afterwards.

*Fails if any step needs a manual repair the spec did not describe.*
